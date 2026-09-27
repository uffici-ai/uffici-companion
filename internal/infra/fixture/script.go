// Package fixture is a scripted AgentsHost: the platform, played from a file.
//
// 🔴 It exists so the local surface and the request state machine can be built
// and PROVEN before a real gateway exists — and so wave 1b's real host slots in
// behind the same interface without the surface changing a line. The script
// format is deliberately the one the smoke's fixture plugin will also play, so
// there is one scripted-host format in the product rather than two that drift.
//
// The fixture is a platform stand-in, not a convenience: it holds a transcript
// store with monotonic cursors and reads the delta OUT of it, from the
// conversation's committed offset to the through cursor. Nothing is canned. A
// canned delta would make the one piece of logic worth testing — which entries
// a brief carries, and when the offset moves — untestable.
package fixture

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/uffici-ai/uffici-companion/internal/schemas"

	contract "github.com/uffici-ai/uffici-companion/fixture"
)

// ScriptVersion is the only script version this build plays.
const ScriptVersion = 1

// Script is a whole scripted run.
type Script struct {
	V             int                      `json:"v"`
	Conversations map[string]*Conversation `json:"conversations"`
	Steps         []Step                   `json:"steps"`
	Assert        Assertions               `json:"assert"`
}

// Conversation is one named conversation's store.
type Conversation struct {
	// ID is the conversation id the wire carries.
	ID string `json:"id"`
	// Offset is the companion's agent's COMMITTED position: the exclusive lower
	// bound of the next delta.
	//
	// 🔴 It advances only when a request closes as answered or empty
	// (decision 40). Cancelled, superseded, expired and errored leave it, so the
	// next brief covers the union — entries nobody answered stay in range.
	Offset string `json:"offset"`
	// Entries is the transcript, in cursor order. Already bounded: the platform
	// bounds, the fixture serves.
	Entries []ScriptEntry `json:"entries"`
}

// ScriptEntry is one transcript entry as the script writes it.
type ScriptEntry struct {
	ID          string             `json:"id"`
	Cursor      string             `json:"cursor"`
	At          string             `json:"at"`
	Author      ScriptAuthor       `json:"author"`
	Kind        string             `json:"kind"`
	Text        string             `json:"text"`
	Attachments []ScriptAttachment `json:"attachments,omitempty"`
}

// ScriptAuthor is who wrote a scripted entry.
type ScriptAuthor struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Label  string `json:"label"`
	Handle string `json:"handle,omitempty"`
	Self   bool   `json:"self"`
}

// ScriptAttachment is a file an entry carries, by reference.
type ScriptAttachment struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Ref  string `json:"ref"`
}

// Step is one timeline entry. `Step` names which kind; the rest are that kind's
// fields, and a field belonging to another kind is simply absent.
type Step struct {
	Step string `json:"step"`

	// ring
	Conversation  string `json:"conversation,omitempty"`
	Reason        string `json:"reason,omitempty"`
	CauseEntry    string `json:"causeEntry,omitempty"`
	DeadlineMs    int    `json:"deadlineMs,omitempty"`
	ThroughCursor string `json:"throughCursor,omitempty"`
	ResetSession  bool   `json:"resetSession,omitempty"`

	// appendEntries
	Entries []ScriptEntry `json:"entries,omitempty"`

	// expectAck
	Outcome string `json:"outcome,omitempty"`
	Session string `json:"session,omitempty"`

	// expectPost
	Matcher string `json:"matcher,omitempty"`
	Value   string `json:"value,omitempty"`

	// expectOffset
	Cursor string `json:"cursor,omitempty"`

	// injectCancel
	AfterFirstPost bool `json:"afterFirstPost,omitempty"`
	AfterMs        int  `json:"afterMs,omitempty"`

	// Note is prose for the reader; the runner ignores it.
	Note string `json:"note,omitempty"`

	// timing, shared by several kinds
	WithinMs int `json:"withinMs,omitempty"`
	ForMs    int `json:"forMs,omitempty"`
	Ms       int `json:"ms,omitempty"`
}

// Assertions is the terminal tally: what must have happened by the end.
//
// 🔴 Counts, not a transcript. A run that asserted only "no expectation failed"
// would pass over a script whose steps never fired at all, which is why
// `RingsDelivered` has a floor in Validate and why `--once` refuses a script
// that delivered nothing.
type Assertions struct {
	RingsDelivered int            `json:"ringsDelivered"`
	Acks           map[string]int `json:"acks,omitempty"`
	Sessions       map[string]int `json:"sessions,omitempty"`
	OpenersSent    int            `json:"openersSent"`
	Posts          int            `json:"posts"`
	Closes         map[string]int `json:"closes,omitempty"`
	Cancels        map[string]int `json:"cancels,omitempty"`
	// 🔴 There is deliberately no `unmetExpectations`. It existed, pinned to 0
	// by the schema and read by nothing — and wiring it into the tally produced
	// a SECOND note for every miss, restating the list of misses that is already
	// the run's verdict and its exit code. A declared field nothing consults
	// reads as coverage, so it is gone from here and from the schema rather than
	// given a job it duplicates.
	// Note is prose for the reader; the runner ignores it.
	Note string `json:"note,omitempty"`
}

// Known step kinds. A script naming anything else is refused at load, not
// ignored: a typo in a step name would otherwise silently remove an assertion
// and leave the run green.
var knownSteps = map[string]bool{
	"ring": true, "appendEntries": true,
	"expectAck": true, "expectOpener": true, "expectNoOpener": true,
	"expectPost": true, "expectNoPost": true, "expectClose": true,
	"expectOffset": true, "injectCancel": true,
	"disconnectPlugin": true, "expectRedelivery": true,
	"secondConnection": true, "wait": true,
}

var knownMatchers = map[string]bool{"nonEmpty": true, "contains": true, "matches": true}

// Load reads and validates a script.
func Load(r io.Reader) (*Script, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("fixture: reading the script: %w", err)
	}

	// 🔴 The SCHEMA first, then Validate. The schema says what shape a script
	// has — and a Go decoder alone would not: an unknown or misspelled field
	// decodes to the zero value in silence, so `withinMs` typed `withinMS` would
	// turn every timed expectation into the default and pass. Validate then
	// checks what a schema cannot: that a ring names a conversation that exists
	// and a cause entry that is in it.
	if err := validateAgainstSchema(raw); err != nil {
		return nil, err
	}

	var script Script
	if err := json.Unmarshal(raw, &script); err != nil {
		return nil, fmt.Errorf("fixture: parsing the script: %w", err)
	}
	if err := script.Validate(); err != nil {
		return nil, err
	}
	// 🔴 The floor belongs to a script somebody WROTE, which is why it is here
	// and not in Validate: the host is also built directly from a Script in unit
	// tests that drive it without a timeline, and those legitimately assert
	// nothing about rings. `--once` refuses a run that delivered none, but that
	// verdict arrives after the script has played — an authored tally expecting
	// zero is one nobody meant, and catching it at load costs nothing
	// (defect-ledger class 1: green is the expensive failure).
	if script.Assert.RingsDelivered < 1 {
		return nil, fmt.Errorf("fixture: the script's assert block expects %d ring(s) delivered; "+
			"a run that delivers none asserts nothing, so the floor is 1", script.Assert.RingsDelivered)
	}
	return &script, nil
}

// compiledSchema is the script schema, compiled once.
var (
	schemaOnce sync.Once
	schemaDoc  *jsonschema.Schema
	schemaErr  error
)

func validateAgainstSchema(raw []byte) error {
	schemaOnce.Do(func() {
		compiler := jsonschema.NewCompiler()
		const id = "https://uffici.ai/companion/fixture/v1/script.schema.json"
		if err := schemas.AddResource(compiler, id, contract.Schema()); err != nil {
			schemaErr = fmt.Errorf("fixture: the embedded script schema did not load: %w", err)
			return
		}
		schemaDoc, schemaErr = compiler.Compile(id)
		if schemaErr != nil {
			schemaErr = fmt.Errorf("fixture: the embedded script schema did not compile: %w", schemaErr)
		}
	})
	if schemaErr != nil {
		return schemaErr
	}

	instance, err := schemas.Parse(raw)
	if err != nil {
		return fmt.Errorf("fixture: parsing the script: %w", err)
	}
	if err := schemaDoc.Validate(instance); err != nil {
		if locations := schemas.SafeLocations(err); len(locations) > 0 {
			return fmt.Errorf("fixture: the script does not match fixture/schema.json at %s",
				strings.Join(locations, ", "))
		}
		// Unreachable today — jsonschema's Validate always returns a
		// *ValidationError — and deliberately NOT `%w`: this is the one path that
		// bypasses leafLocations, and some of the library's error kinds render the
		// offending VALUE, which here is transcript text.
		return fmt.Errorf("fixture: the script does not match fixture/schema.json")
	}
	return nil
}

// Validate refuses a script this build cannot play truthfully.
//
// 🔴 Refuses rather than warns. Every check here is a way a script could run to
// completion while asserting less than its author believed — an unknown step
// name, a ring naming a conversation that does not exist, a cause entry that is
// not in the store. Each would produce a green run and a false conclusion.
func (s *Script) Validate() error {
	if s.V != ScriptVersion {
		return fmt.Errorf("fixture: script version %d, this build plays v%d", s.V, ScriptVersion)
	}
	if len(s.Conversations) == 0 {
		return fmt.Errorf("fixture: the script declares no conversations, so no ring could name one")
	}
	if len(s.Steps) == 0 {
		return fmt.Errorf("fixture: the script has no steps, so it would assert nothing and exit 0")
	}

	// 🔴 Entries a step APPENDS count as present for every later step. Without
	// this, a script that rings on a message it just appended is refused — which
	// is the most ordinary shape there is ("a message arrives, the agent is
	// woken by it"), and the refusal would push authors towards declaring
	// everything up front and never exercising `appendEntries` at all.
	appended := map[string]map[string]bool{}
	rings := 0
	for i, step := range s.Steps {
		if !knownSteps[step.Step] {
			return fmt.Errorf("fixture: step %d names an unknown kind %q — a typo here removes an assertion silently", i, step.Step)
		}
		switch step.Step {
		case "ring":
			rings++
			conversation, ok := s.Conversations[step.Conversation]
			if !ok {
				return fmt.Errorf("fixture: step %d rings conversation %q, which the script does not declare", i, step.Conversation)
			}
			if step.Reason == "" {
				return fmt.Errorf("fixture: step %d rings with no reason", i)
			}
			if step.CauseEntry != "" && !conversation.hasEntry(step.CauseEntry) &&
				!appended[step.Conversation][step.CauseEntry] {
				return fmt.Errorf("fixture: step %d names cause entry %q, which is neither in conversation %q nor appended by an earlier step", i, step.CauseEntry, step.Conversation)
			}
		case "appendEntries":
			if _, ok := s.Conversations[step.Conversation]; !ok {
				return fmt.Errorf("fixture: step %d appends to conversation %q, which the script does not declare", i, step.Conversation)
			}
			if len(step.Entries) == 0 {
				return fmt.Errorf("fixture: step %d appends no entries", i)
			}
			if appended[step.Conversation] == nil {
				appended[step.Conversation] = map[string]bool{}
			}
			for _, entry := range step.Entries {
				appended[step.Conversation][entry.ID] = true
			}
		case "expectPost":
			if step.Matcher != "" && !knownMatchers[step.Matcher] {
				return fmt.Errorf("fixture: step %d uses matcher %q; known matchers are nonEmpty, contains, matches", i, step.Matcher)
			}
			if (step.Matcher == "contains" || step.Matcher == "matches") && step.Value == "" {
				return fmt.Errorf("fixture: step %d uses matcher %q with no value, which matches everything", i, step.Matcher)
			}
		case "expectOffset":
			if _, ok := s.Conversations[step.Conversation]; !ok {
				return fmt.Errorf("fixture: step %d asserts the offset of conversation %q, which the script does not declare", i, step.Conversation)
			}
			if step.Cursor == "" {
				return fmt.Errorf("fixture: step %d asserts an empty offset", i)
			}
		}
	}

	// 🔴 The floor that makes `--once` mean something. A script with no ring
	// delivers nothing, meets every expectation vacuously and exits 0 — a green
	// run that proved the daemon can start. That is defect-ledger class 1.
	if rings == 0 {
		return fmt.Errorf("fixture: the script rings nothing, so a run could only ever prove the daemon starts")
	}

	for name, conversation := range s.Conversations {
		if conversation.ID == "" {
			return fmt.Errorf("fixture: conversation %q has no id", name)
		}
		if err := conversation.validateCursors(name); err != nil {
			return err
		}
	}
	return nil
}

func (c *Conversation) hasEntry(id string) bool {
	for _, entry := range c.Entries {
		if entry.ID == id {
			return true
		}
	}
	return false
}

// validateCursors refuses a store whose cursors are not strictly increasing.
//
// 🔴 The delta is a cursor RANGE, so unordered or duplicated cursors would make
// "everything since the offset" ambiguous — and the ambiguity would show up as
// a delta that is subtly wrong, not as an error.
func (c *Conversation) validateCursors(name string) error {
	seen := make(map[string]bool, len(c.Entries))
	previous := ""
	for i, entry := range c.Entries {
		if entry.Cursor == "" {
			return fmt.Errorf("fixture: conversation %q entry %d has no cursor", name, i)
		}
		if seen[entry.Cursor] {
			return fmt.Errorf("fixture: conversation %q repeats cursor %q", name, entry.Cursor)
		}
		if previous != "" && entry.Cursor <= previous {
			return fmt.Errorf("fixture: conversation %q has cursor %q after %q — cursors must strictly increase, or a delta range is ambiguous", name, entry.Cursor, previous)
		}
		seen[entry.Cursor] = true
		previous = entry.Cursor
	}
	return nil
}

// sortedConversationNames gives a stable order for logs and errors.
func (s *Script) sortedConversationNames() []string {
	names := make([]string, 0, len(s.Conversations))
	for name := range s.Conversations {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// parseAt reads a scripted timestamp, defaulting to the zero time.
func parseAt(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, value)
}
