package wire

import (
	"encoding/json"
	"io/fs"
	"strings"
	"testing"

	contract "github.com/uffici-ai/uffici-companion/wire"
)

// The golden set is the contract. These tests are what stop it becoming
// decoration: every valid golden must validate, every invalid one must be
// rejected with the close reason its FILE NAME promises, and the counts must
// match the committed manifest — so a golden deleted, or added and forgotten,
// fails rather than quietly shrinking the evidence.

func goldens(t *testing.T, kind string) []string {
	t.Helper()
	entries, err := fs.ReadDir(contract.FS(), "golden/"+kind)
	if err != nil {
		t.Fatalf("reading golden/%s: %v", kind, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func read(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := fs.ReadFile(contract.FS(), path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return raw
}

func TestEveryValidGoldenValidates(t *testing.T) {
	names := goldens(t, "valid")
	// 🔴 Non-vacuity: an empty directory would make the loop below assert
	// nothing at all, forever, while reporting success.
	if len(names) < 15 {
		t.Fatalf("found %d valid golden(s) — the set covers every frame, every ack outcome and session state, every done outcome and every cancel reason, so this is too few", len(names))
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			if err := Validate(read(t, "golden/valid/"+name)); err != nil {
				t.Fatalf("a VALID golden was rejected: %v", err)
			}
		})
	}
}

func TestEveryInvalidGoldenIsRejectedWithItsNamedReason(t *testing.T) {
	names := goldens(t, "invalid")
	if len(names) < 8 {
		t.Fatalf("found %d invalid golden(s) — too few to cover the reasons a single frame can earn", len(names))
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			// The file name IS the expectation: `<disposition>--<case>.json`, where
			// the disposition is a close reason, or — for the call tunnel (#2028) —
			// what a call-shaped frame earns instead of a close.
			want, _, found := strings.Cut(name, "--")
			if !found {
				t.Fatalf("an invalid golden must be named <disposition>--<case>.json; %q is not", name)
			}
			err := Validate(read(t, "golden/invalid/"+name))
			switch CallDisposition(want) {
			case DispositionMalformedPayload:
				// 🔴 Answered, not closed: a call over its bound gets a result
				// carrying its own callId, and the connection stays open.
				callErr, ok := err.(*CallError)
				if !ok {
					t.Fatalf("expected a *CallError answering %s, got %T: %v", want, err, err)
				}
				if callErr.Class != ClassMalformedPayload || callErr.CallID == "" {
					t.Fatalf("answered %s for call %q; the golden promises %s with its callId", callErr.Class, callErr.CallID, want)
				}
				return
			case DispositionUnknownCall:
				// 🔴 Schema-valid, and invalid only against state: a result naming
				// no call in flight. That is the PLUGIN's to discard — the
				// companion never receives a result — and the plugin's
				// `test_call_client.py` drives this same golden through its
				// tracker. Here it must validate, and be a result.
				if err != nil {
					t.Fatalf("an unknown_call golden must be schema-valid (its fault is state, not shape): %v", err)
				}
				var envelope Envelope
				if jsonErr := json.Unmarshal(read(t, "golden/invalid/"+name), &envelope); jsonErr != nil || envelope.Type != "result" {
					t.Fatalf("an unknown_call golden must be a result frame, got %q", envelope.Type)
				}
				return
			}
			if err == nil {
				t.Fatalf("an INVALID golden was accepted; it must close with %s", want)
			}
			closeErr, ok := err.(*CloseError)
			if !ok {
				t.Fatalf("expected a *CloseError, got %T: %v", err, err)
			}
			if string(closeErr.Reason) != want {
				t.Fatalf("closed with %s, but the golden's name promises %s", closeErr.Reason, want)
			}
		})
	}
}

func TestGoldenCountsMatchTheManifest(t *testing.T) {
	manifest, err := contract.LoadManifest()
	if err != nil {
		t.Fatalf("loading the manifest: %v", err)
	}

	byFrame := map[string]int{}
	for _, name := range goldens(t, "valid") {
		var envelope Envelope
		if err := json.Unmarshal(read(t, "golden/valid/"+name), &envelope); err != nil {
			t.Fatalf("%s is not a JSON object: %v", name, err)
		}
		byFrame[envelope.Type]++
	}
	byReason := map[string]int{}
	byDisposition := map[string]int{}
	for _, name := range goldens(t, "invalid") {
		prefix, _, _ := strings.Cut(name, "--")
		if isCallDisposition(prefix) {
			byDisposition[prefix]++
		} else {
			byReason[prefix]++
		}
	}

	compare := func(what string, want, got map[string]int) {
		for key, count := range want {
			if got[key] != count {
				t.Errorf("manifest says %d %s golden(s) for %s, found %d", count, what, key, got[key])
			}
		}
		for key, count := range got {
			if _, ok := want[key]; !ok {
				t.Errorf("%d %s golden(s) for %s, which the manifest does not list", count, what, key)
			}
		}
	}
	compare("valid", manifest.ValidGoldensByFrame, byFrame)
	compare("invalid (close)", manifest.InvalidGoldensByCloseReason, byReason)
	compare("invalid (call)", manifest.InvalidGoldensByCallDisposition, byDisposition)

	// The totals are the manifest's own arithmetic; a count moved without its
	// total is a manifest that disagrees with itself.
	sum := func(m map[string]int) (n int) {
		for _, count := range m {
			n += count
		}
		return n
	}
	if got := sum(byFrame); got != manifest.Totals.Valid {
		t.Errorf("totals.valid is %d, found %d valid golden(s)", manifest.Totals.Valid, got)
	}
	if got := sum(byReason) + sum(byDisposition); got != manifest.Totals.Invalid {
		t.Errorf("totals.invalid is %d, found %d invalid golden(s)", manifest.Totals.Invalid, got)
	}
}

func isCallDisposition(prefix string) bool {
	for _, known := range CallDispositions() {
		if string(known) == prefix {
			return true
		}
	}
	return false
}

func TestEveryFrameTypeHasAtLeastOneValidGolden(t *testing.T) {
	manifest, err := contract.LoadManifest()
	if err != nil {
		t.Fatalf("loading the manifest: %v", err)
	}
	for _, frame := range FrameTypes() {
		if manifest.ValidGoldensByFrame[frame] < 1 {
			t.Errorf("no valid golden for the %s frame — it is specified and unexercised", frame)
		}
	}
}

func TestEverySchemaIsAFrameTypeOrTheEntry(t *testing.T) {
	names, err := contract.SchemaNames()
	if err != nil {
		t.Fatalf("listing schemas: %v", err)
	}
	// A schema with no frame and no golden is a specification nothing checks.
	known := map[string]bool{"entry": true}
	for _, frame := range FrameTypes() {
		known[frame] = true
	}
	for _, name := range names {
		if !known[name] {
			t.Errorf("schema %q names neither a frame type nor the entry", name)
		}
	}
	for frame := range known {
		found := false
		for _, name := range names {
			if name == frame {
				found = true
			}
		}
		if !found {
			t.Errorf("no schema for %q", frame)
		}
	}
}

// 🔴 Forward compatibility is a REQUIREMENT of this wire, not a tolerance: the
// summary entry kind of wave 4 must arrive with no version bump. These assert
// the two shapes that must keep validating.
func TestForwardCompatibility(t *testing.T) {
	raw := read(t, "golden/valid/brief-forward-compatible.json")
	if err := Validate(raw); err != nil {
		t.Fatalf("a frame with an unknown field and an unknown entry kind must validate: %v", err)
	}

	var probe struct {
		Priority string `json:"priority"`
		Delta    []struct {
			Kind string `json:"kind"`
		} `json:"delta"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("reading the forward-compatibility golden: %v", err)
	}
	// Non-vacuity: if the golden ever loses the unknown field or the unknown
	// kind, the test above would pass over an ordinary frame and prove nothing.
	if probe.Priority == "" {
		t.Fatal("the forward-compatibility golden no longer carries an unknown FIELD")
	}
	if len(probe.Delta) == 0 || probe.Delta[0].Kind == "message" {
		t.Fatal("the forward-compatibility golden no longer carries an unknown entry KIND")
	}

	decoded, err := Decode(raw, FromCompanion)
	if err != nil {
		t.Fatalf("decoding the forward-compatibility golden: %v", err)
	}
	brief, ok := decoded.(*Brief)
	if !ok {
		t.Fatalf("expected a *Brief, got %T", decoded)
	}
	if brief.Delta[0].Kind != probe.Delta[0].Kind {
		t.Fatalf("the decoder rewrote an unknown kind: %q", brief.Delta[0].Kind)
	}
}

func TestDecodeTellsTheTwoHelloShapesApartByDirection(t *testing.T) {
	// One `type` value, two shapes. Nothing but the direction can separate them,
	// which is why Decode takes it.
	fromPlugin, err := Decode(read(t, "golden/valid/hello-plugin.json"), FromPlugin)
	if err != nil {
		t.Fatalf("decoding the plugin's hello: %v", err)
	}
	if _, ok := fromPlugin.(*PluginHello); !ok {
		t.Fatalf("expected a *PluginHello, got %T", fromPlugin)
	}

	fromCompanion, err := Decode(read(t, "golden/valid/hello-companion.json"), FromCompanion)
	if err != nil {
		t.Fatalf("decoding the companion's hello: %v", err)
	}
	if _, ok := fromCompanion.(*CompanionHello); !ok {
		t.Fatalf("expected a *CompanionHello, got %T", fromCompanion)
	}
}

// 🔴 The token is secret-class. A `%v` on a frame must not print it.
func TestPluginHelloNeverFormatsItsToken(t *testing.T) {
	decoded, err := Decode(read(t, "golden/valid/hello-plugin.json"), FromPlugin)
	if err != nil {
		t.Fatalf("decoding: %v", err)
	}
	hello, ok := decoded.(*PluginHello)
	if !ok {
		t.Fatalf("expected a *PluginHello, got %T", decoded)
	}
	if hello.Token == "" {
		t.Fatal("the golden carries no token, so this test would prove nothing")
	}
	if strings.Contains(hello.String(), hello.Token) {
		t.Fatal("PluginHello.String() prints the token — it is secret-class and must never reach a log line")
	}
}

// A decoder error must carry the frame type and a schema location, never a
// value out of the frame: a post body or the token would otherwise ride into
// whatever log line reports the close.
func TestCloseErrorCarriesNoFrameContent(t *testing.T) {
	raw := read(t, "golden/valid/post.json")
	var post Post
	if err := json.Unmarshal(raw, &post); err != nil {
		t.Fatalf("reading the post golden: %v", err)
	}
	broken := strings.Replace(string(raw), `"requestId"`, `"request_id"`, 1)
	err := Validate([]byte(broken))
	if err == nil {
		t.Fatal("a renamed required field must be rejected")
	}
	if strings.Contains(err.Error(), post.Text) {
		t.Fatalf("the close error quotes the post body: %s", err.Error())
	}
}

// ── The fail-closed paths ──────────────────────────────────────────────────
//
// 🔴 Every one of these is a way in, and each must end in a close rather than
// in a decoded frame. They are the branches a happy-path suite never reaches,
// which is exactly where a validator quietly stops validating.

func TestValidateRefusesTheWaysIn(t *testing.T) {
	for _, testCase := range []struct {
		name string
		raw  []byte
		want CloseReason
	}{
		{"not JSON at all", []byte("this is not a frame"), ReasonMalformedFrame},
		{"a JSON array", []byte(`[{"v":1,"type":"post"}]`), ReasonMalformedFrame},
		{"no version", []byte(`{"type":"post","requestId":"r","text":"hi"}`), ReasonUnsupportedVersion},
		{"a future version", []byte(`{"v":2,"type":"post","requestId":"r","text":"hi"}`), ReasonUnsupportedVersion},
		{"an unknown frame type", []byte(`{"v":1,"type":"whisper"}`), ReasonMalformedFrame},
		{"an oversized frame", oversizedFrame(), ReasonMalformedFrame},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := Validate(testCase.raw)
			if err == nil {
				t.Fatalf("accepted; it must close with %s", testCase.want)
			}
			closeErr, ok := err.(*CloseError)
			if !ok {
				t.Fatalf("expected a *CloseError, got %T: %v", err, err)
			}
			if closeErr.Reason != testCase.want {
				t.Fatalf("closed with %s, want %s", closeErr.Reason, testCase.want)
			}
		})
	}
}

func oversizedFrame() []byte {
	body := make([]byte, FrameMaxBytes+1)
	for i := range body {
		body[i] = 'x'
	}
	return body
}

func TestDecodeRefusesWhatValidateRefuses(t *testing.T) {
	// Decode must not be a second, laxer door into the same frames.
	if _, err := Decode([]byte(`{"v":2,"type":"post","requestId":"r","text":"hi"}`), FromPlugin); err == nil {
		t.Fatal("Decode accepted a frame Validate rejects")
	}
}

func TestCloseErrorMessagesNameTheReason(t *testing.T) {
	withType := &CloseError{Reason: ReasonMalformedFrame, FrameType: "post", Detail: "schema violation at /text"}
	if !strings.Contains(withType.Error(), "malformed_frame") || !strings.Contains(withType.Error(), "post") {
		t.Fatalf("a close error must name its reason and the frame type: %s", withType.Error())
	}
	withoutType := &CloseError{Reason: ReasonUnsupportedVersion, Detail: "v=2"}
	if !strings.Contains(withoutType.Error(), "unsupported_version") {
		t.Fatalf("a close error must name its reason: %s", withoutType.Error())
	}
}

func TestCloseReasonsAndFrameTypesAreTheClosedVocabularies(t *testing.T) {
	if len(CloseReasons()) != 8 {
		t.Fatalf("%d close reasons — the vocabulary is closed at eight", len(CloseReasons()))
	}
	// Ten since #2028's call/result tunnel: generic control-plane operations
	// ride one frame pair instead of a frame each.
	if len(FrameTypes()) != 10 {
		t.Fatalf("%d frame types — the wire has ten", len(FrameTypes()))
	}
	seen := map[string]bool{}
	for _, reason := range CloseReasons() {
		if seen[string(reason)] {
			t.Fatalf("duplicate close reason %q", reason)
		}
		seen[string(reason)] = true
	}
}

// 🔴 Nothing asserted `FrameType` before — every close-reason test read only
// `Reason`, so the field could have been empty everywhere and the suite would
// not have noticed. It is what a log line names the offending frame by.
func TestCloseErrorNamesTheFrameWheneverItWasReadable(t *testing.T) {
	for _, testCase := range []struct {
		name string
		raw  []byte
		want string
	}{
		{"an unknown type is still a readable type", []byte(`{"v":1,"type":"whisper"}`), "whisper"},
		{"a future version on a known type", []byte(`{"v":2,"type":"post","requestId":"r","text":"hi"}`), "post"},
		{"a schema violation", []byte(`{"v":1,"type":"post","requestId":"r"}`), "post"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := Validate(testCase.raw)
			if err == nil {
				t.Fatal("accepted a frame that must be refused")
			}
			closeErr, ok := err.(*CloseError)
			if !ok {
				t.Fatalf("expected a *CloseError, got %T", err)
			}
			if closeErr.FrameType != testCase.want {
				t.Fatalf("FrameType is %q, want %q — a close with no frame named is a log line nobody can act on", closeErr.FrameType, testCase.want)
			}
		})
	}
}
