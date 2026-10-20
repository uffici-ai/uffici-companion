package observe

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/requests"
)

// record is one decoded log line.
type record map[string]any

func decode(t *testing.T, raw *bytes.Buffer) []record {
	t.Helper()
	var out []record
	for _, line := range strings.Split(strings.TrimSpace(raw.String()), "\n") {
		if line == "" {
			continue
		}
		var entry record
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decoding %q: %v", line, err)
		}
		out = append(out, entry)
	}
	return out
}

// 🔴 Every line after the brief carries the CONVERSATION and the ELAPSED time,
// though the observer's later calls name only the request. Without that, the
// per-request timings wave 4 wants would have to be reassembled by joining
// lines, which is exactly the work the log is supposed to have done.
func TestEveryLineCarriesTheRequestItsConversationAndElapsed(t *testing.T) {
	raw := &bytes.Buffer{}
	clock := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	observer := Logging(slog.New(slog.NewJSONHandler(raw, nil)), func() time.Time { return clock })

	observer.BriefSent("req_1", "conv_1", 3, 128)
	clock = clock.Add(250 * time.Millisecond)
	observer.Acked("req_1", requests.AckAccepted, requests.SessionFresh)
	observer.OpenerSent("req_1", 5, 400, 48000, 2)
	observer.Posted("req_1", 64)
	observer.Cancelled("req_1", contracts.CancelSuperseded)
	observer.Redelivered("req_1", 2)
	observer.Closed("req_1", contracts.OutcomeAnswered, "")

	lines := decode(t, raw)
	if len(lines) != 7 {
		t.Fatalf("got %d lines, want one per frame", len(lines))
	}
	for i, line := range lines {
		if line["request"] != "req_1" {
			t.Fatalf("line %d names request %v", i, line["request"])
		}
		if line["conversation"] != "conv_1" {
			t.Fatalf("line %d (%v) names conversation %v, want conv_1", i, line["frame"], line["conversation"])
		}
		if _, ok := line["elapsedMs"]; !ok {
			t.Fatalf("line %d (%v) carries no elapsedMs", i, line["frame"])
		}
	}
	if lines[1]["elapsedMs"].(float64) != 250 {
		t.Fatalf("the ack is %v ms after the brief, want 250", lines[1]["elapsedMs"])
	}

	// Directions are from the companion's point of view: it SENDS briefs,
	// openers and cancels, and RECEIVES acks, posts and dones.
	//
	// 🔴 A redelivery has its OWN name. The manager emits it and then delivers,
	// so a second line named "brief" follows it — counting briefs from a log that
	// called both "brief" would over-count every redelivery.
	want := []struct{ frame, direction string }{
		{"brief", directionOut}, {"ack", directionIn}, {"opener", directionOut},
		{"post", directionIn}, {"cancel", directionOut}, {"redelivery", directionOut},
		{"done", directionIn},
	}
	for i, expected := range want {
		if lines[i]["frame"] != expected.frame || lines[i]["direction"] != expected.direction {
			t.Fatalf("line %d is %v/%v, want %s/%s",
				i, lines[i]["direction"], lines[i]["frame"], expected.direction, expected.frame)
		}
	}

	// 🔴 #2451: the opener counts its summary entries — a number, never their text.
	if lines[2]["openerSummaryCount"] != float64(2) {
		t.Fatalf("the opener line carries openerSummaryCount %v, want 2", lines[2]["openerSummaryCount"])
	}
}

// A request the logger never saw opened still logs — it just has no elapsed to
// report. The alternative, dropping the line, would lose exactly the frames
// that arrive after a restart.
func TestAFrameForAnUnknownRequestStillLogsWithoutElapsed(t *testing.T) {
	raw := &bytes.Buffer{}
	observer := Logging(slog.New(slog.NewJSONHandler(raw, nil)), nil)
	observer.Posted("req_unknown", 12)

	lines := decode(t, raw)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	if _, ok := lines[0]["elapsedMs"]; ok {
		t.Fatal("an unknown request reported an elapsed time it cannot know")
	}
	if lines[0]["request"] != "req_unknown" {
		t.Fatalf("the line names %v", lines[0]["request"])
	}
}

// Logging with no logger and no clock is usable rather than a nil dereference:
// the daemon builds it before it knows whether anything is watching.
func TestLoggingFillsItsDefaults(_ *testing.T) {
	observer := Logging(nil, nil)
	observer.BriefSent("req_1", "conv_1", 1, 1)
	observer.Closed("req_1", contracts.OutcomeEmpty, "")
}

// counter is a test observer that records what it was told.
type counter struct{ events []string }

func (c *counter) BriefSent(string, string, int, int)    { c.events = append(c.events, "brief") }
func (c *counter) OpenerSent(string, int, int, int, int) { c.events = append(c.events, "opener") }
func (c *counter) Acked(string, requests.AckOutcome, requests.Session) {
	c.events = append(c.events, "ack")
}
func (c *counter) Posted(string, int)                       { c.events = append(c.events, "post") }
func (c *counter) Closed(string, contracts.Outcome, string) { c.events = append(c.events, "done") }
func (c *counter) Cancelled(string, contracts.CancelReason) { c.events = append(c.events, "cancel") }
func (c *counter) Redelivered(string, int)                  { c.events = append(c.events, "redeliver") }

// 🔴 EVERY observer sees EVERY event. The manager takes one observer, so a
// fan-out that dropped an event would silently disarm either the script's
// expectations or the log — and a script whose expectations never fire reports
// a pass.
func TestFanoutTellsEveryObserverAndSkipsNils(t *testing.T) {
	first, second := &counter{}, &counter{}
	observer := Fanout(first, nil, second)

	observer.BriefSent("req_1", "conv_1", 1, 1)
	observer.OpenerSent("req_1", 1, 1, 1, 0)
	observer.Acked("req_1", requests.AckAccepted, requests.SessionFresh)
	observer.Posted("req_1", 1)
	observer.Cancelled("req_1", contracts.CancelExpired)
	observer.Redelivered("req_1", 1)
	observer.Closed("req_1", contracts.OutcomeAnswered, "")

	want := []string{"brief", "opener", "ack", "post", "cancel", "redeliver", "done"}
	for _, got := range []*counter{first, second} {
		if strings.Join(got.events, ",") != strings.Join(want, ",") {
			t.Fatalf("an observer saw %v, want %v", got.events, want)
		}
	}
}

// 🔴 Counting briefs from the log must give the number of briefs. The manager
// reports a redelivery and then delivers, so these two calls are one resend —
// naming both "brief" would report two.
func TestARedeliveryIsNotCountedAsASecondBrief(t *testing.T) {
	raw := &bytes.Buffer{}
	observer := Logging(slog.New(slog.NewJSONHandler(raw, nil)), nil)

	observer.BriefSent("req_1", "conv_1", 2, 40)
	observer.Redelivered("req_1", 1)
	observer.BriefSent("req_1", "conv_1", 2, 40)

	briefs := 0
	for _, line := range decode(t, raw) {
		if line["frame"] == "brief" {
			briefs++
		}
	}
	if briefs != 2 {
		t.Fatalf("the log shows %d brief(s) for one brief and one resend, want 2", briefs)
	}
}

// A frame for a request the logger never saw open names no conversation, rather
// than naming an empty one — a blank field reads as a fact about the request.
func TestAnUnknownRequestNamesNoConversation(t *testing.T) {
	raw := &bytes.Buffer{}
	observer := Logging(slog.New(slog.NewJSONHandler(raw, nil)), nil)
	observer.Posted("req_unknown", 12)

	lines := decode(t, raw)
	if _, ok := lines[0]["conversation"]; ok {
		t.Fatalf("the line carries conversation=%v for a request it never saw open", lines[0]["conversation"])
	}
}
