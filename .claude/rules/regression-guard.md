# Regression-Guard Rules

Every bug fix ships a **guard** that fails when the defect returns. Prose is the
weakest guard. Recurred classes and their guards:
[docs/defect-ledger.md](../../docs/defect-ledger.md).

## The loop

1. **Check the ledger before fixing.** A match means the existing guard is missing, mis-scoped, or never mechanical; say which in the issue.
2. **Write the guard first**; it must fail against the unfixed code.
3. **Mutation-check**: break the behavior the test names, confirm *that* test (not merely *a* test) fails, revert. A test whose setup restates the code asserts nothing.
4. **Record the class** in the ledger on its second instance.

## Escalate until the guard can reach the defect

| Defect lives in | Guard |
|---|---|
| Application logic | unit test, mutation-checked |
| A contract CI never crosses (two sides of a wire, a DTO and its consumer) | a cross-layer pin that executes both sides |
| Module graph / built bundle | static assertion over the real graph |
| Configuration vs the code that reads it (a manifest, a chart, an env file) | render the config → real env → the app's settings loader |
| An upgrade path fresh installs skip | run that path with the operator's flags |
| A file class CI only validates on the happy path (workflow YAML, manifests) | a static lint over the real files |
| Only an OS CI does not run | run the suite there; an assertion on another platform is a message, not a guard |
| Only the running system | a fail-loud probe: `exit 1` naming what is unset, never a skip |

**When nothing mechanical can reach it, say so in the PR**, name the real check,
and whether you ran it. "typecheck, tests and build pass" is not verification when
they passed before the fix.

## Non-vacuity

A guard over a set that can become empty needs an explicit assertion
(`expect(candidates.length).toBeGreaterThan(0)` before filtering, or a companion
precondition test). Green is the expensive failure.

## Review + scope

Review checks each fix carries a guard that fails without it and that the ledger
was consulted. **Report explicitly**: "guarded by X; mutation-checked" and "not
mechanically guardable; real check is the next deploy" are both valid, silence
reads as "not checked". Applies to shipped-behavior defects; a one-line fix still
gets a guard or a statement that it cannot.
