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
		script += "if [ \"$2\" = -h ]; then printf '  -version string\\n' >&2; exit 2; fi\n"
	}
	script += "printf 'fake-uffici %s\\n' \"$*\"\n"
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
		"UFFICI_VERSION=",
		// Not run under sudo, whatever the developer's shell is (#3128).
		"SUDO_USER=",
		"UFFICI_HOME=",
		// 🔴 A run with no --gateway-url asks this loopback server, never the
		// production default: it names no release, so the latest is fetched.
		"UFFICI_GATEWAY_URL="+server.URL,
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
	server.Start()
	t.Cleanup(server.Close)
	cmd := exec.Command("sh", filepath.Join(moduleRoot(t), "install.sh"), "--dry-run", "--harness", "hermes")
	cmd.Env = append(os.Environ(),
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

// ── Which release, and which gateway (#2382) ────────────────────────────────

// trainServer serves one companion release by tag (never `latest`, unless
// latest is set) and the gateway's `/v1/release` answer, from one origin.
func trainServer(t *testing.T, tag string, archive []byte, release string, latest bool, sawAuth *bool) *httptest.Server {
	t.Helper()
	assets := map[string][]byte{thisAsset(): archive, ChecksumsFile: []byte(sha(archive) + "  " + thisAsset() + "\n")}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			prefix = "/uffici-ai/uffici-companion/releases/latest/download/"
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
				"PATH="+shims+":"+os.Getenv("PATH"),
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
