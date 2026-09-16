# Self-Improvement Rules

When an avoidable error blocked work, an error class appears twice, a convention was
found by trial and error, or a tool has a footgun: capture it.

- **Prefer something that fails over prose**: ask what would have to fail in CI to make the rule unnecessary (test, render assertion, lint gate) and write that instead, or as well ([regression-guard.md](regression-guard.md)).
- **Where**: the topic file in `.claude/rules/` (path-scoped via `paths:` frontmatter unless global). Amend, never duplicate.
- **Shape**: imperative first, one rationale clause with the issue pointer, no narrative. Rule files carry no "keeping current" section: update the rule in the same commit as the behavior it describes; delete stale entries rather than annotating them.
- **Reality**: an unlanded fix is present tense with its tracking issue; never narrate a fix as done before it lands.
- **Landing**: a lesson from the current PR lands in it; an orthogonal one as a `chore(rules):` PR. A lesson that also applies to velcra-platform is a PR in each repo — the rule sets are copies, not links ([record](../../docs/decisions/2026-09-16-project-management-inherited-from-velcra-platform.md)).
