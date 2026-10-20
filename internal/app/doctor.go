package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/infra/gateway"
	wstransport "github.com/uffici-ai/uffici-companion/internal/infra/transport/websocket"
	"github.com/uffici-ai/uffici-companion/internal/textsafe"
	"github.com/uffici-ai/uffici-companion/internal/transfer"
)

// doctorTimeout bounds the whole probe.
const doctorTimeout = 20 * time.Second

// releaseLineTimeout bounds the doctor's release line on its OWN clock: a slow
// or unreachable gateway must not spend the probe's budget and turn the
// companion's own checks into timeouts.
const releaseLineTimeout = 10 * time.Second

// doctorPoll is how often the doctor re-reads a running companion that has not
// connected yet.
const doctorPoll = 500 * time.Millisecond

// prober is the one thing the doctor asks of the gateway host.
type prober interface {
	Probe(ctx context.Context) gateway.ProbeResult
}

// doorbellReporter is a host that knows whether its doorbell is connected — the
// gateway host; the fixture has no doorbell to report.
type doorbellReporter interface {
	DoorbellState() (connected bool, connectionID string)
}

// DoctorOptions tune Doctor for its caller.
type DoctorOptions struct {
	// AwaitCompanion is how long to wait for a companion to answer on the wire's
	// address before probing alone. `uffici setup` has just (re)started the
	// service, which is not listening yet the instant `systemctl` returns; a
	// probe in that window would open the very second doorbell this avoids.
	AwaitCompanion time.Duration
	// ExpectVersion, when set, is the version the answering companion must
	// report: setup and upgrade have just restarted the service onto a new
	// binary, and an answer from the process being replaced (launchd's bootout
	// is asynchronous) says nothing about the new one.
	ExpectVersion string
	// Release names what the agents gateway names for its machines — the
	// channel and the version (#2382); nil prints no release line.
	Release func(ctx context.Context) string
}

// describeWorkspace is the doctor's workspace line (#2673): where it is, where
// that came from, and whether the companion can write to it. A terminal line
// for the person at the machine — never a log line.
func describeWorkspace(config Config) string {
	root := config.Workspace
	if root == "" {
		root = filepath.Join(config.Home, "workspace")
	}
	from := config.WorkspaceFrom
	if from == "" {
		from = "the default"
	}
	line := root + " (from " + from + ")"
	err := transfer.Writable(root)
	switch {
	case err == nil:
		return line + " — writable"
	case errors.Is(err, os.ErrNotExist):
		return line + " — not created yet; the companion creates it when it starts"
	default:
		return line + " — NOT writable (" + errorKind(err) + "): files cannot be transferred until it is"
	}
}

// Doctor is DoctorWith and no options: what `uffici doctor` runs by hand.
func Doctor(ctx context.Context, config Config, out io.Writer) int {
	return DoctorWith(ctx, config, out, DoctorOptions{})
}

// DoctorWith proves the doorbell from this machine (#2077) and prints what it
// found — never the key, never a ticket — returning the exit code: 0 when the
// doorbell answered, 1 when the platform refused it, 2 when the companion could
// not even try.
//
// 🔴 A companion already running here is ASKED, never probed past (#2257). The
// agents-gateway keeps one doorbell per instance id, and the doctor's own
// probe would present this machine's id — so it would REPLACE the running
// companion's doorbell, flap the agent's presence and log a disconnect on a
// healthy machine, every time setup ended or a person ran the doctor to
// troubleshoot. The running companion reports its own doorbell on the wire's
// `/healthz` instead; only with no companion answering does the doctor open
// one of its own.
//
// A running companion also reports the agent's tools as it last listed them;
// the catalog is read per conversation, so a probe with no conversation has
// none to read.
func DoctorWith(ctx context.Context, config Config, out io.Writer, options DoctorOptions) int {
	say := func(label, value string) { _, _ = fmt.Fprintf(out, "  %-12s %s\n", label, value) }
	_, _ = fmt.Fprintln(out, "uffici doctor")
	say("agents host", config.GatewayURL)
	say("workspace", describeWorkspace(config))

	if options.Release != nil {
		releaseCtx, cancelRelease := context.WithTimeout(ctx, releaseLineTimeout)
		say("release", options.Release(releaseCtx))
		cancelRelease()
	}
	probeCtx, cancel := context.WithTimeout(ctx, doctorTimeout)
	defer cancel()
	if health, running := runningCompanion(probeCtx, config, options); running {
		if options.ExpectVersion != "" && health.Version != options.ExpectVersion {
			say("companion", fmt.Sprintf("running %s, not %s — the service did not restart onto the new binary; restart it",
				NonEmptyVersion(health.Version), options.ExpectVersion))
			return ExitMiss
		}
		return reportRunning(probeCtx, config, health, say)
	}

	host, err := buildGatewayHost(config, slog.New(slog.DiscardHandler), nil, nil)
	if err != nil {
		say("ready", "no — "+err.Error())
		return ExitConfig
	}
	defer func() { _ = host.Close() }()
	instanceID, _ := EnsureInstanceID(config.Home)
	say("instance id", instanceID)
	say("agent key", "present (never shown)")
	say("companion", "not running on this machine — probing the doorbell directly")

	probe, ok := host.(prober)
	if !ok {
		say("doorbell", "this agents host cannot be probed")
		return ExitConfig
	}
	result := probe.Probe(probeCtx)
	if !result.Connected {
		line := "refused: " + result.Reason
		if result.Instruction != "" {
			line += " — " + result.Instruction
		}
		say("doorbell", line)
		return ExitMiss
	}
	say("doorbell", fmt.Sprintf("connected (connection %s, heartbeat %ds), then closed",
		result.ConnectionID, result.HeartbeatSeconds))
	return ExitOK
}

// runningCompanion reads the companion serving this home's wire, waiting up to
// the await for one to start answering — as the expected version, when one is
// expected. The last answer is returned even when its version never matched.
func runningCompanion(ctx context.Context, config Config, options DoctorOptions) (wstransport.Health, bool) {
	deadline := time.Now().Add(options.AwaitCompanion)
	for {
		health, err := wstransport.ReadHealth(ctx, wireOptions(config))
		answered := err == nil
		if answered && (options.ExpectVersion == "" || health.Version == options.ExpectVersion) {
			return health, true
		}
		if (!answered && !errors.Is(err, wstransport.ErrNoCompanion)) || !time.Now().Before(deadline) {
			return health, answered
		}
		select {
		case <-ctx.Done():
			return wstransport.Health{}, false
		case <-time.After(doctorPoll):
		}
	}
}

// reportRunning reports a running companion's own doorbell, re-reading it until
// it connects or the doctor's time runs out: a service restarted a moment ago
// is still dialling.
func reportRunning(ctx context.Context, config Config, health wstransport.Health, say func(label, value string)) int {
	instanceID, _ := EnsureInstanceID(config.Home)
	say("instance id", instanceID)
	say("companion", "running on this machine — asked it, rather than opening a second doorbell")
	if health.Version != "" {
		say("version", health.Version)
	}
	for health.Doorbell == wstransport.DoorbellReconnecting && ctx.Err() == nil {
		select {
		case <-ctx.Done():
		case <-time.After(doctorPoll):
			if again, err := wstransport.ReadHealth(ctx, wireOptions(config)); err == nil {
				health = again
			}
		}
	}
	plugin := "not attached — start the harness"
	if health.PluginAttached {
		plugin = "attached"
	}
	say("plugin", plugin)
	if health.PluginAttached {
		// 🔴 Reported, never refused (#2414): an unsupported harness changes no
		// exit code either.
		say("harness", describeHarness(health.Harness))
		// 🔴 Reported, never refused (#2406): drift changes no exit code.
		for _, line := range describeDrift(health.Drift) {
			say("drift", line)
		}
	}
	if logs := health.Logs; logs != nil {
		say("log lines", describeLogs(*logs))
	}
	switch health.Doorbell {
	case wstransport.DoorbellConnected:
		say("doorbell", fmt.Sprintf("connected (connection %s)", health.ConnectionID))
		say("tools", describeTools(health.Tools))
		return ExitOK
	case wstransport.DoorbellReconnecting:
		say("doorbell", "not connected — the running companion is still trying; its log says why")
		return ExitMiss
	default:
		say("doorbell", "not reported by the running companion — restart it to run this version")
		return ExitMiss
	}
}

// RunningCompanionVersion is the version the companion serving config's wire
// reports, and whether one answered at all (#2250).
func RunningCompanionVersion(ctx context.Context, config Config) (string, bool) {
	health, err := wstransport.ReadHealth(ctx, wireOptions(config))
	if err != nil {
		return "", false
	}
	return health.Version, true
}

// NonEmptyVersion names a version, or says the companion reported none (a
// release from before `/healthz` carried it).
func NonEmptyVersion(version string) string {
	if version == "" {
		return "a version that does not report itself"
	}
	return version
}

// wireOptions is where this home's companion serves the wire.
func wireOptions(config Config) wstransport.Options {
	return wstransport.Options{Kind: config.Transport, SocketPath: config.SocketPath, TCPAddr: config.TCPAddr}
}

// describeLogs is the doctor's one line about log shipping.
func describeLogs(logs wstransport.HealthLogs) string {
	switch {
	case logs.Machine == "off":
		return "not sent — switched off on this machine (uffici setup --log-shipping=on to send them)"
	case logs.Uffici == "off":
		return "not sent — switched off for this agent in Uffici"
	default:
		return fmt.Sprintf("sent to Uffici, redacted; harness lines at %s and above; %d dropped on this machine",
			logs.HarnessFloor, logs.Dropped)
	}
}

// describeTools is the doctor's one line about the agent's Uffici tools.
func describeTools(tools *wstransport.HealthTools) string {
	if tools == nil {
		return "not listed yet — the harness reads the agent's tools in a conversation, on its first turn"
	}
	noun := "tools"
	if tools.Listed == 1 {
		noun = "tool"
	}
	return fmt.Sprintf("%d %s in the most recent conversation's catalog", tools.Listed, noun)
}

// driftKinds are the doctor's words for each kind of drift a plugin reports
// (#2406), in the order it prints them: what the kind is, and what it means.
var driftKinds = []struct {
	label, meaning string
	names          func(*wstransport.HealthDrift) []string
}{
	{"other MCP servers", "their tool calls never pass through Uffici",
		func(d *wstransport.HealthDrift) []string { return d.MCPServers }},
	{"other channels", "this harness also answers there, outside Uffici",
		func(d *wstransport.HealthDrift) []string { return d.Channels }},
	{"local memory still on", "notes no one in the organization can see or delete; uffici setup turns it off",
		func(d *wstransport.HealthDrift) []string { return d.LocalMemory }},
}

// describeDrift is the doctor's lines about the harness's drift from the
// plugin policy (#1994 decision 13): one per kind present, `none` when the
// plugin read every kind and found nothing, and `not reported` when the plugin
// predates the report. A kind the plugin could not read says so rather than
// reading as clean.
func describeDrift(drift *wstransport.HealthDrift) []string {
	if drift == nil {
		return []string{"not reported — the plugin predates the report; uffici upgrade brings one that does"}
	}
	var lines []string
	for _, kind := range driftKinds {
		names := kind.names(drift)
		switch {
		case names == nil:
			lines = append(lines, kind.label+" not reported — the plugin could not read them")
		case len(names) > 0:
			lines = append(lines, fmt.Sprintf("%s: %s — %s", kind.label, printableNames(names), kind.meaning))
		}
	}
	if len(lines) == 0 {
		return []string{"none"}
	}
	return lines
}

// printableNames joins names for a terminal line through textsafe.Visible: a
// name is the harness's configuration, and must not be able to steer or
// disguise the line it is printed on.
func printableNames(names []string) string {
	clean := make([]string, 0, len(names))
	for _, name := range names {
		clean = append(clean, textsafe.Visible(name))
	}
	return strings.Join(clean, ", ")
}

// describeHarness is the doctor's line about the attached harness (#2414): its
// kind and version, and the plugin's word on whether this release supports it.
// The companion never judges a harness version itself — the range and the
// verdict are the plugin's.
func describeHarness(harness *wstransport.HealthHarness) string {
	if harness == nil {
		return "not reported by the running companion — restart it to run this version"
	}
	name := strings.TrimSpace(textsafe.Visible(harness.Kind + " " + harness.Version))
	supports := textsafe.Visible(harness.Range)
	switch {
	case harness.Range == "":
		return name + " — support not reported; the plugin predates the report"
	case harness.Supported == nil:
		return name + " — the plugin could not read the harness's version; it supports " + supports
	case *harness.Supported:
		return name + " — supported by this plugin (" + supports + ")"
	default:
		return name + " — NOT supported by this plugin, which supports " + supports +
			"; it may misbehave without an error. uffici upgrade brings a newer plugin, or run a harness in that range"
	}
}
