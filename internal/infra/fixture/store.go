package fixture

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
)

// store is the fixture's transcript: conversations, their entries in cursor
// order, and the companion's agent's committed offset in each.
//
// 🔴 Deltas are READ from here, never canned. The whole point of the fixture is
// to exercise the two questions that are easy to get subtly wrong — which
// entries a brief carries, and when the offset moves — and a canned delta
// answers neither.
type store struct {
	mu             sync.RWMutex
	byName         map[string]*conversationState
	byID           map[string]*conversationState
	readSinceHits  int
	readOpenerHits int
	// sinceBudgets is every context budget a delta read was given, in order.
	sinceBudgets []int
	// tools and results are the agent's catalog, the same in every
	// conversation (#2349).
	tools   []ScriptTool
	results map[string]ScriptResult
}

type conversationState struct {
	name    string
	id      string
	offset  string
	entries []contracts.Entry
	context *ScriptContext
}

func newStore(script *Script) (*store, error) {
	s := &store{
		byName: make(map[string]*conversationState, len(script.Conversations)),
		byID:   make(map[string]*conversationState, len(script.Conversations)),
		tools:  script.Tools, results: script.Results,
	}
	for _, name := range script.sortedConversationNames() {
		scripted := script.Conversations[name]
		entries := make([]contracts.Entry, 0, len(scripted.Entries))
		for i, entry := range scripted.Entries {
			converted, err := toEntry(entry)
			if err != nil {
				return nil, fmt.Errorf("fixture: conversation %q entry %d: %w", name, i, err)
			}
			entries = append(entries, converted)
		}
		state := &conversationState{
			name: name, id: scripted.ID, offset: scripted.Offset, entries: entries, context: scripted.Context,
		}
		s.byName[name] = state
		if _, clash := s.byID[scripted.ID]; clash {
			return nil, fmt.Errorf("fixture: two conversations share the id %q, so a brief could not name one", scripted.ID)
		}
		s.byID[scripted.ID] = state
	}
	return s, nil
}

func toEntry(scripted ScriptEntry) (contracts.Entry, error) {
	at, err := parseAt(scripted.At)
	if err != nil {
		return contracts.Entry{}, fmt.Errorf("timestamp %q: %w", scripted.At, err)
	}
	attachments := make([]contracts.Attachment, 0, len(scripted.Attachments))
	for _, attachment := range scripted.Attachments {
		attachments = append(attachments, contracts.Attachment{
			ID: attachment.ID, Name: attachment.Name, Ref: attachment.Ref,
		})
	}
	return contracts.Entry{
		ID:     scripted.ID,
		Cursor: scripted.Cursor,
		At:     at,
		Author: contracts.Author{
			Type:   scripted.Author.Type,
			ID:     scripted.Author.ID,
			Label:  scripted.Author.Label,
			Handle: scripted.Author.Handle,
			Self:   scripted.Author.Self,
		},
		Kind:        scripted.Kind,
		Text:        scripted.Text,
		Attachments: attachments,
	}, nil
}

func (s *store) conversationByID(id string) (*conversationState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.byID[id]
	if !ok {
		return nil, fmt.Errorf("fixture: no conversation with id %q", id)
	}
	return state, nil
}

func (s *store) conversationByName(name string) (*conversationState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.byName[name]
	if !ok {
		return nil, fmt.Errorf("fixture: no conversation named %q", name)
	}
	return state, nil
}

// appendEntries adds entries to a conversation, keeping cursors strictly
// increasing.
func (s *store) appendEntries(name string, scripted []ScriptEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.byName[name]
	if !ok {
		return fmt.Errorf("fixture: no conversation named %q", name)
	}
	for i, entry := range scripted {
		converted, err := toEntry(entry)
		if err != nil {
			return fmt.Errorf("fixture: appending to %q, entry %d: %w", name, i, err)
		}
		if len(state.entries) > 0 && converted.Cursor <= state.entries[len(state.entries)-1].Cursor {
			return fmt.Errorf("fixture: appending cursor %q to %q after %q — cursors must strictly increase", converted.Cursor, name, state.entries[len(state.entries)-1].Cursor)
		}
		state.entries = append(state.entries, converted)
	}
	return nil
}

// readSince returns (since, through], omitting the answering agent's own
// entries — the delta a brief carries.
//
// 🔴 `self` entries are omitted here and INCLUDED by readOpener. That asymmetry
// is the design: a delta tells the agent what it has not seen, an opener
// rebuilds the conversation as it reads, which needs its own turns in place.
func (s *store) readSince(conversationID, since, through string, contextBudgetTokens int) (contracts.Read, error) {
	state, err := s.conversationByID(conversationID)
	if err != nil {
		return contracts.Read{}, err
	}
	s.mu.Lock()
	s.readSinceHits++
	s.sinceBudgets = append(s.sinceBudgets, contextBudgetTokens)
	s.mu.Unlock()

	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]contracts.Entry, 0, len(state.entries))
	for _, entry := range state.entries {
		if since != "" && entry.Cursor <= since {
			continue
		}
		if through != "" && entry.Cursor > through {
			continue
		}
		if entry.Author.Self {
			continue
		}
		out = append(out, entry)
	}
	// The fixture never bounds: the platform bounds and the fixture serves what
	// the script gave it, so `bounded` is always false here — and the context is
	// served as written, never sized to the budget for the same reason.
	return contracts.Read{Entries: out, Context: toContext(state.context)}, nil
}

// toContext copies a scripted context into the contract's shape — a fresh
// value per read, so nothing downstream can edit the script.
func toContext(scripted *ScriptContext) *contracts.Context {
	if scripted == nil {
		return nil
	}
	out := &contracts.Context{Tokens: scripted.Tokens, Bounded: scripted.Bounded}
	for _, b := range scripted.Blocks {
		block := contracts.ContextBlock{Name: b.Name, Scope: b.Scope, Tokens: b.Tokens, Text: b.Text}
		if b.Depth != nil {
			depth := *b.Depth
			block.Depth = &depth
		}
		out.Blocks = append(out.Blocks, block)
	}
	for _, m := range scripted.Missing {
		out.Missing = append(out.Missing, contracts.ContextMissing{
			Part: m.Part, Class: m.Class, Instruction: m.Instruction,
		})
	}
	return out
}

// contextBudgets is every context budget the delta reads were given, in order.
func (s *store) contextBudgets() []int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]int(nil), s.sinceBudgets...)
}

// readOpener returns the recent tail through `through`, INCLUDING the agent's
// own entries.
//
// The declared budget is recorded, never used to trim — trimming is the
// platform's job, and a fixture that trimmed would be testing its own
// arithmetic instead of the companion's behaviour. `budgetTokens` reaches the
// log line as `budgetDeclared`, which is the counter wave 4 wants.
func (s *store) readOpener(conversationID, through string, budgetTokens int) ([]contracts.Entry, error) {
	state, err := s.conversationByID(conversationID)
	if err != nil {
		return nil, err
	}
	_ = budgetTokens

	s.mu.Lock()
	s.readOpenerHits++
	s.mu.Unlock()

	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]contracts.Entry, 0, len(state.entries))
	for _, entry := range state.entries {
		if through != "" && entry.Cursor > through {
			continue
		}
		out = append(out, entry)
	}
	return out, nil
}

// commitOffset advances a conversation's offset — and ONLY for an outcome that
// earns it.
//
// 🔴 This is the fixture's half of decision 40, and the reason it takes the
// outcome rather than a cursor: a caller that decided for itself whether to
// commit is a caller that can decide wrongly. Advancing on a cancelled or
// errored request would drop every entry in that range from every future brief,
// silently.
func (s *store) commitOffset(conversationID, through string, outcome contracts.Outcome) (bool, error) {
	state, err := s.conversationByID(conversationID)
	if err != nil {
		return false, err
	}
	if !outcome.AdvancesOffset() {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Never move backwards: a redelivered close must not rewind the offset.
	if through != "" && through > state.offset {
		state.offset = through
	}
	return true, nil
}

func (s *store) offsetOf(name string) (string, error) {
	state, err := s.conversationByName(name)
	if err != nil {
		return "", err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return state.offset, nil
}

// counts reports how many reads the host served, for the terminal tally.
func (s *store) counts() (readSince, readOpener int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.readSinceHits, s.readOpenerHits
}

// toolsFor is the agent's scripted catalog as one conversation reads it — the
// same in every conversation the script declares (#2349).
func (s *store) toolsFor(conversationID string) ([]contracts.Tool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.byID[conversationID]; !ok {
		return nil, fmt.Errorf("fixture: no conversation %q", conversationID)
	}
	out := make([]contracts.Tool, 0, len(s.tools))
	for _, tool := range s.tools {
		parameters := tool.Parameters
		if len(parameters) == 0 {
			parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, contracts.Tool{
			Name: tool.Name, Description: tool.Description, Parameters: parameters, Permission: tool.Permission,
		})
	}
	return out, nil
}

// toolResult is a listed tool's scripted answer — `{"ok":true}` and no URN when
// the script gave none — and whether the catalog lists the tool at all.
func (s *store) toolResult(conversationID, name string) (ScriptResult, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.byID[conversationID]; !ok {
		return ScriptResult{}, false, fmt.Errorf("fixture: no conversation %q", conversationID)
	}
	for _, tool := range s.tools {
		if tool.Name != name {
			continue
		}
		if result, scripted := s.results[name]; scripted {
			return result, true, nil
		}
		return ScriptResult{Result: json.RawMessage(`{"ok":true}`)}, true, nil
	}
	return ScriptResult{}, false, nil
}
