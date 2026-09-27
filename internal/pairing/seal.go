// Package pairing opens the agent key a pairing delivers (#1966).
//
// The portal seals the key to a throwaway X25519 public key this companion sent when
// it asked to pair; only the matching private key — held in memory for the length of
// one `uffici pair`, never written — can open it. The scheme, byte for byte:
//
//	shared = X25519(recipient private, portal's ephemeral public)
//	key    = HKDF-SHA256(shared, salt = pairing id, info = "uffici-pairing-v1", 32 bytes)
//	plain  = AES-256-GCM-Open(key, nonce, ciphertext‖tag, aad = the Binding)
//
// The Binding — pairing id, agent id, org slug, agent slug — names WHO the key is
// for, so a delivery relabelled in transit does not open, and what the person is
// shown before the key is written is what the seal authenticated.
//
// Pinned against the portal's sealer by testdata/seal-vector.json, which the portal's
// own test reproduces from the same inputs.
package pairing

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
)

// Alg is the only sealing scheme this companion opens.
const Alg = "x25519-hkdf-sha256-aes-256-gcm"

// info binds the derived key to this purpose and version.
const info = "uffici-pairing-v1"

// Sealed is a sealed key as the pairing poll delivers it.
type Sealed struct {
	Alg                string `json:"alg"`
	EphemeralPublicKey string `json:"ephemeralPublicKey"`
	Nonce              string `json:"nonce"`
	Ciphertext         string `json:"ciphertext"`
}

// Binding is who a sealed key is for, authenticated as its associated data.
type Binding struct {
	PairingID string
	AgentID   string
	OrgSlug   string
	AgentSlug string
}

// aad is the associated data a binding authenticates — the portal's
// `pairingAad`, byte for byte.
func (b Binding) aad() []byte {
	return []byte(b.PairingID + "\n" + b.AgentID + "\n" + b.OrgSlug + "\n" + b.AgentSlug)
}

// ErrUnopenable is every way a sealed key fails to open. One error on purpose: the
// caller has nothing to do differently between a wrong key and a tampered body.
var ErrUnopenable = errors.New("the sealed key could not be opened")

// NewKey makes the throwaway key pair one pairing uses.
func NewKey() (*ecdh.PrivateKey, error) {
	return ecdh.X25519().GenerateKey(rand.Reader)
}

// PublicKey is the key's public half as the pairing request carries it: raw 32
// bytes, base64url without padding.
func PublicKey(key *ecdh.PrivateKey) string {
	return base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes())
}

// Open opens a sealed key with the private key whose public half it was sealed
// to, for exactly the binding it was sealed for.
func Open(sealed Sealed, key *ecdh.PrivateKey, binding Binding) (string, error) {
	if sealed.Alg != Alg {
		return "", fmt.Errorf("%w: unknown scheme %q", ErrUnopenable, sealed.Alg)
	}
	peerRaw, err := base64.RawURLEncoding.DecodeString(sealed.EphemeralPublicKey)
	if err != nil {
		return "", ErrUnopenable
	}
	peer, err := ecdh.X25519().NewPublicKey(peerRaw)
	if err != nil {
		return "", ErrUnopenable
	}
	nonce, err := base64.RawURLEncoding.DecodeString(sealed.Nonce)
	if err != nil || len(nonce) != 12 {
		return "", ErrUnopenable
	}
	body, err := base64.RawURLEncoding.DecodeString(sealed.Ciphertext)
	if err != nil {
		return "", ErrUnopenable
	}
	shared, err := key.ECDH(peer)
	if err != nil {
		return "", ErrUnopenable
	}
	derived, err := hkdf.Key(sha256.New, shared, []byte(binding.PairingID), info, 32)
	if err != nil {
		return "", ErrUnopenable
	}
	block, err := aes.NewCipher(derived)
	if err != nil {
		return "", ErrUnopenable
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", ErrUnopenable
	}
	plain, err := gcm.Open(nil, nonce, body, binding.aad())
	if err != nil {
		return "", ErrUnopenable
	}
	return string(plain), nil
}
