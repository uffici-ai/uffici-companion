// Package logsim is the log side of the companion's stand-in hosts: a
// contracts.LogHost that answers a scriptable policy and RECORDS every batch
// pushed to it, and a ControlPlaneActivity that is idle unless told otherwise.
//
// One implementation for both stand-ins — the fixture and the in-memory host
// embed it — so what "the fixture recorded" means is the same wherever a test
// or an end-to-end run reads it.
package logsim

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
)

// Ticket is the one ticket a recorder issues.
const Ticket = "logsim-ticket"

// Recorder is the scriptable, recording LogHost.
type Recorder struct {
	mu       sync.Mutex
	shipping contracts.LogsShipping
	batches  []contracts.LogBatch
	mints    int
	busy     bool
	failNext error
}

// NewRecorder answers shipping ON at the platform's default Warning floor.
func NewRecorder() *Recorder {
	return &Recorder{shipping: contracts.LogsShipping{
		Enabled: true, HarnessLevel: "warn", Recheck: 15 * time.Minute,
	}}
}

// SetShipping scripts the policy the next mint answers.
func (r *Recorder) SetShipping(enabled bool, harnessLevel string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.shipping.Enabled = enabled
	r.shipping.HarnessLevel = harnessLevel
}

// SetBusy scripts ControlPlaneActivity.
func (r *Recorder) SetBusy(busy bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.busy = busy
}

// FailNextPush makes the next push answer err.
func (r *Recorder) FailNextPush(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failNext = err
}

// MintLogsTicket answers the scripted policy, with a ticket when it is on.
func (r *Recorder) MintLogsTicket(_ context.Context) (contracts.LogsShipping, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mints++
	answer := r.shipping
	if answer.Enabled {
		answer.Ticket = Ticket
		answer.ExpiresAt = time.Now().Add(time.Hour)
	}
	return answer, nil
}

// PushLogs records the batch and answers it all accepted.
func (r *Recorder) PushLogs(_ context.Context, ticket string, batch contracts.LogBatch) (contracts.LogPushResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failNext != nil {
		err := r.failNext
		r.failNext = nil
		return contracts.LogPushResult{}, err
	}
	if ticket != Ticket {
		return contracts.LogPushResult{}, fmt.Errorf("logsim: a push without the issued ticket")
	}
	if !r.shipping.Enabled {
		// What the gateway answers once an administrator switched shipping off.
		return contracts.LogPushResult{}, Refusal{Class: "logs_disabled", After: 900 * time.Second}
	}
	if len(batch.Entries) == 0 {
		return contracts.LogPushResult{}, fmt.Errorf("logsim: an empty push")
	}
	copied := batch
	copied.Entries = append([]contracts.LogEntry(nil), batch.Entries...)
	r.batches = append(r.batches, copied)
	return contracts.LogPushResult{Accepted: len(batch.Entries)}, nil
}

// Busy answers the scripted activity.
func (r *Recorder) Busy() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.busy
}

// Batches is every batch pushed, in order.
func (r *Recorder) Batches() []contracts.LogBatch {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]contracts.LogBatch(nil), r.batches...)
}

// Mints counts logs-ticket mints.
func (r *Recorder) Mints() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.mints
}

// Counts is the recorded lines per source, and the redactions they carried —
// what a `--once` report prints.
func (r *Recorder) Counts() (bySource map[string]int, redactions int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	bySource = map[string]int{}
	for _, batch := range r.batches {
		for _, e := range batch.Entries {
			bySource[e.Source]++
			redactions += e.Redactions
		}
	}
	return bySource, redactions
}

// Refusal is a classified platform refusal with the delay it names — what the
// real gateway's error carries.
type Refusal struct {
	Class string
	After time.Duration
}

func (r Refusal) Error() string { return "logsim: refused " + r.Class }

// ErrorClass is the refusal's class.
func (r Refusal) ErrorClass() string { return r.Class }

// ErrorInstruction is empty: nothing here reaches a person.
func (r Refusal) ErrorInstruction() string { return "" }

// RetryAfterDelay is the delay the refusal names.
func (r Refusal) RetryAfterDelay() time.Duration { return r.After }
