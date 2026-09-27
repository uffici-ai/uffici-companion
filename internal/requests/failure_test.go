package requests

import (
	"context"
	"errors"
	"testing"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/infra/memory"
)

// classified is a host failure with decision 32's two halves.
type classified struct{ class, instruction string }

func (c *classified) Error() string            { return "host: " + c.class }
func (c *classified) ErrorClass() string       { return c.class }
func (c *classified) ErrorInstruction() string { return c.instruction }

// failingHost is an agents host whose reads or post fail with a class, and
// which records every close.
type failingHost struct {
	*memory.Host
	readErr, openerErr, postErr error
	closes                      []closeCall
}

type closeCall struct {
	requestID  string
	outcome    contracts.Outcome
	errorClass string
}

func (f *failingHost) ReadSince(ctx context.Context, conversationID, since, through string) ([]contracts.Entry, bool, error) {
	if f.readErr != nil {
		return nil, false, f.readErr
	}
	return f.Host.ReadSince(ctx, conversationID, since, through)
}

func (f *failingHost) ReadOpener(ctx context.Context, conversationID, through string, budget int) ([]contracts.Entry, error) {
	if f.openerErr != nil {
		return nil, f.openerErr
	}
	return f.Host.ReadOpener(ctx, conversationID, through, budget)
}

func (f *failingHost) Post(ctx context.Context, requestID, conversationID, text string) (string, error) {
	if f.postErr != nil {
		return "", f.postErr
	}
	return f.Host.Post(ctx, requestID, conversationID, text)
}

func (f *failingHost) CloseRequest(ctx context.Context, requestID string, outcome contracts.Outcome, errorClass string) error {
	f.closes = append(f.closes, closeCall{requestID, outcome, errorClass})
	return f.Host.CloseRequest(ctx, requestID, outcome, errorClass)
}

func failingRig(t *testing.T, host *failingHost) (*Manager, *recorder) {
	t.Helper()
	manager := New(host, Options{})
	sink := newRecorder()
	manager.Attach("conn-1", sink, 48000)
	claim, err := host.OpenDoorbell(context.Background(), contracts.Handlers{
		OnRing: func(contracts.Ring) {}, OnCancel: func(string, contracts.CancelReason) {}, OnDisconnect: func(string) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = claim.Close() })
	// The host must know the request for its close to commit; ring through it,
	// then hand the ring to the manager as the daemon does.
	if err := host.DeliverRing(ring("req_1", "c0001", "c0003")); err != nil {
		t.Fatal(err)
	}
	return manager, sink
}

var unavailable = &classified{class: "platform_unavailable", instruction: "The answer could not be delivered; tell the person."}

// 🔴 Decision 32 on the post path: the plugin learns the class AND the
// instruction, the platform learns the request failed, and the plugin's
// connection survives — a platform failure is not a protocol error.
func TestAFailedPostIsRelayedAndClosedAsAnError(t *testing.T) {
	ctx := context.Background()
	host := &failingHost{Host: seededHost(), postErr: unavailable}
	manager, sink := failingRig(t, host)
	if err := manager.OnRing(ctx, ring("req_1", "c0001", "c0003")); err != nil {
		t.Fatal(err)
	}
	if err := manager.OnAck(ctx, "conn-1", "req_1", AckAccepted, SessionResumed); err != nil {
		t.Fatal(err)
	}
	if err := manager.OnPost(ctx, "conn-1", "req_1", "hello"); err != nil {
		t.Fatalf("OnPost answered %v — the surface would drop the plugin over a platform failure", err)
	}
	got, ok := sink.failures["req_1"]
	if !ok || got[0] != "platform_unavailable" || got[1] != unavailable.instruction {
		t.Fatalf("the plugin was told %v, want the class and the instruction", got)
	}
	if len(host.closes) != 1 || host.closes[0].outcome != contracts.OutcomeError || host.closes[0].errorClass != "platform_unavailable" {
		t.Fatalf("closed upstream as %+v, want one error close carrying the class", host.closes)
	}
	if offset, _ := host.OffsetOf("conv_1"); offset != "c0001" {
		t.Fatalf("a failure moved the offset to %s", offset)
	}
	// A done that crossed the failure is ignored, not fatal.
	if err := manager.OnDone(ctx, "conn-1", "req_1", contracts.OutcomeAnswered, ""); !errors.Is(err, ErrClosedRequest) {
		t.Fatalf("a crossing done gave %v, want ErrClosedRequest", err)
	}
}

func TestAPostToARequestThePlatformClosedIsNotClosedAgain(t *testing.T) {
	ctx := context.Background()
	host := &failingHost{Host: seededHost(), postErr: &classified{
		class: contracts.ClassRequestClosed, instruction: "The request was cancelled or closed; do not post further.",
	}}
	manager, sink := failingRig(t, host)
	if err := manager.OnRing(ctx, ring("req_1", "c0001", "c0003")); err != nil {
		t.Fatal(err)
	}
	if err := manager.OnPost(ctx, "conn-1", "req_1", "late"); err != nil {
		t.Fatal(err)
	}
	if got := sink.failures["req_1"]; got[0] != contracts.ClassRequestClosed {
		t.Fatalf("the plugin was told %v, want request_closed", got)
	}
	if len(host.closes) != 0 {
		t.Fatalf("a request the platform closed was closed again: %+v", host.closes)
	}
	if manager.Open() != 0 {
		t.Fatal("the closed request was kept")
	}
}

// 🔴 A failed read is closed as an ERROR, never left to expire: an expiry
// closes as `cancelled`, which tells the person nothing at all.
func TestAFailedDeltaReadClosesAsAnErrorBeforeAnyBrief(t *testing.T) {
	ctx := context.Background()
	host := &failingHost{Host: seededHost(), readErr: unavailable}
	manager, sink := failingRig(t, host)
	if err := manager.OnRing(ctx, ring("req_1", "c0001", "c0003")); err == nil {
		t.Fatal("a failed read was not reported to the caller's log")
	}
	if len(sink.briefs) != 0 || len(sink.failures) != 0 {
		t.Fatalf("the plugin heard of a request it was never briefed on: %d briefs, %v", len(sink.briefs), sink.failures)
	}
	if len(host.closes) != 1 || host.closes[0].outcome != contracts.OutcomeError {
		t.Fatalf("closed upstream as %+v, want one error close", host.closes)
	}
	if manager.Open() != 0 {
		t.Fatal("the failed request was kept open")
	}
}

func TestAFailedOpenerReadIsRelayedToThePlugin(t *testing.T) {
	ctx := context.Background()
	host := &failingHost{Host: seededHost(), openerErr: unavailable}
	manager, sink := failingRig(t, host)
	if err := manager.OnRing(ctx, ring("req_1", "c0001", "c0003")); err != nil {
		t.Fatal(err)
	}
	if err := manager.OnAck(ctx, "conn-1", "req_1", AckAccepted, SessionFresh); err == nil {
		t.Fatal("a failed opener read was not reported")
	}
	if got := sink.failures["req_1"]; got[0] != "platform_unavailable" {
		t.Fatalf("the plugin was told %v", got)
	}
	if len(host.closes) != 1 || host.closes[0].outcome != contracts.OutcomeError {
		t.Fatalf("closed upstream as %+v", host.closes)
	}
}

func TestAnUnclassifiedFailureStillCarriesAnInstruction(t *testing.T) {
	class, instruction := Classify(errors.New("boom"))
	if class != "internal" || instruction == "" {
		t.Fatalf("Classify gave %q / %q — never a silent failure", class, instruction)
	}
	class, instruction = Classify(&classified{class: "edge_challenge"})
	if class != "edge_challenge" || instruction == "" {
		t.Fatalf("a class with no instruction gave %q / %q", class, instruction)
	}
}

// 🔴 The gateway's own redelivery (#2077 review): the same request under a NEW
// ring id — the ack was late, or the socket dropped before it — must refresh
// the request, never supersede it. Superseding closed it `cancelled` upstream
// and lost the answer with nobody told.
func TestARedeliveredRingIsNotASupersede(t *testing.T) {
	ctx := context.Background()
	host := &failingHost{Host: seededHost()}
	manager, sink := failingRig(t, host)
	first := ring("req_1", "c0001", "c0003")
	if err := manager.OnRing(ctx, first); err != nil {
		t.Fatal(err)
	}
	again := first
	again.RingID = "ring_redelivered"
	if err := manager.OnRing(ctx, again); err != nil {
		t.Fatal(err)
	}
	if len(sink.cancels) != 0 || len(host.closes) != 0 {
		t.Fatalf("a redelivery cancelled its own request: cancels %v, closes %+v", sink.cancels, host.closes)
	}
	if len(sink.briefs) != 1 || manager.Open() != 1 {
		t.Fatalf("%d briefs, %d open — a redelivery is one request", len(sink.briefs), manager.Open())
	}
	if err := manager.OnAck(ctx, "conn-1", "req_1", AckAccepted, SessionResumed); err != nil {
		t.Fatalf("the request did not survive its redelivery: %v", err)
	}
}

func TestARingForARequestAlreadyClosedIsIgnored(t *testing.T) {
	ctx := context.Background()
	host := &failingHost{Host: seededHost()}
	manager, sink := failingRig(t, host)
	if err := manager.OnRing(ctx, ring("req_1", "c0001", "c0003")); err != nil {
		t.Fatal(err)
	}
	if err := manager.OnDone(ctx, "conn-1", "req_1", contracts.OutcomeAnswered, ""); err != nil {
		t.Fatal(err)
	}
	late := ring("req_1", "c0001", "c0003")
	late.RingID = "ring_late"
	if err := manager.OnRing(ctx, late); err != nil {
		t.Fatal(err)
	}
	if len(sink.briefs) != 1 || manager.Open() != 0 {
		t.Fatalf("a closed request was opened again: %d briefs, %d open", len(sink.briefs), manager.Open())
	}
}

// A close whose I/O lands after a newer request took the conversation must not
// free the newer request's slot (#2077 review).
func TestAStaleCloseLeavesTheNewerRequestInItsSlot(t *testing.T) {
	ctx := context.Background()
	host := &failingHost{Host: seededHost()}
	manager, _ := failingRig(t, host)
	if err := manager.OnRing(ctx, ring("req_1", "c0001", "c0003")); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	old := manager.byID["req_1"]
	manager.mu.Unlock()
	if err := host.DeliverRing(ring("req_2", "c0001", "c0003")); err != nil {
		t.Fatal(err)
	}
	if err := manager.OnRing(ctx, ring("req_2", "c0001", "c0003")); err != nil {
		t.Fatal(err)
	}
	manager.forget(old)
	manager.mu.Lock()
	current := manager.byConversation["conv_1"]
	manager.mu.Unlock()
	if current == nil || current.RequestID != "req_2" {
		t.Fatalf("the stale close freed the newer request's slot: %+v", current)
	}
}

// racingSink runs `during` inside SendFailure — the window a real send spends on
// the wire, outside the manager's lock.
type racingSink struct {
	*recorder
	during func()
}

func (r *racingSink) SendFailure(ctx context.Context, requestID, class, instruction string) error {
	if r.during != nil {
		r.during()
	}
	return r.recorder.SendFailure(ctx, requestID, class, instruction)
}

func TestAFailureClaimsTheRequestBeforeItsIO(t *testing.T) {
	ctx := context.Background()
	host := &failingHost{Host: seededHost(), postErr: unavailable}
	manager, _ := failingRig(t, host)
	sink := &racingSink{recorder: newRecorder()}
	manager.Attach("conn-1", sink, 48000)
	if err := manager.OnRing(ctx, ring("req_1", "c0001", "c0003")); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	request := manager.byID["req_1"]
	manager.mu.Unlock()
	// A supersede racing the failure's send must find the request already over.
	sink.during = func() { _ = manager.cancel(ctx, request, contracts.CancelSuperseded) }
	_ = manager.OnPost(ctx, "conn-1", "req_1", "x")
	if len(host.closes) != 1 || len(sink.cancels) != 0 {
		t.Fatalf("closed %d times, cancelled %v — the failure did not claim the request first", len(host.closes), sink.cancels)
	}
}
