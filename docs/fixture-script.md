# The fixture script format

A fixture script plays the **platform** from a file: a transcript store, a
committed offset per conversation, and a timeline of rings and expectations.
`uffici run --fixture <script> --once` plays it and exits with its verdict.

🔴 **This is the one scripted-host format in the product.** The companion's
fixture host plays it today; another implementation, in another language, plays the same format. Two
formats would drift, and the second one would be written by whoever needed it
next under time pressure.

`fixture/schema.json` is the machine-readable description of everything below.
The companion validates every script against it before reading it, and that other
implementation validates against the
same file. 🔴 That is why the schema exists rather than the Go loader alone: the
loader is not readable by the other party, and one of the two formats would be a
description of the other from memory.

The split is deliberate. The **schema** says what shape a script has, including
the typos a decoder swallows in silence: `withinMS` for `withinMs` decodes to
zero, the expectation quietly becomes the default, and the run is still green.
The **loader** says what a script means — that a ring names a conversation that
exists, that cursors increase, that the tally expects at least one ring.

## Shape

```json
{
  "v": 1,
  "conversations": { "<name>": { "id": "...", "offset": "...", "entries": [], "context": {} } },
  "steps": [ { "step": "ring", "...": "..." } ],
  "assert": { "ringsDelivered": 1 },
  "tools": [],
  "results": {},
  "memory": { "unavailableIn": ["<name>"] },
  "logs": { "harnessLevel": "info" }
}
```

`tools` and `results` are optional: the agent's Uffici tools.

- **`tools`** is the **agent's** tool catalog, in the `tools.list` result's shape (`name`, `description`, `parameters`, `permission`), served as written for **every** conversation the script declares. On the platform the catalog is the agent's, the same in each conversation it works in, so a plugin that keeps one catalog for its whole harness finds the same tools whichever conversation it listed first. Leave it out for an agent with no tools.
- **`results`** is each tool's scripted answer, keyed by tool name: `{result, callUrn?}`, given verbatim to every call of it, in any conversation. The `callUrn` is the script's own data, relayed as written — the host never makes one up — and absent it answers `null`, a call that did not run. A listed tool with no answer answers `{"ok": true}` and a null `callUrn`; a call naming a tool the catalog does not list is the platform's `not_found`; a call naming a request no ring delivered is refused before anything is recorded. A **re-run** names a call by its `callUrn`: it must be a call made earlier in the same conversation (anything else is the platform's `not_found`), it answers that tool's scripted result again, and it is recorded as a tool call of its own.

`memory` is optional: the agent's memory. Absent, the host keeps a working memory — a
`remember` keeps one fact on the request's conversation, and a `recall` there answers the
conversation's facts newest first, matched by a shared word of four or more letters (a
stand-in for the platform's search: a run proves the round trip, not the ranking).
`unavailable: true` answers every memory op as the platform does with its memory down;
`unavailableIn` does that only in the conversations it names, so one run shows memory
working in one conversation and an outage in another. The scripted host serves a memory
op or a tool call on any request it rang, closed or not; the platform refuses one on a
closed request, so a script never relies on that.

`logs` is optional: what the scripted host answers a logs ticket with. Absent,
shipping is on with the harness at `warn`, the platform's default. `ci/wave1.json` raises
it to `info` so a turn's Hermes lines are shipped — and its runs raise the machine's own
floor (`UFFICI_LOG_LEVEL_HARNESS=info`) to match, because the stricter of the two wins.

### `conversations`

Keyed by a name the steps use; `id` is what travels the wire.

- **`offset`** is the companion's agent's **committed** position when the run starts: the exclusive lower bound of the first delta. It advances only when a request closes as `answered` or `empty`, and **every ring starts at the offset committed so far**, as the platform's would. Cancelled, superseded, expired and errored leave it, so the next brief covers the union — entries nobody answered stay in range.
- **`entries`** are in cursor order, strictly increasing. They are already bounded: the platform bounds, the fixture serves.
- An entry's `author.self` marks the answering agent's own. A **delta omits** them; an **opener includes** them.
- An entry's **`attachments`** are references (`id`, `name`, `ref`) as the wire carries them; an attachment may also carry **`contentType`** and **`data`** (base64) — the platform's copy of the file, served to a download granted for it and never on the wire's entry. One without `data` cannot be downloaded (`not_found`).
- **`context`** (optional) is what that conversation's delta read carries, written in the brief's context shape (`blocks`, `tokens`, `bounded`, optional `missing`). It is served as written and never sized to the budget — the platform composes and sizes it, the fixture only serves it. Leave it out to play a platform that sends none; the plugin then keeps its static hint.

### `steps`

| Step | What it does |
|---|---|
| `ring` | Opens a request: `conversation`, `reason`, `causeEntry`, `throughCursor`, `deadlineMs`, `resetSession` |
| `appendEntries` | Adds entries mid-run — a message arriving while the agent answers |
| `expectAck` | Waits for an ack on the current request: `outcome`, `session`, `withinMs` |
| `expectOpener` / `expectNoOpener` | Whether **this** request earned an opener |
| `expectPost` | Waits for a post: `matcher` (`nonEmpty`, `contains`, `matches`, `question`), `value`. `question` is met only by a post that carried a question — the question card, never a question asked in plain text. With `attachments`, the post must carry exactly that many, each an upload of its request that arrived with the CRC32C its grant was minted for |
| `expectNoPost` | Asserts no post arrives for this request within `forMs` |
| `expectClose` | Waits for the close: `outcome` |
| `expectOffset` | Reads the store: `conversation`, `cursor` |
| `injectCancel` | Cancels from the platform: `reason`, `afterFirstPost` or `afterMs` |
| `disconnectPlugin` / `expectRedelivery` | Drops the plugin and asserts the brief comes again |
| `secondConnection` | Asserts a second plugin is refused with `already_attached` |
| `refuseNextQuestion` | The platform fails the next post that carries a question (`platform_unavailable`), so the companion withdraws the request that asked — the one real way a turn waiting on a question is cancelled |
| `expectToolCall` | Waits for a tool call on the current request: `name`, optional `argumentsContain` (a substring of the call's arguments as JSON), `withinMs` |
| `expectToolRerun` | Waits for a re-run of a recorded tool call (`tools.rerun`) on the current request: `withinMs` |
| `expectMemory` | Waits for a memory op on the current request: `op` (`recall`, `remember`, `forget` or `unfold`), `withinMs` |
| `expectUpload` | Waits for an upload on the current request that arrived intact: saved into Files at `to` when given, else attached to the reply; `contains` is a string its bytes must carry |
| `interruptNextUpload` | The next upload's connection drops `afterBytes` into a PATCH, once — the bytes written so far kept, the answer lost. The companion must resume from the offset a `HEAD` reports |
| `restartHarness` | Logs `restart the harness` and waits, `withinMs`, until a new harness process has had the open request redelivered. The run's outer driver restarts the harness on that line — `scripts/hermes-e2e.mjs` and the plugin's `dev/e2e.py` both do; a run with no driver fails the step |
| `endHarnessSession` | Logs `end the harness session` with the conversation's wire id as `conversation`, then waits `withinMs` for the run's outer driver to end that conversation's session inside the real harness — both drivers run the plugin's `dev/end_session.py` in the harness's container, which ends Hermes's live rows for it in `state.db`. The step asserts nothing itself: the script's next ring must ack `fresh` and earn an opener. A run with no driver fails the step |
| `wait` | `ms` |

🔴 **Every expectation is scoped to the CURRENT request** — the one the most
recent `ring` opened. An expectation that matched any event in the run would let
turn two be satisfied by turn one's events, and every step after the first turn
would pass instantly while asserting nothing.

### `assert`

The terminal tally: `ringsDelivered`, `acks`, `sessions`, `openersSent`,
`posts`, `closes`, `cancels`, `toolCalls`, `toolReruns`, `memoryOps`, `uploads`, `downloads`,
`uploadsResumed`. `uploads`, `downloads` and `uploadsResumed`, when present, are exactly how many
uploads were recorded, downloads served to their last byte, and recorded uploads that resumed
after a dropped connection. `toolCalls`, when present, is exactly
how many tool calls reached the scripted host — zero included, so a script can
assert that none did. A re-run is a tool call and counts there too; `toolReruns`,
when present, is exactly how many of them were re-runs. `memoryOps` counts each memory op it names, and only those. It is a **second, independent** check — a run can
pass every step and still have done more than the script intended.

## Files

The scripted host moves files, so a run proves both file use cases with a real
harness and no platform: it grants a download of a scripted attachment and an
upload of any size and CRC32C, and records an upload only when every byte arrived
with the checksum its grant names — attached to the request's next post, or saved
at `to`. 🔴 The bytes cross a real HTTP hop: the host serves tus and ranged
downloads on a loopback port, and the companion carries them with the gateway
host's own client (`gateway.FilesClient`), resume included — never an in-memory
hand-over that would prove the engine and not the client. The verdict line adds
`uploads`, `uploadsResumed`, `downloads` and `attachmentsPosted`, which both e2e
judges read.

## The verdict

`--once` exits:

| Code | Meaning |
|---|---|
| `0` | Every expectation held **and at least one ring was delivered** |
| `1` | Something did not hold |
| `2` | The companion could not start — configuration, the token, the script |

🔴 **Both halves of 0.** A script that rings nothing meets every expectation
vacuously, so without the second condition a green run would prove the daemon
starts and nothing else. `1` and `2` are distinct because "your script failed"
and "this build never ran your script" are different answers.

## A run waits for a plugin

🔴 The first step does not play until a plugin has attached (15 seconds, then
the run reports that and stops). Every step is written as though a plugin is
there, and `secondConnection` does more than race one: it dials, **takes** the
single-plugin claim before the real plugin does, and then both sides wait for
each other until the outer timeout — with a log that says `already_attached` and
nothing about why.

## What one run logs

One line per frame, at `info`, carrying `direction`, `frame`, `request`,
`conversation`, `elapsedMs` and that frame's counts — `deltaEntryCount`,
`deltaChars`, `openerEntryCount`, `openerChars`, `budgetDeclared`,
`budgetUsedChars`, `outcome`, `session`. These are the measurements a later release
needs.

After the verdict, one `logs shipped` line counts what the run shipped to the scripted
host, after the shipper's last flush: `companion`, `plugin`, `harness`, `redactions`,
`tokenLeaks` — shipped lines still carrying the run's wire token, which is zero on a
pass — and `canaries`. Before counting, the run hands each capture path — the companion's own,
and a plugin's and a harness's through `logs.push` — a canary line carrying that token;
`canaries` counts the sources whose own canary shipped redacted, which is three on a pass.
`restarts` counts the run's `restartHarness` steps, and `restartsWithLastWords` those
whose window held a shipped harness line at `warn` or above. The window runs from the
step's `restart the harness` line until the old plugin connection is seen gone, and the
line's own timestamp decides. That is the dying process's shutdown warning, which a
per-source count cannot see: the new process's startup lines come after the close.
The window opens at the ask, not at the driver's signal, which the companion cannot
see. A warning the old process logs in the driver's poll gap (about a second) before
it signals therefore counts as well.
Both first-conversation runs require all three sources, all three canaries, and both
restart counts equal to the restarts the driver made.

🔴 **Never any text.** The log is assembled from what the request state machine
reports, and that interface carries ids and counts only — so putting transcript
content in this log would take a new argument on it, which is a review rather
than an accident. `budgetUsedChars` is characters and not tokens deliberately:
nothing in the companion tokenizes, and the harness that declared the budget is
the only party that knows its own tokenizer.

## What the loader refuses

Refusals, not warnings — each is a way a script could run to completion while
asserting less than its author believed:

- a step kind it does not know (a typo silently removes an assertion);
- a ring naming a conversation the script does not declare;
- a cause entry that is neither declared nor appended by an earlier step;
- a `contains` matcher with no value, which matches every post;
- an `expectToolCall` with no `name`, which matches every call;
- an `expectMemory` naming an op that is not one of the four, which nothing could meet;
- memory taken down in a conversation the script does not declare;
- an `interruptNextUpload` after no bytes, which would resume nothing;
- a scripted result for a tool the script's `tools` do not list, or a tool listed twice;
- cursors that do not strictly increase, which makes a delta range ambiguous;
- **a script with no `ring` at all**, which could only ever prove the daemon starts.
