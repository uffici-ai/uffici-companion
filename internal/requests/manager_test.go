package requests

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/infra/memory"
)

// recorder is a PluginSink that remembers what the manager sent it.
type recorder struct {
	briefs   []Brief
	openers  []string
	cancels  map[string]contracts.CancelReason
	failures map[string][2]string
}

func newRecorder() *recorder {
	return &recorder{cancels: map[string]contracts.CancelReason{}, failures: map[string][2]string{}}
}

func (r *recorder) SendFailure(_ context.Context, requestID, class, instruction string) error {
	if r.failures == nil {
		r.failures = map[string][2]string{}
	}
	r.failures[requestID] = [2]string{class, instruction}
	return nil
}

func (r *recorder) SendBrief(_ context.Context, brief Brief) error {
	r.briefs = append(r.briefs, brief)
	return nil
}

func (r *recorder) SendOpener(_ context.Context, requestID string, _ []contracts.Entry) error {
	r.openers = append(r.openers, requestID)
	return nil
}

func (r *recorder) SendCancel(_ context.Context, requestID string, reason contracts.CancelReason) error {
	r.cancels[requestID] = reason
	return nil
}

func seededHost() *memory.Host {
	host := memory.New()
	host.Seed("conv_1", "c0001", []contracts.Entry{
		{ID: "e1", Cursor: "c0001", Kind: "message", Text: "first",
			Author: contracts.Author{Type: "member", ID: "m1"}},
		{ID: "e2", Cursor: "c0002", Kind: "message", Text: "second",
			Author: contracts.Author{Type: "member", ID: "m1"}},
		{ID: "e3", Cursor: "c0003", Kind: "message", Text: "third",
			Author: contracts.Author{Type: "member", ID: "m1"}},
	})
	return host
}

func ring(requestID, since, through string) contracts.Ring {
	return contracts.Ring{
		RingID: "ring_" + requestID, RequestID: requestID, ConversationID: "conv_1",
		Reason: "mentioned", Cause: contracts.Cause{EntryID: "e2", AuthorID: "m1"},
		SinceCursor: since, ThroughCursor: through,
	}
}

// rig wires the manager to the host the way the daemon does: rings arrive
// THROUGH the host's doorbell, not by calling the manager directly.
//
// 🔴 The first cut of these tests called `manager.OnRing` itself, and the
// offset assertion failed for a reason that had nothing to do with the rule:
// the host had never heard of the request, so it had nothing to commit. Driving
// the real path is what makes these tests about the state machine rather than
// about the harness.
type rig struct {
	manager *Manager
	sink    *recorder
	host    *memory.Host
	claim   contracts.DoorbellClaim
	errs    []error
}

func (r *rig) ring(t *testing.T, requestID, since, through string) {
	t.Helper()
	r.ringWith(t, ring(requestID, since, through))
}

func (r *rig) ringWith(t *testing.T, envelope contracts.Ring) {
	t.Helper()
	if err := r.host.DeliverRing(envelope); err != nil {
		t.Fatalf("DeliverRing: %v", err)
	}
	for _, err := range r.errs {
		if err != nil {
			t.Fatalf("handling the ring: %v", err)
		}
	}
}

func attached(t *testing.T, options Options) (*Manager, *recorder, *memory.Host) {
	t.Helper()
	r := newRig(t, options)
	return r.manager, r.sink, r.host
}

func newRig(t *testing.T, options Options) *rig {
	t.Helper()
	host := seededHost()
	manager := New(host, options)
	sink := newRecorder()
	manager.Attach("conn-1", sink, 48000)

	r := &rig{manager: manager, sink: sink, host: host}
	ctx := context.Background()
	claim, err := host.OpenDoorbell(ctx, contracts.Handlers{
		OnRing: func(envelope contracts.Ring) {
			if err := manager.OnRing(ctx, envelope); err != nil {
				r.errs = append(r.errs, err)
			}
		},
		OnCancel: func(requestID string, reason contracts.CancelReason) {
			if err := manager.OnPlatformCancel(ctx, requestID, reason); err != nil {
				r.errs = append(r.errs, err)
			}
		},
		OnDisconnect: func(string) {},
	})
	if err != nil {
		t.Fatalf("OpenDoorbell: %v", err)
	}
	r.claim = claim
	t.Cleanup(func() { _ = claim.Close() })
	return r
}

// 🔴 The opener rule, both directions, in the state machine rather than only in
// the type: a resumed session must cause NO opener read at all.
func TestOnlyAFreshAckReadsAnOpener(t *testing.T) {
	for _, testCase := range []struct {
		session     Session
		wantOpeners int
		wantReads   int
	}{
		{SessionFresh, 1, 1},
		{SessionResumed, 0, 0},
	} {
		t.Run(string(testCase.session), func(t *testing.T) {
			r := newRig(t, Options{})
			manager, sink, host := r.manager, r.sink, r.host
			ctx := context.Background()
			r.ring(t, "req_1", "c0001", "c0003")
			if err := manager.OnAck(ctx, "conn-1", "req_1", AckAccepted, testCase.session); err != nil {
				t.Fatalf("OnAck: %v", err)
			}
			if len(sink.openers) != testCase.wantOpeners {
				t.Fatalf("%d opener frame(s), want %d", len(sink.openers), testCase.wantOpeners)
			}
			if _, openerReads := host.ReadCounts(); openerReads != testCase.wantReads {
				t.Fatalf("%d opener READ(s), want %d — a resumed session paying for a read costs one platform round trip per turn, forever", openerReads, testCase.wantReads)
			}
		})
	}
}

// 🔴 The union range. A supersede whose new delta started at the NEW cursor
// would skip exactly the entries the superseded request never answered.
func TestSupersedeCancelsTheOldRequestAndCoversTheUnion(t *testing.T) {
	r := newRig(t, Options{})
	manager, sink, host := r.manager, r.sink, r.host
	r.ring(t, "req_1", "c0001", "c0002")
	r.ring(t, "req_2", "c0002", "c0003")

	if got := sink.cancels["req_1"]; got != contracts.CancelSuperseded {
		t.Fatalf("the superseded request was cancelled with %q, want %q", got, contracts.CancelSuperseded)
	}
	if len(sink.briefs) != 2 {
		t.Fatalf("%d brief(s) sent, want 2", len(sink.briefs))
	}
	second := sink.briefs[1]
	if second.SinceCursor != "c0001" {
		t.Fatalf("the superseding brief starts at %q, want \"c0001\": the union of both ranges, or the first request's entries reach nobody", second.SinceCursor)
	}
	if len(second.Delta) != 2 {
		t.Fatalf("the superseding delta has %d entries, want 2 (e2 and e3)", len(second.Delta))
	}
	// The offset must NOT have moved: the superseded request answered nothing.
	offset, err := host.OffsetOf("conv_1")
	if err != nil {
		t.Fatalf("OffsetOf: %v", err)
	}
	if offset != "c0001" {
		t.Fatalf("the offset moved to %q on a supersede — every entry it covered is now lost", offset)
	}
	if manager.Open() != 1 {
		t.Fatalf("%d open request(s), want 1: a conversation holds one", manager.Open())
	}
}

func TestADeadlinePassingExpiresTheRequest(t *testing.T) {
	clock := time.Date(2026, 9, 17, 14, 0, 0, 0, time.UTC)
	r := newRig(t, Options{Now: func() time.Time { return clock }})
	manager, sink, host := r.manager, r.sink, r.host
	ctx := context.Background()

	expiring := ring("req_1", "c0001", "c0003")
	expiring.Deadline = clock.Add(time.Second)
	r.ringWith(t, expiring)

	clock = clock.Add(2 * time.Second)
	if err := manager.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := sink.cancels["req_1"]; got != contracts.CancelExpired {
		t.Fatalf("cancelled with %q, want %q", got, contracts.CancelExpired)
	}
	if offset, _ := host.OffsetOf("conv_1"); offset != "c0001" {
		t.Fatalf("an expired request moved the offset to %q", offset)
	}
	if manager.Open() != 0 {
		t.Fatalf("%d request(s) still open after expiry", manager.Open())
	}
}

func TestARejectedAckRedeliversAndThenClosesAsError(t *testing.T) {
	r := newRig(t, Options{Limits: Limits{MaxRedeliveries: 2}})
	manager, sink := r.manager, r.sink
	ctx := context.Background()
	r.ring(t, "req_1", "c0001", "c0003")
	for i := 0; i < 2; i++ {
		if err := manager.OnAck(ctx, "conn-1", "req_1", AckRejected, SessionFresh); err != nil {
			t.Fatalf("rejection %d: %v", i, err)
		}
	}
	if len(sink.briefs) != 3 {
		t.Fatalf("%d brief(s), want 3: the original and two redeliveries", len(sink.briefs))
	}
	// 🔴 The third rejection exhausts the bound and must CLOSE the request, not
	// leave it open forever.
	if err := manager.OnAck(ctx, "conn-1", "req_1", AckRejected, SessionFresh); err != nil {
		t.Fatalf("third rejection: %v", err)
	}
	if manager.Open() != 0 {
		t.Fatalf("%d request(s) open after the redelivery bound — the platform would wait forever", manager.Open())
	}
}

func TestAFrameOnAnotherConnectionIsRefused(t *testing.T) {
	r := newRig(t, Options{})
	manager := r.manager
	ctx := context.Background()
	r.ring(t, "req_1", "c0001", "c0003")
	if err := manager.OnPost(ctx, "conn-2", "req_1", "from somebody else", nil); !errors.Is(err, ErrWrongConnection) {
		t.Fatalf("a post on another connection returned %v, want ErrWrongConnection", err)
	}
	if err := manager.OnDone(ctx, "conn-2", "req_1", contracts.OutcomeAnswered, ""); !errors.Is(err, ErrWrongConnection) {
		t.Fatalf("a done on another connection returned %v, want ErrWrongConnection", err)
	}
}

func TestAFrameForAClosedRequestIsUnknown(t *testing.T) {
	r := newRig(t, Options{})
	manager := r.manager
	ctx := context.Background()
	r.ring(t, "req_1", "c0001", "c0003")
	if err := manager.OnDone(ctx, "conn-1", "req_1", contracts.OutcomeAnswered, ""); err != nil {
		t.Fatalf("OnDone: %v", err)
	}
	if err := manager.OnPost(ctx, "conn-1", "req_1", "too late", nil); !errors.Is(err, ErrUnknownRequest) {
		t.Fatalf("a post after the close returned %v, want ErrUnknownRequest", err)
	}
}

func TestTheCapsCloseTheRequestAsAnError(t *testing.T) {
	t.Run("too many posts", func(t *testing.T) {
		r := newRig(t, Options{Limits: Limits{MaxPosts: 2}})
		manager, host := r.manager, r.host
		ctx := context.Background()
		r.ring(t, "req_1", "c0001", "c0003")
		for i := 0; i < 2; i++ {
			if err := manager.OnPost(ctx, "conn-1", "req_1", "ok", nil); err != nil {
				t.Fatalf("post %d: %v", i, err)
			}
		}
		if err := manager.OnPost(ctx, "conn-1", "req_1", "one too many", nil); err != nil {
			t.Fatalf("the capped post: %v", err)
		}
		if manager.Open() != 0 {
			t.Fatal("the request stayed open past its post cap")
		}
		if offset, _ := host.OffsetOf("conv_1"); offset != "c0001" {
			t.Fatalf("a capped request moved the offset to %q", offset)
		}
	})

	t.Run("a post that is too large", func(t *testing.T) {
		r := newRig(t, Options{Limits: Limits{MaxPostBytes: 4}})
		manager := r.manager
		ctx := context.Background()
		r.ring(t, "req_1", "c0001", "c0003")
		if err := manager.OnPost(ctx, "conn-1", "req_1", "far too long for the cap", nil); err != nil {
			t.Fatalf("the oversized post: %v", err)
		}
		if manager.Open() != 0 {
			t.Fatal("the request stayed open past the byte cap")
		}
	})
}

func TestAnAnsweredCloseCommitsTheOffset(t *testing.T) {
	r := newRig(t, Options{})
	manager, host := r.manager, r.host
	ctx := context.Background()
	r.ring(t, "req_1", "c0001", "c0003")
	if err := manager.OnAck(ctx, "conn-1", "req_1", AckAccepted, SessionResumed); err != nil {
		t.Fatalf("OnAck: %v", err)
	}
	if err := manager.OnPost(ctx, "conn-1", "req_1", "here you go", nil); err != nil {
		t.Fatalf("OnPost: %v", err)
	}
	if err := manager.OnDone(ctx, "conn-1", "req_1", contracts.OutcomeAnswered, ""); err != nil {
		t.Fatalf("OnDone: %v", err)
	}
	if offset, _ := host.OffsetOf("conv_1"); offset != "c0003" {
		t.Fatalf("the offset is %q after an answered close, want \"c0003\"", offset)
	}
}

// A brief with no plugin attached is held, not lost: a harness restarting is
// ordinary, and the request must be there when it comes back.
func TestARingWithNoPluginIsHeldAndRedeliveredOnReconnect(t *testing.T) {
	host := seededHost()
	manager := New(host, Options{})
	ctx := context.Background()
	if err := manager.OnRing(ctx, ring("req_1", "c0001", "c0003")); err != nil {
		t.Fatalf("OnRing: %v", err)
	}
	if manager.Open() != 1 {
		t.Fatal("the request was dropped because no plugin was attached")
	}

	sink := newRecorder()
	manager.Attach("conn-1", sink, 1000)
	if err := manager.RedeliverOpen(ctx); err != nil {
		t.Fatalf("RedeliverOpen: %v", err)
	}
	if len(sink.briefs) != 1 {
		t.Fatalf("%d brief(s) after reconnect, want 1", len(sink.briefs))
	}
}

func TestDetachOnlyForgetsItsOwnConnection(t *testing.T) {
	manager, _, _ := attached(t, Options{})
	manager.Detach("conn-other")
	if !manager.Attached() {
		t.Fatal("detaching a different connection dropped the attached plugin")
	}
	manager.Detach("conn-1")
	if manager.Attached() {
		t.Fatal("the plugin is still attached after its own detach")
	}
}

func TestAPlatformCancelForAnUnknownRequestIsNotAnError(t *testing.T) {
	manager, _, _ := attached(t, Options{})
	if err := manager.OnPlatformCancel(context.Background(), "req_absent", contracts.CancelStop); err != nil {
		t.Fatalf("a cancel for an already-closed request must be ordinary, got %v", err)
	}
}

// The no-op observer is what a caller gets when it wants none, so it must
// satisfy the interface and do nothing rather than panic.
func TestNopObserverIsUsable(_ *testing.T) {
	var observer Observer = NopObserver{}
	observer.BriefSent("r", "c", 1, 2)
	observer.OpenerSent("r", 1, 2, 3)
	observer.Acked("r", AckAccepted, SessionFresh)
	observer.Posted("r", 1)
	observer.Closed("r", contracts.OutcomeAnswered, "")
	observer.Cancelled("r", contracts.CancelStop)
	observer.Redelivered("r", 1)
}

// A platform cancel must reach the plugin as a cancel frame and leave the
// offset where it was.
func TestAPlatformCancelWithdrawsTheRequest(t *testing.T) {
	r := newRig(t, Options{})
	manager, sink, host := r.manager, r.sink, r.host
	r.ring(t, "req_1", "c0001", "c0003")

	if err := host.DeliverCancel("req_1", contracts.CancelStop); err != nil {
		t.Fatalf("DeliverCancel: %v", err)
	}
	if got := sink.cancels["req_1"]; got != contracts.CancelStop {
		t.Fatalf("the plugin was told %q, want %q", got, contracts.CancelStop)
	}
	if offset, _ := host.OffsetOf("conv_1"); offset != "c0001" {
		t.Fatalf("a stopped request moved the offset to %q", offset)
	}
	if manager.Open() != 0 {
		t.Fatalf("%d request(s) open after a cancel", manager.Open())
	}
}

// failingSink is a plugin connection that is already going away.
type failingSink struct {
	recorder
	cancelErr error
}

func (f *failingSink) SendCancel(_ context.Context, requestID string, reason contracts.CancelReason) error {
	f.cancels[requestID] = reason
	return f.cancelErr
}

// refusingHost is an agents host whose CloseRequest fails.
type refusingHost struct {
	*memory.Host
	closeErr  error
	closeCall int
}

func (r *refusingHost) CloseRequest(ctx context.Context, requestID string, outcome contracts.Outcome, errorClass string) error {
	r.closeCall++
	if r.closeErr != nil {
		return r.closeErr
	}
	return r.Host.CloseRequest(ctx, requestID, outcome, errorClass)
}

// 🔴 A cancel that cannot reach the plugin must still reach the PLATFORM, and
// must still forget the request.
//
// The state is terminal by the time either send happens, and `Tick` skips
// terminal requests — so a cancel that returned early would strand the entry for
// the life of the process AND leave the platform waiting on a turn nobody is
// answering. Telling the plugin is the step allowed to fail; the other two are
// not conditional on it.
//
// The trigger is ordinary rather than exotic: a plugin's connection dies just as
// one of its requests hits the expiry sweep.
func TestACancelThatCannotReachThePluginStillClosesAndForgets(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		sendErr   error
		closeErr  error
		wantCalls int
	}{
		{"the plugin's connection is dead", errors.New("write: broken pipe"), nil, 1},
		{"the host refuses the close", nil, errors.New("upstream said no"), 1},
		{"both fail", errors.New("write: broken pipe"), errors.New("upstream said no"), 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			host := &refusingHost{Host: seededHost(), closeErr: testCase.closeErr}
			manager := New(host, Options{})
			sink := &failingSink{recorder: *newRecorder(), cancelErr: testCase.sendErr}
			manager.Attach("conn-1", sink, 48000)

			if err := manager.OnRing(ctx, contracts.Ring{
				RequestID: "req_1", ConversationID: "conv_1", RingID: "ring_1",
				Reason: "mentioned", SinceCursor: "", ThroughCursor: "c0002",
			}); err != nil {
				t.Fatalf("OnRing: %v", err)
			}
			if manager.Open() != 1 {
				t.Fatalf("%d open requests after the ring, want 1", manager.Open())
			}

			err := manager.OnPlatformCancel(ctx, "req_1", contracts.CancelStop)
			if testCase.sendErr == nil && testCase.closeErr == nil && err != nil {
				t.Fatalf("cancel: %v", err)
			}

			// 🔴 The platform was told, whatever the plugin did.
			if host.closeCall != testCase.wantCalls {
				t.Fatalf("CloseRequest was called %d time(s), want %d — the platform must be told "+
					"the request closed even when the plugin could not be", host.closeCall, testCase.wantCalls)
			}
			// 🔴 And the request was forgotten. Terminal state means Tick will never
			// revisit it, so an entry left here is left for the life of the process.
			if manager.Open() != 0 {
				t.Fatalf("%d request(s) still open after the cancel; the entry leaked for good",
					manager.Open())
			}
		})
	}
}

// Non-vacuity for the table above: with everything working, the same sequence
// closes cleanly — so the assertions are about the FAILURE handling, not about
// cancel working at all.
func TestTheSameCancelWithNothingFailingIsClean(t *testing.T) {
	ctx := context.Background()
	host := &refusingHost{Host: seededHost()}
	manager := New(host, Options{})
	manager.Attach("conn-1", &failingSink{recorder: *newRecorder()}, 48000)

	if err := manager.OnRing(ctx, contracts.Ring{
		RequestID: "req_1", ConversationID: "conv_1", RingID: "ring_1",
		Reason: "mentioned", ThroughCursor: "c0002",
	}); err != nil {
		t.Fatalf("OnRing: %v", err)
	}
	if err := manager.OnPlatformCancel(ctx, "req_1", contracts.CancelStop); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if host.closeCall != 1 || manager.Open() != 0 {
		t.Fatalf("closeCall=%d open=%d, want 1 and 0", host.closeCall, manager.Open())
	}
}

// 🔴 The union range, over the cursor values a host other than the fixture can
// produce. An empty cursor is the LOWEST bound — "from the beginning" — so the
// union must keep it, never replace it with a real cursor. Getting this
// backwards silently narrows the delta, and a member's earlier message reaches
// no agent at all: the exact loss supersede's union exists to prevent.
func TestTheUnionRangeTakesTheLowestBoundWithEmptyAsLowest(t *testing.T) {
	for _, testCase := range []struct {
		name            string
		openSince       string
		supersedingFrom string
		want            string
	}{
		{"the superseded request reached further back", "c0001", "c0004", "c0001"},
		{"the new ring reaches further back", "c0004", "c0001", "c0001"},
		{"both the same, as the fixture always produces", "c0002", "c0002", "c0002"},
		{"the superseded request started at the beginning", "", "c0004", ""},
		{"the new ring starts at the beginning", "c0004", "", ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			host := seededHost()
			manager := New(host, Options{})
			sink := newRecorder()
			manager.Attach("conn-1", sink, 48000)

			if err := manager.OnRing(ctx, contracts.Ring{
				RequestID: "req_1", ConversationID: "conv_1", RingID: "ring_1",
				Reason: "mentioned", SinceCursor: testCase.openSince, ThroughCursor: "c0005",
			}); err != nil {
				t.Fatalf("first ring: %v", err)
			}
			if err := manager.OnRing(ctx, contracts.Ring{
				RequestID: "req_2", ConversationID: "conv_1", RingID: "ring_2",
				Reason: "mentioned", SinceCursor: testCase.supersedingFrom, ThroughCursor: "c0006",
			}); err != nil {
				t.Fatalf("superseding ring: %v", err)
			}

			if len(sink.briefs) != 2 {
				t.Fatalf("%d brief(s) were sent, want 2 — the comparison below needs the second one",
					len(sink.briefs))
			}
			if got := sink.briefs[1].SinceCursor; got != testCase.want {
				t.Fatalf("the superseding brief starts at %q, want %q", got, testCase.want)
			}
		})
	}
}

// 🔴 A connection that has gone away owns nothing (#2014).
//
// `connOf` is what refuses a frame from the WRONG connection. An entry left
// pointing at a dead connection makes the NEXT plugin's post for that request
// illegal: the surface closes with `wrong_connection`, the plugin reconnects,
// posts again, and the turn dies in that loop until the deadline expires it.
//
// Latent until acknowledgement moved to receipt: a resumed request used to
// spend a slow `handle_message` in StateDelivered, which a reconnect re-owns.
func TestAReattachedPluginMayPostForARequestItAlreadyAcked(t *testing.T) {
	ctx := context.Background()
	host := seededHost()
	manager := New(host, Options{})

	first := newRecorder()
	manager.Attach("conn-1", first, 48000)
	if err := manager.OnRing(ctx, contracts.Ring{
		RequestID: "req_1", ConversationID: "conv_1", RingID: "ring_1",
		Reason: "mentioned", ThroughCursor: "c0002",
	}); err != nil {
		t.Fatalf("OnRing: %v", err)
	}
	// Acknowledged on receipt, the way the plugin now does it.
	if err := manager.OnAck(ctx, "conn-1", "req_1", AckAccepted, SessionResumed); err != nil {
		t.Fatalf("OnAck: %v", err)
	}

	// The connection drops mid-turn and the plugin comes back on a new one.
	manager.Detach("conn-1")
	second := newRecorder()
	manager.Attach("conn-2", second, 48000)

	// The harness's reply must be accepted, not answered with wrong_connection.
	err := manager.OnPost(ctx, "conn-2", "req_1", "yes, it is filed", nil)
	if errors.Is(err, ErrWrongConnection) {
		t.Fatal(
			"a post from the reattached plugin was refused as wrong_connection: the dead " +
				"connection still owns the request, so the surface closes the wire and the " +
				"turn dies in a reconnect loop",
		)
	}
	if err != nil {
		t.Fatalf("OnPost: %v", err)
	}
}

// 🔴 A brief that was never SENT has not been redelivered (#2014).
//
// A ring arriving with no plugin attached is created in StateDelivered and
// held — `deliver` returns early with no sink — so counting the first real
// delivery as a redelivery burns an attempt on a brief nobody saw. Since the
// `ready` frame that window is the normal one: every run holds its first rings
// until the harness can answer.
func TestAHeldRingDoesNotBurnARedeliveryAttempt(t *testing.T) {
	ctx := context.Background()
	sink := newRecorder()
	counted := &redeliveryCounter{}
	manager := New(seededHost(), Options{Observer: counted})

	// No plugin attached yet: the ring is held, with no brief sent.
	if err := manager.OnRing(ctx, contracts.Ring{
		RequestID: "req_1", ConversationID: "conv_1", RingID: "ring_1",
		Reason: "mentioned", ThroughCursor: "c0002",
	}); err != nil {
		t.Fatalf("OnRing: %v", err)
	}
	manager.Attach("conn-1", sink, 48000)
	if err := manager.RedeliverOpen(ctx); err != nil {
		t.Fatalf("RedeliverOpen: %v", err)
	}

	if len(sink.briefs) != 1 {
		t.Fatalf("%d brief(s) went out, want the held one", len(sink.briefs))
	}
	if counted.count != 0 {
		t.Fatalf(
			"%d redelivery/ies reported for a brief that had never been sent — three "+
				"reconnects during a slow harness boot would close it as redelivery_exhausted",
			counted.count,
		)
	}
}

// redeliveryCounter records only what it is asked about.
type redeliveryCounter struct {
	NopObserver
	count int
}

func (r *redeliveryCounter) Redelivered(string, int) { r.count++ }

// ── A terminal frame that crosses a cancel (#2023) ──────────────────────────

func TestADoneThatCrossesACancelIsIgnoredRatherThanFatal(t *testing.T) {
	// 🔴 The race is unavoidable, not a plugin defect: the companion cancels
	// while the plugin's `done` for the same request is already on the wire, and
	// nothing either side can do makes them ordered. Answering it with
	// `unknown_request` drops the CONNECTION — and with it every other open
	// request the plugin was working on.
	ctx := context.Background()
	manager, sink, _ := attached(t, Options{})

	if err := manager.OnRing(ctx, ring("req_1", "c0001", "c0003")); err != nil {
		t.Fatalf("OnRing: %v", err)
	}
	if err := manager.OnAck(ctx, "conn-1", "req_1", AckAccepted, SessionResumed); err != nil {
		t.Fatalf("OnAck: %v", err)
	}
	if err := manager.OnPlatformCancel(ctx, "req_1", contracts.CancelStop); err != nil {
		t.Fatalf("OnPlatformCancel: %v", err)
	}
	if _, told := sink.cancels["req_1"]; !told {
		t.Fatal("the plugin was never sent the cancel, so this test is not about the race")
	}

	// The plugin's own terminal frame, written before the cancel arrived. It is
	// ErrClosedRequest — which the surface IGNORES — and never
	// ErrUnknownRequest, which closes the wire.
	err := manager.OnDone(ctx, "conn-1", "req_1", contracts.OutcomeAnswered, "")
	if !errors.Is(err, ErrClosedRequest) {
		t.Fatalf("a done crossing a cancel gave %v, want ErrClosedRequest", err)
	}
	if errors.Is(err, ErrUnknownRequest) {
		t.Fatal("a done crossing a cancel reads as an unknown request, which closes the connection")
	}
	// And a post that crossed it is equally ordinary.
	if err := manager.OnPost(ctx, "conn-1", "req_1", "a reply in flight", nil); !errors.Is(err, ErrClosedRequest) {
		t.Fatalf("a post crossing a cancel gave %v, want ErrClosedRequest", err)
	}
	if manager.Open() != 0 {
		t.Fatalf("%d request(s) open — the late frames reopened a closed request", manager.Open())
	}
}

func TestAFrameForARequestThatNeverExistedStillClosesTheWire(t *testing.T) {
	// The other half, and the reason `unknown_request` exists: a peer naming a
	// request this companion never issued is confused, and tolerating THAT
	// would turn the close reason into dead code.
	ctx := context.Background()
	manager, _, _ := attached(t, Options{})

	err := manager.OnDone(ctx, "conn-1", "req_never", contracts.OutcomeAnswered, "")
	if !errors.Is(err, ErrUnknownRequest) {
		t.Fatalf("a done for an unissued request gave %v, want ErrUnknownRequest", err)
	}
}

func TestTheClosedMemoryIsBoundedAndAgesOutHonestly(t *testing.T) {
	// A daemon that remembered every id it ever issued would leak for the life
	// of the process. An id that HAS aged out must land on `unknown_request` —
	// the honest answer — rather than on silence.
	manager := New(seededHost(), Options{})

	for index := 0; index <= closedMemory; index++ {
		manager.mu.Lock()
		manager.remember("req_" + strconv.Itoa(index))
		manager.mu.Unlock()
	}
	if got := len(manager.closed); got != closedMemory {
		t.Fatalf("the closed memory holds %d ids, want %d", got, closedMemory)
	}
	if len(manager.closedOrder) != len(manager.closed) {
		t.Fatalf("the ring holds %d ids and the set %d — they have drifted",
			len(manager.closedOrder), len(manager.closed))
	}

	// The oldest aged out; the newest is still remembered.
	if _, remembered := manager.closed["req_0"]; remembered {
		t.Fatal("the oldest id survived, so the memory is unbounded")
	}
	if _, remembered := manager.closed["req_"+strconv.Itoa(closedMemory)]; !remembered {
		t.Fatal("the newest id is not remembered")
	}

	// Remembering the same id twice must not consume two slots.
	manager.mu.Lock()
	manager.remember("req_" + strconv.Itoa(closedMemory))
	manager.mu.Unlock()
	if got := len(manager.closedOrder); got != closedMemory {
		t.Fatalf("a repeated id grew the ring to %d", got)
	}
}

func TestAFrameAfterACompanionDecidedCloseIsIgnoredToo(t *testing.T) {
	// The cancel is not the only close the plugin has not heard about yet: the
	// caps close a request out from under a turn that is still running, and the
	// frames already in flight when that happens cross exactly the same way.
	ctx := context.Background()
	r := newRig(t, Options{Limits: Limits{MaxPosts: 1}})
	manager := r.manager
	r.ring(t, "req_1", "c0001", "c0003")
	if err := manager.OnAck(ctx, "conn-1", "req_1", AckAccepted, SessionResumed); err != nil {
		t.Fatalf("OnAck: %v", err)
	}
	if err := manager.OnPost(ctx, "conn-1", "req_1", "one", nil); err != nil {
		t.Fatalf("the first post: %v", err)
	}
	// The second post trips the cap, which closes the request here.
	if err := manager.OnPost(ctx, "conn-1", "req_1", "two", nil); err != nil {
		t.Fatalf("the post that trips the cap: %v", err)
	}
	if manager.Open() != 0 {
		t.Fatal("the cap did not close the request, so this test is not about the race")
	}

	if err := manager.OnDone(ctx, "conn-1", "req_1", contracts.OutcomeAnswered, ""); !errors.Is(err, ErrClosedRequest) {
		t.Fatalf("a done after a cap close gave %v, want ErrClosedRequest", err)
	}
}

// blockingSink holds SendCancel open until the test lets it go, so a second
// goroutine can drive the window a real cancel spends inside its I/O.
type blockingSink struct {
	*recorder
	entered chan struct{}
	release chan struct{}
}

func (b *blockingSink) SendCancel(ctx context.Context, requestID string, reason contracts.CancelReason) error {
	close(b.entered)
	<-b.release
	return b.recorder.SendCancel(ctx, requestID, reason)
}

func TestADoneArrivingWHILEACancelIsSendingIsIgnoredToo(t *testing.T) {
	// 🔴 The ordering that actually happens, and the one a single-goroutine test
	// cannot see. `cancel` makes the request terminal, then sends the cancel and
	// closes upstream — a websocket write and a platform round-trip. The
	// plugin's `done` is read by ANOTHER goroutine during exactly that window.
	// A companion that recorded the id only after those calls answered
	// `unknown_request` for the whole window and dropped the connection — the
	// defect #2023 is about, in the shape it takes in production.
	ctx := context.Background()
	host := seededHost()
	manager := New(host, Options{})
	sink := &blockingSink{
		recorder: newRecorder(),
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	manager.Attach("conn-1", sink, 48000)

	if err := manager.OnRing(ctx, ring("req_1", "c0001", "c0003")); err != nil {
		t.Fatalf("OnRing: %v", err)
	}
	if err := manager.OnAck(ctx, "conn-1", "req_1", AckAccepted, SessionResumed); err != nil {
		t.Fatalf("OnAck: %v", err)
	}

	cancelled := make(chan error, 1)
	go func() { cancelled <- manager.OnPlatformCancel(ctx, "req_1", contracts.CancelStop) }()

	select {
	case <-sink.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the cancel never reached the sink")
	}

	// The cancel is mid-flight. This is the window.
	err := manager.OnDone(ctx, "conn-1", "req_1", contracts.OutcomeAnswered, "")
	if errors.Is(err, ErrUnknownRequest) {
		t.Fatal("a done arriving while the cancel was still sending closed the connection")
	}
	if !errors.Is(err, ErrClosedRequest) {
		t.Fatalf("a done arriving mid-cancel gave %v, want ErrClosedRequest", err)
	}

	close(sink.release)
	if err := <-cancelled; err != nil {
		t.Fatalf("the cancel itself failed: %v", err)
	}
}

func TestADetachPurgesItsOwnershipEvenAfterAnotherPluginAttached(t *testing.T) {
	// 🔴 The surface clears its own attachment BEFORE telling the manager, so a
	// reconnect that claims and says `ready` in that gap calls Attach with the
	// new id first. A Detach that checked "am I still the attached one?" before
	// purging would then skip conn-1's rows — and the new plugin's post for that
	// request earns `wrong_connection`, which closes the wire it just opened.
	ctx := context.Background()
	manager, _, _ := attached(t, Options{})
	if err := manager.OnRing(ctx, ring("req_1", "c0001", "c0003")); err != nil {
		t.Fatalf("OnRing: %v", err)
	}
	if err := manager.OnAck(ctx, "conn-1", "req_1", AckAccepted, SessionResumed); err != nil {
		t.Fatalf("OnAck: %v", err)
	}

	// The reconnect wins the race: it attaches before conn-1's detach lands.
	manager.Attach("conn-2", newRecorder(), 48000)
	manager.Detach("conn-1")

	if err := manager.OnPost(ctx, "conn-2", "req_1", "the answer, on the new connection", nil); err != nil {
		t.Fatalf("the reconnected plugin's post was refused: %v", err)
	}
}

// ── A harness that restarts under an acknowledged request (#2234) ────────────

// ackedThenDetached opens req_1 on conn-1 for process `before`, has the plugin
// acknowledge it on receipt, and drops the connection mid-turn.
func ackedThenDetached(t *testing.T, manager *Manager, before string) {
	t.Helper()
	ctx := context.Background()
	manager.Attach("conn-1", newRecorder(), 48000)
	manager.BindProcess(before)
	if err := manager.OnRing(ctx, contracts.Ring{
		RequestID: "req_1", ConversationID: "conv_1", RingID: "ring_1",
		Reason: "mentioned", ThroughCursor: "c0002",
	}); err != nil {
		t.Fatalf("OnRing: %v", err)
	}
	if err := manager.OnAck(ctx, "conn-1", "req_1", AckAccepted, SessionResumed); err != nil {
		t.Fatalf("OnAck: %v", err)
	}
	if err := manager.OnPost(ctx, "conn-1", "req_1", "half an answer", nil); err != nil {
		t.Fatalf("OnPost: %v", err)
	}
	manager.Detach("conn-1")
}

// reattach brings a plugin back the way the surface does on `ready`.
func reattach(t *testing.T, manager *Manager, processID string) *recorder {
	t.Helper()
	sink := newRecorder()
	manager.Attach("conn-2", sink, 48000)
	manager.BindProcess(processID)
	if err := manager.RedeliverOpen(context.Background()); err != nil {
		t.Fatalf("RedeliverOpen: %v", err)
	}
	return sink
}

// 🔴 #2234: on hermes-01 `hermes gateway restart` dropped the request in flight.
// The companion kept it open because the harness "may still be working" — true
// of a reconnect, false of a restart, and the plugin's hello could not say which.
// A NEW process holds nothing of the old one's turn, so the request goes to it.
func TestARestartedHarnessIsHandedWhatTheOldProcessAcknowledged(t *testing.T) {
	manager := New(seededHost(), Options{})
	ackedThenDetached(t, manager, "proc-before")

	sink := reattach(t, manager, "proc-after")

	if posts := manager.byID["req_1"].Posts; posts != 0 {
		t.Fatalf("the redelivered request still counts %d post(s) of the process that died", posts)
	}
	if len(sink.briefs) != 1 || sink.briefs[0].RequestID != "req_1" {
		t.Fatalf(
			"the restarted harness got %d brief(s); the request its predecessor acknowledged "+
				"would sit unanswered until its deadline", len(sink.briefs),
		)
	}
	// Owned by the new connection now: its reply is accepted.
	if err := manager.OnAck(context.Background(), "conn-2", "req_1", AckAccepted, SessionResumed); err != nil {
		t.Fatalf("the new process could not acknowledge its brief: %v", err)
	}
}

// The other half: a reconnect of the SAME process is still working on its turn.
// Handing it the brief again would admit the same delta twice.
func TestAReconnectFromTheSameProcessIsNotHandedItAgain(t *testing.T) {
	manager := New(seededHost(), Options{})
	ackedThenDetached(t, manager, "proc-same")

	sink := reattach(t, manager, "proc-same")

	if len(sink.briefs) != 0 {
		t.Fatalf("a reconnect of the same process got %d brief(s) it was already answering", len(sink.briefs))
	}
}

// An older plugin sends no process id: the old rule, never a redelivery.
func TestAPluginWithNoProcessIDKeepsTheOldRule(t *testing.T) {
	manager := New(seededHost(), Options{})
	ackedThenDetached(t, manager, "")

	sink := reattach(t, manager, "")

	if len(sink.briefs) != 0 {
		t.Fatalf("%d brief(s) redelivered to a plugin that cannot say it restarted", len(sink.briefs))
	}
}

// A harness that restarts on every brief must still END: each restart is a
// redelivery, and the bound closes it as an error the platform can act on.
func TestAHarnessRestartingOnEveryBriefEndsInRedeliveryExhausted(t *testing.T) {
	manager := New(seededHost(), Options{Limits: Limits{MaxRedeliveries: 2}})
	ackedThenDetached(t, manager, "proc-0")

	for i := 1; i <= 3; i++ {
		sink := reattach(t, manager, "proc-"+strconv.Itoa(i))
		if manager.Open() == 0 {
			if i != 3 {
				t.Fatalf("closed after %d restart(s), before the bound of 2 redeliveries", i)
			}
			return
		}
		if len(sink.briefs) != 1 {
			t.Fatalf("restart %d: %d brief(s), want the redelivered one", i, len(sink.briefs))
		}
		if err := manager.OnAck(context.Background(), "conn-2", "req_1", AckAccepted, SessionResumed); err != nil {
			t.Fatalf("restart %d: OnAck: %v", i, err)
		}
		manager.Detach("conn-2")
	}
	t.Fatalf("%d request(s) still open after three restarts — a crash loop would never end", manager.Open())
}
