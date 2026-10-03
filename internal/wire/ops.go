package wire

import (
	"fmt"
	"io/fs"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/uffici-ai/uffici-companion/internal/schemas"
	contract "github.com/uffici-ai/uffici-companion/wire"
)

// OpLogsPush is the call op a plugin hands its own log lines and its harness's
// over (#2264): the first op the companion serves in production.
const OpLogsPush = "logs.push"

// OpToolsList and OpToolsCall are the agent's Uffici tools (#2349): the catalog
// for one conversation, and one call on the request being answered.
const (
	OpToolsList = "tools.list"
	OpToolsCall = "tools.call"
)

// Op payload directions.
const (
	OpRequest = "request"
	OpResult  = "result"
)

// opSchemaBase is the `$id` prefix every op schema declares.
const opSchemaBase = schemaBase + "ops/"

var (
	opsOnce     sync.Once
	opsCompiled map[string]*jsonschema.Schema
	opsErr      error
)

// OpSchemaNames lists the embedded op schemas as `<op>.<direction>`.
func OpSchemaNames() ([]string, error) {
	entries, err := fs.ReadDir(contract.FS(), "ops")
	if err != nil {
		return nil, fmt.Errorf("reading ops/: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, strings.TrimSuffix(entry.Name(), ".schema.json"))
	}
	return names, nil
}

func compileOps() (map[string]*jsonschema.Schema, error) {
	names, err := OpSchemaNames()
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	for _, name := range names {
		raw, readErr := fs.ReadFile(contract.FS(), "ops/"+name+".schema.json")
		if readErr != nil {
			return nil, fmt.Errorf("reading op schema %s: %w", name, readErr)
		}
		if addErr := schemas.AddResource(compiler, opSchemaBase+name+".schema.json", raw); addErr != nil {
			return nil, addErr
		}
	}
	out := make(map[string]*jsonschema.Schema, len(names))
	for _, name := range names {
		schema, compileErr := compiler.Compile(opSchemaBase + name + ".schema.json")
		if compileErr != nil {
			return nil, fmt.Errorf("compiling op schema %s: %w", name, compileErr)
		}
		out[name] = schema
	}
	return out, nil
}

// ValidateOpPayload checks one op payload against its schema. The error names
// only WHERE the payload failed (schemas.SafeLocations), never what it held:
// a logs.push payload is a harness's log lines.
func ValidateOpPayload(op, direction string, raw []byte) error {
	opsOnce.Do(func() { opsCompiled, opsErr = compileOps() })
	if opsErr != nil {
		return opsErr
	}
	schema, ok := opsCompiled[op+"."+direction]
	if !ok {
		return fmt.Errorf("no %s schema for op %q", direction, op)
	}
	instance, err := schemas.Parse(raw)
	if err != nil {
		return fmt.Errorf("the %s payload is not JSON", direction)
	}
	if err := schema.Validate(instance); err != nil {
		return fmt.Errorf("the %s payload failed its schema at %s", direction, strings.Join(schemas.SafeLocations(err), ", "))
	}
	return nil
}
