# The local wire

The wire between the **companion** and a **plugin**, on the customer's own
machine. One duplex connection, versioned JSON frames, one request at a time per
conversation.

This document is the specification. The machine-readable half is
[`wire/v1/`](../wire/v1): JSON Schemas, golden examples and a manifest. Where
the two disagree, the schemas win and this file is the bug.

## Two hops, two words

The platform reaches the companion; the companion reaches the plugin. They are
different hops carrying different things, and they have different words:

| Hop | Word | Carries |
| --- | --- | --- |
| platform → companion (doorbell, over the network) | a **ring** | ids and a reason. **No content, ever.** |
| companion → plugin (this wire, on the machine) | a **brief** | the ids, the reason, and the conversation content the platform authorized |

🔴 **"Ring" never names a local frame.** The doorbell envelope is the platform's
and stays ids-only; if this document or a frame ever called a local message a
ring, the two would blur and the content boundary with it.

## Transport

- **Unix socket by default**: `$UFFICI_HOME/wire.sock`, mode `0600`, owner-only. Custody of the socket file is the authorization boundary.
- **Loopback TCP when configured**: `127.0.0.1:7411`, path `/v1/ring`, for a pod where the plugin and the companion share a network namespace but not a filesystem.
- WebSocket text frames, one JSON object per frame.
- 🔴 **A connection carrying an `Origin` header is refused.** A browser is not a plugin, and an `Origin` is the only thing that says one is on the other end.

## The token

The companion mints a random token at first start into `$UFFICI_HOME/wire-token`
(mode `0600`). Every plugin `hello` presents it; the companion compares it in
constant time; a mismatch closes the connection with `unauthorized`.

🔴 **The token is secret-class.** Neither side logs it — not at any level, not
inside an error, not inside a whole-frame dump. `PluginHello` redacts it when
formatted, and a decoder error names a schema location, never a value.

## The frames

Ten types. Every frame carries `v` (the protocol version, `1`) and `type`.

### `hello` — both directions, two shapes

One type name; who received it is the only thing telling the shapes apart.

**Plugin → companion**: `harness`, `harnessVersion`, `pluginVersion`, `token`,
`contextBudgetTokens` — the harness's share of its window for platform content.
The platform sizes to that budget with its own estimator; the companion never
estimates.

`processId`, optional, is a nonce the plugin mints once per harness **process**.
A reconnect from the same process sends the same id, and the companion keeps the
requests that process acknowledged: the harness is still working on them. A
different id is a restarted harness, which holds nothing of the old one's turns,
so the companion delivers every request the old process had acknowledged again,
counted against the redelivery bound so a harness that restarts on every brief
still ends in `redelivery_exhausted`. A plugin that omits it keeps the older
rule, under which an acknowledged request is never redelivered and sits until
its deadline after a restart.

**Companion → plugin**: `companionVersion`, and once known, `agentSlug` and
`orgSlug`. `features`, optional, lists what this companion can carry beyond what
every v1 companion does, so a newer plugin never sends an older companion a field
it would drop: `question` means a `post` may carry a question; `redelivery` means
a restarted harness process is handed again every request its predecessor had
accepted; `tools` means the plugin may call [`tools.list`](#toolslist) and
[`tools.call`](#toolscall), and a `post` may carry `citations` and `activity`. `memory` means the plugin may call the four [`memory.*`](#memoryrecall-memoryremember-memoryforget-memoryunfold) ops. An
absent list means none, and a plugin ignores a name it does not know.

### `ready` — plugin → companion

The harness can now **answer**. That is not the same as the plugin being
connected, and the difference is not cosmetic.

🔴 **A plugin attaches while its harness is still starting.** Hermes connects
its platform adapters *before the gateway reaches `running`*, and its own base
adapter warns that an inbound message arriving before the message handler is
installed is "silently discarded on an adapter that still polls and sends". So
a companion that rang on `hello` delivered the first brief of every run into a
window where the harness could not answer it: the turn closed `empty`, and the
real reply arrived seconds later for a request that was already gone. That is
what this frame exists to prevent — found by running a real Hermes against a
real companion.

The plugin's `hello` is also where the companion learns WHICH harness it serves,
and the platform reads that on the doorbell's connection ticket. The doorbell opens
at start, before any plugin, so its first connection reports the harness as
`unknown`; once a `hello` has named it, the doorbell reconnects once, at its next
heartbeat, to report it. The same happens whenever the facts change later: an
upgrade restarts the harness under a connected companion, and the new plugin's
`hello` names a new plugin or harness version, which the doorbell reports the
same way. A plugin that merely went away changes nothing, so a harness restart
costs one reconnect, not two. Presence keeps meaning "the companion is
connected", and the Agents screen names the harness and plugin that answer.

So: **`hello` means "I am here"; `ready` means "send me work".** The companion
holds every ring until `ready` arrives, then delivers them — the same holding
it already does for a ring that arrives with no plugin attached at all, so
nothing downstream needed a new idea.

Carries `harnessVersion` when the plugin knows it, which it often does not at
`hello` time, precisely because the harness is still starting.

A plugin that never sends `ready` never receives a brief. That is the safe
direction: a held ring is answered late, and a delivered one is answered by
nobody.

Any frame before `hello` closes the connection with `hello_required`.

### `brief` — companion → plugin

Opens one request on one conversation: `requestId`, `conversationId`, `reason`
(the platform's ring-reason vocabulary, carried verbatim), `cause` —
`{entryId, authorId}`, the recorded human cause — `deadline`, `sinceCursor`,
`throughCursor`, `resetSession`, `delta` and an optional `context`.

- The `delta` is `(sinceCursor, throughCursor]`, with the answering agent's **own** entries omitted; `sinceCursor` is `null` for a conversation's first brief.
- `throughCursor` is fixed when the request opens, so a redelivered brief recomputes the same range.
- `resetSession` is the platform forcing a fresh harness session; the plugin must not resume.
- The plugin renders the `cause`'s author as the inbound sender and the `reason` as one line.
- `context` is what the agent is told before the conversation — see [the context](#the-context). A companion that predates it sends none, and the plugin keeps its static hint.

### `ack` — plugin → companion

`requestId`, `outcome`, `session`, and an optional `reason`.

- `outcome`: `accepted`, `deferred`, `rejected`.
- `session`: `fresh` or `resumed`. **Freshness is the harness's fact**, not the platform's — only the plugin knows whether it opened a new session.
- A `rejected` ack redelivers. A `deferred` ack keeps the request open with no session yet.

### `opener` — companion → plugin

`requestId` and `entries`: the verbatim recent tail within the declared budget,
**including** the agent's own entries.

Sent **only** after an accepted ack with `session: fresh`, and before the turn.

### `post` — plugin → companion

`requestId` and `text`. **Every post means "more coming"**; `done` closes.

`question`, optional, makes the post the harness **asking** the person: the text
is the question, and it reaches them as the question card. It carries the
harness's own `id` for the question — which the answer names — the `choices`
offered (at most ten, possibly none for an open question) and `multiSelect`. The
person's answer arrives as the NEXT brief's delta, like any message, and a typed
reply answers too. Sent only to a companion whose `hello` lists the `question`
feature; to any other, a harness falls back to asking in plain text.

`citations`, optional, lists the `callUrn`s of the agent's **own** tool calls the
answer rests on — at most twenty, each `urn:uffici:tool_call:<n>` as a
[`tools.call`](#toolscall) result named it. Uffici refuses a citation of a call
that is not this agent's in this conversation, and the post then fails like any
refused post: the request is withdrawn with a `failed` cancel carrying Uffici's
sentence.

`activity`, optional, reports what the agent did with its harness's **own**
tools — at most twenty `{tool, summary}` pairs, the tool's name (up to 64
characters) and one line about what it did (up to 280). It is shown marked, as the
harness's account of itself, and is never treated as a Uffici tool call. Never a
tool's arguments or its result.

Both are sent only to a companion whose `hello` lists the `tools` feature.

### `done` — plugin → companion

`requestId`, `outcome`, and `errorClass` when the outcome is an error.

- `outcome`: `answered`, `empty`, `error`, `cancelled`. `empty` is an explicit "nothing to say", which the platform notices rather than guesses at.
- `errorClass`: `harness_error`, `model_error`, `timeout`, `cancelled`, `internal`. A failure is classified, never a message.

### `cancel` — companion → plugin

`requestId` and `reason`: `stopped` (a person's stop), `superseded` (a newer
message), `expired` (the deadline passed), `failed` (the platform refused or
failed a call the request needed — a read, a post).

- `error`: present exactly when `reason` is `failed` — `class`, the platform's error class (`request_closed`, `platform_unavailable`, …) or the companion's own (`edge_challenge`, `unreachable`), for the machine; and `instruction`, the sentence the agent relays to the person (the platform's failure semantics: never a silent failure). Platform vocabulary, never tenant content.
- A `failed` request is already closed on the platform as an error, which tells the person the agent did not answer. The plugin stops the turn exactly as for any other cancel; the class is safe to log.
- 🔴 `failed` and `error` were added to v1 before its first release: nothing had shipped to a machine, so v1 was still free to change. After a release, a new value here is a new version.

### `release` — companion → plugin

`conversationId` and `reason`: `pass_ended` (the agent's lane pass on a card ended —
the card moved to another lane's agent), `archived` (the card was archived) or
`deleted` (its conversation was deleted).

- The plugin may discard whatever it holds for that conversation — a harness session above all. The next arrival in the lane rings with `resetSession: true` anyway, so a release that never arrives costs memory, never correctness.
- 🔴 It withdraws NO request: an open one was cancelled by its own `cancel` first. A `release` is never answered.
- Additive: a plugin that predates it ignores an unknown frame type, as every reader must.

### `call` — plugin → companion

One platform control-plane operation, through the companion. Fields: `callId`, `op`, `deadlineMs`, and an optional object `payload`.

- `callId` is chosen by the plugin: opaque, at most 64 characters, and a UUID v4 is recommended. It is echoed verbatim on the `result`.
- `op` is an **open vocabulary, never an enum**. A closed one here would make every new operation a wire change, which is exactly what this frame exists to avoid. The payload's shape is the control plane's to schema and version, so the discipline moves down a level rather than disappearing.
- `op` is still a **name, never free text**: `^[a-z][a-z0-9_.-]{0,127}$` (for example `memory.recall`). It is the one caller-chosen field either side logs.
- `deadlineMs` is milliseconds relative to send, from 1 to 300,000. The companion answers by then, and if the operation has not finished it answers `timed_out` rather than letting the turn hang. A call never extends the deadline of any request.
- **Calls are concurrent, never lockstep.** Several may be in flight, and their results may arrive in any order.
- **At most 32 are in flight per connection.** Each served call is a request the companion makes to the platform with the agent's key. A call past the bound is answered at once with `upstream_failed` and a sentence saying too many requests are in progress. The error vocabulary is closed, so there is no separate "busy" class.
- 🔴 **There is no companion → plugin call.** Everything the companion starts has its own frame (`brief`, `opener`, `cancel`).
- 🔴 **`post` and `done` stay explicit frames**, permanently. The messaging path keeps its own schemas and goldens; the tunnel is for everything else.
- An op the companion serves carries its payload schemas under `ops/` (`<op>.request.schema.json`, `<op>.result.schema.json`) and goldens under `golden/ops/`, counted per op in the manifest's `ops` section. They are payloads, not frames: adding an op changes no frame and bumps no version.

#### `logs.push`

The plugin hands the companion its own log lines and its harness's. The payload is `entries`: 1 to 500 lines, each `{ts, source, level, logger?, message, attrs?}`, where `source` is `plugin` or `harness` and `level` is `debug`, `info`, `warn` or `error`.

- The companion redacts every line (the message, the logger name and every string inside `attrs`), drops harness lines below the effective harness floor and plugin lines below the plugin floor, and keeps the rest for shipping. The plugin never ships anything itself.
- The result is `{accepted, dropped, disabled, busy, harnessLevel, pluginLevel, retryAfterSeconds?}`. `harnessLevel` and `pluginLevel` are the **effective** floors — the stricter of the machine's setting and the agent's in Uffici — and the plugin applies them from then on.
- 🔴 **At most one in flight per plugin.** A second `logs.push` while one is being handled is answered **success** with `busy: true` and keeps nothing; the plugin sends it again later. `busy` is not an error class, and the class vocabulary stays closed.
- The plugin pushes every ten seconds while no call is in flight, and once more as it disconnects, bounded at two seconds and after its periodic push has stopped, so the lines that explain a restart are not lost with the process.
- `disabled: true` means shipping is off, on this machine or for the agent: the plugin drops what it queued and asks again after `retryAfterSeconds`.
- The payload never reaches a log line, and the companion logs its answer at `debug`, not `info`.

#### `tools.list`

The agent's Uffici tools in one conversation, as Uffici lists them. The payload is
`{conversationId}`; the result is `{tools}`, at most 512, each `{name, description,
parameters, permission}`.

- `name` is `^[A-Za-z0-9_-]{1,128}$`; `parameters` is the tool's JSON Schema, as Uffici wrote it, for the harness to offer its model.
- `permission` is `allow` or `ask`. An `ask` tool needs a person's approval for each call: its first call answers `needs_approval` (below) and the agent ends its turn. A tool the agent may not use is never listed.
- The companion keeps each conversation's catalog for a minute, so a plugin that lists on every turn does not read it from Uffici every turn. A change to the agent's tools reaches the next turn after that minute.
- Sent only to a companion whose `hello` lists the `tools` feature.

#### `tools.call`

One tool call on the request the plugin is answering. The payload is `{requestId,
name, arguments}` — `arguments` an object, `{}` for none; the result is `{callUrn,
result}`.

- `result` is the tool's answer as Uffici returned it. An **actionable refusal is a result, not an error**: `{"ok": false, "error": "needs_approval", "message": …}` and its kind are for the model to read and act on, and the call answers success.
- `callUrn` names the call when it ran — `urn:uffici:tool_call:<n>`, which a later `post` may cite — and is `null` when it did not (an approval was asked for).
- 🔴 **A `requestId` the companion did not deliver is refused** with `not_permitted` and a sentence, and nothing is sent to Uffici. A call answers the request it was made for, under that request's conversation; the companion never chooses one for it.
- 🔴 **A call is sent at most once.** A tool is not idempotent — "add a card" sent twice adds two cards — so the companion repeats only what happened before Uffici could run anything: obtaining its authorization for the conversation. A failure after the call left is answered as it came, as `upstream_failed` carrying Uffici's own class in `upstream` and Uffici's sentence as the `instruction`. Whether to try again is the agent's decision, told by that sentence.
- A result too large for the call bound is answered `upstream_failed` with a sentence telling the agent the tool ran and to ask for less — a narrower query, fewer results — and the call's `callUrn` in `upstream`, so the answer can still cite it. Never a truncation.
- Neither the arguments nor the result reach a log line. The companion logs one line per call: the tool's name, the request and the class.
- Sent only to a companion whose `hello` lists the `tools` feature.

#### `memory.recall`, `memory.remember`, `memory.forget`, `memory.unfold`

The agent's memory in Uffici: what was learned at the place the conversation sits, kept
by the organization rather than by the agent. Every op is bound to the request the
plugin is answering, like a tool call — a `requestId` the companion did not deliver is
refused with `not_permitted` and nothing is sent — and each is sent at most once.

- **`memory.recall`** `{requestId, query, limit?, deadlineMs?}` → `{hits, degraded, missing?}`. Every level the conversation sits under is searched — the organization, its folders, the conversation — and each hit names its `level` (`conversation`, `folder` or `organization`), never a folder. Of two near-duplicates the closer level wins. `distance` is `null` when full text found the hit; `degraded: true` says full text stood in for some or all of the answer, and `missing: ["embeddings"]` that the query itself could not be embedded. An empty `hits` with neither flag means nothing matched.
- 🔴 **`deadlineMs` is the plugin's whole budget**, and the companion answers inside it: it keeps a margin back and hands Uffici a shorter deadline still, so the answer arrives before a harness's own bound on the call. A deadline it misses is answered `upstream_failed` with a sentence saying memory could not be checked — never an empty recall that reads as "nothing remembered".
- **`memory.remember`** `{requestId, text}` → `{urn}` — ONE fact about the work, on the request's conversation. There is no level: an agent writes where it is, and a person promotes.
- **`memory.forget`** `{requestId, urn, replacementUrn?}` → `{urn, supersededBy}` — MARKS a fact an agent wrote in this conversation superseded, by a newer fact of this conversation or by the request. It never deletes; anything else is refused, carried as `upstream_failed` with Uffici's class `forget_refused` in `upstream`.
- **`memory.unfold`** `{requestId, urn, depth?, limit?}` → `{urn, supersededBy, rows}` — what a fact superseded, up to two hops back. `supersededBy: {visible: false}` means it was superseded by something this conversation cannot see.
- A failure is answered as it came, `upstream_failed` with Uffici's class in `upstream` and its sentence as the `instruction`, for the agent to relay.
- Facts, queries and hits never reach a log line. The companion logs one line per op: the op, the request and the class.
- Sent only to a companion whose `hello` lists the `memory` feature, which it announces only when it serves all four.

### `result` — companion → plugin

The one answer to a `call`. Fields: `callId`, then **exactly one** of `payload` (success) or `error`.

- Both, or neither, breaks the schema: a validating decoder reports it as `malformed_frame`, and that is how its golden is named.
- The plugin does not close the connection over it. It fails the call the result names with `malformed_payload`, so that call does not hang until its deadline.

- `error` carries three fields:
  - `class`, from a closed vocabulary of transport concerns only:
    - `unknown_op`: the companion serves no such op.
    - `not_permitted`: the op exists and this call may not make it.
    - `timed_out`: `deadlineMs` passed first.
    - `upstream_failed`: the platform refused or failed, or the connection the op needed was lost.
    - `malformed_payload`: the frame broke its bound, or the op's input did not parse.
  - `instruction`: the sentence the agent relays to the person (the platform's failure semantics: never a silent failure).
  - `upstream`: the control plane's own typed error, opaque to the wire, when there is one. A new platform error therefore never needs a wire change.
- **An unknown `op` is an answer, never a close.** The connection, and every other call on it, stays up.

What a bad call-shaped frame earns, and the invalid goldens that pin each case:

| Frame | Outcome | Golden prefix |
| --- | --- | --- |
| a `call` or `result` over 262,144 encoded bytes, with a readable `callId` | **answered**: a `malformed_payload` result for that `callId`, never a truncation | `malformed_payload--` |
| the same with no `callId` to echo | closed: `malformed_frame` | `malformed_frame--` |
| a `result` whose `callId` matches no call in flight | **discarded** by the plugin: it is late (the plugin already gave up) or it was never asked for | `unknown_call--` |
| any other schema violation in a `call` | closed: `malformed_frame` | `malformed_frame--` |
| a `result` breaking its shape (both, neither, an unknown class) | the plugin fails the call it names with `malformed_payload` and stays connected | `malformed_frame--` |

`unknown_call` is the only disposition a single frame cannot show, because it is schema-valid and wrong only against the plugin's state. The plugin's tests drive that golden through its call tracker.

## The entry

Every `delta` and `opener` carries entries, and the companion **never composes
one**: it maps the platform's bounded read one to one.

`id`, `cursor`, `at`, `author`, `kind`, `text`, and optional `attachments`.

- `author`: `type` (`member`, `agent`, `correspondent`, `system`), `id`, `label`, optional `handle` — the agent's `@slug`, so the model can address it — and `self`, true for the answering agent's own entries.
- `kind`: `message`, `tool_line`, `tombstone` — **and anything else**, see forward compatibility.
- `attachments`: `id`, `name`, `ref`. By reference; bytes never ride this wire.

## The context

A brief's `context` is composed by the platform; the companion forwards it one to
one and composes nothing.

`blocks`, `tokens`, `bounded`, and an optional `missing`.

- `blocks`, in order: the identity block first (`scope` `identity` — the agent is an AI, how the person was told so, and who the agent is), then the organization's context, each folder's from the outermost in, and the conversation's own. Each block has a `name`, a `scope`, an optional `depth`, its `tokens` and its `text` (Markdown, verbatim).
- `scope` is an open name: a scope the plugin does not know is rendered as a labelled block, never dropped.
- `tokens` is every block's cost as the platform counted it, the identity block included. The companion asks the platform for a context within HALF the plugin's `contextBudgetTokens`, so the context never crowds out the transcript. On a fresh session it then asks for an `opener` sized to `contextBudgetTokens` minus this number — never below the platform's smallest opener (1,000 tokens) nor above its largest (200,000): a fresh session with no transcript at all would be worse than a brief slightly over the declaration.
- `bounded` is true when the budget left some of the context out.
- `missing` names a part the brief should have had and does not — `part`, `class` and the `instruction` the agent relays to the person. The plugin renders each instruction inside the context; it never drops one, because a silent gap would let the agent answer as if it had been told everything. A context the companion receives but cannot read arrives the same way: no blocks, and the context named missing with the platform's own sentence.
- The plugin places the context on the **system** side of every turn, fresh or resumed, so an edit reaches the next answer — never on the user side, and the opener never on the system side.

## The per-request state machine

```text
delivered → acked(fresh | resumed) → [opening] → posting → closed{outcome}
                                                         → cancelled{reason}
                                                         → expired
```

- `delivered` — the brief was sent.
- `acked` — the plugin answered, naming `fresh` or `resumed`.
- `opening` — exists **only** after a fresh ack, and ends when the `opener` frame is sent.
- `posting` — one or more posts.
- `closed` — `done` arrived, with its outcome.
- `cancelled` — a `cancel` was sent, with its reason.
- `expired` — the deadline passed with no `done`.

Rules that hold across it:

- **Exactly one open request per conversation.**
- A `post`, `ack` or `done` is accepted **only on the connection that received the brief**, and only for that brief's conversation. Otherwise the connection closes with `wrong_connection`.
- Frames for one conversation are delivered **in order**.
- A `rejected` ack, or a reconnect with no ack, **redelivers** the brief.
- A duplicate brief for an already-closed request is a **no-op**, never a second turn.

## Close reasons

There is **no error frame**. A protocol error closes the connection with a
reason from a closed vocabulary:

| Reason | Produced by | Meaning |
| --- | --- | --- |
| `unsupported_version` | the decoder | `v` is absent or not `1` |
| `hello_required` | the server | a frame arrived before `hello` |
| `unauthorized` | the server | the token was absent or did not match |
| `already_attached` | the server | a second plugin connection |
| `unknown_request` | the server | a frame naming a request this companion never issued |
| `wrong_connection` | the server | a frame for a request delivered elsewhere |
| `malformed_frame` | the decoder | the frame does not match its schema |
| `shutting_down` | the server | the companion is stopping |

🔴 Only `unsupported_version` and `malformed_frame` can be earned by a single
frame; the rest depend on connection state and belong to the server that holds
it. The invalid goldens cover exactly the two, for that reason.

🔴 **A frame for a request the COMPANION closed earns no reason at all — it is
ignored**. A cancel and the plugin's terminal frame for the same request
cross on the wire by construction: the plugin cannot know a cancel is coming, and
by the time it arrives the `done` is already written. Closing the connection over
that would take down every OTHER request the plugin is working on, and a
supersede — a member writing again while the agent is answering — is the ordinary
case, so it would fire in normal use. The companion keeps a bounded FIFO memory of
the ids it closed itself and drops frames naming them; the plugin keeps the
mirror of it for cancels that arrive after it has finished (a later change made that side
FIFO too — an eviction that could drop the NEWEST id let a late hook send a
second terminal frame, which is the one case below that is still fatal). `unknown_request` keeps its
meaning for the case it is actually for: a peer naming a request that was never
issued, or one closed so long ago it has left that memory. A request the PLUGIN
closed with its own `done` is not in the memory — a frame after that is the plugin
talking about a turn it declared over, which is a genuine protocol error.

🔴 **`shutting_down` is not "do not come back".** It says the companion is
letting this connection go, and a companion that says it may be back in a
second: a restart, a redeploy, an operator's `systemctl restart`. **A plugin
reconnects on it**, with backoff — and whatever it had in flight is redelivered
when it does. The companion also sends this reason when it deliberately drops a
plugin without stopping, because what the plugin must do is identical.

🔴 **A stopping companion always sends `shutting_down`.** A plugin that merely
loses its socket cannot tell a deliberate stop from a crash or a severed
connection, and so cannot decide whether to reconnect. This has a consequence
for the server's implementation: a WebSocket read is **never** cancelled by the
shutdown itself, because cancelling a read breaks the connection — a half-read
frame cannot be resumed — and the reason could then never be sent. The read ends
because the connection was closed, with its reason, first.

## The local hop is free

The network hop is the expensive one, so the local hop does not repeat its work
and the harness is not degraded more than the network already does:

- The **opener** is fetched only on a fresh acknowledgement, in its own frame — never carried on every brief.
- Per-turn variable content (the opener, the reason) enters the harness on the **user** side, so the provider's prompt cache survives; the per-turn system addendum carries only what is stable across a session.
- The doorbell is acknowledged **on receipt**. Posts go out immediately; closes are asynchronous.

## Forward compatibility

🔴 **A reader ignores what it does not know.** This is a requirement, not a
tolerance: a later summary entry kind must arrive with no version bump.

- **An unknown field is ignored.** Schemas admit unknown properties deliberately; what they require is the fields that carry meaning, so a *renamed* field still fails.
- **An unknown entry `kind` renders as a labelled line.** `kind` is deliberately **not** a closed enum in the schema. Closing it would make every future kind a breaking change.
- Both are covered by a valid golden carrying an unknown field and an unknown kind, which the decoder must accept and pass through unrewritten.

Closed vocabularies — `ack.outcome`, `ack.session`, `done.outcome`,
`done.errorClass`, `cancel.reason`, `author.type`, `result.error.class`, and the close reasons — are
enums on purpose. A new value there **is** a new version.

## Bounds

| Bound | Value | Why |
| --- | --- | --- |
| `post.text`, `Entry.text` | 131,072 characters | At most 512 KiB of UTF-8, comfortably inside the platform's 4 MiB body ceiling with room for the envelope and several posts |
| posts per request | 64 | A turn wanting more is a runaway, not a long answer |
| one frame | 1 MiB | The socket reads a bounded frame or closes |
| one `call` or `result` (`MAX_CALL_PAYLOAD_BYTES`) | 262,144 encoded bytes | Inside the frame bound on purpose: over it a call is answered `malformed_payload` and the connection stays open. JSON Schema cannot state a byte ceiling, so the decoders enforce it and a golden pins it |
| `call.deadlineMs` | 1–300,000 ms | A call is bounded by its own deadline, never by the request's |
| `delta`, `opener.entries` | 5,000 entries | The platform bounds the read; this is the backstop |

## Logging

🔴 **Neither side logs a `delta`, an `opener`, a `post` body, a `call` or
`result` payload, a whole frame or the token.** A call's payload carries
transcript content and memory rows; what may be logged about a call is its `op`,
its `callId`, byte counts and the error class. What may be logged: ids, frame type names, counts
(`deltaEntryCount`, `deltaChars`), and classified error codes. A decoder error
carries the frame type and a schema location — never the value at it.
