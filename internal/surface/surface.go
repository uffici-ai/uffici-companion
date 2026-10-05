// Package surface is the companion's local face: the protocol on the wire of
// #2010, over whatever transport the registry handed it.
//
// 🔴 It owns the PROTOCOL and nothing else. Accepting connections is the
// transport's; the request lifecycle is the state machine's. What lives here is
// the set of rules a plugin can violate — hello first, the token, one attached
// plugin, every frame validated, a frame on the wrong connection — and every
// one of them ends in a close carrying a reason from the wire's closed
// vocabulary. There is no error frame: a violated contract ends the connection.
package surface

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/requests"
	"github.com/uffici-ai/uffici-companion/internal/wire"
)

// Surface serves one plugin at a time.
type Surface struct {
	listener contracts.Listener
	manager  *requests.Manager
	token    string
	log      *slog.Logger
	version  string
	// calls serves the `call` frame's ops (#2028): `logs.push`, `tools.list` and
	// `tools.call` in production; any other op is `unknown_op`.
	calls map[string]CallHandler

	// handlers tracks the live connection handlers, so Shutdown can wait for a
	// close frame to be written before the caller tears the listener down.
	handlers sync.WaitGroup

	mu           sync.Mutex
	attached     bool
	attachedConn contracts.Conn
	connSeq      int
	// pendingSink is the connected-but-not-ready plugin's sink, held until its
	// `ready` frame arrives.
	pendingSink   requests.PluginSink
	pendingBudget int
	// pendingProcess is the connecting plugin's harness process id (#2234).
	pendingProcess string
	// harnessKind and harnessVersion are the last plugin's hello — the facts
	// the companion reports to the platform on its next connection.
	harnessKind    string
	harnessVersion string
	// pluginVersion is the last plugin's own version, reported likewise (#2236).
	pluginVersion string
}

// HarnessFacts reports the harness the last plugin said it translates for, or
// empty strings before any plugin said hello.
func (s *Surface) HarnessFacts() (kind, version string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.harnessKind, s.harnessVersion
}

// PluginVersion reports the last plugin's own version (#2236), or empty before
// any plugin said hello.
func (s *Surface) PluginVersion() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pluginVersion
}

// Options configure a Surface.
type Options struct {
	// Token is the shared secret from `$UFFICI_HOME/wire-token`.
	//
	// 🔴 SECRET-CLASS. It is compared in constant time and never logged — not at
	// any level, not inside an error, not inside a frame dump.
	Token string
	// Logger receives one line per frame. Counts and ids only.
	Logger *slog.Logger
	// Version is reported in the companion's hello.
	Version string
	// Calls maps a `call` op to its handler (#2028). An op missing here is
	// answered `unknown_op`, never a close.
	Calls map[string]CallHandler
}

// New builds a Surface over a listener and a request manager.
func New(listener contracts.Listener, manager *requests.Manager, options Options) (*Surface, error) {
	if listener == nil {
		return nil, fmt.Errorf("surface: no listener")
	}
	if manager == nil {
		return nil, fmt.Errorf("surface: no request manager")
	}
	if options.Token == "" {
		// 🔴 Refused rather than defaulted to "no token needed". A companion that
		// served an unauthenticated wire because its token file was missing would
		// look identical to one that was working.
		return nil, fmt.Errorf("surface: no wire token — the local wire is never served unauthenticated")
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	version := options.Version
	if version == "" {
		version = "0.0.0-dev"
	}
	return &Surface{listener: listener, manager: manager, token: options.Token, log: logger, version: version, calls: options.Calls}, nil
}

// Serve accepts connections until the context ends or the listener closes.
func (s *Surface) Serve(ctx context.Context) error {
	for {
		conn, err := s.listener.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		s.handlers.Add(1)
		go func() {
			defer s.handlers.Done()
			s.handle(ctx, conn)
		}()
	}
}

// handle runs one connection's whole life.
func (s *Surface) handle(ctx context.Context, conn contracts.Conn) {
	connID := s.nextConnID()

	// 🔴 EXACTLY ONE attached plugin. A second is refused before hello is even
	// read: two plugins answering one agent's briefs would post twice for every
	// turn, and the platform would attribute both to the same agent.
	if !s.claim() {
		s.closeWith(conn, string(wire.ReasonAlreadyAttached), connID, "a plugin is already attached")
		return
	}
	defer s.release(connID)

	hello, err := s.readHello(ctx, conn)
	if err != nil {
		var closeErr *closeReason
		if errors.As(err, &closeErr) {
			s.closeWith(conn, string(closeErr.reason), connID, closeErr.detail)
			return
		}
		s.closeWith(conn, string(wire.ReasonMalformedFrame), connID, "the connection ended before hello")
		return
	}

	sink := &frameSink{conn: conn}
	s.mu.Lock()
	s.attachedConn = conn
	s.pendingSink = sink
	s.pendingBudget = hello.ContextBudgetTokens
	s.pendingProcess = hello.ProcessID
	s.harnessKind, s.harnessVersion = hello.Harness, hello.HarnessVersion
	s.pluginVersion = hello.PluginVersion
	s.mu.Unlock()
	s.log.Info("plugin connected",
		slog.String("conn", connID),
		slog.String("harness", hello.Harness),
		slog.String("harnessVersion", hello.HarnessVersion),
		slog.String("pluginVersion", hello.PluginVersion),
		slog.String("processId", hello.ProcessID),
		slog.Int("budgetDeclared", hello.ContextBudgetTokens),
	)

	if err := sink.sendCompanionHello(ctx, s.version, s.features()); err != nil {
		s.closeWith(conn, string(wire.ReasonShuttingDown), connID, "could not answer hello")
		return
	}

	// 🔴 NOT attached yet. `hello` says a plugin is connected; `ready` says its
	// HARNESS can answer, and the two are seconds apart — a harness connects its
	// adapters while it is still starting. Attaching here delivered the first
	// brief of every run into a window where nothing could answer it, and the
	// turn closed `empty` while the real reply arrived later for a request that
	// was already gone (#2014). Rings stay held until `ready`, which is exactly
	// what the manager already does when no plugin is attached at all.

	s.pump(ctx, conn, connID, sink)
}

// maybeReady attaches the plugin when its `ready` frame arrives, and reports
// whether it consumed the frame.
//
// 🔴 The attach lives HERE and not at hello, so every ring stays held until the
// harness can answer one. A second `ready` is ignored rather than refused: a
// plugin that says it twice is telling the truth twice.
func (s *Surface) maybeReady(ctx context.Context, connID string, raw []byte) bool {
	var envelope wire.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Type != "ready" {
		return false
	}
	if err := wire.Validate(raw); err != nil {
		// Malformed: let the ordinary dispatch path close the connection with
		// the reason it earned, rather than swallowing it here.
		return false
	}

	s.mu.Lock()
	sink, budget, process := s.pendingSink, s.pendingBudget, s.pendingProcess
	s.pendingSink = nil
	s.mu.Unlock()
	if sink == nil {
		// Already ready. Nothing to do, and nothing to complain about.
		return true
	}

	s.manager.Attach(connID, sink, budget)
	// 🔴 Before the redelivery below: a RESTARTED harness is handed what its
	// predecessor had acknowledged, a reconnect of the same one is not (#2234).
	reopened := s.manager.BindProcess(process)
	var ready wire.Ready
	_ = json.Unmarshal(raw, &ready)
	s.log.Info("plugin ready",
		slog.String("conn", connID),
		slog.String("harnessVersion", ready.HarnessVersion),
		slog.Int("reopenedAfterRestart", reopened),
	)
	// Anything that arrived while the harness was starting goes out now.
	if err := s.manager.RedeliverOpen(ctx); err != nil {
		s.log.Error("redelivering open requests",
			slog.String("conn", connID), slog.String("error", classify(err)))
	}
	return true
}

// readHello reads the first frame and refuses anything that is not a valid,
// authenticated hello.
func (s *Surface) readHello(ctx context.Context, conn contracts.Conn) (*wire.PluginHello, error) {
	raw, err := conn.Receive(ctx)
	if err != nil {
		return nil, err
	}
	if err := wire.Validate(raw); err != nil {
		var closeErr *wire.CloseError
		if errors.As(err, &closeErr) {
			return nil, &closeReason{reason: closeErr.Reason, detail: closeErr.Detail}
		}
		return nil, &closeReason{reason: wire.ReasonMalformedFrame, detail: "the first frame did not validate"}
	}

	var envelope wire.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, &closeReason{reason: wire.ReasonMalformedFrame, detail: "the first frame is not an object"}
	}
	if envelope.Type != "hello" {
		return nil, &closeReason{reason: wire.ReasonHelloRequired, detail: "the first frame was a " + envelope.Type}
	}

	decoded, err := wire.Decode(raw, wire.FromPlugin)
	if err != nil {
		return nil, &closeReason{reason: wire.ReasonMalformedFrame, detail: "hello did not decode"}
	}
	hello, ok := decoded.(*wire.PluginHello)
	if !ok {
		return nil, &closeReason{reason: wire.ReasonMalformedFrame, detail: "hello was not the plugin's shape"}
	}

	// 🔴 Constant time. A byte-by-byte comparison leaks the token's prefix to
	// anything that can time a local connection, and the whole point of the
	// token is to stop a DIFFERENT local process attaching.
	if subtle.ConstantTimeCompare([]byte(hello.Token), []byte(s.token)) != 1 {
		return nil, &closeReason{reason: wire.ReasonUnauthorized, detail: "the token did not match"}
	}
	return hello, nil
}

// pump reads frames until the connection ends.
func (s *Surface) pump(ctx context.Context, conn contracts.Conn, connID string, sink *frameSink) {
	// A call runs until its own deadline, the plugin detaching, or the companion
	// stopping — whichever comes first. Nobody answers a detached plugin.
	callCtx, cancelCalls := context.WithCancel(ctx)
	defer cancelCalls()
	slots := make(chan struct{}, MaxCallsInFlight)
	for {
		// 🔴 NOT the serving context. Cancelling a WebSocket read cannot be undone:
		// the library breaks the connection, because a half-read frame cannot be
		// resumed — so a companion whose read was cancelled can never afterwards
		// tell the plugin WHY it stopped, and the plugin sees a bare EOF
		// indistinguishable from a crash. The read ends when the connection ends,
		// and Shutdown is what ends it, gracefully and with a reason.
		raw, err := conn.Receive(context.Background())
		if err != nil {
			s.log.Info("plugin detached", slog.String("conn", connID))
			return
		}
		if s.maybeReady(ctx, connID, raw) {
			continue
		}
		if s.maybeCall(callCtx, connID, sink, raw, slots) {
			continue
		}
		if reason, detail := s.dispatch(ctx, connID, raw); reason != "" {
			s.closeWith(conn, string(reason), connID, detail)
			return
		}
	}
}

// maybeCall answers a `call` frame, and reports whether it consumed the frame.
//
// 🔴 Off the dispatch path, because a call is never a close unless its callId
// is unreadable: an oversized one and an unknown op are both ANSWERED, and
// the connection stays up for every other request and call on it.
func (s *Surface) maybeCall(callCtx context.Context, connID string, sink *frameSink, raw []byte, slots chan struct{}) bool {
	var envelope wire.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Type != "call" {
		return false
	}
	decoded, err := wire.Decode(raw, wire.FromPlugin)
	if err != nil {
		var callErr *wire.CallError
		if errors.As(err, &callErr) {
			s.answerMalformed(callCtx, connID, sink, callErr, len(raw))
			return true
		}
		// Any other failure is the close it earned, on the ordinary path.
		return false
	}
	call, ok := decoded.(*wire.Call)
	if !ok {
		return false
	}
	s.startCall(callCtx, connID, sink, call, len(raw), slots)
	return true
}

// dispatch validates one frame and hands it to the state machine. A non-empty
// reason means the connection must close.
func (s *Surface) dispatch(ctx context.Context, connID string, raw []byte) (wire.CloseReason, string) {
	if err := wire.Validate(raw); err != nil {
		var closeErr *wire.CloseError
		if errors.As(err, &closeErr) {
			return closeErr.Reason, closeErr.Detail
		}
		var callErr *wire.CallError
		if errors.As(err, &callErr) {
			// Only a `result` reaches here (a call is answered in maybeCall), and a
			// result is the companion's frame whatever its size.
			return wire.ReasonMalformedFrame, "that frame is the companion's to send"
		}
		return wire.ReasonMalformedFrame, "the frame did not validate"
	}
	decoded, err := wire.Decode(raw, wire.FromPlugin)
	if err != nil {
		return wire.ReasonMalformedFrame, "the frame did not decode"
	}

	switch frame := decoded.(type) {
	case *wire.PluginHello:
		// A second hello on an attached connection is a protocol error, not a
		// re-attach: the state machine would lose track of which brief is whose.
		return wire.ReasonHelloRequired, "hello arrived twice on one connection"
	case *wire.Ack:
		return s.toCloseReason(s.manager.OnAck(ctx, connID, frame.RequestID,
			requests.AckOutcome(frame.Outcome), requests.Session(frame.Session)))
	case *wire.Post:
		return s.toCloseReason(s.manager.OnPost(ctx, connID, frame.RequestID, frame.Text, postExtrasOf(frame)))
	case *wire.Done:
		return s.toCloseReason(s.manager.OnDone(ctx, connID, frame.RequestID,
			contracts.Outcome(frame.Outcome), frame.ErrorClass))
	default:
		// brief, opener, cancel and result are the COMPANION's frames. A plugin
		// sending one is speaking the wrong direction of the protocol.
		return wire.ReasonMalformedFrame, "that frame is the companion's to send"
	}
}

// toCloseReason maps a manager error onto a close reason, or nothing.
func (s *Surface) toCloseReason(err error) (wire.CloseReason, string) {
	switch {
	case err == nil:
		return "", ""
	case errors.Is(err, requests.ErrClosedRequest):
		// 🔴 Ignored, not closed (#2023). The plugin's terminal frame and the
		// companion's cancel cross on the wire whenever a turn is superseded,
		// and closing the connection over it takes down every other request the
		// plugin is working on. Debug rather than warn: this is the protocol
		// working, not a fault — and it carries only the reason, never the frame.
		s.log.Debug("ignoring a frame for a request already closed here")
		return "", ""
	case errors.Is(err, requests.ErrUnknownRequest):
		return wire.ReasonUnknownRequest, "no such open request"
	case errors.Is(err, requests.ErrWrongConnection):
		return wire.ReasonWrongConnection, "that request was delivered on another connection"
	default:
		// 🔴 `classify(err)` and never the error itself: a manager error can wrap
		// a host error that quotes a URL or a payload.
		s.log.Error("handling a frame", slog.String("error", classify(err)))
		return wire.ReasonMalformedFrame, "the frame could not be handled"
	}
}

// Shutdown closes the attached plugin with `shutting_down` and waits for its
// handler to finish.
//
// 🔴 The WAIT is the point, and it is why this is not just DropAttached. The
// caller closes the listener next, and this transport's listener close tears
// down every accepted connection at once — so without waiting, the close frame
// races the teardown and the plugin usually just sees EOF. Bounded, because a
// plugin that never reads must not hold the daemon open.
func (s *Surface) Shutdown() {
	// A run that never had a plugin has nothing to tell, which is not an error.
	_ = s.DropAttached()

	finished := make(chan struct{})
	go func() {
		s.handlers.Wait()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(shutdownGrace):
		s.log.Warn("a plugin connection did not close within the shutdown grace")
	}
}

// shutdownGrace bounds the wait above.
const shutdownGrace = 2 * time.Second

// DropAttached closes the attached plugin connection, if there is one.
//
// 🔴 The reason is `shutting_down` even when the companion is not stopping — the
// fixture's `disconnectPlugin` step uses this to play a companion that went
// away. The vocabulary is closed (eight reasons) and this is a deliberate reuse,
// not a gap: what the plugin must do is identical either way. A companion that
// says `shutting_down` may be back in a second — a restart, a redeploy, an
// operator's `systemctl restart` — so a plugin that treated it as terminal would
// be broken against the real thing too. `docs/local-wire.md` says so where the
// plugin authors read it.
//
// 🔴 For the fixture's `disconnectPlugin` step, and deliberately the DAEMON
// dropping the connection rather than the plugin leaving. From the state
// machine's point of view the two are the same event, and it is the only half a
// script can drive — a script cannot make somebody else's process exit.
func (s *Surface) DropAttached() error {
	s.mu.Lock()
	conn := s.attachedConn
	s.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("surface: no plugin is attached")
	}
	return conn.Close(string(wire.ReasonShuttingDown))
}

func (s *Surface) claim() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.attached {
		return false
	}
	s.attached = true
	return true
}

func (s *Surface) release(connID string) {
	s.mu.Lock()
	s.attached = false
	s.attachedConn = nil
	s.mu.Unlock()
	s.manager.Detach(connID)
}

func (s *Surface) nextConnID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connSeq++
	return fmt.Sprintf("conn-%d", s.connSeq)
}

func (s *Surface) closeWith(conn contracts.Conn, reason, connID, detail string) {
	s.log.Info("closing the plugin connection",
		slog.String("conn", connID), slog.String("reason", reason), slog.String("detail", detail))
	_ = conn.Close(reason)
}

// closeReason carries a wire close reason out of a helper.
type closeReason struct {
	reason wire.CloseReason
	detail string
}

func (c *closeReason) Error() string { return string(c.reason) + ": " + c.detail }

// classify reduces an error to something safe to log.
//
// 🔴 Never `err.Error()`: a wrapped host or transport error can carry a URL, a
// header or a payload, and this string goes straight into a log line.
func classify(err error) string {
	if err == nil {
		return ""
	}
	var closeErr *wire.CloseError
	if errors.As(err, &closeErr) {
		return string(closeErr.Reason)
	}
	switch {
	case errors.Is(err, requests.ErrUnknownRequest):
		return "unknown_request"
	case errors.Is(err, requests.ErrWrongConnection):
		return "wrong_connection"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "context_ended"
	default:
		return "internal"
	}
}

// frameSink writes the companion's frames.
type frameSink struct {
	mu   sync.Mutex
	conn contracts.Conn
}

// frameSendBound caps one frame's send, so a plugin that stops reading cannot
// hold a companion goroutine.
const frameSendBound = 10 * time.Second

// send writes one frame.
//
// 🔴 NOT under the caller's cancellation (#2115). The WebSocket library watches a
// write's context and CLOSES the connection when it ends — and the watcher is only
// disarmed after the write returns. The callers pass the serving context, so a
// companion stopping while a frame was leaving (under CPU pressure: even after its
// bytes were on the wire) tore the socket down underneath the graceful close, and
// the plugin saw a reset instead of `shutting_down`. The frame keeps the caller's
// values, gets its own bound, and shutdown ends the connection the one way that
// tells the plugin why — the same reasoning as the pump's read.
func (f *frameSink) send(ctx context.Context, frame any) error {
	raw, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("encoding a frame: %w", err)
	}
	return f.sendRaw(ctx, raw)
}

// sendRaw writes an already-encoded frame under the same lock and bound —
// for a call result, whose size was measured before it was sent.
func (f *frameSink) sendRaw(ctx context.Context, raw []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), frameSendBound)
	defer cancel()
	return f.conn.Send(bounded, raw)
}

func (f *frameSink) sendCompanionHello(ctx context.Context, version string, features []string) error {
	return f.send(ctx, map[string]any{
		"v": wire.Version, "type": "hello", "companionVersion": version,
		// 🔴 What this companion carries beyond every v1 companion: a plugin sends a
		// question only to a companion that says it can relay one (#2232), and leaves
		// a restarted harness's interrupted turns to a companion that redelivers them
		// (#2273), and calls the agent's tools only through a companion that serves
		// them (#2349).
		"features": features,
	})
}

// features is what this companion's hello advertises. 🔴 `tools` only when both
// tool ops are served: a plugin that is told so offers the tools to its model,
// and a call answered `unknown_op` would then fail a turn that need not have.
func (s *Surface) features() []string {
	features := []string{wire.FeatureQuestion, wire.FeatureRedelivery}
	if s.calls[wire.OpToolsList] != nil && s.calls[wire.OpToolsCall] != nil {
		features = append(features, wire.FeatureTools)
	}
	// 🔴 `memory` only when all FOUR memory ops are served (#2385): a plugin told so
	// recalls before every model call, and a recall answered `unknown_op` would read
	// as a silent gap — the plugin renders "memory unavailable" for a companion that
	// does not announce it.
	if s.calls[wire.OpMemoryRecall] != nil && s.calls[wire.OpMemoryRemember] != nil &&
		s.calls[wire.OpMemoryForget] != nil && s.calls[wire.OpMemoryUnfold] != nil {
		features = append(features, wire.FeatureMemory)
	}
	return features
}

// SendBrief writes one brief.
func (f *frameSink) SendBrief(ctx context.Context, brief requests.Brief) error {
	frame := map[string]any{
		"v": wire.Version, "type": "brief",
		"requestId": brief.RequestID, "conversationId": brief.ConversationID,
		"reason":        brief.Reason,
		"cause":         map[string]any{"entryId": brief.Cause.EntryID, "authorId": brief.Cause.AuthorID},
		"deadline":      formatTime(brief.Deadline),
		"sinceCursor":   nullable(brief.SinceCursor),
		"throughCursor": brief.ThroughCursor,
		"resetSession":  brief.ResetSession,
		"delta":         wireEntries(brief.Delta),
	}
	// Absent, never empty, when the platform sent none: the plugin then keeps
	// its static hint (#2331).
	if brief.Context != nil {
		frame["context"] = wireContext(brief.Context)
	}
	return f.send(ctx, frame)
}

// wireContext maps the platform's context one to one onto the wire's shape.
func wireContext(in *contracts.Context) wire.Context {
	out := wire.Context{Blocks: []wire.ContextBlock{}, Tokens: in.Tokens, Bounded: in.Bounded}
	for _, b := range in.Blocks {
		out.Blocks = append(out.Blocks, wire.ContextBlock{
			Name: b.Name, Scope: b.Scope, Depth: b.Depth, Tokens: b.Tokens, Text: b.Text,
		})
	}
	for _, m := range in.Missing {
		out.Missing = append(out.Missing, wire.ContextMissing{
			Part: m.Part, Class: m.Class, Instruction: m.Instruction,
		})
	}
	return out
}

// SendOpener writes one opener.
func (f *frameSink) SendOpener(ctx context.Context, requestID string, entries []contracts.Entry) error {
	return f.send(ctx, map[string]any{
		"v": wire.Version, "type": "opener",
		"requestId": requestID, "entries": wireEntries(entries),
	})
}

// SendCancel writes one cancel.
func (f *frameSink) SendCancel(ctx context.Context, requestID string, reason contracts.CancelReason) error {
	return f.send(ctx, map[string]any{
		"v": wire.Version, "type": "cancel",
		"requestId": requestID, "reason": wire.CancelReasonOnWire(reason),
	})
}

// SendRelease writes one release (#2356): the plugin may discard what it holds
// for the conversation. Ids and the platform's reason only.
func (f *frameSink) SendRelease(ctx context.Context, conversationID string, reason contracts.ReleaseReason) error {
	return f.send(ctx, map[string]any{
		"v": wire.Version, "type": "release",
		"conversationId": conversationID, "reason": string(reason),
	})
}

// SendFailure writes a `failed` cancel carrying the platform's class and the
// instruction the agent relays (#2077).
func (f *frameSink) SendFailure(ctx context.Context, requestID, class, instruction string) error {
	return f.send(ctx, map[string]any{
		"v": wire.Version, "type": "cancel", "requestId": requestID, "reason": wire.CancelFailed,
		"error": map[string]any{"class": class, "instruction": instruction},
	})
}

func wireEntries(entries []contracts.Entry) []map[string]any {
	out := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		author := map[string]any{
			"type": entry.Author.Type, "id": entry.Author.ID,
			"label": entry.Author.Label, "self": entry.Author.Self,
		}
		if entry.Author.Handle != "" {
			author["handle"] = entry.Author.Handle
		}
		mapped := map[string]any{
			"id": entry.ID, "cursor": entry.Cursor, "at": formatTime(entry.At),
			"author": author, "kind": entry.Kind, "text": entry.Text,
		}
		if len(entry.Attachments) > 0 {
			attachments := make([]map[string]any, 0, len(entry.Attachments))
			for _, attachment := range entry.Attachments {
				attachments = append(attachments, map[string]any{
					"id": attachment.ID, "name": attachment.Name, "ref": attachment.Ref,
				})
			}
			mapped["attachments"] = attachments
		}
		out = append(out, mapped)
	}
	return out
}

func formatTime(at time.Time) string {
	if at.IsZero() {
		return time.Unix(0, 0).UTC().Format(time.RFC3339)
	}
	return at.UTC().Format(time.RFC3339)
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// questionOf carries a post's question from the wire to the host (#2232).
func questionOf(question *wire.Question) *contracts.Question {
	if question == nil {
		return nil
	}
	return &contracts.Question{ID: question.ID, Choices: question.Choices, MultiSelect: question.MultiSelect}
}

// postExtrasOf carries what a post adds beside its text from the wire to the
// host: the question (#2232), the citations and the activity (#2349). Nil when
// the post carries none of them.
func postExtrasOf(post *wire.Post) *contracts.PostExtras {
	if post.Question == nil && len(post.Citations) == 0 && len(post.Activity) == 0 {
		return nil
	}
	extras := &contracts.PostExtras{Question: questionOf(post.Question), Citations: post.Citations}
	for _, a := range post.Activity {
		extras.Activity = append(extras.Activity, contracts.Activity{Tool: a.Tool, Summary: a.Summary})
	}
	return extras
}
