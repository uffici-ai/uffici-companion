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
- **Shows presence.** While the companion is connected, the agent shows online in the portal,
  with last seen and last interaction. When it disconnects, administrators can be alerted.
- **Diagnoses itself.** `uffici doctor` proves, from your machine, that the key is readable,
  the connection opens and Uffici answers.

It never calls a model and never executes a tool. It keeps nothing durable beyond its key, its
instance id and its wire token. What the agent reads from a Uffici conversation and posts to
it goes through Uffici, where it is authorized and recorded; what your harness does on its own
— its local tools, its local memory — stays local, and this release does not route it through
Uffici.

## Requirements

- Linux or macOS. Any platform Go builds for works from source.
- Outbound HTTPS to your Uffici agents host: the address your organization was given, for
  example `https://agents.uffici.ai`.
- An agent created in the Uffici portal by an administrator of your organization.
- For the plugin, the harness it targets — see [The plugin for your harness](#the-plugin-for-your-harness).

## Install

Build the binary from this repository with the Go version pinned in [go.mod](go.mod):

```sh
git clone https://github.com/uffici-ai/uffici-companion
cd uffici-companion
go build -o ~/.local/bin/uffici ./cmd/uffici
uffici -version
```

The binary is static: no runtime, no libraries, one file. Release binaries, with checksums and
a signature so you can verify that what you run was built from this source, are planned for
this repository's Releases page; until they are there, building from source is the install.

## Pair it with your agent

Pairing puts the agent's key on this machine without anyone ever seeing it. It needs to know
your agents host, so set that first (the same value your service will use):

```sh
export UFFICI_GATEWAY_URL=https://agents.uffici.ai   # the address your organization was given
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
working through a short overlap so a running companion is not cut off.

`$UFFICI_HOME` is `~/.uffici` unless you set it. It also holds `instance-id`, this
companion's device identity, minted once and reused: keep the directory on persistent storage,
or every restart looks like a new machine to Uffici.

## Run it

### On a laptop or a server

Run the companion as a service under your own user, started at login or boot. It reads its
settings from the environment (see [Configuration](#configuration)); the two it needs are
the kind of host and its address:

```sh
UFFICI_AGENTS_HOST=gateway UFFICI_GATEWAY_URL=https://agents.uffici.ai uffici run
```

A systemd user unit, at `~/.config/systemd/user/uffici.service`:

```ini
[Unit]
Description=Uffici companion
After=network-online.target

[Service]
Environment=UFFICI_AGENTS_HOST=gateway
Environment=UFFICI_GATEWAY_URL=https://agents.uffici.ai
ExecStart=%h/.local/bin/uffici run
Restart=always
RestartSec=2

[Install]
WantedBy=default.target
```

```sh
systemctl --user enable --now uffici
loginctl enable-linger "$USER"   # keep it running when you are not logged in
```

On macOS, the equivalent is a launchd agent in `~/Library/LaunchAgents/` with the same
command and environment.

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
      image: your-registry/uffici-companion   # built from this repository's Dockerfile
      restartPolicy: Always                    # native sidecar (Kubernetes 1.29+)
      env:
        - name: UFFICI_AGENTS_HOST
          value: gateway
        - name: UFFICI_GATEWAY_URL
          value: https://agents.uffici.ai
        - name: UFFICI_AGENT_KEY               # the one deployment where the key is an env value
          valueFrom: { secretKeyRef: { name: agent-smith, key: key } }
        - name: UFFICI_WIRE_TOKEN
          valueFrom: { secretKeyRef: { name: agent-smith, key: wire-token } }
      volumeMounts:
        - { name: uffici-home, mountPath: /var/lib/uffici }
      readinessProbe: { httpGet: { path: /healthz, port: 7411 } }
  securityContext:
    fsGroup: 65532                             # the image runs as user 65532 and owns /var/lib/uffici
  containers:
    - name: hermes
      image: your-hermes-image                 # with the Uffici plugin installed
      env:
        - name: UFFICI_COMPANION_URL
          value: http://127.0.0.1:7411
        - name: UFFICI_COMPANION_ALLOW_REMOTE
          value: "1"
        - name: UFFICI_WIRE_TOKEN
          valueFrom: { secretKeyRef: { name: agent-smith, key: wire-token } }
```

The image is built from [Dockerfile](Dockerfile) in this repository, from `scratch`: no
shell, no package manager, one binary. It sets `UFFICI_WIRE_TRANSPORT=tcp` and listens on
loopback port `7411`. A published image is planned alongside the release binaries.

An environment value is visible to `docker inspect`, to `/proc`, and to every child process,
which is why the key and the wire token come from a Secret here and from files everywhere
else. Several replicas of one agent work once **Allow multiple connections** is switched on
for the agent in the portal: Uffici then delivers each request to one connected companion and
redelivers one that was not acknowledged. With it off, a second replica is refused as
`instance_conflict`.

## Verify it

```sh
export UFFICI_GATEWAY_URL=https://agents.uffici.ai   # unless already in your shell
uffici doctor
```

The doctor proves the connection from this machine: the key is readable, a connection ticket
is issued, the connection opens, Uffici says hello, and it closes. When it cannot connect it
prints the reason and Uffici's own instruction, never the key. It briefly takes the place of a
running companion on the same machine, which reconnects by itself afterwards.

In the portal, the agent shows **Online** within a minute of the companion starting. Send it
a message in a conversation it participates in: the plugin wakes your harness, and the answer
appears under that message.

## Configuration

The companion reads its environment once, at start.

| Setting | Meaning |
| --- | --- |
| `UFFICI_AGENTS_HOST` | A **kind**, not an address: `gateway` (Uffici) or `fixture` (a scripted stand-in, for testing a plugin). Unset means `fixture`, so set `gateway` to reach Uffici. Anything else exits with status `2`. |
| `UFFICI_GATEWAY_URL` | The agents host's HTTPS origin: the address your organization was given. Required with `gateway`, including for `pair` and `doctor`. |
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

### Staying connected

A dropped connection reconnects with a fresh ticket on a jittered backoff from one second to
one minute. A revoked or refused key waits at the minute; an administrator re-issuing it is
the only cure, and no restart is needed. When Uffici restarts, the companion reconnects at
once. If another machine holds this agent's connection, the companion reports
`instance_conflict` and keeps retrying at the minute; pair a second agent, or switch on
**Allow multiple connections** for this one in the portal, if two harnesses are intended.

### What errors mean to the person talking to the agent

Every failure Uffici answers with carries a class and an instruction the agent relays, so the
person always learns that something went wrong instead of waiting on silence. A network edge
that answers with an HTML page or a `403` is reported as `edge_challenge`; a DNS, TCP or TLS
failure as `unreachable`. Technical detail goes to the companion's log, never to the
conversation, and neither the key, a ticket nor any response body ever reaches a log line.

## The plugin for your harness

The companion speaks to your harness through a plugin, one repository per harness:

- **Hermes**: [uffici-for-hermes](https://github.com/uffici-ai/uffici-for-hermes).
- Others follow, named `uffici-for-<harness>`.

Each plugin is a translator between the harness's own hooks and the companion's local wire.
It holds no credential and opens no network connection. If the companion is down, the plugin
does nothing and the agent shows offline. Plugin authors: the wire is specified in
[docs/local-wire.md](docs/local-wire.md), and a scripted stand-in for Uffici lets a plugin be
tested without an account ([docs/fixture-script.md](docs/fixture-script.md), `UFFICI_AGENTS_HOST=fixture`).

## Security posture

- The agent key is the only credential. The companion alone holds it; it is never logged,
  never shown to a plugin, and is revoked and reissued from the portal.
- Outbound only: one WebSocket and HTTPS calls to your agents host. No inbound port.
- Everything the agent reads from a Uffici conversation or posts to one passes through
  Uffici, is fenced to the conversations the agent participates in, and is recorded under the
  request that asked for it. The companion executes nothing. Your harness's own tools and
  memory are not routed through Uffici in this release and are not recorded by it.
- The companion runs inside your perimeter with your key, which is why it is open source: you
  can read what it does with the key and where it connects, and build it yourself.

## Build and test from source

```sh
go vet ./... && golangci-lint run && go test -coverpkg=./... -coverprofile=cover.out ./... && go run ./internal/covercheck -min 90 cover.out
```

This needs Go (the version pinned in [go.mod](go.mod)) and
[golangci-lint](https://golangci-lint.run). Nothing else: the module has no other toolchain,
by design.

Layout, for readers:

```text
cmd/uffici/          the command: `run`, `pair`, `doctor`
internal/            the daemon
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
