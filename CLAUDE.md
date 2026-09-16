# CLAUDE.md — Uffici Companion

The map for Claude Code sessions. **[.claude/rules/](.claude/rules/) is the law**:
the always-on set loads into every session; path-scoped rules load when a matching
file is read or edited — **read a scoped rule on demand** whenever you work in its
area (index at the end). Decision records in [docs/decisions/](docs/decisions/README.md);
the human map of the conventions in [docs/conventions.md](docs/conventions.md).

## Project

**Uffici Companion** is the one program a customer runs next to their AI-agent
harness (Hermes, OpenClaw, …) to connect it to **Uffici** — Uffici.AI's office for
AI agents, built as the Velcra Platform in
[uffici-ai/velcra-platform](https://github.com/uffici-ai/velcra-platform). A small
daemon, or a sidecar in the harness's pod, that owns the agent key, the doorbell,
the tickets and the upstream MCP sessions, and exposes two loopback surfaces so each
`uffici-for-<harness>` plugin (its own repo) stays a thin translator with no network
code. **[README.md](README.md) is the design — read it first.** Nothing is
implemented yet. <https://github.com/uffici-ai/uffici-companion> · Owner Miguel
(MiguelTVMS) · Apache-2.0, open source by design: the customer can read what it does
with their key.

**Brand:** Uffici.AI is the company; Velcra is the platform, never a company name.
Public hosts live under `uffici.ai` / `uffici.dev`.

**Status:** design decided 2026-09-15, tracked as epic
[uffici-ai/velcra-platform#1994](https://github.com/uffici-ai/velcra-platform/issues/1994)
under the harness-agnostic platform epic #1958 — the items still to confirm are
listed in README → *Status and tracking*. Toolchain and delivery pipeline are not
decided — [PLAN.md](PLAN.md) carries the phases; the toolchain decision lands the
stack-bound rule files.

## Repository structure

```text
README.md            the design — what the companion owns, the two local surfaces, the plugins
.claude/rules/       the conventions (this file is the map)
.claude/skills/      goal · manage-project · copilot-review
.claude/agents/      issue-analyzer-planner · code-reviewer
.claude/commands/    fix-issue
.github/workflows/   claude-code-review.yml (first push) · claude-mention.yml (@claude)
docs/                conventions, decisions/, defect-ledger; the local wire + pairing flow come with the code
src/ fixture/ packaging/   planned (README → Planned repository layout), not yet created
```

## Conventions

- **Git (Gitflow)**: `develop` (default) ← `feat/…` `fix/…` `chore/…`; `main` = production via release PRs; Conventional Commits; squash merges; no force pushes. PRs: [pr-workflow.md](.claude/rules/pr-workflow.md); discovered defects get an issue first: [issue-workflow.md](.claude/rules/issue-workflow.md); planned work is authored per [issue-authoring.md](.claude/rules/issue-authoring.md); `/goal implement #<n>` runs an issue or epic end to end ([goal](.claude/skills/goal/SKILL.md)); `gh` has two hourly budgets ([github-api.md](.claude/rules/github-api.md)).
- **Tracking**: this repo's issues are filed in velcra-platform, the private tracker, as sub-issues of epic #1994; a PR here closes its issue cross-repo (`Closes uffici-ai/velcra-platform#N`) and branches carry the tracker's number. GitHub Project [uffici-ai #1](https://github.com/orgs/uffici-ai/projects/1) is the source of truth for where work stands; the issue's native type / Priority / Effort for what it is ([project-board.md](.claude/rules/project-board.md), [manage-project](.claude/skills/manage-project/SKILL.md)).
- **Every fix ships a guard that fails without it**, mutation-checked ([regression-guard.md](.claude/rules/regression-guard.md); recurred classes: [docs/defect-ledger.md](docs/defect-ledger.md)).
- **Docs move in the same commit as the change**; locked decisions get a record ([documentation.md](.claude/rules/documentation.md)). A lesson learned becomes a rule, preferably a failing check ([self-improvement.md](.claude/rules/self-improvement.md)).
- **Local gauntlet before push**: 🔴 not yet defined — it arrives with the toolchain ([PLAN.md](PLAN.md), Phase 1) and this line then names the chain. Until then, run whatever CI runs, locally, before pushing. Never `--no-verify`.

## Rule index

**Always-on** (every session): `pr-workflow`, `issue-workflow`, `issue-authoring`,
`project-board`, `github-api`, `regression-guard`, `documentation`,
`self-improvement`.

| Rule | Loads for |
|---|---|
| [github-workflows.md](.claude/rules/github-workflows.md) | `.github/workflows/**` |

Stack-bound rules (architecture, security, logging, testing, code reuse, settings,
dependencies …) are written with the stack, for this stack — not copied ahead of it.

## Working style

- Lead with the outcome; add only the detail that changes what the reader does next. Don't re-narrate rules, history or the diff.
- A defect found while doing anything else gets an issue, then a fix with its guard — never a silent inline fix.
- Rules and docs move in the same commit as the change they describe. Update this map when an area, skill or rule is added, removed or re-scoped; record locked decisions in `docs/decisions/`; reflect issue changes on Project #1; keep [PLAN.md](PLAN.md) current for non-trivial changes.
