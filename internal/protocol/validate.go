package protocol

import (
	"fmt"
	"io/fs"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/uffici-ai/uffici-companion/internal/schemas"
	contract "github.com/uffici-ai/uffici-companion/protocol"
)

// schemaBase is the `$id` prefix every emitted schema declares. Nothing is ever
// fetched from it: every schema is registered under its own `$id` first.
const schemaBase = "https://uffici.ai/schemas/agents/v1/"

var (
	compileOnce sync.Once
	compiled    map[string]*jsonschema.Schema
	compileErr  error
)

// Validate checks a raw JSON document against one of the contract's schemas,
// by name (`ring`, `post-request`, …).
//
// It is the companion's test double's gate and the goldens test's: the double
// refuses every body the real gateway would refuse, so a companion that passes
// against it cannot be sending a field the platform rejects. The error names
// WHERE a document failed, never what it held — a post body or a ticket may be
// in it.
func Validate(name string, raw []byte) error {
	compiledOnce, err := compiledSchemas()
	if err != nil {
		return err
	}
	schema, ok := compiledOnce[name]
	if !ok {
		return fmt.Errorf("protocol: no schema named %q", name)
	}
	doc, err := schemas.Parse(raw)
	if err != nil {
		return fmt.Errorf("protocol: %s is not JSON", name)
	}
	if err := schema.Validate(doc); err != nil {
		return fmt.Errorf("protocol: %s refused at %s", name, strings.Join(schemas.SafeLocations(err), ", "))
	}
	return nil
}

func compiledSchemas() (map[string]*jsonschema.Schema, error) {
	compileOnce.Do(func() { compiled, compileErr = compileSchemas() })
	return compiled, compileErr
}

func compileSchemas() (map[string]*jsonschema.Schema, error) {
	manifest, err := contract.LoadManifest()
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	for _, name := range manifest.Schemas {
		file := name + ".schema.json"
		raw, readErr := fs.ReadFile(contract.FS(), "schema/"+file)
		if readErr != nil {
			return nil, fmt.Errorf("reading schema %s: %w", file, readErr)
		}
		if addErr := schemas.AddResource(compiler, schemaBase+file, raw); addErr != nil {
			return nil, addErr
		}
	}
	out := make(map[string]*jsonschema.Schema, len(manifest.Schemas))
	for _, name := range manifest.Schemas {
		schema, compileErr := compiler.Compile(schemaBase + name + ".schema.json")
		if compileErr != nil {
			return nil, fmt.Errorf("compiling schema %s: %w", name, compileErr)
		}
		out[name] = schema
	}
	return out, nil
}
