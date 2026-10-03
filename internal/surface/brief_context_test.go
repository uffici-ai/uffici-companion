package surface

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/requests"
	"github.com/uffici-ai/uffici-companion/internal/wire"
)

// bytesConn keeps every frame's bytes.
type bytesConn struct {
	contracts.Conn
	frames [][]byte
}

func (c *bytesConn) Send(_ context.Context, raw []byte) error {
	c.frames = append(c.frames, raw)
	return nil
}

func briefWith(c *contracts.Context) requests.Brief {
	return requests.Brief{
		RequestID: "req_1", ConversationID: "conv_1", Reason: "mentioned",
		Cause:    contracts.Cause{EntryID: "e1", AuthorID: "m1"},
		Deadline: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), ThroughCursor: "c0001",
		Context: c,
	}
}

// The brief's context (#2331): carried one to one, a VALID frame by the wire's own
// validator, and absent — never empty — when the platform sent none.
func TestABriefCarriesTheContextAsAValidFrame(t *testing.T) {
	depth := 1
	conn := &bytesConn{}
	sink := &frameSink{conn: conn}
	withContext := &contracts.Context{
		Blocks: []contracts.ContextBlock{
			{Name: "Identity", Scope: "identity", Tokens: 3, Text: "You are an AI agent."},
			{Name: "Clients", Scope: "folder", Depth: &depth, Tokens: 2, Text: "Acme is a client."},
		},
		Tokens:  5,
		Missing: []contracts.ContextMissing{{Part: "context", Class: "context_trimmed", Instruction: "Some was left out."}},
	}
	if err := sink.SendBrief(context.Background(), briefWith(withContext)); err != nil {
		t.Fatal(err)
	}
	if err := sink.SendBrief(context.Background(), briefWith(nil)); err != nil {
		t.Fatal(err)
	}
	for i, raw := range conn.frames {
		if err := wire.Validate(raw); err != nil {
			t.Fatalf("brief %d is not a valid frame: %v", i, err)
		}
	}
	var first wire.Brief
	if err := json.Unmarshal(conn.frames[0], &first); err != nil {
		t.Fatal(err)
	}
	if first.Context == nil || len(first.Context.Blocks) != 2 || first.Context.Blocks[1].Depth == nil ||
		*first.Context.Blocks[1].Depth != 1 || first.Context.Tokens != 5 ||
		len(first.Context.Missing) != 1 || first.Context.Missing[0].Instruction != "Some was left out." {
		t.Fatalf("the context reached the wire as %+v", first.Context)
	}
	var second map[string]any
	if err := json.Unmarshal(conn.frames[1], &second); err != nil {
		t.Fatal(err)
	}
	if _, present := second["context"]; present {
		t.Fatal("a brief with no context carried one — the plugin would drop its static hint for nothing")
	}
}

// An unreadable context reaches the plugin as no blocks and the missing part named
// (#2331) — still a VALID frame, `blocks` an empty array, never null.
func TestAContextNamingOnlyWhatIsMissingIsAValidFrame(t *testing.T) {
	conn := &bytesConn{}
	sink := &frameSink{conn: conn}
	missingOnly := &contracts.Context{
		Blocks: []contracts.ContextBlock{},
		Missing: []contracts.ContextMissing{{
			Part: "context", Class: "knowledge_unavailable", Instruction: "Tell the person.",
		}},
	}
	if err := sink.SendBrief(context.Background(), briefWith(missingOnly)); err != nil {
		t.Fatal(err)
	}
	if err := wire.Validate(conn.frames[0]); err != nil {
		t.Fatalf("a context naming only what is missing is not a valid frame: %v", err)
	}
	var brief struct {
		Context map[string]any `json:"context"`
	}
	if err := json.Unmarshal(conn.frames[0], &brief); err != nil {
		t.Fatal(err)
	}
	if blocks, ok := brief.Context["blocks"].([]any); !ok || len(blocks) != 0 {
		t.Fatalf("blocks reached the wire as %#v, want []", brief.Context["blocks"])
	}
}
