package surface

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	memtransport "github.com/uffici-ai/uffici-companion/internal/infra/transport/memory"
	"github.com/uffici-ai/uffici-companion/internal/wire"
)

// The call tunnel at the surface (#2028): the rules a single frame cannot show.

func call(callID, op string, deadlineMs int, payload map[string]any) map[string]any {
	frame := map[string]any{"v": 1, "type": "call", "callId": callID, "op": op, "deadlineMs": deadlineMs}
	if payload != nil {
		frame["payload"] = payload
	}
	return frame
}

// receiveResult reads the next frame, holds it to the contract, and returns it.
func receiveResult(t *testing.T, conn *memtransport.Conn) map[string]any {
	t.Helper()
	frame := receive(t, conn)
	if frame["type"] != "result" {
		t.Fatalf("expected a result, got %v", frame["type"])
	}
	raw, _ := json.Marshal(frame)
	// 🔴 The companion is held to the contract it enforces.
	if err := wire.Validate(raw); err != nil {
		t.Fatalf("the companion sent a result that fails its own schema: %v", err)
	}
	return frame
}

func errorClass(frame map[string]any) string {
	errObj, _ := frame["error"].(map[string]any)
	class, _ := errObj["class"].(string)
	return class
}

func stillOpen(t *testing.T, conn *memtransport.Conn) {
	t.Helper()
	select {
	case <-conn.Closed():
		t.Fatalf("the connection closed (%s); a call is never a close", conn.CloseReason())
	default:
	}
}

func TestAnUnknownOpIsAnsweredAndTheConnectionStaysOpen(t *testing.T) {
	r := newRig(t)
	conn := r.attach(t, testToken)
	receive(t, conn) // companion hello

	send(t, conn, call("call-1", "memory.recall", 5000, nil))
	result := receiveResult(t, conn)
	if result["callId"] != "call-1" || errorClass(result) != "unknown_op" {
		t.Fatalf("an unknown op must be answered unknown_op for its callId, got %v", result)
	}
	stillOpen(t, conn)
	// And the connection still works: a second call is answered too.
	send(t, conn, call("call-2", "memory.recall", 5000, nil))
	if second := receiveResult(t, conn); second["callId"] != "call-2" {
		t.Fatalf("the connection stopped answering after an unknown op: %v", second)
	}
}

func TestTwoCallsResolveIndependentlyAndOutOfOrder(t *testing.T) {
	release := make(chan struct{})
	r := newRigWith(t, Options{Calls: map[string]CallHandler{
		"slow": func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
			select {
			case <-release:
				return json.RawMessage(`{"which":"slow"}`), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
		"fast": func(context.Context, json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"which":"fast"}`), nil
		},
	}})
	conn := r.attach(t, testToken)
	receive(t, conn)

	send(t, conn, call("call-slow", "slow", 5000, nil))
	send(t, conn, call("call-fast", "fast", 5000, nil))

	first := receiveResult(t, conn)
	if first["callId"] != "call-fast" {
		t.Fatalf("the fast call waited behind the slow one: first result was %v", first["callId"])
	}
	close(release)
	second := receiveResult(t, conn)
	payload, _ := second["payload"].(map[string]any)
	if second["callId"] != "call-slow" || payload["which"] != "slow" {
		t.Fatalf("the slow call's result is wrong or lost: %v", second)
	}
}

func TestACallPastItsDeadlineIsAnsweredTimedOutAndTheRequestCarriesOn(t *testing.T) {
	r := newRigWith(t, Options{Calls: map[string]CallHandler{
		// 🔴 Ignores its context on purpose: the companion must answer BY the
		// deadline whatever the handler is doing.
		"hang": func(context.Context, json.RawMessage) (json.RawMessage, error) {
			select {}
		},
	}})
	conn := r.attach(t, testToken)
	receive(t, conn)

	ctx := context.Background()
	claim, err := r.host.OpenDoorbell(ctx, contracts.Handlers{
		OnRing:       func(ring contracts.Ring) { _ = r.manager.OnRing(ctx, ring) },
		OnCancel:     func(id string, reason contracts.CancelReason) { _ = r.manager.OnPlatformCancel(ctx, id, reason) },
		OnDisconnect: func(string) {},
	})
	if err != nil {
		t.Fatalf("OpenDoorbell: %v", err)
	}
	t.Cleanup(func() { _ = claim.Close() })
	if err := r.host.DeliverRing(contracts.Ring{
		RingID: "ring_1", RequestID: "req_1", ConversationID: "conv_1", Reason: "mentioned",
		Cause: contracts.Cause{EntryID: "e2", AuthorID: "m1"}, SinceCursor: "c0001", ThroughCursor: "c0002",
	}); err != nil {
		t.Fatalf("DeliverRing: %v", err)
	}
	if brief := receive(t, conn); brief["type"] != "brief" {
		t.Fatalf("expected a brief, got %v", brief["type"])
	}
	send(t, conn, map[string]any{"v": 1, "type": "ack", "requestId": "req_1", "outcome": "accepted", "session": "resumed"})

	started := time.Now()
	send(t, conn, call("call-hang", "hang", 60, nil))
	result := receiveResult(t, conn)
	if result["callId"] != "call-hang" || errorClass(result) != "timed_out" {
		t.Fatalf("a call past its deadline must be answered timed_out, got %v", result)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("answered after %v for a 60 ms deadline", elapsed)
	}

	// The request's own life is untouched: it still posts and closes answered.
	send(t, conn, map[string]any{"v": 1, "type": "post", "requestId": "req_1", "text": "the invoice is filed"})
	send(t, conn, map[string]any{"v": 1, "type": "done", "requestId": "req_1", "outcome": "answered"})
	deadline := time.Now().Add(2 * time.Second)
	for {
		offset, err := r.host.OffsetOf("conv_1")
		if err != nil {
			t.Fatalf("OffsetOf: %v", err)
		}
		if offset == "c0002" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the request did not close answered after a timed-out call (offset %q)", offset)
		}
		time.Sleep(5 * time.Millisecond)
	}
	stillOpen(t, conn)
}

func TestAnOversizedCallIsAnsweredMalformedPayload(t *testing.T) {
	r := newRig(t)
	conn := r.attach(t, testToken)
	receive(t, conn)

	send(t, conn, call("call-big", "memory.remember", 5000,
		map[string]any{"text": strings.Repeat("x", wire.MaxCallPayloadBytes)}))
	result := receiveResult(t, conn)
	if result["callId"] != "call-big" || errorClass(result) != "malformed_payload" {
		t.Fatalf("an oversized call must be answered malformed_payload, got %v", result)
	}
	stillOpen(t, conn)
}

func TestAHandlersOwnFailureIsRelayedAndAnyOtherIsUpstreamFailed(t *testing.T) {
	r := newRigWith(t, Options{Calls: map[string]CallHandler{
		"refused": func(context.Context, json.RawMessage) (json.RawMessage, error) {
			return nil, &CallFailure{Class: wire.ClassNotPermitted, Instruction: "Memory is off for this agent; tell the person.",
				Upstream: json.RawMessage(`{"class":"memory_disabled"}`)}
		},
		"broken": func(context.Context, json.RawMessage) (json.RawMessage, error) {
			return nil, errors.New("dial tcp 10.0.0.7:5432: the payload and the DSN are in here")
		},
		"empty": func(context.Context, json.RawMessage) (json.RawMessage, error) { return nil, nil },
		"huge": func(context.Context, json.RawMessage) (json.RawMessage, error) {
			return json.Marshal(map[string]string{"text": strings.Repeat("y", wire.MaxCallPayloadBytes)})
		},
	}})
	conn := r.attach(t, testToken)
	receive(t, conn)

	send(t, conn, call("c-refused", "refused", 5000, nil))
	refused := receiveResult(t, conn)
	errObj, _ := refused["error"].(map[string]any)
	upstream, _ := errObj["upstream"].(map[string]any)
	if errObj["class"] != "not_permitted" || errObj["instruction"] != "Memory is off for this agent; tell the person." || upstream["class"] != "memory_disabled" {
		t.Fatalf("a handler's typed failure must be relayed whole, got %v", refused)
	}

	send(t, conn, call("c-broken", "broken", 5000, nil))
	broken := receiveResult(t, conn)
	if errorClass(broken) != "upstream_failed" {
		t.Fatalf("an untyped handler error must be upstream_failed, got %v", broken)
	}
	if raw, _ := json.Marshal(broken); strings.Contains(string(raw), "10.0.0.7") {
		t.Fatalf("an untyped handler error leaked into the result: %s", raw)
	}

	send(t, conn, call("c-empty", "empty", 5000, nil))
	if empty := receiveResult(t, conn); empty["payload"] == nil {
		t.Fatalf("a success with no output must still carry a payload (exactly one of payload or error): %v", empty)
	}

	send(t, conn, call("c-huge", "huge", 5000, nil))
	if huge := receiveResult(t, conn); errorClass(huge) != "malformed_payload" {
		t.Fatalf("an op whose output breaks the bound must be answered malformed_payload, never truncated: %v", huge)
	}
}

func TestDetachingCancelsTheCallsInFlight(t *testing.T) {
	cancelled := make(chan struct{})
	r := newRigWith(t, Options{Calls: map[string]CallHandler{
		"wait": func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
			<-ctx.Done()
			close(cancelled)
			return nil, ctx.Err()
		},
	}})
	conn := r.attach(t, testToken)
	receive(t, conn)
	send(t, conn, call("c-wait", "wait", 300000, nil))
	time.Sleep(20 * time.Millisecond)
	_ = conn.Close("plugin going away")
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("a call kept running after its plugin detached")
	}
}

func TestAPluginSendingAResultIsRefused(t *testing.T) {
	r := newRig(t)
	conn := r.attach(t, testToken)
	receive(t, conn)
	send(t, conn, map[string]any{"v": 1, "type": "result", "callId": "c-1", "payload": map[string]any{}})
	if reason := waitClosed(t, conn); reason != string(wire.ReasonMalformedFrame) {
		t.Fatalf("a result is the companion's frame; closed with %q", reason)
	}
}

func TestACallWithNoCallIDCloses(t *testing.T) {
	r := newRig(t)
	conn := r.attach(t, testToken)
	receive(t, conn)
	send(t, conn, map[string]any{"v": 1, "type": "call", "op": "memory.recall", "deadlineMs": 5000})
	if reason := waitClosed(t, conn); reason != string(wire.ReasonMalformedFrame) {
		t.Fatalf("a call with nothing to echo cannot be answered; closed with %q", reason)
	}
}

func TestAResultThatDoesNotEncodeIsStillAnswered(t *testing.T) {
	r := newRigWith(t, Options{Calls: map[string]CallHandler{
		// Invalid JSON in a RawMessage makes json.Marshal fail.
		"garbled": func(context.Context, json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage("{"), nil
		},
	}})
	conn := r.attach(t, testToken)
	receive(t, conn)
	send(t, conn, call("c-garbled", "garbled", 5000, nil))
	result := receiveResult(t, conn)
	if result["callId"] != "c-garbled" || errorClass(result) != "upstream_failed" {
		t.Fatalf("a result that did not encode must still be answered upstream_failed, got %v", result)
	}
}

func TestAHandlerClassOutsideTheVocabularyIsUpstreamFailed(t *testing.T) {
	r := newRigWith(t, Options{Calls: map[string]CallHandler{
		"odd": func(context.Context, json.RawMessage) (json.RawMessage, error) {
			return nil, &CallFailure{Class: wire.CallErrorClass("rate_limited")}
		},
	}})
	conn := r.attach(t, testToken)
	receive(t, conn)
	send(t, conn, call("c-odd", "odd", 5000, nil))
	// receiveResult validates against the schema: an unknown class would fail it.
	if result := receiveResult(t, conn); errorClass(result) != "upstream_failed" {
		t.Fatalf("a class outside the closed five must become upstream_failed, got %v", result)
	}
}

func TestCallsPastTheInFlightBoundAreAnsweredAtOnce(t *testing.T) {
	release := make(chan struct{})
	r := newRigWith(t, Options{Calls: map[string]CallHandler{
		"block": func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
			select {
			case <-release:
			case <-ctx.Done():
			}
			return json.RawMessage(`{}`), nil
		},
	}})
	conn := r.attach(t, testToken)
	receive(t, conn)
	defer close(release)

	for i := 0; i < MaxCallsInFlight; i++ {
		send(t, conn, call(fmt.Sprintf("c-%d", i), "block", 300000, nil))
	}
	send(t, conn, call("c-over", "block", 300000, nil))
	result := receiveResult(t, conn)
	errObj, _ := result["error"].(map[string]any)
	if result["callId"] != "c-over" || errObj["class"] != "upstream_failed" || errObj["instruction"] != busyInstruction {
		t.Fatalf("the call past the bound must be answered at once, upstream_failed, busy: %v", result)
	}
	stillOpen(t, conn)
}

func TestAnOversizedResultFromThePluginClosesAsTheCompanionsFrame(t *testing.T) {
	r := newRig(t)
	conn := r.attach(t, testToken)
	receive(t, conn)
	send(t, conn, map[string]any{"v": 1, "type": "result", "callId": "c-1",
		"payload": map[string]any{"text": strings.Repeat("x", wire.MaxCallPayloadBytes)}})
	if reason := waitClosed(t, conn); reason != string(wire.ReasonMalformedFrame) {
		t.Fatalf("a result is the companion's frame, at any size; closed with %q", reason)
	}
}
