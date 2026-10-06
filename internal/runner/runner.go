// Package runner plays a fixture script against the running companion and
// answers one question: did every expectation hold?
//
// 🔴 `--once` exits 0 ONLY if every expectation held AND at least one ring was
// delivered. A script that rang nothing meets every expectation vacuously, so
// without that second condition a green run would prove the daemon starts and
// nothing else — this repository's defect-ledger class 1, in the one place it
// would be least visible.
//
// The runner is an `Observer`, which is how it sees what happened: the state
// machine already reports every brief, ack, opener, post, close and cancel as
// counts and ids. Nothing here reaches into the manager, and nothing here sees
// transcript text.
package runner

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/infra/fixture"
	"github.com/uffici-ai/uffici-companion/internal/requests"
	"github.com/uffici-ai/uffici-companion/internal/wire"
)

// DefaultWait is how long an expectation waits when the step names no timeout.
const DefaultWait = 2 * time.Second

// AttachWait is how long a run waits for a plugin before starting the script.
//
// 🔴 A script does not start against nobody. Every step after the first is
// written as though a plugin is there, and `secondConnection` is the one that
// makes the omission obvious: it dials, WINS the single-plugin claim before the
// real plugin does, and then the run deadlocks with each side waiting for the
// other. Waiting here is what puts the plugin first, deterministically, rather
// than leaving the order to whichever goroutine is scheduled sooner.
const AttachWait = 15 * time.Second

// RestartHarnessLine is what a `restartHarness` step asks the run's driver to
// act on (#2235).
//
// 🔴 A published literal: `scripts/hermes-e2e.mjs` and the plugin's
// `dev/e2e.py` restart the harness's container when the companion logs it, and
// both are pinned to this spelling from the platform's root. Rewording it here
// alone would leave the step waiting on a restart nobody performs.
const RestartHarnessLine = "restart the harness"

// EndHarnessSessionLine is what an `endHarnessSession` step asks the run's
// driver to act on (#2414), with the conversation's wire id as `conversation`.
//
// 🔴 A published literal, like RestartHarnessLine: both drivers end that
// conversation's session INSIDE the real harness when the companion logs it —
// for Hermes, its row in `state.db`, which a live gateway reads at routing time
// and replaces with a new, empty session. Pinned from the platform's root.
const EndHarnessSessionLine = "end the harness session"

// Plugin is what the runner can do to the attached plugin, for the steps that
// exercise reconnection and refusal.
//
// 🔴 An interface so the runner never holds a socket. `disconnectPlugin` is the
// daemon dropping the connection, not the plugin choosing to leave — which is
// the same thing from the state machine's point of view and the only half we
// can drive from here.
// 🔴 Every error an implementation returns must be CLASSIFIED — its own, with a
// message it chose. The runner turns them into unmet expectations, which the
// daemon logs, so a transport's own error would put whatever that transport
// embeds into a log line: coder/websocket renders the dial target, which on the
// socket transport is a path inside `$UFFICI_HOME` — the directory that also
// holds the wire token ([logging.md]).
type Plugin interface {
	// Disconnect drops the attached plugin connection.
	Disconnect() error
	// DialSecond opens a second connection and reports the close reason it got.
	DialSecond(ctx context.Context) (string, error)
	// Attached reports whether a plugin is connected.
	Attached() bool
}

// Result is a run's verdict.
type Result struct {
	RingsDelivered int
	Unmet          []string
	Counts         Counts
	// Restarts is every `restartHarness` step's window, in order (#2313).
	Restarts []Window
}

// Window is one restart of the harness: From the moment the runner asked the
// driver for it, To the moment the OLD plugin connection was first seen gone.
// A line the harness logged inside it is the dying process's last words — the
// shutdown's own warnings, which the plugin's final push exists to ship. The
// new process logs its startup lines after the old connection closed, so they
// fall outside it. To is zero when the close was never seen.
type Window struct {
	From, To time.Time
}

// OK reports whether the run passed.
//
// 🔴 Both halves. `len(Unmet) == 0` alone is true of a script that did nothing.
func (r Result) OK() bool { return len(r.Unmet) == 0 && r.RingsDelivered > 0 }

// Counts is the terminal tally.
type Counts struct {
	Briefs       int
	Openers      int
	Posts        int
	Acks         map[string]int
	Sessions     map[string]int
	Closes       map[string]int
	Cancels      map[string]int
	Redeliveries int
	// ToolCalls is how many tool calls reached the host (#2349).
	ToolCalls int
	// ToolReruns is how many of those tool calls were re-runs of a recorded call
	// (#2454); a re-run is also counted in ToolCalls.
	ToolReruns int
	// MemoryOps is how many of each memory op reached the host (#2386).
	MemoryOps map[string]int
}

// events is everything the runner saw, in order.
type events struct {
	mu sync.Mutex

	briefs   []string
	openers  []string
	posts    []post
	closes   []closed
	cancels  []cancelled
	acks     []acked
	redeliv  []string
	restarts []Window

	changed chan struct{}
}

type post struct {
	requestID string
	chars     int
}
type closed struct {
	requestID  string
	outcome    contracts.Outcome
	errorClass string
}
type cancelled struct {
	requestID string
	reason    contracts.CancelReason
}
type acked struct {
	requestID string
	outcome   requests.AckOutcome
	session   requests.Session
}

func newEvents() *events { return &events{changed: make(chan struct{}, 64)} }

func (e *events) notify() {
	select {
	case e.changed <- struct{}{}:
	default:
	}
}

// Runner plays a script.
type Runner struct {
	script *fixture.Script
	host   *fixture.Host
	plugin Plugin
	events *events
	clock  func() time.Time

	// attachWait is how long Run waits for a plugin. A field so a test can shrink
	// it; AttachWait is the value every real run gets.
	attachWait time.Duration

	// lastRing is the request id the most recent `ring` step opened.
	//
	// 🔴 Tracked here rather than derived from delivered briefs. A ring that
	// arrives before a plugin attaches is HELD, not dropped — so the brief has
	// not been sent yet and a brief-derived "current request" is empty, which
	// makes every expectation after it match nothing. Found by the end-to-end
	// test, where the plugin necessarily connects after the daemon starts.
	lastRing string
	// lastOnConversation is the latest request id each conversation was rung
	// for, so a step can wait for THAT conversation's turn rather than for
	// whichever request happened to be rung most recently.
	lastOnConversation map[string]string
	ringMu             sync.Mutex

	// announce tells the run's driver something only it can do — restart the
	// harness, end a session inside it. Nil in a run with no driver, where
	// `restartHarness` and `endHarnessSession` fail. args are slog key/values.
	announce func(line string, args ...any)
}

// New builds a Runner.
func New(script *fixture.Script, host *fixture.Host, plugin Plugin) (*Runner, error) {
	if script == nil || host == nil {
		return nil, fmt.Errorf("runner: needs a script and a host")
	}
	return &Runner{
		script: script, host: host, plugin: plugin,
		events: newEvents(), clock: time.Now, attachWait: AttachWait,
		lastOnConversation: map[string]string{},
	}, nil
}

// SetAttachWait overrides how long Run waits for a plugin.
//
// 🔴 The default is a laptop's number. Since the `ready` frame, "attached"
// means "its harness can answer", so this waits for a whole harness to boot —
// and a cold container takes longer than 15s. A caller that knows it is on a
// slow machine says so rather than having the companion give up first and
// report that nothing attached.
func (r *Runner) SetAttachWait(wait time.Duration) {
	if wait > 0 {
		r.attachWait = wait
	}
}

// SetAnnounce sets how the runner asks the run's driver to act: the daemon logs
// the line, and the driver following that log acts on it.
func (r *Runner) SetAnnounce(announce func(line string, args ...any)) { r.announce = announce }

// Observer is the runner's view of the state machine.
func (r *Runner) Observer() requests.Observer { return (*observer)(r) }

type observer Runner

func (o *observer) BriefSent(requestID, _ string, _, _ int) {
	o.events.mu.Lock()
	o.events.briefs = append(o.events.briefs, requestID)
	o.events.mu.Unlock()
	o.events.notify()
}

func (o *observer) OpenerSent(requestID string, _, _, _, _ int) {
	o.events.mu.Lock()
	o.events.openers = append(o.events.openers, requestID)
	o.events.mu.Unlock()
	o.events.notify()
}

func (o *observer) Acked(requestID string, outcome requests.AckOutcome, session requests.Session) {
	o.events.mu.Lock()
	o.events.acks = append(o.events.acks, acked{requestID, outcome, session})
	o.events.mu.Unlock()
	o.events.notify()
}

func (o *observer) Posted(requestID string, chars int) {
	o.events.mu.Lock()
	o.events.posts = append(o.events.posts, post{requestID, chars})
	o.events.mu.Unlock()
	o.events.notify()
}

func (o *observer) Closed(requestID string, outcome contracts.Outcome, errorClass string) {
	o.events.mu.Lock()
	o.events.closes = append(o.events.closes, closed{requestID, outcome, errorClass})
	o.events.mu.Unlock()
	o.events.notify()
}

func (o *observer) Cancelled(requestID string, reason contracts.CancelReason) {
	o.events.mu.Lock()
	o.events.cancels = append(o.events.cancels, cancelled{requestID, reason})
	o.events.mu.Unlock()
	o.events.notify()
}

func (o *observer) Redelivered(requestID string, _ int) {
	o.events.mu.Lock()
	o.events.redeliv = append(o.events.redeliv, requestID)
	o.events.mu.Unlock()
	o.events.notify()
}

// Run plays every step and returns the verdict.
func (r *Runner) Run(ctx context.Context) (Result, error) {
	var unmet []string
	note := func(format string, args ...any) {
		unmet = append(unmet, fmt.Sprintf(format, args...))
	}

	if miss := r.awaitPlugin(ctx); miss != "" {
		// Returned rather than noted: every remaining step would assert over a wire
		// nobody is holding, and a page of misses hides the one that explains them.
		return r.result([]string{miss}), nil
	}

	for i, step := range r.script.Steps {
		if err := ctx.Err(); err != nil {
			return r.result(unmet), err
		}
		if miss := r.play(ctx, i, step); miss != "" {
			note("step %d (%s): %s", i, step.Step, miss)
		}
	}

	result := r.result(unmet)
	// 🔴 Load-bearing, unlike the early return above: `checkTally` calls `note`,
	// which appends — and an append may reallocate, so the slice captured by
	// `r.result` above is not the one `note` has been growing.
	r.checkTally(&result, note)
	result.Unmet = unmet
	return result, nil
}

// awaitPlugin blocks until a plugin has attached, and reports the miss when
// none does. A run with no plugin control (a unit test driving the runner
// directly) waits for nothing.
func (r *Runner) awaitPlugin(ctx context.Context) string {
	if r.plugin == nil {
		return ""
	}
	// Wall clock, not `r.clock`: the injected clock drives the SCRIPT's timeline
	// (deadlines, expiry), and this wait is about a real process connecting over
	// a real socket. `waitFor` is on the wall clock for the same reason.
	deadline := time.Now().Add(r.attachWait)
	for {
		if r.plugin.Attached() {
			return ""
		}
		if time.Now().After(deadline) {
			return fmt.Sprintf("no plugin attached within %s, so the script never started", r.attachWait)
		}
		select {
		case <-ctx.Done():
			return "the run ended before a plugin attached"
		case <-time.After(10 * time.Millisecond):
		}
	}
}

//nolint:gocyclo // one switch over the step vocabulary; splitting it would hide the timeline
func (r *Runner) play(ctx context.Context, index int, step fixture.Step) string {
	switch step.Step {
	case "ring":
		return r.ring(index, step)
	case "appendEntries":
		if err := r.host.AppendEntries(step.Conversation, step.Entries); err != nil {
			return err.Error()
		}
		return ""
	case "wait":
		r.sleep(ctx, time.Duration(step.Ms)*time.Millisecond)
		return ""
	case "expectAck":
		return r.expectAck(ctx, step)
	case "expectOpener":
		current := r.latestRequestID()
		return r.waitFor(ctx, step.WithinMs, func() bool { return r.openerSentFor(current) },
			"no opener was sent for "+current)
	case "expectNoOpener":
		// 🔴 Scoped to the CURRENT request, not to the whole run. The first cut
		// compared a total, which made this step usable only before any opener
		// had ever been sent — so a script with a fresh turn followed by a
		// resumed one could not express the very rule the step exists for. Found
		// by writing `fixture/basic.json`, which needs exactly that sequence.
		current := r.latestRequestID()
		if current == "" {
			return "no request to check for an opener"
		}
		if r.openerSentFor(current) {
			return "an opener was sent for " + current + "; a resumed session must not pay for one"
		}
		return ""
	case "expectPost":
		return r.expectPost(ctx, step)
	case "expectNoPost":
		current := r.latestRequestID()
		before := r.postsFor(current)
		r.sleep(ctx, r.within(step.ForMs))
		if r.postsFor(current) > before {
			return "a post arrived for " + current + " where none was expected"
		}
		return ""
	case "expectClose":
		return r.expectClose(ctx, step)
	case "expectOffset":
		return r.expectOffset(step)
	case "expectRedelivery":
		return r.waitFor(ctx, step.WithinMs, func() bool { return r.countRedeliveries() > 0 },
			"nothing was redelivered")
	case "injectCancel":
		return r.injectCancel(ctx, step)
	case "disconnectPlugin":
		if r.plugin == nil {
			return "the script disconnects the plugin, but this run has no plugin control"
		}
		return errorText(r.plugin.Disconnect())
	case "secondConnection":
		return r.secondConnection(ctx)
	case "refuseNextQuestion":
		r.host.RefuseNextQuestion()
		return ""
	case "restartHarness":
		return r.restartHarness(ctx, step)
	case "endHarnessSession":
		return r.endHarnessSession(ctx, step)
	case "expectToolCall":
		return r.expectToolCall(ctx, step)
	case "expectToolRerun":
		return r.expectToolRerun(ctx, step)
	case "expectMemory":
		return r.expectMemory(ctx, step)
	default:
		// Unreachable: Validate refuses unknown steps at load. Stated rather than
		// ignored, because a silently skipped step is a removed assertion.
		return "unknown step kind, which Validate should have refused"
	}
}

func (r *Runner) ring(index int, step fixture.Step) string {
	conversation, ok := r.script.Conversations[step.Conversation]
	if !ok {
		return "no such conversation"
	}
	deadline := time.Time{}
	if step.DeadlineMs > 0 {
		deadline = r.clock().Add(time.Duration(step.DeadlineMs) * time.Millisecond)
	}
	// 🔴 Where the platform would start (#2390): the offset the conversation's
	// last answered or empty close COMMITTED. The script's own `offset` is only
	// where the store began; ringing from it re-carried the whole conversation
	// into every brief, which the platform never does.
	since, err := r.host.OffsetOf(step.Conversation)
	if err != nil {
		return err.Error()
	}
	requestID := fmt.Sprintf("req_%d", index)
	r.ringMu.Lock()
	r.lastRing = requestID
	r.lastOnConversation[step.Conversation] = requestID
	r.ringMu.Unlock()

	envelope := contracts.Ring{
		RingID:         fmt.Sprintf("ring_%d", index),
		RequestID:      requestID,
		ConversationID: conversation.ID,
		Reason:         step.Reason,
		Cause:          contracts.Cause{EntryID: step.CauseEntry},
		Deadline:       deadline,
		SinceCursor:    since,
		ThroughCursor:  step.ThroughCursor,
		ResetSession:   step.ResetSession,
	}
	return errorText(r.host.DeliverRing(envelope))
}

func (r *Runner) expectAck(ctx context.Context, step fixture.Step) string {
	// 🔴 Scoped to the CURRENT request. Matching any ack in the run makes turn
	// two's expectation satisfiable by turn one's event — every step after the
	// first turn then passes instantly, and the script silently stops asserting
	// anything about the turns that follow.
	current := r.latestRequestID()
	matched := func() bool {
		r.events.mu.Lock()
		defer r.events.mu.Unlock()
		for _, ack := range r.events.acks {
			if ack.requestID != current {
				continue
			}
			if step.Outcome != "" && string(ack.outcome) != step.Outcome {
				continue
			}
			if step.Session != "" && string(ack.session) != step.Session {
				continue
			}
			return true
		}
		return false
	}
	return r.waitFor(ctx, step.WithinMs, matched,
		fmt.Sprintf("no ack with outcome %q and session %q", step.Outcome, step.Session))
}

func (r *Runner) expectPost(ctx context.Context, step fixture.Step) string {
	matcher := step.Matcher
	if matcher == "" {
		matcher = "nonEmpty"
	}
	current := r.latestRequestID()
	matched := func() bool {
		for _, sent := range r.host.Posts() {
			if sent.RequestID != current {
				continue
			}
			switch matcher {
			case "contains":
				if strings.Contains(sent.Text, step.Value) {
					return true
				}
			case "matches":
				if sent.Text == step.Value {
					return true
				}
			case "question":
				// A post that CARRIED a question (#2232) — the card, never words.
				if sent.Question != nil {
					return true
				}
			default:
				if sent.Text != "" {
					return true
				}
			}
		}
		return false
	}
	return r.waitFor(ctx, step.WithinMs, matched,
		fmt.Sprintf("no post satisfying %s %q", matcher, step.Value))
}

// expectToolCall waits for a tool call on the CURRENT request naming the tool,
// and — when the step says so — whose arguments contain a string (#2349).
func (r *Runner) expectToolCall(ctx context.Context, step fixture.Step) string {
	current := r.latestRequestID()
	matched := func() bool {
		for _, call := range r.host.ToolCalls() {
			if call.RequestID != current || call.Name != step.Name {
				continue
			}
			if step.ArgumentsContain == "" || strings.Contains(string(call.Arguments), step.ArgumentsContain) {
				return true
			}
		}
		return false
	}
	return r.waitFor(ctx, step.WithinMs, matched,
		fmt.Sprintf("no call of tool %q with arguments containing %q", step.Name, step.ArgumentsContain))
}

// expectToolRerun waits for a re-run of a recorded call on the CURRENT request
// (#2454): the plugin's re-run tool reaching the platform side.
func (r *Runner) expectToolRerun(ctx context.Context, step fixture.Step) string {
	current := r.latestRequestID()
	matched := func() bool {
		for _, call := range r.host.ToolCalls() {
			if call.RequestID == current && call.RerunOf != "" {
				return true
			}
		}
		return false
	}
	return r.waitFor(ctx, step.WithinMs, matched, "no re-run of a recorded tool call")
}

// expectMemory waits for a memory op on the CURRENT request (#2386): the harness's
// own tool reaching the platform, or its prefetch before a turn.
func (r *Runner) expectMemory(ctx context.Context, step fixture.Step) string {
	current := r.latestRequestID()
	matched := func() bool {
		for _, call := range r.host.MemoryCalls() {
			if call.RequestID == current && string(call.Op) == step.Op {
				return true
			}
		}
		return false
	}
	return r.waitFor(ctx, step.WithinMs, matched, fmt.Sprintf("no memory %s on the current request", step.Op))
}

// expectClose waits for a request to close, optionally for a NAMED conversation
// rather than the one rung most recently.
//
// 🔴 The conversation form is what makes a script safe against a slow harness.
// Pacing the next ring with a fixed `wait` is a guess about how long a turn
// takes: against a model mock it is milliseconds, and against a real one it
// ranged from 3s to 83s in a single run (#2014). When the guess is short, the
// next ring on the SAME conversation supersedes the turn still running, and the
// script fails on a close outcome and an offset — reading as a defect in the
// companion or the plugin, which it is not. Waiting for the close instead makes
// the run take exactly as long as the harness needs, and keeps a ring on ANOTHER
// conversation free to overlap, which is the concurrency the script is here to
// exercise.
func (r *Runner) expectClose(ctx context.Context, step fixture.Step) string {
	current := r.latestRequestID()
	where := ""
	if step.Conversation != "" {
		current = r.latestRequestOn(step.Conversation)
		where = " on " + step.Conversation
		if current == "" {
			return "no ring has been sent for " + step.Conversation
		}
	}
	matched := func() bool {
		r.events.mu.Lock()
		defer r.events.mu.Unlock()
		for _, closed := range r.events.closes {
			if closed.requestID != current {
				continue
			}
			if step.Outcome == "" || string(closed.outcome) == step.Outcome {
				return true
			}
		}
		return false
	}
	return r.waitFor(ctx, step.WithinMs, matched, "no close with outcome "+step.Outcome+where)
}

// latestRequestOn is the request id the most recent ring for a conversation
// opened, or "" if that conversation has never been rung.
func (r *Runner) latestRequestOn(conversation string) string {
	r.ringMu.Lock()
	defer r.ringMu.Unlock()
	return r.lastOnConversation[conversation]
}

func (r *Runner) expectOffset(step fixture.Step) string {
	got, err := r.host.OffsetOf(step.Conversation)
	if err != nil {
		return err.Error()
	}
	if got != step.Cursor {
		return fmt.Sprintf("the offset is %q, want %q", got, step.Cursor)
	}
	return ""
}

func (r *Runner) injectCancel(ctx context.Context, step fixture.Step) string {
	if step.AfterFirstPost {
		if miss := r.waitFor(ctx, step.WithinMs, func() bool { return r.countPosts() > 0 },
			"no post to cancel after"); miss != "" {
			return miss
		}
	}
	if step.AfterMs > 0 {
		r.sleep(ctx, time.Duration(step.AfterMs)*time.Millisecond)
	}
	reason := wire.CancelReasonFromWire(step.Reason)
	if reason == "" {
		reason = contracts.CancelStop
	}
	open := r.openRequestID()
	if open == "" {
		return "no open request to cancel"
	}
	return errorText(r.host.DeliverCancel(open, reason))
}

func (r *Runner) secondConnection(ctx context.Context) string {
	if r.plugin == nil {
		return "the script opens a second connection, but this run has no plugin control"
	}
	reason, err := r.plugin.DialSecond(ctx)
	if err != nil {
		return err.Error()
	}
	if reason != "already_attached" {
		return fmt.Sprintf("the second connection closed with %q, want \"already_attached\"", reason)
	}
	return ""
}

// restartHarness asks the driver to restart the harness, then waits until a NEW
// harness process has had the open request redelivered (#2234, #2235).
//
// 🔴 The redelivery is the evidence, not the reconnect: a plugin that reconnects
// from the SAME process is still working on its turn and gets nothing again, so
// waiting for an attach alone would pass on a driver that restarted nothing.
func (r *Runner) restartHarness(ctx context.Context, step fixture.Step) string {
	if r.announce == nil {
		return "the script restarts the harness, but this run has no driver to do it"
	}
	before := r.countRedeliveries()
	window := Window{From: r.clock()}
	r.announce(RestartHarnessLine)
	miss := r.waitFor(ctx, step.WithinMs, func() bool {
		if window.To.IsZero() && r.plugin != nil && !r.plugin.Attached() {
			window.To = r.clock()
		}
		return r.countRedeliveries() > before
	}, "no request was redelivered to a restarted harness")
	r.events.mu.Lock()
	r.events.restarts = append(r.events.restarts, window)
	r.events.mu.Unlock()
	return miss
}

// endHarnessSession asks the driver to end the conversation's session inside
// the harness (#2414), then gives it `withinMs` to do so.
//
// 🔴 Nothing here can see the session end — it lives in the harness — so this
// step is never the evidence. The script's NEXT ring is: a plugin that reads its
// harness's routing acks it `fresh` and earns an opener (`expectAck` with
// `session: fresh`, then `expectOpener`), and one that read a stale lookup acks
// `resumed`. A driver that ended nothing fails those expectations, not this one.
func (r *Runner) endHarnessSession(ctx context.Context, step fixture.Step) string {
	if r.announce == nil {
		return "the script ends a harness session, but this run has no driver to do it"
	}
	conversation, ok := r.script.Conversations[step.Conversation]
	if !ok {
		// Unreachable: Validate refuses an unknown conversation at load.
		return fmt.Sprintf("conversation %q is not in the script", step.Conversation)
	}
	r.announce(EndHarnessSessionLine, "conversation", conversation.ID)
	r.sleep(ctx, r.within(step.WithinMs))
	return ""
}

// waitFor polls a condition until it holds or the window closes.
func (r *Runner) waitFor(ctx context.Context, withinMs int, holds func() bool, miss string) string {
	deadline := time.Now().Add(r.within(withinMs))
	for {
		if holds() {
			return ""
		}
		if time.Now().After(deadline) {
			return miss
		}
		select {
		case <-ctx.Done():
			return miss
		case <-r.events.changed:
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (r *Runner) within(ms int) time.Duration {
	if ms <= 0 {
		return DefaultWait
	}
	return time.Duration(ms) * time.Millisecond
}

func (r *Runner) sleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// latestRequestID is the request the most recent `ring` step opened.
func (r *Runner) latestRequestID() string {
	r.ringMu.Lock()
	defer r.ringMu.Unlock()
	return r.lastRing
}

// openerSentFor reports whether THIS request earned an opener.
func (r *Runner) openerSentFor(requestID string) bool {
	r.events.mu.Lock()
	defer r.events.mu.Unlock()
	for _, sent := range r.events.openers {
		if sent == requestID {
			return true
		}
	}
	return false
}

// postsFor counts the posts forwarded for one request.
func (r *Runner) postsFor(requestID string) int {
	count := 0
	for _, sent := range r.host.Posts() {
		if sent.RequestID == requestID {
			count++
		}
	}
	return count
}

func (r *Runner) countPosts() int {
	r.events.mu.Lock()
	defer r.events.mu.Unlock()
	return len(r.events.posts)
}

func (r *Runner) countRedeliveries() int {
	r.events.mu.Lock()
	defer r.events.mu.Unlock()
	return len(r.events.redeliv)
}

// openRequestID is the most recent request with no close.
func (r *Runner) openRequestID() string {
	r.events.mu.Lock()
	defer r.events.mu.Unlock()
	closedIDs := map[string]bool{}
	for _, c := range r.events.closes {
		closedIDs[c.requestID] = true
	}
	for i := len(r.events.briefs) - 1; i >= 0; i-- {
		if !closedIDs[r.events.briefs[i]] {
			return r.events.briefs[i]
		}
	}
	return ""
}

func (r *Runner) result(unmet []string) Result {
	r.events.mu.Lock()
	defer r.events.mu.Unlock()

	counts := Counts{
		Briefs: len(r.events.briefs), Openers: len(r.events.openers),
		Posts: len(r.events.posts), Redeliveries: len(r.events.redeliv),
		Acks: map[string]int{}, Sessions: map[string]int{},
		Closes: map[string]int{}, Cancels: map[string]int{},
		ToolCalls: len(r.host.ToolCalls()),
		MemoryOps: map[string]int{},
	}
	for _, call := range r.host.ToolCalls() {
		if call.RerunOf != "" {
			counts.ToolReruns++
		}
	}
	for _, call := range r.host.MemoryCalls() {
		counts.MemoryOps[string(call.Op)]++
	}
	for _, ack := range r.events.acks {
		counts.Acks[string(ack.outcome)]++
		counts.Sessions[string(ack.session)]++
	}
	for _, c := range r.events.closes {
		counts.Closes[string(c.outcome)]++
	}
	for _, c := range r.events.cancels {
		counts.Cancels[wire.CancelReasonOnWire(c.reason)]++
	}
	return Result{
		RingsDelivered: r.host.RingsDelivered(), Unmet: unmet, Counts: counts,
		Restarts: append([]Window(nil), r.events.restarts...),
	}
}

// checkTally compares the terminal assert block with what happened.
func (r *Runner) checkTally(result *Result, note func(string, ...any)) {
	want := r.script.Assert
	if want.RingsDelivered > 0 && result.RingsDelivered != want.RingsDelivered {
		note("assert: %d ring(s) delivered, want %d", result.RingsDelivered, want.RingsDelivered)
	}
	if want.OpenersSent > 0 && result.Counts.Openers != want.OpenersSent {
		note("assert: %d opener(s), want %d", result.Counts.Openers, want.OpenersSent)
	}
	if want.Posts > 0 && result.Counts.Posts != want.Posts {
		note("assert: %d post(s), want %d", result.Counts.Posts, want.Posts)
	}
	if want.ToolCalls != nil && result.Counts.ToolCalls != *want.ToolCalls {
		note("assert: %d tool call(s), want %d", result.Counts.ToolCalls, *want.ToolCalls)
	}
	if want.ToolReruns != nil && result.Counts.ToolReruns != *want.ToolReruns {
		note("assert: %d tool re-run(s), want %d", result.Counts.ToolReruns, *want.ToolReruns)
	}
	compare("ack", want.Acks, result.Counts.Acks, note)
	compare("session", want.Sessions, result.Counts.Sessions, note)
	compare("close", want.Closes, result.Counts.Closes, note)
	compare("cancel", want.Cancels, result.Counts.Cancels, note)
	compare("memory op", want.MemoryOps, result.Counts.MemoryOps, note)
}

func compare(label string, want, got map[string]int, note func(string, ...any)) {
	for key, wanted := range want {
		if got[key] != wanted {
			note("assert: %d %s %q, want %d", got[key], label, key, wanted)
		}
	}
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
