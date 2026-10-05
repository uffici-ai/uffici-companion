// Package hostsuite runs the AgentsHost contract suite against every
// implementation, from one table.
//
// 🔴 It lives in its own package so neither backend can reach inside the other,
// and so adding an implementation is one row here rather than a second copy of
// the suite. Wave 1b's gateway host adds its row and changes nothing else — if
// it cannot, the suite has drifted into describing the fixture.
package hostsuite_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/contracts/hosttest"
	"github.com/uffici-ai/uffici-companion/internal/infra/fixture"
	"github.com/uffici-ai/uffici-companion/internal/infra/gateway"
	"github.com/uffici-ai/uffici-companion/internal/infra/gateway/gatewaytest"
	"github.com/uffici-ai/uffici-companion/internal/infra/memory"
	"github.com/uffici-ai/uffici-companion/internal/protocol"
)

// suiteToolAnswer is what every subject's seeded tool answers. The URN is
// configured here, in a test, because the companion never builds one.
var suiteToolAnswer = contracts.ToolResult{
	CallURN: "urn:uffici:tool_call:77", Result: json.RawMessage(`{"boards":[]}`),
}

func TestEveryAgentsHostSatisfiesTheContract(t *testing.T) {
	implementations := map[string]hosttest.Factory{
		"fixture": fixtureSubject,
		"memory":  memorySubject,
		"gateway": gatewaySubject,
	}

	// 🔴 Non-vacuity, and the reason this file exists: one implementation is a
	// description, two are a contract. A table that shrank to one would keep
	// passing while proving nothing about the next backend.
	if len(implementations) < 2 {
		t.Fatalf("the suite runs against %d implementation(s) — it needs at least two to be a contract suite", len(implementations))
	}

	for name, factory := range implementations {
		hosttest.Run(t, name, factory)
	}
}

// The fixture, seeded through a script carrying the standard transcript.
func fixtureSubject(t *testing.T) hosttest.Subject {
	t.Helper()
	entries := make([]fixture.ScriptEntry, 0, len(hosttest.SeedEntries()))
	for _, entry := range hosttest.SeedEntries() {
		entries = append(entries, fixture.ScriptEntry{
			ID: entry.ID, Cursor: entry.Cursor, Kind: entry.Kind, Text: entry.Text,
			Author: fixture.ScriptAuthor{
				Type: entry.Author.Type, ID: entry.Author.ID, Label: entry.Author.Label,
				Handle: entry.Author.Handle, Self: entry.Author.Self,
			},
		})
	}
	script := &fixture.Script{
		V: 1,
		Conversations: map[string]*fixture.Conversation{
			"seeded": {ID: "conv_1", Offset: hosttest.SeedOffset, Entries: entries},
		},
		// The suite's one tool, scripted up front: a script's catalog is fixed
		// when the host is built, and it is the agent's, not a conversation's.
		Tools: []fixture.ScriptTool{{
			Name: "list_boards", Description: "List the boards.",
			Parameters: json.RawMessage(`{"type":"object","properties":{}}`), Permission: "allow",
		}},
		Results: map[string]fixture.ScriptResult{
			"list_boards": {CallURN: suiteToolAnswer.CallURN, Result: suiteToolAnswer.Result},
		},
		Steps: []fixture.Step{{
			Step: "ring", Conversation: "seeded", Reason: "mentioned",
			CauseEntry: "e4", ThroughCursor: hosttest.SeedThrough,
		}},
	}
	host, err := fixture.New(script)
	if err != nil {
		t.Fatalf("building the fixture host: %v", err)
	}
	return hosttest.Subject{
		Host:           host,
		DeliverRing:    host.DeliverRing,
		OffsetOf:       func() (string, error) { return host.OffsetOf("seeded") },
		ConversationID: "conv_1",
		Logs:           host,
		// Already in the script above; the suite seeds the same tool.
		SeedTool:   func(contracts.Tool) {},
		ToolAnswer: suiteToolAnswer,
	}
}

// The stub, seeded directly — no script, no validation, no files.
func memorySubject(t *testing.T) hosttest.Subject {
	t.Helper()
	host := memory.New()
	host.Seed("conv_1", hosttest.SeedOffset, hosttest.SeedEntries())
	return hosttest.Subject{
		Host:           host,
		DeliverRing:    host.DeliverRing,
		OffsetOf:       func() (string, error) { return host.OffsetOf("conv_1") },
		ConversationID: "conv_1",
		Logs:           host,
		SeedTool: func(tool contracts.Tool) {
			host.SetTools("conv_1", []contracts.Tool{tool})
			host.SetToolResult(tool.Name, suiteToolAnswer)
		},
		ToolAnswer: suiteToolAnswer,
		// The stub answers what it is told: a URN for the remember, the fact for a recall.
		SeedMemory: func(text string) {
			urn := "urn:uffici:fact:9c8b7a6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d"
			host.SetMemoryAnswer(contracts.MemoryRemember, json.RawMessage(`{"urn":"`+urn+`"}`))
			host.SetMemoryAnswer(contracts.MemoryRecall, json.RawMessage(`{"hits":[{"urn":"`+urn+
				`","level":"conversation","distance":null,"text":"`+text+
				`","at":"2026-10-04T10:00:00.000Z","source":{"kind":"remembered"}}],"degraded":false}`))
		},
	}
}

// Both implementations must satisfy the interface at compile time, not only at
// run time: a missing method is a build error rather than a skipped subtest.
var (
	_ contracts.AgentsHost = (*fixture.Host)(nil)
	_ contracts.AgentsHost = (*memory.Host)(nil)
	_ contracts.AgentsHost = (*gateway.Host)(nil)

	_ contracts.LogHost              = (*fixture.Host)(nil)
	_ contracts.LogHost              = (*memory.Host)(nil)
	_ contracts.LogHost              = (*gateway.Host)(nil)
	_ contracts.ControlPlaneActivity = (*fixture.Host)(nil)
	_ contracts.ControlPlaneActivity = (*memory.Host)(nil)
	_ contracts.ControlPlaneActivity = (*gateway.Host)(nil)
)

// The real host, against the companion's gateway double (#2077): the one row
// wave 1b was promised it could add without changing the suite.
func gatewaySubject(t *testing.T) hosttest.Subject {
	t.Helper()
	const conversationID = "a1b2c3d4-e5f6-4789-8abc-def012345678"
	double := gatewaytest.New()
	t.Cleanup(double.Close)
	entries := make([]protocol.Entry, 0, len(hosttest.SeedEntries()))
	for _, entry := range hosttest.SeedEntries() {
		authorType := entry.Author.Type
		if authorType == "member" {
			authorType = "user"
		}
		entries = append(entries, protocol.Entry{
			ID: entry.ID, Cursor: entry.Cursor, At: time.Now().UTC(), Kind: entry.Kind, Text: entry.Text,
			Author: protocol.Author{
				Type: authorType, ID: entry.Author.ID, Label: entry.Author.Label,
				Handle: entry.Author.Handle, Self: entry.Author.Self,
			},
			Attachments: []protocol.Attachment{},
		})
	}
	double.Seed(conversationID, gatewaytest.Conversation{Offset: hosttest.SeedOffset, Entries: entries})
	host, err := gateway.New(gateway.Options{
		BaseURL: double.URL(), Key: gatewaytest.Key, HTTPClient: double.Client(), LogsHTTPClient: double.Client(),
		InstanceID: "d4c3b2a1-f6e5-4d7c-8b9a-0f1e2d3c4b5a",
		Sleep:      func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatalf("building the gateway host: %v", err)
	}
	t.Cleanup(func() { _ = host.Close() })
	return hosttest.Subject{
		Host: host,
		DeliverRing: func(ring contracts.Ring) error {
			if !eventually(double.Connected) {
				return fmt.Errorf("the doorbell never connected to the double")
			}
			if err := double.Ring(protocol.Ring{
				RingID: ring.RingID, RequestID: ring.RequestID, ConversationID: ring.ConversationID,
				Reason: ring.Reason, Deadline: time.Now().Add(time.Minute).UTC(),
				Cause: protocol.RingCause{EntryID: ring.Cause.EntryID, Author: protocol.RingAuthor{Type: "user", ID: ring.Cause.AuthorID}},
				Since: ring.SinceCursor, Through: ring.ThroughCursor,
			}); err != nil {
				return err
			}
			if !eventually(func() bool { return host.KnowsRequest(ring.RequestID) }) {
				return fmt.Errorf("the host never read the ring")
			}
			return nil
		},
		OffsetOf:       func() (string, error) { return double.OffsetOf(conversationID) },
		ConversationID: conversationID,
		Logs:           host,
		SeedTool: func(tool contracts.Tool) {
			double.SetTools(conversationID, []protocol.ToolDescriptor{{
				Name: tool.Name, Description: tool.Description, Parameters: tool.Parameters, Permission: tool.Permission,
			}}, map[string]gatewaytest.ToolAnswer{
				tool.Name: {CallURN: suiteToolAnswer.CallURN, Result: suiteToolAnswer.Result},
			})
		},
		ToolAnswer: suiteToolAnswer,
	}
}

func eventually(condition func() bool) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}
