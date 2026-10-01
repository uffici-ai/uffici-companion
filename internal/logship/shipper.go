package logship

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
)

// Shipping cadence and bounds. A push is at most one batch of what the gateway
// takes (500 lines, its 256 KiB body), and there is only ever one in flight.
const (
	// FlushEvery is how often the shipper looks for lines to send.
	FlushEvery = 10 * time.Second
	// FlushAtBytes sends early when this much is waiting.
	FlushAtBytes = 256 << 10
	// StalenessEscape sends even while the control plane is busy once this
	// long has passed without a send — a busy agent's lines are late, never
	// starved.
	StalenessEscape = 60 * time.Second
	// BatchEntries is the most lines one push carries.
	BatchEntries = 500
	// BatchBytes keeps a push's body under the gateway's 256 KiB with room for
	// its JSON framing.
	BatchBytes = 200 << 10
	// TicketRenewBefore re-mints a logs ticket this long before it expires.
	TicketRenewBefore = 60 * time.Second
	// MaxBackoff caps the wait after repeated failures.
	MaxBackoff = 5 * time.Minute
	// MaxAttempts is how often one batch is tried before it is dropped.
	MaxAttempts = 3
	// PushTimeout bounds one push.
	PushTimeout = 20 * time.Second
)

// Classes the shipper answers differently from "back off and retry".
const (
	classLogsDisabled   = "logs_disabled"
	classInvalidRequest = "invalid_request"
)

// ticketRefusals are the classes a fresh logs ticket cures.
var ticketRefusals = map[string]bool{
	"ticket_expired": true, "ticket_invalid": true, "ticket_spent": true,
	"ticket_audience": true, "ticket_revoked": true,
}

// ShipperOptions configures a Shipper.
type ShipperOptions struct {
	// Host mints logs tickets and takes pushes.
	Host contracts.LogHost
	// Activity says whether conversation traffic is using the platform.
	// Nil counts as always idle.
	Activity contracts.ControlPlaneActivity
	// Ring holds what waits to ship.
	Ring *Ring
	// Client describes who is shipping, read at every push.
	Client func() contracts.LogClient
	// MachineHarnessLevel is this machine's own floor for the harness's lines
	// (`UFFICI_LOG_LEVEL_HARNESS`); the stricter of it and the platform's wins.
	MachineHarnessLevel string
	// PluginLevel is the floor for the plugin's own lines
	// (`UFFICI_LOG_SHIPPING_LEVEL`).
	PluginLevel string
	// Logger receives debug lines under `component=logship`, which the tee
	// never captures. Nil discards.
	Logger *slog.Logger
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Shipper sends the ring to the platform, at the lowest priority there is.
//
// 🔴 It waits for the control plane to be idle, holds at most one push in
// flight, backs off on every refusal, drops a batch after three attempts, and
// logs about itself only at DEBUG under `component=logship` — never a line's
// text, and never the ticket.
type Shipper struct {
	host     contracts.LogHost
	activity contracts.ControlPlaneActivity
	ring     *Ring
	client   func() contracts.LogClient
	machine  string
	plugin   string
	log      *slog.Logger
	now      func() time.Time

	mu      sync.Mutex
	ticket  string
	expires time.Time
	// minted and recheck re-read the platform's policy while shipping is on: a
	// raised harness level must reach the machine within the recheck interval,
	// not only when the ticket nears its expiry an hour later (#2259 review).
	minted  time.Time
	recheck time.Duration
	// sent counts shipped batches, so a Flush can tell progress from a refusal.
	sent     int
	portal   string
	hold     time.Time
	lastSend time.Time
	pending  []Entry
	attempts int
	failures int
	dropped  int
	wake     chan struct{}
	// stepping guards the one-push-in-flight rule against a Flush racing a Tick.
	stepping atomic.Bool
	// enabled is read by the tee on EVERY record, so it is atomic: 🔴 the tee
	// must never take s.mu, because the shipper logs while holding it and the
	// log record passes through the tee — a lock there would deadlock.
	enabled atomic.Bool
}

// NewShipper builds a Shipper. It sends nothing until Run or Tick.
func NewShipper(options ShipperOptions) *Shipper {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	client := options.Client
	if client == nil {
		client = func() contracts.LogClient { return contracts.LogClient{Name: "uffici-companion"} }
	}
	shipper := &Shipper{
		host: options.Host, activity: options.Activity, ring: options.Ring, client: client,
		machine:  normalizeLevel(options.MachineHarnessLevel, "warn"),
		plugin:   normalizeLevel(options.PluginLevel, "info"),
		log:      logger.With(slog.String("component", ExcludedComponent)),
		now:      now,
		portal:   "warn",
		lastSend: now(),
		wake:     make(chan struct{}, 1),
	}
	// Until the platform answers, capture as if on: the first mint decides.
	shipper.enabled.Store(true)
	return shipper
}

// Capturing reports whether lines should still be kept — false once the
// platform said shipping is off, until it says otherwise.
func (s *Shipper) Capturing() bool {
	return s.enabled.Load()
}

// Floors are the effective levels below which a plugin's lines are not kept:
// the harness's (the stricter of this machine's and the platform's) and the
// plugin's own.
func (s *Shipper) Floors() (harness, plugin string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return stricter(s.machine, s.portal), s.plugin
}

// RetryAfter is how long until the shipper will send again — what a busy or
// disabled plugin is told to wait.
func (s *Shipper) RetryAfter() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if wait := s.hold.Sub(s.now()); wait > 0 {
		return wait
	}
	return 0
}

// Dropped counts batches given up on — `uffici doctor` prints it beside the
// ring's own dropped lines.
func (s *Shipper) Dropped() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

// Nudge asks for an early look — the ring passed FlushAtBytes.
func (s *Shipper) Nudge() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run ships until ctx ends, then makes one best-effort flush of at most two
// seconds.
func (s *Shipper) Run(ctx context.Context) {
	ticker := time.NewTicker(FlushEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			s.Flush(flushCtx)
			cancel()
			return
		case <-ticker.C:
			s.Tick(ctx)
		case <-s.wake:
			s.Tick(ctx)
		}
	}
}

// Flush sends what is waiting, ignoring the idle gate — the shutdown path. It
// keeps sending until the ring is empty, a push is refused, or ctx ends. With a
// push already in flight it returns at once: never a second one beside it.
func (s *Shipper) Flush(ctx context.Context) {
	for ctx.Err() == nil {
		if !s.stepping.CompareAndSwap(false, true) {
			return
		}
		s.mu.Lock()
		waiting := s.pending != nil || s.ring.Waiting()
		before := s.sent
		s.mu.Unlock()
		if !waiting || !s.enabled.Load() {
			s.stepping.Store(false)
			return
		}
		s.stepLocked(ctx, true)
		s.stepping.Store(false)
		s.mu.Lock()
		progressed := s.sent != before
		s.mu.Unlock()
		if !progressed {
			return
		}
	}
}

// Tick is one look: mint when needed, and send one batch when the control plane
// is idle (or the staleness escape has passed).
func (s *Shipper) Tick(ctx context.Context) {
	s.step(ctx, false)
}

func (s *Shipper) step(ctx context.Context, force bool) {
	if !s.stepping.CompareAndSwap(false, true) {
		return
	}
	defer s.stepping.Store(false)
	s.stepLocked(ctx, force)
}

// stepLocked is one look, with the one-step-at-a-time claim already held.
func (s *Shipper) stepLocked(ctx context.Context, force bool) {
	now := s.now()
	s.mu.Lock()
	// 🔴 A forced flush never mints while the platform said off: the hold is the
	// platform's recheck then, and the agent key is not spent to hear "no" again.
	if (!force && now.Before(s.hold)) || (force && !s.enabled.Load()) {
		s.mu.Unlock()
		return
	}
	needTicket := s.ticket == "" || !now.Before(s.expires.Add(-TicketRenewBefore)) ||
		(s.recheck > 0 && !now.Before(s.minted.Add(s.recheck)))
	s.mu.Unlock()

	if needTicket && !s.mint(ctx, now) {
		return
	}

	s.mu.Lock()
	// 🔴 The idle gate holds for a RETRY too: a batch that failed once must not
	// go out in the middle of a turn just because its backoff ran out.
	busy := s.activity != nil && s.activity.Busy()
	stale := !now.Before(s.lastSend.Add(StalenessEscape))
	if busy && !stale && !force {
		s.mu.Unlock()
		return
	}
	if s.pending == nil {
		due := force || !now.Before(s.lastSend.Add(FlushEvery)) || s.ring.Bytes() >= FlushAtBytes
		if !due {
			s.mu.Unlock()
			return
		}
		s.pending = s.ring.Drain(BatchEntries, BatchBytes)
		if len(s.pending) == 0 {
			s.pending = nil
			s.lastSend = now
			s.mu.Unlock()
			return
		}
	}
	batch, ticket := s.pending, s.ticket
	s.mu.Unlock()

	pushCtx, cancel := context.WithTimeout(ctx, PushTimeout)
	result, err := s.host.PushLogs(pushCtx, ticket, s.batchOf(batch))
	cancel()
	s.settle(now, len(batch), result, err)
}

// mint asks for the policy and a ticket. It answers whether a push may follow.
func (s *Shipper) mint(ctx context.Context, now time.Time) bool {
	mintCtx, cancel := context.WithTimeout(ctx, PushTimeout)
	shipping, err := s.host.MintLogsTicket(mintCtx)
	cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.backoff(now, err)
		s.log.Debug("logs ticket refused", slog.String("class", classOf(err)))
		return false
	}
	s.failures = 0
	s.portal = normalizeLevel(shipping.HarnessLevel, "warn")
	if !shipping.Enabled {
		s.switchedOff(now, shipping.Recheck)
		return false
	}
	s.enabled.Store(true)
	s.ticket, s.expires, s.minted = shipping.Ticket, shipping.ExpiresAt, now
	s.recheck = shipping.Recheck
	if s.recheck <= 0 {
		s.recheck = 15 * time.Minute
	}
	if s.expires.IsZero() {
		// An answer with no expiry is renewed at the recheck, never every tick.
		s.expires = now.Add(s.recheck)
	}
	return true
}

// settle records a push's outcome. Called with the lock NOT held.
func (s *Shipper) settle(now time.Time, lines int, result contracts.LogPushResult, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		s.pending, s.attempts, s.failures, s.lastSend = nil, 0, 0, now
		s.sent++
		s.log.Debug("log batch shipped", slog.Int("lines", lines),
			slog.Int("accepted", result.Accepted), slog.Int("filtered", result.Filtered))
		return
	}
	class := classOf(err)
	s.log.Debug("log batch refused", slog.String("class", class), slog.Int("lines", lines))
	switch {
	case class == classLogsDisabled:
		s.switchedOff(now, retryAfterOf(err))
		return
	case ticketRefusals[class]:
		// A fresh ticket cures it; the batch keeps its place.
		s.ticket = ""
	case class == classInvalidRequest:
		// Resending the same body cannot succeed.
		s.drop()
		return
	case retryAfterOf(err) > 0:
		// A named wait (shed, rate limited) is the platform pacing us, not a
		// failure of this batch: it keeps its attempts.
		s.backoff(now, err)
		return
	}
	s.attempts++
	if s.attempts >= MaxAttempts {
		s.drop()
	}
	s.backoff(now, err)
}

// switchedOff forgets everything: the platform said shipping is off.
func (s *Shipper) switchedOff(now time.Time, recheck time.Duration) {
	if recheck <= 0 {
		recheck = 15 * time.Minute
	}
	s.enabled.Store(false)
	s.ticket, s.pending, s.attempts = "", nil, 0
	s.ring.Clear()
	s.hold = now.Add(recheck)
	s.log.Debug("log shipping is off for this agent")
}

func (s *Shipper) drop() {
	s.dropped++
	s.pending, s.attempts = nil, 0
}

// backoff holds sending: the platform's named delay, or doubling from ten
// seconds, capped at five minutes. An edge challenge backs off like any other.
func (s *Shipper) backoff(now time.Time, err error) {
	s.failures++
	wait := 10 * time.Second << min(s.failures-1, 5)
	if wait > MaxBackoff {
		wait = MaxBackoff
	}
	if named := retryAfterOf(err); named > wait {
		wait = named
	}
	s.hold = now.Add(wait)
}

func (s *Shipper) batchOf(entries []Entry) contracts.LogBatch {
	batch := contracts.LogBatch{Client: s.client(), Entries: make([]contracts.LogEntry, 0, len(entries))}
	for _, e := range entries {
		batch.Entries = append(batch.Entries, contracts.LogEntry{
			TS: e.TS, Source: e.Source, Level: e.Level, Logger: e.Logger,
			Message: e.Message, Attrs: e.Attrs, Redactions: e.Redactions,
		})
	}
	return batch
}

func classOf(err error) string {
	var classified contracts.ClassifiedError
	if errors.As(err, &classified) {
		return classified.ErrorClass()
	}
	return "unclassified"
}

func retryAfterOf(err error) time.Duration {
	var named contracts.RetryAfterError
	if errors.As(err, &named) {
		return named.RetryAfterDelay()
	}
	return 0
}

// levelRank orders the four levels; an unknown one ranks as warn.
func levelRank(level string) int {
	switch level {
	case "debug":
		return 0
	case "info":
		return 1
	case "error":
		return 3
	default:
		return 2
	}
}

func normalizeLevel(level, fallback string) string {
	switch level {
	case "debug", "info", "warn", "error":
		return level
	default:
		return fallback
	}
}

// stricter is the higher of two floors.
func stricter(a, b string) string {
	if levelRank(a) >= levelRank(b) {
		return a
	}
	return b
}

// AtOrAbove reports whether level passes floor.
func AtOrAbove(level, floor string) bool {
	return levelRank(normalizeLevel(level, "info")) >= levelRank(floor)
}
