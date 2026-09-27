package surface

import (
	"context"
	"testing"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
)

// ctxConn records, at the moment of each Send, the state of the context it was handed.
type ctxConn struct {
	contracts.Conn
	sent []sentWith
}

type sentWith struct {
	err         error
	deadline    time.Time
	hasDeadline bool
}

func (c *ctxConn) Send(ctx context.Context, _ []byte) error {
	deadline, ok := ctx.Deadline()
	c.sent = append(c.sent, sentWith{err: ctx.Err(), deadline: deadline, hasDeadline: ok})
	return nil
}

// 🔴 #2115: a send carried the SERVING context into the WebSocket library, which
// arms a watcher that closes the connection when that context ends — and clears
// it only after the write returns. A companion stopping while a frame was still
// leaving (or, under CPU pressure, just after its bytes were on the wire) had the
// socket torn down underneath the graceful close: the plugin saw a reset, never
// `shutting_down`. A frame's send must not inherit the caller's cancellation;
// shutdown ends the connection, gracefully, and a bound stops a wedged plugin.
func TestAFrameIsSentUnderItsOwnBoundNotTheCallersCancellation(t *testing.T) {
	conn := &ctxConn{}
	sink := &frameSink{conn: conn}
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sink.send(stopped, map[string]any{"type": "cancel"}); err != nil {
		t.Fatal(err)
	}
	if len(conn.sent) != 1 {
		t.Fatalf("sent %d frames", len(conn.sent))
	}
	got := conn.sent[0]
	if got.err != nil {
		t.Fatalf("the send inherited the caller's cancellation: %v", got.err)
	}
	if !got.hasDeadline || time.Until(got.deadline) > frameSendBound {
		t.Fatalf("the send is not bounded by %v: deadline %v (%v)", frameSendBound, got.deadline, got.hasDeadline)
	}
}
