// Command uffici is the companion: the one program a customer runs next to
// their AI-agent harness to connect it to Uffici.
//
// It is deliberately thin. Everything that can be wrong — configuration, the
// token, the transport, the fixture — is decided in `internal/app`, where it is
// tested without starting a process.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/app"
	"github.com/uffici-ai/uffici-companion/internal/build"
	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/protocol"
	"github.com/uffici-ai/uffici-companion/internal/runner"
	"github.com/uffici-ai/uffici-companion/internal/setup"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run takes writers rather than files so the flag surface and every exit
// code are testable without starting a process.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "doctor" {
		return doctor(args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "pair" {
		return pair(args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "setup" {
		return setupCommand(args[1:], stdout, stderr)
	}
	if len(args) > 0 && args[0] == "upgrade" {
		return upgradeCommand(args[1:], stdout, stderr)
	}
	flags := flag.NewFlagSet("uffici", flag.ContinueOnError)
	flags.SetOutput(stderr)
	showVersion := flags.Bool("version", false, "print the version and exit")
	fixturePath := flags.String("fixture", "", "path to a fixture script")
	once := flags.Bool("once", false, "play the fixture script, then exit with its verdict")
	socket := flags.String("socket", "", "serve the wire on this Unix socket")
	tcp := flags.String("tcp", "", "serve the wire on this loopback address")
	home := flags.String("home", "", "the companion's home directory (default $UFFICI_HOME or ~/.uffici)")
	// 🔴 Settable because the default is a LAPTOP's number. A `--once` run waits
	// this long for a plugin whose harness can answer, and since the `ready`
	// frame that means waiting for a whole harness to boot — which in a cold
	// container is longer than 15s. A CI runner that gives the harness four
	// minutes needs to say so here too, or the companion gives up first and the
	// failure reads as "the harness never attached" when it was simply slow.
	attachWait := flags.Duration("attach-wait", runner.AttachWait,
		"how long --once waits for a plugin whose harness is ready")

	// `uffici run --fixture …` — the verb is optional so both spellings work.
	if len(args) > 0 && args[0] == "run" {
		args = args[1:]
	}
	if err := flags.Parse(args); err != nil {
		return app.ExitConfig
	}

	if *showVersion {
		_, _ = fmt.Fprintf(stdout, "%s %s\n", build.Name, build.Version())
		return app.ExitOK
	}

	config := app.Config{
		Home: *home, FixturePath: *fixturePath, Once: *once,
		SocketPath: *socket, TCPAddr: *tcp, Version: build.Version(),
		AttachWait: *attachWait,
	}
	// 🔴 An explicit --tcp picks the TCP transport; otherwise the socket is the
	// default. Silently serving loopback TCP when someone asked for a socket
	// would put the wire on an interface they never chose.
	if *tcp != "" {
		config.Transport = contracts.TransportTCP
	} else if *socket != "" {
		config.Transport = contracts.TransportSocket
	}

	resolved, err := app.LoadConfig(os.Getenv, config)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "uffici: %v\n", err)
		return app.ExitConfig
	}

	// 🔴 SIGTERM and SIGINT both stop serving, which closes the plugin
	// connection with `shutting_down` rather than dropping it: a plugin that
	// learns why can say so, and one that simply loses its socket cannot.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	code, err := app.Run(ctx, resolved, stdout)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "uffici: %v\n", err)
	}
	return code
}

// pair is `uffici pair` (#1966): pair this machine with an agent — the code an
// admin approves in the portal, and the key delivered straight to $UFFICI_HOME.
// Like doctor, it always talks to the GATEWAY.
func pair(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("uffici pair", flag.ContinueOnError)
	flags.SetOutput(stderr)
	home := flags.String("home", "", "the companion's home directory (default $UFFICI_HOME or ~/.uffici)")
	harness := flags.String("harness", "unknown", "the harness this companion will serve, as the admin sees it (e.g. hermes)")
	harnessVersion := flags.String("harness-version", "unknown", "that harness's version")
	yes := flags.Bool("yes", false, "install the key without asking which agent and org it is for")
	codeFlag := flags.String("code", "", "a pairing code from the portal (XXXX-XXXX) — pairs with no approval and no question")
	if err := flags.Parse(args); err != nil {
		return app.ExitConfig
	}
	code := ""
	if *codeFlag != "" {
		parsed, err := app.ParsePairingCode(*codeFlag)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "uffici pair: --code %v\n", err)
			return app.ExitConfig
		}
		code = parsed
	}
	environment := func(key string) string {
		if key == "UFFICI_AGENTS_HOST" {
			return "gateway"
		}
		return os.Getenv(key)
	}
	resolved, err := app.LoadConfig(environment, app.Config{Home: *home, Version: build.Version()})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "uffici pair: %v\n", err)
		return app.ExitConfig
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return app.Pair(ctx, resolved, stdout, app.PairOptions{
		Harness:   protocol.HarnessFacts{Kind: *harness, Version: *harnessVersion},
		In:        os.Stdin,
		AssumeYes: *yes,
		Code:      code,
	})
}

// pairingNeedsTerminal decides whether `uffici setup` must read pairing's
// confirmation from the terminal: only when it will pair, ask (no --yes, no
// portal-issued code — the admin who issued it chose the agent), actually act
// (not a dry run), and stdin is not already a terminal.
func pairingNeedsTerminal(noPair, yes, dryRun, hasCode, stdinIsTerminal bool) bool {
	return !noPair && !yes && !dryRun && !hasCode && !stdinIsTerminal
}

// doctor is `uffici doctor`: the gateway probe, against the agents host the
// environment names. It always probes the GATEWAY — a fixture has nothing to
// prove — so `UFFICI_AGENTS_HOST` is not consulted.
func doctor(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("uffici doctor", flag.ContinueOnError)
	flags.SetOutput(stderr)
	home := flags.String("home", "", "the companion's home directory (default $UFFICI_HOME or ~/.uffici)")
	if err := flags.Parse(args); err != nil {
		return app.ExitConfig
	}
	environment := func(key string) string {
		if key == "UFFICI_AGENTS_HOST" {
			return "gateway"
		}
		return os.Getenv(key)
	}
	resolved, err := app.LoadConfig(environment, app.Config{Home: *home, Version: build.Version()})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "uffici doctor: %v\n", err)
		return app.ExitConfig
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return app.Doctor(ctx, resolved, stdout)
}

// setupCommand is `uffici setup` (#2182): the part of the install a shell
// script should not do. The installer script fetches the binary and hands
// over here; a person with the binary already can run it directly.
func setupCommand(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("uffici setup", flag.ContinueOnError)
	flags.SetOutput(stderr)
	home := flags.String("home", "", "the companion's home directory (default $UFFICI_HOME or ~/.uffici)")
	harness := flags.String("harness", "", "the harness to install the plugin for (e.g. hermes); empty installs no plugin")
	harnessVersion := flags.String("harness-version", "unknown", "that harness's version, as the admin sees it when approving the pairing")
	harnessHome := flags.String("harness-home", "", "the harness's home directory (default: the manifest's variable, then its default)")
	gatewayURL := flags.String("gateway-url", "", "the agents host's https origin (default $UFFICI_GATEWAY_URL)")
	pluginVersion := flags.String("plugin-version", "", "pin the plugin release (default: the latest)")
	noPair := flags.Bool("no-pair", false, "skip pairing (the key is already in place, or mounted)")
	noService := flags.Bool("no-service", false, "skip installing the companion as a service")
	yes := flags.Bool("yes", false, "install the key without asking which agent and org it is for")
	dryRun := flags.Bool("dry-run", false, "print every action and perform none")
	codeFlag := flags.String("pairing-code", "", "the pairing code in the command the portal gave (XXXX-XXXX); pairs with no question, and again over a key already in place")
	if err := flags.Parse(args); err != nil {
		return setup.ExitConfig
	}
	pairingCode := ""
	if *codeFlag != "" {
		parsed, err := app.ParsePairingCode(*codeFlag)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "uffici setup: --pairing-code %v\n", err)
			return setup.ExitConfig
		}
		pairingCode = parsed
	}
	if flags.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "uffici setup: unexpected argument %q — the harness is a flag: --harness %s\n", flags.Arg(0), flags.Arg(0))
		return setup.ExitConfig
	}
	if *harness != "" && !harnessName.MatchString(*harness) {
		_, _ = fmt.Fprintf(stderr, "uffici setup: %q is not a harness name (lowercase letters, digits and dashes, like hermes)\n", *harness)
		return setup.ExitConfig
	}
	if runtime.GOOS == "windows" {
		_, _ = fmt.Fprintln(stderr, "uffici setup: Windows is not supported; see the README for the platforms that are")
		return setup.ExitConfig
	}
	gateway := *gatewayURL
	if gateway == "" {
		gateway = os.Getenv("UFFICI_GATEWAY_URL")
	}
	// The same environment the service, the pairing and the doctor will see.
	environment := func(key string) string {
		switch key {
		case "UFFICI_AGENTS_HOST":
			return "gateway"
		case "UFFICI_GATEWAY_URL":
			return gateway
		}
		return os.Getenv(key)
	}
	resolved, err := app.LoadConfig(environment, app.Config{Home: *home, Version: build.Version()})
	if err != nil && (!*noPair || !*noService) {
		_, _ = fmt.Fprintf(stderr, "uffici setup: %v\n", err)
		return setup.ExitConfig
	}
	binary, err := os.Executable()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "uffici setup: cannot find this binary's path: %v\n", err)
		return setup.ExitFailed
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "uffici setup: cannot find your home directory: %v\n", err)
		return setup.ExitFailed
	}
	client, err := app.HTTPClient(os.Getenv("UFFICI_CA_FILE"))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "uffici setup: %v\n", err)
		return setup.ExitConfig
	}
	// Pairing asks which agent the key is for. Under `curl | sh` stdin is the
	// pipe the script came down, so the answer is read from the terminal; with
	// no terminal and no --yes, the question cannot be answered and the run is
	// refused before a key is minted for nothing.
	// A code from the portal asks nothing, so it needs no terminal (#2209).
	answers := io.Reader(os.Stdin)
	info, statErr := os.Stdin.Stat()
	stdinIsTerminal := statErr != nil || info.Mode()&os.ModeCharDevice != 0
	if pairingNeedsTerminal(*noPair, *yes, *dryRun, pairingCode != "", stdinIsTerminal) {
		tty, ttyErr := os.Open("/dev/tty")
		if ttyErr != nil {
			_, _ = fmt.Fprintln(stderr, "uffici setup: pairing needs a terminal to confirm which agent the key is for, and this run has none. Re-run with --yes, with --no-pair if the key is already in place, or from a terminal.")
			return setup.ExitConfig
		}
		defer func() { _ = tty.Close() }()
		answers = tty
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return setup.Run(ctx, setup.Options{
		Harness:        *harness,
		HarnessVersion: *harnessVersion,
		HarnessHome:    *harnessHome,
		GatewayURL:     gateway,
		PluginVersion:  *pluginVersion,
		PairingCode:    pairingCode,
		NoPair:         *noPair,
		NoService:      *noService,
		AssumeYes:      *yes,
		DryRun:         *dryRun,
		GOOS:           runtime.GOOS,
		UserHome:       userHome,
		CompanionHome:  resolved.Home,
		Binary:         binary,
		UID:            strconv.Itoa(os.Getuid()),
		Env:            os.Getenv,
		LookPath:       exec.LookPath,
		Runner:         setup.ExecRunner{},
		Release: func(repo string) setup.Release {
			return setup.Release{
				Owner: "uffici-ai", Repo: repo, Version: *pluginVersion,
				// 🔴 Read from the environment, never a flag: a flag's value is
				// in the process list and the shell history. Only needed while a
				// plugin repository is private.
				Token:   os.Getenv("UFFICI_GITHUB_TOKEN"),
				BaseURL: os.Getenv("UFFICI_RELEASES_BASE_URL"),
				APIURL:  os.Getenv("UFFICI_RELEASES_API_URL"),
				Client:  client,
			}
		},
		Pair: func(ctx context.Context) int {
			return app.Pair(ctx, resolved, stdout, app.PairOptions{
				Harness:   protocol.HarnessFacts{Kind: setup.NonEmpty(*harness, "unknown"), Version: *harnessVersion},
				In:        answers,
				AssumeYes: *yes,
				Code:      pairingCode,
			})
		},
		// Setup has just restarted the service it installed: wait for it to
		// answer, so the doctor asks it rather than replacing its doorbell (#2257).
		Doctor: func(ctx context.Context) int {
			options := app.DoctorOptions{}
			if !*noService {
				options.AwaitCompanion = setupAwaitCompanion
				options.ExpectVersion = build.Version()
			}
			return app.DoctorWith(ctx, resolved, stdout, options)
		},
	}, stdout)
}

// upgradeCommand is `uffici upgrade` (#2250): the companion and the attached
// harness's plugin to their latest releases, with nothing to type — the setup
// record names the harness, its home and the agents host.
func upgradeCommand(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("uffici upgrade", flag.ContinueOnError)
	flags.SetOutput(stderr)
	home := flags.String("home", "", "the companion's home directory (default $UFFICI_HOME or ~/.uffici)")
	check := flags.Bool("check", false, "only report installed versus latest: exit 0 when current, 10 when an upgrade exists")
	dryRun := flags.Bool("dry-run", false, "announce every step and perform none")
	version := flags.String("version", "", "pin the companion release (default: the latest); also how a newer build is replaced on purpose")
	pluginVersion := flags.String("plugin-version", "", "pin the plugin release (default: the latest)")
	// Set by the replaced binary when it hands over to the new one: the version
	// it had, for the before → after line. Not for people.
	fromVersion := flags.String("from-version", "", "")
	if err := flags.Parse(args); err != nil {
		return setup.ExitConfig
	}
	if flags.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "uffici upgrade: unexpected argument %q\n", flags.Arg(0))
		return setup.ExitConfig
	}
	if runtime.GOOS == "windows" {
		_, _ = fmt.Fprintln(stderr, "uffici upgrade: Windows is not supported; see the README for the platforms that are")
		return setup.ExitConfig
	}
	// The record names the agents host; the doctor's environment is set from
	// it below, once it has been read.
	resolved, err := app.LoadConfig(func(key string) string {
		if key == "UFFICI_AGENTS_HOST" {
			return "fixture"
		}
		return os.Getenv(key)
	}, app.Config{Home: *home, Version: build.Version()})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "uffici upgrade: %v\n", err)
		return setup.ExitConfig
	}
	binary, err := os.Executable()
	if err == nil {
		binary, err = filepath.EvalSymlinks(binary)
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "uffici upgrade: cannot find this binary's path: %v\n", err)
		return setup.ExitFailed
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "uffici upgrade: cannot find your home directory: %v\n", err)
		return setup.ExitFailed
	}
	client, err := app.HTTPClient(os.Getenv("UFFICI_CA_FILE"))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "uffici upgrade: %v\n", err)
		return setup.ExitConfig
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return setup.Upgrade(ctx, setup.UpgradeOptions{
		Check: *check, DryRun: *dryRun, Version: *version, PluginVersion: *pluginVersion,
		Current: build.Version(), FromVersion: *fromVersion,
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, UserHome: userHome, CompanionHome: resolved.Home,
		Binary: binary, UID: strconv.Itoa(os.Getuid()),
		Env: os.Getenv, LookPath: exec.LookPath, Runner: setup.ExecRunner{},
		Release: func(repo, version string) setup.Release {
			return setup.Release{
				Owner: "uffici-ai", Repo: repo, Version: version,
				// 🔴 Read from the environment, never a flag: a flag's value is in
				// the process list and the shell history.
				Token:   os.Getenv("UFFICI_GITHUB_TOKEN"),
				BaseURL: os.Getenv("UFFICI_RELEASES_BASE_URL"),
				APIURL:  os.Getenv("UFFICI_RELEASES_API_URL"),
				Client:  client,
			}
		},
		// The environment travels as it is (the token with it, never in argv).
		Exec: func(path string, argv []string) error {
			return syscall.Exec(path, append([]string{path}, argv...), os.Environ())
		},
		RunningVersion: func(ctx context.Context) (string, bool) {
			return app.RunningCompanionVersion(ctx, resolved)
		},
		Doctor: func(ctx context.Context) int {
			record, err := setup.ReadRecord(resolved.Home)
			if err != nil || record.GatewayURL == "" {
				// Set up with neither service nor pairing: nothing to prove.
				_, _ = fmt.Fprintln(stdout, "uffici upgrade: no agents host is recorded, so the doctor has nothing to prove")
				return setup.ExitOK
			}
			doctorConfig, err := app.LoadConfig(func(key string) string {
				switch key {
				case "UFFICI_AGENTS_HOST":
					return "gateway"
				case "UFFICI_GATEWAY_URL":
					return record.GatewayURL
				}
				return os.Getenv(key)
			}, app.Config{Home: resolved.Home, Version: build.Version()})
			if err != nil {
				_, _ = fmt.Fprintf(stdout, "uffici upgrade: %v\n", err)
				return setup.ExitConfig
			}
			options := app.DoctorOptions{}
			if record.Service {
				options = app.DoctorOptions{AwaitCompanion: setupAwaitCompanion, ExpectVersion: build.Version()}
			}
			return app.DoctorWith(ctx, doctorConfig, stdout, options)
		},
	}, stdout)
}

// setupAwaitCompanion is how long setup's doctor waits for the service it just
// restarted to answer on the wire.
const setupAwaitCompanion = 10 * time.Second

// harnessName is what --harness may be: it becomes a repository name.
var harnessName = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
