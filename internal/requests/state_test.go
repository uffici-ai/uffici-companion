package requests

import (
	"testing"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
)

// 🔴 The free-local-hop rule, as one assertion. A resumed session that reads an
// opener still answers correctly — nothing fails, nothing logs — it simply pays
// a platform read on every turn forever. Only a rule and a count can see it.
func TestOnlyAFreshSessionEarnsAnOpener(t *testing.T) {
	if !SessionFresh.EarnsOpener() {
		t.Fatal("a fresh session must earn an opener — otherwise a new harness session starts blind")
	}
	if SessionResumed.EarnsOpener() {
		t.Fatal("a resumed session earned an opener: it already holds the conversation, so this is a platform read per turn, forever (decision 43)")
	}
	if Session("something-new").EarnsOpener() {
		t.Fatal("an unknown session state earned an opener — the default must be not to pay for a read nobody asked for")
	}
}

func TestTerminalStatesAreTerminal(t *testing.T) {
	for state, want := range map[State]bool{
		StateDelivered: false,
		StateAcked:     false,
		StateOpening:   false,
		StatePosting:   false,
		StateClosed:    true,
		StateCancelled: true,
		StateExpired:   true,
	} {
		if got := state.Terminal(); got != want {
			t.Errorf("%s.Terminal() = %v, want %v", state, got, want)
		}
	}
}

// 🔴 The mapping that decides whether entries survive. A cancelled or expired
// request must reach the platform as an outcome that does NOT advance the
// offset — otherwise everything it covered vanishes from every later brief.
func TestCancelledAndExpiredNeverReachThePlatformAsAnswered(t *testing.T) {
	for _, testCase := range []struct {
		state State
		done  contracts.Outcome
		want  contracts.Outcome
	}{
		{StateClosed, contracts.OutcomeAnswered, contracts.OutcomeAnswered},
		{StateClosed, contracts.OutcomeEmpty, contracts.OutcomeEmpty},
		{StateClosed, contracts.OutcomeError, contracts.OutcomeError},
		{StateCancelled, contracts.OutcomeAnswered, contracts.OutcomeCancelled},
		{StateExpired, contracts.OutcomeAnswered, contracts.OutcomeCancelled},
		{StatePosting, contracts.OutcomeAnswered, contracts.OutcomeError},
	} {
		got := OutcomeFor(testCase.state, testCase.done)
		if got != testCase.want {
			t.Fatalf("OutcomeFor(%s, %s) = %s, want %s", testCase.state, testCase.done, got, testCase.want)
		}
		if (testCase.state == StateCancelled || testCase.state == StateExpired) && got.AdvancesOffset() {
			t.Fatalf("a %s request reached the platform as %s, which COMMITS the offset — the entries it covered would be lost from every later brief", testCase.state, got)
		}
	}
}

func TestLimitsFillTheirDefaults(t *testing.T) {
	filled := Limits{}.withDefaults()
	if filled != DefaultLimits() {
		t.Fatalf("empty limits filled to %+v, want %+v", filled, DefaultLimits())
	}
	// A caller setting one field keeps the rest.
	partial := Limits{MaxPosts: 2}.withDefaults()
	if partial.MaxPosts != 2 {
		t.Fatalf("MaxPosts is %d, want the caller's 2", partial.MaxPosts)
	}
	if partial.MaxPostBytes != DefaultLimits().MaxPostBytes || partial.AckTimeout != DefaultLimits().AckTimeout {
		t.Fatalf("setting one limit cleared another: %+v", partial)
	}
	// 🔴 Redeliveries must never be unbounded: a plugin rejecting every brief
	// would otherwise be retried forever and the platform would see a request
	// that simply never closes.
	if DefaultLimits().MaxRedeliveries <= 0 {
		t.Fatal("MaxRedeliveries must be a positive bound")
	}
	if DefaultLimits().AckTimeout <= time.Duration(0) {
		t.Fatal("AckTimeout must be positive, or a delivered brief waits forever")
	}
}
