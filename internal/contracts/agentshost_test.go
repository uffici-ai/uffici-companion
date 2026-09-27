package contracts

import "testing"

// 🔴 The one rule in this package that is a rule rather than a shape, and the
// one whose violation is silent: advancing a conversation's offset on a request
// that was cancelled or errored means the next brief starts AFTER entries
// nobody answered. Those entries are then invisible to every agent, forever —
// no error, no log line, nothing to notice. Decision 40 names only two outcomes
// that commit, and this is where that lives.
func TestOnlyAnsweredAndEmptyAdvanceTheOffset(t *testing.T) {
	for outcome, want := range map[Outcome]bool{
		OutcomeAnswered:  true,
		OutcomeEmpty:     true,
		OutcomeError:     false,
		OutcomeCancelled: false,
	} {
		if got := outcome.AdvancesOffset(); got != want {
			t.Errorf("%s.AdvancesOffset() = %v, want %v — see decision 40: a committed offset means those entries were considered", outcome, got, want)
		}
	}
}

// Non-vacuity: an unknown outcome must not quietly commit.
func TestAnUnknownOutcomeDoesNotAdvanceTheOffset(t *testing.T) {
	if Outcome("something-new").AdvancesOffset() {
		t.Fatal("an outcome this build does not know advanced the offset — the default must be to keep the entries in range")
	}
}
