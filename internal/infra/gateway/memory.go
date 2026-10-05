package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/protocol"
)

// memoryShapes pairs each memory op with the protocol's request and answer
// structs — the companion's typed copy of the platform's schemas (#2385).
func memoryShapes(op contracts.MemoryOp) (request, answer any, err error) {
	switch op {
	case contracts.MemoryRecall:
		return &protocol.MemoryRecallRequest{}, &protocol.MemoryRecallResponse{}, nil
	case contracts.MemoryRemember:
		return &protocol.MemoryRememberRequest{}, &protocol.MemoryRememberResponse{}, nil
	case contracts.MemoryForget:
		return &protocol.MemoryForgetRequest{}, &protocol.MemoryForgetResponse{}, nil
	case contracts.MemoryUnfold:
		return &protocol.MemoryUnfoldRequest{}, &protocol.MemoryUnfoldResponse{}, nil
	}
	return nil, nil, fmt.Errorf("gateway: no memory op %q", op)
}

// Memory runs one memory op on a request this companion was rung for, under the
// ticket of that request's conversation (#2385).
//
// 🔴 A request this process never saw rung is refused HERE, before any ticket
// mint or call — exactly as a tool call is. The body is decoded into the
// protocol's request struct (so a field the platform would refuse never leaves),
// the request id set from the argument, sent ONCE, and the answer decoded into the
// protocol's answer struct before it is handed back: a field this build does not
// know is dropped, never passed through (answers decode leniently, for forward
// compatibility), and the wire's result schema is checked again by memoryops.
// Neither is ever logged.
func (h *Host) Memory(ctx context.Context, op contracts.MemoryOp, requestID string, body json.RawMessage) (json.RawMessage, error) {
	conversationID, known := h.conversationOf(requestID)
	if !known {
		return nil, contracts.ErrUnknownRequest
	}
	request, answer, err := memoryShapes(op)
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		body = json.RawMessage("{}")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(request); err != nil {
		return nil, localError(ClassPlatformError, http.StatusBadRequest)
	}
	setRequestID(request, requestID)
	if err := h.api.ticketedOnce(ctx, conversationID, http.MethodPost, "/v1/memory/"+string(op),
		request, answer, controlAttempts, h.sleep); err != nil {
		h.logFailure("memory", err)
		return nil, err
	}
	encoded, err := json.Marshal(answer)
	if err != nil {
		return nil, localError(ClassPlatformError, http.StatusOK)
	}
	return encoded, nil
}

// setRequestID stamps the request a memory op is bound to — the argument's, never
// one the body carried.
func setRequestID(request any, requestID string) {
	switch r := request.(type) {
	case *protocol.MemoryRecallRequest:
		r.RequestID = requestID
	case *protocol.MemoryRememberRequest:
		r.RequestID = requestID
	case *protocol.MemoryForgetRequest:
		r.RequestID = requestID
	case *protocol.MemoryUnfoldRequest:
		r.RequestID = requestID
	}
}
