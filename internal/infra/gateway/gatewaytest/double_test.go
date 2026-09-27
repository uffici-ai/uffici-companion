package gatewaytest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/uffici-ai/uffici-companion/internal/protocol"
)

// The double is only a pin if it REFUSES what the gateway refuses. These drive
// it with raw HTTP, off-contract on purpose, and assert the platform's answers.

func call(t *testing.T, d *Double, method, path, bearer, body string) (int, protocol.ErrorEnvelope) {
	t.Helper()
	req, err := http.NewRequest(method, d.URL()+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := d.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var envelope protocol.ErrorEnvelope
	_ = json.Unmarshal(raw, &envelope)
	return resp.StatusCode, envelope
}

func conversationTicket(t *testing.T, d *Double, conversationID string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, d.URL()+"/v1/tickets/conversation",
		strings.NewReader(`{"instanceId":"d4c3b2a1-f6e5-4d7c-8b9a-0f1e2d3c4b5a","conversationId":"`+conversationID+`"}`))
	req.Header.Set("Authorization", "Bearer "+Key)
	resp, err := d.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var answer protocol.ConversationTicketResponse
	if err := json.NewDecoder(resp.Body).Decode(&answer); err != nil || answer.Ticket == "" {
		t.Fatalf("minting a conversation ticket: %v", err)
	}
	return answer.Ticket
}

const conversation = "a1b2c3d4-e5f6-4789-8abc-def012345678"

func TestTheDoubleRefusesWhatTheGatewayRefuses(t *testing.T) {
	d := New()
	t.Cleanup(d.Close)
	d.Seed(conversation, Conversation{Offset: "1"})
	ticket := conversationTicket(t, d, conversation)
	for name, testCase := range map[string]struct {
		method, path, bearer, body string
		status                     int
		class                      string
	}{
		"a wrong key":             {"POST", "/v1/tickets/connect", "nope", `{}`, 401, "unauthorized"},
		"an extra connect field":  {"POST", "/v1/tickets/connect", Key, `{"instanceId":"d4c3b2a1-f6e5-4d7c-8b9a-0f1e2d3c4b5a","client":{"name":"c","version":"1"},"harness":{"kind":"h","version":"1"},"extra":1}`, 400, "invalid_request"},
		"a bad conversation mint": {"POST", "/v1/tickets/conversation", Key, `{"instanceId":"x"}`, 400, "invalid_request"},
		"no ticket":               {"GET", "/v1/conversations/" + conversation + "/entries", "nope", "", 401, "ticket_invalid"},
		"another conversation":    {"GET", "/v1/conversations/b1c2d3e4-f5a6-4b7c-8d9e-0f1a2b3c4d5e/entries", ticket, "", 400, "conversation_mismatch"},
		"an unknown route":        {"GET", "/v1/conversations/" + conversation + "/nothing", ticket, "", 404, "not_found"},
		"a malformed path":        {"GET", "/v1/conversations/x", ticket, "", 404, "not_found"},
		"a post with moreComing":  {"POST", "/v1/conversations/" + conversation + "/posts", ticket, `{"postId":"5e4d3c2b-1a09-4f8e-9d7c-6b5a4f3e2d1c","requestId":"6f1c0f7a-2f0e-4f2e-9b1e-0d6a0b5c7e21","text":"x","audience":"external","moreComing":true}`, 400, "invalid_request"},
		"a post for no request":   {"POST", "/v1/conversations/" + conversation + "/posts", ticket, `{"postId":"5e4d3c2b-1a09-4f8e-9d7c-6b5a4f3e2d1c","requestId":"6f1c0f7a-2f0e-4f2e-9b1e-0d6a0b5c7e21","text":"x","audience":"external"}`, 404, "not_found"},
		"a close with a bad body": {"POST", "/v1/requests/r/close", ticket, `{"outcome":"maybe"}`, 400, "invalid_request"},
		"an unknown error class":  {"POST", "/v1/requests/r/close", ticket, `{"outcome":"error","errorClass":"model_error"}`, 400, "invalid_error_class"},
		"a close for no request":  {"POST", "/v1/requests/r/close", ticket, `{"outcome":"answered"}`, 404, "not_found"},
		"a close without ticket":  {"POST", "/v1/requests/r/close", "nope", `{"outcome":"answered"}`, 401, "ticket_invalid"},
		"nothing at all":          {"GET", "/elsewhere", "", "", 404, "not_found"},
	} {
		t.Run(name, func(t *testing.T) {
			status, envelope := call(t, d, testCase.method, testCase.path, testCase.bearer, testCase.body)
			if status != testCase.status || envelope.Error.Class != testCase.class {
				t.Fatalf("answered %d %q, want %d %q", status, envelope.Error.Class, testCase.status, testCase.class)
			}
		})
	}
	if len(d.Refusals()) == 0 {
		t.Fatal("no off-contract body was recorded as refused")
	}
}

func TestAnUnknownConversationIsNotFound(t *testing.T) {
	d := New()
	t.Cleanup(d.Close)
	ticket := conversationTicket(t, d, conversation)
	if status, envelope := call(t, d, "GET", "/v1/conversations/"+conversation+"/opener?through=1&budgetTokens=1", ticket, ""); status != 404 || envelope.Error.Class != "not_found" {
		t.Fatalf("answered %d %q", status, envelope.Error.Class)
	}
	if d.Reads("opener") != 1 {
		t.Fatalf("counted %d opener reads", d.Reads("opener"))
	}
	if _, err := d.OffsetOf("absent"); err == nil {
		t.Fatal("an unseeded offset was read")
	}
}

// 🔴 The doorbell takes an ack and NOTHING ELSE: any other client frame is a
// protocol error and a disconnect, which is what makes the host's "ack only"
// discipline testable at all.
func TestTheDoorbellDisconnectsAnythingButAnAck(t *testing.T) {
	d := New()
	t.Cleanup(d.Close)
	req, _ := http.NewRequest(http.MethodPost, d.URL()+"/v1/tickets/connect", bytes.NewBufferString(
		`{"instanceId":"d4c3b2a1-f6e5-4d7c-8b9a-0f1e2d3c4b5a","client":{"name":"c","version":"1"},"harness":{"kind":"h","version":"1"}}`))
	req.Header.Set("Authorization", "Bearer "+Key)
	resp, err := d.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var ticket protocol.TicketResponse
	_ = json.NewDecoder(resp.Body).Decode(&ticket)
	_ = resp.Body.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, d.URL()+"/v1/doorbell", &websocket.DialOptions{
		HTTPClient: d.Client(), HTTPHeader: http.Header{"Authorization": []string{"Bearer " + ticket.Ticket}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.CloseNow() }()
	if _, _, err := conn.Read(ctx); err != nil { // hello
		t.Fatal(err)
	}
	if err := d.Cancel("6f1c0f7a-2f0e-4f2e-9b1e-0d6a0b5c7e21", "stop"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.Read(ctx); err != nil { // the cancel
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"ack","v":1,"ringId":"0b9d2f4e-3c1a-4e8b-9f7d-5a6c8e0b1d23","outcome":"answered"}`)); err != nil {
		t.Fatal(err)
	}
	_, raw, err := conn.Read(ctx)
	if err != nil || !strings.Contains(string(raw), "protocol_error") {
		t.Fatalf("an ack with an outcome answered %q, %v — want a protocol_error disconnect", raw, err)
	}
	_, _, err = conn.Read(ctx)
	if websocket.CloseStatus(err) != 4400 {
		t.Fatalf("closed with %v, want 4400", err)
	}
	// A spent ticket does not open a second socket.
	second, _, err := websocket.Dial(ctx, d.URL()+"/v1/doorbell", &websocket.DialOptions{
		HTTPClient: d.Client(), HTTPHeader: http.Header{"Authorization": []string{"Bearer " + ticket.Ticket}},
	})
	if err == nil {
		_, _, err = second.Read(ctx)
		if websocket.CloseStatus(err) != 4401 {
			t.Fatalf("a spent ticket answered %v, want 4401", err)
		}
	}
	if d.Cancel("x", "stop") == nil || d.CloseSocket(1000, "x") == nil || d.Disconnect("idle", 4408) == nil {
		t.Fatal("a doorbell frame was sent with no socket live")
	}
	if d.Ring(protocol.Ring{}) == nil {
		t.Fatal("a ring was sent with no socket live")
	}
}

func TestTheDoublesPairingRefusesWhatTheGatewayRefuses(t *testing.T) {
	d := New()
	t.Cleanup(d.Close)
	if err := d.ApprovePairing("ZZZZ-ZZZZ", "agent", "acme", "smith", "uffici_live_key_0000"); err == nil {
		t.Fatal("an unknown code was approved")
	}
	body := `{"instanceId":"d4c3b2a1-f6e5-4d7c-8b9a-0f1e2d3c4b5a","client":{"name":"c","version":"1"},` +
		`"harness":{"kind":"h","version":"1"},"publicKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}`
	req, _ := http.NewRequest(http.MethodPost, d.URL()+"/v1/pairings", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer uffici_live_should_not_be_here")
	resp, err := d.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if refusals := d.Refusals(); len(refusals) != 1 || !strings.Contains(refusals[0], "Authorization") {
		t.Fatalf("a credentialed pairing request was not flagged: %v", refusals)
	}
	bad, _ := http.NewRequest(http.MethodPost, d.URL()+"/v1/pairings", strings.NewReader(`{}`))
	resp, err = d.Client().Do(bad)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an invalid pairing body got %d", resp.StatusCode)
	}
	poll, _ := http.NewRequest(http.MethodGet, d.URL()+"/v1/pairings/unknown", nil)
	resp, err = d.Client().Do(poll)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("an unknown pairing's poll got %d", resp.StatusCode)
	}
}
