package gatewaytest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/infra/fixture"
	"github.com/uffici-ai/uffici-companion/internal/protocol"
)

// MemoryCall is one memory op the double ADMITTED (#2385): the request was rung,
// in the ticket's conversation and still open.
type MemoryCall struct {
	Op             string
	RequestID      string
	ConversationID string
}

type doubleFact struct {
	urn       string
	text      string
	forgotten bool
}

// memory serves `POST /v1/memory/{recall,remember,forget,unfold}` (#2385) with the
// gateway's rules — a request this double rang, in the ticket's conversation, and
// still open; anything else is `not_found` — over a small working memory per
// conversation: remember keeps a fact, recall answers the conversation's facts that
// share a word of four or more letters with the query, newest first. Every request
// is checked against the protocol, and every answer too.
func (d *Double) memory(w http.ResponseWriter, r *http.Request) {
	op := strings.TrimPrefix(r.URL.Path, "/v1/memory/")
	if answer, ok := d.next("memory"); ok {
		writeInjected(w, answer)
		return
	}
	conversationID, ok := d.convTicket(w, r, "")
	if !ok {
		return
	}
	raw := readBody(r)
	if err := protocol.Validate("memory-"+op+"-request", raw); err != nil {
		d.refuse("memory: " + err.Error())
		fail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	var envelope struct {
		RequestID string `json:"requestId"`
		Query     string `json:"query"`
		Text      string `json:"text"`
		URN       string `json:"urn"`
	}
	_ = json.Unmarshal(raw, &envelope)
	d.mu.Lock()
	defer d.mu.Unlock()
	request, rung := d.requests[envelope.RequestID]
	if !rung || request.conversationID != conversationID || d.closedReqs[envelope.RequestID] {
		fail(w, http.StatusNotFound, "not_found")
		return
	}
	d.memoryCalls = append(d.memoryCalls, MemoryCall{Op: op, RequestID: envelope.RequestID, ConversationID: conversationID})
	if d.facts == nil {
		d.facts = map[string][]*doubleFact{}
	}
	var answer any
	switch op {
	case "remember":
		f := &doubleFact{urn: protocol.FactURN(fmt.Sprintf("00000000-0000-4000-8000-%012d", len(d.memoryCalls))), text: envelope.Text}
		d.facts[conversationID] = append(d.facts[conversationID], f)
		answer = protocol.MemoryRememberResponse{URN: f.urn}
	case "recall":
		hits := []protocol.MemoryHit{}
		facts := d.facts[conversationID]
		for i := len(facts) - 1; i >= 0; i-- {
			if f := facts[i]; !f.forgotten && fixture.SharesAWord(envelope.Query, f.text) {
				hits = append(hits, protocol.MemoryHit{
					URN: f.urn, Level: "conversation", Text: f.text,
					At: time.Now().UTC().Format(time.RFC3339Nano), Source: protocol.MemorySource{Kind: "remembered"},
				})
			}
		}
		answer = protocol.MemoryRecallResponse{Hits: hits, Degraded: false}
	case "forget":
		for _, f := range d.facts[conversationID] {
			if f.urn == envelope.URN && !f.forgotten {
				f.forgotten = true
				answer = protocol.MemoryForgetResponse{URN: f.urn, SupersededBy: protocol.RequestURN(envelope.RequestID)}
			}
		}
		if answer == nil {
			fail(w, http.StatusConflict, "forget_refused")
			return
		}
	case "unfold":
		answer = protocol.MemoryUnfoldResponse{URN: envelope.URN, Rows: []protocol.MemoryUnfoldRow{}}
	}
	encoded := mustJSON(answer)
	if err := protocol.Validate("memory-"+op+"-response", encoded); err != nil {
		panic("gatewaytest: a memory answer is off-contract: " + err.Error())
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(encoded)
}

// MemoryCalls returns every memory op the double admitted.
func (d *Double) MemoryCalls() []MemoryCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]MemoryCall(nil), d.memoryCalls...)
}
