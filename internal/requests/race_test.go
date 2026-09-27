package requests

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
)

// 🔴 #2112: an ack racing a cancel. `requestOn` checks "not terminal" under the
// lock and releases it; a write that follows without re-checking resurrects a
// request the cancel has already withdrawn — and `cancel` then read the state
// back and told the platform `error/stop` for a stop the person asked for.

// cancelRacingSink runs `during` inside SendCancel — the window `cancel` spends
// outside the lock, where a frame for the same request is being handled.
type cancelRacingSink struct {
	*recorder
	during func()
}

func (c *cancelRacingSink) SendCancel(ctx context.Context, requestID string, reason contracts.CancelReason) error {
	if c.during != nil {
		c.during()
	}
	return c.recorder.SendCancel(ctx, requestID, reason)
}

func TestACancelClosesWithTheOutcomeOfTheStateItSet(t *testing.T) {
	ctx := context.Background()
	host := &failingHost{Host: seededHost()}
	manager, _ := failingRig(t, host)
	sink := &cancelRacingSink{recorder: newRecorder()}
	manager.Attach("conn-1", sink, 48000)
	if err := manager.OnRing(ctx, ring("req_1", "c0001", "c0003")); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	request := manager.byID["req_1"]
	manager.mu.Unlock()
	// The unguarded ack write, landing inside the cancel's I/O window.
	sink.during = func() {
		manager.mu.Lock()
		request.State = StateAcked
		manager.mu.Unlock()
	}
	if err := manager.OnPlatformCancel(ctx, "req_1", contracts.CancelStop); err != nil {
		t.Fatal(err)
	}
	if len(host.closes) != 1 || host.closes[0].outcome != contracts.OutcomeCancelled {
		t.Fatalf("closes = %+v, want one `cancelled` — a stop reached the platform as an error", host.closes)
	}
}

func TestAWithdrawnRequestCannotBeMovedAgain(t *testing.T) {
	manager := New(seededHost(), Options{})
	for _, terminal := range []State{StateCancelled, StateExpired, StateClosed} {
		request := &Request{RequestID: "req_x", State: terminal}
		for _, to := range []State{StateDelivered, StateAcked, StateOpening, StatePosting, StateClosed} {
			if manager.advance(request, to) {
				t.Errorf("advance moved a %s request to %s", terminal, to)
			}
			if request.State != terminal {
				t.Fatalf("a %s request became %s", terminal, request.State)
			}
		}
	}
	live := &Request{RequestID: "req_y", State: StateDelivered}
	if !manager.advance(live, StateAcked) || live.State != StateAcked {
		t.Fatalf("advance refused a live request: %s", live.State)
	}
}

// Every write of a request's state goes through `advance`, or is one of the few
// transitions that OWN a request's end (they check and claim in one critical
// section). A new bare write is the #2112 shape again.
func TestEveryStateWriteIsGuarded(t *testing.T) {
	source, err := os.ReadFile("manager.go")
	if err != nil {
		t.Fatal(err)
	}
	owners := map[string]bool{"advanceLocked": true, "advance": true, "cancel": true, "fail": true, "forget": true, "closeUpstream": true}
	fn := regexp.MustCompile(`^func \([a-z]+ \*Manager\) ([A-Za-z]+)\(`)
	current := ""
	writes := 0
	for i, line := range strings.Split(string(source), "\n") {
		if m := fn.FindStringSubmatch(line); m != nil {
			current = m[1]
		}
		if strings.Contains(line, ".State = ") && !strings.HasPrefix(strings.TrimSpace(line), "//") {
			writes++
			if !owners[current] {
				t.Errorf("manager.go:%d — %s writes a request's state without advance: %s", i+1, current, strings.TrimSpace(line))
			}
		}
	}
	if writes == 0 {
		t.Fatal("found no state writes — this guard is vacuous")
	}
}
