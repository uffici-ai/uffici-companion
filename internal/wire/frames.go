// Package wire decodes the local wire's frames and validates every one of them
// against the shipped JSON Schemas.
//
// 🔴 VALIDATION IS NOT OPTIONAL AND NEVER SKIPPED. The plugin is a third party's
// process on the customer's machine, translating a harness we do not control; a
// frame that does not match the contract is a protocol error, and the connection
// closes with a reason from a closed vocabulary. Accepting a frame "that looked
// close enough" is how a wire contract stops being one.
//
// The types here are hand-written rather than generated, and
// `frames_parity_test.go` is what makes that safe: it reflects over every struct
// and compares its JSON field set against the schema's properties, in both
// directions, so a field added to a schema without a struct field (or the
// reverse) fails. A generator was weighed and declined — `oneOf` and the
// discriminated frame union generate poorly in Go, and a code generator is a
// second toolchain dependency inside a child whose whole purpose is to need
// none (the platform's rule for this tree). The parity test is the guarantee either
// approach would have rested on.
package wire

import "encoding/json"

// Version is the only protocol version this decoder accepts.
const Version = 1

// TextMaxChars bounds `post.text` and every `Entry.text`.
//
// The platform refuses a whole conversations-plane body over 4 MiB
// (`MAX_CHAT_BODY_BYTES`) and caps no single message below that, so this is the
// wire's own bound rather than a mirror of one: 128 Ki characters is at most
// 512 KiB of UTF-8 in the worst case, which leaves the envelope, the ids and
// several posts comfortably inside the platform's ceiling.
const TextMaxChars = 131_072

// PostsPerRequestMax bounds how many `post` frames one request may carry.
//
// A turn that wants more than this is a runaway, not a long answer; the
// companion closes the request rather than forwarding an unbounded stream to
// the platform.
const PostsPerRequestMax = 64

// FrameMaxBytes bounds one text frame on the socket.
const FrameMaxBytes = 1 << 20

// MaxCallPayloadBytes bounds one `call` or `result` frame, encoded (#2028).
//
// JSON Schema cannot express a byte ceiling, so the decoder is the enforcement
// and a golden pins it. It sits inside FrameMaxBytes on purpose: over THIS
// bound a call is answered `malformed_payload` with its own callId and the
// connection stays open — only past the frame bound does it close.
const MaxCallPayloadBytes = 262_144

// Direction says who RECEIVED a frame, which is the only thing that tells the
// two `hello` shapes apart.
type Direction string

const (
	// FromPlugin is a frame the companion received.
	FromPlugin Direction = "from_plugin"
	// FromCompanion is a frame the plugin received.
	FromCompanion Direction = "from_companion"
)

// CloseReason is the closed vocabulary a protocol error closes the connection
// with. There is no error FRAME: a violated contract ends the connection.
type CloseReason string

const (
	// ReasonUnsupportedVersion — the frame's `v` is absent or not 1.
	ReasonUnsupportedVersion CloseReason = "unsupported_version"
	// ReasonHelloRequired — a frame arrived before `hello`.
	ReasonHelloRequired CloseReason = "hello_required"
	// ReasonUnauthorized — the token was absent or did not match.
	ReasonUnauthorized CloseReason = "unauthorized"
	// ReasonAlreadyAttached — a second plugin connection for one companion.
	ReasonAlreadyAttached CloseReason = "already_attached"
	// ReasonUnknownRequest — a frame naming a request that is not open.
	ReasonUnknownRequest CloseReason = "unknown_request"
	// ReasonWrongConnection — a frame for a request delivered on another connection.
	ReasonWrongConnection CloseReason = "wrong_connection"
	// ReasonMalformedFrame — the frame does not match its schema.
	ReasonMalformedFrame CloseReason = "malformed_frame"
	// ReasonShuttingDown — the companion is stopping.
	ReasonShuttingDown CloseReason = "shutting_down"
)

// CloseReasons lists the vocabulary in the order `docs/local-wire.md` states it.
//
// 🔴 Only the first and the seventh are reachable from a single frame; the rest
// are state-dependent and belong to the server that holds the connection
// (#2011). The doc says which layer produces each, and the invalid goldens
// cover exactly the two this package can earn.
func CloseReasons() []CloseReason {
	return []CloseReason{
		ReasonUnsupportedVersion,
		ReasonHelloRequired,
		ReasonUnauthorized,
		ReasonAlreadyAttached,
		ReasonUnknownRequest,
		ReasonWrongConnection,
		ReasonMalformedFrame,
		ReasonShuttingDown,
	}
}

// FrameTypes lists every frame `type` value.
func FrameTypes() []string {
	return []string{"hello", "ready", "brief", "ack", "opener", "post", "done", "cancel", "release", "call", "result"}
}

// CallErrorClass is a `result` error's class: transport concerns only, a closed
// vocabulary (#2028). The control plane's own typed error rides inside
// `error.upstream`, so a new platform error never needs a wire change.
type CallErrorClass string

const (
	// ClassUnknownOp — the companion serves no such op. An answer, never a close.
	ClassUnknownOp CallErrorClass = "unknown_op"
	// ClassNotPermitted — the op exists and this call may not make it.
	ClassNotPermitted CallErrorClass = "not_permitted"
	// ClassTimedOut — the call's deadlineMs passed first.
	ClassTimedOut CallErrorClass = "timed_out"
	// ClassUpstreamFailed — the platform refused or failed, or its connection was lost.
	ClassUpstreamFailed CallErrorClass = "upstream_failed"
	// ClassMalformedPayload — the frame broke MaxCallPayloadBytes, or the op's input did not parse.
	ClassMalformedPayload CallErrorClass = "malformed_payload"
)

// CallErrorClasses lists the vocabulary in the schema's order.
func CallErrorClasses() []CallErrorClass {
	return []CallErrorClass{ClassUnknownOp, ClassNotPermitted, ClassTimedOut, ClassUpstreamFailed, ClassMalformedPayload}
}

// CallDisposition is what an invalid call-shaped frame earns INSTEAD of a close
// — the prefix of its invalid golden's file name, beside the close reasons.
type CallDisposition string

const (
	// DispositionMalformedPayload — answered with a malformed_payload result.
	DispositionMalformedPayload CallDisposition = "malformed_payload"
	// DispositionUnknownCall — a result naming no call in flight; the plugin
	// discards it. Stateful: it is schema-valid, and only the plugin can see it.
	DispositionUnknownCall CallDisposition = "unknown_call"
)

// CallDispositions lists both.
func CallDispositions() []CallDisposition {
	return []CallDisposition{DispositionMalformedPayload, DispositionUnknownCall}
}

// Envelope is the part every frame carries.
type Envelope struct {
	V    int    `json:"v"`
	Type string `json:"type"`
}

// Author is who wrote an entry.
type Author struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Label  string `json:"label"`
	Handle string `json:"handle,omitempty"`
	Self   bool   `json:"self"`
}

// Attachment is a file carried by an entry, by reference.
type Attachment struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Ref  string `json:"ref"`
	// LocalPath is where the companion prefetched the file, relative to the
	// workspace (#2673) — only when the plugin's hello asked for prefetch.
	LocalPath string `json:"localPath,omitempty"`
	// PrefetchError says why an asked-for prefetch did not happen.
	PrefetchError *PrefetchError `json:"prefetchError,omitempty"`
}

// PrefetchError is an attachment the companion was asked to prefetch and did
// not: a class, never a sentence — the plugin can still download it.
type PrefetchError struct {
	Class string `json:"class"`
}

// Entry is one transcript entry, mapped one to one from the platform's read.
type Entry struct {
	ID          string       `json:"id"`
	Cursor      string       `json:"cursor"`
	At          string       `json:"at"`
	Author      Author       `json:"author"`
	Kind        string       `json:"kind"`
	Text        string       `json:"text"`
	Attachments []Attachment `json:"attachments,omitempty"`
}

// PluginHello is the plugin's opening frame.
//
// 🔴 `Token` is SECRET-CLASS. It must never reach a log line, an error string
// or a whole-frame dump, on either side. `String()` below exists so that a
// careless `%v` cannot print it.
type PluginHello struct {
	Envelope
	Harness             string `json:"harness"`
	HarnessVersion      string `json:"harnessVersion"`
	PluginVersion       string `json:"pluginVersion"`
	Token               string `json:"token"`
	ContextBudgetTokens int    `json:"contextBudgetTokens"`
	// ProcessID is minted once per harness process (#2234): the same id on a
	// reconnect is the same process, a different one is a restarted harness.
	// Optional — an older plugin omits it. Not a secret.
	ProcessID string `json:"processId,omitempty"`
	// Drift is what in the harness's own configuration reaches past Uffici
	// (#2406), as the plugin read it at this hello. Optional — an older plugin
	// omits it, and `uffici doctor` then says the plugin did not report it.
	// Never a refusal: nothing here closes a connection or holds a ring.
	Drift *Drift `json:"drift,omitempty"`
	// HarnessSupport is the harness versions this plugin release supports and
	// its verdict on HarnessVersion against them (#2414). Optional — an older
	// plugin omits it. Never a refusal: the companion never parses a harness
	// version, it only carries the plugin's word to `uffici doctor`.
	HarnessSupport *HarnessSupport `json:"harnessSupport,omitempty"`
	// Capabilities is what the plugin asks the companion to do for it (#2673):
	// the PLUGIN's choice, because a harness's needs are its plugin's to know.
	// Optional — an older plugin asks nothing.
	Capabilities *Capabilities `json:"capabilities,omitempty"`
}

// Capabilities is what a plugin asks of the companion beyond every v1 plugin.
type Capabilities struct {
	// PrefetchAttachments asks for a brief's attachments to be downloaded into
	// the workspace before the brief is handed over.
	PrefetchAttachments *PrefetchAttachments `json:"prefetchAttachments,omitempty"`
}

// PrefetchAttachments is the plugin's prefetch budget, in bytes per brief.
type PrefetchAttachments struct {
	MaxBytes int64 `json:"maxBytes"`
}

// HarnessSupport is a plugin's statement of the harness versions it supports
// (#2414): client-reported, like Drift.
type HarnessSupport struct {
	// Range is the supported versions in the plugin's own words.
	Range string `json:"range"`
	// Supported is the plugin's verdict on the harness's version; nil when the
	// plugin could not read that version.
	Supported *bool `json:"supported,omitempty"`
}

// Drift is a plugin's report of its harness's drift from the plugin policy
// (#1994 decision 13, #1958 decision 27): client-reported, names only.
//
// A kind the plugin could not read is nil; an empty list means it read the
// source and found nothing.
type Drift struct {
	// MCPServers names the MCP servers the harness has configured and enabled.
	MCPServers []string `json:"mcpServers"`
	// Channels names the harness's other live channels, Uffici's own excluded.
	Channels []string `json:"channels"`
	// LocalMemory names the settings that keep a memory of the harness's own.
	LocalMemory []string `json:"localMemory"`
}

// String redacts the token so the struct is safe to format.
func (h PluginHello) String() string {
	return "PluginHello{harness=" + h.Harness + " harnessVersion=" + h.HarnessVersion +
		" pluginVersion=" + h.PluginVersion + " processId=" + h.ProcessID + " token=<redacted> }"
}

// CompanionHello is the companion's answer.
type CompanionHello struct {
	Envelope
	CompanionVersion string `json:"companionVersion"`
	AgentSlug        string `json:"agentSlug,omitempty"`
	OrgSlug          string `json:"orgSlug,omitempty"`
	// Features is what this companion can carry beyond every v1 companion
	// (#2232): `question`, `redelivery` and `tools`. Optional, so an older plugin reads nothing new.
	Features []string `json:"features,omitempty"`
	// Workspace is the workspace's absolute path as the companion sees it
	// (#2673): every `localPath` on the wire is relative to it. Sent beside the
	// `files` feature.
	Workspace string `json:"workspace,omitempty"`
}

// FeatureQuestion says a post may carry a question (#2232).
const FeatureQuestion = "question"

// FeatureRedelivery says a restarted harness process is handed again every request
// its predecessor had accepted (#2234). A plugin whose harness also resumes an
// interrupted turn on its own declines that resume, or the person is answered twice
// (#2273).
const FeatureRedelivery = "redelivery"

// FeatureTools says the plugin may call `tools.list` and `tools.call`, and a post
// may carry `citations` and `activity` (#2349).
const FeatureTools = "tools"

// FeatureToolsRerun says the plugin may call `tools.rerun` (#2454): re-run a call
// the platform recorded, from its `tool_call` URN. Announced only beside `tools`,
// so a plugin told so never meets `unknown_op` mid-turn.
const FeatureToolsRerun = "rerun"

// FeatureMemory says the plugin may call the four `memory.*` ops (#2385): recall,
// remember, forget and unfold. Announced only when all four are served, so a plugin
// told so never meets `unknown_op` mid-turn — and an older companion's silence tells
// the plugin to render "memory unavailable" instead of an empty recall.
const FeatureMemory = "memory"

// FeatureFiles says the plugin may call `files.download` and `files.upload`
// (#2673), and that the companion's hello names its `workspace`. Announced only
// when both are served.
const FeatureFiles = "files"

// Cause is the recorded human cause a request answers.
type Cause struct {
	EntryID  string `json:"entryId"`
	AuthorID string `json:"authorId"`
}

// Brief opens one request on one conversation.
type Brief struct {
	Envelope
	RequestID      string  `json:"requestId"`
	ConversationID string  `json:"conversationId"`
	Reason         string  `json:"reason"`
	Cause          Cause   `json:"cause"`
	Deadline       string  `json:"deadline"`
	SinceCursor    *string `json:"sinceCursor"`
	ThroughCursor  string  `json:"throughCursor"`
	ResetSession   bool    `json:"resetSession"`
	Delta          []Entry `json:"delta"`
	// Context is what the agent is told before the conversation (#2331), as
	// the platform composed it. Optional: an older companion sends none, and
	// the plugin then keeps its static hint.
	Context *Context `json:"context,omitempty"`
	// ContextChanged names the context levels a person changed since the agent
	// last answered here (2026-10-12). Optional: absent when nothing changed and
	// from an older companion; the plugin then says nothing.
	ContextChanged []ContextChange `json:"contextChanged,omitempty"`
}

// ContextChange is one context level that changed since the agent's last answer.
type ContextChange struct {
	Name  string `json:"name"`
	Scope string `json:"scope"`
}

// Context is the platform's composed context: the identity block first, then
// the pinned levels. The companion forwards it one to one.
type Context struct {
	Blocks  []ContextBlock   `json:"blocks"`
	Tokens  int              `json:"tokens"`
	Bounded bool             `json:"bounded"`
	Missing []ContextMissing `json:"missing,omitempty"`
}

// ContextBlock is one level; Depth is absent on the identity block.
type ContextBlock struct {
	Name   string `json:"name"`
	Scope  string `json:"scope"`
	Depth  *int   `json:"depth,omitempty"`
	Tokens int    `json:"tokens"`
	Text   string `json:"text"`
}

// ContextMissing is a part the brief should have had, with the sentence the
// agent relays — rendered by the plugin, never dropped.
type ContextMissing struct {
	Part        string `json:"part"`
	Class       string `json:"class"`
	Instruction string `json:"instruction"`
}

// Ack names the outcome and the harness's session state.
type Ack struct {
	Envelope
	RequestID string `json:"requestId"`
	Outcome   string `json:"outcome"`
	Session   string `json:"session"`
	Reason    string `json:"reason,omitempty"`
}

// Ready is the plugin saying its HARNESS can answer — which is not the same as
// the plugin being connected.
//
// 🔴 The gap between the two is real, and it is where the first turn of every
// run was being lost: a harness connects its platform adapters while it is
// still starting (Hermes does so "before the gateway reaches running"), and an
// inbound message arriving before its own message handler is installed is
// discarded without a trace. `hello` means "I am here"; this means "send me
// work". The companion HOLDS every ring until it arrives.
type Ready struct {
	Envelope
	// HarnessVersion is often unknown at hello time, because the harness is
	// still starting when the plugin attaches. It is known by now.
	HarnessVersion string `json:"harnessVersion,omitempty"`
}

// Opener carries the verbatim recent tail, after a fresh ack.
type Opener struct {
	Envelope
	RequestID string  `json:"requestId"`
	Entries   []Entry `json:"entries"`
}

// Post is one outbound message; every post means "more coming".
type Post struct {
	Envelope
	RequestID string `json:"requestId"`
	Text      string `json:"text"`
	// Question is present when the harness is ASKING (#2232): the text is the
	// question, and it reaches the person as the question card.
	Question *Question `json:"question,omitempty"`
	// Citations are the `callUrn`s of the agent's own tool calls this answer
	// rests on; sent only to a companion whose hello lists `tools`.
	Citations []string `json:"citations,omitempty"`
	// Activity is what the agent did with its harness's own tools, by name and a
	// summary — never arguments or results.
	Activity []Activity `json:"activity,omitempty"`
	// Attachments are files this post carries, as `files.upload` answered them
	// (#2673); sent only to a companion whose hello lists `files`.
	Attachments []PostAttachment `json:"attachments,omitempty"`
}

// PostAttachment is an uploaded file a post carries, by reference.
type PostAttachment struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Ref  string `json:"ref"`
}

// Activity is one harness-tool use a post reports.
type Activity struct {
	Tool    string `json:"tool"`
	Summary string `json:"summary"`
}

// Question is what a harness's question offers.
type Question struct {
	ID          string   `json:"id"`
	Choices     []string `json:"choices"`
	MultiSelect bool     `json:"multiSelect"`
}

// Done closes a request.
type Done struct {
	Envelope
	RequestID  string `json:"requestId"`
	Outcome    string `json:"outcome"`
	ErrorClass string `json:"errorClass,omitempty"`
}

// Release tells the plugin it may discard what it holds for one conversation
// (#2356): `pass_ended`, `archived` or `deleted`. It withdraws no request.
type Release struct {
	Envelope
	ConversationID string `json:"conversationId"`
	Reason         string `json:"reason"`
}

// The release reasons, the platform's words carried through verbatim.
const (
	ReleasePassEnded = "pass_ended"
	ReleaseArchived  = "archived"
	ReleaseDeleted   = "deleted"
)

// Cancel withdraws an open request.
type Cancel struct {
	Envelope
	RequestID string `json:"requestId"`
	Reason    string `json:"reason"`
	// Error is present exactly when Reason is `failed` (#2077): the platform's
	// class and the instruction the agent relays.
	Error *CancelError `json:"error,omitempty"`
}

// CancelError is decision 32's two halves, relayed on a `failed` cancel.
type CancelError struct {
	Class       string `json:"class"`
	Instruction string `json:"instruction"`
}

// Call is one platform operation, plugin → companion (#2028).
//
// 🔴 `Payload` is transcript content or memory rows. It never reaches a log
// line on either side: log Op, CallID, byte counts and the error class.
type Call struct {
	Envelope
	CallID     string          `json:"callId"`
	Op         string          `json:"op"`
	DeadlineMs int             `json:"deadlineMs"`
	Payload    json.RawMessage `json:"payload,omitempty"`
}

// Result is the one answer to a call, companion → plugin: exactly one of
// Payload or Error.
type Result struct {
	Envelope
	CallID  string          `json:"callId"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Error   *ResultError    `json:"error,omitempty"`
}

// ResultError is a failed call's class, the sentence the agent relays, and the
// control plane's own error when there is one.
type ResultError struct {
	Class       string          `json:"class"`
	Instruction string          `json:"instruction"`
	Upstream    json.RawMessage `json:"upstream,omitempty"`
}
