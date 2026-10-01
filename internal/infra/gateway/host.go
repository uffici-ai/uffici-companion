// Package gateway is the companion's real AgentsHost: the platform's
// agents-gateway, over the doorbell (WebSocket) and the JSON control plane
// (HTTPS) of #2066–#2071 (#2077).
//
// 🔴 It is a TRANSLATOR and nothing more. The request lifecycle — supersede,
// expiry, redelivery, the caps — is `internal/requests`'s, exactly as it was
// against the fixture; this package turns each contract call into the
// protocol's frames and bodies, and each platform answer back. Stateless about
// position (decision 40): cursors come from the ring and go straight back to
// the platform, and nothing here decides what was answered.
//
// 🔴 Secrets: the agent key is sent on the three ticket mints and nowhere else,
// and neither the key, a ticket nor a response body ever reaches a log line or
// an error string. Every error this package returns is an `*Error` built from a
// class and a status (ledger class 18: an upstream body in a log).
package gateway

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/protocol"
)

// ClientName is how the companion names itself to the platform.
const ClientName = "uffici-companion"

// controlAttempts bounds how often one control-plane call is sent.
const controlAttempts = 4

// Options configure a Host.
type Options struct {
	// BaseURL is the agents host: `https://agents.uffici.dev`,
	// `https://agents.localhost`.
	BaseURL string
	// Key is the agent key. 🔴 SECRET-CLASS.
	Key string
	// InstanceID is this companion's persistent device id.
	InstanceID string
	// Version is the companion's version, reported on every connection.
	Version string
	// Harness reports the attached harness's kind and version, read at each
	// connection. Nil, or empty answers, report `unknown`.
	Harness func() (kind, version string)
	// Plugin reports the attached plugin's version (#2236), read at each
	// connection. Nil, or an empty answer, reports none.
	Plugin func() string
	// HostFacts describes this machine (#2236); nil takes the local one.
	HostFacts *protocol.HostFacts
	// HTTPClient carries the TLS configuration (a dev CA, a proxy). Nil takes a
	// default client with a 30-second timeout.
	HTTPClient *http.Client
	// LogsHTTPClient is the log shipper's OWN client. 🔴 Never HTTPClient: a
	// push must not share a connection pool with a call a person is waiting on.
	// Nil builds a second default client.
	LogsHTTPClient *http.Client
	// Logger receives classified lines only. Nil discards.
	Logger *slog.Logger
	// Now and Sleep are the clock, injected so tests neither wait nor race it.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
}

// Host is the gateway-backed AgentsHost.
type Host struct {
	api       *client
	logsAPI   *client
	version   string
	harnessOf func() (string, string)
	pluginOf  func() string
	hostFacts *protocol.HostFacts
	log       *slog.Logger
	sleep     sleeper
	backoff   *backoff

	mu        sync.Mutex
	open      bool
	conn      *websocket.Conn
	writeMu   sync.Mutex
	requests  map[string]string
	requestAt []string
	// acked remembers the ring ids the read loop already acked, so the
	// companion's on-receipt Ack of the same ring sends nothing twice.
	acked   map[string]struct{}
	ackedAt []string
	// doorbellID is the connection id of the doorbell that said hello, empty
	// while none is connected (#2257): what `uffici doctor` reads from a running
	// companion instead of opening a second doorbell that would replace it.
	doorbellID string
}

// requestMemory bounds the request → conversation map a close reads.
const requestMemory = 1024

// New builds a Host. It dials nothing until OpenDoorbell.
func New(options Options) (*Host, error) {
	base, err := url.Parse(options.BaseURL)
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") {
		return nil, fmt.Errorf("the agents host must be an https URL, like https://agents.uffici.dev")
	}
	if options.Key == "" {
		return nil, fmt.Errorf("the gateway agents host needs the agent key")
	}
	if options.InstanceID == "" {
		return nil, fmt.Errorf("the gateway agents host needs the companion's instance id")
	}
	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	sleep := options.Sleep
	if sleep == nil {
		sleep = sleepContext
	}
	version := options.Version
	if version == "" {
		version = "0.0.0-dev"
	}
	logsHTTP := options.LogsHTTPClient
	if logsHTTP == nil {
		// A pool of its own even here: a nil Transport is the shared default one.
		logsHTTP = &http.Client{
			Timeout:   30 * time.Second,
			Transport: http.DefaultTransport.(*http.Transport).Clone(),
		}
	}
	return &Host{
		api: &client{
			base: base, key: options.Key, instanceID: options.InstanceID,
			userAgent: ClientName + "/" + version, http: httpClient, now: now,
			tickets: map[string]conversationTicket{},
		},
		logsAPI: &client{
			base: base, key: options.Key, instanceID: options.InstanceID,
			userAgent: ClientName + "/" + version, http: logsHTTP, now: now,
			tickets: map[string]conversationTicket{},
		},
		version:   version,
		harnessOf: options.Harness,
		pluginOf:  options.Plugin,
		hostFacts: hostFactsOr(options.HostFacts),
		log:       logger,
		sleep:     sleep,
		backoff:   newBackoff(),
		requests:  map[string]string{},
		acked:     map[string]struct{}{},
	}, nil
}

// hostFactsOr is the given facts, or this machine's.
func hostFactsOr(facts *protocol.HostFacts) *protocol.HostFacts {
	if facts != nil {
		return facts
	}
	return localHostFacts()
}

// plugin reads the attached plugin's facts, nil when no plugin said hello.
func (h *Host) plugin() *protocol.PluginFacts {
	if h.pluginOf == nil {
		return nil
	}
	if version := h.pluginOf(); version != "" {
		return &protocol.PluginFacts{Version: clipFact(version)}
	}
	return nil
}

// harness reads the attached harness's facts, `unknown` when there are none.
func (h *Host) harness() protocol.HarnessFacts {
	facts := protocol.HarnessFacts{Kind: "unknown", Version: "unknown"}
	if h.harnessOf != nil {
		if kind, version := h.harnessOf(); kind != "" && version != "" {
			facts = protocol.HarnessFacts{Kind: kind, Version: version}
		}
	}
	return facts
}

// OpenDoorbell starts delivery: it connects, and reconnects for as long as the
// claim is held.
func (h *Host) OpenDoorbell(ctx context.Context, handlers contracts.Handlers) (contracts.DoorbellClaim, error) {
	if handlers.OnRing == nil || handlers.OnCancel == nil || handlers.OnDisconnect == nil {
		return nil, fmt.Errorf("gateway: OpenDoorbell needs all three handlers; a nil one would drop deliveries silently")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.open {
		return nil, fmt.Errorf("gateway: the doorbell is already open — one connection per companion")
	}
	h.open = true
	// The loop outlives the call that opened it: its context is the claim's,
	// not the caller's request-scoped one.
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	d := &doorbell{
		host: h, handlers: handlers, cancel: cancel,
		done: make(chan struct{}), frames: make(chan any, 256),
	}
	go d.run(loopCtx)
	return &claim{d: d}, nil
}

func (h *Host) release() {
	h.mu.Lock()
	h.open = false
	h.mu.Unlock()
}

// DoorbellState reports whether the doorbell is connected, and its connection
// id — the platform's, never a credential.
func (h *Host) DoorbellState() (connected bool, connectionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.doorbellID != "", h.doorbellID
}

func (h *Host) setDoorbell(connectionID string) {
	h.mu.Lock()
	h.doorbellID = connectionID
	h.mu.Unlock()
}

func (h *Host) setConn(conn *websocket.Conn) {
	h.mu.Lock()
	h.conn = conn
	h.mu.Unlock()
}

// remember records which conversation a request belongs to, for its close —
// the one route whose path names a request rather than a conversation.
func (h *Host) remember(requestID, conversationID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, known := h.requests[requestID]; !known {
		h.requestAt = append(h.requestAt, requestID)
		if len(h.requestAt) > requestMemory {
			delete(h.requests, h.requestAt[0])
			h.requestAt = h.requestAt[1:]
		}
	}
	h.requests[requestID] = conversationID
}

// KnowsRequest reports whether a ring for this request has reached the host —
// the precondition of its close. For tests, which must not close a request
// before the ring that opened it has been read off the socket.
func (h *Host) KnowsRequest(requestID string) bool {
	_, ok := h.conversationOf(requestID)
	return ok
}

func (h *Host) conversationOf(requestID string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	conversationID, ok := h.requests[requestID]
	return conversationID, ok
}

// Ack tells the platform this companion has the ring — on RECEIPT. The read
// loop has normally acked it already, on the socket it came on; then this is a
// no-op.
func (h *Host) Ack(ctx context.Context, ringID string) error {
	if ringID == "" {
		return fmt.Errorf("gateway: Ack with no ring id")
	}
	h.mu.Lock()
	_, done := h.acked[ringID]
	conn := h.conn
	h.mu.Unlock()
	if done {
		return nil
	}
	if conn == nil {
		// The socket the ring came on is gone. The gateway redelivers an unacked
		// ring to the next connection under a NEW ring id, so this one is moot.
		return localError(ClassUnreachable, 0)
	}
	raw, err := json.Marshal(protocol.NewAck(ringID))
	if err != nil {
		return fmt.Errorf("gateway: encoding an ack: %w", err)
	}
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		return localError(ClassUnreachable, 0)
	}
	h.markAcked(ringID)
	return nil
}

// ackOn acks a ring on the socket it arrived on. A failed write is logged by
// class and left to the gateway's redelivery, which the request machine reads
// as the same request (#2077 review).
func (h *Host) ackOn(ctx context.Context, conn *websocket.Conn, ringID string) {
	raw, err := json.Marshal(protocol.NewAck(ringID))
	if err != nil {
		return
	}
	h.writeMu.Lock()
	err = conn.Write(ctx, websocket.MessageText, raw)
	h.writeMu.Unlock()
	if err != nil {
		h.log.Warn("a ring could not be acked; the gateway will redeliver it", slog.String("class", ClassUnreachable))
		return
	}
	h.markAcked(ringID)
}

func (h *Host) markAcked(ringID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, known := h.acked[ringID]; known {
		return
	}
	h.acked[ringID] = struct{}{}
	h.ackedAt = append(h.ackedAt, ringID)
	if len(h.ackedAt) > requestMemory {
		delete(h.acked, h.ackedAt[0])
		h.ackedAt = h.ackedAt[1:]
	}
}

// ReadSince reads a request's delta: (since, through], the agent's own omitted.
func (h *Host) ReadSince(ctx context.Context, conversationID, since, through string) ([]contracts.Entry, bool, error) {
	if since == "" {
		// The contract's empty cursor is "from the beginning"; the protocol's is 0.
		since = "0"
	}
	var answer protocol.ReadResponse
	err := h.api.ticketed(ctx, conversationID, http.MethodGet,
		"/v1/conversations/"+url.PathEscape(conversationID)+"/entries",
		url.Values{"since": {since}, "through": {through}}, nil, &answer, controlAttempts, h.sleep)
	if err != nil {
		h.logFailure("read", err)
		return nil, false, err
	}
	if answer.Identity == nil {
		// 🔴 The gateway refuses a read without it; one that arrives anyway is
		// not the protocol, and a brief short of its identity floor is not sent.
		return nil, false, localError(ClassPlatformError, http.StatusOK)
	}
	return toEntries(answer.Entries), answer.Bounded, nil
}

// ReadOpener reads the recent tail within the harness's declared budget.
func (h *Host) ReadOpener(ctx context.Context, conversationID, through string, budgetTokens int) ([]contracts.Entry, error) {
	var answer protocol.ReadResponse
	err := h.api.ticketed(ctx, conversationID, http.MethodGet,
		"/v1/conversations/"+url.PathEscape(conversationID)+"/opener",
		url.Values{"through": {through}, "budgetTokens": {strconv.Itoa(budgetTokens)}}, nil, &answer,
		controlAttempts, h.sleep)
	if err != nil {
		h.logFailure("read", err)
		return nil, err
	}
	if answer.Identity == nil {
		return nil, localError(ClassPlatformError, http.StatusOK)
	}
	return toEntries(answer.Entries), nil
}

// Post sends one message. The `postId` is minted here, once per call, and every
// retry of the call carries the same one — so a post that reached the platform
// but whose answer was lost is answered with the same message, never a second.
func (h *Host) Post(ctx context.Context, requestID, conversationID, text string, question *contracts.Question) (string, error) {
	if requestID == "" || conversationID == "" {
		return "", fmt.Errorf("gateway: Post needs a request and a conversation")
	}
	postID, err := newUUID()
	if err != nil {
		return "", err
	}
	var answer protocol.PostResponse
	err = h.api.ticketed(ctx, conversationID, http.MethodPost,
		"/v1/conversations/"+url.PathEscape(conversationID)+"/posts", nil,
		protocol.PostRequest{
			PostID: postID, RequestID: requestID, Text: text, Audience: "external",
			Question: toPostQuestion(question),
		},
		&answer, controlAttempts, h.sleep)
	if err != nil {
		h.logFailure("post", err)
		return "", err
	}
	return answer.MessageID, nil
}

// CloseRequest closes a request with its outcome.
func (h *Host) CloseRequest(ctx context.Context, requestID string, outcome contracts.Outcome, errorClass string) error {
	if requestID == "" {
		return fmt.Errorf("gateway: CloseRequest with no request id")
	}
	conversationID, known := h.conversationOf(requestID)
	if !known {
		// A close is authorized by a CONVERSATION ticket; a request this process
		// never saw rung has none to use.
		return fmt.Errorf("gateway: no ring was received for request %s", requestID)
	}
	body := protocol.CloseRequest{Outcome: string(outcome)}
	if outcome == contracts.OutcomeError {
		body.ErrorClass = PlatformErrorClass(errorClass)
	}
	var answer protocol.CloseResponse
	err := h.api.ticketed(ctx, conversationID, http.MethodPost,
		"/v1/requests/"+url.PathEscape(requestID)+"/close", nil, body, &answer, controlAttempts, h.sleep)
	if err != nil && classOf(err) != ClassRequestClosed {
		h.logFailure("close", err)
		return err
	}
	return nil
}

// Close closes the current doorbell socket and releases the host. Idempotent.
// It does not end a claim's reconnect loop — closing the claim does, and the
// daemon closes the claim first.
func (h *Host) Close() error {
	h.mu.Lock()
	conn := h.conn
	h.mu.Unlock()
	if conn != nil {
		_ = conn.Close(websocket.StatusNormalClosure, "shutting down")
	}
	return nil
}

// logFailure logs one classified control-plane failure: the family and the
// class, never a body.
func (h *Host) logFailure(family string, err error) {
	var classified *Error
	if errors.As(err, &classified) {
		h.log.Warn("a platform call failed",
			slog.String("family", family), slog.String("class", classified.Class),
			slog.Int("status", classified.Status), slog.Bool("retryable", classified.Retryable))
		return
	}
	h.log.Warn("a platform call failed", slog.String("family", family), slog.String("class", "context_ended"))
}

// PlatformCloseClasses is the conversations API's closed close vocabulary: it
// refuses any other `errorClass` as `invalid_error_class`, which would leave the
// request open. The schema types the field as a string, so the manifest cannot
// carry it; the double's copy is pinned to this one (vocabulary_test.go).
var PlatformCloseClasses = []string{"no_runtime", "harness_error", "refused", "over_budget"}

// closeClassOf names every class that does NOT fall to `harness_error`. Its keys
// that are the platform's own are pinned to the manifest (vocabulary_test.go).
var closeClassOf = map[string]string{
	"harness_error": "harness_error",
	"refused":       "refused",
	"over_budget":   "over_budget",
	"no_runtime":    "no_runtime",
	// The plugin never took the brief — a harness failing, not a decision to
	// decline, which is what `refused` means.
	"redelivery_exhausted": "harness_error",
	"post_too_large":       "over_budget",
	"post_cap_exceeded":    "over_budget",
}

// PlatformErrorClass maps a companion or plugin error class onto the platform's
// closed close vocabulary (PlatformCloseClasses).
func PlatformErrorClass(class string) string {
	if mapped, ok := closeClassOf[class]; ok {
		return mapped
	}
	// model_error, timeout, internal, cancelled, and every platform or transport
	// class: the turn failed on the harness's side of the office.
	return "harness_error"
}

// authorTypes maps the protocol's author types onto the local wire's.
var authorTypes = map[string]string{
	"user": "member", "agent": "agent", "system": "system", "external": "correspondent",
}

func toEntries(entries []protocol.Entry) []contracts.Entry {
	out := make([]contracts.Entry, 0, len(entries))
	for _, entry := range entries {
		authorType, ok := authorTypes[entry.Author.Type]
		if !ok {
			authorType = "system"
		}
		mapped := contracts.Entry{
			ID: entry.ID, Cursor: entry.Cursor, At: entry.At, Kind: entry.Kind, Text: entry.Text,
			Author: contracts.Author{
				Type: authorType, ID: entry.Author.ID, Label: entry.Author.Label,
				Handle: entry.Author.Handle, Self: entry.Author.Self,
			},
		}
		for _, attachment := range entry.Attachments {
			mapped.Attachments = append(mapped.Attachments, contracts.Attachment{
				ID: attachment.ID, Name: attachment.Name, Ref: attachment.Ref,
			})
		}
		out = append(out, mapped)
	}
	return out
}

// newUUID mints a version-4 uuid.
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("gateway: minting an id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

var _ contracts.AgentsHost = (*Host)(nil)

// toPostQuestion carries a harness's question onto the post (#2232).
func toPostQuestion(question *contracts.Question) *protocol.PostQuestion {
	if question == nil {
		return nil
	}
	choices := question.Choices
	if choices == nil {
		// The protocol requires the list; a question with no choices is open-ended.
		choices = []string{}
	}
	return &protocol.PostQuestion{ID: question.ID, Choices: choices, MultiSelect: question.MultiSelect}
}

// nonEmpty is value, or fallback when value is empty.
func nonEmpty(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
