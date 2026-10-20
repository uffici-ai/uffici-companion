package memoryops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/surface"
	"github.com/uffici-ai/uffici-companion/internal/wire"
)

const (
	request = "6f1c0f7a-2f0e-4f2e-9b1e-0d6a0b5c7e21"
	factURN = "urn:uffici:fact:9c8b7a6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d"
	canary  = "memory-canary-in-a-fact-71"
)

// fakeHost records what it was asked and answers what the case queued.
type fakeHost struct {
	answer json.RawMessage
	err    error
	block  bool
	op     contracts.MemoryOp
	id     string
	body   json.RawMessage
	calls  int
}

func (f *fakeHost) Memory(ctx context.Context, op contracts.MemoryOp, requestID string, body json.RawMessage) (json.RawMessage, error) {
	f.calls++
	f.op, f.id, f.body = op, requestID, body
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return f.answer, f.err
}

type classified struct{ class, instruction string }

func (c classified) Error() string            { return c.class }
func (c classified) ErrorClass() string       { return c.class }
func (c classified) ErrorInstruction() string { return c.instruction }

func handler(host *fakeHost, logs *bytes.Buffer) map[string]surface.CallHandler {
	logger := slog.New(slog.DiscardHandler)
	if logs != nil {
		logger = slog.New(slog.NewJSONHandler(logs, nil))
	}
	return (&Handler{Host: host, Logger: logger}).Handlers()
}

func failureOfErr(t *testing.T, err error) *surface.CallFailure {
	t.Helper()
	var failure *surface.CallFailure
	if !errors.As(err, &failure) {
		t.Fatalf("not a call failure: %v", err)
	}
	return failure
}

func TestServesAllFourOps(t *testing.T) {
	handlers := handler(&fakeHost{}, nil)
	for _, op := range []string{wire.OpMemoryRecall, wire.OpMemoryRemember, wire.OpMemoryForget, wire.OpMemoryUnfold} {
		if handlers[op] == nil {
			t.Fatalf("no handler for %s — the feature would not be announced", op)
		}
	}
}

func TestRememberRelaysTheAnswer(t *testing.T) {
	host := &fakeHost{answer: json.RawMessage(`{"urn":"` + factURN + `"}`)}
	out, err := handler(host, nil)[wire.OpMemoryRemember](context.Background(),
		json.RawMessage(`{"requestId":"`+request+`","text":"the pump leaks"}`))
	if err != nil || string(out) != `{"urn":"`+factURN+`"}` {
		t.Fatalf("remember = %s, %v", out, err)
	}
	if host.op != contracts.MemoryRemember || host.id != request {
		t.Fatalf("the host was asked %s on %s", host.op, host.id)
	}
}

func TestRefusesAMalformedPayloadWithoutAsking(t *testing.T) {
	host := &fakeHost{}
	_, err := handler(host, nil)[wire.OpMemoryRemember](context.Background(),
		json.RawMessage(`{"requestId":"`+request+`","text":"x","level":"organization"}`))
	if failureOfErr(t, err).Class != wire.ClassMalformedPayload || host.calls != 0 {
		t.Fatalf("a level argument = %v (asked %d times)", err, host.calls)
	}
}

// 🔴 A request never rung is `not_permitted`, and nothing is sent.
func TestUnknownRequestIsNotPermitted(t *testing.T) {
	host := &fakeHost{err: contracts.ErrUnknownRequest}
	_, err := handler(host, nil)[wire.OpMemoryRecall](context.Background(),
		json.RawMessage(`{"requestId":"`+request+`","query":"pump"}`))
	failure := failureOfErr(t, err)
	if failure.Class != wire.ClassNotPermitted || failure.Instruction != UnknownRequestInstruction {
		t.Fatalf("unknown request = %+v", failure)
	}
}

// 🔴 The platform's class and sentence ride the failure — never a bare error, and
// never an empty recall.
func TestRelaysThePlatformsClassAndSentence(t *testing.T) {
	host := &fakeHost{err: classified{"forget_refused", "That memory cannot be marked forgotten."}}
	_, err := handler(host, nil)[wire.OpMemoryForget](context.Background(),
		json.RawMessage(`{"requestId":"`+request+`","urn":"`+factURN+`"}`))
	failure := failureOfErr(t, err)
	if failure.Class != wire.ClassUpstreamFailed || failure.Instruction != "That memory cannot be marked forgotten." ||
		!strings.Contains(string(failure.Upstream), "forget_refused") {
		t.Fatalf("a refusal = %+v", failure)
	}
}

// 🔴 The recall answers INSIDE the plugin's deadline, and hands the platform a
// shorter one — at the wire's floor too, where a margin matters most.
func TestRecallAnswersInsideTheDeadline(t *testing.T) {
	host := &fakeHost{block: true}
	start := time.Now()
	_, err := handler(host, nil)[wire.OpMemoryRecall](context.Background(),
		json.RawMessage(`{"requestId":"`+request+`","query":"pump","deadlineMs":1500}`))
	if elapsed := time.Since(start); elapsed >= 1500*time.Millisecond {
		t.Fatalf("the recall took %s, past the plugin's 1500 ms deadline", elapsed)
	}
	failure := failureOfErr(t, err)
	if failure.Instruction != UnavailableInstruction || !strings.Contains(string(failure.Upstream), "platform_unavailable") {
		t.Fatalf("a missed deadline = %+v — it must say memory was not checked", failure)
	}
	var sent map[string]any
	_ = json.Unmarshal(host.body, &sent)
	if sent["deadlineMs"] != float64(1350) {
		t.Fatalf("the platform was handed deadlineMs %v, want 1350 (the plugin's 1500 less the margin)", sent["deadlineMs"])
	}
}

// A deadline under the wire's floor leaves no room for a margin, and is refused.
func TestRefusesARecallDeadlineUnderTheFloor(t *testing.T) {
	host := &fakeHost{}
	_, err := handler(host, nil)[wire.OpMemoryRecall](context.Background(),
		json.RawMessage(`{"requestId":"`+request+`","query":"pump","deadlineMs":400}`))
	if failureOfErr(t, err).Class != wire.ClassMalformedPayload || host.calls != 0 {
		t.Fatalf("a 400 ms recall = %v, %d call(s)", err, host.calls)
	}
}

// 🔴 A remember or forget whose answer never came back MAY have happened: the agent
// is told not to repeat it, never that memory "could not be checked".
func TestAWriteWithNoAnswerSaysNotToRepeatIt(t *testing.T) {
	for _, call := range []struct {
		op   string
		body string
	}{
		{wire.OpMemoryRemember, `{"requestId":"` + request + `","text":"pump"}`},
		{wire.OpMemoryForget, `{"requestId":"` + request + `","urn":"` + factURN + `"}`},
	} {
		host := &fakeHost{err: context.DeadlineExceeded}
		_, err := handler(host, nil)[call.op](context.Background(), json.RawMessage(call.body))
		if got := failureOfErr(t, err).Instruction; got != UncertainWriteInstruction {
			t.Fatalf("%s with no answer = %q", call.op, got)
		}
	}
	host := &fakeHost{err: context.DeadlineExceeded}
	_, err := handler(host, nil)[wire.OpMemoryUnfold](context.Background(),
		json.RawMessage(`{"requestId":"`+request+`","urn":"`+factURN+`"}`))
	if failureOfErr(t, err).Instruction != UnavailableInstruction {
		t.Fatal("a read with no answer must say memory could not be checked")
	}
}

func TestRefusesAnAnswerOffTheWiresShape(t *testing.T) {
	host := &fakeHost{answer: json.RawMessage(`{"hits":[{"urn":"` + factURN + `","level":"folder","folderId":"f1","distance":0.1,"text":"x","at":"t","source":{"kind":"remembered"}}],"degraded":false}`)}
	_, err := handler(host, nil)[wire.OpMemoryRecall](context.Background(),
		json.RawMessage(`{"requestId":"`+request+`","query":"pump"}`))
	if failureOfErr(t, err).Instruction != UnreadableInstruction {
		t.Fatalf("a hit carrying a folder id = %v", err)
	}
}

// 🔴 Facts, queries and hits never reach a log line.
func TestNeverLogsAPayload(t *testing.T) {
	var logs bytes.Buffer
	ok := &fakeHost{answer: json.RawMessage(`{"hits":[{"urn":"` + factURN + `","level":"conversation","distance":null,"text":"` + canary + `","at":"t","source":{"kind":"remembered"}}],"degraded":false}`)}
	_, _ = handler(ok, &logs)[wire.OpMemoryRecall](context.Background(),
		json.RawMessage(`{"requestId":"`+request+`","query":"`+canary+`"}`))
	failing := &fakeHost{err: classified{"platform_unavailable", "down"}}
	_, _ = handler(failing, &logs)[wire.OpMemoryRemember](context.Background(),
		json.RawMessage(`{"requestId":"`+request+`","text":"`+canary+`"}`))
	if logs.Len() == 0 || !strings.Contains(logs.String(), "memory op") {
		t.Fatal("nothing was logged — the assertion below would be vacuous")
	}
	if strings.Contains(logs.String(), canary) {
		t.Fatalf("a payload reached the log: %s", logs.String())
	}
}
