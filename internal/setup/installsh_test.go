//go:build unix

// The installer is a POSIX shell script for Linux and macOS, and its tests run
// it detached from any terminal (Setsid), which only a unix build can express.

package setup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
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
// The fake prints its arguments, which is how the hand-over is asserted, and
// lists `-version` in its setup usage as a binary that knows the flag does.
func fakeBinaryArchive(t *testing.T) []byte {
	t.Helper()
	return fakeBinaryArchiveWith(t, true)
}

// fakeBinaryArchiveWith builds the fake; knowsVersion=false is a release from
// before `uffici setup --version`.
func fakeBinaryArchiveWith(t *testing.T, knowsVersion bool) []byte {
	t.Helper()
	script := "#!/bin/sh\n"
	if knowsVersion {
		script += "if [ \"$2\" = -h ]; then printf '  -version string\\n    (default $UFFICI_PAIRING_CODE)\\n' >&2; exit 2; fi\n"
	}
	script += "printf 'fake-uffici %s\\n' \"$*\"\n"
	script += "if [ -n \"$UFFICI_PAIRING_CODE\" ]; then echo 'fake-uffici: a pairing code in the environment'; fi\n"
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

// fakeCosign is a directory holding a `cosign` that logs its arguments to
// $FAKE_COSIGN_LOG and exits $FAKE_COSIGN_EXIT (0 by default), put first on
// every run's PATH: the script's signature step is exercised the same way on
// every machine, whether or not a real cosign is installed there (the release
// workflow installs one before it runs these tests). The real verification is
// the Go side's, against a virtual Sigstore (signature_test.go).
func fakeCosign(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n[ -n \"$FAKE_COSIGN_LOG\" ] && printf '%s\\n' \"$*\" >>\"$FAKE_COSIGN_LOG\"\n" +
		"[ \"${FAKE_COSIGN_EXIT:-0}\" = 0 ] || { echo 'Error: none of the expected identities matched' >&2; exit \"$FAKE_COSIGN_EXIT\"; }\n"
	if err := os.WriteFile(filepath.Join(dir, "cosign"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// releaseAssets is one release of the companion: the archive, its checksum
// line, and the checksum list's signature bundle.
func releaseAssets(archive []byte, sums string) map[string][]byte {
	return map[string][]byte{thisAsset(): archive, ChecksumsFile: []byte(sums), ChecksumsFile + BundleSuffix: []byte("a bundle")}
}

// installLatest is the tag a loopback release's latest stands for: the version
// the API's listing names, which the script resolves before it verifies (#3118
// review), and whose download path it then fetches from.
const installLatest = "0.9.0"

// withLatest answers the web's `releases/latest` the way GitHub does — a
// redirect to installLatest's tag page — and the anonymous API the way GitHub
// does past its hourly budget, 403 (#3118 review): every run here resolves the
// latest without the API. Every other request goes to next.
func withLatest(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/uffici-ai/uffici-companion/releases/latest":
			w.Header().Set("Location", "https://github.com/uffici-ai/uffici-companion/releases/tag/v"+installLatest)
			w.WriteHeader(http.StatusFound)
			return
		case "/repos/uffici-ai/uffici-companion/releases/latest":
			http.Error(w, `{"message":"API rate limit exceeded"}`, http.StatusForbidden)
			return
		}
		next(w, r)
	})
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
	return runInstallerEnv(t, attrs, server, prefix, nil, args...)
}

// runInstallerEnv runs the script with extra environment, which wins over the
// defaults (a later entry of the same name is the one a process sees).
func runInstallerEnv(t *testing.T, attrs *syscall.SysProcAttr, server *httptest.Server, prefix string, extra []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{filepath.Join(moduleRoot(t), "install.sh")}, args...)...)
	cmd.SysProcAttr = attrs
	cmd.Env = append(os.Environ(),
		"PATH="+fakeCosign(t)+string(os.PathListSeparator)+os.Getenv("PATH"),
		"UFFICI_PAIRING_CODE=",
		"UFFICI_DEV_RELEASE_IDENTITY=",
		"UFFICI_COMPANION_REPO=",
		"UFFICI_RELEASES_BASE_URL="+server.URL,
		"UFFICI_RELEASES_API_URL="+server.URL,
		"UFFICI_INSTALL_PREFIX="+prefix,
		"UFFICI_GITHUB_TOKEN=",
		"UFFICI_VERSION=",
		// Not run under sudo, whatever the developer's shell is (#3128).
		"SUDO_USER=",
		"UFFICI_HOME=",
		// 🔴 A run with no --gateway-url asks this loopback server, never the
		// production default: it names no release, so the latest is fetched.
		"UFFICI_GATEWAY_URL="+server.URL,
		"HOME="+t.TempDir(),
	)
	cmd.Env = append(cmd.Env, extra...)
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
	assets := releaseAssets(archive, sha(archive)+"  "+thisAsset()+"\n")
	server := httptest.NewServer(withLatest(func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if !strings.HasPrefix(r.URL.Path, "/uffici-ai/uffici-companion/releases/download/v"+installLatest+"/") {
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
	assets := releaseAssets(archive, sha(archive)+"  "+thisAsset()+"\n")
	server := httptest.NewServer(withLatest(func(w http.ResponseWriter, r *http.Request) {
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
	// Counted on the server's goroutines, read by the test (#2696).
	var bearerHits, publicHits atomic.Int32
	var server *httptest.Server
	// Unstarted, so `server` is assigned before any handler can read its URL
	// (#2696).
	server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/uffici-ai/") {
			publicHits.Add(1)
			http.NotFound(w, r) // a private repository answers the public URL with nothing useful
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "no", http.StatusNotFound)
			return
		}
		bearerHits.Add(1)
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"), strings.HasSuffix(r.URL.Path, "/releases/tags/v"+installLatest):
			// GitHub's real shape: pretty-printed, `url` before `name`, an
			// uploader object in between, and sibling assets with longer names.
			_, _ = w.Write([]byte("{\n  \"tag_name\": \"v" + installLatest + "\",\n  \"assets\": [\n    {\n      \"url\": \"" + server.URL + "/repos/uffici-ai/uffici-companion/releases/assets/1\",\n      \"id\": 1,\n      \"uploader\": {\"login\": \"x\", \"id\": 2},\n      \"name\": \"" + thisAsset() + ".sig\"\n    },\n    {\n      \"url\": \"" + server.URL + "/repos/uffici-ai/uffici-companion/releases/assets/2\",\n      \"id\": 2,\n      \"uploader\": {\"login\": \"x\", \"id\": 2},\n      \"name\": \"" + thisAsset() + "\"\n    },\n    {\n      \"url\": \"" + server.URL + "/repos/uffici-ai/uffici-companion/releases/assets/3\",\n      \"id\": 3,\n      \"name\": \"checksums.txt\"\n    },\n    {\n      \"url\": \"" + server.URL + "/repos/uffici-ai/uffici-companion/releases/assets/4\",\n      \"id\": 4,\n      \"name\": \"checksums.txt.sigstore.json\"\n    }\n  ]\n}\n"))
		case strings.HasSuffix(r.URL.Path, "/assets/2"):
			_, _ = w.Write(archive)
		case strings.HasSuffix(r.URL.Path, "/assets/3"):
			_, _ = w.Write(sums)
		case strings.HasSuffix(r.URL.Path, "/assets/4"):
			_, _ = w.Write([]byte("a bundle"))
		default:
			http.NotFound(w, r)
		}
	}))
	server.Start()
	t.Cleanup(server.Close)
	cmd := exec.Command("sh", filepath.Join(moduleRoot(t), "install.sh"), "--dry-run", "--harness", "hermes")
	cmd.Env = append(os.Environ(),
		"PATH="+fakeCosign(t)+string(os.PathListSeparator)+os.Getenv("PATH"),
		"UFFICI_DEV_RELEASE_IDENTITY=",
		"UFFICI_COMPANION_REPO=",
		"UFFICI_RELEASES_BASE_URL="+server.URL,
		"UFFICI_RELEASES_API_URL="+server.URL,
		"UFFICI_INSTALL_PREFIX="+t.TempDir(),
		"UFFICI_GITHUB_TOKEN="+token,
		"UFFICI_VERSION=",
		"UFFICI_GATEWAY_URL="+server.URL, // answers 404: the latest release
		"HOME="+t.TempDir(),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("installer failed on the token path: %v\n%s", err, out)
	}
	if bearerHits.Load() < 4 || publicHits.Load() != 0 {
		t.Fatalf("expected the listing, checksums and asset through the API with the token (got %d bearer hits, %d public hits)", bearerHits.Load(), publicHits.Load())
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
	assets := releaseAssets(archive, strings.Repeat("00", 32)+"  "+thisAsset()+"\n")
	server := httptest.NewServer(withLatest(func(w http.ResponseWriter, r *http.Request) {
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
	assets := releaseAssets(archive, sha(archive)+"  other.tar.gz\n")
	server := httptest.NewServer(withLatest(func(w http.ResponseWriter, r *http.Request) {
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

// ── Which release, and which gateway (#2382) ────────────────────────────────

// trainServer serves one companion release by tag (never `latest`, unless
// latest is set) and the gateway's `/v1/release` answer, from one origin.
func trainServer(t *testing.T, tag string, archive []byte, release string, latest bool, sawAuth *bool) *httptest.Server {
	t.Helper()
	assets := releaseAssets(archive, sha(archive)+"  "+thisAsset()+"\n")
	server := httptest.NewServer(withLatest(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/release" {
			if r.Header.Get("Authorization") != "" {
				*sawAuth = true
			}
			_, _ = w.Write([]byte(release))
			return
		}
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		prefix := "/uffici-ai/uffici-companion/releases/download/v" + tag + "/"
		if latest {
			prefix = "/uffici-ai/uffici-companion/releases/download/v" + installLatest + "/"
		}
		if body, ok := assets[name]; ok && tag != "" && strings.HasPrefix(r.URL.Path, prefix) {
			_, _ = w.Write(body)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestInstallScriptInstallsTheVersionTheGatewayNames(t *testing.T) {
	var sawAuth bool
	server := trainServer(t, "0.10.0-dev.4", fakeBinaryArchive(t), `{"channel": "develop", "version": "0.10.0-dev.4"}`, false, &sawAuth)
	out, err := runInstaller(t, server, t.TempDir(), "--harness", "hermes", "--yes", "--no-pair", "--gateway-url", server.URL)
	if err != nil {
		t.Fatalf("installer failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		"agents gateway: " + server.URL + " (from --gateway-url)",
		"version: 0.10.0-dev.4, named by the agents gateway " + server.URL + " (channel develop)",
		"fetching uffici 0.10.0-dev.4",
		// Both processes agree: setup is handed the version the script installed.
		"fake-uffici setup --harness hermes --yes --no-pair --gateway-url " + server.URL + " --version 0.10.0-dev.4",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("no %q:\n%s", want, out)
		}
	}
	if sawAuth {
		t.Fatal("the gateway was sent an Authorization header")
	}

	// A gateway rolled back to a release whose setup lacks --version: the
	// binary is installed, and setup is not handed a flag it would refuse.
	old := trainServer(t, "0.8.0", fakeBinaryArchiveWith(t, false), `{"channel":"main","version":"0.8.0"}`, false, &sawAuth)
	out, err = runInstaller(t, old, t.TempDir(), "--yes", "--no-pair", "--gateway-url", old.URL)
	if err != nil || strings.Contains(out, "--version 0.8.0") || !strings.Contains(out, "does not take setup --version") {
		t.Fatalf("an older release was handed --version: %v\n%s", err, out)
	}
}

func TestInstallScriptFallsBackToTheLatestWithOneLine(t *testing.T) {
	var sawAuth bool
	for _, tc := range []struct{ name, answer, line string }{
		{name: "no version named", answer: `{"channel":"develop","version":null}`, line: "names no version"},
		{name: "not a release", answer: `<html>`, line: "sent an answer that is not a release"},
		{name: "not a release version", answer: `{"channel":"main","version":"latest"}`, line: "which is not a release version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := trainServer(t, "latest", fakeBinaryArchive(t), tc.answer, true, &sawAuth)
			out, err := runInstaller(t, server, t.TempDir(), "--yes", "--no-pair", "--gateway-url", server.URL)
			if err != nil || !strings.Contains(out, "fetching the latest uffici") {
				t.Fatalf("the latest was not fetched: %v\n%s", err, out)
			}
			if !strings.Contains(out, "version: the latest release — the agents gateway "+server.URL) || !strings.Contains(out, tc.line) {
				t.Fatalf("no fallback line naming %q:\n%s", tc.line, out)
			}
			if strings.Count(out, "uffici-install: version: ") != 1 || strings.Contains(out, " --version ") {
				t.Fatalf("want ONE version line and no --version handed over:\n%s", out)
			}
		})
	}
	// Unreachable: a closed loopback port.
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	server := trainServer(t, "latest", fakeBinaryArchive(t), "", true, &sawAuth)
	out, err := runInstaller(t, server, t.TempDir(), "--yes", "--no-pair", "--gateway-url", closed.URL)
	if err != nil || !strings.Contains(out, "version: the latest release — the agents gateway "+closed.URL+" could not be reached") {
		t.Fatalf("an unreachable gateway did not fall back with its line: %v\n%s", err, out)
	}
}

// A release whose files are not up yet names the version it waited for.
func TestInstallScriptNamesAVersionWithoutAssetsYet(t *testing.T) {
	var sawAuth bool
	server := trainServer(t, "", fakeBinaryArchive(t), `{"channel":"develop","version":"0.10.0-dev.5"}`, false, &sawAuth)
	out, err := runInstaller(t, server, t.TempDir(), "--yes", "--no-pair", "--gateway-url", server.URL)
	if err == nil || !strings.Contains(out, "release v0.10.0-dev.5 has no assets yet") || !strings.Contains(out, "try again in a few minutes") {
		t.Fatalf("want the no-assets-yet refusal: %v\n%s", err, out)
	}
}

// 🔴 --gateway-url, UFFICI_GATEWAY_URL, the record, the production default —
// each beating the next, and named before anything is downloaded. A `curl`
// shim answers every gateway question unreachable and logs the URL, so no run
// here reaches the production gateway.
func TestInstallScriptResolvesTheGatewayInFourSteps(t *testing.T) {
	realCurl, err := exec.LookPath("curl")
	if err != nil {
		t.Fatal(err)
	}
	shims := t.TempDir()
	shim := "#!/bin/sh\nfor a in \"$@\"; do case \"$a\" in */v1/release) printf '%s\\n' \"$a\" >>\"$GATEWAY_LOG\"; exit 7 ;; esac; done\nexec " + realCurl + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(shims, "curl"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	var sawAuth bool
	server := trainServer(t, "latest", fakeBinaryArchive(t), "", true, &sawAuth)
	for _, tc := range []struct {
		name, flag, env, recorded, want, from string
		homeFlag                              bool // the record lives under --home, not ~/.uffici
	}{
		{name: "the flag", flag: "https://flag.example", env: "https://env.example", recorded: "https://record.example", want: "https://flag.example", from: "from --gateway-url"},
		{name: "the variable", env: "https://env.example", recorded: "https://record.example", want: "https://env.example", from: "from UFFICI_GATEWAY_URL"},
		{name: "the record", recorded: "https://record.example", want: "https://record.example", from: "recorded in setup.json"},
		{name: "the record under --home", recorded: "https://home.example", homeFlag: true, want: "https://home.example", from: "recorded in setup.json"},
		{name: "the default", want: "https://agents.uffici.ai", from: "the default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			recordHome := filepath.Join(home, ".uffici")
			if tc.homeFlag {
				recordHome = filepath.Join(t.TempDir(), "elsewhere")
			}
			if tc.recorded != "" {
				must(t, WriteRecord(recordHome, Record{GatewayURL: tc.recorded}))
			}
			log := filepath.Join(t.TempDir(), "asked")
			args := []string{filepath.Join(moduleRoot(t), "install.sh"), "--yes", "--no-pair"}
			if tc.homeFlag {
				args = append(args, "--home", recordHome)
			}
			if tc.flag != "" {
				args = append(args, "--gateway-url", tc.flag)
			}
			cmd := exec.Command("sh", args...)
			cmd.Env = append(os.Environ(),
				"PATH="+shims+":"+fakeCosign(t)+":"+os.Getenv("PATH"),
				"UFFICI_DEV_RELEASE_IDENTITY=",
				"UFFICI_COMPANION_REPO=",
				"GATEWAY_LOG="+log,
				"UFFICI_RELEASES_BASE_URL="+server.URL,
				"UFFICI_RELEASES_API_URL="+server.URL,
				"UFFICI_INSTALL_PREFIX="+t.TempDir(),
				"UFFICI_GITHUB_TOKEN=",
				"UFFICI_VERSION=",
				"UFFICI_HOME=",
				"UFFICI_GATEWAY_URL="+tc.env,
				"HOME="+home,
			)
			raw, err := cmd.CombinedOutput()
			out := string(raw)
			if err != nil {
				t.Fatalf("installer failed: %v\n%s", err, out)
			}
			named := strings.Index(out, "agents gateway: "+tc.want+" ("+tc.from+")")
			fetched := strings.Index(out, "fetching the latest uffici")
			if named < 0 || fetched < 0 || named > fetched {
				t.Fatalf("the gateway %s (%s) is not named before the download:\n%s", tc.want, tc.from, out)
			}
			asked, _ := os.ReadFile(log)
			if strings.TrimSpace(string(asked)) != tc.want+"/v1/release" {
				t.Fatalf("asked %q, want %s/v1/release", asked, tc.want)
			}
		})
	}
}

func TestInstallScriptRefusesRootThroughSudo(t *testing.T) {
	// #3128: under `sudo` the companion would become root's service, pointed at root's
	// harness, while the harness runs as someone else. A fake `id` stands in for root.
	fake := t.TempDir()
	if err := os.WriteFile(filepath.Join(fake, "id"), []byte("#!/bin/sh\necho 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, sudoUser := range []string{"alice", ""} {
		// --yes: a run with no terminal must not stop for its pairing question first.
		cmd := exec.Command("sh", filepath.Join(moduleRoot(t), "install.sh"), "--dry-run", "--yes")
		cmd.Env = append(os.Environ(),
			"PATH="+fake+string(os.PathListSeparator)+os.Getenv("PATH"),
			"SUDO_USER="+sudoUser,
			"UFFICI_RELEASES_BASE_URL=http://127.0.0.1:1",
			"UFFICI_RELEASES_API_URL=http://127.0.0.1:1",
			"UFFICI_GATEWAY_URL=http://127.0.0.1:1",
			"HOME="+t.TempDir(),
		)
		out, err := cmd.CombinedOutput()
		refused := strings.Contains(string(out), "do not run this with sudo")
		if sudoUser != "" && (err == nil || !strings.Contains(string(out), "run it as alice")) {
			t.Fatalf("root through sudo was not refused: %v\n%s", err, out)
		}
		if sudoUser != "" && strings.Contains(string(out), " 2\n") {
			t.Fatalf("the exit status was printed inside the message:\n%s", out)
		}
		// Root itself (a server where the harness runs as root) is NOT refused: the run
		// must get past the check to the line it prints next, whatever happens after.
		if sudoUser == "" && (refused || !strings.Contains(string(out), "uffici-install: agents gateway:")) {
			t.Fatalf("root without sudo did not get past the check:\n%s", out)
		}
	}
}

// ── The release's signature (#3118) ─────────────────────────────────────────

// pathWithout is a PATH holding every program the current one does except
// name: the machine without cosign, whatever this one has installed.
func pathWithout(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	for _, entry := range filepath.SplitList(os.Getenv("PATH")) {
		files, err := os.ReadDir(entry)
		if err != nil {
			continue
		}
		for _, f := range files {
			link := filepath.Join(dir, f.Name())
			if f.Name() == name || f.IsDir() {
				continue
			}
			if _, err := os.Lstat(link); err == nil {
				continue // the first on the PATH wins, as it would
			}
			_ = os.Symlink(filepath.Join(entry, f.Name()), link)
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, "curl")); err != nil {
		t.Fatal("the stand-in PATH lost curl")
	}
	return dir
}

func TestInstallScriptVerifiesTheChecksumListsSignatureFirst(t *testing.T) {
	var sawAuth bool
	for _, tc := range []struct {
		name, release, tag string
		latest             bool
		extra              []string
		want               string
	}{
		// A version named: the workflow at exactly that tag.
		{name: "pinned", tag: "0.10.0-dev.4", release: `{"channel":"develop","version":"0.10.0-dev.4"}`,
			want: "verify-blob --bundle {work}/checksums.txt.sigstore.json --certificate-identity " + ReleaseIdentity(CompanionRepo, "0.10.0-dev.4") + " --certificate-oidc-issuer https://token.actions.githubusercontent.com {work}/checksums.txt"},
		// 🔴 The latest path: the tag the listing names, resolved FIRST, and the
		// workflow at exactly that tag — the identity Go builds (ReleaseIdentity),
		// so the two sides cannot drift.
		{name: "latest", tag: "latest", latest: true, release: `{"channel":"develop","version":null}`,
			want: "--certificate-identity " + ReleaseIdentity(CompanionRepo, installLatest) + " --certificate-oidc-issuer https://token.actions.githubusercontent.com"},
		// 🔴 A redirected repository moves the download, never the signer.
		{name: "redirected", tag: "0.10.0", release: `{"channel":"main","version":"0.10.0"}`, extra: []string{"UFFICI_COMPANION_REPO=someone/fork"},
			want: "--certificate-identity " + ReleaseIdentity(CompanionRepo, "0.10.0") + " --certificate-oidc-issuer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			archive := fakeBinaryArchive(t)
			server := trainServer(t, tc.tag, archive, tc.release, tc.latest, &sawAuth)
			if len(tc.extra) > 0 {
				server = forkServer(t, server)
			}
			log := filepath.Join(t.TempDir(), "cosign")
			out, err := runInstallerEnv(t, nil, server, t.TempDir(), append([]string{"FAKE_COSIGN_LOG=" + log}, tc.extra...), "--yes", "--no-pair", "--gateway-url", server.URL)
			if err != nil {
				t.Fatalf("installer failed: %v\n%s", err, out)
			}
			raw, _ := os.ReadFile(log)
			got := strings.TrimSpace(string(raw))
			if work := strings.TrimSuffix(strings.Fields(got + " x x x")[2], "/checksums.txt.sigstore.json"); work != "" {
				got = strings.ReplaceAll(got, work, "{work}")
			}
			if strings.Count(got, "verify-blob") != 1 || !strings.Contains(got, tc.want) {
				t.Fatalf("cosign was asked %q, want %q", got, tc.want)
			}
			signed, checked := strings.Index(out, "signature verified: checksums.txt was signed by"), strings.Index(out, "checksum verified")
			if signed < 0 || checked < 0 || signed > checked {
				t.Fatalf("the signature was not verified before the checksum:\n%s", out)
			}
		})
	}
}

// forkServer serves the companion's release under someone/fork as well, the
// shape of a redirected repository.
func forkServer(t *testing.T, upstream *httptest.Server) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.Replace(r.URL.Path, "/someone/fork/", "/uffici-ai/uffici-companion/", 1)
		resp, err := upstream.Client().Get(upstream.URL + path)
		if err != nil {
			http.Error(w, "upstream", http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(server.Close)
	return server
}

// 🔴 The guard on the script's side: cosign refusing the bundle (another
// identity signed it) stops the run before the archive is even downloaded.
func TestInstallScriptRefusesAReleaseCosignRefuses(t *testing.T) {
	archive := fakeBinaryArchive(t)
	var archiveFetched atomic.Bool
	assets := releaseAssets(archive, sha(archive)+"  "+thisAsset()+"\n")
	server := httptest.NewServer(withLatest(func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if name == thisAsset() {
			archiveFetched.Store(true)
		}
		if body, ok := assets[name]; ok {
			_, _ = w.Write(body)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	prefix := t.TempDir()
	out, err := runInstallerEnv(t, nil, server, prefix, []string{"FAKE_COSIGN_EXIT=1"}, "--yes", "--no-pair")
	if err == nil || !strings.Contains(out, "the signature on checksums.txt was not made by") || !strings.Contains(out, "none of the expected identities matched") {
		t.Fatalf("a release cosign refused was installed: %v\n%s", err, out)
	}
	if archiveFetched.Load() {
		t.Fatal("the archive was downloaded before the signature was checked")
	}
	if _, err := os.Stat(filepath.Join(prefix, "bin", "uffici")); err == nil {
		t.Fatal("the binary was written despite the refused signature")
	}
	// A release with no bundle at all is refused too.
	delete(assets, ChecksumsFile+BundleSuffix)
	if out, err := runInstaller(t, server, prefix, "--yes", "--no-pair"); err == nil || !strings.Contains(out, "checksums.txt.sigstore.json") {
		t.Fatalf("a release without a signature was installed: %v\n%s", err, out)
	}
}

// Without cosign the script says, in so many words, that it did not verify the
// signature — and still installs, because the binary verifies what it fetches.
func TestInstallScriptSaysWhenItCouldNotVerifyTheSignature(t *testing.T) {
	archive := fakeBinaryArchive(t)
	var bundleFetched atomic.Bool
	assets := releaseAssets(archive, sha(archive)+"  "+thisAsset()+"\n")
	server := httptest.NewServer(withLatest(func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if name == ChecksumsFile+BundleSuffix {
			bundleFetched.Store(true)
		}
		if body, ok := assets[name]; ok {
			_, _ = w.Write(body)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	cmd := exec.Command("sh", filepath.Join(moduleRoot(t), "install.sh"), "--dry-run")
	cmd.Env = append(os.Environ(),
		"PATH="+pathWithout(t, "cosign"), "UFFICI_PAIRING_CODE=", "UFFICI_DEV_RELEASE_IDENTITY=", "UFFICI_COMPANION_REPO=",
		"UFFICI_RELEASES_BASE_URL="+server.URL, "UFFICI_RELEASES_API_URL="+server.URL, "UFFICI_INSTALL_PREFIX="+t.TempDir(),
		"UFFICI_GITHUB_TOKEN=", "UFFICI_VERSION=", "SUDO_USER=", "UFFICI_HOME=", "UFFICI_GATEWAY_URL="+server.URL, "HOME="+t.TempDir(),
	)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("installer failed: %v\n%s%s", err, stdout.String(), stderr.String())
	}
	// 🔴 On STDERR, and naming the residual risk: the binary itself is unverified.
	warning := "WARNING: signature NOT verified — cosign is not on the PATH, so the uffici binary about to be installed is checked only against a checksum list from the same release"
	if !strings.Contains(stderr.String(), warning) || !strings.Contains(stderr.String(), "verify the archive's .sigstore.json bundle yourself") {
		t.Fatalf("no warning on stderr naming the risk:\nstderr: %s", stderr.String())
	}
	if strings.Contains(stdout.String(), "NOT verified") || strings.Contains(stdout.String()+stderr.String(), "signature verified:") {
		t.Fatalf("the warning is on stdout, or a verification was claimed:\nstdout: %s", stdout.String())
	}
	if bundleFetched.Load() {
		t.Fatal("the bundle was fetched with nothing to check it")
	}
}

// The redirects take https only (plain http on loopback, for these tests), and
// the repository is owner/name.
func TestInstallScriptRefusesAnInsecureRedirect(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	for _, extra := range [][]string{
		{"UFFICI_RELEASES_BASE_URL=http://releases.example"},
		{"UFFICI_RELEASES_API_URL=http://api.example"},
		{"UFFICI_RELEASES_BASE_URL=ftp://releases.example"},
		{"UFFICI_COMPANION_REPO=just-a-name"},
		{"UFFICI_COMPANION_REPO=a/b/c"},
		// 🔴 A user name turns a loopback-looking origin into another host, in the
		// clear (#3118 review): `localhost:1@evil.example` is evil.example.
		{"UFFICI_RELEASES_API_URL=http://localhost:1@evil.example"},
		{"UFFICI_RELEASES_BASE_URL=http://127.0.0.1:1@evil.example"},
		{"UFFICI_RELEASES_API_URL=https://api.github.com@evil.example"},
		{"UFFICI_RELEASES_API_URL=http://localhost:1.evil.example"},
		{"UFFICI_RELEASES_API_URL=http://localhost:"},
		{"UFFICI_RELEASES_BASE_URL=http://[::1]:8x"},
	} {
		out, err := runInstallerEnv(t, nil, server, t.TempDir(), extra, "--dry-run")
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 2 || (!strings.Contains(out, "https only") && !strings.Contains(out, "owner/name")) {
			t.Fatalf("%v was not refused: %v\n%s", extra, err, out)
		}
	}
}

// 🔴 The development switch replaces the identity cosign is given, and says so
// out loud.
func TestInstallScriptAnnouncesTheDevelopmentIdentitySwitch(t *testing.T) {
	archive := fakeBinaryArchive(t)
	assets := releaseAssets(archive, sha(archive)+"  "+thisAsset()+"\n")
	server := httptest.NewServer(withLatest(func(w http.ResponseWriter, r *http.Request) {
		if body, ok := assets[r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]]; ok {
			_, _ = w.Write(body)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	log := filepath.Join(t.TempDir(), "cosign")
	out, err := runInstallerEnv(t, nil, server, t.TempDir(), []string{"FAKE_COSIGN_LOG=" + log, "UFFICI_DEV_RELEASE_IDENTITY=^https://github\\.com/me/"}, "--dry-run")
	if err != nil {
		t.Fatalf("installer failed: %v\n%s", err, out)
	}
	asked, _ := os.ReadFile(log)
	// 🔴 Anchored as Go anchors it (anchored, signature.go): a whole identity, never a substring.
	if !strings.Contains(out, "WARNING: UFFICI_DEV_RELEASE_IDENTITY is set") || !strings.Contains(string(asked), "--certificate-identity-regexp "+anchored("^https://github\\.com/me/")+" --certificate-oidc-issuer https://token.actions.githubusercontent.com") {
		t.Fatalf("the switch was not announced or not used:\n%s\ncosign: %s", out, asked)
	}
}

// ── The pairing code out of argv (#3119) ────────────────────────────────────

func TestInstallScriptTakesThePairingCodeFromTheEnvironment(t *testing.T) {
	archive := fakeBinaryArchive(t)
	assets := releaseAssets(archive, sha(archive)+"  "+thisAsset()+"\n")
	server := httptest.NewServer(withLatest(func(w http.ResponseWriter, r *http.Request) {
		if body, ok := assets[r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]]; ok {
			_, _ = w.Write(body)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	// No terminal, no flag: the code in the environment is what lets it run.
	out, err := runInstallerEnv(t, &syscall.SysProcAttr{Setsid: true}, server, t.TempDir(), []string{"UFFICI_PAIRING_CODE=K7QP-2XMD"}, "--harness", "hermes", "--gateway-url", "https://agents.example")
	if err != nil {
		t.Fatalf("the installer failed without a terminal: %v\n%s", err, out)
	}
	if !strings.Contains(out, "fake-uffici setup --harness hermes --gateway-url https://agents.example\n") || !strings.Contains(out, "fake-uffici: a pairing code in the environment") {
		t.Fatalf("the code did not travel in the environment, out of argv:\n%s", out)
	}
	if strings.Contains(out, "K7QP") {
		t.Fatalf("the code was printed:\n%s", out)
	}
	if !strings.Contains(out, "pairing code: ****-**** (from UFFICI_PAIRING_CODE)") {
		t.Fatalf("the script did not say where the code came from:\n%s", out)
	}

	// A release from before the variable: its setup is handed the flag it knows,
	// and the script still prints only the masked code.
	old := fakeBinaryArchiveWith(t, false)
	for name, body := range releaseAssets(old, sha(old)+"  "+thisAsset()+"\n") {
		assets[name] = body
	}
	out, err = runInstallerEnv(t, &syscall.SysProcAttr{Setsid: true}, server, t.TempDir(), []string{"UFFICI_PAIRING_CODE=K7QP-2XMD"}, "--harness", "hermes", "--gateway-url", "https://agents.example")
	if err != nil || !strings.Contains(out, "fake-uffici setup --harness hermes --gateway-url https://agents.example --pairing-code K7QP-2XMD") {
		t.Fatalf("an older setup was not handed the code as its flag: %v\n%s", err, out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "uffici-install:") && strings.Contains(line, "K7QP") {
			t.Fatalf("the script printed the code: %q", line)
		}
	}
}

// 🔴 #3118 review — the latest path with cosign names its tag from the listing
// before anything is fetched, and a listing that names no final release tag is
// a refusal, never a pattern that any older signed release would satisfy.
func TestInstallScriptRefusesALatestWithoutAReleaseTag(t *testing.T) {
	archive := fakeBinaryArchive(t)
	assets := releaseAssets(archive, sha(archive)+"  "+thisAsset()+"\n")
	for _, listing := range []string{"/releases/tag/v1.0.0-rc.1", "/releases/tag/main", "/releases/tag/v1.0.0/x", ""} {
		var fetched atomic.Bool
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/uffici-ai/uffici-companion/releases/latest" {
				if listing != "" {
					w.Header().Set("Location", "https://github.com/uffici-ai/uffici-companion"+listing)
					w.WriteHeader(http.StatusFound)
				}
				return
			}
			if body, ok := assets[r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]]; ok {
				fetched.Store(true)
				_, _ = w.Write(body)
				return
			}
			http.NotFound(w, r)
		}))
		t.Cleanup(server.Close)
		out, err := runInstaller(t, server, t.TempDir(), "--dry-run")
		if err == nil || !strings.Contains(out, "names no release tag") || fetched.Load() {
			t.Fatalf("%s: the latest was fetched without a release tag to verify against: %v\n%s", listing, err, out)
		}
	}
}

// 🔴 #3118 review: with cosign, the latest is resolved from the web's redirect —
// the anonymous API's hourly budget (60 per address) is not on the path, so a
// machine behind a shared address installs with cosign exactly as without it.
func TestInstallScriptResolvesTheLatestWithoutTheAPI(t *testing.T) {
	archive := fakeBinaryArchive(t)
	assets := releaseAssets(archive, sha(archive)+"  "+thisAsset()+"\n")
	var apiHits atomic.Int32
	server := httptest.NewServer(withLatest(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/repos/") {
			apiHits.Add(1)
		}
		if body, ok := assets[r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]]; ok && strings.Contains(r.URL.Path, "/releases/download/v"+installLatest+"/") {
			_, _ = w.Write(body)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	log := filepath.Join(t.TempDir(), "cosign")
	out, err := runInstallerEnv(t, nil, server, t.TempDir(), []string{"FAKE_COSIGN_LOG=" + log}, "--dry-run")
	if err != nil {
		t.Fatalf("the latest could not be resolved with the API refusing: %v\n%s", err, out)
	}
	asked, _ := os.ReadFile(log)
	if !strings.Contains(out, "the latest release is v"+installLatest) || !strings.Contains(string(asked), "--certificate-identity "+ReleaseIdentity(CompanionRepo, installLatest)+" ") {
		t.Fatalf("the redirect's tag was not the one verified:\n%s\ncosign: %s", out, asked)
	}
	if apiHits.Load() != 0 {
		t.Fatalf("the anonymous API was asked %d times", apiHits.Load())
	}
}
