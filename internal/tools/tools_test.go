package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/surface"
	"github.com/uffici-ai/uffici-companion/internal/wire"
)

const (
	argumentMarker = "argument-marker-do-not-log"
	resultMarker   = "result-marker-do-not-log"
)

// fakeHost is the agents host as this package sees it: a catalog per
// conversation, a result per call, and a count of every call made.
type fakeHost struct {
	mu        sync.Mutex
	tools     []contracts.Tool
	listErr   error
	result    contracts.ToolResult
	callErr   error
	lists     int
	calls     int
	lastCalls []string
}

func (f *fakeHost) ListTools(_ context.Context, _ string) ([]contracts.Tool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	return f.tools, f.listErr
}

func (f *fakeHost) CallTool(_ context.Context, requestID, name string, arguments json.RawMessage) (contracts.ToolResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastCalls = append(f.lastCalls, requestID+"/"+name+"/"+string(arguments))
	return f.result, f.callErr
}

type classified struct{ class, instruction string }

func (c classified) Error() string            { return "host: " + c.class }
func (c classified) ErrorClass() string       { return c.class }
func (c classified) ErrorInstruction() string { return c.instruction }

func boards() []contracts.Tool {
	return []contracts.Tool{{
		Name: "list_boards", Description: "List the boards.",
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`), Permission: "allow",
	}}
}

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

func newHandler(host *fakeHost) (*Handler, *clock, *bytes.Buffer) {
	logs := &bytes.Buffer{}
	c := &clock{now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	return &Handler{Host: host, Now: c.Now, Logger: slog.New(slog.NewJSONHandler(logs, nil))}, c, logs
}

func failureOf(t *testing.T, err error) *surface.CallFailure {
	t.Helper()
	var failure *surface.CallFailure
	if !errors.As(err, &failure) {
		t.Fatalf("the error %v is not a call failure", err)
	}
	return failure
}

func TestListAnswersTheCatalogInTheOpsShape(t *testing.T) {
	host := &fakeHost{tools: boards()}
	h, _, _ := newHandler(host)
	payload, err := h.List(context.Background(), json.RawMessage(`{"conversationId":"conv_1"}`))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if err := wire.ValidateOpPayload(wire.OpToolsList, wire.OpResult, payload); err != nil {
		t.Fatalf("the answer fails tools.list's result schema: %v", err)
	}
	if !strings.Contains(string(payload), `"name":"list_boards"`) {
		t.Fatalf("List = %s", payload)
	}
}

func TestAnEmptyCatalogIsAnEmptyList(t *testing.T) {
	h, _, _ := newHandler(&fakeHost{})
	payload, err := h.List(context.Background(), json.RawMessage(`{"conversationId":"conv_1"}`))
	if err != nil || string(payload) != `{"tools":[]}` {
		t.Fatalf("List = %s, %v", payload, err)
	}
}

func TestAToolWithNoParametersIsGivenAnEmptyObjectSchema(t *testing.T) {
	h, _, _ := newHandler(&fakeHost{tools: []contracts.Tool{{Name: "ping", Permission: "allow"}}})
	payload, err := h.List(context.Background(), json.RawMessage(`{"conversationId":"conv_1"}`))
	if err != nil || !strings.Contains(string(payload), `"parameters":{"type":"object","properties":{}}`) {
		t.Fatalf("List = %s, %v", payload, err)
	}
}

// 🔴 The catalog is cached per conversation for CatalogTTL: a plugin lists on
// every turn, and each list would otherwise be a platform read.
func TestTheCatalogIsCachedPerConversationForAMinute(t *testing.T) {
	host := &fakeHost{tools: boards()}
	h, c, _ := newHandler(host)
	list := func(conversation string) {
		t.Helper()
		if _, err := h.List(context.Background(), json.RawMessage(`{"conversationId":"`+conversation+`"}`)); err != nil {
			t.Fatalf("List: %v", err)
		}
	}
	list("conv_1")
	c.now = c.now.Add(CatalogTTL - time.Second)
	list("conv_1")
	if host.lists != 1 {
		t.Fatalf("%d platform reads inside the minute, want 1", host.lists)
	}
	list("conv_2")
	if host.lists != 2 {
		t.Fatalf("another conversation was served this one's catalog (%d reads)", host.lists)
	}
	c.now = c.now.Add(time.Second)
	list("conv_1")
	if host.lists != 3 {
		t.Fatalf("a catalog a minute old was served from memory (%d reads)", host.lists)
	}
}

func TestAFailedListIsNotCachedAndCarriesThePlatformsSentence(t *testing.T) {
	host := &fakeHost{listErr: classified{"platform_unavailable", "The platform is unavailable; try again."}}
	h, _, _ := newHandler(host)
	_, err := h.List(context.Background(), json.RawMessage(`{"conversationId":"conv_1"}`))
	failure := failureOf(t, err)
	if failure.Class != wire.ClassUpstreamFailed || failure.Instruction != "The platform is unavailable; try again." ||
		string(failure.Upstream) != `{"class":"platform_unavailable"}` {
		t.Fatalf("failure = %+v", failure)
	}
	host.listErr, host.tools = nil, boards()
	if _, err := h.List(context.Background(), json.RawMessage(`{"conversationId":"conv_1"}`)); err != nil {
		t.Fatalf("List after recovery: %v", err)
	}
	if host.lists != 2 {
		t.Fatalf("%d reads, want 2 — a failure was cached", host.lists)
	}
}

func TestAnUnclassifiedFailureIsUpstreamFailedWithNoUpstream(t *testing.T) {
	h, _, _ := newHandler(&fakeHost{listErr: errors.New("boom")})
	_, err := h.List(context.Background(), json.RawMessage(`{"conversationId":"conv_1"}`))
	failure := failureOf(t, err)
	if failure.Class != wire.ClassUpstreamFailed || failure.Upstream != nil {
		t.Fatalf("failure = %+v", failure)
	}
}

func TestAMalformedRequestIsMalformedPayload(t *testing.T) {
	h, _, _ := newHandler(&fakeHost{})
	for name, call := range map[string]func() error{
		"list without a conversation": func() error { _, err := h.List(context.Background(), json.RawMessage(`{}`)); return err },
		"list not json":               func() error { _, err := h.List(context.Background(), json.RawMessage(`nope`)); return err },
		"call without arguments": func() error {
			_, err := h.Call(context.Background(), json.RawMessage(`{"requestId":"r","name":"list_boards"}`))
			return err
		},
		"call not json": func() error { _, err := h.Call(context.Background(), json.RawMessage(`[`)); return err },
	} {
		t.Run(name, func(t *testing.T) {
			if failure := failureOf(t, call()); failure.Class != wire.ClassMalformedPayload {
				t.Fatalf("class %s, want malformed_payload", failure.Class)
			}
		})
	}
}

// A catalog the platform sent off the op's shape is never handed on: the
// plugin would refuse it, and the agent would hear nothing useful.
func TestAnOffContractCatalogIsUpstreamFailed(t *testing.T) {
	h, _, _ := newHandler(&fakeHost{tools: []contracts.Tool{{Name: "x", Permission: "deny"}}})
	_, err := h.List(context.Background(), json.RawMessage(`{"conversationId":"conv_1"}`))
	if failure := failureOf(t, err); failure.Class != wire.ClassUpstreamFailed || failure.Instruction != UnreadableInstruction {
		t.Fatalf("failure = %+v", failure)
	}
}

func TestACatalogOverTheBoundAsksForLess(t *testing.T) {
	huge := strings.Repeat("d", 15000)
	var many []contracts.Tool
	for i := range 40 {
		many = append(many, contracts.Tool{Name: fmt.Sprintf("tool_%d", i), Description: huge,
			Parameters: json.RawMessage(`{}`), Permission: "allow"})
	}
	h, _, _ := newHandler(&fakeHost{tools: many})
	_, err := h.List(context.Background(), json.RawMessage(`{"conversationId":"conv_1"}`))
	if failure := failureOf(t, err); failure.Class != wire.ClassUpstreamFailed || failure.Instruction != TooLargeInstruction {
		t.Fatalf("failure = %+v", failure)
	}
}

func TestACallAnswersTheResultAndItsURN(t *testing.T) {
	host := &fakeHost{result: contracts.ToolResult{
		CallURN: "urn:uffici:tool_call:42", Result: json.RawMessage(`{"boards":[{"id":"b-1","name":"Support"}]}`),
	}}
	h, _, logs := newHandler(host)
	payload, err := h.Call(context.Background(),
		json.RawMessage(`{"requestId":"req_1","name":"list_boards","arguments":{"q":"`+argumentMarker+`"}}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if err := wire.ValidateOpPayload(wire.OpToolsCall, wire.OpResult, payload); err != nil {
		t.Fatalf("the answer fails tools.call's result schema: %v", err)
	}
	if string(payload) != `{"callUrn":"urn:uffici:tool_call:42","result":{"boards":[{"id":"b-1","name":"Support"}]}}` {
		t.Fatalf("Call = %s", payload)
	}
	if host.calls != 1 || !strings.Contains(host.lastCalls[0], argumentMarker) {
		t.Fatalf("the host saw %v", host.lastCalls)
	}
	line := logs.String()
	if !strings.Contains(line, `"tool":"list_boards"`) || !strings.Contains(line, `"class":"ok"`) {
		t.Fatalf("the call's line names no tool and class: %s", line)
	}
	if strings.Contains(line, argumentMarker) || strings.Contains(line, "Support") {
		t.Fatalf("a log line carries a tool's arguments or result: %s", line)
	}
}

func TestACallThatDidNotRunAnswersANullURN(t *testing.T) {
	h, _, _ := newHandler(&fakeHost{result: contracts.ToolResult{
		Result: json.RawMessage(`{"ok":false,"error":"needs_approval","message":"A person must approve this tool."}`),
	}})
	payload, err := h.Call(context.Background(), json.RawMessage(`{"requestId":"req_1","name":"delete_board","arguments":{}}`))
	if err != nil || !strings.HasPrefix(string(payload), `{"callUrn":null,`) {
		t.Fatalf("Call = %s, %v", payload, err)
	}
}

// 🔴 A request the companion was never rung for is NOT PERMITTED, with a
// sentence — and this package adds no call of its own: the host refused it
// before anything left (the gateway's guard is pinned in its own tests).
func TestACallOnARequestNeverRungIsNotPermitted(t *testing.T) {
	host := &fakeHost{callErr: contracts.ErrUnknownRequest}
	h, _, logs := newHandler(host)
	_, err := h.Call(context.Background(), json.RawMessage(`{"requestId":"req_x","name":"list_boards","arguments":{}}`))
	failure := failureOf(t, err)
	if failure.Class != wire.ClassNotPermitted || failure.Instruction != UnknownRequestInstruction {
		t.Fatalf("failure = %+v", failure)
	}
	if host.calls != 1 {
		t.Fatalf("%d host calls for one tool call — a refusal must never be retried", host.calls)
	}
	if !strings.Contains(logs.String(), `"upstream":"unknown_request"`) {
		t.Fatalf("the refusal's line: %s", logs.String())
	}
}

func TestAPlatformFailureCarriesItsClassAndSentenceAndIsSentOnce(t *testing.T) {
	host := &fakeHost{callErr: classified{"request_closed", "This request is already over; stop."}}
	h, _, logs := newHandler(host)
	_, err := h.Call(context.Background(), json.RawMessage(`{"requestId":"req_1","name":"list_boards","arguments":{"q":"`+argumentMarker+`"}}`))
	failure := failureOf(t, err)
	if failure.Class != wire.ClassUpstreamFailed || failure.Instruction != "This request is already over; stop." ||
		string(failure.Upstream) != `{"class":"request_closed"}` {
		t.Fatalf("failure = %+v", failure)
	}
	if host.calls != 1 {
		t.Fatalf("%d host calls — a tool call is never retried", host.calls)
	}
	if strings.Contains(logs.String(), argumentMarker) {
		t.Fatalf("a log line carries the arguments: %s", logs.String())
	}
}

// 🔴 A result over the call bound: the tool RAN, so the failure says so, tells
// the agent to ask for less, and keeps the URN so the call can still be cited.
func TestAResultOverTheBoundAsksForLessAndKeepsTheURN(t *testing.T) {
	big := `{"rows":"` + strings.Repeat(resultMarker, wire.MaxCallPayloadBytes/len(resultMarker)) + `"}`
	h, _, logs := newHandler(&fakeHost{result: contracts.ToolResult{CallURN: "urn:uffici:tool_call:9", Result: json.RawMessage(big)}})
	_, err := h.Call(context.Background(), json.RawMessage(`{"requestId":"req_1","name":"list_rows","arguments":{}}`))
	failure := failureOf(t, err)
	if failure.Class != wire.ClassUpstreamFailed || failure.Instruction != TooLargeInstruction ||
		string(failure.Upstream) != `{"callUrn":"urn:uffici:tool_call:9","class":"result_too_large"}` {
		t.Fatalf("failure = %+v", failure)
	}
	if strings.Contains(logs.String(), resultMarker) {
		t.Fatal("a log line carries the oversized result")
	}
}

func TestAResultOverTheBoundWithNoURNHasNoUpstream(t *testing.T) {
	big := `{"rows":"` + strings.Repeat("x", wire.MaxCallPayloadBytes) + `"}`
	h, _, _ := newHandler(&fakeHost{result: contracts.ToolResult{Result: json.RawMessage(big)}})
	_, err := h.Call(context.Background(), json.RawMessage(`{"requestId":"req_1","name":"list_rows","arguments":{}}`))
	if failure := failureOf(t, err); failure.Upstream != nil || failure.Instruction != TooLargeInstruction {
		t.Fatalf("failure = %+v", failure)
	}
}

// A result that is not JSON cannot be encoded into the frame at all.
func TestAResultThatIsNotJSONIsUpstreamFailed(t *testing.T) {
	h, _, _ := newHandler(&fakeHost{result: contracts.ToolResult{Result: json.RawMessage(`{nope`)}})
	_, err := h.Call(context.Background(), json.RawMessage(`{"requestId":"req_1","name":"list_rows","arguments":{}}`))
	if failure := failureOf(t, err); failure.Class != wire.ClassUpstreamFailed || failure.Instruction != UnreadableInstruction {
		t.Fatalf("failure = %+v", failure)
	}
}

// The cache is bounded: past catalogMemory conversations the oldest goes, and an
// expired one goes first.
func TestTheCatalogCacheIsBounded(t *testing.T) {
	host := &fakeHost{tools: boards()}
	h, c, _ := newHandler(host)
	for i := range catalogMemory + 5 {
		c.now = c.now.Add(time.Millisecond)
		if _, err := h.List(context.Background(), json.RawMessage(fmt.Sprintf(`{"conversationId":"conv_%d"}`, i))); err != nil {
			t.Fatal(err)
		}
	}
	if len(h.cache) != catalogMemory {
		t.Fatalf("%d cached catalogs, want the bound %d", len(h.cache), catalogMemory)
	}
	if _, kept := h.cache["conv_0"]; kept {
		t.Fatal("the oldest catalog outlived the bound")
	}
	c.now = c.now.Add(CatalogTTL)
	if _, err := h.List(context.Background(), json.RawMessage(`{"conversationId":"conv_new"}`)); err != nil {
		t.Fatal(err)
	}
	if len(h.cache) != 1 {
		t.Fatalf("%d cached catalogs after every other expired, want 1", len(h.cache))
	}
}

func TestDefaultsNeedNoClockOrLogger(t *testing.T) {
	h := &Handler{Host: &fakeHost{result: contracts.ToolResult{Result: json.RawMessage(`{}`)}}}
	if _, err := h.Call(context.Background(), json.RawMessage(`{"requestId":"r","name":"n","arguments":{}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.List(context.Background(), json.RawMessage(`{"conversationId":"c"}`)); err != nil {
		t.Fatal(err)
	}
}
