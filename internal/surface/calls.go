package surface

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/wire"
)

// CallHandler serves one `call` op (#2028): the payload in, the result payload
// out. It must honour ctx — the call's own deadline, and the plugin detaching.
//
// A returned *CallFailure is answered with its class; any other error is
// answered `upstream_failed`, and only its classification reaches the log.
type CallHandler func(ctx context.Context, payload json.RawMessage) (json.RawMessage, error)

// CallFailure is a handler's typed refusal: the wire class, the sentence the
// agent relays, and the control plane's own error when there is one.
type CallFailure struct {
	Class       wire.CallErrorClass
	Instruction string
	Upstream    json.RawMessage
}

func (f *CallFailure) Error() string { return "call failed: " + string(f.Class) }

// MaxCallsInFlight bounds one connection's concurrent calls.
//
// 🔴 Every served call is a request the companion makes to the platform with
// the agent's key, so an unbounded plugin would make the companion an
// amplifier. A slot is held until the HANDLER returns — not merely until the
// call is answered — so a handler that ignores its context stays bounded too.
const MaxCallsInFlight = 32

// busyInstruction is what a call over MaxCallsInFlight relays. The error
// vocabulary is closed, so "busy" is an upstream_failed with its own sentence.
const busyInstruction = "Too many platform requests are already in progress; tell the person, and try again shortly."

// The sentence each class relays when the failure carries none of its own —
// platform vocabulary, never tenant content (decision 32).
//
// 🔴 The plugin carries the same sentences (`call_contract.py`), pinned from the
// platform repository by `scripts/__tests__/uffici-call-instructions-parity.test.mjs`:
// the person hears the same thing whichever side answered the call.
var defaultInstructions = map[wire.CallErrorClass]string{
	wire.ClassUnknownOp:        "This action is not available on this connection; tell the person it could not be done.",
	wire.ClassNotPermitted:     "This action is not permitted for this agent; tell the person it could not be done.",
	wire.ClassTimedOut:         "The platform did not answer in time; tell the person, and try again shortly.",
	wire.ClassUpstreamFailed:   "The platform could not complete this; tell the person, and try again shortly.",
	wire.ClassMalformedPayload: "The request was too large or could not be read; tell the person it could not be done.",
}

// startCall answers one call off the read loop, so calls run concurrently and
// their results go out in whatever order they finish.
//
// 🔴 The payload never reaches a log line — not the call's, not the result's:
// they carry transcript content and memory rows. The one line per call names
// the op, the callId, byte counts and the class.
func (s *Surface) startCall(callCtx context.Context, connID string, sink *frameSink, call *wire.Call, requestBytes int, slots chan struct{}) {
	handler, served := s.calls[call.Op]
	if !served {
		// An answer, never a close: the connection and every other call on it stay up.
		s.answerCall(callCtx, connID, sink, call, requestBytes, 0, nil, &CallFailure{Class: wire.ClassUnknownOp})
		return
	}
	select {
	case slots <- struct{}{}:
	default:
		s.answerCall(callCtx, connID, sink, call, requestBytes, 0, nil,
			&CallFailure{Class: wire.ClassUpstreamFailed, Instruction: busyInstruction})
		return
	}
	go func() {
		started := time.Now()
		opCtx, cancel := context.WithTimeout(callCtx, time.Duration(call.DeadlineMs)*time.Millisecond)
		defer cancel()

		type outcome struct {
			payload json.RawMessage
			err     error
		}
		done := make(chan outcome, 1)
		go func() {
			defer func() { <-slots }()
			payload, err := handler(opCtx, call.Payload)
			done <- outcome{payload, err}
		}()

		select {
		case out := <-done:
			s.answerCall(callCtx, connID, sink, call, requestBytes, time.Since(started), out.payload, out.err)
		case <-opCtx.Done():
			if callCtx.Err() != nil {
				// The plugin detached or the companion is stopping: nobody is left
				// to answer, and the plugin fails its own pending calls.
				return
			}
			// 🔴 The companion answers BY the deadline, whatever the handler is
			// doing — a call can never hang the turn that made it.
			s.answerCall(callCtx, connID, sink, call, requestBytes, time.Since(started), nil, &CallFailure{Class: wire.ClassTimedOut})
		}
	}()
}

// answerMalformed answers a call-shaped frame the decoder refused over its bound.
func (s *Surface) answerMalformed(ctx context.Context, connID string, sink *frameSink, callErr *wire.CallError, requestBytes int) {
	call := &wire.Call{CallID: callErr.CallID}
	s.answerCall(ctx, connID, sink, call, requestBytes, 0, nil, &CallFailure{Class: wire.ClassMalformedPayload})
}

// answerCall writes the one result for a call and logs it — ids, sizes, class.
func (s *Surface) answerCall(ctx context.Context, connID string, sink *frameSink, call *wire.Call,
	requestBytes int, elapsed time.Duration, payload json.RawMessage, err error,
) {
	result := resultFor(call.CallID, payload, err)
	raw, marshalErr := json.Marshal(result)
	if marshalErr == nil && len(raw) > wire.MaxCallPayloadBytes {
		// 🔴 The bound holds in BOTH directions: an op whose output does not fit
		// is answered malformed_payload, never truncated.
		result = resultFor(call.CallID, nil, &CallFailure{Class: wire.ClassMalformedPayload})
		raw, marshalErr = json.Marshal(result)
	}
	encodingFailed := marshalErr != nil
	if encodingFailed {
		// 🔴 Never silence: a handler that returned JSON that does not encode
		// still owes the plugin an answer by the deadline. The fallback carries
		// nothing the handler produced, so it always encodes.
		result = resultFor(call.CallID, nil, &CallFailure{Class: wire.ClassUpstreamFailed})
		raw, marshalErr = json.Marshal(result)
	}
	class := "ok"
	if result.Error != nil {
		class = result.Error.Class
	}
	attrs := []any{
		slog.String("conn", connID),
		slog.String("op", call.Op),
		slog.String("callId", call.CallID),
		slog.String("class", class),
		slog.Int("requestBytes", requestBytes),
		slog.Int("resultBytes", len(raw)),
		slog.Int64("elapsedMs", elapsed.Milliseconds()),
	}
	if encodingFailed {
		s.log.Error("a call result did not encode; answered upstream_failed", attrs...)
	}
	if marshalErr != nil {
		// Unreachable: the fallback holds only strings the companion wrote.
		s.log.Error("encoding a call result", attrs...)
		return
	}
	if sendErr := sink.sendRaw(ctx, raw); sendErr != nil {
		s.log.Warn("sending a call result", append(attrs, slog.String("error", classify(sendErr)))...)
		return
	}
	s.log.Info("call answered", attrs...)
}

// resultFor builds the result frame: the payload on success, the class and its
// sentence on failure.
func resultFor(callID string, payload json.RawMessage, err error) wire.Result {
	result := wire.Result{Envelope: wire.Envelope{V: wire.Version, Type: "result"}, CallID: callID}
	if err == nil {
		if len(payload) == 0 {
			payload = json.RawMessage("{}")
		}
		result.Payload = payload
		return result
	}
	failure := CallFailure{Class: wire.ClassUpstreamFailed}
	var typed *CallFailure
	if errors.As(err, &typed) {
		failure = *typed
	}
	if _, known := defaultInstructions[failure.Class]; !known {
		// The vocabulary is closed: a class outside it would produce a result
		// the plugin must refuse. What it meant is "the operation failed".
		failure = CallFailure{Class: wire.ClassUpstreamFailed, Upstream: failure.Upstream}
	}
	instruction := failure.Instruction
	if instruction == "" {
		instruction = defaultInstructions[failure.Class]
	}
	result.Error = &wire.ResultError{Class: string(failure.Class), Instruction: instruction, Upstream: failure.Upstream}
	return result
}
