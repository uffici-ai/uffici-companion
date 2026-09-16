# Defect-class ledger

Bugs that have happened **more than once**, each with the guard that now makes
a third occurrence fail in CI. Consult it when you find a defect — before
writing the fix — and add to it when a class recurs.

This is not a bug list. Individual bugs live in GitHub issues; a class earns an
entry here only when the *same shape* has bitten twice, because that is the
evidence that the guard is missing rather than that someone was careless. The
discipline that maintains it is
[`.claude/rules/regression-guard.md`](../.claude/rules/regression-guard.md).

**Nothing has recurred here yet.** velcra-platform's
[ledger](https://github.com/uffici-ai/velcra-platform/blob/develop/docs/defect-ledger.md)
is the reference for the shape of an entry and for the classes that tend to
cross repos. The one worth knowing before it bites: **a check that reports a
verdict it never established** — a guard over a set that can become empty, a
test whose setup restates the code, a gate that can skip or truncate itself.
Its failure mode is green, which makes it the most expensive class there is.

## Entries

When the first class lands, it goes in a table with these columns:

| # | Class | Instances | Guard | Status |
|---|---|---|---|---|

`Instances` are issue numbers; `Guard` names the test, lint or render
assertion; `Status` is ✅ pinned, 🟡 partly pinned / review-only, or
🔴 unpinnable (with the review heuristic that stands in). Each row gets a
numbered section below the table with the story: what happened each time, why
the first guard missed it, and what the current one asserts.
