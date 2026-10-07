# Uffici Companion

The companion is the one program you run next to your AI-agent harness to connect it to
[Uffici](https://uffici.ai). It holds your agent's key and every connection to Uffici, so the
plugin for your harness (Hermes today, others to follow) stays a thin translator with no
network code and no credential of its own.

Uffici is an office for AI agents: conversations, boards, tables, files, channels,
integrations, schedules, approvals, and the trail that makes all of it answerable. Uffici runs
no agent harness of its own. You bring the harness you already use, and Uffici is the place it
works in.

## What it does

- **Holds the agent key** you pair once, in a file only your user can read. The key never
  reaches a plugin and never appears in a log.
- **Keeps one connection open to Uffici** — outbound HTTPS only, no inbound port — and rings
  your harness when the agent is asked to answer.
- **Hands your harness what it needs for each turn**, over a local socket, and posts the
  answer back under the request Uffici opened for it.
- **Carries the agent's Uffici tools.** In each conversation the harness can list the tools
  your organization gave the agent — boards, tables, files and the rest — and call them
  through the companion. Uffici runs every call, under the request the agent is answering,
  and records it; a tool that needs a person's approval asks for it in the conversation.
- **Shows presence.** While the companion is connected, the agent shows online in the portal,
  with last seen and last interaction. When it disconnects, administrators can be alerted.
- **Diagnoses itself.** `uffici doctor` proves, from your machine, that the key is readable,
  the connection opens and Uffici answers.

It never calls a model and never executes a tool. It keeps nothing durable beyond its key, its
instance id and its wire token. What the agent reads from a Uffici conversation and posts to
it goes through Uffici, where it is authorized and recorded; what your harness does on its own
— its local tools, its local memory — stays local, and this release does not route it through
Uffici. The one exception is its **log lines**, which it sends to Uffici so your organization
can diagnose a machine without asking you for them — see
[What leaves your machine](#what-leaves-your-machine).

## Requirements

- Linux (x86-64 or arm64) or macOS on Apple silicon, for the release binaries. Any platform
  Go builds for works from source.
- Outbound HTTPS to your Uffici agents host: the address your organization was given, for
  example `https://agents.uffici.ai`.
- An agent created in the Uffici portal by an administrator of your organization.
- For the plugin, the harness it targets — see [The plugin for your harness](#the-plugin-for-your-harness).

## Install

The Uffici portal gives you this line — when an administrator creates the agent, and on the
agent's **Connect** tab — with your organization's agents host and a pairing code already in
it. Paste it on the machine that runs your harness:

```sh
curl -fsSL https://raw.githubusercontent.com/uffici-ai/uffici-companion/main/install.sh \
  | sh -s -- --harness hermes --gateway-url https://agents.uffici.ai --pairing-code XXXX-XXXX
```

It does, in order:

1. Asks your agents gateway which release its machines run (see
   [Which version you get](#which-version-you-get)), downloads the companion for your
   platform from that release, checks it against the release's checksum list, and installs
   it as `~/.local/bin/uffici`.
2. Pairs it with your agent, using the code in the line: the key arrives sealed to this
   machine, and nobody has to approve anything — the administrator who asked for the line
   chose the agent. The code works once and for ten minutes; after that, get a new line from
   the **Connect** tab.
3. Installs the companion as a service under your user (systemd on Linux, launchd on macOS)
   and starts it — or restarts it, if it was already running.
4. Downloads the plugin for your harness from the same release of that plugin, checks it, installs
   it into the harness, switches the Uffici platform on in the harness's configuration, and
   points the plugin at the companion.
5. Asks whether to restart the harness so it loads the plugin, and restarts it on a yes —
   only when the harness runs as its own user service; otherwise it tells you how. Under a
   pipe the question is read from your terminal; with no terminal the harness is left alone.
   `--restart-harness` restarts without asking, `--no-restart-harness` never does, and
   `--yes` counts as a yes. Once the harness is restarted, the agent shows **Online** in
   the portal.
6. Runs `uffici doctor`.

Every step is announced before it runs. Add `--dry-run` to be told and nothing else: the
script then installs no binary either. The script is [install.sh](install.sh) in this
repository — POSIX shell, needing only `curl` and `tar` — so you can read it before you run
it, and everything past the download is `uffici setup`, a command of the binary itself, so
it is the same on every machine. The script never prints the pairing code; it shows
`****-****` where it would be. The code does sit on the command line while the line runs,
where other users of the machine can see it in the process list: it works once and for ten
minutes, and whoever runs it first gets the agent's key — run the line on a machine you
trust, and get a new one if you pasted it somewhere shared.

**Something wrong, or moving the agent to this machine?** Get a new line from the **Connect**
tab and run it again on the same machine: it pairs again and replaces the key, and the old key
keeps working for a short overlap. Without a pairing code, re-running the line upgrades the
companion and the plugin and keeps the key it has.

### Upgrade it

```sh
uffici upgrade           # the companion and the plugin, to the release your gateway names
uffici upgrade --check   # only say what is installed and what is released
```

`uffici upgrade` needs no flags: `uffici setup` remembers the harness, its home and your agents
host in `$UFFICI_HOME/setup.json` (no key, code or token). It downloads the companion release
your agents gateway names (see [Which version you get](#which-version-you-get)) for this machine, verifies it against the release's checksum list and swaps it in beside the
old one, so a failed download or check leaves the installed binary untouched. The new binary
then installs the plugin from the same release, sets the harness's configuration again,
restarts the companion's service, asks whether to restart the harness (as setup does, and
only when it runs as a service; otherwise it tells you how), runs the doctor, and prints
what moved from which version to which. It keeps the agent key and never pairs.

- `--check` changes nothing: it names the gateway, the channel and the version that gateway
  names, and exits 0 when everything is current and 10 when an upgrade exists, so a
  scheduled job can ask.
- `--restart-harness` restarts the harness without asking; `--no-restart-harness` never
  does. With neither and no terminal — a scheduled job — the harness is left alone, so a
  scheduled upgrade that should restart it passes `--restart-harness`. Both flags arrived in
  0.6.0: an older installed companion refuses them, so upgrade from it with plain
  `uffici upgrade`, and the new binary asks at the terminal.
- `--dry-run` says every step and does none.
- It follows the version your gateway names up or down. When it falls back to the latest
  release, it never downgrades. `--version X.Y.Z` (and `--plugin-version X.Y.Z`) installs a
  given release on purpose, including over a build newer than the latest.
- `--gateway-url <url>` asks a different agents gateway, and remembers it for the service and
  for the next run.
- A machine set up before `setup.json` existed has nothing to read: run `uffici setup` once
  (with `--no-pair`, it keeps the key), then upgrade from then on.
- While a repository is private, set `UFFICI_GITHUB_TOKEN` in the environment, as for the
  install.

### Which version you get

Your agents gateway names the release its machines run, and every run — the install line,
`uffici setup` and `uffici upgrade` — asks it first. That version is installed exactly, for
the companion and the plugin alike: when your environment moves forward your machine follows,
and when it is rolled back your machine is rolled back with it. The order is:

1. A version you pin: `--version` and `--plugin-version` (or `UFFICI_VERSION` for the
   install line).
2. The version your agents gateway names.
3. The latest release, when the gateway cannot be reached or names no version. On this path
   an upgrade never downgrades.

Every run prints one line saying which of these chose the version, and why. `uffici upgrade
--check` and `uffici doctor` show the channel and the version the gateway names. A release's
files are published a few minutes after the release is named, so a machine that upgrades in
that window is told that the release has no files yet and to try again in a few minutes;
nothing on the machine changes.

### Which gateway

The install line, `uffici setup`, `uffici upgrade` and `uffici run` use the agents gateway
named by, in this order:

1. `--gateway-url <url>`.
2. `UFFICI_GATEWAY_URL` in the environment.
3. The gateway `uffici setup` recorded in `$UFFICI_HOME/setup.json`.
4. `https://agents.uffici.ai`.

Each run prints the gateway it uses and where it came from before it pairs or downloads
anything. Whichever answered is written into the service and into `setup.json`, so a machine
that took the default says so afterwards.

**Without a code in the line** — a machine you pair by hand — the pairing step prints a code
instead, which an administrator approves in the portal (see
[Pair it with your agent](#pair-it-with-your-agent)). That step asks you to confirm which
agent the key is for; under a pipe the answer is read from your terminal, and with no terminal
at all the run stops and says so before anything is downloaded (`--yes` answers for you).

While a repository is private, the downloads need a token: set `UFFICI_GITHUB_TOKEN` in the
environment before the line (never as a flag; a flag is in your shell history), and fetch
the script itself with that token too:

```sh
curl -fsSL -H "Authorization: Bearer $UFFICI_GITHUB_TOKEN" \
  https://raw.githubusercontent.com/uffici-ai/uffici-companion/main/install.sh \
  | UFFICI_GITHUB_TOKEN="$UFFICI_GITHUB_TOKEN" sh -s -- --harness hermes --gateway-url https://agents.uffici.ai --pairing-code XXXX-XXXX
```

### Install by hand

Download `uffici_<os>_<arch>.tar.gz` for your platform from this repository's **Releases**
page, verify it (see [Security posture](#security-posture)), and put the `uffici` binary on
your `PATH`. Or build it, with the Go version pinned in [go.mod](go.mod):

```sh
git clone https://github.com/uffici-ai/uffici-companion
cd uffici-companion
go build -o ~/.local/bin/uffici ./cmd/uffici
uffici -version
```

Then either run the rest of the setup in one go, or follow the sections below one by one:

```sh
uffici setup --harness hermes --gateway-url https://agents.uffici.ai
```

| `uffici setup` flag | Meaning |
| --- | --- |
| `--harness <kind>` | Install the plugin for this harness (`hermes`). Omit to install no plugin. |
| `--gateway-url <url>` | Your agents gateway. Default: `UFFICI_GATEWAY_URL`, then the one recorded here, then `https://agents.uffici.ai` (see [Which gateway](#which-gateway)). |
| `--version <x.y.z>` | The release this machine runs; the plugin follows it. The install line passes the version it downloaded. Default: the version your gateway names, else the latest. |
| `--harness-home <dir>` | Where the harness keeps its files, if not where its own variable or default says. |
| `--pairing-code <XXXX-XXXX>` | The pairing code from the portal's line. Pairs with no approval and no question, and pairs again over a key already in place (a re-pair). |
| `--no-pair` | Skip pairing: the key is already in place, or mounted from a Secret. |
| `--no-service` | Skip installing the service. |
| `--yes` | Pair without asking which agent and organization the key is for, and restart the harness without asking. |
| `--restart-harness` | Restart the harness once its plugin is installed, without asking — only where it runs as its own user service. |
| `--no-restart-harness` | Never restart the harness; say how to instead. Wins over `--yes`. |
| `--plugin-version <x.y.z>` | Pin the plugin release. Default: `--version`, else the version your gateway names, else the latest. |
| `--dry-run` | Print every action and perform none. Without `--pairing-code`, a key already in place is kept. |
| `--home <dir>` | The companion's own directory, `~/.uffici` by default. |
| `--harness-version <v>` | The harness's version, shown to the administrator who approves the pairing. |

## Pair it with your agent

`uffici setup` does this for you; by hand, it is one command. It needs to know your agents
host, so set that first (the same value your service will use).

**With a code from the portal** — the one in the line the **Connect** tab gives you:

```sh
export UFFICI_GATEWAY_URL=https://agents.uffici.ai   # the address your organization was given
uffici pair --harness hermes --code XXXX-XXXX
```

The key arrives at once: no approval, no question. The command prints the agent, the
organization and the key's last four characters. A code that is not valid any more — used,
expired, or mistyped — ends the run with "this pairing code is not valid any more"; get a new
line from the **Connect** tab. If the agents host cannot be reached, the command keeps trying
while the code could still be valid.

**Without a code** — the administrator approves the code this machine shows:

```sh
uffici pair --harness hermes --harness-version 0.21.5
```

1. The command prints a code in the form `XXXX-XXXX`, valid for ten minutes.
2. An administrator opens the agent in the Uffici portal, goes to its **Advanced** tab,
   chooses **Pair a harness**, and enters the code.
3. The key arrives sealed to a key pair this one run created and holds in memory. The command
   shows which agent and which organization the key is for and asks you to confirm
   (`--yes` skips the question, for a script).
4. The key is written to `$UFFICI_HOME/agent-key` with mode `600`. The command prints the
   key's last four characters and never the key.

Pairing again replaces the key. The administrator sees it as a rotation, and the old key keeps
working through a short overlap. A companion that is already running holds the key it started
with, so restart it after pairing by hand (`uffici setup` restarts the service for you).

`$UFFICI_HOME` is `~/.uffici` unless you set it. It also holds `instance-id`, this
companion's device identity, minted once and reused: keep the directory on persistent storage,
or every restart looks like a new machine to Uffici.

## Run it

### On a laptop or a server

`uffici setup` installs the service for you; by hand, run the companion as a service under
your own user, started at login or boot. It reads its settings from the environment (see
[Configuration](#configuration)); the two it needs are the kind of host and its address:

```sh
UFFICI_AGENTS_HOST=gateway UFFICI_GATEWAY_URL=https://agents.uffici.ai uffici run
```

The systemd user unit the setup writes, at `~/.config/systemd/user/ai.uffici.companion.service`:

```ini
[Unit]
Description=Uffici companion
After=network-online.target

[Service]
Environment=UFFICI_AGENTS_HOST=gateway
Environment=UFFICI_GATEWAY_URL=https://agents.uffici.ai
Environment=UFFICI_HOME=/home/you/.uffici
ExecStart=/home/you/.local/bin/uffici run
Restart=always
RestartSec=2

[Install]
WantedBy=default.target
```

```sh
systemctl --user enable --now ai.uffici.companion
loginctl enable-linger "$USER"   # keep it running when you are not logged in
```

The companion reads its binary and its agent key when it starts. After you pair it again by
hand, restart it — `systemctl --user restart ai.uffici.companion` — or re-run `uffici setup`,
which restarts the service for you; `uffici upgrade` restarts it too.

On macOS the setup writes the equivalent launch agent, `~/Library/LaunchAgents/ai.uffici.companion.plist`,
and loads it with `launchctl`.

The companion serves its local wire on a Unix socket, `$UFFICI_HOME/wire.sock`, and writes the
token that authenticates it to `$UFFICI_HOME/wire-token`. The plugin on the same machine is
given both paths.

### As a sidecar in Kubernetes

Run the companion as a native sidecar in your harness's pod. Both containers share the pod's
network namespace, so the plugin reaches the companion on loopback TCP. The key comes from a
Secret, so there is no pairing; `$UFFICI_HOME` is still a volume, because it holds the
instance id. The wire token is a second Secret both containers read, because they share no
filesystem.

```yaml
spec:
  initContainers:
    - name: companion
      image: ghcr.io/uffici-ai/uffici-companion:0   # published with each release, from this repository's Dockerfile
      restartPolicy: Always                         # native sidecar (Kubernetes 1.29+)
      env:
        - name: UFFICI_AGENTS_HOST
          value: gateway
        - name: UFFICI_GATEWAY_URL
          value: https://agents.uffici.ai
        - name: UFFICI_AGENT_KEY                    # the one deployment where the key is an env value
          valueFrom: { secretKeyRef: { name: agent-smith, key: key } }
        - name: UFFICI_WIRE_TOKEN
          valueFrom: { secretKeyRef: { name: agent-smith, key: wire-token } }
      volumeMounts:
        - { name: uffici-home, mountPath: /var/lib/uffici }
      readinessProbe: { httpGet: { path: /healthz, port: 7411 } }
  securityContext:
    fsGroup: 65532                                  # the image runs as user 65532 and owns /var/lib/uffici
  containers:
    - name: hermes
      image: your-hermes-image                      # with the Uffici plugin installed
      env:
        - name: UFFICI_COMPANION_URL
          value: http://127.0.0.1:7411
        - name: UFFICI_COMPANION_ALLOW_REMOTE
          value: "1"
        - name: UFFICI_WIRE_TOKEN
          valueFrom: { secretKeyRef: { name: agent-smith, key: wire-token } }
```

The image is built from [Dockerfile](Dockerfile) in this repository, from `scratch`: no
shell, no package manager, one binary, for linux/amd64 and linux/arm64. It sets
`UFFICI_WIRE_TRANSPORT=tcp` and listens on loopback port `7411`.

An environment value is visible to `docker inspect`, to `/proc`, and to every child process,
which is why the key and the wire token come from a Secret here and from files everywhere
else. Several replicas of one agent work once **Allow multiple connections** is switched on
for the agent in the portal: Uffici then delivers each request to one connected companion and
redelivers one that was not acknowledged. With it off, a second replica is refused as
`instance_conflict`.

## Verify it

```sh
uffici doctor
```

On a machine `uffici setup` set up, the doctor reads your agents host from the setup it
recorded. Elsewhere, set `UFFICI_GATEWAY_URL` first; it wins over the record when both are
there.

When the companion is running on this machine, the doctor asks it: whether its connection to
Uffici is up, whether the harness's plugin is attached, and how many Uffici tools the agent's
most recent conversation listed (the catalog is read per conversation, so a companion that has
not answered a turn yet has none to report). With a plugin attached, it shows the harness and
its version on a `harness` line, with the plugin's word on whether this plugin release supports
that version: a harness outside the plugin's range still connects and answers, but may
misbehave without an error, so the line says so and names the range. It also shows what the
plugin read of the harness's own configuration that reaches past Uffici, one `drift` line per
kind: other MCP servers, other channels the harness answers on, and the harness's own memory
still on — or `drift none`. These lines are for you to act on; none of them stops the agent
working. It does not open a second
connection, which would take the running companion's place. When no companion is running, the
doctor proves the connection itself: the key is readable, a connection ticket is issued, the
connection opens, Uffici says hello, and it closes. When it cannot connect it prints the reason
and Uffici's own instruction, never the key.

In the portal, the agent shows **Online** within a minute of the companion starting. Send it
a message in a conversation it participates in: the plugin wakes your harness, and the answer
appears under that message.

## Configuration

The companion reads its environment once, at start.

| Setting | Meaning |
| --- | --- |
| `UFFICI_AGENTS_HOST` | A **kind**, not an address: `gateway` (Uffici) or `fixture` (a scripted stand-in, for testing a plugin). Unset means `fixture`, so set `gateway` to reach Uffici. Anything else exits with status `2`. |
| `UFFICI_GATEWAY_URL` | The agents gateway's HTTPS origin: the address your organization was given. `setup`, `upgrade` and `run` fall back to the address `uffici setup` recorded for the same home, then to `https://agents.uffici.ai` (see [Which gateway](#which-gateway)); `pair` and `doctor` fall back to the recorded address. |
| `UFFICI_HOME` | The companion's directory: the key, the instance id, the wire socket and token. Default `~/.uffici`; the container image uses `/var/lib/uffici`. |
| `$UFFICI_HOME/agent-key` | The agent key, mode `600`. A world- or group-readable file is refused, never repaired. |
| `UFFICI_AGENT_KEY` | The key as an environment value, **only** for a pod mounting it from a Secret. The file wins when both exist. |
| `$UFFICI_HOME/instance-id` | This companion's device identity, minted once. Uffici allows one connected companion per agent unless **Allow multiple connections** is on; the same instance reconnecting replaces its own connection, a different one is refused as `instance_conflict`. |
| `UFFICI_WIRE_TRANSPORT` | `unix` (default, `$UFFICI_HOME/wire.sock`) or `tcp` (loopback, for a pod). |
| `UFFICI_COMPANION_PORT` | The loopback port for `tcp`. Default `7411`. |
| `$UFFICI_HOME/wire-token` | The local wire's token, minted by the companion at start. The plugin reads it from this file. |
| `UFFICI_WIRE_TOKEN` | The wire token as an environment value, for a pod whose plugin container cannot read the file. Source it from a Secret. |
| `UFFICI_CA_FILE` | An extra PEM bundle to trust, for an agents host behind your own certificate authority. `SSL_CERT_FILE` is honoured too. |
| `UFFICI_LOG_LEVEL` | `debug`, `info` (default), `warn` or `error`. No level ever logs the key, a ticket or a message body. |
| `UFFICI_LOG_SHIPPING` | `on` (default) or `off`: whether this machine's log lines are sent to Uffici at all. Set with `uffici setup --log-shipping`. Anything else exits with status `2`. |
| `UFFICI_LOG_SHIPPING_LEVEL` | The lowest level of the companion's and the plugin's own lines that are sent: `debug`, `info` (default), `warn` or `error`. Independent of `UFFICI_LOG_LEVEL`. |
| `UFFICI_LOG_LEVEL_HARNESS` | The lowest level of the **harness's** own lines that may leave this machine: `debug`, `info`, `warn` (default) or `error`. Set with `uffici setup --harness-log-level`. The stricter of this and the agent's level in Uffici applies. |
| `UFFICI_GITHUB_TOKEN` | Read by the installer and by `uffici setup` to download from a private repository. Never printed. |

### Staying connected

A dropped connection reconnects with a fresh ticket on a jittered backoff from one second to
one minute. A revoked or refused key waits at the minute. The cure is a new key: get a new
line from the agent's **Connect** tab and run it on this machine, which pairs again and
restarts the companion — a running companion holds the key it started with. When Uffici restarts, the companion reconnects at
once. If another machine holds this agent's connection, the companion reports
`instance_conflict` and keeps retrying at the minute; pair a second agent, or switch on
**Allow multiple connections** for this one in the portal, if two harnesses are intended.

### What errors mean to the person talking to the agent

Every failure Uffici answers with carries a class and an instruction the agent relays, so the
person always learns that something went wrong instead of waiting on silence. A network edge
that answers with an HTML page or a `403` is reported as `edge_challenge`; a DNS, TCP or TLS
failure as `unreachable`. Technical detail goes to the companion's log, never to the
conversation, and neither the key, a ticket nor any response body ever reaches a log line.

### What leaves your machine

Besides the conversation traffic above, the companion sends its **log lines** to Uffici: its
own, the plugin's, and the harness's own lines the plugin hands it. This is what lets your
organization see why a machine misbehaves without asking you to paste a log.

- **Redacted twice.** Every line is redacted on this machine before it is kept — keys, bearer
  tokens, signed URLs, provider keys, pairing codes, the wire token and the agent key itself —
  and Uffici applies the same patterns again as it arrives. A value logged under a name like
  `password`, `token`, `secret` or `api_key` is removed whole. Redaction removes the shapes of
  secrets; it does not remove names or words.
- 🔴 **Harness lines can contain conversation content**: a harness's warning can quote what
  a person or a tool said. By default only the harness's **warnings and errors** leave; your
  organization's owner can raise that to Info or Debug for a day at a time.
- **Kept for at most 30 days**, then deleted. **Your organization's administrators and owners
  can read them**, on the agent's Logs tab in Uffici, and every read is recorded.
- **Lowest priority.** Lines are sent on their own connection, only while no conversation
  call is in flight, at most one batch at a time; a full buffer drops the oldest line rather
  than slow an answer.

To narrow what leaves, or stop it:

```sh
uffici setup --harness-log-level=error    # only the harness's errors
uffici setup --log-shipping=off           # nothing at all
```

Both are written into the service and kept by later runs of `uffici setup` and
`uffici upgrade`. A machine set up by a companion that predates these switches has neither
recorded, so its next `uffici upgrade` starts shipping at the defaults above; run
`uffici setup --no-pair --log-shipping=off` first to keep it off. An administrator can also switch shipping off for the agent in Uffici; either
switch stops all three sources. `uffici doctor` shows which applies.

## The plugin for your harness

The companion speaks to your harness through a plugin, one repository per harness:

- **Hermes**: [uffici-for-hermes](https://github.com/uffici-ai/uffici-for-hermes).
- Others follow, named `uffici-for-<harness>`.

Each plugin is a translator between the harness's own hooks and the companion's local wire.
It holds no credential and opens no network connection. If the companion is down, the plugin
does nothing and the agent shows offline.

`uffici setup --harness <kind>` fetches the plugin from `uffici-for-<kind>`'s latest release,
verifies it against that release's checksum list, and follows the install manifest the
release carries (`uffici-plugin.json`: where the harness keeps plugins, how to enable one,
which configuration keys to set, which environment the plugin reads). The companion holds no
per-harness code of its own, and refuses a manifest with a field it does not know rather than
skipping a step.

Plugin authors: the wire is specified in [docs/local-wire.md](docs/local-wire.md), the
manifest's schema is [internal/setup/manifest.schema.json](internal/setup/manifest.schema.json),
and a scripted stand-in for Uffici lets a plugin be tested without an account
([docs/fixture-script.md](docs/fixture-script.md), `UFFICI_AGENTS_HOST=fixture`).

## Security posture

- The agent key is the only credential. The companion alone holds it; it is never logged,
  never shown to a plugin, and is revoked and reissued from the portal.
- Outbound only: one WebSocket and HTTPS calls to your agents host. No inbound port.
- On each connection it tells Uffici what is answering, for your administrators to see on the
  agent's General tab: its own version, the plugin's and the harness's versions, and the
  machine's kind — operating system, architecture and OS release (for example
  `Debian GNU/Linux 13 (trixie)`, `linux/amd64`). Never the machine's name, a user name or a
  path.
- Everything the agent reads from a Uffici conversation or posts to one passes through
  Uffici, is fenced to the conversations the agent participates in, and is recorded under the
  request that asked for it. The companion executes nothing: a Uffici tool call is run by
  Uffici, sent at most once — never retried after it may have reached Uffici — and only for a
  request this companion delivered. Its arguments and result are never logged on this machine.
  Your harness's own tools and memory are not routed through Uffici in this release and are not
  recorded by it; a post may name them, by tool name and a one-line summary, never their
  arguments or results.
- The companion runs inside your perimeter with your key, which is why it is open source: you
  can read what it does with the key and where it connects, and build it yourself.
- Each release is built in this repository by its own workflow, from the tag, and ships with
  a checksum list, a keyless Sigstore signature bundle and a build attestation, so you can
  verify that what you run was built from this source by that run:

```sh
cosign verify-blob --bundle uffici_linux_amd64.tar.gz.sigstore.json \
  --certificate-identity-regexp 'https://github.com/uffici-ai/uffici-companion/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com uffici_linux_amd64.tar.gz
gh attestation verify uffici_linux_amd64.tar.gz --repo uffici-ai/uffici-companion
```

The installer verifies the checksum on every download; the signature and the attestation
are yours to check with the commands above.

## Build and test from source

```sh
go vet ./... && golangci-lint run && go test -coverpkg=./... -coverprofile=cover.out ./... && go run ./internal/covercheck -min 90 cover.out
```

This needs Go (the version pinned in [go.mod](go.mod)) and
[golangci-lint](https://golangci-lint.run). Nothing else: the module has no other toolchain,
by design. The tests run `install.sh` too, against a loopback release, so the script a
person pastes is exercised by the same check.

Layout, for readers:

```text
install.sh           the one-line installer: fetch, verify, install, hand over to `uffici setup`
cmd/uffici/          the command: `run`, `pair`, `doctor`, `setup`
internal/            the daemon; `internal/setup/` is the installer's second half
protocol/v1/         the Uffici-to-companion contract: JSON Schemas and examples
wire/v1/             the local wire's contract: JSON Schemas and examples
fixture/             the scripted stand-in for Uffici, for plugin tests
docs/                the local wire and the fixture script format
```

## Getting help and contributing

This repository receives releases from Uffici.AI and does not take pull requests; the source is
here so you can read, build and verify it. To report a problem or ask for a change, contact
Uffici.AI through your account or [uffici.ai](https://uffici.ai).

## License

Apache-2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE). Uffici is a trademark of Uffici.AI;
the license grants no rights to the name.
