package fixture

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/protocol"
)

// MemoryRecord is one memory op the scripted host served (#2385) — what a `--once`
// run reports and the end-to-end run asserts on. The text is the fixture's own.
type MemoryRecord struct {
	Op             contracts.MemoryOp
	RequestID      string
	ConversationID string
	At             time.Time
}

// fact is one remembered fact, kept per conversation.
type fact struct {
	urn       string
	text      string
	at        time.Time
	forgotten bool
}

// memoryUnavailable is the platform's answer while knowledge-api is down: a class
// and the memory family's sentence, like every host failure (decision 32).
type memoryUnavailable struct{}

func (memoryUnavailable) Error() string      { return "fixture: memory is unavailable" }
func (memoryUnavailable) ErrorClass() string { return "platform_unavailable" }
func (memoryUnavailable) ErrorInstruction() string {
	return "Memory was unavailable because of a system problem; tell the person you could not check what was remembered."
}

// memoryNotFound is the platform's answer to a URN this conversation cannot see.
type memoryNotFound struct{}

func (memoryNotFound) Error() string      { return "fixture: no such memory" }
func (memoryNotFound) ErrorClass() string { return "not_found" }
func (memoryNotFound) ErrorInstruction() string {
	return "That memory does not exist or cannot be seen from this conversation, or the request is no longer this agent's; do not retry it."
}

// Memory serves the agent's memory as a WORKING stand-in for knowledge-api (#2385):
// remember keeps a fact on the request's conversation; recall answers that
// conversation's facts newest first, matched by a shared word of four or more
// letters; forget marks one; unfold answers nothing earlier. A request no ring
// delivered is refused before anything is recorded, as the gateway host refuses it
// before anything is sent. The script's `memory.unavailable` makes every op the
// platform's `platform_unavailable`.
func (h *Host) Memory(_ context.Context, op contracts.MemoryOp, requestID string, body json.RawMessage) (json.RawMessage, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	conversationID, known := h.rung[requestID]
	if !known {
		return nil, contracts.ErrUnknownRequest
	}
	h.memoryCalls = append(h.memoryCalls, MemoryRecord{
		Op: op, RequestID: requestID, ConversationID: conversationID, At: time.Now(),
	})
	if h.memoryDownIn(conversationID) {
		return nil, memoryUnavailable{}
	}
	if h.facts == nil {
		h.facts = map[string][]*fact{}
	}
	switch op {
	case contracts.MemoryRemember:
		var in struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(body, &in); err != nil || in.Text == "" {
			return nil, fmt.Errorf("fixture: a remember needs its text")
		}
		f := &fact{urn: protocol.FactURN(newUUID()), text: in.Text, at: time.Now()}
		h.facts[conversationID] = append(h.facts[conversationID], f)
		return json.Marshal(map[string]string{"urn": f.urn})
	case contracts.MemoryRecall:
		var in struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
		}
		if err := json.Unmarshal(body, &in); err != nil {
			return nil, fmt.Errorf("fixture: a recall needs its query")
		}
		limit := in.Limit
		if limit <= 0 {
			limit = 8
		}
		hits := []map[string]any{}
		facts := h.facts[conversationID]
		for i := len(facts) - 1; i >= 0 && len(hits) < limit; i-- {
			f := facts[i]
			if f.forgotten || !SharesAWord(in.Query, f.text) {
				continue
			}
			hits = append(hits, map[string]any{
				"urn": f.urn, "level": "conversation", "distance": nil, "text": f.text,
				"at": f.at.UTC().Format(time.RFC3339Nano), "source": map[string]string{"kind": "remembered"},
			})
		}
		return json.Marshal(map[string]any{"hits": hits, "degraded": false})
	case contracts.MemoryForget:
		var in struct {
			URN string `json:"urn"`
		}
		if err := json.Unmarshal(body, &in); err != nil {
			return nil, fmt.Errorf("fixture: a forget needs its urn")
		}
		for _, f := range h.facts[conversationID] {
			if f.urn == in.URN && !f.forgotten {
				f.forgotten = true
				return json.Marshal(map[string]string{"urn": f.urn, "supersededBy": protocol.RequestURN(requestID)})
			}
		}
		return nil, memoryNotFound{}
	case contracts.MemoryUnfold:
		var in struct {
			URN string `json:"urn"`
		}
		if err := json.Unmarshal(body, &in); err != nil {
			return nil, fmt.Errorf("fixture: an unfold needs its urn")
		}
		return json.Marshal(map[string]any{"urn": in.URN, "supersededBy": nil, "rows": []any{}})
	}
	return nil, fmt.Errorf("fixture: no memory op %q", op)
}

// MemoryCalls returns every memory op served, in order.
func (h *Host) MemoryCalls() []MemoryRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]MemoryRecord(nil), h.memoryCalls...)
}

// SharesAWord is the stand-in search both scripted hosts use: whether query and
// text share a word of four or more letters or digits.
func SharesAWord(query, text string) bool {
	words := map[string]bool{}
	for _, w := range wordsOf(text) {
		words[w] = true
	}
	for _, w := range wordsOf(query) {
		if words[w] {
			return true
		}
	}
	return false
}

func wordsOf(text string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if utf8.RuneCountInString(w) >= 4 {
			out = append(out, w)
		}
	}
	return out
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	s := hex.EncodeToString(b[:])
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

// memoryDownIn is whether the script takes memory down for this conversation —
// everywhere, or in the conversations `unavailableIn` names.
func (h *Host) memoryDownIn(conversationID string) bool {
	scripted := h.script.Memory
	if scripted == nil {
		return false
	}
	if scripted.Unavailable {
		return true
	}
	for _, name := range scripted.UnavailableIn {
		if conversation, ok := h.script.Conversations[name]; ok && conversation.ID == conversationID {
			return true
		}
	}
	return false
}
