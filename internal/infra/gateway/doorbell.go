package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/protocol"
)

// DoorbellPath is where the gateway upgrades the doorbell.
const DoorbellPath = "/v1/doorbell"

// maxFrameBytes bounds one server frame. Rings are a few hundred bytes of ids.
const maxFrameBytes = 64 << 10

// Close codes the gateway sends (the protocol's DISCONNECT_CLOSE_CODES).
const (
	closeUnauthorized     = 4401
	closeKeyRevoked       = 4403
	closeIdle             = 4408
	closeInstanceConflict = 4409
	closeProtocolError    = 4400
)

// Reasons OnDisconnect reports that are the companion's, not the platform's:
// the socket ended with no `disconnect` frame to name why.
const (
	// DisconnectConnectionLost — the socket died, or stopped carrying the
	// server's heartbeat.
	DisconnectConnectionLost = "connection_lost"
	// DisconnectUnauthorized — the upgrade or a ticket mint was refused.
	DisconnectUnauthorized = "unauthorized"
	// DisconnectReplaced — the gateway replaced this socket with a newer one
	// from the same instance id.
	DisconnectReplaced = "replaced"
)

// doorbell is the connection loop behind one claim.
type doorbell struct {
	host     *Host
	handlers contracts.Handlers
	cancel   context.CancelFunc
	done     chan struct{}

	// frames is the hand-off from the socket reader to the handlers: the reader
	// never runs a handler itself, because a handler that does I/O would stop
	// the reads — and with them the pong replies the gateway's heartbeat counts.
	// FIFO, so a cancel is never handled before the ring it withdraws.
	frames chan any
}

// run dials, reads and redials until the claim is closed.
func (d *doorbell) run(ctx context.Context) {
	defer close(d.done)
	dispatched := make(chan struct{})
	go d.dispatch(dispatched)
	defer func() {
		close(d.frames)
		<-dispatched
	}()

	policy := d.host.backoff
	for ctx.Err() == nil {
		kind, retryAfter, reason := d.connectOnce(ctx, policy)
		d.host.setDoorbell("")
		if ctx.Err() != nil {
			return
		}
		// Reporting a harness that became known is not a disconnect: no handler
		// hears of it and no backoff is spent (#2105).
		if kind == endHarnessKnown {
			continue
		}
		if !d.enqueue(ctx, disconnected(reason)) {
			return
		}
		delay := policy.next(kind, retryAfter)
		d.host.log.Info("the doorbell will reconnect",
			slog.String("reason", reason), slog.Duration("after", delay))
		if d.host.sleep(ctx, delay) != nil {
			return
		}
	}
}

// enqueue hands a frame to the handlers, or gives up when the claim closes — a
// handler stuck on I/O must never pin the connection loop, or `claim.Close`
// behind it.
func (d *doorbell) enqueue(ctx context.Context, frame any) bool {
	select {
	case d.frames <- frame:
		return true
	case <-ctx.Done():
		return false
	}
}

// disconnected is a frame-queue entry carrying OnDisconnect's reason.
type disconnected string

func (d *doorbell) dispatch(finished chan<- struct{}) {
	defer close(finished)
	for frame := range d.frames {
		switch value := frame.(type) {
		case protocol.Ring:
			d.host.remember(value.RequestID, value.ConversationID)
			d.handlers.OnRing(toRing(value))
		case protocol.Cancel:
			d.handlers.OnCancel(value.RequestID, contracts.CancelReason(value.Reason))
		case disconnected:
			d.handlers.OnDisconnect(string(value))
		}
	}
}

// connectOnce is one connection's whole life, and why it ended.
func (d *doorbell) connectOnce(ctx context.Context, policy *backoff) (endKind, time.Duration, string) {
	h := d.host
	minted := h.harness()
	ticket, err := h.api.mintConnect(ctx, minted, h.version, h.plugin(), h.hostFacts)
	if err != nil {
		return mintEnd(err)
	}

	lastPing := make(chan struct{}, 1)
	conn, resp, err := websocket.Dial(ctx, h.api.base.JoinPath(DoorbellPath).String(), &websocket.DialOptions{
		HTTPClient: h.api.http,
		HTTPHeader: http.Header{
			"Authorization": []string{"Bearer " + ticket},
			"User-Agent":    []string{h.api.userAgent},
		},
		OnPingReceived: func(context.Context, []byte) bool {
			select {
			case lastPing <- struct{}{}:
			default:
			}
			return true
		},
	})
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if ctx.Err() != nil {
			return endTransient, 0, DisconnectConnectionLost
		}
		// The body of a refused upgrade is never read: it is an edge's page or
		// an envelope whose class the close code would have carried.
		if resp != nil && resp.StatusCode == http.StatusForbidden {
			h.log.Warn("the doorbell upgrade was refused by the network edge", slog.Int("status", resp.StatusCode))
			return endTransient, 0, ClassEdgeChallenge
		}
		h.log.Warn("the doorbell could not connect", slog.String("class", ClassUnreachable))
		return endTransient, 0, ClassUnreachable
	}
	conn.SetReadLimit(maxFrameBytes)
	h.setConn(conn)
	// 🔴 Closed on EVERY exit, before the loop dials again (#2094). A socket this
	// companion has given up on but never closed stays registered on its gateway
	// replica until three heartbeats pass — and its next pong puts the entry
	// back. Closing it says so now, on whichever replica holds it.
	defer func() {
		h.setConn(nil)
		_ = conn.Close(websocket.StatusNormalClosure, "reconnecting")
	}()

	connCtx, stop := context.WithCancel(ctx)
	defer stop()
	frames := make(chan []byte)
	readErr := make(chan error, 1)
	go func() {
		for {
			_, raw, err := conn.Read(connCtx)
			if err != nil {
				readErr <- err
				return
			}
			select {
			case frames <- raw:
			case <-connCtx.Done():
				return
			}
		}
	}()

	// Until `hello` names the heartbeat, a connection that says nothing for this
	// long is treated as dead.
	silence := 30 * time.Second
	watchdog := time.NewTimer(silence * 3)
	defer watchdog.Stop()
	var disconnect *protocol.Disconnect
	// A connection that held for a whole heartbeat ends a conflict streak; one
	// replaced sooner continues it (#2077 review).
	var helloAt time.Time
	defer func() {
		if !helloAt.IsZero() && h.api.now().Sub(helloAt) >= silence {
			policy.stable()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return endTransient, 0, DisconnectConnectionLost
		case <-lastPing:
			watchdog.Reset(silence * 3)
			// 🔴 #2105: the daemon opens the doorbell before any plugin says hello,
			// so this connection may have reported the harness `unknown`. Once the
			// facts are known it reconnects ONCE to report them — presence keeps
			// meaning "the companion is connected" and names what answers.
			if minted.Kind == "unknown" && h.harness().Kind != "unknown" {
				h.log.Info("the harness is known now; reconnecting to report it")
				return endHarnessKnown, 0, "harness_known"
			}
		case <-watchdog.C:
			// 🔴 Three missed heartbeats: the gateway's own idle rule, applied
			// from this side. A half-open socket reads nothing forever, and
			// without this the companion would believe it was online.
			h.log.Warn("the doorbell stopped carrying heartbeats")
			return endTransient, 0, DisconnectConnectionLost
		case err := <-readErr:
			return ended(err, disconnect)
		case raw := <-frames:
			watchdog.Reset(silence * 3)
			var envelope protocol.Envelope
			if json.Unmarshal(raw, &envelope) != nil {
				// An unreadable server frame is logged by type only and skipped:
				// the protocol is additive, and closing over it would turn a
				// newer platform into an outage.
				h.log.Warn("skipping an unreadable doorbell frame")
				continue
			}
			switch envelope.Type {
			case protocol.FrameHello:
				var hello protocol.Hello
				if json.Unmarshal(raw, &hello) == nil && hello.HeartbeatSeconds > 0 {
					silence = time.Duration(hello.HeartbeatSeconds) * time.Second
					watchdog.Reset(silence * 3)
				}
				policy.connected()
				helloAt = h.api.now()
				h.setDoorbell(nonEmpty(hello.ConnectionID, "unnamed"))
				h.log.Info("the doorbell is connected", slog.String("connectionId", hello.ConnectionID))
			case protocol.FrameRing:
				var ring protocol.Ring
				if err := json.Unmarshal(raw, &ring); err != nil || ring.RingID == "" || ring.RequestID == "" {
					h.log.Warn("skipping a ring that did not decode")
					continue
				}
				// 🔴 Acked HERE, on the socket the ring came on, before a handler
				// runs (#2077 review): an ack queued behind a slow handler misses the
				// gateway's ack timeout, and the redelivery that follows is the same
				// request under a new ring id.
				h.ackOn(connCtx, conn, ring.RingID)
				if !d.enqueue(ctx, ring) {
					return endTransient, 0, DisconnectConnectionLost
				}
			case protocol.FrameCancel:
				var cancel protocol.Cancel
				if err := json.Unmarshal(raw, &cancel); err != nil || cancel.RequestID == "" {
					h.log.Warn("skipping a cancel that did not decode")
					continue
				}
				if !d.enqueue(ctx, cancel) {
					return endTransient, 0, DisconnectConnectionLost
				}
			case protocol.FrameDisconnect:
				var frame protocol.Disconnect
				if json.Unmarshal(raw, &frame) == nil {
					disconnect = &frame
					// The instruction is the platform's own sentence — platform
					// vocabulary, never tenant content — so it is safe to log.
					h.log.Warn("the platform is disconnecting the doorbell",
						slog.String("reason", frame.Reason), slog.String("instruction", frame.Instruction))
				}
			default:
				// Additive: a frame type this build does not know is ignored.
				h.log.Debug("ignoring an unknown doorbell frame", slog.String("type", envelope.Type))
			}
		}
	}
}

// ended classifies a socket's end: the `disconnect` frame when one came first,
// otherwise the close code.
func ended(err error, disconnect *protocol.Disconnect) (endKind, time.Duration, string) {
	if disconnect != nil {
		return kindOfReason(disconnect.Reason), 0, disconnect.Reason
	}
	var closeErr websocket.CloseError
	if !errors.As(err, &closeErr) {
		return endTransient, 0, DisconnectConnectionLost
	}
	switch int(closeErr.Code) {
	case closeInstanceConflict:
		return endConflict, 0, "instance_conflict"
	case closeKeyRevoked:
		return endRefused, 0, "key_revoked"
	case closeUnauthorized:
		// A connection ticket is single-use and seconds-lived: a refused one is
		// most often simply stale, and the next dial mints another.
		return endTransient, 0, DisconnectUnauthorized
	case closeIdle:
		return endTransient, 0, "idle"
	case closeProtocolError:
		return endTransient, 0, "protocol_error"
	case int(websocket.StatusGoingAway):
		return endShutdown, 0, "server_shutdown"
	case int(websocket.StatusNormalClosure):
		if strings.EqualFold(closeErr.Reason, DisconnectReplaced) {
			// Replaced by a newer socket from THIS instance id — another process
			// sharing this companion's home, or the tail of this one's own
			// reconnect. Either way the policy treats it as a conflict.
			return endConflict, 0, DisconnectReplaced
		}
	}
	return endTransient, 0, DisconnectConnectionLost
}

// disconnectKinds names every disconnect reason that is not transient. Its keys
// are pinned to the manifest's `disconnectReasons` (vocabulary_test.go).
var disconnectKinds = map[string]endKind{
	"instance_conflict": endConflict,
	"key_revoked":       endRefused,
	"server_shutdown":   endShutdown,
}

func kindOfReason(reason string) endKind {
	if kind, ok := disconnectKinds[reason]; ok {
		return kind
	}
	return endTransient
}

// mintEnd classifies a failed connection-ticket mint.
func mintEnd(err error) (endKind, time.Duration, string) {
	var classified *Error
	if !errors.As(err, &classified) {
		return endTransient, 0, DisconnectConnectionLost
	}
	switch classified.Class {
	case "unauthorized", "ticket_revoked":
		return endRefused, 0, classified.Class
	default:
		return endTransient, classified.RetryAfter, classified.Class
	}
}

func toRing(ring protocol.Ring) contracts.Ring {
	return contracts.Ring{
		RingID:         ring.RingID,
		RequestID:      ring.RequestID,
		ConversationID: ring.ConversationID,
		Reason:         ring.Reason,
		Cause: contracts.Cause{
			EntryID: ring.Cause.EntryID, AuthorType: ring.Cause.Author.Type, AuthorID: ring.Cause.Author.ID,
		},
		Deadline:      ring.Deadline,
		SinceCursor:   ring.Since,
		ThroughCursor: ring.Through,
		ResetSession:  ring.ResetSession,
	}
}

// claim is one open doorbell; closing it stops the loop and the socket.
type claim struct {
	once sync.Once
	d    *doorbell
}

func (c *claim) Close() error {
	c.once.Do(func() {
		c.d.cancel()
		<-c.d.done
		c.d.host.release()
	})
	return nil
}
