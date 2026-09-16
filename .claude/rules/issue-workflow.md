# Discovered-Issue Workflow (always active)

A defect found while doing anything becomes a tracked issue first, then a fix — never a silent inline fix or a chat report. Planned work: [issue-authoring.md](issue-authoring.md).

- **The unit is the situation.** One failed action with two broken links = one issue documenting both; two unrelated problems = two issues.
- **Even a one-line fix gets an issue.** In scope: defects in shipped behavior. Out: net-new features, pure refactors.

## The sequence

1. Reproduce; identify every root cause with evidence. Check [docs/defect-ledger.md](../../docs/defect-ledger.md): a match means the existing guard is missing or mis-scoped — say which.
2. File it **in the tracker repo** (`uffici-ai/velcra-platform` — [project-board.md](project-board.md)): `fix(companion): <situation>`; body **Symptom / Root cause(s) + evidence / Why / Fix plan per bug / Guard / Scope**.
3. Add to Project #1; set type, Priority and Effort on the issue and Status on the board ([project-board.md](project-board.md)).
4. Move to `In progress`.
5. Implement on `fix/<n>-<slug>` (the tracker's number), TDD; anything that touches a running environment lands via a merged PR, never an ad-hoc write. Land the guard with the fix, mutation-checked ([regression-guard.md](regression-guard.md)); a recurred class goes in the ledger.
6. PR with `Closes uffici-ai/velcra-platform#<n>`, `In review`, the [pr-workflow.md](pr-workflow.md) loop.
7. On merge: `Done`; flip what it unblocked.
8. If the merge deploys anywhere, follow it to green; a red environment belongs to whoever merged — fix forward, never revert.

`/goal implement #<n>` ([goal](../skills/goal/SKILL.md)) runs steps 3–8.
