// Package protocol ships the platform-to-companion contract as files: the JSON
// Schemas, the goldens and the manifest the platform's agents-gateway emits.
//
// 🔴 `v1/` is a BYTE-IDENTICAL copy of what the platform emits from its zod
// source (#2067), pinned from the platform repository's root. It is never edited
// here: a change is made on the platform, emitted, and copied. The hand-written
// Go structs in `internal/protocol` are pinned against these files in both
// directions, which is why they may be hand-written at all.
//
// A package of its own, at the directory the contract lives in, because
// `//go:embed` cannot reach above its own package — the local wire's precedent
// (`wire/`).
package protocol

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
)

// Version is the protocol version this package carries.
const Version = 1

//go:embed v1/schema/*.json v1/goldens v1/MANIFEST.json
var files embed.FS

// FS is the contract's files, rooted at `v1/`.
func FS() fs.FS {
	sub, err := fs.Sub(files, "v1")
	if err != nil {
		// Unreachable: the embed pattern above fixes the layout at compile time.
		panic(fmt.Sprintf("protocol: embedded contract is missing v1/: %v", err))
	}
	return sub
}

// Manifest is the platform's statement of what the contract contains.
type Manifest struct {
	Version           int            `json:"version"`
	Schemas           []string       `json:"schemas"`
	Reserved          []string       `json:"reserved"`
	ErrorClasses      []string       `json:"errorClasses"`
	CancelReasons     []string       `json:"cancelReasons"`
	DisconnectReasons []string       `json:"disconnectReasons"`
	ValidGoldens      map[string]int `json:"validGoldens"`
	InvalidGoldens    map[string]int `json:"invalidGoldens"`
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
