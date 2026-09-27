package wire

import "github.com/uffici-ai/uffici-companion/internal/contracts"

// wireStopped is the local wire's word for a person's stop. The platform's
// protocol says `stop`; the two vocabularies are versioned separately, so they
// meet in exactly these two functions and nowhere else (#2077).
const wireStopped = "stopped"

// CancelReasonOnWire is the local wire's word for a cancel reason.
func CancelReasonOnWire(reason contracts.CancelReason) string {
	if reason == contracts.CancelStop {
		return wireStopped
	}
	return string(reason)
}

// CancelReasonFromWire reads a local-wire cancel reason — what a fixture script
// says, since scripts speak the wire's vocabulary — as the contract's.
func CancelReasonFromWire(reason string) contracts.CancelReason {
	if reason == wireStopped {
		return contracts.CancelStop
	}
	return contracts.CancelReason(reason)
}

// CancelFailed is the local wire's reason for a request withdrawn because the
// platform refused or failed a call it needed. It has no platform counterpart:
// the platform does not cancel a request for failing — the companion closes it.
const CancelFailed = "failed"
