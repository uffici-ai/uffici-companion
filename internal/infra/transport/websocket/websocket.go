// Package websocket serves the local wire over a Unix socket or loopback TCP.
//
// 🔴 It is the ONLY package that knows what a WebSocket is. Everything above it
// speaks `contracts.Conn`, which is why the protocol, the state machine and
// every rule they enforce are tested over an in-process pipe rather than
// through a socket. What lives here is exactly what a pipe cannot stand in for:
// file permissions, the upgrade, and the `Origin` refusal.
package websocket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	ws "github.com/coder/websocket"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
)

// WirePath is where a plugin connects over TCP.
const WirePath = "/v1/ring"

// HealthPath answers a readiness probe on either listener (the socket answers it
// too: `uffici doctor` reads a running companion there, #2257).
const HealthPath = "/healthz"

// SocketPathMax is the longest Unix socket path the platform accepts.
//
// 🔴 The kernel's `sun_path` is a fixed-size array — 104 bytes on macOS and the
// BSDs, 108 on Linux — and `bind` answers a path over it with `invalid
// argument`, which says nothing about length. The conservative bound is checked
// here so a deep `UFFICI_HOME` fails with a sentence instead. Found the hard way
// while testing: a temp directory named after the test case was enough to
// exceed it.
const SocketPathMax = 100

// SocketMode is the Unix socket's permission: owner only.
//
// 🔴 Custody of this file IS the authorization boundary. The token stops a
// DIFFERENT local process attaching; the mode is what stops a different local
// USER reaching the socket at all. A mode that let the group in would make the
// token the only barrier, which it was never meant to be.
const SocketMode os.FileMode = 0o600

// Health is what `/healthz` answers.
type Health struct {
	Status         string `json:"status"`
	AgentsHost     string `json:"agentsHost"`
	PluginAttached bool   `json:"pluginAttached"`
	// Doorbell is the gateway doorbell's state, "connected" or "reconnecting";
	// empty when the agents host has none (the fixture). ConnectionID is the
	// platform's id for the connected doorbell. Both are what `uffici doctor`
	// reports for a running companion (#2257) — never a credential.
	Doorbell     string `json:"doorbell,omitempty"`
	ConnectionID string `json:"connectionId,omitempty"`
	// Version is the running companion's own version: what tells a doctor or
	// an upgrade that the process answering is the binary on disk, and not the
	// one it replaced (#2250).
	Version string `json:"version,omitempty"`
}

// Doorbell states a Health reports.
const (
	DoorbellConnected    = "connected"
	DoorbellReconnecting = "reconnecting"
)

// HealthFunc reports the companion's health.
type HealthFunc func() Health

// Listener serves the wire and hands out accepted connections.
type Listener struct {
	server   *http.Server
	net      net.Listener
	incoming chan contracts.Conn
	addr     string
	socket   string

	closeOnce sync.Once
	closed    chan struct{}
}

// Options configure a Listener.
type Options struct {
	// Kind is socket or tcp.
	Kind contracts.TransportKind
	// SocketPath is the Unix socket path, for the socket transport.
	SocketPath string
	// TCPAddr is the loopback address, for the tcp transport.
	TCPAddr string
	// Health answers `/healthz`, on the TCP port and the socket alike.
	Health HealthFunc
}

// Listen starts serving.
func Listen(options Options) (*Listener, error) {
	switch options.Kind {
	case contracts.TransportSocket, "":
		return listenSocket(options)
	case contracts.TransportTCP:
		return listenTCP(options)
	default:
		return nil, fmt.Errorf("websocket: unknown transport %q; this build serves %q and %q",
			options.Kind, contracts.TransportSocket, contracts.TransportTCP)
	}
}

func listenSocket(options Options) (*Listener, error) {
	path := options.SocketPath
	if path == "" {
		return nil, fmt.Errorf("websocket: the socket transport needs a path")
	}
	if len(path) > SocketPathMax {
		return nil, fmt.Errorf(
			"websocket: the socket path is %d bytes and the kernel accepts at most %d (`bind` would answer "+
				"\"invalid argument\", which says nothing about length): %s — set UFFICI_HOME somewhere shorter, "+
				"or pass --socket with a shorter path",
			len(path), SocketPathMax, path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("websocket: creating the socket directory: %w", err)
	}
	// A socket left by a previous run would make Listen fail with "address in
	// use" on a daemon that is not actually running.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("websocket: removing the stale socket: %w", err)
	}
	netListener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("websocket: listening on %s: %w", path, err)
	}
	// 🔴 Chmod AFTER Listen: the socket file does not exist until then, and a
	// mode applied to the directory is not a mode on the socket.
	if err := os.Chmod(path, SocketMode); err != nil {
		_ = netListener.Close()
		return nil, fmt.Errorf("websocket: setting the socket mode: %w", err)
	}
	return serve(netListener, path, path, options)
}

func listenTCP(options Options) (*Listener, error) {
	addr := options.TCPAddr
	if addr == "" {
		addr = "127.0.0.1:7411"
	}
	netListener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("websocket: listening on %s: %w", addr, err)
	}
	return serve(netListener, netListener.Addr().String(), "", options)
}

func serve(netListener net.Listener, addr, socket string, options Options) (*Listener, error) {
	listener := &Listener{
		net:      netListener,
		incoming: make(chan contracts.Conn, 4),
		addr:     addr,
		socket:   socket,
		closed:   make(chan struct{}),
	}

	mux := http.NewServeMux()
	mux.HandleFunc(HealthPath, func(w http.ResponseWriter, _ *http.Request) {
		health := Health{Status: "ok"}
		if options.Health != nil {
			health = options.Health()
		}
		writeJSON(w, health)
	})
	mux.HandleFunc(WirePath, listener.upgrade)
	// A Unix socket has no path convention, so the wire answers at the root too.
	mux.HandleFunc("/", listener.upgrade)

	listener.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = listener.server.Serve(netListener) }()
	return listener, nil
}

// upgrade turns an HTTP request into a wire connection.
func (l *Listener) upgrade(w http.ResponseWriter, r *http.Request) {
	// 🔴 A connection carrying an Origin header is REFUSED, whatever the value.
	// An Origin means a browser is on the other end, and a browser is never a
	// plugin — this wire has no CSRF defence and needs none, because nothing
	// that can set an Origin is allowed to speak it.
	if r.Header.Get("Origin") != "" {
		http.Error(w, "the local wire refuses browser origins", http.StatusForbidden)
		return
	}

	conn, err := ws.Accept(w, r, &ws.AcceptOptions{
		// The Origin check above is stricter than any pattern list, so the
		// library's own check has nothing left to do.
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	// A frame may be up to the wire's bound; the default is far smaller.
	conn.SetReadLimit(int64(wireFrameMax))

	wrapped := &Conn{conn: conn, addr: l.addr}
	select {
	case l.incoming <- wrapped:
		// The handler must not return until the connection is finished, or the
		// server tears it down underneath the surface.
		<-wrapped.done()
	case <-l.closed:
		_ = conn.Close(ws.StatusGoingAway, "shutting_down")
	}
}

// wireFrameMax mirrors the wire's own frame bound.
const wireFrameMax = 1 << 20

// Accept returns the next plugin connection.
func (l *Listener) Accept(ctx context.Context) (contracts.Conn, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.closed:
		return nil, net.ErrClosed
	case conn := <-l.incoming:
		return conn, nil
	}
}

// Addr describes where it listens.
func (l *Listener) Addr() string { return l.addr }

// Close stops serving and removes the socket file.
func (l *Listener) Close() error {
	var err error
	l.closeOnce.Do(func() {
		close(l.closed)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		err = l.server.Shutdown(ctx)
		if l.socket != "" {
			// Leaving the socket behind makes the next start fail with "address in
			// use" on a daemon that is not running.
			_ = os.Remove(l.socket)
		}
	})
	return err
}

// Conn is one plugin connection.
type Conn struct {
	conn *ws.Conn
	addr string

	closeOnce sync.Once
	finished  chan struct{}
	initOnce  sync.Once
}

func (c *Conn) done() <-chan struct{} {
	c.initOnce.Do(func() { c.finished = make(chan struct{}) })
	return c.finished
}

// Receive blocks for the next text frame.
func (c *Conn) Receive(ctx context.Context) ([]byte, error) {
	kind, raw, err := c.conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	if kind != ws.MessageText {
		// The wire is JSON text frames. A binary frame is not a frame this
		// protocol has, so it is a protocol error rather than something to guess at.
		return nil, fmt.Errorf("websocket: a binary frame arrived; the local wire is text")
	}
	return raw, nil
}

// Send writes one text frame.
func (c *Conn) Send(ctx context.Context, frame []byte) error {
	return c.conn.Write(ctx, ws.MessageText, frame)
}

// Close ends the connection, carrying the wire's close reason.
//
// 🔴 The reason travels in the WebSocket close frame, which is the only thing a
// refused plugin ever learns about why. It is truncated to the protocol's 123
// bytes rather than dropped, because a close with no reason is the one that
// costs somebody an afternoon.
func (c *Conn) Close(reason string) error {
	var err error
	c.closeOnce.Do(func() {
		if len(reason) > 123 {
			reason = reason[:123]
		}

		// 🔴 BOUNDED. A graceful WebSocket close waits for the peer's close frame,
		// and the peer here is a third party's plugin — the very plugin we are
		// refusing, which may be wedged, mid-crash or simply not reading. Left
		// unbounded this pins a companion goroutine per refusal, so a plugin that
		// ignores the handshake becomes a slow resource leak in OUR process.
		// Try the courteous close, then stop waiting and drop the connection.
		// 🔴 The graceful close runs on its own goroutine and we stop WAITING for
		// it; we do not race it with CloseNow. The library serialises both on one
		// mutex, so calling CloseNow while Close is in flight deadlocks — which is
		// worse than the hang it was meant to cure, and is how the first cut of
		// this failed its own test. The library's close has its own timeout, so
		// that goroutine is short-lived; what matters here is that OUR caller,
		// the surface, is never held by a peer that will not answer.
		done := make(chan error, 1)
		go func() { done <- c.conn.Close(ws.StatusNormalClosure, reason) }()
		select {
		case err = <-done:
		case <-time.After(closeHandshakeGrace):
			err = nil
		}

		c.initOnce.Do(func() { c.finished = make(chan struct{}) })
		close(c.finished)
	})
	return err
}

// closeHandshakeGrace is how long a refused plugin has to answer the close.
const closeHandshakeGrace = 500 * time.Millisecond

// RemoteHint is a non-identifying label for logs.
func (c *Conn) RemoteHint() string { return c.addr }

func writeJSON(w http.ResponseWriter, health Health) {
	w.Header().Set("Content-Type", "application/json")
	// A failed write to a probe's connection is the prober going away, which is
	// ordinary — but it is swallowed deliberately and visibly, not silently.
	_ = json.NewEncoder(w).Encode(health)
}

// ErrNoCompanion is ReadHealth's answer when nothing answers on the wire's
// address: no companion is running there.
var ErrNoCompanion = errors.New("websocket: no companion answers on the wire's address")

// ReadHealth asks the companion serving `options` for its health, over the
// same socket or loopback port a plugin would use (#2257).
//
// 🔴 Its errors are CLASSIFIED, like ProbeClose's: a dial error renders the
// socket path inside `$UFFICI_HOME`, the directory that holds the wire token.
func ReadHealth(ctx context.Context, options Options) (Health, error) {
	client := &http.Client{}
	target := "http://" + options.TCPAddr + HealthPath
	if options.Kind == contracts.TransportSocket || options.Kind == "" {
		socket := options.SocketPath
		transport := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			},
		}
		defer transport.CloseIdleConnections()
		client = &http.Client{Transport: transport}
		target = "http://companion" + HealthPath
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return Health{}, ErrNoCompanion
	}
	response, err := client.Do(request)
	if err != nil {
		return Health{}, ErrNoCompanion
	}
	defer func() { _ = response.Body.Close() }()
	var health Health
	if response.StatusCode != http.StatusOK ||
		json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&health) != nil || health.Status == "" {
		return Health{}, ErrNoCompanion
	}
	return health, nil
}

// ProbeClose opens a connection the way a plugin would and reports the close
// reason the companion answered with, or "" if the companion left it open.
//
// 🔴 For the fixture's `secondConnection` step, which asserts a second plugin is
// refused with `already_attached`. It sends no hello: the refusal must happen
// before a token is even read, because two plugins answering one agent's briefs
// is a problem whether or not the second one knows the token.
//
// 🔴 The read is BOUNDED, and the timeout is the ANSWER rather than an error.
// The step exists to catch the single-plugin refusal regressing — and if it
// regresses, nothing ever arrives on this connection. An unbounded read would
// then hang the daemon with no output instead of reporting the miss: the one
// defect this guard is for would present as a CI timeout with nothing to read.
//
// 🔴 Its errors are CLASSIFIED, never the transport's own. The runner turns a
// returned error into an unmet expectation, which the daemon logs — and a
// coder/websocket dial error renders the target, which on the socket transport
// is the path inside `$UFFICI_HOME`, the directory that also holds the wire
// token. `logging.md`: never a whole error object, never a URL.
func ProbeClose(ctx context.Context, options Options) (string, error) {
	dialer := &http.Client{}
	target := "ws://" + options.TCPAddr + WirePath
	if options.Kind == contracts.TransportSocket || options.Kind == "" {
		socket := options.SocketPath
		transport := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			},
		}
		// One probe per step, but a daemon left running is a daemon that probes
		// again; an idle connection per probe is a leak however slow.
		defer transport.CloseIdleConnections()
		dialer = &http.Client{Transport: transport}
		target = "ws://companion" + WirePath
	}

	conn, _, err := ws.Dial(ctx, target, &ws.DialOptions{HTTPClient: dialer})
	if err != nil {
		// An HTTP-level refusal (an Origin, say) never becomes a WebSocket, so
		// there is no close frame to read — and the classified error is all the
		// caller may be told.
		return "", ErrProbeDial
	}
	defer func() { _ = conn.CloseNow() }()

	readCtx, stopReading := context.WithTimeout(ctx, ProbeGrace)
	defer stopReading()
	if _, _, err := conn.Read(readCtx); err != nil {
		var closeErr ws.CloseError
		if errors.As(err, &closeErr) {
			return closeErr.Reason, nil
		}
		if errors.Is(readCtx.Err(), context.DeadlineExceeded) {
			// The companion neither closed nor spoke: it left the connection open,
			// which IS the answer the caller asked for.
			return "", nil
		}
		return "", ErrProbeRead
	}
	// A frame instead of a close: also "left open", and the caller's assertion
	// on the reason is what fails.
	return "", nil
}

// ProbeGrace bounds ProbeClose's read.
const ProbeGrace = 5 * time.Second

// The probe's classified errors. 🔴 A transport error is never handed back: see
// ProbeClose's note on where the caller's error strings end up.
var (
	// ErrProbeDial — the probe could not open a connection at all.
	ErrProbeDial = errors.New("the probe could not reach the companion's wire")
	// ErrProbeRead — the connection opened but ended without a close reason.
	ErrProbeRead = errors.New("the probe's connection ended without a close reason")
)
