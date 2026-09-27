package wire_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/infra/memory"
	memtransport "github.com/uffici-ai/uffici-companion/internal/infra/transport/memory"
	"github.com/uffici-ai/uffici-companion/internal/requests"
	"github.com/uffici-ai/uffici-companion/internal/surface"
	"github.com/uffici-ai/uffici-companion/internal/wire"
)

// 🔴 THE CALL LOG-CLASSIFICATION GUARD (#2028 requirement 7).
//
// A call's payload and its result's payload carry transcript content and
// memory rows. Calls carrying a recognisable marker go through the real decoder
// and the real surface — answered, refused, oversized, timed out — and the
// companion's whole log is then searched for the marker. End to end rather than
// per call site, because the risk is whatever line a future change adds.

const payloadMarker = "the Lisbon invoice for 48,000 EUR is unpaid"

// syncBuffer is a log sink the surface's goroutines may write concurrently.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestACallPayloadNeverReachesALogLine(t *testing.T) {
	logs := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	listener := memtransport.NewListener("memory://call-logging")
	manager := requests.New(memory.New(), requests.Options{})
	companion, err := surface.New(listener, manager, surface.Options{
		Token:  "wire-token-for-the-call-logging-guard",
		Logger: logger,
		Calls: map[string]surface.CallHandler{
			// Echoes its input back, so the marker is in the RESULT too.
			"echo": func(_ context.Context, payload json.RawMessage) (json.RawMessage, error) {
				return payload, nil
			},
			"refuse": func(context.Context, json.RawMessage) (json.RawMessage, error) {
				return nil, &surface.CallFailure{Class: wire.ClassNotPermitted,
					Upstream: json.RawMessage(`{"detail":"` + payloadMarker + `"}`)}
			},
			"hang": func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			},
		},
	})
	if err != nil {
		t.Fatalf("surface.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = companion.Serve(ctx) }()
	defer func() { _ = listener.Close() }()

	conn, err := listener.Dial()
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	sendFrame(t, conn, map[string]any{"v": 1, "type": "hello", "harness": "hermes", "harnessVersion": "0.21.4",
		"pluginVersion": "0.1.0", "token": "wire-token-for-the-call-logging-guard", "contextBudgetTokens": 48000})
	sendFrame(t, conn, map[string]any{"v": 1, "type": "ready"})
	receiveFrame(t, conn) // the companion's hello

	payload := map[string]any{"text": payloadMarker}
	calls := []map[string]any{
		{"v": 1, "type": "call", "callId": "c-echo", "op": "echo", "deadlineMs": 5000, "payload": payload},
		{"v": 1, "type": "call", "callId": "c-refuse", "op": "refuse", "deadlineMs": 5000, "payload": payload},
		{"v": 1, "type": "call", "callId": "c-unknown", "op": "memory.recall", "deadlineMs": 5000, "payload": payload},
		{"v": 1, "type": "call", "callId": "c-hang", "op": "hang", "deadlineMs": 40, "payload": payload},
		{"v": 1, "type": "call", "callId": "c-big", "op": "echo", "deadlineMs": 5000,
			"payload": map[string]any{"text": payloadMarker + strings.Repeat("x", wire.MaxCallPayloadBytes)}},
	}
	for _, frame := range calls {
		sendFrame(t, conn, frame)
	}
	sawMarkerOnTheWire := false
	for range calls {
		raw := receiveFrame(t, conn)
		if strings.Contains(string(raw), payloadMarker) {
			sawMarkerOnTheWire = true
		}
	}

	// 🔴 Non-vacuity: the marker must really have travelled — the echo's result
	// carries it back — and the calls must really have been logged.
	if !sawMarkerOnTheWire {
		t.Fatal("no result carried the marker, so its absence from the log would prove nothing")
	}
	logged := logs.String()
	if strings.Count(logged, `"msg":"call answered"`) < len(calls) {
		t.Fatalf("expected a log line per call, got:\n%s", logged)
	}
	if strings.Contains(logged, payloadMarker) || strings.Contains(logged, "Lisbon") {
		t.Fatalf("a call or result payload reached a log line:\n%s", logged)
	}
	for _, id := range []string{"c-echo", "c-refuse", "c-unknown", "c-hang", "c-big"} {
		if !strings.Contains(logged, id) {
			t.Errorf("the log never names callId %s — ids are what a call's line is FOR", id)
		}
	}
}

func sendFrame(t *testing.T, conn *memtransport.Conn, frame map[string]any) {
	t.Helper()
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Send(context.Background(), raw); err != nil {
		t.Fatalf("send: %v", err)
	}
}

func receiveFrame(t *testing.T, conn *memtransport.Conn) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	raw, err := conn.Receive(ctx)
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	return raw
}
