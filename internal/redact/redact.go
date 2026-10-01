// Package redact removes credential-shaped text from a log line before it
// leaves the machine.
//
// 🔴 It is best-effort minimization, not a guarantee. The patterns catch the
// shapes of the secrets this program and its harness are known to handle —
// agent keys, bearer tokens, provider keys, signed URLs, pairing codes — and
// nothing catches a secret with no recognizable shape. The platform applies the
// SAME pattern file a second time when a line arrives, so a pattern added here
// is added there by copying the file, never by rewriting it.
//
// Two rules make the pattern file portable to the platform's JavaScript copy:
// every regular expression is valid in both Go's RE2 and ECMAScript (no inline
// flags — case-insensitivity is the `caseInsensitive` field — no named groups,
// no POSIX classes, no `\A`/`\z`), and a replacement references a group only
// as `$1`…`$9` followed by a non-word character. `patterns_test.go` pins both.
//
// Order matters twice. The patterns run in file order, most specific first, so
// a JWT inside an Authorization header is named as a JWT. And a line is
// redacted BEFORE it is truncated: a cut token no longer matches its pattern.
package redact

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

//go:embed patterns.json
var patternsJSON []byte

// Pattern is one entry of `patterns.json`.
type Pattern struct {
	// Name identifies the pattern in a test failure; it never reaches a log line.
	Name string `json:"name"`
	// Regex is the expression, valid in both RE2 and ECMAScript.
	Regex string `json:"regex"`
	// Replacement is what a match becomes; `$1` keeps a captured prefix.
	Replacement string `json:"replacement"`
	// CaseInsensitive compiles the expression with `(?i)` here and the `i` flag
	// in the platform's copy.
	CaseInsensitive bool `json:"caseInsensitive"`
	// Scope is empty for a pattern over text, or `attrKey` for one over an
	// attribute's NAME: a value under a matching key is replaced whole, because a
	// secret passed as `password=hunter2` has a shape and one passed as
	// `{"password": "hunter2"}` has only its key (#2259 review).
	Scope string `json:"scope,omitempty"`
}

// ScopeAttrKey marks a pattern matched against attribute names.
const ScopeAttrKey = "attrKey"

type compiled struct {
	re          *regexp.Regexp
	replacement string
}

// Patterns parses the embedded pattern file. It is exported for the tests and
// for a reader who wants the list; Apply is what redacts.
func Patterns() ([]Pattern, error) {
	var patterns []Pattern
	if err := json.Unmarshal(patternsJSON, &patterns); err != nil {
		return nil, fmt.Errorf("parsing patterns.json: %w", err)
	}
	return patterns, nil
}

var defaultPatterns, keyPatterns = mustCompile()

// mustCompile answers the text patterns and the attribute-name patterns, each in
// file order.
func mustCompile() (text, keys []compiled) {
	patterns, err := Patterns()
	if err != nil {
		panic(err)
	}
	for _, p := range patterns {
		expr := p.Regex
		if p.CaseInsensitive {
			expr = "(?i)" + expr
		}
		c := compiled{re: regexp.MustCompile(expr), replacement: p.Replacement}
		if p.Scope == ScopeAttrKey {
			keys = append(keys, c)
		} else {
			text = append(text, c)
		}
	}
	return text, keys
}

// Redactor applies the pattern file plus the exact values of this process's
// own secrets.
type Redactor struct {
	exact []string
}

// New returns a Redactor that also removes each of `secrets` wherever it
// appears verbatim — the wire token and the agent key, which are runtime
// values no pattern can name. Empty values are ignored.
//
// 🔴 The exact values are held only in memory and never logged.
func New(secrets ...string) *Redactor {
	r := &Redactor{}
	for _, s := range secrets {
		if strings.TrimSpace(s) != "" {
			r.exact = append(r.exact, s)
		}
	}
	return r
}

// Apply redacts s and returns the result with the number of replacements made.
// It never fails: an input with nothing to redact comes back unchanged with 0.
func (r *Redactor) Apply(s string) (string, int) {
	count := 0
	if r != nil {
		for _, secret := range r.exact {
			if n := strings.Count(s, secret); n > 0 {
				s = strings.ReplaceAll(s, secret, "[redacted]")
				count += n
			}
		}
	}
	s, n := applyPatterns(defaultPatterns, s)
	return s, count + n
}

func applyPatterns(patterns []compiled, s string) (string, int) {
	count := 0
	for _, p := range patterns {
		if matches := p.re.FindAllStringIndex(s, -1); len(matches) > 0 {
			count += len(matches)
			s = p.re.ReplaceAllString(s, p.replacement)
		}
	}
	return s, count
}

// Apply redacts s with the pattern file alone — no exact secrets.
func Apply(s string) (string, int) {
	return (*Redactor)(nil).Apply(s)
}

// MaxDepth is how deep Value walks before it redacts a value as its encoding.
const MaxDepth = 8

// Value redacts a decoded JSON value — a line's attributes — and answers the
// replacements made: every string through Apply, every name too, and the whole
// value under a name an attribute-key pattern matches. Past MaxDepth a value is
// redacted as its encoding rather than walked, never passed through.
func (r *Redactor) Value(value any) (any, int) {
	total := 0
	out := r.value(keyPatterns, value, &total, 0)
	return out, total
}

// Value redacts with the pattern file alone — no exact secrets.
func Value(value any) (any, int) {
	return (*Redactor)(nil).Value(value)
}

func (r *Redactor) value(keys []compiled, value any, total *int, depth int) any {
	switch v := value.(type) {
	case string:
		text, n := r.Apply(v)
		*total += n
		return text
	case map[string]any:
		if depth >= MaxDepth {
			return r.encoded(v, total)
		}
		out := make(map[string]any, len(v))
		for key, inner := range v {
			name, n := r.Apply(key)
			*total += n
			if replacement, secret := secretKey(keys, key); secret {
				out[name] = replacement
				*total++
				continue
			}
			out[name] = r.value(keys, inner, total, depth+1)
		}
		return out
	case []any:
		if depth >= MaxDepth {
			return r.encoded(v, total)
		}
		out := make([]any, len(v))
		for i, inner := range v {
			out[i] = r.value(keys, inner, total, depth+1)
		}
		return out
	default:
		return v
	}
}

func (r *Redactor) encoded(value any, total *int) string {
	raw, _ := json.Marshal(value)
	text, n := r.Apply(string(raw))
	*total += n
	return text
}

func secretKey(keys []compiled, key string) (string, bool) {
	for _, k := range keys {
		if k.re.MatchString(key) {
			return k.replacement, true
		}
	}
	return "", false
}

// Clip cuts s to at most maxBytes bytes on a rune boundary. Byte length bounds
// the platform's UTF-16 length too (every UTF-16 unit takes at least one byte),
// so a clipped line always fits the platform's character bound.
//
// 🔴 Clip after Apply, never before: a token cut in half no longer matches its
// pattern and would ship as the half that survived.
func Clip(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
