package surface

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/uffici-ai/uffici-companion/internal/wire"
)

// capturingConn records what the sink sends.
type capturingConn struct{ sent [][]byte }

func (c *capturingConn) Receive(context.Context) ([]byte, error) { return nil, context.Canceled }
func (c *capturingConn) Send(_ context.Context, frame []byte) error {
	c.sent = append(c.sent, frame)
	return nil
}
func (c *capturingConn) Close(string) error { return nil }
func (c *capturingConn) RemoteHint() string { return "test" }

// 🔴 The frame decision 32 rides on the local wire (#2077): a `failed` cancel
// the wire's own schema accepts, carrying the class and the instruction.
func TestAFailureIsAFailedCancelTheWireAccepts(t *testing.T) {
	conn := &capturingConn{}
	sink := &frameSink{conn: conn}
	if err := sink.SendFailure(context.Background(), "req_1", "platform_unavailable", "tell the person"); err != nil {
		t.Fatal(err)
	}
	if len(conn.sent) != 1 {
		t.Fatalf("sent %d frames", len(conn.sent))
	}
	if err := wire.Validate(conn.sent[0]); err != nil {
		t.Fatalf("the wire refuses the companion's own failure frame: %v", err)
	}
	var frame wire.Cancel
	if err := json.Unmarshal(conn.sent[0], &frame); err != nil {
		t.Fatal(err)
	}
	if frame.Reason != wire.CancelFailed || frame.Error == nil ||
		frame.Error.Class != "platform_unavailable" || frame.Error.Instruction != "tell the person" {
		t.Fatalf("sent %+v", frame)
	}
}
