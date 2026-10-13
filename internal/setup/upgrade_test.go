package setup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// releases serves several repositories' latest releases, the API way (the
// listing, `tag_name`, assets by API URL) — what `uffici upgrade` reads.
type releases struct {
	*httptest.Server
	token  string
	latest map[string]string            // repo → version
	assets map[string]map[string][]byte // repo → asset → body
}

func newReleases(t *testing.T, token string) *releases {
	t.Helper()
	rs := &releases{token: token, latest: map[string]string{}, assets: map[string]map[string][]byte{}}
	rs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token && token != "" {
			http.Error(w, "auth", http.StatusNotFound)
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		// repos/uffici-ai/<repo>/releases/{latest | tags/v<x> | assets/<name>}
		if len(parts) < 5 || parts[0] != "repos" {
			http.NotFound(w, r)
			return
		}
		repo, rest := parts[2], parts[4:]
		switch {
		case rest[0] == "assets" && len(rest) == 2:
			if body, ok := rs.assets[repo][rest[1]]; ok {
				_, _ = w.Write(body)
				return
			}
			http.NotFound(w, r)
		case rest[0] == "latest" || rest[0] == "tags":
			version, ok := rs.latest[repo]
			if !ok {
				http.NotFound(w, r)
				return
			}
			var list []map[string]string
			for name := range rs.assets[repo] {
				list = append(list, map[string]string{"name": name, "url": rs.URL + "/repos/uffici-ai/" + repo + "/releases/assets/" + name})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": "v" + version, "assets": list})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(rs.Close)
	return rs
}

// publish makes version the latest release of repo, with assets and their
// checksum list.
func (rs *releases) publish(repo, version string, assets map[string][]byte) {
	var sums strings.Builder
	all := map[string][]byte{}
	for name, body := range assets {
		all[name] = body
		sums.WriteString(sha(body) + "  " + name + "\n")
	}
	all[ChecksumsFile] = []byte(sums.String())
	rs.latest[repo], rs.assets[repo] = version, all
}

func (rs *releases) release(repo, version string) Release {
	// The API path serves both with and without a token here; a public
	// repository's assets would come from the web origin, which Fetch picks
	// only without a token — so the rig always carries one.
	return Release{Owner: "uffici-ai", Repo: repo, Version: version, Token: rs.token, BaseURL: rs.URL, APIURL: rs.URL, Client: rs.Client()}
}

// companionArchive is a release archive with `uffici` at its top level.
func companionArchive(t *testing.T, binary string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{"uffici": binary, "LICENSE": "Apache-2.0\n"} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const testToken = "ghp_upgradeTokenNeverPrinted"

// upgradeRig is one machine set up by `uffici setup`, against a loopback release.
type upgradeRig struct {
	opts     UpgradeOptions
	rs       *releases
	runner   *recorder
	out      *bytes.Buffer
	home     string // the harness home
	execs    [][]string
	doctored int
}

func pluginManifest(version string, restart bool) map[string]any {
	m := goodManifest()
	m["version"] = version
	if restart {
		m["restartCommand"] = []string{"hermes", "gateway", "restart"}
		m["restartService"] = map[string]string{"systemd": "hermes-gateway", "launchd": "ai.hermes.gateway"}
	}
	return m
}

func newUpgradeRig(t *testing.T, current, installedPlugin string) *upgradeRig {
	t.Helper()
	userHome := t.TempDir()
	r := &upgradeRig{
		rs:     newReleases(t, testToken),
		runner: &recorder{answers: map[string]string{}, fail: map[string]bool{}},
		out:    &bytes.Buffer{},
		home:   filepath.Join(userHome, ".hermes"),
	}
	companionHome := filepath.Join(userHome, ".uffici")
	binary := filepath.Join(userHome, "bin", "uffici")
	must(t, os.MkdirAll(filepath.Dir(binary), 0o755))
	must(t, os.WriteFile(binary, []byte("old binary"), 0o755))
	must(t, os.MkdirAll(companionHome, 0o700))
	must(t, os.WriteFile(filepath.Join(companionHome, "agent-key"), []byte("the-agent-key"), 0o600))
	must(t, WriteRecord(companionHome, Record{
		Harness: "hermes", HarnessHome: r.home, PluginDir: "plugins/uffici-for-hermes",
		GatewayURL: "https://agents.example", Service: true,
	}))
	if installedPlugin != "" {
		dir := filepath.Join(r.home, "plugins", "uffici-for-hermes")
		must(t, os.MkdirAll(dir, 0o755))
		must(t, os.WriteFile(filepath.Join(dir, ManifestFile), mustJSON(t, pluginManifest(installedPlugin, true)), 0o644))
	}
	r.opts = UpgradeOptions{
		Current: current, GOOS: "linux", GOARCH: "amd64",
		UserHome: userHome, CompanionHome: companionHome, Binary: binary, UID: "1000",
		Env: func(string) string { return "" },
		LookPath: func(name string) (string, error) {
			if name == "hermes" {
				return "/usr/bin/hermes", nil
			}
			return "", errors.New("not found")
		},
		Runner:  r.runner,
		Release: r.rs.release,
		// The gateway names no version unless a test says otherwise: the
		// latest-release path every test before #2382 was written against.
		Gateway: noGatewayVersion,
		Exec: func(binary string, args []string) error {
			r.execs = append(r.execs, append([]string{binary}, args...))
			return nil
		},
		Doctor: func(context.Context) int { r.doctored++; return ExitOK },
	}
	return r
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// publishBoth releases the companion and the plugin at version.
func (r *upgradeRig) publishBoth(t *testing.T, version string, restart bool) {
	t.Helper()
	r.rs.publish(CompanionRepo, version, map[string][]byte{companionAsset("linux", "amd64"): companionArchive(t, "new binary "+version)})
	r.rs.publish("uffici-for-hermes", version, map[string][]byte{"uffici-for-hermes.tar.gz": tarball(t, "uffici-for-hermes", map[string]string{
		ManifestFile: string(mustJSON(t, pluginManifest(version, restart))),
		"adapter.py": "print('" + version + "')\n",
	}, nil)})
}

func (r *upgradeRig) binary(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(r.opts.Binary)
	must(t, err)
	return string(raw)
}

func (r *upgradeRig) assertNoSecret(t *testing.T) {
	t.Helper()
	for _, secret := range []string{testToken, "the-agent-key"} {
		if strings.Contains(r.out.String(), secret) {
			t.Fatalf("the output carries a secret:\n%s", r.out)
		}
	}
}

func TestUpgradeWithoutARecordNamesTheSetupLine(t *testing.T) {
	r := newUpgradeRig(t, "0.4.0", "0.4.0")
	must(t, os.Remove(filepath.Join(r.opts.CompanionHome, RecordFile)))
	if code := Upgrade(context.Background(), r.opts, r.out); code != ExitConfig {
		t.Fatalf("exit %d:\n%s", code, r.out)
	}
	if !strings.Contains(r.out.String(), "uffici setup --no-pair --harness") {
		t.Fatalf("the message does not name the setup line:\n%s", r.out)
	}
}

func TestUpgradeCheckReportsAndChangesNothing(t *testing.T) {
	r := newUpgradeRig(t, "0.4.0", "0.4.0")
	r.publishBoth(t, "0.5.0", true)
	r.opts.Check = true
	if code := Upgrade(context.Background(), r.opts, r.out); code != ExitUpgradeAvailable {
		t.Fatalf("exit %d, want %d:\n%s", code, ExitUpgradeAvailable, r.out)
	}
	if !strings.Contains(r.out.String(), "companion  0.4.0 → 0.5.0 available") || !strings.Contains(r.out.String(), "plugin     0.4.0 → 0.5.0 available") {
		t.Fatalf("the check did not report both components:\n%s", r.out)
	}
	if r.binary(t) != "old binary" || len(r.runner.calls) != 0 || len(r.execs) != 0 || r.doctored != 0 {
		t.Fatalf("--check changed something: binary %q, %d commands, %d execs", r.binary(t), len(r.runner.calls), len(r.execs))
	}

	current := newUpgradeRig(t, "0.5.0", "0.5.0")
	current.publishBoth(t, "0.5.0", true)
	current.opts.Check = true
	if code := Upgrade(context.Background(), current.opts, current.out); code != ExitOK {
		t.Fatalf("a current machine answered %d:\n%s", code, current.out)
	}
	r.assertNoSecret(t)
}

// 🔴 The replace is ATOMIC: written beside the running binary, then renamed.
// A second link to the old file keeps the old bytes only if the new binary is a
// new file — a write in place would change what every link sees, including a
// process that is executing it.
func TestUpgradeReplacesTheBinaryAtomicallyThenHandsOver(t *testing.T) {
	r := newUpgradeRig(t, "0.4.0", "0.4.0")
	r.publishBoth(t, "0.5.0", true)
	link := r.opts.Binary + ".link"
	must(t, os.Link(r.opts.Binary, link))

	if code := Upgrade(context.Background(), r.opts, r.out); code != ExitFailed || len(r.execs) != 1 {
		// The injected Exec returns, which a real one never does: the old binary
		// reports that the handover failed. What matters is that it was asked.
		t.Fatalf("exit %d, %d execs:\n%s", code, len(r.execs), r.out)
	}
	if got := r.binary(t); got != "new binary 0.5.0" {
		t.Fatalf("the binary is %q", got)
	}
	if old, _ := os.ReadFile(link); string(old) != "old binary" {
		t.Fatalf("the binary was rewritten in place (the old link now reads %q)", old)
	}
	if info, _ := os.Stat(r.opts.Binary); info.Mode().Perm() != 0o755 {
		t.Fatalf("the new binary's mode is %v", info.Mode().Perm())
	}
	if _, err := os.Stat(r.opts.Binary + ".new"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the staged file was left behind")
	}
	// 🔴 --home travels: without it the new binary reads the default home, which
	// may hold no record — or another machine's setup.
	want := []string{r.opts.Binary, "upgrade", "--home", r.opts.CompanionHome, "--version", "0.5.0", "--from-version", "0.4.0", "--plugin-version", "0.5.0"}
	if strings.Join(r.execs[0], " ") != strings.Join(want, " ") {
		t.Fatalf("handed over with %v, want %v", r.execs[0], want)
	}
	// The OLD binary installs no plugin: it may not know the new manifest.
	if len(r.runner.calls) != 0 {
		t.Fatalf("the old binary ran %v before handing over", r.runner.calls)
	}
	r.assertNoSecret(t)
}

func TestUpgradeACorruptArchiveLeavesTheBinaryInPlace(t *testing.T) {
	r := newUpgradeRig(t, "0.4.0", "0.4.0")
	r.publishBoth(t, "0.5.0", true)
	// The archive changes after the checksum list was published.
	r.rs.assets[CompanionRepo][companionAsset("linux", "amd64")] = companionArchive(t, "tampered")
	if code := Upgrade(context.Background(), r.opts, r.out); code != ExitFailed {
		t.Fatalf("exit %d:\n%s", code, r.out)
	}
	if !strings.Contains(r.out.String(), "does not match its published checksum") {
		t.Fatalf("the failure does not name the check:\n%s", r.out)
	}
	if r.binary(t) != "old binary" || len(r.execs) != 0 {
		t.Fatalf("a refused archive touched the binary (%q) or handed over", r.binary(t))
	}
}

// The new binary, handed over to, finishes: the plugin and its settings, the
// companion service, the harness, the doctor and the before → after lines.
func TestUpgradeTheNewBinaryInstallsThePluginAndRestartsBoth(t *testing.T) {
	r := newUpgradeRig(t, "0.5.0", "0.4.0")
	r.publishBoth(t, "0.5.0", true)
	r.opts.FromVersion, r.opts.Version, r.opts.PluginVersion = "0.4.0", "0.5.0", "0.5.0"
	r.opts.RestartHarness = RestartYes // handed over as --restart-harness (#2278)
	unit := filepath.Join(r.opts.UserHome, ".config", "systemd", "user", "hermes-gateway.service")
	must(t, os.MkdirAll(filepath.Dir(unit), 0o755))
	must(t, os.WriteFile(unit, []byte("[Service]\n"), 0o644))

	if code := Upgrade(context.Background(), r.opts, r.out); code != ExitOK {
		t.Fatalf("exit %d:\n%s", code, r.out)
	}
	if adapter, _ := os.ReadFile(filepath.Join(r.home, "plugins", "uffici-for-hermes", "adapter.py")); !strings.Contains(string(adapter), "0.5.0") {
		t.Fatalf("the new plugin was not installed: %q", adapter)
	}
	for _, want := range []string{
		"hermes config set gateway.platforms.uffici.enabled true", // the settings, re-applied
		"hermes plugins enable uffici-for-hermes",
		"systemctl --user restart " + ServiceLabel + ".service", // the companion
		"hermes gateway restart",                                // the harness
	} {
		if !r.runner.ran(want) {
			t.Fatalf("did not run %q; ran %v", want, r.runner.calls)
		}
	}
	if r.doctored != 1 {
		t.Fatal("the doctor did not run")
	}
	for _, want := range []string{"companion  0.4.0 → 0.5.0", "plugin     0.4.0 → 0.5.0", "hermes     restarted"} {
		if !strings.Contains(r.out.String(), want) {
			t.Fatalf("the summary lacks %q:\n%s", want, r.out)
		}
	}
	// It keeps the key and never pairs: there is no pairing in upgrade at all,
	// and the key file is the one setup left.
	if key, _ := os.ReadFile(filepath.Join(r.opts.CompanionHome, "agent-key")); string(key) != "the-agent-key" {
		t.Fatalf("the agent key changed: %q", key)
	}
	r.assertNoSecret(t)
}

// Hermes's restart command starts a gateway in the FOREGROUND when it is not a
// service — it would hang the upgrade. With no service definition the person
// is told instead, and nothing is run.
func TestUpgradeNeverRunsTheRestartCommandForAHarnessRunByHand(t *testing.T) {
	r := newUpgradeRig(t, "0.5.0", "0.4.0")
	r.publishBoth(t, "0.5.0", true)
	if code := Upgrade(context.Background(), r.opts, r.out); code != ExitOK {
		t.Fatalf("exit %d:\n%s", code, r.out)
	}
	if r.runner.ran("hermes gateway restart") {
		t.Fatal("the restart command ran with no harness service installed")
	}
	if !strings.Contains(r.out.String(), "Restart the Hermes gateway") || !strings.Contains(r.out.String(), "not run as a service here") {
		t.Fatalf("the person was not told to restart:\n%s", r.out)
	}
	// A plugin-only upgrade leaves the companion's service alone.
	if r.runner.ran("systemctl --user restart " + ServiceLabel) {
		t.Fatal("the companion was restarted although its binary did not change")
	}
}

func TestUpgradeWithoutARestartCommandPrintsTheManifestsSentence(t *testing.T) {
	r := newUpgradeRig(t, "0.5.0", "0.4.0")
	r.publishBoth(t, "0.5.0", false)
	if code := Upgrade(context.Background(), r.opts, r.out); code != ExitOK {
		t.Fatalf("exit %d:\n%s", code, r.out)
	}
	if r.runner.ran("hermes gateway restart") || !strings.Contains(r.out.String(), "Restart the Hermes gateway") {
		t.Fatalf("a manifest without restartCommand must only print its sentence:\n%s", r.out)
	}
}

func TestUpgradeRefusesADowngradeUnlessPinned(t *testing.T) {
	for name, current := range map[string]string{"a newer release": "0.6.0", "a development build": "0.0.0-dev"} {
		t.Run(name, func(t *testing.T) {
			r := newUpgradeRig(t, current, "0.5.0")
			r.publishBoth(t, "0.5.0", true)
			if code := Upgrade(context.Background(), r.opts, r.out); code != ExitConfig {
				t.Fatalf("exit %d:\n%s", code, r.out)
			}
			if r.binary(t) != "old binary" || len(r.execs) != 0 {
				t.Fatal("a refused downgrade replaced the binary")
			}
			r.opts.Version = "0.5.0"
			r.out.Reset()
			Upgrade(context.Background(), r.opts, r.out)
			if r.binary(t) != "new binary 0.5.0" {
				t.Fatalf("a pinned version was not installed:\n%s", r.out)
			}
		})
	}
}

func TestUpgradeDryRunPerformsNothing(t *testing.T) {
	r := newUpgradeRig(t, "0.4.0", "0.4.0")
	r.publishBoth(t, "0.5.0", true)
	r.opts.DryRun = true
	if code := Upgrade(context.Background(), r.opts, r.out); code != ExitOK {
		t.Fatalf("exit %d:\n%s", code, r.out)
	}
	if r.binary(t) != "old binary" || len(r.execs) != 0 || len(r.runner.calls) != 0 || r.doctored != 0 {
		t.Fatalf("a dry run acted: binary %q, execs %v, commands %v", r.binary(t), r.execs, r.runner.calls)
	}
	for _, want := range []string{"would fetch " + companionAsset("linux", "amd64"), "would hand over to the new companion, which would"} {
		if !strings.Contains(r.out.String(), want) {
			t.Fatalf("the dry run did not announce %q:\n%s", want, r.out)
		}
	}
	// The plugin's manifest is the NEW binary's to read: the old one never
	// fetches it, even to describe it.
	if strings.Contains(r.out.String(), "fetching uffici-for-hermes") {
		t.Fatalf("the old binary read the new plugin in a dry run:\n%s", r.out)
	}

	// With the companion current, a dry run describes the plugin steps.
	current := newUpgradeRig(t, "0.5.0", "0.4.0")
	current.publishBoth(t, "0.5.0", true)
	current.opts.DryRun = true
	if code := Upgrade(context.Background(), current.opts, current.out); code != ExitOK || !strings.Contains(current.out.String(), "would install the plugin") {
		t.Fatalf("exit %d:\n%s", code, current.out)
	}
	if len(current.runner.calls) != 0 {
		t.Fatalf("a dry run ran %v", current.runner.calls)
	}
}

func TestUpgradeEverythingCurrentDoesNothing(t *testing.T) {
	r := newUpgradeRig(t, "0.5.0", "0.5.0")
	r.publishBoth(t, "0.5.0", true)
	if code := Upgrade(context.Background(), r.opts, r.out); code != ExitOK || !strings.Contains(r.out.String(), "Everything is current") {
		t.Fatalf("exit %d:\n%s", code, r.out)
	}
	if len(r.runner.calls) != 0 || r.doctored != 0 {
		t.Fatal("a current machine was touched")
	}
}

func TestVersionsCompareAsReleases(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"0.4.0", "0.5.0", -1}, {"0.10.0", "0.9.9", 1}, {"1.0.0", "1.0.0", 0},
		{"0.5.0-rc.1", "0.5.0", -1}, {"v0.5.0", "0.5.0", 0},
	} {
		a, okA := parseVersion(c.a)
		b, okB := parseVersion(c.b)
		if !okA || !okB || compareVersions(a, b) != c.want {
			t.Fatalf("compare(%s, %s) = %d, want %d", c.a, c.b, compareVersions(a, b), c.want)
		}
	}
	for _, bad := range []string{"0.0.0-dev", "1.2", "x.y.z", "1.2.3-", ""} {
		if _, ok := parseVersion(bad); ok {
			t.Fatalf("%q parsed as a release", bad)
		}
	}
}

func TestARestartCommandNeedsTheServiceItRestarts(t *testing.T) {
	m := goodManifest()
	m["restartCommand"] = []string{"hermes", "gateway", "restart"}
	if _, err := ParseManifest(mustJSON(t, m)); err == nil {
		t.Fatal("restartCommand without restartService was accepted")
	}
	orphan := goodManifest()
	orphan["restartService"] = map[string]string{"systemd": "hermes-gateway"}
	if _, err := ParseManifest(mustJSON(t, orphan)); err == nil {
		t.Fatal("restartService without restartCommand was accepted")
	}
	m["restartService"] = map[string]string{"systemd": "hermes-gateway"}
	if _, err := ParseManifest(mustJSON(t, m)); err != nil {
		t.Fatalf("a complete restart declaration was refused: %v", err)
	}
}

func TestTheRecordIsPrivateAndHoldsNoSecret(t *testing.T) {
	home := t.TempDir()
	must(t, WriteRecord(home, Record{Harness: "hermes", GatewayURL: "https://agents.example", Service: true}))
	info, err := os.Stat(filepath.Join(home, RecordFile))
	must(t, err)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("the record's mode is %v", info.Mode().Perm())
	}
	record, err := ReadRecord(home)
	if err != nil || record.Harness != "hermes" || !record.Service {
		t.Fatalf("read back %+v, %v", record, err)
	}
	must(t, os.WriteFile(filepath.Join(home, RecordFile), []byte("{"), 0o600))
	if _, err := ReadRecord(home); err == nil {
		t.Fatal("a broken record was read")
	}
}

func TestUpgradeRefusalsAlongTheWay(t *testing.T) {
	if _, err := binaryFromArchive([]byte("not gzip")); err == nil {
		t.Fatal("a non-gzip archive was read")
	}
	if _, err := binaryFromArchive(tarball(t, "top", map[string]string{"other": "x"}, nil)); err == nil {
		t.Fatal("an archive without uffici was read")
	}
	r := newUpgradeRig(t, "0.4.0", "0.4.0")
	if _, err := targetVersion(context.Background(), r.opts, CompanionRepo, "latest-please", ""); err == nil {
		t.Fatal("a malformed pin was accepted")
	}
	// No release published: the latest cannot be read, and the run says so.
	if code := Upgrade(context.Background(), r.opts, r.out); code != ExitFailed {
		t.Fatalf("exit %d with no release:\n%s", code, r.out)
	}
	r.rs.latest[CompanionRepo] = "not-a-tag"
	r.rs.assets[CompanionRepo] = map[string][]byte{}
	if _, err := r.rs.release(CompanionRepo, "").LatestVersion(context.Background()); err == nil {
		t.Fatal("a tag without its v was read as a version")
	}
	insecure := Release{Owner: "o", Repo: "r", APIURL: "http://example.invalid"}
	if _, err := insecure.LatestVersion(context.Background()); err == nil {
		t.Fatal("a plain-http API off loopback was asked")
	}
	// A broken installed manifest reads as no version, never a crash.
	dir := filepath.Join(r.home, "plugins", "uffici-for-hermes")
	must(t, os.WriteFile(filepath.Join(dir, ManifestFile), []byte("{"), 0o644))
	if v := installedPluginVersion(Record{HarnessHome: r.home, PluginDir: "plugins/uffici-for-hermes"}); v != "" {
		t.Fatalf("a broken manifest read as %q", v)
	}
	if v := installedPluginVersion(Record{}); v != "" {
		t.Fatalf("no plugin read as %q", v)
	}
}

func TestHarnessServiceDefinitionsPerManager(t *testing.T) {
	home := t.TempDir()
	service := RestartService{Systemd: "hermes-gateway", Launchd: "ai.hermes.gateway"}
	env := func(string) string { return "" }
	if harnessServiceInstalled(service, "linux", home, env) || harnessServiceInstalled(service, "darwin", home, env) || harnessServiceInstalled(service, "windows", home, env) {
		t.Fatal("a service was found where none is defined")
	}
	plist := filepath.Join(home, "Library", "LaunchAgents", "ai.hermes.gateway.plist")
	must(t, os.MkdirAll(filepath.Dir(plist), 0o755))
	must(t, os.WriteFile(plist, nil, 0o644))
	if !harnessServiceInstalled(service, "darwin", home, env) {
		t.Fatal("the launch agent was not found")
	}
	xdg := t.TempDir()
	unit := filepath.Join(xdg, "systemd", "user", "hermes-gateway.service")
	must(t, os.MkdirAll(filepath.Dir(unit), 0o755))
	must(t, os.WriteFile(unit, nil, 0o644))
	if !harnessServiceInstalled(service, "linux", home, func(string) string { return xdg }) {
		t.Fatal("a unit under XDG_CONFIG_HOME was not found")
	}
}

// A restart that fails is reported with the manifest's sentence, and the
// upgrade still finishes: the plugin is installed either way.
func TestUpgradeAFailedHarnessRestartTellsThePerson(t *testing.T) {
	r := newUpgradeRig(t, "0.5.0", "0.4.0")
	r.publishBoth(t, "0.5.0", true)
	unit := filepath.Join(r.opts.UserHome, ".config", "systemd", "user", "hermes-gateway.service")
	must(t, os.MkdirAll(filepath.Dir(unit), 0o755))
	must(t, os.WriteFile(unit, nil, 0o644))
	r.runner.fail["hermes gateway restart"] = true
	r.opts.RestartHarness = RestartYes
	if code := Upgrade(context.Background(), r.opts, r.out); code != ExitOK {
		t.Fatalf("exit %d:\n%s", code, r.out)
	}
	if !strings.Contains(r.out.String(), "hermes     restart failed") || !strings.Contains(r.out.String(), "Restart the Hermes gateway") {
		t.Fatalf("a failed restart was not reported:\n%s", r.out)
	}
}

// Set up without the service (`--no-service`), a new binary is the person's to
// restart.
func TestUpgradeWithoutTheServiceSaysToRestartTheCompanion(t *testing.T) {
	r := newUpgradeRig(t, "0.5.0", "0.5.0")
	r.publishBoth(t, "0.5.0", true)
	must(t, WriteRecord(r.opts.CompanionHome, Record{Harness: "hermes", HarnessHome: r.home, PluginDir: "plugins/uffici-for-hermes", GatewayURL: "https://agents.example"}))
	r.opts.FromVersion = "0.4.0"
	if code := Upgrade(context.Background(), r.opts, r.out); code != ExitOK {
		t.Fatalf("exit %d:\n%s", code, r.out)
	}
	if r.runner.ran("systemctl") || !strings.Contains(r.out.String(), "restart it yourself to run the new version") {
		t.Fatalf("a companion without a service was restarted, or the person was not told:\n%s", r.out)
	}
}

// 🔴 The new binary is started with --version pinned to itself. A release whose
// binary reports another version would, without this stop, download, replace
// and hand over again, forever.
func TestUpgradeAMisStampedReleaseStopsInsteadOfLooping(t *testing.T) {
	r := newUpgradeRig(t, "0.4.9", "0.4.0")
	r.publishBoth(t, "0.5.0", true)
	r.opts.FromVersion, r.opts.Version = "0.4.0", "0.5.0"
	if code := Upgrade(context.Background(), r.opts, r.out); code != ExitFailed || !strings.Contains(r.out.String(), "mis-stamped") {
		t.Fatalf("exit %d:\n%s", code, r.out)
	}
	if len(r.execs) != 0 || r.binary(t) != "old binary" {
		t.Fatal("a mis-stamped release was replaced and handed over again")
	}
}

// A handover that failed leaves the new binary on disk and the old daemon
// running. The next run finds everything "current" by version — and must still
// restart the service, which is what that upgrade never got to.
func TestUpgradeRestartsADaemonStillRunningTheOldBinary(t *testing.T) {
	r := newUpgradeRig(t, "0.5.0", "0.5.0")
	r.publishBoth(t, "0.5.0", true)
	r.opts.RunningVersion = func(context.Context) (string, bool) { return "0.4.0", true }
	check := r.opts
	check.Check = true
	if code := Upgrade(context.Background(), check, r.out); code != ExitUpgradeAvailable {
		t.Fatalf("--check on a stale daemon answered %d:\n%s", code, r.out)
	}
	if code := Upgrade(context.Background(), r.opts, r.out); code != ExitOK {
		t.Fatalf("exit %d:\n%s", code, r.out)
	}
	if !r.runner.ran("systemctl --user restart "+ServiceLabel) || r.doctored != 1 {
		t.Fatalf("the stale daemon was not restarted:\n%s", r.out)
	}
	// A daemon on the current version is left alone.
	fresh := newUpgradeRig(t, "0.5.0", "0.5.0")
	fresh.publishBoth(t, "0.5.0", true)
	fresh.opts.RunningVersion = func(context.Context) (string, bool) { return "0.5.0", true }
	if code := Upgrade(context.Background(), fresh.opts, fresh.out); code != ExitOK || len(fresh.runner.calls) != 0 {
		t.Fatalf("a current daemon was touched (%v):\n%s", fresh.runner.calls, fresh.out)
	}
}

// 🔴 A service that is installed but STOPPED means the person runs the gateway
// by hand now; restarting the unit would start a second gateway beside theirs.
func TestUpgradeLeavesAStoppedHarnessServiceAlone(t *testing.T) {
	r := newUpgradeRig(t, "0.5.0", "0.4.0")
	r.publishBoth(t, "0.5.0", true)
	unit := filepath.Join(r.opts.UserHome, ".config", "systemd", "user", "hermes-gateway.service")
	must(t, os.MkdirAll(filepath.Dir(unit), 0o755))
	must(t, os.WriteFile(unit, nil, 0o644))
	r.runner.fail["systemctl --user is-active --quiet hermes-gateway"] = true
	if code := Upgrade(context.Background(), r.opts, r.out); code != ExitOK {
		t.Fatalf("exit %d:\n%s", code, r.out)
	}
	if r.runner.ran("hermes gateway restart") || !strings.Contains(r.out.String(), "not run as a service here") {
		t.Fatalf("a stopped harness service was restarted:\n%s", r.out)
	}
}

func TestARecordThatBreaksSetupsRulesIsRefused(t *testing.T) {
	for name, body := range map[string]string{
		"a harness that is not a name": `{"harness":"../../evil","service":true}`,
		"a relative harness home":      `{"harness":"hermes","harnessHome":".hermes","service":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			must(t, os.WriteFile(filepath.Join(home, RecordFile), []byte(body), 0o600))
			if _, err := ReadRecord(home); err == nil {
				t.Fatalf("%s was read", name)
			}
		})
	}
}

// 🔴 An upgrade rewrites the unit with the log settings the last setup recorded
// (#2264): a machine switched off stays off across an upgrade, and the record
// keeps saying so.
func TestUpgradeKeepsTheMachinesLogSettings(t *testing.T) {
	r := newUpgradeRig(t, "0.5.0", "0.4.0")
	r.publishBoth(t, "0.5.0", true)
	record, err := ReadRecord(r.opts.CompanionHome)
	must(t, err)
	record.LogShipping, record.HarnessLogLevel = "off", "error"
	must(t, WriteRecord(r.opts.CompanionHome, record))
	r.opts.FromVersion, r.opts.Version, r.opts.PluginVersion = "0.4.0", "0.5.0", "0.5.0"

	if code := Upgrade(context.Background(), r.opts, r.out); code != ExitOK {
		t.Fatalf("exit %d:\n%s", code, r.out)
	}
	unit, err := os.ReadFile(filepath.Join(r.opts.UserHome, ".config", "systemd", "user", ServiceLabel+".service"))
	must(t, err)
	if !strings.Contains(string(unit), `Environment="UFFICI_LOG_SHIPPING=off"`) ||
		!strings.Contains(string(unit), `Environment="UFFICI_LOG_LEVEL_HARNESS=error"`) {
		t.Fatalf("the upgrade reset the machine's log settings:\n%s", unit)
	}
	after, err := ReadRecord(r.opts.CompanionHome)
	must(t, err)
	if after.LogShipping != "off" || after.HarnessLogLevel != "error" {
		t.Fatalf("the record lost the log settings: %+v", after)
	}
}

// withHarnessService installs and runs the harness's own user service, so a
// restart is SAFE and only the person's say decides it.
func (r *upgradeRig) withHarnessService(t *testing.T) {
	t.Helper()
	unit := filepath.Join(r.opts.UserHome, ".config", "systemd", "user", "hermes-gateway.service")
	must(t, os.MkdirAll(filepath.Dir(unit), 0o755))
	must(t, os.WriteFile(unit, nil, 0o644))
}

// 🔴 #2278: a person's harness is restarted only on their say — a flag, --yes
// or a yes at the terminal. With none (a scheduled upgrade, no terminal) it is
// left alone and the person is told how, even where a restart would be safe.
func TestUpgradeRestartsTheHarnessOnlyOnThePersonsSay(t *testing.T) {
	for _, tc := range []struct {
		name     string
		choice   RestartChoice
		confirm  func(string) (bool, bool)
		restarts bool
		line     string
	}{
		{"no flag and no terminal", RestartAsk, nil, false, "hermes     not restarted — no terminal to ask"},
		{"asked, yes", RestartAsk, func(string) (bool, bool) { return true, true }, true, "hermes     restarted"},
		{"asked, no", RestartAsk, func(string) (bool, bool) { return false, true }, false, "hermes     not restarted — you said no"},
		{"--restart-harness", RestartYes, nil, true, "hermes     restarted"},
		{"--no-restart-harness", RestartNo, func(string) (bool, bool) { t.Fatal("asked although the flag said no"); return false, false }, false, "hermes     not restarted — you said no"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newUpgradeRig(t, "0.5.0", "0.4.0")
			r.publishBoth(t, "0.5.0", true)
			r.withHarnessService(t)
			var asked []string
			r.opts.RestartHarness = tc.choice
			if tc.confirm != nil {
				r.opts.Confirm = func(q string) (bool, bool) { asked = append(asked, q); return tc.confirm(q) }
			}
			if code := Upgrade(context.Background(), r.opts, r.out); code != ExitOK {
				t.Fatalf("exit %d:\n%s", code, r.out)
			}
			if got := r.runner.ran("hermes gateway restart"); got != tc.restarts {
				t.Fatalf("restarted = %v, want %v:\n%s", got, tc.restarts, r.out)
			}
			if !strings.Contains(r.out.String(), tc.line) {
				t.Fatalf("the summary lacks %q:\n%s", tc.line, r.out)
			}
			if !tc.restarts && !strings.Contains(r.out.String(), "Restart the Hermes gateway") {
				t.Fatalf("the person was not told how to restart:\n%s", r.out)
			}
			if tc.choice == RestartAsk && tc.confirm != nil &&
				(len(asked) != 1 || !strings.Contains(asked[0], "Restart hermes now to load the plugin?")) {
				t.Fatalf("asked %q, want the one restart question", asked)
			}
		})
	}
}

// 🔴 #2278: a yes is never asked for, nor acted on, where the restart is not
// safe — a harness run by hand would be started in the foreground.
func TestUpgradeAYesNeverRestartsAHarnessRunByHand(t *testing.T) {
	r := newUpgradeRig(t, "0.5.0", "0.4.0")
	r.publishBoth(t, "0.5.0", true)
	r.opts.RestartHarness = RestartYes
	r.opts.Confirm = func(string) (bool, bool) { t.Fatal("asked where no restart was safe"); return true, true }
	if code := Upgrade(context.Background(), r.opts, r.out); code != ExitOK {
		t.Fatalf("exit %d:\n%s", code, r.out)
	}
	if r.runner.ran("hermes gateway restart") || !strings.Contains(r.out.String(), "not run as a service here") {
		t.Fatalf("a harness run by hand was restarted, or the person was not told why:\n%s", r.out)
	}
}

// The handover carries the person's say to the new binary; with none, the new
// binary asks on the same terminal (#2278).
func TestUpgradeHandsTheRestartChoiceOver(t *testing.T) {
	for choice, want := range map[RestartChoice]string{RestartYes: "--restart-harness", RestartNo: "--no-restart-harness", RestartAsk: ""} {
		r := newUpgradeRig(t, "0.6.0", "0.6.0")
		r.publishBoth(t, "0.7.0", true)
		r.opts.RestartHarness = choice
		Upgrade(context.Background(), r.opts, r.out)
		if len(r.execs) != 1 {
			t.Fatalf("choice %d: no handover:\n%s", choice, r.out)
		}
		args := strings.Join(r.execs[0], " ")
		if want != "" && !strings.HasSuffix(args, " "+want) {
			t.Fatalf("choice %d handed over %q, want it ending %q", choice, args, want)
		}
		if want == "" && strings.Contains(args, "restart-harness") {
			t.Fatalf("no choice handed over %q", args)
		}
	}
}

// A dry run says it would ask, and asks nothing.
func TestUpgradeDryRunNeverAsksAboutTheRestart(t *testing.T) {
	r := newUpgradeRig(t, "0.5.0", "0.4.0")
	r.publishBoth(t, "0.5.0", true)
	r.withHarnessService(t)
	r.opts.DryRun = true
	r.opts.Confirm = func(string) (bool, bool) { t.Fatal("a dry run asked"); return true, true }
	if code := Upgrade(context.Background(), r.opts, r.out); code != ExitOK {
		t.Fatalf("exit %d:\n%s", code, r.out)
	}
	if r.runner.ran("hermes gateway restart") || !strings.Contains(r.out.String(), "would ask whether to restart hermes now") {
		t.Fatalf("a dry run did not say it would ask:\n%s", r.out)
	}
}

// The restart question defaults to yes, understands y/yes/n/no, asks again on
// anything else, and reports no answer when its input ends (#2278).
func TestAskRestartReadsTheAnswer(t *testing.T) {
	for input, want := range map[string][2]bool{
		"\n":         {true, true},
		"y\n":        {true, true},
		"YES\n":      {true, true},
		"n\n":        {false, true},
		" No \n":     {false, true},
		"maybe\nn\n": {false, true},
		"":           {false, false},
		"maybe\n":    {false, false},
		"y":          {true, true},
	} {
		var out bytes.Buffer
		yes, answered := AskRestart(strings.NewReader(input), &out, "Restart? [Y/n] ")
		if [2]bool{yes, answered} != want {
			t.Fatalf("%q answered (%v, %v), want %v", input, yes, answered, want)
		}
		if !strings.HasPrefix(out.String(), "Restart? [Y/n] ") {
			t.Fatalf("%q: the question was not asked: %q", input, out.String())
		}
	}
}

func TestParseRestartChoice(t *testing.T) {
	for _, tc := range []struct {
		restart, noRestart bool
		want               RestartChoice
		err                bool
	}{
		{false, false, RestartAsk, false},
		{true, false, RestartYes, false},
		{false, true, RestartNo, false},
		{true, true, RestartAsk, true},
	} {
		got, err := ParseRestartChoice(tc.restart, tc.noRestart)
		if got != tc.want || (err != nil) != tc.err {
			t.Fatalf("(%v, %v) = %v, %v", tc.restart, tc.noRestart, got, err)
		}
	}
}

// 🔴 A pinned downgrade hands over to a release that does not know the restart
// flags; handing them over would make its flag parser refuse the run after the
// swap, and every re-run with them (#2278 review).
func TestUpgradeNeverHandsTheRestartFlagsToABinaryThatLacksThem(t *testing.T) {
	r := newUpgradeRig(t, "0.6.0", "0.6.0")
	r.publishBoth(t, "0.5.0", true)
	r.opts.Version, r.opts.PluginVersion, r.opts.RestartHarness = "0.5.0", "0.5.0", RestartYes
	code := Upgrade(context.Background(), r.opts, r.out)
	if len(r.execs) != 1 {
		t.Fatalf("no handover (exit %d):\n%s", code, r.out)
	}
	if args := strings.Join(r.execs[0], " "); strings.Contains(args, "restart-harness") {
		t.Fatalf("handed %q to 0.5.0, which does not know the flag", args)
	}
	if !strings.Contains(r.out.String(), "uffici 0.5.0 does not know --restart-harness") {
		t.Fatalf("the dropped choice was not said:\n%s", r.out)
	}
}

// 🔴 #2688: the question is asked even where the companion cannot restart the
// harness itself. A yes there never runs the restart command — it would start
// a second gateway in the foreground — but tells the person what to run and
// waits for them to say it is done.
func TestUpgradeAsksAboutAHarnessRunByHandAndGuidesTheRestart(t *testing.T) {
	for _, tc := range []struct {
		name   string
		done   bool
		line   string
		paused int
	}{
		{"restarted by the person", true, "hermes     restarted by you", 1},
		{"no answer while waiting", false, "hermes     not restarted — restart it yourself", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newUpgradeRig(t, "0.5.0", "0.4.0")
			r.publishBoth(t, "0.5.0", true)
			var asked, prompts []string
			r.opts.Confirm = func(q string) (bool, bool) { asked = append(asked, q); return true, true }
			r.opts.Pause = func(lead []string, p string) bool {
				prompts = append(prompts, p)
				for _, line := range lead {
					r.out.WriteString(line + "\n")
				}
				return tc.done
			}
			if code := Upgrade(context.Background(), r.opts, r.out); code != ExitOK {
				t.Fatalf("exit %d:\n%s", code, r.out)
			}
			if len(asked) != 1 || !strings.Contains(asked[0], "Restart hermes now to load the plugin? [Y/n]") {
				t.Fatalf("asked %q, want the one restart question", asked)
			}
			if r.runner.ran("hermes gateway restart") {
				t.Fatal("the restart command ran for a harness run by hand")
			}
			if len(prompts) != tc.paused || !strings.Contains(r.out.String(), "Restart the Hermes gateway") {
				t.Fatalf("waited %d times (want %d), or never said what to run:\n%s", len(prompts), tc.paused, r.out)
			}
			if !strings.Contains(r.out.String(), tc.line) {
				t.Fatalf("the summary lacks %q:\n%s", tc.line, r.out)
			}
			if pending := strings.Contains(r.out.String(), "Until hermes restarts"); pending == tc.done {
				t.Fatalf("pending paragraph shown = %v, want %v:\n%s", pending, !tc.done, r.out)
			}
		})
	}
}

// 🔴 #2688: every path that leaves the harness running the old plugin says what
// that means until it restarts — the versions, the settings, and how to check.
func TestUpgradeSaysWhatIsPendingUntilTheHarnessRestarts(t *testing.T) {
	no := func(string) (bool, bool) { return false, true }
	for _, tc := range []struct {
		name    string
		service bool
		choice  RestartChoice
		confirm func(string) (bool, bool)
	}{
		{"a service, no terminal", true, RestartAsk, nil},
		{"a service, asked, no", true, RestartAsk, no},
		{"a service, --no-restart-harness", true, RestartNo, nil},
		{"run by hand, no terminal", false, RestartAsk, nil},
		{"run by hand, asked, no", false, RestartAsk, no},
		{"run by hand, --restart-harness, no terminal", false, RestartYes, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newUpgradeRig(t, "0.5.0", "0.4.0")
			r.publishBoth(t, "0.5.0", true)
			if tc.service {
				r.withHarnessService(t)
			}
			r.opts.RestartHarness, r.opts.Confirm = tc.choice, tc.confirm
			if tc.choice != RestartYes {
				r.opts.Pause = func([]string, string) bool { t.Fatal("waited for a restart nobody agreed to"); return false }
			}
			if code := Upgrade(context.Background(), r.opts, r.out); code != ExitOK {
				t.Fatalf("exit %d:\n%s", code, r.out)
			}
			out := r.out.String()
			for _, want := range []string{
				"Until hermes restarts it keeps running plugin 0.4.0, not 0.5.0",
				"the settings this run wrote",
				"Restart the Hermes gateway",
				"uffici doctor",
			} {
				if !strings.Contains(out, want) {
					t.Fatalf("lacks %q:\n%s", want, out)
				}
			}
			if r.runner.ran("hermes gateway restart") {
				t.Fatal("the harness was restarted")
			}
		})
	}
}

// A restart that happened says nothing is pending.
func TestUpgradeARestartedHarnessHasNothingPending(t *testing.T) {
	r := newUpgradeRig(t, "0.5.0", "0.4.0")
	r.publishBoth(t, "0.5.0", true)
	r.withHarnessService(t)
	r.opts.RestartHarness = RestartYes
	if code := Upgrade(context.Background(), r.opts, r.out); code != ExitOK {
		t.Fatalf("exit %d:\n%s", code, r.out)
	}
	if strings.Contains(r.out.String(), "Until hermes restarts") {
		t.Fatalf("a restarted harness was said to be pending:\n%s", r.out)
	}
}

func TestRestartPendingPerPreviousPlugin(t *testing.T) {
	for _, tc := range []struct{ previous, target, want string }{
		{"", "0.5.0", "Until hermes restarts it has not loaded the plugin"},
		{"0.5.0", "0.5.0", "Until hermes restarts it keeps the plugin as it loaded it"},
		{"0.4.0", "0.5.0", "Until hermes restarts it keeps running plugin 0.4.0, not 0.5.0"},
		{"0.4.0", "", "Until hermes restarts it keeps running plugin 0.4.0, not the new one"},
	} {
		if got := restartPending("hermes", tc.previous, tc.target); !strings.HasPrefix(got, tc.want) {
			t.Fatalf("previous %q: %q, want it to start %q", tc.previous, got, tc.want)
		}
	}
}

// Enter is a done; an input that ends first is nobody there (#2688).
func TestWaitForEnter(t *testing.T) {
	var out strings.Builder
	if !WaitForEnter(strings.NewReader("\n"), &out, []string{"Restart it."}, "Press Enter… ") || out.String() != "Restart it.\nPress Enter… " {
		t.Fatalf("Enter was not read, or the prompt not printed: %q", out.String())
	}
	if WaitForEnter(strings.NewReader(""), &out, nil, "") {
		t.Fatal("an ended input counted as done")
	}
}

// 🔴 #2688 review: a dry run says what it would leave pending and never claims
// settings were written.
func TestUpgradeDryRunNeverClaimsWrittenSettings(t *testing.T) {
	for _, choice := range []RestartChoice{RestartNo, RestartYes} {
		r := newUpgradeRig(t, "0.5.0", "0.4.0")
		r.publishBoth(t, "0.5.0", true)
		r.opts.DryRun, r.opts.RestartHarness = true, choice
		if code := Upgrade(context.Background(), r.opts, r.out); code != ExitOK {
			t.Fatalf("exit %d:\n%s", code, r.out)
		}
		if strings.Contains(r.out.String(), "this run wrote") || !strings.Contains(r.out.String(), "would leave hermes running") {
			t.Fatalf("choice %d: a dry run claimed written settings, or did not say what it would leave:\n%s", choice, r.out)
		}
	}
}

// 🔴 #2688 review: --restart-harness on a harness run by hand gets the same help
// as a typed yes when a terminal is there to wait on.
func TestUpgradeRestartFlagGuidesAHarnessRunByHand(t *testing.T) {
	r := newUpgradeRig(t, "0.5.0", "0.4.0")
	r.publishBoth(t, "0.5.0", true)
	r.opts.RestartHarness = RestartYes
	r.opts.Confirm = func(string) (bool, bool) { t.Fatal("asked although the flag said yes"); return false, false }
	waited := 0
	r.opts.Pause = func([]string, string) bool { waited++; return true }
	if code := Upgrade(context.Background(), r.opts, r.out); code != ExitOK {
		t.Fatalf("exit %d:\n%s", code, r.out)
	}
	if waited != 1 || r.runner.ran("hermes gateway restart") || !strings.Contains(r.out.String(), "hermes     restarted by you") {
		t.Fatalf("waited %d times; want one guided restart:\n%s", waited, r.out)
	}
}

// 🔴 #2688 review: a manifest with no restart command is never told it lacks a
// service — the reason given is the reason that holds.
func TestUpgradeRestartFlagGivesTheRightReason(t *testing.T) {
	r := newUpgradeRig(t, "0.5.0", "0.4.0")
	r.publishBoth(t, "0.5.0", false)
	r.opts.RestartHarness = RestartYes
	if code := Upgrade(context.Background(), r.opts, r.out); code != ExitOK {
		t.Fatalf("exit %d:\n%s", code, r.out)
	}
	if strings.Contains(r.out.String(), "does not run as its own service") || !strings.Contains(r.out.String(), "names no command to restart it") {
		t.Fatalf("the wrong reason was given:\n%s", r.out)
	}
}

// 🔴 #2688 re-review: --restart-harness with no terminal on a harness run by hand
// says the restart sentence once, and never "now" with nothing to wait on.
func TestUpgradeRestartFlagWithNoTerminalSaysTheSentenceOnce(t *testing.T) {
	r := newUpgradeRig(t, "0.5.0", "0.4.0")
	r.publishBoth(t, "0.5.0", true)
	r.opts.RestartHarness = RestartYes
	r.opts.Pause = func([]string, string) bool { return false } // no /dev/tty: prints nothing
	if code := Upgrade(context.Background(), r.opts, r.out); code != ExitOK {
		t.Fatalf("exit %d:\n%s", code, r.out)
	}
	out := r.out.String()
	if n := strings.Count(out, "Restart the Hermes gateway"); n != 1 || strings.Contains(out, "Restart it yourself now") {
		t.Fatalf("the restart sentence appears %d times, or a dangling \"now\":\n%s", n, out)
	}
	if !strings.Contains(out, "Until hermes restarts it keeps running plugin 0.4.0") {
		t.Fatalf("the pending paragraph is missing:\n%s", out)
	}
}
