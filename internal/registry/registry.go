// Package registry selects implementations by configuration, and is the only
// place in the companion that knows which backend is in play.
//
// This is the platform's registry pattern in Go idiom (`.claude/rules/
// architecture.md`): everything else depends on `internal/contracts`, and a
// test substitutes a slot rather than reaching for a concrete type. There is no
// package-level singleton — the daemon builds one Registry at start-up and
// passes it down, because a global would make two differently-configured
// registries in one test process impossible.
package registry

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/infra/fixture"
	"github.com/uffici-ai/uffici-companion/internal/infra/gateway"
)

// AgentsHostKind names which platform the companion talks to.
type AgentsHostKind string

const (
	// KindFixture plays a scripted platform from a file.
	KindFixture AgentsHostKind = "fixture"
	// KindGateway is the real platform: the agents-gateway at
	// `UFFICI_GATEWAY_URL` (#2077).
	KindGateway AgentsHostKind = "gateway"
)

// Config is everything the registry needs to choose.
type Config struct {
	// AgentsHost is `UFFICI_AGENTS_HOST`; empty means fixture.
	AgentsHost AgentsHostKind
	// FixturePath is the script to play, required for the fixture host.
	FixturePath string
	// Gateway configures the gateway host, required for it.
	Gateway gateway.Options
}

// Registry hands out the selected implementations.
type Registry struct {
	config     Config
	agentsHost contracts.AgentsHost
}

// New builds a registry from configuration, constructing the selected host.
func New(config Config) (*Registry, error) {
	kind := config.AgentsHost
	if kind == "" {
		kind = KindFixture
	}
	switch kind {
	case KindFixture:
		host, err := newFixtureHost(config.FixturePath)
		if err != nil {
			return nil, err
		}
		return &Registry{config: config, agentsHost: host}, nil
	case KindGateway:
		// 🔴 A refusal on any missing piece, never a fallback to the fixture: a
		// companion answering from a script while it is believed to reach the
		// platform looks exactly like one that works.
		host, err := gateway.New(config.Gateway)
		if err != nil {
			return nil, err
		}
		return &Registry{config: config, agentsHost: host}, nil
	default:
		return nil, fmt.Errorf(
			"unknown agents host %q: this build knows %q and %q",
			kind, KindFixture, KindGateway)
	}
}

// NewWithHost builds a registry around an already-constructed host.
//
// For tests and for the runner, which builds the fixture itself so it can drive
// the script. It is the substitution seam the architecture rule asks for.
func NewWithHost(host contracts.AgentsHost) *Registry {
	return &Registry{config: Config{AgentsHost: KindFixture}, agentsHost: host}
}

// AgentsHost is the selected host.
func (r *Registry) AgentsHost() contracts.AgentsHost { return r.agentsHost }

// Kind reports which host is in play, for `/healthz` and the startup log.
func (r *Registry) Kind() AgentsHostKind {
	if r.config.AgentsHost == "" {
		return KindFixture
	}
	return r.config.AgentsHost
}

// Close releases the selected host.
func (r *Registry) Close() error {
	if r.agentsHost == nil {
		return nil
	}
	return r.agentsHost.Close()
}

func newFixtureHost(path string) (contracts.AgentsHost, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("the fixture agents host needs a script: pass --fixture <path>")
	}
	file, err := os.Open(path) //nolint:gosec // an operator-supplied script path is the point of the flag
	if err != nil {
		return nil, fmt.Errorf("opening the fixture script: %w", err)
	}
	defer func() { _ = file.Close() }()
	return newFixtureHostFrom(file)
}

func newFixtureHostFrom(r io.Reader) (contracts.AgentsHost, error) {
	script, err := fixture.Load(r)
	if err != nil {
		return nil, err
	}
	return fixture.New(script)
}

// ParseAgentsHostKind reads the env value, defaulting to the fixture. 🔴 A
// value this build does not know is REFUSED — the companion exits 2 — never
// read as the default: `UFFICI_AGENTS_HOST=gatway` must not quietly play a
// script.
func ParseAgentsHostKind(value string) (AgentsHostKind, error) {
	switch kind := AgentsHostKind(strings.TrimSpace(value)); kind {
	case "":
		return KindFixture, nil
	case KindFixture, KindGateway:
		return kind, nil
	default:
		return "", fmt.Errorf("UFFICI_AGENTS_HOST is %q; this build knows %q and %q", value, KindFixture, KindGateway)
	}
}
