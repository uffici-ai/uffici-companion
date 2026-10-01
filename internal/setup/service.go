package setup

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Runner starts a command. Injected so a test records what would run instead
// of running it, and so a dry run does the same for real.
//
// Run returns the combined output, for a command whose text the person should
// see when it fails. Stdout returns standard output ALONE, for a command whose
// output is data — a path — that a warning on stderr must not corrupt.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
	Stdout(ctx context.Context, name string, args ...string) (string, error)
}

// ExecRunner runs commands for real.
type ExecRunner struct{}

// Run implements Runner with os/exec. A failure carries the command's own
// output, trimmed: "exit status 1" from systemctl says nothing, its stderr
// says "Failed to connect to bus".
func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := command(ctx, name, args...).CombinedOutput()
	if err != nil {
		text := strings.TrimSpace(string(out))
		if text == "" {
			return "", fmt.Errorf("%s exited with an error: %w", name, err)
		}
		return string(out), fmt.Errorf("%s exited with an error: %w — %s", name, err, text)
	}
	return string(out), nil
}

// Stdout implements Runner.Stdout with os/exec.
func (ExecRunner) Stdout(ctx context.Context, name string, args ...string) (string, error) {
	out, err := command(ctx, name, args...).Output()
	if err != nil {
		return "", fmt.Errorf("%s exited with an error: %w", name, err)
	}
	return string(out), nil
}

// waitDelay bounds how long a command's output is read after the command
// itself ended or was killed.
const waitDelay = 5 * time.Second

// command builds the process a Runner starts. 🔴 WaitDelay: a context kill
// reaches only the direct process, and a child it left holding stdout (a
// harness restart that daemonised a gateway) would otherwise keep the output
// read — and the upgrade — waiting forever.
func command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = waitDelay
	return cmd
}

// ServiceLabel is the launchd label and the systemd unit name.
const ServiceLabel = "ai.uffici.companion"

// LogSettings are this machine's log-shipping switch and its floor for the
// harness's own lines, written into the service so they hold across restarts.
type LogSettings struct {
	// Shipping is `on` or `off`; empty is `on`.
	Shipping string
	// HarnessLevel is `debug`, `info`, `warn` or `error`; empty is `warn`.
	HarnessLevel string
}

func (l LogSettings) shipping() string { return NonEmpty(l.Shipping, "on") }

func (l LogSettings) harnessLevel() string { return NonEmpty(l.HarnessLevel, "warn") }

// SystemdUnit renders the user unit that keeps the companion running.
//
// The settings the gateway host needs ride in the unit, so the service is
// whole on its own: a person reading it sees where the companion connects, and
// whether — and how much of — its log leaves the machine.
func SystemdUnit(binary, gatewayURL, home string, logs LogSettings) string {
	return strings.Join([]string{
		"[Unit]",
		"Description=Uffici companion",
		"After=network-online.target",
		"",
		"[Service]",
		// Quoted, with `%` doubled: a home with a space or a `%` is otherwise a
		// unit systemd reads wrong (`%` is a specifier there).
		`Environment="UFFICI_AGENTS_HOST=gateway"`,
		`Environment="UFFICI_GATEWAY_URL=` + unitEscape(gatewayURL) + `"`,
		`Environment="UFFICI_HOME=` + unitEscape(home) + `"`,
		`Environment="UFFICI_LOG_SHIPPING=` + logs.shipping() + `"`,
		`Environment="UFFICI_LOG_LEVEL_HARNESS=` + logs.harnessLevel() + `"`,
		`ExecStart="` + unitEscape(binary) + `" run`,
		"Restart=always",
		"RestartSec=2",
		"",
		"[Install]",
		"WantedBy=default.target",
		"",
	}, "\n")
}

// LaunchdPlist renders the launch agent that keeps the companion running.
func LaunchdPlist(binary, gatewayURL, home string, logs LogSettings) string {
	return strings.Join([]string{
		`<?xml version="1.0" encoding="UTF-8"?>`,
		`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">`,
		`<plist version="1.0">`,
		`<dict>`,
		`  <key>Label</key><string>` + ServiceLabel + `</string>`,
		`  <key>ProgramArguments</key><array><string>` + xmlEscape(binary) + `</string><string>run</string></array>`,
		`  <key>EnvironmentVariables</key><dict>`,
		`    <key>UFFICI_AGENTS_HOST</key><string>gateway</string>`,
		`    <key>UFFICI_GATEWAY_URL</key><string>` + xmlEscape(gatewayURL) + `</string>`,
		`    <key>UFFICI_HOME</key><string>` + xmlEscape(home) + `</string>`,
		`    <key>UFFICI_LOG_SHIPPING</key><string>` + logs.shipping() + `</string>`,
		`    <key>UFFICI_LOG_LEVEL_HARNESS</key><string>` + logs.harnessLevel() + `</string>`,
		`  </dict>`,
		`  <key>RunAtLoad</key><true/>`,
		`  <key>KeepAlive</key><true/>`,
		`</dict>`,
		`</plist>`,
		``,
	}, "\n")
}

// unitEscape prepares a value for a quoted systemd unit field.
func unitEscape(s string) string {
	return strings.NewReplacer("%", "%%", `\`, `\\`, `"`, `\"`).Replace(s)
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}

// servicePlan is where the service file goes and what starts it, per OS.
type servicePlan struct {
	path     string
	content  string
	commands [][]string
}

// planService decides the service file and the commands for goos. userHome is
// the person's home directory, not the companion's; uid is launchd's domain.
func planService(goos, userHome, binary, gatewayURL, companionHome, uid string, logs LogSettings) (servicePlan, error) {
	switch goos {
	case "linux":
		return servicePlan{
			path:    filepath.Join(userHome, ".config", "systemd", "user", ServiceLabel+".service"),
			content: SystemdUnit(binary, gatewayURL, companionHome, logs),
			commands: [][]string{
				{"systemctl", "--user", "daemon-reload"},
				{"systemctl", "--user", "enable", ServiceLabel + ".service"},
				// restart, never `enable --now`: `--now` starts a stopped unit and
				// leaves a running one alone, and the companion reads its binary and
				// its agent key once, at start — so an upgrade or a re-pair would not
				// take until a reboot, and a re-paired machine would drop off when
				// the old key's overlap ends (#2213). restart starts a stopped unit too.
				{"systemctl", "--user", "restart", ServiceLabel + ".service"},
			},
		}, nil
	case "darwin":
		path := filepath.Join(userHome, "Library", "LaunchAgents", ServiceLabel+".plist")
		return servicePlan{
			path:    path,
			content: LaunchdPlist(binary, gatewayURL, companionHome, logs),
			commands: [][]string{
				// bootout first so a re-run replaces the running agent rather than
				// failing on "already loaded"; its failure on a first install is fine.
				{"launchctl", "bootout", "gui/" + uid + "/" + ServiceLabel},
				{"launchctl", "bootstrap", "gui/" + uid, path},
			},
		}, nil
	default:
		return servicePlan{}, fmt.Errorf("no service shape for %s — Linux (systemd) and macOS (launchd) are supported", goos)
	}
}
