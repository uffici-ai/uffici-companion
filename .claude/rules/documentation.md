# Documentation Rules

[docs/](../../docs/README.md) is the current state of the system — authority beside
these rules; when it disagrees with the code, fix one in the commit that finds it.
**Docs move in the same commit/PR as the change, never as a follow-up.**

- **Decisions are recorded as taken**: a locked design decision gets `docs/decisions/YYYY-MM-DD-<slug>.md` in the PR that locks it, added to the [index](../../docs/decisions/README.md). Bar: someone will ask why in six months and the PR trail will not answer in one place. Code-local, reversible choices belong in the PR description.
- **Architecture changes update `docs/architecture.md`**: the file lands with the first architecture decision and moves with every later one — components added/removed/renamed, trust boundaries, data stores, public surface, topology.
- **Convention changes update [docs/conventions.md](../../docs/conventions.md)**: a rule file added/retired/re-scoped, an invariant moved, the gauntlet or delivery workflow changed.
- **Guards**: none mechanical yet. The two that exist on velcra-platform — every relative link in the instruction set resolves; every decision record is in the index and named to pattern — arrive with the toolchain ([PLAN.md](../../PLAN.md), Phase 1). Until then review carries them. The same-commit obligation is judgment either way: review states the outcome — "no doc impact" is valid; silence reads as "not checked".
