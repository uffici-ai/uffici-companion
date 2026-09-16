---
name: manage-project
description: Add, update, and control issues on the uffici-ai GitHub Project #1 board — set board Status, set the issue's native type/Priority/Effort/dates, label epics, link sub-issues, set blocked-by dependencies, and reorder Ready items. Use whenever issues are created, closed, or re-scoped so the board stays in sync.
argument-hint: "sync | add <#issues> | status <#issue> <Status> | fields <#issue> | deps"
---

# Manage Project Board (uffici-ai Project #1)

Keep <https://github.com/orgs/uffici-ai/projects/1> in lockstep with repo
issues. Conventions: [.claude/rules/project-board.md](../../rules/project-board.md);
this skill is the mechanics. The board is the organization's — one board, shared
with velcra-platform — so every project id below is shared too.

🔴 **This repo's work is filed in velcra-platform, not here** (owner call,
2026-09-15; [README.md](../../../README.md) → *Status and tracking*): sub-issues
of epic [uffici-ai/velcra-platform#1994](https://github.com/uffici-ai/velcra-platform/issues/1994).
So `$TRACKER` in the recipes below is `uffici-ai/velcra-platform` (set it once:
`TRACKER=uffici-ai/velcra-platform`), and the issue numbers in branches, PR
bodies and board lookups are that repo's. An issue that lives in this repo — a
report filed here once it is public — uses `uffici-ai/uffici-companion` in the
same recipes; read `.content.repository` before assuming.

## Prerequisite: `project` token scope

`gh` needs the `project` scope (separate from `repo`). Symptom when missing:
`your authentication token is missing required scopes [read:project]`.

```sh
gh auth refresh -s project --hostname github.com
```

Device flow — prints a one-time code + <https://github.com/login/device>. Run
with `run_in_background: true` so it keeps polling, surface the code to the
user, continue once they approve. Do NOT wrap in a short `timeout` — that kills
the poll before they authorize.

## Canonical IDs (snapshot — re-derive if a set fails)

Project node id: `PVT_kwDOEEkxTM4BUF52` (org `uffici-ai`, number `1`).
Field + option IDs can change if someone edits the board — re-derive with
`gh project field-list 1 --owner uffici-ai --format json` and read
`.fields[].id` plus `.fields[].options[].{name,id}`.

| Field | Field ID | Options (name=optionId) |
|---|---|---|
| **Status** | `PVTSSF_lADOEEkxTM4BUF52zhBQZd8` | Backlog=`f75ad846` Blocked=`ec769a00` Ready=`61e4505c` In progress=`47fc9ee4` In review=`df73e18b` Done=`98236657` |
| **Iteration** | `PVTIF_lADOEEkxTM4BUF52zhSIxIg` | this skill does not set it |
| **Systems** (was *Main System*) | `PVTMSF_lADOEEkxTM4BUF52zhai1NA` | 🔴 **not settable from the CLI** — see below |

🔴 **Priority, Effort and the dates are NOT project fields** (2026-09-03,
[record](https://github.com/uffici-ai/velcra-platform/blob/develop/docs/decisions/2026-09-03-issue-metadata-on-native-fields.md)).
They are GitHub-native, **organization-level issue fields** set on the issue; the
board only mirrors them. Setting them is in [Recipes](#set-a-native-issue-field)
below — never through `gh project item-edit`.

| Native field (org-level) | Field node id | REST id | Options |
|---|---|---|---|
| **Priority** | `IFSS_kgDOAnJoCg` | `41052170` | `P0` `P1` `P2` `P3` |
| **Effort** | `IFSS_kgDOAnJoDQ` | `41052173` | `XS` `S` `M` `L` `XL` |
| **Start date** | `IFD_kgDOAnJoCw` | `41052171` | date |
| **Target date** | `IFD_kgDOAnJoDA` | `41052172` | date |

Option ids for the GraphQL mutation are node ids — resolve them by name with the
query in the recipe; the REST listing (`gh api orgs/uffici-ai/issue-fields`)
shows integer ids, which are for the REST option-edit path only.

> 🔴 **`Systems` is a MULTI-select and cannot be written by this skill**
> (observed 2026-08-14 on velcra-platform). Two independent blockers, so there
> is no workaround to hunt for: `gh project field-list` returns
> `{"id": "", "name": ""}` for a `ProjectV2MultiSelectField`, and GitHub's
> GraphQL schema exposes **no `options`** on that type (`undefinedField:
> options`), so the option ids cannot be enumerated at all. `gh project
> item-edit` has no multi-select flag either.
>
> **So: skip it.** Until `gh` supports multi-selects, leave Systems unset and
> move on — it is exempt from "fill every field" (owner call, Miguel,
> 2026-08-17; [project-board.md](../../rules/project-board.md)). Do not stop to
> ask about it, and do not re-raise it on each filing. Set board Status here and
> the issue's native fields below, then say **once**, plainly, that Systems was
> left unset — the thing to avoid is reporting an item fully filled when it is
> not, not the empty field. Re-check when `gh` gains multi-select support, and
> drop this exemption in the same commit.
>
> 🔴 **`Definition` is GONE — removed from the board on 2026-08-30.** Its field
> id no longer resolves and a write fails with `Could not resolve to a node with
> the global id` — which inside a loop with no `set -e` reads as a partial
> success, because the surrounding `echo` still prints. Do not re-add it.
>
> Do **not** add an option to a single-select via `updateProjectV2Field` — that
> regenerates every option's id and wipes the field on every existing item. If
> the team wants a new option, add it in the project UI.

## Footguns

- **`gh project item-list` truncates silently at `--limit`** (default 30; a
  limit below the item count drops the newest items with no warning). 🔴 **Never
  hard-code the limit — derive it**, per the recipe below: a literal goes stale
  as the board grows and then fails in the one direction nobody checks. **Write
  to a file** (`> /tmp/items.json`) — piping large JSON straight into
  `python3 -c` can break with an empty read.
- **Every `gh project` call spends the GraphQL points budget** — Projects v2
  has no REST surface — and ~50 `item-edit` calls plus two full exports in one
  session exhausts it ([github-api.md](../../rules/github-api.md)). Preflight
  and act on the number:

  ```sh
  gh api rate_limit --jq '.resources.graphql | {remaining, reset}'
  ```

  Export **once** to a file and reuse it (a full `item-list` is a single
  large-page query — the most expensive call in this skill, and the board has
  well over a thousand items). Verify with one re-read at the end, not per item;
  if the re-read is throttled, the `item-edit` exit codes under `set -e` are the
  ground truth.

  🔴 **Measured, not theoretical: two full exports exhausted the entire 5,000-point
  hourly budget** in one session — the second one 422'd mid-board-update and left
  a just-created issue off the board for the rest of the hour. Points are charged
  for what a query *could* return, so a 1,000+-item page is enormous whatever it
  actually yields. Prefer `item-add --format json`'s returned id over any export;
  when you genuinely need the board, export once and treat it as a one-per-session
  resource.

  🔴 **`gh pr comment` and `gh pr merge` are GraphQL too**, so exhausting the
  budget on board work blocks the PR workflow as a side effect. The REST
  equivalents are unaffected and cost nothing from this bucket:
  `gh api repos/{owner}/{repo}/issues/<N>/comments --method POST --input <file>`
  and `gh api repos/{owner}/{repo}/pulls/<N>/merge --method PUT --input <file>`
  (the merge payload needs the **full 40-char** `sha`). The issue,
  sub-issue and blocked-by recipes below are REST and cost nothing from this
  budget — never replace one with a GraphQL equivalent to "be consistent".
- **A throttled read reads as an empty board.** `gh project item-list` under
  exhaustion fails rather than returning fewer items, but a swallowed error
  looks like "no items" — re-check `remaining` before concluding the board is
  out of sync.
- **JSON export camelCases field names oddly.** "Main System" exports as
  **`main System`** (capital S, space kept). Built-ins export as `status`,
  `labels`. The board's mirrored copies of the native fields export under their
  own names — read the issue, not the mirror, when you need a value.
- **The board is cross-repo.** `.items[].content.repository` says which repo an
  item's number belongs to; filter on it before matching a number, or `#12`
  resolves to the wrong repo's `#12`.
- **Sub-issue link** uses the issue's **integer DB id** with `-F` (not `-f`):
  `gh api repos/$TRACKER/issues/<PARENT>/sub_issues -F sub_issue_id=$(gh api repos/$TRACKER/issues/<CHILD> -q .id)`
  — a new sub-issue of epic #1994 is exactly this call with `1994` as the parent.
- **Dependencies** are GitHub-native blocked-by links:
  - set: `gh api --method POST repos/$TRACKER/issues/<N>/dependencies/blocked_by -F issue_id=<DB_ID_of_blocker>`
  - read: `gh api repos/$TRACKER/issues/<N>/dependencies/blocked_by`

## Recipes

### Add an issue to the board — and keep the item id it hands back

```sh
gh project item-add 1 --owner uffici-ai --format json \
  --url https://github.com/$TRACKER/issues/<N>
```

🔴 **`--format json` is not cosmetic — it returns the new item's `id`**, which is
the same id the field recipes below need. Take it from here and the expensive
export never happens. Adding an issue and then *searching the board* for the
item you just created is the single most costly mistake available in this skill:
it turns one small mutation into a full-board read.

### Map issue number → project item id (the fallback, for items already on the board)

```sh
# 🔴 Ask the board how big it is, then export THAT many. A one-item page still
# reports the true `totalCount` and costs almost nothing.
LIMIT=$(gh project item-list 1 --owner uffici-ai --format json --limit 1 --jq '.totalCount')
gh project item-list 1 --owner uffici-ai --format json --limit "$LIMIT" > /tmp/items.json
# then read .items[] where .content.repository == "$TRACKER"
# and .content.number == <N> → .id
```

🔴 **Only reach for this when you did not just add the item**, and 🔴 **never
with a hard-coded limit.** On velcra-platform this recipe shipped a stale literal
twice, and both times the dropped rows were the NEWEST — exactly the issue
somebody was looking for. A lookup that "cannot find" a brand-new issue is that
truncation, not a missing item. Re-reading `.totalCount` afterwards is not a
substitute: it is a step you have to remember at the moment you are already
reading a plausible-looking answer.

### Set a project single-select field (Status)

```sh
gh project item-edit --project-id PVT_kwDOEEkxTM4BUF52 \
  --id <ITEM_ID> --field-id <FIELD_ID> --single-select-option-id <OPTION_ID>
```

Set fields in separate calls (batching four in a tight loop occasionally races
— re-run any that don't stick and verify by re-reading the board).

### Set the issue TYPE

Free of the GraphQL budget, and it takes the type name directly:

```sh
gh api --method PATCH repos/$TRACKER/issues/<N> -f type=Task   # Task | Bug | Feature
```

### Set a native issue field

Priority / Effort / Start date / Target date live on the **issue**, not the board.
Resolve the ids by name — they are org-level, so any repo in the org returns the same set:

```sh
gh api graphql -f query='query{repository(owner:"uffici-ai",name:"velcra-platform"){
  issueFields(first:50){nodes{__typename
    ... on IssueFieldSingleSelect{id name options{id name}}
    ... on IssueFieldDate{id name}}}}}'
```

Then write — **one mutation sets every field on an issue**, so never loop per field:

```sh
gh api graphql -f query='mutation{setIssueFieldValue(input:{
  issueId:"<ISSUE_NODE_ID>", issueFields:[
    {fieldId:"<Priority field id>", singleSelectOptionId:"<P1 option id>"},
    {fieldId:"<Effort field id>",   singleSelectOptionId:"<M option id>"},
    {fieldId:"<Target date field id>", dateValue:"2026-09-30"}
  ]}){issue{number}}}'
```

The issue node id comes from `gh api repos/$TRACKER/issues/<N> --jq .node_id`.

🔴 **Never change a native field's OPTION LIST through GraphQL or the org settings UI.**
`updateIssueField`'s option input has no id, so the list is a positional REPLACE — it
regenerates every option id and silently drops the value on every issue using them
(315 issues on velcra-platform, 2026-09-03). Passing the current names back fails
`Name has already been taken`, because it appends rather than updates. Edit over
REST with each option's integer `id`, which renames/describes in place:

```sh
gh api orgs/uffici-ai/issue-fields                       # ids, names, colors, priorities
gh api --method PATCH orgs/uffici-ai/issue-fields/<FIELD_DB_ID> --input options.json
# options.json: {"options":[{"id":81319223,"name":"P0","color":"pink","priority":1,"description":"…"}, …]}
```

### Reorder items in the board's manual order

```sh
gh api graphql -f query='mutation($p:ID!,$i:ID!,$a:ID!){
  updateProjectV2ItemPosition(input:{projectId:$p,itemId:$i,afterId:$a}){clientMutationId}}' \
  -f p=PVT_kwDOEEkxTM4BUF52 -f i=<ITEM_to_move> -f a=<ITEM_it_goes_after>
```

Chain to make a contiguous run (order A→B→C: move B after A, then C after B).
Omit `afterId` (null) to move an item to the top.

## Status ↔ dependency convention (the rule this skill enforces)

Set **Status** from the blocked-by graph:

- any open blocker → `Blocked`
- no open blockers, not started → `Ready`
- epic: `Backlog` only while not yet startable; `Ready` when fully specced, `In progress` once any sub-issue is in development, `Done` when all sub-issues are done (owner call, 2026-08-22)
- work started (branch created / first commit) → `In progress`
- PR open → `In review`
- closed → `Done`

**Move to `In progress` the moment work starts** — at branch creation or first
commit, not only when the PR opens (right after `git checkout -b feat/N-...`,
run `status <#N> "In progress"`). Then `In review` when the PR is up, `Done` on
merge. After closing an issue, flip its now-unblocked dependents from
`Blocked` → `Ready` and set the closed issue to `Done`.

## `$ARGUMENTS` modes

- `sync` (default) — reconcile the whole board: every open issue touched this
  session is on the board, Status matches its dependency graph, newly
  unblocked issues move to Ready.
- `add <#issues>` — add the listed issues, set Status from deps, fill fields.
- `status <#issue> <Status>` — set one issue's Status.
- `fields <#issue>` — set the issue's **type** and native **Priority** / **Effort**
  (infer from the title's Conventional-Commit prefix and scope; ask only when it is
  genuinely ambiguous), and apply the `epic` label when it is one. 🔴 **`Systems` is
  NOT in this list** — it cannot be written from the CLI and is skipped; see the field
  table above. 🔴 These are issue fields now: `gh project item-edit` cannot set them.
- `deps` — print each tracked issue's blocked-by list; flag Status mismatches.

Always verify by re-reading the board (the derived-limit export above — not a
literal) and report what changed.
