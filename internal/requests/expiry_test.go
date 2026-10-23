package requests

import (
	"context"
	"testing"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
)

// expiringRig wires a manager to the host's doorbell exactly as the daemon
// does, with a clock the test moves and WITHOUT attaching a plugin: a ring
// that arrives here is held, the way it is on a machine whose harness is down.
func expiringRig(t *testing.T, clock *time.Time) (*Manager, *failingHost) {
	t.Helper()
	host := &failingHost{Host: seededHost()}
	manager := New(host, Options{Now: func() time.Time { return *clock }})
	ctx := context.Background()
	claim, err := host.OpenDoorbell(ctx, contracts.Handlers{
		OnRing: func(envelope contracts.Ring) {
			if err := manager.OnRing(ctx, envelope); err != nil {
				t.Errorf("handling the ring: %v", err)
			}
		},
		OnCancel:     func(string, contracts.CancelReason) {},
		OnDisconnect: func(string) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = claim.Close() })
	return manager, host
}

// 🔴 The incident this guards (#2842): the harness was down, the companion was
// connected, the ring was held, and the deadline closed it `cancelled` — which
// the platform keeps silent. The person saw nothing and the agent read Online.
// An expiry with no plugin attached is `no_runtime`, and it must reach the
// platform as an ERROR so the "did not answer" note is written.
func TestAHeldRingExpiringWithNoPluginClosesAsNoRuntime(t *testing.T) {
	clock := time.Date(2026, 10, 19, 17, 50, 35, 0, time.UTC)
	manager, host := expiringRig(t, &clock)

	held := ring("req_1", "c0001", "c0003")
	held.Deadline = clock.Add(5 * time.Minute)
	if err := host.DeliverRing(held); err != nil {
		t.Fatalf("DeliverRing: %v", err)
	}
	if manager.Open() != 1 {
		t.Fatalf("%d open request(s) after a held ring, want 1", manager.Open())
	}

	clock = clock.Add(5*time.Minute + time.Second)
	if err := manager.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if len(host.closes) != 1 {
		t.Fatalf("closes = %+v, want exactly one", host.closes)
	}
	got := host.closes[0]
	if got.requestID != "req_1" || got.outcome != contracts.OutcomeError || got.errorClass != "no_runtime" {
		t.Fatalf("the expiry closed upstream as %+v, want error / no_runtime — a `cancelled` close is silent on the platform, and the person is never told the agent did not answer", got)
	}
	if offset, _ := host.OffsetOf("conv_1"); offset != "c0001" {
		t.Fatalf("an expired request moved the offset to %q", offset)
	}
	if manager.Open() != 0 {
		t.Fatalf("%d request(s) still open after expiry", manager.Open())
	}
}

// A plugin that took the brief and never finished is a harness failure, not a
// decision: the expiry reaches the platform as `error` with the companion's
// `timeout` class (the platform reads it as `harness_error`), and the plugin
// still hears `cancel reason=expired`.
func TestADeliveredRequestExpiringClosesAsATimeoutError(t *testing.T) {
	clock := time.Date(2026, 10, 19, 17, 50, 35, 0, time.UTC)
	manager, host := expiringRig(t, &clock)
	sink := newRecorder()
	manager.Attach("conn-1", sink, 48000)

	expiring := ring("req_1", "c0001", "c0003")
	expiring.Deadline = clock.Add(time.Second)
	if err := host.DeliverRing(expiring); err != nil {
		t.Fatalf("DeliverRing: %v", err)
	}

	clock = clock.Add(2 * time.Second)
	if err := manager.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if got := sink.cancels["req_1"]; got != contracts.CancelExpired {
		t.Fatalf("the plugin heard %q, want %q", got, contracts.CancelExpired)
	}
	if len(host.closes) != 1 {
		t.Fatalf("closes = %+v, want exactly one", host.closes)
	}
	got := host.closes[0]
	if got.outcome != contracts.OutcomeError || got.errorClass != "timeout" {
		t.Fatalf("the expiry closed upstream as %+v, want error / timeout", got)
	}
	if offset, _ := host.OffsetOf("conv_1"); offset != "c0001" {
		t.Fatalf("an expired request moved the offset to %q", offset)
	}
}

// 🔴 An expiry AFTER a post is a slow answer, not a missing one: the platform's
// own expiry writes "did not answer" only when nothing was posted, and a note
// beside the agent's reply would be a lie. So it closes `cancelled` — quiet,
// the offset unmoved — and the plugin still hears `expired`.
func TestAPostedRequestExpiringClosesQuietlyAsCancelled(t *testing.T) {
	clock := time.Date(2026, 10, 19, 17, 50, 35, 0, time.UTC)
	manager, host := expiringRig(t, &clock)
	sink := newRecorder()
	manager.Attach("conn-1", sink, 48000)
	ctx := context.Background()

	expiring := ring("req_1", "c0001", "c0003")
	expiring.Deadline = clock.Add(time.Minute)
	if err := host.DeliverRing(expiring); err != nil {
		t.Fatalf("DeliverRing: %v", err)
	}
	if err := manager.OnAck(ctx, "conn-1", "req_1", AckAccepted, SessionResumed); err != nil {
		t.Fatalf("OnAck: %v", err)
	}
	if err := manager.OnPost(ctx, "conn-1", "req_1", "on it", nil); err != nil {
		t.Fatalf("OnPost: %v", err)
	}

	clock = clock.Add(2 * time.Minute)
	if err := manager.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if got := sink.cancels["req_1"]; got != contracts.CancelExpired {
		t.Fatalf("the plugin heard %q, want %q", got, contracts.CancelExpired)
	}
	if len(host.closes) != 1 || host.closes[0].outcome != contracts.OutcomeCancelled {
		t.Fatalf("closes = %+v, want one `cancelled` — an error close here puts \"the agent did not answer\" next to its reply", host.closes)
	}
	if offset, _ := host.OffsetOf("conv_1"); offset != "c0001" {
		t.Fatalf("an expired request moved the offset to %q", offset)
	}
}

// 🔴 The posted bit outlives a harness restart: `Posts` is reset when a request
// is reopened for the new process (its cap starts over), and the person still
// heard the first process's reply. An expiry after that is still quiet.
func TestThePostedBitSurvivesAReopenForANewHarnessProcess(t *testing.T) {
	clock := time.Date(2026, 10, 19, 17, 50, 35, 0, time.UTC)
	manager, host := expiringRig(t, &clock)
	sink := newRecorder()
	manager.Attach("conn-1", sink, 48000)
	ctx := context.Background()

	expiring := ring("req_1", "c0001", "c0003")
	expiring.Deadline = clock.Add(time.Minute)
	if err := host.DeliverRing(expiring); err != nil {
		t.Fatalf("DeliverRing: %v", err)
	}
	if err := manager.OnAck(ctx, "conn-1", "req_1", AckAccepted, SessionResumed); err != nil {
		t.Fatalf("OnAck: %v", err)
	}
	if err := manager.OnPost(ctx, "conn-1", "req_1", "first half", nil); err != nil {
		t.Fatalf("OnPost: %v", err)
	}
	manager.BindProcess("proc-1")
	manager.Detach("conn-1")
	manager.Attach("conn-2", newRecorder(), 48000)
	if reopened := manager.BindProcess("proc-2"); reopened != 1 {
		t.Fatalf("%d request(s) reopened for the new process, want 1", reopened)
	}
	manager.mu.Lock()
	posts, posted := manager.byID["req_1"].Posts, manager.byID["req_1"].Posted
	manager.mu.Unlock()
	if posts != 0 || !posted {
		t.Fatalf("after the reopen Posts = %d, Posted = %v; want 0 and true", posts, posted)
	}

	clock = clock.Add(2 * time.Minute)
	if err := manager.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(host.closes) != 1 || host.closes[0].outcome != contracts.OutcomeCancelled {
		t.Fatalf("closes = %+v, want one `cancelled`", host.closes)
	}
}

// A stop or a supersede is a decision, never a failure: those still close
// `cancelled`, exactly as before. Pinned beside the expiry so a later edit
// cannot widen the error close to them.
func TestAStopAndASupersedeStillCloseAsCancelled(t *testing.T) {
	t.Run("a person's stop", func(t *testing.T) {
		host := &failingHost{Host: seededHost()}
		manager, _ := failingRig(t, host)
		ctx := context.Background()
		if err := manager.OnRing(ctx, ring("req_1", "c0001", "c0003")); err != nil {
			t.Fatal(err)
		}
		if err := manager.OnPlatformCancel(ctx, "req_1", contracts.CancelStop); err != nil {
			t.Fatal(err)
		}
		if len(host.closes) != 1 || host.closes[0].outcome != contracts.OutcomeCancelled {
			t.Fatalf("closes = %+v, want one `cancelled`", host.closes)
		}
	})

	t.Run("a newer ring on the same conversation", func(t *testing.T) {
		host := &failingHost{Host: seededHost()}
		manager, sink := failingRig(t, host)
		ctx := context.Background()
		if err := manager.OnRing(ctx, ring("req_1", "c0001", "c0002")); err != nil {
			t.Fatal(err)
		}
		if err := manager.OnRing(ctx, ring("req_2", "c0002", "c0003")); err != nil {
			t.Fatal(err)
		}
		if got := sink.cancels["req_1"]; got != contracts.CancelSuperseded {
			t.Fatalf("the superseded request was cancelled with %q, want %q", got, contracts.CancelSuperseded)
		}
		if len(host.closes) != 1 || host.closes[0].requestID != "req_1" || host.closes[0].outcome != contracts.OutcomeCancelled {
			t.Fatalf("closes = %+v, want one `cancelled` for req_1", host.closes)
		}
	})
}
