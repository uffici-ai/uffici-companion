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
  "conversations": { "<name>": { "id": "...", "offset": "...", "entries": [] } },
  "steps": [ { "step": "ring", "...": "..." } ],
  "assert": { "ringsDelivered": 1 }
}
```

### `conversations`

Keyed by a name the steps use; `id` is what travels the wire.

- **`offset`** is the companion's agent's **committed** position: the exclusive lower bound of the next delta. It advances only when a request closes as `answered` or `empty`. Cancelled, superseded, expired and errored leave it, so the next brief covers the union — entries nobody answered stay in range.
- **`entries`** are in cursor order, strictly increasing. They are already bounded: the platform bounds, the fixture serves.
- An entry's `author.self` marks the answering agent's own. A **delta omits** them; an **opener includes** them.

### `steps`

| Step | What it does |
|---|---|
| `ring` | Opens a request: `conversation`, `reason`, `causeEntry`, `throughCursor`, `deadlineMs`, `resetSession` |
| `appendEntries` | Adds entries mid-run — a message arriving while the agent answers |
| `expectAck` | Waits for an ack on the current request: `outcome`, `session`, `withinMs` |
| `expectOpener` / `expectNoOpener` | Whether **this** request earned an opener |
| `expectPost` | Waits for a post: `matcher` (`nonEmpty`, `contains`, `matches`), `value` |
| `expectNoPost` | Asserts no post arrives for this request within `forMs` |
| `expectClose` | Waits for the close: `outcome` |
| `expectOffset` | Reads the store: `conversation`, `cursor` |
| `injectCancel` | Cancels from the platform: `reason`, `afterFirstPost` or `afterMs` |
| `disconnectPlugin` / `expectRedelivery` | Drops the plugin and asserts the brief comes again |
| `secondConnection` | Asserts a second plugin is refused with `already_attached` |
| `wait` | `ms` |

🔴 **Every expectation is scoped to the CURRENT request** — the one the most
recent `ring` opened. An expectation that matched any event in the run would let
turn two be satisfied by turn one's events, and every step after the first turn
would pass instantly while asserting nothing.

### `assert`

The terminal tally: `ringsDelivered`, `acks`, `sessions`, `openersSent`,
`posts`, `closes`, `cancels`. It is a **second, independent** check — a run can
pass every step and still have done more than the script intended.

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
- cursors that do not strictly increase, which makes a delta range ambiguous;
- **a script with no `ring` at all**, which could only ever prove the daemon starts.
