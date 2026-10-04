package setup

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ExitUpgradeAvailable is `uffici upgrade --check`'s answer when an upgrade
// exists, so a cron or an operator can ask cheaply (#2250).
const ExitUpgradeAvailable = 10

// CompanionRepo is the companion's own release repository.
const CompanionRepo = "uffici-companion"

// DefaultRestartTimeout bounds the harness restart: a service manager's restart
// returns in seconds, and one that does not is not left to hang the upgrade.
const DefaultRestartTimeout = 3 * time.Minute

// UpgradeOptions is everything `uffici upgrade` decides from, every machine fact
// and side effect injected, like setup's.
type UpgradeOptions struct {
	// Check only reports installed versus latest and changes nothing.
	Check bool
	// DryRun announces every step and performs none.
	DryRun bool
	// Version pins the companion release; PluginVersion the plugin's. Empty is
	// the version the agents gateway names, else the latest (#2382).
	Version, PluginVersion string
	// GatewayURL is the --gateway-url flag; empty resolves it from the
	// environment, the setup record, then DefaultGatewayURL (ResolveGateway).
	GatewayURL string
	// Gateway asks an agents gateway which release its machines run; nil asks
	// through the companion's Release (Release.GatewayVersion).
	Gateway func(ctx context.Context, gatewayURL string) (GatewayRelease, error)
	// Current is this binary's version.
	Current string
	// FromVersion is the version the previous binary had, when this run is the
	// new binary taking over after the replace; empty otherwise.
	FromVersion string

	GOOS, GOARCH, UserHome, CompanionHome, Binary, UID string
	Env                                                func(string) string
	LookPath                                           func(string) (string, error)

	Runner Runner
	// Release names one repository's release, at a version (empty: the latest).
	Release func(repo, version string) Release
	// Exec replaces this process with the new binary; it returns only on failure.
	Exec   func(binary string, args []string) error
	Doctor func(ctx context.Context) int
	// RunningVersion is the version the companion serving this home reports,
	// and whether one answered. Nil answers nothing.
	RunningVersion func(ctx context.Context) (string, bool)
	// RestartTimeout bounds the harness restart; zero is DefaultRestartTimeout.
	RestartTimeout time.Duration
	// RestartHarness is the person's say on restarting the harness (#2278);
	// Confirm asks when that is RestartAsk. Nil Confirm is a run with no
	// terminal: the harness is not restarted.
	RestartHarness RestartChoice
	Confirm        func(question string) (yes, answered bool)
}

// Upgrade moves the companion and the attached harness's plugin to their latest
// releases, re-applies the plugin's harness settings, restarts both and says
// what changed (#2250). It needs no flags: the setup record names the harness,
// its home and the agents host. It keeps the agent key and never pairs.
//
// 🔴 It runs in TWO processes. The binary it replaces is the one running, and
// that binary does not know the manifest fields a newer plugin release may
// carry — a manifest refuses what it does not know. So the old binary only
// replaces itself and hands over (Exec) to the new one, which installs the
// plugin, restarts and reports: the same handover install.sh makes to setup.
func Upgrade(ctx context.Context, opts UpgradeOptions, out io.Writer) int {
	say := func(format string, args ...any) { _, _ = fmt.Fprintf(out, format+"\n", args...) }
	fail := func(format string, args ...any) int {
		say("uffici upgrade: "+format, args...)
		return ExitFailed
	}

	record, err := ReadRecord(opts.CompanionHome)
	if errors.Is(err, ErrNoRecord) {
		say("uffici upgrade: this machine has no setup record (%s), so there is nothing to read the harness and the agents host from.", filepath.Join(opts.CompanionHome, RecordFile))
		say("Run the setup line once — it keeps the agent key and pairs nothing — and upgrade from then on:")
		say("  uffici setup --no-pair --harness <your harness, e.g. hermes> --gateway-url <your agents host>")
		return ExitConfig
	}
	if err != nil {
		return fail("%v", err)
	}

	// ── Which gateway, and which version it names (#2382) ───────────────────
	gateway, from := ResolveGateway(opts.GatewayURL, opts.Env, record.GatewayURL)
	gatewayChanged := gateway != record.GatewayURL
	say("agents gateway: %s (%s)", gateway, from)
	// The gateway's version is a pin: it moves the machine up OR down, so a
	// rolled-back environment rolls its machines back. Only the latest-release
	// path refuses a downgrade. Asked once, and only when something is unpinned.
	var choice releaseChoice
	if opts.Version == "" || (record.Harness != "" && NonEmpty(opts.PluginVersion, opts.Version) == "") {
		ask := opts.Gateway
		if ask == nil {
			ask = opts.Release(CompanionRepo, "").GatewayVersion
		}
		choice = askGateway(ctx, ask, gateway)
		say("%s", choice.Line())
	}

	// ── What is installed, and what is released ─────────────────────────────
	companionTarget, err := targetVersion(ctx, opts, CompanionRepo, opts.Version, choice.version())
	if err != nil {
		return fail("%v", err)
	}
	companionMove, err := decide("the companion", opts.Current, companionTarget, NonEmpty(opts.Version, choice.version()) != "")
	if err != nil {
		say("uffici upgrade: %v", err)
		return ExitConfig
	}

	var plugin pluginState
	if record.Harness != "" {
		plugin.repo = "uffici-for-" + record.Harness
		plugin.installed = installedPluginVersion(record)
		// One train: a pinned companion pins the plugin too, unless the plugin
		// was pinned on its own — the same rule setup follows.
		pluginPin := NonEmpty(opts.PluginVersion, opts.Version)
		plugin.target, err = targetVersion(ctx, opts, plugin.repo, pluginPin, choice.version())
		if err != nil {
			return fail("%v", err)
		}
		plugin.move, err = decide("the plugin", plugin.installed, plugin.target, NonEmpty(pluginPin, choice.version()) != "")
		if err != nil {
			say("uffici upgrade: %v", err)
			return ExitConfig
		}
	}

	// 🔴 The new binary was started with --version pinned to what it should
	// be. If it is not that version the release is mis-stamped, and replacing
	// and handing over again would loop forever.
	if opts.FromVersion != "" && companionMove {
		return fail("the new companion reports %s, not %s — the release is mis-stamped; nothing further was changed", opts.Current, companionTarget)
	}
	// A daemon still running an older binary than the one on disk: a handover
	// that failed, or a run that stopped before the restart. Restarting it is
	// what finishes that upgrade.
	stale := ""
	if record.Service && !companionMove && opts.RunningVersion != nil {
		if running, ok := opts.RunningVersion(ctx); ok && running != opts.Current {
			stale = NonEmpty(running, "an unreported version")
		}
	}

	say("companion  %s", describeMove(opts.Current, companionTarget, companionMove))
	if stale != "" {
		say("           the service still runs %s — it will be restarted", stale)
	}
	if record.Harness != "" {
		say("plugin     %s", describeMove(NonEmpty(plugin.installed, "not installed"), plugin.target, plugin.move))
	}
	if opts.Check {
		if companionMove || plugin.move || stale != "" {
			return ExitUpgradeAvailable
		}
		return ExitOK
	}
	if !companionMove && !plugin.move && opts.FromVersion == "" && stale == "" && !gatewayChanged {
		say("Everything is current; nothing to do.")
		return ExitOK
	}

	do := func(what string, act func() error) error {
		if opts.DryRun {
			say("would %s", what)
			return nil
		}
		say("step: %s", what)
		if err := act(); err != nil {
			return err
		}
		say("done: %s", what)
		return nil
	}

	// The resolved gateway is remembered BEFORE any handover, so the new binary
	// — which gets no --gateway-url — resolves the same one.
	if gatewayChanged {
		if err := do(fmt.Sprintf("remember the agents gateway %s in %s", gateway, filepath.Join(opts.CompanionHome, RecordFile)), func() error {
			changed := record
			changed.GatewayURL = gateway
			return WriteRecord(opts.CompanionHome, changed)
		}); err != nil {
			return fail("remembering the setup: %v", err)
		}
		record.GatewayURL = gateway
	}

	// ── The companion: replace the binary, then hand over ───────────────────
	if companionMove {
		if err := replaceBinary(ctx, opts, companionTarget, do); err != nil {
			return fail("%v", err)
		}
		// 🔴 A dry run stops here: the plugin's manifest is the NEW binary's to
		// read, and the old one may refuse a field it does not know.
		if opts.DryRun {
			say("would hand over to the new companion, which would restart the companion's service, install the plugin, restart the harness and run the doctor")
			return ExitOK
		}
		args := []string{"upgrade", "--home", opts.CompanionHome, "--version", companionTarget, "--from-version", opts.Current}
		if record.Harness != "" {
			args = append(args, "--plugin-version", plugin.target)
		}
		// The person's say travels with the handover; the new binary asks on the
		// same terminal when there is none (#2278). 🔴 Only to a binary that knows
		// the flags: a pinned downgrade hands over to an older release, whose flag
		// parser would refuse them AFTER the swap — and refuse every re-run too.
		switch {
		case knowsRestartFlags(companionTarget):
			args = append(args, opts.RestartHarness.Flags()...)
		case opts.RestartHarness != RestartAsk:
			say("uffici %s does not know %s: it restarts the harness as that release does", companionTarget, opts.RestartHarness.Flags()[0])
		}
		say("handing over to the new companion: uffici %s", strings.Join(args, " "))
		err := opts.Exec(opts.Binary, args)
		return fail("the new companion is installed but did not take over (%v) — run `uffici upgrade` again to finish: it restarts the service and installs the plugin", err)
	}

	// ── The plugin, its settings, and the restarts ──────────────────────────
	setupOpts := Options{
		Harness: record.Harness, HarnessHome: record.HarnessHome, GatewayURL: gateway,
		PluginVersion: plugin.target, DryRun: opts.DryRun,
		// 🔴 The machine's log settings as the last setup wrote them: an upgrade
		// rewrites the unit, and must not quietly turn shipping back on.
		Logs: LogSettings{Shipping: record.LogShipping, HarnessLevel: record.HarnessLogLevel},
		GOOS: opts.GOOS, UserHome: opts.UserHome, CompanionHome: opts.CompanionHome, Binary: opts.Binary, UID: opts.UID,
		Env: opts.Env, LookPath: opts.LookPath, Runner: opts.Runner,
		Release: func(repo string) Release { return opts.Release(repo, plugin.target) },
	}
	// A new binary only runs once the service restarts; the service unit is
	// rewritten too, so it names this binary and this agents host — and a new
	// agents host alone rewrites it. FIRST, so a plugin step that fails cannot
	// leave the old daemon running.
	companionChanged := opts.FromVersion != "" || stale != ""
	switch {
	case (companionChanged || gatewayChanged) && record.Service:
		if err := installService(ctx, setupOpts, do); err != nil {
			return fail("%v", err)
		}
	case companionChanged:
		say("The companion was not installed as a service here: restart it yourself to run the new version.")
	}

	var installed installedPlugin
	if plugin.move {
		installed, err = installPlugin(ctx, setupOpts, out, do)
		if err != nil {
			return fail("%v", err)
		}
		if !opts.DryRun {
			record.HarnessHome, record.PluginDir = installed.home, installed.manifest.PluginDir
			if err := WriteRecord(opts.CompanionHome, record); err != nil {
				return fail("remembering the setup: %v", err)
			}
		}
	}

	harnessLine := "unchanged"
	if plugin.move {
		harnessLine = restartHarness(ctx, harnessRestart{
			GOOS: opts.GOOS, UserHome: opts.UserHome, UID: opts.UID, Env: opts.Env, Runner: opts.Runner,
			Timeout: opts.RestartTimeout, Choice: opts.RestartHarness, Confirm: opts.Confirm, DryRun: opts.DryRun,
		}, installed, do, say)
	}

	if opts.DryRun {
		say("would run uffici doctor — a dry run changes nothing on this machine")
		return ExitOK
	}
	say("step: run uffici doctor")
	code := opts.Doctor(ctx)

	say("")
	say("companion  %s", describeDone(NonEmpty(opts.FromVersion, opts.Current), opts.Current))
	if record.Harness != "" {
		say("plugin     %s", describeDone(NonEmpty(plugin.installed, "not installed"), plugin.target))
		say("%-10s %s", record.Harness, harnessLine)
	}
	return code
}

// pluginState is the plugin's side of the plan.
type pluginState struct {
	repo, installed, target string
	move                    bool
}

// targetVersion is the pinned version, else the one the agents gateway names,
// else the repository's latest (#2382).
func targetVersion(ctx context.Context, opts UpgradeOptions, repo, pinned, gatewayVersion string) (string, error) {
	if pinned != "" {
		if _, ok := parseVersion(pinned); !ok {
			return "", fmt.Errorf("%q is not a release version (like 1.2.3)", pinned)
		}
		return strings.TrimPrefix(pinned, "v"), nil
	}
	if gatewayVersion != "" {
		return gatewayVersion, nil
	}
	return opts.Release(repo, "").LatestVersion(ctx)
}

// decide reports whether what moves from installed to target. A pinned target
// — a flag, or the version the agents gateway names — is installed exactly, up
// or down (#2382); only the latest release refuses a downgrade.
func decide(what, installed, target string, pinned bool) (bool, error) {
	if installed == "" {
		return true, nil
	}
	if pinned {
		// By name, not by order: two prereleases of one release (`-dev.3`,
		// `-dev.4`) are different builds that parseVersion ranks alike.
		return strings.TrimPrefix(installed, "v") != target, nil
	}
	have, ok := parseVersion(installed)
	if !ok {
		return false, fmt.Errorf("%s is %s, a development build rather than a release; pass the version to install (like --version %s) to replace it anyway", what, installed, target)
	}
	want, _ := parseVersion(target)
	switch compareVersions(have, want) {
	case 0:
		return false, nil
	case 1:
		return false, fmt.Errorf("%s is %s, newer than the latest release %s — upgrade never downgrades; pin the version to install one on purpose", what, installed, target)
	default:
		return true, nil
	}
}

func describeMove(installed, target string, move bool) string {
	if !move {
		return installed + " (current)"
	}
	return installed + " → " + target + " available"
}

func describeDone(before, after string) string {
	if before == after {
		return after + " (unchanged)"
	}
	return before + " → " + after
}

// installedPluginVersion reads the version the installed plugin's own manifest
// carries; empty when none is installed or it says nothing.
func installedPluginVersion(record Record) string {
	if record.HarnessHome == "" || record.PluginDir == "" {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(record.HarnessHome, filepath.FromSlash(record.PluginDir), ManifestFile))
	if err != nil {
		return ""
	}
	manifest, err := ParseManifest(raw)
	if err != nil {
		return ""
	}
	return manifest.Version
}

// companionAsset is the release archive for this machine, as install.sh names it.
func companionAsset(goos, goarch string) string {
	return fmt.Sprintf("uffici_%s_%s.tar.gz", goos, goarch)
}

// replaceBinary fetches the release for this machine, verifies it against the
// release's checksums exactly as install.sh does, and swaps it in ATOMICALLY:
// written beside the running binary, then renamed over it. A failed download
// or check leaves the installed binary untouched.
func replaceBinary(ctx context.Context, opts UpgradeOptions, version string, do func(string, func() error) error) error {
	release := opts.Release(CompanionRepo, version)
	asset := companionAsset(opts.GOOS, opts.GOARCH)
	if opts.DryRun {
		return do(fmt.Sprintf("fetch %s from %s, verify it and replace %s", asset, release.Describe(), opts.Binary), nil)
	}
	data, err := release.FetchVerified(ctx, asset)
	if err != nil {
		return err
	}
	binary, err := binaryFromArchive(data)
	if err != nil {
		return fmt.Errorf("unpacking %s: %w", asset, err)
	}
	return do(fmt.Sprintf("replace %s with uffici %s", opts.Binary, version), func() error {
		staged := opts.Binary + ".new"
		if err := os.WriteFile(staged, binary, 0o755); err != nil {
			return err
		}
		// WriteFile keeps a leftover file's mode; the binary must be executable.
		if err := os.Chmod(staged, 0o755); err != nil {
			_ = os.Remove(staged)
			return err
		}
		if err := os.Rename(staged, opts.Binary); err != nil {
			_ = os.Remove(staged)
			return err
		}
		return nil
	})
}

// binaryFromArchive reads the one `uffici` file a companion release archive
// carries at its top level.
func binaryFromArchive(data []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("not a gzip archive: %w", err)
	}
	reader := tar.NewReader(gz)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("the archive holds no uffici binary")
		}
		if err != nil {
			return nil, fmt.Errorf("not a tar archive: %w", err)
		}
		if strings.TrimPrefix(filepath.ToSlash(header.Name), "./") != "uffici" || header.Typeflag != tar.TypeReg {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(reader, MaxAssetBytes+1))
		if err != nil {
			return nil, err
		}
		if len(body) > MaxAssetBytes || len(body) == 0 {
			return nil, errors.New("the archive's uffici binary is empty or too large")
		}
		return body, nil
	}
}

// RestartChoice is the person's say on restarting the harness after its plugin
// is installed (#2278).
type RestartChoice int

const (
	// RestartAsk asks at a terminal, and without one leaves the harness alone.
	RestartAsk RestartChoice = iota
	// RestartYes restarts without asking (`--restart-harness`, `--yes`).
	RestartYes
	// RestartNo never restarts (`--no-restart-harness`).
	RestartNo
)

// Flags are the command-line flags that carry this choice to another run.
func (c RestartChoice) Flags() []string {
	switch c {
	case RestartYes:
		return []string{"--restart-harness"}
	case RestartNo:
		return []string{"--no-restart-harness"}
	}
	return nil
}

// RestartFlagsSince is the first release whose binary knows --restart-harness
// and --no-restart-harness (#2278).
const RestartFlagsSince = "0.6.0"

// knowsRestartFlags reports whether a release's binary parses the restart flags.
func knowsRestartFlags(version string) bool {
	v, ok := parseVersion(version)
	since, _ := parseVersion(RestartFlagsSince)
	return ok && compareVersions(v, since) >= 0
}

// ParseRestartChoice is the choice the two flags make; both at once is an error.
func ParseRestartChoice(restart, noRestart bool) (RestartChoice, error) {
	switch {
	case restart && noRestart:
		return RestartAsk, errors.New("--restart-harness and --no-restart-harness contradict each other; pass one")
	case restart:
		return RestartYes, nil
	case noRestart:
		return RestartNo, nil
	}
	return RestartAsk, nil
}

// AskRestart asks the restart question and reads the answer from in: an empty
// line, y or yes is a yes (the question's default), n or no a no, and anything
// else asks again. answered is false when in ends before an answer.
// Not pairing's `confirm` (internal/app/pair.go), deliberately: that one mints a
// key and defaults to no, and it cannot say that nobody answered.
func AskRestart(in io.Reader, out io.Writer, question string) (yes, answered bool) {
	reader := bufio.NewReader(in)
	for {
		_, _ = fmt.Fprint(out, question)
		line, err := reader.ReadString('\n')
		if err != nil && line == "" {
			return false, false
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "", "y", "yes":
			return true, true
		case "n", "no":
			return false, true
		}
		if err != nil {
			return false, false
		}
	}
}

// harnessRestart is what restarting the harness decides from, for setup and
// upgrade alike.
type harnessRestart struct {
	GOOS, UserHome, UID string
	Env                 func(string) string
	Runner              Runner
	Timeout             time.Duration
	Choice              RestartChoice
	Confirm             func(question string) (yes, answered bool)
	DryRun              bool
}

// restartHarness restarts the harness with the manifest's command when that is
// safe AND the person said so, and otherwise tells the person how. It returns
// the summary line.
//
// 🔴 Safe means: the manifest declares both the command and the service it
// restarts, that service's definition exists for this user, and the plugin went
// into the harness's DEFAULT home. A harness run by hand has no service to
// restart, and its restart command may then start it in the foreground — Hermes's
// does — which would hang this run; a non-default home is served by a
// differently named service the manifest cannot know. Safety is checked FIRST:
// the person is never asked a question whose yes could not be carried out.
//
// 🔴 Said so means a flag, `--yes`, or a yes at the terminal (#2278): a
// person's harness is never restarted without their say, so a run with none of
// the three — a scheduled upgrade, a pipe with no terminal — leaves it alone.
func restartHarness(ctx context.Context, r harnessRestart, installed installedPlugin, do func(string, func() error) error, say func(string, ...any)) string {
	manifest := installed.manifest
	if len(manifest.RestartCommand) == 0 || manifest.RestartService == nil {
		say("%s", manifest.Restart)
		return "restart it yourself to load the new plugin"
	}
	if !harnessServiceRunning(ctx, r, *manifest.RestartService) ||
		installed.home != expandHome(manifest.DefaultHome, r.UserHome) {
		say("%s", manifest.Restart)
		return "not run as a service here — restart it yourself to load the new plugin"
	}
	switch r.Choice {
	case RestartNo:
		say("%s", manifest.Restart)
		return "not restarted — you said no; restart it yourself to load the new plugin"
	case RestartAsk:
		if r.DryRun {
			say("would ask whether to restart %s now to load the plugin", manifest.Harness)
			return "would ask before restarting"
		}
		yes, answered := false, false
		if r.Confirm != nil {
			yes, answered = r.Confirm(fmt.Sprintf("Restart %s now to load the plugin? [Y/n] ", manifest.Harness))
		}
		switch {
		case !answered:
			say("%s", manifest.Restart)
			return "not restarted — no terminal to ask (--restart-harness restarts it); restart it yourself to load the new plugin"
		case !yes:
			say("%s", manifest.Restart)
			return "not restarted — you said no; restart it yourself to load the new plugin"
		}
	}
	timeout := r.Timeout
	if timeout == 0 {
		timeout = DefaultRestartTimeout
	}
	argv := manifest.RestartCommand
	if err := do("run "+strings.Join(argv, " "), func() error {
		restartCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		_, err := r.Runner.Run(restartCtx, argv[0], argv[1:]...)
		return err
	}); err != nil {
		say("The harness restart failed (%v). %s", err, manifest.Restart)
		return "restart failed — restart it yourself to load the new plugin"
	}
	if r.DryRun {
		return "would restart"
	}
	return "restarted"
}

// harnessServiceRunning reports whether the harness runs as its own user
// service right now: the definition exists AND the manager says it is running.
// 🔴 Installed is not enough: a person who once installed the service and now
// runs the gateway by hand has a stopped unit, and restarting it would start a
// second gateway beside theirs.
func harnessServiceRunning(ctx context.Context, r harnessRestart, service RestartService) bool {
	if !harnessServiceInstalled(service, r.GOOS, r.UserHome, r.Env) {
		return false
	}
	var argv []string
	switch r.GOOS {
	case "linux":
		argv = []string{"systemctl", "--user", "is-active", "--quiet", service.Systemd}
	case "darwin":
		argv = []string{"launchctl", "print", "gui/" + r.UID + "/" + service.Launchd}
	}
	_, err := r.Runner.Run(ctx, argv[0], argv[1:]...)
	return err == nil
}

// harnessServiceInstalled reports whether the harness's own user service
// definition exists: a systemd user unit on Linux, a launch agent on macOS.
func harnessServiceInstalled(service RestartService, goos, userHome string, env func(string) string) bool {
	var path string
	switch {
	case goos == "linux" && service.Systemd != "":
		config := env("XDG_CONFIG_HOME")
		if config == "" {
			config = filepath.Join(userHome, ".config")
		}
		path = filepath.Join(config, "systemd", "user", service.Systemd+".service")
	case goos == "darwin" && service.Launchd != "":
		path = filepath.Join(userHome, "Library", "LaunchAgents", service.Launchd+".plist")
	default:
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// parseVersion reads MAJOR.MINOR.PATCH with an optional -prerelease; ok is
// false for anything else, the development build included.
func parseVersion(v string) ([4]int, bool) {
	core, pre, hasPre := strings.Cut(strings.TrimPrefix(v, "v"), "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 || (hasPre && (pre == "" || pre == "dev")) {
		return [4]int{}, false
	}
	var out [4]int
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return [4]int{}, false
		}
		out[i] = n
	}
	// A prerelease sorts before its release.
	if !hasPre {
		out[3] = 1
	}
	return out, true
}

// IsReleaseVersion reports whether v is a release version (`1.2.3`,
// `1.2.3-rc.1`), as every comparison here reads one.
func IsReleaseVersion(v string) bool {
	_, ok := parseVersion(v)
	return ok
}

func compareVersions(a, b [4]int) int {
	for i := range a {
		switch {
		case a[i] > b[i]:
			return 1
		case a[i] < b[i]:
			return -1
		}
	}
	return 0
}
