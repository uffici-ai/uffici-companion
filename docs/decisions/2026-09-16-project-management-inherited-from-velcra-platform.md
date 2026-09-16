# Project management is inherited from velcra-platform, on the shared org board

**Date:** 2026-09-16 · **Status:** approved · **Owner call:** Miguel ·
**Related:** the velcra-platform record
[2026-09-03-issue-metadata-on-native-fields](https://github.com/uffici-ai/velcra-platform/blob/develop/docs/decisions/2026-09-03-issue-metadata-on-native-fields.md)

## What was decided

Uffici Companion tracks work the way velcra-platform does, on the same
surfaces:

- **Issues in the tracker, code and PRs here.** This repo's work is filed in
  velcra-platform — the private tracker — as sub-issues of epic
  [uffici-ai/velcra-platform#1994](https://github.com/uffici-ai/velcra-platform/issues/1994)
  (owner call, 2026-09-15, recorded in the README's *Status and tracking*).
  Branches carry the tracker's number; a PR closes its issue with the
  cross-repo `Closes uffici-ai/velcra-platform#N`. An issue filed here directly
  follows the same conventions on this repo.
- **One board for the organization.**
  [uffici-ai Project #1](https://github.com/orgs/uffici-ai/projects/1) holds
  the tracker's issues, this repo's among them. Status and Iteration are the
  board's; nothing else is.
- **Issue metadata on the issue.** The organization-level native fields —
  type (`Task` / `Bug` / `Feature`), Priority (`P0`–`P3`), Effort
  (`XS`–`XL`), Start date, Target date — and the `epic` label. The fields and
  types are org-level, so they already applied; the `epic` label was created
  in this repo too, for the day an issue is filed here.
- **The same conventions, copied into this repo.** The always-on rule set
  (`project-board`, `issue-workflow`, `issue-authoring`, `pr-workflow`,
  `github-api`, `regression-guard`, `self-improvement`, `documentation`) plus
  the path-scoped `github-workflows`; the `manage-project`, `goal` and
  `copilot-review` skills; the `issue-analyzer-planner` and `code-reviewer`
  subagents; the `fix-issue` command; and the two review workflows
  (`claude-code-review.yml`, `claude-mention.yml`).
- **Gitflow.** `develop` is the default branch; `main` is production via
  release PRs; `feat/` `fix/` `chore/` branches; Conventional Commits; squash
  merges; no force pushes.

## Why

The conventions are a body of hard-won lessons — the issue pointers in the
rule files are velcra-platform's, and they stay qualified as
`uffici-ai/velcra-platform#N` so they resolve — and the board is already where
the owner looks. Two boards or two vocabularies would mean the same session
writing the wrong one. Copying rather than linking is deliberate: a Claude
Code session loads `.claude/rules/` from the repo it is in, so a cross-repo
pointer would load nothing.

## Decisions worth recording

- **Copied, then adapted — not symlinked or vendored.** Repo names, issue
  references and stack-bound steps were rewritten; the rest is verbatim so the
  two sets stay easy to diff. Drift is expected and fine: a lesson learned here
  lands here first ([self-improvement.md](../../.claude/rules/self-improvement.md));
  one that applies to both is a PR in each.
- **Stack-bound rules stayed behind.** `architecture`, `database`, `security`,
  `logging`, `code-reuse`, `testing`, `settings` and the rest of
  velcra-platform's set describe a Next.js + Express + Postgres monorepo this
  repo is not. They come in with the toolchain decision, rewritten for a
  daemon that pins an npm protocol package — not copied ahead of it.
- **The gauntlet and the deploy phase are placeholders.** `/goal` P7 and P12
  say so in place; the guards the rules cite (instruction-set link check,
  decisions-index check, workflow YAML validity) are listed in
  [PLAN.md](../../PLAN.md) Phase 1, with velcra-platform's `scripts/__tests__/`
  as the reference implementations.
- **The review workflows are inert until `CLAUDE_CODE_OAUTH_TOKEN` is set** as
  a repository secret. They are committed first anyway, so the convention is
  in place on the first PR and the failure, when the secret is missing, is a
  visible red job rather than a silently absent review.
- **The board's `Systems` field stays unset from this repo too** — the
  velcra-platform exemption (a multi-select `gh` cannot write) applies as is.

## Scope

Tracking and workflow only. No product code, no data, no external service
beyond GitHub.

## Where the rules live

[.claude/rules/project-board.md](../../.claude/rules/project-board.md) (the
convention) and
[.claude/skills/manage-project/SKILL.md](../../.claude/skills/manage-project/SKILL.md)
(the mechanics). The human map is [docs/conventions.md](../conventions.md).
