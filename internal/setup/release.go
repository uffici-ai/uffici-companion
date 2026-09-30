package setup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// MaxAssetBytes bounds every download: a plugin tarball is kilobytes, the
// companion's own archive a few megabytes, and a server that answers with more
// is not the one we asked.
const MaxAssetBytes = 64 << 20

// ChecksumsFile is the checksum list every release publishes beside its assets:
// `<sha256 hex>  <asset name>` per line, the format `sha256sum -c` reads.
const ChecksumsFile = "checksums.txt"

// Release names one repository's release to fetch assets from.
//
// Without a token the public download URL is used, which redirects to the
// asset. With one — needed while a repository is private — the release is
// looked up through the API and the asset fetched by its API URL, because the
// public URL answers a private repository with a login page.
type Release struct {
	Owner, Repo string
	// Version pins a release (`0.1.1`); empty means the latest.
	Version string
	// Token is a GitHub token for a private repository; empty for a public one.
	// 🔴 Never logged, never printed.
	Token string
	// BaseURL is the web origin (`https://github.com`); APIURL the API origin
	// (`https://api.github.com`). Both overridable so a test serves a release
	// from loopback.
	BaseURL, APIURL string
	Client          *http.Client
}

func (r Release) base() string {
	if r.BaseURL != "" {
		return strings.TrimRight(r.BaseURL, "/")
	}
	return "https://github.com"
}

func (r Release) api() string {
	if r.APIURL != "" {
		return strings.TrimRight(r.APIURL, "/")
	}
	return "https://api.github.com"
}

func (r Release) client() *http.Client {
	if r.Client != nil {
		return r.Client
	}
	return http.DefaultClient
}

// Describe names the release for a log line or a plan: no token, no URL
// beyond the repository.
func (r Release) Describe() string {
	if r.Version == "" {
		return fmt.Sprintf("%s/%s (latest release)", r.Owner, r.Repo)
	}
	return fmt.Sprintf("%s/%s v%s", r.Owner, r.Repo, r.Version)
}

// LatestVersion is the version of the repository's latest release, without its
// `v`, read from the API's release listing (which answers a public repository
// without a token, and a private one with it).
func (r Release) LatestVersion(ctx context.Context) (string, error) {
	if !secureOrigin(r.api()) {
		return "", fmt.Errorf("%s is not https — a token or a download over plain http is refused (loopback excepted, for tests)", r.api())
	}
	raw, err := r.get(ctx, fmt.Sprintf("%s/repos/%s/%s/releases/latest", r.api(), r.Owner, r.Repo), "application/vnd.github+json")
	if err != nil {
		return "", err
	}
	var listing struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(raw, &listing); err != nil || !strings.HasPrefix(listing.TagName, "v") {
		return "", fmt.Errorf("the latest release of %s/%s names no version tag", r.Owner, r.Repo)
	}
	version := strings.TrimPrefix(listing.TagName, "v")
	// A tag that is not a release version would compare as 0.0.0 and read as
	// "a downgrade" or "current" — never trusted.
	if _, ok := parseVersion(version); !ok {
		return "", fmt.Errorf("the latest release of %s/%s is tagged %q, which is not a release version", r.Owner, r.Repo, listing.TagName)
	}
	return version, nil
}

// Fetch downloads one asset by name.
func (r Release) Fetch(ctx context.Context, asset string) ([]byte, error) {
	for _, origin := range []string{r.base(), r.api()} {
		if !secureOrigin(origin) {
			return nil, fmt.Errorf("%s is not https — a token or a download over plain http is refused (loopback excepted, for tests)", origin)
		}
	}
	if r.Token == "" {
		return r.get(ctx, r.publicURL(asset), "")
	}
	url, err := r.assetAPIURL(ctx, asset)
	if err != nil {
		return nil, err
	}
	return r.get(ctx, url, "application/octet-stream")
}

func (r Release) publicURL(asset string) string {
	if r.Version == "" {
		return fmt.Sprintf("%s/%s/%s/releases/latest/download/%s", r.base(), r.Owner, r.Repo, asset)
	}
	return fmt.Sprintf("%s/%s/%s/releases/download/v%s/%s", r.base(), r.Owner, r.Repo, r.Version, asset)
}

// assetAPIURL resolves an asset's API URL through the release listing.
func (r Release) assetAPIURL(ctx context.Context, asset string) (string, error) {
	release := "latest"
	if r.Version != "" {
		release = "tags/v" + r.Version
	}
	raw, err := r.get(ctx, fmt.Sprintf("%s/repos/%s/%s/releases/%s", r.api(), r.Owner, r.Repo, release), "application/vnd.github+json")
	if err != nil {
		return "", err
	}
	var listing struct {
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(raw, &listing); err != nil {
		return "", fmt.Errorf("reading the release listing of %s: %w", r.Describe(), err)
	}
	for _, a := range listing.Assets {
		if a.Name == asset {
			// 🔴 The Bearer header goes wherever this URL points, so the listing
			// may only send it back to the API it came from.
			if !strings.HasPrefix(a.URL, r.api()+"/repos/") {
				return "", fmt.Errorf("the release listing of %s names an asset outside its API — refusing it", r.Describe())
			}
			return a.URL, nil
		}
	}
	return "", fmt.Errorf("%s has no asset named %s", r.Describe(), asset)
}

func (r Release) get(ctx context.Context, url, accept string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("building the request for %s: %w", r.Describe(), err)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if r.Token != "" {
		// Go drops Authorization on a redirect to another host, which is what a
		// pre-signed asset redirect expects.
		req.Header.Set("Authorization", "Bearer "+r.Token)
	}
	resp, err := r.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching from %s: the request failed (%s)", r.Describe(), classify(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%s has no such release or asset (404) — is there a release yet, and is the repository public or a token set?", r.Describe())
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching from %s answered %d", r.Describe(), resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxAssetBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading from %s: %w", r.Describe(), err)
	}
	if len(body) > MaxAssetBytes {
		return nil, fmt.Errorf("an asset of %s is larger than %d bytes, which no release of ours is", r.Describe(), MaxAssetBytes)
	}
	return body, nil
}

// secureOrigin admits https anywhere and plain http on loopback only, where a
// test serves a release from the same machine.
func secureOrigin(origin string) bool {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" {
		return false
	}
	if parsed.Scheme == "https" {
		return true
	}
	// 🔴 The HOST, compared exactly: a prefix match admitted
	// `http://localhost.example` and `http://127.0.0.1.nip.io`, which would carry
	// the token in the clear to whoever answers them.
	switch parsed.Hostname() {
	case "127.0.0.1", "localhost", "::1":
		return parsed.Scheme == "http"
	}
	return false
}

// classify keeps a transport error's kind and drops its text: an error from
// the HTTP stack embeds the URL, and a URL is not something to print twice.
func classify(err error) string {
	var netErr interface{ Timeout() bool }
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	return "unreachable"
}

// FetchVerified downloads an asset and checks it against the release's
// checksum list. A missing entry is a refusal: an asset the release did not
// vouch for is not installed.
func (r Release) FetchVerified(ctx context.Context, asset string) ([]byte, error) {
	sums, err := r.Fetch(ctx, ChecksumsFile)
	if err != nil {
		return nil, err
	}
	expected, err := ParseChecksums(sums)
	if err != nil {
		return nil, fmt.Errorf("%s of %s: %w", ChecksumsFile, r.Describe(), err)
	}
	want, ok := expected[asset]
	if !ok {
		return nil, fmt.Errorf("%s of %s does not list %s — refusing an asset the release did not vouch for", ChecksumsFile, r.Describe(), asset)
	}
	body, err := r.Fetch(ctx, asset)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != want {
		return nil, fmt.Errorf("%s from %s does not match its published checksum — refusing to install it", asset, r.Describe())
	}
	return body, nil
}

// ParseChecksums reads `<hex>  <name>` lines. Blank lines are skipped;
// anything else is a refusal.
func ParseChecksums(raw []byte) (map[string]string, error) {
	out := map[string]string{}
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[0]) != 64 {
			return nil, fmt.Errorf("line %d is not `<sha256> <name>`", i+1)
		}
		if _, err := hex.DecodeString(fields[0]); err != nil {
			return nil, fmt.Errorf("line %d is not a hex checksum", i+1)
		}
		out[strings.TrimPrefix(fields[1], "*")] = strings.ToLower(fields[0])
	}
	if len(out) == 0 {
		return nil, errors.New("lists nothing")
	}
	return out, nil
}
