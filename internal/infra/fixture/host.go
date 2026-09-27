package fixture

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
)

// Host is a scripted AgentsHost.
//
// 🔴 It implements the interface the REAL gateway host will implement, and
// nothing more. Every temptation to give the fixture a method the gateway could
// not offer — "tell me the current request", "wait for the surface" — is a
// temptation to make the fixture the shape of the surface's convenience rather
// than the shape of the platform. Those needs are served by the runner instead,
// which is not an AgentsHost.
type Host struct {
	script *Script
	store  *store

	mu       sync.Mutex
	handlers contracts.Handlers
	open     bool
	// requests is what the host remembers between a ring and its close: the
	// conversation and the through cursor, which is what an offset commits to.
	requests map[string]openRequest
	rings    int

	// Recorded for the terminal tally and for the runner's expectations.
	acked  []string
	posts  []PostRecord
	closes []CloseRecord
}

// PostRecord is one forwarded post.
//
// 🔴 It records the request, the conversation and the LENGTH, plus the text —
// the text because the runner's `contains` matcher needs it and this is a test
// double. It is never logged: the surface's log line carries counts only, and
// `TestLoggerNeverSeesContent` pins that.
type PostRecord struct {
	RequestID      string
	ConversationID string
	Text           string
	At             time.Time
}

// CloseRecord is one closed request.
type CloseRecord struct {
	RequestID  string
	Outcome    contracts.Outcome
	ErrorClass string
	// OffsetCommitted reports whether this close advanced the conversation's
	// offset — the fixture's observation of decision 40.
	OffsetCommitted bool
	At              time.Time
}

// openRequest is what the host remembers between a ring and its close.
type openRequest struct {
	conversationID string
	throughCursor  string
}

// New builds a Host from a validated script.
func New(script *Script) (*Host, error) {
	if script == nil {
		return nil, fmt.Errorf("fixture: no script")
	}
	if err := script.Validate(); err != nil {
		return nil, err
	}
	built, err := newStore(script)
	if err != nil {
		return nil, err
	}
	return &Host{script: script, store: built, requests: map[string]openRequest{}}, nil
}

// OpenDoorbell registers the companion's handlers. Rings arrive when the runner
// plays them, not on a timer: a fixture that delivered on its own schedule
// would make every timing assertion in a script a race.
func (h *Host) OpenDoorbell(_ context.Context, handlers contracts.Handlers) (contracts.DoorbellClaim, error) {
	if handlers.OnRing == nil || handlers.OnCancel == nil || handlers.OnDisconnect == nil {
		return nil, fmt.Errorf("fixture: OpenDoorbell needs all three handlers; a nil one would drop deliveries silently")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.open {
		return nil, fmt.Errorf("fixture: the doorbell is already open — one claim per host, as the gateway enforces one per key")
	}
	h.handlers = handlers
	h.open = true
	return &claim{host: h}, nil
}

type claim struct{ host *Host }

func (c *claim) Close() error {
	c.host.mu.Lock()
	defer c.host.mu.Unlock()
	c.host.open = false
	return nil
}

// Ack records that the companion has the ring.
//
// 🔴 It records rather than verifies the ring is still open: an acknowledgement
// means "this daemon received it", and a redelivered ring acknowledged twice is
// ordinary, not an error.
func (h *Host) Ack(_ context.Context, ringID string) error {
	if ringID == "" {
		return fmt.Errorf("fixture: Ack with no ring id")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.acked = append(h.acked, ringID)
	return nil
}

// ReadSince returns the delta for a request.
func (h *Host) ReadSince(_ context.Context, conversationID, since, through string) ([]contracts.Entry, bool, error) {
	return h.store.readSince(conversationID, since, through)
}

// ReadOpener returns the recent tail, including the agent's own entries.
func (h *Host) ReadOpener(_ context.Context, conversationID, through string, budgetTokens int) ([]contracts.Entry, error) {
	return h.store.readOpener(conversationID, through, budgetTokens)
}

// Post records one outbound message and returns its id.
func (h *Host) Post(_ context.Context, requestID, conversationID, text string) (string, error) {
	if requestID == "" || conversationID == "" {
		return "", fmt.Errorf("fixture: Post needs a request and a conversation")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.posts = append(h.posts, PostRecord{
		RequestID: requestID, ConversationID: conversationID, Text: text, At: time.Now(),
	})
	return fmt.Sprintf("msg_%03d", len(h.posts)), nil
}

// CloseRequest closes a request and commits the offset when — and only when —
// the outcome earns it.
func (h *Host) CloseRequest(_ context.Context, requestID string, outcome contracts.Outcome, errorClass string) error {
	if requestID == "" {
		return fmt.Errorf("fixture: CloseRequest with no request id")
	}
	h.mu.Lock()
	request, known := h.requests[requestID]
	if known {
		delete(h.requests, requestID)
	}
	h.mu.Unlock()

	committed := false
	if known {
		var err error
		committed, err = h.store.commitOffset(request.conversationID, request.throughCursor, outcome)
		if err != nil {
			return err
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	h.closes = append(h.closes, CloseRecord{
		RequestID: requestID, Outcome: outcome, ErrorClass: errorClass,
		OffsetCommitted: committed, At: time.Now(),
	})
	return nil
}

// DeliverRing plays one ring at the companion.
//
// 🔴 Not part of AgentsHost, and deliberately: the gateway host delivers because
// the platform rang, not because somebody called it. This is the runner's door
// into the fixture, which is why it lives beside the interface rather than in
// it.
func (h *Host) DeliverRing(ring contracts.Ring) error {
	h.mu.Lock()
	if !h.open {
		h.mu.Unlock()
		return fmt.Errorf("fixture: no doorbell is open, so ring %s would be delivered to nobody", ring.RingID)
	}
	handler := h.handlers.OnRing
	h.requests[ring.RequestID] = openRequest{
		conversationID: ring.ConversationID,
		throughCursor:  ring.ThroughCursor,
	}
	h.rings++
	h.mu.Unlock()

	handler(ring)
	return nil
}

// DeliverCancel withdraws an open request at the companion.
func (h *Host) DeliverCancel(requestID string, reason contracts.CancelReason) error {
	h.mu.Lock()
	if !h.open {
		h.mu.Unlock()
		return fmt.Errorf("fixture: no doorbell is open, so the cancel would reach nobody")
	}
	handler := h.handlers.OnCancel
	h.mu.Unlock()

	handler(requestID, reason)
	return nil
}

// RingsDelivered reports how many rings the fixture played.
func (h *Host) RingsDelivered() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.rings
}

// Close releases the host.
func (h *Host) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.open = false
	return nil
}

// ── What the runner reads, and the gateway host will not have ───────────────

// Posts returns the forwarded posts so far.
func (h *Host) Posts() []PostRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]PostRecord(nil), h.posts...)
}

// Closes returns the closed requests so far.
func (h *Host) Closes() []CloseRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]CloseRecord(nil), h.closes...)
}

// Acks returns the acknowledged ring ids.
func (h *Host) Acks() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.acked...)
}

// OffsetOf reports a conversation's committed offset by script name.
func (h *Host) OffsetOf(name string) (string, error) { return h.store.offsetOf(name) }

// ReadCounts reports how many delta and opener reads the host served.
//
// 🔴 The opener count is how `expectNoOpener` is proven: reading an opener on a
// RESUMED ack is invisible in behaviour — the turn still answers — and costs a
// platform read every turn forever. A count is the only way to see it.
func (h *Host) ReadCounts() (readSince, readOpener int) { return h.store.counts() }

// AppendEntries adds entries to a conversation mid-run, which is how a script
// makes a message arrive while the agent is answering.
func (h *Host) AppendEntries(conversation string, entries []ScriptEntry) error {
	return h.store.appendEntries(conversation, entries)
}
