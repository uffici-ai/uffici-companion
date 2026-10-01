package logship

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/infra/logsim"
	"github.com/uffici-ai/uffici-companion/internal/redact"
)

// refusal is a classified platform refusal, with the delay it names.
type refusal struct {
	class string
	after time.Duration
}

func (r refusal) Error() string                  { return "refused: " + r.class }
func (r refusal) ErrorClass() string             { return r.class }
func (r refusal) ErrorInstruction() string       { return "" }
func (r refusal) RetryAfterDelay() time.Duration { return r.after }

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type rig struct {
	host    *logsim.Recorder
	ring    *Ring
	clock   *clock
	shipper *Shipper
	logs    *bytes.Buffer
}

func newRig(machine string) *rig {
	c := &clock{t: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)}
	host := logsim.NewRecorder()
	ring := NewRing(100, 1<<20, stamp)
	logs := &bytes.Buffer{}
	shipper := NewShipper(ShipperOptions{
		Host: host, Activity: host, Ring: ring, MachineHarnessLevel: machine, PluginLevel: "info",
		Logger: slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:    c.now,
		Client: func() contracts.LogClient {
			return contracts.LogClient{Name: "uffici-companion", Version: "1.2.3"}
		},
	})
	return &rig{host: host, ring: ring, clock: c, shipper: shipper, logs: logs}
}

func (r *rig) lines(n int) {
	for i := 0; i < n; i++ {
		r.ring.Push(line(i))
	}
}

func (r *rig) pushed() int { return len(r.host.Batches()) }

var ctx = context.Background()

func TestNothingIsSentBeforeTheCadence(t *testing.T) {
	r := newRig("warn")
	r.lines(3)
	r.shipper.Tick(ctx)
	if r.pushed() != 0 || r.host.Mints() != 1 {
		t.Fatalf("pushed %d, minted %d", r.pushed(), r.host.Mints())
	}
	r.clock.advance(FlushEvery)
	r.shipper.Tick(ctx)
	if r.pushed() != 1 || len(r.host.Batches()[0].Entries) != 3 {
		t.Fatalf("pushed %+v", r.host.Batches())
	}
	if got := r.host.Batches()[0].Client.Version; got != "1.2.3" {
		t.Fatalf("client facts: %q", got)
	}
	// The ticket is reused: no second mint while it lives.
	r.lines(1)
	r.clock.advance(FlushEvery)
	r.shipper.Tick(ctx)
	if r.host.Mints() != 1 || r.pushed() != 2 {
		t.Fatalf("minted %d pushed %d", r.host.Mints(), r.pushed())
	}
}

func TestEnoughBytesSendEarly(t *testing.T) {
	r := newRig("warn")
	big := Entry{TS: stamp(), Source: SourceCompanion, Level: "info", Message: strings.Repeat("x", 8000)}
	ring := NewRing(1000, 8<<20, stamp)
	r.shipper.ring = ring
	for ring.Bytes() < FlushAtBytes {
		ring.Push(big)
	}
	r.shipper.Tick(ctx)
	if r.pushed() != 1 {
		t.Fatalf("256 KiB waiting did not send early: %d", r.pushed())
	}
}

// 🔴 Busy gating: nothing is sent while the control plane is busy, it is sent
// once idle, and after the staleness escape it is sent anyway.
func TestPushesWaitForAnIdleControlPlaneWithAnEscape(t *testing.T) {
	r := newRig("warn")
	r.lines(2)
	r.host.SetBusy(true)
	r.clock.advance(FlushEvery)
	r.shipper.Tick(ctx)
	if r.pushed() != 0 {
		t.Fatal("pushed while the control plane was busy")
	}
	r.host.SetBusy(false)
	r.shipper.Tick(ctx)
	if r.pushed() != 1 {
		t.Fatal("idle, and still not sent")
	}

	r.lines(2)
	r.host.SetBusy(true)
	r.clock.advance(StalenessEscape - time.Second)
	r.shipper.Tick(ctx)
	if r.pushed() != 1 {
		t.Fatal("the escape fired early")
	}
	r.clock.advance(time.Second)
	r.shipper.Tick(ctx)
	if r.pushed() != 2 {
		t.Fatal("a busy agent's lines were starved past the escape")
	}
}

// blockingHost holds every push until released.
type blockingHost struct {
	*logsim.Recorder
	release chan struct{}
	entered chan struct{}
}

func (b *blockingHost) PushLogs(ctx context.Context, ticket string, batch contracts.LogBatch) (contracts.LogPushResult, error) {
	b.entered <- struct{}{}
	<-b.release
	return b.Recorder.PushLogs(ctx, ticket, batch)
}

// 🔴 One push in flight: a Tick or a Flush while a push is out does nothing.
func TestAtMostOnePushInFlight(t *testing.T) {
	c := &clock{t: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)}
	host := &blockingHost{Recorder: logsim.NewRecorder(), release: make(chan struct{}), entered: make(chan struct{}, 4)}
	ring := NewRing(100, 1<<20, stamp)
	shipper := NewShipper(ShipperOptions{Host: host, Ring: ring, Now: c.now})
	ring.Push(line(1))
	c.advance(FlushEvery)
	done := make(chan struct{})
	go func() {
		shipper.Tick(ctx)
		close(done)
	}()
	<-host.entered
	ring.Push(line(2))
	shipper.Tick(ctx)
	shipper.Flush(ctx)
	select {
	case <-host.entered:
		t.Fatal("a second push went out while one was in flight")
	default:
	}
	close(host.release)
	<-done
	if len(host.Batches()) != 1 {
		t.Fatalf("%d pushes", len(host.Batches()))
	}
}

// 🔴 The platform's named delay is obeyed, and an edge challenge backs off.
func TestRefusalsBackOffAndHonourTheNamedDelay(t *testing.T) {
	r := newRig("warn")
	r.lines(1)
	r.clock.advance(FlushEvery)
	r.host.FailNextPush(refusal{class: "shed", after: 2 * time.Minute})
	r.shipper.Tick(ctx)
	if r.pushed() != 0 || r.shipper.RetryAfter() != 2*time.Minute {
		t.Fatalf("pushed %d, hold %v", r.pushed(), r.shipper.RetryAfter())
	}
	r.clock.advance(time.Minute)
	r.shipper.Tick(ctx)
	if r.pushed() != 0 {
		t.Fatal("sent before the named delay")
	}
	r.clock.advance(time.Minute)
	r.shipper.Tick(ctx)
	if r.pushed() != 1 {
		t.Fatal("not sent after the named delay")
	}

	r.lines(1)
	r.clock.advance(FlushEvery)
	r.host.FailNextPush(refusal{class: "edge_challenge"})
	r.shipper.Tick(ctx)
	if hold := r.shipper.RetryAfter(); hold != 10*time.Second {
		t.Fatalf("an edge challenge held %v, want the first backoff step", hold)
	}
}

func TestABatchIsDroppedAfterThreeAttemptsAndAnInvalidOneAtOnce(t *testing.T) {
	r := newRig("warn")
	r.lines(1)
	for attempt := 0; attempt < MaxAttempts; attempt++ {
		r.clock.advance(MaxBackoff)
		r.host.FailNextPush(refusal{class: "platform_unavailable"})
		r.shipper.Tick(ctx)
	}
	if r.shipper.Dropped() != 1 || r.shipper.pending != nil {
		t.Fatalf("dropped %d, pending %v", r.shipper.Dropped(), r.shipper.pending)
	}
	r.lines(1)
	r.clock.advance(MaxBackoff)
	r.host.FailNextPush(refusal{class: "invalid_request"})
	r.shipper.Tick(ctx)
	if r.shipper.Dropped() != 2 {
		t.Fatalf("an invalid batch was kept: %d", r.shipper.Dropped())
	}
	// Backoff caps at five minutes however long the failures run.
	for i := 0; i < 10; i++ {
		r.shipper.backoff(r.clock.now(), refusal{class: "x"})
	}
	if r.shipper.RetryAfter() != MaxBackoff {
		t.Fatalf("backoff %v", r.shipper.RetryAfter())
	}
}

func TestATicketRefusalRemintsAndKeepsTheBatch(t *testing.T) {
	r := newRig("warn")
	r.lines(2)
	r.clock.advance(FlushEvery)
	r.host.FailNextPush(refusal{class: "ticket_expired"})
	r.shipper.Tick(ctx)
	r.clock.advance(MaxBackoff)
	r.shipper.Tick(ctx)
	if r.host.Mints() != 2 || r.pushed() != 1 || len(r.host.Batches()[0].Entries) != 2 {
		t.Fatalf("minted %d, pushed %+v", r.host.Mints(), r.host.Batches())
	}
}

// 🔴 Switched off in Uffici: the ring is cleared, capture stops, and the policy
// is asked again after the recheck.
func TestShippingSwitchedOffClearsAndRechecks(t *testing.T) {
	r := newRig("warn")
	r.lines(3)
	r.host.SetShipping(false, "warn")
	r.shipper.Tick(ctx)
	if r.shipper.Capturing() || r.ring.Len() != 0 || r.pushed() != 0 {
		t.Fatalf("capturing %v, ring %d", r.shipper.Capturing(), r.ring.Len())
	}
	r.host.SetShipping(true, "warn")
	r.clock.advance(14 * time.Minute)
	r.shipper.Tick(ctx)
	if r.host.Mints() != 1 {
		t.Fatal("asked again before the recheck")
	}
	r.clock.advance(time.Minute)
	r.shipper.Tick(ctx)
	if !r.shipper.Capturing() || r.host.Mints() != 2 {
		t.Fatalf("capturing %v after the recheck, minted %d", r.shipper.Capturing(), r.host.Mints())
	}

	// A push refused `logs_disabled` does the same.
	r.lines(2)
	r.clock.advance(FlushEvery)
	r.host.FailNextPush(refusal{class: "logs_disabled", after: 900 * time.Second})
	r.shipper.Tick(ctx)
	if r.shipper.Capturing() || r.ring.Len() != 0 || r.shipper.RetryAfter() != 900*time.Second {
		t.Fatalf("after logs_disabled: capturing %v ring %d hold %v",
			r.shipper.Capturing(), r.ring.Len(), r.shipper.RetryAfter())
	}
}

// 🔴 The effective harness floor is the STRICTER of the machine's and Uffici's.
func TestTheHarnessFloorIsTheStricterOfTheTwo(t *testing.T) {
	for _, tc := range []struct{ machine, portal, want string }{
		{"info", "warn", "warn"},
		{"error", "debug", "error"},
		{"debug", "debug", "debug"},
		{"nonsense", "info", "warn"},
	} {
		r := newRig(tc.machine)
		r.host.SetShipping(true, tc.portal)
		r.shipper.Tick(ctx)
		harness, plugin := r.shipper.Floors()
		if harness != tc.want || plugin != "info" {
			t.Errorf("machine %s + Uffici %s → %s/%s, want %s", tc.machine, tc.portal, harness, plugin, tc.want)
		}
	}
}

func TestAFailedMintBacksOff(t *testing.T) {
	c := &clock{t: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)}
	host := &mintFails{}
	shipper := NewShipper(ShipperOptions{Host: host, Ring: NewRing(10, 1<<20, stamp), Now: c.now})
	shipper.Tick(ctx)
	shipper.Tick(ctx)
	if host.calls != 1 || shipper.RetryAfter() != 10*time.Second {
		t.Fatalf("mint calls %d, hold %v", host.calls, shipper.RetryAfter())
	}
}

type mintFails struct{ calls int }

func (m *mintFails) MintLogsTicket(context.Context) (contracts.LogsShipping, error) {
	m.calls++
	return contracts.LogsShipping{}, refusal{class: "unreachable"}
}

func (m *mintFails) PushLogs(context.Context, string, contracts.LogBatch) (contracts.LogPushResult, error) {
	return contracts.LogPushResult{}, nil
}

// Run flushes once on the way out, whatever the cadence says.
func TestRunFlushesOnShutdown(t *testing.T) {
	r := newRig("warn")
	r.lines(2)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		r.shipper.Run(runCtx)
		close(done)
	}()
	r.shipper.Nudge()
	cancel()
	<-done
	if r.pushed() != 1 {
		t.Fatalf("the shutdown flush sent %d batches", r.pushed())
	}
}

// 🔴 The shipper talks about itself at DEBUG under component=logship only —
// which the tee never captures — and never names a line or the ticket.
func TestTheShipperLogsNoContent(t *testing.T) {
	r := newRig("warn")
	r.ring.Push(Entry{TS: stamp(), Source: SourceCompanion, Level: "info", Message: "a person's words"})
	r.clock.advance(FlushEvery)
	r.shipper.Tick(ctx)
	out := r.logs.String()
	if out == "" {
		t.Fatal("no log line at all — the assertion below would be vacuous")
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if !strings.Contains(line, `"component":"logship"`) || !strings.Contains(line, `"level":"DEBUG"`) {
			t.Fatalf("a shipper line outside component=logship at DEBUG: %s", line)
		}
	}
	if strings.Contains(out, "a person's words") || strings.Contains(out, logsim.Ticket) {
		t.Fatalf("content or the ticket reached a log line: %s", out)
	}
}

func TestAtOrAbove(t *testing.T) {
	if !AtOrAbove("error", "warn") || AtOrAbove("info", "warn") || !AtOrAbove("bogus", "info") {
		t.Fatal("level ordering")
	}
}

// 🔴 The shipper logs while holding its own lock, and its logger is the teed
// one; the tee asks the shipper whether to capture on EVERY record. Were that
// question to take the shipper's lock, the first debug line would deadlock the
// process — as it did once, before `enabled` became atomic.
func TestTheShipperLoggingThroughTheTeeNeverDeadlocks(t *testing.T) {
	c := &clock{t: time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)}
	host := logsim.NewRecorder()
	ring := NewRing(100, 1<<20, stamp)
	var shipper *Shipper
	tee := NewTee(TeeOptions{Ring: ring, Redactor: redact.New(), Level: slog.LevelDebug,
		Enabled: func() bool { return shipper.Capturing() }})
	shipper = NewShipper(ShipperOptions{Host: host, Ring: ring, Now: c.now, Logger: slog.New(tee)})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ring.Push(line(1))
		c.advance(FlushEvery)
		shipper.Tick(ctx) // mint + push, each logging at debug
		host.SetShipping(false, "warn")
		c.advance(FlushEvery)
		ring.Push(line(2))
		shipper.Tick(ctx) // refused logs_disabled, logged under the lock
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the shipper deadlocked logging through the tee")
	}
	if len(host.Batches()) != 1 || shipper.Capturing() {
		t.Fatalf("batches %d, capturing %v", len(host.Batches()), shipper.Capturing())
	}
}

// #2259 review: a retried batch honours the idle gate like a first send — its
// backoff running out mid-turn is not a reason to compete with the turn.
func TestARetryWaitsForTheControlPlaneToBeIdle(t *testing.T) {
	r := newRig("warn")
	r.lines(1)
	r.clock.advance(FlushEvery)
	r.host.FailNextPush(refusal{class: "unavailable"})
	r.shipper.Tick(ctx)
	if r.pushed() != 0 {
		t.Fatal("the failed push was recorded")
	}
	r.host.SetBusy(true)
	// Past the first backoff, well inside the staleness escape.
	r.clock.advance(10 * time.Second)
	r.shipper.Tick(ctx)
	if r.pushed() != 0 {
		t.Fatal("a retry went out while a call was in flight")
	}
	r.host.SetBusy(false)
	r.shipper.Tick(ctx)
	if r.pushed() != 1 {
		t.Fatalf("%d pushes once idle, want 1", r.pushed())
	}
}

// #2259 review: a level raised in the portal reaches the machine at the recheck,
// not only when the hour-long ticket nears its expiry.
func TestThePolicyIsReadAgainAtTheRecheck(t *testing.T) {
	r := newRig("debug")
	r.lines(1)
	r.clock.advance(FlushEvery)
	r.shipper.Tick(ctx)
	if r.host.Mints() != 1 {
		t.Fatalf("%d mints, want 1", r.host.Mints())
	}
	r.host.SetShipping(true, "info")
	r.clock.advance(14 * time.Minute)
	r.shipper.Tick(ctx)
	if r.host.Mints() != 1 {
		t.Fatalf("re-minted before the recheck: %d", r.host.Mints())
	}
	r.clock.advance(time.Minute)
	r.shipper.Tick(ctx)
	if r.host.Mints() != 2 {
		t.Fatalf("%d mints after the recheck, want 2", r.host.Mints())
	}
	if harness, _ := r.shipper.Floors(); harness != "info" {
		t.Fatalf("the harness floor is %q after the recheck, want info", harness)
	}
}

// #2259 review: a forced flush while the platform said off spends no key.
func TestAFlushWhileSwitchedOffMintsNothing(t *testing.T) {
	r := newRig("warn")
	r.host.SetShipping(false, "warn")
	r.lines(1)
	r.clock.advance(FlushEvery)
	r.shipper.Tick(ctx)
	mints := r.host.Mints()
	r.lines(1)
	r.shipper.Flush(ctx)
	if r.host.Mints() != mints {
		t.Fatalf("the flush minted while off: %d → %d", mints, r.host.Mints())
	}
}

// #2259 review: a named wait is the platform pacing the machine, so three of
// them in a row do not throw a healthy batch away.
func TestNamedWaitsDoNotSpendTheBatchsAttempts(t *testing.T) {
	r := newRig("warn")
	r.lines(1)
	r.clock.advance(FlushEvery)
	for i := 0; i < MaxAttempts+1; i++ {
		r.host.FailNextPush(refusal{class: "rate_limited", after: time.Minute})
		r.shipper.Tick(ctx)
		r.clock.advance(MaxBackoff)
	}
	r.shipper.Tick(ctx)
	if r.pushed() != 1 || r.shipper.Dropped() != 0 {
		t.Fatalf("pushed %d, dropped %d — the batch should have survived its waits", r.pushed(), r.shipper.Dropped())
	}
}

// The shutdown flush sends everything waiting, not just one batch.
func TestAFlushDrainsTheRing(t *testing.T) {
	host := logsim.NewRecorder()
	ring := NewRing(BatchEntries*3, 1<<22, stamp)
	shipper := NewShipper(ShipperOptions{Host: host, Ring: ring})
	for i := 0; i < BatchEntries*2+3; i++ {
		ring.Push(line(i))
	}
	shipper.Flush(ctx)
	if len(host.Batches()) != 3 || ring.Len() != 0 {
		t.Fatalf("pushed %d batches, %d lines left", len(host.Batches()), ring.Len())
	}
}

// 🔴 #2259 review: a ring holding only the note that lines were dropped still has
// something to say at shutdown — Len reads zero, and the loss went unreported.
func TestAFlushSendsALoneDroppedNote(t *testing.T) {
	host := logsim.NewRecorder()
	ring := NewRing(10, 200, stamp)
	ring.Push(Entry{TS: stamp(), Source: SourceCompanion, Level: "info", Message: strings.Repeat("x", 500)})
	if ring.Len() != 0 || ring.Dropped() != 1 {
		t.Fatalf("setup: %d waiting, %d dropped", ring.Len(), ring.Dropped())
	}
	NewShipper(ShipperOptions{Host: host, Ring: ring}).Flush(ctx)
	batches := host.Batches()
	if len(batches) != 1 || len(batches[0].Entries) != 1 || batches[0].Entries[0].Message != "logs_dropped" {
		t.Fatalf("the dropped note was not shipped: %+v", batches)
	}
}
