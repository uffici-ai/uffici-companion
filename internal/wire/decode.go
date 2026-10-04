package wire

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/uffici-ai/uffici-companion/internal/schemas"

	contract "github.com/uffici-ai/uffici-companion/wire"
)

// CloseError is a protocol error: the connection closes with Reason.
//
// 🔴 It carries no frame content, deliberately. A decoder error is logged by
// the side that produced it, and a message quoting the offending frame would
// put a post body — or the token — into a log line. The frame TYPE and the
// reason are what a reader needs; the rest is in the frame the sender still has.
type CloseError struct {
	Reason CloseReason
	// FrameType is the `type` value when it was readable, otherwise empty.
	FrameType string
	// Detail is a schema path or a parse position — never a value from the frame.
	Detail string
}

// CallError is a call-shaped frame the receiver ANSWERS rather than closing on
// (#2028): a `call` or `result` over MaxCallPayloadBytes. It carries the
// callId to echo and the class — never the payload.
type CallError struct {
	Class  CallErrorClass
	CallID string
	// FrameType is `call` or `result`.
	FrameType string
}

func (e *CallError) Error() string {
	return fmt.Sprintf("wire: answering %s %s with %s", e.FrameType, e.CallID, e.Class)
}

func (e *CloseError) Error() string {
	if e.FrameType == "" {
		return fmt.Sprintf("wire: closing with %s (%s)", e.Reason, e.Detail)
	}
	return fmt.Sprintf("wire: closing with %s on a %s frame (%s)", e.Reason, e.FrameType, e.Detail)
}

var (
	compileOnce sync.Once
	compiled    map[string]*jsonschema.Schema
	compileErr  error
)

// compiledSchemas compiles every embedded schema once.
func compiledSchemas() (map[string]*jsonschema.Schema, error) {
	compileOnce.Do(func() {
		compiled, compileErr = compileSchemas()
	})
	return compiled, compileErr
}

// schemaBase is the `$id` prefix every schema declares. Resolution stays
// entirely offline: nothing is ever fetched from this URL.
const schemaBase = "https://uffici.ai/wire/v1/"

func compileSchemas() (map[string]*jsonschema.Schema, error) {
	files := contract.FS()
	names, err := contract.SchemaNames()
	if err != nil {
		return nil, err
	}

	compiler := jsonschema.NewCompiler()
	// 🔴 Registered under each schema's own `$id`, not under its bare file name.
	// A `$ref: "entry.schema.json"` resolves RELATIVE TO THE REFERRING SCHEMA'S
	// `$id`, so it becomes `https://uffici.ai/wire/v1/entry.schema.json`; a
	// resource added under the bare name is never found, and the compiler then
	// tries to FETCH the URL. That is the difference between a validator that
	// works on a laptop with no network and one that fails closed at the worst
	// possible moment.
	for _, name := range names {
		file := name + ".schema.json"
		raw, readErr := fs.ReadFile(files, "schema/"+file)
		if readErr != nil {
			return nil, fmt.Errorf("reading schema %s: %w", file, readErr)
		}
		if addErr := schemas.AddResource(compiler, schemaBase+file, raw); addErr != nil {
			return nil, addErr
		}
	}

	out := make(map[string]*jsonschema.Schema, len(names))
	for _, name := range names {
		schema, compileError := compiler.Compile(schemaBase + name + ".schema.json")
		if compileError != nil {
			return nil, fmt.Errorf("compiling schema %s: %w", name, compileError)
		}
		out[name] = schema
	}
	return out, nil
}

// Validate checks one raw frame against the contract and returns the close
// reason it earns, or nil.
//
// The order is the order the server must close in: a frame from a protocol
// version we do not speak is `unsupported_version` whatever else is wrong with
// it, because we cannot be sure the rest means what we think it means.
func Validate(raw []byte) error {
	if len(raw) > FrameMaxBytes {
		return &CloseError{Reason: ReasonMalformedFrame, Detail: "frame exceeds the byte bound"}
	}

	var envelope Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return &CloseError{Reason: ReasonMalformedFrame, Detail: "frame is not a JSON object"}
	}
	if envelope.V != Version {
		return &CloseError{
			Reason:    ReasonUnsupportedVersion,
			FrameType: envelope.Type,
			Detail:    fmt.Sprintf("v=%d, this build speaks v%d", envelope.V, Version),
		}
	}
	if envelope.Type == "call" || envelope.Type == "result" {
		if err := checkCallBound(raw, envelope.Type); err != nil {
			return err
		}
	}
	if !knownFrameType(envelope.Type) {
		// The type WAS readable — attaching it is what `CloseError`'s own doc
		// comment promises, and what the sibling branch below already does.
		return &CloseError{Reason: ReasonMalformedFrame, FrameType: envelope.Type, Detail: "unknown frame type"}
	}

	all, err := compiledSchemas()
	if err != nil {
		return fmt.Errorf("wire: the embedded contract did not compile: %w", err)
	}
	schema, ok := all[envelope.Type]
	if !ok {
		return &CloseError{Reason: ReasonMalformedFrame, FrameType: envelope.Type, Detail: "no schema for this frame type"}
	}

	doc, err := schemas.Parse(raw)
	if err != nil {
		return &CloseError{Reason: ReasonMalformedFrame, FrameType: envelope.Type, Detail: "frame is not valid JSON"}
	}
	if err := schema.Validate(doc); err != nil {
		// 🔴 Locations only — never the value at one, which would put frame
		// content (a post body, the token) into a close frame the plugin reads.
		// `schemas.SafeLocations` is the one reduction: the fixture's script
		// loader publishes its failures too, and "where" must mean the same thing
		// on both paths.
		detail := "does not match its schema"
		if locations := schemas.SafeLocations(err); len(locations) > 0 {
			detail = "schema violation at " + strings.Join(locations, ", ")
		}
		return &CloseError{Reason: ReasonMalformedFrame, FrameType: envelope.Type, Detail: detail}
	}
	return nil
}

// Decode validates a raw frame and unmarshals it into its Go type.
//
// `dir` says who received the frame, which is what tells the two `hello` shapes
// apart — the wire uses one type name for both, so nothing else can.
func Decode(raw []byte, dir Direction) (any, error) {
	if err := Validate(raw); err != nil {
		return nil, err
	}
	var envelope Envelope
	// Unmarshalled again rather than threaded through: Validate is the gate, and
	// a caller that skipped it must not get a half-checked frame from here.
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, &CloseError{Reason: ReasonMalformedFrame, Detail: "frame is not a JSON object"}
	}

	var target any
	switch envelope.Type {
	case "hello":
		if dir == FromPlugin {
			target = &PluginHello{}
		} else {
			target = &CompanionHello{}
		}
	case "brief":
		target = &Brief{}
	case "ack":
		target = &Ack{}
	case "ready":
		target = &Ready{}
	case "opener":
		target = &Opener{}
	case "post":
		target = &Post{}
	case "done":
		target = &Done{}
	case "cancel":
		target = &Cancel{}
	case "release":
		target = &Release{}
	case "call":
		target = &Call{}
	case "result":
		target = &Result{}
	default:
		return nil, &CloseError{Reason: ReasonMalformedFrame, Detail: "unknown frame type"}
	}

	if err := json.Unmarshal(raw, target); err != nil {
		return nil, &CloseError{
			Reason:    ReasonMalformedFrame,
			FrameType: envelope.Type,
			Detail:    "frame does not fit its Go type",
		}
	}
	return target, nil
}

func knownFrameType(name string) bool {
	for _, known := range FrameTypes() {
		if known == name {
			return true
		}
	}
	return false
}

// checkCallBound answers an oversized call or result with its own callId.
//
// 🔴 Before the schema, because the schema would still pass it: a byte ceiling
// is the one rule JSON Schema cannot state. And the callId must be readable —
// with nothing to echo there is nothing to answer, so that one closes.
func checkCallBound(raw []byte, frameType string) error {
	if len(raw) <= MaxCallPayloadBytes {
		return nil
	}
	var ids struct {
		CallID string `json:"callId"`
	}
	// Counted in characters, as the schema's maxLength counts them.
	if err := json.Unmarshal(raw, &ids); err != nil || ids.CallID == "" || utf8.RuneCountInString(ids.CallID) > 64 {
		return &CloseError{Reason: ReasonMalformedFrame, FrameType: frameType, Detail: "an oversized " + frameType + " with no callId to answer"}
	}
	return &CallError{Class: ClassMalformedPayload, CallID: ids.CallID, FrameType: frameType}
}
