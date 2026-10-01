package logship

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/redact"
	"github.com/uffici-ai/uffici-companion/internal/surface"
	"github.com/uffici-ai/uffici-companion/internal/wire"
)

// disabledRetry is what a plugin is told to wait when shipping is off on this
// machine — it will not come back on without a restart.
const disabledRetry = 15 * time.Minute

// pushRequest is the `logs.push` payload (wire/v1/ops/logs.push.request).
type pushRequest struct {
	Entries []struct {
		TS      string         `json:"ts"`
		Source  string         `json:"source"`
		Level   string         `json:"level"`
		Logger  string         `json:"logger"`
		Message string         `json:"message"`
		Attrs   map[string]any `json:"attrs"`
	} `json:"entries"`
}

// PushResult is the `logs.push` answer (wire/v1/ops/logs.push.result).
type PushResult struct {
	Accepted          int    `json:"accepted"`
	Dropped           int    `json:"dropped"`
	Disabled          bool   `json:"disabled"`
	Busy              bool   `json:"busy"`
	HarnessLevel      string `json:"harnessLevel"`
	PluginLevel       string `json:"pluginLevel"`
	RetryAfterSeconds *int   `json:"retryAfterSeconds,omitempty"`
}

// PushHandler serves the `logs.push` call op: a plugin's own lines and its
// harness's, redacted, floored and kept in the ring for the shipper.
//
// 🔴 One in flight: a second push while one is being handled answers
// SUCCESS with `busy: true` — never an error class, because the wire's error
// vocabulary is closed and "come back later" is not a failure. The payload never
// reaches a log line.
type PushHandler struct {
	// Shipper supplies the floors and the switch; nil means shipping is off on
	// this machine, and every push answers `disabled`.
	Shipper *Shipper
	Ring    *Ring
	// Redactor applies the pattern file and this process's own secrets.
	Redactor *redact.Redactor
	// Now stamps a line whose timestamp does not parse.
	Now func() time.Time

	inFlight atomic.Bool
}

// Handle is the surface's CallHandler for `logs.push`.
func (h *PushHandler) Handle(_ context.Context, payload json.RawMessage) (json.RawMessage, error) {
	if err := wire.ValidateOpPayload(wire.OpLogsPush, wire.OpRequest, payload); err != nil {
		return nil, &surface.CallFailure{Class: wire.ClassMalformedPayload}
	}
	var request pushRequest
	if err := json.Unmarshal(payload, &request); err != nil {
		return nil, &surface.CallFailure{Class: wire.ClassMalformedPayload}
	}
	harnessFloor, pluginFloor := "warn", "info"
	if h.Shipper != nil {
		harnessFloor, pluginFloor = h.Shipper.Floors()
	}
	result := PushResult{HarnessLevel: harnessFloor, PluginLevel: pluginFloor}

	if h.Shipper == nil || !h.Shipper.Capturing() {
		wait := int(disabledRetry.Seconds())
		if h.Shipper != nil {
			if after := int(h.Shipper.RetryAfter().Seconds()); after > 0 {
				wait = after
			}
		}
		result.Disabled, result.Dropped, result.RetryAfterSeconds = true, len(request.Entries), &wait
		return json.Marshal(result)
	}
	if !h.inFlight.CompareAndSwap(false, true) {
		result.Busy = true
		return json.Marshal(result)
	}
	defer h.inFlight.Store(false)

	now := time.Now
	if h.Now != nil {
		now = h.Now
	}
	for _, e := range request.Entries {
		floor := pluginFloor
		if e.Source == SourceHarness {
			floor = harnessFloor
		}
		if !AtOrAbove(e.Level, floor) {
			result.Dropped++
			continue
		}
		message, n := h.Redactor.Apply(e.Message)
		logger, m := h.Redactor.Apply(e.Logger)
		attrs, k := EncodeAttrs(e.Attrs, h.Redactor)
		h.Ring.Push(Entry{
			TS:         normalizeTS(e.TS, now),
			Source:     e.Source,
			Level:      e.Level,
			Logger:     redact.Clip(logger, MaxLoggerBytes),
			Message:    redact.Clip(message, MaxMessageBytes),
			Attrs:      attrs,
			Redactions: n + m + k,
		})
		result.Accepted++
	}
	if h.Ring.Bytes() >= FlushAtBytes && h.Shipper != nil {
		h.Shipper.Nudge()
	}
	return json.Marshal(result)
}

// normalizeTS renders a plugin's RFC 3339 timestamp the way the platform stores
// it, or now when it does not parse.
func normalizeTS(ts string, now func() time.Time) string {
	if parsed, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		return FormatTS(parsed)
	}
	return FormatTS(now())
}
