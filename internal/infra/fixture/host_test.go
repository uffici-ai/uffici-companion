package fixture

import (
	"context"
	"encoding/json"
	"errors"
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
	read, err := host.ReadSince(context.Background(), "conv_1", "c0001", "c0004", 0)
	entries, bounded := read.Entries, read.Bounded
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
	if _, err := host.ReadSince(ctx, "conv_1", "c0001", "c0004", 0); err != nil {
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
	read, err := host.ReadSince(context.Background(), "conv_1", "c0004", "c0005", 0)
	entries := read.Entries
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

// #2267: the script decides what a logs ticket answers, so the CI run can raise
// the harness to info and assert that a turn's Hermes lines were shipped.
func TestTheScriptSetsTheLogsTicketsHarnessLevel(t *testing.T) {
	plain := newTestHost(t)
	answer, err := plain.MintLogsTicket(context.Background())
	if err != nil || !answer.Enabled || answer.HarnessLevel != "warn" {
		t.Fatalf("an unscripted host answers %+v (%v), want on at warn — the platform's default", answer, err)
	}
	script := testScript()
	script.Logs = &ScriptLogs{HarnessLevel: "info"}
	host, err := New(script)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	answer, err = host.MintLogsTicket(context.Background())
	if err != nil || !answer.Enabled || answer.HarnessLevel != "info" || answer.Ticket == "" {
		t.Fatalf("a scripted host answers %+v (%v), want on at info with a ticket", answer, err)
	}
}

// The scripted context (#2331): served with the delta as the script wrote it,
// one per conversation, and nil where the script gave none — with every budget
// the companion passed through recorded for the run's expectations.
func TestTheDeltaReadServesTheScriptedContext(t *testing.T) {
	script := testScript()
	depth := 0
	script.Conversations["invoices"].Context = &ScriptContext{
		Blocks: []ScriptContextBlock{
			{Name: "Identity", Scope: "identity", Tokens: 3, Text: "You are an AI agent."},
			{Name: "Organization", Scope: "organization", Depth: &depth, Tokens: 2, Text: "[context-canary]"},
		},
		Tokens:  5,
		Missing: []ScriptContextMissing{{Part: "context", Class: "context_trimmed", Instruction: "Some was left out."}},
	}
	script.Conversations["plain"] = &Conversation{ID: "conv_2", Entries: []ScriptEntry{}}
	host, err := New(script)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	read, err := host.ReadSince(context.Background(), "conv_1", "c0001", "c0004", 48000)
	if err != nil {
		t.Fatalf("ReadSince: %v", err)
	}
	c := read.Context
	if c == nil || c.Tokens != 5 || len(c.Blocks) != 2 || c.Blocks[1].Text != "[context-canary]" ||
		c.Blocks[1].Depth == nil || *c.Blocks[1].Depth != 0 || c.Blocks[0].Depth != nil ||
		len(c.Missing) != 1 || c.Missing[0].Class != "context_trimmed" {
		t.Fatalf("the scripted context arrived as %+v", c)
	}
	// A fresh copy per read: editing one cannot reach the script or the next read.
	c.Blocks[1].Text = "edited"
	*c.Blocks[1].Depth = 9
	again, _ := host.ReadSince(context.Background(), "conv_1", "c0001", "c0004", 0)
	if again.Context.Blocks[1].Text != "[context-canary]" || *again.Context.Blocks[1].Depth != 0 {
		t.Fatal("a read's context shares memory with the script")
	}
	plain, err := host.ReadSince(context.Background(), "conv_2", "", "", 48000)
	if err != nil {
		t.Fatalf("ReadSince: %v", err)
	}
	if plain.Context != nil {
		t.Fatalf("a conversation with no scripted context read %+v", plain.Context)
	}
	if got := host.ContextBudgets(); len(got) != 3 || got[0] != 48000 || got[1] != 0 || got[2] != 48000 {
		t.Fatalf("recorded budgets %v, want [48000 0 48000]", got)
	}
}

// 🔴 #2349: the scripted catalog and results, a call recorded with its
// arguments, the script's own URN relayed (none when it scripted none), and a request no ring delivered refused before
// anything is recorded — never forgotten at close, as the gateway host's
// memory is not.
func TestTheHostServesScriptedToolsAndRecordsCalls(t *testing.T) {
	script := testScript()
	script.Tools = []ScriptTool{
		{Name: "list_boards", Description: "List the boards.", Parameters: json.RawMessage(`{"type":"object"}`), Permission: "allow"},
		{Name: "delete_board", Description: "Delete a board.", Permission: "ask"},
	}
	script.Results = map[string]ScriptResult{
		"list_boards": {CallURN: "urn:uffici:tool_call:1", Result: json.RawMessage(`{"boards":[{"id":"b-1","name":"Support"}]}`)},
	}
	host, err := New(script)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	tools, err := host.ListTools(ctx, "conv_1")
	if err != nil || len(tools) != 2 || tools[0].Name != "list_boards" ||
		string(tools[1].Parameters) != `{"type":"object","properties":{}}` {
		t.Fatalf("ListTools = %+v, %v", tools, err)
	}
	if _, err := host.ListTools(ctx, "conv_absent"); err == nil {
		t.Fatal("an unknown conversation listed tools")
	}

	if _, err := host.CallTool(ctx, "req_1", "list_boards", json.RawMessage(`{}`)); !errors.Is(err, contracts.ErrUnknownRequest) {
		t.Fatalf("a call before any ring = %v, want ErrUnknownRequest", err)
	}
	claim, err := host.OpenDoorbell(ctx, contracts.Handlers{
		OnRing: func(contracts.Ring) {}, OnCancel: func(string, contracts.CancelReason) {}, OnDisconnect: func(string) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = claim.Close() }()
	if err := host.DeliverRing(contracts.Ring{RingID: "ring_1", RequestID: "req_1", ConversationID: "conv_1"}); err != nil {
		t.Fatal(err)
	}
	result, err := host.CallTool(ctx, "req_1", "list_boards", json.RawMessage(`{"q":"x"}`))
	if err != nil || result.CallURN != "urn:uffici:tool_call:1" || !strings.Contains(string(result.Result), "Support") {
		t.Fatalf("CallTool = %+v, %v", result, err)
	}
	// Closed requests are still known: the platform answers a late call, not the host.
	if err := host.CloseRequest(ctx, "req_1", contracts.OutcomeAnswered, ""); err != nil {
		t.Fatal(err)
	}
	unscripted, err := host.CallTool(ctx, "req_1", "delete_board", json.RawMessage(`{}`))
	if err != nil || string(unscripted.Result) != `{"ok":true}` || unscripted.CallURN != "" {
		t.Fatalf("an unscripted listed tool answered %+v, %v", unscripted, err)
	}
	_, err = host.CallTool(ctx, "req_1", "rename_board", json.RawMessage(`{}`))
	var classified contracts.ClassifiedError
	if !errors.As(err, &classified) || classified.ErrorClass() != "not_found" || classified.ErrorInstruction() == "" ||
		err.Error() == "" {
		t.Fatalf("an unlisted tool answered %v", err)
	}
	calls := host.ToolCalls()
	if len(calls) != 2 || calls[0].Name != "list_boards" || string(calls[0].Arguments) != `{"q":"x"}` ||
		calls[0].ConversationID != "conv_1" {
		t.Fatalf("ToolCalls = %+v", calls)
	}
}

func TestAPostsCitationsAndActivityAreRecorded(t *testing.T) {
	host := newTestHost(t)
	extras := &contracts.PostExtras{Citations: []string{"urn:uffici:tool_call:1"},
		Activity: []contracts.Activity{{Tool: "web_extract", Summary: "Read it."}}}
	if _, err := host.Post(context.Background(), "req_1", "conv_1", "cited", extras); err != nil {
		t.Fatal(err)
	}
	posts := host.Posts()
	if len(posts) != 1 || len(posts[0].Citations) != 1 || len(posts[0].Activity) != 1 {
		t.Fatalf("Posts = %+v", posts)
	}
}

// 🔴 The catalog is the AGENT's, not a conversation's: a plugin that registers
// one catalog per harness process lists it in whichever conversation it sees
// first, and a fixture that served tools in one conversation only would hand it
// an empty catalog there — the turn that needed the tool then calls nothing.
func TestTheAgentsCatalogIsServedInEveryConversation(t *testing.T) {
	script := testScript()
	script.Conversations["desk"] = &Conversation{ID: "conv_2", Entries: []ScriptEntry{
		{ID: "d1", Cursor: "c0001", Kind: "message", Text: "hi", Author: ScriptAuthor{Type: "member", ID: "m2", Label: "Rui"}},
	}}
	script.Tools = []ScriptTool{{Name: "list_boards", Permission: "allow"}}
	script.Results = map[string]ScriptResult{"list_boards": {CallURN: "urn:uffici:tool_call:5", Result: json.RawMessage(`{"boards":[]}`)}}
	host, err := New(script)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	for _, conversation := range []string{"conv_1", "conv_2"} {
		tools, err := host.ListTools(ctx, conversation)
		if err != nil || len(tools) != 1 || tools[0].Name != "list_boards" {
			t.Fatalf("ListTools(%s) = %+v, %v — the agent's catalog must be the same in every conversation", conversation, tools, err)
		}
	}
	claim, err := host.OpenDoorbell(ctx, contracts.Handlers{
		OnRing: func(contracts.Ring) {}, OnCancel: func(string, contracts.CancelReason) {}, OnDisconnect: func(string) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = claim.Close() }()
	if err := host.DeliverRing(contracts.Ring{RingID: "r2", RequestID: "req_2", ConversationID: "conv_2"}); err != nil {
		t.Fatal(err)
	}
	result, err := host.CallTool(ctx, "req_2", "list_boards", json.RawMessage(`{}`))
	if err != nil || result.CallURN != "urn:uffici:tool_call:5" {
		t.Fatalf("a call in the second conversation = %+v, %v", result, err)
	}
	if calls := host.ToolCalls(); len(calls) != 1 || calls[0].ConversationID != "conv_2" {
		t.Fatalf("ToolCalls = %+v", calls)
	}
}

// The scripted memory (#2385): a working stand-in for knowledge-api — remember,
// recall by a shared word, forget, unfold — and, scripted `unavailable`, every op
// the platform's `platform_unavailable` with the memory family's sentence.
func TestTheHostServesAWorkingMemoryAndCanBeTakenDown(t *testing.T) {
	ring := func(t *testing.T, host *Host) {
		t.Helper()
		claim, err := host.OpenDoorbell(context.Background(), contracts.Handlers{
			OnRing: func(contracts.Ring) {}, OnCancel: func(string, contracts.CancelReason) {}, OnDisconnect: func(string) {},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = claim.Close() })
		if err := host.DeliverRing(contracts.Ring{RingID: "ring_1", RequestID: "req_1", ConversationID: "conv_1"}); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	host, err := New(testScript())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := host.Memory(ctx, contracts.MemoryRecall, "req_1", json.RawMessage(`{}`)); !errors.Is(err, contracts.ErrUnknownRequest) {
		t.Fatalf("memory before any ring = %v", err)
	}
	ring(t, host)
	remembered, err := host.Memory(ctx, contracts.MemoryRemember, "req_1", json.RawMessage(`{"text":"The forklift needs a service."}`))
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	var fact struct {
		URN string `json:"urn"`
	}
	_ = json.Unmarshal(remembered, &fact)
	if _, err := host.Memory(ctx, contracts.MemoryRemember, "req_1", json.RawMessage(`{}`)); err == nil {
		t.Fatal("a remember with no text was taken")
	}
	hit, _ := host.Memory(ctx, contracts.MemoryRecall, "req_1", json.RawMessage(`{"query":"when is the forklift serviced?","limit":3}`))
	miss, _ := host.Memory(ctx, contracts.MemoryRecall, "req_1", json.RawMessage(`{"query":"coffee"}`))
	if !strings.Contains(string(hit), "forklift") || strings.Contains(string(miss), "forklift") {
		t.Fatalf("recall answered %s and %s", hit, miss)
	}
	if _, err := host.Memory(ctx, contracts.MemoryForget, "req_1", json.RawMessage(`{"urn":"`+fact.URN+`"}`)); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if _, err := host.Memory(ctx, contracts.MemoryForget, "req_1", json.RawMessage(`{"urn":"`+fact.URN+`"}`)); err == nil {
		t.Fatal("a fact was forgotten twice")
	}
	if after, _ := host.Memory(ctx, contracts.MemoryRecall, "req_1", json.RawMessage(`{"query":"forklift"}`)); strings.Contains(string(after), "forklift") {
		t.Fatalf("a forgotten fact was recalled: %s", after)
	}
	unfolded, err := host.Memory(ctx, contracts.MemoryUnfold, "req_1", json.RawMessage(`{"urn":"`+fact.URN+`"}`))
	if err != nil || !strings.Contains(string(unfolded), `"rows":[]`) {
		t.Fatalf("unfold = %s, %v", unfolded, err)
	}
	if _, err := host.Memory(ctx, contracts.MemoryOp("dream"), "req_1", nil); err == nil {
		t.Fatal("an unknown op was served")
	}
	if calls := host.MemoryCalls(); len(calls) != 9 || calls[0].Op != contracts.MemoryRemember {
		t.Fatalf("recorded %d memory calls: %+v", len(calls), calls)
	}

	down := testScript()
	down.Memory = &ScriptMemory{Unavailable: true}
	downHost, err := New(down)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ring(t, downHost)
	_, err = downHost.Memory(ctx, contracts.MemoryRecall, "req_1", json.RawMessage(`{"query":"x"}`))
	var classified contracts.ClassifiedError
	if !errors.As(err, &classified) || classified.ErrorClass() != "platform_unavailable" ||
		!strings.Contains(classified.ErrorInstruction(), "could not check what was remembered") {
		t.Fatalf("an unavailable memory answered %v", err)
	}
}

// `unavailableIn` takes memory down in the conversations it names and nowhere else
// (#2386): one run shows a working memory and an outage.
func TestMemoryCanBeTakenDownInOneConversation(t *testing.T) {
	s := testScript()
	s.Conversations["onboarding"] = &Conversation{ID: "conv_2", Offset: "", Entries: s.Conversations["invoices"].Entries}
	s.Memory = &ScriptMemory{UnavailableIn: []string{"onboarding"}}
	host, err := New(s)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	claim, err := host.OpenDoorbell(context.Background(), contracts.Handlers{
		OnRing: func(contracts.Ring) {}, OnCancel: func(string, contracts.CancelReason) {}, OnDisconnect: func(string) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = claim.Close() })
	for _, ring := range []contracts.Ring{
		{RingID: "ring_1", RequestID: "req_up", ConversationID: "conv_1"},
		{RingID: "ring_2", RequestID: "req_down", ConversationID: "conv_2"},
	} {
		if err := host.DeliverRing(ring); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	if _, err := host.Memory(ctx, contracts.MemoryRecall, "req_up", json.RawMessage(`{"query":"x"}`)); err != nil {
		t.Fatalf("memory was down outside the named conversation: %v", err)
	}
	_, err = host.Memory(ctx, contracts.MemoryRecall, "req_down", json.RawMessage(`{"query":"x"}`))
	var classified contracts.ClassifiedError
	if !errors.As(err, &classified) || classified.ErrorClass() != "platform_unavailable" {
		t.Fatalf("memory in the named conversation answered %v", err)
	}
}
