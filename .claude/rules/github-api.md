# GitHub API Rules (always active)

Signature: `gh` fails with `GraphQL: API rate limit already exceeded` while `gh api <path>` keeps working.

| Bucket | Budget | Charged |
|---|---|---|
| REST (`core`) | 5,000 requests/hr | 1 per request |
| GraphQL | 5,000 points/hr | query complexity — page size × nesting, charged for what a query COULD return |
| Actions `GITHUB_TOKEN` | 1,000 requests/hr/repo | its own budget: a burned local session never explains a CI failure, or vice versa |

- **Preflight before bulk work** (free): `gh api rate_limit --jq '{core: .resources.core, graphql: .resources.graphql}'`. Under ~500 GraphQL points, finish the item in hand and report.
- **Route to REST when both exist**: `gh api repos/{owner}/{repo}/issues/<n>` over `gh issue view`; `gh api …/issues?state=open` over `gh issue list`; sub-issues and blocked-by are already REST. Projects v2 and PR review threads are GraphQL-only — that spend is non-negotiable, so avoidable reads must not compete with it. Raising `first:` to avoid paginating is the most expensive move.
- **When limited: stop, report, wait.** Never retry in a loop; honor `Retry-After` / the exhausted bucket's own `reset` (the two clocks differ).
- **A throttled read is not a negative result** — re-check the bucket before concluding anything about the data.
- **Secondary limits** (bursts of mutations) trip with points still on the clock: serialize and back off; REST does not help.
