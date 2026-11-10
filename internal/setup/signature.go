package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// A release's signature (#3118). Every release of the companion and of each
// plugin is signed keyless by its own release workflow, on GitHub Actions, at
// the tag it publishes; the signature is a Sigstore bundle beside the file it
// signs (`<file>.sigstore.json`). The checksum list proves an asset is the one
// the release lists; the signature proves the release is Uffici's — a token
// that can write a release cannot mint a certificate for Uffici's workflow.

const (
	// BundleSuffix names a file's Sigstore bundle: `checksums.txt.sigstore.json`.
	BundleSuffix = ".sigstore.json"
	// ReleaseOwner is the GitHub organization whose release workflows sign.
	// 🔴 Pinned, never the redirected repository: a release served from
	// anywhere must still be signed by Uffici's workflow.
	ReleaseOwner = "uffici-ai"
	// ReleaseIssuer is the OIDC issuer every release certificate must name:
	// GitHub Actions.
	ReleaseIssuer = "https://token.actions.githubusercontent.com"
	// DevReleaseIdentityEnv is the development switch: a regular expression
	// that REPLACES the official certificate identity, for a fork's or a
	// rehearsal's release. The issuer stays GitHub Actions and the signature is
	// still verified; never set on a real machine.
	DevReleaseIdentityEnv = "UFFICI_DEV_RELEASE_IDENTITY"
)

// ReleaseIdentity is the exact certificate identity of repo's release
// workflow at tag v<version>: what `cosign verify-blob --certificate-identity`
// is given. version is bare (`1.2.3`); a Release hands it Release.version().
func ReleaseIdentity(repo, version string) string {
	return fmt.Sprintf("https://github.com/%s/%s/.github/workflows/release.yml@refs/tags/v%s", ReleaseOwner, repo, version)
}

// releaseIdentityPattern matches repo's release workflow at any FINAL release
// tag — the latest path, where no version was named before the download, and
// `releases/latest` never names a prerelease. The tag it matched is the
// release's version from then on (signedTag): the latest path learns its
// version from the signature, never from the listing alone.
func releaseIdentityPattern(repo string) string {
	return "^" + regexp.QuoteMeta(fmt.Sprintf("https://github.com/%s/%s/.github/workflows/release.yml@refs/tags/v", ReleaseOwner, repo)) +
		`[0-9]+\.[0-9]+\.[0-9]+$`
}

// tagOfIdentity reads the release version out of a verified certificate
// identity: what follows `@refs/tags/v`, when it is a release version.
var tagOfIdentity = regexp.MustCompile(`@refs/tags/v([0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?)$`)

// signedTag is the release version a verified identity was signed at.
func signedTag(identity string) (string, bool) {
	match := tagOfIdentity.FindStringSubmatch(identity)
	if match == nil {
		return "", false
	}
	return match[1], true
}

// anchored makes a development pattern match a WHOLE identity: a pattern
// handed to sigstore-go or cosign as it is matches any identity CONTAINING it.
func anchored(pattern string) string {
	return "^(?:" + pattern + ")$"
}

// Signer is who must have signed: an exact certificate identity, or a pattern
// when the version was not known (the latest path) or the development switch
// replaced it; the issuer is always exact.
type Signer struct {
	Identity, IdentityPattern, Issuer string
}

// Describe names the signer for a refusal: the identity, or its pattern.
func (s Signer) Describe() string {
	if s.Identity != "" {
		return s.Identity
	}
	return s.IdentityPattern
}

// SignatureVerifier checks a Sigstore bundle over an artifact's bytes and
// returns the certificate identity that signed it.
type SignatureVerifier interface {
	Verify(ctx context.Context, bundleJSON, artifact []byte, want Signer) (string, error)
}

// signer is who must have signed this release: the official identity for its
// repository at its version, or the development switch's pattern.
func (r Release) signer() (Signer, error) {
	if r.DevIdentity != "" {
		if _, err := regexp.Compile(r.DevIdentity); err != nil {
			return Signer{}, fmt.Errorf("%s is not a regular expression", DevReleaseIdentityEnv)
		}
		return Signer{IdentityPattern: anchored(r.DevIdentity), Issuer: ReleaseIssuer}, nil
	}
	if r.version() == "" {
		return Signer{IdentityPattern: releaseIdentityPattern(r.Repo), Issuer: ReleaseIssuer}, nil
	}
	return Signer{Identity: ReleaseIdentity(r.Repo, r.version()), Issuer: ReleaseIssuer}, nil
}

// DevIdentityWarning is the line every run prints when the development switch
// is set: loud, because it means a release signed by someone else is accepted.
func DevIdentityWarning(pattern string) string {
	return fmt.Sprintf("WARNING: %s is set — releases signed by %q are accepted instead of Uffici's release workflows. This is a development switch; unset it on any real machine.", DevReleaseIdentityEnv, pattern)
}

// sigstoreVerifier verifies with sigstore-go against a trusted root.
type sigstoreVerifier struct {
	material func(ctx context.Context) (root.TrustedMaterial, error)
	// requireSCT asks for the certificate's transparency-log timestamp, as
	// `cosign verify-blob` does. Only a test's virtual Sigstore, which issues
	// certificates without one, turns it off.
	requireSCT bool
}

// PublicGoodVerifier verifies against Sigstore's public-good instance, whose
// trusted root (Fulcio's certificate authorities, Rekor's and the CT log's
// keys, the timestamp authority) is fetched through TUF from
// tuf-repo-cdn.sigstore.dev, starting from the root embedded in sigstore-go,
// and cached under cacheDir (sigstore-go's default when empty). Fetched once
// per process, on the first verification; a root that cannot be fetched or
// validated is a refusal, never a skipped check.
func PublicGoodVerifier(cacheDir string) SignatureVerifier {
	return sigstoreVerifier{requireSCT: true, material: cachedMaterial(func(ctx context.Context) (root.TrustedMaterial, error) {
		opts := tuf.DefaultOptions().WithContext(ctx)
		if cacheDir != "" {
			if err := os.MkdirAll(cacheDir, 0o700); err != nil {
				return nil, fmt.Errorf("creating Sigstore's cache: %w", err)
			}
			opts = opts.WithCachePath(cacheDir)
		}
		return root.FetchTrustedRootWithOptions(opts)
	})}
}

// cachedMaterial fetches the trusted root once per process and keeps the
// answer — except a fetch its CALLER's context ended (a Ctrl-C, a deadline):
// that says nothing about the root, so the next verification fetches again
// rather than inherit one caller's cancellation (#3118 review).
func cachedMaterial(fetch func(ctx context.Context) (root.TrustedMaterial, error)) func(ctx context.Context) (root.TrustedMaterial, error) {
	var (
		mu       sync.Mutex
		done     bool
		material root.TrustedMaterial
		fetchErr error
	)
	return func(ctx context.Context) (root.TrustedMaterial, error) {
		mu.Lock()
		defer mu.Unlock()
		if !done {
			material, fetchErr = fetch(ctx)
			cancelled := ctx.Err() != nil || errors.Is(fetchErr, context.Canceled) || errors.Is(fetchErr, context.DeadlineExceeded)
			done = fetchErr == nil || !cancelled
		}
		return material, fetchErr
	}
}

// Verify implements SignatureVerifier: the bundle must parse, its certificate
// chain to the trusted Fulcio, its entry be in the trusted Rekor log with a
// trusted timestamp, its signature cover exactly artifact, and its certificate
// name want.
func (v sigstoreVerifier) Verify(ctx context.Context, bundleJSON, artifact []byte, want Signer) (string, error) {
	var b bundle.Bundle
	if err := b.UnmarshalJSON(bundleJSON); err != nil {
		return "", fmt.Errorf("it is not a Sigstore bundle (%s)", firstLine(err))
	}
	material, err := v.material(ctx)
	if err != nil {
		return "", fmt.Errorf("the trusted root of Sigstore could not be read (%s)", firstLine(err))
	}
	options := []verify.VerifierOption{verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1)}
	if v.requireSCT {
		options = append(options, verify.WithSignedCertificateTimestamps(1))
	}
	verifier, err := verify.NewVerifier(material, options...)
	if err != nil {
		return "", fmt.Errorf("the verifier could not be built (%s)", firstLine(err))
	}
	identity, err := verify.NewShortCertificateIdentity(want.Issuer, "", want.Identity, want.IdentityPattern)
	if err != nil {
		return "", fmt.Errorf("the expected identity is malformed (%s)", firstLine(err))
	}
	result, err := verifier.Verify(&b, verify.NewPolicy(verify.WithArtifact(bytes.NewReader(artifact)), verify.WithCertificateIdentity(identity)))
	if err != nil {
		return "", errors.New(firstLine(err))
	}
	if result.Signature == nil || result.Signature.Certificate == nil {
		return "", errors.New("the bundle carries no certificate")
	}
	return result.Signature.Certificate.SubjectAlternativeName, nil
}

// firstLine keeps an error to one line, for a refusal a person reads.
func firstLine(err error) string {
	text, _, _ := strings.Cut(err.Error(), "\n")
	return text
}

// verifySignature checks one file's bundle, names the refusal for a person,
// and returns the release version the signature was made at.
//
// 🔴 The version comes from the VERIFIED identity, and must be this release's
// own when the release names one: a release can never carry another tag's
// signature, and the latest path cannot be pointed at an older release
// without the version it reports being that older one's.
func (r Release) verifySignature(ctx context.Context, file string, bundleJSON, artifact []byte, want Signer) (string, error) {
	if r.Verifier == nil {
		return "", fmt.Errorf("no signature verifier is configured for %s — refusing an unverified download", r.Describe())
	}
	identity, err := r.Verifier.Verify(ctx, bundleJSON, artifact, want)
	if err != nil {
		return "", fmt.Errorf("the signature on %s of %s was not made by %s — refusing it: %v", file, r.Describe(), want.Describe(), err)
	}
	tag, ok := signedTag(identity)
	if !ok {
		return "", fmt.Errorf("the signature on %s of %s was made at no release tag (%s) — refusing it", file, r.Describe(), identity)
	}
	if r.version() != "" && tag != r.version() {
		return "", fmt.Errorf("the signature on %s of %s was made at v%s, not at this release's tag — refusing it", file, r.Describe(), tag)
	}
	return tag, nil
}
