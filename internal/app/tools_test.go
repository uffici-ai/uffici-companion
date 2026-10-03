package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ws "github.com/coder/websocket"

	"github.com/uffici-ai/uffici-companion/internal/infra/gateway/gatewaytest"
	wstransport "github.com/uffici-ai/uffici-companion/internal/infra/transport/websocket"
	"github.com/uffici-ai/uffici-companion/internal/protocol"
)

// readFrame reads frames until one of the given type arrives.
func (p *testPlugin) readFrame(ctx context.Context, t *testing.T, want string) map[string]any {
	t.Helper()
	for {
		_, raw, err := p.conn.Read(ctx)
		if err != nil {
			t.Fatalf("reading a %s: %v", want, err)
		}
		var frame map[string]any
		if json.Unmarshal(raw, &frame) == nil && frame["type"] == want {
			return frame
		}
	}
}

// 🔴 #2349, end to end through the daemon: the hello says `tools`, the plugin
// lists the conversation's tools and calls one on the request it was briefed,
// the result comes back with its URN, a call naming a request never rung is
// `not_permitted` with nothing sent, and the answer's post cites the call.
// Neither the arguments nor the result reach the daemon's log.
func TestTheDaemonServesTheAgentsToolsThroughTheGateway(t *testing.T) {
	const (
		conversationID = "a1b2c3d4-e5f6-4789-8abc-def012345678"
		requestID      = "6f1c0f7a-2f0e-4f2e-9b1e-0d6a0b5c7e21"
		otherRequest   = "7f1c0f7a-2f0e-4f2e-9b1e-0d6a0b5c7e22"
		ringID         = "0b9d2f4e-3c1a-4e8b-9f7d-5a6c8e0b1d23"
		argumentMarker = "argument-marker-do-not-log"
	)
	double := gatewaytest.New()
	t.Cleanup(double.Close)
	double.Seed(conversationID, gatewaytest.Conversation{Offset: "1041", Entries: []protocol.Entry{{
		ID: "1042", Cursor: "1042", At: time.Now().UTC(), Kind: "message", Text: "is there a board?",
		Author:      protocol.Author{Type: "user", ID: "u-1", Label: "Mona"},
		Attachments: []protocol.Attachment{},
	}}})
	double.SetTools(conversationID, []protocol.ToolDescriptor{{
		Name: "list_boards", Description: "List the boards.",
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`), Permission: "allow",
	}}, map[string]gatewaytest.ToolAnswer{"list_boards": {
		CallURN: "urn:uffici:tool_call:1", Result: json.RawMessage(`{"boards":[{"id":"b-1","name":"Support"}]}`),
	}})

	config := doctorConfig(t, double)
	logs := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		code, _ := Run(ctx, config, logs)
		done <- code
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the daemon did not stop")
		}
	})
	waitUntil(t, "the doorbell", double.Connected)

	// Before any catalog is read, the doctor says so.
	out := &syncBuffer{}
	if code := Doctor(context.Background(), config, out); code != ExitOK || !strings.Contains(out.String(), "tools        not listed yet") {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}

	plugin := dialPlugin(t, filepath.Join(config.Home, SocketFile), waitForToken(t, config.Home))
	t.Cleanup(func() { _ = plugin.conn.CloseNow() })
	readCtx, stopReading := context.WithTimeout(ctx, 15*time.Second)
	defer stopReading()

	if err := double.Ring(protocol.Ring{
		RingID: ringID, RequestID: requestID, ConversationID: conversationID, Reason: "mention",
		Cause:    protocol.RingCause{EntryID: "1042", Author: protocol.RingAuthor{Type: "user", ID: "u-1"}},
		Deadline: time.Now().Add(5 * time.Minute).UTC(), Since: "1041", Through: "1042",
	}); err != nil {
		t.Fatal(err)
	}
	plugin.readFrame(readCtx, t, "brief")
	plugin.send(t, map[string]any{"v": 1, "type": "ack", "requestId": requestID, "outcome": "accepted", "session": "resumed"})

	call := func(callID, op string, payload map[string]any) map[string]any {
		t.Helper()
		plugin.send(t, map[string]any{"v": 1, "type": "call", "callId": callID, "op": op, "deadlineMs": 10000, "payload": payload})
		result := plugin.readFrame(readCtx, t, "result")
		if result["callId"] != callID {
			t.Fatalf("result for %v, want %s", result["callId"], callID)
		}
		return result
	}

	listed := call("c-list", "tools.list", map[string]any{"conversationId": conversationID})
	tools, _ := listed["payload"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "list_boards" {
		t.Fatalf("tools.list answered %v", listed)
	}

	ran := call("c-call", "tools.call", map[string]any{
		"requestId": requestID, "name": "list_boards", "arguments": map[string]any{"q": argumentMarker},
	})
	payload, _ := ran["payload"].(map[string]any)
	if payload["callUrn"] != "urn:uffici:tool_call:1" || !strings.Contains(mustMarshal(t, payload["result"]), "Support") {
		t.Fatalf("tools.call answered %v", ran)
	}

	before := double.Mints("conversation")
	refused := call("c-other", "tools.call", map[string]any{"requestId": otherRequest, "name": "list_boards", "arguments": map[string]any{}})
	failure, _ := refused["error"].(map[string]any)
	if failure["class"] != "not_permitted" || failure["instruction"] == "" {
		t.Fatalf("a call on a request never rung answered %v", refused)
	}
	if calls := double.ToolCalls(); len(calls) != 1 || double.Mints("conversation") != before {
		t.Fatalf("the refused call reached the platform: %d calls, %d new mints", len(calls), double.Mints("conversation")-before)
	}

	plugin.send(t, map[string]any{"v": 1, "type": "post", "requestId": requestID, "text": "There is one board, Support.",
		"citations": []string{"urn:uffici:tool_call:1"}})
	plugin.send(t, map[string]any{"v": 1, "type": "done", "requestId": requestID, "outcome": "answered"})
	waitUntil(t, "the post", func() bool { return len(double.Posts()) == 1 })
	if posts := double.Posts(); len(posts[0].Citations) != 1 || posts[0].Citations[0] != "urn:uffici:tool_call:1" {
		t.Fatalf("posted %+v", posts)
	}
	if refusals := double.Refusals(); len(refusals) != 0 {
		t.Fatalf("the double refused off-contract bodies: %v", refusals)
	}

	out = &syncBuffer{}
	if code := Doctor(context.Background(), config, out); code != ExitOK ||
		!strings.Contains(out.String(), "tools        1 tool in the most recent conversation's catalog") {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	for _, secret := range []string{argumentMarker, "Support", gatewaytest.Key} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("the daemon's log carries %q", secret)
		}
	}
	if !strings.Contains(logs.String(), `"tool":"list_boards"`) {
		t.Fatalf("the tool call left no line naming the tool:\n%s", logs.String())
	}
	_ = plugin.conn.Close(ws.StatusNormalClosure, "")
}

func TestTheDoctorDescribesTheTools(t *testing.T) {
	if got := describeTools(nil); !strings.Contains(got, "not listed yet") {
		t.Fatal(got)
	}
	if got := describeTools(&wstransport.HealthTools{Listed: 3}); got != "3 tools in the most recent conversation's catalog" {
		t.Fatal(got)
	}
}

func mustMarshal(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
