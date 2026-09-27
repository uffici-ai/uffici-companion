package gatewaytest

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/protocol"
)

// pendingPairing is one pairing the double opened (#1966).
type pendingPairing struct {
	id        string
	code      string
	pollToken string
	request   protocol.PairingRequest
	expiresAt time.Time
	paired    *protocol.PairingPollResponse
	delivered bool
}

// openPairing serves `POST /v1/pairings` — no credential, as the gateway.
func (d *Double) openPairing(w http.ResponseWriter, r *http.Request) {
	if answer, ok := d.next("pairing"); ok {
		writeInjected(w, answer)
		return
	}
	if r.Header.Get("Authorization") != "" {
		// Pairing asks with NO credential; a companion that sent one is a bug.
		d.refuse("pairing: sent an Authorization header")
	}
	raw := readBody(r)
	if err := protocol.Validate("pairing-request", raw); err != nil {
		d.refuse("pairing: " + err.Error())
		fail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var body protocol.PairingRequest
	_ = json.Unmarshal(raw, &body)
	d.mu.Lock()
	d.pairingSeq++
	p := &pendingPairing{
		id:        fmt.Sprintf("3a2b1c0d-9e8f-4a7b-8c6d-%012d", d.pairingSeq),
		code:      "K7QP-2XM" + string("ABCDEFGHJK"[d.pairingSeq%10]),
		pollToken: strings.Repeat("p", 42) + fmt.Sprint(d.pairingSeq%10),
		request:   body,
		expiresAt: time.Now().Add(d.PairingTTL),
	}
	d.pairings[p.id] = p
	d.mu.Unlock()
	answer, _ := json.Marshal(protocol.PairingResponse{
		PairingID: p.id, Code: p.code, PollToken: p.pollToken,
		ExpiresAt: p.expiresAt.UTC().Format(time.RFC3339), PollAfterSeconds: 1,
	})
	if err := protocol.Validate("pairing-response", answer); err != nil {
		d.refuse("pairing answer: " + err.Error())
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(answer)
}

// pollPairing serves `GET /v1/pairings/{id}` — the poll token, then the key once.
func (d *Double) pollPairing(w http.ResponseWriter, r *http.Request) {
	if answer, ok := d.next("pairing-poll"); ok {
		writeInjected(w, answer)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/pairings/")
	d.mu.Lock()
	p, ok := d.pairings[id]
	if !ok || bearer(r) != p.pollToken || time.Now().After(p.expiresAt) || p.delivered {
		d.mu.Unlock()
		fail(w, http.StatusNotFound, "not_found")
		return
	}
	answer := protocol.PairingPollResponse{Status: protocol.PairingPending, RetryAfterSeconds: 1}
	if p.paired != nil {
		answer = *p.paired
		p.delivered = true
	}
	d.mu.Unlock()
	raw, _ := json.Marshal(answer)
	if err := protocol.Validate("pairing-poll-response", raw); err != nil {
		d.refuse("pairing poll answer: " + err.Error())
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

// ApprovePairing plays the admin in the portal: it seals `key` to the pairing
// `code` names — the portal's scheme, byte for byte — so the companion's next
// poll collects it. Test-only: in production the PORTAL seals, never the gateway.
func (d *Double) ApprovePairing(code, agentID, orgSlug, agentSlug, key string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, p := range d.pairings {
		if p.code != code {
			continue
		}
		sealed, err := sealForTest(key, p.request.PublicKey, p.id+"\n"+agentID+"\n"+orgSlug+"\n"+agentSlug)
		if err != nil {
			return err
		}
		p.paired = &protocol.PairingPollResponse{
			Status: protocol.PairingPaired, AgentID: agentID, OrgSlug: orgSlug, AgentSlug: agentSlug,
			KeyLast4: key[len(key)-4:], Sealed: &sealed,
		}
		return nil
	}
	return fmt.Errorf("gatewaytest: no pairing with code %q", code)
}

// PairingRequests are the pairing bodies the double received, in order.
func (d *Double) PairingRequests() []protocol.PairingRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []protocol.PairingRequest
	for i := 1; i <= d.pairingSeq; i++ {
		if p, ok := d.pairings[fmt.Sprintf("3a2b1c0d-9e8f-4a7b-8c6d-%012d", i)]; ok {
			out = append(out, p.request)
		}
	}
	return out
}

// sealForTest is the portal's sealForCompanion restated in Go, for the double.
func sealForTest(plaintext, recipient, aad string) (protocol.SealedKey, error) {
	pairingID := strings.SplitN(aad, "\n", 2)[0]
	raw, err := base64.RawURLEncoding.DecodeString(recipient)
	if err != nil {
		return protocol.SealedKey{}, err
	}
	peer, err := ecdh.X25519().NewPublicKey(raw)
	if err != nil {
		return protocol.SealedKey{}, err
	}
	ephemeral, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return protocol.SealedKey{}, err
	}
	shared, err := ephemeral.ECDH(peer)
	if err != nil {
		return protocol.SealedKey{}, err
	}
	derived, err := hkdf.Key(sha256.New, shared, []byte(pairingID), "uffici-pairing-v1", 32)
	if err != nil {
		return protocol.SealedKey{}, err
	}
	block, err := aes.NewCipher(derived)
	if err != nil {
		return protocol.SealedKey{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return protocol.SealedKey{}, err
	}
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return protocol.SealedKey{}, err
	}
	return protocol.SealedKey{
		Alg:                "x25519-hkdf-sha256-aes-256-gcm",
		EphemeralPublicKey: base64.RawURLEncoding.EncodeToString(ephemeral.PublicKey().Bytes()),
		Nonce:              base64.RawURLEncoding.EncodeToString(nonce),
		Ciphertext:         base64.RawURLEncoding.EncodeToString(gcm.Seal(nil, nonce, []byte(plaintext), []byte(aad))),
	}, nil
}
