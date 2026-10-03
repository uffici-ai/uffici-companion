package surface

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/infra/memory"
	memtransport "github.com/uffici-ai/uffici-companion/internal/infra/transport/memory"
	"github.com/uffici-ai/uffici-companion/internal/requests"
	"github.com/uffici-ai/uffici-companion/internal/wire"
)

const testToken = "test-wire-token-example-not-a-secret-0000"

// rig runs the REAL surface over the in-memory transport: real frames, real
// validation, real state machine, no socket.
type rig struct {
	surface  *Surface
	listener *memtransport.Listener
	host     *memory.Host
	manager  *requests.Manager
	cancel   context.CancelFunc
}

func newRig(t *testing.T) *rig {
	t.Helper()
	return newRigWith(t, Options{})
}

// newRigWith builds the rig with the given options; the token and version are
// always the rig's own.
func newRigWith(t *testing.T, options Options) *rig {
	t.Helper()
	host := memory.New()
	host.Seed("conv_1", "c0001", []contracts.Entry{
		{ID: "e1", Cursor: "c0001", Kind: "message", Text: "first",
			Author: contracts.Author{Type: "member", ID: "m1", Label: "Ana"}},
		{ID: "e2", Cursor: "c0002", Kind: "message", Text: "second",
			Author: contracts.Author{Type: "member", ID: "m1", Label: "Ana"}},
	})
	manager := requests.New(host, requests.Options{})
	listener := memtransport.NewListener("memory://surface")
	options.Token, options.Version = testToken, "0.1.0"
	surface, err := New(listener, manager, options)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = surface.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
	})
	return &rig{surface: surface, listener: listener, host: host, manager: manager, cancel: cancel}
}

// dial connects a test plugin and optionally completes the hello handshake.
//
// 🔴 `hello` alone leaves the plugin CONNECTED, not attached: no brief is
// delivered until it also says `ready` (#2014). Tests that want a working
// plugin use `attach`.
func (r *rig) dial(t *testing.T, token string, sayHello bool) *memtransport.Conn {
	t.Helper()
	conn, err := r.listener.Dial()
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if !sayHello {
		return conn
	}
	send(t, conn, map[string]any{
		"v": 1, "type": "hello", "harness": "hermes", "harnessVersion": "0.21.3",
		"pluginVersion": "0.1.0", "token": token, "contextBudgetTokens": 48000,
	})
	return conn
}

// attach says hello AND ready — a plugin whose harness can answer.
func (r *rig) attach(t *testing.T, token string) *memtransport.Conn {
	t.Helper()
	conn := r.dial(t, token, true)
	send(t, conn, map[string]any{"v": 1, "type": "ready", "harnessVersion": "0.21.3"})
	return conn
}

func send(t *testing.T, conn *memtransport.Conn, frame map[string]any) {
	t.Helper()
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	if err := conn.Send(context.Background(), raw); err != nil {
		t.Fatalf("send: %v", err)
	}
}

// receiveWithin returns the next frame, or nil when none arrives in the window.
// For asserting that something is NOT sent.
func receiveWithin(t *testing.T, conn *memtransport.Conn, window time.Duration) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), window)
	defer cancel()
	raw, err := conn.Receive(ctx)
	if err != nil {
		return nil
	}
	var frame map[string]any
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	return frame
}

func receive(t *testing.T, conn *memtransport.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	raw, err := conn.Receive(ctx)
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	var frame map[string]any
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	return frame
}

// waitClosed waits for the surface to close the connection and returns why.
func waitClosed(t *testing.T, conn *memtransport.Conn) string {
	t.Helper()
	select {
	case <-conn.Closed():
		return conn.CloseReason()
	case <-time.After(2 * time.Second):
		t.Fatal("the surface never closed the connection")
		return ""
	}
}

func TestAValidHelloIsAnswered(t *testing.T) {
	r := newRig(t)
	conn := r.dial(t, testToken, true)
	frame := receive(t, conn)
	if frame["type"] != "hello" {
		t.Fatalf("the companion answered with a %v, want hello", frame["type"])
	}
	if frame["companionVersion"] != "0.1.0" {
		t.Fatalf("companionVersion is %v, want 0.1.0", frame["companionVersion"])
	}
}

// 🔴 The token is the only thing standing between this wire and a different
// local process. A wrong one closes with `unauthorized`, never with a hint.
func TestAWrongTokenIsRefused(t *testing.T) {
	r := newRig(t)
	conn := r.dial(t, "the-wrong-token-------------------", true)
	if got := waitClosed(t, conn); got != string(wire.ReasonUnauthorized) {
		t.Fatalf("closed with %q, want %q", got, wire.ReasonUnauthorized)
	}
}

func TestAFrameBeforeHelloIsRefused(t *testing.T) {
	r := newRig(t)
	conn := r.dial(t, testToken, false)
	send(t, conn, map[string]any{"v": 1, "type": "post", "requestId": "req_1", "text": "too early"})
	if got := waitClosed(t, conn); got != string(wire.ReasonHelloRequired) {
		t.Fatalf("closed with %q, want %q", got, wire.ReasonHelloRequired)
	}
}

// 🔴 Two plugins answering one agent's briefs would post twice per turn, and
// the platform would attribute both to the same agent.
func TestASecondPluginIsRefused(t *testing.T) {
	r := newRig(t)
	first := r.dial(t, testToken, true)
	receive(t, first) // the companion's hello

	second := r.dial(t, testToken, false)
	if got := waitClosed(t, second); got != string(wire.ReasonAlreadyAttached) {
		t.Fatalf("the second plugin closed with %q, want %q", got, wire.ReasonAlreadyAttached)
	}
}

func TestAMalformedFrameIsRefused(t *testing.T) {
	r := newRig(t)
	conn := r.dial(t, testToken, true)
	receive(t, conn)
	if err := conn.Send(context.Background(), []byte("this is not a frame")); err != nil {
		t.Fatalf("send: %v", err)
	}
	if got := waitClosed(t, conn); got != string(wire.ReasonMalformedFrame) {
		t.Fatalf("closed with %q, want %q", got, wire.ReasonMalformedFrame)
	}
}

func TestAFutureProtocolVersionIsRefused(t *testing.T) {
	r := newRig(t)
	conn := r.dial(t, testToken, true)
	receive(t, conn)
	send(t, conn, map[string]any{"v": 2, "type": "post", "requestId": "req_1", "text": "hi"})
	if got := waitClosed(t, conn); got != string(wire.ReasonUnsupportedVersion) {
		t.Fatalf("closed with %q, want %q", got, wire.ReasonUnsupportedVersion)
	}
}

// A plugin sending one of the COMPANION's frames is speaking the protocol
// backwards; it is refused rather than ignored.
func TestAPluginSendingTheCompanionsFrameIsRefused(t *testing.T) {
	r := newRig(t)
	conn := r.dial(t, testToken, true)
	receive(t, conn)
	send(t, conn, map[string]any{"v": 1, "type": "cancel", "requestId": "req_1", "reason": "stopped"})
	if got := waitClosed(t, conn); got != string(wire.ReasonMalformedFrame) {
		t.Fatalf("closed with %q, want %q", got, wire.ReasonMalformedFrame)
	}
}

func TestASecondHelloIsRefused(t *testing.T) {
	r := newRig(t)
	conn := r.dial(t, testToken, true)
	receive(t, conn)
	send(t, conn, map[string]any{
		"v": 1, "type": "hello", "harness": "hermes", "harnessVersion": "0.21.3",
		"pluginVersion": "0.1.0", "token": testToken, "contextBudgetTokens": 48000,
	})
	if got := waitClosed(t, conn); got != string(wire.ReasonHelloRequired) {
		t.Fatalf("closed with %q, want %q", got, wire.ReasonHelloRequired)
	}
}

func TestAPostForAnUnknownRequestIsRefused(t *testing.T) {
	r := newRig(t)
	conn := r.dial(t, testToken, true)
	receive(t, conn)
	send(t, conn, map[string]any{"v": 1, "type": "post", "requestId": "req_absent", "text": "hello?"})
	if got := waitClosed(t, conn); got != string(wire.ReasonUnknownRequest) {
		t.Fatalf("closed with %q, want %q", got, wire.ReasonUnknownRequest)
	}
}

// The whole turn, over the real protocol: ring in, brief out, ack, opener,
// post, done — and the offset committed at the end.
func TestAWholeTurnTravelsTheWire(t *testing.T) {
	r := newRig(t)
	conn := r.attach(t, testToken)
	receive(t, conn) // companion hello

	ctx := context.Background()
	claim, err := r.host.OpenDoorbell(ctx, contracts.Handlers{
		OnRing:       func(ring contracts.Ring) { _ = r.manager.OnRing(ctx, ring) },
		OnCancel:     func(id string, reason contracts.CancelReason) { _ = r.manager.OnPlatformCancel(ctx, id, reason) },
		OnDisconnect: func(string) {},
	})
	if err != nil {
		t.Fatalf("OpenDoorbell: %v", err)
	}
	t.Cleanup(func() { _ = claim.Close() })

	if err := r.host.DeliverRing(contracts.Ring{
		RingID: "ring_1", RequestID: "req_1", ConversationID: "conv_1", Reason: "mentioned",
		Cause: contracts.Cause{EntryID: "e2", AuthorID: "m1"}, SinceCursor: "c0001", ThroughCursor: "c0002",
	}); err != nil {
		t.Fatalf("DeliverRing: %v", err)
	}

	brief := receive(t, conn)
	if brief["type"] != "brief" || brief["requestId"] != "req_1" {
		t.Fatalf("expected a brief for req_1, got %v", brief)
	}
	delta, ok := brief["delta"].([]any)
	if !ok || len(delta) != 1 {
		t.Fatalf("the brief carried %v, want one delta entry", brief["delta"])
	}
	// 🔴 The brief the plugin receives must itself be a valid frame: the
	// companion is held to the contract it enforces.
	raw, _ := json.Marshal(brief)
	if err := wire.Validate(raw); err != nil {
		t.Fatalf("the companion sent a brief that fails its own schema: %v", err)
	}

	send(t, conn, map[string]any{"v": 1, "type": "ack", "requestId": "req_1", "outcome": "accepted", "session": "fresh"})
	opener := receive(t, conn)
	if opener["type"] != "opener" {
		t.Fatalf("after a fresh ack the companion sent a %v, want an opener", opener["type"])
	}
	rawOpener, _ := json.Marshal(opener)
	if err := wire.Validate(rawOpener); err != nil {
		t.Fatalf("the companion sent an opener that fails its own schema: %v", err)
	}

	send(t, conn, map[string]any{"v": 1, "type": "post", "requestId": "req_1", "text": "the invoice is filed"})
	send(t, conn, map[string]any{"v": 1, "type": "done", "requestId": "req_1", "outcome": "answered"})

	deadline := time.Now().Add(2 * time.Second)
	for {
		offset, err := r.host.OffsetOf("conv_1")
		if err != nil {
			t.Fatalf("OffsetOf: %v", err)
		}
		if offset == "c0002" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the offset is %q after an answered turn, want \"c0002\"", offset)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestNewRefusesAnIncompleteSurface(t *testing.T) {
	listener := memtransport.NewListener("memory://x")
	manager := requests.New(memory.New(), requests.Options{})
	if _, err := New(nil, manager, Options{Token: testToken}); err == nil {
		t.Fatal("a surface was built with no listener")
	}
	if _, err := New(listener, nil, Options{Token: testToken}); err == nil {
		t.Fatal("a surface was built with no request manager")
	}
	// 🔴 An empty token must be refused, not treated as "no token needed": a
	// companion serving an unauthenticated wire looks exactly like a working one.
	if _, err := New(listener, manager, Options{}); err == nil {
		t.Fatal("a surface was built with no token")
	}
}

// 🔴 The stop path, over the real wire. `/stop` is one of the oversight
// controls the platform depends on (Art. 14), and the plugin only learns about
// it through this frame — so the frame must arrive, and must itself validate.
func TestAPlatformCancelReachesThePluginAsAValidFrame(t *testing.T) {
	r := newRig(t)
	conn := r.attach(t, testToken)
	receive(t, conn) // companion hello

	ctx := context.Background()
	claim, err := r.host.OpenDoorbell(ctx, contracts.Handlers{
		OnRing:       func(ring contracts.Ring) { _ = r.manager.OnRing(ctx, ring) },
		OnCancel:     func(id string, reason contracts.CancelReason) { _ = r.manager.OnPlatformCancel(ctx, id, reason) },
		OnDisconnect: func(string) {},
	})
	if err != nil {
		t.Fatalf("OpenDoorbell: %v", err)
	}
	t.Cleanup(func() { _ = claim.Close() })

	if err := r.host.DeliverRing(contracts.Ring{
		RingID: "ring_1", RequestID: "req_1", ConversationID: "conv_1", Reason: "mentioned",
		Cause: contracts.Cause{EntryID: "e2", AuthorID: "m1"}, SinceCursor: "c0001", ThroughCursor: "c0002",
	}); err != nil {
		t.Fatalf("DeliverRing: %v", err)
	}
	if brief := receive(t, conn); brief["type"] != "brief" {
		t.Fatalf("expected a brief, got %v", brief["type"])
	}

	if err := r.host.DeliverCancel("req_1", contracts.CancelStop); err != nil {
		t.Fatalf("DeliverCancel: %v", err)
	}
	cancel := receive(t, conn)
	if cancel["type"] != "cancel" {
		t.Fatalf("expected a cancel, got %v", cancel["type"])
	}
	if cancel["reason"] != "stopped" {
		t.Fatalf("the cancel's reason is %v, want %q — the local wire says stopped", cancel["reason"], "stopped")
	}
	raw, _ := json.Marshal(cancel)
	if err := wire.Validate(raw); err != nil {
		t.Fatalf("the companion sent a cancel that fails its own schema: %v", err)
	}

	// The offset must not have moved: a stopped turn answered nothing.
	if offset, _ := r.host.OffsetOf("conv_1"); offset != "c0001" {
		t.Fatalf("a stopped request moved the offset to %q", offset)
	}
}

// A close reason formats into something a reader can act on.
func TestCloseReasonsAndClassificationReadWell(t *testing.T) {
	reason := &closeReason{reason: wire.ReasonUnauthorized, detail: "the token did not match"}
	if got := reason.Error(); !strings.Contains(got, "unauthorized") || !strings.Contains(got, "token") {
		t.Fatalf("a close reason reads %q", got)
	}
	// 🔴 classify never returns a raw error string: a wrapped host error can
	// carry a URL or a payload, and this value goes into a log line.
	for err, want := range map[error]string{
		nil:                                    "",
		requests.ErrUnknownRequest:             "unknown_request",
		requests.ErrWrongConnection:            "wrong_connection",
		context.Canceled:                       "context_ended",
		errors.New("a secret-bearing failure"): "internal",
	} {
		if got := classify(err); got != want {
			t.Errorf("classify(%v) = %q, want %q", err, got, want)
		}
	}
}

// A manager error that is neither an unknown request nor a wrong connection
// closes the connection rather than leaving the plugin waiting on a turn that
// will never proceed.
func TestAnUnexpectedManagerErrorStillClosesTheConnection(t *testing.T) {
	r := newRig(t)
	reason, detail := r.surface.toCloseReason(errors.New("something the surface did not anticipate"))
	if reason != wire.ReasonMalformedFrame {
		t.Fatalf("an unexpected error closed with %q, want %q", reason, wire.ReasonMalformedFrame)
	}
	if detail == "" {
		t.Fatal("the close carried no detail at all")
	}
	// 🔴 And it must not carry the error's text: a wrapped host error can quote
	// a URL or a payload, and this string reaches a log line.
	if strings.Contains(detail, "something the surface did not anticipate") {
		t.Fatalf("the close detail quotes the underlying error: %q", detail)
	}
	if got, _ := r.surface.toCloseReason(nil); got != "" {
		t.Fatalf("a nil error produced close reason %q", got)
	}
}

// 🔴 THE REASON THE `ready` FRAME EXISTS (#2014).
//
// A plugin attaches while its harness is still starting — Hermes connects its
// platform adapters before the gateway reaches `running`, and an inbound
// message arriving before its message handler is installed is discarded
// without a trace. A companion that delivered on `hello` therefore lost the
// first turn of every run: it closed `empty` while the real reply arrived
// seconds later, for a request that was already gone.
//
// So a ring that arrives after `hello` but before `ready` must be HELD, and
// delivered the moment `ready` lands.
func TestARingIsHeldUntilTheHarnessSaysItIsReady(t *testing.T) {
	r := newRig(t)
	conn := r.dial(t, testToken, true)
	receive(t, conn) // the companion's hello

	if err := r.manager.OnRing(context.Background(), contracts.Ring{
		RequestID: "req_1", ConversationID: "conv_1", RingID: "ring_1",
		Reason: "mentioned", ThroughCursor: "c0002",
	}); err != nil {
		t.Fatalf("OnRing: %v", err)
	}

	// Connected, but not ready: nothing may be delivered.
	if r.manager.Attached() {
		t.Fatal("the manager counts a connected plugin as attached; a brief would go out now")
	}
	if frame := receiveWithin(t, conn, 150*time.Millisecond); frame != nil {
		t.Fatalf("a %v frame was delivered before the harness said it was ready", frame["type"])
	}

	// Ready: the held ring goes out.
	send(t, conn, map[string]any{"v": 1, "type": "ready", "harnessVersion": "0.21.3"})
	brief := receive(t, conn)
	if brief["type"] != "brief" {
		t.Fatalf("after ready the companion sent a %v, want the held brief", brief["type"])
	}
	if brief["requestId"] != "req_1" {
		t.Fatalf("the brief is for %v, want the ring that was held", brief["requestId"])
	}
}

// A second `ready` is the plugin telling the truth twice, not an error.
func TestASecondReadyIsIgnoredRatherThanRefused(t *testing.T) {
	r := newRig(t)
	conn := r.attach(t, testToken)
	receive(t, conn) // the companion's hello

	send(t, conn, map[string]any{"v": 1, "type": "ready"})
	if frame := receiveWithin(t, conn, 150*time.Millisecond); frame != nil {
		t.Fatalf("a second ready produced a %v; it must be ignored", frame["type"])
	}
	if !r.manager.Attached() {
		t.Fatal("a second ready detached the plugin")
	}
}

// A plugin's terminal frame that crossed a cancel must NOT cost the connection
// (#2023). The wire is asynchronous: by the time a cancel arrives, the `done`
// for that request is already written, and closing over it takes down every
// other request the plugin is working on.
func TestATerminalFrameThatCrossedACancelKeepsTheConnection(t *testing.T) {
	r := newRig(t)
	conn := r.attach(t, testToken)
	receive(t, conn) // companion hello

	ctx := context.Background()
	claim, err := r.host.OpenDoorbell(ctx, contracts.Handlers{
		OnRing:       func(ring contracts.Ring) { _ = r.manager.OnRing(ctx, ring) },
		OnCancel:     func(id string, reason contracts.CancelReason) { _ = r.manager.OnPlatformCancel(ctx, id, reason) },
		OnDisconnect: func(string) {},
	})
	if err != nil {
		t.Fatalf("OpenDoorbell: %v", err)
	}
	t.Cleanup(func() { _ = claim.Close() })

	if err := r.host.DeliverRing(contracts.Ring{
		RingID: "ring_1", RequestID: "req_1", ConversationID: "conv_1", Reason: "mentioned",
		Cause: contracts.Cause{EntryID: "e2", AuthorID: "m1"}, SinceCursor: "c0001", ThroughCursor: "c0002",
	}); err != nil {
		t.Fatalf("DeliverRing: %v", err)
	}
	if brief := receive(t, conn); brief["type"] != "brief" {
		t.Fatalf("expected a brief, got %v", brief["type"])
	}
	send(t, conn, map[string]any{
		"v": 1, "type": "ack", "requestId": "req_1", "outcome": "accepted", "session": "resumed",
	})

	if err := r.host.DeliverCancel("req_1", contracts.CancelStop); err != nil {
		t.Fatalf("DeliverCancel: %v", err)
	}
	if cancel := receive(t, conn); cancel["type"] != "cancel" {
		t.Fatalf("expected a cancel, got %v", cancel["type"])
	}

	// 🔴 A second brief FIRST, so that what follows is ordered by the WIRE.
	// "Send the late done, then deliver a ring, then read a brief" proves
	// nothing: the ring is delivered from the test's own goroutine and the brief
	// is written before the surface has necessarily read the done, so the read
	// succeeds against a companion that closes the connection a moment later.
	// Frames on one connection are read in order by one pump goroutine, so the
	// ordering that proves anything is plugin-side: the late done, and behind it
	// a frame whose ANSWER can only arrive on a connection that is still open.
	if err := r.host.DeliverRing(contracts.Ring{
		RingID: "ring_2", RequestID: "req_2", ConversationID: "conv_1", Reason: "mentioned",
		Cause: contracts.Cause{EntryID: "e2", AuthorID: "m1"}, SinceCursor: "c0001", ThroughCursor: "c0002",
	}); err != nil {
		t.Fatalf("DeliverRing: %v", err)
	}
	if brief := receive(t, conn); brief["requestId"] != "req_2" {
		t.Fatalf("the second brief is for %v, want req_2", brief["requestId"])
	}

	// The frame the plugin had already written when the cancel arrived...
	send(t, conn, map[string]any{
		"v": 1, "type": "done", "requestId": "req_1", "outcome": "answered",
	})
	// ...and, behind it on the same connection, a frame the companion must
	// answer. A fresh ack earns an opener; a closed wire earns nothing.
	send(t, conn, map[string]any{
		"v": 1, "type": "ack", "requestId": "req_2", "outcome": "accepted", "session": "fresh",
	})

	answer := receive(t, conn)
	if answer["type"] != "opener" {
		t.Fatalf("after a crossed done the companion answered %v, want an opener — the wire was closed",
			answer["type"])
	}
	if answer["requestId"] != "req_2" {
		t.Fatalf("the opener is for %v, want req_2", answer["requestId"])
	}
}

// attachAs says hello with a harness process id, then ready (#2234).
func (r *rig) attachAs(t *testing.T, processID string) *memtransport.Conn {
	t.Helper()
	conn, err := r.listener.Dial()
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	send(t, conn, map[string]any{
		"v": 1, "type": "hello", "harness": "hermes", "harnessVersion": "0.21.3",
		"pluginVersion": "0.1.0", "token": testToken, "contextBudgetTokens": 48000,
		"processId": processID,
	})
	receive(t, conn) // the companion's hello
	send(t, conn, map[string]any{"v": 1, "type": "ready", "harnessVersion": "0.21.3"})
	return conn
}

// 🔴 #2234 through the real surface: the hello's processId reaches the manager, so
// a harness that RESTARTED is handed the request its predecessor acknowledged,
// and a reconnect of the same process is not.
func TestAHarnessRestartIsHandedTheAcknowledgedRequestAndAReconnectIsNot(t *testing.T) {
	for _, tc := range []struct {
		name       string
		after      string
		redelivers bool
	}{
		{"a restarted process", "proc-after-restart-0002", true},
		{"a reconnect of the same process", "proc-before-restart-0001", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			first := r.attachAs(t, "proc-before-restart-0001")
			if err := r.manager.OnRing(context.Background(), contracts.Ring{
				RequestID: "req_1", ConversationID: "conv_1", RingID: "ring_1",
				Reason: "mentioned", ThroughCursor: "c0002",
			}); err != nil {
				t.Fatalf("OnRing: %v", err)
			}
			if brief := receive(t, first); brief["requestId"] != "req_1" {
				t.Fatalf("expected the brief for req_1, got %v", brief)
			}
			send(t, first, map[string]any{
				"v": 1, "type": "ack", "requestId": "req_1", "outcome": "accepted", "session": "resumed",
			})
			// Hermes goes away mid-turn.
			_ = first.Close("gone")
			deadline := time.Now().Add(2 * time.Second)
			for r.manager.Attached() {
				if time.Now().After(deadline) {
					t.Fatal("the surface never noticed the plugin left")
				}
				time.Sleep(5 * time.Millisecond)
			}

			second := r.attachAs(t, tc.after)
			frame := receiveWithin(t, second, 300*time.Millisecond)
			switch {
			case tc.redelivers && (frame == nil || frame["requestId"] != "req_1"):
				t.Fatalf("the restarted harness was handed %v; its predecessor's request sat unanswered", frame)
			case !tc.redelivers && frame != nil:
				t.Fatalf("a reconnect of the same process was handed a %v it was already answering", frame["type"])
			}
		})
	}
}

// 🔴 #2232: the companion says it can relay a question, and a post carrying one
// reaches the platform WITH it — the plugin asks through the card only when told.
// 🔴 #2273: and it says it redelivers to a restarted harness, which is what lets the
// plugin decline its harness's own resume of the same turn.
func TestAQuestionIsAdvertisedAndRelayed(t *testing.T) {
	r := newRig(t)
	conn := r.dial(t, testToken, true)
	hello := receive(t, conn)
	features, _ := hello["features"].([]any)
	if len(features) != 2 || features[0] != "question" || features[1] != "redelivery" {
		t.Fatalf("the companion's hello lists %v, want [question redelivery] — this rig serves no tool ops", hello["features"])
	}
	raw, _ := json.Marshal(hello)
	if err := wire.Validate(raw); err != nil {
		t.Fatalf("the companion's hello fails its own schema: %v", err)
	}
	send(t, conn, map[string]any{"v": 1, "type": "ready", "harnessVersion": "0.21.3"})
	if err := r.manager.OnRing(context.Background(), contracts.Ring{
		RequestID: "req_1", ConversationID: "conv_1", RingID: "ring_1",
		Reason: "mentioned", ThroughCursor: "c0002",
	}); err != nil {
		t.Fatalf("OnRing: %v", err)
	}
	receive(t, conn) // the brief
	send(t, conn, map[string]any{"v": 1, "type": "ack", "requestId": "req_1", "outcome": "accepted", "session": "resumed"})
	send(t, conn, map[string]any{
		"v": 1, "type": "post", "requestId": "req_1", "text": "Which pump?",
		"question": map[string]any{"id": "clarify_1", "choices": []string{"North", "South"}, "multiSelect": false},
	})
	deadline := time.Now().Add(2 * time.Second)
	for len(r.host.Questions()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the question never reached the host: the post went out as plain text")
		}
		time.Sleep(5 * time.Millisecond)
	}
	got := r.host.Questions()[0]
	if got.ID != "clarify_1" || len(got.Choices) != 2 || got.Choices[1] != "South" || got.MultiSelect {
		t.Fatalf("the host was handed %+v", got)
	}
}

// 🔴 #2349: `tools` is advertised exactly when BOTH tool ops are served — a
// plugin told so offers the tools to its model, and a call answered unknown_op
// would fail a turn that need not have.
func TestToolsAreAdvertisedOnlyWhenBothOpsAreServed(t *testing.T) {
	serve := func(context.Context, json.RawMessage) (json.RawMessage, error) { return json.RawMessage("{}"), nil }
	for name, tc := range map[string]struct {
		calls map[string]CallHandler
		want  bool
	}{
		"both":      {map[string]CallHandler{wire.OpToolsList: serve, wire.OpToolsCall: serve}, true},
		"list only": {map[string]CallHandler{wire.OpToolsList: serve}, false},
		"call only": {map[string]CallHandler{wire.OpToolsCall: serve}, false},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRigWith(t, Options{Calls: tc.calls})
			conn := r.dial(t, testToken, true)
			hello := receive(t, conn)
			features, _ := hello["features"].([]any)
			got := false
			for _, feature := range features {
				got = got || feature == "tools"
			}
			if got != tc.want {
				t.Fatalf("features %v: tools advertised = %v, want %v", features, got, tc.want)
			}
			raw, _ := json.Marshal(hello)
			if err := wire.Validate(raw); err != nil {
				t.Fatalf("the hello fails its own schema: %v", err)
			}
		})
	}
}

// 🔴 #2349: a post's citations and activity reach the host with it.
func TestAPostsCitationsAndActivityReachTheHost(t *testing.T) {
	r := newRig(t)
	conn := r.dial(t, testToken, true)
	receive(t, conn) // hello
	send(t, conn, map[string]any{"v": 1, "type": "ready"})
	if err := r.manager.OnRing(context.Background(), contracts.Ring{
		RequestID: "req_1", ConversationID: "conv_1", RingID: "ring_1",
		Reason: "mentioned", ThroughCursor: "c0002",
	}); err != nil {
		t.Fatalf("OnRing: %v", err)
	}
	receive(t, conn) // the brief
	send(t, conn, map[string]any{"v": 1, "type": "ack", "requestId": "req_1", "outcome": "accepted", "session": "resumed"})
	send(t, conn, map[string]any{
		"v": 1, "type": "post", "requestId": "req_1", "text": "There is one board, Support.",
		"citations": []string{"urn:uffici:tool_call:42"},
		"activity":  []map[string]string{{"tool": "web_extract", "summary": "Read the status page."}},
	})
	deadline := time.Now().Add(2 * time.Second)
	for len(r.host.PostExtras()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the post's citations and activity never reached the host")
		}
		time.Sleep(5 * time.Millisecond)
	}
	extras := r.host.PostExtras()[0]
	if len(extras.Citations) != 1 || extras.Citations[0] != "urn:uffici:tool_call:42" ||
		len(extras.Activity) != 1 || extras.Activity[0] != (contracts.Activity{Tool: "web_extract", Summary: "Read the status page."}) {
		t.Fatalf("the host received %+v", extras)
	}
}
