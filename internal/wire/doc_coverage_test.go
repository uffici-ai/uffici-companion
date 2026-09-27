package wire

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	contract "github.com/uffici-ai/uffici-companion/wire"
)

// Guard: `docs/local-wire.md` names every field, close reason and state.
//
// 🔴 The document and the schemas are two halves of one contract, and only one
// of them is executable. A field added to a schema and not to the document is
// invisible to the person implementing the other side — which, for this wire,
// is somebody writing a plugin for a harness we do not control, reading the
// document because the schemas are not their language. The lists are DERIVED
// from the schemas here, never restated, so the document cannot fall behind
// them without this failing.

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("reading the working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("walked to the filesystem root without finding go.mod")
		}
		dir = parent
	}
}

func localWireDoc(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(moduleRoot(t), "docs", "local-wire.md"))
	if err != nil {
		t.Fatalf("reading docs/local-wire.md: %v", err)
	}
	return string(raw)
}

// everyPropertyName walks each schema and collects every property name it
// declares, at any depth.
func everyPropertyName(t *testing.T) []string {
	t.Helper()
	names := map[string]bool{}

	var walk func(node any)
	walk = func(node any) {
		object, ok := node.(map[string]any)
		if !ok {
			for _, item := range toSlice(node) {
				walk(item)
			}
			return
		}
		if properties, ok := object["properties"].(map[string]any); ok {
			for name, child := range properties {
				names[name] = true
				walk(child)
			}
		}
		for key, child := range object {
			if key == "properties" {
				continue
			}
			walk(child)
		}
	}

	schemaNames, err := contract.SchemaNames()
	if err != nil {
		t.Fatalf("listing schemas: %v", err)
	}
	for _, name := range schemaNames {
		raw, readErr := fs.ReadFile(contract.FS(), "schema/"+name+".schema.json")
		if readErr != nil {
			t.Fatalf("reading schema %s: %v", name, readErr)
		}
		var doc any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("parsing schema %s: %v", name, err)
		}
		walk(doc)
	}

	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func toSlice(node any) []any {
	if items, ok := node.([]any); ok {
		return items
	}
	return nil
}

func TestDocNamesEveryField(t *testing.T) {
	doc := localWireDoc(t)
	fields := everyPropertyName(t)
	// Non-vacuity: an empty derivation would make the loop assert nothing.
	if len(fields) < 25 {
		t.Fatalf("derived %d field name(s) from the schemas — the walk is broken", len(fields))
	}
	var missing []string
	for _, field := range fields {
		if !strings.Contains(doc, field) {
			missing = append(missing, field)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("docs/local-wire.md never names: %s\nThe document is the only half of this contract a plugin author reads.", strings.Join(missing, ", "))
	}
}

func TestDocNamesEveryCloseReason(t *testing.T) {
	doc := localWireDoc(t)
	reasons := CloseReasons()
	if len(reasons) != 8 {
		t.Fatalf("%d close reason(s) — the vocabulary is closed at eight", len(reasons))
	}
	for _, reason := range reasons {
		if !strings.Contains(doc, string(reason)) {
			t.Errorf("docs/local-wire.md never names the close reason %q", reason)
		}
	}
}

func TestDocNamesEveryState(t *testing.T) {
	doc := localWireDoc(t)
	// The states of the per-request machine, as the issue names them.
	for _, state := range []string{
		"delivered", "acked", "fresh", "resumed", "opening", "posting",
		"closed", "cancelled", "expired",
	} {
		if !strings.Contains(doc, state) {
			t.Errorf("docs/local-wire.md never names the state %q", state)
		}
	}
}

func TestDocNamesEveryFrameTypeAndEnumValue(t *testing.T) {
	doc := localWireDoc(t)
	for _, frame := range FrameTypes() {
		if !strings.Contains(doc, "`"+frame+"`") {
			t.Errorf("docs/local-wire.md never names the %s frame", frame)
		}
	}
	// Every closed vocabulary, derived from the schemas rather than restated.
	schemaNames, err := contract.SchemaNames()
	if err != nil {
		t.Fatalf("listing schemas: %v", err)
	}
	found := 0
	for _, name := range schemaNames {
		raw, readErr := fs.ReadFile(contract.FS(), "schema/"+name+".schema.json")
		if readErr != nil {
			t.Fatalf("reading schema %s: %v", name, readErr)
		}
		for _, value := range enumValues(t, raw) {
			found++
			if !strings.Contains(doc, value) {
				t.Errorf("docs/local-wire.md never names %q, a value the %s schema admits", value, name)
			}
		}
	}
	if found < 15 {
		t.Fatalf("derived %d enum value(s) — the walk is broken", found)
	}
}

func enumValues(t *testing.T, raw []byte) []string {
	t.Helper()
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing a schema: %v", err)
	}
	var out []string
	var walk func(node any)
	walk = func(node any) {
		switch typed := node.(type) {
		case map[string]any:
			if values, ok := typed["enum"].([]any); ok {
				for _, value := range values {
					if text, ok := value.(string); ok {
						out = append(out, text)
					}
				}
			}
			for _, child := range typed {
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(doc)
	return out
}
