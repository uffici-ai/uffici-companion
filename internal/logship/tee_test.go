package logship

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/redact"
)

var fixed = time.Date(2026, 9, 29, 12, 0, 0, 123_000_000, time.UTC)

func newTee(level slog.Level, secrets ...string) (*Tee, *Ring) {
	ring := NewRing(100, 1<<20, stamp)
	return NewTee(TeeOptions{Ring: ring, Redactor: redact.New(secrets...), Level: level,
		Now: func() time.Time { return fixed }}), ring
}

// fixedTime strips the handler's own clock, so two outputs can be compared.
func fixedTime(_ []string, a slog.Attr) slog.Attr {
	if a.Key == slog.TimeKey {
		return slog.Attr{}
	}
	return a
}

func emit(logger *slog.Logger) {
	logger.Info("companion listening", slog.String("transport", "socket"))
	logger.With(slog.String("component", "gateway")).Warn("reconnecting", slog.Int("attempt", 2))
	logger.Debug("frame", slog.String("type", "brief"))
	logger.With(slog.String("component", ExcludedComponent)).Info("pushed a batch")
	logger.Info("call answered", slog.String("op", ExcludedOp))
	logger.WithGroup("req").Error("failed", slog.String("id", "r1"))
}

// 🔴 The tee sits beside stdout and changes nothing about it: the same records
// produce the same bytes with and without it, at every stdout level.
func TestStdoutIsUnchangedByTheTee(t *testing.T) {
	for _, level := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelError} {
		var plain, teed bytes.Buffer
		opts := &slog.HandlerOptions{Level: level, ReplaceAttr: fixedTime}
		emit(slog.New(slog.NewJSONHandler(&plain, opts)))
		tee, _ := newTee(slog.LevelDebug)
		emit(slog.New(slog.NewMultiHandler(slog.NewJSONHandler(&teed, opts), tee)))
		if plain.String() != teed.String() {
			t.Fatalf("level %v: stdout changed\n plain: %s\n teed:  %s", level, plain.String(), teed.String())
		}
	}
}

// 🔴 The recursion guard: a line about shipping is never shipped.
func TestTheTeeNeverCapturesItsOwnShipping(t *testing.T) {
	tee, ring := newTee(slog.LevelDebug)
	emit(slog.New(tee))
	batch := ring.Drain(100, 1<<20)
	if len(batch) != 4 {
		t.Fatalf("want four captured lines, got %d: %+v", len(batch), batch)
	}
	for _, e := range batch {
		if e.Logger == ExcludedComponent || strings.Contains(e.Attrs, ExcludedOp) || e.Message == "pushed a batch" {
			t.Fatalf("a shipping line was captured: %+v", e)
		}
	}
}

func TestTheTeeMapsARecordToAnEntry(t *testing.T) {
	tee, ring := newTee(slog.LevelInfo)
	logger := slog.New(tee).With(slog.String("component", "gateway"))
	logger.Warn("reconnecting", slog.Int("attempt", 2), slog.Bool("fresh", true),
		slog.Duration("wait", time.Second), slog.Float64("ratio", 0.5), slog.Uint64("n", 7),
		slog.Time("at", fixed), slog.Group("peer", slog.String("host", "h")))
	logger.Debug("below the threshold")
	batch := ring.Drain(10, 1<<20)
	if len(batch) != 1 {
		t.Fatalf("got %d entries", len(batch))
	}
	e := batch[0]
	if e.Source != SourceCompanion || e.Level != "warn" || e.Logger != "gateway" || e.Message != "reconnecting" {
		t.Fatalf("entry %+v", e)
	}
	var attrs map[string]any
	if err := json.Unmarshal([]byte(e.Attrs), &attrs); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"component": "gateway", "attempt": float64(2), "fresh": true, "wait": "1s",
		"ratio": 0.5, "n": float64(7), "at": "2026-09-29T12:00:00.123Z", "peer.host": "h"}
	for k, v := range want {
		if attrs[k] != v {
			t.Fatalf("attrs[%s] = %v, want %v (all: %v)", k, attrs[k], v, attrs)
		}
	}
	if len(e.TS) != len("2026-09-29T12:00:00.000Z") || !strings.HasSuffix(e.TS, "Z") {
		t.Fatalf("ts %q is not the platform's shape", e.TS)
	}
}

func TestTheTeeRedactsMessageAndEveryAttrBeforeCapture(t *testing.T) {
	token := "the-wire-token-value-0123456789"
	tee, ring := newTee(slog.LevelInfo, token)
	slog.New(tee).WithGroup("g").Info("bad key ik_0123456789abcdef0123456789abcdef0123456789abcdef",
		slog.String("header", "Authorization: Bearer abcdefgh12345678"),
		slog.String("echo", token))
	e := ring.Drain(10, 1<<20)[0]
	for _, secret := range []string{"ik_0123", "abcdefgh12345678", token} {
		if strings.Contains(e.Message+e.Attrs, secret) {
			t.Fatalf("%q survived: %+v", secret, e)
		}
	}
	if e.Redactions != 3 {
		t.Fatalf("redactions %d", e.Redactions)
	}
	if !strings.Contains(e.Attrs, `"g.header"`) {
		t.Fatalf("the group prefix was lost: %s", e.Attrs)
	}
}

func TestTheTeeClipsAndBoundsWhatItCaptures(t *testing.T) {
	tee, ring := newTee(slog.LevelInfo)
	slog.New(tee).Info(strings.Repeat("é", MaxMessageBytes), slog.String("big", strings.Repeat("x", MaxAttrsBytes)))
	e := ring.Drain(10, 1<<20)[0]
	if len(e.Message) > MaxMessageBytes || e.Attrs != `{"truncated":true}` {
		t.Fatalf("message %d bytes, attrs %q", len(e.Message), e.Attrs)
	}
}

func TestTheTeeHonoursTheSwitchAndHandlerPlumbing(t *testing.T) {
	on := false
	ring := NewRing(10, 1<<20, stamp)
	tee := NewTee(TeeOptions{Ring: ring, Redactor: redact.New(), Enabled: func() bool { return on }})
	if tee.Enabled(context.Background(), slog.LevelError) {
		t.Fatal("captured while switched off")
	}
	on = true
	if !tee.Enabled(context.Background(), slog.LevelInfo) || tee.Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("the default threshold is info")
	}
	if tee.WithGroup("") != slog.Handler(tee) {
		t.Fatal("an empty group must be a no-op")
	}
	// A record with no time is stamped by the tee's clock; one with no attrs ships none.
	if err := tee.Handle(context.Background(), slog.NewRecord(time.Time{}, slog.LevelInfo, "bare", 0)); err != nil {
		t.Fatal(err)
	}
	e := ring.Drain(10, 1<<20)[0]
	if e.Attrs != "" || e.TS == "" {
		t.Fatalf("entry %+v", e)
	}
}

func TestLevelName(t *testing.T) {
	for level, want := range map[slog.Level]string{
		slog.LevelDebug - 4: "debug", slog.LevelDebug: "debug", slog.LevelInfo: "info",
		slog.LevelWarn: "warn", slog.LevelError: "error", slog.LevelError + 4: "error",
	} {
		if got := LevelName(level); got != want {
			t.Fatalf("LevelName(%v) = %q, want %q", level, got, want)
		}
	}
}
