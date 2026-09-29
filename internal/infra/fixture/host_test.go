package fixture

import (
	"context"
	"strings"
	"testing"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
)

// A store with a lagging offset, a member entry, another agent's entry and the
// companion's own — the shape the "mention from a lagging offset" scenario needs.
func testScript() *Script {
	return &Script{
		V: 1,
		Conversations: map[string]*Conversation{
			"invoices": {
				ID:     "conv_1",
				Offset: "c0001",
				Entries: []ScriptEntry{
					{ID: "e1", Cursor: "c0001", At: "2026-09-17T14:01:00Z", Kind: "message", Text: "first",
						Author: ScriptAuthor{Type: "member", ID: "m1", Label: "Ana"}},
					{ID: "e2", Cursor: "c0002", At: "2026-09-17T14:02:00Z", Kind: "message", Text: "mine",
						Author: ScriptAuthor{Type: "agent", ID: "a1", Label: "Smith", Handle: "@smith", Self: true}},
					{ID: "e3", Cursor: "c0003", At: "2026-09-17T14:03:00Z", Kind: "message", Text: "from another agent",
						Author: ScriptAuthor{Type: "agent", ID: "a2", Label: "Archer", Handle: "@archer"}},
					{ID: "e4", Cursor: "c0004", At: "2026-09-17T14:04:00Z", Kind: "message", Text: "@smith please look",
						Author: ScriptAuthor{Type: "member", ID: "m1", Label: "Ana"}},
				},
			},
		},
		Steps: []Step{{Step: "ring", Conversation: "invoices", Reason: "mentioned", CauseEntry: "e4", ThroughCursor: "c0004"}},
	}
}

func newTestHost(t *testing.T) *Host {
	t.Helper()
	host, err := New(testScript())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return host
}

func TestItSatisfiesTheAgentsHostContract(_ *testing.T) {
	var _ contracts.AgentsHost = (*Host)(nil)
}

// 🔴 The delta is what the agent has NOT seen: everything after the offset, with
// its own entries left out. Another agent's entry stays in — an agent must see
// what its colleagues said, or a multi-agent conversation reads as a monologue.
func TestTheDeltaSpansTheOffsetAndOmitsOnlyItsOwn(t *testing.T) {
	host := newTestHost(t)
	entries, bounded, err := host.ReadSince(context.Background(), "conv_1", "c0001", "c0004")
	if err != nil {
		t.Fatalf("ReadSince: %v", err)
	}
	if bounded {
		t.Fatal("the fixture must never report bounding — the platform bounds, the fixture serves")
	}
	var ids []string
	for _, entry := range entries {
		ids = append(ids, entry.ID)
	}
	if got := strings.Join(ids, ","); got != "e3,e4" {
		t.Fatalf("delta is %q, want \"e3,e4\": e1 is at the offset, e2 is the agent's own", got)
	}
}

// The opener rebuilds the conversation as the agent reads it, so its own turns
// must be there — otherwise a fresh session sees a conversation it never spoke in.
func TestTheOpenerIncludesTheAgentsOwnEntries(t *testing.T) {
	host := newTestHost(t)
	entries, err := host.ReadOpener(context.Background(), "conv_1", "c0004", 48000)
	if err != nil {
		t.Fatalf("ReadOpener: %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("opener has %d entries, want all 4 through the cursor", len(entries))
	}
	found := false
	for _, entry := range entries {
		if entry.Author.Self {
			found = true
		}
	}
	if !found {
		t.Fatal("the opener omitted the agent's own entry — a fresh session would not see its own turns")
	}
}

// 🔴 The bug worth the most: an offset advanced on a cancelled request means the
// next brief starts after entries nobody answered, losing them for every agent.
func TestOnlyAnsweredAndEmptyMoveTheOffset(t *testing.T) {
	for _, testCase := range []struct {
		outcome contracts.Outcome
		want    string
	}{
		{contracts.OutcomeAnswered, "c0004"},
		{contracts.OutcomeEmpty, "c0004"},
		{contracts.OutcomeCancelled, "c0001"},
		{contracts.OutcomeError, "c0001"},
	} {
		t.Run(string(testCase.outcome), func(t *testing.T) {
			host := newTestHost(t)
			ctx := context.Background()
			claim, err := host.OpenDoorbell(ctx, contracts.Handlers{
				OnRing: func(contracts.Ring) {}, OnCancel: func(string, contracts.CancelReason) {},
				OnDisconnect: func(string) {},
			})
			if err != nil {
				t.Fatalf("OpenDoorbell: %v", err)
			}
			t.Cleanup(func() { _ = claim.Close() })

			if err := host.DeliverRing(contracts.Ring{
				RingID: "ring_1", RequestID: "req_1", ConversationID: "conv_1", ThroughCursor: "c0004",
			}); err != nil {
				t.Fatalf("DeliverRing: %v", err)
			}
			if err := host.CloseRequest(ctx, "req_1", testCase.outcome, ""); err != nil {
				t.Fatalf("CloseRequest: %v", err)
			}

			got, err := host.OffsetOf("invoices")
			if err != nil {
				t.Fatalf("OffsetOf: %v", err)
			}
			if got != testCase.want {
				t.Fatalf("after closing as %s the offset is %q, want %q — see decision 40", testCase.outcome, got, testCase.want)
			}
		})
	}
}

// The opener read count is the only way to see a resumed session paying for an
// opener it does not need: the turn answers either way.
func TestTheHostCountsItsReads(t *testing.T) {
	host := newTestHost(t)
	ctx := context.Background()
	if _, _, err := host.ReadSince(ctx, "conv_1", "c0001", "c0004"); err != nil {
		t.Fatalf("ReadSince: %v", err)
	}
	since, opener := host.ReadCounts()
	if since != 1 || opener != 0 {
		t.Fatalf("counts are (since=%d, opener=%d), want (1, 0)", since, opener)
	}
	if _, err := host.ReadOpener(ctx, "conv_1", "c0004", 100); err != nil {
		t.Fatalf("ReadOpener: %v", err)
	}
	if _, opener = host.ReadCounts(); opener != 1 {
		t.Fatalf("opener count is %d, want 1", opener)
	}
}

func TestADoorbellIsClaimedOnce(t *testing.T) {
	host := newTestHost(t)
	handlers := contracts.Handlers{
		OnRing: func(contracts.Ring) {}, OnCancel: func(string, contracts.CancelReason) {},
		OnDisconnect: func(string) {},
	}
	claim, err := host.OpenDoorbell(context.Background(), handlers)
	if err != nil {
		t.Fatalf("OpenDoorbell: %v", err)
	}
	if _, err := host.OpenDoorbell(context.Background(), handlers); err == nil {
		t.Fatal("a second doorbell claim was accepted — the gateway allows one per key")
	}
	if err := claim.Close(); err != nil {
		t.Fatalf("claim.Close: %v", err)
	}
	if _, err := host.OpenDoorbell(context.Background(), handlers); err != nil {
		t.Fatalf("reclaiming after close failed: %v", err)
	}
}

// A nil handler would drop deliveries with no error anywhere.
func TestOpenDoorbellRefusesAMissingHandler(t *testing.T) {
	host := newTestHost(t)
	if _, err := host.OpenDoorbell(context.Background(), contracts.Handlers{OnRing: func(contracts.Ring) {}}); err == nil {
		t.Fatal("a doorbell opened with two nil handlers — deliveries would vanish silently")
	}
}

func TestRingsAreRefusedWithNoOpenDoorbell(t *testing.T) {
	host := newTestHost(t)
	if err := host.DeliverRing(contracts.Ring{RingID: "r", RequestID: "q", ConversationID: "conv_1"}); err == nil {
		t.Fatal("a ring was delivered with no doorbell open, so it reached nobody and said so to nobody")
	}
}

// A script appends entries mid-run to drive "a message arrived while the agent
// was answering". The store must keep cursors strictly increasing through it,
// or the next delta's range stops meaning anything.
func TestAppendedEntriesJoinTheDeltaAndKeepCursorsOrdered(t *testing.T) {
	host := newTestHost(t)
	if err := host.store.appendEntries("invoices", []ScriptEntry{
		{ID: "e5", Cursor: "c0005", At: "2026-09-17T14:05:00Z", Kind: "message", Text: "and one more",
			Author: ScriptAuthor{Type: "member", ID: "m1", Label: "Ana"}},
	}); err != nil {
		t.Fatalf("appendEntries: %v", err)
	}
	entries, _, err := host.ReadSince(context.Background(), "conv_1", "c0004", "c0005")
	if err != nil {
		t.Fatalf("ReadSince: %v", err)
	}
	if len(entries) != 1 || entries[0].ID != "e5" {
		t.Fatalf("the appended entry did not reach the delta: %+v", entries)
	}

	if err := host.store.appendEntries("invoices", []ScriptEntry{
		{ID: "e6", Cursor: "c0003", At: "2026-09-17T14:06:00Z", Kind: "message", Text: "out of order",
			Author: ScriptAuthor{Type: "member", ID: "m1", Label: "Ana"}},
	}); err == nil {
		t.Fatal("an out-of-order cursor was appended — every later delta range would be ambiguous")
	}
	if err := host.store.appendEntries("nowhere", nil); err == nil {
		t.Fatal("appending to an undeclared conversation was accepted")
	}
}

// The runner reads these to build its verdict, so an empty or stale answer
// would make a passing script meaningless.
func TestTheHostReportsWhatItRecorded(t *testing.T) {
	host := newTestHost(t)
	ctx := context.Background()
	claim, err := host.OpenDoorbell(ctx, contracts.Handlers{
		OnRing: func(contracts.Ring) {}, OnCancel: func(string, contracts.CancelReason) {},
		OnDisconnect: func(string) {},
	})
	if err != nil {
		t.Fatalf("OpenDoorbell: %v", err)
	}
	t.Cleanup(func() { _ = claim.Close() })

	if err := host.DeliverRing(contracts.Ring{
		RingID: "ring_1", RequestID: "req_1", ConversationID: "conv_1", ThroughCursor: "c0004",
	}); err != nil {
		t.Fatalf("DeliverRing: %v", err)
	}
	if err := host.Ack(ctx, "ring_1"); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if _, err := host.Post(ctx, "req_1", "conv_1", "answered", nil); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if err := host.CloseRequest(ctx, "req_1", contracts.OutcomeAnswered, ""); err != nil {
		t.Fatalf("CloseRequest: %v", err)
	}

	if got := host.Acks(); len(got) != 1 || got[0] != "ring_1" {
		t.Fatalf("Acks() is %v, want [ring_1]", got)
	}
	if got := host.Posts(); len(got) != 1 || got[0].RequestID != "req_1" {
		t.Fatalf("Posts() is %+v", got)
	}
	closes := host.Closes()
	if len(closes) != 1 || !closes[0].OffsetCommitted {
		t.Fatalf("Closes() is %+v; an answered close must record that it committed", closes)
	}
	if host.RingsDelivered() != 1 {
		t.Fatalf("RingsDelivered() is %d, want 1", host.RingsDelivered())
	}
}

func TestDeliveringWithNoDoorbellIsRefused(t *testing.T) {
	host := newTestHost(t)
	if err := host.DeliverCancel("req_1", contracts.CancelStop); err == nil {
		t.Fatal("a cancel was delivered with no doorbell open, reaching nobody silently")
	}
}
