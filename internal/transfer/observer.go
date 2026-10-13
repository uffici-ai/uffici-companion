package transfer

import (
	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/requests"
)

// Observer is the engine as the request machine's observer: 🔴 a request that is
// cancelled — stopped, superseded, expired — or closed takes its transfers with
// it, and each deletes its partial file as it unwinds (#2673 requirement 8).
func (e *Engine) Observer() requests.Observer { return abortOnEnd{engine: e} }

type abortOnEnd struct {
	requests.NopObserver
	engine *Engine
}

// Cancelled aborts the request's transfers.
func (o abortOnEnd) Cancelled(requestID string, _ contracts.CancelReason) {
	o.engine.Abort(requestID)
}

// Closed aborts what is left of the request's transfers: a closed request's
// grants are spent.
func (o abortOnEnd) Closed(requestID string, _ contracts.Outcome, _ string) {
	o.engine.Abort(requestID)
}
