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

// Observer is told what happened, for the fixture runner's expectations and for
// the surface's one-line-per-frame log.
//
// 🔴 It receives COUNTS and ids, never text. The log line is built from what
// lands here, so a field carrying a post body or a delta would put transcript
// content into a log by construction rather than by accident.
type Observer interface {
	BriefSent(requestID, conversationID string, deltaEntries, deltaChars int)
	OpenerSent(requestID string, entries, chars, budgetDeclared int)
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
func (NopObserver) OpenerSent(string, int, int, int) {}

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
}

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
// hermes-01 it sat silent until its deadline. The plugin's `processId` tells the
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

// deliver reads the delta and sends the brief.
func (m *Manager) deliver(ctx context.Context, request *Request) error {
	delta, _, err := m.host.ReadSince(ctx, request.ConversationID, request.SinceCursor, request.ThroughCursor)
	if err != nil {
		// 🔴 Closed as an error, never left to expire: an expiry closes as
		// `cancelled`, which tells the person nothing, and the host has already
		// retried what could be retried. The error close is what puts "the agent
		// did not answer" in front of them.
		return m.fail(ctx, request, fmt.Errorf("reading the delta for %s: %w", request.RequestID, err))
	}

	m.mu.Lock()
	sink, connID := m.sink, m.connID
	m.mu.Unlock()
	if sink == nil {
		// No plugin attached: the request stays open and is delivered when one
		// arrives. It is NOT an error — a harness restarting is ordinary.
		return nil
	}

	brief := Brief{
		RequestID:      request.RequestID,
		ConversationID: request.ConversationID,
		Reason:         request.Reason,
		Cause:          request.Cause,
		Deadline:       request.Deadline,
		SinceCursor:    request.SinceCursor,
		ThroughCursor:  request.ThroughCursor,
		ResetSession:   request.ResetSession,
		Delta:          delta,
	}
	if err := sink.SendBrief(ctx, brief); err != nil {
		return fmt.Errorf("sending the brief for %s: %w", request.RequestID, err)
	}

	m.mu.Lock()
	if !m.advanceLocked(request, StateDelivered) {
		// Withdrawn while the brief was on the wire: nothing more to do for it.
		m.mu.Unlock()
		return nil
	}
	request.DeliveredAt = m.now()
	m.connOf[request.RequestID] = connID
	m.mu.Unlock()

	m.observer.BriefSent(request.RequestID, request.ConversationID, len(delta), charsOf(delta))
	return nil
}

// supersede cancels an open request and closes it upstream as cancelled.
func (m *Manager) supersede(ctx context.Context, request *Request) error {
	return m.cancel(ctx, request, contracts.CancelSuperseded)
}

// cancel withdraws a request from the plugin and closes it upstream.
//
// 🔴 The upstream close is what keeps the offset where it was: it goes as
// `cancelled`, which does not advance. A supersede that closed as answered
// would lose every entry the superseded request covered.
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

	closeErr := m.host.CloseRequest(ctx, request.RequestID, outcome, string(reason))
	if closeErr == nil {
		m.observer.Closed(request.RequestID, outcome, string(reason))
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
		return m.redeliver(ctx, request)
	}
	if outcome == AckDeferred || !session.EarnsOpener() {
		// 🔴 A resumed session reads NOTHING here. The turn answers either way, so
		// the only evidence of getting this wrong is an opener read per turn.
		if !m.advance(request, StatePosting) {
			return ErrClosedRequest
		}
		return nil
	}

	m.mu.Lock()
	if !m.advanceLocked(request, StateOpening) {
		m.mu.Unlock()
		return ErrClosedRequest
	}
	sink, budget := m.sink, m.budget
	m.mu.Unlock()

	entries, err := m.host.ReadOpener(ctx, request.ConversationID, request.ThroughCursor, budget)
	if err != nil {
		return m.fail(ctx, request, fmt.Errorf("reading the opener for %s: %w", requestID, err))
	}
	if sink != nil {
		if err := sink.SendOpener(ctx, requestID, entries); err != nil {
			return fmt.Errorf("sending the opener for %s: %w", requestID, err)
		}
	}
	m.observer.OpenerSent(requestID, len(entries), charsOf(entries), budget)

	if !m.advance(request, StatePosting) {
		return ErrClosedRequest
	}
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

// OnPost forwards one post upstream, immediately. A non-nil question makes it
// the harness ASKING the person (#2232); it counts as a post like any other.
func (m *Manager) OnPost(ctx context.Context, connID, requestID, text string, question *contracts.Question) error {
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
	posts := request.Posts
	m.mu.Unlock()

	if posts > m.limits.MaxPosts {
		// A turn wanting more than the cap is a runaway, not a long answer.
		return m.closeUpstream(ctx, request, contracts.OutcomeError, "post_cap_exceeded", byCompanion)
	}

	if _, err := m.host.Post(ctx, requestID, request.ConversationID, text, question); err != nil {
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
		if request.State == StateDelivered && !request.DeliveredAt.IsZero() &&
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
func (m *Manager) RedeliverOpen(ctx context.Context) error {
	m.mu.Lock()
	var pending []*Request
	for _, request := range m.byID {
		if request.State == StateDelivered {
			pending = append(pending, request)
		}
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

func charsOf(entries []contracts.Entry) int {
	total := 0
	for _, entry := range entries {
		total += len(entry.Text)
	}
	return total
}
