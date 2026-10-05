package wire

import (
	"encoding/json"
	"io/fs"
	"reflect"
	"sort"
	"strings"
	"testing"

	contract "github.com/uffici-ai/uffici-companion/wire"
)

// 🔴 THIS TEST IS WHY THE TYPES MAY BE HAND-WRITTEN.
//
// The issue asked for generated types. A generator was weighed and declined:
// `oneOf` and the two-shaped `hello` generate poorly in Go, and a code
// generator is a second toolchain dependency inside a child whose whole purpose
// is to need none. What generation would actually have bought is the guarantee
// below — that the structs and the schemas cannot drift — so that guarantee is
// asserted directly, in both directions, which a generator alone would not do:
// generated code drifts the moment someone edits it by hand.
//
// A field added to a schema without a struct field decodes to nothing and reads
// as a zero value at run time. A struct field with no schema property is a
// field the contract does not admit, which the validator will reject in
// production and never here.

// jsonFieldNames reads the JSON field set of a struct, flattening the embedded
// Envelope so `v` and `type` are counted once.
func jsonFieldNames(t *testing.T, value any) []string {
	t.Helper()
	typ := reflect.TypeOf(value)
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	var names []string
	for i := range typ.NumField() {
		field := typ.Field(i)
		if field.Anonymous {
			names = append(names, jsonFieldNames(t, reflect.New(field.Type).Interface())...)
			continue
		}
		tag := field.Tag.Get("json")
		if tag == "" || tag == "-" {
			t.Fatalf("%s.%s has no json tag — every wire field is named by the contract", typ.Name(), field.Name)
		}
		names = append(names, strings.Split(tag, ",")[0])
	}
	sort.Strings(names)
	return names
}

// schemaProperties reads the property names one schema admits, following the
// `oneOf` branches so the two `hello` shapes are covered.
func schemaProperties(t *testing.T, name string) map[string][]string {
	t.Helper()
	raw, err := fs.ReadFile(contract.FS(), "schema/"+name+".schema.json")
	if err != nil {
		t.Fatalf("reading schema %s: %v", name, err)
	}
	var doc struct {
		Properties map[string]json.RawMessage `json:"properties"`
		OneOf      []struct {
			Ref string `json:"$ref"`
		} `json:"oneOf"`
		Defs map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing schema %s: %v", name, err)
	}

	shared := make([]string, 0, len(doc.Properties))
	for property := range doc.Properties {
		shared = append(shared, property)
	}

	out := map[string][]string{}
	if len(doc.OneOf) == 0 {
		sort.Strings(shared)
		out[name] = shared
		return out
	}
	for _, branch := range doc.OneOf {
		key := strings.TrimPrefix(branch.Ref, "#/$defs/")
		def, ok := doc.Defs[key]
		if !ok {
			t.Fatalf("schema %s references $defs/%s, which is absent", name, key)
		}
		names := append([]string{}, shared...)
		for property := range def.Properties {
			names = append(names, property)
		}
		sort.Strings(names)
		out[key] = names
	}
	return out
}

func TestEveryStructMatchesItsSchema(t *testing.T) {
	cases := []struct {
		schema string
		branch string
		value  any
	}{
		{"brief", "brief", &Brief{}},
		{"ack", "ack", &Ack{}},
		{"ready", "ready", &Ready{}},
		{"opener", "opener", &Opener{}},
		{"post", "post", &Post{}},
		{"done", "done", &Done{}},
		{"cancel", "cancel", &Cancel{}},
		{"release", "release", &Release{}},
		{"call", "call", &Call{}},
		{"result", "result", &Result{}},
		{"hello", "pluginHello", &PluginHello{}},
		{"hello", "companionHello", &CompanionHello{}},
		{"entry", "entry", &Entry{}},
		{"context", "context", &Context{}},
	}
	// Non-vacuity: every frame type plus the entry, the context and both hello shapes.
	if len(cases) != len(FrameTypes())+3 {
		t.Fatalf("%d case(s) for %d frame types plus entry, context and the second hello shape — a frame is unchecked", len(cases), len(FrameTypes()))
	}

	for _, testCase := range cases {
		t.Run(testCase.branch, func(t *testing.T) {
			properties := schemaProperties(t, testCase.schema)
			want, ok := properties[testCase.branch]
			if !ok {
				t.Fatalf("schema %s has no branch %q", testCase.schema, testCase.branch)
			}
			got := jsonFieldNames(t, testCase.value)
			if len(want) == 0 {
				t.Fatalf("schema %s/%s declares no properties — the comparison would be empty", testCase.schema, testCase.branch)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("struct and schema disagree.\n  struct: %v\n  schema: %v\nA field in one and not the other is a value that silently reads as zero, or one the validator refuses in production and never here.", got, want)
			}
		})
	}
}

// The nested shapes the frames embed, which the loop above cannot reach.
func TestNestedStructsMatchTheirSchemaDefs(t *testing.T) {
	raw, err := fs.ReadFile(contract.FS(), "schema/entry.schema.json")
	if err != nil {
		t.Fatalf("reading the entry schema: %v", err)
	}
	var doc struct {
		Defs map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing the entry schema: %v", err)
	}

	for _, testCase := range []struct {
		def   string
		value any
	}{
		{"author", &Author{}},
		{"attachment", &Attachment{}},
	} {
		t.Run(testCase.def, func(t *testing.T) {
			def, ok := doc.Defs[testCase.def]
			if !ok {
				t.Fatalf("the entry schema has no $defs/%s", testCase.def)
			}
			want := make([]string, 0, len(def.Properties))
			for property := range def.Properties {
				want = append(want, property)
			}
			sort.Strings(want)
			if len(want) == 0 {
				t.Fatalf("$defs/%s declares no properties", testCase.def)
			}
			if got := jsonFieldNames(t, testCase.value); !reflect.DeepEqual(got, want) {
				t.Fatalf("struct and schema disagree.\n  struct: %v\n  schema: %v", got, want)
			}
		})
	}
}

// The hello's inline objects on the plugin's shape: `drift` (#2406) and
// `harnessSupport` (#2414).
func TestHelloInlineObjectsMatchTheHelloSchema(t *testing.T) {
	raw, err := fs.ReadFile(contract.FS(), "schema/hello.schema.json")
	if err != nil {
		t.Fatalf("reading the hello schema: %v", err)
	}
	var doc struct {
		Defs struct {
			PluginHello struct {
				Properties map[string]struct {
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"properties"`
			} `json:"pluginHello"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing the hello schema: %v", err)
	}
	for _, testCase := range []struct {
		property string
		value    any
	}{
		{"drift", &Drift{}},
		{"harnessSupport", &HarnessSupport{}},
	} {
		t.Run(testCase.property, func(t *testing.T) {
			want := make([]string, 0, 3)
			for property := range doc.Defs.PluginHello.Properties[testCase.property].Properties {
				want = append(want, property)
			}
			sort.Strings(want)
			if len(want) == 0 {
				t.Fatalf("the plugin hello's %s declares no properties — the comparison would be empty", testCase.property)
			}
			if got := jsonFieldNames(t, testCase.value); !reflect.DeepEqual(got, want) {
				t.Fatalf("struct and schema disagree.\n  struct: %v\n  schema: %v", got, want)
			}
		})
	}
}

// The `cause` object lives inline on the brief rather than in `$defs`.
func TestCauseMatchesTheBriefSchema(t *testing.T) {
	raw, err := fs.ReadFile(contract.FS(), "schema/brief.schema.json")
	if err != nil {
		t.Fatalf("reading the brief schema: %v", err)
	}
	var doc struct {
		Properties struct {
			Cause struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"cause"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing the brief schema: %v", err)
	}
	want := make([]string, 0, len(doc.Properties.Cause.Properties))
	for property := range doc.Properties.Cause.Properties {
		want = append(want, property)
	}
	sort.Strings(want)
	if len(want) == 0 {
		t.Fatal("the brief schema declares no cause properties")
	}
	if got := jsonFieldNames(t, &Cause{}); !reflect.DeepEqual(got, want) {
		t.Fatalf("Cause and the brief schema disagree.\n  struct: %v\n  schema: %v", got, want)
	}
}

// The `error` object lives inline on the cancel rather than in `$defs`.
func TestCancelErrorMatchesTheCancelSchema(t *testing.T) {
	raw, err := fs.ReadFile(contract.FS(), "schema/cancel.schema.json")
	if err != nil {
		t.Fatalf("reading the cancel schema: %v", err)
	}
	var doc struct {
		Properties struct {
			Error struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"error"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing the cancel schema: %v", err)
	}
	want := make([]string, 0, len(doc.Properties.Error.Properties))
	for property := range doc.Properties.Error.Properties {
		want = append(want, property)
	}
	sort.Strings(want)
	if len(want) == 0 {
		t.Fatal("the cancel schema declares no error properties")
	}
	if got := jsonFieldNames(t, &CancelError{}); !reflect.DeepEqual(got, want) {
		t.Fatalf("CancelError and the cancel schema disagree.\n  struct: %v\n  schema: %v", got, want)
	}
}

// The `error` object lives inline on the result rather than in `$defs` (#2028).
func TestResultErrorMatchesTheResultSchema(t *testing.T) {
	raw, err := fs.ReadFile(contract.FS(), "schema/result.schema.json")
	if err != nil {
		t.Fatalf("reading the result schema: %v", err)
	}
	var doc struct {
		Properties struct {
			Error struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"error"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing the result schema: %v", err)
	}
	want := make([]string, 0, len(doc.Properties.Error.Properties))
	for property := range doc.Properties.Error.Properties {
		want = append(want, property)
	}
	sort.Strings(want)
	if len(want) == 0 {
		t.Fatal("the result schema declares no error properties")
	}
	if got := jsonFieldNames(t, &ResultError{}); !reflect.DeepEqual(got, want) {
		t.Fatalf("ResultError and the result schema disagree.\n  struct: %v\n  schema: %v", got, want)
	}
}

// The context's block and missing shapes live inline in its `items`.
func TestContextItemsMatchTheContextSchema(t *testing.T) {
	raw, err := fs.ReadFile(contract.FS(), "schema/context.schema.json")
	if err != nil {
		t.Fatalf("reading the context schema: %v", err)
	}
	type items struct {
		Items struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"items"`
	}
	var doc struct {
		Properties struct {
			Blocks  items `json:"blocks"`
			Missing items `json:"missing"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing the context schema: %v", err)
	}
	for _, testCase := range []struct {
		name       string
		properties map[string]json.RawMessage
		value      any
	}{
		{"blocks", doc.Properties.Blocks.Items.Properties, &ContextBlock{}},
		{"missing", doc.Properties.Missing.Items.Properties, &ContextMissing{}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			want := make([]string, 0, len(testCase.properties))
			for property := range testCase.properties {
				want = append(want, property)
			}
			sort.Strings(want)
			if len(want) == 0 {
				t.Fatalf("context/%s declares no item properties", testCase.name)
			}
			if got := jsonFieldNames(t, testCase.value); !reflect.DeepEqual(got, want) {
				t.Fatalf("struct and schema disagree.\n  struct: %v\n  schema: %v", got, want)
			}
		})
	}
}
