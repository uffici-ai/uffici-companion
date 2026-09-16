# Project Board Rules (always active)

[uffici-ai Project #1](https://github.com/orgs/uffici-ai/projects/1) is the source of truth for **where work stands**; the issue itself carries **what it is**. One board for the whole organization ([record](../../docs/decisions/2026-09-16-project-management-inherited-from-velcra-platform.md)). Mechanics: the [manage-project](../skills/manage-project/SKILL.md) skill; run its `sync` before ending a session that created or closed issues.

**Where this repo's issues live (owner call, 2026-09-15; [README.md](../../README.md) → *Status and tracking*):** in velcra-platform, the private tracker, as sub-issues of epic [uffici-ai/velcra-platform#1994](https://github.com/uffici-ai/velcra-platform/issues/1994). This repo carries the code and the PRs: branches carry the tracker's issue number, and a PR closes its issue cross-repo (`Closes uffici-ai/velcra-platform#N`, [pr-workflow.md](pr-workflow.md)). An issue filed here directly — a report once the repo is public — follows the same rules on this repo; say which repo an `#N` means whenever it is not obvious.

**The split (2026-09-03, [record](https://github.com/uffici-ai/velcra-platform/blob/develop/docs/decisions/2026-09-03-issue-metadata-on-native-fields.md)):** Priority, Effort, Start date and Target date are **GitHub-native, organization-level issue fields** set on the issue; the board only mirrors them. Status and Iteration stay project fields. The board's own `Project Priority` / `Size` / `Project Start date` / `Project Target date` were migrated and removed — do not re-create them.

## Update the board when you

- **Create an issue**: set its **type** and native **Priority** / **Effort** on the issue, label it `epic` if it is one, then add it to the board and set Status from its dependency graph.
- **Close an issue**: `Done`; flip dependents with no open blockers `Blocked` → `Ready`.
- **Re-scope / supersede**: update Status; absorbed → close + `Done`.
- **Change a blocked-by**: re-evaluate the dependent.
- **Open a branch/PR**: `In progress`, then `In review`.

## Status ↔ dependencies

- `Backlog`: epic not yet startable. `Ready`: epic specced with sub-issues filed, or an issue with no open blockers.
- `In progress` / `In review`: a branch/PR in flight; an epic with any sub-issue in development.
- `Blocked`: ≥1 open blocker. `Done`: closed, or an epic whose sub-issues are all done.
- Epics move through Status like issues (owner call, 2026-08-22). Keep `Ready` honest, ordered by critical-path depth via the board's manual order.

## Fields on the issue (org-level, native)

- **Type** `Task` / `Bug` / `Feature` — every issue carries one. Follow the title's Conventional-Commit prefix: `feat`/`epic` → Feature, `fix` → Bug, `chore`/`refactor`/`docs`/`ci`/`test`/`harden`/`infra` → Task. 🔴 `harden(...)` is a **Task**, not a Bug: the author chose not to call it `fix(...)`, and that distinction is deliberate.
- **Priority** `P0` `P1` `P2` `P3`. **Effort** `XS` `S` `M` `L` `XL`. **Start date** / **Target date**.
- **`epic` label** marks every epic. An epic is an issue that owns sub-issues, or whose title declares one (`^epic`, `(epic)`, `: epic —`). A title that merely *cites* one ("missing epic-#12 columns") is not an epic.
- 🔴 **Never edit a native field's option list through GraphQL `updateIssueField` or the org settings UI.** Its option input carries no id, so the list is a positional REPLACE: every option id is regenerated and every issue value pointing at them is silently dropped (315 issues on velcra-platform, 2026-09-03). Edit over REST instead — `PATCH /orgs/uffici-ai/issue-fields/{id}` with each option's existing integer `id` in the payload renames and describes in place, ids and values intact. Passing the current names back through GraphQL does not work either: it fails `Name has already been taken`, because the list appends rather than updates.

## Fields on the board

- **Status** and **Iteration** are the board's own; everything else it shows is a mirror of the issue.
- **Systems**: a multi-select `gh` cannot write; leave unset, say so once (owner call).
- 🔴 `Definition` is deleted (2026-08-30; a write fails `Could not resolve to a node`); do not re-add.
- Never add a single-select option to a **project** field via the API either (same failure mode: it regenerates every option id and clears the field across the board).
- Hierarchy is GitHub-native: sub-issues + blocked-by feed `Parent issue` / `Sub-issues progress`.
