// Package contracts holds the interfaces the companion depends on, and nothing
// else: no implementation, no I/O, no third-party import.
//
// This is the platform's interface-first pattern carried into Go idiom
// (`.claude/rules/architecture.md`, `contracts.md`): code depends on the
// interface, implementations live under `internal/infra/<backend>/`, and
// `internal/registry` hands out the one selected by configuration. A test
// substitutes an implementation; it never reaches for a concrete type.
//
// 🔴 `AgentsHost` is named for the REAL host, not for the fixture. Wave 1 shipped
// a scripted implementation so the local surface could be built and proven
// without a platform; wave 1b's `GatewayAgentsHost` (#2077) slots in behind this
// interface with its METHODS unchanged. Two struct-level amendments were made
// when it did, both to say what the platform's protocol says: `Cause` carries
// the author's type (the ring's `cause.author.type`), and the person's stop is
// `stop`, the protocol's word — the local wire keeps its own `stopped`, mapped at
// the surface. Every method here is written for what the platform can actually
// answer, not for what a script finds convenient — if the fixture ever needs a
// method the gateway could not implement, the interface is wrong and the
// fixture is the thing to change.
package contracts

import (
	"context"
	"time"
)

// Ring is the platform's doorbell envelope: **ids and a reason, never content**.
//
// 🔴 The word is deliberate and reserved. A ring travels the network hop; a
// brief travels the local wire and carries the conversation content the
// platform authorized. Calling either by the other's name is how the content
// boundary between the two hops stops being visible in the code.
type Ring struct {
	// RingID identifies this delivery, and is what Ack acknowledges. A
	// redelivery of the same request carries a NEW ring id.
	RingID string
	// RequestID identifies the request across redeliveries.
	RequestID string
	// ConversationID is the conversation this request answers in.
	ConversationID string
	// Reason is the platform's wake vocabulary, carried through verbatim.
	Reason string
	// Cause is the recorded human cause: the entry and its author.
	Cause Cause
	// Deadline is when the companion must give up and close as expired.
	Deadline time.Time
	// SinceCursor is the exclusive lower bound of the delta; empty for a
	// conversation's first request.
	SinceCursor string
	// ThroughCursor is the inclusive upper bound, fixed when the request opened,
	// so a redelivery recomputes the same range.
	ThroughCursor string
	// ResetSession tells the plugin it must not resume a harness session.
	ResetSession bool
}

// Cause is the entry that caused a request, and who wrote it — by type and id,
// never by name: a ring carries ids only.
type Cause struct {
	EntryID string
	// AuthorType is the protocol's author type: `user`, `agent`, `system` or
	// `external`. Empty from a host that does not know it.
	AuthorType string
	AuthorID   string
}

// Entry is one transcript entry as the platform read it.
//
// It mirrors the wire's entry, deliberately: the companion maps one to one and
// composes nothing. Keeping a separate type here is what stops the wire's JSON
// shape from becoming the host's interface — they are versioned separately, and
// the day they diverge the mapping is one function rather than a rewrite.
type Entry struct {
	ID          string
	Cursor      string
	At          time.Time
	Author      Author
	Kind        string
	Text        string
	Attachments []Attachment
}

// Author is who wrote an entry.
type Author struct {
	Type   string
	ID     string
	Label  string
	Handle string
	// Self is true for the answering agent's own entries: a delta omits them,
	// an opener includes them.
	Self bool
}

// Attachment is a file an entry carries, by reference.
type Attachment struct {
	ID   string
	Name string
	Ref  string
}

// Outcome is how a request closed.
type Outcome string

const (
	// OutcomeAnswered — the agent answered. Advances the offset.
	OutcomeAnswered Outcome = "answered"
	// OutcomeEmpty — the agent had nothing to say, explicitly. Advances the
	// offset: the entries WERE considered.
	OutcomeEmpty Outcome = "empty"
	// OutcomeError — the turn failed. 🔴 Leaves the offset.
	OutcomeError Outcome = "error"
	// OutcomeCancelled — stopped, superseded or expired. 🔴 Leaves the offset.
	OutcomeCancelled Outcome = "cancelled"
)

// AdvancesOffset reports whether closing with this outcome commits the
// conversation's offset.
//
// 🔴 ONLY answered and empty. This is the one-line rule behind the most
// expensive silent bug this design admits: advancing on a cancelled or errored
// request means the next brief starts AFTER entries nobody answered, so a
// superseded turn's messages are never seen by any agent, ever. It is a
// function rather than a `switch` at each call site precisely so there is one
// place to get it right and one place to test (decision 40).
func (o Outcome) AdvancesOffset() bool {
	return o == OutcomeAnswered || o == OutcomeEmpty
}

// CancelReason is why an open request was withdrawn.
type CancelReason string

const (
	// CancelStop — a person stopped the turn. The protocol's word; the local
	// wire calls it `stopped`, and the surface maps one onto the other.
	CancelStop CancelReason = "stop"
	// CancelSuperseded — a newer request arrived for the same conversation.
	CancelSuperseded CancelReason = "superseded"
	// CancelExpired — the deadline passed with no close.
	CancelExpired CancelReason = "expired"
)

// Handlers is what an AgentsHost calls on the companion. Every one is invoked
// on the host's own goroutine and must not block.
type Handlers struct {
	// OnRing delivers a new request.
	OnRing func(Ring)
	// OnCancel withdraws an open request.
	OnCancel func(requestID string, reason CancelReason)
	// OnDisconnect reports the doorbell closing, with a classified reason.
	OnDisconnect func(reason string)
}

// DoorbellClaim is one open doorbell connection. Closing it stops delivery.
type DoorbellClaim interface {
	Close() error
}

// AgentsHost is the platform, as the companion sees it.
//
// 🔴 The companion acknowledges a ring ON RECEIPT — before the harness has seen
// anything — because the acknowledgement means "this daemon has it", not "the
// agent answered it". Conflating the two would make every slow harness look
// like a dropped delivery and trigger redelivery storms.
type AgentsHost interface {
	// OpenDoorbell starts delivery and returns the claim that stops it.
	OpenDoorbell(ctx context.Context, handlers Handlers) (DoorbellClaim, error)

	// Ack tells the platform this companion has the ring.
	Ack(ctx context.Context, ringID string) error

	// ReadSince returns the delta for a request: (sinceCursor, throughCursor],
	// with the answering agent's own entries omitted. `bounded` reports whether
	// the platform trimmed the range.
	ReadSince(ctx context.Context, conversationID, sinceCursor, throughCursor string) (entries []Entry, bounded bool, err error)

	// ReadOpener returns the verbatim recent tail within the harness's declared
	// budget, INCLUDING the agent's own entries.
	//
	// 🔴 Called only after an ack naming a FRESH session. The local hop is free
	// and the network hop is not: a resumed session already has its context, so
	// reading an opener for one would buy nothing and cost a platform read on
	// every turn (decision 43).
	ReadOpener(ctx context.Context, conversationID, throughCursor string, budgetTokens int) ([]Entry, error)

	// Post sends one outbound message. Every post means "more coming"; the
	// request closes with CloseRequest.
	Post(ctx context.Context, requestID, conversationID, text string) (messageID string, err error)

	// CloseRequest closes a request with its outcome, and an error class when
	// the outcome is an error.
	CloseRequest(ctx context.Context, requestID string, outcome Outcome, errorClass string) error

	// Close releases the host's resources.
	Close() error
}

// ClassifiedError is a host failure with decision 32's two halves: a class for
// the machine and an instruction the agent relays to the person. A host
// returns one wherever it can say more than "it failed", and the request
// machine relays both instead of flattening them into a generic error.
type ClassifiedError interface {
	error
	ErrorClass() string
	ErrorInstruction() string
}

// ClassRequestClosed is the platform's class for a request that is already
// over: posting to it again is pointless, and closing it again is harmless.
const ClassRequestClosed = "request_closed"
