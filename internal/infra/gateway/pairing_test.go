package gateway_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/uffici-ai/uffici-companion/internal/infra/gateway"
	"github.com/uffici-ai/uffici-companion/internal/infra/gateway/gatewaytest"
	"github.com/uffici-ai/uffici-companion/internal/pairing"
	"github.com/uffici-ai/uffici-companion/internal/protocol"
)

func pairerFor(t *testing.T, double *gatewaytest.Double) *gateway.Pairer {
	t.Helper()
	pairer, err := gateway.NewPairer(gateway.PairerOptions{
		BaseURL: double.URL(), InstanceID: "d4c3b2a1-f6e5-4d7c-8b9a-0f1e2d3c4b5a", HTTPClient: double.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return pairer
}

func TestAPairerNeedsAnHTTPSAgentsHost(t *testing.T) {
	for _, base := range []string{"http://agents.example", "not a url", ""} {
		if _, err := gateway.NewPairer(gateway.PairerOptions{BaseURL: base}); err == nil {
			t.Errorf("NewPairer(%q) built a pairer", base)
		}
	}
	if _, err := gateway.NewPairer(gateway.PairerOptions{BaseURL: "https://agents.example"}); err != nil {
		t.Fatalf("defaults: %v", err)
	}
}

// 🔴 Opening a pairing carries no credential, and a malformed answer is refused.
func TestOpeningAPairingSendsNoCredentialAndChecksTheAnswer(t *testing.T) {
	double := gatewaytest.New()
	t.Cleanup(double.Close)
	pairer := pairerFor(t, double)
	opened, err := pairer.Open(context.Background(), protocol.HarnessFacts{Kind: "hermes", Version: "0.21.4"}, "1.2.3",
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "")
	if err != nil || opened.Code == "" || opened.PollToken == "" {
		t.Fatalf("Open = %+v, %v", opened, err)
	}
	if refusals := double.Refusals(); len(refusals) != 0 {
		t.Fatalf("the double refused: %v", refusals)
	}
	double.Inject("pairing", gatewaytest.Injected{Status: http.StatusCreated, ContentType: "application/json", Body: `{}`})
	if _, err := pairer.Open(context.Background(), protocol.HarnessFacts{Kind: "hermes", Version: "0.21.4"}, "1.2.3",
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", ""); err == nil {
		t.Fatal("an answer with no code or token was accepted")
	}
}

// A portal-issued code (#2209) rides the open request; the double — the gateway
// and the portal's claim together — pairs at once for a code it was offered,
// and answers `not_found` for one it was not, creating nothing.
func TestAPortalCodeIsSentAndAnUnknownOneIsNotFound(t *testing.T) {
	double := gatewaytest.New()
	t.Cleanup(double.Close)
	pairer := pairerFor(t, double)
	facts := protocol.HarnessFacts{Kind: "hermes", Version: "0.21.4"}
	// A real X25519 key: the double seals to it, as the portal would.
	key, err := pairing.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	publicKey := pairing.PublicKey(key)

	_, err = pairer.Open(context.Background(), facts, "1.2.3", publicKey, "ZZZZ-ZZZZ")
	var classified *gateway.Error
	if !errors.As(err, &classified) || classified.Class != "not_found" || classified.Status != http.StatusNotFound {
		t.Fatalf("an unknown code opened: %v", err)
	}
	if len(double.PairingRequests()) != 0 {
		t.Fatal("a refused code left a pairing behind")
	}

	double.OfferPairing("K7QP-2XMD", "e2a7f46c-aa42-48ec-ae37-a63815f4a700", "acme", "smith", "uffici_live_key_9e9c")
	opened, err := pairer.Open(context.Background(), facts, "1.2.3", publicKey, "K7QP-2XMD")
	if err != nil {
		t.Fatal(err)
	}
	if requests := double.PairingRequests(); len(requests) != 1 || requests[0].Code != "K7QP-2XMD" {
		t.Fatalf("the code did not ride the request: %+v", requests)
	}
	answer, err := pairer.Poll(context.Background(), opened.PairingID, opened.PollToken)
	if err != nil || answer.Status != protocol.PairingPaired || answer.KeyLast4 != "9e9c" {
		t.Fatalf("the first poll = %+v, %v", answer, err)
	}
	// Spent: the same code again is `not_found`.
	if _, err := pairer.Open(context.Background(), facts, "1.2.3", publicKey, "K7QP-2XMD"); !errors.As(err, &classified) || classified.Class != "not_found" {
		t.Fatalf("a spent code opened again: %v", err)
	}
	if refusals := double.Refusals(); len(refusals) != 0 {
		t.Fatalf("the double refused: %v", refusals)
	}
}

func TestAPollRefusesAnAnswerOutsideTheContract(t *testing.T) {
	double := gatewaytest.New()
	t.Cleanup(double.Close)
	pairer := pairerFor(t, double)
	for name, body := range map[string]string{
		"paired without a seal": `{"status":"paired","agentId":"a","keyLast4":"9e9c"}`,
		"an unknown status":     `{"status":"approved"}`,
	} {
		double.Inject("pairing-poll", gatewaytest.Injected{Status: http.StatusOK, ContentType: "application/json", Body: body})
		_, err := pairer.Poll(context.Background(), "3a2b1c0d-9e8f-4a7b-8c6d-000000000001", "token")
		var classified *gateway.Error
		if !errors.As(err, &classified) || classified.Class != gateway.ClassPlatformError {
			t.Errorf("%s: err %v, want platform_error", name, err)
		}
	}
	double.Inject("pairing-poll", gatewaytest.Injected{
		Status: http.StatusNotFound, ContentType: "application/json",
		Body: `{"error":{"class":"not_found","instruction":"That pairing has expired.","retryable":false}}`,
	})
	if _, err := pairer.Poll(context.Background(), "3a2b1c0d-9e8f-4a7b-8c6d-000000000001", "token"); err == nil {
		t.Fatal("a not_found poll was not an error")
	}
}
