# PR Workflow Rules (always active)

## Issue association

- **`Closes uffici-ai/velcra-platform#N` in the PR BODY, one keyword per line, as plain text.** This repo's issues live in velcra-platform ([project-board.md](project-board.md)), so the cross-repo form is the normal one — a bare `#N` would point at this repo's issue of that number. Cross-repo closing fires on merge into the default branch, which `develop` is here. Comma-separated forms link only the first; a keyword inside backticks renders fine and links nothing (uffici-ai/velcra-platform#1237); a keyword only in a commit message is discarded by the squash merge (uffici-ai/velcra-platform#1439). After merging, confirm each issue actually closed.

## Reviews

- **First push is automatic** (`claude-code-review.yml` on `opened` / `ready_for_review` / `reopened`, deliberately not `synchronize`). Never post a `@claude` self-review on open — it double-reviews.
- **Every later push gets exactly one `@claude` mention** naming the commit (`@claude please re-review the fixes in <sha> — confirm <X>`), which runs `claude-mention.yml`. These mentions are the ONLY review later pushes get.
- 🔴 **A reply that accepts or declines a review point without a push still needs the `@claude` mention** — a plain comment triggers nothing, so the reviewer never sees the disposition and the point stays open. Fold it into the push's one mention when a push is coming; otherwise that reply IS the round's one mention (uffici-ai/velcra-platform#1733).
- 🔴 **A PR opened with conflicts gets NO workflow runs** — no `refs/pull/<n>/merge` exists, so neither CI nor the automatic review is scheduled, and no red X says why. A later rebase fixes CI but not the review (`synchronize` is skipped). Rebase onto `develop` immediately before opening; after resolving, post an explicit `@claude` mention saying it is the FIRST review; before merging any PR confirm `gh pr checks <n>` shows runs (`mergeable_state: "dirty"` is the cause).

## Fix loop

Poll every 5–10 min (`/loop`): read new CI failures and reviews, fix what is actionable, run the local gauntlet, push once, post the mention. Treat `@claude` feedback like a human reviewer — reply in-thread with the fixing SHA per point. Exit when CI is green with nothing unaddressed, or the user merges/closes/says stop. A preventable error class → [add a rule](self-improvement.md) on the same branch. The [goal skill](../skills/goal/SKILL.md) drives this as P10.

## Every push costs billed CI minutes

- **Never push per commit.** The local gauntlet is free; collect the round's fixes, gauntlet, push once.
- **One `@claude` mention per push, never two**, never on the first push.
- **Open the PR ready, once.** `draft → ready` and close+reopen both re-fire the gate; legitimate only for stacked-PR retargeting.
- **Fewer, larger PRs** — each merge also fires every post-merge workflow. Group related issues, one `Closes` line each.

## Pushes are serial

Never run two pushes at once: two pre-push gauntlets race each other's shared resources, one loses, and the failure lands in a file the diff never touched — which reads as a flake, and a flake may never be dismissed. `git push … | tail` reports the PIPELINE's exit code, so a hook-rejected push reads as success: verify a push landed with `git ls-remote origin <branch>` against `git rev-parse HEAD`, never the exit code.
