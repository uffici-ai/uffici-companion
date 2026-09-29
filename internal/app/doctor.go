package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/infra/gateway"
)

// doctorTimeout bounds the whole probe.
const doctorTimeout = 20 * time.Second

// prober is the one thing the doctor asks of the gateway host.
type prober interface {
	Probe(ctx context.Context) gateway.ProbeResult
}

// Doctor proves the doorbell from this machine (#2077): the key is readable,
// a connection ticket mints, the doorbell opens and says hello, and it closes.
// It prints what it found — never the key, never a ticket — and returns the
// exit code: 0 when the doorbell answered, 1 when the platform refused it, 2
// when the companion could not even try.
//
// The MCP list and a tool call join this probe in stage 3; the drift checks the
// README names arrive with the plugins that can report them.
func Doctor(ctx context.Context, config Config, out io.Writer) int {
	say := func(label, value string) { _, _ = fmt.Fprintf(out, "  %-12s %s\n", label, value) }
	_, _ = fmt.Fprintln(out, "uffici doctor")
	say("agents host", config.GatewayURL)

	host, err := buildGatewayHost(config, slog.New(slog.DiscardHandler), nil, nil)
	if err != nil {
		say("ready", "no — "+err.Error())
		return ExitConfig
	}
	defer func() { _ = host.Close() }()
	instanceID, _ := EnsureInstanceID(config.Home)
	say("instance id", instanceID)
	say("agent key", "present (never shown)")

	probe, ok := host.(prober)
	if !ok {
		say("doorbell", "this agents host cannot be probed")
		return ExitConfig
	}
	probeCtx, cancel := context.WithTimeout(ctx, doctorTimeout)
	defer cancel()
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
