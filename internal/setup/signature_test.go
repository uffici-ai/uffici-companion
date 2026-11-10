package setup

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/testing/ca"

	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	protocommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	protorekor "github.com/sigstore/protobuf-specs/gen/pb-go/rekor/v1"
)

// A release's signature, checked offline (#3118): sigstore-go's virtual
// Sigstore stands in for the public-good instance (its own Fulcio, Rekor and
// timestamp authority), so a loopback release can be signed by Uffici's
// workflow identity — or by ANOTHER one — with no network.

var (
	virtualOnce sync.Once
	virtual     *ca.VirtualSigstore
	virtualErr  error
)

func virtualSigstore(t *testing.T) *ca.VirtualSigstore {
	t.Helper()
	virtualOnce.Do(func() { virtual, virtualErr = ca.NewVirtualSigstore() })
	if virtualErr != nil {
		t.Fatal(virtualErr)
	}
	return virtual
}

// testVerifier is the production verifier against the virtual Sigstore. The
// one difference: the virtual Fulcio issues no certificate-transparency
// timestamp, so that one check is off (the real bundle below has it ON).
func testVerifier(t *testing.T) SignatureVerifier {
	vs := virtualSigstore(t)
	return sigstoreVerifier{material: func(context.Context) (root.TrustedMaterial, error) { return vs, nil }}
}

// signBundle is a Sigstore bundle (v0.3, the shape cosign v3 writes) over
// artifact, signed by identity with GitHub Actions as the issuer.
func signBundle(t *testing.T, identity string, artifact []byte) []byte {
	t.Helper()
	return signBundleBy(t, identity, ReleaseIssuer, artifact)
}

func signBundleBy(t *testing.T, identity, issuer string, artifact []byte) []byte {
	t.Helper()
	vs := virtualSigstore(t)
	entity, err := vs.Sign(identity, issuer, artifact)
	if err != nil {
		t.Fatal(err)
	}
	content, _ := entity.VerificationContent()
	signature, _ := entity.SignatureContent()
	message := signature.MessageSignatureContent()
	timestamps, _ := entity.Timestamps()
	entries, _ := entity.TlogEntries()
	tle := entries[0].TransparencyLogEntry()
	proof, err := vs.GetInclusionProof(tle.CanonicalizedBody)
	if err != nil {
		t.Fatal(err)
	}
	rootHash, _ := hex.DecodeString(*proof.RootHash)
	tle.KindVersion = &protorekor.KindVersion{Kind: "hashedrekord", Version: "0.0.1"}
	tle.InclusionProof = &protorekor.InclusionProof{
		LogIndex: *proof.LogIndex, RootHash: rootHash, TreeSize: *proof.TreeSize,
		Checkpoint: &protorekor.Checkpoint{Envelope: *proof.Checkpoint},
	}
	var rfc3161 []*protocommon.RFC3161SignedTimestamp
	for _, ts := range timestamps {
		rfc3161 = append(rfc3161, &protocommon.RFC3161SignedTimestamp{SignedTimestamp: ts})
	}
	pb := &protobundle.Bundle{
		MediaType: "application/vnd.dev.sigstore.bundle.v0.3+json",
		VerificationMaterial: &protobundle.VerificationMaterial{
			Content:                   &protobundle.VerificationMaterial_Certificate{Certificate: &protocommon.X509Certificate{RawBytes: content.Certificate().Raw}},
			TlogEntries:               []*protorekor.TransparencyLogEntry{tle},
			TimestampVerificationData: &protobundle.TimestampVerificationData{Rfc3161Timestamps: rfc3161},
		},
		Content: &protobundle.Bundle_MessageSignature{MessageSignature: &protocommon.MessageSignature{
			MessageDigest: &protocommon.HashOutput{Algorithm: protocommon.HashAlgorithm_SHA2_256, Digest: message.Digest()},
			Signature:     message.Signature(),
		}},
	}
	b, err := bundle.NewBundle(pb)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := b.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestReleaseIdentityIsTheWorkflowAtTheTag(t *testing.T) {
	if got := ReleaseIdentity(CompanionRepo, "1.2.3"); got != "https://github.com/uffici-ai/uffici-companion/.github/workflows/release.yml@refs/tags/v1.2.3" {
		t.Fatal(got)
	}
	r := Release{Owner: "someone-else", Repo: CompanionRepo, Version: "1.2.3"}
	if s, err := r.signer(); err != nil || s.Identity != ReleaseIdentity(CompanionRepo, "1.2.3") || s.Issuer != ReleaseIssuer {
		// 🔴 The owner a release is SERVED from never moves the identity.
		t.Fatalf("a pinned release's signer: %+v %v", s, err)
	}
	r.Version = ""
	s, err := r.signer()
	if err != nil || s.Identity != "" || s.Describe() != s.IdentityPattern {
		t.Fatalf("the latest path's signer: %+v %v", s, err)
	}
	r.DevIdentity = "("
	if _, err := r.signer(); err == nil || !strings.Contains(err.Error(), DevReleaseIdentityEnv) {
		t.Fatalf("a malformed development pattern was accepted: %v", err)
	}
	if !strings.Contains(DevIdentityWarning("^x$"), "WARNING") || !strings.Contains(DevIdentityWarning("^x$"), DevReleaseIdentityEnv) {
		t.Fatal("the development switch's warning does not shout")
	}
}

// The verifier itself, over the virtual Sigstore: Uffici's identity at the tag
// passes, and everything else is refused.
func TestSigstoreVerifierPinsTheIdentity(t *testing.T) {
	ctx := context.Background()
	v := testVerifier(t)
	artifact := []byte("checksums")
	official := ReleaseIdentity(CompanionRepo, "1.2.3")
	pinned := Signer{Identity: official, Issuer: ReleaseIssuer}
	signed := signBundle(t, official, artifact)
	if who, err := v.Verify(ctx, signed, artifact, pinned); err != nil || who != official {
		t.Fatalf("Uffici's own signature was refused: %q %v", who, err)
	}
	latest := Signer{IdentityPattern: releaseIdentityPattern(CompanionRepo), Issuer: ReleaseIssuer}
	if _, err := v.Verify(ctx, signed, artifact, latest); err != nil {
		t.Fatalf("the latest path refused a release tag: %v", err)
	}
	for name, tc := range map[string]struct {
		bundle   []byte
		artifact []byte
		want     Signer
	}{
		"another repository's workflow": {signBundle(t, ReleaseIdentity("uffici-for-hermes", "1.2.3"), artifact), artifact, pinned},
		"a fork's workflow":             {signBundle(t, "https://github.com/someone/uffici-companion/.github/workflows/release.yml@refs/tags/v1.2.3", artifact), artifact, latest},
		"another tag":                   {signBundle(t, ReleaseIdentity(CompanionRepo, "1.2.2"), artifact), artifact, pinned},
		"a branch, not a tag":           {signBundle(t, "https://github.com/uffici-ai/uffici-companion/.github/workflows/release.yml@refs/heads/main", artifact), artifact, latest},
		"a person, not a workflow":      {signBundleBy(t, "someone@example.com", "https://accounts.google.com", artifact), artifact, latest},
		"another issuer":                {signBundleBy(t, official, "https://issuer.example", artifact), artifact, pinned},
		"other bytes":                   {signed, []byte("checksums, edited"), pinned},
		// 🔴 Whole identities only: nothing after the tag, nothing before the URL.
		"a path after the tag":            {signBundle(t, official+"/x", artifact), artifact, pinned},
		"a suffix on the tag":             {signBundle(t, official+"-evil", artifact), artifact, pinned},
		"a path after the tag, latest":    {signBundle(t, official+"/x", artifact), artifact, latest},
		"a prerelease suffix, latest":     {signBundle(t, official+"-evil", artifact), artifact, latest},
		"a prefix before the URL":         {signBundle(t, "x"+official, artifact), artifact, pinned},
		"a prefix before the URL, latest": {signBundle(t, "evil/"+official, artifact), artifact, latest},
		"not a bundle":                    {[]byte("{}"), artifact, pinned},
	} {
		if _, err := v.Verify(ctx, tc.bundle, tc.artifact, tc.want); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	broken := sigstoreVerifier{material: func(context.Context) (root.TrustedMaterial, error) { return nil, errors.New("no root\nsecond line") }}
	if _, err := broken.Verify(ctx, signed, artifact, pinned); err == nil || !strings.Contains(err.Error(), "trusted root") || strings.Contains(err.Error(), "second line") {
		t.Fatalf("an unreadable trusted root was not a one-line refusal: %v", err)
	}
	if _, err := v.Verify(ctx, signed, artifact, Signer{IdentityPattern: "(", Issuer: ReleaseIssuer}); err == nil {
		t.Fatal("a malformed identity was accepted")
	}
}

// The real thing, offline: v0.22.1's checksum list and its bundle as the
// release published them, against a snapshot of the public-good trusted root —
// the production policy, certificate-transparency timestamp included.
func TestARealReleaseVerifiesAgainstThePublicGoodRoot(t *testing.T) {
	read := func(name string) []byte {
		raw, err := os.ReadFile(filepath.Join("testdata", "sigstore", name))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	trusted, err := root.NewTrustedRootFromJSON(read("trusted_root.json"))
	if err != nil {
		t.Fatal(err)
	}
	v := sigstoreVerifier{requireSCT: true, material: func(context.Context) (root.TrustedMaterial, error) { return trusted, nil }}
	sums, signed := read("checksums.txt"), read("checksums.txt.sigstore.json")
	ctx := context.Background()
	official := ReleaseIdentity(CompanionRepo, "0.22.1")
	if who, err := v.Verify(ctx, signed, sums, Signer{Identity: official, Issuer: ReleaseIssuer}); err != nil || who != official {
		t.Fatalf("the published v0.22.1 was refused: %q %v", who, err)
	}
	if _, err := v.Verify(ctx, signed, sums, Signer{IdentityPattern: releaseIdentityPattern(CompanionRepo), Issuer: ReleaseIssuer}); err != nil {
		t.Fatalf("the latest path refused the published v0.22.1: %v", err)
	}
	for name, want := range map[string]Signer{
		"another version": {Identity: ReleaseIdentity(CompanionRepo, "0.22.0"), Issuer: ReleaseIssuer},
		"the plugin":      {Identity: ReleaseIdentity("uffici-for-hermes", "0.22.1"), Issuer: ReleaseIssuer},
	} {
		if _, err := v.Verify(ctx, signed, sums, want); err == nil {
			t.Fatalf("%s: the published bundle passed for the wrong signer", name)
		}
	}
	tampered := bytes.Replace(sums, []byte("5ccb"), []byte("5ccc"), 1)
	if _, err := v.Verify(ctx, signed, tampered, Signer{Identity: official, Issuer: ReleaseIssuer}); err == nil {
		t.Fatal("an edited checksum list passed the published signature")
	}
}

func TestPublicGoodVerifierRefusesWithoutARoot(t *testing.T) {
	// A cache directory that cannot be created: the root is never fetched, and
	// the verification is a refusal, never a pass.
	file := filepath.Join(t.TempDir(), "file")
	must(t, os.WriteFile(file, nil, 0o600))
	v := PublicGoodVerifier(filepath.Join(file, "sigstore"))
	if _, err := v.Verify(context.Background(), signBundle(t, "x", []byte("a")), []byte("a"), Signer{Identity: "x", Issuer: ReleaseIssuer}); err == nil || !strings.Contains(err.Error(), "trusted root") {
		t.Fatalf("no root, and the verification passed: %v", err)
	}
}

// 🔴 The guard of #3118: a loopback release whose checksum list was signed by
// ANOTHER identity — or not at all, or edited after signing — is refused before
// any checksum in it is trusted, and nothing is returned to install.
func TestFetchVerifiedRefusesAReleaseNotSignedByUffici(t *testing.T) {
	ctx := context.Background()
	body := []byte("the plugin")
	const asset = "uffici-for-hermes.tar.gz"
	sums := []byte(sha(body) + "  " + asset + "\n")
	serve := func(t *testing.T, token string) *releaseServer {
		return newReleaseServer(t, "uffici-for-hermes", token, map[string][]byte{asset: body, ChecksumsFile: sums})
	}
	for _, token := range []string{"", "ghp_test_token_not_a_secret"} {
		for _, version := range []string{"", "0.3.0"} {
			// Non-vacuity: the same release, signed by its own workflow, is accepted.
			rs := serve(t, token)
			if got, err := rs.release("uffici-for-hermes", version).FetchVerified(ctx, asset); err != nil || !bytes.Equal(got, body) {
				t.Fatalf("token=%v version=%q: Uffici's own release was refused: %v", token != "", version, err)
			}
			for name, identity := range map[string]string{
				"another repository's workflow": ReleaseIdentity("uffici-companion", NonEmpty(version, latestTag)),
				"a fork":                        "https://github.com/someone/uffici-for-hermes/.github/workflows/release.yml@refs/tags/v" + NonEmpty(version, latestTag),
				"a branch":                      "https://github.com/uffici-ai/uffici-for-hermes/.github/workflows/release.yml@refs/heads/main",
			} {
				rs := serve(t, token)
				rs.identity = func(string) string { return identity }
				got, err := rs.release("uffici-for-hermes", version).FetchVerified(ctx, asset)
				if err == nil || got != nil || !strings.Contains(err.Error(), "was not made by") {
					t.Fatalf("token=%v version=%q %s: a release re-signed by another identity was accepted: %v", token != "", version, name, err)
				}
			}
		}
	}
	// A pinned release signed at ANOTHER tag of the right workflow: a replayed
	// old release under a new version's name.
	rs := serve(t, "")
	rs.identity = func(string) string { return ReleaseIdentity("uffici-for-hermes", "0.2.9") }
	if _, err := rs.release("uffici-for-hermes", "0.3.0").FetchVerified(ctx, asset); err == nil {
		t.Fatal("a release signed at another tag was accepted")
	}
	// Signed, then the list edited to vouch for another asset.
	rs = serve(t, "")
	rs.assets[ChecksumsFile+BundleSuffix] = signBundle(t, ReleaseIdentity("uffici-for-hermes", latestTag), []byte(strings.Repeat("00", 32)+"  "+asset+"\n"))
	if _, err := rs.release("uffici-for-hermes", "").FetchVerified(ctx, asset); err == nil || !strings.Contains(err.Error(), "was not made by") {
		t.Fatalf("a list edited after signing was trusted: %v", err)
	}
	// No signature of any kind.
	rs = serve(t, "")
	rs.unsigned = true
	if _, err := rs.release("uffici-for-hermes", "").FetchVerified(ctx, asset); err == nil || !strings.Contains(err.Error(), "carries no signature") {
		t.Fatalf("an unsigned release was accepted: %v", err)
	}
	// No verifier wired: a refusal, never a skipped check.
	r := serve(t, "").release("uffici-for-hermes", "")
	r.Verifier = nil
	if _, err := r.FetchVerified(ctx, asset); err == nil || !strings.Contains(err.Error(), "no signature verifier") {
		t.Fatalf("a release was fetched with no verifier: %v", err)
	}
}

// A release that signs its asset rather than its list (the plugin's, today) is
// held to the asset's own bundle, by the same signer.
func TestFetchVerifiedHoldsAnAssetSignedOnItsOwn(t *testing.T) {
	ctx := context.Background()
	body := []byte("the plugin")
	const asset = "uffici-for-hermes.tar.gz"
	sums := []byte(sha(body) + "  " + asset + "\n")
	for name, tc := range map[string]struct {
		identity string
		ok       bool
	}{
		"its own workflow": {ReleaseIdentity("uffici-for-hermes", "0.3.0"), true},
		"another identity": {ReleaseIdentity("uffici-companion", "0.3.0"), false},
	} {
		rs := newReleaseServer(t, "uffici-for-hermes", "", map[string][]byte{
			asset: body, ChecksumsFile: sums, asset + BundleSuffix: signBundle(t, tc.identity, body),
		})
		rs.unsigned = true
		got, err := rs.release("uffici-for-hermes", "0.3.0").FetchVerified(ctx, asset)
		if tc.ok != (err == nil) || tc.ok != bytes.Equal(got, body) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// A list-bundle fetch that fails for another reason than "not there" stops
	// the run rather than falling back.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, BundleSuffix) {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		_, _ = w.Write(sums)
	}))
	t.Cleanup(srv.Close)
	r := Release{Owner: "uffici-ai", Repo: "uffici-for-hermes", BaseURL: srv.URL, APIURL: srv.URL, Client: srv.Client(), Verifier: testVerifier(t)}
	if _, err := r.FetchVerified(ctx, asset); err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("a failed signature fetch was not reported: %v", err)
	}
	// An asset bundle that cannot be fetched for another reason stops it too.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ChecksumsFile+BundleSuffix):
			http.NotFound(w, r)
		case strings.HasSuffix(r.URL.Path, BundleSuffix):
			http.Error(w, "down", http.StatusBadGateway)
		case strings.HasSuffix(r.URL.Path, ChecksumsFile):
			_, _ = w.Write(sums)
		default:
			_, _ = w.Write(body)
		}
	}))
	t.Cleanup(srv2.Close)
	r.BaseURL, r.APIURL, r.Client = srv2.URL, srv2.URL, srv2.Client()
	if _, err := r.FetchVerified(ctx, asset); err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("a failed asset-signature fetch was not reported: %v", err)
	}
}

// The development switch replaces the identity — a fork's release passes, and
// Uffici's own then does NOT — and never turns verification off.
func TestTheDevelopmentSwitchReplacesTheIdentityAndStillVerifies(t *testing.T) {
	ctx := context.Background()
	body := []byte("the plugin")
	const asset = "uffici-for-hermes.tar.gz"
	fork := "https://github.com/someone/uffici-for-hermes/.github/workflows/release.yml@refs/tags/v0.1.0"
	rs := newReleaseServer(t, "uffici-for-hermes", "", map[string][]byte{asset: body, ChecksumsFile: []byte(sha(body) + "  " + asset + "\n")})
	rs.identity = func(string) string { return fork }
	r := rs.release("uffici-for-hermes", "")
	r.DevIdentity = "^" + regexp.QuoteMeta(fork) + "$"
	if _, err := r.FetchVerified(ctx, asset); err != nil {
		t.Fatalf("the switch did not admit the fork: %v", err)
	}
	rs.identity = func(v string) string { return ReleaseIdentity("uffici-for-hermes", v) }
	if _, err := r.FetchVerified(ctx, asset); err == nil {
		t.Fatal("the switch admitted an identity it does not name")
	}
	rs.unsigned = true
	if _, err := r.FetchVerified(ctx, asset); err == nil || !strings.Contains(err.Error(), "carries no signature") {
		t.Fatalf("the switch turned verification off: %v", err)
	}
	r.DevIdentity = "("
	if _, err := r.FetchVerified(ctx, asset); err == nil {
		t.Fatal("a malformed switch was accepted")
	}
}

// Setup installs no plugin from a release another identity signed.
func TestSetupRefusesAPluginReleaseSignedByAnotherIdentity(t *testing.T) {
	r := newRig(t, goodManifest(), false)
	r.rs.identity = func(v string) string { return ReleaseIdentity("uffici-companion", v) }
	if code := Run(context.Background(), r.opts, r.out); code != ExitFailed {
		t.Fatalf("exit %d:\n%s", code, r.out)
	}
	if !strings.Contains(r.out.String(), "was not made by") {
		t.Fatalf("the refusal does not name the signature:\n%s", r.out)
	}
	if _, err := os.Stat(filepath.Join(r.home, "plugins", "uffici-for-hermes")); err == nil {
		t.Fatal("the plugin was installed from a release Uffici did not sign")
	}
}

// Upgrade replaces no binary with one from a release another identity signed.
func TestUpgradeRefusesACompanionReleaseSignedByAnotherIdentity(t *testing.T) {
	r := newUpgradeRig(t, "0.4.0", "0.4.0")
	r.publishBoth(t, "0.5.0", true)
	assets := r.rs.assets[CompanionRepo]
	assets[ChecksumsFile+BundleSuffix] = signBundle(t, ReleaseIdentity("uffici-for-hermes", "0.5.0"), assets[ChecksumsFile])
	if code := Upgrade(context.Background(), r.opts, r.out); code != ExitFailed {
		t.Fatalf("exit %d:\n%s", code, r.out)
	}
	if !strings.Contains(r.out.String(), "was not made by "+ReleaseIdentity(CompanionRepo, "0.5.0")) {
		t.Fatalf("the refusal does not name the signer it expected:\n%s", r.out)
	}
	if r.binary(t) != "old binary" || len(r.execs) != 0 {
		t.Fatalf("a release signed by another identity replaced the binary (%q) or handed over", r.binary(t))
	}
}

func TestSignedTagReadsTheVersionOutOfAVerifiedIdentity(t *testing.T) {
	for identity, want := range map[string]string{
		ReleaseIdentity(CompanionRepo, "1.2.3"):                                                       "1.2.3",
		ReleaseIdentity(CompanionRepo, "1.2.3-rc.1"):                                                  "1.2.3-rc.1",
		"https://github.com/someone/x/.github/workflows/release.yml@refs/tags/v0.4.0":                 "0.4.0",
		"https://github.com/uffici-ai/uffici-companion/.github/workflows/release.yml@refs/heads/main": "",
		ReleaseIdentity(CompanionRepo, "1.2.3") + "/x":                                                "",
	} {
		got, ok := signedTag(identity)
		if got != want || ok != (want != "") {
			t.Fatalf("signedTag(%q) = %q, %v; want %q", identity, got, ok, want)
		}
	}
}

// 🔴 #3118 review: a pattern the development switch names matches a WHOLE
// identity — a substring of Uffici's own (or anyone's) does not.
func TestTheDevelopmentSwitchIsAnchored(t *testing.T) {
	r := Release{Repo: CompanionRepo, DevIdentity: `github\.com/someone/`}
	s, err := r.signer()
	if err != nil || s.IdentityPattern != `^(?:github\.com/someone/)$` {
		t.Fatalf("the switch's pattern is not anchored: %+v %v", s, err)
	}
	artifact := []byte("checksums")
	signed := signBundle(t, "https://github.com/someone/uffici-companion/.github/workflows/release.yml@refs/tags/v1.0.0", artifact)
	if _, err := testVerifier(t).Verify(context.Background(), signed, artifact, s); err == nil {
		t.Fatal("a substring of the identity passed the switch")
	}
}

// 🔴 #3118 review — the latest path pointed at an OLDER signed release: upgrade
// refuses it (the latest never downgrades), and the binary stays as it is.
func TestUpgradeRefusesALatestThatIsAnOlderSignedRelease(t *testing.T) {
	r := newUpgradeRig(t, "0.5.0", "0.5.0")
	// Someone who can write releases marks a genuine, signed, older one latest.
	r.publishBoth(t, "0.4.0", true)
	if code := Upgrade(context.Background(), r.opts, r.out); code == ExitOK {
		t.Fatalf("an older latest was installed:\n%s", r.out)
	}
	if !strings.Contains(r.out.String(), "never downgrades") {
		t.Fatalf("the refusal does not say why:\n%s", r.out)
	}
	if r.binary(t) != "old binary" || len(r.execs) != 0 || r.runner.ran("hermes") {
		t.Fatalf("an older latest touched the machine (%q)", r.binary(t))
	}
	// The same version as installed is a no-op, as it always was.
	r = newUpgradeRig(t, "0.5.0", "0.5.0")
	r.publishBoth(t, "0.5.0", true)
	if code := Upgrade(context.Background(), r.opts, r.out); code != ExitOK || !strings.Contains(r.out.String(), "nothing to do") || len(r.execs) != 0 {
		t.Fatalf("the installed version was not a no-op: %d\n%s", code, r.out)
	}
}

// 🔴 #3118 review — a release can never carry ANOTHER tag's signature: a
// pinned release signed at another tag is refused even when the signer pattern
// would admit both (the development switch), and a plugin whose manifest
// claims another version than its release was signed at is never installed.
func TestASignatureFromAnotherTagIsRefused(t *testing.T) {
	ctx := context.Background()
	body := []byte("the plugin")
	const asset = "uffici-for-hermes.tar.gz"
	rs := newReleaseServer(t, "uffici-for-hermes", "", map[string][]byte{asset: body, ChecksumsFile: []byte(sha(body) + "  " + asset + "\n")})
	rs.identity = func(string) string { return ReleaseIdentity("uffici-for-hermes", "0.2.9") }
	r := rs.release("uffici-for-hermes", "0.3.0")
	r.DevIdentity = `https://github\.com/uffici-ai/uffici-for-hermes/\.github/workflows/release\.yml@refs/tags/v.*`
	if _, err := r.FetchVerified(ctx, asset); err == nil || !strings.Contains(err.Error(), "was made at v0.2.9, not at this release's tag") {
		t.Fatalf("release v0.3.0 carrying v0.2.9's signature was accepted: %v", err)
	}
	// Signed at no tag at all (a branch, admitted by the switch): refused.
	rs.identity = func(string) string {
		return "https://github.com/uffici-ai/uffici-for-hermes/.github/workflows/release.yml@refs/heads/main"
	}
	r.DevIdentity = `.*`
	if _, err := r.FetchVerified(ctx, asset); err == nil || !strings.Contains(err.Error(), "at no release tag") {
		t.Fatalf("a signature made at no tag was accepted: %v", err)
	}
	// The latest path: the release signed at v0.1.0 whose manifest says 0.1.2.
	manifest := goodManifest()
	manifest["version"] = latestTag
	setupRig := newRig(t, manifest, false)
	setupRig.rs.identity = func(string) string { return ReleaseIdentity("uffici-for-hermes", "0.1.0") }
	if code := Run(ctx, setupRig.opts, setupRig.out); code != ExitFailed || !strings.Contains(setupRig.out.String(), "but its release was signed at v0.1.0") {
		t.Fatalf("a plugin claiming another version than its signature was installed: %d\n%s", code, setupRig.out)
	}
	if _, err := os.Stat(filepath.Join(setupRig.home, "plugins", "uffici-for-hermes")); err == nil {
		t.Fatal("the plugin was installed")
	}
}

// 🔴 #3118 review: the version is normalised once — `v1.2.3` and `1.2.3` name
// the same signer, the same download and the same refusal, never `vv1.2.3`.
func TestAReleaseVersionWithOrWithoutItsVIsTheSameRelease(t *testing.T) {
	ctx := context.Background()
	body := []byte("the plugin")
	const asset = "uffici-for-hermes.tar.gz"
	rs := newReleaseServer(t, "uffici-for-hermes", "", map[string][]byte{asset: body, ChecksumsFile: []byte(sha(body) + "  " + asset + "\n")})
	for _, version := range []string{"1.2.3", "v1.2.3"} {
		r := rs.release("uffici-for-hermes", version)
		s, err := r.signer()
		if err != nil || s.Identity != ReleaseIdentity("uffici-for-hermes", "1.2.3") || strings.Contains(s.Identity, "vv") {
			t.Fatalf("%q: signer %+v %v", version, s, err)
		}
		if r.publicURL(asset) != rs.URL+"/uffici-ai/uffici-for-hermes/releases/download/v1.2.3/"+asset || r.Describe() != "uffici-ai/uffici-for-hermes v1.2.3" {
			t.Fatalf("%q: %s / %s", version, r.publicURL(asset), r.Describe())
		}
		got, signed, err := r.FetchSigned(ctx, asset)
		if err != nil || signed != "1.2.3" || !bytes.Equal(got, body) {
			t.Fatalf("%q: %q %v", version, signed, err)
		}
	}
}

// 🔴 #3118 review: a root fetch the caller's context ended is fetched again by
// the next caller; a root fetched, or a real failure, is kept.
func TestTheTrustedRootIsFetchedAgainAfterACancelledFetch(t *testing.T) {
	vs := virtualSigstore(t)
	calls := 0
	answers := []error{context.Canceled, nil, nil}
	material := cachedMaterial(func(context.Context) (root.TrustedMaterial, error) {
		err := answers[calls]
		calls++
		if err != nil {
			return nil, fmt.Errorf("fetching: %w", err)
		}
		return vs, nil
	})
	if _, err := material(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("the first fetch: %v", err)
	}
	for range 2 {
		if got, err := material(context.Background()); err != nil || got != vs {
			t.Fatalf("after a cancelled fetch: %v %v", got, err)
		}
	}
	if calls != 2 {
		t.Fatalf("fetched %d times, want 2: the cancelled one, then once", calls)
	}
	// A caller whose own context is done is not cached either, whatever the
	// fetch said.
	calls = 0
	answers = []error{errors.New("offline"), nil}
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	material = cachedMaterial(func(context.Context) (root.TrustedMaterial, error) {
		err := answers[calls]
		calls++
		if err != nil {
			return nil, err
		}
		return vs, nil
	})
	_, _ = material(ended)
	if got, err := material(context.Background()); err != nil || got != vs || calls != 2 {
		t.Fatalf("a fetch by a cancelled caller was kept: %v %v %d", got, err, calls)
	}
	// A real failure is kept: no fetch storm against an unreachable root.
	calls = 0
	answers = []error{errors.New("offline"), nil}
	material = cachedMaterial(func(context.Context) (root.TrustedMaterial, error) {
		err := answers[calls]
		calls++
		return nil, err
	})
	_, _ = material(context.Background())
	if _, err := material(context.Background()); err == nil || calls != 1 {
		t.Fatalf("a real failure was not kept: %v %d", err, calls)
	}
}
