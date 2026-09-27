package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
}

func TestSetupWithoutAGatewayURLExitsTwo(t *testing.T) {
	t.Setenv("UFFICI_GATEWAY_URL", "")
	var out, errOut bytes.Buffer
	if code := run([]string{"setup", "--home", t.TempDir(), "--harness", "hermes"}, &out, &errOut); code != 2 {
		t.Fatalf("exit %d, want 2:\n%s%s", code, out.String(), errOut.String())
	}
}

func TestSetupDryRunReachesThePluginReleaseAndNamesTheMissingHarnessTool(t *testing.T) {
	// A loopback "release" of the plugin, so the wiring from the command down to
	// the fetch and the manifest is exercised without GitHub. The harness's own
	// tool is not on this machine's PATH, which is the refusal the run ends on.
	manifest := `{"manifestVersion":1,"harness":"hermes","name":"uffici-for-hermes","pluginDir":"plugins/uffici-for-hermes","homeEnv":"HERMES_HOME","defaultHome":"~/.hermes","enable":["definitely-not-a-real-tool-on-this-machine","plugins","enable","uffici-for-hermes"],"env":{},"restart":"restart"}`
	archive := gzippedTar(t, "uffici-for-hermes", map[string]string{"uffici-plugin.json": manifest, "plugin.yaml": "name: x\n"})
	sum := sha256.Sum256(archive)
	assets := map[string][]byte{
		"uffici-for-hermes.tar.gz": archive,
		"checksums.txt":            []byte(hex.EncodeToString(sum[:]) + "  uffici-for-hermes.tar.gz\n"),
	}
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
	code := run([]string{"setup", "--home", t.TempDir(), "--harness", "hermes", "--no-pair", "--no-service", "--dry-run", "--gateway-url", "https://agents.example"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "plugin uffici-for-hermes") || !strings.Contains(out.String(), "is not on the PATH") {
		t.Fatalf("the run did not reach the manifest and name the missing tool:\n%s", out.String())
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
