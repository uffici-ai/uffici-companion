// Package waits holds the companion's one context-aware sleep: every retry
// loop — the gateway's calls and doorbell, pairing, a transfer's bytes — waits
// through it, so a request that ends stops its backoff at once.
package waits

import (
	"context"
	"time"
)

// Sleep waits d, or returns the context's error as soon as it ends.
func Sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
