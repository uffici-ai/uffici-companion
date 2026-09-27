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
	"time"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/infra/fixture"
	"github.com/uffici-ai/uffici-companion/internal/infra/gateway"
	wstransport "github.com/uffici-ai/uffici-companion/internal/infra/transport/websocket"
	"github.com/uffici-ai/uffici-companion/internal/observe"
	"github.com/uffici-ai/uffici-companion/internal/registry"
	"github.com/uffici-ai/uffici-companion/internal/requests"
	"github.com/uffici-ai/uffici-companion/internal/runner"
	"github.com/uffici-ai/uffici-companion/internal/surface"
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
	Version     string
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

	level, err := parseLevel(env("UFFICI_LOG_LEVEL"))
	if err != nil {
		return config, err
	}
	config.LogLevel = level
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
		return fmt.Errorf("UFFICI_AGENTS_HOST=gateway needs UFFICI_GATEWAY_URL, like https://agents.uffici.ai")
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

func parseLevel(value string) (slog.Level, error) {
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
		return slog.LevelInfo, fmt.Errorf("UFFICI_LOG_LEVEL is %q; use debug, info, warn or error", value)
	}
}

// Run starts the companion and returns its exit code.
func Run(ctx context.Context, config Config, out io.Writer) (int, error) {
	logger := slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{Level: config.LogLevel}))

	token, err := EnsureToken(config.Home, config.Token)
	if err != nil {
		return ExitConfig, err
	}

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
		// The script's expectations and the operational log both watch the
		// state machine, which takes one observer.
		observer = observe.Fanout(scriptRunner.Observer(), observer)
	}
	manager := requests.New(host, requests.Options{Observer: observer})
	control.manager = manager

	listener, err := wstransport.Listen(wstransport.Options{
		Kind: config.Transport, SocketPath: config.SocketPath, TCPAddr: config.TCPAddr,
		Health: func() wstransport.Health {
			return wstransport.Health{
				Status: "ok", AgentsHost: string(config.AgentsHost), PluginAttached: manager.Attached(),
			}
		},
	})
	if err != nil {
		return ExitConfig, err
	}
	defer func() { _ = listener.Close() }()

	wire, err = surface.New(listener, manager, surface.Options{
		Token: token, Logger: logger, Version: config.Version,
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
	if !result.OK() {
		return ExitMiss, nil
	}
	return ExitOK, nil
}

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

// buildGatewayHost reads the key and the instance id from $UFFICI_HOME and
// builds the real host through the registry.
func buildGatewayHost(config Config, logger *slog.Logger, harness func() (string, string)) (contracts.AgentsHost, error) {
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
	reg, err := registry.New(registry.Config{AgentsHost: registry.KindGateway, Gateway: gateway.Options{
		BaseURL: config.GatewayURL, Key: key, InstanceID: instanceID, Version: config.Version,
		Harness: harness, HTTPClient: httpClient, Logger: logger.With(slog.String("component", "gateway")),
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
