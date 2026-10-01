package gatewaytest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/protocol"
)

// LogsShipping is the log policy the double answers on a logs-ticket mint.
type LogsShipping struct {
	Enabled      bool
	HarnessLevel string
}

// SetLogsShipping scripts the policy the next logs-ticket mint answers.
func (d *Double) SetLogsShipping(shipping LogsShipping) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.logsShipping = shipping
}

// HoldLogPushes makes every `POST /v1/logs` wait until the returned release is
// called — a push held in flight while the control plane carries on.
func (d *Double) HoldLogPushes() (release func()) {
	gate := make(chan struct{})
	d.mu.Lock()
	d.logsGate = gate
	d.mu.Unlock()
	return func() { close(gate) }
}

// LogBatches is every batch the double accepted, in order.
func (d *Double) LogBatches() []protocol.LogBatch {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]protocol.LogBatch(nil), d.logBatches...)
}

func (d *Double) mintLogs(w http.ResponseWriter, r *http.Request) {
	if answer, ok := d.next("logs-ticket"); ok {
		writeInjected(w, answer)
		return
	}
	if bearer(r) != Key {
		fail(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if err := protocol.Validate("logs-ticket-request", readBody(r)); err != nil {
		d.refuse("logs-ticket: " + err.Error())
		fail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	d.mu.Lock()
	d.mints["logs"]++
	shipping := d.logsShipping
	answer := protocol.LogsTicketResponse{Shipping: protocol.LogsShipping{
		Enabled: shipping.Enabled, HarnessLevel: shipping.HarnessLevel, RecheckSeconds: 900,
	}}
	if shipping.Enabled {
		d.ticketSeq++
		token := fmt.Sprintf("logs-ticket-%d", d.ticketSeq)
		d.logsTickets[token] = true
		expires := time.Now().Add(time.Hour).UTC()
		answer.Ticket, answer.ExpiresAt = token, &expires
	}
	d.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(answer)
}

func (d *Double) pushLogs(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	gate := d.logsGate
	d.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
	}
	if answer, ok := d.next("logs"); ok {
		writeInjected(w, answer)
		return
	}
	d.mu.Lock()
	known := d.logsTickets[bearer(r)]
	d.mu.Unlock()
	if !known {
		fail(w, http.StatusUnauthorized, "ticket_invalid")
		return
	}
	raw := readBody(r)
	if err := protocol.Validate("log-batch", raw); err != nil {
		d.refuse("logs: " + err.Error())
		fail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var batch protocol.LogBatch
	_ = json.Unmarshal(raw, &batch)
	d.mu.Lock()
	d.logBatches = append(d.logBatches, batch)
	d.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(protocol.LogBatchResponse{Accepted: len(batch.Entries)})
}
