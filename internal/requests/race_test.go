package requests

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/infra/memory"
)

// 🔴 #2112: an ack racing a cancel. `requestOn` checks "not terminal" under the
// lock and releases it; a write that follows without re-checking resurrects a
// request the cancel has already withdrawn — and `cancel` then read the state
// back and told the platform `error/stop` for a stop the person asked for.

// cancelRacingSink runs `during` inside SendCancel — the window `cancel` spends
// outside the lock, where a frame for the same request is being handled.
type cancelRacingSink struct {
	*recorder
	during func()
}

func (c *cancelRacingSink) SendCancel(ctx context.Context, requestID string, reason contracts.CancelReason) error {
	if c.during != nil {
		c.during()
	}
	return c.recorder.SendCancel(ctx, requestID, reason)
}

func TestACancelClosesWithTheOutcomeOfTheStateItSet(t *testing.T) {
	ctx := context.Background()
	host := &failingHost{Host: seededHost()}
	manager, _ := failingRig(t, host)
	sink := &cancelRacingSink{recorder: newRecorder()}
	manager.Attach("conn-1", sink, 48000)
	if err := manager.OnRing(ctx, ring("req_1", "c0001", "c0003")); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	request := manager.byID["req_1"]
	manager.mu.Unlock()
	// The unguarded ack write, landing inside the cancel's I/O window.
	sink.during = func() {
		manager.mu.Lock()
		request.State = StateAcked
		manager.mu.Unlock()
	}
	if err := manager.OnPlatformCancel(ctx, "req_1", contracts.CancelStop); err != nil {
		t.Fatal(err)
	}
	if len(host.closes) != 1 || host.closes[0].outcome != contracts.OutcomeCancelled {
		t.Fatalf("closes = %+v, want one `cancelled` — a stop reached the platform as an error", host.closes)
	}
}

func TestAWithdrawnRequestCannotBeMovedAgain(t *testing.T) {
	manager := New(seededHost(), Options{})
	for _, terminal := range []State{StateCancelled, StateExpired, StateClosed} {
		request := &Request{RequestID: "req_x", State: terminal}
		for _, to := range []State{StateDelivered, StateAcked, StateOpening, StatePosting, StateClosed} {
			if manager.advance(request, to) {
				t.Errorf("advance moved a %s request to %s", terminal, to)
			}
			if request.State != terminal {
				t.Fatalf("a %s request became %s", terminal, request.State)
			}
		}
	}
	live := &Request{RequestID: "req_y", State: StateDelivered}
	if !manager.advance(live, StateAcked) || live.State != StateAcked {
		t.Fatalf("advance refused a live request: %s", live.State)
	}
}

// Every write of a request's state goes through `advance`, or is one of the few
// transitions that OWN a request's end (they check and claim in one critical
// section). A new bare write is the #2112 shape again.
func TestEveryStateWriteIsGuarded(t *testing.T) {
	source, err := os.ReadFile("manager.go")
	if err != nil {
		t.Fatal(err)
	}
	owners := map[string]bool{"advanceLocked": true, "advance": true, "cancel": true, "fail": true, "forget": true, "closeUpstream": true}
	fn := regexp.MustCompile(`^func \([a-z]+ \*Manager\) ([A-Za-z]+)\(`)
	current := ""
	writes := 0
	for i, line := range strings.Split(string(source), "\n") {
		if m := fn.FindStringSubmatch(line); m != nil {
			current = m[1]
		}
		if strings.Contains(line, ".State = ") && !strings.HasPrefix(strings.TrimSpace(line), "//") {
			writes++
			if !owners[current] {
				t.Errorf("manager.go:%d — %s writes a request's state without advance: %s", i+1, current, strings.TrimSpace(line))
			}
		}
	}
	if writes == 0 {
		t.Fatal("found no state writes — this guard is vacuous")
	}
}

// ackInSendSink runs `during` inside SendBrief, after the brief is on the wire
// and before the manager's own bookkeeping for the send — the window in which a
// plugin on a local socket answers (#2694).
type ackInSendSink struct {
	*recorder
	during func(Brief)
}

func (s *ackInSendSink) SendBrief(ctx context.Context, brief Brief) error {
	if err := s.recorder.SendBrief(ctx, brief); err != nil {
		return err
	}
	if s.during != nil {
		during := s.during
		s.during = nil
		during(brief)
	}
	return nil
}

// 🔴 #2694: an ack that lands before the send's bookkeeping is kept. Moved back
// to `delivered`, the ack sweep later judged the request unanswered and briefed
// the harness a second time — a second turn for one question.
func TestAnAckBeforeTheBriefsBookkeepingIsKept(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_000, 0)
	manager := New(seededHost(), Options{Now: func() time.Time { return now }})
	sink := &ackInSendSink{recorder: newRecorder()}
	sink.during = func(brief Brief) {
		if err := manager.OnAck(ctx, "conn-1", brief.RequestID, AckAccepted, SessionResumed); err != nil {
			t.Errorf("OnAck: %v", err)
		}
	}
	manager.Attach("conn-1", sink, 48000)
	if err := manager.OnRing(ctx, ring("req_1", "c0001", "c0003")); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	state := manager.byID["req_1"].State
	manager.mu.Unlock()
	if state != StatePosting {
		t.Fatalf("state %s after the ack, want %s — the send's bookkeeping undid the ack", state, StatePosting)
	}
	now = now.Add(time.Hour)
	if err := manager.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sink.briefs) != 1 {
		t.Fatalf("%d briefs, want 1 — an answered request was briefed again", len(sink.briefs))
	}
}

// 🔴 #2694: a harness that restarts after acking, inside the send's
// bookkeeping, gets the request redelivered to its new process. The attach's
// RedeliverOpen skipped it as still in flight, and the bookkeeping then pinned it
// to the dead connection, so nothing briefed the new harness.
func TestARestartInsideTheBriefsBookkeepingRedeliversToTheNewProcess(t *testing.T) {
	ctx := context.Background()
	manager := New(seededHost(), Options{})
	manager.BindProcess("process-1")
	first := &ackInSendSink{recorder: newRecorder()}
	second := newRecorder()
	first.during = func(brief Brief) {
		if err := manager.OnAck(ctx, "conn-1", brief.RequestID, AckAccepted, SessionResumed); err != nil {
			t.Errorf("OnAck: %v", err)
		}
		manager.Detach("conn-1")
		manager.Attach("conn-2", second, 48000)
		manager.BindProcess("process-2")
		if err := manager.RedeliverOpen(ctx); err != nil {
			t.Errorf("RedeliverOpen: %v", err)
		}
	}
	manager.Attach("conn-1", first, 48000)
	if err := manager.OnRing(ctx, ring("req_1", "c0001", "c0003")); err != nil {
		t.Fatal(err)
	}
	if len(second.briefs) != 1 || second.briefs[0].RequestID != "req_1" {
		t.Fatalf("the new process got %d brief(s), want req_1 once", len(second.briefs))
	}
}

// The same restart from the SAME process is a reconnect: the acknowledged
// request is still being worked on, and nothing is redelivered.
func TestAReconnectInsideTheBriefsBookkeepingRedeliversNothing(t *testing.T) {
	ctx := context.Background()
	manager := New(seededHost(), Options{})
	manager.BindProcess("process-1")
	first := &ackInSendSink{recorder: newRecorder()}
	second := newRecorder()
	first.during = func(brief Brief) {
		_ = manager.OnAck(ctx, "conn-1", brief.RequestID, AckAccepted, SessionResumed)
		manager.Detach("conn-1")
		manager.Attach("conn-2", second, 48000)
		manager.BindProcess("process-1")
		_ = manager.RedeliverOpen(ctx)
	}
	manager.Attach("conn-1", first, 48000)
	if err := manager.OnRing(ctx, ring("req_1", "c0001", "c0003")); err != nil {
		t.Fatal(err)
	}
	if len(second.briefs) != 0 {
		t.Fatalf("a reconnect of the same process was briefed %d time(s)", len(second.briefs))
	}
	// 🔴 #2694 review: the send's bookkeeping never pins the request back to the
	// connection that detached during it — the reconnected plugin's frames on
	// its new connection are its own.
	if err := manager.OnPost(ctx, "conn-2", "req_1", "still working", nil); err != nil {
		t.Fatalf("a post on the reconnected connection was refused: %v", err)
	}
}

// restartOnAck restarts the harness into a new process the moment an ack is
// observed — between the ack's own state change and the ones that follow it.
type restartOnAck struct {
	NopObserver
	restart func()
}

func (o *restartOnAck) Acked(string, AckOutcome, Session) {
	if o.restart != nil {
		restart := o.restart
		o.restart = nil
		restart()
	}
}

// 🔴 #2694: an ack's later moves (to `opening`, then `posting`) never overwrite a
// request a restart reopened for a NEW process meanwhile — that skipped the
// redelivery, and the new harness never heard of the request. Fresh and
// resumed acks alike.
func TestARestartRightAfterAnAckRedeliversToTheNewProcess(t *testing.T) {
	for _, session := range []Session{SessionFresh, SessionResumed} {
		t.Run(string(session), func(t *testing.T) {
			ctx := context.Background()
			observer := &restartOnAck{}
			manager := New(seededHost(), Options{Observer: observer})
			manager.BindProcess("process-1")
			first, second := newRecorder(), newRecorder()
			observer.restart = func() {
				manager.Detach("conn-1")
				manager.Attach("conn-2", second, 48000)
				manager.BindProcess("process-2")
				if err := manager.RedeliverOpen(ctx); err != nil {
					t.Errorf("RedeliverOpen: %v", err)
				}
			}
			manager.Attach("conn-1", first, 48000)
			if err := manager.OnRing(ctx, ring("req_1", "c0001", "c0003")); err != nil {
				t.Fatal(err)
			}
			if err := manager.OnAck(ctx, "conn-1", "req_1", AckAccepted, session); err != nil {
				t.Fatalf("OnAck: %v", err)
			}
			if len(second.briefs) != 1 {
				t.Fatalf("the new process got %d brief(s), want 1", len(second.briefs))
			}
			if len(first.openers) != 0 {
				t.Fatalf("the dead process was sent an opener for a request handed to the new one")
			}
			manager.mu.Lock()
			state := manager.byID["req_1"].State
			manager.mu.Unlock()
			if state != StateDelivered {
				t.Fatalf("state %s, want %s — the old ack moved the new delivery", state, StateDelivered)
			}
		})
	}
}

// 🔴 #2694 review: a rejected ack is redelivered as `delivered` — unanswered —
// so a redelivered brief nobody acks is still reached by the ack sweep and ends
// in `redelivery_exhausted`, never left to expire unseen.
func TestARejectedAcksRedeliveryIsStillSweptWhenNobodyAcksIt(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_000, 0)
	manager := New(seededHost(), Options{Now: func() time.Time { return now }})
	sink := newRecorder()
	manager.Attach("conn-1", sink, 48000)
	if err := manager.OnRing(ctx, ring("req_1", "c0001", "c0003")); err != nil {
		t.Fatal(err)
	}
	if err := manager.OnAck(ctx, "conn-1", "req_1", AckRejected, SessionFresh); err != nil {
		t.Fatalf("OnAck: %v", err)
	}
	manager.mu.Lock()
	state := manager.byID["req_1"].State
	manager.mu.Unlock()
	if state != StateDelivered || len(sink.briefs) != 2 {
		t.Fatalf("after a rejection: state %s with %d brief(s), want delivered with 2", state, len(sink.briefs))
	}
	now = now.Add(time.Hour)
	if err := manager.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sink.briefs) != 3 {
		t.Fatalf("%d brief(s) after the ack sweep, want 3 — the unanswered redelivery was never swept", len(sink.briefs))
	}
}

// openerHookHost runs `during` inside ReadOpener — after a fresh ack moved the
// request to `opening`, while the opener is read outside the lock.
type openerHookHost struct {
	*memory.Host
	during func()
}

func (h *openerHookHost) ReadOpener(ctx context.Context, conversationID, through string, budget int) ([]contracts.Entry, error) {
	if h.during != nil {
		during := h.during
		h.during = nil
		during()
	}
	return h.Host.ReadOpener(ctx, conversationID, through, budget)
}

// 🔴 #2694 review: a restart into a new process while the opener is read leaves
// the request reopened for that process — the opener's last move never
// overwrites it.
func TestARestartWhileTheOpenerIsReadRedeliversToTheNewProcess(t *testing.T) {
	ctx := context.Background()
	host := &openerHookHost{Host: seededHost()}
	manager := New(host, Options{})
	manager.BindProcess("process-1")
	first, second := newRecorder(), newRecorder()
	host.during = func() {
		manager.Detach("conn-1")
		manager.Attach("conn-2", second, 48000)
		manager.BindProcess("process-2")
		if err := manager.RedeliverOpen(ctx); err != nil {
			t.Errorf("RedeliverOpen: %v", err)
		}
	}
	manager.Attach("conn-1", first, 48000)
	if err := manager.OnRing(ctx, ring("req_1", "c0001", "c0003")); err != nil {
		t.Fatal(err)
	}
	if err := manager.OnAck(ctx, "conn-1", "req_1", AckAccepted, SessionFresh); err != nil {
		t.Fatalf("OnAck: %v", err)
	}
	manager.mu.Lock()
	state := manager.byID["req_1"].State
	manager.mu.Unlock()
	if len(second.briefs) != 1 || state != StateDelivered {
		t.Fatalf("the new process got %d brief(s) and the state is %s; want 1 and delivered", len(second.briefs), state)
	}
	if len(first.openers) != 0 || len(second.openers) != 0 {
		t.Fatalf("openers %d (dead) / %d (new); want none — the new process is briefed afresh", len(first.openers), len(second.openers))
	}
}

// 🔴 #2698: a same-process reconnect while the opener is read gets the opener on
// its NEW connection — the harness is still working on the acked brief. Sent on
// the sink captured before the read, it went to the dead socket and the request
// sat in `opening`, which no redelivery picks up, until its deadline.
func TestAReconnectWhileTheOpenerIsReadGetsTheOpener(t *testing.T) {
	ctx := context.Background()
	host := &openerHookHost{Host: seededHost()}
	manager := New(host, Options{})
	manager.BindProcess("process-1")
	first, second := newRecorder(), newRecorder()
	host.during = func() {
		manager.Detach("conn-1")
		manager.Attach("conn-2", second, 48000)
		manager.BindProcess("process-1")
		_ = manager.RedeliverOpen(ctx)
	}
	manager.Attach("conn-1", first, 48000)
	if err := manager.OnRing(ctx, ring("req_1", "c0001", "c0003")); err != nil {
		t.Fatal(err)
	}
	if err := manager.OnAck(ctx, "conn-1", "req_1", AckAccepted, SessionFresh); err != nil {
		t.Fatalf("OnAck: %v", err)
	}
	manager.mu.Lock()
	state := manager.byID["req_1"].State
	manager.mu.Unlock()
	if len(second.openers) != 1 || len(first.openers) != 0 || state != StatePosting {
		t.Fatalf("openers %d (old) / %d (reconnected), state %s; want 0 / 1 and posting", len(first.openers), len(second.openers), state)
	}
	if len(second.briefs) != 0 {
		t.Fatalf("the reconnected harness was briefed again (%d)", len(second.briefs))
	}
}
