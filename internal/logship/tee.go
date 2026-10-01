package logship

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/redact"
)

// Excluded attributes: a record carrying either is never captured.
//
// 🔴 This is the recursion guard. The shipper logs about itself under
// `component=logship`, and the local wire logs the plugin's `logs.push` calls
// under `op=logs.push`; capturing either would ship a line about shipping, which
// produces another line, forever.
const (
	ExcludedComponent = "logship"
	ExcludedOp        = "logs.push"
)

// Tee is the slog.Handler that copies the companion's own records into a Ring.
// It sits beside the stdout handler in a slog.MultiHandler, so stdout is exactly
// what it was without it.
type Tee struct {
	ring    *Ring
	redact  *redact.Redactor
	level   slog.Leveler
	enabled func() bool
	attrs   []slog.Attr
	groups  []string
	now     func() time.Time
}

// TeeOptions configures a Tee.
type TeeOptions struct {
	// Ring receives the captured entries.
	Ring *Ring
	// Redactor applies the pattern file and the process's own secrets.
	Redactor *redact.Redactor
	// Level is the capture threshold (`UFFICI_LOG_SHIPPING_LEVEL`).
	Level slog.Leveler
	// Enabled, when set, is consulted on every record; false captures nothing
	// (shipping switched off by the portal). Nil means always.
	Enabled func() bool
	// Now stamps entries; nil means time.Now.
	Now func() time.Time
}

// NewTee returns the capture handler.
func NewTee(options TeeOptions) *Tee {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	level := options.Level
	if level == nil {
		level = slog.LevelInfo
	}
	return &Tee{ring: options.Ring, redact: options.Redactor, level: level, enabled: options.Enabled, now: now}
}

// Enabled reports whether a record at level would be captured.
func (t *Tee) Enabled(_ context.Context, level slog.Level) bool {
	if t.enabled != nil && !t.enabled() {
		return false
	}
	return level >= t.level.Level()
}

// Handle captures one record. It never fails and never blocks beyond the
// ring's lock: a line it cannot capture is simply not captured.
func (t *Tee) Handle(_ context.Context, record slog.Record) error {
	flat := make(map[string]any, record.NumAttrs()+len(t.attrs))
	logger := ""
	excluded := false
	visit := func(key string, value slog.Value) {
		if key == "component" {
			logger = value.String()
			if logger == ExcludedComponent {
				excluded = true
			}
		}
		if key == "op" && value.String() == ExcludedOp {
			excluded = true
		}
		flat[key] = value
	}
	for _, a := range t.attrs {
		flattenAttr("", a, visit)
	}
	prefix := groupPrefix(t.groups)
	record.Attrs(func(a slog.Attr) bool {
		flattenAttr(prefix, a, visit)
		return true
	})
	if excluded {
		return nil
	}

	message, redactions := t.redact.Apply(record.Message)
	message = redact.Clip(message, MaxMessageBytes)
	attrs, n := t.encodeAttrs(flat)
	redactions += n

	ts := record.Time
	if ts.IsZero() {
		ts = t.now()
	}
	t.ring.Push(Entry{
		TS:         FormatTS(ts),
		Source:     SourceCompanion,
		Level:      LevelName(record.Level),
		Logger:     redact.Clip(logger, MaxLoggerBytes),
		Message:    message,
		Attrs:      attrs,
		Redactions: redactions,
	})
	return nil
}

// encodeAttrs turns slog values into plain ones and encodes them.
func (t *Tee) encodeAttrs(flat map[string]any) (string, int) {
	plain := make(map[string]any, len(flat))
	for key, raw := range flat {
		value, ok := raw.(slog.Value)
		if !ok {
			continue
		}
		switch value.Kind() {
		case slog.KindInt64:
			plain[key] = value.Int64()
		case slog.KindUint64:
			plain[key] = value.Uint64()
		case slog.KindFloat64:
			plain[key] = value.Float64()
		case slog.KindBool:
			plain[key] = value.Bool()
		case slog.KindDuration:
			plain[key] = value.Duration().String()
		case slog.KindTime:
			plain[key] = FormatTS(value.Time())
		default:
			plain[key] = value.String()
		}
	}
	return EncodeAttrs(plain, t.redact)
}

// EncodeAttrs redacts every string inside attrs BEFORE encoding — a secret split
// across JSON escaping would otherwise slip past its pattern — and replaces an
// encoding over the bound with a marker rather than cutting it mid-JSON. Empty
// attrs encode to "".
func EncodeAttrs(attrs map[string]any, redactor *redact.Redactor) (string, int) {
	if len(attrs) == 0 {
		return "", 0
	}
	cleaned, total := redactor.Value(attrs)
	encoded, err := json.Marshal(cleaned)
	if err != nil {
		return "", total
	}
	if len(encoded) > MaxAttrsBytes {
		return `{"truncated":true}`, total
	}
	return string(encoded), total
}

// WithAttrs returns a Tee that carries attrs on every record.
func (t *Tee) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *t
	prefix := groupPrefix(t.groups)
	next.attrs = append(append([]slog.Attr(nil), t.attrs...), prefixed(prefix, attrs)...)
	return &next
}

// WithGroup returns a Tee that nests later attributes under name.
func (t *Tee) WithGroup(name string) slog.Handler {
	if name == "" {
		return t
	}
	next := *t
	next.groups = append(append([]string(nil), t.groups...), name)
	return &next
}

func prefixed(prefix string, attrs []slog.Attr) []slog.Attr {
	if prefix == "" {
		return attrs
	}
	out := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		out = append(out, slog.Attr{Key: prefix + a.Key, Value: a.Value})
	}
	return out
}

func groupPrefix(groups []string) string {
	prefix := ""
	for _, g := range groups {
		prefix += g + "."
	}
	return prefix
}

// flattenAttr walks groups into dotted keys.
func flattenAttr(prefix string, a slog.Attr, visit func(string, slog.Value)) {
	value := a.Value.Resolve()
	if value.Kind() == slog.KindGroup {
		inner := prefix
		if a.Key != "" {
			inner = prefix + a.Key + "."
		}
		for _, child := range value.Group() {
			flattenAttr(inner, child, visit)
		}
		return
	}
	if a.Key == "" {
		return
	}
	visit(prefix+a.Key, value)
}

// FormatTS renders a time the way the platform stores it: RFC 3339, UTC,
// milliseconds, `Z`.
func FormatTS(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// LevelName maps a slog level onto the platform's four.
func LevelName(level slog.Level) string {
	switch {
	case level >= slog.LevelError:
		return "error"
	case level >= slog.LevelWarn:
		return "warn"
	case level >= slog.LevelInfo:
		return "info"
	default:
		return "debug"
	}
}
