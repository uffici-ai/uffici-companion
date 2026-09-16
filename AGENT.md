# AGENT.md — Uffici Companion

Instructions for AI coding agents working in this repository, whatever the
provider. Canonical: this file; `.github/copilot-instructions.md` points here.
Paths are repository-relative.

The project instructions live in **`CLAUDE.md`** (map, conventions, the rule
index) and **`.claude/rules/`** (the conventions). Every agent reads and follows
them; this file only points and collects quick reference.

## Read first

1. `README.md` — the design: what the companion owns, the two local surfaces, the plugins, the security posture
2. `CLAUDE.md` — overview, conventions, the rule index
3. `.claude/rules/issue-workflow.md` + `issue-authoring.md` — a defect gets an issue before a fix; how epics and sub-issues are written
4. `.claude/rules/pr-workflow.md` — `Closes uffici-ai/velcra-platform#N` per line; one `@claude` mention per push after the first
5. `.claude/rules/project-board.md` — Project #1 Status, the native issue fields, the `epic` label, and where the issues live
6. `.claude/rules/regression-guard.md` — every fix ships a guard that fails without it

## Rule files and when they apply

Always active: `pr-workflow`, `issue-workflow`, `issue-authoring`,
`project-board`, `github-api`, `regression-guard`, `documentation`,
`self-improvement`. Path-scoped: `github-workflows.md` for
`.github/workflows/**`. Stack-bound rules arrive with the stack (`PLAN.md`,
Phase 1).

## Tracking

This repo's issues are filed in velcra-platform (the private tracker), as
sub-issues of epic `uffici-ai/velcra-platform#1994`; PRs here close them
cross-repo. [uffici-ai Project #1](https://github.com/orgs/uffici-ai/projects/1)
is the source of truth for where work stands — it is the organization's one
board. Every issue carries a native type, Priority and Effort; epics carry the
`epic` label. Mechanics: `.claude/skills/manage-project/SKILL.md`.

## Local tooling

Not yet defined — the gauntlet, lint and spellcheck arrive with the toolchain
(`PLAN.md`, Phase 1). Until then, run what CI runs before pushing. Never
`--no-verify`.

Update `CLAUDE.md` / `.claude/rules/` first, then this file.
