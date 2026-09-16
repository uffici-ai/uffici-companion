# Issue-Authoring Rules (always active)

**Epics are written for humans; sub-issues for AI agents, as development contracts** (Miguel, 2026-08-19): each reader gets only the altitude it consumes. Discovered defects keep [issue-workflow.md](issue-workflow.md)'s Symptom / Root cause / Fix plan / Guard shape.

## Epics: decision altitude

Problem, outcome, shape of the solution, locked decisions and invariants, sub-issues in dependency order, definition of done. **No file paths, function names or per-task mechanics.** Shape: `Why · Outcome · Shape of the solution · Decisions & invariants · Sub-issues · Definition of done · Panel dispositions`.

## Sub-issues: self-contained contracts

Implementable from the body alone. Omit a heading only when genuinely empty:

- **Deliverable**: one sentence naming the artifact that exists at merge.
- **Context**: file paths, rule files, prior art to read first.
- **Requirements**: numbered, testable MUST / NEVER statements.
- **Interfaces**: exact exports, env vars, JSON keys, routes, config names.
- **Acceptance criteria**: a mechanically verifiable checklist; CI green is implied.
- **Guard**: the test/lint that fails on regression ([regression-guard.md](regression-guard.md)), or why nothing mechanical reaches it.
- **Out of scope**: what NOT to build, including tempting deferred siblings.
- **Dependencies**: `Blocked by #N` lines, mirrored on the board.
