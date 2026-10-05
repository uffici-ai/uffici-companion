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
)

// factMarker stands for tenant content: a fact or a query may never reach a log.
const factMarker = "fact-marker-do-not-log"

func TestMemoryRemembersAndRecallsOnTheRequestsConversation(t *testing.T) {
	r := newRig(t)
	r.open(t)
	r.ring(t)
	ctx := context.Background()
	remembered, err := r.host.Memory(ctx, contracts.MemoryRemember, requestID,
		json.RawMessage(`{"requestId":"`+requestID+`","text":"The pump leaks: `+factMarker+`"}`))
	if err != nil || !strings.Contains(string(remembered), `"urn":"urn:uffici:fact:`) {
		t.Fatalf("remember = %s, %v", remembered, err)
	}
	recalled, err := r.host.Memory(ctx, contracts.MemoryRecall, requestID,
		json.RawMessage(`{"requestId":"`+requestID+`","query":"does the pump leak?","deadlineMs":3000}`))
	if err != nil || !strings.Contains(string(recalled), factMarker) {
		t.Fatalf("recall = %s, %v", recalled, err)
	}
	calls := r.double.MemoryCalls()
	if len(calls) != 2 || calls[0].ConversationID != conversationID || calls[1].Op != "recall" {
		t.Fatalf("the double admitted %+v", calls)
	}
	if strings.Contains(r.logs.String(), factMarker) {
		t.Fatalf("a log line carries a fact:\n%s", r.logs.String())
	}
}

// 🔴 Refused HERE, before any ticket or call: a request this process never rang.
func TestMemoryOnARequestNeverRungSendsNothing(t *testing.T) {
	r := newRig(t)
	_, err := r.host.Memory(context.Background(), contracts.MemoryRecall, "req_never_rung", json.RawMessage(`{}`))
	if !errors.Is(err, contracts.ErrUnknownRequest) {
		t.Fatalf("Memory on an unrung request = %v", err)
	}
	if len(r.double.MemoryCalls()) != 0 || r.double.Reads("conversation") != 0 {
		t.Fatal("something was sent for a request never rung")
	}
}

// 🔴 The request id is the ARGUMENT's, never one the body carried — a plugin cannot
// bind a memory op to another request than the one the companion resolved.
func TestMemoryStampsTheResolvedRequest(t *testing.T) {
	r := newRig(t)
	r.open(t)
	r.ring(t)
	const other = "00000000-0000-4000-8000-0000000000ff"
	if _, err := r.host.Memory(context.Background(), contracts.MemoryRemember, requestID,
		json.RawMessage(`{"requestId":"`+other+`","text":"x"}`)); err != nil {
		t.Fatalf("remember: %v", err)
	}
	if calls := r.double.MemoryCalls(); len(calls) != 1 || calls[0].RequestID != requestID {
		t.Fatalf("the double saw %+v, want the resolved request %s", calls, requestID)
	}
}

func TestMemoryRefusesAFieldThePlatformWouldNotTakeBeforeSending(t *testing.T) {
	r := newRig(t)
	r.open(t)
	r.ring(t)
	_, err := r.host.Memory(context.Background(), contracts.MemoryRemember, requestID,
		json.RawMessage(`{"requestId":"`+requestID+`","text":"x","level":"organization"}`))
	var classified contracts.ClassifiedError
	if !errors.As(err, &classified) || classified.ErrorClass() != gateway.ClassPlatformError {
		t.Fatalf("a level argument read as %v", err)
	}
	if len(r.double.MemoryCalls()) != 0 {
		t.Fatal("an off-contract body was sent")
	}
}

func TestMemoryRelaysTheGatewaysClass(t *testing.T) {
	r := newRig(t)
	r.open(t)
	r.ring(t)
	_, err := r.host.Memory(context.Background(), contracts.MemoryForget, requestID,
		json.RawMessage(`{"requestId":"`+requestID+`","urn":"urn:uffici:fact:9c8b7a6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d"}`))
	var classified contracts.ClassifiedError
	if !errors.As(err, &classified) || classified.ErrorClass() != "forget_refused" {
		t.Fatalf("a refused forget read as %v", err)
	}
}

func TestMemoryAnAnswerOffTheProtocolIsPlatformError(t *testing.T) {
	r := newRig(t)
	r.open(t)
	r.ring(t)
	r.double.Inject("memory", gatewaytest.Injected{Status: http.StatusOK, Body: `{"urn":7}`})
	_, err := r.host.Memory(context.Background(), contracts.MemoryRemember, requestID,
		json.RawMessage(`{"requestId":"`+requestID+`","text":"x"}`))
	var classified contracts.ClassifiedError
	if !errors.As(err, &classified) {
		t.Fatalf("an off-shape answer read as %v", err)
	}
}

func TestMemoryNamesNoOpItDoesNotKnow(t *testing.T) {
	r := newRig(t)
	r.open(t)
	r.ring(t)
	if _, err := r.host.Memory(context.Background(), contracts.MemoryOp("dream"), requestID, nil); err == nil {
		t.Fatal("an unknown op was sent")
	}
}
