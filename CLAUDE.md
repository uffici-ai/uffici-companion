# CLAUDE.md — Uffici Companion

The companion is the one program a customer runs next to their AI-agent harness
to connect it to Uffici. It holds the agent's key and every connection to Uffici,
so the plugin for a given harness stays a thin translator with no network code of
its own. [README.md](README.md) is the document to read first: it says what the
program does, how to install, pair, run and verify it, and what it does with the key.

## This repository receives releases; it is not where the work happens

Uffici.AI develops the companion in its own platform repository and publishes each
release here as a single squashed commit, tagged — no history, nothing
force-pushed. There are no contribution rules, review workflows or plans here, and
a pull request opened here has no reader. To report a problem or ask for a change,
contact Uffici.AI through your account or [uffici.ai](https://uffici.ai).

## Check

One entry, in this module's own toolchain. It is what CI runs here:

```sh
go vet ./... && golangci-lint run && go test -coverpkg=./... -coverprofile=cover.out ./... && go run ./internal/covercheck -min 90 cover.out
```

Requires Go (the version pinned in [go.mod](go.mod)) and
[golangci-lint](https://golangci-lint.run). The Markdown link guard runs inside
`go test` — this module depends on no other toolchain, by design.

The documentation is checked in a job of its own,
[.github/workflows/lint-docs.yml](.github/workflows/lint-docs.yml): Markdown lint
and spelling against [.markdownlint-cli2.jsonc](.markdownlint-cli2.jsonc) and
[cspell.json](cspell.json), with Node and the two tools installed by exact version
in the job, so the check above stays Go only.

## Layout

- `install.sh` — the one-line installer: fetches a release archive, verifies it, installs the binary and hands over to `uffici setup`. Run by the tests against a loopback release.
- `.goreleaser.yaml` — the release: three targets, version-less archive names, checksums, keyless signatures, SBOMs. `.github/workflows/release.yml` runs it on a tag and publishes the image.
- `cmd/uffici/` — the command: the daemon (`run`), `pair`, `doctor` and `setup`.
- `wire/v1/` — **the local wire's contract**: JSON Schemas, golden examples and the manifest that counts them. Embedded into the binary, so validation never depends on files beside it. Specified in `docs/local-wire.md`; where the two disagree, the schemas win.
- `fixture/` — **the scripted stand-in for Uffici**: its schema and an acceptance script, embedded for the same reason. Specified in `docs/fixture-script.md`.
- `protocol/v1/` — **the Uffici-to-companion contract**, a byte-identical copy of what Uffici's agents gateway publishes; never edited here. `internal/protocol/` holds its Go structs, pinned to it both ways.
- `internal/infra/gateway/` — the real agents host (the doorbell and the JSON control plane); `gatewaytest/` its in-process double, which validates every body against `protocol/v1`.
- `internal/wire/` — the frame types and the schema-validating decoder.
- `internal/contracts/` — the interfaces; `internal/infra/<backend>/` the implementations; `internal/registry/` selects one. Application code takes a contract, never an implementation.
- `internal/schemas/` — the one JSON-Schema seam: registering an embedded document under its `$id`, and reducing a validation failure to safe locations.
- `internal/requests/` — the request state machine; `internal/surface/` the local wire server; `internal/observe/` the one-line-per-frame log; `internal/runner/` the script timeline; `internal/pairing/` the sealed key exchange.
- `internal/setup/` — `uffici setup`: the service, the pairing, the plugin fetched from its own release and installed by its manifest (`manifest.schema.json`, refusing unknown fields), the doctor; every side effect injected, a `--dry-run` that only tells.
- `internal/redact/` — removes credential-shaped text from a log line: `patterns.json` (valid in RE2 and ECMAScript, because Uffici applies the same file a second time) and `testdata/vectors.json`, which both implementations must pass.
- `internal/logship/` — log lines on their way to Uffici: a tee beside stdout, a bounded ring that drops the oldest line rather than block, the plugin's `logs.push` op, and the shipper — its own connection, only while the control plane is idle, one push at a time.
- `internal/infra/logsim/` — the stand-in hosts' log side: a scriptable policy and every pushed batch recorded, which a `--once` run reports.
- `internal/build/` — the identity a release stamps into the binary.
- `internal/docs/` — the Markdown link guard.

The plugin repositories carry a **byte-identical copy** of `wire/v1/`; the copies are
held equal at the source.

## License

Apache-2.0 ([LICENSE](LICENSE), [NOTICE](NOTICE)).
