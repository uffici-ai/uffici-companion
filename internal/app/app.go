// Package app wires the companion together and runs it.
//
// It exists so `cmd/uffici/main.go` stays a flag parser: everything that could
// be wrong — configuration, the token, the transport, the fixture — is decided
// here, where it can be tested without a process.
package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/infra/fixture"
	"github.com/uffici-ai/uffici-companion/internal/infra/gateway"
	wstransport "github.com/uffici-ai/uffici-companion/internal/infra/transport/websocket"
	"github.com/uffici-ai/uffici-companion/internal/logship"
	"github.com/uffici-ai/uffici-companion/internal/observe"
	"github.com/uffici-ai/uffici-companion/internal/redact"
	"github.com/uffici-ai/uffici-companion/internal/registry"
	"github.com/uffici-ai/uffici-companion/internal/requests"
	"github.com/uffici-ai/uffici-companion/internal/runner"
	"github.com/uffici-ai/uffici-companion/internal/surface"
	"github.com/uffici-ai/uffici-companion/internal/tools"
	wirepkg "github.com/uffici-ai/uffici-companion/internal/wire"
)

// Exit codes, as the issue names them.
const (
	// ExitOK — every expectation held.
	ExitOK = 0
	// ExitMiss — the run completed and something did not hold.
	ExitMiss = 1
	// ExitConfig — the companion could not start.
	//
	// 🔴 Distinct from ExitMiss on purpose: "your script failed" and "this build
	// never ran your script" are different answers, and a CI job that collapsed
	// them would report a misconfiguration as a product defect.
	ExitConfig = 2
)

// SocketFile is the wire socket's name inside `$UFFICI_HOME`.
const SocketFile = "wire.sock"

// DefaultTCPPort is the loopback port for the pod shape.
const DefaultTCPPort = 7411

// Config is everything the companion needs to run.
type Config struct {
	Home        string
	FixturePath string
	AgentsHost  registry.AgentsHostKind
	Transport   contracts.TransportKind
	SocketPath  string
	TCPAddr     string
	Once        bool
	LogLevel    slog.Level
	// ShipLevel is the threshold at which the companion's own lines are captured
	// for shipping (`UFFICI_LOG_SHIPPING_LEVEL`, default info). Independent of
	// LogLevel: stdout keeps its own threshold.
	ShipLevel slog.Level
	// LogShipping is this machine's switch (`UFFICI_LOG_SHIPPING`, default on):
	// off captures nothing and never asks Uffici for a logs ticket.
	LogShipping bool
	// HarnessLogLevel is this machine's floor for the harness's own lines
	// (`UFFICI_LOG_LEVEL_HARNESS`, default warn). The stricter of it and the
	// agent's level in Uffici wins.
	HarnessLogLevel string
	Version         string
	// AttachWait bounds how long a `--once` run waits for a plugin whose harness
	// is READY. Zero takes the runner's own default; a CI runner that gives a
	// cold container minutes to boot passes a matching value, or the companion
	// gives up first and the failure blames the wrong half.
	AttachWait time.Duration
	// Token is a wire token supplied from outside, for a deployment where the
	// plugin cannot read the companion's file — the pod shape, where the two are
	// separate containers with separate filesystems. Empty means "mint and keep
	// one in $UFFICI_HOME", which is what a laptop does.
	//
	// 🔴 Never logged, never written to disk, never quoted in an error string:
	// it is the only thing stopping another local process from answering this
	// agent's briefs.
	Token string
	// GatewayURL is the agents host the gateway kind connects to
	// (`UFFICI_GATEWAY_URL`): `https://agents.uffici.ai`, or a local stack's origin.
	GatewayURL string
	// CAFile is an extra PEM bundle to trust (`UFFICI_CA_FILE`) — the local
	// stack's dev CA. Go also honours `SSL_CERT_FILE`.
	CAFile string
	// AgentKey is `UFFICI_AGENT_KEY`, the pod shape's Secret mount. The file in
	// $UFFICI_HOME wins when both exist. 🔴 SECRET-CLASS.
	AgentKey string
}

// Env is the environment, injected so tests need no process state.
type Env func(string) string

// LoadConfig builds a Config from the environment and already-parsed flags.
func LoadConfig(env Env, flags Config) (Config, error) {
	config := flags

	if config.Home == "" {
		config.Home = env("UFFICI_HOME")
	}
	if config.Home == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return config, fmt.Errorf("no --home and no home directory: %w", err)
		}
		config.Home = filepath.Join(home, ".uffici")
	}

	kind, err := registry.ParseAgentsHostKind(env("UFFICI_AGENTS_HOST"))
	if err != nil {
		return config, err
	}
	config.AgentsHost = kind
	if kind == registry.KindFixture {
		// The flags win; the environment is how a container whose command is
		// fixed selects a script — the pod shape's CI run (#2078).
		if config.FixturePath == "" {
			config.FixturePath = strings.TrimSpace(env("UFFICI_FIXTURE"))
		}
		if !config.Once {
			config.Once = strings.TrimSpace(env("UFFICI_ONCE")) == "1"
		}
	}
	if kind == registry.KindGateway {
		if err := loadGatewayConfig(env, &config); err != nil {
			return config, err
		}
	}

	if config.Token == "" {
		config.Token = strings.TrimSpace(env("UFFICI_WIRE_TOKEN"))
	}

	if config.Transport == "" {
		switch strings.TrimSpace(env("UFFICI_WIRE_TRANSPORT")) {
		case string(contracts.TransportTCP):
			config.Transport = contracts.TransportTCP
		case "", string(contracts.TransportSocket):
			config.Transport = contracts.TransportSocket
		default:
			return config, fmt.Errorf(
				"UFFICI_WIRE_TRANSPORT is %q; this build serves %q and %q",
				env("UFFICI_WIRE_TRANSPORT"), contracts.TransportSocket, contracts.TransportTCP)
		}
	}
	if config.SocketPath == "" {
		config.SocketPath = filepath.Join(config.Home, SocketFile)
	}
	if config.TCPAddr == "" {
		port := DefaultTCPPort
		if raw := strings.TrimSpace(env("UFFICI_COMPANION_PORT")); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed <= 0 || parsed > 65535 {
				return config, fmt.Errorf("UFFICI_COMPANION_PORT is %q, which is not a port", raw)
			}
			port = parsed
		}
		// 🔴 Loopback, always. A wildcard bind would put the wire — whose only
		// authentication is a local token — on every interface of a pod.
		config.TCPAddr = fmt.Sprintf("127.0.0.1:%d", port)
	}

	level, err := parseLevel("UFFICI_LOG_LEVEL", env("UFFICI_LOG_LEVEL"))
	if err != nil {
		return config, err
	}
	config.LogLevel = level
	shipLevel, err := parseLevel("UFFICI_LOG_SHIPPING_LEVEL", env("UFFICI_LOG_SHIPPING_LEVEL"))
	if err != nil {
		return config, err
	}
	config.ShipLevel = shipLevel
	switch strings.ToLower(strings.TrimSpace(env("UFFICI_LOG_SHIPPING"))) {
	case "", "on":
		config.LogShipping = true
	case "off":
		config.LogShipping = false
	default:
		return config, fmt.Errorf("UFFICI_LOG_SHIPPING is %q; use on or off", env("UFFICI_LOG_SHIPPING"))
	}
	switch value := strings.ToLower(strings.TrimSpace(env("UFFICI_LOG_LEVEL_HARNESS"))); value {
	case "":
		config.HarnessLogLevel = "warn"
	case "debug", "info", "warn", "error":
		config.HarnessLogLevel = value
	default:
		return config, fmt.Errorf("UFFICI_LOG_LEVEL_HARNESS is %q; use debug, info, warn or error", env("UFFICI_LOG_LEVEL_HARNESS"))
	}
	return config, nil
}

// loadGatewayConfig reads what only the gateway kind needs, and refuses what it
// cannot use.
func loadGatewayConfig(env Env, config *Config) error {
	if config.Once {
		return fmt.Errorf("--once plays a fixture script to a verdict; it has no meaning against the gateway")
	}
	raw := strings.TrimSpace(env("UFFICI_GATEWAY_URL"))
	if raw == "" {
		return fmt.Errorf("UFFICI_AGENTS_HOST=gateway needs UFFICI_GATEWAY_URL, like https://agents.uffici.ai — or run uffici setup, which records it for this home")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" {
		// 🔴 https only, and the URL is echoed so the operator sees what was read —
		// except credentials in it, which are never echoed.
		shown := raw
		if err == nil && parsed.User != nil {
			parsed.User = url.User("REDACTED")
			shown = parsed.String()
		}
		return fmt.Errorf("UFFICI_GATEWAY_URL is %q; it must be the agents host's https origin, like https://agents.uffici.ai", shown)
	}
	config.GatewayURL = parsed.Scheme + "://" + parsed.Host
	config.CAFile = strings.TrimSpace(env("UFFICI_CA_FILE"))
	config.AgentKey = strings.TrimSpace(env("UFFICI_AGENT_KEY"))
	return nil
}

func parseLevel(name, value string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("%s is %q; use debug, info, warn or error", name, value)
	}
}

// Run starts the companion and returns its exit code.
func Run(ctx context.Context, config Config, out io.Writer) (int, error) {
	token, err := EnsureToken(config.Home, config.Token)
	if err != nil {
		return ExitConfig, err
	}
	logging := newLogging(config, out, token)
	logger := logging.logger

	// The fixture host is built here rather than through the registry when a
	// script is in play, because the runner needs the concrete host to drive it.
	var (
		host        contracts.AgentsHost
		fixtureHost *fixture.Host
		script      *fixture.Script
		// wire is filled in below; the gateway host reads the attached
		// harness's facts through it at each connection.
		wire *surface.Surface
	)
	switch config.AgentsHost {
	case registry.KindGateway:
		host, err = buildGatewayHost(config, logger, func() (string, string) {
			if wire == nil {
				return "", ""
			}
			return wire.HarnessFacts()
		}, func() string {
			if wire == nil {
				return ""
			}
			return wire.PluginVersion()
		})
		if err != nil {
			return ExitConfig, err
		}
		defer func() { _ = host.Close() }()
	default:
		script, fixtureHost, err = loadFixture(config.FixturePath)
		if err != nil {
			return ExitConfig, err
		}
		host = fixtureHost
	}

	// The runner is built before the surface exists, so its plugin control is
	// filled in once both do — the two refer to each other by construction.
	control := &daemonPlugin{}
	var scriptRunner *runner.Runner
	observer := observe.Logging(logger, nil)
	if script != nil {
		scriptRunner, err = runner.New(script, fixtureHost, control)
		if err != nil {
			return ExitConfig, err
		}
		if config.AttachWait > 0 {
			scriptRunner.SetAttachWait(config.AttachWait)
		}
		scriptRunner.SetAnnounce(func(line string) { logger.Info(line) })
		// The script's expectations and the operational log both watch the
		// state machine, which takes one observer.
		observer = observe.Fanout(scriptRunner.Observer(), observer)
	}
	manager := requests.New(host, requests.Options{Observer: observer})
	control.manager = manager
	// Filled in below, once the surface exists; /healthz reads it through
	// logging.shipper, which is atomic, never through this variable.
	var shipper *logship.Shipper

	// The agent's Uffici tools (#2349), over whichever host is running. Built
	// before the listener: /healthz reports the last catalog's size.
	toolCalls := &tools.Handler{Host: host, Logger: logger}

	listener, err := wstransport.Listen(wstransport.Options{
		Kind: config.Transport, SocketPath: config.SocketPath, TCPAddr: config.TCPAddr,
		Health: func() wstransport.Health {
			health := wstransport.Health{
				Status: "ok", AgentsHost: string(config.AgentsHost), PluginAttached: manager.Attached(),
				Version: config.Version,
			}
			if reporter, ok := host.(doorbellReporter); ok {
				health.Doorbell = wstransport.DoorbellReconnecting
				if connected, id := reporter.DoorbellState(); connected {
					health.Doorbell, health.ConnectionID = wstransport.DoorbellConnected, id
				}
			}
			// Through the atomic pointer: this closure serves before the shipper exists.
			health.Logs = logsHealth(config, logging.shipper.Load(), logging.ring)
			if listed, ok := toolCalls.CatalogSize(); ok {
				health.Tools = &wstransport.HealthTools{Listed: listed}
			}
			return health
		},
	})
	if err != nil {
		return ExitConfig, err
	}
	defer func() { _ = listener.Close() }()

	// A machine's log lines (#2264): the shipper runs only where the switch is on
	// and the host takes logs; the plugin's `logs.push` is served either way, so
	// a switched-off machine answers `disabled` rather than `unknown_op`.
	shipper = newShipper(config, host, logging, func() contracts.LogClient {
		client := contracts.LogClient{Name: gateway.ClientName, Version: config.Version}
		if wire != nil {
			client.HarnessKind, client.HarnessVersion = wire.HarnessFacts()
			client.PluginVersion = wire.PluginVersion()
		}
		return client
	})
	pushes := &logship.PushHandler{Shipper: shipper, Ring: logging.ring, Redactor: logging.redactor}
	wire, err = surface.New(listener, manager, surface.Options{
		Token: token, Logger: logger, Version: config.Version,
		Calls: map[string]surface.CallHandler{
			wireOpLogsPush:      pushes.Handle,
			wirepkg.OpToolsList: toolCalls.List,
			wirepkg.OpToolsCall: toolCalls.Call,
		},
	})
	if err != nil {
		return ExitConfig, err
	}
	control.surface = wire
	control.transport = wstransport.Options{
		Kind: config.Transport, SocketPath: config.SocketPath, TCPAddr: config.TCPAddr,
	}

	serveCtx, stopServing := context.WithCancel(ctx)
	defer stopServing()
	// 🔴 Registered AFTER stopServing and the listener's close, so it runs BEFORE
	// both: the plugin is told why the wire is going away while the wire is still
	// there to say it on. The reverse order buys nothing but a bare EOF.
	defer wire.Shutdown()
	go func() { _ = wire.Serve(serveCtx) }()

	claim, err := host.OpenDoorbell(serveCtx, contracts.Handlers{
		OnRing: func(ring contracts.Ring) {
			// 🔴 Acknowledged on RECEIPT, before the harness has seen anything:
			// the ack means "this daemon has it", not "the agent answered".
			if err := host.Ack(serveCtx, ring.RingID); err != nil {
				logger.Warn("acknowledging a ring", slog.String("request", ring.RequestID),
					slog.String("error", classifyTickError(err)))
			}
			if err := manager.OnRing(serveCtx, ring); err != nil {
				logger.Error("handling a ring", slog.String("request", ring.RequestID))
			}
		},
		OnCancel: func(requestID string, reason contracts.CancelReason) {
			if err := manager.OnPlatformCancel(serveCtx, requestID, reason); err != nil {
				logger.Error("handling a cancel", slog.String("request", requestID))
			}
		},
		OnRelease: func(conversationID string, reason contracts.ReleaseReason) {
			logger.Info("releasing a conversation",
				slog.String("conversation", conversationID), slog.String("reason", string(reason)))
			if err := manager.OnPlatformRelease(serveCtx, conversationID, reason); err != nil {
				logger.Warn("passing a release to the plugin", slog.String("conversation", conversationID))
			}
		},
		OnDisconnect: func(reason string) {
			logger.Warn("the agents host disconnected", slog.String("reason", reason))
		},
	})
	if err != nil {
		return ExitConfig, err
	}
	defer func() { _ = claim.Close() }()

	logger.Info("companion listening",
		slog.String("transport", string(config.Transport)),
		slog.String("address", listener.Addr()),
		slog.String("agentsHost", string(config.AgentsHost)),
		slog.String("version", config.Version),
	)

	// The deadline and ack-timeout sweep. Cheap, and the only thing that turns a
	// passed deadline into an expiry.
	go tick(serveCtx, manager, logger)

	// The shipper stops with the serve context and makes one flush of at most
	// two seconds on its way out; waiting for it keeps that flush inside Run.
	shipCtx, stopShipping := context.WithCancel(serveCtx)
	defer stopShipping()
	shipperDone := make(chan struct{})
	if shipper != nil {
		go func() {
			defer close(shipperDone)
			shipper.Run(shipCtx)
		}()
	} else {
		close(shipperDone)
	}
	defer func() { <-shipperDone }()
	defer stopServing()

	if !config.Once {
		<-serveCtx.Done()
		return ExitOK, nil
	}

	if scriptRunner == nil {
		// Unreachable: LoadConfig refuses --once without a fixture script.
		return ExitConfig, fmt.Errorf("--once needs a fixture script")
	}
	result, err := scriptRunner.Run(serveCtx)
	if err != nil && !errors.Is(err, context.Canceled) {
		return ExitConfig, err
	}
	report(logger, result)
	// The lines this run shipped (#2267). Canaries first — the wire token through
	// every source's own capture path, so "no line carries the token" is a claim
	// about redaction and not about a run that never tried — then the shipper is
	// STOPPED and its shutdown flush drains the ring, so nothing races the count.
	injectRedactionCanaries(serveCtx, logging, pushes, token)
	stopShipping()
	<-shipperDone
	reportLogs(logger, logReportOf(fixtureHost.Batches(), token, result.Restarts))
	if !result.OK() {
		return ExitMiss, nil
	}
	return ExitOK, nil
}

// wireOpLogsPush is the local wire's log op, named once here for the call map.
const wireOpLogsPush = wirepkg.OpLogsPush

// logging is the companion's logger and what ships its lines.
type logging struct {
	logger   *slog.Logger
	ring     *logship.Ring
	redactor *redact.Redactor
	// tee is the ship side alone — a logger over it writes nothing to stdout.
	tee *logship.Tee
	// shipper is set once the host is known; the tee reads it on every record,
	// so a switch-off in Uffici stops capture without rebuilding the logger.
	shipper atomic.Pointer[logship.Shipper]
}

// newLogging builds the companion's logger: stdout exactly as before, teed into
// the ship ring at the shipping threshold. The redactor knows this process's
// wire token and, for the gateway kind, its agent key, so neither can ship even
// if a line quotes it verbatim.
//
// 🔴 With the machine switch off the tee captures nothing, ever.
func newLogging(config Config, out io.Writer, token string) *logging {
	stdout := slog.NewJSONHandler(out, &slog.HandlerOptions{Level: config.LogLevel})
	secrets := []string{token}
	if config.AgentsHost == registry.KindGateway {
		if key, err := LoadAgentKey(config.Home, config.AgentKey); err == nil {
			secrets = append(secrets, key)
		}
	}
	l := &logging{
		ring: logship.NewRing(logship.DefaultMaxEntries, logship.DefaultMaxBytes, func() string {
			return logship.FormatTS(time.Now())
		}),
		redactor: redact.New(secrets...),
	}
	tee := logship.NewTee(logship.TeeOptions{
		Ring: l.ring, Redactor: l.redactor, Level: config.ShipLevel,
		Enabled: func() bool {
			if !config.LogShipping {
				return false
			}
			if shipper := l.shipper.Load(); shipper != nil {
				return shipper.Capturing()
			}
			return true
		},
	})
	l.tee = tee
	l.logger = slog.New(slog.NewMultiHandler(stdout, tee))
	return l
}

// newShipper builds the log shipper, or nil — and nil means nothing ships and
// nothing is minted: 🔴 the machine switch is off, or the host takes no logs.
func newShipper(config Config, host contracts.AgentsHost, logging *logging, client func() contracts.LogClient) *logship.Shipper {
	logHost, ok := host.(contracts.LogHost)
	if !ok || !config.LogShipping {
		return nil
	}
	activity, _ := host.(contracts.ControlPlaneActivity)
	shipper := logship.NewShipper(logship.ShipperOptions{
		Host: logHost, Activity: activity, Ring: logging.ring,
		MachineHarnessLevel: config.HarnessLogLevel, PluginLevel: levelName(config.ShipLevel),
		Logger: logging.logger, Client: client,
	})
	logging.shipper.Store(shipper)
	return shipper
}

// logsHealth is the shipper's state as `/healthz` reports it.
func logsHealth(config Config, shipper *logship.Shipper, ring *logship.Ring) *wstransport.HealthLogs {
	logs := &wstransport.HealthLogs{Machine: "off", Uffici: "unknown", HarnessFloor: config.HarnessLogLevel,
		Dropped: ring.Dropped()}
	if !config.LogShipping || shipper == nil {
		return logs
	}
	logs.Machine = "on"
	logs.Uffici = "on"
	if !shipper.Capturing() {
		logs.Uffici = "off"
	}
	logs.HarnessFloor, _ = shipper.Floors()
	logs.Dropped += shipper.Dropped()
	return logs
}

// levelName is a slog level as the platform names it.
func levelName(level slog.Level) string { return logship.LevelName(level) }

// daemonPlugin is the runner's control over the attached plugin, implemented
// against the real surface and transport.
//
// 🔴 It exists so `disconnectPlugin` and `secondConnection` exercise the DAEMON
// rather than a stand-in. Both steps assert behaviour that only the real
// surface has: dropping a live connection, and refusing a second one before a
// token is read.
type daemonPlugin struct {
	surface   *surface.Surface
	transport wstransport.Options
	manager   *requests.Manager
}

// 🔴 Each method checks its field. They are filled in after construction, and
// today every write and the only reader are on Run's own goroutine — but that
// ordering is invisible and unenforced, and a `/healthz` hook or a Run moved
// into a goroutine would turn it into a nil dereference that kills the daemon.
// A refusal that names what is missing costs three lines.
func (d *daemonPlugin) Disconnect() error {
	if d.surface == nil {
		return fmt.Errorf("plugin control: the surface is not built yet")
	}
	return d.surface.DropAttached()
}

func (d *daemonPlugin) DialSecond(ctx context.Context) (string, error) {
	if d.transport.Kind == "" && d.transport.SocketPath == "" && d.transport.TCPAddr == "" {
		return "", fmt.Errorf("plugin control: the transport is not configured yet")
	}
	return wstransport.ProbeClose(ctx, d.transport)
}

func (d *daemonPlugin) Attached() bool { return d.manager != nil && d.manager.Attached() }

// tick is the deadline and ack-timeout sweep — the one autonomous path in the
// companion, which is why its error is LOGGED rather than discarded. A sweep
// that silently fails takes the expiry with it, and expiry is what turns a
// passed deadline into a cancel the platform hears about.
func tick(ctx context.Context, manager *requests.Manager, logger *slog.Logger) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := manager.Tick(ctx); err != nil {
				logger.Error("the expiry sweep failed", slog.String("error", classifyTickError(err)))
			}
		}
	}
}

// classifyTickError reduces a sweep failure to something safe to publish: the
// error can wrap a host error that quotes a URL or a payload ([logging.md]).
// A host's own classified error publishes its class.
func classifyTickError(err error) string {
	var classified contracts.ClassifiedError
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "context_ended"
	case errors.As(err, &classified):
		return classified.ErrorClass()
	default:
		return "sweep_failed"
	}
}

// report logs the verdict — counts and misses, never content.
func report(logger *slog.Logger, result runner.Result) {
	logger.Info("script complete",
		slog.Int("ringsDelivered", result.RingsDelivered),
		slog.Int("briefs", result.Counts.Briefs),
		slog.Int("openers", result.Counts.Openers),
		slog.Int("posts", result.Counts.Posts),
		slog.Int("toolCalls", result.Counts.ToolCalls),
		slog.Int("redeliveries", result.Counts.Redeliveries),
		slog.Int("unmet", len(result.Unmet)),
	)
	for _, miss := range result.Unmet {
		logger.Error("unmet expectation", slog.String("detail", miss))
	}
	if result.RingsDelivered == 0 {
		logger.Error("no ring was delivered — every expectation held over nothing at all")
	}
}

// LogReport is what an --once run shipped, by source — counts only, never a
// line (#2267). TokenLeaks counts shipped lines that still carry this run's wire
// token: the plugin presents it in every hello, so it is the one secret a
// fixture run holds, and any line carrying it is a redaction that failed.
// Canaries counts the SOURCES whose own canary line shipped redacted — a total
// of redactions could be met by ordinary traffic while a canary path was broken,
// and zero leaks would then be a claim about nothing (#2259 review).
// Restarts counts the run's harness restarts, and RestartsWithLastWords those
// whose window holds a shipped harness line at warn or above — the dying
// process's own shutdown warning. A per-source count cannot see it: any harness
// line passes that, while the lines lost at a restart's edges are exactly the
// ones an operator reads the Logs tab for (#2313). The window opens at the ask,
// not at the driver's signal (the companion cannot see that), so a warning the
// old process logs in the driver's poll gap before it signals counts too — a
// bounded blind spot of about a second, not a substitute for the shutdown line.
type LogReport struct {
	Companion, Plugin, Harness, Redactions, TokenLeaks, Canaries int
	Restarts, RestartsWithLastWords                              int
}

// canaryMessage starts every canary line; the token follows it, and redaction
// replaces only the token.
const canaryMessage = "redaction canary "

// logReportOf counts the fixture host's recorded pushes.
func logReportOf(batches []contracts.LogBatch, token string, restarts []runner.Window) LogReport {
	out := LogReport{Restarts: len(restarts)}
	canaries := map[string]bool{}
	spoke := make([]bool, len(restarts))
	for _, batch := range batches {
		for _, entry := range batch.Entries {
			if i := lastWordsOf(entry, restarts); i >= 0 {
				spoke[i] = true
			}
			switch entry.Source {
			case "companion":
				out.Companion++
			case "plugin":
				out.Plugin++
			case "harness":
				out.Harness++
			}
			out.Redactions += entry.Redactions
			leaked := token != "" && (strings.Contains(entry.Message, token) ||
				strings.Contains(entry.Attrs, token) || strings.Contains(entry.Logger, token))
			if leaked {
				out.TokenLeaks++
			}
			if strings.HasPrefix(entry.Message, canaryMessage) && entry.Redactions > 0 && !leaked {
				canaries[entry.Source] = true
			}
		}
	}
	for _, source := range []string{"companion", "plugin", "harness"} {
		if canaries[source] {
			out.Canaries++
		}
	}
	for _, said := range spoke {
		if said {
			out.RestartsWithLastWords++
		}
	}
	return out
}

// lastWordsOf is the index of the restart window a harness warning was logged
// in, or -1. The line's own timestamp decides, never when it was pushed: the
// plugin's final push ships the shutdown's lines after they were logged. TS is
// millisecond-precise (floored), so a window opens ON its first millisecond and
// closes BEFORE its last: a line stamped with the close's own millisecond may
// have been logged just after it, and is never credited to the window.
func lastWordsOf(entry contracts.LogEntry, restarts []runner.Window) int {
	if entry.Source != "harness" || (entry.Level != "warn" && entry.Level != "error") {
		return -1
	}
	at, err := time.Parse(time.RFC3339Nano, entry.TS)
	if err != nil {
		return -1
	}
	for i, window := range restarts {
		if window.To.IsZero() {
			continue
		}
		if !at.Before(window.From.Truncate(time.Millisecond)) && at.Before(window.To.Truncate(time.Millisecond)) {
			return i
		}
	}
	return -1
}

// injectRedactionCanaries hands the capture paths one line per source that
// carries the wire token, in its text and in an attribute: the companion's
// through the tee (never stdout), the plugin's and the harness's through the
// `logs.push` handler, exactly as a plugin would. A fixture run only.
func injectRedactionCanaries(ctx context.Context, l *logging, pushes *logship.PushHandler, token string) {
	slog.New(l.tee).Warn(canaryMessage+token, slog.String("probe", token))
	entry := func(source string) map[string]any {
		return map[string]any{
			"ts": logship.FormatTS(time.Now()), "source": source, "level": "warn",
			"logger": "uffici.canary", "message": canaryMessage + token,
			"attrs": map[string]any{"probe": token},
		}
	}
	payload, err := json.Marshal(map[string]any{"entries": []any{entry("plugin"), entry("harness")}})
	if err == nil {
		_, _ = pushes.Handle(ctx, payload)
	}
}

// reportLogs logs the shipped-lines verdict beside the script's own.
func reportLogs(logger *slog.Logger, r LogReport) {
	logger.Info("logs shipped",
		slog.Int("companion", r.Companion),
		slog.Int("plugin", r.Plugin),
		slog.Int("harness", r.Harness),
		slog.Int("redactions", r.Redactions),
		slog.Int("tokenLeaks", r.TokenLeaks),
		slog.Int("canaries", r.Canaries),
		slog.Int("restarts", r.Restarts),
		slog.Int("restartsWithLastWords", r.RestartsWithLastWords),
	)
}

// buildGatewayHost reads the key and the instance id from $UFFICI_HOME and
// builds the real host through the registry.
func buildGatewayHost(config Config, logger *slog.Logger, harness func() (string, string), plugin func() string) (contracts.AgentsHost, error) {
	key, err := LoadAgentKey(config.Home, config.AgentKey)
	if err != nil {
		return nil, err
	}
	instanceID, err := EnsureInstanceID(config.Home)
	if err != nil {
		return nil, err
	}
	httpClient, err := HTTPClient(config.CAFile)
	if err != nil {
		return nil, err
	}
	// 🔴 A SECOND client, not a copy of the first: HTTPClient clones the default
	// transport per call, so log pushes get a connection pool of their own and
	// never queue behind — or ahead of — a call a person is waiting on.
	logsClient, err := HTTPClient(config.CAFile)
	if err != nil {
		return nil, err
	}
	reg, err := registry.New(registry.Config{AgentsHost: registry.KindGateway, Gateway: gateway.Options{
		BaseURL: config.GatewayURL, Key: key, InstanceID: instanceID, Version: config.Version,
		Harness: harness, Plugin: plugin, HTTPClient: httpClient, LogsHTTPClient: logsClient,
		Logger: logger.With(slog.String("component", "gateway")),
	}})
	if err != nil {
		return nil, err
	}
	logger.Info("connecting to the agents host",
		slog.String("gatewayUrl", config.GatewayURL), slog.String("instanceId", instanceID))
	return reg.AgentsHost(), nil
}

// HTTPClient is the client every platform call and the doorbell's upgrade use:
// the system's trust store, plus `caFile` when one is named.
func HTTPClient(caFile string) (*http.Client, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if caFile != "" {
		pem, readErr := os.ReadFile(caFile) //nolint:gosec // an operator-named CA bundle is the point
		if readErr != nil {
			return nil, fmt.Errorf("reading UFFICI_CA_FILE: %w", readErr)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("UFFICI_CA_FILE at %s holds no PEM certificate", caFile)
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return &http.Client{Transport: transport, Timeout: 30 * time.Second}, nil
}

func loadFixture(path string) (*fixture.Script, *fixture.Host, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil, fmt.Errorf("the fixture agents host needs a script: pass --fixture <path>")
	}
	file, err := os.Open(path) //nolint:gosec // an operator-supplied script path is the point of the flag
	if err != nil {
		return nil, nil, fmt.Errorf("opening the fixture script: %w", err)
	}
	defer func() { _ = file.Close() }()

	script, err := fixture.Load(file)
	if err != nil {
		return nil, nil, err
	}
	host, err := fixture.New(script)
	if err != nil {
		return nil, nil, err
	}
	return script, host, nil
}
