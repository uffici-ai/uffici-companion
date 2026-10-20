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
	"regexp"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/protocol"
	"github.com/uffici-ai/uffici-companion/internal/waits"
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
	// FilesHTTPClient carries a transfer's bytes (#2673). 🔴 Its own pool, and no
	// whole-request timeout: a file takes as long as it takes, and the call's or
	// the request's deadline bounds it instead. Nil builds one.
	FilesHTTPClient *http.Client
	// FilesHeaderTimeout bounds the wait for a transfer answer's headers, and
	// FilesIdleTimeout the wait for each read of a download's body: a server
	// that stops sending is a failed attempt, retried, never a hang until the
	// request's deadline. Zero takes DefaultFilesHeaderTimeout and
	// DefaultFilesIdleTimeout.
	FilesHeaderTimeout time.Duration
	FilesIdleTimeout   time.Duration
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
	filesHTTP *http.Client
	filesIdle time.Duration
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
		sleep = waits.Sleep
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
	headerTimeout := options.FilesHeaderTimeout
	if headerTimeout <= 0 {
		headerTimeout = DefaultFilesHeaderTimeout
	}
	filesIdle := options.FilesIdleTimeout
	if filesIdle <= 0 {
		filesIdle = DefaultFilesIdleTimeout
	}
	return &Host{
		filesHTTP: withHeaderTimeout(options.FilesHTTPClient, headerTimeout),
		filesIdle: filesIdle,
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
// forgetConversation drops what the host caches for one conversation (#2356):
// its conversation ticket. A later read mints a fresh one.
func (h *Host) forgetConversation(conversationID string) {
	h.api.forget(conversationID)
}

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

// ReadSince reads a request's delta: (since, through], the agent's own omitted,
// and the context the brief carries (#2331). The plugin's declared budget rides
// along for the platform to size the context; 0 sends none.
func (h *Host) ReadSince(ctx context.Context, conversationID, since, through string, contextBudgetTokens int) (contracts.Read, error) {
	if since == "" {
		// The contract's empty cursor is "from the beginning"; the protocol's is 0.
		since = "0"
	}
	query := url.Values{"since": {since}, "through": {through}}
	if contextBudgetTokens > 0 {
		query.Set("budgetTokens", strconv.Itoa(contextBudgetTokens))
	}
	// The context is decoded on its own: a malformed one must not cost the
	// brief, so it shadows the struct's typed field as raw bytes here.
	var answer struct {
		protocol.ReadResponse
		Context json.RawMessage `json:"context"`
	}
	err := h.api.ticketed(ctx, conversationID, http.MethodGet,
		"/v1/conversations/"+url.PathEscape(conversationID)+"/entries",
		query, nil, &answer, controlAttempts, h.sleep)
	if err != nil {
		h.logFailure("read", err)
		return contracts.Read{}, err
	}
	if answer.Identity == nil {
		// 🔴 The gateway refuses a read without it; one that arrives anyway is
		// not the protocol, and a brief short of its identity floor is not sent.
		return contracts.Read{}, localError(ClassPlatformError, http.StatusOK)
	}
	return contracts.Read{
		Entries:        toEntries(answer.Entries),
		Bounded:        answer.Bounded,
		Context:        h.decodeContext(answer.Context),
		ContextChanged: toContextChanged(answer.ContextChanged),
	}, nil
}

// Wire bounds the context must fit (wire/v1/schema/context.schema.json) — the
// protocol admits more, and a brief over them would be refused by the plugin.
const (
	maxContextBlocks  = 64
	maxContextMissing = 16
)

// KnowledgeUnavailableInstruction is the platform's sentence for a context it could
// not deliver — the agents protocol's `knowledge_unavailable` instruction, verbatim.
const KnowledgeUnavailableInstruction = "The organization's and this place's context were unavailable because of a system problem; tell the person you are working without them."

// unreadableContext stands in for a context that arrived but could not be read: no
// blocks, and the missing part NAMED — so the agent tells the person, rather than
// working on as though nothing were missing (decision 32).
func unreadableContext() *contracts.Context {
	return &contracts.Context{
		Blocks: []contracts.ContextBlock{},
		Missing: []contracts.ContextMissing{{
			Part: "context", Class: "knowledge_unavailable", Instruction: KnowledgeUnavailableInstruction,
		}},
	}
}

// decodeContext reads the platform's context: nil when ABSENT (an older gateway — the
// plugin keeps its static hint), and an UNREADABLE one — not the protocol's shape, or
// over the wire's bounds — becomes a context that names the missing part. 🔴 Never a
// failed read, and never silent. Logged by class, never by text.
func (h *Host) decodeContext(raw json.RawMessage) *contracts.Context {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var decoded protocol.Context
	if protocol.Validate("context", raw) != nil || json.Unmarshal(raw, &decoded) != nil ||
		len(decoded.Blocks) > maxContextBlocks || len(decoded.Missing) > maxContextMissing {
		h.log.Warn("the read's context is not the protocol's shape; the brief names it missing",
			slog.String("class", "knowledge_unavailable"))
		return unreadableContext()
	}
	return toContext(&decoded)
}

// maxContextChanged is the wire's bound on a change list (the protocol's
// CONTEXT_CHANGES_MAX); a longer list is cut, never refused — it only informs.
const maxContextChanged = 16

// contextScope is the protocol's scope grammar.
var contextScope = regexp.MustCompile(`^[a-z][a-z_]{0,31}$`)

// toContextChanged maps the levels a person changed since the agent's last
// answer (2026-10-12) one to one, dropping an entry the wire could not carry.
func toContextChanged(in []protocol.ContextChange) []contracts.ContextChange {
	var out []contracts.ContextChange
	for _, c := range in {
		// Characters, never bytes: the protocol counts a name's characters, and an
		// accented folder name is longer in bytes than the 128 it is allowed.
		if c.Name == "" || utf8.RuneCountInString(c.Name) > 128 || !contextScope.MatchString(c.Scope) {
			continue
		}
		out = append(out, contracts.ContextChange{Name: c.Name, Scope: c.Scope})
		if len(out) == maxContextChanged {
			break
		}
	}
	return out
}

// toContext maps the platform's context one to one — the companion composes
// nothing. Absent stays nil: an older gateway sends none, and the plugin then
// keeps its static hint rather than an empty stand-in.
func toContext(in *protocol.Context) *contracts.Context {
	if in == nil {
		return nil
	}
	out := &contracts.Context{Tokens: in.Tokens, Bounded: in.Bounded}
	for _, b := range in.Blocks {
		out.Blocks = append(out.Blocks, contracts.ContextBlock{
			Name: b.Name, Scope: b.Scope, Depth: b.Depth, Tokens: b.Tokens, Text: b.Text,
		})
	}
	for _, m := range in.Missing {
		out.Missing = append(out.Missing, contracts.ContextMissing{
			Part: m.Part, Class: m.Class, Instruction: m.Instruction,
		})
	}
	return out
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
func (h *Host) Post(ctx context.Context, requestID, conversationID, text string, extras *contracts.PostExtras) (string, error) {
	if requestID == "" || conversationID == "" {
		return "", fmt.Errorf("gateway: Post needs a request and a conversation")
	}
	postID, err := newUUID()
	if err != nil {
		return "", err
	}
	body := protocol.PostRequest{PostID: postID, RequestID: requestID, Text: text, Audience: "external"}
	if extras != nil {
		body.Question = toPostQuestion(extras.Question)
		body.Citations = extras.Citations
		for _, a := range extras.Activity {
			body.Activity = append(body.Activity, protocol.PostActivity{Tool: a.Tool, Summary: a.Summary})
		}
		// Uploads completed on this request (#2673): the platform refuses one it
		// did not record for it.
		for _, a := range extras.Attachments {
			body.Attachments = append(body.Attachments, protocol.Attachment{ID: a.ID, Name: a.Name, Ref: a.Ref})
		}
	}
	var answer protocol.PostResponse
	err = h.api.ticketed(ctx, conversationID, http.MethodPost,
		"/v1/conversations/"+url.PathEscape(conversationID)+"/posts", nil, body,
		&answer, controlAttempts, h.sleep)
	if err != nil {
		h.logFailure("post", err)
		return "", err
	}
	return answer.MessageID, nil
}

// ListTools reads the agent's tool catalog for one conversation (#2349), under
// that conversation's ticket. A read: retried like every other.
func (h *Host) ListTools(ctx context.Context, conversationID string) ([]contracts.Tool, error) {
	if conversationID == "" {
		return nil, fmt.Errorf("gateway: ListTools needs a conversation")
	}
	var answer protocol.ToolCatalogResponse
	err := h.api.ticketed(ctx, conversationID, http.MethodGet,
		"/v1/conversations/"+url.PathEscape(conversationID)+"/tools", nil, nil, &answer, controlAttempts, h.sleep)
	if err != nil {
		h.logFailure("tool_catalog", err)
		return nil, err
	}
	if answer.Tools == nil {
		// The protocol requires the list; an answer without it is not the protocol.
		return nil, localError(ClassPlatformError, http.StatusOK)
	}
	tools := make([]contracts.Tool, 0, len(answer.Tools))
	for _, tool := range answer.Tools {
		tools = append(tools, contracts.Tool{
			Name: tool.Name, Description: tool.Description, Parameters: tool.Parameters, Permission: tool.Permission,
		})
	}
	return tools, nil
}

// CallTool makes one tool call on a request this companion was rung for, under
// the ticket of that request's conversation (#2349).
//
// 🔴 A request this process never saw rung is refused HERE, before any ticket
// mint or call: its conversation is unknown, and a plugin naming one is not
// answering anything this companion delivered.
//
// 🔴 Sent at most once. A tool is not idempotent — "add a card" sent twice adds
// two — so only what provably happened BEFORE the platform could run it is
// retried: the ticket mint, and a ticket the gateway refused (it refuses before
// it runs anything). A failure after the body left is answered as it came.
func (h *Host) CallTool(ctx context.Context, requestID, name string, arguments json.RawMessage) (contracts.ToolResult, error) {
	conversationID, known := h.conversationOf(requestID)
	if !known {
		return contracts.ToolResult{}, contracts.ErrUnknownRequest
	}
	if len(arguments) == 0 {
		arguments = json.RawMessage("{}")
	}
	return h.toolCallOnce(ctx, conversationID, "/v1/tool-calls",
		protocol.ToolCallRequest{RequestID: requestID, Name: name, Arguments: arguments})
}

// RerunTool re-runs a call the platform recorded, named by its `tool_call` URN,
// on a request this companion was rung for (#2454): the same ticket, the same
// at-most-once send and the same answer as CallTool — a re-run IS a tool call,
// so a failure after the body left is never retried. A call whose arguments
// were not kept is a RESULT (`rerun_unavailable`), never an error.
func (h *Host) RerunTool(ctx context.Context, requestID, call string) (contracts.ToolResult, error) {
	conversationID, known := h.conversationOf(requestID)
	if !known {
		return contracts.ToolResult{}, contracts.ErrUnknownRequest
	}
	return h.toolCallOnce(ctx, conversationID, "/v1/tool-calls/rerun",
		protocol.ToolRerunRequest{RequestID: requestID, Call: call})
}

// toolCallOnce sends one tool call's body under the conversation's ticket, at
// most once, and maps the platform's answer to a ToolResult.
func (h *Host) toolCallOnce(ctx context.Context, conversationID, path string, body any) (contracts.ToolResult, error) {
	var answer protocol.ToolCallResponse
	err := h.api.ticketedOnce(ctx, conversationID, http.MethodPost, path, body, &answer, controlAttempts, h.sleep)
	if err != nil {
		h.logFailure("tool_call", err)
		return contracts.ToolResult{}, err
	}
	if len(answer.Result) == 0 {
		return contracts.ToolResult{}, localError(ClassPlatformError, http.StatusOK)
	}
	result := contracts.ToolResult{Result: answer.Result}
	if answer.CallURN != nil {
		result.CallURN = *answer.CallURN
	}
	return result, nil
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
func (h *Host) logFailure(family string, err error) { logPlatformFailure(h.log, family, err) }

// logPlatformFailure is logFailure for a logger that may be nil (the bytes
// client's): the family and the class, never a body.
func logPlatformFailure(log *slog.Logger, family string, err error) {
	if log == nil {
		return
	}
	var classified *Error
	if errors.As(err, &classified) {
		log.Warn("a platform call failed",
			slog.String("family", family), slog.String("class", classified.Class),
			slog.Int("status", classified.Status), slog.Bool("retryable", classified.Retryable))
		return
	}
	log.Warn("a platform call failed", slog.String("family", family), slog.String("class", "context_ended"))
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
	// A post the platform refused for citing a tool call that is not the agent's
	// own in that conversation (#2348): the harness answered wrongly, which is
	// a harness error, never `refused` (the agent declining). Named here so the
	// mapping is a decision, not the default's accident.
	"invalid_citation": "harness_error",
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
