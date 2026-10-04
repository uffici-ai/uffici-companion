package protocol

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	contract "github.com/uffici-ai/uffici-companion/protocol"
)

// 🔴 THIS TEST IS WHY THE STRUCTS MAY BE HAND-WRITTEN (#2077).
//
// Every schema the platform emits is walked against its struct, RECURSIVELY and
// in both directions:
//
//   - a property with no struct field is a value the platform sends that this
//     companion silently reads as zero;
//   - a struct field with no property is a field the platform's strict request
//     schemas refuse — `invalid_request` in production, never here;
//   - a REQUIRED property whose field is `omitempty` is a zero value the encoder
//     drops, and an OPTIONAL one without `omitempty` is a zero value it sends
//     (`errorClass: ""` fails the schema's `minLength`).

// schemaNode is the part of an emitted JSON Schema this walk reads.
type schemaNode struct {
	Type       any                    `json:"type"`
	Properties map[string]*schemaNode `json:"properties"`
	Required   []string               `json:"required"`
	Items      *schemaNode            `json:"items"`
	// OneOf is a discriminated union (#1966's poll answer). One struct decodes every
	// arm, so it is walked as their merge: a property of ANY arm, required only when
	// EVERY arm requires it.
	OneOf []*schemaNode `json:"oneOf"`
}

// merged folds a union's arms into the one shape a struct decoding all of them has.
func (n *schemaNode) merged() *schemaNode {
	if len(n.OneOf) == 0 || len(n.Properties) > 0 {
		return n
	}
	out := &schemaNode{Type: "object", Properties: map[string]*schemaNode{}}
	counts := map[string]int{}
	for _, arm := range n.OneOf {
		for name, property := range arm.Properties {
			if _, seen := out.Properties[name]; !seen {
				out.Properties[name] = property
			}
		}
		for _, name := range arm.Required {
			counts[name]++
		}
	}
	for name, count := range counts {
		if count == len(n.OneOf) {
			out.Required = append(out.Required, name)
		}
	}
	sort.Strings(out.Required)
	return out
}

func loadSchema(t *testing.T, name string) *schemaNode {
	t.Helper()
	raw, err := fs.ReadFile(contract.FS(), "schema/"+name+".schema.json")
	if err != nil {
		t.Fatalf("reading schema %s: %v", name, err)
	}
	var node schemaNode
	if err := json.Unmarshal(raw, &node); err != nil {
		t.Fatalf("parsing schema %s: %v", name, err)
	}
	return node.merged()
}

type field struct {
	name      string
	omitempty bool
	typ       reflect.Type
}

// fieldsOf reads a struct's JSON fields, flattening the embedded Envelope.
func fieldsOf(t *testing.T, typ reflect.Type) []field {
	t.Helper()
	var out []field
	for i := range typ.NumField() {
		f := typ.Field(i)
		if f.Anonymous {
			out = append(out, fieldsOf(t, f.Type)...)
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "" || tag == "-" {
			t.Fatalf("%s.%s has no json tag — every protocol field is named by the contract", typ.Name(), f.Name)
		}
		parts := strings.Split(tag, ",")
		out = append(out, field{name: parts[0], omitempty: len(parts) > 1 && parts[1] == "omitempty", typ: f.Type})
	}
	return out
}

var timeType = reflect.TypeOf(time.Time{})

// structOf returns the struct a field recurses into, or nil for a leaf.
func structOf(typ reflect.Type) reflect.Type {
	for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct || typ == timeType {
		return nil
	}
	return typ
}

// compare walks one struct against one schema object node.
func compare(t *testing.T, path string, typ reflect.Type, node *schemaNode) error {
	t.Helper()
	if len(node.Properties) == 0 {
		return fmt.Errorf("%s: the schema declares no properties — the comparison would be empty", path)
	}
	fields := fieldsOf(t, typ)
	got := make([]string, 0, len(fields))
	for _, f := range fields {
		got = append(got, f.name)
	}
	want := make([]string, 0, len(node.Properties))
	for name := range node.Properties {
		want = append(want, name)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		return fmt.Errorf("%s: struct and schema disagree.\n  struct: %v\n  schema: %v", path, got, want)
	}
	required := map[string]bool{}
	for _, name := range node.Required {
		required[name] = true
	}
	for _, f := range fields {
		if required[f.name] == f.omitempty {
			return fmt.Errorf("%s.%s: required=%v but omitempty=%v — a required field must always be sent, an optional one only when set",
				path, f.name, required[f.name], f.omitempty)
		}
		inner := structOf(f.typ)
		if inner == nil {
			continue
		}
		property := node.Properties[f.name]
		if property.Items != nil {
			property = property.Items
		}
		// A nested union (a read's card, #2354) is walked as its arms' merge, like a
		// top-level one.
		if err := compare(t, path+"."+f.name, inner, property.merged()); err != nil {
			return err
		}
	}
	return nil
}

func TestEveryStructMatchesItsSchema(t *testing.T) {
	cases := map[string]any{
		"ack":                          Ack{},
		"cancel":                       Cancel{},
		"close-request":                CloseRequest{},
		"close-response":               CloseResponse{},
		"connect-ticket-request":       ConnectTicketRequest{},
		"context":                      Context{},
		"conversation-ticket-request":  ConversationTicketRequest{},
		"conversation-ticket-response": ConversationTicketResponse{},
		"disconnect":                   Disconnect{},
		"entry":                        Entry{},
		"error":                        ErrorEnvelope{},
		"hello":                        Hello{},
		"identity":                     Identity{},
		"log-batch":                    LogBatch{},
		"log-batch-response":           LogBatchResponse{},
		"logs-ticket-request":          LogsTicketRequest{},
		"logs-ticket-response":         LogsTicketResponse{},
		"pairing-poll-response":        PairingPollResponse{},
		"pairing-request":              PairingRequest{},
		"pairing-response":             PairingResponse{},
		"post-request":                 PostRequest{},
		"post-response":                PostResponse{},
		"read-response":                ReadResponse{},
		"release":                      Release{},
		"ring":                         Ring{},
		"ticket-response":              TicketResponse{},
		"tool-call-request":            ToolCallRequest{},
		"tool-call-response":           ToolCallResponse{},
		"tool-catalog-response":        ToolCatalogResponse{},
	}
	manifest, err := contract.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	// Non-vacuity, and the other direction: every schema the platform emits has
	// a struct here, and no struct here names a schema the platform dropped.
	if len(manifest.Schemas) < 21 {
		t.Fatalf("the manifest names %d schemas — the copy is broken or thinned", len(manifest.Schemas))
	}
	names := make([]string, 0, len(cases))
	for name := range cases {
		names = append(names, name)
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, manifest.Schemas) {
		t.Fatalf("structs cover %v, the manifest names %v", names, manifest.Schemas)
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			if err := compare(t, name, reflect.TypeOf(value), loadSchema(t, name)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The walk itself must be able to fail — the mutation this guard exists for,
// run on every build rather than once by hand.
func TestTheWalkRefusesADriftedStruct(t *testing.T) {
	type extraField struct {
		Envelope
		RingID  string `json:"ringId"`
		Outcome string `json:"outcome"`
	}
	type missingField struct {
		Envelope
	}
	type optionalRequired struct {
		Envelope
		RingID string `json:"ringId,omitempty"`
	}
	for name, typ := range map[string]reflect.Type{
		"a field the schema does not admit":   reflect.TypeOf(extraField{}),
		"a property with no field":            reflect.TypeOf(missingField{}),
		"a required field sent only when set": reflect.TypeOf(optionalRequired{}),
	} {
		t.Run(name, func(t *testing.T) {
			if compare(t, "ack", typ, loadSchema(t, "ack")) == nil {
				t.Fatal("the parity walk accepted a struct that disagrees with the ack schema")
			}
		})
	}
}
