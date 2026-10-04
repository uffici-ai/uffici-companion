package setup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// goodManifest is a manifest every case starts from.
func goodManifest() map[string]any {
	return map[string]any{
		"manifestVersion": 1,
		"harness":         "hermes",
		"name":            "uffici-for-hermes",
		"version":         "0.1.2",
		"pluginDir":       "plugins/uffici-for-hermes",
		"homeEnv":         "HERMES_HOME",
		"defaultHome":     "~/.hermes",
		"enable":          []string{"hermes", "plugins", "enable", "uffici-for-hermes"},
		"configSet":       []string{"hermes", "config", "set"},
		"config":          []map[string]string{{"key": "gateway.platforms.uffici.enabled", "value": "true"}},
		"envPath":         []string{"hermes", "config", "env-path"},
		"env": map[string]string{
			"UFFICI_WIRE_SOCKET":     "${wireSocket}",
			"UFFICI_WIRE_TOKEN_FILE": "${wireTokenFile}",
		},
		"restart": "Restart the Hermes gateway so it loads the plugin.",
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestManifestSchemaIsTheEmbeddedDocument(t *testing.T) {
	raw := ManifestSchema()
	if !bytes.Contains(raw, []byte(`"additionalProperties": false`)) {
		t.Fatal("the embedded schema must refuse unknown fields")
	}
}

func TestParseManifestAcceptsTheShapeAPluginReleaseShips(t *testing.T) {
	m, err := ParseManifest(mustJSON(t, goodManifest()))
	if err != nil {
		t.Fatalf("a valid manifest was refused: %v", err)
	}
	if m.Name != "uffici-for-hermes" || m.EnvFile != ".env" || len(m.Config) != 1 {
		t.Fatalf("manifest read wrong: %+v", m)
	}
}

func TestParseManifestRefusesWhatItDoesNotKnow(t *testing.T) {
	cases := map[string]func(map[string]any){
		"an unknown field":                             func(m map[string]any) { m["postInstall"] = []string{"rm", "-rf", "/"} },
		"a missing required field":                     func(m map[string]any) { delete(m, "restart") },
		"a pluginDir leaving the home":                 func(m map[string]any) { m["pluginDir"] = "../elsewhere" },
		"a pluginDir that is the plugin folder itself": func(m map[string]any) { m["pluginDir"] = "plugins" },
		"a pluginDir not named after the plugin":       func(m map[string]any) { m["pluginDir"] = "plugins/other" },
		"an absolute pluginDir":                        func(m map[string]any) { m["pluginDir"] = "/etc/plugins" },
		"a non-UFFICI env name":                        func(m map[string]any) { m["env"] = map[string]string{"PATH": "x"} },
		"another manifest version":                     func(m map[string]any) { m["manifestVersion"] = 2 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := goodManifest()
			mutate(m)
			if _, err := ParseManifest(mustJSON(t, m)); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
	if _, err := ParseManifest([]byte("{not json")); err == nil {
		t.Fatal("malformed JSON was accepted")
	}
}

func TestParseManifestDotSegmentIsRefused(t *testing.T) {
	m := goodManifest()
	m["pluginDir"] = "plugins/./uffici-for-hermes"
	if _, err := ParseManifest(mustJSON(t, m)); err == nil {
		t.Fatal("a `.` segment was accepted")
	}
}

func TestSecureOrigin(t *testing.T) {
	for origin, ok := range map[string]bool{
		"https://github.com": true, "http://127.0.0.1:1234": true, "http://localhost:9": true, "http://[::1]:8": true,
		"http://github.com": false, "ftp://x": false, "http://localhost.example": false, "http://127.0.0.1.nip.io": false, "ftp://localhost": false,
	} {
		if got := secureOrigin(origin); got != ok {
			t.Fatalf("secureOrigin(%q) = %v", origin, got)
		}
	}
	r := Release{Owner: "o", Repo: "r", BaseURL: "http://example.invalid", Client: &http.Client{}}
	if _, err := r.Fetch(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "not https") {
		t.Fatalf("a plain-http origin off loopback was not refused: %v", err)
	}
}

func TestRenderEnvSubstitutesAndRefusesUnknownPlaceholders(t *testing.T) {
	m, _ := ParseManifest(mustJSON(t, goodManifest()))
	env, err := m.RenderEnv(map[string]string{"wireSocket": "/h/wire.sock", "wireTokenFile": "/h/wire-token"})
	if err != nil || env["UFFICI_WIRE_SOCKET"] != "/h/wire.sock" || env["UFFICI_WIRE_TOKEN_FILE"] != "/h/wire-token" {
		t.Fatalf("render: %v %v", env, err)
	}
	m.Env["UFFICI_X"] = "${nope}"
	if _, err := m.RenderEnv(map[string]string{"wireSocket": "a", "wireTokenFile": "b"}); err == nil || !strings.Contains(err.Error(), "${nope}") {
		t.Fatalf("an unknown placeholder was not refused by name: %v", err)
	}
}

func TestParseChecksums(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	got, err := ParseChecksums([]byte("\n" + sum + "  one.tar.gz\n" + strings.ToUpper(sum) + " *two.tar.gz\n"))
	if err != nil || got["one.tar.gz"] != sum || got["two.tar.gz"] != sum {
		t.Fatalf("parse: %v %v", got, err)
	}
	for _, bad := range []string{"", "not a line\n", "zz " + sum[2:] + "  x\n", sum + "\n"} {
		if _, err := ParseChecksums([]byte(bad)); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

// tarball builds a gzipped tar of files under top/, in memory.
func tarball(t *testing.T, top string, files map[string]string, extra func(*tar.Writer)) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: top + "/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: top + "/" + name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if extra != nil {
		extra(tw)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// releaseServer serves one release both the public way and the API way.
type releaseServer struct {
	*httptest.Server
	assets map[string][]byte
	token  string
	hits   []string
}

func newReleaseServer(t *testing.T, repo, token string, assets map[string][]byte) *releaseServer {
	t.Helper()
	rs := &releaseServer{assets: assets, token: token}
	mux := http.NewServeMux()
	// Public: /uffici-ai/<repo>/releases/latest/download/<asset> and /releases/download/v<ver>/<asset>
	mux.HandleFunc("/uffici-ai/"+repo+"/releases/", func(w http.ResponseWriter, r *http.Request) {
		rs.hits = append(rs.hits, r.URL.Path)
		if token != "" {
			http.Error(w, "private", http.StatusNotFound)
			return
		}
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if body, ok := assets[name]; ok {
			_, _ = w.Write(body)
			return
		}
		http.NotFound(w, r)
	})
	// API: /repos/uffici-ai/<repo>/releases/latest|tags/vX and /assets/<name>
	mux.HandleFunc("/repos/uffici-ai/"+repo+"/releases/", func(w http.ResponseWriter, r *http.Request) {
		rs.hits = append(rs.hits, r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "auth", http.StatusNotFound)
			return
		}
		if strings.Contains(r.URL.Path, "/assets/") {
			if r.Header.Get("Accept") != "application/octet-stream" {
				http.Error(w, "accept", http.StatusBadRequest)
				return
			}
			name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			if body, ok := assets[name]; ok {
				_, _ = w.Write(body)
				return
			}
			http.NotFound(w, r)
			return
		}
		type asset struct {
			Name, URL string
		}
		var list []asset
		for name := range assets {
			list = append(list, asset{Name: name, URL: rs.URL + "/repos/uffici-ai/" + repo + "/releases/assets/" + name})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"assets": list})
	})
	rs.Server = httptest.NewServer(mux)
	t.Cleanup(rs.Close)
	return rs
}

func (rs *releaseServer) release(repo, version string) Release {
	return Release{Owner: "uffici-ai", Repo: repo, Version: version, Token: rs.token, BaseURL: rs.URL, APIURL: rs.URL, Client: rs.Client()}
}

func TestReleaseFetchVerifiedPublicAndPrivate(t *testing.T) {
	body := []byte("hello")
	sums := []byte(sha(body) + "  uffici-for-hermes.tar.gz\n")
	for _, token := range []string{"", "ghp_test_token_not_a_secret"} {
		rs := newReleaseServer(t, "uffici-for-hermes", token, map[string][]byte{"uffici-for-hermes.tar.gz": body, ChecksumsFile: sums})
		got, err := rs.release("uffici-for-hermes", "").FetchVerified(context.Background(), "uffici-for-hermes.tar.gz")
		if err != nil || string(got) != "hello" {
			t.Fatalf("token=%v: %v %q", token != "", err, got)
		}
		pinned, err := rs.release("uffici-for-hermes", "0.1.2").Fetch(context.Background(), ChecksumsFile)
		if err != nil || !bytes.Equal(pinned, sums) {
			t.Fatalf("pinned fetch: %v", err)
		}
	}
}

func TestReleaseFetchVerifiedRefusals(t *testing.T) {
	body := []byte("hello")
	t.Run("checksum mismatch", func(t *testing.T) {
		rs := newReleaseServer(t, "r", "", map[string][]byte{"a.tar.gz": body, ChecksumsFile: []byte(strings.Repeat("00", 32) + "  a.tar.gz\n")})
		if _, err := rs.release("r", "").FetchVerified(context.Background(), "a.tar.gz"); err == nil || !strings.Contains(err.Error(), "checksum") {
			t.Fatalf("mismatch accepted: %v", err)
		}
	})
	t.Run("asset not vouched for", func(t *testing.T) {
		rs := newReleaseServer(t, "r", "", map[string][]byte{"a.tar.gz": body, ChecksumsFile: []byte(sha(body) + "  other.tar.gz\n")})
		if _, err := rs.release("r", "").FetchVerified(context.Background(), "a.tar.gz"); err == nil || !strings.Contains(err.Error(), "vouch") {
			t.Fatalf("unlisted asset accepted: %v", err)
		}
	})
	t.Run("no release", func(t *testing.T) {
		rs := newReleaseServer(t, "r", "", map[string][]byte{})
		if _, err := rs.release("r", "").Fetch(context.Background(), ChecksumsFile); err == nil || !strings.Contains(err.Error(), "404") {
			t.Fatalf("404 not reported: %v", err)
		}
	})
	t.Run("private without a token", func(t *testing.T) {
		rs := newReleaseServer(t, "r", "secret", map[string][]byte{ChecksumsFile: []byte("x")})
		r := rs.release("r", "")
		r.Token = ""
		if _, err := r.Fetch(context.Background(), ChecksumsFile); err == nil {
			t.Fatal("a private release answered without a token")
		}
	})
	t.Run("an asset URL outside the API is refused with the token unsent", func(t *testing.T) {
		var sentBearerTo []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "" {
				sentBearerTo = append(sentBearerTo, r.URL.Path)
			}
			if strings.HasSuffix(r.URL.Path, "/releases/latest") {
				_, _ = w.Write([]byte(`{"assets":[{"url":"http://127.0.0.1:1/elsewhere","name":"x"}]}`))
				return
			}
			http.NotFound(w, r)
		}))
		t.Cleanup(srv.Close)
		r := Release{Owner: "o", Repo: "r", Token: "tok", BaseURL: srv.URL, APIURL: srv.URL, Client: srv.Client()}
		if _, err := r.Fetch(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "outside its API") {
			t.Fatalf("an off-API asset URL was followed: %v", err)
		}
		if len(sentBearerTo) != 1 {
			t.Fatalf("the token went to %v; only the listing may see it", sentBearerTo)
		}
	})
	t.Run("asset missing from the API listing", func(t *testing.T) {
		rs := newReleaseServer(t, "r", "tok", map[string][]byte{ChecksumsFile: []byte("x")})
		if _, err := rs.release("r", "").Fetch(context.Background(), "nope.tar.gz"); err == nil || !strings.Contains(err.Error(), "no asset named") {
			t.Fatalf("missing asset not named: %v", err)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		r := Release{Owner: "o", Repo: "r", BaseURL: "http://127.0.0.1:1", Client: &http.Client{}}
		if _, err := r.Fetch(context.Background(), "x"); err == nil || strings.Contains(err.Error(), "127.0.0.1") {
			t.Fatalf("an unreachable host must be reported without its URL: %v", err)
		}
	})
	t.Run("too large", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", fmt.Sprint(MaxAssetBytes+2))
			_, _ = w.Write(bytes.Repeat([]byte("x"), MaxAssetBytes+2))
		}))
		t.Cleanup(srv.Close)
		r := Release{Owner: "o", Repo: "r", BaseURL: srv.URL, Client: srv.Client()}
		if _, err := r.Fetch(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "larger") {
			t.Fatalf("oversize accepted: %v", err)
		}
	})
	if d := (Release{Owner: "o", Repo: "r", Version: "1.2.3"}).Describe(); d != "o/r v1.2.3" {
		t.Fatal(d)
	}
}

func TestExtractTarRefusesEscapesAndOddEntries(t *testing.T) {
	good := tarball(t, "uffici-for-hermes", map[string]string{"a.py": "x", "sub/b.py": "y"}, nil)
	dest := t.TempDir()
	top, err := extractTar(good, dest, "uffici-for-hermes")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(top, "sub", "b.py")); err != nil {
		t.Fatal("nested file not extracted")
	}
	cases := map[string]func(*tar.Writer){
		"a symlink": func(tw *tar.Writer) {
			_ = tw.WriteHeader(&tar.Header{Name: "uffici-for-hermes/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"})
		},
		"a path outside the top": func(tw *tar.Writer) {
			_ = tw.WriteHeader(&tar.Header{Name: "elsewhere/x", Typeflag: tar.TypeReg, Size: 1})
			_, _ = tw.Write([]byte("z"))
		},
		"a dot-dot": func(tw *tar.Writer) {
			_ = tw.WriteHeader(&tar.Header{Name: "uffici-for-hermes/../x", Typeflag: tar.TypeReg, Size: 1})
			_, _ = tw.Write([]byte("z"))
		},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			bad := tarball(t, "uffici-for-hermes", map[string]string{"a.py": "x"}, extra)
			if _, err := extractTar(bad, t.TempDir(), "uffici-for-hermes"); err == nil {
				t.Fatalf("%s was extracted", name)
			}
		})
	}
	if _, err := extractTar([]byte("not gzip"), t.TempDir(), "x"); err == nil {
		t.Fatal("non-gzip accepted")
	}
	empty := tarball(t, "uffici-for-hermes", map[string]string{}, nil)
	if _, err := extractTar(empty, t.TempDir(), "uffici-for-hermes"); err == nil {
		t.Fatal("an archive with no files was accepted")
	}
}

func TestMergeEnvFileReplacesAndAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", ".env")
	if err := mergeEnvFile(path, map[string]string{"UFFICI_B": "1", "UFFICI_A": "old"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("OTHER=keep\nexport UFFICI_A=old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mergeEnvFile(path, map[string]string{"UFFICI_A": "new", "UFFICI_B": "2"}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "OTHER=keep\nUFFICI_A=new\nUFFICI_B=2\n" {
		t.Fatalf("merged wrong:\n%s", got)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("env file mode %o, want 0600", info.Mode().Perm())
	}
}

func TestPlanServicePerOS(t *testing.T) {
	linux, err := planService("linux", "/home/u", "/home/u/.local/bin/uffici", "https://agents.example", "/home/u/.uffici", "1000", LogSettings{})
	if err != nil || !strings.HasSuffix(linux.path, ".config/systemd/user/"+ServiceLabel+".service") || !strings.Contains(linux.content, `ExecStart="/home/u/.local/bin/uffici" run`) || !strings.Contains(linux.content, `Environment="UFFICI_GATEWAY_URL=https://agents.example"`) {
		t.Fatalf("linux plan: %+v %v", linux, err)
	}
	// 🔴 A re-run of the line replaces the RUNNING companion (#2213): the process
	// reads its binary and its agent key once, at start, so a re-pair or an
	// upgrade that only `enable --now`s a running unit leaves it on the old ones
	// — and the old key stops resolving when its overlap ends.
	var linuxCommands []string
	for _, argv := range linux.commands {
		linuxCommands = append(linuxCommands, strings.Join(argv, " "))
	}
	if want := []string{
		"systemctl --user daemon-reload",
		"systemctl --user enable " + ServiceLabel + ".service",
		"systemctl --user restart " + ServiceLabel + ".service",
	}; strings.Join(linuxCommands, "\n") != strings.Join(want, "\n") {
		t.Fatalf("linux commands = %q, want %q", linuxCommands, want)
	}
	mac, err := planService("darwin", "/Users/u", "/usr/local/bin/uffici", "https://a&b", "/Users/u/.uffici", "501", LogSettings{})
	if err != nil || !strings.HasSuffix(mac.path, "Library/LaunchAgents/"+ServiceLabel+".plist") || !strings.Contains(mac.content, "https://a&amp;b") || mac.commands[1][2] != "gui/501" {
		t.Fatalf("darwin plan: %+v %v", mac, err)
	}
	odd, _ := planService("linux", "/home/o d", "/home/o d/bin/uffici", "https://a", "/home/o d/.uffici%", "7", LogSettings{})
	if !strings.Contains(odd.content, `ExecStart="/home/o d/bin/uffici" run`) || !strings.Contains(odd.content, `UFFICI_HOME=/home/o d/.uffici%%"`) {
		t.Fatalf("a home with a space or %% is not quoted for systemd:\n%s", odd.content)
	}
	if _, err := planService("plan9", "/h", "/b", "u", "/h/.uffici", "1", LogSettings{}); err == nil {
		t.Fatal("an unknown OS got a service plan")
	}
	// 🔴 The machine's log switches ride the service (#2264): on and warn unless
	// setup was told otherwise, and exactly what it was told otherwise.
	if !strings.Contains(linux.content, `Environment="UFFICI_LOG_SHIPPING=on"`) ||
		!strings.Contains(linux.content, `Environment="UFFICI_LOG_LEVEL_HARNESS=warn"`) {
		t.Fatalf("the default unit carries no log settings:\n%s", linux.content)
	}
	off, _ := planService("linux", "/home/u", "/b", "https://a", "/home/u/.uffici", "1000", LogSettings{Shipping: "off", HarnessLevel: "error"})
	if !strings.Contains(off.content, `Environment="UFFICI_LOG_SHIPPING=off"`) ||
		!strings.Contains(off.content, `Environment="UFFICI_LOG_LEVEL_HARNESS=error"`) {
		t.Fatalf("the unit lost the machine's log settings:\n%s", off.content)
	}
	macOff, _ := planService("darwin", "/Users/u", "/b", "https://a", "/Users/u/.uffici", "501", LogSettings{Shipping: "off", HarnessLevel: "info"})
	if !strings.Contains(macOff.content, `<key>UFFICI_LOG_SHIPPING</key><string>off</string>`) ||
		!strings.Contains(macOff.content, `<key>UFFICI_LOG_LEVEL_HARNESS</key><string>info</string>`) {
		t.Fatalf("the plist lost the machine's log settings:\n%s", macOff.content)
	}
}

// recorder is a Runner that records argv and answers as told.
type recorder struct {
	calls   [][]string
	answers map[string]string
	fail    map[string]bool
}

func (r *recorder) Run(_ context.Context, name string, args ...string) (string, error) {
	argv := append([]string{name}, args...)
	r.calls = append(r.calls, argv)
	key := strings.Join(argv, " ")
	if r.fail[key] {
		return "", errors.New("boom")
	}
	return r.answers[key], nil
}

func (r *recorder) Stdout(ctx context.Context, name string, args ...string) (string, error) {
	return r.Run(ctx, name, args...)
}

func (r *recorder) ran(prefix string) bool {
	for _, c := range r.calls {
		if strings.HasPrefix(strings.Join(c, " "), prefix) {
			return true
		}
	}
	return false
}

// rig is one whole setup against a loopback release and temporary homes.
type rig struct {
	opts   Options
	runner *recorder
	out    *bytes.Buffer
	home   string // the harness home
	paired int
	doctor int
	rs     *releaseServer
}

func newRig(t *testing.T, manifest map[string]any, dryRun bool) *rig {
	t.Helper()
	body := tarball(t, "uffici-for-hermes", map[string]string{
		ManifestFile:  string(mustJSON(t, manifest)),
		"adapter.py":  "print('hi')\n",
		"plugin.yaml": "name: uffici-for-hermes\n",
	}, nil)
	sums := []byte(sha(body) + "  uffici-for-hermes.tar.gz\n")
	rs := newReleaseServer(t, "uffici-for-hermes", "", map[string][]byte{"uffici-for-hermes.tar.gz": body, ChecksumsFile: sums})
	userHome := t.TempDir()
	harnessHome := filepath.Join(userHome, ".hermes")
	r := &rig{runner: &recorder{answers: map[string]string{"hermes config env-path": filepath.Join(harnessHome, "custom.env") + "\n"}, fail: map[string]bool{}}, out: &bytes.Buffer{}, home: harnessHome, rs: rs}
	r.opts = Options{
		Harness: "hermes", HarnessVersion: "0.21.5", GatewayURL: "https://agents.example", DryRun: dryRun,
		GOOS: "linux", UserHome: userHome, CompanionHome: filepath.Join(userHome, ".uffici"), Binary: "/opt/uffici", UID: "1000",
		Env: func(string) string { return "" },
		LookPath: func(name string) (string, error) {
			if name == "hermes" {
				return "/usr/bin/hermes", nil
			}
			return "", errors.New("not found")
		},
		Runner:  r.runner,
		Release: func(repo string) Release { return rs.release(repo, "") },
		Gateway: noGatewayVersion,
		Pair:    func(context.Context) int { r.paired++; return ExitOK },
		Doctor:  func(context.Context) int { r.doctor++; return ExitOK },
	}
	return r
}

func TestRunInstallsEverythingInOrder(t *testing.T) {
	r := newRig(t, goodManifest(), false)
	if code := Run(context.Background(), r.opts, r.out); code != ExitOK {
		t.Fatalf("exit %d:\n%s", code, r.out)
	}
	unit, err := os.ReadFile(filepath.Join(r.opts.UserHome, ".config", "systemd", "user", ServiceLabel+".service"))
	if err != nil || !strings.Contains(string(unit), `ExecStart="/opt/uffici" run`) {
		t.Fatalf("unit not written: %v", err)
	}
	if !r.runner.ran("systemctl --user restart") {
		t.Fatal("the service was not (re)started")
	}
	if r.paired != 1 {
		t.Fatal("pairing did not run once")
	}
	if _, err := os.Stat(filepath.Join(r.home, "plugins", "uffici-for-hermes", "adapter.py")); err != nil {
		t.Fatal("the plugin was not copied into the harness home's plugin directory")
	}
	if !r.runner.ran("hermes config set gateway.platforms.uffici.enabled true") {
		t.Fatal("the configuration key was not set")
	}
	env, err := os.ReadFile(filepath.Join(r.home, "custom.env"))
	if err != nil || !strings.Contains(string(env), "UFFICI_WIRE_SOCKET="+filepath.Join(r.opts.CompanionHome, "wire.sock")) || !strings.Contains(string(env), "UFFICI_WIRE_TOKEN_FILE="+filepath.Join(r.opts.CompanionHome, "wire-token")) {
		t.Fatalf("env not written where `hermes config env-path` said: %v\n%s", err, env)
	}
	if !r.runner.ran("hermes plugins enable uffici-for-hermes") {
		t.Fatal("the plugin was not enabled")
	}
	if r.doctor != 1 {
		t.Fatal("the doctor did not run")
	}
	// What `uffici upgrade` reads (#2250): the harness, its resolved home, the
	// plugin's directory and the agents host — and no secret.
	record, err := ReadRecord(r.opts.CompanionHome)
	if err != nil || record.Harness != "hermes" || record.HarnessHome != r.home ||
		record.PluginDir != "plugins/uffici-for-hermes" || record.GatewayURL != "https://agents.example" || !record.Service {
		t.Fatalf("the setup record is %+v, %v", record, err)
	}
	// Pairing runs BEFORE the service, and each step is announced before it acts.
	text := r.out.String()
	if strings.Index(text, "step: pair") > strings.Index(text, "step: write the service") {
		t.Fatalf("the service was installed before pairing:\n%s", text)
	}
	if strings.Index(text, "step: write the service") > strings.Index(text, "done: write the service") {
		t.Fatalf("a step was not announced before it ran:\n%s", text)
	}
	if !strings.Contains(r.out.String(), "Restart the Hermes gateway") {
		t.Fatalf("the restart line was not printed:\n%s", r.out)
	}
	// A second run replaces the plugin in place, keeps the env file merged, and
	// does NOT pair again once the key exists.
	if err := os.MkdirAll(r.opts.CompanionHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.opts.CompanionHome, "agent-key"), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := Run(context.Background(), r.opts, r.out); code != ExitOK {
		t.Fatalf("second run exit %d:\n%s", code, r.out)
	}
	if r.paired != 1 || !strings.Contains(r.out.String(), "already paired") {
		t.Fatalf("a re-run with the key in place must not pair again (paired=%d):\n%s", r.paired, r.out)
	}
	env, _ = os.ReadFile(filepath.Join(r.home, "custom.env"))
	if strings.Count(string(env), "UFFICI_WIRE_SOCKET=") != 1 {
		t.Fatalf("env duplicated on re-run:\n%s", env)
	}
}

func TestRunDryRunPerformsNothing(t *testing.T) {
	r := newRig(t, goodManifest(), true)
	if code := Run(context.Background(), r.opts, r.out); code != ExitOK {
		t.Fatalf("exit %d:\n%s", code, r.out)
	}
	if len(r.runner.calls) != 0 || r.paired != 0 || r.doctor != 0 {
		t.Fatalf("a dry run acted: calls=%v paired=%d doctor=%d", r.runner.calls, r.paired, r.doctor)
	}
	if _, err := os.Stat(r.home); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a dry run touched the harness home")
	}
	for _, want := range []string{"would write the service definition", "would pair", "would install the plugin into", "would run hermes config set", "would run hermes plugins enable", "would run uffici doctor", "plugin uffici-for-hermes 0.1.2"} {
		if !strings.Contains(r.out.String(), want) {
			t.Fatalf("the plan does not say %q:\n%s", want, r.out)
		}
	}
	if _, err := ReadRecord(r.opts.CompanionHome); !errors.Is(err, ErrNoRecord) {
		t.Fatalf("a dry run wrote the setup record: %v", err)
	}
}

// 🔴 The re-pair of #2209: the portal's command carries a code, and running it
// on a machine that is already paired pairs it AGAIN — the new key replaces the
// old one — where a re-run without a code keeps the key it has.
func TestRunWithAPortalCodePairsOverAnExistingKey(t *testing.T) {
	r := newRig(t, goodManifest(), false)
	if err := os.MkdirAll(r.opts.CompanionHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.opts.CompanionHome, "agent-key"), []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.opts.PairingCode = "K7QP-2XMD"
	if code := Run(context.Background(), r.opts, r.out); code != ExitOK {
		t.Fatalf("exit %d:\n%s", code, r.out)
	}
	if r.paired != 1 {
		t.Fatalf("a code did not pair over the existing key (paired=%d):\n%s", r.paired, r.out)
	}
	text := r.out.String()
	for _, want := range []string{"pair this machine again with the code from the portal", "replaces", "overlap"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the step does not say %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "K7QP-2XMD") || strings.Contains(text, "already paired") {
		t.Fatalf("the code was printed, or the key was kept:\n%s", text)
	}

	// The same machine, re-run WITHOUT a code: the key it has is kept.
	r.opts.PairingCode = ""
	if code := Run(context.Background(), r.opts, r.out); code != ExitOK {
		t.Fatalf("exit %d:\n%s", code, r.out)
	}
	if r.paired != 1 || !strings.Contains(r.out.String(), "already paired") {
		t.Fatalf("a re-run without a code paired again (paired=%d)", r.paired)
	}
}

func TestRunDryRunWithAPortalCodeOnlySaysSo(t *testing.T) {
	r := newRig(t, goodManifest(), true)
	r.opts.PairingCode = "K7QP-2XMD"
	if code := Run(context.Background(), r.opts, r.out); code != ExitOK {
		t.Fatalf("exit %d:\n%s", code, r.out)
	}
	if r.paired != 0 || len(r.runner.calls) != 0 {
		t.Fatalf("a dry run acted: paired=%d calls=%v", r.paired, r.runner.calls)
	}
	if !strings.Contains(r.out.String(), "would pair with the code from the portal") || strings.Contains(r.out.String(), "K7QP-2XMD") {
		t.Fatalf("the plan:\n%s", r.out)
	}
}

func TestRunRefusals(t *testing.T) {
	t.Run("nothing to do", func(t *testing.T) {
		var out bytes.Buffer
		if code := Run(context.Background(), Options{NoPair: true, NoService: true}, &out); code != ExitConfig {
			t.Fatalf("exit %d", code)
		}
	})
	t.Run("no gateway url", func(t *testing.T) {
		var out bytes.Buffer
		if code := Run(context.Background(), Options{Harness: "hermes"}, &out); code != ExitConfig || !strings.Contains(out.String(), "gateway-url") {
			t.Fatalf("exit %d: %s", code, out.String())
		}
	})
	t.Run("a config tool missing fails before the plugin directory is touched", func(t *testing.T) {
		r := newRig(t, goodManifest(), false)
		r.opts.NoService, r.opts.NoPair = true, true
		r.opts.LookPath = func(name string) (string, error) {
			if name == "hermes" {
				return "", errors.New("no")
			}
			return "/bin/x", nil
		}
		if code := Run(context.Background(), r.opts, r.out); code != ExitFailed {
			t.Fatalf("exit %d:\n%s", code, r.out)
		}
		if _, err := os.Stat(r.home); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("the harness home was touched although a tool was missing")
		}
	})
	t.Run("a relative harness home is refused", func(t *testing.T) {
		r := newRig(t, goodManifest(), false)
		r.opts.NoService, r.opts.NoPair = true, true
		r.opts.HarnessHome = "relative/home"
		if code := Run(context.Background(), r.opts, r.out); code != ExitFailed || !strings.Contains(r.out.String(), "not an absolute path") {
			t.Fatalf("exit %d:\n%s", code, r.out)
		}
	})
	t.Run("an env-path command whose output is not a path falls back to .env", func(t *testing.T) {
		r := newRig(t, goodManifest(), false)
		r.opts.NoService, r.opts.NoPair = true, true
		r.runner.answers = map[string]string{"hermes config env-path": "DeprecationWarning: something\nnot/absolute\n"}
		if code := Run(context.Background(), r.opts, r.out); code != ExitOK {
			t.Fatalf("exit %d:\n%s", code, r.out)
		}
		if _, err := os.Stat(filepath.Join(r.home, ".env")); err != nil {
			t.Fatalf("the env file did not fall back to .env: %v\n%s", err, r.out)
		}
	})
	t.Run("the harness tool is not on the PATH", func(t *testing.T) {
		r := newRig(t, goodManifest(), false)
		r.opts.LookPath = func(string) (string, error) { return "", errors.New("no") }
		if code := Run(context.Background(), r.opts, r.out); code != ExitFailed || !strings.Contains(r.out.String(), "hermes is not on the PATH") {
			t.Fatalf("exit %d:\n%s", code, r.out)
		}
	})
	t.Run("the manifest is for another harness", func(t *testing.T) {
		m := goodManifest()
		m["harness"] = "openclaw"
		r := newRig(t, m, false)
		if code := Run(context.Background(), r.opts, r.out); code != ExitFailed || !strings.Contains(r.out.String(), `for the harness "openclaw"`) {
			t.Fatalf("exit %d:\n%s", code, r.out)
		}
	})
	t.Run("config without a configSet command", func(t *testing.T) {
		m := goodManifest()
		delete(m, "configSet")
		r := newRig(t, m, false)
		if code := Run(context.Background(), r.opts, r.out); code != ExitFailed || !strings.Contains(r.out.String(), "configSet") {
			t.Fatalf("exit %d:\n%s", code, r.out)
		}
	})
	t.Run("pairing fails and stops the rest", func(t *testing.T) {
		r := newRig(t, goodManifest(), false)
		r.opts.Pair = func(context.Context) int { return 7 }
		if code := Run(context.Background(), r.opts, r.out); code != 7 || r.runner.ran("hermes") {
			t.Fatalf("exit %d, hermes ran=%v", code, r.runner.ran("hermes"))
		}
	})
	t.Run("the service command fails", func(t *testing.T) {
		r := newRig(t, goodManifest(), false)
		r.runner.fail["systemctl --user daemon-reload"] = true
		if code := Run(context.Background(), r.opts, r.out); code != ExitFailed || !strings.Contains(r.out.String(), "starting the service") {
			t.Fatalf("exit %d:\n%s", code, r.out)
		}
	})
	t.Run("the enable command fails", func(t *testing.T) {
		r := newRig(t, goodManifest(), false)
		r.runner.fail["hermes plugins enable uffici-for-hermes"] = true
		if code := Run(context.Background(), r.opts, r.out); code != ExitFailed || !strings.Contains(r.out.String(), "enabling the plugin") {
			t.Fatalf("exit %d:\n%s", code, r.out)
		}
	})
	t.Run("an unknown OS", func(t *testing.T) {
		r := newRig(t, goodManifest(), false)
		r.opts.GOOS = "plan9"
		if code := Run(context.Background(), r.opts, r.out); code != ExitFailed {
			t.Fatalf("exit %d:\n%s", code, r.out)
		}
	})
	t.Run("the release is unreachable", func(t *testing.T) {
		r := newRig(t, goodManifest(), false)
		r.opts.NoService, r.opts.NoPair = true, true
		r.opts.Release = func(string) Release {
			return Release{Owner: "o", Repo: "r", BaseURL: "http://127.0.0.1:1", Client: &http.Client{}}
		}
		if code := Run(context.Background(), r.opts, r.out); code != ExitFailed {
			t.Fatalf("exit %d:\n%s", code, r.out)
		}
	})
}

// A refusal is the one line a person reads when the install stops, so it names
// the program once: `Run` adds `uffici setup: `, and nothing in the package adds
// a second `setup: ` of its own (#2197 — the first dry run against a real
// release printed `uffici setup: setup: … (404)`).
func TestRunRefusalNamesTheProgramOnce(t *testing.T) {
	unknownField := goodManifest()
	unknownField["unknownField"] = true
	cases := map[string]struct {
		manifest map[string]any
		arrange  func(r *rig)
	}{
		"the release has no such asset (404)": {goodManifest(), func(r *rig) {
			r.opts.NoService, r.opts.NoPair = true, true
			empty := newReleaseServer(t, "uffici-for-hermes", "", map[string][]byte{})
			r.opts.Release = func(repo string) Release { return empty.release(repo, "") }
		}},
		"no service shape for the OS": {goodManifest(), func(r *rig) {
			r.opts.NoPair = true
			r.opts.GOOS = "plan9"
		}},
		"a manifest that does not match the schema": {unknownField, func(r *rig) {
			r.opts.NoService, r.opts.NoPair = true, true
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, c.manifest, false)
			c.arrange(r)
			if code := Run(context.Background(), r.opts, r.out); code != ExitFailed {
				t.Fatalf("exit %d:\n%s", code, r.out)
			}
			lines := strings.Split(strings.TrimRight(r.out.String(), "\n"), "\n")
			refusal := lines[len(lines)-1]
			if !strings.HasPrefix(refusal, "uffici setup: ") {
				t.Fatalf("the refusal does not start with the program's name: %q", refusal)
			}
			if n := strings.Count(refusal, "setup: "); n != 1 {
				t.Fatalf("the refusal names setup %d times: %q", n, refusal)
			}
		})
	}
}

// The Run cases above sample three refusal paths; this holds every error the
// package can build to the same rule, so a new `setup: ` prefix anywhere in it
// fails here rather than on the one line a person reads (#2197).
func TestNoErrorInThePackageCarriesItsOwnPrefix(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	scanned := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		for i, line := range strings.Split(string(source), "\n") {
			if strings.Contains(line, `"setup: `) {
				t.Errorf("%s:%d builds a message starting `setup: ` — Run already names the program: %s", file, i+1, strings.TrimSpace(line))
			}
		}
	}
	if scanned < 4 {
		t.Fatalf("scanned %d source files — the glob no longer finds the package", scanned)
	}
}

func TestRunHonoursHarnessHomeAndDefaults(t *testing.T) {
	r := newRig(t, goodManifest(), false)
	r.opts.NoService, r.opts.NoPair = true, true
	custom := filepath.Join(t.TempDir(), "elsewhere")
	r.opts.HarnessHome = custom
	// No envPath answer: the env file falls back to `.env` under the home.
	r.runner.answers = map[string]string{}
	if code := Run(context.Background(), r.opts, r.out); code != ExitOK {
		t.Fatalf("exit %d:\n%s", code, r.out)
	}
	if _, err := os.Stat(filepath.Join(custom, "plugins", "uffici-for-hermes", "plugin.yaml")); err != nil {
		t.Fatal("--harness-home was not honoured")
	}
	if _, err := os.Stat(filepath.Join(custom, ".env")); err != nil {
		t.Fatal("without an envPath answer the env file must be .env under the home")
	}
	// The env var names the home when no flag does.
	r2 := newRig(t, goodManifest(), false)
	r2.opts.NoService, r2.opts.NoPair = true, true
	viaEnv := filepath.Join(t.TempDir(), "via-env")
	r2.opts.Env = func(key string) string {
		if key == "HERMES_HOME" {
			return viaEnv
		}
		return ""
	}
	if code := Run(context.Background(), r2.opts, r2.out); code != ExitOK {
		t.Fatalf("exit %d:\n%s", code, r2.out)
	}
	if _, err := os.Stat(filepath.Join(viaEnv, "plugins", "uffici-for-hermes")); err != nil {
		t.Fatal("HERMES_HOME was not honoured")
	}
	if got := expandHome("~", "/u"); got != "/u" {
		t.Fatal(got)
	}
	if got := expandHome("/abs", "/u"); got != "/abs" {
		t.Fatal(got)
	}
}

func TestExecRunnerReportsFailure(t *testing.T) {
	if _, err := (ExecRunner{}).Run(context.Background(), "sh", "-c", "exit 3"); err == nil {
		t.Fatal("a failing command was reported as success")
	}
	if out, err := (ExecRunner{}).Run(context.Background(), "sh", "-c", "echo ok"); err != nil || strings.TrimSpace(out) != "ok" {
		t.Fatalf("%q %v", out, err)
	}
}

func TestExecRunnerStdoutIgnoresStderr(t *testing.T) {
	out, err := (ExecRunner{}).Stdout(context.Background(), "sh", "-c", "echo warn >&2; echo /the/path")
	if err != nil || strings.TrimSpace(out) != "/the/path" {
		t.Fatalf("stdout alone was expected: %q %v", out, err)
	}
	if _, err := (ExecRunner{}).Stdout(context.Background(), "sh", "-c", "exit 2"); err == nil {
		t.Fatal("a failing command was reported as success")
	}
	if _, err := (ExecRunner{}).Run(context.Background(), "sh", "-c", "echo bus failed >&2; exit 1"); err == nil || !strings.Contains(err.Error(), "bus failed") {
		t.Fatalf("a failing command's own output must reach the error: %v", err)
	}
	if got := lastLine("a\n b \n\n"); got != "b" {
		t.Fatal(got)
	}
}

func TestExtractTarSkipsPaxGlobalHeaderAndCapsSize(t *testing.T) {
	withGlobal := tarball(t, "uffici-for-hermes", map[string]string{"a.py": "x"}, func(tw *tar.Writer) {
		_ = tw.WriteHeader(&tar.Header{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": "git archive"}})
	})
	if _, err := extractTar(withGlobal, t.TempDir(), "uffici-for-hermes"); err != nil {
		t.Fatalf("a pax global header must be skipped, not refused: %v", err)
	}
	// One file past the cap is a refusal, never a truncation.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	size := int64(MaxAssetBytes + 1)
	if err := tw.WriteHeader(&tar.Header{Name: "uffici-for-hermes/big", Typeflag: tar.TypeReg, Mode: 0o644, Size: size}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(tw, zeroReader{}, size); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = gz.Close()
	if _, err := extractTar(buf.Bytes(), t.TempDir(), "uffici-for-hermes"); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("an oversize file was extracted: %v", err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func TestReleaseDefaultsAndTimeouts(t *testing.T) {
	r := Release{Owner: "o", Repo: "r"}
	if r.base() != "https://github.com" || r.api() != "https://api.github.com" || r.client() != http.DefaultClient {
		t.Fatal("the defaults are GitHub and the default client")
	}
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte("late"))
	}))
	t.Cleanup(slow.Close)
	quick := Release{Owner: "o", Repo: "r", BaseURL: slow.URL, Client: &http.Client{Timeout: 20 * time.Millisecond}}
	if _, err := quick.Fetch(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("a timeout must be classified as one: %v", err)
	}
}

func TestInstallServiceRefusesAnUnwritableHome(t *testing.T) {
	r := newRig(t, goodManifest(), false)
	r.opts.NoPair = true
	r.opts.Harness = ""
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.opts.UserHome = file // the unit's directory cannot be created under a file
	if code := Run(context.Background(), r.opts, r.out); code != ExitFailed || !strings.Contains(r.out.String(), "installing the service") {
		t.Fatalf("exit %d:\n%s", code, r.out)
	}
}

// restartableRig is a setup whose harness runs as its own user service and
// whose manifest declares how to restart it — a restart is SAFE, so only the
// person's say decides it (#2278).
func restartableRig(t *testing.T) *rig {
	t.Helper()
	manifest := goodManifest()
	manifest["restartCommand"] = []string{"hermes", "gateway", "restart"}
	manifest["restartService"] = map[string]string{"systemd": "hermes-gateway", "launchd": "ai.hermes.gateway"}
	r := newRig(t, manifest, false)
	unit := filepath.Join(r.opts.UserHome, ".config", "systemd", "user", "hermes-gateway.service")
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unit, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return r
}

// 🔴 #2278: the install restarts the harness when the person says so, and only
// AFTER the companion's service — so the plugin it loads says hello to the
// companion that will serve it.
func TestRunRestartsTheHarnessOnYesAfterTheCompanion(t *testing.T) {
	r := restartableRig(t)
	r.opts.AssumeYes = true
	if code := Run(context.Background(), r.opts, r.out); code != ExitOK {
		t.Fatalf("exit %d:\n%s", code, r.out)
	}
	companion, harness := -1, -1
	for i, c := range r.runner.calls {
		switch strings.Join(c, " ") {
		case "systemctl --user restart " + ServiceLabel + ".service":
			companion = i
		case "hermes gateway restart":
			harness = i
		}
	}
	if harness < 0 || companion < 0 || harness < companion {
		t.Fatalf("want the companion restarted, then the harness; ran %v", r.runner.calls)
	}
	if !strings.Contains(r.out.String(), "hermes: restarted") {
		t.Fatalf("the outcome was not said:\n%s", r.out)
	}
}

// 🔴 #2278: with no flag and no terminal, the install leaves the harness alone
// and says how — and a terminal's answer, or a flag, decides otherwise.
func TestRunAsksBeforeRestartingTheHarness(t *testing.T) {
	for _, tc := range []struct {
		name     string
		choice   RestartChoice
		confirm  func(string) (bool, bool)
		restarts bool
		line     string
	}{
		{"no flag and no terminal", RestartAsk, nil, false, "hermes: not restarted — no terminal to ask"},
		{"asked, yes", RestartAsk, func(string) (bool, bool) { return true, true }, true, "hermes: restarted"},
		{"asked, no", RestartAsk, func(string) (bool, bool) { return false, true }, false, "hermes: not restarted — you said no"},
		{"--no-restart-harness beats --yes", RestartNo, nil, false, "hermes: not restarted — you said no"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := restartableRig(t)
			r.opts.RestartHarness, r.opts.Confirm = tc.choice, tc.confirm
			r.opts.AssumeYes = tc.choice == RestartNo
			if code := Run(context.Background(), r.opts, r.out); code != ExitOK {
				t.Fatalf("exit %d:\n%s", code, r.out)
			}
			if got := r.runner.ran("hermes gateway restart"); got != tc.restarts {
				t.Fatalf("restarted = %v, want %v:\n%s", got, tc.restarts, r.out)
			}
			if !strings.Contains(r.out.String(), tc.line) {
				t.Fatalf("lacks %q:\n%s", tc.line, r.out)
			}
		})
	}
}
