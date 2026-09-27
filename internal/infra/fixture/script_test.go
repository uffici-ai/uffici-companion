package fixture

import (
	"encoding/json"
	"strings"
	"testing"

	contract "github.com/uffici-ai/uffici-companion/fixture"
)

// 🔴 Every refusal here prevents the same thing: a script that RUNS TO
// COMPLETION while asserting less than its author believed. None of them would
// produce an error at run time — they produce a green run and a false
// conclusion, which is this repository's defect-ledger class 1.

func valid() *Script { return testScript() }

func TestLoadAcceptsAValidScript(t *testing.T) {
	script, err := Load(strings.NewReader(`{
      "v": 1,
      "conversations": {"c": {"id": "conv_1", "offset": "", "entries": [
        {"id":"e1","cursor":"c0001","at":"2026-09-17T14:01:00Z","kind":"message","text":"hi",
         "author":{"type":"member","id":"m1","label":"Ana","self":false}}
      ]}},
      "steps": [{"step":"ring","conversation":"c","reason":"mentioned","causeEntry":"e1","throughCursor":"c0001"}],
      "assert": {"ringsDelivered": 1}
    }`))
	if err != nil {
		t.Fatalf("a valid script was refused: %v", err)
	}
	if len(script.Steps) != 1 {
		t.Fatalf("parsed %d step(s), want 1", len(script.Steps))
	}
}

func TestValidateRefusesTheWaysAScriptCanAssertNothing(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*Script)
		want   string
	}{
		{"a version this build does not play", func(s *Script) { s.V = 2 }, "this build plays v1"},
		{"no conversations", func(s *Script) { s.Conversations = nil }, "no conversations"},
		{"no steps", func(s *Script) { s.Steps = nil }, "no steps"},
		{
			"a misspelt step kind",
			func(s *Script) { s.Steps[0].Step = "expectPostt" },
			"removes an assertion silently",
		},
		{
			// 🔴 The floor that gives `--once` its meaning: no ring means nothing
			// was delivered, every expectation holds vacuously, and the run exits 0
			// having proved only that the daemon starts.
			"no ring at all",
			func(s *Script) { s.Steps = []Step{{Step: "wait", Ms: 1}} },
			"could only ever prove the daemon starts",
		},
		{
			"a ring naming an undeclared conversation",
			func(s *Script) { s.Steps[0].Conversation = "nowhere" },
			"does not declare",
		},
		{
			"a ring with no reason",
			func(s *Script) { s.Steps[0].Reason = "" },
			"rings with no reason",
		},
		{
			"a cause entry that is not in the store",
			func(s *Script) { s.Steps[0].CauseEntry = "e99" },
			"nor appended by an earlier step",
		},
		{
			// A `contains` matcher with no value matches every post, so the
			// expectation passes whatever the agent said.
			"a contains matcher with no value",
			func(s *Script) {
				s.Steps = append(s.Steps, Step{Step: "expectPost", Matcher: "contains"})
			},
			"matches everything",
		},
		{
			"an unknown matcher",
			func(s *Script) {
				s.Steps = append(s.Steps, Step{Step: "expectPost", Matcher: "vaguely"})
			},
			"known matchers are",
		},
		{
			"an offset assertion with no cursor",
			func(s *Script) {
				s.Steps = append(s.Steps, Step{Step: "expectOffset", Conversation: "invoices"})
			},
			"asserts an empty offset",
		},
		{
			"a conversation with no id",
			func(s *Script) { s.Conversations["invoices"].ID = "" },
			"has no id",
		},
		{
			// An ambiguous range produces a delta that is subtly wrong rather than
			// an error anywhere.
			"cursors that do not strictly increase",
			func(s *Script) { s.Conversations["invoices"].Entries[2].Cursor = "c00015" },
			"strictly increase",
		},
		{
			"an appendEntries with nothing to append",
			func(s *Script) {
				s.Steps = append(s.Steps, Step{Step: "appendEntries", Conversation: "invoices"})
			},
			"appends no entries",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			script := valid()
			testCase.mutate(script)
			err := script.Validate()
			if err == nil {
				t.Fatalf("accepted; it must be refused with a message about %q", testCase.want)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("refused with %q, which does not mention %q", err.Error(), testCase.want)
			}
		})
	}
}

func TestValidateAcceptsTheScriptTheOtherTestsUse(t *testing.T) {
	// Non-vacuity for the table above: if the base script were invalid, every
	// case there would pass for the wrong reason.
	if err := valid().Validate(); err != nil {
		t.Fatalf("the base script is invalid, so every refusal test above proves nothing: %v", err)
	}
}

func TestTwoConversationsMayNotShareAnID(t *testing.T) {
	script := valid()
	script.Conversations["other"] = &Conversation{ID: "conv_1", Entries: nil}
	if _, err := New(script); err == nil {
		t.Fatal("two conversations shared an id — a brief could not name one of them")
	}
}

func TestLoadRefusesMalformedJSON(t *testing.T) {
	if _, err := Load(strings.NewReader("{not json")); err == nil {
		t.Fatal("malformed JSON was accepted")
	}
}

// validScript is the smallest script fixture/schema.json accepts.
const validScript = `{
  "v": 1,
  "conversations": {"c": {"id": "conv_1", "offset": "", "entries": [
    {"id":"e1","cursor":"c0001","at":"2026-09-17T14:01:00Z","kind":"message","text":"hi",
     "author":{"type":"member","id":"m1","label":"Ana","self":false}}
  ]}},
  "steps": [{"step":"ring","conversation":"c","reason":"mentioned","causeEntry":"e1","throughCursor":"c0001"}],
  "assert": {"ringsDelivered": 1}
}`

// 🔴 What the SCHEMA adds over the Go decoder, and the reason it exists: a
// misspelled field decodes to the zero value in silence. `withinMS` for
// `withinMs` would turn a three-second expectation into the default and the run
// would still be green — the script would simply assert something else than its
// author wrote.
func TestASchemaRefusesTheTyposADecoderWouldSwallow(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		script string
		want   string
	}{
		{
			"a misspelled field on a step",
			strings.Replace(validScript, `"throughCursor":"c0001"`, `"throughCursour":"c0001"`, 1),
			"/steps/0",
		},
		{
			"a step kind no build plays",
			strings.Replace(validScript, `"step":"ring"`, `"step":"rign"`, 1),
			"/steps/0/step",
		},
		{
			"a top-level key no build reads",
			strings.Replace(validScript, `"assert"`, `"asserts"`, 1),
			"at / (required)",
		},
		{
			"an entry with no author",
			strings.Replace(validScript, `"author":{"type":"member","id":"m1","label":"Ana","self":false}`, `"author":{}`, 1),
			"/conversations/c/entries/0/author",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := Load(strings.NewReader(testCase.script))
			if err == nil {
				t.Fatal("the script was accepted, so the schema is not gating anything")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("the refusal is %q, want it to name %q", err, testCase.want)
			}
		})
	}
}

// 🔴 A refusal names WHERE, never WHAT. A script's entries are transcript text,
// and an error string quoting the value at a failing location publishes that
// text into stderr and into whatever CI keeps.
func TestASchemaRefusalNamesTheLocationAndNotTheContent(t *testing.T) {
	const secret = "the Lisbon invoice is unpaid"
	script := strings.Replace(validScript, `"text":"hi"`, `"text":"`+secret+`","kindd":"message"`, 1)
	_, err := Load(strings.NewReader(script))
	if err == nil {
		t.Fatal("the script was accepted")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("the refusal quotes transcript content: %v", err)
	}
	if !strings.Contains(err.Error(), "/conversations/c/entries/0") {
		t.Fatalf("the refusal does not say where: %v", err)
	}
}

// 🔴 An authored script that expects no rings is one nobody meant. Caught at
// load, because the alternative — `--once`'s verdict — arrives after the run.
func TestAnAuthoredScriptMustExpectAtLeastOneRing(t *testing.T) {
	script := strings.Replace(validScript, `"ringsDelivered": 1`, `"ringsDelivered": 0`, 1)
	_, err := Load(strings.NewReader(script))
	if err == nil {
		t.Fatal("a script expecting zero rings was accepted; it would assert nothing and exit 0")
	}
	if !strings.Contains(err.Error(), "the floor is 1") {
		t.Fatalf("the refusal is %q", err)
	}
}

// 🔴 The schema's step vocabulary and the loader's are ONE list in two files.
// A step added to the Go map but not the schema is refused by the schema with a
// confusing message; one added to the schema but not the map loads and then
// reports "unknown step kind" at run time. Either way the script's author is
// told the wrong thing.
func TestTheSchemaAndTheLoaderKnowTheSameSteps(t *testing.T) {
	var schema struct {
		Defs struct {
			Step struct {
				Properties struct {
					Step struct {
						Enum []string `json:"enum"`
					} `json:"step"`
				} `json:"properties"`
			} `json:"step"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(contract.Schema(), &schema); err != nil {
		t.Fatalf("decoding the schema: %v", err)
	}
	inSchema := schema.Defs.Step.Properties.Step.Enum

	// Non-vacuity: an empty enum would satisfy every comparison below.
	if len(inSchema) == 0 {
		t.Fatal("the schema lists no step kinds, so this comparison looked at nothing")
	}
	if len(knownSteps) == 0 {
		t.Fatal("the loader knows no step kinds")
	}

	listed := map[string]bool{}
	for _, step := range inSchema {
		listed[step] = true
		if !knownSteps[step] {
			t.Fatalf("the schema allows step %q, which the loader refuses", step)
		}
	}
	for step := range knownSteps {
		if !listed[step] {
			t.Fatalf("the loader plays step %q, which the schema refuses", step)
		}
	}
}

// 🔴 One version, stated twice — once for the loader and once for the format's
// own description — and nothing but this makes them agree. A schema still
// saying `"v": 1` while the loader plays v2 accepts every v1 script and then
// refuses it with a version error, which reads as a corrupt file.
func TestTheSchemaAndTheLoaderAgreeOnTheVersion(t *testing.T) {
	var schema struct {
		Properties struct {
			V struct {
				Const *int `json:"const"`
			} `json:"v"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(contract.Schema(), &schema); err != nil {
		t.Fatalf("decoding the schema: %v", err)
	}
	if schema.Properties.V.Const == nil {
		t.Fatal("the schema pins no version, so this comparison looked at nothing")
	}
	if *schema.Properties.V.Const != ScriptVersion {
		t.Fatalf("the schema describes v%d and the loader plays v%d",
			*schema.Properties.V.Const, ScriptVersion)
	}
}
