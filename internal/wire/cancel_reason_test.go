package wire

import (
	"testing"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
)

// The two vocabularies meet here; every reason the platform can send must
// arrive on the local wire as a word the wire's schema admits.
func TestEveryPlatformCancelReasonMapsOntoTheWire(t *testing.T) {
	for reason, want := range map[contracts.CancelReason]string{
		contracts.CancelStop:       "stopped",
		contracts.CancelSuperseded: "superseded",
		contracts.CancelExpired:    "expired",
	} {
		got := CancelReasonOnWire(reason)
		if got != want {
			t.Fatalf("%q reaches the wire as %q, want %q", reason, got, want)
		}
		if back := CancelReasonFromWire(got); back != reason {
			t.Fatalf("%q does not round-trip: back as %q", reason, back)
		}
		raw := []byte(`{"v":1,"type":"cancel","requestId":"req_1","reason":"` + got + `"}`)
		if err := Validate(raw); err != nil {
			t.Fatalf("the wire refuses the mapped reason %q: %v", got, err)
		}
	}
}
