# Uffici Companion — Implementation Plan

## Context

**Problem:** Uffici runs no agent harness of its own — a customer brings the
one they already use (Hermes, OpenClaw, …) — and that harness needs a
connection to the platform that a per-harness plugin cannot safely own: the
agent key, the doorbell, the tickets, the retries.

**Solution:** the companion — one daemon or sidecar next to the harness that
holds every connection and every secret and exposes two loopback surfaces (an
MCP endpoint and a ring feed), so each `uffici-for-<harness>` plugin is a format
translator with no network code of its own. [README.md](README.md) is the
design: what it owns, the two surfaces, conversation binding, context and
memory, running it, the plugins, the security posture.

**Key decisions so far:**

- **The design**, decided with the owner on 2026-09-15 — epic
  [uffici-ai/velcra-platform#1994](https://github.com/uffici-ai/velcra-platform/issues/1994)
  under the harness-agnostic platform epic #1958. The items still to confirm
  at breakdown are listed in README → *Status and tracking*.
- **Issues are filed in velcra-platform**, the private tracker; this repo
  carries code and PRs, and a PR closes its issue cross-repo
  ([record](docs/decisions/2026-09-16-project-management-inherited-from-velcra-platform.md)).
- **Project management is inherited from velcra-platform**, on the shared org
  board (same record).
- Gitflow, Conventional Commits, squash merges ([CLAUDE.md](CLAUDE.md)).
- **The protocol package** (doorbell envelopes, ticket claims, tool schemas)
  is developed in the platform repository and published as a public npm package
  this repository pins — which points at a Node/TypeScript daemon, but the
  toolchain is Phase 1's call, not this line's.

## Phased delivery

### Phase 0 — Repo scaffolding: project management ✅ 2026-09-16

The conventions, skills, subagents, review workflows and docs skeleton — see
the record above. Left for the owner: the `CLAUDE_CODE_OAUTH_TOKEN` repository
secret (the review workflows are inert without it), and optionally the
`CLAUDE_CODE_PR_MODEL` / `CLAUDE_CODE_MENTION_MODEL` repository variables
(the workflows default to `claude-sonnet-5`).

### Phase 1 — Stack decision and toolchain

- The toolchain decision: a record in `docs/decisions/`, `docs/architecture.md`
  (the README's diagram and surfaces, as living authority), the planned layout
  (`src/`, `fixture/`, `packaging/`, `docs/` — README → *Planned repository
  layout*), and the stack-bound rule files (`architecture`, `security`,
  `logging`, `testing`, `code-reuse`, `settings`, …) written for this stack.
- The local gauntlet (`typecheck → test → lint → lint:md → spellcheck → …`),
  the pre-push hook and a `run-gauntlet` skill; CLAUDE.md's "Local gauntlet
  before push" line then names the chain, and `/goal` P7 stops being a
  placeholder.
- The mechanical guards the rules already cite: every relative link in the
  instruction set resolves; every decision record is in the index and named
  `YYYY-MM-DD-<slug>.md`; every workflow file is valid YAML; a job's
  `additional_permissions` never exceeds its `permissions`. velcra-platform's
  `scripts/__tests__/` has the reference implementations.
- `pr.yml` — the CI the review workflows and `pr-workflow.md` assume.

### Phase 2 — The companion (epic #1994)

The daemon — doorbell, tickets, upstream sessions, the two local surfaces, the
doctor, the fixture mode, pairing, the installers and the sidecar image — in
the slices the epic lists, filed as its sub-issues in velcra-platform
([issue-authoring.md](.claude/rules/issue-authoring.md)) and run with `/goal`.
The `uffici-for-hermes` and `uffici-for-openclaw` plugins follow in their own
repositories, Claude Code after. When a release pipeline lands (signed,
reproducible builds per the README), `/goal` P12 names it.
