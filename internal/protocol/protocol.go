// Package protocol is the platform-to-companion contract as Go types: the
// doorbell's four server frames and its one client frame, the JSON control
// plane's bodies, and the error envelope every non-2xx answer carries.
//
// 🔴 HAND-WRITTEN, and pinned against the embedded JSON Schemas in both
// directions by `parity_test.go` — the local wire's precedent
// (`internal/wire/frames_parity_test.go`). A generator was weighed and declined
// on #2077: it would be a second toolchain in a module whose purpose is to need
// none, and what it would buy — structs that cannot drift from the schemas — is
// asserted directly instead.
//
// Decoding IGNORES unknown fields (#1994 decision 44): the server's frames and
// responses are additive, so a newer platform must never break an older
// companion. Encoding emits exactly the fields below, because every CLIENT body
// is strict on the platform: an extra field is `invalid_request`, or on the
// doorbell a `protocol_error` and a disconnect.
package protocol

import (
	"encoding/json"
	"time"
)

// ── The doorbell ────────────────────────────────────────────────────────────

// Envelope is the part of every doorbell frame that says what it is.
type Envelope struct {
	Type string `json:"type"`
	V    int    `json:"v"`
}

// Hello is the server's first frame on a doorbell connection.
type Hello struct {
	Envelope
	ConnectionID     string      `json:"connectionId"`
	HeartbeatSeconds int         `json:"heartbeatSeconds"`
	Limits           HelloLimits `json:"limits"`
}

// HelloLimits are the bounds the gateway enforces on this connection's posts.
type HelloLimits struct {
	PostsPerMinute int `json:"postsPerMinute"`
	MaxPostBytes   int `json:"maxPostBytes"`
}

// Ring opens (or redelivers) one answer request: ids and a reason, never
// content (#1958 decision 4).
type Ring struct {
	Envelope
	// RingID is fresh per DELIVERY; a redelivery carries a new one.
	RingID         string    `json:"ringId"`
	RequestID      string    `json:"requestId"`
	ConversationID string    `json:"conversationId"`
	Reason         string    `json:"reason"`
	Cause          RingCause `json:"cause"`
	Deadline       time.Time `json:"deadline"`
	Since          string    `json:"since"`
	Through        string    `json:"through"`
	// ResetSession says start fresh (#2356): use no state from before this ring
	// for this conversation — the first ring of a card's lane pass.
	ResetSession bool `json:"resetSession"`
}

// RingCause is the entry that caused the request, and who wrote it — by
// `{type, id}` only, so a name can never ride a ring.
type RingCause struct {
	EntryID string     `json:"entryId"`
	Author  RingAuthor `json:"author"`
}

// RingAuthor is the cause's author: a type and an id, nothing else.
type RingAuthor struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// Cancel withdraws an open request.
type Cancel struct {
	Envelope
	RequestID string `json:"requestId"`
	// Reason is `stop`, `superseded` or `expired`.
	Reason string `json:"reason"`
}

// Release says the harness may discard what it holds for one conversation
// (#2356): the agent's lane pass on a card ended, or the card was archived or
// its conversation deleted. It withdraws no request.
type Release struct {
	Envelope
	ConversationID string `json:"conversationId"`
	// Reason is `pass_ended`, `archived` or `deleted`.
	Reason string `json:"reason"`
}

// Disconnect is the server saying why it is about to close the socket.
type Disconnect struct {
	Envelope
	Reason string `json:"reason"`
	// Instruction is a sentence the agent relays — decision 32 on the doorbell.
	Instruction string `json:"instruction"`
}

// Ack is the ONLY client frame: this companion has the ring.
type Ack struct {
	Envelope
	RingID string `json:"ringId"`
}

// NewAck builds the one frame a companion ever sends on the doorbell.
func NewAck(ringID string) Ack {
	return Ack{Envelope: Envelope{Type: "ack", V: 1}, RingID: ringID}
}

// Frame types the server sends.
const (
	FrameHello      = "hello"
	FrameRing       = "ring"
	FrameCancel     = "cancel"
	FrameRelease    = "release"
	FrameDisconnect = "disconnect"
)

// ── Tickets ─────────────────────────────────────────────────────────────────

// ConnectTicketRequest is `POST /v1/tickets/connect`'s body.
type ConnectTicketRequest struct {
	// InstanceID is this companion's persistent device id — the one-key-one-
	// harness gate compares it.
	InstanceID string       `json:"instanceId"`
	Client     ClientFacts  `json:"client"`
	Harness    HarnessFacts `json:"harness"`
	// Plugin and Host are optional (#2236): the attached plugin's version and
	// the machine's kind, for the General tab — never authority.
	Plugin *PluginFacts `json:"plugin,omitempty"`
	Host   *HostFacts   `json:"host,omitempty"`
}

// PluginFacts is the attached plugin's self-description, from its hello (#2236).
type PluginFacts struct {
	Version string `json:"version"`
}

// HostFacts is the machine's kind (#2236): the operating system and
// architecture as Go names them, and the OS release when it can be read.
// 🔴 Never a hostname, a user or a path.
type HostFacts struct {
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	OSRelease string `json:"osRelease,omitempty"`
}

// ClientFacts is the companion's self-description, for the Agents screen.
type ClientFacts struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// HarnessFacts is the harness's self-description, from the plugin's hello.
type HarnessFacts struct {
	Kind    string `json:"kind"`
	Version string `json:"version"`
}

// PairingRequest is `POST /v1/pairings`'s body (#1966) — sent with NO credential.
type PairingRequest struct {
	InstanceID string       `json:"instanceId"`
	Client     ClientFacts  `json:"client"`
	Harness    HarnessFacts `json:"harness"`
	// PublicKey is this pairing's throwaway X25519 public key, base64url — the
	// agent key comes back sealed to it.
	PublicKey string `json:"publicKey"`
	// Code is a pairing code the portal issued for one agent, pasted here as part
	// of the command an admin was given (#2209): with it the pairing needs no
	// approval. Empty on the typed-code path. Secret-class: never logged.
	Code string `json:"code,omitempty"`
}

// PairingResponse is `POST /v1/pairings`' answer.
type PairingResponse struct {
	PairingID string `json:"pairingId"`
	// Code is what the person reads out to an admin. Short-lived, single-use.
	Code string `json:"code"`
	// PollToken alone collects the result. Secret-class: never printed, never logged.
	PollToken        string `json:"pollToken"`
	ExpiresAt        string `json:"expiresAt"`
	PollAfterSeconds int    `json:"pollAfterSeconds"`
}

// Pairing poll statuses.
const (
	PairingPending = "pending"
	PairingPaired  = "paired"
)

// PairingPollResponse is `GET /v1/pairings/{id}`'s answer: `pending` with a
// retry delay, or `paired` with the key sealed to this pairing's public key.
type PairingPollResponse struct {
	Status            string     `json:"status"`
	RetryAfterSeconds int        `json:"retryAfterSeconds,omitempty"`
	AgentID           string     `json:"agentId,omitempty"`
	OrgSlug           string     `json:"orgSlug,omitempty"`
	AgentSlug         string     `json:"agentSlug,omitempty"`
	KeyLast4          string     `json:"keyLast4,omitempty"`
	Sealed            *SealedKey `json:"sealed,omitempty"`
}

// SealedKey is the agent key as a pairing delivers it — only the pairing's own
// private key opens it (internal/pairing).
type SealedKey struct {
	Alg                string `json:"alg"`
	EphemeralPublicKey string `json:"ephemeralPublicKey"`
	Nonce              string `json:"nonce"`
	Ciphertext         string `json:"ciphertext"`
}

// ConversationTicketRequest is `POST /v1/tickets/conversation`'s body.
type ConversationTicketRequest struct {
	InstanceID     string `json:"instanceId"`
	ConversationID string `json:"conversationId"`
}

// TicketResponse answers a connection-ticket mint.
type TicketResponse struct {
	Ticket    string    `json:"ticket"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// ConversationTicketResponse answers a conversation-ticket mint.
type ConversationTicketResponse struct {
	Ticket         string    `json:"ticket"`
	ExpiresAt      time.Time `json:"expiresAt"`
	ConversationID string    `json:"conversationId"`
}

// ── The control plane ───────────────────────────────────────────────────────

// Entry is one transcript entry as the platform bounded it.
type Entry struct {
	ID          string       `json:"id"`
	Cursor      string       `json:"cursor"`
	At          time.Time    `json:"at"`
	Author      Author       `json:"author"`
	Kind        string       `json:"kind"`
	Text        string       `json:"text"`
	Attachments []Attachment `json:"attachments"`
}

// Author is who wrote an entry.
type Author struct {
	// Type is `user`, `agent`, `system` or `external`.
	Type   string `json:"type"`
	ID     string `json:"id"`
	Label  string `json:"label,omitempty"`
	Handle string `json:"handle,omitempty"`
	Self   bool   `json:"self"`
}

// Attachment is a file an entry carries, by reference.
type Attachment struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Ref  string `json:"ref"`
}

// ReadResponse answers both reads: the delta and the opener.
type ReadResponse struct {
	Entries []Entry `json:"entries"`
	Bounded bool    `json:"bounded"`
	// Identity is REQUIRED (#2072): a read without it is refused.
	Identity *Identity `json:"identity"`
	// Context is the composed context (#2331) — on the entries read only, and
	// optional, so an older gateway's read still decodes.
	Context *Context `json:"context,omitempty"`
	// Card is the card a card conversation's read is about (#2354). The companion
	// forwards the context's Card block — the platform composed it — and never
	// reads this; it is decoded so the struct matches the contract.
	Card *CardRead `json:"card,omitempty"`
}

// CardRead is a read's card: `state` is "ok" with every other field, or
// "unavailable" with none — the context then names `card_unavailable`.
type CardRead struct {
	State       string        `json:"state"`
	CardURN     string        `json:"cardUrn,omitempty"`
	BoardURN    string        `json:"boardUrn,omitempty"`
	Title       string        `json:"title,omitempty"`
	Description *string       `json:"description,omitempty"`
	Board       *CardBoard    `json:"board,omitempty"`
	Lane        *CardLane     `json:"lane,omitempty"`
	Lanes       []CardLaneRef `json:"lanes,omitempty"`
	Role        string        `json:"role,omitempty"`
	// Pass is the lane pass the read is in (#2356).
	Pass *CardPass `json:"pass,omitempty"`
	// Work is the card's Work section, by pass (#2357).
	Work []CardWorkPart `json:"work,omitempty"`
}

// CardWorkPart is one author's part of a card's Work section for one lane pass.
// An agent author is its slug; a person carries no name at all.
type CardWorkPart struct {
	LaneName  *string        `json:"laneName"`
	PassSeq   int            `json:"passSeq"`
	Author    CardWorkAuthor `json:"author"`
	Body      string         `json:"body"`
	UpdatedAt string         `json:"updatedAt"`
	Yours     bool           `json:"yours"`
}

// CardWorkAuthor is `agent` with its slug, or `user` with nothing else.
type CardWorkAuthor struct {
	Type string  `json:"type"`
	Slug *string `json:"slug,omitempty"`
}

// CardPass is the arrival that began the pass, and how many entries of earlier
// passes the read counted instead of serving.
type CardPass struct {
	ArrivalEntryID int `json:"arrivalEntryId"`
	EarlierEntries int `json:"earlierEntries"`
}

// CardBoard is the card's board: its name and its briefing.
type CardBoard struct {
	Name     string  `json:"name"`
	Briefing *string `json:"briefing"`
}

// CardLane is the lane the card is in, with its instruction and its agent.
type CardLane struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Instruction *string `json:"instruction"`
	AgentID     *string `json:"agentId"`
}

// CardLaneRef is one lane of the board's lane map.
type CardLaneRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Context is what the agent is told before the conversation, composed by the
// platform: the identity block first, then the pinned levels. A part it could
// not get is NAMED in Missing — never a silent gap (#2331).
type Context struct {
	Blocks  []ContextBlock   `json:"blocks"`
	Tokens  int              `json:"tokens"`
	Bounded bool             `json:"bounded"`
	Missing []ContextMissing `json:"missing,omitempty"`
}

// ContextBlock is one composed level; Depth is absent on the identity block.
type ContextBlock struct {
	Name   string `json:"name"`
	Scope  string `json:"scope"`
	Depth  *int   `json:"depth,omitempty"`
	Tokens int    `json:"tokens"`
	Text   string `json:"text"`
}

// ContextMissing is a part the brief should have had, with the sentence the
// agent relays.
type ContextMissing struct {
	Part        string `json:"part"`
	Class       string `json:"class"`
	Instruction string `json:"instruction"`
}

// Identity is the AI-identity floor a brief carries (Art. 50(1)).
type Identity struct {
	Lines      []string   `json:"lines"`
	Disclosure Disclosure `json:"disclosure"`
	// Workspace is the platform's sentences on what the agent's Uffici tools are
	// for — optional, so an older gateway's read still decodes.
	Workspace []string `json:"workspace,omitempty"`
}

// Disclosure is the conversation's disclosure state.
type Disclosure struct {
	// Mode is `labelled_ui`, `sent` or `off`.
	Mode string `json:"mode"`
	Text string `json:"text,omitempty"`
}

// PostRequest is `POST /v1/conversations/:id/posts`'s body.
type PostRequest struct {
	// PostID is minted by the companion per post and reused on every retry of
	// it: the idempotency key, so a redelivered post answers the same message.
	PostID    string `json:"postId"`
	RequestID string `json:"requestId"`
	Text      string `json:"text"`
	// Audience is `external`, the only value in stage 1.
	Audience    string       `json:"audience"`
	Attachments []Attachment `json:"attachments,omitempty"`
	// Question is present when the post is the agent ASKING (#2232).
	Question *PostQuestion `json:"question,omitempty"`
	// Citations are the `callUrn`s of the agent's own tool calls this answer
	// rests on (#2348); the platform refuses a citation of anyone else's call.
	Citations []string `json:"citations,omitempty"`
	// Activity is what the agent did with its harness's OWN tools, by name —
	// shown marked, never treated as a platform tool call.
	Activity []PostActivity `json:"activity,omitempty"`
}

// PostActivity is one harness-tool use a post reports: the tool's name and a
// one-line summary, never its arguments or result.
type PostActivity struct {
	Tool    string `json:"tool"`
	Summary string `json:"summary"`
}

// PostQuestion is what an agent's question offers (#2232): the harness's own
// id for it, the choices, and whether several may be picked.
type PostQuestion struct {
	ID          string   `json:"id"`
	Choices     []string `json:"choices"`
	MultiSelect bool     `json:"multiSelect"`
}

// PostResponse answers a post.
type PostResponse struct {
	MessageID string `json:"messageId"`
}

// CloseRequest is `POST /v1/requests/:id/close`'s body.
type CloseRequest struct {
	Outcome    string `json:"outcome"`
	ErrorClass string `json:"errorClass,omitempty"`
}

// CloseResponse answers a close.
type CloseResponse struct {
	Outcome   string `json:"outcome"`
	Committed bool   `json:"committed"`
}

// ToolCatalogResponse is `GET /v1/conversations/:id/tools`' answer: every tool
// the agent may call in that conversation. A denied tool is never listed.
type ToolCatalogResponse struct {
	Tools []ToolDescriptor `json:"tools"`
}

// ToolDescriptor is one tool as the platform lists it. Parameters is the tool's
// JSON Schema, carried verbatim.
type ToolDescriptor struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	// Permission is `allow` or `ask`.
	Permission string `json:"permission"`
}

// ToolCallRequest is `POST /v1/tool-calls`' body: one tool call on the request
// the agent is answering. 🔴 Arguments are tenant content: never logged.
type ToolCallRequest struct {
	RequestID string          `json:"requestId"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ToolRerunRequest is `POST /v1/tool-calls/rerun`' body (#2454): re-run the call
// the URN names on the request the agent is answering. The answer is a
// ToolCallResponse; a call whose arguments were not kept answers the result
// `rerun_unavailable` with no URN.
type ToolRerunRequest struct {
	RequestID string `json:"requestId"`
	Call      string `json:"call"`
}

// MemoryRecallRequest is `POST /v1/memory/recall`' body (#2385): every level the
// conversation sits under, searched for the query. 🔴 The query is tenant content:
// never logged. DeadlineMS is the plugin's whole budget; every hop answers inside it.
type MemoryRecallRequest struct {
	RequestID  string `json:"requestId"`
	Query      string `json:"query"`
	Limit      int    `json:"limit,omitempty"`
	DeadlineMS int    `json:"deadlineMs,omitempty"`
}

// MemorySource is who a fact came from: `remembered` or `member_written`.
type MemorySource struct {
	Kind string `json:"kind"`
}

// MemoryHit is one recalled fact. 🔴 A level, never a folder id or a chain; the
// text is tenant content, never logged. Distance is nil when full text found it.
type MemoryHit struct {
	URN      string       `json:"urn"`
	Level    string       `json:"level"`
	Distance *float64     `json:"distance"`
	Text     string       `json:"text"`
	At       string       `json:"at"`
	Source   MemorySource `json:"source"`
}

// MemoryRecallResponse answers a recall. Degraded says full text stood in;
// Missing names what could not be had (`embeddings`).
type MemoryRecallResponse struct {
	Hits     []MemoryHit `json:"hits"`
	Degraded bool        `json:"degraded"`
	Missing  []string    `json:"missing,omitempty"`
}

// MemoryRememberRequest is `POST /v1/memory/remember`' body: ONE fact on the
// conversation. There is no level: an agent writes where it is.
type MemoryRememberRequest struct {
	RequestID string `json:"requestId"`
	Text      string `json:"text"`
}

// MemoryRememberResponse names the remembered fact.
type MemoryRememberResponse struct {
	URN string `json:"urn"`
}

// MemoryForgetRequest is `POST /v1/memory/forget`' body: MARK a fact superseded,
// by a newer fact or by the request. Never a delete.
type MemoryForgetRequest struct {
	RequestID      string `json:"requestId"`
	URN            string `json:"urn"`
	ReplacementURN string `json:"replacementUrn,omitempty"`
}

// MemoryForgetResponse names what superseded the fact.
type MemoryForgetResponse struct {
	URN          string `json:"urn"`
	SupersededBy string `json:"supersededBy"`
}

// MemoryUnfoldRequest is `POST /v1/memory/unfold`' body: what a URN superseded.
type MemoryUnfoldRequest struct {
	RequestID string `json:"requestId"`
	URN       string `json:"urn"`
	Depth     int    `json:"depth,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

// MemorySupersededBy is what superseded an unfolded row: a URN this place can see,
// or `{visible: false}` — something it cannot.
type MemorySupersededBy struct {
	Visible bool   `json:"visible"`
	URN     string `json:"urn,omitempty"`
}

// MemoryUnfoldRow is one row an unfold reaches, `depth` hops back.
type MemoryUnfoldRow struct {
	URN            string        `json:"urn"`
	Level          string        `json:"level"`
	Text           string        `json:"text"`
	At             string        `json:"at"`
	Depth          int           `json:"depth"`
	SupersededKind string        `json:"supersededKind"`
	Source         *MemorySource `json:"source"`
}

// MemoryUnfoldResponse answers an unfold.
type MemoryUnfoldResponse struct {
	URN          string              `json:"urn"`
	SupersededBy *MemorySupersededBy `json:"supersededBy"`
	Rows         []MemoryUnfoldRow   `json:"rows"`
}

// ToolCallResponse answers a tool call: the tool's result as the platform
// returned it (an actionable refusal included), and the call's ledger URN when
// it ran — null when it did not (an approval asked for). 🔴 Never logged.
type ToolCallResponse struct {
	CallURN *string         `json:"callUrn"`
	Result  json.RawMessage `json:"result"`
}

// ── Errors ──────────────────────────────────────────────────────────────────

// ErrorEnvelope is every non-2xx body the gateway answers.
type ErrorEnvelope struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody is decision 32's shape: a class for the machine, an instruction the
// agent relays to the person.
type ErrorBody struct {
	Class       string            `json:"class"`
	Instruction string            `json:"instruction"`
	Retryable   bool              `json:"retryable"`
	Details     map[string]string `json:"details,omitempty"`
}

// ── A machine's log lines ───────────────────────────────────────────────────

// LogsTicketRequest is `POST /v1/tickets/logs`'s body.
type LogsTicketRequest struct {
	InstanceID string `json:"instanceId"`
}

// LogsShipping is the agent's log policy, answered on every logs-ticket mint.
type LogsShipping struct {
	// Enabled is false when an administrator switched shipping off for the
	// agent; no ticket comes with it.
	Enabled bool `json:"enabled"`
	// HarnessLevel is the effective floor for the harness's own lines.
	HarnessLevel string `json:"harnessLevel"`
	// RecheckSeconds is when to ask again.
	RecheckSeconds int `json:"recheckSeconds"`
}

// LogsTicketResponse answers a logs-ticket mint: the policy, and a ticket when
// shipping is on.
type LogsTicketResponse struct {
	Shipping  LogsShipping `json:"shipping"`
	Ticket    string       `json:"ticket,omitempty"`
	ExpiresAt *time.Time   `json:"expiresAt,omitempty"`
}

// LogBatchClient describes who is shipping: the companion, and the harness and
// plugin it speaks for when one is attached.
type LogBatchClient struct {
	Name           string `json:"name"`
	Version        string `json:"version"`
	HarnessKind    string `json:"harnessKind,omitempty"`
	HarnessVersion string `json:"harnessVersion,omitempty"`
	PluginVersion  string `json:"pluginVersion,omitempty"`
}

// LogEntry is one line on the wire, already redacted and clipped.
type LogEntry struct {
	TS         string `json:"ts"`
	Source     string `json:"source"`
	Level      string `json:"level"`
	Logger     string `json:"logger,omitempty"`
	Message    string `json:"message"`
	Attrs      string `json:"attrs,omitempty"`
	Redactions int    `json:"redactions,omitempty"`
}

// LogBatch is `POST /v1/logs`'s body. It names no agent and no organization:
// the ticket does.
type LogBatch struct {
	Client  LogBatchClient `json:"client"`
	Entries []LogEntry     `json:"entries"`
}

// LogBatchResponse is what the platform took.
type LogBatchResponse struct {
	Accepted int `json:"accepted"`
	Filtered int `json:"filtered"`
}

// ReleaseResponse is `GET /v1/release`'s answer (#2382): the line the agents
// gateway is deployed from and the companion release it names for its
// machines. Unauthenticated — it carries nothing but those two.
type ReleaseResponse struct {
	// Channel is `develop`, `uac` or `main`.
	Channel string `json:"channel"`
	// Version is `x.y.z[-pre]`, or null when the gateway names none — the
	// machine then takes the latest release.
	Version *string `json:"version"`
}
