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
	"fmt"
	"sync"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
)

// Host is an in-memory AgentsHost.
type Host struct {
	mu       sync.Mutex
	handlers contracts.Handlers
	open     bool

	conversations map[string]*conversation
	requests      map[string]openRequest
	acked         []string
	posts         int
	openerReads   int
	deltaReads    int
}

type conversation struct {
	offset  string
	entries []contracts.Entry
}

type openRequest struct {
	conversationID string
	throughCursor  string
}

// New returns an empty host.
func New() *Host {
	return &Host{
		conversations: map[string]*conversation{},
		requests:      map[string]openRequest{},
	}
}

// Seed adds a conversation with its entries and committed offset.
func (h *Host) Seed(conversationID, offset string, entries []contracts.Entry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.conversations[conversationID] = &conversation{offset: offset, entries: entries}
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

// ReadSince returns (since, through], omitting the agent's own entries.
func (h *Host) ReadSince(_ context.Context, conversationID, since, through string) ([]contracts.Entry, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	state, ok := h.conversations[conversationID]
	if !ok {
		return nil, false, fmt.Errorf("memory: no conversation %q", conversationID)
	}
	h.deltaReads++
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
	return out, false, nil
}

// ReadOpener returns the tail through `through`, including the agent's own.
func (h *Host) ReadOpener(_ context.Context, conversationID, through string, _ int) ([]contracts.Entry, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	state, ok := h.conversations[conversationID]
	if !ok {
		return nil, fmt.Errorf("memory: no conversation %q", conversationID)
	}
	h.openerReads++
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
func (h *Host) Post(_ context.Context, requestID, conversationID, _ string) (string, error) {
	if requestID == "" || conversationID == "" {
		return "", fmt.Errorf("memory: Post needs a request and a conversation")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.posts++
	return fmt.Sprintf("msg_%03d", h.posts), nil
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
