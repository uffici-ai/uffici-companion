# CLAUDE.md — Uffici Companion

The companion is the one program a customer runs next to their AI-agent harness
to connect it to Uffici. It holds every connection and every secret, so the
plugin for a given harness stays a thin translator with no network code of its
own. [README.md](README.md) is the design.

## This repository receives releases; it is not where the work happens

Development happens in the Uffici platform repository, in the `uffici/` tree.
A release dispatched there exports this module's subtree here as a single squashed
commit and tags it — no history, nothing force-pushed. So there are no rules, skills,
agents or review workflows here, and a pull request opened here has no reader:
file the issue and the change in the platform repository instead.

## Check

One entry, in this module's own toolchain. It is what CI runs and what the
platform's `pnpm check:uffici` delegates to:

```sh
go vet ./... && golangci-lint run && go test -coverpkg=./... -coverprofile=cover.out ./... && go run ./internal/covercheck -min 90 cover.out
```

Requires Go (the version pinned in [go.mod](go.mod)) and
[golangci-lint](https://golangci-lint.run). The Markdown link guard runs inside
`go test` — this module depends on no other toolchain, by design.

## Layout

- `cmd/uffici/` — the command: the daemon and its local clients.
- `wire/v1/` — **the local wire's contract**: JSON Schemas, golden examples and the manifest that counts them. Embedded into the binary, so validation never depends on files beside it.
- `fixture/` — **the scripted agents host's contract**: `schema.json` and the acceptance script `basic.json`. Embedded for the same reason, and shared with wave 1b's smoke plugin, which plays the same format in another language.
- `docs/local-wire.md` — the wire's specification, and `docs/fixture-script.md` the script format's. Where either and the schemas disagree, the schemas win.
- `protocol/v1/` — **the platform-to-companion contract**, a byte-identical copy of what the platform's agents-gateway emits; never edited here. `internal/protocol/` holds its Go structs, pinned to it both ways.
- `internal/infra/gateway/` — the real agents host (doorbell + JSON control plane); `gatewaytest/` its in-process double, which validates every body against `protocol/v1`.
- `internal/wire/` — the frame types and the schema-validating decoder.
- `internal/contracts/` — the interfaces; `internal/infra/<backend>/` the implementations; `internal/registry/` selects one. Application code takes a contract, never an implementation.
- `internal/schemas/` — the one JSON-Schema seam: registering an embedded document under its `$id`, and reducing a validation failure to safe locations. Both the wire and the fixture publish their refusals, so "where" means one thing.
- `internal/requests/` — the request state machine; `internal/surface/` the local wire server; `internal/observe/` the one-line-per-frame log; `internal/runner/` the script timeline.
- `internal/build/` — the identity a release stamps into the binary.
- `internal/docs/` — the Markdown link guard.

🔴 The plugin children carry a **byte-identical copy** of `wire/v1/`. Edit this
one; the platform's parity guard fails until the copies match.

## License

Apache-2.0 ([LICENSE](LICENSE), [NOTICE](NOTICE)).
