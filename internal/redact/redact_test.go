package redact

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

type vector struct {
	Name       string `json:"name"`
	Input      string `json:"input"`
	Expected   string `json:"expected"`
	TruncateAt int    `json:"truncateAt"`
	// AttrsInput / AttrsExpected make an attributes vector: a line's decoded attrs
	// through Value, for the patterns scoped to attribute names.
	AttrsInput    map[string]any `json:"attrsInput"`
	AttrsExpected map[string]any `json:"attrsExpected"`
}

func loadVectors(t *testing.T) []vector {
	t.Helper()
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []vector
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	return vectors
}

// 🔴 The platform's topic transform runs these same vectors against its copy of
// patterns.json, so a vector is a contract on both implementations.
func TestEveryVector(t *testing.T) {
	vectors := loadVectors(t)
	if len(vectors) == 0 {
		t.Fatal("no vectors — the suite would pass over nothing")
	}
	for _, v := range vectors {
		t.Run(v.Name, func(t *testing.T) {
			if v.AttrsInput != nil {
				got, _ := Value(v.AttrsInput)
				if !reflect.DeepEqual(got, any(v.AttrsExpected)) {
					t.Fatalf("\n attrs: %v\n   got: %v\n  want: %v", v.AttrsInput, got, v.AttrsExpected)
				}
				return
			}
			got, _ := Apply(v.Input)
			if v.TruncateAt > 0 {
				got = Clip(got, v.TruncateAt)
			}
			if got != v.Expected {
				t.Fatalf("\n input: %q\n  got: %q\n want: %q", v.Input, got, v.Expected)
			}
		})
	}
}

// 🔴 Every pattern has a vector that exercises it: deleting a pattern must turn
// a vector red, or the vector file stops being the contract.
func TestEveryPatternHasAVector(t *testing.T) {
	patterns, err := Patterns()
	if err != nil {
		t.Fatal(err)
	}
	vectors := loadVectors(t)
	for _, p := range patterns {
		expr := p.Regex
		if p.CaseInsensitive {
			expr = "(?i)" + expr
		}
		re := regexp.MustCompile(expr)
		hit := false
		for _, v := range vectors {
			if p.Scope == ScopeAttrKey {
				for key := range v.AttrsInput {
					if re.MatchString(key) {
						hit = true
					}
				}
				continue
			}
			if re.MatchString(v.Input) && v.Input != v.Expected {
				hit = true
				break
			}
		}
		if !hit {
			t.Errorf("pattern %q has no vector whose input it matches", p.Name)
		}
	}
}

// 🔴 Every pattern is load-bearing: without it, at least one vector comes out
// different. A pattern another one fully covers is dead weight in BOTH copies
// and, worse, makes "remove a pattern" a change no test notices.
func TestEveryPatternIsLoadBearing(t *testing.T) {
	vectors := loadVectors(t)
	for i := range defaultPatterns {
		rest := append(append([]compiled(nil), defaultPatterns[:i]...), defaultPatterns[i+1:]...)
		changed := false
		for _, v := range vectors {
			without, _ := applyPatterns(rest, v.Input)
			with, _ := applyPatterns(defaultPatterns, v.Input)
			if without != with {
				changed = true
				break
			}
		}
		if !changed {
			t.Errorf("pattern %d (%s) changes no vector's output without it", i, defaultPatterns[i].re)
		}
	}
	// And every attribute-name pattern: without the key patterns at all, an
	// attributes vector must come out different.
	if len(keyPatterns) == 0 {
		t.Fatal("no attribute-name pattern — a secret passed by its key would ship")
	}
	for i := range keyPatterns {
		rest := append(append([]compiled(nil), keyPatterns[:i]...), keyPatterns[i+1:]...)
		changed := false
		for _, v := range vectors {
			if v.AttrsInput == nil {
				continue
			}
			total := 0
			without := (*Redactor)(nil).value(rest, v.AttrsInput, &total, 0)
			with, _ := Value(v.AttrsInput)
			if !reflect.DeepEqual(without, with) {
				changed = true
				break
			}
		}
		if !changed {
			t.Errorf("attribute-name pattern %d (%s) changes no vector without it", i, keyPatterns[i].re)
		}
	}
}

// jsIncompatible names RE2 syntax ECMAScript rejects or reads differently. The
// platform's copy runs in a JavaScript transform, so none may appear.
var jsIncompatible = []string{
	"(?i", "(?s", "(?m", "(?U", "(?P<", "(?<", `\A`, `\z`, `\Q`, `\E`, "[[:", `\p`, `\P`, `\C`, `\x{`,
}

// quantifiedGroupWithQuantifier finds a group that is itself quantified and
// holds a quantifier — `(a+)+`, the catastrophic-backtracking shape a
// JavaScript engine can spend seconds on.
var quantifiedGroupWithQuantifier = regexp.MustCompile(`\((?:\?:)?[^()]*[*+}][^()]*\)[*+?{]`)

var replacementGroupRef = regexp.MustCompile(`\$(.)(.?)`)

func TestPatternsArePortable(t *testing.T) {
	patterns, err := Patterns()
	if err != nil {
		t.Fatal(err)
	}
	if len(patterns) < 15 {
		t.Fatalf("only %d patterns — the file lost its coverage", len(patterns))
	}
	seen := map[string]bool{}
	for _, p := range patterns {
		if p.Name == "" || seen[p.Name] {
			t.Errorf("pattern names must be present and unique: %q", p.Name)
		}
		seen[p.Name] = true
		if _, err := regexp.Compile(p.Regex); err != nil {
			t.Errorf("%s: not RE2: %v", p.Name, err)
		}
		for _, bad := range jsIncompatible {
			if strings.Contains(p.Regex, bad) {
				t.Errorf("%s: %q is not portable to ECMAScript", p.Name, bad)
			}
		}
		if quantifiedGroupWithQuantifier.MatchString(p.Regex) {
			t.Errorf("%s: a quantified group holding a quantifier can backtrack catastrophically", p.Name)
		}
		for _, m := range replacementGroupRef.FindAllStringSubmatch(p.Replacement, -1) {
			if m[1] < "1" || m[1] > "9" {
				t.Errorf("%s: replacement references $%s; use $1…$9 only", p.Name, m[1])
			}
			if len(m[2]) == 1 && (m[2][0] == '_' || (m[2][0] >= '0' && m[2][0] <= '9') ||
				(m[2][0] >= 'a' && m[2][0] <= 'z') || (m[2][0] >= 'A' && m[2][0] <= 'Z')) {
				t.Errorf("%s: $%s is followed by a word character, which Go and JS read differently", p.Name, m[1])
			}
		}
	}
}

func TestTheWireTokenNeverSurvives(t *testing.T) {
	token := "Zm9vYmFyYmF6cXV4LXdpcmUtdG9rZW4tdmFsdWUtMzI" // gitleaks:allow — a fake wire token
	r := New(token, "", "  ")
	for _, line := range []string{
		"hello " + token,
		`{"token":"` + token + `"}`,
		token + token,
	} {
		got, n := r.Apply(line)
		if strings.Contains(got, token) || n == 0 {
			t.Fatalf("the wire token survived: %q (%d)", got, n)
		}
	}
	if got, n := r.Apply("nothing here"); got != "nothing here" || n != 0 {
		t.Fatalf("an innocent line changed: %q (%d)", got, n)
	}
}

func TestApplyCountsEveryReplacement(t *testing.T) {
	got, n := Apply("AKIAIOSFODNN7EXAMPLE and AKIAIOSFODNN7EXAMPLF and ABCD-EF23") // gitleaks:allow — AWS's documented example key
	if n != 3 || strings.Contains(got, "AKIA") {
		t.Fatalf("got %q with %d replacements", got, n)
	}
}

func TestClip(t *testing.T) {
	if Clip("héllo", 2) != "h" { // é is two bytes; never split a rune
		t.Fatalf("clip split a rune: %q", Clip("héllo", 2))
	}
	if Clip("abc", 10) != "abc" || Clip("abc", 0) != "" || Clip("abc", 3) != "abc" {
		t.Fatal("clip bounds")
	}
}
