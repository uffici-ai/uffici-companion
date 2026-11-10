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
	// Verifier checks the release's Sigstore signature (#3118); FetchVerified
	// refuses without one.
	Verifier SignatureVerifier
	// DevIdentity is the development switch's pattern (DevReleaseIdentityEnv),
	// replacing the official signer; empty on every real machine.
	DevIdentity string
}

// version is the release's version without a leading `v` — THE one place it is
// normalised (#3118 review), so a caller passing `v1.2.3` and one passing
// `1.2.3` name the same release, URL and signer, and nothing builds
// `@refs/tags/vv…`.
func (r Release) version() string {
	return strings.TrimPrefix(r.Version, "v")
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
	if r.version() == "" {
		return fmt.Sprintf("%s/%s (latest release)", r.Owner, r.Repo)
	}
	return fmt.Sprintf("%s/%s v%s", r.Owner, r.Repo, r.version())
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
	if r.version() == "" {
		return fmt.Sprintf("%s/%s/%s/releases/latest/download/%s", r.base(), r.Owner, r.Repo, asset)
	}
	return fmt.Sprintf("%s/%s/%s/releases/download/v%s/%s", r.base(), r.Owner, r.Repo, r.version(), asset)
}

// assetAPIURL resolves an asset's API URL through the release listing.
func (r Release) assetAPIURL(ctx context.Context, asset string) (string, error) {
	release := "latest"
	if r.version() != "" {
		release = "tags/v" + r.version()
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
	if r.version() != "" {
		return "", notFound(fmt.Sprintf("%s has no asset named %s: %s", r.Describe(), asset, noAssetsYet(r.version())))
	}
	return "", notFound(fmt.Sprintf("%s has no asset named %s", r.Describe(), asset))
}

// errNoSuchAsset is what a missing release or asset is, under its sentence.
var errNoSuchAsset = errors.New("no such release or asset")

// notFoundError is a refusal whose sentence a person reads and whose kind
// code tests with errors.Is(err, errNoSuchAsset).
type notFoundError string

func notFound(sentence string) error { return notFoundError(sentence) }

func (e notFoundError) Error() string { return string(e) }

// Is makes a missing asset errNoSuchAsset.
func (e notFoundError) Is(target error) bool { return target == errNoSuchAsset }

// noAssetsYet is the sentence for a named release whose assets are missing.
func noAssetsYet(version string) string {
	return fmt.Sprintf("release v%s has no assets yet — a release's files are published a few minutes after it is tagged; try again in a few minutes", version)
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
		if r.version() != "" {
			// 🔴 A release's assets are built a few minutes AFTER its tag (#2382),
			// and a gateway names a version the moment it deploys: a machine
			// upgrading in that window gets here. Say which version it waited for.
			return nil, notFound(fmt.Sprintf("%s: %s", r.Describe(), noAssetsYet(r.version())))
		}
		return nil, notFound(fmt.Sprintf("%s has no such release or asset (404) — is there such a release yet? A release's files appear a few minutes after it is tagged", r.Describe()))
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
	// 🔴 No user name either (#3118 review): `…@host` reads as another host to
	// anyone checking the origin by eye, and the install line refuses it too.
	if err != nil || parsed.Host == "" || parsed.User != nil {
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
// checksum list, AFTER checking the release's signature (#3118). A missing
// entry is a refusal: an asset the release did not vouch for is not installed.
//
// 🔴 The signature comes first: the checksum list is trusted only once its own
// bundle (`checksums.txt.sigstore.json`) proves Uffici's release workflow
// signed exactly those bytes, at this release's tag. A release that signs its
// assets one by one instead (a plugin's, today) is held to the asset's own
// bundle, by the same signer, before the asset is returned. A release with
// neither is refused — never a warning, never installed.
func (r Release) FetchVerified(ctx context.Context, asset string) ([]byte, error) {
	body, _, err := r.FetchSigned(ctx, asset)
	return body, err
}

// FetchSigned is FetchVerified that also returns the release version the
// signature was made at — the version of a release fetched by the latest path,
// which whatever the asset claims about itself must then equal.
func (r Release) FetchSigned(ctx context.Context, asset string) ([]byte, string, error) {
	want, err := r.signer()
	if err != nil {
		return nil, "", err
	}
	sums, err := r.Fetch(ctx, ChecksumsFile)
	if err != nil {
		return nil, "", err
	}
	sumsBundle, err := r.Fetch(ctx, ChecksumsFile+BundleSuffix)
	signedList := err == nil
	if err != nil && !errors.Is(err, errNoSuchAsset) {
		return nil, "", err
	}
	version := ""
	if signedList {
		if version, err = r.verifySignature(ctx, ChecksumsFile, sumsBundle, sums, want); err != nil {
			return nil, "", err
		}
	}
	expected, err := ParseChecksums(sums)
	if err != nil {
		return nil, "", fmt.Errorf("%s of %s: %w", ChecksumsFile, r.Describe(), err)
	}
	wantSum, ok := expected[asset]
	if !ok {
		return nil, "", fmt.Errorf("%s of %s does not list %s — refusing an asset the release did not vouch for", ChecksumsFile, r.Describe(), asset)
	}
	body, err := r.Fetch(ctx, asset)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != wantSum {
		return nil, "", fmt.Errorf("%s from %s does not match its published checksum — refusing to install it", asset, r.Describe())
	}
	if !signedList {
		assetBundle, err := r.Fetch(ctx, asset+BundleSuffix)
		if errors.Is(err, errNoSuchAsset) {
			return nil, "", fmt.Errorf("%s carries no signature — neither %s nor %s — refusing an unsigned release", r.Describe(), ChecksumsFile+BundleSuffix, asset+BundleSuffix)
		}
		if err != nil {
			return nil, "", err
		}
		if version, err = r.verifySignature(ctx, asset, assetBundle, body, want); err != nil {
			return nil, "", err
		}
	}
	return body, version, nil
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
