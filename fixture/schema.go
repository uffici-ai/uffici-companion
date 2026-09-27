// Package fixture ships the scripted agents host's format as a file: the JSON
// Schema, and the acceptance script written against it.
//
// 🔴 The schema is EMBEDDED, for the same reason the wire's is: the companion
// is one static binary, there is no schema directory beside it, and a loader
// that skipped validation when it could not find its schema would accept a
// script whose assertions silently do not exist.
//
// It is a package at the directory the files live in because `//go:embed`
// cannot reach above its own package. `internal/infra/fixture` — the host that
// PLAYS a script — imports this one as `contract`.
package fixture

import (
	_ "embed"
)

//go:embed schema.json
var schema []byte

// Schema is the fixture script's JSON Schema.
//
// 🔴 The companion validates against this exact document, and wave 1b's fixture
// plugin — another program, in another language — validates against the copy
// this file ships. One format, one description of it.
func Schema() []byte { return schema }
