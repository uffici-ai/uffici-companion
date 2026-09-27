package gateway

import (
	"context"
	"errors"
	"testing"
	"time"
)

func exact() *backoff { return &backoff{jitter: func() float64 { return 0.5 }} }

func TestTheOrdinaryStepDoublesToTheCeilingWithJitter(t *testing.T) {
	b := exact()
	var got []time.Duration
	for range 8 {
		got = append(got, b.next(endTransient, 0))
	}
	// Equal jitter at 0.5: three quarters of 1, 2, 4, 8, 16, 32, 60, 60 seconds.
	want := []time.Duration{750 * time.Millisecond, 1500 * time.Millisecond, 3 * time.Second, 6 * time.Second,
		12 * time.Second, 24 * time.Second, 45 * time.Second, 45 * time.Second}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("step %d waited %v, want %v (all: %v)", i, got[i], want[i], got)
		}
	}
	b.connected()
	if d := b.next(endTransient, 0); d != 750*time.Millisecond {
		t.Fatalf("after a hello the step restarted at %v", d)
	}
}

func TestTheJitterStaysInsideHalfToWhole(t *testing.T) {
	for _, jitter := range []float64{0, 0.999999} {
		b := &backoff{jitter: func() float64 { return jitter }}
		d := b.next(endTransient, 0)
		if d < BackoffFloor/2 || d > BackoffFloor {
			t.Fatalf("jitter %v gave %v, outside [%v, %v]", jitter, d, BackoffFloor/2, BackoffFloor)
		}
	}
	if newBackoff().jitter == nil {
		t.Fatal("the production policy has no jitter source")
	}
}

// 🔴 #2094: the first conflict is the ordinary step, a streak is the ceiling,
// and anything else in between breaks the streak.
func TestAConflictStreakGoesToTheCeiling(t *testing.T) {
	b := exact()
	if d := b.next(endConflict, 0); d >= BackoffCeiling {
		t.Fatalf("a first conflict waited %v", d)
	}
	if d := b.next(endConflict, 0); d != BackoffCeiling {
		t.Fatalf("a second conflict waited %v", d)
	}
	b.next(endTransient, 0)
	if d := b.next(endConflict, 0); d >= BackoffCeiling {
		t.Fatalf("a conflict after a different end waited %v — the streak did not reset", d)
	}
	b.connected()
	if b.conflict == 0 {
		t.Fatal("a hello reset the conflict streak — two companions sharing a home would flap for ever")
	}
	b.stable()
	if b.conflict != 0 {
		t.Fatal("a connection that held did not end the streak")
	}
}

func TestRefusalShutdownAndANamedDelay(t *testing.T) {
	b := exact()
	if d := b.next(endRefused, 0); d != BackoffCeiling {
		t.Fatalf("a refused key waited %v", d)
	}
	if d := b.next(endShutdown, 0); d != BackoffFloor {
		t.Fatalf("a drain waited %v", d)
	}
	if d := b.next(endTransient, 90*time.Second); d != 90*time.Second {
		t.Fatalf("a named 90s delay was cut to %v", d)
	}
}

func TestSleepContextReturnsEarlyOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepContext(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled sleep returned %v", err)
	}
	if err := sleepContext(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("a short sleep returned %v", err)
	}
}

func TestErrorsRenderWithoutABody(t *testing.T) {
	if got := (&Error{Class: "unreachable"}).Error(); got != "gateway: unreachable" {
		t.Fatalf("rendered %q", got)
	}
	if got := (&Error{Class: "platform_error", Status: 502}).Error(); got != "gateway: platform_error (HTTP 502)" {
		t.Fatalf("rendered %q", got)
	}
	if classOf(errors.New("plain")) != "" || retryable(errors.New("plain")) {
		t.Fatal("a plain error was classified")
	}
	if e := classifyResponse(500, "application/json", []byte(`not json`)); e.Class != ClassPlatformError {
		t.Fatalf("an unreadable JSON 500 classified as %s", e.Class)
	}
}

func TestKindOfEveryDisconnectReason(t *testing.T) {
	for reason, want := range map[string]endKind{
		"instance_conflict": endConflict, "key_revoked": endRefused, "server_shutdown": endShutdown,
		"idle": endTransient, "protocol_error": endTransient, "ticket_expired": endTransient,
	} {
		if got := kindOfReason(reason); got != want {
			t.Fatalf("%s is kind %d, want %d", reason, got, want)
		}
	}
}
