package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/protocol"
)

// PairerOptions configures a Pairer. There is no key: pairing is how one arrives.
type PairerOptions struct {
	// BaseURL is the agents host's https origin.
	BaseURL string
	// InstanceID is this companion's persistent device id.
	InstanceID string
	// Version is the companion's own version, for the connection facts.
	Version string
	// HTTPClient is the TLS-configured client; nil takes a default.
	HTTPClient *http.Client
}

// Pairer is the two pairing calls (#1966), made with NO credential: open a
// pairing, then poll it until an admin approves or it expires.
//
// 🔴 The poll token it is handed is secret-class — the one credential that
// collects the sealed key — and goes into an Authorization header only, never
// a log line, an error or a URL.
type Pairer struct {
	api *client
}

// NewPairer builds a Pairer against an https agents host.
func NewPairer(options PairerOptions) (*Pairer, error) {
	base, err := url.Parse(strings.TrimRight(options.BaseURL, "/"))
	if err != nil || base.Scheme != "https" || base.Host == "" {
		return nil, fmt.Errorf("gateway: the agents host must be an https origin")
	}
	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	version := options.Version
	if version == "" {
		version = "0.0.0-dev"
	}
	return &Pairer{api: &client{
		base: base, instanceID: options.InstanceID,
		userAgent: ClientName + "/" + version, http: httpClient, now: time.Now,
		tickets: map[string]conversationTicket{},
	}}, nil
}

// Open asks the agents host for a pairing, declaring this companion and its
// harness and the public key the agent key will be sealed to. code is a pairing
// code the portal issued (#2209), or "" for the typed-code path; it is
// secret-class and goes into the request body only.
func (p *Pairer) Open(ctx context.Context, harness protocol.HarnessFacts, version, publicKey, code string) (protocol.PairingResponse, error) {
	var answer protocol.PairingResponse
	err := p.api.call(ctx, http.MethodPost, "/v1/pairings", nil, "", protocol.PairingRequest{
		InstanceID: p.api.instanceID,
		Client:     protocol.ClientFacts{Name: ClientName, Version: version},
		Harness:    harness,
		PublicKey:  publicKey,
		Code:       code,
	}, &answer)
	if err != nil {
		return protocol.PairingResponse{}, err
	}
	if answer.PairingID == "" || answer.Code == "" || answer.PollToken == "" {
		return protocol.PairingResponse{}, localError(ClassPlatformError, http.StatusCreated)
	}
	return answer, nil
}

// Poll asks once whether the pairing was approved. A `not_found` means it
// expired or was already collected; the caller starts over.
func (p *Pairer) Poll(ctx context.Context, pairingID, pollToken string) (protocol.PairingPollResponse, error) {
	var answer protocol.PairingPollResponse
	err := p.api.call(ctx, http.MethodGet, "/v1/pairings/"+url.PathEscape(pairingID), nil, pollToken, nil, &answer)
	if err != nil {
		return protocol.PairingPollResponse{}, err
	}
	switch answer.Status {
	case protocol.PairingPending:
		return answer, nil
	case protocol.PairingPaired:
		if answer.Sealed == nil || answer.KeyLast4 == "" {
			return protocol.PairingPollResponse{}, localError(ClassPlatformError, http.StatusOK)
		}
		return answer, nil
	default:
		return protocol.PairingPollResponse{}, localError(ClassPlatformError, http.StatusOK)
	}
}
