# Uffici Companion

The companion is the one program a customer runs next to their AI-agent harness to connect
it to Uffici. It holds every connection and every secret, so the plugin for a given harness
(Hermes, OpenClaw, and others) can be a thin translator with no network code of its own.

This repository holds the design today. The code follows the design; nothing here is
implemented yet. Read this file first, then the tracking section at the end.

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
                 │ WebSocket (doorbell)  │ MCP over HTTPS
                 ▼                      ▼
              agents.<uffici host>  — the platform's agent gateway
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
- **Upstream MCP sessions.** One ticketed session per conversation the harness touches,
  opened lazily on first use and closed on idle. The platform's ticket decides what that
  session may see.
- **Presence and the doctor.** `uffici doctor` proves the doorbell, the MCP list and one tool
  call from the machine, and reports drift the platform's hygiene panel shows: other MCP
  servers configured in the harness, harness channels still live, local memory still on.
- **A fixture mode.** The companion can play a scripted agents host from a local file, so
  every plugin repository tests against a real harness plus the companion, with no platform.

## The two local surfaces

The companion exposes exactly two things on the machine, both on loopback, and nothing to
the network.

1. **A loopback MCP endpoint**, Streamable HTTP, that proxies the platform's MCP server and
   attaches the right ticket per conversation. A **stdio face** of the same binary exists for
   harnesses that spawn an MCP server per session; it talks to the daemon over loopback, never
   to the platform directly. Both Hermes and OpenClaw accept a plain-HTTP MCP server on
   loopback in their static MCP configuration.
2. **A ring feed** the plugin subscribes to. Each event is a turn brief: the request id, the
   conversation id, the reason, and the context block already composed and bounded by the
   platform. The platform's doorbell stays ids-only; the brief exists only on this local hop.

The local wire is plain JSON with a version field. The companion validates it; plugins only
read it, so a Python plugin never needs a JavaScript package.

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

**Kubernetes.** A native sidecar container in the harness pod, sharing the pod's network
namespace so loopback works unchanged. Started before the harness and stopped after it.

```yaml
spec:
  initContainers:
    - name: companion
      image: ghcr.io/uffici-ai/uffici-companion:1
      restartPolicy: Always            # native sidecar (Kubernetes 1.29+)
      env:
        - name: UFFICI_AGENT_KEY
          valueFrom: { secretKeyRef: { name: agent-smith, key: key } }
        - name: UFFICI_AGENTS_HOST
          value: agents.uffici.ai
      readinessProbe: { httpGet: { path: /healthz, port: 7411 } }
  containers:
    - name: hermes
      image: your-hermes-image        # plugin installed, memory and MCP pointed at loopback
```

The key comes from a Secret, so no pairing and no persistent volume. The stdio face does not
cross containers; in a pod the harness uses the loopback HTTP face, or an init step copies the
binary into a shared volume the harness container spawns it from.

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
- Outbound only: the WebSocket doorbell and MCP over HTTPS to the agents host. No inbound
  port on the machine.
- Tickets are minted from the key, bound to one conversation, and never leave the companion.
- Everything the harness reads or writes passes through the platform, is fenced by its ticket
  and recorded under its request. The companion executes nothing.
- The companion runs inside the customer's perimeter with their key, which is why it is open
  source: the customer can read what it does with the key and where it connects. Releases
  are signed and the build is reproducible.

## Planned repository layout

```text
uffici-companion/
  README.md            this file
  src/                 the daemon: doorbell, tickets, upstream sessions, local surfaces, doctor
  fixture/             the scripted agents host for plugin repositories' CI
  packaging/           installers, the container image, the sidecar chart snippet
  docs/                the local wire (ring brief, loopback MCP contract) and the pairing flow
```

The protocol package (doorbell envelopes, ticket claims, the tool schemas) is developed in the
platform repository and published as a public npm package this repository pins.

## Status and tracking

Nothing is implemented yet. The design above was decided with the owner on 2026-09-15 and is
tracked as epic [#1994](https://github.com/uffici-ai/velcra-platform/issues/1994) in the
platform repository (private), under the harness-agnostic platform epic #1958. That epic marks
each item as decided or as recommended and to be confirmed at breakdown, lists the candidate
slices, and is where sub-issues for this repository will be filed. Items still to confirm there:
the turn brief carrying the context block on the local hop, the folder level equalling the source
instance, the organization briefing, summary segments, and the protocol package being published
from the platform repository.

Companion plugins: `uffici-for-hermes` and `uffici-for-openclaw` first, Claude Code after.

## Working here

- [CLAUDE.md](CLAUDE.md) is the map for AI-agent sessions and
  [.claude/rules/](.claude/rules/) the conventions; [docs/conventions.md](docs/conventions.md)
  is the human map of the same set, and [docs/decisions/](docs/decisions/README.md) records why.
- Work stands on [uffici-ai Project #1](https://github.com/orgs/uffici-ai/projects/1); issues
  are filed in the platform repository (above), and a PR here closes its issue with
  `Closes uffici-ai/velcra-platform#N` ([pr-workflow.md](.claude/rules/pr-workflow.md)).
- Gitflow: `develop` is the default branch, `main` is production; `feat/` `fix/` `chore/`
  branches, Conventional Commits, squash merges ([PLAN.md](PLAN.md) has the phases).

## License

Apache-2.0. See [LICENSE](LICENSE). Uffici is a trademark of Uffici.AI; the license grants no
rights to the name.
