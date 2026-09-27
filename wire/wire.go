// Package wire ships the local wire's contract as files: the JSON Schemas, the
// golden examples and the manifest that counts them.
//
// 🔴 The schemas are EMBEDDED rather than read from disk. The companion is a
// single static binary a customer drops on a laptop or into a scratch container
// — there is no schema directory beside it, and a validator that silently
// skipped validation when it could not find its schemas would be worse than no
// validator at all.
//
// It is a package of its own, at the directory the contract lives in, because
// `//go:embed` cannot reach above its own package. `internal/wire` imports it
// as `contract`.
package wire

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"
)

// Version is the protocol version this package carries. A shape change is a new
// version directory, never an edit of v1 in place.
const Version = 1

//go:embed v1/schema/*.json v1/golden/valid/*.json v1/golden/invalid/*.json v1/MANIFEST.json
var files embed.FS

// FS is the contract's files, rooted at `v1/`.
func FS() fs.FS {
	sub, err := fs.Sub(files, "v1")
	if err != nil {
		// Unreachable: the embed pattern above fixes the layout at compile time.
		panic(fmt.Sprintf("wire: embedded contract is missing v1/: %v", err))
	}
	return sub
}

// Manifest is the committed expectation for the contract's files.
type Manifest struct {
	Version                     int            `json:"version"`
	Schemas                     []string       `json:"schemas"`
	ValidGoldensByFrame         map[string]int `json:"validGoldensByFrame"`
	InvalidGoldensByCloseReason map[string]int `json:"invalidGoldensByCloseReason"`
	// InvalidGoldensByCallDisposition counts the call tunnel's invalid goldens
	// that are ANSWERED or DISCARDED rather than closed (#2028).
	InvalidGoldensByCallDisposition map[string]int `json:"invalidGoldensByCallDisposition"`
	Totals                          struct {
		Schemas int `json:"schemas"`
		Valid   int `json:"valid"`
		Invalid int `json:"invalid"`
	} `json:"totals"`
}

// LoadManifest reads the embedded MANIFEST.json.
func LoadManifest() (Manifest, error) {
	var m Manifest
	raw, err := fs.ReadFile(FS(), "MANIFEST.json")
	if err != nil {
		return m, fmt.Errorf("reading MANIFEST.json: %w", err)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, fmt.Errorf("parsing MANIFEST.json: %w", err)
	}
	return m, nil
}

// SchemaNames lists the embedded schemas, without the `.schema.json` suffix.
func SchemaNames() ([]string, error) {
	entries, err := fs.ReadDir(FS(), "schema")
	if err != nil {
		return nil, fmt.Errorf("reading schema/: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, strings.TrimSuffix(entry.Name(), ".schema.json"))
	}
	return names, nil
}
