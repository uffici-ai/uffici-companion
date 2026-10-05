package fixture

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/infra/logsim"
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
	// Recorder is the log side (#2264): a scriptable policy and every pushed
	// batch, which a `--once` run reports and the end-to-end run asserts on.
	*logsim.Recorder

	script *Script
	store  *store

	mu       sync.Mutex
	handlers contracts.Handlers
	open     bool
	// requests is what the host remembers between a ring and its close: the
	// conversation and the through cursor, which is what an offset commits to.
	requests map[string]openRequest
	rings    int
	// refuseQuestion makes the next post carrying a question fail (#2235).
	refuseQuestion bool

	// Recorded for the terminal tally and for the runner's expectations.
	acked     []string
	posts     []PostRecord
	closes    []CloseRecord
	toolCalls []ToolCallRecord
	// memoryCalls and facts are the agent's memory (#2385).
	memoryCalls []MemoryRecord
	facts       map[string][]*fact
	// rung is every request a ring delivered, by id, to its conversation — never
	// forgotten at close, as the gateway host's memory is not (#2349).
	rung map[string]string
}

// ToolCallRecord is one tool call that reached the host (#2349). The arguments
// are kept for the runner's `argumentsContain`; this is a test double, and
// they are never logged.
type ToolCallRecord struct {
	RequestID      string
	ConversationID string
	Name           string
	Arguments      json.RawMessage
	At             time.Time
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
	// Question is set when the post asked the person something (#2232).
	Question *contracts.Question
	// Citations and Activity are what the post reported (#2349).
	Citations []string
	Activity  []contracts.Activity
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
	recorder := logsim.NewRecorder()
	if script.Logs != nil && script.Logs.HarnessLevel != "" {
		recorder.SetShipping(true, script.Logs.HarnessLevel)
	}
	return &Host{
		Recorder: recorder,
		script:   script, store: built, requests: map[string]openRequest{}, rung: map[string]string{},
	}, nil
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

// ReadSince returns the delta for a request, and the context the script gave
// the conversation (#2331).
func (h *Host) ReadSince(_ context.Context, conversationID, since, through string, contextBudgetTokens int) (contracts.Read, error) {
	return h.store.readSince(conversationID, since, through, contextBudgetTokens)
}

// ContextBudgets is every context budget a delta read was given, in order —
// what the companion passed through from the plugin's hello.
func (h *Host) ContextBudgets() []int {
	return h.store.contextBudgets()
}

// ReadOpener returns the recent tail, including the agent's own entries.
func (h *Host) ReadOpener(_ context.Context, conversationID, through string, budgetTokens int) ([]contracts.Entry, error) {
	return h.store.readOpener(conversationID, through, budgetTokens)
}

// Post records one outbound message and returns its id.
func (h *Host) Post(_ context.Context, requestID, conversationID, text string, extras *contracts.PostExtras) (string, error) {
	if requestID == "" || conversationID == "" {
		return "", fmt.Errorf("fixture: Post needs a request and a conversation")
	}
	if extras == nil {
		extras = &contracts.PostExtras{}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if extras.Question != nil && h.refuseQuestion {
		h.refuseQuestion = false
		return "", errQuestionRefused
	}
	h.posts = append(h.posts, PostRecord{
		RequestID: requestID, ConversationID: conversationID, Text: text, At: time.Now(),
		Question: extras.Question, Citations: extras.Citations, Activity: extras.Activity,
	})
	return fmt.Sprintf("msg_%03d", len(h.posts)), nil
}

// ListTools serves the agent's scripted catalog (#2349) — the same for every
// conversation the script declares, as the platform's is the agent's.
func (h *Host) ListTools(_ context.Context, conversationID string) ([]contracts.Tool, error) {
	return h.store.toolsFor(conversationID)
}

// CallTool records one tool call and answers the script's scripted
// result and `callUrn` (#2349) — the URN is the script's data, never built here. A request no ring delivered is refused before anything is
// recorded, as the gateway host refuses it before anything is sent; a tool
// the catalog does not list is the platform's `not_found`.
func (h *Host) CallTool(_ context.Context, requestID, name string, arguments json.RawMessage) (contracts.ToolResult, error) {
	h.mu.Lock()
	conversationID, known := h.rung[requestID]
	h.mu.Unlock()
	if !known {
		return contracts.ToolResult{}, contracts.ErrUnknownRequest
	}
	scripted, listed, err := h.store.toolResult(conversationID, name)
	if err != nil {
		return contracts.ToolResult{}, err
	}
	if !listed {
		return contracts.ToolResult{}, errToolNotFound
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.toolCalls = append(h.toolCalls, ToolCallRecord{
		RequestID: requestID, ConversationID: conversationID, Name: name,
		Arguments: append(json.RawMessage(nil), arguments...), At: time.Now(),
	})
	return contracts.ToolResult{CallURN: scripted.CallURN, Result: scripted.Result}, nil
}

// toolNotFound is the platform's answer to a call naming a tool the catalog does
// not list: a class and a sentence, like every host failure (decision 32).
type toolNotFound struct{}

func (toolNotFound) Error() string { return "fixture: the agent has no such tool" }

func (toolNotFound) ErrorClass() string { return "not_found" }

func (toolNotFound) ErrorInstruction() string {
	return "That tool is not available in this conversation; tell the person it could not be done."
}

var errToolNotFound error = toolNotFound{}

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
	h.rung[ring.RequestID] = ring.ConversationID
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

// RefuseNextQuestion makes the next post that carries a question fail, as a
// platform that could not take it would (#2235).
//
// 🔴 It is the one real way a turn waiting on a question is cancelled: the
// question SETTLES its request, so the platform has nothing open to cancel — but
// when the question's post fails, the companion withdraws the request that asked,
// and the harness's turn is still blocked on the question. A plugin that only
// stops that turn leaves it holding the harness's session, and the person's next
// message waits behind it until its deadline.
func (h *Host) RefuseNextQuestion() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.refuseQuestion = true
}

// questionRefused is the platform's answer to a refused question post: a class and
// the sentence the agent relays, like every host failure (decision 32).
type questionRefused struct{}

func (questionRefused) Error() string { return "fixture: the platform refused the question post" }

func (questionRefused) ErrorClass() string { return "platform_unavailable" }

func (questionRefused) ErrorInstruction() string {
	return "The platform could not take the question; tell the person to ask again."
}

var errQuestionRefused error = questionRefused{}

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

// ToolCalls returns the tool calls that reached the host so far (#2349).
func (h *Host) ToolCalls() []ToolCallRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]ToolCallRecord(nil), h.toolCalls...)
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
