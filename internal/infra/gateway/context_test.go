package gateway_test

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/infra/gateway"
	"github.com/uffici-ai/uffici-companion/internal/infra/gateway/gatewaytest"
	"github.com/uffici-ai/uffici-companion/internal/protocol"
)

// The brief's context (#2331): read with the delta, the plugin's budget passed
// through, mapped one to one — and never the reason a read fails.

func contextFixture() *protocol.Context {
	depth := 0
	return &protocol.Context{
		Blocks: []protocol.ContextBlock{
			{Name: "Identity", Scope: "identity", Tokens: 31, Text: "You are an AI agent, not a human."},
			{Name: "Organization", Scope: "organization", Depth: &depth, Tokens: 9, Text: "Never quote prices."},
		},
		Tokens:  40,
		Bounded: true,
		Missing: []protocol.ContextMissing{{Part: "context", Class: "context_trimmed", Instruction: "Some context was left out."}},
	}
}

func seedWithContext(t *testing.T, c *protocol.Context) *rig {
	t.Helper()
	r := newRig(t)
	// After construction: the rig seeds its own conversation last.
	r.double.Seed(conversationID, gatewaytest.Conversation{Offset: "1040", Entries: []protocol.Entry{
		entry("1042", "external", "c-1", false, "@smith hello"),
	}, Context: c})
	return r
}

func TestTheDeltaReadCarriesTheContextAndPassesTheBudget(t *testing.T) {
	r := seedWithContext(t, contextFixture())
	read, err := r.host.ReadSince(context.Background(), conversationID, "1040", "1042", 48000)
	if err != nil {
		t.Fatalf("ReadSince: %v", err)
	}
	depth := 0
	want := &contracts.Context{
		Blocks: []contracts.ContextBlock{
			{Name: "Identity", Scope: "identity", Tokens: 31, Text: "You are an AI agent, not a human."},
			{Name: "Organization", Scope: "organization", Depth: &depth, Tokens: 9, Text: "Never quote prices."},
		},
		Tokens:  40,
		Bounded: true,
		Missing: []contracts.ContextMissing{{Part: "context", Class: "context_trimmed", Instruction: "Some context was left out."}},
	}
	if !reflect.DeepEqual(read.Context, want) {
		t.Fatalf("the context arrived as %+v, want %+v", read.Context, want)
	}
	if _, err := r.host.ReadSince(context.Background(), conversationID, "1040", "1042", 0); err != nil {
		t.Fatalf("ReadSince: %v", err)
	}
	// A declared budget rides along; none is sent when the plugin declared none.
	if got := r.double.EntriesBudgets(); !reflect.DeepEqual(got, []string{"48000", ""}) {
		t.Fatalf("the entries reads carried budgets %q, want [48000 \"\"]", got)
	}
}

func TestAReadWithoutAContextCarriesNil(t *testing.T) {
	r := seedWithContext(t, nil)
	read, err := r.host.ReadSince(context.Background(), conversationID, "1040", "1042", 48000)
	if err != nil {
		t.Fatalf("ReadSince: %v", err)
	}
	if read.Context != nil {
		t.Fatalf("an absent context arrived as %+v — the plugin would lose its static hint", read.Context)
	}
	if len(read.Entries) != 1 {
		t.Fatalf("the delta is %+v", read.Entries)
	}
}

func TestAMalformedContextIsNamedMissingNeverDroppedNorAFailedRead(t *testing.T) {
	identity := `"identity":{"lines":["You are an AI agent."],"disclosure":{"mode":"labelled_ui"}}`
	for name, body := range map[string]string{
		"a block without text": `{"entries":[],"bounded":false,` + identity +
			`,"context":{"blocks":[{"name":"Org","scope":"organization","tokens":1}],"tokens":1,"bounded":false}}`,
		"not an object": `{"entries":[],"bounded":false,` + identity + `,"context":"oops"}`,
		"a block over the wire's bound": `{"entries":[],"bounded":false,` + identity +
			`,"context":{"blocks":[` + strings.TrimSuffix(strings.Repeat(`{"name":"N","scope":"folder","tokens":1,"text":"t"},`, 65), ",") +
			`],"tokens":65,"bounded":false}}`,
	} {
		t.Run(name, func(t *testing.T) {
			r := seedWithContext(t, nil)
			r.double.Inject("entries", gatewaytest.Injected{Status: http.StatusOK, Body: body})
			read, err := r.host.ReadSince(context.Background(), conversationID, "1040", "1042", 100)
			if err != nil {
				t.Fatalf("a malformed context failed the read: %v", err)
			}
			c := read.Context
			if c == nil {
				t.Fatal("a malformed context was dropped silently — the agent would never be told")
			}
			if len(c.Blocks) != 0 || c.Tokens != 0 || len(c.Missing) != 1 {
				t.Fatalf("a malformed context was relayed, or named wrongly: %+v", c)
			}
			if m := c.Missing[0]; m.Part != "context" || m.Class != "knowledge_unavailable" ||
				m.Instruction != gateway.KnowledgeUnavailableInstruction {
				t.Fatalf("the missing part is not the platform's: %+v", m)
			}
		})
	}
}
