// Package transfer moves files between the organization's Files and the
// workspace on this machine, for a request the companion is answering (#2673,
// epic #2667): a download writes a conversation's attachment or a Files path
// into the workspace, an upload sends a workspace file to the platform —
// attached to the agent's reply, or saved into Files.
//
// 🔴 HARNESS-AGNOSTIC (owner, 2026-10-12): this package knows the platform, the
// local wire and a workspace, and nothing of any harness — no harness's name,
// configuration, path convention or media rule. What a harness needs from a file
// is its PLUGIN's business, through the same two ops every plugin calls. Pinned
// by a test that fails on a harness name anywhere in this package's sources.
//
// 🔴 No whole file in memory (decision 1): every byte streams through a fixed
// buffer, or a tus chunk of a bounded size. 🔴 Never a path, a filename or the
// grant in a log line — ids, sizes and closed classes only.
package transfer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/waits"
)

// Defaults and bounds.
const (
	// DefaultChunkBytes is a tus chunk when UFFICI_UPLOAD_CHUNK_BYTES is unset.
	DefaultChunkBytes = 16 << 20
	// MaxChunkBytes is the agents host's request-body cap: a larger chunk is
	// refused at the edge.
	MaxChunkBytes = 32 << 20
	// DefaultPrefetchMaxBytes is a brief's prefetch budget when
	// UFFICI_PREFETCH_MAX_BYTES is unset.
	DefaultPrefetchMaxBytes = 50 << 20
	// DefaultAttempts is how many tries in a row without progress a transfer
	// makes before it gives up.
	DefaultAttempts = 4
)

// Options configure an Engine.
type Options struct {
	Host      contracts.TransferHost
	Workspace *Workspace
	// ChunkBytes is an upload's tus chunk; zero takes DefaultChunkBytes.
	ChunkBytes int64
	// PrefetchMaxBytes is the companion's own cap on a brief's prefetch; the
	// plugin's ask can only lower it.
	PrefetchMaxBytes int64
	// Attempts bounds the retries without progress; zero takes DefaultAttempts.
	Attempts int
	// Backoff is the pause before retry n (0-based); Sleep waits, or returns
	// early when the context ends. Both injected so tests do not wait.
	Backoff func(attempt int) time.Duration
	Sleep   func(ctx context.Context, d time.Duration) error
	// PrefetchAsked reports the attached plugin's prefetch budget, and whether
	// it asked for prefetch at all — 🔴 the PLUGIN's choice, never this side's.
	PrefetchAsked func() (maxBytes int64, asked bool)
	// Logger receives one line per transfer: ids, sizes, classes. Nil discards.
	Logger *slog.Logger
}

// Engine runs the transfers.
type Engine struct {
	host          contracts.TransferHost
	ws            *Workspace
	chunk         int64
	prefetchMax   int64
	attempts      int
	backoff       func(int) time.Duration
	sleep         func(context.Context, time.Duration) error
	prefetchAsked func() (int64, bool)
	log           *slog.Logger

	mu     sync.Mutex
	seq    uint64
	active map[string]map[uint64]context.CancelFunc
}

// New builds an Engine and clears partial downloads a previous process left:
// none can be resumed, because a partial lives only as long as its grant's call.
func New(options Options) (*Engine, error) {
	if options.Host == nil || options.Workspace == nil {
		return nil, fmt.Errorf("transfer: a host and a workspace are required")
	}
	e := &Engine{
		host: options.Host, ws: options.Workspace,
		chunk: options.ChunkBytes, prefetchMax: options.PrefetchMaxBytes, attempts: options.Attempts,
		backoff: options.Backoff, sleep: options.Sleep, prefetchAsked: options.PrefetchAsked,
		log: options.Logger, active: map[string]map[uint64]context.CancelFunc{},
	}
	if e.chunk <= 0 {
		e.chunk = DefaultChunkBytes
	}
	if e.prefetchMax <= 0 {
		e.prefetchMax = DefaultPrefetchMaxBytes
	}
	if e.chunk > MaxChunkBytes {
		e.chunk = MaxChunkBytes
	}
	if e.attempts <= 0 {
		e.attempts = DefaultAttempts
	}
	if e.backoff == nil {
		e.backoff = func(attempt int) time.Duration { return time.Duration(250*(1<<attempt)) * time.Millisecond }
	}
	if e.sleep == nil {
		e.sleep = waits.Sleep
	}
	if e.prefetchAsked == nil {
		e.prefetchAsked = func() (int64, bool) { return 0, false }
	}
	if e.log == nil {
		e.log = slog.New(slog.DiscardHandler)
	}
	if err := e.ws.clearPartials(); err != nil {
		return nil, fmt.Errorf("transfer: clearing partial downloads: %w", err)
	}
	return e, nil
}

// begin registers one transfer under its request, so Abort can stop it.
func (e *Engine) begin(ctx context.Context, requestID string) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	e.mu.Lock()
	e.seq++
	id := e.seq
	if e.active[requestID] == nil {
		e.active[requestID] = map[uint64]context.CancelFunc{}
	}
	e.active[requestID][id] = cancel
	e.mu.Unlock()
	return ctx, func() {
		cancel()
		e.mu.Lock()
		delete(e.active[requestID], id)
		if len(e.active[requestID]) == 0 {
			delete(e.active, requestID)
		}
		e.mu.Unlock()
	}
}

// Abort stops every transfer of a request — it was cancelled, it expired, or it
// closed. 🔴 Each one deletes its own partial file as it unwinds; nothing here
// blocks, because the request machine calls it inline.
func (e *Engine) Abort(requestID string) {
	e.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(e.active[requestID]))
	for _, cancel := range e.active[requestID] {
		cancels = append(cancels, cancel)
	}
	e.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// Active reports how many transfers are in flight, for tests and the doctor.
func (e *Engine) Active() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, transfers := range e.active {
		n += len(transfers)
	}
	return n
}

// The companion's own failure classes, beside the platform's.
const (
	// ClassPathRefused — the workspace fence refused a path.
	ClassPathRefused = "path_refused"
	// ClassLocalFileMissing — no regular file at the path the plugin named.
	ClassLocalFileMissing = "local_file_missing"
	// ClassLocalWriteFailed — the workspace could not be written.
	ClassLocalWriteFailed = "local_write_failed"
	// ClassChecksumMismatch — the downloaded bytes are not the platform's file;
	// they were deleted.
	ClassChecksumMismatch = "checksum_mismatch"
	// ClassSizeMismatch — the downloaded bytes are not the platform's size.
	ClassSizeMismatch = "size_mismatch"
	// ClassOverBudget — a prefetch the plugin's budget did not cover.
	ClassOverBudget = "over_budget"
	// ClassCancelled — the request ended while the transfer ran.
	ClassCancelled = "cancelled"
	// ClassTransferFailed — the bytes did not cross, and trying again did not help.
	ClassTransferFailed = "transfer_failed"
)

// Failure is a transfer that failed for a reason this side can name: a class
// for the machine and the sentence the agent relays. 🔴 Never a path.
type Failure struct {
	Class       string
	Instruction string
	cause       error
}

func (f *Failure) Error() string { return "transfer: " + f.Class }
func (f *Failure) Unwrap() error { return f.cause }

// ErrorClass is the class, which makes a Failure a contracts.ClassifiedError.
func (f *Failure) ErrorClass() string { return f.Class }

// ErrorInstruction is the sentence the agent relays.
func (f *Failure) ErrorInstruction() string { return f.Instruction }

// The sentences this package relays — platform vocabulary, never tenant content.
var instructions = map[string]string{
	ClassPathRefused: "That path is outside the workspace this agent may use, or names a hidden file; " +
		"use a path inside the workspace.",
	ClassLocalFileMissing: "There is no file at that path in the workspace; check the path and try again.",
	ClassLocalWriteFailed: "The file could not be written into the workspace on this machine; " +
		"tell the person something went wrong on the agent's machine.",
	ClassChecksumMismatch: "The downloaded file did not match the platform's copy and was discarded; " +
		"try the download again.",
	ClassSizeMismatch: "The downloaded file was not the size the platform named and was discarded; " +
		"try the download again.",
	ClassOverBudget: "The file was not downloaded ahead of the turn because it is larger than the budget; " +
		"download it explicitly if it is needed.",
	ClassCancelled: "The request ended before the transfer finished; nothing was kept.",
	ClassTransferFailed: "The file could not be transferred because of a system problem; " +
		"tell the person, and try again shortly.",
}

func fail(class string, cause error) *Failure {
	return &Failure{Class: class, Instruction: instructions[class], cause: cause}
}

// ClassOf is an error's class for a log line or a prefetch mark: the platform's
// own, the companion's, or a closed fallback. Never the error's text.
func ClassOf(err error) string {
	var failure *Failure
	var fence *FenceError
	var classified contracts.ClassifiedError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &fence):
		return ClassPathRefused
	case errors.As(err, &failure):
		return failure.Class
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return ClassCancelled
	case errors.Is(err, contracts.ErrUnknownRequest):
		return "unknown_request"
	case errors.As(err, &classified) && classified.ErrorClass() != "":
		return classified.ErrorClass()
	}
	return ClassTransferFailed
}

// retry decides whether a failed attempt is tried again, and waits the backoff
// when it is. A context that ended never retries. Its twin for an upload's tus
// bytes is the gateway's `Host.retryTransfer` (internal/infra/gateway/files.go):
// the same rule — no retry past the attempts without progress, a non-retryable
// class ends it, Retry-After lengthens the wait — over go-tus's errors. Change
// one, change both.
func (e *Engine) retry(ctx context.Context, err error, failures int) bool {
	if ctx.Err() != nil || failures >= e.attempts {
		return false
	}
	var retryable contracts.RetryableError
	if errors.As(err, &retryable) && !retryable.CanRetry() {
		return false
	}
	delay := e.backoff(failures - 1)
	var after contracts.RetryAfterError
	if errors.As(err, &after) && after.RetryAfterDelay() > delay {
		delay = after.RetryAfterDelay()
	}
	return e.sleep(ctx, delay) == nil
}

// A segment a platform id may become on disk: no separator, no leading dot.
var safeSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,199}$`)

// attachmentIDPattern is an attachment id the companion caches on disk — the
// platform's `<messageId>.<index>`, and anything else WITHOUT a `-`: the id
// prefixes the cached file's name, `<attachmentId>-<name>`, and only an id with
// no `-` of its own ends unambiguously at the first one.
var attachmentIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._]{0,63}$`)

// jtiPattern is a grant's id: a UUID, which becomes a partial file's name.
var jtiPattern = regexp.MustCompile(`^[0-9a-fA-F-]{36}$`)

// maxNameBytes bounds a file name the companion writes: most filesystems take 255.
const maxNameBytes = 255

// localName is a platform file name made safe to be ONE path segment: every
// separator and control character replaced, a leading dot dropped, clipped to
// the filesystem's bound on a rune boundary. Never empty.
func localName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '/' || r == '\\' || r == 0 || r < 0x20 || r == 0x7f:
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.TrimLeft(strings.TrimSpace(b.String()), ".")
	for len(out) > maxNameBytes-80 {
		_, size := lastRune(out)
		out = out[:len(out)-size]
	}
	if out == "" {
		out = "file"
	}
	return out
}

func lastRune(s string) (rune, int) {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i]&0xC0 != 0x80 {
			return rune(s[i]), len(s) - i
		}
	}
	return 0, 1
}

// ensureParent creates a destination's parent inside the workspace, through
// the root — and re-fences the result, so a directory swapped for a link while
// it was created is refused with its reason before the rename (which the root
// would refuse anyway).
func (e *Engine) ensureParent(dest string) error {
	if err := e.ws.mkdirParent(e.ws.Rel(dest)); err != nil {
		return fail(ClassLocalWriteFailed, err)
	}
	if _, err := e.ws.Resolve(dest); err != nil {
		return err
	}
	return nil
}
