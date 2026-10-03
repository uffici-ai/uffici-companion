// Package gatewaytest is an in-process double of the platform's agents-gateway,
// for the companion's own tests (#2077).
//
// 🔴 The companion's double, never a platform import: the fence forbids a child
// reaching the platform's tree, and the platform's gateway is TypeScript. What
// keeps the double honest instead is the CONTRACT: every body the companion
// sends is validated against the embedded protocol schemas — the byte-identical
// copy of what the platform emits — and refused exactly as the gateway refuses
// it (`invalid_request`, `protocol_error`). A companion that passes against this
// double cannot be sending a field the platform rejects.
//
// It plays the platform's SEMANTICS only as far as the contract promises them:
// single-use connection tickets, conversation tickets scoped to one
// conversation, a delta that omits the agent's own entries, an opener that
// includes them, a `postId` that answers the same message twice, and an offset
// that moves on `answered` and `empty` only.
package gatewaytest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/uffici-ai/uffici-companion/internal/protocol"
)

// Key is the agent key the double accepts.
const Key = "uffici_test_agent_key_000000000000000000000000"

// PlatformErrorClasses is the conversations API's closed close vocabulary — an
// independent copy, pinned to the host's (gateway's vocabulary_test.go), so the
// double refuses what the platform refuses.
var PlatformErrorClasses = map[string]bool{
	"no_runtime": true, "harness_error": true, "refused": true, "over_budget": true,
}

// Injected is one scripted answer for a route, served instead of the real one.
type Injected struct {
	Status      int
	ContentType string
	Body        string
	// AfterEffect serves the injected answer AFTER the route did its work — a
	// post that was recorded and whose answer was lost on the way back.
	AfterEffect bool
}

// Conversation is one seeded conversation.
type Conversation struct {
	Offset  string
	Entries []protocol.Entry
	// Context is what the entries read carries (#2331); nil sends none, as an
	// older gateway does. The opener never carries one.
	Context *protocol.Context
	// Tools is the conversation's tool catalog (#2349); nil lists none.
	Tools []protocol.ToolDescriptor
	// ToolResults is each tool's scripted answer, by name; a listed tool with
	// none answers `{"ok":true}` and a null `callUrn`.
	ToolResults map[string]ToolAnswer
}

// ToolAnswer is a scripted tool-call answer. 🔴 CallURN is configured by the
// test, never built here: a call URN is an opaque string the companion relays,
// and the one codec for it is the platform's. Empty answers a null URN.
type ToolAnswer struct {
	CallURN string
	Result  json.RawMessage
}

// ToolCall is one recorded tool call (#2349): what reached the platform.
type ToolCall struct {
	RequestID      string
	ConversationID string
	Name           string
	Arguments      json.RawMessage
}

// Post is one recorded message.
type Post struct {
	MessageID string
	PostID    string
	RequestID string
	Text      string
	// Question is set when the post asked the person something (#2232).
	Question *protocol.PostQuestion
	// Citations and Activity are what the post reported (#2349).
	Citations []string
	Activity  []protocol.PostActivity
}

// Close is one recorded close.
type Close struct {
	RequestID  string
	Outcome    string
	ErrorClass string
}

// Double is the gateway double.
type Double struct {
	Server *httptest.Server
	// HeartbeatSeconds is sent in `hello`, and the double pings at that period
	// unless Silent is set.
	HeartbeatSeconds int
	// CloseLag is how long an ended session still reads as live (#2276): a
	// real registry lags the socket, and a test that closes "the next" session
	// must not mistake the last one for it. Zero in most tests.
	CloseLag time.Duration
	// ConversationTTL is a conversation ticket's lifetime.
	ConversationTTL time.Duration
	// PairingTTL is how long a pairing code lives (#1966).
	PairingTTL time.Duration
	// PairingOffers are the codes the PORTAL issued (#2209), by code: a pairing
	// opened with one is paired at once, and the code is spent. A code the
	// double was not told about is `not_found`, as the gateway answers it. Set
	// entries with OfferPairing.
	PairingOffers map[string]PairingOffer

	mu             sync.Mutex
	silent         bool
	conflict       bool
	connectTicket  map[string]bool
	convTickets    map[string]string
	ticketSeq      int
	mints          map[string]int
	connectFacts   []protocol.ConnectTicketRequest
	inject         map[string][]Injected
	conversations  map[string]*Conversation
	requests       map[string]ringed
	closedReqs     map[string]bool
	posts          []Post
	postsByID      map[string]string
	closes         []Close
	acks           []string
	refusals       []string
	sessions       []*session
	upgrades       int
	reads          map[string]int
	entriesBudgets []string
	pairings       map[string]*pendingPairing
	pairingSeq     int
	logsShipping   LogsShipping
	logsTickets    map[string]bool
	logsGate       chan struct{}
	logBatches     []protocol.LogBatch
	toolCalls      []ToolCall
}

type ringed struct {
	conversationID string
	through        string
}

type session struct {
	conn   *websocket.Conn
	cancel context.CancelFunc
	closed chan struct{}
}

// New starts a double on an httptest TLS server.
func New() *Double {
	d := &Double{
		HeartbeatSeconds: 30,
		ConversationTTL:  900 * time.Second,
		PairingTTL:       10 * time.Minute,
		PairingOffers:    map[string]PairingOffer{},
		pairings:         map[string]*pendingPairing{},
		connectTicket:    map[string]bool{},
		convTickets:      map[string]string{},
		mints:            map[string]int{},
		inject:           map[string][]Injected{},
		conversations:    map[string]*Conversation{},
		requests:         map[string]ringed{},
		closedReqs:       map[string]bool{},
		postsByID:        map[string]string{},
		reads:            map[string]int{},
		logsShipping:     LogsShipping{Enabled: true, HarnessLevel: "warn"},
		logsTickets:      map[string]bool{},
	}
	d.Server = httptest.NewTLSServer(http.HandlerFunc(d.serve))
	return d
}

// Close stops the server and every socket.
func (d *Double) Close() {
	d.mu.Lock()
	sessions := append([]*session(nil), d.sessions...)
	d.mu.Unlock()
	for _, s := range sessions {
		s.cancel()
		_ = s.conn.CloseNow()
	}
	d.Server.Close()
}

// URL is the double's base URL.
func (d *Double) URL() string { return d.Server.URL }

// Client is an HTTP client that trusts the double's certificate.
func (d *Double) Client() *http.Client { return d.Server.Client() }

// Seed adds a conversation.
func (d *Double) Seed(conversationID string, conversation Conversation) {
	d.mu.Lock()
	defer d.mu.Unlock()
	copied := conversation
	d.conversations[conversationID] = &copied
}

// SetTools gives a seeded conversation its catalog and scripted results (#2349).
func (d *Double) SetTools(conversationID string, tools []protocol.ToolDescriptor, results map[string]ToolAnswer) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if conversation, ok := d.conversations[conversationID]; ok {
		conversation.Tools, conversation.ToolResults = tools, results
	}
}

// Inject queues scripted answers for a route key: `connect`, `conversation`,
// `entries`, `opener`, `posts`, `close`, `doorbell`, `tools`, `tool-calls`.
func (d *Double) Inject(route string, answers ...Injected) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.inject[route] = append(d.inject[route], answers...)
}

// SetConflict makes every upgrade answer `instance_conflict` (4409).
func (d *Double) SetConflict(on bool) {
	d.mu.Lock()
	d.conflict = on
	d.mu.Unlock()
}

// SetSilent stops the heartbeat pings, so a client sees a half-open socket.
func (d *Double) SetSilent(on bool) {
	d.mu.Lock()
	d.silent = on
	d.mu.Unlock()
}

func (d *Double) next(route string) (Injected, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	queue := d.inject[route]
	if len(queue) == 0 {
		return Injected{}, false
	}
	d.inject[route] = queue[1:]
	return queue[0], true
}

func writeInjected(w http.ResponseWriter, answer Injected) {
	contentType := answer.ContentType
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(answer.Status)
	_, _ = io.WriteString(w, answer.Body)
}

// Envelope builds an error body in the protocol's shape.
func Envelope(class string, retryable bool, details map[string]string) string {
	raw, _ := json.Marshal(protocol.ErrorEnvelope{Error: protocol.ErrorBody{
		Class: class, Instruction: "instruction for " + class, Retryable: retryable, Details: details,
	}})
	return string(raw)
}

func fail(w http.ResponseWriter, status int, class string) {
	writeInjected(w, Injected{Status: status, Body: Envelope(class, class == "platform_unavailable", nil)})
}

func (d *Double) refuse(reason string) {
	d.mu.Lock()
	d.refusals = append(d.refusals, reason)
	d.mu.Unlock()
}

func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

func (d *Double) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case r.Method == http.MethodPost && path == "/v1/tickets/connect":
		d.mint(w, r, "connect")
	case r.Method == http.MethodPost && path == "/v1/tickets/conversation":
		d.mint(w, r, "conversation")
	case r.Method == http.MethodPost && path == "/v1/tickets/logs":
		d.mintLogs(w, r)
	case r.Method == http.MethodPost && path == "/v1/logs":
		d.pushLogs(w, r)
	case r.Method == http.MethodPost && path == "/v1/pairings":
		d.openPairing(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/pairings/"):
		d.pollPairing(w, r)
	case path == "/v1/doorbell":
		d.upgrade(w, r)
	case strings.HasPrefix(path, "/v1/conversations/"):
		d.control(w, r)
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/v1/requests/") && strings.HasSuffix(path, "/close"):
		d.closeRoute(w, r)
	case r.Method == http.MethodPost && path == "/v1/tool-calls":
		d.toolCall(w, r)
	default:
		fail(w, http.StatusNotFound, "not_found")
	}
}

func readBody(r *http.Request) []byte {
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	return raw
}

// mint serves both ticket routes.
func (d *Double) mint(w http.ResponseWriter, r *http.Request, route string) {
	if answer, ok := d.next(route); ok {
		writeInjected(w, answer)
		return
	}
	if bearer(r) != Key {
		fail(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	raw := readBody(r)
	schema := "connect-ticket-request"
	if route == "conversation" {
		schema = "conversation-ticket-request"
	}
	if err := protocol.Validate(schema, raw); err != nil {
		d.refuse(route + ": " + err.Error())
		fail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	d.mu.Lock()
	d.ticketSeq++
	token := fmt.Sprintf("%s-ticket-%d", route, d.ticketSeq)
	d.mints[route]++
	var answer any
	if route == "connect" {
		var body protocol.ConnectTicketRequest
		_ = json.Unmarshal(raw, &body)
		d.connectFacts = append(d.connectFacts, body)
		d.connectTicket[token] = true
		answer = protocol.TicketResponse{Ticket: token, ExpiresAt: time.Now().Add(30 * time.Second).UTC()}
	} else {
		var body protocol.ConversationTicketRequest
		_ = json.Unmarshal(raw, &body)
		d.convTickets[token] = body.ConversationID
		answer = protocol.ConversationTicketResponse{
			Ticket: token, ExpiresAt: time.Now().Add(d.ConversationTTL).UTC(), ConversationID: body.ConversationID,
		}
	}
	d.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(answer)
}

func (d *Double) upgrade(w http.ResponseWriter, r *http.Request) {
	if answer, ok := d.next("doorbell"); ok {
		writeInjected(w, answer)
		return
	}
	token := bearer(r)
	d.mu.Lock()
	valid := d.connectTicket[token]
	// 🔴 Single use, as the gateway spends a ticket's jti before any socket exists.
	delete(d.connectTicket, token)
	conflict := d.conflict
	d.upgrades++
	d.mu.Unlock()

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	if !valid {
		_ = conn.Close(4401, "ticket_invalid")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	if conflict {
		_ = conn.Write(ctx, websocket.MessageText, mustJSON(protocol.Disconnect{
			Envelope: protocol.Envelope{Type: protocol.FrameDisconnect, V: 1}, Reason: "instance_conflict",
			Instruction: "another harness is connected with this agent's key",
		}))
		_ = conn.Close(4409, "instance_conflict")
		cancel()
		return
	}
	s := &session{conn: conn, cancel: cancel, closed: make(chan struct{})}
	d.mu.Lock()
	d.sessions = append(d.sessions, s)
	sequence := len(d.sessions)
	d.mu.Unlock()
	_ = conn.Write(ctx, websocket.MessageText, mustJSON(protocol.Hello{
		Envelope:         protocol.Envelope{Type: protocol.FrameHello, V: 1},
		ConnectionID:     fmt.Sprintf("7c5e3a1b-9d8f-4b6a-8c2e-%012d", sequence),
		HeartbeatSeconds: d.HeartbeatSeconds,
		Limits:           protocol.HelloLimits{PostsPerMinute: 120, MaxPostBytes: 409600},
	}))
	go d.heartbeat(ctx, s)
	d.readAcks(ctx, s)
}

func (d *Double) heartbeat(ctx context.Context, s *session) {
	ticker := time.NewTicker(time.Duration(d.HeartbeatSeconds) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.closed:
			return
		case <-ticker.C:
			d.mu.Lock()
			silent := d.silent
			d.mu.Unlock()
			if !silent {
				pingCtx, stop := context.WithTimeout(ctx, time.Second)
				_ = s.conn.Ping(pingCtx)
				stop()
			}
		}
	}
}

// readAcks reads the client's frames: acks, and 🔴 NOTHING ELSE — any other
// frame is a protocol error and a disconnect, as on the gateway.
func (d *Double) readAcks(ctx context.Context, s *session) {
	defer func() {
		// A replica's registry learns a socket ended a moment after the client
		// does; CloseLag makes that moment long enough for a test to meet.
		time.Sleep(d.CloseLag)
		close(s.closed)
	}()
	for {
		_, raw, err := s.conn.Read(ctx)
		if err != nil {
			return
		}
		if err := protocol.Validate("ack", raw); err != nil {
			d.refuse("ack: " + err.Error())
			_ = s.conn.Write(ctx, websocket.MessageText, mustJSON(protocol.Disconnect{
				Envelope: protocol.Envelope{Type: protocol.FrameDisconnect, V: 1}, Reason: "protocol_error",
				Instruction: "the harness sent a frame the doorbell does not accept",
			}))
			_ = s.conn.Close(4400, "protocol_error")
			return
		}
		var ack protocol.Ack
		_ = json.Unmarshal(raw, &ack)
		d.mu.Lock()
		d.acks = append(d.acks, ack.RingID)
		d.mu.Unlock()
	}
}

func (d *Double) live() (*session, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := len(d.sessions) - 1; i >= 0; i-- {
		select {
		case <-d.sessions[i].closed:
		default:
			return d.sessions[i], nil
		}
	}
	return nil, ErrNoDoorbell
}

// Ring sends a ring on the live doorbell and records the request.
func (d *Double) Ring(ring protocol.Ring) error {
	s, err := d.live()
	if err != nil {
		return err
	}
	ring.Envelope = protocol.Envelope{Type: protocol.FrameRing, V: 1}
	d.mu.Lock()
	d.requests[ring.RequestID] = ringed{conversationID: ring.ConversationID, through: ring.Through}
	d.mu.Unlock()
	return s.conn.Write(context.Background(), websocket.MessageText, mustJSON(ring))
}

// Cancel sends a cancel on the live doorbell.
func (d *Double) Cancel(requestID, reason string) error {
	s, err := d.live()
	if err != nil {
		return err
	}
	return s.conn.Write(context.Background(), websocket.MessageText, mustJSON(protocol.Cancel{
		Envelope: protocol.Envelope{Type: protocol.FrameCancel, V: 1}, RequestID: requestID, Reason: reason,
	}))
}

// Disconnect sends a disconnect frame and closes the live socket with code.
func (d *Double) Disconnect(reason string, code websocket.StatusCode) error {
	s, err := d.live()
	if err != nil {
		return err
	}
	ctx := context.Background()
	_ = s.conn.Write(ctx, websocket.MessageText, mustJSON(protocol.Disconnect{
		Envelope: protocol.Envelope{Type: protocol.FrameDisconnect, V: 1}, Reason: reason,
		Instruction: "instruction for " + reason,
	}))
	return s.conn.Close(code, reason)
}

// CloseSocket closes the live socket with a code and no disconnect frame.
func (d *Double) CloseSocket(code websocket.StatusCode, reason string) error {
	s, err := d.live()
	if err != nil {
		return err
	}
	return s.conn.Close(code, reason)
}

// ErrNoDoorbell is what every doorbell-side call answers when no session is
// live: none ever connected, or the last one ended.
var ErrNoDoorbell = errors.New("gatewaytest: no doorbell is connected")

// IsGone is whether an error from CloseSocket means the session was ALREADY
// gone — either no doorbell was live any more, or the close handshake found
// the socket closed underneath it. The companion under test tears a doorbell
// down between a caller's LiveSessions() and its CloseSocket (a replacement
// flap does exactly that, #2195), and in both shapes the session has ended,
// which is what the caller wanted. A test that means "close it, or it is
// already closed" checks this; one that means "nothing must be connected"
// keeps the error.
func IsGone(err error) bool {
	return errors.Is(err, ErrNoDoorbell) || errors.Is(err, net.ErrClosed) ||
		strings.Contains(err.Error(), "use of closed network connection")
}

// convTicket checks a conversation ticket and the path's conversation.
func (d *Double) convTicket(w http.ResponseWriter, r *http.Request, pathConversation string) (string, bool) {
	d.mu.Lock()
	conversationID, ok := d.convTickets[bearer(r)]
	d.mu.Unlock()
	if !ok {
		fail(w, http.StatusUnauthorized, "ticket_invalid")
		return "", false
	}
	if pathConversation != "" && pathConversation != conversationID {
		fail(w, http.StatusBadRequest, "conversation_mismatch")
		return "", false
	}
	return conversationID, true
}

func (d *Double) control(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/conversations/"), "/")
	if len(parts) != 2 {
		fail(w, http.StatusNotFound, "not_found")
		return
	}
	route := parts[1]
	injected, hasInjected := d.next(route)
	if hasInjected && !injected.AfterEffect {
		writeInjected(w, injected)
		return
	}
	conversationID, ok := d.convTicket(w, r, parts[0])
	if !ok {
		return
	}
	switch {
	case r.Method == http.MethodGet && (route == "entries" || route == "opener"):
		d.read(w, r, conversationID, route)
	case r.Method == http.MethodPost && route == "posts":
		d.post(w, r, conversationID, injected, hasInjected)
	case r.Method == http.MethodGet && route == "tools":
		d.tools(w, conversationID)
	default:
		fail(w, http.StatusNotFound, "not_found")
	}
}

func (d *Double) read(w http.ResponseWriter, r *http.Request, conversationID, route string) {
	d.mu.Lock()
	conversation, known := d.conversations[conversationID]
	d.reads[route]++
	d.mu.Unlock()
	if !known {
		fail(w, http.StatusNotFound, "not_found")
		return
	}
	query := r.URL.Query()
	through := query.Get("through")
	if route == "entries" {
		d.mu.Lock()
		d.entriesBudgets = append(d.entriesBudgets, query.Get("budgetTokens"))
		d.mu.Unlock()
	}
	var entries []protocol.Entry
	for _, entry := range conversation.Entries {
		if after(entry.Cursor, through) {
			continue
		}
		if route == "entries" {
			if !after(entry.Cursor, query.Get("since")) || entry.Author.Self {
				continue
			}
		}
		entries = append(entries, entry)
	}
	if entries == nil {
		entries = []protocol.Entry{}
	}
	w.Header().Set("Content-Type", "application/json")
	answer := protocol.ReadResponse{
		Entries: entries, Bounded: false,
		Identity: &protocol.Identity{
			Lines:      []string{"You are an AI agent, not a human.", "Never present yourself as a human."},
			Disclosure: protocol.Disclosure{Mode: "labelled_ui"},
		},
	}
	if route == "entries" {
		answer.Context = conversation.Context
	}
	_ = json.NewEncoder(w).Encode(answer)
}

func (d *Double) post(w http.ResponseWriter, r *http.Request, conversationID string, injected Injected, hasInjected bool) {
	raw := readBody(r)
	if err := protocol.Validate("post-request", raw); err != nil {
		d.refuse("post: " + err.Error())
		fail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var body protocol.PostRequest
	_ = json.Unmarshal(raw, &body)
	d.mu.Lock()
	request, rung := d.requests[body.RequestID]
	closed := d.closedReqs[body.RequestID]
	messageID, seen := d.postsByID[body.PostID]
	if rung && request.conversationID == conversationID && !closed && !seen {
		messageID = strconv.Itoa(2000 + len(d.posts))
		d.postsByID[body.PostID] = messageID
		d.posts = append(d.posts, Post{
			MessageID: messageID, PostID: body.PostID, RequestID: body.RequestID, Text: body.Text,
			Question: body.Question, Citations: body.Citations, Activity: body.Activity,
		})
	}
	d.mu.Unlock()
	switch {
	case !rung || request.conversationID != conversationID:
		fail(w, http.StatusNotFound, "not_found")
	case closed && !seen:
		fail(w, http.StatusConflict, "request_closed")
	case hasInjected:
		writeInjected(w, injected)
	default:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protocol.PostResponse{MessageID: messageID})
	}
}

// tools serves a conversation's catalog (#2349), validated against the protocol
// before it leaves, so a test seeding an off-contract catalog finds out here.
func (d *Double) tools(w http.ResponseWriter, conversationID string) {
	d.mu.Lock()
	conversation, known := d.conversations[conversationID]
	d.reads["tools"]++
	var tools []protocol.ToolDescriptor
	if known {
		tools = append(tools, conversation.Tools...)
	}
	d.mu.Unlock()
	if !known {
		fail(w, http.StatusNotFound, "not_found")
		return
	}
	if tools == nil {
		tools = []protocol.ToolDescriptor{}
	}
	raw := mustJSON(protocol.ToolCatalogResponse{Tools: tools})
	if err := protocol.Validate("tool-catalog-response", raw); err != nil {
		panic("gatewaytest: a seeded catalog is off-contract: " + err.Error())
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

// toolCall serves `POST /v1/tool-calls` (#2349): the request must be one the
// double rang, in the ticket's conversation, and still open — the gateway's
// rules — and every call that passes them is RECORDED before it is answered,
// so an injected AfterEffect answer is a call that ran and whose answer was lost.
func (d *Double) toolCall(w http.ResponseWriter, r *http.Request) {
	injected, hasInjected := d.next("tool-calls")
	if hasInjected && !injected.AfterEffect {
		writeInjected(w, injected)
		return
	}
	conversationID, ok := d.convTicket(w, r, "")
	if !ok {
		return
	}
	raw := readBody(r)
	if err := protocol.Validate("tool-call-request", raw); err != nil {
		d.refuse("tool-call: " + err.Error())
		fail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var body protocol.ToolCallRequest
	_ = json.Unmarshal(raw, &body)
	d.mu.Lock()
	request, rung := d.requests[body.RequestID]
	closed := d.closedReqs[body.RequestID]
	conversation := d.conversations[conversationID]
	listed := false
	var answer ToolAnswer
	if conversation != nil {
		for _, tool := range conversation.Tools {
			listed = listed || tool.Name == body.Name
		}
		answer = conversation.ToolResults[body.Name]
	}
	admitted := rung && request.conversationID == conversationID && !closed && listed
	if admitted {
		d.toolCalls = append(d.toolCalls, ToolCall{
			RequestID: body.RequestID, ConversationID: conversationID, Name: body.Name, Arguments: body.Arguments,
		})
	}
	d.mu.Unlock()
	switch {
	case !rung || request.conversationID != conversationID || !listed:
		fail(w, http.StatusNotFound, "not_found")
	case closed:
		fail(w, http.StatusConflict, "request_closed")
	case hasInjected:
		writeInjected(w, injected)
	default:
		response := protocol.ToolCallResponse{Result: answer.Result}
		if response.Result == nil {
			response.Result = json.RawMessage(`{"ok":true}`)
		}
		if answer.CallURN != "" {
			urn := answer.CallURN
			response.CallURN = &urn
		}
		raw := mustJSON(response)
		if err := protocol.Validate("tool-call-response", raw); err != nil {
			panic("gatewaytest: a scripted tool answer is off-contract: " + err.Error())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}
}

func (d *Double) closeRoute(w http.ResponseWriter, r *http.Request) {
	if answer, ok := d.next("close"); ok {
		writeInjected(w, answer)
		return
	}
	requestID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/requests/"), "/close")
	conversationID, ok := d.convTicket(w, r, "")
	if !ok {
		return
	}
	raw := readBody(r)
	if err := protocol.Validate("close-request", raw); err != nil {
		d.refuse("close: " + err.Error())
		fail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var body protocol.CloseRequest
	_ = json.Unmarshal(raw, &body)
	if body.Outcome == "error" && !PlatformErrorClasses[body.ErrorClass] {
		d.refuse("close: errorClass " + body.ErrorClass)
		fail(w, http.StatusBadRequest, "invalid_error_class")
		return
	}
	d.mu.Lock()
	request, rung := d.requests[requestID]
	if !rung || request.conversationID != conversationID {
		d.mu.Unlock()
		fail(w, http.StatusNotFound, "not_found")
		return
	}
	committed := false
	if !d.closedReqs[requestID] {
		d.closedReqs[requestID] = true
		d.closes = append(d.closes, Close{RequestID: requestID, Outcome: body.Outcome, ErrorClass: body.ErrorClass})
		if body.Outcome == "answered" || body.Outcome == "empty" {
			if conversation, ok := d.conversations[conversationID]; ok {
				conversation.Offset = request.through
			}
			committed = true
		}
	}
	d.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(protocol.CloseResponse{Outcome: body.Outcome, Committed: committed})
}

// after reports whether cursor a sorts after b: numerically when both are
// numbers, as the platform's are, and as strings otherwise.
func after(a, b string) bool {
	if b == "" {
		return true
	}
	x, errA := strconv.ParseUint(a, 10, 64)
	y, errB := strconv.ParseUint(b, 10, 64)
	if errA == nil && errB == nil {
		return x > y
	}
	return a > b
}

func mustJSON(value any) []byte {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return raw
}

// ── What a test reads ───────────────────────────────────────────────────────

// Posts returns the recorded messages.
func (d *Double) Posts() []Post {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]Post(nil), d.posts...)
}

// ToolCalls returns every tool call that reached the platform (#2349).
func (d *Double) ToolCalls() []ToolCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]ToolCall(nil), d.toolCalls...)
}

// Closes returns the recorded closes.
func (d *Double) Closes() []Close {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]Close(nil), d.closes...)
}

// Acks returns the acknowledged ring ids.
func (d *Double) Acks() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.acks...)
}

// Refusals returns every body the double refused as off-contract.
func (d *Double) Refusals() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.refusals...)
}

// Mints counts ticket mints by route (`connect`, `conversation`).
func (d *Double) Mints(route string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.mints[route]
}

// EntriesBudgets is the `budgetTokens` every entries read carried, in order —
// "" when it carried none.
func (d *Double) EntriesBudgets() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.entriesBudgets...)
}

// Reads counts reads by route (`entries`, `opener`).
func (d *Double) Reads(route string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reads[route]
}

// Upgrades counts doorbell upgrade attempts.
func (d *Double) Upgrades() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.upgrades
}

// ConnectFacts returns every connection-ticket body received.
func (d *Double) ConnectFacts() []protocol.ConnectTicketRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]protocol.ConnectTicketRequest(nil), d.connectFacts...)
}

// OffsetOf reports a conversation's committed offset.
func (d *Double) OffsetOf(conversationID string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	conversation, ok := d.conversations[conversationID]
	if !ok {
		return "", fmt.Errorf("gatewaytest: no conversation %s", conversationID)
	}
	return conversation.Offset, nil
}

// Connected reports whether a doorbell socket is live.
func (d *Double) Connected() bool {
	_, err := d.live()
	return err == nil
}

// Sessions counts every doorbell socket the double ever accepted, open or not.
// 🔴 A test that closes "the next" session waits on this, never on
// LiveSessions alone (#2276): right after a close, the session just ended can
// still read as live while its replacement is not yet accepted, and a close
// aimed then lands on the dead one and leaves the new one open.
func (d *Double) Sessions() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.sessions)
}

// LiveSessions counts the doorbell sockets still open on the double — what a
// replica's registry would hold for this companion (#2094).
func (d *Double) LiveSessions() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	live := 0
	for _, s := range d.sessions {
		select {
		case <-s.closed:
		default:
			live++
		}
	}
	return live
}
