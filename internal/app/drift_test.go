package app

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	ws "github.com/coder/websocket"

	"github.com/uffici-ai/uffici-companion/internal/infra/gateway/gatewaytest"
	wstransport "github.com/uffici-ai/uffici-companion/internal/infra/transport/websocket"
)

// driftLines are the doctor's `drift` lines, in order.
func driftLines(out string) []string {
	var lines []string
	for line := range strings.SplitSeq(out, "\n") {
		if rest, ok := strings.CutPrefix(line, "  drift        "); ok {
			lines = append(lines, rest)
		}
	}
	return lines
}

// 🔴 #2406, end to end through the daemon: what the plugin says at hello is
// what the doctor prints — three lines for a harness that drifted on every
// kind, `none` for a clean one, `not reported` for a plugin that predates the
// report — and none of it is a refusal: every plugin attaches, and the doctor
// exits OK.
func TestTheDoctorReportsThePluginsDrift(t *testing.T) {
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
			t.Fatalf("drift changed the doctor's exit to %d:\n%s", code, out.String())
		}
		return driftLines(out.String())
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
			waitUntil(t, "the plugin detached", func() bool {
				health, err := wstransport.ReadHealth(context.Background(), wireOptions(config))
				return err == nil && !health.PluginAttached
			})
			// The report is the CONNECTED plugin's: it leaves with the connection.
			waitUntil(t, "the detached plugin's drift gone from /healthz", func() bool {
				health, err := wstransport.ReadHealth(context.Background(), wireOptions(config))
				return err == nil && health.Drift == nil
			})
		}
	}

	if lines := doctor(); len(lines) != 0 {
		t.Fatalf("with no plugin attached the doctor reported drift: %q", lines)
	}

	detach := attach(map[string]any{"drift": map[string]any{
		"mcpServers":  []string{"github"},
		"channels":    []string{"telegram"},
		"localMemory": []string{"memory.memory_enabled"},
	}})
	want := []string{
		"other MCP servers: github — their tool calls never pass through Uffici",
		"other channels: telegram — this harness also answers there, outside Uffici",
		"local memory still on: memory.memory_enabled — notes no one in the organization can see or delete; uffici setup turns it off",
	}
	if lines := doctor(); !slices.Equal(lines, want) {
		t.Fatalf("drift lines:\n  got  %q\n  want %q", lines, want)
	}
	detach()
	if lines := doctor(); len(lines) != 0 {
		t.Fatalf("a detached plugin's drift outlived it: %q", lines)
	}

	detach = attach(map[string]any{"drift": map[string]any{
		"mcpServers": []string{}, "channels": []string{}, "localMemory": []string{},
	}})
	if lines := doctor(); !slices.Equal(lines, []string{"none"}) {
		t.Fatalf("a clean harness printed %q, want drift none", lines)
	}
	detach()

	detach = attach(nil)
	if lines := doctor(); len(lines) != 1 || !strings.HasPrefix(lines[0], "not reported") {
		t.Fatalf("a plugin that predates the report printed %q", lines)
	}
	detach()
}

func TestTheDoctorDescribesTheDrift(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		drift *wstransport.HealthDrift
		want  []string
	}{
		{"an older plugin", nil, []string{"not reported — the plugin predates the report; uffici upgrade brings one that does"}},
		{"clean", &wstransport.HealthDrift{MCPServers: []string{}, Channels: []string{}, LocalMemory: []string{}}, []string{"none"}},
		{"one kind", &wstransport.HealthDrift{MCPServers: []string{"github", "linear"}, Channels: []string{}, LocalMemory: []string{}},
			[]string{"other MCP servers: github, linear — their tool calls never pass through Uffici"}},
		{"a kind the plugin could not read is never clean", &wstransport.HealthDrift{MCPServers: []string{}, Channels: nil, LocalMemory: []string{}},
			[]string{"other channels not reported — the plugin could not read them"}},
		{"a name cannot steer the terminal", &wstransport.HealthDrift{MCPServers: []string{"git\x1b[2Jhub\n"}, Channels: []string{}, LocalMemory: []string{}},
			[]string{"other MCP servers: git[2Jhub — their tool calls never pass through Uffici"}},
		{"nor reorder or disguise it", &wstransport.HealthDrift{MCPServers: []string{"a\u202ebc", "zero\u200bwidth"}, Channels: []string{}, LocalMemory: []string{}},
			[]string{"other MCP servers: abc, zerowidth — their tool calls never pass through Uffici"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := describeDrift(testCase.drift); !slices.Equal(got, testCase.want) {
				t.Fatalf("got  %q\nwant %q", got, testCase.want)
			}
		})
	}
}
