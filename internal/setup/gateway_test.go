package setup

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// noGatewayVersion is a gateway that names no version: the latest-release path.
func noGatewayVersion(context.Context, string) (GatewayRelease, error) {
	return GatewayRelease{Channel: "main"}, nil
}

// gatewayNaming is a gateway on channel that names version, counting the asks.
func gatewayNaming(channel, version string, asked *int) func(context.Context, string) (GatewayRelease, error) {
	return func(context.Context, string) (GatewayRelease, error) {
		if asked != nil {
			*asked++
		}
		return GatewayRelease{Channel: channel, Version: version}, nil
	}
}

// releaseGateway serves `GET /v1/release` from loopback with body, recording
// whether any request carried an Authorization header.
func releaseGateway(t *testing.T, status int, body string, sawAuth *bool) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" && sawAuth != nil {
			*sawAuth = true
		}
		if r.URL.Path != "/v1/release" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestGatewayVersionReadsTheAnswer(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	for _, tc := range []struct {
		name, body, wantVersion, wantChannel, wantErr string
		status                                        int
		url                                           string
	}{
		{name: "a dev train", status: 200, body: `{"channel":"develop","version":"0.10.0-dev.41"}`, wantChannel: "develop", wantVersion: "0.10.0-dev.41"},
		{name: "a release", status: 200, body: `{"channel":"main","version":"0.9.0"}`, wantChannel: "main", wantVersion: "0.9.0"},
		{name: "no version", status: 200, body: `{"channel":"uac","version":null}`, wantChannel: "uac"},
		{name: "not a release version", status: 200, body: `{"channel":"main","version":"latest"}`, wantErr: "not a release version"},
		{name: "not a release", status: 200, body: `<html>`, wantErr: "not a release"},
		{name: "no route", status: 404, body: ``, wantErr: "answered 404"},
		{name: "unreachable", url: closed.URL, wantErr: "could not be reached (unreachable)"},
		{name: "plain http off loopback", url: "http://agents.example", wantErr: "not an https origin"},
		{name: "none set", url: " ", wantErr: "no agents gateway"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sawAuth bool
			url := tc.url
			if url == "" {
				url = releaseGateway(t, tc.status, tc.body, &sawAuth).URL
			}
			// 🔴 A release with a GitHub token: the gateway must never receive it.
			got, err := Release{Token: testToken}.GatewayVersion(context.Background(), url)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got.Version != tc.wantVersion || got.Channel != tc.wantChannel {
				t.Fatalf("got %+v, %v; want %s / %q", got, err, tc.wantChannel, tc.wantVersion)
			}
			if sawAuth {
				t.Fatal("the GitHub token was sent to the agents gateway")
			}
		})
	}
}

// #2382: --gateway-url, then UFFICI_GATEWAY_URL, then the record, then the
// production default — each step beating the next.
func TestResolveGatewayFourSteps(t *testing.T) {
	env := func(value string) func(string) string {
		return func(key string) string {
			if key == "UFFICI_GATEWAY_URL" {
				return value
			}
			return ""
		}
	}
	for _, tc := range []struct {
		name, flag, env, recorded, want, from string
	}{
		{name: "the flag beats all", flag: "https://flag.example", env: "https://env.example", recorded: "https://record.example", want: "https://flag.example", from: "from --gateway-url"},
		{name: "the variable beats the record", env: "https://env.example", recorded: "https://record.example", want: "https://env.example", from: "from UFFICI_GATEWAY_URL"},
		{name: "the record beats the default", recorded: "https://record.example", want: "https://record.example", from: "recorded in setup.json"},
		{name: "the default", want: "https://agents.uffici.ai", from: "the default"},
		{name: "blank is unset", flag: "  ", env: " ", recorded: "", want: DefaultGatewayURL, from: "the default"},
	} {
		got, from := ResolveGateway(tc.flag, env(tc.env), tc.recorded)
		if got != tc.want || from != tc.from {
			t.Fatalf("%s: got %s (%s), want %s (%s)", tc.name, got, from, tc.want, tc.from)
		}
	}
}

// ── uffici upgrade ──────────────────────────────────────────────────────────

func versionLines(out string) int {
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "version: ") {
			n++
		}
	}
	return n
}

func TestUpgradeTakesTheVersionTheGatewayNames(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		version, pluginVersion    string
		gateway                   string // the version the gateway names; empty names none
		unreachable               bool
		wantCompanion, wantPlugin string
		wantLine                  string
		wantAsked                 int
	}{
		{name: "the gateway beats the latest", gateway: "0.6.0-dev.2",
			wantCompanion: "0.4.0 → 0.6.0-dev.2 available", wantPlugin: "0.4.0 → 0.6.0-dev.2 available",
			wantLine: "version: 0.6.0-dev.2, named by the agents gateway https://agents.example (channel develop)", wantAsked: 1},
		{name: "a pin beats the gateway", version: "0.5.0", pluginVersion: "0.5.0", gateway: "0.6.0-dev.2",
			wantCompanion: "0.4.0 → 0.5.0 available", wantPlugin: "0.4.0 → 0.5.0 available"},
		// 🔴 One train: a companion pin pins the plugin too — never the companion at
		// one version and its plugin at the gateway's (setup's rule, review of #2382).
		{name: "a companion pin pins the plugin too", version: "0.4.5", gateway: "0.6.0-dev.2",
			wantCompanion: "0.4.0 → 0.4.5 available", wantPlugin: "0.4.0 → 0.4.5 available"},
		{name: "a plugin pin of its own still wins", version: "0.5.0", pluginVersion: "0.4.0", gateway: "0.6.0-dev.2",
			wantCompanion: "0.4.0 → 0.5.0 available", wantPlugin: "0.4.0 (current)"},
		{name: "a null version falls back to the latest", gateway: "",
			wantCompanion: "0.4.0 → 0.5.0 available", wantPlugin: "0.4.0 → 0.5.0 available",
			wantLine: "version: the latest release — the agents gateway https://agents.example (channel develop) names no version", wantAsked: 1},
		{name: "an unreachable gateway falls back to the latest", unreachable: true,
			wantCompanion: "0.4.0 → 0.5.0 available", wantPlugin: "0.4.0 → 0.5.0 available",
			wantLine: "version: the latest release — the agents gateway https://agents.example could not be reached (unreachable)", wantAsked: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newUpgradeRig(t, "0.4.0", "0.4.0")
			r.publishBoth(t, "0.5.0", true)
			r.opts.Check, r.opts.Version, r.opts.PluginVersion = true, tc.version, tc.pluginVersion
			asked := 0
			r.opts.Gateway = gatewayNaming("develop", tc.gateway, &asked)
			if tc.unreachable {
				r.opts.Gateway = func(context.Context, string) (GatewayRelease, error) {
					asked++
					return GatewayRelease{}, errors.New("could not be reached (unreachable)")
				}
			}
			if code := Upgrade(context.Background(), r.opts, r.out); code != ExitUpgradeAvailable {
				t.Fatalf("exit %d:\n%s", code, r.out)
			}
			out := r.out.String()
			if !strings.Contains(out, "companion  "+tc.wantCompanion) || !strings.Contains(out, "plugin     "+tc.wantPlugin) {
				t.Fatalf("want companion %q and plugin %q:\n%s", tc.wantCompanion, tc.wantPlugin, out)
			}
			if asked != tc.wantAsked {
				t.Fatalf("the gateway was asked %d times, want %d", asked, tc.wantAsked)
			}
			if tc.wantLine != "" && !strings.Contains(out, tc.wantLine) {
				t.Fatalf("no line %q:\n%s", tc.wantLine, out)
			}
			// ONE line names the path, however many components read it.
			if want := min(tc.wantAsked, 1); versionLines(out) != want {
				t.Fatalf("%d version lines, want %d:\n%s", versionLines(out), want, out)
			}
			if !strings.Contains(out, "agents gateway: https://agents.example (recorded in setup.json)") {
				t.Fatalf("the gateway and its source are not named:\n%s", out)
			}
		})
	}
}

// 🔴 A gateway's version is a pin: a rolled-back environment rolls its machines
// back, where the latest-release path never downgrades.
func TestUpgradeAGatewayVersionBelowTheInstalledOneMovesDown(t *testing.T) {
	r := newUpgradeRig(t, "0.6.0", "0.6.0")
	r.publishBoth(t, "0.5.0", true)
	r.opts.Gateway = gatewayNaming("main", "0.5.0", nil)
	if code := Upgrade(context.Background(), r.opts, r.out); code != ExitFailed || len(r.execs) != 1 {
		t.Fatalf("exit %d, %d execs (the injected Exec returns, so a handover reads as failed):\n%s", code, len(r.execs), r.out)
	}
	if r.binary(t) != "new binary 0.5.0" {
		t.Fatalf("the binary was not moved down to 0.5.0: %q\n%s", r.binary(t), r.out)
	}
	if got := strings.Join(r.execs[0], " "); !strings.Contains(got, "--version 0.5.0") || !strings.Contains(got, "--plugin-version 0.5.0") {
		t.Fatalf("the handover does not pin the gateway's version: %s", got)
	}

	// Non-vacuity: the same machine on the latest-release path refuses.
	latest := newUpgradeRig(t, "0.6.0", "0.6.0")
	latest.publishBoth(t, "0.5.0", true)
	if code := Upgrade(context.Background(), latest.opts, latest.out); code != ExitConfig || !strings.Contains(latest.out.String(), "never downgrades") {
		t.Fatalf("the latest path did not refuse the downgrade: exit %d\n%s", code, latest.out)
	}
}

// Two builds of one train differ by name, though they rank alike.
func TestUpgradeMovesBetweenTwoDevBuildsOfOneRelease(t *testing.T) {
	r := newUpgradeRig(t, "0.6.0-dev.3", "0.6.0-dev.3")
	r.publishBoth(t, "0.5.0", true)
	r.opts.Check = true
	r.opts.Gateway = gatewayNaming("develop", "0.6.0-dev.4", nil)
	if code := Upgrade(context.Background(), r.opts, r.out); code != ExitUpgradeAvailable || !strings.Contains(r.out.String(), "0.6.0-dev.3 → 0.6.0-dev.4 available") {
		t.Fatalf("exit %d:\n%s", code, r.out)
	}
}

// The gateway asked is the resolved one; a new one is remembered and written
// into the service, and the default is taken — and named — when nothing else is.
func TestUpgradeResolvesTheGatewayAndRemembersIt(t *testing.T) {
	for _, tc := range []struct {
		name, flag, recorded, want, from string
	}{
		{name: "the flag beats the record", flag: "https://agents.other.example", recorded: "https://agents.example", want: "https://agents.other.example", from: "from --gateway-url"},
		{name: "no record takes the default", recorded: "", want: DefaultGatewayURL, from: "the default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newUpgradeRig(t, "0.5.0", "0.5.0")
			r.publishBoth(t, "0.5.0", true)
			record, err := ReadRecord(r.opts.CompanionHome)
			must(t, err)
			record.GatewayURL = tc.recorded
			must(t, WriteRecord(r.opts.CompanionHome, record))
			r.opts.GatewayURL = tc.flag
			var asked []string
			r.opts.Gateway = func(_ context.Context, url string) (GatewayRelease, error) {
				asked = append(asked, url)
				return GatewayRelease{Channel: "main"}, nil
			}
			if code := Upgrade(context.Background(), r.opts, r.out); code != ExitOK {
				t.Fatalf("exit %d:\n%s", code, r.out)
			}
			if !strings.Contains(r.out.String(), "agents gateway: "+tc.want+" ("+tc.from+")") {
				t.Fatalf("the gateway is not named with its source:\n%s", r.out)
			}
			if len(asked) != 1 || asked[0] != tc.want {
				t.Fatalf("asked %v, want %s", asked, tc.want)
			}
			after, err := ReadRecord(r.opts.CompanionHome)
			if err != nil || after.GatewayURL != tc.want {
				t.Fatalf("the record holds %q, want %s (%v)", after.GatewayURL, tc.want, err)
			}
			unit, err := os.ReadFile(filepath.Join(r.opts.UserHome, ".config", "systemd", "user", ServiceLabel+".service"))
			if err != nil || !strings.Contains(string(unit), "UFFICI_GATEWAY_URL="+tc.want) {
				t.Fatalf("the service does not name %s (%v):\n%s", tc.want, err, unit)
			}
		})
	}
}

// A release names its version before its assets exist: the message says which
// version was waited for.
func TestAReleaseWithoutAssetsYetNamesTheVersion(t *testing.T) {
	rs := newReleaseServer(t, "uffici-companion", "", map[string][]byte{})
	_, err := rs.release("uffici-companion", "0.10.0-dev.4").FetchVerified(context.Background(), "uffici_linux_amd64.tar.gz")
	if err == nil || !strings.Contains(err.Error(), "release v0.10.0-dev.4 has no assets yet") || !strings.Contains(err.Error(), "try again in a few minutes") {
		t.Fatalf("err = %v", err)
	}
	private := newReleaseServer(t, "uffici-companion", testToken, map[string][]byte{})
	_, err = private.release("uffici-companion", "0.10.0-dev.4").FetchVerified(context.Background(), ChecksumsFile)
	if err == nil || !strings.Contains(err.Error(), "release v0.10.0-dev.4 has no assets yet") {
		t.Fatalf("err = %v", err)
	}
}

// ── uffici setup ────────────────────────────────────────────────────────────

func TestRunNamesTheGatewayBeforePairingAndRecordsIt(t *testing.T) {
	r := newRig(t, goodManifest(), false)
	r.opts.GatewayURL, r.opts.GatewayFrom, r.opts.NoService = DefaultGatewayURL, "the default", true
	if code := Run(context.Background(), r.opts, r.out); code != ExitOK {
		t.Fatalf("exit %d:\n%s", code, r.out)
	}
	out := r.out.String()
	named := strings.Index(out, "agents gateway: https://agents.uffici.ai (the default)")
	paired := strings.Index(out, "step: pair")
	fetched := strings.Index(out, "fetching uffici-for-hermes.tar.gz")
	if named < 0 || paired < 0 || fetched < 0 || named > paired || named > fetched {
		t.Fatalf("the gateway must be named before pairing and the download:\n%s", out)
	}
	record, err := ReadRecord(r.opts.CompanionHome)
	if err != nil || record.GatewayURL != DefaultGatewayURL {
		t.Fatalf("the default was not recorded: %q, %v", record.GatewayURL, err)
	}
}

func TestRunPicksThePluginVersion(t *testing.T) {
	for _, tc := range []struct {
		name, pluginVersion, version, gateway string
		wantPath                              string
		wantAsked                             int
		wantLine                              string
	}{
		{name: "the plugin pin beats all", pluginVersion: "0.3.0", version: "0.4.0", gateway: "0.5.0", wantPath: "/releases/download/v0.3.0/"},
		{name: "the train's version beats the gateway", version: "0.4.0", gateway: "0.5.0", wantPath: "/releases/download/v0.4.0/"},
		{name: "the gateway beats the latest", gateway: "0.5.0", wantPath: "/releases/download/v0.5.0/", wantAsked: 1, wantLine: "version: 0.5.0, named by the agents gateway"},
		{name: "no version named is the latest", wantPath: "/releases/latest/download/", wantAsked: 1, wantLine: "version: the latest release"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The release at each tag carries that tag in its manifest, as the
			// plugin's release workflow stamps it — and setup holds it to that (#3118).
			manifest := goodManifest()
			manifest["version"] = NonEmpty(tc.pluginVersion, NonEmpty(tc.version, NonEmpty(tc.gateway, latestTag)))
			r := newRig(t, manifest, false)
			r.opts.NoService, r.opts.NoPair = true, true
			r.opts.PluginVersion, r.opts.Version = tc.pluginVersion, tc.version
			asked := 0
			r.opts.Gateway = gatewayNaming("develop", tc.gateway, &asked)
			if code := Run(context.Background(), r.opts, r.out); code != ExitOK {
				t.Fatalf("exit %d:\n%s", code, r.out)
			}
			if asked != tc.wantAsked {
				t.Fatalf("the gateway was asked %d times, want %d", asked, tc.wantAsked)
			}
			if !strings.Contains(strings.Join(r.rs.hits, " "), tc.wantPath) {
				t.Fatalf("no fetch under %s: %v", tc.wantPath, r.rs.hits)
			}
			if tc.wantLine != "" && !strings.Contains(r.out.String(), tc.wantLine) {
				t.Fatalf("no line %q:\n%s", tc.wantLine, r.out)
			}
		})
	}
}
