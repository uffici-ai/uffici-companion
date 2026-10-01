// Package logship holds the companion's own log lines, and the plugin's, until
// they are shipped to Uffici.
//
// 🔴 Shipping logs is the lowest-priority thing this program does. Everything
// here is bounded and drops the OLDEST line rather than block or grow: a full
// buffer costs history, never an answer. Every line is redacted before it is
// buffered (package redact), so nothing unredacted waits here.
package logship

import (
	"encoding/json"
	"sync"
	"unicode/utf8"
)

// The row bounds the platform stores. A longer message is clipped after
// redaction; attributes that encode longer than MaxAttrsBytes are replaced by a
// marker rather than cut mid-JSON.
const (
	MaxMessageBytes = 8192
	MaxAttrsBytes   = 4096
	MaxLoggerBytes  = 128
)

// Default ring bounds: whichever is reached first drops the oldest line.
const (
	DefaultMaxEntries = 5000
	DefaultMaxBytes   = 2 << 20
)

// Sources and levels, as the platform names them.
const (
	SourceCompanion = "companion"
	SourcePlugin    = "plugin"
	SourceHarness   = "harness"
)

// Entry is one line waiting to ship.
type Entry struct {
	// TS is when the line was written, RFC 3339 in UTC with milliseconds.
	TS string `json:"ts"`
	// Source is companion, plugin or harness.
	Source string `json:"source"`
	// Level is debug, info, warn or error.
	Level string `json:"level"`
	// Logger names the component or logger inside the source, when there is one.
	Logger string `json:"logger,omitempty"`
	// Message is the redacted, clipped line.
	Message string `json:"message"`
	// Attrs is the line's attributes as a JSON object, each value redacted
	// before encoding; empty when there are none.
	Attrs string `json:"attrs,omitempty"`
	// Redactions counts the replacements made on this machine.
	Redactions int `json:"redactions,omitempty"`
}

// entryFraming bounds everything in an entry's encoding that is not its text: the
// braces, the seven keys with their quotes, colons and commas, and the digits of
// Redactions.
const entryFraming = 96

// size is what an entry costs against the byte bound: never less than its JSON
// encoding. 🔴 The ENCODED size, not the raw one — a `"` or `\` encodes as two
// bytes and a control character as six, so a batch of quote- or ANSI-heavy lines
// sized by its raw text would pass the gateway's body limit and be refused
// every time it was rebuilt (#2259 review).
func (e Entry) size() int {
	return jsonLen(e.TS) + jsonLen(e.Source) + jsonLen(e.Level) + jsonLen(e.Logger) +
		jsonLen(e.Message) + jsonLen(e.Attrs) + entryFraming
}

// jsonLen is an upper bound on the bytes s takes inside a JSON string, as the
// gateway client encodes it (HTML escaping off): two for a quote, a backslash,
// a newline, a return or a tab; six for any other control character, an invalid
// byte (encoded as \ufffd) and U+2028/U+2029; the UTF-8 bytes otherwise.
func jsonLen(s string) int {
	n := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch {
			case c == '"' || c == '\\' || c == '\n' || c == '\r' || c == '\t':
				n += 2
			case c < 0x20 || c == 0x7f:
				n += 6
			default:
				n++
			}
			i++
			continue
		}
		r, width := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && width == 1 || r == '\u2028' || r == '\u2029' {
			n += 6
		} else {
			n += width
		}
		i += width
	}
	return n
}

// Ring is a bounded, drop-oldest queue of entries, safe for concurrent use.
type Ring struct {
	mu         sync.Mutex
	entries    []Entry
	head       int
	count      int
	bytes      int
	maxEntries int
	maxBytes   int
	// pending counts lines dropped since the last drain reported them; total
	// counts every line ever dropped, for `uffici doctor`.
	pending int
	total   int
	// now stamps the synthetic `logs_dropped` line.
	now func() string
}

// NewRing returns a ring holding at most maxEntries entries and maxBytes bytes.
func NewRing(maxEntries, maxBytes int, now func() string) *Ring {
	if maxEntries <= 0 {
		maxEntries = DefaultMaxEntries
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	return &Ring{entries: make([]Entry, maxEntries), maxEntries: maxEntries, maxBytes: maxBytes, now: now}
}

// Push adds an entry, dropping the oldest until both bounds hold. It never
// blocks on anything but the ring's own short lock, and never grows.
func (r *Ring) Push(e Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cost := e.size()
	if cost > r.maxBytes {
		// Unreachable with clipped entries; refused BEFORE anything is evicted, so
		// one oversized line never empties the ring on its way to being dropped.
		r.pending++
		r.total++
		return
	}
	for r.count > 0 && (r.count == r.maxEntries || r.bytes+cost > r.maxBytes) {
		r.dropOldest()
	}
	tail := (r.head + r.count) % r.maxEntries
	r.entries[tail] = e
	r.count++
	r.bytes += cost
}

func (r *Ring) dropOldest() {
	r.bytes -= r.entries[r.head].size()
	r.entries[r.head] = Entry{}
	r.head = (r.head + 1) % r.maxEntries
	r.count--
	r.pending++
	r.total++
}

// Clear empties the ring without counting the lines as dropped — shipping was
// switched off, and what it held will never be sent.
func (r *Ring) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for r.count > 0 {
		r.entries[r.head] = Entry{}
		r.head = (r.head + 1) % r.maxEntries
		r.count--
	}
	r.bytes = 0
	r.pending = 0
}

// Drain removes and returns up to maxEntries entries and roughly maxBytes
// bytes, oldest first. When lines were dropped since the last drain, the batch
// starts with one `warn` entry, `logs_dropped`, whose attrs carry the count —
// reported once.
func (r *Ring) Drain(maxEntries, maxBytes int) []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if maxEntries <= 0 || (r.count == 0 && r.pending == 0) {
		return nil
	}
	var batch []Entry
	used := 0
	if r.pending > 0 {
		attrs, _ := json.Marshal(map[string]int{"count": r.pending})
		note := Entry{TS: r.now(), Source: SourceCompanion, Level: "warn", Logger: "logship",
			Message: "logs_dropped", Attrs: string(attrs)}
		batch = append(batch, note)
		used += note.size()
		r.pending = 0
	}
	for r.count > 0 && len(batch) < maxEntries {
		next := r.entries[r.head]
		// Always take one real line, so a batch can never stall on a bound.
		if len(batch) > 0 && used+next.size() > maxBytes {
			break
		}
		batch = append(batch, next)
		used += next.size()
		r.bytes -= next.size()
		r.entries[r.head] = Entry{}
		r.head = (r.head + 1) % r.maxEntries
		r.count--
	}
	return batch
}

// Len is the number of entries waiting.
func (r *Ring) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}

// Waiting is true while a drain would return something: a line, or only the
// note that lines were dropped. 🔴 Len alone misses the second, and a shutdown
// flush that asked it would never report the loss it was holding (#2259 review).
func (r *Ring) Waiting() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count > 0 || r.pending > 0
}

// Bytes is the byte cost of the entries waiting.
func (r *Ring) Bytes() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bytes
}

// Dropped is every line dropped since the process started.
func (r *Ring) Dropped() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.total
}
