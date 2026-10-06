package protocol

// The companion's ONE spelling of the platform's URN prefix (#2386). The platform's
// codec is `@velcra/field-types`' urn.ts, which this tree cannot import (a child
// references nothing outside itself); the scripted stand-ins for the platform mint
// the URNs the platform would, and do it here alone. Pinned to the codec's prefix
// from the platform's root by `scripts/__tests__/no-second-urn-codec.test.mjs`.
const urnPrefix = "urn:uffici:"

// FactURN is a remembered fact's URN.
func FactURN(id string) string { return urnPrefix + "fact:" + id }

// RequestURN is an answer request's URN — what a forget with no replacement is
// superseded by.
func RequestURN(id string) string { return urnPrefix + "request:" + id }

// ToolCallURN is a ledgered tool call's URN — what a re-run names (#2454).
func ToolCallURN(id string) string { return urnPrefix + "tool_call:" + id }
