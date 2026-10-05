package app

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	ws "github.com/coder/websocket"

	"github.com/uffici-ai/uffici-companion/internal/infra/gateway/gatewaytest"
	wstransport "github.com/uffici-ai/uffici-companion/internal/infra/transport/websocket"
)

// harnessLines are the doctor's `harness` lines, in order.
func harnessLines(out string) []string {
	var lines []string
	for line := range strings.SplitSeq(out, "\n") {
		if rest, ok := strings.CutPrefix(line, "  harness      "); ok {
			lines = append(lines, rest)
		}
	}
	return lines
}

// 🔴 #2414, end to end through the daemon: the plugin's statement of the
// harness versions it supports is what the doctor prints beside the harness —
// and an unsupported harness is never a refusal: it attaches, and the doctor
// exits OK.
func TestTheDoctorReportsThePluginsHarnessSupport(t *testing.T) {
	double := gatewaytest.New()
	t.Cleanup(double.Close)
	config := doctorConfig(t, double)
	startDaemon(t, config)
	waitUntil(t, "the doorbell", double.Connected)
	socket, token := filepath.Join(config.Home, SocketFile), waitForToken(t, config.Home)

	doctor := func() []string {
		t.Helper()
		out := &syncBuffer{}
		if code := Doctor(context.Background(), config, out); code != ExitOK {
			t.Fatalf("harness support changed the doctor's exit to %d:\n%s", code, out.String())
		}
		return harnessLines(out.String())
	}
	attach := func(extra map[string]any) func() {
		t.Helper()
		plugin, err := dialPluginHello(socket, token, extra)
		if err != nil {
			t.Fatalf("a plugin reporting %v did not attach: %v", extra, err)
		}
		waitUntil(t, "the plugin attached", func() bool {
			health, err := wstransport.ReadHealth(context.Background(), wireOptions(config))
			return err == nil && health.PluginAttached
		})
		return func() {
			_ = plugin.conn.Close(ws.StatusNormalClosure, "")
			waitUntil(t, "the detached plugin's harness gone from /healthz", func() bool {
				health, err := wstransport.ReadHealth(context.Background(), wireOptions(config))
				return err == nil && !health.PluginAttached && health.Harness == nil
			})
		}
	}

	if lines := doctor(); len(lines) != 0 {
		t.Fatalf("with no plugin attached the doctor reported a harness: %q", lines)
	}

	detach := attach(map[string]any{"harnessSupport": map[string]any{"range": ">=0.21.0,<0.22", "supported": false}})
	lines := doctor()
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "hermes 0.21.3 — NOT supported by this plugin, which supports >=0.21.0,<0.22") {
		t.Fatalf("an unsupported harness printed %q", lines)
	}
	detach()

	detach = attach(map[string]any{"harnessSupport": map[string]any{"range": ">=0.21.0,<0.22", "supported": true}})
	if lines := doctor(); len(lines) != 1 || lines[0] != "hermes 0.21.3 — supported by this plugin (>=0.21.0,<0.22)" {
		t.Fatalf("a supported harness printed %q", lines)
	}
	detach()

	detach = attach(nil)
	if lines := doctor(); len(lines) != 1 || !strings.Contains(lines[0], "support not reported") {
		t.Fatalf("a plugin that predates the report printed %q", lines)
	}
	detach()
}

func TestTheDoctorDescribesTheHarness(t *testing.T) {
	yes, no := true, false
	for _, testCase := range []struct {
		name    string
		harness *wstransport.HealthHarness
		want    string
	}{
		{"an older companion", nil, "not reported by the running companion — restart it to run this version"},
		{"an older plugin", &wstransport.HealthHarness{Kind: "hermes", Version: "0.21.5"},
			"hermes 0.21.5 — support not reported; the plugin predates the report"},
		{"supported", &wstransport.HealthHarness{Kind: "hermes", Version: "0.21.5", Range: ">=0.21.5,<0.22", Supported: &yes},
			"hermes 0.21.5 — supported by this plugin (>=0.21.5,<0.22)"},
		{"unsupported", &wstransport.HealthHarness{Kind: "hermes", Version: "0.23.0", Range: ">=0.21.5,<0.22", Supported: &no},
			"hermes 0.23.0 — NOT supported by this plugin, which supports >=0.21.5,<0.22; it may misbehave without an error. uffici upgrade brings a newer plugin, or run a harness in that range"},
		{"a version the plugin could not read is never supported", &wstransport.HealthHarness{Kind: "hermes", Version: "unknown", Range: ">=0.21.5,<0.22"},
			"hermes unknown — the plugin could not read the harness's version; it supports >=0.21.5,<0.22"},
		{"a version cannot steer the terminal", &wstransport.HealthHarness{Kind: "hermes", Version: "0.21\x1b[2J.5", Range: ">=0.21.5\n", Supported: &yes},
			"hermes 0.21[2J.5 — supported by this plugin (>=0.21.5)"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := describeHarness(testCase.harness); got != testCase.want {
				t.Fatalf("got  %q\nwant %q", got, testCase.want)
			}
		})
	}
}
