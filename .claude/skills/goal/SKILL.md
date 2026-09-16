---
name: goal
description: Run a GitHub issue — or a whole epic — end to end: read, branch, TDD + guard, gauntlet, PR, review loop, merge, deploy, CI watch, board close-out.
disable-model-invocation: true
argument-hint: "[implement|plan|continue] #<issue-or-epic>"
---

# Goal

One deterministic path from a tracked issue to merged, deployed and green.

This skill **orchestrates what already exists** — it does not restate it. Every
phase delegates to the rule, skill or subagent that owns that step, and the
value is the sequencing plus the things that get dropped when there is no
sequence: the board move at branch creation, the mutation-check on the guard,
the `@claude` mention after each fix push, and watching what happens afterwards.

| Step | Owner |
|---|---|
| Issue + board conventions | [issue-workflow.md](../../rules/issue-workflow.md), [project-board.md](../../rules/project-board.md) |
| Board mechanics | [manage-project](../manage-project/SKILL.md) |
| Analysis written back to the issue | `issue-analyzer-planner` subagent |
| Test-first, guards, mutation-check | [regression-guard.md](../../rules/regression-guard.md), [defect-ledger.md](../../../docs/defect-ledger.md) |
| Self-review dimensions | `code-reviewer` subagent |
| Pre-push chain | the local gauntlet named in [CLAUDE.md](../../../CLAUDE.md) → Conventions — 🔴 not yet defined for this repo ([PLAN.md](../../../PLAN.md), Phase 1) |
| PR + review loop | [pr-workflow.md](../../rules/pr-workflow.md), [copilot-review](../copilot-review/SKILL.md) |
| Deploying and verifying | 🔴 no deploy pipeline yet — P12 below says what to do until there is one |
| `gh` budget | [github-api.md](../../rules/github-api.md) |
| Docs and decision records | [documentation.md](../../rules/documentation.md) |

## `$ARGUMENTS`

`implement` is the default, so `/goal #42` and `/goal implement #42` are the
same run.

- **`implement #N`** — the full loop, P0 → P13.
- **`plan #N`** — P0–P3 only, then stop. On an epic this produces the wave
  plan. Delegate the write-back to the `issue-analyzer-planner` subagent. This
  is the explicit opt-in for when the design is *not* settled yet.
- **`continue [#N]`** — re-derive the current phase from `git`, `gh pr view` and
  the board, then resume. Works mid-epic. **No state file**: the branch, the PR
  and the board item *are* the state, and a file would go stale.

A bare `#N` is a **velcra-platform** number — this repo's issues live in that
tracker ([project-board.md](../../rules/project-board.md)); write
`uffici-companion#N` or paste the URL for an issue that lives here.

Anything that is not an issue reference — prose, a bare description — is not a
`/goal`. Stop and point at [issue-workflow.md](../../rules/issue-workflow.md):
the issue comes first.

## Two run shapes

Decided in P1 from the sub-issue graph.

| Shape | Trigger | Scope |
|---|---|---|
| **Single** | no sub-issues | that issue only; finish it and stop |
| **Epic** | has sub-issues | every sub-issue, batched into the fewest PRs that can land |

`/goal #<sub-issue>` stays **that sub-issue only, then stop** — name the next
one in a line, never start it. Batching is what you get by pointing `/goal` at
the **epic**.

## Batching — default to ONE PR

The default is that the whole epic is one PR. A split needs a **named,
mechanical reason**:

1. **Schema before code, across workflows** — a migration that must be observed
   applied before the new code deploys is a real ordering constraint.
2. **A manual one-time step between parts** — a secret to hand-provision, a DNS
   cutover, an access grant on a new artifact.
3. **A sub-issue that is genuinely deferred** and was not asked for.

Blocked-by links do **not** force a split when both ends ride the same PR —
they land together. They only order one wave against the next.

**Review size never splits a wave on its own**, and never prompts a question.
State the scale in P3 (`N sub-issues, ~M files`) and carry on.

## Actions spend is a design constraint

Each push to a PR fires the CI workflow plus one `claude-mention.yml` run for
the re-review mention; each merge fires every post-merge workflow.

- **Batching is the primary lever.** One PR per epic means one merge's
  post-merge runs instead of N.
- **Never push per commit.** The gauntlet is free; CI is not. Push once per
  review round.
- **One `@claude` mention per push, never two**, and never on the first push.
- **Open the PR ready, once.** Draft → ready re-triggers the automatic review;
  close+reopen re-fires the whole gate. Use them only where stacked-PR
  retargeting requires it.
- **Poll with REST** — `gh pr checks`, `gh run list`. Costs nothing from the
  Actions budget. Poll at 5–10 min, not 30s.

## Phases

State each phase's outcome before starting the next. One todo per phase.

### P0 — Preflight

`gh auth status`; `gh api rate_limit` (both budgets — Projects v2 is
GraphQL-only and a throttled read reads as an empty board); clean tree; whatever
the local gauntlet needs running (named with the gauntlet in CLAUDE.md).

### P1 — Read the issue, and one hard stop

`gh issue view N --comments --repo uffici-ai/velcra-platform`, plus the REST
sub-issue and blocked-by reads from [manage-project](../manage-project/SKILL.md)
— REST costs nothing from the GraphQL budget, so never swap them for a GraphQL
equivalent. Pick the run shape.

**Open blockers outside the batch → stop** and name them. An epic is not a stop.

### P2 — Understand before implementing

Read the rule files the issue's area names. For a `fix(...)`, read
[defect-ledger.md](../../../docs/defect-ledger.md) **first** — a match means the
existing guard is missing or mis-scoped, and saying which is part of the root
cause. Read any spec the issue references under `docs/decisions/`.
Search for the helper that already exists before writing a new one.

### P3 — Restate the plan of record, then proceed

Post a short summary and keep going: the wave plan (which sub-issues in which
PR, and the named reason for every split) · files · **the guard that fails
without the fix** · docs and rules moving in the same PR · out of scope.

**This is a notification, not a checkpoint.**

🔴 **Do not re-open the design.** `implement` means it is settled. No options
menu, no "shall I instead", no re-deriving a decision the issue already made.
The one legitimate stop is a **genuine doubt** — the issue is silent on
something the implementation cannot proceed without, or its plan contradicts a
rule file. Then: do everything that does not depend on the answer first, answer
it yourself if the code, the rules or the spec can (and say which), and only
then ask that one narrow question in a sentence or two.

### P4 — Branch, then the board

`git checkout -b <type>/<N>-<slug>` off `develop` — `feat/` `fix/` `chore/`,
lead issue's number (the tracker's) for a wave.

Move **every** sub-issue in the wave to `In progress` **at branch creation**,
not at PR open. Export the board once to a file and reuse it; batch the writes
and re-read once at the end. Move the epic to `In progress` too, the first time
any of its children does — epics track Status like issues (owner call,
2026-08-22; [project-board.md](../../rules/project-board.md)).

### P5 — Implement, test-first

Test-driven throughout. For a fix, the [regression-guard.md](../../rules/regression-guard.md)
ordering is not optional:

1. Write the guard.
2. Confirm it **fails against the unfixed code**.
3. Fix.
4. **Mutation-check**: break the behavior the test names, confirm *that* test —
   not merely *a* test — fails, revert.

Escalate past a unit test when the defect lives in the module graph, a
config↔code gap, or an upgrade path. A guard over a set that can go empty
needs an explicit non-vacuity assertion.

**One commit per sub-issue**, so the branch stays attributable under a squash
merge. Rule files, `docs/`, CLAUDE.md tables and decision records land in the
same commit set — never a follow-up.

### P6 — Self-review

Run the `code-reviewer` subagent. Then state an explicit outcome for the
dimensions this repo demands one for — **silence reads as "not checked"**:

- **Guard present and mutation-checked** — or an honest statement that nothing
  mechanical can reach it, naming what the real check is and whether you ran it.
- **Docs and decisions** ([documentation.md](../../rules/documentation.md)) —
  "no doc impact" is a valid result.
- Any dimension a stack-bound rule file adds once the stack lands (log hygiene,
  duplication, …).

### P7 — Gauntlet

The local pre-push chain, as one `&&` chain. Green, or fix and re-run. Never
"the failure looks unrelated".

🔴 **No chain is defined for this repo yet.** Until [PLAN.md](../../../PLAN.md)
Phase 1 lands one, run locally whatever the CI workflow runs, and say in the PR
body that this is what "gauntlet green" meant.

### P8 — Commit and push

Conventional Commits, with the `Co-Authored-By` trailer the harness specifies for this session.
Push once, without asking.

### P9 — Open the PR

Title `<type>(<scopes>): <task ids or theme> — <what changed>`. Body in the
house shape: **What** · **Decisions worth reviewing** · **Guards**, including
the mutation-check evidence · **Docs** when a record or a doc moved.

🔴 **`Closes uffici-ai/velcra-platform#N` on its own line, one per issue** —
the cross-repo form, because that is where the issues live. Comma-separated
forms link only the first.

Move every batched sub-issue to `In review`. **No `@claude` self-review on the
first push** — the automatic review already fires on `opened`.

### P10 — Review fix-loop

Poll every 5–10 min: the CI jobs; the automatic `@claude` review; Copilot
threads via [copilot-review](../copilot-review/SKILL.md), which resolves every
thread.

Batch the fixes, gauntlet locally, push once, **then** post one focused
`@claude` re-review mention naming the SHA. The automatic review skips
`synchronize`, so these mentions are the only review later pushes get. Reply
in-thread per point with the fixing SHA.

Exit when CI is green with nothing unaddressed.

### P11 — Merge

Squash-merge. Two hard rules:

- **Never merge** with a failing required check or an unresolved review thread.
- **Never `--delete-branch` a branch that is the base of an open PR** — GitHub
  closes the child un-recoverably. For a stack: merge-commit the parent,
  retarget children to `develop`, close+reopen each to force CI, then delete
  branches.

More waves left → back to P4. Otherwise ↓.

### P12 — Deploy, once, at the end of the epic

🔴 **This repo has no deploy pipeline yet.** Until one lands (and this phase
names it — trigger model, what to wait for, the known red signatures), P12 is:
confirm every post-merge workflow on the merge SHA concluded green, and say so.
When a pipeline exists: a green deploy job is not proof — confirm the thing
that was deployed is actually serving, and **red is yours to fix, forward**:
diagnose to evidence, file the issue per
[issue-workflow.md](../../rules/issue-workflow.md), run the fix through P4–P11
as its own small PR. Never revert.

### P13 — Close out, then stop

Board → `Done` for every merged sub-issue. Flip newly-unblocked dependents
`Blocked` → `Ready`. Close the epic and set it `Done` once its last child is —
its Status has been `In progress` since P4, so this is the lifecycle's last
step, not a jump from `Backlog`.
If the run exposed a preventable error class, land a rule
([self-improvement.md](../../rules/self-improvement.md)).

Then **stop.** Report what merged, what the deployed state looks like, and name
any remaining work in one line — without starting it.

## Keeping This Skill Current

Update when a phase's owner changes (a rule file moves, a skill is renamed, a
subagent is added), when the batching split-reasons change, when the gauntlet
or a deploy pipeline lands (P7 and P12 are placeholders until then), or when
the Actions-cost shape changes. No mechanical guard pins the link targets above
yet — that check is Phase 1 in [PLAN.md](../../../PLAN.md); until it lands, a
rename here is a review obligation.
