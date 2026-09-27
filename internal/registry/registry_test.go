package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/uffici-ai/uffici-companion/internal/infra/gateway"
	"github.com/uffici-ai/uffici-companion/internal/infra/memory"
)

const validScript = `{
  "v": 1,
  "conversations": {"c": {"id": "conv_1", "offset": "", "entries": [
    {"id":"e1","cursor":"c0001","at":"2026-09-17T14:01:00Z","kind":"message","text":"hi",
     "author":{"type":"member","id":"m1","label":"Ana","self":false}}
  ]}},
  "steps": [{"step":"ring","conversation":"c","reason":"mentioned","causeEntry":"e1","throughCursor":"c0001"}],
  "assert": {"ringsDelivered": 1}
}`

func writeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "script.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing the script: %v", err)
	}
	return path
}

func TestTheFixtureHostIsTheDefault(t *testing.T) {
	reg, err := New(Config{FixturePath: writeScript(t, validScript)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	if reg.Kind() != KindFixture {
		t.Fatalf("kind is %q, want %q", reg.Kind(), KindFixture)
	}
	if reg.AgentsHost() == nil {
		t.Fatal("no agents host was constructed")
	}
}

// 🔴 The gateway host is refused on any missing piece rather than faked: a
// companion that fell back to a script when told to reach the platform would
// look completely healthy while nothing a customer said ever arrived.
func TestTheGatewayHostNeedsItsConfiguration(t *testing.T) {
	reg, err := New(Config{AgentsHost: KindGateway})
	if err == nil || reg != nil {
		t.Fatalf("an unconfigured gateway host was built: %v", err)
	}
	built, err := New(Config{AgentsHost: KindGateway, Gateway: gateway.Options{
		BaseURL: "https://agents.localhost", Key: "k", InstanceID: "d4c3b2a1-f6e5-4d7c-8b9a-0f1e2d3c4b5a",
	}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := built.AgentsHost().(*gateway.Host); !ok || built.Kind() != KindGateway {
		t.Fatalf("the gateway kind built %T", built.AgentsHost())
	}
}

func TestAnUnknownHostNamesWhatThisBuildKnows(t *testing.T) {
	_, err := New(Config{AgentsHost: "postgres"})
	if err == nil {
		t.Fatal("an unknown agents host was accepted")
	}
	if !strings.Contains(err.Error(), "fixture") || !strings.Contains(err.Error(), "gateway") {
		t.Fatalf("the error does not name what this build knows: %v", err)
	}
}

func TestTheFixtureHostNeedsAScript(t *testing.T) {
	if _, err := New(Config{AgentsHost: KindFixture}); err == nil {
		t.Fatal("the fixture host was built with no script path")
	}
	if _, err := New(Config{AgentsHost: KindFixture, FixturePath: "   "}); err == nil {
		t.Fatal("whitespace was accepted as a script path")
	}
	if _, err := New(Config{FixturePath: filepath.Join(t.TempDir(), "absent.json")}); err == nil {
		t.Fatal("a missing script file was accepted")
	}
}

// An invalid script must fail at construction, not at the first ring: a daemon
// that started and then could not play its script would report healthy.
func TestAnInvalidScriptFailsAtConstruction(t *testing.T) {
	_, err := New(Config{FixturePath: writeScript(t, `{"v": 1, "conversations": {}, "steps": []}`)})
	if err == nil {
		t.Fatal("a script with no conversations was accepted")
	}
}

func TestNewWithHostSubstitutes(t *testing.T) {
	stub := memory.New()
	reg := NewWithHost(stub)
	if reg.AgentsHost() != stub {
		t.Fatal("the substituted host was not returned")
	}
	if err := reg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Closing an empty registry must be safe: shutdown runs on paths that never
	// finished building one.
	if err := (&Registry{}).Close(); err != nil {
		t.Fatalf("closing an empty registry: %v", err)
	}
}

func TestParseAgentsHostKindDefaultsToFixture(t *testing.T) {
	for value, want := range map[string]AgentsHostKind{
		"":         KindFixture,
		"   ":      KindFixture,
		"fixture":  KindFixture,
		" gateway": KindGateway,
	} {
		if got, err := ParseAgentsHostKind(value); err != nil || got != want {
			t.Errorf("ParseAgentsHostKind(%q) = %q, %v; want %q", value, got, err, want)
		}
	}
	// 🔴 A typo is refused, never read as the fixture.
	for _, value := range []string{"nonsense", "gatway", "agents.uffici.dev"} {
		if _, err := ParseAgentsHostKind(value); err == nil {
			t.Errorf("ParseAgentsHostKind(%q) was accepted", value)
		}
	}
}

// `/healthz` and the startup log report the kind, so an empty value would read
// as "no agents host" on a companion that has one.
func TestKindReportsTheSelectedHost(t *testing.T) {
	reg, err := New(Config{FixturePath: writeScript(t, validScript)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	if reg.Kind() != KindFixture {
		t.Fatalf("Kind() is %q, want %q", reg.Kind(), KindFixture)
	}
	explicit := &Registry{config: Config{AgentsHost: KindGateway}}
	if explicit.Kind() != KindGateway {
		t.Fatalf("Kind() is %q, want %q", explicit.Kind(), KindGateway)
	}
}
