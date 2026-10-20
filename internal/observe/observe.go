// Package observe turns the request state machine's events into the
// companion's operational log.
//
// 🔴 It is built on `requests.Observer` deliberately, and that is the whole
// design: the observer receives ids and COUNTS and nothing else, so a log line
// assembled from what lands here cannot carry a post body, a delta or an
// opener, however the logging is later extended. Putting transcript content in
// this log would take a new argument on the interface, which is a review, not
// an accident.
//
// What it emits is what wave 4 measures (epic #1994 decision 45): per request,
// how much was read, how much was declared, how long it took and how it ended.
package observe

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/requests"
)

// Fanout sends every event to each observer in order.
//
// The manager takes ONE observer, and a fixture run needs two: the script's
// expectations and this log. Their order is the argument order, so a test's
// observer never waits behind an I/O-bound one.
func Fanout(observers ...requests.Observer) requests.Observer {
	kept := make([]requests.Observer, 0, len(observers))
	for _, observer := range observers {
		if observer != nil {
			kept = append(kept, observer)
		}
	}
	return fanout(kept)
}

type fanout []requests.Observer

func (f fanout) BriefSent(requestID, conversationID string, entries, chars int) {
	for _, o := range f {
		o.BriefSent(requestID, conversationID, entries, chars)
	}
}

func (f fanout) OpenerSent(requestID string, entries, chars, budget, summaries int) {
	for _, o := range f {
		o.OpenerSent(requestID, entries, chars, budget, summaries)
	}
}

func (f fanout) Acked(requestID string, outcome requests.AckOutcome, session requests.Session) {
	for _, o := range f {
		o.Acked(requestID, outcome, session)
	}
}

func (f fanout) Posted(requestID string, chars int) {
	for _, o := range f {
		o.Posted(requestID, chars)
	}
}

func (f fanout) Closed(requestID string, outcome contracts.Outcome, errorClass string) {
	for _, o := range f {
		o.Closed(requestID, outcome, errorClass)
	}
}

func (f fanout) Cancelled(requestID string, reason contracts.CancelReason) {
	for _, o := range f {
		o.Cancelled(requestID, reason)
	}
}

func (f fanout) Redelivered(requestID string, attempt int) {
	for _, o := range f {
		o.Redelivered(requestID, attempt)
	}
}

// Direction is which way a frame travelled, from the companion's point of view.
const (
	// directionOut — the companion sent it to the plugin.
	directionOut = "out"
	// directionIn — the plugin sent it to the companion.
	directionIn = "in"
)

// Logging writes one line per frame.
func Logging(logger *slog.Logger, now func() time.Time) requests.Observer {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if now == nil {
		now = time.Now
	}
	return &logging{log: logger, now: now, opened: map[string]open{}}
}

type open struct {
	at           time.Time
	conversation string
}

type logging struct {
	log *slog.Logger
	now func() time.Time

	mu sync.Mutex
	// opened remembers when each request started and which conversation it
	// belongs to, so every later line carries both — the observer's later calls
	// name only the request.
	opened map[string]open
}

// line emits one frame's log record with the fields every line shares.
func (l *logging) line(requestID, direction, frame string, attrs ...slog.Attr) {
	l.mu.Lock()
	state, known := l.opened[requestID]
	l.mu.Unlock()

	fields := []slog.Attr{
		slog.String("direction", direction),
		slog.String("frame", frame),
		slog.String("request", requestID),
	}
	// Omitted rather than emitted empty, for the same reason as elapsedMs below:
	// a field that is present and blank reads as "this request has no
	// conversation", which is never true — it means we have not seen its brief.
	if known && state.conversation != "" {
		fields = append(fields, slog.String("conversation", state.conversation))
	}
	if known && !state.at.IsZero() {
		fields = append(fields, slog.Int64("elapsedMs", l.now().Sub(state.at).Milliseconds()))
	}
	fields = append(fields, attrs...)

	l.log.LogAttrs(context.Background(), slog.LevelInfo, "frame", fields...)
}

// maxTracked bounds the opened map.
//
// 🔴 Entries leave on a close, and a request whose upstream close permanently
// fails never reports one — so without a cap this map is unbounded on a daemon
// that runs for months. Dropping the oldest loses an elapsed time on a request
// that was already abandoned, which is the cheap half of the trade.
const maxTracked = 4096

func (l *logging) BriefSent(requestID, conversationID string, entries, chars int) {
	l.mu.Lock()
	state, known := l.opened[requestID]
	if !known {
		state = open{at: l.now()}
	}
	state.conversation = conversationID
	l.opened[requestID] = state
	l.evictLocked()
	l.mu.Unlock()

	l.line(requestID, directionOut, "brief",
		slog.Int("deltaEntryCount", entries), slog.Int("deltaChars", chars))
}

func (l *logging) OpenerSent(requestID string, entries, chars, budget, summaries int) {
	// 🔴 `budgetUsedChars`, not `budgetUsed`: nothing in the companion tokenizes,
	// and the harness that declared the budget is the only party that knows its
	// own tokenizer. Emitting characters — the fact this process actually has —
	// beats a second chars-per-token guess nobody would ever reconcile.
	l.line(requestID, directionOut, "opener",
		slog.Int("openerEntryCount", entries),
		slog.Int("openerChars", chars),
		// 🔴 How many of the entries are the platform's summaries (#2451) — a
		// count, never their text.
		slog.Int("openerSummaryCount", summaries),
		slog.Int("budgetDeclared", budget),
		slog.Int("budgetUsedChars", chars))
}

func (l *logging) Acked(requestID string, outcome requests.AckOutcome, session requests.Session) {
	l.line(requestID, directionIn, "ack",
		slog.String("outcome", string(outcome)), slog.String("session", string(session)))
}

func (l *logging) Posted(requestID string, chars int) {
	l.line(requestID, directionIn, "post", slog.Int("postChars", chars))
}

func (l *logging) Closed(requestID string, outcome contracts.Outcome, errorClass string) {
	l.line(requestID, directionIn, "done",
		slog.String("outcome", string(outcome)), slog.String("errorClass", errorClass))

	// Forgotten only after the closing line, so that line still carries elapsed.
	l.mu.Lock()
	delete(l.opened, requestID)
	l.mu.Unlock()
}

// evictLocked drops the oldest tracked request once the map is over its cap.
// The caller holds the mutex.
func (l *logging) evictLocked() {
	if len(l.opened) <= maxTracked {
		return
	}
	oldestID, oldest := "", time.Time{}
	for id, state := range l.opened {
		if oldest.IsZero() || state.at.Before(oldest) {
			oldestID, oldest = id, state.at
		}
	}
	delete(l.opened, oldestID)
}

func (l *logging) Cancelled(requestID string, reason contracts.CancelReason) {
	l.line(requestID, directionOut, "cancel", slog.String("reason", string(reason)))
}

// 🔴 `redelivery`, not `brief`. A redelivery is followed immediately by the
// real brief line, so naming both "brief" would double-count every redelivery
// in the log whose whole purpose is the counts wave 4 measures.
func (l *logging) Redelivered(requestID string, attempt int) {
	l.line(requestID, directionOut, "redelivery", slog.Int("attempt", attempt))
}
