// Package tools serves the agent's Uffici tools on the local wire (#2349): the
// `tools.list` and `tools.call` call ops, over the agents host.
//
// 🔴 A translator, like the host beneath it. The catalog is the platform's and
// the result is the platform's; this package validates both against the wire's
// op schemas, keeps them inside the call bound, and answers a failure with the
// platform's class and sentence. It decides nothing about what a tool may do.
//
// 🔴 Arguments and results are tenant content. Neither reaches a log line: what
// is logged about a call is the tool's name, the request, and the class.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/surface"
	"github.com/uffici-ai/uffici-companion/internal/wire"
)

// CatalogTTL is how long one conversation's catalog is served from memory: a
// plugin lists tools on every turn, and the catalog changes when a person
// changes the agent's tools — rarely, and a minute late is acceptable.
const CatalogTTL = 60 * time.Second

// catalogMemory bounds how many conversations' catalogs are cached at once.
const catalogMemory = 256

// resultSlack is the room a result frame's envelope needs around the payload
// (`v`, `type`, a 64-character `callId`), kept free so a payload that fits here
// always fits the frame.
const resultSlack = 256

// The sentences this package relays — platform vocabulary, never tenant content.
const (
	// UnknownRequestInstruction answers a call naming a request the companion
	// was never rung for.
	UnknownRequestInstruction = "This tool call names a request you are not answering, so it was not sent; " +
		"tell the person it could not be done."
	// TooLargeInstruction answers a result over the call bound. The tool RAN.
	TooLargeInstruction = "The tool ran, but its answer was too large to hand over; if you call it again, " +
		"ask for less — a narrower query or fewer results."
	// UnreadableInstruction answers a platform answer that is not the op's shape.
	UnreadableInstruction = "The platform answered in a shape this companion does not understand; " +
		"tell the person something went wrong, and update the companion."
)

// Host is the part of the agents host this package needs.
type Host interface {
	ListTools(ctx context.Context, conversationID string) ([]contracts.Tool, error)
	CallTool(ctx context.Context, requestID, name string, arguments json.RawMessage) (contracts.ToolResult, error)
}

// Handler serves the two ops.
type Handler struct {
	Host Host
	// Logger receives one line per tool call: the tool, the request, the class.
	// Nil discards.
	Logger *slog.Logger
	// Now is the cache's clock; nil is time.Now.
	Now func() time.Time

	mu    sync.Mutex
	cache map[string]cachedCatalog
	// last is the size of the most recently read catalog; listed says one was.
	last   int
	listed bool
}

type cachedCatalog struct {
	payload json.RawMessage
	at      time.Time
}

type listRequest struct {
	ConversationID string `json:"conversationId"`
}

type listResult struct {
	Tools []listedTool `json:"tools"`
}

type listedTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Permission  string          `json:"permission"`
}

type callRequest struct {
	RequestID string          `json:"requestId"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type callResult struct {
	CallURN *string         `json:"callUrn"`
	Result  json.RawMessage `json:"result"`
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *Handler) log() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.New(slog.DiscardHandler)
}

// List is the surface's CallHandler for `tools.list`.
func (h *Handler) List(ctx context.Context, payload json.RawMessage) (json.RawMessage, error) {
	if err := wire.ValidateOpPayload(wire.OpToolsList, wire.OpRequest, payload); err != nil {
		return nil, &surface.CallFailure{Class: wire.ClassMalformedPayload}
	}
	var request listRequest
	if err := json.Unmarshal(payload, &request); err != nil {
		return nil, &surface.CallFailure{Class: wire.ClassMalformedPayload}
	}
	if cached, ok := h.cached(request.ConversationID); ok {
		return cached, nil
	}
	tools, err := h.Host.ListTools(ctx, request.ConversationID)
	if err != nil {
		return nil, upstreamFailure(err)
	}
	result := listResult{Tools: make([]listedTool, 0, len(tools))}
	for _, tool := range tools {
		parameters := tool.Parameters
		if len(parameters) == 0 {
			parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		result.Tools = append(result.Tools, listedTool{
			Name: tool.Name, Description: tool.Description, Parameters: parameters, Permission: tool.Permission,
		})
	}
	encoded, err := bounded(wire.OpToolsList, result)
	if err != nil {
		return nil, err
	}
	h.remember(request.ConversationID, encoded, len(result.Tools))
	return encoded, nil
}

// CatalogSize is how many tools the most recently READ catalog held, and
// whether one has been read at all — what `uffici doctor` reports.
func (h *Handler) CatalogSize() (int, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.last, h.listed
}

// Call is the surface's CallHandler for `tools.call`.
//
// 🔴 Sent at most once, never retried here: a tool is not idempotent, and the
// host already repeats only what happened before the platform could run it.
func (h *Handler) Call(ctx context.Context, payload json.RawMessage) (json.RawMessage, error) {
	if err := wire.ValidateOpPayload(wire.OpToolsCall, wire.OpRequest, payload); err != nil {
		return nil, &surface.CallFailure{Class: wire.ClassMalformedPayload}
	}
	var request callRequest
	if err := json.Unmarshal(payload, &request); err != nil {
		return nil, &surface.CallFailure{Class: wire.ClassMalformedPayload}
	}
	answer, err := h.Host.CallTool(ctx, request.RequestID, request.Name, request.Arguments)
	if err != nil {
		failure := h.callFailure(err)
		h.logCall(request, failure.Class, classOf(err))
		return nil, failure
	}
	result := callResult{Result: answer.Result}
	if answer.CallURN != "" {
		urn := answer.CallURN
		result.CallURN = &urn
	}
	encoded, err := bounded(wire.OpToolsCall, result)
	if err != nil {
		var failure *surface.CallFailure
		if errors.As(err, &failure) && result.CallURN != nil {
			// The call ran: its URN rides the failure, so the agent can still cite it.
			failure.Upstream = mustJSON(map[string]string{"class": "result_too_large", "callUrn": *result.CallURN})
		}
		h.logCall(request, wire.ClassUpstreamFailed, "result_too_large")
		return nil, err
	}
	h.logCall(request, "", "")
	return encoded, nil
}

// callFailure answers a host failure: a request the companion was never rung
// for is `not_permitted`, and nothing was sent; anything else is the platform's.
func (h *Handler) callFailure(err error) *surface.CallFailure {
	if errors.Is(err, contracts.ErrUnknownRequest) {
		return &surface.CallFailure{Class: wire.ClassNotPermitted, Instruction: UnknownRequestInstruction}
	}
	return upstreamFailure(err)
}

// logCall logs one tool call by name, request and class — 🔴 never its
// arguments or its result.
func (h *Handler) logCall(request callRequest, class wire.CallErrorClass, upstream string) {
	attrs := []any{slog.String("tool", request.Name), slog.String("request", request.RequestID)}
	if class == "" {
		h.log().Info("tool call answered", append(attrs, slog.String("class", "ok"))...)
		return
	}
	h.log().Warn("tool call failed", append(attrs, slog.String("class", string(class)),
		slog.String("upstream", upstream))...)
}

// upstreamFailure carries the platform's class and sentence onto the wire.
func upstreamFailure(err error) *surface.CallFailure {
	failure := &surface.CallFailure{Class: wire.ClassUpstreamFailed}
	var classified contracts.ClassifiedError
	if errors.As(err, &classified) {
		failure.Instruction = classified.ErrorInstruction()
		if class := classified.ErrorClass(); class != "" {
			failure.Upstream = mustJSON(map[string]string{"class": class})
		}
	}
	return failure
}

func classOf(err error) string {
	if errors.Is(err, contracts.ErrUnknownRequest) {
		return "unknown_request"
	}
	var classified contracts.ClassifiedError
	if errors.As(err, &classified) {
		return classified.ErrorClass()
	}
	return "internal"
}

// bounded encodes an op's result, checks it against the op's result schema and
// keeps it inside the call bound. 🔴 An answer over the bound is a failure with
// a sentence telling the agent to ask for less — never a truncation, and never
// the surface's `malformed_payload`, which would blame the plugin.
func bounded(op string, result any) (json.RawMessage, error) {
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, &surface.CallFailure{Class: wire.ClassUpstreamFailed, Instruction: UnreadableInstruction}
	}
	if len(encoded) > wire.MaxCallPayloadBytes-resultSlack {
		return nil, &surface.CallFailure{Class: wire.ClassUpstreamFailed, Instruction: TooLargeInstruction}
	}
	if err := wire.ValidateOpPayload(op, wire.OpResult, encoded); err != nil {
		return nil, &surface.CallFailure{Class: wire.ClassUpstreamFailed, Instruction: UnreadableInstruction}
	}
	return encoded, nil
}

func (h *Handler) cached(conversationID string) (json.RawMessage, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	entry, ok := h.cache[conversationID]
	if !ok || h.now().Sub(entry.at) >= CatalogTTL {
		return nil, false
	}
	return entry.payload, true
}

func (h *Handler) remember(conversationID string, payload json.RawMessage, size int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.last, h.listed = size, true
	if h.cache == nil {
		h.cache = map[string]cachedCatalog{}
	}
	now := h.now()
	if len(h.cache) >= catalogMemory {
		oldestID, oldest := "", now
		for id, entry := range h.cache {
			if now.Sub(entry.at) >= CatalogTTL {
				delete(h.cache, id)
				continue
			}
			if !entry.at.After(oldest) {
				oldestID, oldest = id, entry.at
			}
		}
		if len(h.cache) >= catalogMemory {
			delete(h.cache, oldestID)
		}
	}
	h.cache[conversationID] = cachedCatalog{payload: payload, at: now}
}

func mustJSON(value map[string]string) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}
