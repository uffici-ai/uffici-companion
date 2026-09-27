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
	"os/signal"
	"syscall"

	"github.com/uffici-ai/uffici-companion/internal/app"
	"github.com/uffici-ai/uffici-companion/internal/build"
	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/protocol"
	"github.com/uffici-ai/uffici-companion/internal/runner"
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
		_, _ = fmt.Fprintf(stderr, "uffici pair: %v\n", err)
		return app.ExitConfig
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return app.Pair(ctx, resolved, stdout, app.PairOptions{
		Harness:   protocol.HarnessFacts{Kind: *harness, Version: *harnessVersion},
		In:        os.Stdin,
		AssumeYes: *yes,
	})
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
