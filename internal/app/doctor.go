package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/infra/gateway"
	wstransport "github.com/uffici-ai/uffici-companion/internal/infra/transport/websocket"
)

// doctorTimeout bounds the whole probe.
const doctorTimeout = 20 * time.Second

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
// The MCP list and a tool call join this probe in stage 3; the drift checks the
// README names arrive with the plugins that can report them.
func DoctorWith(ctx context.Context, config Config, out io.Writer, options DoctorOptions) int {
	say := func(label, value string) { _, _ = fmt.Fprintf(out, "  %-12s %s\n", label, value) }
	_, _ = fmt.Fprintln(out, "uffici doctor")
	say("agents host", config.GatewayURL)

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
	switch health.Doorbell {
	case wstransport.DoorbellConnected:
		say("doorbell", fmt.Sprintf("connected (connection %s)", health.ConnectionID))
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
