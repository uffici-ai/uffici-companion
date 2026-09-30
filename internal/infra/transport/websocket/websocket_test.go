package websocket

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ws "github.com/coder/websocket"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
)

// These tests exercise exactly what the in-memory transport cannot stand in
// for: file permissions, the HTTP upgrade and the Origin refusal. Everything
// else about the protocol is tested over the pipe, on purpose.

func TestItSatisfiesTheContracts(_ *testing.T) {
	var _ contracts.Listener = (*Listener)(nil)
	var _ contracts.Conn = (*Conn)(nil)
}

// shortTempDir gives a directory well inside the kernel's sun_path limit.
//
// 🔴 `t.TempDir()` embeds the TEST NAME, and a descriptive name is enough to
// push a Unix socket path past 104 bytes on macOS — which surfaces as `bind:
// invalid argument` and reads like a permissions problem.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "uw")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func socketListener(t *testing.T) (*Listener, string) {
	t.Helper()
	path := filepath.Join(shortTempDir(t), "wire.sock")
	listener, err := Listen(Options{Kind: contracts.TransportSocket, SocketPath: path})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener, path
}

// 🔴 Custody of the socket file IS the authorization boundary: the token stops
// a different local PROCESS, the mode stops a different local USER. A mode that
// let the group in would leave the token as the only barrier, which it was
// never meant to be.
func TestTheSocketIsOwnerOnly(t *testing.T) {
	_, path := socketListener(t)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != SocketMode {
		t.Fatalf("the socket's mode is %04o, want %04o — anything wider makes the token the only barrier", got, SocketMode)
	}
}

// A socket left by a previous run makes Listen fail with "address in use" on a
// daemon that is not running — a confusing start-up failure with a healthy box.
func TestAStaleSocketIsReplaced(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "wire.sock")
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("creating the stale socket: %v", err)
	}
	_ = stale.Close() // leaves the file behind

	listener, err := Listen(Options{Kind: contracts.TransportSocket, SocketPath: path})
	if err != nil {
		t.Fatalf("Listen over a stale socket: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
}

func TestTheSocketIsRemovedOnClose(t *testing.T) {
	listener, path := socketListener(t)
	if err := listener.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the socket file survived Close — the next start would fail with 'address in use'")
	}
	// Closing twice must be safe: shutdown runs from more than one place.
	if err := listener.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// dialSocket connects a WebSocket client over the Unix socket.
func dialSocket(t *testing.T, path string, header http.Header) (*ws.Conn, *http.Response, error) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	return ws.Dial(ctx, "ws://companion"+WirePath, &ws.DialOptions{
		HTTPClient: client, HTTPHeader: header,
	})
}

func TestAFrameTravelsTheSocketUnchanged(t *testing.T) {
	listener, path := socketListener(t)

	clientConn, _, err := dialSocket(t, path, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = clientConn.Close(ws.StatusNormalClosure, "done") })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	served, err := listener.Accept(ctx)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}

	want := []byte(`{"v":1,"type":"post","requestId":"req_1","text":"hello"}`)
	if err := clientConn.Write(ctx, ws.MessageText, want); err != nil {
		t.Fatalf("client write: %v", err)
	}
	got, err := served.Receive(ctx)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("got %q, want %q — the transport must not touch the bytes", got, want)
	}

	// And back the other way.
	if err := served.Send(ctx, []byte(`{"v":1,"type":"cancel","requestId":"req_1","reason":"stopped"}`)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	kind, raw, err := clientConn.Read(ctx)
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	if kind != ws.MessageText {
		t.Fatalf("the companion sent a %v frame, want text", kind)
	}
	var frame map[string]any
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatalf("the companion's frame is not JSON: %v", err)
	}
	if frame["type"] != "cancel" {
		t.Fatalf("got a %v, want a cancel", frame["type"])
	}
}

// 🔴 An Origin header means a browser is on the other end, and a browser is
// never a plugin. This wire has no CSRF defence and needs none — because
// nothing that can set an Origin is allowed to speak it at all.
func TestAConnectionCarryingAnOriginIsRefused(t *testing.T) {
	_, path := socketListener(t)
	header := http.Header{}
	header.Set("Origin", "https://evil.example.com")

	_, response, err := dialSocket(t, path, header)
	if err == nil {
		t.Fatal("a connection carrying an Origin was upgraded")
	}
	if response == nil {
		t.Fatalf("no HTTP response to inspect: %v", err)
	}
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("the refusal answered %d, want %d", response.StatusCode, http.StatusForbidden)
	}
}

// Even a same-origin header is refused: the rule is the header's PRESENCE, not
// its value, because only a browser sends one.
func TestEvenAPlausibleOriginIsRefused(t *testing.T) {
	_, path := socketListener(t)
	header := http.Header{}
	header.Set("Origin", "http://127.0.0.1:7411")
	if _, _, err := dialSocket(t, path, header); err == nil {
		t.Fatal("a loopback Origin was accepted — the rule is the header's presence")
	}
}

func TestHealthAnswersOnTheTCPTransport(t *testing.T) {
	listener, err := Listen(Options{
		Kind: contracts.TransportTCP, TCPAddr: "127.0.0.1:0",
		Health: func() Health {
			return Health{Status: "ok", AgentsHost: "fixture", PluginAttached: true}
		},
	})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	response, err := http.Get("http://" + listener.Addr() + HealthPath) //nolint:noctx // a test probe
	if err != nil {
		t.Fatalf("GET %s: %v", HealthPath, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("/healthz answered %d", response.StatusCode)
	}
	var health Health
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatalf("decoding /healthz: %v", err)
	}
	if health.Status != "ok" || health.AgentsHost != "fixture" || !health.PluginAttached {
		t.Fatalf("/healthz answered %+v", health)
	}
}

// `uffici doctor` reads a running companion's doorbell over the same socket a
// plugin uses (#2257), and learns "nothing is running" from an absent one.
func TestReadHealthReadsARunningCompanionOverTheSocket(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "wire.sock")
	options := Options{Kind: contracts.TransportSocket, SocketPath: path}
	if _, err := ReadHealth(context.Background(), options); !errors.Is(err, ErrNoCompanion) {
		t.Fatalf("with nothing listening ReadHealth answered %v, want ErrNoCompanion", err)
	}
	listener, err := Listen(Options{
		Kind: contracts.TransportSocket, SocketPath: path,
		Health: func() Health {
			return Health{Status: "ok", AgentsHost: "gateway", Doorbell: DoorbellConnected, ConnectionID: "c-1"}
		},
	})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	health, err := ReadHealth(context.Background(), options)
	if err != nil || health.Doorbell != DoorbellConnected || health.ConnectionID != "c-1" || health.AgentsHost != "gateway" {
		t.Fatalf("ReadHealth answered %+v, %v", health, err)
	}
	// A dial error names the socket path, inside the home that holds the token.
	_, err = ReadHealth(context.Background(), Options{Kind: contracts.TransportTCP, TCPAddr: "127.0.0.1:1"})
	if !errors.Is(err, ErrNoCompanion) || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("an unanswered port gave %v — want the classified error alone", err)
	}
}

func TestTheTCPTransportDefaultsItsAddress(t *testing.T) {
	// Not bound here — only that the default is the documented loopback port,
	// never a wildcard interface.
	listener, err := Listen(Options{Kind: contracts.TransportTCP, TCPAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	host, _, err := net.SplitHostPort(listener.Addr())
	if err != nil {
		t.Fatalf("parsing the address: %v", err)
	}
	if host != "127.0.0.1" {
		t.Fatalf("listening on %q — the wire is loopback only", host)
	}
}

func TestUnknownTransportsAndMissingPathsAreRefused(t *testing.T) {
	if _, err := Listen(Options{Kind: "carrier-pigeon"}); err == nil {
		t.Fatal("an unknown transport was accepted")
	}
	if _, err := Listen(Options{Kind: contracts.TransportSocket}); err == nil {
		t.Fatal("the socket transport was started with no path")
	}
}

func TestAcceptHonoursItsContextAndClose(t *testing.T) {
	listener, _ := socketListener(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := listener.Accept(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Accept with nothing pending returned %v, want DeadlineExceeded", err)
	}

	other, _ := socketListener(t)
	_ = other.Close()
	if _, err := other.Accept(context.Background()); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Accept on a closed listener returned %v, want net.ErrClosed", err)
	}
}

// 🔴 The close reason is the only thing a refused plugin learns. The WebSocket
// protocol caps it at 123 bytes, so it is truncated rather than dropped — a
// close with no reason at all is the one that costs somebody an afternoon.
func TestTheCloseReasonTravelsAndIsTruncatedNotDropped(t *testing.T) {
	listener, path := socketListener(t)
	clientConn, _, err := dialSocket(t, path, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	served, err := listener.Accept(ctx)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}

	long := "unauthorized"
	for len(long) < 200 {
		long += "-and-more-detail"
	}
	// The client must be READING for the close handshake to complete; a real
	// plugin always is, and this is what lets the reason arrive.
	readErr := make(chan error, 1)
	go func() {
		_, _, err := clientConn.Read(ctx)
		readErr <- err
	}()
	if err := served.Close(long); err != nil {
		t.Fatalf("Close: %v", err)
	}
	err = <-readErr
	if err == nil {
		t.Fatal("the client kept reading after the close")
	}
	var closeErr ws.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("the client saw %v, not a WebSocket close", err)
	}
	if closeErr.Reason == "" {
		t.Fatal("the close carried no reason at all")
	}
	if len(closeErr.Reason) > 123 {
		t.Fatalf("the close reason is %d bytes; the protocol caps it at 123", len(closeErr.Reason))
	}
}

func TestRemoteHintNamesThePlaceNotThePeer(t *testing.T) {
	listener, path := socketListener(t)
	clientConn, _, err := dialSocket(t, path, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = clientConn.Close(ws.StatusNormalClosure, "done") })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	served, err := listener.Accept(ctx)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if served.RemoteHint() != path {
		t.Fatalf("RemoteHint is %q, want the socket path", served.RemoteHint())
	}
}

// 🔴 A path over the kernel's limit must fail with a sentence, not with `bind:
// invalid argument`. The opaque form reads like a permissions problem and sends
// the reader looking in entirely the wrong place.
func TestAnOverlongSocketPathFailsWithAnExplanation(t *testing.T) {
	long := filepath.Join(shortTempDir(t), strings.Repeat("deep/", 30), "wire.sock")
	_, err := Listen(Options{Kind: contracts.TransportSocket, SocketPath: long})
	if err == nil {
		t.Fatal("an overlong socket path was accepted")
	}
	for _, phrase := range []string{"kernel accepts at most", "invalid argument", "UFFICI_HOME"} {
		if !strings.Contains(err.Error(), phrase) {
			t.Errorf("the error never mentions %q — a reader would chase permissions instead:\n%s", phrase, err.Error())
		}
	}
}

// 🔴 A plugin that never answers the close handshake must not hold a companion
// goroutine. Without the bound, refusing such a plugin costs one wedged
// goroutine per refusal — a slow leak in OUR process caused by THEIR bug.
func TestClosingAnUnresponsivePluginDoesNotHang(t *testing.T) {
	listener, path := socketListener(t)
	clientConn, _, err := dialSocket(t, path, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = clientConn.CloseNow() })

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	served, err := listener.Accept(ctx)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}

	// The client never reads, so it never answers the close frame.
	done := make(chan struct{})
	go func() {
		_ = served.Close("unauthorized")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close blocked on an unresponsive peer — one wedged goroutine per refused plugin")
	}
}

// 🔴 The probe's errors reach a LOG LINE: the runner turns them into unmet
// expectations and the daemon logs every one. coder/websocket's own dial error
// renders the target, which on this transport is a path inside `$UFFICI_HOME` —
// the directory that also holds the wire token. Neither the path nor the URL
// may survive the classification.
func TestTheProbesErrorsCarryNoPathAndNoURL(t *testing.T) {
	// A socket that does not exist, inside a directory whose name would be
	// unmistakable in a log line.
	home := filepath.Join(shortTempDir(t), "wire-token-lives-here")
	socket := filepath.Join(home, "wire.sock")

	reason, err := ProbeClose(context.Background(), Options{
		Kind: contracts.TransportSocket, SocketPath: socket,
	})
	if err == nil {
		t.Fatalf("dialling a socket that does not exist returned reason %q and no error", reason)
	}
	if !errors.Is(err, ErrProbeDial) {
		t.Fatalf("the probe returned %v, want ErrProbeDial", err)
	}
	for _, leaked := range []string{home, socket, "wire-token-lives-here", "ws://", "unix"} {
		if strings.Contains(err.Error(), leaked) {
			t.Fatalf("the probe's error contains %q: %v", leaked, err)
		}
	}
}

// 🔴 A companion that stops refusing a second plugin — the exact defect the
// `secondConnection` step exists to catch — sends this probe nothing at all. The
// bounded read is what turns that into an ANSWER the step can report, instead of
// a daemon that hangs until CI's outer timeout with nothing to read.
func TestAProbeAgainstASilentServerAnswersRatherThanHanging(t *testing.T) {
	// A listener that accepts the upgrade and then says nothing — a surface with
	// its single-plugin refusal removed.
	socket := filepath.Join(shortTempDir(t), "s.sock")
	netListener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = netListener.Close() })

	held := make(chan *ws.Conn, 1)
	server := &http.Server{
		ReadHeaderTimeout: time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, acceptErr := ws.Accept(w, r, &ws.AcceptOptions{InsecureSkipVerify: true})
			if acceptErr != nil {
				return
			}
			held <- conn
			// Hold it open for longer than the probe's grace, saying nothing.
			time.Sleep(ProbeGrace + time.Second)
		}),
	}
	go func() { _ = server.Serve(netListener) }()
	t.Cleanup(func() { _ = server.Close() })

	started := time.Now()
	reason, err := ProbeClose(context.Background(), Options{
		Kind: contracts.TransportSocket, SocketPath: socket,
	})
	if err != nil {
		t.Fatalf("the probe errored instead of reporting an open connection: %v", err)
	}
	if reason != "" {
		t.Fatalf("the probe reported close reason %q; the server never closed anything", reason)
	}
	if elapsed := time.Since(started); elapsed > ProbeGrace+2*time.Second {
		t.Fatalf("the probe took %s; it must give up after %s", elapsed, ProbeGrace)
	}
	select {
	case conn := <-held:
		_ = conn.CloseNow()
	default:
		t.Fatal("the server never accepted the probe, so this test proved nothing about a silent one")
	}
}
