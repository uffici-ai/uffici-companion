package schemas

import (
	"errors"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const schemaID = "https://uffici.ai/test/thing.schema.json"

const testSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://uffici.ai/test/thing.schema.json",
  "type": "object",
  "required": ["token", "entries"],
  "properties": {
    "token": { "type": "string", "minLength": 32 },
    "entries": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["text"],
        "properties": { "text": { "type": "string" }, "kind": { "enum": ["message"] } }
      }
    }
  }
}`

func compile(t *testing.T) *jsonschema.Schema {
	t.Helper()
	compiler := jsonschema.NewCompiler()
	if err := AddResource(compiler, schemaID, []byte(testSchema)); err != nil {
		t.Fatalf("AddResource: %v", err)
	}
	schema, err := compiler.Compile(schemaID)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return schema
}

// 🔴 THE INVARIANT. Both callers publish what this returns — the wire puts it in
// a close frame the plugin reads, the fixture puts it in a log line. The values
// in this module are a plugin's frames and a script's transcript, so a location
// that carried its value would publish a post body, a display name or the wire
// token.
//
// Measured, not assumed: this library's renderings today describe the
// CONSTRAINT ("minLength: got 5, want 32"), not the instance — so the reduction
// is not repairing a leak that exists now. What it does is make the property
// STRUCTURAL rather than per-kind: safety currently depends on every error kind
// choosing to describe the constraint, and a kind added upstream that quotes the
// value would publish it on both paths at once. This test is what would then
// fail, in one place, instead of a plugin receiving a post body in a close
// frame.
func TestSafeLocationsNameWhereAndNeverWhat(t *testing.T) {
	schema := compile(t)
	const secretToken = "tok_thisisthewiretokenandmustnotleak"
	const memberText = "the Lisbon invoice is unpaid"

	doc, err := Parse([]byte(`{
      "token": "` + secretToken + `",
      "entries": [{"text": "` + memberText + `", "kind": "shout"}]
    }`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	validationErr := schema.Validate(doc)
	if validationErr == nil {
		t.Fatal("the document validated, so this test looked at no error at all")
	}

	locations := SafeLocations(validationErr)
	if len(locations) == 0 {
		t.Fatal("no locations were reported")
	}
	joined := strings.Join(locations, ", ")
	for _, secret := range []string{secretToken, memberText, "shout"} {
		if strings.Contains(joined, secret) {
			t.Fatalf("the locations contain %q: %s", secret, joined)
		}
	}
	if !strings.Contains(joined, "/entries/0/kind") {
		t.Fatalf("the locations do not name the failing leaf: %s", joined)
	}
}

// Leaves, not the root: a nested failure's root location is always `/`, which
// tells an author nothing. One answer for both callers.
func TestSafeLocationsReportLeavesNotTheRoot(t *testing.T) {
	schema := compile(t)
	doc, err := Parse([]byte(`{"token":"short","entries":[{"text":1}]}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	locations := SafeLocations(schema.Validate(doc))
	joined := strings.Join(locations, ", ")
	for _, want := range []string{"/token", "/entries/0/text"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("the locations %s do not name %s", joined, want)
		}
	}
}

// An error that is not a validation failure yields nothing, so a caller's
// fallback message is what gets published.
func TestSafeLocationsIgnoreAnUnrelatedError(t *testing.T) {
	if got := SafeLocations(errors.New("the disk went away")); got != nil {
		t.Fatalf("an unrelated error produced %v", got)
	}
}

// A document that is not JSON is refused by AddResource rather than compiled
// into a schema that validates nothing.
func TestAddResourceRefusesADocumentThatIsNotJSON(t *testing.T) {
	compiler := jsonschema.NewCompiler()
	if err := AddResource(compiler, schemaID, []byte("{not json")); err == nil {
		t.Fatal("a malformed document was accepted")
	}
}
