package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/protocol"
)

// DefaultGatewayURL is the production agents gateway (#2382): what a machine
// talks to when nothing else names one. The resolved gateway is always written
// into the service and the setup record, so a machine that took the default is
// self-describing afterwards.
const DefaultGatewayURL = "https://agents.uffici.ai"

// gatewayAskTimeout bounds the release question: the gateway answers from
// memory, and an answer that does not come falls back to the latest release.
const gatewayAskTimeout = 10 * time.Second

// maxReleaseAnswerBytes bounds the release answer, two short strings.
const maxReleaseAnswerBytes = 4 << 10

// ResolveGateway is the agents gateway a command uses and where it came from,
// in this order: the --gateway-url flag, UFFICI_GATEWAY_URL, the URL the setup
// record holds, DefaultGatewayURL (#2382). from is for the line that names it.
func ResolveGateway(flag string, env func(string) string, recorded string) (url, from string) {
	if value := strings.TrimSpace(flag); value != "" {
		return value, "from --gateway-url"
	}
	if env != nil {
		if value := strings.TrimSpace(env("UFFICI_GATEWAY_URL")); value != "" {
			return value, "from UFFICI_GATEWAY_URL"
		}
	}
	if value := strings.TrimSpace(recorded); value != "" {
		return value, "recorded in " + RecordFile
	}
	return DefaultGatewayURL, "the default"
}

// GatewayRelease is what an agents gateway names for its machines: the line it
// is deployed from, and the companion release — the same version every child of
// one train carries — or empty when it names none.
type GatewayRelease struct {
	Channel, Version string
}

// GatewayVersion asks the agents gateway which release its machines run
// (`GET <gateway>/v1/release`, #2382). Unauthenticated: 🔴 the GitHub token is
// never sent to it. A version that is not a release version is an error, so a
// caller falls back to the latest release rather than installing it.
func (r Release) GatewayVersion(ctx context.Context, gatewayURL string) (GatewayRelease, error) {
	origin := strings.TrimRight(strings.TrimSpace(gatewayURL), "/")
	if origin == "" {
		return GatewayRelease{}, errors.New("no agents gateway is set")
	}
	if !secureOrigin(origin) {
		return GatewayRelease{}, errors.New("the agents gateway is not an https origin")
	}
	askCtx, cancel := context.WithTimeout(ctx, gatewayAskTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(askCtx, http.MethodGet, origin+"/v1/release", nil)
	if err != nil {
		return GatewayRelease{}, errors.New("the agents gateway's address is not a URL")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := r.client().Do(req)
	if err != nil {
		return GatewayRelease{}, fmt.Errorf("could not be reached (%s)", classify(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return GatewayRelease{}, fmt.Errorf("answered %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxReleaseAnswerBytes+1))
	if err != nil || len(raw) > maxReleaseAnswerBytes {
		return GatewayRelease{}, errors.New("sent an unreadable answer")
	}
	var answer protocol.ReleaseResponse
	if err := json.Unmarshal(raw, &answer); err != nil || answer.Channel == "" {
		return GatewayRelease{}, errors.New("sent an answer that is not a release")
	}
	out := GatewayRelease{Channel: answer.Channel}
	if answer.Version != nil {
		if _, ok := parseVersion(*answer.Version); !ok {
			return GatewayRelease{Channel: answer.Channel}, fmt.Errorf("names %q, which is not a release version", *answer.Version)
		}
		out.Version = strings.TrimPrefix(*answer.Version, "v")
	}
	return out, nil
}

// releaseChoice is the version a run installs when nothing is pinned: the one
// the gateway names, or — empty — the latest release, with why.
type releaseChoice struct {
	gateway string
	release GatewayRelease
	err     error
}

// askGateway asks once per run; every component of the run reads the answer.
func askGateway(ctx context.Context, ask func(context.Context, string) (GatewayRelease, error), gatewayURL string) releaseChoice {
	release, err := ask(ctx, gatewayURL)
	return releaseChoice{gateway: gatewayURL, release: release, err: err}
}

// version is the gateway's version, or empty for the latest release.
func (c releaseChoice) version() string {
	if c.err != nil {
		return ""
	}
	return c.release.Version
}

// Line is the ONE line naming which path chose the version, and why.
func (c releaseChoice) Line() string {
	switch {
	case c.err != nil:
		return fmt.Sprintf("version: the latest release — the agents gateway %s %v", c.gateway, c.err)
	case c.release.Version == "":
		return fmt.Sprintf("version: the latest release — the agents gateway %s (channel %s) names no version", c.gateway, c.release.Channel)
	}
	return fmt.Sprintf("version: %s, named by the agents gateway %s (channel %s)", c.release.Version, c.gateway, c.release.Channel)
}

// DescribeGatewayRelease is the doctor's line for what the gateway names.
func DescribeGatewayRelease(release GatewayRelease, err error) string {
	switch {
	case err != nil:
		return fmt.Sprintf("the agents gateway %v — upgrades take the latest release", err)
	case release.Version == "":
		return fmt.Sprintf("channel %s, no version named — upgrades take the latest release", release.Channel)
	}
	return fmt.Sprintf("channel %s, version %s", release.Channel, release.Version)
}
