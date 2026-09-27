// Package schemas holds the two things every JSON Schema user in this module
// needs and neither should write twice: registering an embedded document under
// its `$id`, and turning a validation failure into something safe to publish.
//
// 🔴 The second is the one that matters. Both callers PUBLISH their message —
// the wire's becomes a close frame's `detail` that the plugin reads, the
// fixture's becomes a log line — and the values being validated here are a
// plugin's frames and a script's transcript: a post body, a display name, the
// wire token.
//
// The library's renderings describe the constraint rather than the instance
// today, so this is not repairing a present leak. It makes the property
// structural instead of per-kind: without it, safety rests on every error kind
// choosing well, and one kind added upstream that quotes the value would publish
// it on both paths at once.
//
// Compiling itself is NOT here: the wire compiles a set of schemas that `$ref`
// each other and the fixture compiles one, and a single "compile" that served
// both would be a map API for a caller with one schema.
package schemas

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// AddResource registers one embedded schema under the `$id` its `$ref`s resolve
// against.
//
// 🔴 The id is the schema's own `$id`, never its bare file name. A
// `$ref: "entry.schema.json"` resolves relative to the REFERRING schema's `$id`,
// so a resource added under the bare name is never found — and the compiler then
// tries to FETCH the URL, which is the difference between a validator that works
// offline and one that fails closed at the worst moment.
func AddResource(compiler *jsonschema.Compiler, id string, raw []byte) error {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("parsing schema %s: %w", id, err)
	}
	if err := compiler.AddResource(id, doc); err != nil {
		return fmt.Errorf("adding schema %s: %w", id, err)
	}
	return nil
}

// Parse turns a raw document into the form Validate takes.
func Parse(raw []byte) (any, error) {
	return jsonschema.UnmarshalJSON(bytes.NewReader(raw))
}

// SafeLocations reduces a validation failure to WHERE it happened — the leaf
// causes' instance locations, each with the keyword that refused it — and never
// to WHAT is there.
//
// Leaves rather than the root: a root-only location is always `/` for a nested
// failure, which tells a script's author nothing. Both callers get leaves, so
// "where" means one thing across the module.
func SafeLocations(err error) []string {
	var validationErr *jsonschema.ValidationError
	if !errors.As(err, &validationErr) {
		return nil
	}
	return leaves(validationErr)
}

func leaves(err *jsonschema.ValidationError) []string {
	if len(err.Causes) == 0 {
		location := "/" + strings.Join(err.InstanceLocation, "/")
		if path := err.ErrorKind.KeywordPath(); len(path) > 0 {
			location += " (" + strings.Join(path, "/") + ")"
		}
		return []string{location}
	}
	var out []string
	for _, cause := range err.Causes {
		out = append(out, leaves(cause)...)
	}
	return out
}
