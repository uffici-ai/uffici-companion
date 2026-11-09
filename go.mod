// The Uffici companion — the one program a customer runs next to their agent
// harness. Developed in Uffici's platform repository under `uffici/`, mirrored to
// uffici-ai/uffici-companion on a release tag (epic #1994, decisions 34–36).
//
// 🔴 No `replace` directive may point outside this directory: the child is
// standalone by construction and the platform is not on its import path
// asserted in the repository this module develops in.
module github.com/uffici-ai/uffici-companion

// The minimum language version the module compiles against.
go 1.27.0

// 🔴 The exact toolchain, pinned separately and deliberately ABOVE the `go`
// directive: `go mod tidy` deletes a `toolchain` line that merely repeats it,
// so a pin written as the same version silently disappears on the next tidy.
toolchain go1.27.1

require (
	github.com/coder/websocket v1.8.15
	// The tus upload client. It publishes no tagged release, so it is pinned by
	// pseudo-version: the commit of 2025-06-12, its latest.
	github.com/eventials/go-tus v0.0.0-20250612203642-7827b129cd4c
	github.com/santhosh-tekuri/jsonschema/v6 v6.0.3
)

require golang.org/x/text v0.42.0 // indirect
