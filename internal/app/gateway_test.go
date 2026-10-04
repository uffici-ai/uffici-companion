package app

import (
	"context"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/infra/gateway/gatewaytest"
	wstransport "github.com/uffici-ai/uffici-companion/internal/infra/transport/websocket"
	"github.com/uffici-ai/uffici-companion/internal/protocol"
	"github.com/uffici-ai/uffici-companion/internal/registry"
)

// ── The agent key ───────────────────────────────────────────────────────────

func writeKey(t *testing.T, home string, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(home, AgentKeyFile)
	if err := os.WriteFile(path, []byte(gatewaytest.Key+"\n"), mode); err != nil {
		t.Fatal(err)
	}
	// WriteFile's mode is masked by the umask; set it exactly.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func TestTheKeyFileIsReadAndPreferredOverTheEnvironment(t *testing.T) {
	home := shortHome(t)
	writeKey(t, home, 0o600)
	key, err := LoadAgentKey(home, "from-the-environment")
	if err != nil {
		t.Fatalf("LoadAgentKey: %v", err)
	}
	if key != gatewaytest.Key {
		t.Fatal("the environment won over the key file")
	}
}

func TestTheEnvironmentKeyIsTheFallback(t *testing.T) {
	key, err := LoadAgentKey(shortHome(t), "  from-a-secret-mount \n")
	if err != nil || key != "from-a-secret-mount" {
		t.Fatalf("LoadAgentKey = %q, %v", key, err)
	}
}

// 🔴 Refused, never repaired: the window in which the key was readable is the
// question, and the message must not quote the key while saying so.
func TestAWorldOrGroupReadableKeyIsRefused(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604} {
		home := shortHome(t)
		writeKey(t, home, mode)
		_, err := LoadAgentKey(home, "")
		if err == nil {
			t.Fatalf("a key file of mode %04o was accepted", mode)
		}
		if strings.Contains(err.Error(), gatewaytest.Key) {
			t.Fatal("the refusal quotes the key")
		}
		if !strings.Contains(err.Error(), "chmod 600") {
			t.Fatalf("the refusal does not say how to fix it: %v", err)
		}
	}
}

func TestNoKeyAtAllSaysWhereOneGoes(t *testing.T) {
	_, err := LoadAgentKey(shortHome(t), "")
	if err == nil || !strings.Contains(err.Error(), AgentKeyFile) || !strings.Contains(err.Error(), "UFFICI_AGENT_KEY") {
		t.Fatalf("got %v", err)
	}
	empty := shortHome(t)
	writeKey(t, empty, 0o600)
	if err := os.WriteFile(filepath.Join(empty, AgentKeyFile), []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAgentKey(empty, "fallback"); err == nil {
		t.Fatal("an empty key file was accepted")
	}
}

// ── The instance id ─────────────────────────────────────────────────────────

func TestTheInstanceIDSurvivesARestart(t *testing.T) {
	home := shortHome(t)
	first, err := EnsureInstanceID(home)
	if err != nil {
		t.Fatalf("EnsureInstanceID: %v", err)
	}
	if !uuidPattern.MatchString(first) {
		t.Fatalf("minted %q, which the gateway would refuse", first)
	}
	second, err := EnsureInstanceID(home)
	if err != nil {
		t.Fatal(err)
	}
	// 🔴 A new id per start makes the companion its own ghost: refused
	// `instance_conflict` by its previous connection after every restart.
	if second != first {
		t.Fatal("a restart minted a new instance id")
	}
	info, err := os.Stat(filepath.Join(home, InstanceIDFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != TokenMode {
		t.Fatalf("the instance id is mode %04o, want %04o", info.Mode().Perm(), TokenMode)
	}
}

func TestAMalformedInstanceIDIsRefusedNotReplaced(t *testing.T) {
	home := shortHome(t)
	path := filepath.Join(home, InstanceIDFile)
	if err := os.WriteFile(path, []byte("not-a-uuid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureInstanceID(home); err == nil {
		t.Fatal("a malformed instance id was accepted")
	}
	raw, _ := os.ReadFile(path)
	if strings.TrimSpace(string(raw)) != "not-a-uuid" {
		t.Fatal("the malformed file was silently replaced")
	}
}

// ── Configuration ───────────────────────────────────────────────────────────

func TestLoadConfigReadsTheGatewayKind(t *testing.T) {
	home := shortHome(t)
	config, err := LoadConfig(env(map[string]string{
		"UFFICI_HOME": home, "UFFICI_AGENTS_HOST": "gateway",
		"UFFICI_GATEWAY_URL": "https://agents.localhost/", "UFFICI_CA_FILE": "/certs/dev.crt",
		"UFFICI_AGENT_KEY": "secret",
	}), Config{})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if config.AgentsHost != registry.KindGateway || config.GatewayURL != "https://agents.localhost" ||
		config.CAFile != "/certs/dev.crt" || config.AgentKey != "secret" {
		t.Fatalf("read %+v", config)
	}
}

func TestLoadConfigRefusesABrokenGatewayConfiguration(t *testing.T) {
	home := shortHome(t)
	for name, testCase := range map[string]struct {
		environment map[string]string
		flags       Config
	}{
		// 🔴 Exit 2, never a quiet fixture: `other` is not a host this build knows.
		"an unknown agents host":  {environment: map[string]string{"UFFICI_AGENTS_HOST": "other"}},
		"no gateway URL":          {environment: map[string]string{"UFFICI_AGENTS_HOST": "gateway"}},
		"plain http":              {environment: map[string]string{"UFFICI_AGENTS_HOST": "gateway", "UFFICI_GATEWAY_URL": "http://agents.localhost"}},
		"a host with no scheme":   {environment: map[string]string{"UFFICI_AGENTS_HOST": "gateway", "UFFICI_GATEWAY_URL": "agents.uffici.dev"}},
		"a path":                  {environment: map[string]string{"UFFICI_AGENTS_HOST": "gateway", "UFFICI_GATEWAY_URL": "https://agents.uffici.dev/v1"}},
		"credentials in the URL":  {environment: map[string]string{"UFFICI_AGENTS_HOST": "gateway", "UFFICI_GATEWAY_URL": "https://u:hunter2@agents.uffici.dev"}},
		"--once against the host": {environment: map[string]string{"UFFICI_AGENTS_HOST": "gateway", "UFFICI_GATEWAY_URL": "https://agents.uffici.dev"}, flags: Config{Once: true}},
	} {
		t.Run(name, func(t *testing.T) {
			testCase.environment["UFFICI_HOME"] = home
			if _, err := LoadConfig(env(testCase.environment), testCase.flags); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestHTTPClientRefusesACAFileWithNoCertificate(t *testing.T) {
	path := filepath.Join(shortHome(t), "ca.pem")
	if err := os.WriteFile(path, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := HTTPClient(path); err == nil {
		t.Fatal("a CA file with no certificate was accepted")
	}
	if _, err := HTTPClient(filepath.Join(shortHome(t), "absent.pem")); err == nil {
		t.Fatal("a missing CA file was accepted")
	}
	if _, err := HTTPClient(""); err != nil {
		t.Fatalf("the system trust store alone: %v", err)
	}
}

// ── The whole daemon against the gateway double ─────────────────────────────

// writeCA writes the double's certificate as a PEM file — the dev CA of gate 1,
// here the httptest server's own.
func writeCA(t *testing.T, double *gatewaytest.Double, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "ca.pem")
	block := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: double.Server.Certificate().Raw})
	if err := os.WriteFile(path, block, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTheDaemonAnswersARingThroughTheGateway(t *testing.T) {
	const (
		conversationID = "a1b2c3d4-e5f6-4789-8abc-def012345678"
		requestID      = "6f1c0f7a-2f0e-4f2e-9b1e-0d6a0b5c7e21"
		ringID         = "0b9d2f4e-3c1a-4e8b-9f7d-5a6c8e0b1d23"
	)
	double := gatewaytest.New()
	t.Cleanup(double.Close)
	double.Seed(conversationID, gatewaytest.Conversation{Offset: "1041", Entries: []protocol.Entry{{
		ID: "1042", Cursor: "1042", At: time.Now().UTC(), Kind: "message", Text: "@smith is it filed?",
		Author:      protocol.Author{Type: "user", ID: "u-1", Label: "Mona", Self: false},
		Attachments: []protocol.Attachment{},
	}}})

	home := shortHome(t)
	writeKey(t, home, 0o600)
	logs := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		code, _ := Run(ctx, Config{
			Home: home, AgentsHost: registry.KindGateway, GatewayURL: double.URL(),
			CAFile: writeCA(t, double, home), Transport: "socket",
			SocketPath: filepath.Join(home, SocketFile), TCPAddr: "127.0.0.1:0", Version: "9.9.9",
		}, logs)
		done <- code
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the daemon did not stop")
		}
	})

	waitUntil(t, "the doorbell", double.Connected)
	token := waitForToken(t, home)
	plugin := dialPlugin(t, filepath.Join(home, SocketFile), token)
	t.Cleanup(func() { _ = plugin.conn.CloseNow() })
	go plugin.answer(ctx, t)

	if err := double.Ring(protocol.Ring{
		RingID: ringID, RequestID: requestID, ConversationID: conversationID, Reason: "mention",
		Cause:    protocol.RingCause{EntryID: "1042", Author: protocol.RingAuthor{Type: "user", ID: "u-1"}},
		Deadline: time.Now().Add(5 * time.Minute).UTC(), Since: "1041", Through: "1042",
	}); err != nil {
		t.Fatal(err)
	}

	waitUntil(t, "the close", func() bool { return len(double.Closes()) == 1 })
	// 🔴 The ack is a doorbell FRAME the double reads on its own goroutine, and
	// the close is an HTTP call it records on another: on a loaded runner the
	// close was observed first and a one-shot read of the acks saw none (#2199,
	// the companion's v0.2.1 release run). Each of the double's lists is waited
	// for, never read once, before its contents are judged.
	waitUntil(t, "the ack", func() bool { return len(double.Acks()) == 1 })
	if acks := double.Acks(); acks[0] != ringID {
		t.Fatalf("acked %v, want the ring on receipt", acks)
	}
	waitUntil(t, "the post", func() bool { return len(double.Posts()) == 1 })
	if posts := double.Posts(); posts[0].Text != pluginReply {
		t.Fatalf("posted %+v", posts)
	}
	if closes := double.Closes(); closes[0].Outcome != "answered" {
		t.Fatalf("closed as %+v", closes[0])
	}
	if offset, _ := double.OffsetOf(conversationID); offset != "1042" {
		t.Fatalf("the offset is %s, want 1042", offset)
	}
	if refusals := double.Refusals(); len(refusals) != 0 {
		t.Fatalf("the double refused off-contract bodies: %v", refusals)
	}
	facts := double.ConnectFacts()
	if len(facts) == 0 || facts[0].Client.Version != "9.9.9" {
		t.Fatalf("connected with %+v", facts)
	}
	// 🔴 Neither the key nor the member's words nor the reply reach the log.
	for _, secret := range []string{gatewaytest.Key, "is it filed", pluginReply, token} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("the log carries %q", secret)
		}
	}
}

func waitUntil(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func waitForToken(t *testing.T, home string) string {
	t.Helper()
	var token string
	waitUntil(t, "the wire token", func() bool {
		raw, err := os.ReadFile(filepath.Join(home, TokenFile))
		token = strings.TrimSpace(string(raw))
		return err == nil && token != ""
	})
	return token
}

// ── uffici doctor ───────────────────────────────────────────────────────────

func doctorConfig(t *testing.T, double *gatewaytest.Double) Config {
	t.Helper()
	home := shortHome(t)
	writeKey(t, home, 0o600)
	return Config{Home: home, AgentsHost: registry.KindGateway, GatewayURL: double.URL(),
		CAFile: writeCA(t, double, home), Version: "9.9.9",
		Transport: "socket", SocketPath: filepath.Join(home, SocketFile), TCPAddr: "127.0.0.1:0"}
}

// startDaemon runs the companion on config, as the machine's service would,
// until the test ends.
func startDaemon(t *testing.T, config Config) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		code, _ := Run(ctx, config, &syncBuffer{})
		done <- code
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the daemon did not stop")
		}
	})
}

// 🔴 #2257: the doctor presents this machine's instance id, and the gateway
// keeps one doorbell per instance — so a doctor that probed on a machine whose
// companion was running replaced that companion's doorbell, at the end of
// every setup. It must ask the running companion and open nothing.
func TestTheDoctorAsksARunningCompanionAndOpensNoSecondDoorbell(t *testing.T) {
	double := gatewaytest.New()
	t.Cleanup(double.Close)
	config := doctorConfig(t, double)
	startDaemon(t, config)
	waitUntil(t, "the doorbell", double.Connected)
	upgrades, mints := double.Upgrades(), double.Mints("connect")

	out := &syncBuffer{}
	if code := Doctor(context.Background(), config, out); code != ExitOK {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	if double.Upgrades() != upgrades || double.Mints("connect") != mints {
		t.Fatalf("the doctor opened a doorbell of its own beside the running companion's: %d upgrades, %d mints (were %d, %d)",
			double.Upgrades(), double.Mints("connect"), upgrades, mints)
	}
	for _, want := range []string{"running on this machine — asked it", "connected (connection ", "plugin       not attached"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("the doctor did not say %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), gatewaytest.Key) {
		t.Fatalf("the doctor printed the key:\n%s", out.String())
	}
}

// Setup restarts the service and runs the doctor at once, before the service
// listens: the doctor waits for it rather than probing in that window.
func TestTheDoctorWaitsForAServiceThatIsStillStarting(t *testing.T) {
	double := gatewaytest.New()
	t.Cleanup(double.Close)
	config := doctorConfig(t, double)
	go func() {
		time.Sleep(700 * time.Millisecond)
		startDaemon(t, config)
	}()

	out := &syncBuffer{}
	code := DoctorWith(context.Background(), config, out, DoctorOptions{AwaitCompanion: 10 * time.Second})
	if code != ExitOK || !strings.Contains(out.String(), "running on this machine — asked it") {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	if double.Upgrades() != 1 {
		t.Fatalf("%d doorbells for one companion — the doctor probed instead of waiting", double.Upgrades())
	}
}

// A running companion that cannot connect is reported as such — and still not
// probed past, which would fail for the same reason and replace nothing useful.
func TestTheDoctorReportsARunningCompanionThatIsNotConnected(t *testing.T) {
	double := gatewaytest.New()
	t.Cleanup(double.Close)
	double.Inject("connect", gatewaytest.Injected{Status: 401, Body: gatewaytest.Envelope("unauthorized", false, nil)})
	config := doctorConfig(t, double)
	startDaemon(t, config)
	waitUntil(t, "the companion's wire", func() bool {
		_, err := wstransport.ReadHealth(context.Background(), wireOptions(config))
		return err == nil
	})

	out := &syncBuffer{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if code := Doctor(ctx, config, out); code != ExitMiss {
		t.Fatalf("exit %d, want %d:\n%s", code, ExitMiss, out.String())
	}
	if !strings.Contains(out.String(), "not connected — the running companion is still trying") {
		t.Fatalf("the doctor said:\n%s", out.String())
	}
}

func TestTheDoctorProvesTheDoorbell(t *testing.T) {
	double := gatewaytest.New()
	t.Cleanup(double.Close)
	out := &syncBuffer{}
	if code := Doctor(context.Background(), doctorConfig(t, double), out); code != ExitOK {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "connected (connection") || strings.Contains(out.String(), gatewaytest.Key) {
		t.Fatalf("the doctor said:\n%s", out.String())
	}
	if double.Upgrades() != 1 {
		t.Fatalf("%d upgrades for one probe", double.Upgrades())
	}
	if !strings.Contains(out.String(), "not running on this machine") {
		t.Fatalf("the doctor did not say why it probed:\n%s", out.String())
	}
}

func TestTheDoctorReportsTheDisconnectReason(t *testing.T) {
	double := gatewaytest.New()
	t.Cleanup(double.Close)
	double.SetConflict(true)
	out := &syncBuffer{}
	if code := Doctor(context.Background(), doctorConfig(t, double), out); code != ExitMiss {
		t.Fatalf("exit %d, want %d:\n%s", code, ExitMiss, out.String())
	}
	if !strings.Contains(out.String(), "refused: instance_conflict — ") {
		t.Fatalf("the doctor said:\n%s", out.String())
	}
}

func TestTheDoctorReportsARefusedKeyAndAnEdge(t *testing.T) {
	for name, injected := range map[string]struct {
		route  string
		answer gatewaytest.Injected
		want   string
	}{
		"a refused key": {"connect", gatewaytest.Injected{Status: 401, Body: gatewaytest.Envelope("unauthorized", false, nil)}, "refused: unauthorized"},
		"an edge page":  {"doorbell", gatewaytest.Injected{Status: 403, ContentType: "text/html", Body: "<html/>"}, "refused: edge_challenge"},
	} {
		t.Run(name, func(t *testing.T) {
			double := gatewaytest.New()
			t.Cleanup(double.Close)
			double.Inject(injected.route, injected.answer)
			out := &syncBuffer{}
			if code := Doctor(context.Background(), doctorConfig(t, double), out); code != ExitMiss {
				t.Fatalf("exit %d:\n%s", code, out.String())
			}
			if !strings.Contains(out.String(), injected.want) {
				t.Fatalf("the doctor said:\n%s", out.String())
			}
		})
	}
}

func TestTheDoctorWithoutAKeyCannotTry(t *testing.T) {
	out := &syncBuffer{}
	code := Doctor(context.Background(), Config{Home: shortHome(t), GatewayURL: "https://agents.localhost"}, out)
	if code != ExitConfig || !strings.Contains(out.String(), AgentKeyFile) {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
}

// The pod shape's command is fixed; the fixture run selects itself through the
// environment (#2078), and only when the fixture kind is explicit.
func TestTheFixtureRunCanBeSelectedFromTheEnvironment(t *testing.T) {
	home := shortHome(t)
	config, err := LoadConfig(env(map[string]string{
		"UFFICI_HOME": home, "UFFICI_AGENTS_HOST": "fixture",
		"UFFICI_FIXTURE": "/fixture/ci/wave1.json", "UFFICI_ONCE": "1",
	}), Config{})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if config.FixturePath != "/fixture/ci/wave1.json" || !config.Once {
		t.Fatalf("read %+v", config)
	}
	flagged, err := LoadConfig(env(map[string]string{
		"UFFICI_HOME": home, "UFFICI_FIXTURE": "/from-env.json",
	}), Config{FixturePath: "/from-flag.json"})
	if err != nil || flagged.FixturePath != "/from-flag.json" {
		t.Fatalf("the flag did not win: %+v %v", flagged, err)
	}
	// 🔴 A gateway run never picks up a script, whatever the environment says.
	gatewayRun, err := LoadConfig(env(map[string]string{
		"UFFICI_HOME": home, "UFFICI_AGENTS_HOST": "gateway", "UFFICI_GATEWAY_URL": "https://agents.localhost",
		"UFFICI_FIXTURE": "/fixture/ci/wave1.json", "UFFICI_ONCE": "1",
	}), Config{})
	if err != nil || gatewayRun.FixturePath != "" || gatewayRun.Once {
		t.Fatalf("a gateway run read the fixture's environment: %+v %v", gatewayRun, err)
	}
}

func TestARefusedGatewayURLNeverEchoesItsCredentials(t *testing.T) {
	_, err := LoadConfig(env(map[string]string{
		"UFFICI_HOME": shortHome(t), "UFFICI_AGENTS_HOST": "gateway",
		"UFFICI_GATEWAY_URL": "https://u:hunter2@agents.uffici.dev",
	}), Config{})
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("got %v", err)
	}
}

// After setup or upgrade restarts the service, an answer from the process being
// replaced says nothing about the new binary (launchd's bootout is
// asynchronous): the doctor waits for the version it expects and otherwise
// says the restart did not take.
func TestTheDoctorWaitsForTheVersionItExpects(t *testing.T) {
	double := gatewaytest.New()
	t.Cleanup(double.Close)
	config := doctorConfig(t, double) // the daemon reports 9.9.9
	startDaemon(t, config)
	waitUntil(t, "the doorbell", double.Connected)

	out := &syncBuffer{}
	if code := DoctorWith(context.Background(), config, out, DoctorOptions{AwaitCompanion: time.Second, ExpectVersion: "10.0.0"}); code != ExitMiss {
		t.Fatalf("exit %d, want %d:\n%s", code, ExitMiss, out.String())
	}
	if !strings.Contains(out.String(), "running 9.9.9, not 10.0.0") || double.Upgrades() != 1 {
		t.Fatalf("the doctor did not name the stale version, or probed (%d upgrades):\n%s", double.Upgrades(), out.String())
	}
	out = &syncBuffer{}
	if code := DoctorWith(context.Background(), config, out, DoctorOptions{AwaitCompanion: time.Second, ExpectVersion: "9.9.9"}); code != ExitOK {
		t.Fatalf("the expected version answered %d:\n%s", code, out.String())
	}
	if version, ok := RunningCompanionVersion(context.Background(), config); !ok || version != "9.9.9" {
		t.Fatalf("RunningCompanionVersion answered %q, %v", version, ok)
	}
}

// The release line runs on its own clock (#2382's review): a gateway slow to
// answer the release question must not spend the probe's budget.
func TestTheDoctorsReleaseLineHasItsOwnBudget(t *testing.T) {
	double := gatewaytest.New()
	t.Cleanup(double.Close)
	config := doctorConfig(t, double)
	startDaemon(t, config)
	waitUntil(t, "the doorbell", double.Connected)

	var releaseDeadline time.Time
	out := &syncBuffer{}
	release := func(ctx context.Context) string {
		releaseDeadline, _ = ctx.Deadline()
		return "develop, 0.10.0-dev.4"
	}
	if code := DoctorWith(context.Background(), config, out, DoctorOptions{AwaitCompanion: time.Second, Release: release}); code != ExitOK {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	if releaseDeadline.IsZero() || time.Until(releaseDeadline) > releaseLineTimeout {
		t.Fatalf("the release line ran on the probe's clock (deadline in %s), not its own %s", time.Until(releaseDeadline), releaseLineTimeout)
	}
	if !strings.Contains(out.String(), "release      develop, 0.10.0-dev.4") {
		t.Fatalf("no release line:\n%s", out.String())
	}
}
