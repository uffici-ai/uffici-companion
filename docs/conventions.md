# Conventions

How we work in this repo. The **canonical conventions live in
[.claude/rules/](../.claude/rules/)** — one file per topic. An always-on set
loads into every AI-agent session; the rest carry a `paths:` frontmatter and
load only when a matching file is touched (the index in
[CLAUDE.md](../CLAUDE.md#rule-index) says which is which). This page is the
human map over that set: what exists, where to look, and the gates that
enforce it. It deliberately does not restate the rules — when this page and a
rule file disagree, the rule file wins and this page has a bug.

[AGENT.md](../AGENT.md) is the equivalent entry point for AI coding agents of
any provider; `.github/copilot-instructions.md` points at it.

The set was inherited from velcra-platform on 2026-09-16
([record](decisions/2026-09-16-project-management-inherited-from-velcra-platform.md)):
the workflow conventions came over whole; the stack-bound ones come with the
stack.

## Non-negotiable invariants

The invariants most likely to bite a new contributor, each with its rule:

- **Discovered defects get a tracked issue first**, then a fix — even a
  one-liner ([issue-workflow.md](../.claude/rules/issue-workflow.md)).
- **Every bug fix ships a mechanical guard that fails without it**,
  mutation-checked ([regression-guard.md](../.claude/rules/regression-guard.md);
  recurred classes: [defect-ledger.md](defect-ledger.md)).
- **Documentation moves in the same commit as the change** — decisions
  recorded as they are taken, conventions docs updated with what they describe
  ([documentation.md](../.claude/rules/documentation.md)).
- **The board is the source of truth for where work stands; the issue for what
  it is** ([project-board.md](../.claude/rules/project-board.md)).
- **A lesson learned becomes a rule, preferably a failing check**
  ([self-improvement.md](../.claude/rules/self-improvement.md)).

## Rule map

Grouped by what you are doing; every file lives in
[.claude/rules/](../.claude/rules/).

| Doing | Read |
|---|---|
| Anything at all (always-on: the cross-cutting obligations) | `regression-guard.md`, `documentation.md`, `self-improvement.md` |
| PRs, issues, the project board (always-on) | `pr-workflow.md`, `issue-workflow.md`, `issue-authoring.md`, `project-board.md`, `github-api.md` |
| GitHub workflows | `github-workflows.md` |

Stack-bound rules — architecture, security, logging, testing, code reuse,
settings, dependencies — are written with the stack decision, for this stack,
not copied ahead of it ([PLAN.md](../PLAN.md), Phase 1).

## Quality gates

**None mechanical yet.** The pre-push gauntlet, lint, spellcheck and the guards
the rules cite (instruction-set links resolve; every decision record is in the
index; every workflow file is valid YAML) arrive with the toolchain. The target
shape is velcra-platform's: one `&&` chain the pre-push hook runs and CI
repeats, with `typecheck → test → lint → lint:md → spellcheck → …` as the spine.
Until then: run whatever CI runs, locally, before pushing, and never bypass a
hook with `--no-verify` — fix the underlying issue.

## Git + delivery workflow

Gitflow ([CLAUDE.md](../CLAUDE.md#conventions)): `develop` is the default
branch; `main` is production via release PRs. Feature branches from `develop`
(`feat/…`, `fix/…`, `chore/…`), Conventional Commits, squash merges, no force
pushes to shared branches.

- **PRs** ([pr-workflow.md](../.claude/rules/pr-workflow.md)): the first push
  gets an automatic deep review; every later push gets exactly one `@claude`
  re-review mention. Every push costs a full CI round — batch fixes. 🔴 A PR
  opened **with conflicts** gets no workflow runs at all — no CI, no review —
  and rebasing afterwards cannot recover the automatic pass.
- **Issues** ([issue-workflow.md](../.claude/rules/issue-workflow.md),
  [issue-authoring.md](../.claude/rules/issue-authoring.md)): defects found
  while doing anything become tracked issues before fixes — even one-liners;
  epics are written for humans, sub-issues as contracts for agents. The
  `/goal` skill runs an issue end-to-end (branch → TDD → gauntlet → PR →
  review loop → merge → board close-out).
- **Board** ([project-board.md](../.claude/rules/project-board.md)):
  [uffici-ai Project #1](https://github.com/orgs/uffici-ai/projects/1) is the
  source of truth; every issue created/closed/re-scoped is reflected there.
  It is the organization's one board, and the native issue fields are
  org-level. This repo's issues are filed in velcra-platform, the private
  tracker, as sub-issues of epic #1994; PRs here close them cross-repo.
- **Deploys**: no pipeline yet. When one lands, `/goal` P12 names it.

## Working across Claude accounts

Everything an agent session needs is in-repo, so any contributor's Claude
behaves the same way: [CLAUDE.md](../CLAUDE.md) is the map,
[.claude/rules/](../.claude/rules/) is the law, and repo skills live under
`.claude/skills/` (`goal`, `manage-project`, `copilot-review`). Two
consequences:

- **Durable knowledge belongs in the repo** — a rule file, a subsystem doc,
  or a [decision record](decisions/README.md) — never in one person's
  session memory, which no other account can see.
- **Rules are code-reviewed like code**: update the matching rule file in
  the same commit as the behavior it describes
  ([self-improvement.md](../.claude/rules/self-improvement.md)), and prefer
  a mechanical guard over prose whenever one can exist.
