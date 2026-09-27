# Uffici Companion

The companion is the one program a customer runs next to their AI-agent harness to connect
it to Uffici. It holds every connection and every secret, so the plugin for a given harness
(Hermes, OpenClaw, and others) can be a thin translator with no network code of its own.

The local wire, the request state machine, the fixture host, the real gateway host and
pairing are built; the loopback MCP endpoint and memory are not yet. Read this file first,
then [Configuration](#configuration) to run it.

## What it is

Uffici is an office for AI agents: conversations, boards, tables, files, channels, memory,
integrations, schedules, approvals, and the trail that makes all of it answerable. Uffici runs
no agent harness of its own. A customer brings the harness they already use, and the platform
is the place that harness works **in**.

Two pieces make that connection:

- **The companion** (this repository): a small daemon on the harness's machine, or a sidecar
  container in its pod. It owns the agent key, the doorbell, the tickets, the sessions to the
  platform, presence and a doctor. It exposes two local surfaces and nothing to the network.
- **A plugin per harness** (`uffici-for-hermes`, `uffici-for-openclaw`, `uffici-for-<harness>`,
  each in its own repository): a format translator between the harness's native hooks and the
  companion. No network client, no credential, no retry logic, no schema of its own.

The split is deliberate. Plugins are maintained against a third party's release cadence, so
they must stay trivial. Everything that can change on our side lives in the companion, on our
cadence, in one codebase.

## What it is not

- Not a model loop, not an agent framework, not a harness. It never calls a model.
- Not a tool executor. Every tool call is forwarded to the platform, which executes it,
  authorizes it, audits it and returns the result.
- Not a store. Nothing durable lives in the companion or in the harness. Memory, context,
  summaries and large tool results are the platform's, so a customer who changes harness
  loses nothing.
- Not a channel. WhatsApp, mail and the rest terminate on the platform. The harness only ever
  sees a conversation through the platform.

## How it works

```text
                    customer machine or pod
   ┌───────────────────────────────────────────────────────┐
   │   harness (Hermes, OpenClaw, ...)                     │
   │      └─ uffici-for-<harness>  (plugin, no network)    │
   │             │ ring feed            │ loopback MCP     │
   │             ▼                      ▼                  │
   │   uffici-companion  (key, doorbell, tickets, sessions)│
   └─────────────┬──────────────────────┬──────────────────┘
                 │ WebSocket (doorbell)  │ our JSON control plane (HTTPS)
                 ▼                      ▼
              agents.<uffici host>  — the platform's agents-gateway, companion only
```

**Connecting.** An admin creates the agent in the portal and reveals its key once. The
customer installs the companion, pairs it with the key (or mounts the key from a Secret in a
cluster), and installs the plugin for their harness. The agent shows online within a minute.

**Answering.** The platform never lets an agent post freely. Every agent message answers a
request the platform opened on a recorded human cause: a member's message, a card arrival, a
schedule a person configured, a webhook. When the platform's wake rule selects this agent, the
gateway rings the companion's doorbell with ids and a reason, never content. The companion
fetches the conversation's context through MCP, then hands the plugin a **turn brief** on the
local ring feed. The plugin opens or resumes a harness session for that conversation and places
the brief where the harness takes a system prompt. The harness reads what it may see and answers
through the MCP tools, which the companion proxies to the platform with the right ticket. The
answer, every tool call and every read are recorded by the platform under the request.

**Staying online.** The doorbell connection is presence. The companion sends heartbeats and
acknowledgements; the platform shows Online, Last seen and Last interaction, and alerts admins
when an agent is offline or unresponsive.

## What the companion owns

- **The agent key.** Paired once on a laptop or server, mounted from a Secret in a cluster.
  It never reaches a plugin and never appears in a log.
- **The doorbell.** One WebSocket connection per key per machine: connection ticket, ring
  envelopes carrying ids and a reason, acknowledgements, redelivery of unacknowledged rings
  on reconnect, heartbeat, server-side disconnect with a reason. The client sends
  acknowledgements and nothing else. One connection is one request claim, so one daemon per
  machine avoids competing claims.
- **Tickets.** Session tickets are minted from the key and never leave the companion.
- **One ticket per conversation.** Minted from the key, used on the control plane and on
  every tool call the companion forwards for that conversation; the platform's fence
  decides what it may see. The companion never speaks MCP to the platform — its harness
  speaks MCP to the companion on loopback, and the companion translates onto our
  protocol.
- **Presence and the doctor.** `uffici doctor` proves the doorbell, the MCP list and one tool
  call from the machine, and reports drift the platform's hygiene panel shows: other MCP
  servers configured in the harness, harness channels still live, local memory still on.
- **A fixture mode.** The companion can play a scripted agents host from a local file, so
  every plugin repository tests against a real harness plus the companion, with no platform.

## The two local surfaces

The companion exposes exactly two things on the machine, both on loopback, and nothing to
the network.

1. **A loopback MCP endpoint**, Streamable HTTP — the only MCP server a plugin harness
   ever sees — that translates each tool call onto the platform's JSON control plane
   with the right ticket per conversation. A **stdio face** of the same binary exists for
   harnesses that spawn an MCP server per session; it talks to the daemon over loopback, never
   to the platform directly. Both Hermes and OpenClaw accept a plain-HTTP MCP server on
   loopback in their static MCP configuration.
2. **A ring feed** the plugin subscribes to. Each event is a turn brief: the request id, the
   conversation id, the reason, and the context block already composed and bounded by the
   platform. The platform's doorbell stays ids-only; the brief exists only on this local hop.

The local wire is plain JSON with a version field. The companion validates it; plugins only
read it, so a Python plugin never needs a JavaScript package.

Beside the messaging frames, a generic **call/result pair** carries any platform
control-plane operation from the plugin through the companion, under an open `op` name, so
a new operation never needs a new wire version (#2028, [docs/local-wire.md](docs/local-wire.md)).
The tunnel ships before its first operation; conversation-scoped memory is its first
consumer.

## Binding a call to a conversation

One harness serves many conversations concurrently and its MCP configuration is static, so
the companion must know which conversation a local call belongs to. Two modes, same binary:

- **Per-session stdio pinning.** Where the harness spawns an MCP server per session (OpenClaw
  does), the plugin launches the stdio face with the conversation id and the session is pinned
  without the model naming anything.
- **Explicit conversation id.** Everywhere else, the conversations-family tools carry the
  conversation id explicitly, which the platform's read and post tools already require. The
  companion routes the call to that conversation's upstream session.

In both modes the platform's ticket is the fence, so the fence holds twice.

## Context and memory through the companion

**Context.** The platform composes one context block from three levels: the organization's
briefing, the folder (the source instance the conversation belongs to: a channel, a board, the
portal), and the conversation's own context. The harness receives one fenced block in the turn
brief and never interprets the levels. Admins edit each level in the portal and see a token count
per level. The block is fetched per request, so an edit lands on the next answer with no deploy.

**Memory.** Memory stays in the platform, in its organizational scopes, with the closest scope
winning on a conflict. The companion exposes the platform's memory tools on the loopback MCP
endpoint (search, remember with a scope, forget). Both Hermes and OpenClaw have a single-select
memory provider slot; the plugin registers Uffici there, so recall happens before every model
call without the model asking, and every write goes through the platform. Local memory is
switched off by the plugin's policy. A platform-side extractor runs regardless of the plugin,
so memory exists whether the harness cooperates or not.

**History.** A late session opens small: the context block, the conversation's summary
segments, and the recent unsummarized messages. An agent can unfold the messages behind any
segment, and large tool results come back as a gist plus a reference it can recall later.
Compaction of the harness's own session context stays the harness's business.

## Running it

**Laptop or server.** A user service (launchd or systemd) started by the installer, with
`uffici pair` and `uffici doctor`. Outbound HTTPS to the agents host only. No inbound port.

`uffici pair [--harness hermes --harness-version 0.21.4]` gets the agent key onto this
machine without it ever being shown: it prints a code (`XXXX-XXXX`, valid ten minutes), an
administrator enters it on the agent's Advanced tab in the portal (**Pair a harness**), and
the key arrives sealed to a key pair this one run made and held in memory. It is opened
here — but only after the command has shown which agent and which organization the key is
for and you have said yes (`--yes` skips the question, for a script) — and written to
`$UFFICI_HOME/agent-key` at mode `600`. It prints the key's last four characters and never
the key. Pairing again replaces the key — the approval is a
rotation, and the old key keeps working through the overlap.

**Kubernetes.** A native sidecar container in the harness pod, sharing the pod's network
namespace so loopback works unchanged. Started before the harness and stopped after it.

```yaml
spec:
  initContainers:
    - name: companion
      image: ghcr.io/uffici-ai/uffici-companion:1
      restartPolicy: Always            # native sidecar (Kubernetes 1.29+)
      env:
        - name: UFFICI_AGENTS_HOST
          value: gateway               # a KIND: gateway or fixture, never a hostname
        - name: UFFICI_GATEWAY_URL
          value: https://agents.uffici.ai
        - name: UFFICI_AGENT_KEY       # the one deployment where the key is an env value
          valueFrom: { secretKeyRef: { name: agent-smith, key: key } }
      volumeMounts:
        - { name: uffici-home, mountPath: /var/lib/uffici }   # keeps the instance id
      readinessProbe: { httpGet: { path: /healthz, port: 7411 } }
  containers:
    - name: hermes
      image: your-hermes-image        # plugin installed, memory and MCP pointed at loopback
```

The key comes from a Secret, so no pairing. `$UFFICI_HOME` is still a volume, because
it holds the **instance id** (see [Configuration](#configuration)); mount it with
`fsGroup: 65532`. The stdio face does not cross containers; in a pod the harness uses the
loopback HTTP face, or an init step copies the binary into a shared volume the harness
container spawns it from.

**Replicas.** Several replicas of one agent work by construction: the gateway delivers each
ring to one open connection of that key and redelivers an unacknowledged ring. A conversation
whose rings land on different pods loses only the harness's local session context; the platform
hands the new session the transcript, the summary segments and the memory hits.

**Hosted harnesses** with no machine connect to the platform's MCP directly and do not use the
companion.

## The plugins

Each `uffici-for-<harness>` repository holds one plugin at its root, because the harness
installers read a plugin from a repository root. A plugin does four translations and nothing
else:

1. **A ring becomes a session.** Subscribe to the ring feed; open or resume a harness session
   for the conversation; put the brief where the harness takes a system prompt (Hermes:
   a session-start hook; OpenClaw: the prompt-build hook).
2. **The memory seam becomes companion calls.** Hermes's memory provider (`memory.provider`),
   OpenClaw's memory slot (`plugins.slots.memory`). Prefetch before each model call, tools for
   remember and search. The turn-sync hook is a no-op, because the agent's post already reaches
   the transcript through MCP. Hermes's pre-compaction checkpoint is satisfied by the platform
   holding the transcript.
3. **The MCP configuration becomes one pointer** at the loopback endpoint or the stdio face.
4. **The harness's defaults become ours.** Local memory off, no second memory provider, and
   the doctor reading the rest.

Both harnesses also install the same vendor-neutral package format (a manifest, skills, and an
MCP list that may point at a Streamable HTTP server). The MCP pointer and a skill teaching the
model to read context first ship as that package with no code; the native plugin adds the hooks
and the memory provider on top.

The test that a plugin stayed simple: if the companion is down, the plugin does nothing and the
agent shows offline. A harness release can only break the hook wiring, never the wire.

## Security posture

- The key is the only credential, held by the companion alone, never logged, never shown
  to a plugin, revoked and reissued from the portal.
- Outbound only: the WebSocket doorbell and the JSON control plane over HTTPS to the
  agents host. No inbound port on the machine. (A harness with no companion uses the
  platform's separate `tools.<domain>` MCP face — tools only, never conversations.)
- Tickets are minted from the key, bound to one conversation, and never leave the companion.
- Everything the harness reads or writes passes through the platform, is fenced by its ticket
  and recorded under its request. The companion executes nothing.
- The companion runs inside the customer's perimeter with their key, which is why it is open
  source: the customer can read what it does with the key and where it connects. Releases
  are signed and the build is reproducible.

## Configuration

The companion reads its environment once, at start. Everything the gateway host needs:

| Setting | Meaning |
| --- | --- |
| `UFFICI_AGENTS_HOST` | A **kind**, not a host: `gateway` (the platform) or `fixture` (a scripted host, for plugin tests). Anything else exits `2` — never a quiet fixture. |
| `UFFICI_GATEWAY_URL` | The agents host's HTTPS origin: `https://agents.uffici.dev`, or `https://agents.localhost` on the platform's local stack. |
| `$UFFICI_HOME/agent-key` | The agent key, mode `600`. A world- or group-readable file is refused, never repaired. |
| `UFFICI_AGENT_KEY` | The key as an environment value — **only** for a pod mounting it from a Secret. The file wins when both exist, because an environment value shows in `docker inspect` and `/proc/<pid>/environ` and is inherited by every child process. |
| `$UFFICI_HOME/instance-id` | This companion's device id, minted once and reused. The platform's one-key-one-harness gate compares it: the same instance reconnecting replaces its own connection; a different one is refused `instance_conflict`. **Keep `$UFFICI_HOME` on a volume** in a container or an LXC, or every restart looks like a second harness. |
| `UFFICI_CA_FILE` | An extra PEM bundle to trust — the local stack's dev CA. Go also honours `SSL_CERT_FILE`. |

`$UFFICI_HOME` defaults to `~/.uffici`; the container image sets `/var/lib/uffici`.

**`uffici doctor`** proves the doorbell from this machine: the key is readable, a
connection ticket mints, the doorbell opens and says hello, and it closes. It prints the
disconnect reason and the platform's instruction when it cannot connect, never the key.
It probes with this companion's own instance id, so a daemon already running here is
replaced for a moment and reconnects by itself.

**Staying connected.** A dropped doorbell reconnects with a fresh connection ticket on a
jittered backoff from one second to one minute. A revoked or refused key waits at the
minute (an administrator re-issuing it is the only cure, and no restart is needed). A
platform restart reconnects at once. `instance_conflict` is relayed every time, and
retried on the ordinary step the **first** time — the platform also answers it when this
companion's own reconnect loses a two-second race for the agent's gate — then held at the
minute while it repeats. A socket the companion gives up on is closed before the next
dial, so the platform never holds two connections for one instance.

**Errors are classes.** Every failure the platform answers carries a class and an
instruction the agent relays to the person; the companion hands both to the plugin on a
`failed` cancel and closes the request as an error, so the person learns the agent did
not answer. An HTML page or a `403` from the network's edge is `edge_challenge`, a DNS,
TCP or TLS failure is `unreachable`, and neither the key, a ticket nor any response body
ever reaches a log line.

**Coming from wave 1.** The fixture is no longer the default route to a platform:
`UFFICI_AGENTS_HOST=gateway` and `UFFICI_GATEWAY_URL` reach the real one, with the key as
the file above. The fixture stays, for plugin repositories' CI and for `--once` runs.

## Repository layout

The companion is written in Go and ships as one static binary per operating
system and architecture (`CGO_ENABLED=0`), with a scratch image for the sidecar.

```text
uffici-companion/
  README.md            this file
  go.mod               module github.com/uffici-ai/uffici-companion
  cmd/uffici/          the command: the daemon, `uffici doctor`
  internal/            the daemon: the request machine, the local wire, the hosts
  internal/build/      the identity a release stamps into the binary
  internal/protocol/   the platform protocol's Go structs, pinned to protocol/v1
  internal/infra/gateway/  the real agents host, and its test double
  protocol/v1/         the platform-to-companion contract (a byte-identical copy)
  wire/v1/             the local wire's contract
  fixture/             the scripted agents host for plugin repositories' CI
  docs/                the local wire and the fixture script format
```

The platform-to-companion contract is **JSON Schema plus goldens** in
`protocol/v1/`, a byte-identical copy of what the platform emits from its zod
definitions. The companion's structs are hand-written and pinned against that
copy, the local wire's precedent: a code generator would be a second toolchain in
a module whose purpose is to need none. There is no npm protocol package.

## Status and tracking

Built: the local wire (including the call/result tunnel, #2028), the request state machine,
the fixture host, and the gateway host over the doorbell and the JSON control plane (#2077),
and pairing (`uffici pair`, #1966).
Not yet: the loopback MCP endpoint and tool calls (stage 3), memory (stage 4), installers and signed
releases. The design was decided with the owner on 2026-09-15 and is tracked privately, in
the Uffici platform repository, along with every sub-issue for this module.

Companion plugins: `uffici-for-hermes` and `uffici-for-openclaw` first, Claude Code after.

## Check

One entry, in this module's own toolchain. It is what CI runs:

```sh
go vet ./... && golangci-lint run && go test -coverpkg=./... -coverprofile=cover.out ./... && go run ./internal/covercheck -min 90 cover.out
```

Requires Go (the version pinned in [go.mod](go.mod)) and
[golangci-lint](https://golangci-lint.run). The Markdown link guard runs inside
`go test` — this module depends on no other toolchain, by design.

## Working here

🔴 **This repository receives releases; it is not where the work happens.**
The companion is developed in the Uffici platform repository (private) in its
`uffici/` tree, under the platform's own rules, skills and review workflows.
A release dispatched there exports this module's subtree here as a single squashed
commit against an allow-list and tags it — no history leaves, nothing is force-pushed.

So there are deliberately no rules, skills, agents, review workflows or plan
file here, and a pull request opened here has no reader. File the issue and the
change in the platform repository, where the work is tracked.

What this repository does own is its **check**, which its CI runs and the
platform's `pnpm check:uffici` delegates to — see [CLAUDE.md](CLAUDE.md).

## License

Apache-2.0. See [LICENSE](LICENSE). Uffici is a trademark of Uffici.AI; the license grants no
rights to the name.
