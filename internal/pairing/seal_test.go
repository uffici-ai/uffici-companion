package pairing

import (
	"crypto/ecdh"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"testing"
)

// vector is testdata/seal-vector.json — the portal's sealer reproduces it from the
// same inputs (apps/portal/src/lib/__tests__/pairing-seal.test.ts), so a drift on
// either side of the scheme fails a test on that side.
type vector struct {
	RecipientPrivateKey string `json:"recipientPrivateKey"`
	RecipientPublicKey  string `json:"recipientPublicKey"`
	Binding             struct {
		PairingID string `json:"pairingId"`
		AgentID   string `json:"agentId"`
		OrgSlug   string `json:"orgSlug"`
		AgentSlug string `json:"agentSlug"`
	} `json:"binding"`
	Plaintext string `json:"plaintext"`
	Sealed    Sealed `json:"sealed"`
}

func (v vector) binding() Binding {
	return Binding{PairingID: v.Binding.PairingID, AgentID: v.Binding.AgentID, OrgSlug: v.Binding.OrgSlug, AgentSlug: v.Binding.AgentSlug}
}

func loadVector(t *testing.T) (vector, *ecdh.PrivateKey) {
	t.Helper()
	raw, err := os.ReadFile("testdata/seal-vector.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vector
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	priv, err := base64.RawURLEncoding.DecodeString(v.RecipientPrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdh.X25519().NewPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return v, key
}

func TestTheSharedVectorOpensToItsPlaintext(t *testing.T) {
	v, key := loadVector(t)
	if PublicKey(key) != v.RecipientPublicKey {
		t.Fatalf("the vector's public key is not its private key's: %s", PublicKey(key))
	}
	got, err := Open(v.Sealed, key, v.binding())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got != v.Plaintext {
		t.Fatalf("opened %q, want the vector's plaintext", got)
	}
}

func TestASealOpensOnlyForItsPairingAndItsKey(t *testing.T) {
	v, key := loadVector(t)
	other, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	tampered := v.Sealed
	body, _ := base64.RawURLEncoding.DecodeString(tampered.Ciphertext)
	body[0] ^= 0x01
	tampered.Ciphertext = base64.RawURLEncoding.EncodeToString(body)
	wrongAlg := v.Sealed
	wrongAlg.Alg = "rot13"
	for name, attempt := range map[string]func() (string, error){
		"another pairing": func() (string, error) {
			b := v.binding()
			b.PairingID = "00000000-0000-4000-8000-000000000000"
			return Open(v.Sealed, key, b)
		},
		"another agent": func() (string, error) {
			b := v.binding()
			b.AgentID = "00000000-0000-4000-8000-000000000000"
			return Open(v.Sealed, key, b)
		},
		"another org": func() (string, error) {
			b := v.binding()
			b.OrgSlug = "evil"
			return Open(v.Sealed, key, b)
		},
		"another agent slug": func() (string, error) {
			b := v.binding()
			b.AgentSlug = "archer"
			return Open(v.Sealed, key, b)
		},
		"another key":     func() (string, error) { return Open(v.Sealed, other, v.binding()) },
		"a tampered body": func() (string, error) { return Open(tampered, key, v.binding()) },
		"another scheme":  func() (string, error) { return Open(wrongAlg, key, v.binding()) },
	} {
		if got, err := attempt(); !errors.Is(err, ErrUnopenable) || got != "" {
			t.Errorf("%s: opened %q, err %v — want ErrUnopenable and nothing", name, got, err)
		}
	}
}

func TestAFreshKeyIsThirtyTwoBytesOnTheWire(t *testing.T) {
	key, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	if len(PublicKey(key)) != 43 {
		t.Fatalf("public key %q is not 43 base64url characters", PublicKey(key))
	}
}

func TestMalformedPartsAreUnopenable(t *testing.T) {
	v, key := loadVector(t)
	bad := func(edit func(*Sealed)) Sealed {
		s := v.Sealed
		edit(&s)
		return s
	}
	for name, sealed := range map[string]Sealed{
		"peer not base64":       bad(func(s *Sealed) { s.EphemeralPublicKey = "***" }),
		"peer not a curve key":  bad(func(s *Sealed) { s.EphemeralPublicKey = "AAAA" }),
		"nonce not base64":      bad(func(s *Sealed) { s.Nonce = "***" }),
		"nonce the wrong size":  bad(func(s *Sealed) { s.Nonce = "AAAA" }),
		"ciphertext not base64": bad(func(s *Sealed) { s.Ciphertext = "***" }),
	} {
		if _, err := Open(sealed, key, v.binding()); !errors.Is(err, ErrUnopenable) {
			t.Errorf("%s: %v, want ErrUnopenable", name, err)
		}
	}
}
