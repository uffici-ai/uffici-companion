// Package requests is the per-request state machine: one request per
// conversation, from the ring that opens it to the close that ends it.
//
// The states and their transitions are #2010's, exactly — the wire's document
// and this package must agree, because a plugin author reads the document and
// this code decides what actually happens:
//
//	delivered → acked(fresh | resumed) → [opening] → posting → closed{outcome}
//	                                                         → cancelled{reason}
//	                                                         → expired
//
// 🔴 The machine owns the ORDER, not the transport. It sends through a
// PluginSink and reads through an AgentsHost, so every rule below — supersede,
// expiry, redelivery, the caps, one request per conversation — is exercised in
// tests with no socket anywhere. That is deliberate: these are the rules that
// are wrong in ways nothing crashes over.
package requests

import (
	"fmt"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
)

// State is where a request stands.
type State string

const (
	// StateDelivered — the brief was sent; the plugin has not answered.
	StateDelivered State = "delivered"
	// StateAcked — the plugin answered, naming a session state.
	StateAcked State = "acked"
	// StateOpening — a FRESH ack earned an opener, which is being read and sent.
	// Reached only from a fresh ack, and left when the opener frame goes out.
	StateOpening State = "opening"
	// StatePosting — the turn is under way.
	StatePosting State = "posting"
	// StateClosed — `done` arrived.
	StateClosed State = "closed"
	// StateCancelled — a cancel was sent.
	StateCancelled State = "cancelled"
	// StateExpired — the deadline passed with no close.
	StateExpired State = "expired"
)

// Terminal reports whether nothing further may happen to a request in this
// state.
func (s State) Terminal() bool {
	return s == StateClosed || s == StateCancelled || s == StateExpired
}

// Session is the harness's answer about its own context.
type Session string

const (
	// SessionFresh — a new harness session, which earns an opener.
	SessionFresh Session = "fresh"
	// SessionResumed — an existing session, which does NOT.
	SessionResumed Session = "resumed"
)

// EarnsOpener reports whether this session state earns an opener read.
//
// 🔴 Fresh only, and the asymmetry is the free-local-hop rule (decision 43). A
// resumed session already holds the conversation; reading an opener for one
// buys nothing and costs a platform read on EVERY turn, forever. Nothing about
// the turn looks different — it answers either way — so only a count can see it,
// which is why the fixture counts opener reads and `expectNoOpener` asserts zero.
func (s Session) EarnsOpener() bool { return s == SessionFresh }

// AckOutcome is how the plugin answered a brief.
type AckOutcome string

const (
	// AckAccepted — the plugin took the request.
	AckAccepted AckOutcome = "accepted"
	// AckDeferred — the plugin took it but has no session yet.
	AckDeferred AckOutcome = "deferred"
	// AckRejected — the plugin refused; the request redelivers.
	AckRejected AckOutcome = "rejected"
)

// Request is one open request's whole state.
type Request struct {
	RequestID      string
	ConversationID string
	RingID         string
	Reason         string
	Cause          contracts.Cause
	SinceCursor    string
	ThroughCursor  string
	ResetSession   bool
	Deadline       time.Time

	State   State
	Session Session

	// Posts counts forwarded posts, against PostsPerRequestMax.
	Posts int
	// Redeliveries counts how many times this request has been sent again.
	Redeliveries int

	// DeliveredAt is when the brief last went out, for the ack timeout.
	DeliveredAt time.Time
}

// Limits bound one request. Zero values mean the defaults.
type Limits struct {
	// MaxPosts is how many posts one request may carry.
	MaxPosts int
	// MaxPostBytes is the largest single post.
	MaxPostBytes int
	// MaxRedeliveries is how many times a rejected or unacknowledged request is
	// sent again before it is closed as an error.
	//
	// 🔴 Bounded, not infinite. A plugin that rejects every brief — a crash loop,
	// a misconfigured harness — would otherwise be redelivered forever, and the
	// platform would see a request that never closes rather than an error it can
	// act on.
	MaxRedeliveries int
	// AckTimeout is how long a delivered brief waits for an ack.
	AckTimeout time.Duration
}

// DefaultLimits are the stated bounds.
func DefaultLimits() Limits {
	return Limits{
		MaxPosts:        64,
		MaxPostBytes:    131_072,
		MaxRedeliveries: 3,
		AckTimeout:      30 * time.Second,
	}
}

// withDefaults fills zero values, so a caller may set only what it cares about.
func (l Limits) withDefaults() Limits {
	defaults := DefaultLimits()
	if l.MaxPosts <= 0 {
		l.MaxPosts = defaults.MaxPosts
	}
	if l.MaxPostBytes <= 0 {
		l.MaxPostBytes = defaults.MaxPostBytes
	}
	if l.MaxRedeliveries <= 0 {
		l.MaxRedeliveries = defaults.MaxRedeliveries
	}
	if l.AckTimeout <= 0 {
		l.AckTimeout = defaults.AckTimeout
	}
	return l
}

// ErrUnknownRequest is the close reason a frame naming no open request earns.
var ErrUnknownRequest = fmt.Errorf("unknown_request")

// ErrClosedRequest marks a frame for a request this companion closed itself.
//
// 🔴 NOT a close reason, and that is the whole point (#2023). A cancel and the
// plugin's terminal frame for the same request cross on the wire by
// construction — the plugin cannot know a cancel is coming, and by the time it
// arrives the `done` is already written. Answering that with
// `unknown_request` drops the connection, taking every OTHER open request on it
// down as well; a supersede is the ordinary case, so it fires in normal use.
var ErrClosedRequest = fmt.Errorf("closed_request")

// ErrWrongConnection is the close reason a frame on the wrong connection earns.
var ErrWrongConnection = fmt.Errorf("wrong_connection")

// OutcomeFor maps a terminal state and its cause onto the platform outcome.
//
// 🔴 One function, because the mapping is where the offset rule is decided: a
// cancelled or expired request must reach the platform as an outcome that does
// NOT advance the offset, or the entries it covered are lost from every later
// brief. Keeping it beside the state machine means there is one place to read.
func OutcomeFor(state State, done contracts.Outcome) contracts.Outcome {
	switch state {
	case StateClosed:
		return done
	case StateCancelled, StateExpired:
		return contracts.OutcomeCancelled
	default:
		return contracts.OutcomeError
	}
}
