package setup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Exit codes, the companion's: 0 done, 1 something failed, 2 the request
// itself was wrong.
const (
	ExitOK     = 0
	ExitFailed = 1
	ExitConfig = 2
)

// Options is everything `uffici setup` decides from, with every machine fact
// and every side effect injected so the whole run is testable in a temporary
// directory against a loopback release server.
type Options struct {
	// Harness is the kind whose plugin to install (`hermes`); empty installs none.
	Harness string
	// HarnessVersion is what pairing declares to the admin.
	HarnessVersion string
	// HarnessHome overrides the manifest's `homeEnv` / `defaultHome`.
	HarnessHome string
	// GatewayURL is the agents host; required unless both the service and the
	// pairing are skipped.
	GatewayURL string
	// PluginVersion pins the plugin release; empty is the latest.
	PluginVersion string

	NoPair, NoService, AssumeYes, DryRun bool

	// Facts about the machine.
	GOOS, UserHome, CompanionHome, Binary, UID string
	Env                                        func(string) string
	LookPath                                   func(string) (string, error)

	// Side effects.
	Runner  Runner
	Release func(repo string) Release
	Pair    func(ctx context.Context) int
	Doctor  func(ctx context.Context) int
}

// Run performs the setup and returns the exit code. Every line written to out
// is for the person: what was done, or on a dry run what would be.
func Run(ctx context.Context, opts Options, out io.Writer) int {
	if opts.Env == nil {
		opts.Env = func(string) string { return "" }
	}
	if opts.Harness == "" && opts.NoPair && opts.NoService {
		_, _ = fmt.Fprintln(out, "uffici setup: nothing to do — no harness, no pairing, no service")
		return ExitConfig
	}
	if opts.GatewayURL == "" && (!opts.NoService || !opts.NoPair) {
		_, _ = fmt.Fprintln(out, "uffici setup: --gateway-url (or UFFICI_GATEWAY_URL) is required: the service and the pairing both need the agents host's address")
		return ExitConfig
	}
	// Every step is announced BEFORE it runs — the person reads what is about to
	// happen, and a step that hangs is named — and confirmed after.
	do := func(what string, act func() error) error {
		if opts.DryRun {
			_, _ = fmt.Fprintf(out, "would %s\n", what)
			return nil
		}
		_, _ = fmt.Fprintf(out, "step: %s\n", what)
		if err := act(); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "done: %s\n", what)
		return nil
	}

	// Pairing FIRST, then the service: a service started before a key exists
	// crash-loops on "no agent key" until pairing lands, and can trip systemd's
	// start limit on the way. A key already in place is kept — a re-run of the
	// line is an upgrade, not a new pairing, which would mint a key an admin
	// then has to revoke.
	if !opts.NoPair {
		keyPath := filepath.Join(opts.CompanionHome, "agent-key")
		if _, err := os.Stat(keyPath); err == nil {
			_, _ = fmt.Fprintf(out, "already paired: %s exists; run `uffici pair` to pair again\n", keyPath)
		} else if opts.DryRun {
			_, _ = fmt.Fprintf(out, "would pair this machine with your agent (uffici pair --harness %s)\n", NonEmpty(opts.Harness, "unknown"))
		} else {
			_, _ = fmt.Fprintln(out, "step: pair this machine with your agent")
			if code := opts.Pair(ctx); code != ExitOK {
				_, _ = fmt.Fprintln(out, "uffici setup: pairing did not complete; nothing after it was done")
				return code
			}
		}
	}

	if !opts.NoService {
		if err := installService(ctx, opts, do); err != nil {
			_, _ = fmt.Fprintf(out, "uffici setup: %v\n", err)
			return ExitFailed
		}
	}

	if opts.Harness != "" {
		if err := installPlugin(ctx, opts, out, do); err != nil {
			_, _ = fmt.Fprintf(out, "uffici setup: %v\n", err)
			return ExitFailed
		}
	}

	if opts.DryRun {
		_, _ = fmt.Fprintln(out, "would run uffici doctor — a dry run changes nothing on this machine")
		return ExitOK
	}
	_, _ = fmt.Fprintln(out, "step: run uffici doctor")
	return opts.Doctor(ctx)
}

// NonEmpty is value, or fallback when value is empty.
func NonEmpty(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func installService(ctx context.Context, opts Options, do func(string, func() error) error) error {
	plan, err := planService(opts.GOOS, opts.UserHome, opts.Binary, opts.GatewayURL, opts.CompanionHome, opts.UID)
	if err != nil {
		return err
	}
	if err := do(fmt.Sprintf("write the service definition %s", plan.path), func() error {
		if err := os.MkdirAll(filepath.Dir(plan.path), 0o755); err != nil {
			return err
		}
		return os.WriteFile(plan.path, []byte(plan.content), 0o644)
	}); err != nil {
		return fmt.Errorf("installing the service: %w", err)
	}
	for _, argv := range plan.commands {
		optional := argv[0] == "launchctl" && argv[1] == "bootout"
		if err := do("run "+strings.Join(argv, " "), func() error {
			_, err := opts.Runner.Run(ctx, argv[0], argv[1:]...)
			if err != nil && optional {
				return nil
			}
			return err
		}); err != nil {
			return fmt.Errorf("starting the service: %w", err)
		}
	}
	return nil
}

func installPlugin(ctx context.Context, opts Options, out io.Writer, do func(string, func() error) error) error {
	repo := "uffici-for-" + opts.Harness
	release := opts.Release(repo)
	asset := repo + ".tar.gz"
	_, _ = fmt.Fprintf(out, "fetching %s from %s\n", asset, release.Describe())
	data, err := release.FetchVerified(ctx, asset)
	if err != nil {
		return err
	}
	stage, err := os.MkdirTemp("", "uffici-plugin-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	top, err := extractTar(data, stage, repo)
	if err != nil {
		return fmt.Errorf("unpacking %s: %w", asset, err)
	}
	raw, err := os.ReadFile(filepath.Join(top, ManifestFile))
	if err != nil {
		return fmt.Errorf("%s carries no %s at its top level", asset, ManifestFile)
	}
	manifest, err := ParseManifest(raw)
	if err != nil {
		return err
	}
	if manifest.Harness != opts.Harness {
		return fmt.Errorf("%s is for the harness %q, not %q", asset, manifest.Harness, opts.Harness)
	}
	_, _ = fmt.Fprintf(out, "plugin %s %s, for %s\n", manifest.Name, NonEmpty(manifest.Version, "(unversioned)"), manifest.Harness)

	home := opts.HarnessHome
	if home == "" {
		home = opts.Env(manifest.HomeEnv)
	}
	if home == "" {
		home = expandHome(manifest.DefaultHome, opts.UserHome)
	}
	if !filepath.IsAbs(home) {
		return fmt.Errorf("the harness home %q is not an absolute path — set --harness-home or %s to one", home, manifest.HomeEnv)
	}
	// Every tool the manifest will run is checked BEFORE anything is removed
	// or written, so a missing tool fails with the harness untouched.
	for _, argv := range [][]string{manifest.Enable, manifest.ConfigSet, manifest.EnvPath} {
		if len(argv) == 0 {
			continue
		}
		if _, err := opts.LookPath(argv[0]); err != nil {
			return fmt.Errorf("%s is not on the PATH — install %s first, or run this from the environment that has it", argv[0], opts.Harness)
		}
	}

	dest := filepath.Join(home, filepath.FromSlash(manifest.PluginDir))
	if err := do(fmt.Sprintf("install the plugin into %s", dest), func() error {
		if err := os.RemoveAll(dest); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		return copyTree(top, dest)
	}); err != nil {
		return fmt.Errorf("installing the plugin: %w", err)
	}

	if len(manifest.Config) > 0 && len(manifest.ConfigSet) == 0 {
		return fmt.Errorf("%s lists configuration to set but no configSet command", ManifestFile)
	}
	for _, cfg := range manifest.Config {
		argv := append(append([]string{}, manifest.ConfigSet...), cfg.Key, cfg.Value)
		if err := do("run "+strings.Join(argv, " "), func() error {
			_, err := opts.Runner.Run(ctx, argv[0], argv[1:]...)
			return err
		}); err != nil {
			return fmt.Errorf("configuring %s: %w", opts.Harness, err)
		}
	}

	env, err := manifest.RenderEnv(map[string]string{
		"wireSocket":    filepath.Join(opts.CompanionHome, "wire.sock"),
		"wireTokenFile": filepath.Join(opts.CompanionHome, "wire-token"),
		"companionHome": opts.CompanionHome,
		"home":          home,
	})
	if err != nil {
		return err
	}
	if len(env) > 0 {
		envPath := filepath.Join(home, manifest.EnvFile)
		if len(manifest.EnvPath) > 0 && !opts.DryRun {
			// Stdout alone, last non-empty line, and it must be absolute: a
			// warning on stderr or a banner must not become part of a path.
			if printed, err := opts.Runner.Stdout(ctx, manifest.EnvPath[0], manifest.EnvPath[1:]...); err == nil {
				if line := lastLine(printed); filepath.IsAbs(line) {
					envPath = line
				} else {
					_, _ = fmt.Fprintf(out, "%s printed no absolute path; using %s\n", strings.Join(manifest.EnvPath, " "), envPath)
				}
			}
		}
		if err := do(fmt.Sprintf("write %s into %s", strings.Join(sortedKeys(env), ", "), envPath), func() error {
			return mergeEnvFile(envPath, env)
		}); err != nil {
			return fmt.Errorf("writing the plugin's environment: %w", err)
		}
	}

	if err := do("run "+strings.Join(manifest.Enable, " "), func() error {
		_, err := opts.Runner.Run(ctx, manifest.Enable[0], manifest.Enable[1:]...)
		return err
	}); err != nil {
		return fmt.Errorf("enabling the plugin: %w", err)
	}
	_, _ = fmt.Fprintln(out, manifest.Restart)
	return nil
}

// lastLine is the last non-empty line of text, trimmed.
func lastLine(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func expandHome(path, userHome string) string {
	if path == "~" {
		return userHome
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(userHome, path[2:])
	}
	return path
}

func sortedKeys(m map[string]string) []string {
	return slices.Sorted(maps.Keys(m))
}

// mergeEnvFile sets each variable in a `KEY=value` file, replacing a line
// that already sets it and appending the rest, and keeps the file private.
func mergeEnvFile(path string, env map[string]string) error {
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var lines []string
	if len(existing) > 0 {
		lines = strings.Split(strings.TrimRight(string(existing), "\n"), "\n")
	}
	seen := map[string]bool{}
	for i, line := range lines {
		key, _, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(strings.TrimPrefix(key, "export "))
		if ok {
			if value, set := env[key]; set {
				lines[i] = key + "=" + value
				seen[key] = true
			}
		}
	}
	for _, key := range sortedKeys(env) {
		if !seen[key] {
			lines = append(lines, key+"="+env[key])
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}

// extractTar unpacks a gzipped tar whose every entry sits under one top-level
// directory named top, refusing anything that would land elsewhere or is not a
// plain file or directory. Returns the extracted top directory's path.
func extractTar(data []byte, dest, top string) (string, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("not a gzip archive: %w", err)
	}
	reader := tar.NewReader(gz)
	prefix := top + "/"
	var files int
	var total int64
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("not a tar archive: %w", err)
		}
		if header.Typeflag == tar.TypeXGlobalHeader {
			continue // pax global metadata (git archive writes one): not an entry
		}
		name := filepath.ToSlash(header.Name)
		name = strings.TrimPrefix(name, "./")
		if name == top || name == prefix {
			continue
		}
		if !strings.HasPrefix(name, prefix) || strings.Contains(name, "..") || filepath.IsAbs(name) {
			return "", fmt.Errorf("entry %q is outside %s/", header.Name, top)
		}
		target := filepath.Join(dest, filepath.FromSlash(name))
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return "", err
			}
			mode := os.FileMode(0o644)
			if header.Mode&0o111 != 0 {
				mode = 0o755
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return "", err
			}
			// One byte past the cap, per file and in total, is a refusal, never a
			// silent truncation.
			n, err := io.Copy(file, io.LimitReader(reader, MaxAssetBytes+1))
			total += n
			if err == nil && (n > MaxAssetBytes || total > MaxAssetBytes) {
				err = fmt.Errorf("the archive is larger than %d bytes, which no plugin of ours is", MaxAssetBytes)
			}
			if err != nil {
				_ = file.Close()
				return "", err
			}
			if err := file.Close(); err != nil {
				return "", err
			}
			files++
		default:
			return "", fmt.Errorf("entry %q is neither a file nor a directory (a symlink or device is refused)", header.Name)
		}
	}
	if files == 0 {
		return "", fmt.Errorf("the archive holds no files under %s/", top)
	}
	return filepath.Join(dest, top), nil
}

// copyTree copies every file under src to dst, creating directories as needed.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}
