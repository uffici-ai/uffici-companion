// Package hosttest is the one contract suite every AgentsHost must pass.
//
// 🔴 ONE table, every implementation. The fixture and the in-memory stub run it
// today; wave 1b's real gateway host runs the same table unchanged, and that is
// the point — a suite written against a single implementation quietly becomes a
// second description of that implementation, and then passes for the next one
// while asserting nothing about it.
//
// What belongs here: behaviour the CONTRACT promises, that a caller may rely on
// whoever is behind it. What does not: anything only one backend can do. If an
// assertion cannot be written for both, it is not a contract assertion — it
// belongs in that backend's own tests.
package hosttest

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/uffici-ai/uffici-companion/internal/wire"
	"strings"
	"testing"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
)

// Subject is one implementation under test, plus the two things the suite needs
// that the interface deliberately does not offer: a way to make a ring happen,
// and a way to see a committed offset.
//
// 🔴 Both are outside `AgentsHost` on purpose. The real host delivers because
// the platform rang, and its offsets live on the platform — so a suite that
// demanded them on the interface would force the gateway host to grow methods
// it has no business having.
type Subject struct {
	Host contracts.AgentsHost
	// DeliverRing plays a ring at the registered handlers.
	DeliverRing func(contracts.Ring) error
	// OffsetOf reports the committed offset of the seeded conversation.
	OffsetOf func() (string, error)
	// ConversationID is the seeded conversation.
	ConversationID string
	// Logs is the same host as a LogHost (#2264) — every implementation takes a
	// machine's log lines, so the log cases run for each one.
	Logs contracts.LogHost
	// SeedTool lists one tool in the seeded conversation's catalog (#2349),
	// whose every call answers ToolAnswer.
	SeedTool func(contracts.Tool)
	// ToolAnswer is what the seeded tool's call answers. 🔴 The subject's own
	// configuration, never built here: a call URN is opaque to the companion.
	ToolAnswer contracts.ToolResult
	// SeedMemory prepares the subject so a remember of `text` and then a recall
	// sharing a word with it answer that text (#2385). Nil for a subject whose
	// memory already works; a canned host configures its answers here.
	SeedMemory func(text string)
}

// Factory builds a Subject seeded with the standard transcript below.
type Factory func(t *testing.T) Subject

// SeedEntries is the transcript every implementation is seeded with: a member's
// entry at the offset, the agent's OWN entry, a second agent's entry, and the
// member entry that causes the request.
//
// It is deliberately not the simplest possible transcript. A single member
// entry would pass every implementation of `ReadSince` anyone could write,
// including one that ignores `self` and one that ignores the range.
func SeedEntries() []contracts.Entry {
	return []contracts.Entry{
		{ID: "e1", Cursor: "c0001", Kind: "message", Text: "first",
			Author: contracts.Author{Type: "member", ID: "m1", Label: "Ana"}},
		{ID: "e2", Cursor: "c0002", Kind: "message", Text: "mine",
			Author: contracts.Author{Type: "agent", ID: "a1", Label: "Smith", Handle: "@smith", Self: true}},
		{ID: "e3", Cursor: "c0003", Kind: "message", Text: "from a colleague",
			Author: contracts.Author{Type: "agent", ID: "a2", Label: "Archer", Handle: "@archer"}},
		{ID: "e4", Cursor: "c0004", Kind: "message", Text: "@smith please look",
			Author: contracts.Author{Type: "member", ID: "m1", Label: "Ana"}},
	}
}

// SeedOffset is the committed offset every implementation starts from: after
// the first entry, so a delta has something to span.
const SeedOffset = "c0001"

// SeedThrough is the through cursor of the standard request.
const SeedThrough = "c0004"

// Run executes the contract suite against one implementation.
func Run(t *testing.T, name string, factory Factory) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		t.Run("the delta spans the offset and omits only the agent's own", func(t *testing.T) {
			subject := factory(t)
			read, err := subject.Host.ReadSince(context.Background(), subject.ConversationID, SeedOffset, SeedThrough, 0)
			entries, bounded := read.Entries, read.Bounded
			if err != nil {
				t.Fatalf("ReadSince: %v", err)
			}
			if bounded {
				t.Fatal("reported bounding: the platform bounds, a host serves what it was given")
			}
			if got := ids(entries); got != "e3,e4" {
				// e1 sits AT the offset (exclusive); e2 is the agent's own.
				// 🔴 e3 is a COLLEAGUE's — dropping it would make a multi-agent
				// conversation read as a monologue to whoever answers next.
				t.Fatalf("delta is %q, want \"e3,e4\"", got)
			}
		})

		t.Run("the opener includes the agent's own entries", func(t *testing.T) {
			subject := factory(t)
			entries, err := subject.Host.ReadOpener(context.Background(), subject.ConversationID, SeedThrough, 48000)
			if err != nil {
				t.Fatalf("ReadOpener: %v", err)
			}
			if got := ids(entries); got != "e1,e2,e3,e4" {
				t.Fatalf("opener is %q, want all four: a fresh session must see its own turns", got)
			}
		})

		t.Run("the opener honours the through cursor", func(t *testing.T) {
			subject := factory(t)
			entries, err := subject.Host.ReadOpener(context.Background(), subject.ConversationID, "c0002", 48000)
			if err != nil {
				t.Fatalf("ReadOpener: %v", err)
			}
			if got := ids(entries); got != "e1,e2" {
				t.Fatalf("opener is %q, want \"e1,e2\": the request's range is fixed when it opens", got)
			}
		})

		// 🔴 The assertion worth the whole suite. Advancing on a cancelled or
		// errored request means the next delta starts after entries nobody
		// answered — they are then invisible to every agent, with no error.
		for _, testCase := range []struct {
			outcome contracts.Outcome
			want    string
		}{
			{contracts.OutcomeAnswered, SeedThrough},
			{contracts.OutcomeEmpty, SeedThrough},
			{contracts.OutcomeCancelled, SeedOffset},
			{contracts.OutcomeError, SeedOffset},
		} {
			t.Run("closing as "+string(testCase.outcome)+" leaves the offset at "+testCase.want, func(t *testing.T) {
				subject := factory(t)
				ctx := context.Background()
				claim := openDoorbell(t, subject)
				t.Cleanup(func() { _ = claim.Close() })

				if err := subject.DeliverRing(standardRing(subject.ConversationID)); err != nil {
					t.Fatalf("DeliverRing: %v", err)
				}
				if err := subject.Host.CloseRequest(ctx, "req_1", testCase.outcome, ""); err != nil {
					t.Fatalf("CloseRequest: %v", err)
				}
				got, err := subject.OffsetOf()
				if err != nil {
					t.Fatalf("OffsetOf: %v", err)
				}
				if got != testCase.want {
					t.Fatalf("offset is %q after closing as %s, want %q (decision 40)", got, testCase.outcome, testCase.want)
				}
			})
		}

		t.Run("a doorbell is claimed once, and again after release", func(t *testing.T) {
			subject := factory(t)
			claim := openDoorbell(t, subject)
			if _, err := subject.Host.OpenDoorbell(context.Background(), noopHandlers()); err == nil {
				t.Fatal("a second claim was accepted: the gateway allows one per key")
			}
			if err := claim.Close(); err != nil {
				t.Fatalf("claim.Close: %v", err)
			}
			second, err := subject.Host.OpenDoorbell(context.Background(), noopHandlers())
			if err != nil {
				t.Fatalf("reclaiming after release failed: %v", err)
			}
			_ = second.Close()
		})

		t.Run("a doorbell needs every handler", func(t *testing.T) {
			subject := factory(t)
			if _, err := subject.Host.OpenDoorbell(context.Background(), contracts.Handlers{
				OnRing: func(contracts.Ring) {},
			}); err == nil {
				t.Fatal("opened with nil handlers: deliveries would vanish with nothing said")
			}
		})

		t.Run("a ring is delivered to the registered handler", func(t *testing.T) {
			subject := factory(t)
			delivered := make(chan contracts.Ring, 1)
			claim, err := subject.Host.OpenDoorbell(context.Background(), contracts.Handlers{
				OnRing:       func(ring contracts.Ring) { delivered <- ring },
				OnCancel:     func(string, contracts.CancelReason) {},
				OnDisconnect: func(string) {},
			})
			if err != nil {
				t.Fatalf("OpenDoorbell: %v", err)
			}
			t.Cleanup(func() { _ = claim.Close() })

			want := standardRing(subject.ConversationID)
			if err := subject.DeliverRing(want); err != nil {
				t.Fatalf("DeliverRing: %v", err)
			}
			got := <-delivered
			if got.RequestID != want.RequestID || got.ConversationID != want.ConversationID {
				t.Fatalf("the handler received %+v, want request %q in conversation %q", got, want.RequestID, want.ConversationID)
			}
			if got.Cause.EntryID != want.Cause.EntryID {
				t.Fatalf("the cause was not carried through: got %q, want %q", got.Cause.EntryID, want.Cause.EntryID)
			}
		})

		t.Run("posting and acknowledging need their ids", func(t *testing.T) {
			subject := factory(t)
			ctx := context.Background()
			if _, err := subject.Host.Post(ctx, "", subject.ConversationID, "text", nil); err == nil {
				t.Fatal("a post with no request id was accepted")
			}
			if err := subject.Host.Ack(ctx, ""); err == nil {
				t.Fatal("an ack with no ring id was accepted")
			}
			if err := subject.Host.CloseRequest(ctx, "", contracts.OutcomeAnswered, ""); err == nil {
				t.Fatal("a close with no request id was accepted")
			}
		})

		t.Run("an unknown conversation is an error, not an empty read", func(t *testing.T) {
			subject := factory(t)
			// 🔴 Empty would read as "nothing new", and the agent would answer a
			// conversation it was never shown.
			if _, err := subject.Host.ReadSince(context.Background(), "conv_absent", "", "", 0); err == nil {
				t.Fatal("ReadSince on an unknown conversation returned no error")
			}
			if _, err := subject.Host.ReadOpener(context.Background(), "conv_absent", "", 1); err == nil {
				t.Fatal("ReadOpener on an unknown conversation returned no error")
			}
		})

		// 🔴 #2349: a tool call names a request; one no ring delivered is refused
		// with the contract's own error, so the companion answers `not_permitted`
		// whichever host is behind it — and sends nothing.
		t.Run("a tool call on a request never rung is refused", func(t *testing.T) {
			subject := factory(t)
			_, err := subject.Host.CallTool(context.Background(), "req_never_rung", "list_boards", json.RawMessage(`{}`))
			if !errors.Is(err, contracts.ErrUnknownRequest) {
				t.Fatalf("CallTool on an unrung request = %v, want ErrUnknownRequest", err)
			}
		})

		t.Run("the catalog lists what was seeded, and a rung request's call runs", func(t *testing.T) {
			subject := factory(t)
			if subject.SeedTool == nil {
				t.Fatal("the subject cannot seed a tool — every implementation serves the catalog")
			}
			subject.SeedTool(contracts.Tool{
				Name: "list_boards", Description: "List the boards.",
				Parameters: json.RawMessage(`{"type":"object","properties":{}}`), Permission: "allow",
			})
			ctx := context.Background()
			tools, err := subject.Host.ListTools(ctx, subject.ConversationID)
			if err != nil || len(tools) != 1 || tools[0].Name != "list_boards" || tools[0].Permission != "allow" {
				t.Fatalf("ListTools = %+v, %v", tools, err)
			}
			claim := openDoorbell(t, subject)
			t.Cleanup(func() { _ = claim.Close() })
			// A uuid request id: the protocol's tool-call body requires one.
			ring := standardRing(subject.ConversationID)
			ring.RequestID = "6f1c0f7a-2f0e-4f2e-9b1e-0d6a0b5c7e21"
			if err := subject.DeliverRing(ring); err != nil {
				t.Fatalf("DeliverRing: %v", err)
			}
			result, err := subject.Host.CallTool(ctx, ring.RequestID, "list_boards", json.RawMessage(`{}`))
			if err != nil {
				t.Fatalf("CallTool: %v", err)
			}
			if subject.ToolAnswer.CallURN == "" {
				t.Fatal("the subject configured no call URN — the comparison below would be vacuous")
			}
			if result.CallURN != subject.ToolAnswer.CallURN || string(result.Result) != string(subject.ToolAnswer.Result) {
				t.Fatalf("CallTool = %+v, want the configured %+v: a call's URN and result are relayed as given", result, subject.ToolAnswer)
			}
		})

		// The agent's memory (#2385): refused on a request never rung, like a tool call.
		t.Run("a memory op on a request never rung is refused", func(t *testing.T) {
			subject := factory(t)
			for _, op := range contracts.MemoryOps {
				_, err := subject.Host.Memory(context.Background(), op, "req_never_rung", json.RawMessage(`{}`))
				if !errors.Is(err, contracts.ErrUnknownRequest) {
					t.Fatalf("Memory(%s) on an unrung request = %v, want ErrUnknownRequest", op, err)
				}
			}
		})

		t.Run("a rung request remembers, and a recall relays it in the wire's shape", func(t *testing.T) {
			subject := factory(t)
			const fact = "The Lisbon client is invoiced on the fifth."
			if subject.SeedMemory != nil {
				subject.SeedMemory(fact)
			}
			claim := openDoorbell(t, subject)
			t.Cleanup(func() { _ = claim.Close() })
			ring := standardRing(subject.ConversationID)
			ring.RequestID = "6f1c0f7a-2f0e-4f2e-9b1e-0d6a0b5c7e21"
			if err := subject.DeliverRing(ring); err != nil {
				t.Fatalf("DeliverRing: %v", err)
			}
			ctx := context.Background()
			remembered, err := subject.Host.Memory(ctx, contracts.MemoryRemember, ring.RequestID,
				json.RawMessage(`{"requestId":"`+ring.RequestID+`","text":"`+fact+`"}`))
			if err != nil {
				t.Fatalf("Memory(remember): %v", err)
			}
			if err := wire.ValidateOpPayload(wire.OpMemoryRemember, wire.OpResult, remembered); err != nil {
				t.Fatalf("a remember's answer is not the wire's shape: %v (%s)", err, remembered)
			}
			recalled, err := subject.Host.Memory(ctx, contracts.MemoryRecall, ring.RequestID,
				json.RawMessage(`{"requestId":"`+ring.RequestID+`","query":"When is Lisbon invoiced?"}`))
			if err != nil {
				t.Fatalf("Memory(recall): %v", err)
			}
			if err := wire.ValidateOpPayload(wire.OpMemoryRecall, wire.OpResult, recalled); err != nil {
				t.Fatalf("a recall's answer is not the wire's shape: %v (%s)", err, recalled)
			}
			if !strings.Contains(string(recalled), fact) {
				t.Fatalf("the recall did not relay what was remembered: %s", recalled)
			}
		})

		t.Run("close is idempotent", func(t *testing.T) {
			subject := factory(t)
			if err := subject.Host.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if err := subject.Host.Close(); err != nil {
				t.Fatalf("closing twice: %v — shutdown paths call it from more than one place", err)
			}
		})

		t.Run("a logs ticket answers the policy and a push is taken whole", func(t *testing.T) {
			subject := factory(t)
			if subject.Logs == nil {
				t.Fatal("the subject offers no LogHost — every implementation takes logs")
			}
			ctx := context.Background()
			shipping, err := subject.Logs.MintLogsTicket(ctx)
			if err != nil || !shipping.Enabled || shipping.Ticket == "" || shipping.HarnessLevel == "" {
				t.Fatalf("MintLogsTicket: %+v %v", shipping, err)
			}
			batch := contracts.LogBatch{
				Client: contracts.LogClient{Name: "uffici-companion", Version: "1.2.3"},
				Entries: []contracts.LogEntry{
					{TS: "2026-09-29T12:00:00.000Z", Source: "companion", Level: "info", Message: "one"},
					{TS: "2026-09-29T12:00:01.000Z", Source: "plugin", Level: "warn", Message: "two"},
				},
			}
			result, err := subject.Logs.PushLogs(ctx, shipping.Ticket, batch)
			if err != nil || result.Accepted != 2 {
				t.Fatalf("PushLogs: %+v %v", result, err)
			}
			if _, err := subject.Logs.PushLogs(ctx, "not-the-ticket", batch); err == nil {
				t.Fatal("a push under a ticket nobody issued was taken")
			}
		})
	})
}

func standardRing(conversationID string) contracts.Ring {
	return contracts.Ring{
		RingID:         "ring_1",
		RequestID:      "req_1",
		ConversationID: conversationID,
		Reason:         "mentioned",
		Cause:          contracts.Cause{EntryID: "e4", AuthorID: "m1"},
		SinceCursor:    SeedOffset,
		ThroughCursor:  SeedThrough,
	}
}

func noopHandlers() contracts.Handlers {
	return contracts.Handlers{
		OnRing:       func(contracts.Ring) {},
		OnCancel:     func(string, contracts.CancelReason) {},
		OnDisconnect: func(string) {},
	}
}

func openDoorbell(t *testing.T, subject Subject) contracts.DoorbellClaim {
	t.Helper()
	claim, err := subject.Host.OpenDoorbell(context.Background(), noopHandlers())
	if err != nil {
		t.Fatalf("OpenDoorbell: %v", err)
	}
	return claim
}

func ids(entries []contracts.Entry) string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.ID)
	}
	return strings.Join(out, ",")
}
