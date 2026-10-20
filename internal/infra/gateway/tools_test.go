package gateway_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/infra/gateway"
	"github.com/uffici-ai/uffici-companion/internal/infra/gateway/gatewaytest"
	"github.com/uffici-ai/uffici-companion/internal/protocol"
)

// argumentMarker and resultMarker stand for tenant content: neither may reach a log.
const (
	argumentMarker = "argument-marker-do-not-log"
	resultMarker   = "result-marker-do-not-log"
)

func boardsCatalog(r *rig) {
	r.double.SetTools(conversationID, []protocol.ToolDescriptor{{
		Name: "list_boards", Description: "List the boards.",
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`), Permission: "allow",
	}}, map[string]gatewaytest.ToolAnswer{
		"list_boards": {
			CallURN: "urn:uffici:tool_call:1",
			Result:  json.RawMessage(`{"boards":[{"id":"b-1","name":"Support"}],"note":"` + resultMarker + `"}`),
		},
	})
}

func assertNoToolContentLogged(t *testing.T, r *rig) {
	t.Helper()
	for _, marker := range []string{argumentMarker, resultMarker} {
		if strings.Contains(r.logs.String(), marker) {
			t.Fatalf("a log line carries tool content %q:\n%s", marker, r.logs.String())
		}
	}
}

func TestListToolsReadsTheConversationsCatalog(t *testing.T) {
	r := newRig(t)
	boardsCatalog(r)
	tools, err := r.host.ListTools(context.Background(), conversationID)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "list_boards" || tools[0].Permission != "allow" ||
		string(tools[0].Parameters) != `{"type":"object","properties":{}}` {
		t.Fatalf("ListTools = %+v", tools)
	}
	if r.double.Reads("tools") != 1 {
		t.Fatalf("%d catalog reads, want 1", r.double.Reads("tools"))
	}
	if _, err := r.host.ListTools(context.Background(), ""); err == nil {
		t.Fatal("ListTools with no conversation succeeded")
	}
}

func TestACatalogWithoutItsListIsNotTheProtocol(t *testing.T) {
	r := newRig(t)
	r.double.Inject("tools", gatewaytest.Injected{Status: http.StatusOK, Body: `{}`})
	_, err := r.host.ListTools(context.Background(), conversationID)
	var classified contracts.ClassifiedError
	if !errors.As(err, &classified) || classified.ErrorClass() != gateway.ClassPlatformError {
		t.Fatalf("an answer with no tools read as %v", err)
	}
}

func TestAToolCallRidesTheRequestsConversationTicket(t *testing.T) {
	r := newRig(t)
	r.open(t)
	r.ring(t)
	boardsCatalog(r)
	result, err := r.host.CallTool(context.Background(), requestID, "list_boards",
		json.RawMessage(`{"query":"`+argumentMarker+`"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if refusals := r.double.Refusals(); len(refusals) != 0 {
		t.Fatalf("the double refused the tool call: %v", refusals)
	}
	calls := r.double.ToolCalls()
	if len(calls) != 1 || calls[0].RequestID != requestID || calls[0].ConversationID != conversationID ||
		!strings.Contains(string(calls[0].Arguments), argumentMarker) {
		t.Fatalf("the platform recorded %+v", calls)
	}
	if result.CallURN != "urn:uffici:tool_call:1" || !strings.Contains(string(result.Result), "Support") {
		t.Fatalf("CallTool = %+v", result)
	}
	assertNoToolContentLogged(t, r)
}

func TestAToolCallWithNoArgumentsSendsAnEmptyObject(t *testing.T) {
	r := newRig(t)
	r.open(t)
	r.ring(t)
	boardsCatalog(r)
	if _, err := r.host.CallTool(context.Background(), requestID, "list_boards", nil); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if calls := r.double.ToolCalls(); len(calls) != 1 || string(calls[0].Arguments) != "{}" {
		t.Fatalf("the platform recorded %+v", calls)
	}
}

// 🔴 The refusal this op's safety rests on: a request this companion was never
// rung for is refused BEFORE anything leaves — no ticket mint, no call.
func TestAToolCallOnARequestNeverRungSendsNothing(t *testing.T) {
	r := newRig(t)
	boardsCatalog(r)
	_, err := r.host.CallTool(context.Background(), requestID, "list_boards", json.RawMessage(`{}`))
	if !errors.Is(err, contracts.ErrUnknownRequest) {
		t.Fatalf("CallTool on an unrung request = %v, want ErrUnknownRequest", err)
	}
	if mints := r.double.Mints("conversation"); mints != 0 {
		t.Fatalf("%d conversation tickets minted for a request never rung", mints)
	}
	if calls := r.double.ToolCalls(); len(calls) != 0 {
		t.Fatalf("a tool call reached the platform: %+v", calls)
	}
}

// 🔴 A tool is not idempotent: a failure after the body left is answered as it
// came, never sent again — whether the platform ran the tool or not.
func TestAToolCallIsNeverRetriedOnceSent(t *testing.T) {
	for name, afterEffect := range map[string]bool{"the tool ran": true, "the tool did not run": false} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.open(t)
			r.ring(t)
			boardsCatalog(r)
			r.double.Inject("tool-calls", gatewaytest.Injected{
				Status: http.StatusServiceUnavailable, Body: gatewaytest.Envelope("platform_unavailable", true, nil),
				AfterEffect: afterEffect,
			})
			_, err := r.host.CallTool(context.Background(), requestID, "list_boards", json.RawMessage(`{}`))
			var classified contracts.ClassifiedError
			if !errors.As(err, &classified) || classified.ErrorClass() != "platform_unavailable" ||
				classified.ErrorInstruction() == "" {
				t.Fatalf("CallTool = %v, want the platform's classified failure", err)
			}
			want := 0
			if afterEffect {
				want = 1
			}
			if calls := r.double.ToolCalls(); len(calls) != want {
				t.Fatalf("%d tool calls recorded, want %d — a retryable failure was sent again", len(calls), want)
			}
			if delays := r.backoffDelays(); len(delays) != 0 {
				t.Fatalf("the call waited to retry: %v", delays)
			}
		})
	}
}

// A refused TICKET is refused before the gateway runs anything: re-minted once
// and sent again, exactly as every other call does.
func TestARefusedTicketIsReMintedAndTheCallSentOnce(t *testing.T) {
	r := newRig(t)
	r.open(t)
	r.ring(t)
	boardsCatalog(r)
	r.double.Inject("tool-calls", gatewaytest.Injected{
		Status: http.StatusUnauthorized, Body: gatewaytest.Envelope("ticket_expired", false, nil),
	})
	before := r.double.Mints("conversation")
	if _, err := r.host.CallTool(context.Background(), requestID, "list_boards", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if calls := r.double.ToolCalls(); len(calls) != 1 {
		t.Fatalf("%d tool calls recorded, want 1", len(calls))
	}
	if mints := r.double.Mints("conversation") - before; mints != 2 {
		t.Fatalf("%d tickets minted for the call, want 2 — the refused one and one fresh one", mints)
	}
}

// Only once: a second refusal of the fresh ticket is the answer.
func TestATicketRefusedTwiceIsTheAnswer(t *testing.T) {
	r := newRig(t)
	r.open(t)
	r.ring(t)
	boardsCatalog(r)
	refusal := gatewaytest.Injected{Status: http.StatusUnauthorized, Body: gatewaytest.Envelope("ticket_expired", false, nil)}
	r.double.Inject("tool-calls", refusal, refusal)
	_, err := r.host.CallTool(context.Background(), requestID, "list_boards", json.RawMessage(`{}`))
	var classified contracts.ClassifiedError
	if !errors.As(err, &classified) || classified.ErrorClass() != "ticket_expired" {
		t.Fatalf("CallTool = %v", err)
	}
}

// The mint carries no tool, so a retryable mint failure IS retried.
func TestAFailedTicketMintIsRetriedBeforeTheCallLeaves(t *testing.T) {
	r := newRig(t)
	r.open(t)
	r.ring(t)
	boardsCatalog(r)
	r.double.Inject("conversation", gatewaytest.Injected{
		Status: http.StatusServiceUnavailable, Body: gatewaytest.Envelope("platform_unavailable", true, nil),
	})
	if _, err := r.host.CallTool(context.Background(), requestID, "list_boards", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if calls := r.double.ToolCalls(); len(calls) != 1 {
		t.Fatalf("%d tool calls recorded, want 1", len(calls))
	}
}

func TestAMintThatNeverSucceedsIsTheAnswer(t *testing.T) {
	r := newRig(t)
	r.open(t)
	r.ring(t)
	boardsCatalog(r)
	unavailable := gatewaytest.Injected{
		Status: http.StatusServiceUnavailable, Body: gatewaytest.Envelope("platform_unavailable", true, nil),
	}
	r.double.Inject("conversation", unavailable, unavailable, unavailable, unavailable, unavailable)
	_, err := r.host.CallTool(context.Background(), requestID, "list_boards", json.RawMessage(`{}`))
	var classified contracts.ClassifiedError
	if !errors.As(err, &classified) || classified.ErrorClass() != "platform_unavailable" {
		t.Fatalf("CallTool = %v", err)
	}
	if calls := r.double.ToolCalls(); len(calls) != 0 {
		t.Fatalf("a call left without a ticket: %+v", calls)
	}
}

func TestAMintInterruptedByItsContextStops(t *testing.T) {
	r := newRig(t)
	r.open(t)
	r.ring(t)
	r.double.Inject("conversation", gatewaytest.Injected{
		Status: http.StatusServiceUnavailable, Body: gatewaytest.Envelope("platform_unavailable", true, nil),
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.host.CallTool(ctx, requestID, "list_boards", json.RawMessage(`{}`)); err == nil {
		t.Fatal("a cancelled call succeeded")
	}
}

func TestAToolCallAnswerWithoutAResultIsNotTheProtocol(t *testing.T) {
	r := newRig(t)
	r.open(t)
	r.ring(t)
	boardsCatalog(r)
	r.double.Inject("tool-calls", gatewaytest.Injected{Status: http.StatusOK, Body: `{"callUrn":null}`})
	_, err := r.host.CallTool(context.Background(), requestID, "list_boards", json.RawMessage(`{}`))
	var classified contracts.ClassifiedError
	if !errors.As(err, &classified) || classified.ErrorClass() != gateway.ClassPlatformError {
		t.Fatalf("CallTool = %v", err)
	}
}

func TestAToolCallThatDidNotRunHasNoURN(t *testing.T) {
	r := newRig(t)
	r.open(t)
	r.ring(t)
	boardsCatalog(r)
	r.double.Inject("tool-calls", gatewaytest.Injected{Status: http.StatusOK,
		Body: `{"callUrn":null,"result":{"ok":false,"error":"needs_approval","message":"A person must approve this tool."}}`})
	result, err := r.host.CallTool(context.Background(), requestID, "list_boards", json.RawMessage(`{}`))
	if err != nil || result.CallURN != "" || !strings.Contains(string(result.Result), "needs_approval") {
		t.Fatalf("CallTool = %+v, %v", result, err)
	}
}

// 🔴 #2349: citations and activity ride the post in the protocol's shape.
func TestCitationsAndActivityRideThePost(t *testing.T) {
	r := newRig(t)
	r.open(t)
	r.ring(t)
	extras := &contracts.PostExtras{
		Citations: []string{"urn:uffici:tool_call:42"},
		Activity:  []contracts.Activity{{Tool: "web_extract", Summary: "Read the vendor's status page."}},
	}
	if _, err := r.host.Post(context.Background(), requestID, conversationID, "There is one board.", extras); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if refusals := r.double.Refusals(); len(refusals) != 0 {
		t.Fatalf("the double refused the post: %v", refusals)
	}
	posts := r.double.Posts()
	if len(posts) != 1 || len(posts[0].Citations) != 1 || posts[0].Citations[0] != "urn:uffici:tool_call:42" ||
		len(posts[0].Activity) != 1 || posts[0].Activity[0].Tool != "web_extract" || posts[0].Question != nil {
		t.Fatalf("the platform recorded %+v", posts)
	}
}

// A post refused for a citation that is not the agent's own (`invalid_citation`,
// 422) is the platform's answer: relayed with its sentence, never sent again.
func TestAnInvalidCitationIsTheAnswerNotARetry(t *testing.T) {
	r := newRig(t)
	r.open(t)
	r.ring(t)
	r.double.Inject("posts", gatewaytest.Injected{
		Status: http.StatusUnprocessableEntity, Body: gatewaytest.Envelope("invalid_citation", false, nil),
	})
	_, err := r.host.Post(context.Background(), requestID, conversationID, "cited",
		&contracts.PostExtras{Citations: []string{"urn:uffici:tool_call:7"}})
	var classified contracts.ClassifiedError
	if !errors.As(err, &classified) || classified.ErrorClass() != "invalid_citation" ||
		classified.ErrorInstruction() == "" {
		t.Fatalf("Post = %v, want the platform's invalid_citation", err)
	}
	if posts := r.double.Posts(); len(posts) != 0 {
		t.Fatalf("the refused post was sent again and recorded: %+v", posts)
	}
	if got := gateway.PlatformErrorClass("invalid_citation"); got != "harness_error" {
		t.Fatalf("an invalid citation closes as %q, want harness_error", got)
	}
}

// 🔴 #2454: a re-run rides the same ticket as a tool call, names the call by its
// URN, and relays the platform's answer as given.
func TestARerunRidesTheRequestsConversationTicket(t *testing.T) {
	r := newRig(t)
	r.open(t)
	r.ring(t)
	boardsCatalog(r)
	if _, err := r.host.CallTool(context.Background(), requestID, "list_boards",
		json.RawMessage(`{"query":"`+argumentMarker+`"}`)); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	result, err := r.host.RerunTool(context.Background(), requestID, "urn:uffici:tool_call:1")
	if err != nil {
		t.Fatalf("RerunTool: %v", err)
	}
	if refusals := r.double.Refusals(); len(refusals) != 0 {
		t.Fatalf("the double refused the re-run: %v", refusals)
	}
	calls := r.double.ToolCalls()
	if len(calls) != 2 || calls[1].RerunOf != "urn:uffici:tool_call:1" || calls[1].Name != "list_boards" ||
		calls[1].RequestID != requestID || calls[1].ConversationID != conversationID {
		t.Fatalf("the platform recorded %+v", calls)
	}
	if result.CallURN != "urn:uffici:tool_call:1" || !strings.Contains(string(result.Result), "Support") {
		t.Fatalf("RerunTool = %+v", result)
	}
	assertNoToolContentLogged(t, r)
}

func TestARerunOfACallNeverMadeIsNotFound(t *testing.T) {
	r := newRig(t)
	r.open(t)
	r.ring(t)
	boardsCatalog(r)
	_, err := r.host.RerunTool(context.Background(), requestID, "urn:uffici:tool_call:404")
	var classified contracts.ClassifiedError
	if !errors.As(err, &classified) || classified.ErrorClass() != "not_found" || classified.ErrorInstruction() == "" {
		t.Fatalf("RerunTool = %v, want the platform's not_found", err)
	}
}

// A call whose arguments were not kept is the platform's RESULT, never an error.
func TestARerunThatCannotRunIsAResultWithNoURN(t *testing.T) {
	r := newRig(t)
	r.open(t)
	r.ring(t)
	boardsCatalog(r)
	r.double.Inject("tool-reruns", gatewaytest.Injected{Status: http.StatusOK,
		Body: `{"callUrn":null,"result":{"ok":false,"error":"rerun_unavailable","message":"The call's arguments were not kept, so it cannot be re-run. Run the tool again with what you need."}}`})
	result, err := r.host.RerunTool(context.Background(), requestID, "urn:uffici:tool_call:1")
	if err != nil || result.CallURN != "" || !strings.Contains(string(result.Result), "rerun_unavailable") {
		t.Fatalf("RerunTool = %+v, %v", result, err)
	}
}

func TestARerunOnARequestNeverRungSendsNothing(t *testing.T) {
	r := newRig(t)
	boardsCatalog(r)
	_, err := r.host.RerunTool(context.Background(), requestID, "urn:uffici:tool_call:1")
	if !errors.Is(err, contracts.ErrUnknownRequest) {
		t.Fatalf("RerunTool on an unrung request = %v, want ErrUnknownRequest", err)
	}
	if mints := r.double.Mints("conversation"); mints != 0 {
		t.Fatalf("%d conversation tickets minted for a request never rung", mints)
	}
}

// 🔴 A re-run is a tool call: a failure after the body left is never sent again.
func TestARerunIsNeverRetriedOnceSent(t *testing.T) {
	r := newRig(t)
	r.open(t)
	r.ring(t)
	boardsCatalog(r)
	if _, err := r.host.CallTool(context.Background(), requestID, "list_boards", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	r.double.Inject("tool-reruns", gatewaytest.Injected{
		Status: http.StatusServiceUnavailable, Body: gatewaytest.Envelope("platform_unavailable", true, nil),
		AfterEffect: true,
	})
	_, err := r.host.RerunTool(context.Background(), requestID, "urn:uffici:tool_call:1")
	var classified contracts.ClassifiedError
	if !errors.As(err, &classified) || classified.ErrorClass() != "platform_unavailable" {
		t.Fatalf("RerunTool = %v, want the platform's classified failure", err)
	}
	if calls := r.double.ToolCalls(); len(calls) != 2 {
		t.Fatalf("%d tool calls recorded, want 2 (the call and ONE re-run)", len(calls))
	}
	if delays := r.backoffDelays(); len(delays) != 0 {
		t.Fatalf("the re-run waited to retry: %v", delays)
	}
}
