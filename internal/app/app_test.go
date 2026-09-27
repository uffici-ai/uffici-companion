package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	ws "github.com/coder/websocket"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/registry"
	"github.com/uffici-ai/uffici-companion/internal/runner"
)

// shortHome keeps the socket path inside the kernel's sun_path limit; t.TempDir
// embeds the test name and is enough to exceed it on macOS.
func shortHome(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "uh")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// ── The token ───────────────────────────────────────────────────────────────

func TestATokenIsMintedOnFirstStartAndReused(t *testing.T) {
	home := shortHome(t)
	first, err := EnsureToken(home, "")
	if err != nil {
		t.Fatalf("EnsureToken: %v", err)
	}
	if len(first) < 32 {
		t.Fatalf("the minted token is %d characters; the wire requires at least 32", len(first))
	}
	second, err := EnsureToken(home, "")
	if err != nil {
		t.Fatalf("EnsureToken again: %v", err)
	}
	if second != first {
		t.Fatal("a second start minted a NEW token — every attached plugin would be locked out on restart")
	}

	info, err := os.Stat(filepath.Join(home, TokenFile))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != TokenMode {
		t.Fatalf("the token file is mode %04o, want %04o", got, TokenMode)
	}
}

// 🔴 A readable token file is refused, not repaired. Quietly fixing the mode
// would hide the window in which it was readable — and that window is the whole
// question.
func TestAWorldReadableTokenIsRefusedRatherThanRepaired(t *testing.T) {
	home := shortHome(t)
	if _, err := EnsureToken(home, ""); err != nil {
		t.Fatalf("EnsureToken: %v", err)
	}
	path := filepath.Join(home, TokenFile)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	_, err := EnsureToken(home, "")
	if err == nil {
		t.Fatal("a world-readable token was accepted")
	}
	for _, phrase := range []string{"lets other users read it", "chmod 600"} {
		if !strings.Contains(err.Error(), phrase) {
			t.Errorf("the refusal never mentions %q:\n%s", phrase, err.Error())
		}
	}
	if info, statErr := os.Stat(path); statErr == nil && info.Mode().Perm() != 0o644 {
		t.Fatal("the mode was repaired — the operator must decide, and must know it happened")
	}
}

func TestATruncatedTokenIsRefused(t *testing.T) {
	home := shortHome(t)
	if err := os.WriteFile(filepath.Join(home, TokenFile), []byte("short\n"), TokenMode); err != nil {
		t.Fatalf("writing: %v", err)
	}
	if _, err := EnsureToken(home, ""); err == nil {
		t.Fatal("a 5-character token was accepted where the wire requires 32")
	}
}

// ── Configuration ───────────────────────────────────────────────────────────

func env(pairs map[string]string) Env {
	return func(key string) string { return pairs[key] }
}

func TestLoadConfigDefaults(t *testing.T) {
	home := shortHome(t)
	config, err := LoadConfig(env(map[string]string{"UFFICI_HOME": home}), Config{})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if config.Transport != contracts.TransportSocket {
		t.Fatalf("transport defaults to %q, want the socket", config.Transport)
	}
	if config.SocketPath != filepath.Join(home, SocketFile) {
		t.Fatalf("socket path is %q", config.SocketPath)
	}
	if config.AgentsHost != registry.KindFixture {
		t.Fatalf("agents host defaults to %q, want the fixture", config.AgentsHost)
	}
	// 🔴 Loopback, never a wildcard: the wire's only authentication is a local
	// token, so a wildcard bind would expose it on every interface of a pod.
	if !strings.HasPrefix(config.TCPAddr, "127.0.0.1:") {
		t.Fatalf("the TCP address is %q, which is not loopback", config.TCPAddr)
	}
}

func TestLoadConfigRefusesNonsense(t *testing.T) {
	home := shortHome(t)
	for name, environment := range map[string]map[string]string{
		"an unknown transport":      {"UFFICI_HOME": home, "UFFICI_WIRE_TRANSPORT": "carrier-pigeon"},
		"a port that is not a port": {"UFFICI_HOME": home, "UFFICI_COMPANION_PORT": "seventy"},
		"an out-of-range port":      {"UFFICI_HOME": home, "UFFICI_COMPANION_PORT": "99999"},
		"an unknown log level":      {"UFFICI_HOME": home, "UFFICI_LOG_LEVEL": "chatty"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadConfig(env(environment), Config{}); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestLoadConfigReadsTheLogLevel(t *testing.T) {
	home := shortHome(t)
	for value, want := range map[string]slog.Level{
		"": slog.LevelInfo, "debug": slog.LevelDebug, "WARN": slog.LevelWarn, "error": slog.LevelError,
	} {
		config, err := LoadConfig(env(map[string]string{"UFFICI_HOME": home, "UFFICI_LOG_LEVEL": value}), Config{})
		if err != nil {
			t.Fatalf("LoadConfig(%q): %v", value, err)
		}
		if config.LogLevel != want {
			t.Errorf("log level %q parsed to %v, want %v", value, config.LogLevel, want)
		}
	}
}

// 🔴 The gateway kind with no key must fail, never fall back to the fixture: a
// companion answering from a script while an operator believes it is reaching
// the platform is indistinguishable from one that works.
func TestRunRefusesTheGatewayHostWithoutAKey(t *testing.T) {
	home := shortHome(t)
	code, err := Run(context.Background(), Config{
		Home: home, AgentsHost: registry.KindGateway, GatewayURL: "https://agents.localhost",
	}, io.Discard)
	if code != ExitConfig {
		t.Fatalf("exit code %d, want %d", code, ExitConfig)
	}
	if err == nil || !strings.Contains(err.Error(), AgentKeyFile) {
		t.Fatalf("the refusal does not say where the key goes: %v", err)
	}
}

func TestRunNeedsAScript(t *testing.T) {
	code, err := Run(context.Background(), Config{Home: shortHome(t)}, io.Discard)
	if code != ExitConfig || err == nil {
		t.Fatalf("running with no fixture gave code %d, err %v", code, err)
	}
}

// ── The whole daemon, with a plugin answering ───────────────────────────────

// testPlugin is a plugin: it connects, says hello, and answers every brief.
// It reports a FRESH session the first time it sees a conversation and a
// RESUMED one afterwards, which is what a real harness does.
type testPlugin struct {
	conn *ws.Conn

	mu   sync.Mutex
	seen map[string]bool
}

func dialPlugin(t *testing.T, socket, token string) *testPlugin {
	t.Helper()
	plugin, err := dialPluginErr(socket, token)
	if err != nil {
		t.Fatalf("dialling the companion: %v", err)
	}
	return plugin
}

// pluginReply is the one statement of what the test plugin posts. 🔴 Hoisted
// because the log guard looks for it: a hand-copied second spelling would leave
// the guard searching for a string that never existed, and passing.
const pluginReply = "yes, it is filed"

// dialPluginErr is dialPlugin without the testing.T, for the goroutine that
// reattaches after a disconnect — `t.Fatalf` off the test's own goroutine does
// not stop the test, it just stops that goroutine, and the run then hangs
// until the outer timeout with no reason given.
func dialPluginErr(socket, token string) (*testPlugin, error) {
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 🔴 The WHOLE handshake is retried, not just the dial, and an attach is not
	// believed until the companion's own hello comes back. The single-plugin
	// claim is released asynchronously — after the dropped connection's handler
	// unwinds — so a reconnect can land in the window where the socket accepts,
	// the upgrade succeeds and the companion then closes it with
	// `already_attached`. Treating the upgrade as success there hands back a
	// connection that is already dead, and every later expectation times out
	// naming something other than the cause.
	var lastErr error
	for attempt := 0; attempt < 100; attempt++ {
		plugin, err := attachOnce(ctx, client, token)
		if err == nil {
			return plugin, nil
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	return nil, lastErr
}

// attachOnce performs one dial + hello + companion-hello exchange.
func attachOnce(ctx context.Context, client *http.Client, token string) (*testPlugin, error) {
	conn, _, err := ws.Dial(ctx, "ws://companion/v1/ring", &ws.DialOptions{HTTPClient: client})
	if err != nil {
		return nil, err
	}

	raw, err := json.Marshal(map[string]any{
		"v": 1, "type": "hello", "harness": "hermes", "harnessVersion": "0.21.3",
		"pluginVersion": "0.1.0", "token": token, "contextBudgetTokens": 48000,
	})
	if err != nil {
		_ = conn.CloseNow()
		return nil, err
	}
	if err := conn.Write(ctx, ws.MessageText, raw); err != nil {
		_ = conn.CloseNow()
		return nil, err
	}

	// The companion answers hello with its own. Anything else — a close frame, a
	// dropped connection — means this attach did not take.
	_, answer, err := conn.Read(ctx)
	if err != nil {
		_ = conn.CloseNow()
		return nil, err
	}
	var frame map[string]any
	if err := json.Unmarshal(answer, &frame); err != nil || frame["type"] != "hello" {
		_ = conn.CloseNow()
		return nil, fmt.Errorf("the companion answered hello with %v", frame["type"])
	}

	// 🔴 And `ready`, or nothing is ever delivered. `hello` says a plugin is
	// connected; `ready` says its HARNESS can answer, and the companion holds
	// every ring until it hears the second one (#2014).
	ready, err := json.Marshal(map[string]any{
		"v": 1, "type": "ready", "harnessVersion": "0.21.3",
	})
	if err != nil {
		_ = conn.CloseNow()
		return nil, err
	}
	if err := conn.Write(ctx, ws.MessageText, ready); err != nil {
		_ = conn.CloseNow()
		return nil, err
	}
	return &testPlugin{conn: conn, seen: map[string]bool{}}, nil
}

func (p *testPlugin) send(t *testing.T, frame map[string]any) {
	t.Helper()
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := p.conn.Write(ctx, ws.MessageText, raw); err != nil {
		t.Logf("plugin write: %v", err)
	}
}

// answer reads frames and replies until the context ends.
func (p *testPlugin) answer(ctx context.Context, t *testing.T) {
	for {
		_, raw, err := p.conn.Read(ctx)
		if err != nil {
			return
		}
		var frame map[string]any
		if err := json.Unmarshal(raw, &frame); err != nil {
			continue
		}
		if frame["type"] != "brief" {
			continue
		}
		requestID, _ := frame["requestId"].(string)
		conversationID, _ := frame["conversationId"].(string)

		p.mu.Lock()
		session := "fresh"
		if p.seen[conversationID] {
			session = "resumed"
		}
		p.seen[conversationID] = true
		p.mu.Unlock()

		p.send(t, map[string]any{
			"v": 1, "type": "ack", "requestId": requestID,
			"outcome": "accepted", "session": session,
		})
		p.send(t, map[string]any{
			"v": 1, "type": "post", "requestId": requestID, "text": pluginReply,
		})
		p.send(t, map[string]any{
			"v": 1, "type": "done", "requestId": requestID, "outcome": "answered",
		})
	}
}

// 🔴 THE ACCEPTANCE CRITERION: `--once` against a test plugin exits 0, having
// played a fresh session, a resumed one in the same conversation, and a mention
// from a lagging offset.
func TestTheFixtureScriptPassesAgainstAPlugin(t *testing.T) {
	home := shortHome(t)
	token, err := EnsureToken(home, "")
	if err != nil {
		t.Fatalf("EnsureToken: %v", err)
	}
	socket := filepath.Join(home, SocketFile)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	type outcome struct {
		code int
		err  error
	}
	logSink := &syncBuffer{}
	done := make(chan outcome, 1)
	go func() {
		code, err := Run(ctx, Config{
			Home: home, FixturePath: fixturePath(t), Once: true,
			Transport: contracts.TransportSocket, SocketPath: socket,
			AgentsHost: registry.KindFixture, Version: "test",
		}, logSink)
		done <- outcome{code, err}
	}()

	plugin := dialPlugin(t, socket, token)
	pluginCtx, stopPlugin := context.WithCancel(ctx)
	defer stopPlugin()
	go plugin.answer(pluginCtx, t)

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("the run errored: %v", got.err)
		}
		if got.code != ExitOK {
			t.Fatalf("the fixture script exited %d, want %d — every expectation must hold.\nMisses:\n%s",
				got.code, ExitOK, logSink.misses())
		}
	case <-ctx.Done():
		t.Fatal("the run never finished")
	}
}

// syncBuffer collects the daemon's log so a failure can show the misses.
type syncBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	return len(p), nil
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}

func (b *syncBuffer) misses() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, line := range strings.Split(string(b.data), "\n") {
		if strings.Contains(line, "unmet expectation") || strings.Contains(line, "script complete") {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// fixturePath finds fixture/basic.json from the module root.
func fixturePath(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		candidate := filepath.Join(dir, "fixture", "basic.json")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find fixture/basic.json")
		}
		dir = parent
	}
}

// 🔴 The verdict's log line is what a CI job reads. It must carry the counts
// and every miss — and never a post body or a delta, which is why the runner
// hands over counts in the first place.
func TestTheVerdictReportsCountsAndMissesAndNoContent(t *testing.T) {
	sink := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(sink, nil))

	report(logger, runner.Result{
		RingsDelivered: 0,
		Unmet:          []string{"step 3 (expectPost): no post satisfying contains \"filed\""},
		Counts:         runner.Counts{Briefs: 2, Openers: 1, Posts: 0, Redeliveries: 1},
	})

	logged := sink.String()
	for _, want := range []string{"ringsDelivered", "unmet expectation", "no ring was delivered"} {
		if !strings.Contains(logged, want) {
			t.Errorf("the verdict never mentions %q:\n%s", want, logged)
		}
	}
	// The daemon's own log must not become a transcript.
	for _, forbidden := range []string{"\"delta\"", "\"text\"", "\"token\""} {
		if strings.Contains(logged, forbidden) {
			t.Errorf("the verdict log carries %s:\n%s", forbidden, logged)
		}
	}
}

// 🔴 The two script steps that assert on the DAEMON rather than on the fixture:
// `secondConnection` (a second plugin is refused while one holds the wire) and
// `disconnectPlugin` (an unacknowledged request is redelivered when a plugin
// attaches again). Both were unreachable while the runner's plugin control was
// a stand-in — this is the test that makes them reach the real surface.
func TestASecondPluginIsRefusedAndADroppedOneRedelivers(t *testing.T) {
	home := shortHome(t)
	token, err := EnsureToken(home, "")
	if err != nil {
		t.Fatalf("EnsureToken: %v", err)
	}
	socket := filepath.Join(home, SocketFile)
	script := writeReconnectScript(t, home)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	logSink := &syncBuffer{}
	type outcome struct {
		code int
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		code, runErr := Run(ctx, Config{
			Home: home, FixturePath: script, Once: true,
			Transport: contracts.TransportSocket, SocketPath: socket,
			AgentsHost: registry.KindFixture, Version: "test",
		}, logSink)
		done <- outcome{code, runErr}
	}()

	// The first plugin attaches and stays SILENT: it takes the brief and never
	// acknowledges it, which is what leaves a request redeliverable.
	first := dialPlugin(t, socket, token)

	// A real plugin reattaches after its process is dropped. This goroutine is
	// that: it waits for the daemon to end the connection, dials again, and
	// answers whatever is redelivered to it.
	reconnected := make(chan error, 1)
	go func() {
		for {
			if _, _, readErr := first.conn.Read(ctx); readErr != nil {
				break
			}
		}
		second, dialErr := dialPluginErr(socket, token)
		if dialErr != nil {
			reconnected <- dialErr
			return
		}
		reconnected <- nil
		second.answer(ctx, t)
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("the run errored: %v", got.err)
		}
		if got.code != ExitOK {
			t.Fatalf("the script exited %d, want %d.\nMisses:\n%s", got.code, ExitOK, logSink.misses())
		}
	case <-ctx.Done():
		t.Fatalf("the run never finished.\nLog:\n%s", logSink.String())
	}

	select {
	case dialErr := <-reconnected:
		if dialErr != nil {
			t.Fatalf("the plugin never reattached: %v", dialErr)
		}
	default:
		t.Fatal("the script passed without the plugin ever reattaching, so the redelivery " +
			"was asserted over a connection that never came back")
	}
}

// writeReconnectScript writes the script the test above drives. It lives here
// rather than in `fixture/` because it is this test's subject, not the
// companion's shipped acceptance script.
func writeReconnectScript(t *testing.T, dir string) string {
	t.Helper()
	member := map[string]any{"type": "member", "id": "m1", "label": "Ana Marques", "self": false}
	script := map[string]any{
		"v": 1,
		"conversations": map[string]any{
			"reconnect": map[string]any{
				"id": "conv_reconnect", "offset": "c0001",
				"entries": []any{
					map[string]any{
						"id": "e1", "cursor": "c0002", "at": "2026-09-17T14:01:00Z",
						"kind": "message", "text": "Is the Lisbon invoice approved?", "author": member,
					},
				},
			},
		},
		"steps": []any{
			map[string]any{"step": "secondConnection"},
			map[string]any{
				"step": "ring", "conversation": "reconnect", "reason": "mentioned",
				"causeEntry": "e1", "throughCursor": "c0002", "deadlineMs": 30000,
			},
			// Long enough for the brief to be DELIVERED: an unacknowledged request
			// is redeliverable, one still queued is not, and dropping the plugin
			// too early would assert nothing.
			map[string]any{"step": "wait", "ms": 500},
			map[string]any{"step": "disconnectPlugin"},
			map[string]any{"step": "expectRedelivery", "withinMs": 20000},
			map[string]any{"step": "expectAck", "outcome": "accepted", "session": "fresh", "withinMs": 20000},
			map[string]any{"step": "expectPost", "matcher": "nonEmpty", "withinMs": 20000},
			map[string]any{"step": "expectClose", "outcome": "answered", "withinMs": 20000},
			map[string]any{"step": "expectOffset", "conversation": "reconnect", "cursor": "c0002"},
		},
		"assert": map[string]any{"ringsDelivered": 1},
	}
	raw, err := json.MarshalIndent(script, "", "  ")
	if err != nil {
		t.Fatalf("encoding the script: %v", err)
	}
	path := filepath.Join(dir, "reconnect.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("writing the script: %v", err)
	}
	return path
}

// 🔴 THE LOG-CLASSIFICATION GUARD. A whole fixture run at the most verbose
// level, with every piece of tenant content in the script — and the wire token —
// looked for in the log that run produced.
//
// It is end to end rather than a unit test over the logger because the risk is
// not one call site: it is any of the surface, the state machine, the fixture
// and the observer gaining a field. Running the real daemon is what makes the
// absence mean something.
func TestNoTranscriptContentOrTokenReachesTheLog(t *testing.T) {
	home := shortHome(t)
	token, err := EnsureToken(home, "")
	if err != nil {
		t.Fatalf("EnsureToken: %v", err)
	}
	socket := filepath.Join(home, SocketFile)

	// Every text the script carries, plus the reply the plugin posts and the
	// token. Read from the script itself, so an entry added to the fixture is
	// covered without touching this test.
	secrets := append(scriptTexts(t), token, pluginReply)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	logSink := &syncBuffer{}
	done := make(chan int, 1)
	go func() {
		code, _ := Run(ctx, Config{
			Home: home, FixturePath: fixturePath(t), Once: true,
			Transport: contracts.TransportSocket, SocketPath: socket,
			AgentsHost: registry.KindFixture, Version: "test",
			// 🔴 Debug, not the default: "never at any level" is the claim, and a
			// test at INFO would pass over a debug line that prints the frame.
			LogLevel: slog.LevelDebug,
		}, logSink)
		done <- code
	}()

	plugin := dialPlugin(t, socket, token)
	pluginCtx, stopPlugin := context.WithCancel(ctx)
	defer stopPlugin()
	go plugin.answer(pluginCtx, t)

	select {
	case code := <-done:
		if code != ExitOK {
			t.Fatalf("the run exited %d; this guard needs a run that actually happened.\n%s",
				code, logSink.misses())
		}
	case <-ctx.Done():
		t.Fatal("the run never finished")
	}

	log := logSink.String()

	// 🔴 Non-vacuity, both halves: the log must exist, and it must carry the
	// counts — a daemon that logged nothing would satisfy every assertion below.
	if len(log) == 0 {
		t.Fatal("the run produced no log at all, so this guard looked at nothing")
	}
	for _, field := range []string{"deltaEntryCount", "deltaChars", "openerChars", "budgetDeclared", "elapsedMs"} {
		if !strings.Contains(log, field) {
			t.Fatalf("the log carries no %q, so the counts wave 4 needs are not being emitted", field)
		}
	}

	if len(secrets) < 4 {
		t.Fatalf("only %d texts to look for; the fixture script must carry more than that", len(secrets))
	}
	for _, secret := range secrets {
		if strings.Contains(log, secret) {
			t.Fatalf("the log contains %q — transcript content and the wire token are never logged", secret)
		}
	}
}

// scriptTexts reads every entry's text out of the fixture script, so the guard
// above covers whatever the script carries rather than a copied list.
func scriptTexts(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(fixturePath(t))
	if err != nil {
		t.Fatalf("reading the fixture: %v", err)
	}
	// 🔴 Labels, handles and attachment names too, not only entry text. They are
	// personal data sitting in the same script, and the realistic next leak is
	// one of them: the surface already logs peer-supplied identity strings from
	// `hello`, so a display name is one field away from a log line.
	type entry struct {
		Text   string `json:"text"`
		Author struct {
			Label  string `json:"label"`
			Handle string `json:"handle"`
		} `json:"author"`
		Attachments []struct {
			Name string `json:"name"`
		} `json:"attachments"`
	}
	var script struct {
		Conversations map[string]struct {
			Entries []entry `json:"entries"`
		} `json:"conversations"`
		Steps []struct {
			Entries []entry `json:"entries"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(raw, &script); err != nil {
		t.Fatalf("decoding the fixture: %v", err)
	}
	var texts []string
	collect := func(entries []entry) {
		for _, item := range entries {
			for _, candidate := range []string{item.Text, item.Author.Label, item.Author.Handle} {
				if candidate != "" {
					texts = append(texts, candidate)
				}
			}
			for _, attachment := range item.Attachments {
				if attachment.Name != "" {
					texts = append(texts, attachment.Name)
				}
			}
		}
	}
	for _, conversation := range script.Conversations {
		collect(conversation.Entries)
	}
	// `appendEntries` steps carry content too, and theirs is what a later delta
	// is built from.
	for _, step := range script.Steps {
		collect(step.Entries)
	}
	return texts
}

// 🔴 A shutdown TELLS the plugin. `cmd/uffici` stops serving on SIGTERM, and a
// plugin that simply loses its socket cannot tell a deliberate shutdown from a
// crash or a severed connection — so it cannot decide whether to reconnect.
// Before the fix this test's close reason was empty: the pump returned on the
// cancelled context without ever sending a close frame.
func TestShutdownClosesThePluginWithAReason(t *testing.T) {
	home := shortHome(t)
	token, err := EnsureToken(home, "")
	if err != nil {
		t.Fatalf("EnsureToken: %v", err)
	}
	socket := filepath.Join(home, SocketFile)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	serveCtx, shutdown := context.WithCancel(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = Run(serveCtx, Config{
			Home: home, FixturePath: fixturePath(t), Once: false,
			Transport: contracts.TransportSocket, SocketPath: socket,
			AgentsHost: registry.KindFixture, Version: "test",
		}, &syncBuffer{})
	}()

	// dialPlugin does not return until the companion's own hello came back, so
	// the plugin is attached before the shutdown rather than racing it.
	plugin := dialPlugin(t, socket, token)

	shutdown()

	_, _, readErr := plugin.conn.Read(ctx)
	if readErr == nil {
		t.Fatal("the connection stayed open through the shutdown")
	}
	if got := ws.CloseStatus(readErr); got == -1 {
		t.Fatalf("the plugin was dropped without a close frame: %v", readErr)
	}
	if !strings.Contains(readErr.Error(), "shutting_down") {
		t.Fatalf("the plugin was closed with %v, want the reason shutting_down", readErr)
	}
	<-done
}

func TestTheAttachWaitIsSettableFromTheConfig(t *testing.T) {
	// 🔴 Not a style preference. Since the `ready` frame, "a plugin attached"
	// means "its harness can answer", so this budget spans a whole harness boot
	// — and the built-in 15s is a laptop's number. A CI runner passes its own,
	// or the companion gives up while the harness is still starting and the run
	// blames the half that was working. The mirror of this test lives in
	// `scripts/__tests__/uffici-compose-profile.test.mjs`, which pins that the
	// e2e actually passes one.
	home := shortHome(t)
	if _, err := EnsureToken(home, ""); err != nil {
		t.Fatalf("EnsureToken: %v", err)
	}

	started := time.Now()
	code, err := Run(context.Background(), Config{
		Home: home, FixturePath: fixturePath(t), Once: true,
		Transport:  contracts.TransportSocket,
		SocketPath: filepath.Join(home, SocketFile),
		AgentsHost: registry.KindFixture, Version: "test",
		AttachWait: 50 * time.Millisecond,
	}, io.Discard)
	elapsed := time.Since(started)

	if err != nil {
		t.Fatalf("the run errored: %v", err)
	}
	if code != ExitMiss {
		t.Fatalf("exit code %d, want %d — no plugin ever attached", code, ExitMiss)
	}
	// Generous, and still an order of magnitude under the 15s default: the
	// point is that the CONFIG decided, not the constant.
	if elapsed > 5*time.Second {
		t.Fatalf("the run waited %s, so it used the built-in default rather than the configured 50ms", elapsed)
	}
}

// ── The token in a container (#2014's pod shape) ────────────────────────────

func TestASuppliedTokenIsUsedAndNeverWrittenToDisk(t *testing.T) {
	// 🔴 The pod shape has no other way. The plugin and the companion are two
	// containers with separate filesystems, so the file the companion mints is
	// unreadable to the plugin — compose hands BOTH the same `UFFICI_WIRE_TOKEN`
	// instead. A companion that ignored it minted its own and refused every
	// connection with `unauthorized`, which reads as a plugin defect.
	supplied := strings.Repeat("s", 43)
	home := shortHome(t)

	config, err := LoadConfig(func(key string) string {
		if key == "UFFICI_WIRE_TOKEN" {
			return supplied
		}
		return ""
	}, Config{Home: home})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	token, err := EnsureToken(config.Home, config.Token)
	if err != nil {
		t.Fatalf("EnsureToken: %v", err)
	}
	if token != supplied {
		t.Fatalf("the companion used a token of its own, not the supplied one")
	}
	// 🔴 And it is not on disk. A supplied token belongs to whoever supplied it;
	// writing it into `$UFFICI_HOME` would leave a copy behind in a volume that
	// outlives the container, at a path whose whole security argument is that it
	// holds a secret nothing else has seen.
	if _, err := os.Stat(filepath.Join(home, TokenFile)); !os.IsNotExist(err) {
		t.Fatalf("a supplied token was written to %s", filepath.Join(home, TokenFile))
	}
}

func TestATooShortSuppliedTokenIsRefused(t *testing.T) {
	// The wire's schema requires 32 characters. Refusing here names the variable;
	// accepting would fail later as a schema violation on the plugin's hello,
	// which says nothing about where the value came from.
	_, err := EnsureToken(shortHome(t), "too-short")
	if err == nil || !strings.Contains(err.Error(), "UFFICI_WIRE_TOKEN") {
		t.Fatalf("a nine-character supplied token gave %v, want a refusal naming the variable", err)
	}
	// 🔴 And the refusal does not quote the token itself.
	if err != nil && strings.Contains(err.Error(), "too-short") {
		t.Fatal("the refusal quotes the token, which is secret-class")
	}
}
