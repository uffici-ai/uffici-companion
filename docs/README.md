# Uffici Companion Documentation

Developer-facing documentation. AI-agent sessions load
[CLAUDE.md](../CLAUDE.md) + [.claude/rules/](../.claude/rules/) automatically
and should follow those, not this index.

## Map

| Entry | What it is |
|---|---|
| [conventions.md](conventions.md) | How we work — the conventions map, quality gates, git + delivery workflow. |
| [decisions/](decisions/README.md) | Frozen design decision records (`YYYY-MM-DD-<slug>.md`) — point-in-time rationale, never edited after landing. |
| [defect-ledger.md](defect-ledger.md) | Defect classes that have recurred, each with its mechanical guard. |

Until the code lands, the design is the repository [README](../README.md).
With it come `architecture.md` (the README's diagram and surfaces as living
authority), the local wire (ring brief, loopback MCP contract) and the pairing
flow ([PLAN.md](../PLAN.md), Phase 1; README → *Planned repository layout*).

## Authority model

Documentation here follows one ordering, stated per subsystem and worth
knowing globally:

1. **Living subsystem docs** (`docs/<subsystem>/`) describe what runs today
   and are updated **in the same PR** as the change they describe.
2. **GitHub issues** carry task-level scoping and the discussion trail.
3. **Decision records** ([decisions/](decisions/README.md)) are frozen once
   the work lands — they record *why*, not *what is*.

[.claude/rules/](../.claude/rules/) is the distilled convention set AI-agent
sessions load — an always-on core plus path-scoped topic files; when a rule
file and a doc here disagree, fix one of them in the same commit — stale
instructions steer sessions wrong.

**Documentation is part of the change, never a follow-up**
([documentation.md](../.claude/rules/documentation.md), always active):
decisions are recorded in [decisions/](decisions/README.md) as they are
taken, and [conventions.md](conventions.md) moves with the convention set.
AI-agent sessions treat these docs as the current state of the system and
carry the same update obligation.
