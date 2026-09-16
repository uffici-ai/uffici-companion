---
paths:
  - ".github/workflows/**"
---

# GitHub Workflows Rules

Footguns of `anthropics/claude-code-action` and `pull_request` validation, each led by its diagnostic signature. Learned on velcra-platform — the issue pointers are that repo's.

- **Workflow files on `pull_request` events must be byte-identical to `develop`'s copy**, or the action's OIDC → app-token exchange 401s (`Workflow validation failed. The workflow file must exist and have identical content…`). Cross-cutting edits to `claude-code-review.yml` / `claude-mention.yml` land on `develop` first in their own PR; feature branches never carry a divergent copy.
- **A custom `prompt:` on `pull_request` needs `track_progress: true`** (tag mode) + `use_sticky_comment: true`; otherwise the action auto-detects agent mode, hunts for the PR (`gh pr list` is denied) and exits green with nothing posted (uffici-ai/velcra-platform#490). Signature: `"Auto-detected mode: agent"`, `permission_denials_count ≥ 1`, no PR comment. A clean review is never silent. `permissions:` needs `pull-requests: write`, `issues: write`, `id-token: write`, `contents: read`.
- **`additional_permissions:` only surfaces scopes the job's `permissions:` block already grants** — `actions: read` must appear in both, or the action silently skips its CI-reading MCP server and the review runs blind to check results while the job stays green (uffici-ai/velcra-platform#858).
- **Mention-triggered runs check out the PR head, not the event ref** — none of `claude-mention.yml`'s triggers is `pull_request`, so `github.sha` is the default branch's and every file the PR adds is missing; the bot posts "encountered an error" instead of a review (uffici-ai/velcra-platform#1118). The `ref:` expression in that file is the fix; keep it.
- **`--no-verify` is legitimate only on workflow-restore commits** where `git diff --check --cached` flags EOF blank lines; say so in the commit message.
- 🔴 **An invalid workflow file is a startup failure**: a failed run with zero jobs (`gh run view --json jobs` → `[]`), no log, blamed on `push`, while its real triggers never fire (uffici-ai/velcra-platform#1561). `${{ }}` is plain YAML text, so a plain scalar containing a colon followed by a space breaks the file: quote any expression whose text can carry `:`, `#`, or a leading `*` / `&` / `{` / `[`. `inputs.*` is not the hazard.
- 🔴 **A scheduled check MUST `exit 1` when it cannot assert**: failure is the only signal that leaves a scheduled workflow. Name every unset variable, validate every variable the probe depends on in the same guard, fail on every condition the runbook triages.
- **Pin actions to the latest major** (`actions/checkout@v7`, `actions/upload-artifact@v7`, `actions/github-script@v9`, `anthropics/claude-code-action@v1`). Check: `gh release view --repo <owner>/<repo> --json tagName -q .tagName`. Cross-cutting bumps are a standalone `chore(workflows):` PR to `develop`.
- **`runs-on` reads `vars.RUNNER_LABELS`** with `["ubuntu-latest"]` as the fallback, so the org's self-hosted fleet can be opted into per repo without a workflow edit; unset keeps hosted behavior. The fallback is config-time, not failover: offline self-hosted runners queue jobs until timeout, and recovery is unsetting the variable.
