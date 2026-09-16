---
argument-hint: <issue-number>
---

!`gh issue view $ARGUMENTS --repo uffici-ai/velcra-platform`

Investigate and fix the issue above, per `.claude/rules/issue-workflow.md`. This repo's issues live in velcra-platform (`.claude/rules/project-board.md`); pass an issue URL instead of a number to point anywhere else.

1. Trace the bug to its root cause — every contributing bug, with evidence; check `docs/defect-ledger.md` for a matching class
2. Write the guard first and confirm it fails against the unfixed code (`.claude/rules/regression-guard.md`)
3. Implement the fix; mutation-check the guard — break the named behavior, confirm *that* test fails, revert
4. Run the repo's local gauntlet (CLAUDE.md → Conventions) and fix anything red
5. Summarize what you changed, why, and how the guard proves it
