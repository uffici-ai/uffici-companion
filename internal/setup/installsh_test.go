//go:build unix

// The installer is a POSIX shell script for Linux and macOS, and its tests run
// it detached from any terminal (Setsid), which only a unix build can express.

package setup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

// The installer script at the module root is a customer's first contact with
// the companion, and a shell script is the one part of this module no
// compiler reads. So it is exercised here, for real: parsed by sh, run
// against a loopback release serving a fake binary, and refused a corrupted
// archive.

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test")
		}
		dir = parent
	}
}

// fakeBinaryArchive is what a release archive holds: `uffici` at the root.
// The fake prints its arguments, which is how the hand-over is asserted.
func fakeBinaryArchive(t *testing.T) []byte {
	t.Helper()
	script := "#!/bin/sh\nprintf 'fake-uffici %s\\n' \"$*\"\n"
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{"uffici": script, "LICENSE": "Apache-2.0\n"} {
		mode := int64(0o644)
		if name == "uffici" {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode, Size: int64(len(body))}); err != nil {
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

func thisAsset() string {
	return "uffici_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz"
}

func runInstaller(t *testing.T, server *httptest.Server, prefix string, args ...string) (string, error) {
	t.Helper()
	return runInstallerWith(t, nil, server, prefix, args...)
}

// runInstallerWithoutATerminal runs the script in a new session, so it has no
// controlling terminal and `/dev/tty` cannot be opened — the shape of a
// `curl | sh` from a CI job or a provisioning tool, deterministic on any host.
func runInstallerWithoutATerminal(t *testing.T, server *httptest.Server, prefix string, args ...string) (string, error) {
	t.Helper()
	return runInstallerWith(t, &syscall.SysProcAttr{Setsid: true}, server, prefix, args...)
}

func runInstallerWith(t *testing.T, attrs *syscall.SysProcAttr, server *httptest.Server, prefix string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{filepath.Join(moduleRoot(t), "install.sh")}, args...)...)
	cmd.SysProcAttr = attrs
	cmd.Env = append(os.Environ(),
		"UFFICI_RELEASES_BASE_URL="+server.URL,
		"UFFICI_RELEASES_API_URL="+server.URL,
		"UFFICI_INSTALL_PREFIX="+prefix,
		"UFFICI_GITHUB_TOKEN=",
		"HOME="+t.TempDir(),
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestInstallScriptParsesAsPOSIXShell(t *testing.T) {
	out, err := exec.Command("sh", "-n", filepath.Join(moduleRoot(t), "install.sh")).CombinedOutput()
	if err != nil {
		t.Fatalf("install.sh does not parse: %v\n%s", err, out)
	}
	raw, _ := os.ReadFile(filepath.Join(moduleRoot(t), "install.sh"))
	for _, bashism := range []string{"[[ ", "function ", "$(<", "source ", " == "} {
		if strings.Contains(string(raw), bashism) {
			t.Fatalf("install.sh carries a bashism: %q", bashism)
		}
	}
}

func TestInstallScriptFetchesVerifiesInstallsAndHandsOver(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Fatalf("the installer supports linux and darwin; this test runs on %s", runtime.GOOS)
	}
	archive := fakeBinaryArchive(t)
	assets := map[string][]byte{thisAsset(): archive, ChecksumsFile: []byte(sha(archive) + "  " + thisAsset() + "\n")}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if !strings.HasPrefix(r.URL.Path, "/uffici-ai/uffici-companion/releases/latest/download/") {
			http.NotFound(w, r)
			return
		}
		if body, ok := assets[name]; ok {
			_, _ = w.Write(body)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)

	prefix := t.TempDir()
	out, err := runInstaller(t, server, prefix, "--harness", "hermes", "--dry-run", "--gateway-url", "https://agents.example")
	if err != nil {
		t.Fatalf("installer failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "checksum verified") {
		t.Fatalf("the checksum was not verified:\n%s", out)
	}
	if !strings.Contains(out, "fake-uffici setup --harness hermes --dry-run --gateway-url https://agents.example") {
		t.Fatalf("the script did not hand over to `uffici setup` with its arguments:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(prefix, "bin", "uffici")); err == nil {
		t.Fatal("a dry run must install nothing")
	}
	if !strings.Contains(out, "dry run: would install") {
		t.Fatalf("the dry run did not say what it would install:\n%s", out)
	}

	// A real run installs the binary and hands over; `--yes` means no terminal
	// is needed, which is the shape a script uses.
	out, err = runInstaller(t, server, prefix, "--harness", "hermes", "--yes", "--no-pair", "--gateway-url", "https://agents.example")
	if err != nil {
		t.Fatalf("installer failed: %v\n%s", err, out)
	}
	info, err := os.Stat(filepath.Join(prefix, "bin", "uffici"))
	if err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("the binary was not installed executable: %v", err)
	}
	if !strings.Contains(out, "fake-uffici setup --harness hermes --yes --no-pair") {
		t.Fatalf("the hand-over lost its arguments:\n%s", out)
	}
	if strings.Contains(out, "uffici-install.") {
		t.Fatalf("the temporary directory leaked into the output (and probably onto disk):\n%s", out)
	}
}

// 🔴 The line the portal gives carries a pairing code (#2209). It must run where
// there is no terminal — the code asks nothing — hand the code to `uffici setup`
// untouched, and never print it itself.
func TestInstallScriptWithAPairingCodeNeedsNoTerminalAndNeverPrintsTheCode(t *testing.T) {
	archive := fakeBinaryArchive(t)
	assets := map[string][]byte{thisAsset(): archive, ChecksumsFile: []byte(sha(archive) + "  " + thisAsset() + "\n")}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body, ok := assets[r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]]; ok {
			_, _ = w.Write(body)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)

	// Non-vacuity: without a code or --yes, this very run has no terminal to ask on.
	out, err := runInstallerWithoutATerminal(t, server, t.TempDir(), "--harness", "hermes", "--gateway-url", "https://agents.example")
	if err == nil || !strings.Contains(out, "needs a terminal") {
		t.Fatalf("a run that must ask was not refused without a terminal: %v\n%s", err, out)
	}

	for _, args := range [][]string{
		{"--harness", "hermes", "--gateway-url", "https://agents.example", "--pairing-code", "K7QP-2XMD"},
		{"--harness", "hermes", "--pairing-code=K7QP-2XMD", "--gateway-url", "https://agents.example"},
	} {
		out, err := runInstallerWithoutATerminal(t, server, t.TempDir(), args...)
		if err != nil {
			t.Fatalf("%v: the installer failed without a terminal: %v\n%s", args, err, out)
		}
		// The fake binary prints what it was handed: the code arrives intact.
		if !strings.Contains(out, "fake-uffici setup "+strings.Join(args, " ")) {
			t.Fatalf("%v: the hand-over changed the arguments:\n%s", args, out)
		}
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "uffici-install:") && strings.Contains(line, "K7QP") {
				t.Fatalf("the script printed the code: %q", line)
			}
		}
		if !strings.Contains(out, "****-****") {
			t.Fatalf("the hand-over line does not show the masked code:\n%s", out)
		}
	}
}

func TestInstallScriptFetchesThroughTheAPIWithAToken(t *testing.T) {
	archive := fakeBinaryArchive(t)
	sums := []byte(sha(archive) + "  " + thisAsset() + "\n")
	const token = "ghp_installer_test_token_not_a_secret"
	var bearerHits, publicHits int
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/uffici-ai/") {
			publicHits++
			http.NotFound(w, r) // a private repository answers the public URL with nothing useful
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "no", http.StatusNotFound)
			return
		}
		bearerHits++
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			// GitHub's real shape: pretty-printed, `url` before `name`, an
			// uploader object in between, and sibling assets with longer names.
			_, _ = w.Write([]byte("{\n  \"assets\": [\n    {\n      \"url\": \"" + server.URL + "/repos/uffici-ai/uffici-companion/releases/assets/1\",\n      \"id\": 1,\n      \"uploader\": {\"login\": \"x\", \"id\": 2},\n      \"name\": \"" + thisAsset() + ".sig\"\n    },\n    {\n      \"url\": \"" + server.URL + "/repos/uffici-ai/uffici-companion/releases/assets/2\",\n      \"id\": 2,\n      \"uploader\": {\"login\": \"x\", \"id\": 2},\n      \"name\": \"" + thisAsset() + "\"\n    },\n    {\n      \"url\": \"" + server.URL + "/repos/uffici-ai/uffici-companion/releases/assets/3\",\n      \"id\": 3,\n      \"name\": \"checksums.txt\"\n    }\n  ]\n}\n"))
		case strings.HasSuffix(r.URL.Path, "/assets/2"):
			_, _ = w.Write(archive)
		case strings.HasSuffix(r.URL.Path, "/assets/3"):
			_, _ = w.Write(sums)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	cmd := exec.Command("sh", filepath.Join(moduleRoot(t), "install.sh"), "--dry-run", "--harness", "hermes")
	cmd.Env = append(os.Environ(),
		"UFFICI_RELEASES_BASE_URL="+server.URL,
		"UFFICI_RELEASES_API_URL="+server.URL,
		"UFFICI_INSTALL_PREFIX="+t.TempDir(),
		"UFFICI_GITHUB_TOKEN="+token,
		"HOME="+t.TempDir(),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("installer failed on the token path: %v\n%s", err, out)
	}
	if bearerHits < 4 || publicHits != 0 {
		t.Fatalf("expected the listing, checksums and asset through the API with the token (got %d bearer hits, %d public hits)", bearerHits, publicHits)
	}
	if strings.Contains(string(out), token) {
		t.Fatal("the token was printed")
	}
	if !strings.Contains(string(out), "fake-uffici setup --dry-run --harness hermes") {
		t.Fatalf("no hand-over:\n%s", out)
	}
}

func TestInstallScriptRefusesACorruptedArchive(t *testing.T) {
	archive := fakeBinaryArchive(t)
	assets := map[string][]byte{thisAsset(): archive, ChecksumsFile: []byte(strings.Repeat("00", 32) + "  " + thisAsset() + "\n")}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if body, ok := assets[name]; ok {
			_, _ = w.Write(body)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	prefix := t.TempDir()
	out, err := runInstaller(t, server, prefix, "--dry-run")
	if err == nil || !strings.Contains(out, "does not match its published checksum") {
		t.Fatalf("a corrupted archive was installed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(prefix, "bin", "uffici")); err == nil {
		t.Fatal("the binary was written despite the checksum mismatch")
	}
}

func TestInstallScriptRefusesAnUnlistedAsset(t *testing.T) {
	archive := fakeBinaryArchive(t)
	assets := map[string][]byte{thisAsset(): archive, ChecksumsFile: []byte(sha(archive) + "  other.tar.gz\n")}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if body, ok := assets[name]; ok {
			_, _ = w.Write(body)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	out, err := runInstaller(t, server, t.TempDir(), "--dry-run")
	if err == nil || !strings.Contains(out, "did not vouch") {
		t.Fatalf("an unlisted asset was installed: %v\n%s", err, out)
	}
}
