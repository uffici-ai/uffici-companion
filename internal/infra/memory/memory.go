// Package memory is an AgentsHost held entirely in memory, seeded by its
// caller rather than by a script.
//
// 🔴 Its only job is to prove the contract suite is a suite and not a second
// description of the fixture. A suite written against one implementation drifts
// into testing that implementation's shape — its script format, its validation,
// its file loading — and then passes for the real gateway host on the day it
// arrives while asserting nothing about it. Two implementations from one table
// is what keeps the assertions about the CONTRACT.
//
// It is deliberately the dumbest thing that can satisfy the interface: no
// script, no validation, no files. Where it and the fixture agree, the suite is
// testing the contract; where they would have to differ, the suite has no
// business asserting.
package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/infra/logsim"
)

// Host is an in-memory AgentsHost.
type Host struct {
	// Recorder is the log side (#2264), shared with the fixture.
	*logsim.Recorder

	mu       sync.Mutex
	handlers contracts.Handlers
	open     bool

	conversations map[string]*conversation
	requests      map[string]openRequest
	acked         []string
	posts         int
	questions     []contracts.Question
	openerReads   int
	deltaReads    int
	// The budgets every read was given, in order (#2331).
	deltaBudgets  []int
	openerBudgets []int
	// rung is every request a ring delivered, never forgotten at close (#2349).
	rung      map[string]string
	tools     map[string][]contracts.Tool
	answers   map[string]contracts.ToolResult
	toolCalls []ToolCall
	extras    []contracts.PostExtras
}

// ToolCall is one recorded tool call (#2349).
type ToolCall struct {
	RequestID string
	Name      string
	Arguments json.RawMessage
}

type conversation struct {
	offset  string
	entries []contracts.Entry
	context *contracts.Context
}

type openRequest struct {
	conversationID string
	throughCursor  string
}

// New returns an empty host.
func New() *Host {
	return &Host{
		Recorder:      logsim.NewRecorder(),
		conversations: map[string]*conversation{},
		requests:      map[string]openRequest{},
		rung:          map[string]string{},
		tools:         map[string][]contracts.Tool{},
		answers:       map[string]contracts.ToolResult{},
	}
}

// Seed adds a conversation with its entries and committed offset.
func (h *Host) Seed(conversationID, offset string, entries []contracts.Entry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.conversations[conversationID] = &conversation{offset: offset, entries: entries}
}

// SetContext gives a seeded conversation the context its delta read carries
// (#2331); nil reads as a gateway that sends none.
func (h *Host) SetContext(conversationID string, context *contracts.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if state, ok := h.conversations[conversationID]; ok {
		state.context = context
	}
}

// OpenDoorbell registers the handlers.
func (h *Host) OpenDoorbell(_ context.Context, handlers contracts.Handlers) (contracts.DoorbellClaim, error) {
	if handlers.OnRing == nil || handlers.OnCancel == nil || handlers.OnDisconnect == nil {
		return nil, fmt.Errorf("memory: OpenDoorbell needs all three handlers")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.open {
		return nil, fmt.Errorf("memory: the doorbell is already open")
	}
	h.handlers = handlers
	h.open = true
	return claimFunc(func() error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.open = false
		return nil
	}), nil
}

type claimFunc func() error

func (c claimFunc) Close() error { return c() }

// Ack records the acknowledgement.
func (h *Host) Ack(_ context.Context, ringID string) error {
	if ringID == "" {
		return fmt.Errorf("memory: Ack with no ring id")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.acked = append(h.acked, ringID)
	return nil
}

// ReadSince returns (since, through], omitting the agent's own entries, and the
// conversation's context.
func (h *Host) ReadSince(_ context.Context, conversationID, since, through string, contextBudgetTokens int) (contracts.Read, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	state, ok := h.conversations[conversationID]
	if !ok {
		return contracts.Read{}, fmt.Errorf("memory: no conversation %q", conversationID)
	}
	h.deltaReads++
	h.deltaBudgets = append(h.deltaBudgets, contextBudgetTokens)
	out := make([]contracts.Entry, 0, len(state.entries))
	for _, entry := range state.entries {
		if since != "" && entry.Cursor <= since {
			continue
		}
		if through != "" && entry.Cursor > through {
			continue
		}
		if entry.Author.Self {
			continue
		}
		out = append(out, entry)
	}
	return contracts.Read{Entries: out, Context: state.context}, nil
}

// ReadOpener returns the tail through `through`, including the agent's own.
func (h *Host) ReadOpener(_ context.Context, conversationID, through string, budgetTokens int) ([]contracts.Entry, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	state, ok := h.conversations[conversationID]
	if !ok {
		return nil, fmt.Errorf("memory: no conversation %q", conversationID)
	}
	h.openerReads++
	h.openerBudgets = append(h.openerBudgets, budgetTokens)
	out := make([]contracts.Entry, 0, len(state.entries))
	for _, entry := range state.entries {
		if through != "" && entry.Cursor > through {
			continue
		}
		out = append(out, entry)
	}
	return out, nil
}

// Post records one outbound message.
func (h *Host) Post(_ context.Context, requestID, conversationID, _ string, extras *contracts.PostExtras) (string, error) {
	if requestID == "" || conversationID == "" {
		return "", fmt.Errorf("memory: Post needs a request and a conversation")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.posts++
	if extras != nil {
		h.extras = append(h.extras, *extras)
		if extras.Question != nil {
			h.questions = append(h.questions, *extras.Question)
		}
	}
	return fmt.Sprintf("msg_%03d", h.posts), nil
}

// SetTools gives a conversation its tool catalog (#2349).
func (h *Host) SetTools(conversationID string, tools []contracts.Tool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tools[conversationID] = tools
}

// SetToolResult is what a call of one tool answers. 🔴 The URN is the
// caller's, never built here: a call URN is opaque to the companion.
func (h *Host) SetToolResult(name string, result contracts.ToolResult) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.answers[name] = result
}

// ListTools returns a conversation's catalog; an unseeded one lists none.
func (h *Host) ListTools(_ context.Context, conversationID string) ([]contracts.Tool, error) {
	if conversationID == "" {
		return nil, fmt.Errorf("memory: ListTools needs a conversation")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]contracts.Tool{}, h.tools[conversationID]...), nil
}

// CallTool records a call on a rung request and answers the tool's configured
// result — `{"ok":true}` and no URN when none was set; a request no ring
// delivered is refused before anything is recorded.
func (h *Host) CallTool(_ context.Context, requestID, name string, arguments json.RawMessage) (contracts.ToolResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, known := h.rung[requestID]; !known {
		return contracts.ToolResult{}, contracts.ErrUnknownRequest
	}
	h.toolCalls = append(h.toolCalls, ToolCall{RequestID: requestID, Name: name, Arguments: arguments})
	if answer, ok := h.answers[name]; ok {
		return answer, nil
	}
	return contracts.ToolResult{Result: json.RawMessage(`{"ok":true}`)}, nil
}

// PostExtras returns every post's extras that carried any, in order.
func (h *Host) PostExtras() []contracts.PostExtras {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]contracts.PostExtras(nil), h.extras...)
}

// ToolCalls returns the recorded tool calls.
func (h *Host) ToolCalls() []ToolCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]ToolCall(nil), h.toolCalls...)
}

// CloseRequest closes a request, committing the offset only when the outcome
// earns it.
func (h *Host) CloseRequest(_ context.Context, requestID string, outcome contracts.Outcome, _ string) error {
	if requestID == "" {
		return fmt.Errorf("memory: CloseRequest with no request id")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	request, known := h.requests[requestID]
	if !known {
		return nil
	}
	delete(h.requests, requestID)
	if !outcome.AdvancesOffset() {
		return nil
	}
	if state, ok := h.conversations[request.conversationID]; ok {
		if request.throughCursor > state.offset {
			state.offset = request.throughCursor
		}
	}
	return nil
}

// Close releases the host.
func (h *Host) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.open = false
	return nil
}

// DeliverRing plays a ring at the companion.
func (h *Host) DeliverRing(ring contracts.Ring) error {
	h.mu.Lock()
	if !h.open {
		h.mu.Unlock()
		return fmt.Errorf("memory: no doorbell is open")
	}
	handler := h.handlers.OnRing
	h.requests[ring.RequestID] = openRequest{
		conversationID: ring.ConversationID, throughCursor: ring.ThroughCursor,
	}
	h.rung[ring.RequestID] = ring.ConversationID
	h.mu.Unlock()
	handler(ring)
	return nil
}

// OffsetOf reports a conversation's committed offset.
func (h *Host) OffsetOf(conversationID string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	state, ok := h.conversations[conversationID]
	if !ok {
		return "", fmt.Errorf("memory: no conversation %q", conversationID)
	}
	return state.offset, nil
}

// ReadCounts reports the delta and opener reads served.
func (h *Host) ReadCounts() (delta, opener int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.deltaReads, h.openerReads
}

// DeliverCancel withdraws an open request at the companion.
//
// The fixture host's counterpart. Both delivery doors sit outside AgentsHost
// deliberately — the real host delivers because the platform rang — but the two
// stand-ins must offer the same ones, or a test written against one cannot run
// against the other.
func (h *Host) DeliverCancel(requestID string, reason contracts.CancelReason) error {
	h.mu.Lock()
	if !h.open {
		h.mu.Unlock()
		return fmt.Errorf("memory: no doorbell is open")
	}
	handler := h.handlers.OnCancel
	h.mu.Unlock()
	handler(requestID, reason)
	return nil
}

// DeliverRelease tells the companion a conversation was released (#2356), as
// the doorbell would. A nil OnRelease drops it, as the real host does.
func (h *Host) DeliverRelease(conversationID string, reason contracts.ReleaseReason) error {
	h.mu.Lock()
	if !h.open {
		h.mu.Unlock()
		return fmt.Errorf("memory: no doorbell is open")
	}
	handler := h.handlers.OnRelease
	h.mu.Unlock()
	if handler != nil {
		handler(conversationID, reason)
	}
	return nil
}

// Questions returns every question a post carried (#2232), in order.
func (h *Host) Questions() []contracts.Question {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]contracts.Question(nil), h.questions...)
}

// Budgets reports the context budget every delta read and the budget every
// opener read was given, in order.
func (h *Host) Budgets() (delta, opener []int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]int(nil), h.deltaBudgets...), append([]int(nil), h.openerBudgets...)
}
