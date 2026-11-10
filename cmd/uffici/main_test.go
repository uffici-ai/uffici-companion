package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/uffici-ai/uffici-companion/internal/app"
	"github.com/uffici-ai/uffici-companion/internal/setup"
)

// The CLI's contract is its exit codes and its refusals. Both are what a CI job
// reads, so both are tested here rather than left to a manual run.

func TestVersionPrintsAndExitsZero(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"--version"}, &out, &errOut); code != 0 {
		t.Fatalf("--version exited %d, want 0", code)
	}
	if !strings.Contains(out.String(), "uffici-companion") {
		t.Fatalf("--version printed %q", out.String())
	}
}

// 🔴 Exit 2, not 1. "This build never ran your script" and "your script failed"
// are different answers, and a CI job that collapsed them would report a
// misconfiguration as a product defect.
func TestAMisconfigurationExitsTwo(t *testing.T) {
	home := t.TempDir()
	for name, args := range map[string][]string{
		"no fixture":       {"run", "--home", home},
		"a missing script": {"run", "--home", home, "--fixture", filepath.Join(home, "absent.json")},
		"an unknown flag":  {"--nonsense"},
	} {
		t.Run(name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := run(args, &out, &errOut); code != 2 {
				t.Fatalf("exited %d, want 2\nstderr: %s", code, errOut.String())
			}
		})
	}
}

// A script that cannot assert anything must be refused at load, not played.
func TestAnInvalidScriptExitsTwo(t *testing.T) {
	home := t.TempDir()
	script := filepath.Join(home, "empty.json")
	if err := os.WriteFile(script, []byte(`{"v":1,"conversations":{},"steps":[]}`), 0o600); err != nil {
		t.Fatalf("writing: %v", err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"run", "--home", home, "--fixture", script, "--once"}, &out, &errOut); code != 2 {
		t.Fatalf("exited %d, want 2\nstderr: %s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "conversations") {
		t.Fatalf("the refusal does not say what was wrong: %s", errOut.String())
	}
}

// The `run` verb is optional; both spellings must reach the same place.
func TestTheRunVerbIsOptional(t *testing.T) {
	var withVerb, withoutVerb bytes.Buffer
	var ignored bytes.Buffer
	withCode := run([]string{"run", "--version"}, &withVerb, &ignored)
	withoutCode := run([]string{"--version"}, &withoutVerb, &ignored)
	if withCode != withoutCode || withVerb.String() != withoutVerb.String() {
		t.Fatalf("`run --version` and `--version` disagree: %d/%q vs %d/%q",
			withCode, withVerb.String(), withoutCode, withoutVerb.String())
	}
}

// `uffici doctor` with nowhere to probe says so and exits 2, never 0.
func TestDoctorWithoutAGatewayURLExitsTwo(t *testing.T) {
	t.Setenv("UFFICI_GATEWAY_URL", "")
	var out, errOut bytes.Buffer
	if code := run([]string{"doctor", "--home", t.TempDir()}, &out, &errOut); code != 2 {
		t.Fatalf("exited %d, want 2\nstderr: %s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "UFFICI_GATEWAY_URL") {
		t.Fatalf("the refusal does not name what is missing: %s", errOut.String())
	}
	if code := run([]string{"doctor", "--nonsense"}, &out, &errOut); code != 2 {
		t.Fatalf("an unknown doctor flag exited %d", code)
	}
}

// `uffici pair` with nowhere to pair against says so and exits 2 (#1966).
func TestPairWithoutAGatewayURLExitsTwo(t *testing.T) {
	t.Setenv("UFFICI_GATEWAY_URL", "")
	var out, errOut bytes.Buffer
	if code := run([]string{"pair", "--home", t.TempDir(), "--harness", "hermes"}, &out, &errOut); code != 2 {
		t.Fatalf("exited %d, want 2\nstderr: %s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "UFFICI_GATEWAY_URL") {
		t.Fatalf("the refusal does not name what is missing: %s", errOut.String())
	}
	if code := run([]string{"pair", "--nonsense"}, &out, &errOut); code != 2 {
		t.Fatalf("an unknown pair flag exited %d", code)
	}
}

func TestSetupRefusesBadFlagsAndNothingToDo(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"setup", "--nonsense"}, &out, &errOut); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	// Nothing asked for: no plugin, no pairing, no service — a request that
	// would do nothing is refused rather than "done".
	if code := run([]string{"setup", "--no-pair", "--no-service", "--home", t.TempDir()}, &out, &errOut); code != 2 {
		t.Fatalf("exit %d, want 2:\n%s%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "nothing to do") {
		t.Fatalf("the refusal does not say why:\n%s", out.String())
	}
	if code := run([]string{"setup", "hermes"}, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "--harness hermes") {
		t.Fatalf("a positional harness must be refused naming the flag: exit %d\n%s", code, errOut.String())
	}
	if code := run([]string{"setup", "--harness", "../x"}, &out, &errOut); code != 2 {
		t.Fatalf("a harness name that is not a repository name must be refused: exit %d", code)
	}
	// The machine's log switches (#2264): a value outside the vocabulary is a
	// configuration error, before anything is done.
	for _, args := range [][]string{
		{"setup", "--log-shipping", "maybe", "--no-pair", "--no-service", "--harness", "hermes"},
		{"setup", "--harness-log-level", "trace", "--no-pair", "--no-service", "--harness", "hermes"},
	} {
		errOut.Reset()
		if code := run(args, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), args[1]) {
			t.Fatalf("%v: exit %d\n%s", args, code, errOut.String())
		}
	}
}

// 🔴 #2382: setup resolves the gateway — --gateway-url, UFFICI_GATEWAY_URL, the
// record of its home, the production default — and names it, with where it came
// from, before it pairs. No harness here, so nothing asks the gateway anything.
func TestSetupResolvesTheGatewayInFourStepsAndNamesIt(t *testing.T) {
	for _, tc := range []struct {
		name, flag, env, recorded, want, from string
	}{
		{name: "the flag", flag: "https://flag.example", env: "https://env.example", recorded: "https://record.example", want: "https://flag.example", from: "from --gateway-url"},
		{name: "the variable", env: "https://env.example", recorded: "https://record.example", want: "https://env.example", from: "from UFFICI_GATEWAY_URL"},
		{name: "the record", recorded: "https://record.example", want: "https://record.example", from: "recorded in setup.json"},
		{name: "the default", want: "https://agents.uffici.ai", from: "the default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("UFFICI_GATEWAY_URL", tc.env)
			home := t.TempDir()
			if tc.recorded != "" {
				if err := setup.WriteRecord(home, setup.Record{GatewayURL: tc.recorded, Service: true}); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{"setup", "--home", home, "--no-service", "--dry-run"}
			if tc.flag != "" {
				args = append(args, "--gateway-url", tc.flag)
			}
			var out, errOut bytes.Buffer
			if code := run(args, &out, &errOut); code != 0 {
				t.Fatalf("exit %d:\n%s%s", code, out.String(), errOut.String())
			}
			named := strings.Index(out.String(), "agents gateway: "+tc.want+" ("+tc.from+")")
			paired := strings.Index(out.String(), "would pair")
			if named < 0 || paired < 0 || named > paired {
				t.Fatalf("the gateway %s (%s) is not named before pairing:\n%s", tc.want, tc.from, out.String())
			}
		})
	}
}

// --version and --plugin-version are release versions, refused otherwise.
func TestSetupRefusesAVersionThatIsNotARelease(t *testing.T) {
	for _, flag := range []string{"--version", "--plugin-version"} {
		var out, errOut bytes.Buffer
		if code := run([]string{"setup", "--home", t.TempDir(), "--harness", "hermes", flag, "newest"}, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), flag) {
			t.Fatalf("%s: exit %d, stderr %q", flag, code, errOut.String())
		}
	}
}

// `uffici run` against the gateway resolves it the same four ways, and names it;
// any other kind is left alone.
func TestRunResolvesTheGatewayLikeSetup(t *testing.T) {
	home := t.TempDir()
	if err := setup.WriteRecord(home, setup.Record{GatewayURL: "https://record.example"}); err != nil {
		t.Fatal(err)
	}
	envOf := func(values map[string]string) func(string) string {
		return func(key string) string { return values[key] }
	}
	for _, tc := range []struct {
		name, flag, home string
		env              map[string]string
		want, from       string
	}{
		{name: "the flag", flag: "https://flag.example", home: home, env: map[string]string{"UFFICI_GATEWAY_URL": "https://env.example"}, want: "https://flag.example", from: "from --gateway-url"},
		{name: "the variable", home: home, env: map[string]string{"UFFICI_AGENTS_HOST": "gateway", "UFFICI_GATEWAY_URL": "https://env.example"}, want: "https://env.example", from: "from UFFICI_GATEWAY_URL"},
		{name: "the record", home: home, env: map[string]string{"UFFICI_AGENTS_HOST": "gateway"}, want: "https://record.example", from: "recorded in setup.json"},
		{name: "the default", home: t.TempDir(), env: map[string]string{"UFFICI_AGENTS_HOST": "gateway"}, want: setup.DefaultGatewayURL, from: "the default"},
	} {
		env, line, err := daemonEnvironment(tc.flag, tc.home, envOf(tc.env))
		if err != nil || env("UFFICI_GATEWAY_URL") != tc.want || env("UFFICI_AGENTS_HOST") != "gateway" {
			t.Fatalf("%s: %q, %v", tc.name, env("UFFICI_GATEWAY_URL"), err)
		}
		if line != "agents gateway: "+tc.want+" ("+tc.from+")" {
			t.Fatalf("%s: line %q", tc.name, line)
		}
	}
	// The fixture is not a gateway: nothing resolved, nothing named.
	env, line, err := daemonEnvironment("", home, envOf(map[string]string{}))
	if err != nil || line != "" || env("UFFICI_GATEWAY_URL") != "" {
		t.Fatalf("a fixture run was given a gateway: %q %q %v", env("UFFICI_GATEWAY_URL"), line, err)
	}
	if _, _, err := daemonEnvironment("https://flag.example", home, envOf(map[string]string{"UFFICI_AGENTS_HOST": "fixture"})); err == nil {
		t.Fatal("--gateway-url with UFFICI_AGENTS_HOST=fixture must be refused")
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"--gateway-url", "https://flag.example", "--home", home}, &out, &errOut); code != 2 {
		// No agent key in this home: the gateway kind refuses to start — after
		// naming the gateway it resolved.
		t.Fatalf("exit %d:\n%s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "agents gateway: https://flag.example (from --gateway-url)") {
		t.Fatalf("the run did not name its gateway:\n%s", errOut.String())
	}
}

func TestSetupDryRunReachesThePluginReleaseAndNamesTheMissingHarnessTool(t *testing.T) {
	// A loopback "release" of the plugin, so the wiring from the command down to
	// the fetch and the manifest is exercised without GitHub. The harness's own
	// tool is not on this machine's PATH, which is the refusal the run ends on.
	manifest := `{"manifestVersion":1,"harness":"hermes","name":"uffici-for-hermes","version":"0.4.0","pluginDir":"plugins/uffici-for-hermes","homeEnv":"HERMES_HOME","defaultHome":"~/.hermes","enable":["definitely-not-a-real-tool-on-this-machine","plugins","enable","uffici-for-hermes"],"env":{},"restart":"restart"}`
	archive := gzippedTar(t, "uffici-for-hermes", map[string]string{"uffici-plugin.json": manifest, "plugin.yaml": "name: x\n"})
	sum := sha256.Sum256(archive)
	assets := map[string][]byte{
		"uffici-for-hermes.tar.gz":    archive,
		"checksums.txt":               []byte(hex.EncodeToString(sum[:]) + "  uffici-for-hermes.tar.gz\n"),
		"checksums.txt.sigstore.json": []byte("a bundle"),
	}
	// The command's own verifier, wired to every download (#3118): a stand-in
	// that records whom it was asked to hold the release to.
	signers := standInVerifier(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if body, ok := assets[name]; ok {
			_, _ = w.Write(body)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	t.Setenv("UFFICI_RELEASES_BASE_URL", server.URL)
	t.Setenv("UFFICI_RELEASES_API_URL", server.URL)
	t.Setenv("UFFICI_GITHUB_TOKEN", "")
	t.Setenv("UFFICI_CA_FILE", "")
	var out, errOut bytes.Buffer
	// The gateway is the loopback server too, which names no release (404).
	code := run([]string{"setup", "--home", t.TempDir(), "--harness", "hermes", "--no-pair", "--no-service", "--dry-run", "--gateway-url", server.URL}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "plugin uffici-for-hermes") || !strings.Contains(out.String(), "is not on the PATH") {
		t.Fatalf("the run did not reach the manifest and name the missing tool:\n%s", out.String())
	}
	if len(*signers) != 1 || (*signers)[0].Issuer != setup.ReleaseIssuer || !strings.Contains((*signers)[0].IdentityPattern, "uffici-for-hermes") {
		t.Fatalf("the plugin's release was not held to its workflow's signature: %+v", *signers)
	}
}

// standInVerifier replaces the public-good verifier for one test, accepting
// every bundle and recording the signer each was held to.
func standInVerifier(t *testing.T) *[]setup.Signer {
	t.Helper()
	var asked []setup.Signer
	previous := releaseVerifier
	releaseVerifier = func(string) setup.SignatureVerifier { return recordingVerifier{&asked} }
	t.Cleanup(func() { releaseVerifier = previous })
	return &asked
}

type recordingVerifier struct{ asked *[]setup.Signer }

func (v recordingVerifier) Verify(_ context.Context, _, _ []byte, want setup.Signer) (string, error) {
	*v.asked = append(*v.asked, want)
	// The identity a real release at v0.4.0 carries: the version setup then
	// holds the plugin's manifest to.
	return setup.ReleaseIdentity("uffici-for-hermes", "0.4.0"), nil
}

// 🔴 The development switch is loud: every run that reads it says so first.
func TestTheDevelopmentIdentitySwitchIsAnnounced(t *testing.T) {
	var out bytes.Buffer
	if _, dev := releaseSignatures("", func(string) string { return "" }, &out); dev != "" || out.Len() != 0 {
		t.Fatalf("no switch, yet %q / %q", dev, out.String())
	}
	_, dev := releaseSignatures(t.TempDir(), func(key string) string {
		if key == setup.DevReleaseIdentityEnv {
			return " ^https://github\\.com/me/ "
		}
		return ""
	}, &out)
	if dev != "^https://github\\.com/me/" || !strings.Contains(out.String(), "WARNING: "+setup.DevReleaseIdentityEnv) {
		t.Fatalf("the switch was not announced: %q / %q", dev, out.String())
	}
}

// gzippedTar builds a release-shaped archive: every file under top/.
func gzippedTar(t *testing.T, top string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: top + "/" + name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

// ── A portal-issued code (#2209) ────────────────────────────────────────────

func TestSetupTakesAPortalCodeIntoThePairingStep(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"setup", "--home", t.TempDir(), "--no-service", "--dry-run", "--gateway-url", "https://agents.example", "--pairing-code", "k7qp-2xmd"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d:\n%s%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "would pair with the code from the portal") {
		t.Fatalf("the code did not reach the pairing step:\n%s", out.String())
	}
	if strings.Contains(strings.ToUpper(out.String()+errOut.String()), "K7QP-2XMD") {
		t.Fatalf("the code was printed:\n%s%s", out.String(), errOut.String())
	}
}

// 🔴 The code can arrive in the environment (#3119), out of argv: setup pairs
// with it, never prints it, and removes it before running anything else.
func TestSetupTakesThePairingCodeFromTheEnvironment(t *testing.T) {
	t.Setenv(PairingCodeEnv, "k7qp-2xmd")
	var out, errOut bytes.Buffer
	code := run([]string{"setup", "--home", t.TempDir(), "--no-service", "--dry-run", "--gateway-url", "https://agents.example"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d:\n%s%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "would pair with the code from the portal") {
		t.Fatalf("the code in the environment did not reach the pairing step:\n%s", out.String())
	}
	if strings.Contains(strings.ToUpper(out.String()+errOut.String()), "K7QP-2XMD") {
		t.Fatalf("the code was printed:\n%s%s", out.String(), errOut.String())
	}
	if _, set := os.LookupEnv(PairingCodeEnv); set {
		t.Fatal("the code stayed in the environment every later command inherits")
	}
}

// 🔴 install.sh decides from `uffici setup -h` whether this setup reads the
// code from the environment (else it hands an older one the flag): the help
// must name the variable.
func TestSetupHelpNamesThePairingCodeVariable(t *testing.T) {
	var out, errOut bytes.Buffer
	_ = run([]string{"setup", "-h"}, &out, &errOut)
	if !strings.Contains(errOut.String(), "$"+PairingCodeEnv) {
		t.Fatalf("setup -h does not name %s:\n%s", PairingCodeEnv, errOut.String())
	}
}

func TestSetupPairingCodeSources(t *testing.T) {
	env := func(value string) func(string) string {
		return func(key string) string {
			if key == PairingCodeEnv {
				return value
			}
			return ""
		}
	}
	var unset []string
	record := func(key string) error { unset = append(unset, key); return nil }
	for _, tc := range []struct{ flag, env, want, refusal string }{
		{want: ""},
		{flag: "k7qp-2xmd", want: "K7QP-2XMD"},
		{env: "k7qp-2xmd", want: "K7QP-2XMD"},
		// The flag wins over the variable.
		{flag: "AAAA-BBBB", env: "k7qp-2xmd", want: "AAAA-BBBB"},
		{env: "ABCD-0O1I", refusal: PairingCodeEnv},
		{flag: "ABCD-0O1I", refusal: "--pairing-code"},
	} {
		unset = nil
		got, err := setupPairingCode(tc.flag, env(tc.env), record)
		if tc.refusal != "" {
			if err == nil || !strings.Contains(err.Error(), tc.refusal) || strings.Contains(err.Error(), "ABCD") {
				t.Fatalf("%+v: the refusal must name %s and not the value: %v", tc, tc.refusal, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Fatalf("%+v: got %q, %v", tc, got, err)
		}
		if (tc.env != "") != (len(unset) == 1) {
			t.Fatalf("%+v: the variable was not removed once read (%v)", tc, unset)
		}
	}
}

func TestAMalformedPairingCodeExitsTwoNamingTheFlagAndNotTheValue(t *testing.T) {
	for _, args := range [][]string{
		{"setup", "--home", t.TempDir(), "--gateway-url", "https://agents.example", "--pairing-code", "ABCD-0O1I"},
		{"pair", "--home", t.TempDir(), "--code", "ABCD-0O1I"},
	} {
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code != 2 {
			t.Fatalf("%v exited %d:\n%s%s", args, code, out.String(), errOut.String())
		}
		flag := args[len(args)-2]
		if !strings.Contains(errOut.String(), flag) || strings.Contains(errOut.String(), "ABCD-0O1I") {
			t.Fatalf("%v: the refusal must name %s and not repeat the value:\n%s", args, flag, errOut.String())
		}
	}
}

// 🔴 Under `curl | sh` stdin is a pipe. Pairing reads its confirmation from the
// terminal — except with a code from the portal, which asks nothing, so the
// line works where there is no terminal at all.
func TestPairingNeedsATerminalOnlyWhenItWillAsk(t *testing.T) {
	for _, tc := range []struct {
		name                                     string
		noPair, yes, dryRun, hasCode, stdinIsTTY bool
		want                                     bool
	}{
		{name: "a piped run that will ask", want: true},
		{name: "a code from the portal", hasCode: true},
		{name: "--yes", yes: true},
		{name: "--no-pair", noPair: true},
		{name: "a dry run", dryRun: true},
		{name: "stdin is the terminal", stdinIsTTY: true},
	} {
		if got := pairingNeedsTerminal(tc.noPair, tc.yes, tc.dryRun, tc.hasCode, tc.stdinIsTTY); got != tc.want {
			t.Fatalf("%s: needs a terminal = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// ── uffici upgrade (#2250) ──────────────────────────────────────────────────

func TestUpgradeRefusesBadFlagsAndAMachineWithoutARecord(t *testing.T) {
	for _, args := range [][]string{{"upgrade", "--nope"}, {"upgrade", "--home", t.TempDir(), "extra"}} {
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code != 2 {
			t.Fatalf("%v: exit %d, want 2", args, code)
		}
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"upgrade", "--home", t.TempDir(), "--check"}, &out, &errOut); code != 2 || !strings.Contains(out.String(), "uffici setup --no-pair") {
		t.Fatalf("exit %d:\n%s%s", code, out.String(), errOut.String())
	}
}

// The command reads the record and asks the plugin's release for its latest
// version; a pinned companion version is what lets a development build (the one
// under test) be compared at all.
//
// The recorded gateway is a loopback server too (#2382): --check names the
// channel and the version it names, or that it names none.
func TestUpgradeCheckReadsTheRecordAndTheLatestRelease(t *testing.T) {
	for _, tc := range []struct {
		name, answer, plugin, line string
		code                       int
	}{
		// The latest path never replaces a development build unasked — the line still
		// says the gateway named nothing.
		{name: "no version named", answer: `{"channel":"develop","version":null}`, line: "(channel develop) names no version", code: 2},
		// The gateway's version is a pin, for the companion and the plugin alike.
		{name: "a version named", answer: `{"channel":"develop","version":"0.6.0-dev.1"}`, plugin: "0.6.0-dev.1", line: "version: 0.6.0-dev.1, named by the agents gateway", code: 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repos/uffici-ai/uffici-for-hermes/releases/latest", "/repos/uffici-ai/uffici-companion/releases/latest":
					_, _ = w.Write([]byte(`{"tag_name":"v0.5.0","assets":[]}`))
				case "/v1/release":
					_, _ = w.Write([]byte(tc.answer))
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			home := t.TempDir()
			if err := os.WriteFile(filepath.Join(home, "setup.json"), []byte(`{"harness":"hermes","gatewayUrl":"`+server.URL+`","service":true}`), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("UFFICI_RELEASES_BASE_URL", server.URL)
			t.Setenv("UFFICI_RELEASES_API_URL", server.URL)
			t.Setenv("UFFICI_GITHUB_TOKEN", "")
			t.Setenv("UFFICI_CA_FILE", "")
			t.Setenv("UFFICI_GATEWAY_URL", "")
			var out, errOut bytes.Buffer
			// Nothing pinned: the companion and the plugin both follow the gateway's
			// answer — one train (a --version would pin both, #2382's review).
			code := run([]string{"upgrade", "--home", home, "--check"}, &out, &errOut)
			if code != tc.code || !strings.Contains(out.String(), tc.line) {
				t.Fatalf("exit %d, want %d and %q:\n%s%s", code, tc.code, tc.line, out.String(), errOut.String())
			}
			if tc.plugin != "" && (!strings.Contains(out.String(), "companion  0.0.0-dev → "+tc.plugin+" available") ||
				!strings.Contains(out.String(), "plugin     not installed → "+tc.plugin+" available")) {
				t.Fatalf("want the companion and the plugin at %s:\n%s", tc.plugin, out.String())
			}
		})
	}
}

// `uffici doctor` names the channel and the version the gateway names (#2382).
func TestDoctorNamesTheGatewaysRelease(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/release" {
			_, _ = w.Write([]byte(`{"channel":"develop","version":"0.10.0-dev.4"}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("UFFICI_GATEWAY_URL", server.URL)
	t.Setenv("UFFICI_CA_FILE", ca)
	var out, errOut bytes.Buffer
	// No agent key in this home, so the probe itself stops — after the release line.
	_ = run([]string{"doctor", "--home", t.TempDir()}, &out, &errOut)
	if !strings.Contains(out.String(), "release      channel develop, version 0.10.0-dev.4") {
		t.Fatalf("the doctor did not name the gateway's release:\n%s%s", out.String(), errOut.String())
	}
}

// #2278: the two restart flags contradict each other, on both commands.
func TestTheRestartFlagsContradictEachOther(t *testing.T) {
	for _, command := range []string{"setup", "upgrade"} {
		var out, errOut bytes.Buffer
		code := run([]string{command, "--restart-harness", "--no-restart-harness", "--home", t.TempDir()}, &out, &errOut)
		if code != 2 || !strings.Contains(errOut.String(), "contradict each other") {
			t.Fatalf("%s: exit %d, stderr %q", command, code, errOut.String())
		}
	}
}

// 🔴 #2294: a person's shell has no UFFICI_GATEWAY_URL, and the doctor and pair
// used to refuse to run by hand although `uffici setup` recorded the agents host.
// The variable still wins; without it the record of the same home answers.
func TestDoctorAndPairReadTheRecordedAgentsHost(t *testing.T) {
	home := t.TempDir()
	if err := setup.WriteRecord(home, setup.Record{Harness: "hermes", GatewayURL: "https://agents.recorded.example", Service: true}); err != nil {
		t.Fatal(err)
	}
	unset := func(string) string { return "" }
	env := gatewayEnvironment(home, unset)
	if got := env("UFFICI_GATEWAY_URL"); got != "https://agents.recorded.example" {
		t.Fatalf("with no variable the agents host is %q, want the recorded one", got)
	}
	if got := env("UFFICI_AGENTS_HOST"); got != "gateway" {
		t.Fatalf("UFFICI_AGENTS_HOST = %q, want gateway", got)
	}
	resolved, err := app.LoadConfig(env, app.Config{Home: home})
	if err != nil || resolved.GatewayURL != "https://agents.recorded.example" {
		t.Fatalf("the config did not carry the recorded host: %q, %v", resolved.GatewayURL, err)
	}

	set := func(key string) string {
		if key == "UFFICI_GATEWAY_URL" {
			return "https://agents.variable.example"
		}
		return ""
	}
	if got := gatewayEnvironment(home, set)("UFFICI_GATEWAY_URL"); got != "https://agents.variable.example" {
		t.Fatalf("the variable did not win over the record: %q", got)
	}

	// $UFFICI_HOME names the home when --home does not.
	byVariable := func(key string) string {
		if key == "UFFICI_HOME" {
			return home
		}
		return ""
	}
	if got := gatewayEnvironment("", byVariable)("UFFICI_GATEWAY_URL"); got != "https://agents.recorded.example" {
		t.Fatalf("the record under $UFFICI_HOME was not read: %q", got)
	}
}

// With neither the variable nor a record, the refusal says how to get one (#2294).
func TestDoctorWithNeitherNamesSetup(t *testing.T) {
	t.Setenv("UFFICI_GATEWAY_URL", "")
	var out, errOut bytes.Buffer
	if code := run([]string{"doctor", "--home", t.TempDir()}, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "uffici setup") {
		t.Fatalf("exit %d, stderr %q — want 2 naming uffici setup", code, errOut.String())
	}
}

// 🔴 #2688: `update` is the verb people who run harnesses type (`hermes update`,
// `claude update`), so it is `upgrade` by another name — the same flags, the
// same refusals.
func TestUpdateIsUpgrade(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"update", "--restart-harness", "--no-restart-harness", "--home", t.TempDir()}, &out, &errOut)
	if code != 2 || !strings.Contains(errOut.String(), "contradict each other") {
		t.Fatalf("update did not reach the upgrade command: exit %d, stderr %q", code, errOut.String())
	}
}

// 🔴 #2688: a word the companion does not know is refused, never ignored — the
// daemon's flag set stops at the first word, and `uffici updat` used to start
// the companion in the foreground.
func TestAnUnknownCommandIsRefused(t *testing.T) {
	for _, args := range [][]string{{"updat"}, {"run", "extra"}, {"--home", t.TempDir(), "status"}} {
		var out, errOut bytes.Buffer
		code := run(args, &out, &errOut)
		word := args[len(args)-1]
		if code != 2 || !strings.Contains(errOut.String(), `unknown command "`+word+`"`) {
			t.Fatalf("%v: exit %d, stderr %q", args, code, errOut.String())
		}
	}
}
