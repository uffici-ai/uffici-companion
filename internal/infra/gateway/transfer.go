package gateway

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/protocol"
)

// The files routes' base path on the agents host (#2672): tus uploads under it,
// the ranged download beside them. Authorized by the grant alone.
const filesBasePath = "/v1/files"

// ConversationOf returns the conversation of a request this process was rung
// for (#2673) — the transfer engine's answer to "where does this attachment go".
func (h *Host) ConversationOf(requestID string) (string, bool) {
	return h.conversationOf(requestID)
}

// GrantTransfer asks for a transfer grant on a request this companion was rung
// for (#2673), under that request's conversation ticket.
//
// 🔴 A request never rung is refused HERE, before any mint or call. Sent ONCE: a
// grant records a call on the request's ledger, and a second one is a second
// record. The grant is SECRET-CLASS and never reaches a log line; the answer is
// checked against the protocol's shape before it is handed back.
func (h *Host) GrantTransfer(ctx context.Context, requestID string, ask contracts.TransferAsk) (contracts.TransferGrant, error) {
	conversationID, known := h.conversationOf(requestID)
	if !known {
		return contracts.TransferGrant{}, contracts.ErrUnknownRequest
	}
	body := protocol.TransferRequest{Op: string(ask.Op)}
	switch ask.Op {
	case contracts.TransferDownload:
		body.AttachmentID, body.Path = ask.AttachmentID, ask.Path
	case contracts.TransferUpload:
		size := ask.Size
		body.Filename, body.Size, body.CRC32C, body.ContentType = ask.Filename, &size, ask.CRC32C, ask.ContentType
		body.To, body.Overwrite = ask.To, ask.Overwrite
	}
	var answer protocol.TransferResponse
	if err := h.api.ticketedOnce(ctx, conversationID, http.MethodPost, transfersPath(requestID),
		body, &answer, controlAttempts, h.sleep); err != nil {
		h.logFailure("transfer", err)
		return contracts.TransferGrant{}, err
	}
	if answer.Grant == "" || answer.JTI == "" || answer.Op != string(ask.Op) {
		return contracts.TransferGrant{}, localError(ClassPlatformError, http.StatusOK)
	}
	return contracts.TransferGrant{
		Op: contracts.TransferOp(answer.Op), JTI: answer.JTI, Grant: answer.Grant,
		ExpiresAt: answer.ExpiresAt, CallURN: answer.CallURN,
		File: contracts.TransferFile{
			Name: answer.File.Name, Size: answer.File.Size,
			ContentType: answer.File.ContentType, CRC32C: answer.File.CRC32C,
		},
	}, nil
}

// CompleteTransfer records an uploaded file (#2673): attached to the request's
// next post, or saved into Files. Sent once, like the grant.
func (h *Host) CompleteTransfer(ctx context.Context, requestID, jti string) (contracts.TransferCompletion, error) {
	conversationID, known := h.conversationOf(requestID)
	if !known {
		return contracts.TransferCompletion{}, contracts.ErrUnknownRequest
	}
	var answer protocol.TransferCompleteResponse
	if err := h.api.ticketedOnce(ctx, conversationID, http.MethodPost,
		transfersPath(requestID)+"/"+url.PathEscape(jti)+"/complete", nil, &answer, controlAttempts, h.sleep); err != nil {
		h.logFailure("transfer", err)
		return contracts.TransferCompletion{}, err
	}
	if answer.Path == "" && (answer.ID == "" || answer.Ref == "") {
		return contracts.TransferCompletion{}, localError(ClassPlatformError, http.StatusOK)
	}
	return contracts.TransferCompletion{ID: answer.ID, Name: answer.Name, Ref: answer.Ref, Path: answer.Path}, nil
}

// filesURL is the files routes' base URL — the bytes' address, never the
// control plane's paths.
func (h *Host) filesURL() string {
	return strings.TrimSuffix(h.api.base.String(), "/") + filesBasePath
}

func transfersPath(requestID string) string {
	return "/v1/requests/" + url.PathEscape(requestID) + "/transfers"
}

// RetryDelay is the pause before a control-plane retry, exported for the
// transfer engine's download retries (#2673): the same short doubling, so a
// dropped connection mid-file is retried on the companion's one policy.
func RetryDelay(attempt int) time.Duration { return retryDelay(attempt) }

var _ contracts.TransferHost = (*Host)(nil)
