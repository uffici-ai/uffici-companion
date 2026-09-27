package gateway

import (
	"math/rand/v2"
	"time"
)

// The reconnection bounds (#2077): jittered, from one second to one minute.
const (
	BackoffFloor   = time.Second
	BackoffCeiling = 60 * time.Second
)

// Why a doorbell connection ended, as the reconnect policy reads it.
type endKind int

const (
	// endTransient — a dropped socket, a restart, an unreachable host, an idle
	// timeout: reconnect on the ordinary backoff.
	endTransient endKind = iota
	// endConflict — `instance_conflict` (4409) or `replaced`: another connection
	// holds this agent.
	endConflict
	// endRefused — the key itself was refused or revoked. Nothing this process
	// does will change that; only an administrator can. Retried at the ceiling,
	// so a re-issued key reconnects within a minute with no restart.
	endRefused
	// endShutdown — the gateway is draining (`server_shutdown`, 1001): another
	// replica is ready now, so reconnect promptly.
	endShutdown
	// endHarnessKnown — this side closed a connection minted with the harness
	// `unknown` because its facts are known now (#2105). Not a fault: the
	// doorbell redials at once, never consulting this policy, and reports no
	// disconnect.
	endHarnessKnown
)

// backoff is the reconnect policy's state across attempts.
//
// 🔴 **A first `instance_conflict` is retried on the ORDINARY backoff, and only
// a second in a row goes to the ceiling** (#2094). The gateway answers 4409 for
// two different things: a DIFFERENT instance holding the agent (a real conflict,
// which retrying fast only hammers), and THIS instance's own reconnect failing
// to take the per-agent gate lock within two seconds because another upgrade
// for the agent — very often this companion's previous socket, still being torn
// down on another replica — was holding it. The second is transient and clears
// in seconds; the close code cannot tell them apart. So the first 4409 is
// treated as the transient one, and a repeat is taken as the conflict it
// almost certainly is — and relayed, every time, so a person is told.
type backoff struct {
	attempt  int
	conflict int
	// jitter returns a value in [0, 1). Injected so tests are exact.
	jitter func() float64
}

func newBackoff() *backoff {
	return &backoff{jitter: rand.Float64}
}

// connected resets the ordinary step: a connection that reached `hello` proves
// the key and the path work.
//
// 🔴 It does NOT reset the conflict streak (#2077 review). Two processes sharing
// one `$UFFICI_HOME` — one instance id — each reach `hello` and are then
// `replaced` by the other; resetting the streak at `hello` made every
// replacement look like a first conflict, and the pair swapped about once a
// second for ever. Only a connection that STAYED up is evidence the conflict
// is over (`stable`).
func (b *backoff) connected() {
	b.attempt = 0
}

// stable ends a conflict streak: the connection held for at least a heartbeat.
func (b *backoff) stable() {
	b.conflict = 0
}

// next returns how long to wait before the next dial, given why the last
// connection ended and the delay the platform named, if any.
func (b *backoff) next(kind endKind, retryAfter time.Duration) time.Duration {
	var delay time.Duration
	switch kind {
	case endShutdown:
		b.conflict = 0
		delay = BackoffFloor
	case endRefused:
		b.conflict = 0
		delay = BackoffCeiling
	case endConflict:
		b.conflict++
		if b.conflict >= 2 {
			delay = BackoffCeiling
		} else {
			delay = b.exponential()
		}
	default:
		b.conflict = 0
		delay = b.exponential()
	}
	if retryAfter > delay {
		delay = retryAfter
	}
	return delay
}

// exponential is the ordinary step: doubling from the floor to the ceiling,
// with equal jitter — half fixed, half random — so a fleet of companions a
// restart disconnected at once does not reconnect at once.
func (b *backoff) exponential() time.Duration {
	base := BackoffFloor << min(b.attempt, 6)
	if base > BackoffCeiling {
		base = BackoffCeiling
	}
	b.attempt++
	half := base / 2
	return half + time.Duration(b.jitter()*float64(half))
}
