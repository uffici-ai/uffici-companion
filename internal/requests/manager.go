package requests

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
)

// Brief is what the manager hands the plugin when a request opens.
type Brief struct {
	RequestID      string
	ConversationID string
	Reason         string
	Cause          contracts.Cause
	Deadline       time.Time
	SinceCursor    string
	ThroughCursor  string
	ResetSession   bool
	Delta          []contracts.Entry
	// Context is what the agent is told before the conversation (#2331), as the
	// platform composed it; nil when it sent none.
	Context *contracts.Context
	// ContextChanged names the levels a person changed since the agent last
	// answered here (2026-10-12); empty when nothing changed.
	ContextChanged []contracts.ContextChange
}

// PluginSink is the attached plugin, as the manager sees it.
//
// 🔴 An interface, so every rule in this file is tested with no socket: the
// supersede cancel, the expiry cancel, the opener that must follow a fresh ack
// and must not follow a resumed one. Those are the rules that are wrong
// silently, and they are unreachable through an integration test alone.
type PluginSink interface {
	SendBrief(ctx context.Context, brief Brief) error
	SendOpener(ctx context.Context, requestID string, entries []contracts.Entry) error
	SendCancel(ctx context.Context, requestID string, reason contracts.CancelReason) error
	// SendFailure withdraws a request the platform refused or failed, relaying
	// the class and the instruction (#2077, decision 32).
	SendFailure(ctx context.Context, requestID, class, instruction string) error
}

// ReleaseSink is a plugin sink that can say a conversation was released
// (#2356). Optional: a sink without it simply hears nothing, and the next
// arrival's `resetSession` starts the harness fresh anyway.
type ReleaseSink interface {
	SendRelease(ctx context.Context, conversationID string, reason contracts.ReleaseReason) error
}

// Observer is told what happened, for the fixture runner's expectations and for
// the surface's one-line-per-frame log.
//
// 🔴 It receives COUNTS and ids, never text. The log line is built from what
// lands here, so a field carrying a post body or a delta would put transcript
// content into a log by construction rather than by accident.
type Observer interface {
	BriefSent(requestID, conversationID string, deltaEntries, deltaChars int)
	OpenerSent(requestID string, entries, chars, budgetDeclared, summaryEntries int)
	Acked(requestID string, outcome AckOutcome, session Session)
	Posted(requestID string, chars int)
	Closed(requestID string, outcome contracts.Outcome, errorClass string)
	Cancelled(requestID string, reason contracts.CancelReason)
	Redelivered(requestID string, attempt int)
}

// NopObserver ignores everything, for callers that want no observability.
type NopObserver struct{}

// BriefSent ignores a brief.
func (NopObserver) BriefSent(string, string, int, int) {}

// OpenerSent ignores an opener.
func (NopObserver) OpenerSent(string, int, int, int, int) {}

// Acked ignores an acknowledgement.
func (NopObserver) Acked(string, AckOutcome, Session) {}

// Posted ignores a post.
func (NopObserver) Posted(string, int) {}

// Closed ignores a close.
func (NopObserver) Closed(string, contracts.Outcome, string) {}

// Cancelled ignores a cancel.
func (NopObserver) Cancelled(string, contracts.CancelReason) {}

// Redelivered ignores a redelivery.
func (NopObserver) Redelivered(string, int) {}

// closedMemory is how many closed request ids the manager keeps.
//
// One open request per conversation, and a plugin talks about one request at a
// time, so this is orders of magnitude more than the in-flight set — sized for
// "long after any frame could still be in flight", not for a workload.
const closedMemory = 256

// Manager runs the state machine for every conversation.
type Manager struct {
	host     contracts.AgentsHost
	limits   Limits
	observer Observer
	now      func() time.Time
	prefetch PrefetchFunc
	report   func(requestID string, err error)
	// background counts the deliveries running off their caller's goroutine
	// (a brief with attachments to prefetch); tests wait on it.
	background sync.WaitGroup

	mu sync.Mutex
	// byID and byConversation are two views of the same requests. The second is
	// what enforces one open request per conversation.
	byID           map[string]*Request
	byConversation map[string]*Request
	// connOf records which connection received each brief, so a frame arriving
	// on another one earns `wrong_connection`.
	connOf map[string]string
	// closed remembers the ids this companion has closed, so a frame that
	// crossed the close is ignored rather than fatal (#2023). Bounded and
	// insertion-ordered: `closedOrder` is a ring of at most closedMemory ids,
	// and the oldest leaves as the newest arrives. The plugin keeps the mirror
	// of this for cancels it receives after finishing (`CLOSED_MEMORY`).
	closed      map[string]struct{}
	closedOrder []string
	// over remembers EVERY request this companion saw end, however it ended, so a
	// ring the gateway redelivers after the close is ignored rather than opening
	// the request again (#2077). Separate from `closed`, whose meaning is the
	// frame rule of #2023: a request the PLUGIN closed must keep earning
	// `unknown_request` for a late frame.
	over      map[string]struct{}
	overOrder []string

	sink   PluginSink
	connID string
	// processID is the harness process the plugin last attached for (#2234):
	// a different one on the next attach is a restart, not a reconnect.
	processID string
	// budget is the context budget the attached plugin declared in hello.
	budget int
}

// Options configure a Manager. Zero values take the defaults.
type Options struct {
	Limits   Limits
	Observer Observer
	// Now is the clock, injected so deadline and timeout tests do not sleep.
	Now func() time.Time
	// Prefetch, optional, is handed a brief's delta before the brief is sent
	// (#2673) and returns it with what it fetched marked on the attachments. It
	// runs for at most PrefetchWindow, OFF the goroutine that delivered the ring;
	// a request withdrawn while it ran is not briefed.
	Prefetch PrefetchFunc
	// Report, optional, hears a delivery that failed off its caller's goroutine
	// — what OnRing would otherwise have returned.
	Report func(requestID string, err error)
}

// PrefetchMaxWindow bounds a brief's prefetch, whatever the request's deadline.
const PrefetchMaxWindow = 15 * time.Second

// PrefetchWindow is how long a brief's prefetch may run: a quarter of what is
// left of the request, and never more than PrefetchMaxWindow — an attachment
// that takes longer is handed over without its local path, never a brief that
// misses its turn.
func PrefetchWindow(now, deadline time.Time) time.Duration {
	window := PrefetchMaxWindow
	if !deadline.IsZero() {
		window = min(window, deadline.Sub(now)/4)
	}
	return max(window, 0)
}

// PrefetchFunc prepares a brief's entries — the file transfer engine's prefetch.
type PrefetchFunc func(ctx context.Context, requestID string, entries []contracts.Entry) []contracts.Entry

// New builds a Manager over an AgentsHost.
func New(host contracts.AgentsHost, options Options) *Manager {
	observer := options.Observer
	if observer == nil {
		observer = NopObserver{}
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Manager{
		host:           host,
		limits:         options.Limits.withDefaults(),
		observer:       observer,
		prefetch:       options.Prefetch,
		report:         options.Report,
		now:            now,
		byID:           map[string]*Request{},
		closed:         map[string]struct{}{},
		over:           map[string]struct{}{},
		byConversation: map[string]*Request{},
		connOf:         map[string]string{},
	}
}

// Attach registers the one plugin connection.
func (m *Manager) Attach(connID string, sink PluginSink, budgetTokens int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sink = sink
	m.connID = connID
	m.budget = budgetTokens
}

// BindProcess records which harness PROCESS the attached plugin speaks for, and
// answers how many requests it handed back for delivery (#2234).
//
// 🔴 A reconnect and a restart look identical at the connection: both are a new
// plugin connection after the old one ended. They are opposite for an
// acknowledged request. A reconnect of the same process is still working on it,
// so it stays open and its reply is accepted on the new connection. A restarted
// process holds nothing of the old one's turn, so nobody is working on it — on
// a real Hermes machine it sat silent until its deadline. The plugin's `processId` tells the
// two apart: a DIFFERENT id moves every acknowledged request back to delivered,
// so the attach's RedeliverOpen sends it again, counted as a redelivery — a
// harness that restarts on every brief still ends in `redelivery_exhausted`.
//
// An empty id (an older plugin) changes nothing and forgets nothing: the old
// rule, never a redelivery. Call it after Attach and before RedeliverOpen.
func (m *Manager) BindProcess(processID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if processID == "" {
		return 0
	}
	previous := m.processID
	m.processID = processID
	if previous == "" || previous == processID {
		// The first process this companion has seen holds everything it has, and a
		// reconnect of the same one is still working.
		return 0
	}
	reopened := 0
	for _, request := range m.byID {
		switch request.State {
		case StateAcked, StateOpening, StatePosting:
			if m.advanceLocked(request, StateDelivered) {
				// The new process answers from the start; the old one's posts do not
				// count against the new answer's cap.
				request.Posts = 0
				// 🔴 And its ack clock starts now. Kept at the dead process's delivery,
				// the ack sweep judged the request overdue in the moment before the
				// attach's RedeliverOpen restamped it and redelivered it as well: two
				// briefs to the new harness, two redeliveries spent per restart (seen
				// on a real Hermes machine). Still non-zero, so the redelivery counts as one.
				request.DeliveredAt = m.now()
				reopened++
			}
		}
	}
	return reopened
}

// Detach forgets the plugin connection. Requests that were never acknowledged
// are redelivered when a plugin attaches again; acknowledged ones stay open,
// because the harness may still be working.
func (m *Manager) Detach(connID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 🔴 The purge is UNCONDITIONAL, before the identity check. `connOf` is what
	// `requestOn` uses to refuse a frame from the WRONG connection, so an entry
	// left pointing at a dead connection makes the NEXT plugin's post for that
	// request illegal: the surface closes with `wrong_connection`, the plugin
	// reconnects, posts again, and the turn dies in that loop.
	//
	// A detaching connection owns nothing, whether or not it is still the
	// ATTACHED one — and it sometimes is not: the surface clears its own
	// attachment before calling here, so a reconnect that claims and says
	// `ready` in that gap has already called Attach with the new id. Purging
	// after the identity check would then skip the very entries this exists to
	// remove.
	//
	// Latent until acknowledgement moved to receipt (#2014): a resumed request
	// used to spend a slow `handle_message` in StateDelivered, which a reconnect
	// re-owns through RedeliverOpen. Acking on receipt moves it past that state
	// while the connection can still drop underneath it.
	for requestID, owner := range m.connOf {
		if owner == connID {
			delete(m.connOf, requestID)
		}
	}

	if m.connID != connID {
		return
	}
	m.sink = nil
	m.connID = ""
}

// Attached reports whether a plugin is connected.
func (m *Manager) Attached() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sink != nil
}

// OnRing opens a request, superseding whatever was open in that conversation.
func (m *Manager) OnRing(ctx context.Context, ring contracts.Ring) error {
	m.mu.Lock()
	// 🔴 A REDELIVERY is not a new request (#2077). The gateway redelivers a ring
	// it saw no ack for — the socket dropped before the ack, or the ack was late —
	// under a NEW ring id and the SAME request id. Read as a new ring, it would
	// supersede its own request: the plugin cancelled, the request closed
	// `cancelled` upstream, and the answer lost with nobody told. The request this
	// companion already holds is refreshed instead; if it was never briefed, the
	// attach or the ack sweep briefs it, as for any held request.
	if held, known := m.byID[ring.RequestID]; known && !held.State.Terminal() {
		held.RingID = ring.RingID
		m.mu.Unlock()
		return nil
	}
	if _, ended := m.over[ring.RequestID]; ended {
		m.mu.Unlock()
		return nil
	}
	existing, busy := m.byConversation[ring.ConversationID]
	since := ring.SinceCursor
	if busy {
		// 🔴 THE UNION RANGE. The superseded request never closed as answered, so
		// its offset never moved and the entries it covered are still unanswered.
		// Starting the new delta at the NEW ring's cursor would skip exactly those
		// — the member's earlier message would never reach any agent. Taking the
		// older lower bound is what makes supersede lossless.
		// 🔴 An EMPTY cursor is the lowest bound there is — "from the beginning of
		// the conversation", not "unknown". So the union keeps an empty `since`
		// rather than replacing it, and prefers an empty `existing` over any
		// cursor. Written the other way round this narrowed exactly the case it
		// exists to widen. The fixture's rings always derive `since` from the
		// conversation's frozen offset, so both values are equal while a request
		// is busy and the mistake is invisible there — but the rule belongs to
		// this state machine, not to whichever host happens to be ringing.
		if since != "" && (existing.SinceCursor == "" || existing.SinceCursor < since) {
			since = existing.SinceCursor
		}
	}
	m.mu.Unlock()

	if busy {
		if err := m.supersede(ctx, existing); err != nil {
			return err
		}
	}

	request := &Request{
		RequestID:      ring.RequestID,
		ConversationID: ring.ConversationID,
		RingID:         ring.RingID,
		Reason:         ring.Reason,
		Cause:          ring.Cause,
		SinceCursor:    since,
		ThroughCursor:  ring.ThroughCursor,
		ResetSession:   ring.ResetSession,
		Deadline:       ring.Deadline,
		State:          StateDelivered,
	}

	m.mu.Lock()
	m.byID[request.RequestID] = request
	m.byConversation[request.ConversationID] = request
	m.mu.Unlock()

	return m.deliver(ctx, request)
}

// deliver reads the delta and the context, and sends the brief.
func (m *Manager) deliver(ctx context.Context, request *Request) error {
	m.mu.Lock()
	budget := contextBudget(m.budget)
	request.briefing = true
	m.mu.Unlock()
	read, err := m.host.ReadSince(ctx, request.ConversationID, request.SinceCursor, request.ThroughCursor, budget)
	if err != nil {
		m.doneBriefing(request)
		// 🔴 Closed as an error, never left to expire: an expiry closes as
		// `cancelled`, which tells the person nothing, and the host has already
		// retried what could be retried. The error close is what puts "the agent
		// did not answer" in front of them.
		return m.fail(ctx, request, fmt.Errorf("reading the delta for %s: %w", request.RequestID, err))
	}

	m.mu.Lock()
	sink, connID := m.sink, m.connID
	withdrawn := request.State.Terminal()
	if sink == nil || withdrawn {
		// 🔴 Cleared in the SAME critical section that found no sink: an Attach
		// after this point is followed by a RedeliverOpen that sees the request
		// unmarked and briefs it, so a held ring is never stranded (#2372).
		request.briefing = false
	}
	m.mu.Unlock()
	if withdrawn {
		// Cancelled while its delta was read: the plugin was told, so no brief
		// follows the cancel (#2373). A cancel landing during the send itself
		// cannot be stopped here; advanceLocked drops that one afterwards.
		return nil
	}
	if sink == nil {
		// No plugin attached: the request stays open and is delivered when one
		// arrives. It is NOT an error — a harness restarting is ordinary.
		return nil
	}

	if m.prefetch != nil && hasAttachments(read.Entries) {
		// 🔴 OFF the caller's goroutine (#2673 self-review H4). A ring is
		// delivered on the doorbell's one dispatch goroutine, which also carries
		// every other ring and the platform's cancel — a person's stop (Art. 14).
		// A prefetch held there would hold the stop behind a slow download. The
		// request keeps its `briefing` mark meanwhile, so nothing else briefs it.
		m.background.Add(1)
		go func() {
			defer m.background.Done()
			if err := m.prefetchThenBrief(ctx, request, read, sink, connID); err != nil && m.report != nil {
				m.report(request.RequestID, err)
			}
		}()
		return nil
	}
	return m.brief(ctx, request, read, sink, connID, read.Entries)
}

// prefetchThenBrief fetches a brief's attachments for at most PrefetchWindow,
// then sends the brief — unless the request was withdrawn meanwhile.
func (m *Manager) prefetchThenBrief(ctx context.Context, request *Request, read contracts.Read, sink PluginSink, connID string) error {
	prefetchCtx, cancel := context.WithTimeout(ctx, PrefetchWindow(m.now(), request.Deadline))
	defer cancel()
	m.mu.Lock()
	if request.State.Terminal() {
		// Withdrawn before the prefetch began: nothing to fetch for.
		request.briefing = false
		m.mu.Unlock()
		return nil
	}
	request.stopPrefetch = cancel
	m.mu.Unlock()
	entries := m.prefetch(prefetchCtx, request.RequestID, read.Entries)
	m.mu.Lock()
	request.stopPrefetch = nil
	withdrawn := request.State.Terminal()
	if withdrawn {
		request.briefing = false
	}
	m.mu.Unlock()
	if withdrawn {
		// Cancelled while its attachments were fetched: the cancel reached the
		// plugin, and the transfers were aborted with it.
		return nil
	}
	return m.brief(ctx, request, read, sink, connID, entries)
}

// hasAttachments reports whether a delta carries an attachment to prefetch.
func hasAttachments(entries []contracts.Entry) bool {
	for _, entry := range entries {
		for _, attachment := range entry.Attachments {
			if attachment.ID != "" {
				return true
			}
		}
	}
	return false
}

// brief sends a brief over the sink chosen at delivery.
func (m *Manager) brief(ctx context.Context, request *Request, read contracts.Read, sink PluginSink, connID string, entries []contracts.Entry) error {
	brief := Brief{
		RequestID:      request.RequestID,
		ConversationID: request.ConversationID,
		Reason:         request.Reason,
		Cause:          request.Cause,
		Deadline:       request.Deadline,
		SinceCursor:    request.SinceCursor,
		ThroughCursor:  request.ThroughCursor,
		ResetSession:   request.ResetSession,
		Delta:          entries,
		Context:        read.Context,
		ContextChanged: read.ContextChanged,
	}
	if err := sink.SendBrief(ctx, brief); err != nil {
		// 🔴 A send that failed because ANOTHER connection replaced this one keeps
		// its claim and goes out on the new one (#2372): that connection's
		// RedeliverOpen skipped the request while this send was in flight, so
		// nothing else would — it would sit until its deadline and expire. A
		// request cancelled while the send was blocked is owed nothing (#2373).
		m.mu.Lock()
		withdrawn := request.State.Terminal()
		replaced := m.sink != nil && m.connID != connID && !withdrawn
		if !replaced {
			request.briefing = false
		}
		m.mu.Unlock()
		if replaced {
			return m.deliver(ctx, request)
		}
		if withdrawn {
			// The cancel already reached the plugin; the lost send is not a
			// failure of anything still owed (#2373).
			return nil
		}
		return fmt.Errorf("sending the brief for %s: %w", request.RequestID, err)
	}

	m.mu.Lock()
	request.briefing = false
	if request.State.Terminal() {
		// Withdrawn while the brief was on the wire: nothing more to do for it.
		m.mu.Unlock()
		return nil
	}
	// 🔴 #2694: a request is created `delivered`, and the plugin may have
	// answered already — a local socket acks before this goroutine gets the
	// lock back. Moving it to `delivered` here undid that ack, and the ack sweep
	// then briefed the harness a second time. The state is left as it is.
	request.DeliveredAt = m.now()
	request.ContextTokens = contextTokens(read.Context)
	// 🔴 The owner only while it is still the attached connection (#2694
	// review): Detach purged this connection's entries if it went during the
	// send, and writing it back refused the reconnected plugin's frames as
	// `wrong_connection`. A replaced send records its owner when it re-briefs.
	if m.connID == connID {
		m.connOf[request.RequestID] = connID
	}
	// 🔴 #2694: the connection was replaced while this send was in flight, and
	// the request is still unanswered — un-acked, or reopened by BindProcess for
	// a new harness process. The attach's RedeliverOpen skipped it as still
	// briefing, so nothing else would brief the new connection: this send does.
	// An acked request a same-process reconnect kept is not re-briefed.
	// redeliver claims it under its own lock, so a RedeliverOpen racing this one
	// still briefs it once.
	replaced := m.sink != nil && m.connID != connID && request.State == StateDelivered
	m.mu.Unlock()

	// An ack that landed during the send is observed BEFORE this line, so the
	// frame log may show the ack ahead of its brief (#2694 review); both happened.
	m.observer.BriefSent(request.RequestID, request.ConversationID, len(read.Entries), charsOf(read.Entries))
	if replaced {
		return m.redeliver(ctx, request)
	}
	return nil
}

// doneBriefing clears a delivery's in-flight mark on a path that sent nothing.
func (m *Manager) doneBriefing(request *Request) {
	m.mu.Lock()
	request.briefing = false
	m.mu.Unlock()
}

// supersede cancels an open request and closes it upstream as cancelled.
func (m *Manager) supersede(ctx context.Context, request *Request) error {
	return m.cancel(ctx, request, contracts.CancelSuperseded)
}

// cancel withdraws a request from the plugin and closes it upstream.
//
// 🔴 The upstream close is what keeps the offset where it was: a stop or a
// supersede goes as `cancelled` and an expiry as `error`, neither of which
// advances. A supersede that closed as answered would lose every entry the
// superseded request covered.
func (m *Manager) cancel(ctx context.Context, request *Request, reason contracts.CancelReason) error {
	m.mu.Lock()
	if request.State.Terminal() {
		m.mu.Unlock()
		return nil
	}
	switch reason {
	case contracts.CancelExpired:
		request.State = StateExpired
	default:
		request.State = StateCancelled
	}
	// 🔴 The outcome of the state THIS call set, read under the lock (#2112): a
	// frame handled while the sends below are on the wire must not decide it.
	outcome := OutcomeFor(request.State, contracts.OutcomeCancelled)
	sink := m.sink
	// 🔴 An expiry closes upstream as an ERROR with a class the platform turns into
	// the "did not answer" note (#2842): `no_runtime` when no plugin is attached —
	// a ring held on a machine whose harness is down, the incident's shape — and
	// `timeout` when a plugin took the brief and never finished (the platform reads
	// it as `harness_error`). Decided HERE, under the lock, from the sink this
	// companion holds at the moment of the expiry. 🔴 Except a request the agent
	// already POSTED on: the platform's own expiry writes the note only when
	// nothing was posted ("a did-not-answer note beside an answer is a lie"), so a
	// slow answer that ran out of time closes `cancelled` — quiet, the offset
	// unmoved — exactly as the platform would have closed it. A stop and a
	// supersede close `cancelled` too; the class below is IGNORED on a `cancelled`
	// close (`CloseRequest` sends it only with `error`), so the plugin's reason
	// rides along for the log and nothing else.
	errorClass := string(reason)
	if reason == contracts.CancelExpired {
		if request.Posted {
			outcome = contracts.OutcomeCancelled
		} else {
			errorClass = "timeout"
			if sink == nil {
				errorClass = "no_runtime"
			}
		}
	}
	m.release(request)
	delete(m.connOf, request.RequestID)
	// 🔴 Remembered HERE, beside the state transition, not after the two I/O
	// calls below (#2023). The moment the state goes terminal, `requestOn`
	// refuses every frame for this request — and the plugin's `done` may already
	// be on the wire, being read by another goroutine while this one is inside
	// `SendCancel` and `CloseRequest`. `CloseRequest` is a network call in
	// production, so that window is milliseconds, not microseconds: remembering
	// after it leaves the race open for the ordering that actually happens.
	m.remember(request.RequestID)
	m.mu.Unlock()

	// 🔴 NOTHING here returns early. The state is already terminal and the
	// request is already out of `byConversation`, so a return before the cleanup
	// below strands it: `Tick` skips terminal requests, so nothing ever revisits
	// it, `byID` grows for the life of the process, and — far worse — the
	// PLATFORM is never told the request closed, so it waits on a turn nobody is
	// answering. Telling the plugin is the step allowed to fail; telling the host
	// and forgetting the request are not conditional on it.
	//
	// The realistic trigger is ordinary: a plugin's connection dies just as one
	// of its requests hits the expiry sweep, and the send fails on a socket that
	// is already going away.
	var sendErr error
	if sink != nil {
		sendErr = sink.SendCancel(ctx, request.RequestID, reason)
	}
	m.observer.Cancelled(request.RequestID, reason)

	closeErr := m.host.CloseRequest(ctx, request.RequestID, outcome, errorClass)
	if closeErr == nil {
		m.observer.Closed(request.RequestID, outcome, errorClass)
	}

	// Forgotten either way, exactly as closeUpstream does it: an entry kept
	// because its close failed is an entry kept forever. (Its id went into the
	// closed memory above, before the sends, not here.)
	m.mu.Lock()
	delete(m.byID, request.RequestID)
	m.mu.Unlock()

	if sendErr != nil {
		return fmt.Errorf("sending the cancel for %s: %w", request.RequestID, sendErr)
	}
	if closeErr != nil {
		return fmt.Errorf("closing %s upstream: %w", request.RequestID, closeErr)
	}
	return nil
}

// OnAck records the plugin's answer and, for a fresh session, sends the opener.
func (m *Manager) OnAck(ctx context.Context, connID, requestID string, outcome AckOutcome, session Session) error {
	request, err := m.requestOn(connID, requestID)
	if err != nil {
		return err
	}

	m.mu.Lock()
	if !m.advanceLocked(request, StateAcked) {
		m.mu.Unlock()
		return ErrClosedRequest
	}
	request.Session = session
	m.mu.Unlock()
	m.observer.Acked(requestID, outcome, session)

	if outcome == AckRejected {
		// 🔴 Back to `delivered` first (#2694 review): the send no longer moves a
		// request there, so a rejection left in `acked` hid the redelivered brief
		// from the ack sweep and from RedeliverOpen — nobody acking it meant it
		// sat until its deadline instead of reaching `redelivery_exhausted`.
		// Only the rejection's own move re-briefs: a restart that reopened the
		// request meanwhile has its new process briefed already.
		m.mu.Lock()
		terminal := request.State.Terminal()
		moved := request.State == StateAcked && m.advanceLocked(request, StateDelivered)
		m.mu.Unlock()
		if terminal {
			return ErrClosedRequest
		}
		if !moved {
			return nil
		}
		return m.redeliver(ctx, request)
	}
	if outcome == AckDeferred || !session.EarnsOpener() {
		// 🔴 A resumed session reads NOTHING here. The turn answers either way, so
		// the only evidence of getting this wrong is an opener read per turn.
		return m.advanceFrom(request, StateAcked, StatePosting)
	}

	m.mu.Lock()
	if request.State != StateAcked || !m.advanceLocked(request, StateOpening) {
		terminal := request.State.Terminal()
		m.mu.Unlock()
		if terminal {
			return ErrClosedRequest
		}
		// Reopened for a new harness process since this ack (#2694): the new
		// process is briefed afresh, and this ack has nothing left to do.
		return nil
	}
	// 🔴 The context arrived with the brief, before this ack, and sits in the
	// same system-side budget the plugin declared — so the opener gets what is
	// left. The number is the platform's; the companion estimates nothing.
	budget := openerBudget(m.budget, request.ContextTokens)
	m.mu.Unlock()

	entries, err := m.host.ReadOpener(ctx, request.ConversationID, request.ThroughCursor, budget)
	if err != nil {
		return m.fail(ctx, request, fmt.Errorf("reading the opener for %s: %w", requestID, err))
	}
	// 🔴 The sink is read AFTER the opener (#2698): a same-process reconnect
	// during the read replaced it, and the harness on the new connection is still
	// working on this brief. A request no longer `opening` was reopened for a new
	// process, which is briefed afresh — no opener is owed to anyone.
	m.mu.Lock()
	terminal, opening, sink := request.State.Terminal(), request.State == StateOpening, m.sink
	m.mu.Unlock()
	if terminal {
		return ErrClosedRequest
	}
	if !opening {
		return nil
	}
	if sink != nil {
		if err := sink.SendOpener(ctx, requestID, entries); err != nil {
			return fmt.Errorf("sending the opener for %s: %w", requestID, err)
		}
	}
	m.observer.OpenerSent(requestID, len(entries), charsOf(entries), budget, summariesOf(entries))

	return m.advanceFrom(request, StateOpening, StatePosting)
}

// advanceFrom moves a request from the state its caller left it in, and only
// from there (#2694). An ack's later moves run outside the lock; a harness
// that restarted into a new process meanwhile reopened the request to
// `delivered` for that process, and moving it on would hide it from the
// redelivery that briefs the new one. That is no error: nil, nothing moved.
// A withdrawn request is ErrClosedRequest, as `advance` reports it.
func (m *Manager) advanceFrom(request *Request, from, to State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if request.State.Terminal() {
		return ErrClosedRequest
	}
	if request.State != from {
		return nil
	}
	m.advanceLocked(request, to)
	return nil
}

// redeliver sends a request again, up to the bound, then closes it as an error.
func (m *Manager) redeliver(ctx context.Context, request *Request) error {
	m.mu.Lock()
	// 🔴 A request that was never actually SENT is not being redelivered — it is
	// being delivered for the first time. A ring that arrives with no plugin
	// attached is created in StateDelivered and held (`deliver` returns early
	// when there is no sink), so without this the first real delivery counts as
	// attempt 1 and three reconnects during a slow harness boot close a brief
	// nobody ever saw as `redelivery_exhausted`.
	//
	// Since #2014's `ready` frame that window is the NORMAL one, not an edge:
	// every run holds its first rings until the harness can answer. `DeliveredAt`
	// is the existing signal — `Tick`'s ack sweep already guards on it.
	// 🔴 "Redelivered" means "sent again after a brief that GOT THERE", because
	// DeliveredAt is stamped only by a successful send. A request whose every
	// send fails therefore never increments and never reaches
	// `redelivery_exhausted` — deliberate: that bound exists for a plugin that
	// keeps REJECTING or never acking, not for a wire that is down, where the
	// ack timeout and the request's own deadline are what end it.
	// 🔴 Claimed HERE, in the same critical section that counts it (#2372): the
	// ack sweep and RedeliverOpen choose requests under one lock and deliver under
	// another, so both may have chosen this one. A request whose delivery is in
	// flight is never delivered a second time, and is not counted again.
	if request.briefing {
		m.mu.Unlock()
		return nil
	}
	request.briefing = true
	firstDelivery := request.DeliveredAt.IsZero()
	if !firstDelivery {
		request.Redeliveries++
	}
	attempt := request.Redeliveries
	over := attempt > m.limits.MaxRedeliveries
	m.mu.Unlock()

	if over {
		// 🔴 Closed as an ERROR, not left open. A request that never closes is
		// invisible to the platform's own timeouts; an error is something it can
		// act on. The offset stays where it was either way.
		return m.closeUpstream(ctx, request, contracts.OutcomeError, "redelivery_exhausted", byCompanion)
	}
	if !firstDelivery {
		m.observer.Redelivered(request.RequestID, attempt)
	}
	return m.deliver(ctx, request)
}

// OnPost forwards one post upstream, immediately. Extras carry a question —
// the harness ASKING the person (#2232), which counts as a post like any other —
// and the citations and activity a post may report (#2349).
func (m *Manager) OnPost(ctx context.Context, connID, requestID, text string, extras *contracts.PostExtras) error {
	request, err := m.requestOn(connID, requestID)
	if err != nil {
		return err
	}

	if len(text) > m.limits.MaxPostBytes {
		return m.closeUpstream(ctx, request, contracts.OutcomeError, "post_too_large", byCompanion)
	}

	m.mu.Lock()
	if !m.advanceLocked(request, StatePosting) {
		m.mu.Unlock()
		return ErrClosedRequest
	}
	request.Posts++
	request.Posted = true
	posts := request.Posts
	m.mu.Unlock()

	if posts > m.limits.MaxPosts {
		// A turn wanting more than the cap is a runaway, not a long answer.
		return m.closeUpstream(ctx, request, contracts.OutcomeError, "post_cap_exceeded", byCompanion)
	}

	if _, err := m.host.Post(ctx, requestID, request.ConversationID, text, extras); err != nil {
		// 🔴 Handled HERE and answered nil: the plugin sent a valid frame, and a
		// platform failure is not a protocol error on the local wire. Returning
		// it would close the plugin's connection and take every other open
		// request down with this one. The host logged the class; the close is
		// observed like any other.
		_ = m.fail(ctx, request, fmt.Errorf("forwarding a post for %s: %w", requestID, err))
		return nil
	}
	m.observer.Posted(requestID, len(text))
	return nil
}

// genericInstruction is what the plugin is told when a failure carries no
// platform instruction of its own — never silence.
const genericInstruction = "Something went wrong between the companion and the platform; tell the person the answer could not be completed."

// Classify reads a failure's class and instruction: the host's own when it
// classified the failure, `internal` and a generic sentence otherwise.
func Classify(err error) (class, instruction string) {
	var classified contracts.ClassifiedError
	if errors.As(err, &classified) {
		class, instruction = classified.ErrorClass(), classified.ErrorInstruction()
	}
	if class == "" {
		class = "internal"
	}
	if instruction == "" {
		instruction = genericInstruction
	}
	return class, instruction
}

// fail ends a request the platform refused or failed (#2077, decision 32).
//
// 🔴 Never a silent failure. The plugin — if it holds the brief — is sent a
// `failed` cancel carrying the class and the instruction; the platform is told
// the request closed as an ERROR, which puts "the agent did not answer" in
// front of the person. The one exception is `request_closed`: the platform
// already ended it, so there is nothing to close upstream. The offset stays
// where it was either way (decision 40).
func (m *Manager) fail(ctx context.Context, request *Request, cause error) error {
	class, instruction := Classify(cause)
	m.mu.Lock()
	if request.State.Terminal() {
		m.mu.Unlock()
		return cause
	}
	// 🔴 Claimed HERE, in the same critical section as the check: the sends below
	// run outside the lock, and a supersede or a cancel racing them must find the
	// request already over, not close it a second time.
	request.State = StateClosed
	sink := m.sink
	_, briefed := m.connOf[request.RequestID]
	m.mu.Unlock()

	var sendErr error
	if sink != nil && briefed {
		sendErr = sink.SendFailure(ctx, request.RequestID, class, instruction)
	}
	var closeErr error
	if class == contracts.ClassRequestClosed {
		m.forget(request)
	} else {
		closeErr = m.closeUpstream(ctx, request, contracts.OutcomeError, class, byCompanion)
	}
	return errors.Join(cause, sendErr, closeErr)
}

// forget drops a request the platform already closed, remembering its id so a
// frame that crossed the close is ignored rather than fatal.
func (m *Manager) forget(request *Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	request.State = StateClosed
	m.release(request)
	delete(m.connOf, request.RequestID)
	delete(m.byID, request.RequestID)
	m.remember(request.RequestID)
}

// OnDone closes a request with the plugin's outcome.
func (m *Manager) OnDone(ctx context.Context, connID, requestID string, outcome contracts.Outcome, errorClass string) error {
	request, err := m.requestOn(connID, requestID)
	if err != nil {
		return err
	}
	if !m.advance(request, StateClosed) {
		return ErrClosedRequest
	}
	return m.closeUpstream(ctx, request, OutcomeFor(StateClosed, outcome), errorClass, byPlugin)
}

// advance moves a live request to `to`; a request already over is never moved
// (#2112). `requestOn` checks "not terminal" and releases the lock, so every
// transition after it re-checks here, in the critical section that writes.
func (m *Manager) advance(request *Request, to State) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.advanceLocked(request, to)
}

// advanceLocked is advance for a caller already holding m.mu.
func (m *Manager) advanceLocked(request *Request, to State) bool {
	if request.State.Terminal() {
		return false
	}
	request.State = to
	return true
}

// OnPlatformCancel withdraws a request the platform cancelled.
func (m *Manager) OnPlatformCancel(ctx context.Context, requestID string, reason contracts.CancelReason) error {
	m.mu.Lock()
	request, known := m.byID[requestID]
	m.mu.Unlock()
	if !known {
		// A cancel for a request already closed is ordinary, not an error.
		return nil
	}
	return m.cancel(ctx, request, reason)
}

// OnPlatformRelease passes a release to the plugin (#2356) and drops the
// manager's own slot for the conversation when the request in it is over.
//
// 🔴 A release withdraws NO request: one still open keeps its slot until its
// own cancel or close — the platform cancels first, so an open one here means a
// cancel still in flight, and dropping the slot would orphan it.
func (m *Manager) OnPlatformRelease(ctx context.Context, conversationID string, reason contracts.ReleaseReason) error {
	m.mu.Lock()
	if request, held := m.byConversation[conversationID]; held && request.State.Terminal() {
		delete(m.byConversation, conversationID)
	}
	sink := m.sink
	m.mu.Unlock()
	releaser, ok := sink.(ReleaseSink)
	if !ok {
		return nil
	}
	return releaser.SendRelease(ctx, conversationID, reason)
}

// Tick expires requests past their deadline and redelivers ones whose ack never
// came. The clock is injected, so tests do not sleep.
func (m *Manager) Tick(ctx context.Context) error {
	now := m.now()

	m.mu.Lock()
	var expired, unacked []*Request
	for _, request := range m.byID {
		if request.State.Terminal() {
			continue
		}
		if !request.Deadline.IsZero() && now.After(request.Deadline) {
			expired = append(expired, request)
			continue
		}
		if request.State == StateDelivered && !request.DeliveredAt.IsZero() && !request.briefing &&
			now.Sub(request.DeliveredAt) > m.limits.AckTimeout {
			unacked = append(unacked, request)
		}
	}
	m.mu.Unlock()

	for _, request := range expired {
		if err := m.cancel(ctx, request, contracts.CancelExpired); err != nil {
			return err
		}
	}
	for _, request := range unacked {
		if err := m.redeliver(ctx, request); err != nil {
			return err
		}
	}
	return nil
}

// RedeliverOpen sends every unacknowledged request again — what a reconnect does.
//
// 🔴 Except one already briefed on the connection attached NOW (#2372). A ring
// that arrives before `ready` reads its delta before it looks for a sink, so a
// `ready` landing during that read lets the delivery brief it on the new
// connection — and re-sending it here briefed the same request twice: two
// `fresh` acks, two openers, a redelivery attempt spent. A held ring (no
// owner) and a request reopened for a restarted harness (its owner purged at
// Detach) still go out.
func (m *Manager) RedeliverOpen(ctx context.Context) error {
	m.mu.Lock()
	var pending []*Request
	for _, request := range m.byID {
		if request.State != StateDelivered {
			continue
		}
		if owner, sent := m.connOf[request.RequestID]; sent && owner == m.connID {
			continue
		}
		// …and one whose delivery is still in flight: it will find the sink this
		// attach just set, and a second delivery would brief it twice (#2372).
		if request.briefing {
			continue
		}
		pending = append(pending, request)
	}
	m.mu.Unlock()

	for _, request := range pending {
		if err := m.redeliver(ctx, request); err != nil {
			return err
		}
	}
	return nil
}

// closedBy says who decided a request was over, which decides what a LATER
// frame naming it means (#2023).
type closedBy int

const (
	// byCompanion — the plugin has not been told yet, or is being told right
	// now, so a frame for this request may already be on the wire. Remembered:
	// such a frame is ignored.
	byCompanion closedBy = iota
	// byPlugin — the plugin sent `done` itself. A frame after that is the plugin
	// talking about a turn IT declared over, which is a protocol error and keeps
	// earning `unknown_request`.
	byPlugin
)

// closeUpstream closes a request at the platform and forgets it.
func (m *Manager) closeUpstream(ctx context.Context, request *Request, outcome contracts.Outcome, errorClass string, who closedBy) error {
	m.mu.Lock()
	if request.State.Terminal() && request.State != StateClosed {
		// Withdrawn meanwhile (#2112): its cancel already closed it upstream.
		m.mu.Unlock()
		return nil
	}
	if request.State != StateClosed {
		request.State = StateClosed
	}
	m.release(request)
	delete(m.connOf, request.RequestID)
	delete(m.byID, request.RequestID)
	if who == byCompanion {
		m.remember(request.RequestID)
	} else {
		m.ended(request.RequestID)
	}
	m.mu.Unlock()

	if err := m.host.CloseRequest(ctx, request.RequestID, outcome, errorClass); err != nil {
		return fmt.Errorf("closing %s upstream: %w", request.RequestID, err)
	}
	m.observer.Closed(request.RequestID, outcome, errorClass)
	return nil
}

// remember records a request id as closed. The caller holds m.mu.
//
// Bounded on purpose: a daemon that ran for a week would otherwise hold every
// id it had ever issued. closedMemory is far larger than any plausible in-flight
// set, so an id only ages out long after the plugin could still be talking
// about it — and an id that HAS aged out lands on the honest answer,
// `unknown_request`, rather than on silence.
func (m *Manager) remember(requestID string) {
	m.ended(requestID)
	if _, already := m.closed[requestID]; already {
		return
	}
	m.closed[requestID] = struct{}{}
	m.closedOrder = append(m.closedOrder, requestID)
	if len(m.closedOrder) > closedMemory {
		delete(m.closed, m.closedOrder[0])
		m.closedOrder = m.closedOrder[1:]
	}
}

// release frees the conversation's slot — but only while it is still THIS
// request's. The caller holds m.mu.
//
// 🔴 Never unconditional: a close that runs its I/O outside the lock can land
// after a newer ring already took the slot, and deleting it then orphans the
// newer request — the next ring would not supersede it, and the conversation
// would hold two open requests.
func (m *Manager) release(request *Request) {
	if request.stopPrefetch != nil {
		request.stopPrefetch()
		request.stopPrefetch = nil
	}
	if m.byConversation[request.ConversationID] == request {
		delete(m.byConversation, request.ConversationID)
	}
}

// ended records a request as over, for ring de-duplication. The caller holds
// m.mu. Bounded exactly like `closed`.
func (m *Manager) ended(requestID string) {
	if _, already := m.over[requestID]; already {
		return
	}
	m.over[requestID] = struct{}{}
	m.overOrder = append(m.overOrder, requestID)
	if len(m.overOrder) > closedMemory {
		delete(m.over, m.overOrder[0])
		m.overOrder = m.overOrder[1:]
	}
}

// requestOn finds an open request and checks it belongs to this connection.
//
// 🔴 Three outcomes, and the middle one is the reason this is not two lines.
// A frame for a request THIS companion closed is ordinary — a cancel and the
// plugin's terminal frame cross on the wire — and earns `ErrClosedRequest`,
// which the surface ignores. A frame naming a request that was never issued is
// a confused peer and earns `unknown_request`, a close. A request on ANOTHER
// connection is a second plugin trying to answer someone else's brief, which is
// the one that must never be honoured (#2023).
func (m *Manager) requestOn(connID, requestID string) (*Request, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	request, known := m.byID[requestID]
	if !known || request.State.Terminal() {
		if _, closedHere := m.closed[requestID]; closedHere {
			return nil, ErrClosedRequest
		}
		return nil, ErrUnknownRequest
	}
	if owner, ok := m.connOf[requestID]; ok && owner != connID {
		return nil, ErrWrongConnection
	}
	return request, nil
}

// Open reports how many requests are open, for `/healthz` and tests.
func (m *Manager) Open() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.byID)
}

// summariesOf counts the platform's summary entries (#2451) — the fold a fresh
// session's opener carries. The kind is the platform's; the companion only counts.
func summariesOf(entries []contracts.Entry) int {
	total := 0
	for _, entry := range entries {
		if entry.Kind == contracts.EntryKindSummary {
			total++
		}
	}
	return total
}

func charsOf(entries []contracts.Entry) int {
	total := 0
	for _, entry := range entries {
		total += len(entry.Text)
	}
	return total
}

// contextTokens is what a brief's context cost, as the platform counted it.
func contextTokens(c *contracts.Context) int {
	if c == nil {
		return 0
	}
	return c.Tokens
}

// The platform's bounds on an opener's budget (`OPENER_MIN_BUDGET` /
// `OPENER_MAX_BUDGET` in the conversations API's runtime plane) — a budget outside
// them is refused as invalid_budget and fails the whole request. Pinned against the
// platform's source by scripts/__tests__/uffici-opener-budget-parity.test.mjs.
const (
	OpenerMinBudget = 1000
	OpenerMaxBudget = 200000
	// ContextMaxBudget is the largest context budget the agents-gateway's entries read
	// accepts. 🔴 Over it the gateway refuses the WHOLE read, and every brief on the
	// machine would close as an error; the wire admits a declaration far larger.
	ContextMaxBudget = 1000000
)

// contextBudget is the share of the declared budget the brief's context may take:
// half, never over the platform's ceiling. 🔴 The context must never crowd out the
// transcript, and the identity block the platform puts first sits outside it anyway.
func contextBudget(declared int) int {
	return min(declared/2, ContextMaxBudget)
}

// openerBudget is the declared budget minus what the brief's context already took,
// clamped to the platform's bounds (#2331). Under the floor it still asks for the
// floor: a fresh session with no transcript at all is worse than a brief slightly
// over the declaration. The numbers are the platform's; the companion estimates
// nothing.
func openerBudget(declared, contextTokens int) int {
	return min(max(declared-contextTokens, OpenerMinBudget), OpenerMaxBudget)
}
