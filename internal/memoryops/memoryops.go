// Package memoryops serves the agent's memory on the local wire (#2385): the
// `memory.recall`, `memory.remember`, `memory.forget` and `memory.unfold` call ops,
// over the agents host.
//
// 🔴 A translator, like the tool ops beside it. The memory is the platform's —
// knowledge-api's, reached through the gateway on the request's conversation
// ticket; this package validates each payload against the wire's op schemas,
// keeps a recall inside the plugin's own deadline, and answers a failure with the
// platform's class and sentence. It decides nothing about what may be remembered.
//
// 🔴 Facts, queries and hits are tenant content. None reaches a log line: what is
// logged about an op is its name, the request, and the class.
package memoryops

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/surface"
	"github.com/uffici-ai/uffici-companion/internal/wire"
)

// The sentences this package relays — platform vocabulary, never tenant content.
const (
	// UnknownRequestInstruction answers an op naming a request the companion was
	// never rung for: nothing was sent.
	UnknownRequestInstruction = "This memory request names a request you are not answering, so it was not sent; " +
		"answer without memory."
	// UnavailableInstruction answers a missed deadline or a platform that could
	// not be reached — 🔴 never an empty recall that reads as "nothing remembered".
	UnavailableInstruction = "Memory was unavailable because of a system problem; " +
		"tell the person you could not check what was remembered."
	// UncertainWriteInstruction answers a remember or forget whose answer never came
	// back: it may have been done, and a repeat would store it twice.
	UncertainWriteInstruction = "The memory change may or may not have been saved because of a system problem; " +
		"do not repeat it, and tell the person it could not be confirmed."
	// UnreadableInstruction answers a platform answer that is not the op's shape.
	UnreadableInstruction = "The platform answered in a shape this companion does not understand; " +
		"tell the person something went wrong, and update the companion."
)

// RecallDefaultDeadline is a recall's whole budget when the plugin names none.
const RecallDefaultDeadline = 4 * time.Second

// recallMargin is what the companion keeps back from a recall's deadline: it hands
// the gateway a SHORTER deadline than it waits itself, so each hop answers before
// the one above it gives up.
const recallMargin = 150 * time.Millisecond

// Host is the part of the agents host this package needs.
type Host interface {
	Memory(ctx context.Context, op contracts.MemoryOp, requestID string, body json.RawMessage) (json.RawMessage, error)
}

// Handler serves the four ops.
type Handler struct {
	Host Host
	// Logger receives one line per op: the op, the request, the class. Nil discards.
	Logger *slog.Logger
}

// Handlers maps each wire op to its CallHandler — all four, or the feature is not
// announced.
func (h *Handler) Handlers() map[string]surface.CallHandler {
	return map[string]surface.CallHandler{
		wire.OpMemoryRecall:   h.serve(wire.OpMemoryRecall, contracts.MemoryRecall),
		wire.OpMemoryRemember: h.serve(wire.OpMemoryRemember, contracts.MemoryRemember),
		wire.OpMemoryForget:   h.serve(wire.OpMemoryForget, contracts.MemoryForget),
		wire.OpMemoryUnfold:   h.serve(wire.OpMemoryUnfold, contracts.MemoryUnfold),
	}
}

type envelope struct {
	RequestID  string `json:"requestId"`
	DeadlineMS int    `json:"deadlineMs"`
}

func (h *Handler) log() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.New(slog.DiscardHandler)
}

// serve is one op's CallHandler.
//
// 🔴 Sent at most once, never retried here: a remember is not idempotent.
func (h *Handler) serve(op string, memoryOp contracts.MemoryOp) surface.CallHandler {
	return func(ctx context.Context, payload json.RawMessage) (json.RawMessage, error) {
		if err := wire.ValidateOpPayload(op, wire.OpRequest, payload); err != nil {
			return nil, &surface.CallFailure{Class: wire.ClassMalformedPayload}
		}
		var request envelope
		if err := json.Unmarshal(payload, &request); err != nil {
			return nil, &surface.CallFailure{Class: wire.ClassMalformedPayload}
		}
		body := payload
		if memoryOp == contracts.MemoryRecall {
			deadline := RecallDefaultDeadline
			if request.DeadlineMS > 0 {
				deadline = time.Duration(request.DeadlineMS) * time.Millisecond
			}
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, deadline-recallMargin/3)
			defer cancel()
			body = withDeadline(payload, deadline-recallMargin)
		}
		answer, err := h.Host.Memory(ctx, memoryOp, request.RequestID, body)
		if err != nil {
			failure := failureOf(ctx, memoryOp, err)
			h.logOp(op, request.RequestID, string(failure.Class), classOf(ctx, err))
			return nil, failure
		}
		if len(answer) > wire.MaxCallPayloadBytes-256 {
			h.logOp(op, request.RequestID, string(wire.ClassUpstreamFailed), "result_too_large")
			return nil, &surface.CallFailure{Class: wire.ClassUpstreamFailed, Instruction: UnreadableInstruction}
		}
		if err := wire.ValidateOpPayload(op, wire.OpResult, answer); err != nil {
			h.logOp(op, request.RequestID, string(wire.ClassUpstreamFailed), "unreadable")
			return nil, &surface.CallFailure{Class: wire.ClassUpstreamFailed, Instruction: UnreadableInstruction}
		}
		h.logOp(op, request.RequestID, "", "")
		return answer, nil
	}
}

// withDeadline rewrites a recall's deadline to the shorter one the gateway gets.
func withDeadline(payload json.RawMessage, deadline time.Duration) json.RawMessage {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return payload
	}
	ms := max(int(deadline/time.Millisecond), 100)
	encoded, _ := json.Marshal(ms)
	fields["deadlineMs"] = encoded
	out, err := json.Marshal(fields)
	if err != nil {
		return payload
	}
	return out
}

// failureOf answers a host failure: a request never rung is `not_permitted` and
// nothing was sent; a missed deadline is memory unavailable — 🔴 or, for a write, a
// change that may have happened, which must not be repeated; anything else carries
// the platform's class and sentence.
func failureOf(ctx context.Context, op contracts.MemoryOp, err error) *surface.CallFailure {
	if errors.Is(err, contracts.ErrUnknownRequest) {
		return &surface.CallFailure{Class: wire.ClassNotPermitted, Instruction: UnknownRequestInstruction}
	}
	failure := &surface.CallFailure{Class: wire.ClassUpstreamFailed, Instruction: UnavailableInstruction}
	write := op == contracts.MemoryRemember || op == contracts.MemoryForget
	if write {
		failure.Instruction = UncertainWriteInstruction
	}
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
		failure.Upstream = mustJSON(map[string]string{"class": "platform_unavailable"})
		return failure
	}
	var classified contracts.ClassifiedError
	if errors.As(err, &classified) {
		if instruction := classified.ErrorInstruction(); instruction != "" {
			failure.Instruction = instruction
		}
		if class := classified.ErrorClass(); class != "" {
			failure.Upstream = mustJSON(map[string]string{"class": class})
		}
	}
	return failure
}

func classOf(ctx context.Context, err error) string {
	if errors.Is(err, contracts.ErrUnknownRequest) {
		return "unknown_request"
	}
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
		return "deadline"
	}
	var classified contracts.ClassifiedError
	if errors.As(err, &classified) {
		return classified.ErrorClass()
	}
	return "internal"
}

// logOp logs one op by name, request and class — 🔴 never its payload or answer.
func (h *Handler) logOp(op, requestID, class, upstream string) {
	attrs := []any{slog.String("op", op), slog.String("request", requestID)}
	if class == "" {
		h.log().Info("memory op answered", append(attrs, slog.String("class", "ok"))...)
		return
	}
	h.log().Warn("memory op failed", append(attrs, slog.String("class", class), slog.String("upstream", upstream))...)
}

func mustJSON(value map[string]string) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}
