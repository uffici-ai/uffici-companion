package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/infra/gateway"
	"github.com/uffici-ai/uffici-companion/internal/infra/gateway/gatewaytest"
	"github.com/uffici-ai/uffici-companion/internal/protocol"
)

// A key shaped like a real one, so a leak of it would be unmistakable.
const pairedKey = "uffici_live_paired_key_0123456789abcdef9e9c"

var codeLine = regexp.MustCompile(`Code:\s+([A-Z0-9]{4}-[A-Z0-9]{4})`)

func pairRig(t *testing.T) (*gatewaytest.Double, Config, *gateway.Pairer) {
	t.Helper()
	double := gatewaytest.New()
	t.Cleanup(double.Close)
	home := filepath.Join(t.TempDir(), "home")
	instanceID, err := EnsureInstanceID(home)
	if err != nil {
		t.Fatal(err)
	}
	pairer, err := gateway.NewPairer(gateway.PairerOptions{
		BaseURL: double.URL(), InstanceID: instanceID, Version: "1.2.3", HTTPClient: double.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return double, Config{Home: home, GatewayURL: double.URL(), Version: "1.2.3"}, pairer
}

// 🔴 The issue's acceptance, against the companion's own gateway double: the
// binary pairs end to end, and the key never appears anywhere but its file.
func TestPairingDeliversTheKeyToItsFileAndNeverPrintsIt(t *testing.T) {
	double, config, pairer := pairRig(t)
	out := &syncBuffer{}
	approved := false
	var asked string
	code := Pair(context.Background(), config, out, PairOptions{
		Harness: protocol.HarnessFacts{Kind: "hermes", Version: "0.21.4"},
		Pairer:  pairer,
		Confirm: func(question string) bool { asked = question; return true },
		// The admin approves while the companion waits: at its first wait.
		Sleep: func(context.Context, time.Duration) error {
			if !approved {
				approved = true
				match := codeLine.FindStringSubmatch(out.String())
				if match == nil {
					t.Fatalf("no code was printed:\n%s", out.String())
				}
				if err := double.ApprovePairing(match[1], "e2a7f46c-aa42-48ec-ae37-a63815f4a700", "acme", "smith", pairedKey); err != nil {
					t.Fatal(err)
				}
			}
			return nil
		},
	})
	if code != ExitOK {
		t.Fatalf("Pair exited %d:\n%s", code, out.String())
	}
	path := filepath.Join(config.Home, AgentKeyFile)
	raw, err := os.ReadFile(path) //nolint:gosec // the test's own temp dir
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(raw)) != pairedKey {
		t.Fatalf("the key file holds %q", raw)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("the key file is mode %04o, want 0600", info.Mode().Perm())
	}
	if loaded, err := LoadAgentKey(config.Home, ""); err != nil || loaded != pairedKey {
		t.Fatalf("the daemon would not read it back: %q, %v", loaded, err)
	}
	printed := out.String()
	if strings.Contains(printed, pairedKey) {
		t.Fatalf("the key reached stdout:\n%s", printed)
	}
	if !strings.Contains(printed, "••••9e9c") {
		t.Fatalf("the last four were not shown:\n%s", printed)
	}
	// 🔴 The person is told WHO the key is for, and asked, before it is written.
	if !strings.Contains(printed, `agent "smith" in organization "acme"`) || asked == "" {
		t.Fatalf("the agent and org were not named before asking:\n%s", printed)
	}
	requests := double.PairingRequests()
	instanceID, _ := EnsureInstanceID(config.Home)
	if len(requests) != 1 || requests[0].InstanceID != instanceID || requests[0].Harness.Kind != "hermes" {
		t.Fatalf("pairing requests = %+v", requests)
	}
	if refusals := double.Refusals(); len(refusals) != 0 {
		t.Fatalf("the double refused: %v", refusals)
	}
	// The files the run left: the instance id and the key — nothing temporary.
	entries, _ := os.ReadDir(config.Home)
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if strings.Join(names, ",") != "agent-key,instance-id" {
		t.Fatalf("the home holds %v", names)
	}
}

func TestACodeThatExpiresUnapprovedWritesNoKey(t *testing.T) {
	_, config, pairer := pairRig(t)
	out := &syncBuffer{}
	later := time.Now().Add(time.Hour)
	code := Pair(context.Background(), config, out, PairOptions{
		Pairer: pairer,
		Now:    func() time.Time { return later },
		Sleep:  func(context.Context, time.Duration) error { return nil },
	})
	if code != ExitMiss {
		t.Fatalf("exit %d, want ExitMiss", code)
	}
	if !strings.Contains(out.String(), "expired before it was approved") {
		t.Fatalf("output:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(config.Home, AgentKeyFile)); !os.IsNotExist(err) {
		t.Fatalf("a key file exists after an expired pairing: %v", err)
	}
}

func TestAPairingTheHostForgotEndsWithItsInstruction(t *testing.T) {
	double, config, pairer := pairRig(t)
	double.PairingTTL = -time.Second // every poll: not_found
	out := &syncBuffer{}
	code := Pair(context.Background(), config, out, PairOptions{
		Pairer: pairer,
		Sleep:  func(context.Context, time.Duration) error { return nil },
	})
	if code != ExitMiss {
		t.Fatalf("exit %d, want ExitMiss:\n%s", code, out.String())
	}
	if _, err := os.Stat(filepath.Join(config.Home, AgentKeyFile)); !os.IsNotExist(err) {
		t.Fatalf("a key file exists: %v", err)
	}
}

func TestWriteAgentKeyReplacesAtomicallyAtMode600(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	if err := WriteAgentKey(home, "  "); err == nil {
		t.Fatal("an empty key was written")
	}
	if err := WriteAgentKey(home, "first_key_value"); err != nil {
		t.Fatal(err)
	}
	if err := WriteAgentKey(home, "second_key_value"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(home, AgentKeyFile)) //nolint:gosec // the test's own temp dir
	if string(raw) != "second_key_value\n" {
		t.Fatalf("the key file holds %q", raw)
	}
	info, _ := os.Stat(filepath.Join(home, AgentKeyFile))
	homeInfo, _ := os.Stat(home)
	if info.Mode().Perm() != 0o600 || homeInfo.Mode().Perm()&0o077 != 0 {
		t.Fatalf("key %04o, home %04o", info.Mode().Perm(), homeInfo.Mode().Perm())
	}
}

// scriptedPairer answers from a script, for the branches the double does not reach.
type scriptedPairer struct {
	openErr error
	polls   []func() (protocol.PairingPollResponse, error)
}

func (s *scriptedPairer) Open(context.Context, protocol.HarnessFacts, string, string) (protocol.PairingResponse, error) {
	if s.openErr != nil {
		return protocol.PairingResponse{}, s.openErr
	}
	return protocol.PairingResponse{
		PairingID: "3a2b1c0d-9e8f-4a7b-8c6d-5e4f3a2b1c0d", Code: "K7QP-2XMD", PollToken: "t",
		ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}, nil
}

func (s *scriptedPairer) Poll(context.Context, string, string) (protocol.PairingPollResponse, error) {
	next := s.polls[0]
	s.polls = s.polls[1:]
	return next()
}

func noSleep(context.Context, time.Duration) error { return nil }

func TestPairsBranchesEachEndTheRunWithoutAKey(t *testing.T) {
	pending := func() (protocol.PairingPollResponse, error) {
		return protocol.PairingPollResponse{Status: protocol.PairingPending, RetryAfterSeconds: 1}, nil
	}
	retryable := func() (protocol.PairingPollResponse, error) {
		return protocol.PairingPollResponse{}, &gateway.Error{Class: "rate_limited", Retryable: true}
	}
	garbage := func() (protocol.PairingPollResponse, error) {
		return protocol.PairingPollResponse{Status: protocol.PairingPaired, KeyLast4: "9e9c", Sealed: &protocol.SealedKey{
			Alg: "x25519-hkdf-sha256-aes-256-gcm", EphemeralPublicKey: "AAAA", Nonce: "AAAA", Ciphertext: "AAAA",
		}}, nil
	}
	refused := &gateway.Error{Class: "rate_limited", Instruction: "The platform is refusing further requests for a moment."}
	for name, tc := range map[string]struct {
		pairer *scriptedPairer
		sleep  func(context.Context, time.Duration) error
		want   string
	}{
		"the open is refused": {&scriptedPairer{openErr: refused}, noSleep, "refusing further requests"},
		"an unclassified open failure": {
			&scriptedPairer{openErr: errors.New("dial")}, noSleep, "could not be reached",
		},
		"a retryable poll, then a key that will not open": {
			&scriptedPairer{polls: []func() (protocol.PairingPollResponse, error){retryable, pending, garbage}}, noSleep,
			"could not be opened",
		},
		"stopped while waiting": {
			&scriptedPairer{}, func(context.Context, time.Duration) error { return context.Canceled }, "stopped before",
		},
	} {
		t.Run(name, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "home")
			out := &syncBuffer{}
			code := Pair(context.Background(), Config{Home: home}, out, PairOptions{Pairer: tc.pairer, Sleep: tc.sleep})
			if code != ExitMiss || !strings.Contains(out.String(), tc.want) {
				t.Fatalf("exit %d, output:\n%s", code, out.String())
			}
			if _, err := os.Stat(filepath.Join(home, AgentKeyFile)); !os.IsNotExist(err) {
				t.Fatalf("a key file exists: %v", err)
			}
		})
	}
}

func TestPairRefusesAConfigItCannotUse(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	out := &syncBuffer{}
	if code := Pair(context.Background(), Config{Home: home, CAFile: filepath.Join(home, "missing.pem")}, out, PairOptions{}); code != ExitConfig {
		t.Fatalf("a missing CA file exited %d", code)
	}
	if code := Pair(context.Background(), Config{Home: home, GatewayURL: "http://agents.example"}, out, PairOptions{}); code != ExitConfig {
		t.Fatalf("a non-https host exited %d", code)
	}
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := Pair(context.Background(), Config{Home: filepath.Join(blocked, "home")}, out, PairOptions{}); code != ExitConfig {
		t.Fatalf("an unusable home exited %d", code)
	}
	if err := WriteAgentKey(filepath.Join(blocked, "home"), "k"); err == nil {
		t.Fatal("a key was written under a file")
	}
}

func TestSleepForWaitsOrStops(t *testing.T) {
	if err := sleepFor(context.Background(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepFor(ctx, time.Hour); err == nil {
		t.Fatal("a stopped wait reported success")
	}
}

// 🔴 #1966 review: a code someone else learned can be approved by an admin of another
// org. The person who ran `uffici pair` sees the agent and org and can refuse.
func TestAPairingThePersonRefusesWritesNothing(t *testing.T) {
	double, config, pairer := pairRig(t)
	out := &syncBuffer{}
	approved := false
	code := Pair(context.Background(), config, out, PairOptions{
		Pairer:  pairer,
		Confirm: func(string) bool { return false },
		Sleep: func(context.Context, time.Duration) error {
			if !approved {
				approved = true
				match := codeLine.FindStringSubmatch(out.String())
				if err := double.ApprovePairing(match[1], "e2a7f46c-aa42-48ec-ae37-a63815f4a700", "evil-corp", "their-agent", pairedKey); err != nil {
					t.Fatal(err)
				}
			}
			return nil
		},
	})
	if code != ExitMiss || !strings.Contains(out.String(), `organization "evil-corp"`) {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	if _, err := os.Stat(filepath.Join(config.Home, AgentKeyFile)); !os.IsNotExist(err) {
		t.Fatalf("a refused key was written: %v", err)
	}
	if strings.Contains(out.String(), pairedKey) {
		t.Fatal("the refused key reached stdout")
	}
}

func TestTheDefaultQuestionReadsYesAndDefaultsToNo(t *testing.T) {
	out := &syncBuffer{}
	if !confirm(PairOptions{In: strings.NewReader("y\n")}, out, "Install? ") || !strings.Contains(out.String(), "Install? ") {
		t.Fatal("a typed yes was not a yes, or the question was not shown")
	}
	for _, in := range []string{"\n", "no\n", "maybe\n"} {
		if confirm(PairOptions{In: strings.NewReader(in)}, out, "Install? ") {
			t.Fatalf("%q was a yes", in)
		}
	}
	if confirm(PairOptions{}, out, "Install? ") {
		t.Fatal("no input was a yes")
	}
}
