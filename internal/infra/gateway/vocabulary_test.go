package gateway

import (
	"slices"
	"sort"
	"testing"

	contract "github.com/uffici-ai/uffici-companion/protocol"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/infra/gateway/gatewaytest"
)

// 🔴 #2106 review: the companion hand-keeps the platform's vocabularies in maps,
// and the byte-identical MANIFEST.json that states them was embedded and never
// asserted against. A platform-side rename would land silently: a ticket refusal
// no longer re-minted, a revoked key reconnecting once a second.

// companionOwnClasses are classes the companion or the plugin mint themselves —
// never the platform's, so the manifest does not list them.
var companionOwnClasses = []string{"redelivery_exhausted", "post_cap_exceeded"}

func manifest(t *testing.T) contract.Manifest {
	t.Helper()
	m, err := contract.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	if len(m.ErrorClasses) == 0 || len(m.DisconnectReasons) == 0 || len(m.CancelReasons) == 0 {
		t.Fatalf("the manifest states an empty vocabulary — this guard is vacuous: %+v", m)
	}
	return m
}

func TestTicketRefusalsAreThePlatformsErrorClasses(t *testing.T) {
	m := manifest(t)
	for class := range ticketRefusals {
		if !slices.Contains(m.ErrorClasses, class) {
			t.Errorf("ticketRefusals names %q, which the platform's errorClasses do not", class)
		}
	}
}

func TestDisconnectKindsAreThePlatformsDisconnectReasons(t *testing.T) {
	m := manifest(t)
	for reason := range disconnectKinds {
		if !slices.Contains(m.DisconnectReasons, reason) {
			t.Errorf("disconnectKinds names %q, which the platform's disconnectReasons do not", reason)
		}
	}
}

func TestCancelReasonsAreThePlatformsBothWays(t *testing.T) {
	m := manifest(t)
	ours := []string{string(contracts.CancelStop), string(contracts.CancelSuperseded), string(contracts.CancelExpired)}
	theirs := slices.Clone(m.CancelReasons)
	sort.Strings(ours)
	sort.Strings(theirs)
	if !slices.Equal(ours, theirs) {
		t.Errorf("the companion's cancel reasons %v are not the platform's %v", ours, theirs)
	}
}

func TestReleaseReasonsAreThePlatformsBothWays(t *testing.T) {
	m := manifest(t)
	ours := []string{string(contracts.ReleasePassEnded), string(contracts.ReleaseArchived), string(contracts.ReleaseDeleted)}
	theirs := slices.Clone(m.ReleaseReasons)
	if len(theirs) == 0 {
		t.Fatal("the manifest states no release reasons — this guard is vacuous")
	}
	sort.Strings(ours)
	sort.Strings(theirs)
	if !slices.Equal(ours, theirs) {
		t.Errorf("the companion's release reasons %v are not the platform's %v", ours, theirs)
	}
}

func TestCloseClassMappingSpeaksOnlyThePlatformsVocabulary(t *testing.T) {
	m := manifest(t)
	for input, output := range closeClassOf {
		known := slices.Contains(m.ErrorClasses, input) ||
			slices.Contains(PlatformCloseClasses, input) ||
			slices.Contains(companionOwnClasses, input)
		if !known {
			t.Errorf("closeClassOf maps %q, which is neither the platform's nor the companion's own", input)
		}
		if !slices.Contains(PlatformCloseClasses, output) {
			t.Errorf("closeClassOf maps %q onto %q, outside the close vocabulary", input, output)
		}
	}
	for _, class := range append(slices.Clone(m.ErrorClasses), companionOwnClasses...) {
		if got := PlatformErrorClass(class); !slices.Contains(PlatformCloseClasses, got) {
			t.Errorf("PlatformErrorClass(%q) = %q, outside the close vocabulary", class, got)
		}
	}
	// The double is an independent copy — it must refuse exactly what the host sends.
	double := make([]string, 0, len(gatewaytest.PlatformErrorClasses))
	for class := range gatewaytest.PlatformErrorClasses {
		double = append(double, class)
	}
	host := slices.Clone(PlatformCloseClasses)
	sort.Strings(double)
	sort.Strings(host)
	if !slices.Equal(double, host) {
		t.Errorf("the double's close vocabulary %v is not the host's %v", double, host)
	}
}
