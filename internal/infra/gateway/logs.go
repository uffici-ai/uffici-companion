package gateway

import (
	"context"
	"net/http"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/protocol"
)

// busyWindow is how long after a control-plane call starts the host still
// counts as busy, so a push does not slip in between two calls of one turn.
const busyWindow = time.Second

// MintLogsTicket asks for the agent's log policy, and a logs ticket when
// shipping is on (`POST /v1/tickets/logs`).
//
// 🔴 On the LOG client, never the conversation client: it shares no connection
// pool, retry budget or attempt count with a call a person is waiting on, and
// it is sent once — the shipper decides when to try again.
func (h *Host) MintLogsTicket(ctx context.Context) (contracts.LogsShipping, error) {
	var answer protocol.LogsTicketResponse
	err := h.logsAPI.call(ctx, http.MethodPost, "/v1/tickets/logs", nil, h.logsAPI.key,
		protocol.LogsTicketRequest{InstanceID: h.logsAPI.instanceID}, &answer)
	if err != nil {
		return contracts.LogsShipping{}, err
	}
	shipping := contracts.LogsShipping{
		Enabled:      answer.Shipping.Enabled,
		HarnessLevel: answer.Shipping.HarnessLevel,
		Recheck:      time.Duration(answer.Shipping.RecheckSeconds) * time.Second,
		Ticket:       answer.Ticket,
	}
	if answer.ExpiresAt != nil {
		shipping.ExpiresAt = *answer.ExpiresAt
	}
	if shipping.Enabled && shipping.Ticket == "" {
		return contracts.LogsShipping{}, localError(ClassPlatformError, http.StatusOK)
	}
	return shipping, nil
}

// PushLogs sends one batch under a logs ticket (`POST /v1/logs`), once.
func (h *Host) PushLogs(ctx context.Context, ticket string, batch contracts.LogBatch) (contracts.LogPushResult, error) {
	body := protocol.LogBatch{
		Client: protocol.LogBatchClient{
			Name: batch.Client.Name, Version: batch.Client.Version,
			HarnessKind: clipFact(batch.Client.HarnessKind), HarnessVersion: clipFact(batch.Client.HarnessVersion),
			PluginVersion: clipFact(batch.Client.PluginVersion),
		},
		Entries: make([]protocol.LogEntry, 0, len(batch.Entries)),
	}
	for _, e := range batch.Entries {
		body.Entries = append(body.Entries, protocol.LogEntry{
			TS: e.TS, Source: e.Source, Level: e.Level, Logger: e.Logger,
			Message: e.Message, Attrs: e.Attrs, Redactions: e.Redactions,
		})
	}
	var answer protocol.LogBatchResponse
	if err := h.logsAPI.call(ctx, http.MethodPost, "/v1/logs", nil, ticket, body, &answer); err != nil {
		return contracts.LogPushResult{}, err
	}
	return contracts.LogPushResult{Accepted: answer.Accepted, Filtered: answer.Filtered}, nil
}

// Busy reports conversation traffic: a control-plane call in flight, or one
// started within the last second (`contracts.ControlPlaneActivity`).
func (h *Host) Busy() bool {
	if h.api.inFlight.Load() > 0 {
		return true
	}
	last := h.api.lastStart.Load()
	return last != 0 && h.api.now().Sub(time.Unix(0, last)) < busyWindow
}
