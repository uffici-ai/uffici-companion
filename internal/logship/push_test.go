package logship

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/redact"
	"github.com/uffici-ai/uffici-companion/internal/surface"
	"github.com/uffici-ai/uffici-companion/internal/wire"
)

func pushRig(machine string) (*PushHandler, *rig) {
	r := newRig(machine)
	r.shipper.Tick(ctx) // the first mint: the policy is known
	return &PushHandler{
		Shipper: r.shipper, Ring: r.ring, Redactor: redact.New("wire-token-value-0123456789"),
		Now: func() time.Time { return time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC) },
	}, r
}

func payload(entries ...map[string]any) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"entries": entries})
	return raw
}

func decode(t *testing.T, raw json.RawMessage) PushResult {
	t.Helper()
	if err := wire.ValidateOpPayload(wire.OpLogsPush, wire.OpResult, raw); err != nil {
		t.Fatalf("the answer breaks its own schema: %v", err)
	}
	var result PushResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestAPushIsRedactedFlooredAndKept(t *testing.T) {
	h, r := pushRig("info")
	raw, err := h.Handle(ctx, payload(
		map[string]any{"ts": "2026-09-29T11:59:00.5+00:00", "source": "plugin", "level": "info",
			"logger": "uffici_for_hermes.adapter", "message": "brief delivered",
			"attrs": map[string]any{"token": "wire-token-value-0123456789", "n": 2, "deep": map[string]any{"k": "ik_0123456789abcdef0123456789abcdef"}}},
		map[string]any{"ts": "not a time", "source": "harness", "level": "info", "message": "harness chatter"},
		map[string]any{"ts": "2026-09-29T11:59:01Z", "source": "harness", "level": "error",
			"message": "failed: Authorization: Bearer abcdefgh12345678"},
		map[string]any{"ts": "2026-09-29T11:59:02Z", "source": "plugin", "level": "debug", "message": "plugin noise"},
	))
	if err != nil {
		t.Fatal(err)
	}
	result := decode(t, raw)
	// The harness floor is warn (Uffici's default beats the machine's info); the
	// plugin's own floor is info.
	if result.Accepted != 2 || result.Dropped != 2 || result.HarnessLevel != "warn" || result.PluginLevel != "info" ||
		result.Busy || result.Disabled {
		t.Fatalf("result %+v", result)
	}
	kept := r.ring.Drain(10, 1<<20)
	if len(kept) != 2 {
		t.Fatalf("kept %+v", kept)
	}
	if kept[0].TS != "2026-09-29T11:59:00.500Z" || kept[0].Source != SourcePlugin || kept[0].Logger != "uffici_for_hermes.adapter" {
		t.Fatalf("first %+v", kept[0])
	}
	if strings.Contains(kept[0].Attrs, "wire-token-value") || strings.Contains(kept[0].Attrs, "ik_0123") ||
		!strings.Contains(kept[0].Attrs, `"n":2`) || kept[0].Redactions != 2 {
		t.Fatalf("attrs %q (%d redactions)", kept[0].Attrs, kept[0].Redactions)
	}
	if strings.Contains(kept[1].Message, "abcdefgh12345678") || kept[1].Redactions != 1 {
		t.Fatalf("message %q", kept[1].Message)
	}
}

func TestAnUnparseableTimeIsStampedNow(t *testing.T) {
	h, r := pushRig("debug")
	r.host.SetShipping(true, "debug")
	r.shipper.ticket = ""
	r.shipper.Tick(ctx)
	if _, err := h.Handle(ctx, payload(map[string]any{"ts": "yesterday", "source": "harness", "level": "debug", "message": "x"})); err != nil {
		t.Fatal(err)
	}
	if got := r.ring.Drain(10, 1<<20)[0].TS; got != "2026-09-29T12:00:00.000Z" {
		t.Fatalf("ts %q", got)
	}
}

// 🔴 A push arriving while one is being handled answers SUCCESS with busy.
func TestASecondPushIsBusyNeverAnError(t *testing.T) {
	h, r := pushRig("warn")
	h.inFlight.Store(true)
	raw, err := h.Handle(ctx, payload(map[string]any{"ts": "2026-09-29T12:00:00Z", "source": "plugin", "level": "error", "message": "x"}))
	if err != nil {
		t.Fatalf("busy must be an answer, not a failure: %v", err)
	}
	if result := decode(t, raw); !result.Busy || result.Accepted != 0 {
		t.Fatalf("result %+v", result)
	}
	if r.ring.Len() != 0 {
		t.Fatal("a busy push kept lines")
	}
	h.inFlight.Store(false)

	// Real concurrency, under -race: every call answers, and none is an error.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := h.Handle(ctx, payload(map[string]any{"ts": "2026-09-29T12:00:00Z", "source": "plugin", "level": "error", "message": "x"})); err != nil {
				t.Errorf("concurrent push: %v", err)
			}
		}()
	}
	wg.Wait()
}

func TestShippingOffAnswersDisabledWithTheWait(t *testing.T) {
	// Off on this machine: no shipper at all.
	h := &PushHandler{Ring: NewRing(10, 1<<20, stamp), Redactor: redact.New()}
	result := decode(t, mustHandle(t, h))
	if !result.Disabled || result.Dropped != 1 || result.RetryAfterSeconds == nil || *result.RetryAfterSeconds != 900 {
		t.Fatalf("machine off: %+v", result)
	}
	// Off in Uffici: the shipper's recheck is the wait.
	on, r := pushRig("warn")
	r.host.SetShipping(false, "warn")
	r.shipper.ticket = ""
	r.clock.advance(FlushEvery)
	r.shipper.Tick(ctx)
	result = decode(t, mustHandle(t, on))
	if !result.Disabled || result.RetryAfterSeconds == nil || *result.RetryAfterSeconds != 900 || r.ring.Len() != 0 {
		t.Fatalf("Uffici off: %+v", result)
	}
}

func mustHandle(t *testing.T, h *PushHandler) json.RawMessage {
	t.Helper()
	raw, err := h.Handle(ctx, payload(map[string]any{"ts": "2026-09-29T12:00:00Z", "source": "plugin", "level": "error", "message": "x"}))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestAnOffContractPayloadIsMalformedWithoutEchoingIt(t *testing.T) {
	h, _ := pushRig("warn")
	for _, raw := range []string{
		`{"entries":[{"ts":"x","source":"companion","level":"info","message":"the secret"}]}`,
		`{"entries":[]}`,
		`not json`,
	} {
		_, err := h.Handle(ctx, json.RawMessage(raw))
		var failure *surface.CallFailure
		if !errors.As(err, &failure) || failure.Class != wire.ClassMalformedPayload {
			t.Fatalf("%s: %v", raw, err)
		}
		if strings.Contains(err.Error(), "the secret") {
			t.Fatal("the refusal echoed the payload")
		}
	}
}

func TestABigPushNudgesTheShipper(t *testing.T) {
	h, r := pushRig("warn")
	h.Ring = NewRing(1000, 8<<20, stamp)
	r.shipper.ring = h.Ring
	var entries []map[string]any
	for i := 0; i < 40; i++ {
		entries = append(entries, map[string]any{"ts": "2026-09-29T12:00:00Z", "source": "plugin", "level": "error",
			"message": strings.Repeat("y", 8000)})
	}
	if _, err := h.Handle(ctx, payload(entries...)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.shipper.wake:
	default:
		t.Fatal("256 KiB arrived and the shipper was not nudged")
	}
}
