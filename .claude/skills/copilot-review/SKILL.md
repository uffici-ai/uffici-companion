---
description: Review and address Copilot review comments on a PR — triage, fix, push, and resolve threads
argument-hint: [pr-number]
---

## Copilot Review Handler

Handle all unresolved Copilot review comments on a GitHub PR: triage each
comment, fix valid issues, reply with a short status, and **resolve every
thread** before the skill exits. No thread is left open.

### Step 1 — Preflight

`gh --version` must succeed (else: install from <https://cli.github.com/>) and
`gh auth status` must be authenticated (else: `gh auth login`). Stop on either
failure.

### Step 2 — Identify the PR

Use `$ARGUMENTS` if a PR number was provided; otherwise auto-detect:

```bash
gh pr list --head "$(git branch --show-current)" --json number,title --jq '.[0]'
```

No PR found → tell the user and stop. Store the number as `PR_NUMBER`.

### Step 3 — Gather unresolved review threads

Review threads are GraphQL-only, so this is the one unavoidable points spend
in the skill ([github-api.md](../../rules/github-api.md)) — page it, don't
inflate it.

```bash
gh api graphql -f query='
query($owner: String!, $repo: String!, $pr: Int!, $after: String) {
  rateLimit { cost remaining resetAt }
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $pr) {
      reviewThreads(first: 50, after: $after) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id
          isResolved
          comments(first: 5) {
            nodes {
              body
              author { login }
              path
              line
              originalLine
              diffHunk
            }
          }
        }
      }
    }
  }
}' -f owner='{owner}' -f repo='{repo}' -F pr="$PR_NUMBER" -f after=''
```

Replace `{owner}` / `{repo}` from `gh repo view --json owner,name`. While
`pageInfo.hasNextPage` is true, repeat with `-f after='<endCursor>'` — **never
raise `first:` to skip paging**, since cost is charged on page size × nesting
(50 × 5 ≈ 3 points/page; 100 × 10 ≈ 11). The `rateLimit` block prices each
call in its own response; if `remaining` drops under ~500, finish the threads
in hand, report, and resume after `resetAt`.

Filter to `isResolved == false`. None left → report "No unresolved review
threads found" and stop. **A throttled query is not an empty result** — a
failed page means stop and say so, never "no unresolved threads".

### Step 4 — Triage each comment

Read the file at the referenced path/line plus the comment body + diff hunk,
then classify into exactly one outcome:

| Outcome | Meaning | Reply |
|---------|---------|-------|
| **Fix** | Valid & actionable — change the code in Step 5 | `Fixed` |
| **Already fixed** | A prior commit on this PR addressed it | `Already fixed` (cite the prior commit SHA) |
| **Won't fix** | False positive, duplicate of an already-decided item, or architectural deferral | `Won't fix — <≤12-word reason>` |

If any comment lands in **Won't fix**, present those rows before posting:

| # | File | Line | Comment | Proposed reply |
|---|------|------|---------|---------------|

Ask the user to confirm each reply, override to **Fix**, or supply a different
one-line reason. If the user re-invokes the skill without answering, proceed
with the proposed replies — never leave a thread open.

### Step 5 — Implement fixes

For each **Fix** (or user-approved) comment: make the fix per project
conventions (CLAUDE.md, `.claude/rules/`), keeping changes minimal — only what
the comment asks for, no drive-by improvements.

### Step 6 — Lint and test

Run the repo's local gauntlet (CLAUDE.md → Conventions names the chain). Fix any
failures and re-run. Never commit with broken lint or tests. While no gauntlet
is defined for this repo, say so in the report rather than skipping silently.

### Step 7 — Commit and push

Round number = 1 + count of prior round commits:

```bash
git log --oneline --grep="address Copilot review round" | wc -l
```

Commit as `fix: address Copilot review round N — X issues on PR #Y`
(N = round, X = issues fixed, Y = PR number). Push to the current branch, then
post the round's one `@claude` re-review mention naming the SHA
([pr-workflow.md](../../rules/pr-workflow.md)).

### Step 8 — Reply and resolve every thread

Every thread from Step 3 ends **resolved** — no exceptions. Post a one-line
reply per thread using these templates verbatim, then resolve:

| Outcome | Reply template |
|---------|---------------|
| Fix | `Fixed.` |
| Already fixed | `Already fixed in <commit-sha>.` |
| Won't fix | `Won't fix — <≤12-word reason>.` |

No rationales or multi-sentence explanations. If a reason needs more than ~12
words, file a follow-up issue and reply `Won't fix — follow-up in #<issue>.`

```bash
# Reply
gh api graphql -f query='
mutation($threadId: ID!, $body: String!) {
  addPullRequestReviewThreadReply(input: { pullRequestReviewThreadId: $threadId, body: $body }) {
    comment { id }
  }
}' -f threadId='THREAD_ID' -f body='REPLY_TEXT'

# Resolve
gh api graphql -f query='
mutation($threadId: ID!) {
  resolveReviewThread(input: { threadId: $threadId }) {
    thread { isResolved }
  }
}' -f threadId='THREAD_ID'
```

### Step 9 — Report summary

Every row contributes to a resolved thread — there is no "left unresolved"
state.

| Status | Count | Details |
|--------|-------|---------|
| Fixed | N | Short list of files changed |
| Already fixed | N | Thread + pointer commit |
| Won't fix | N | Thread + one-line reason |

Include the commit SHA and a link to the PR.
