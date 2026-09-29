package memory

import (
	"context"
	"testing"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
)

// The stub's own refusals. It exists to keep the contract suite honest, so the
// suite must be able to rely on it failing where the fixture fails — an error
// path that differs between the two would let one implementation pass a
// contract assertion the other could not.
func TestTheStubRefusesWhatTheContractRefuses(t *testing.T) {
	host := New()
	ctx := context.Background()

	if err := host.Ack(ctx, ""); err == nil {
		t.Fatal("an ack with no ring id was accepted")
	}
	if _, err := host.Post(ctx, "", "conv_1", "text", nil); err == nil {
		t.Fatal("a post with no request id was accepted")
	}
	if err := host.CloseRequest(ctx, "", contracts.OutcomeAnswered, ""); err == nil {
		t.Fatal("a close with no request id was accepted")
	}
	if _, err := host.OffsetOf("conv_absent"); err == nil {
		t.Fatal("the offset of an unknown conversation was returned")
	}
	if err := host.DeliverRing(contracts.Ring{RequestID: "r"}); err == nil {
		t.Fatal("a ring was delivered with no doorbell open")
	}
	if err := host.DeliverCancel("r", contracts.CancelStop); err == nil {
		t.Fatal("a cancel was delivered with no doorbell open")
	}
	// A close for a request the host never saw is ordinary, not an error: it
	// happens whenever a request closed twice.
	if err := host.CloseRequest(ctx, "unknown", contracts.OutcomeAnswered, ""); err != nil {
		t.Fatalf("closing an unknown request: %v", err)
	}
}

func TestTheStubCountsItsReads(t *testing.T) {
	host := New()
	host.Seed("conv_1", "", []contracts.Entry{{ID: "e1", Cursor: "c0001", Kind: "message"}})
	ctx := context.Background()

	if _, _, err := host.ReadSince(ctx, "conv_1", "", ""); err != nil {
		t.Fatalf("ReadSince: %v", err)
	}
	if _, err := host.ReadOpener(ctx, "conv_1", "", 10); err != nil {
		t.Fatalf("ReadOpener: %v", err)
	}
	delta, opener := host.ReadCounts()
	if delta != 1 || opener != 1 {
		t.Fatalf("counts are (%d, %d), want (1, 1)", delta, opener)
	}
}
