# Design Decision Records

Frozen, point-in-time records of significant design decisions and their
rationale. A record is written when a design is locked in review (typically
the output of a design/brainstorming session approved by the owner), realized
as an epic + sub-issues on GitHub, and then **frozen once the work lands** —
it records *why*, and is never edited to track what runs today.

## When to write one

**As the decision is taken** — in the same PR that locks it, or before
implementation starts when the decision precedes the code
([documentation.md](../../.claude/rules/documentation.md)). A record written
after the fact reconstructs rationale instead of recording it. Not every
choice qualifies: reversible, code-local choices belong in the PR
description; the bar is *someone will ask why in six months and the PR trail
won't answer in one place*.

## Conventions

- **Filename:** `YYYY-MM-DD-<slug>.md`, dated by the day the design was
  locked.
- **Header:** every record opens with its date, a **Status** line (approved /
  frozen decision log, naming what supersedes it as living authority), and
  the related issues.
- **Frozen means frozen.** Once the implementation lands, the living
  authority is the subsystem doc (`docs/<subsystem>/`) and the matching
  [.claude/rules/](../../.claude/rules/) file — both updated in the same PR
  as any change. A record is only touched to amend its Status line or to
  note what superseded it. Changing a landed decision means a **new record**
  (and usually a new design review), not an edit here.
- **Authority order** (restated from the subsystem rules):
  living subsystem docs > GitHub issues > these records.
- **Every record is listed in the index below.** On velcra-platform a test
  pins this (index completeness, every listed record exists, names match the
  date pattern); the same guard is Phase 1 in [PLAN.md](../../PLAN.md) here.
  Until it lands, review checks it.

## Index

| Record | Decision | Living authority |
|---|---|---|
| [2026-09-16-project-management-inherited-from-velcra-platform.md](2026-09-16-project-management-inherited-from-velcra-platform.md) | Project management is inherited from velcra-platform: the shared org board, the native issue fields, the copied rule set, Gitflow | [project-board.md](../../.claude/rules/project-board.md), [conventions.md](../conventions.md) |
