package logship

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func stamp() string { return "2026-09-29T12:00:00.000Z" }

func line(i int) Entry {
	return Entry{TS: stamp(), Source: SourceCompanion, Level: "info", Message: fmt.Sprintf("line %d", i)}
}

func TestRingDropsTheOldestByCount(t *testing.T) {
	r := NewRing(3, 1<<20, stamp)
	for i := 0; i < 5; i++ {
		r.Push(line(i))
	}
	if r.Len() != 3 || r.Dropped() != 2 {
		t.Fatalf("len %d dropped %d", r.Len(), r.Dropped())
	}
	batch := r.Drain(10, 1<<20)
	if len(batch) != 4 {
		t.Fatalf("want the note plus three lines, got %d", len(batch))
	}
	if batch[0].Message != "logs_dropped" || batch[0].Level != "warn" || batch[0].Attrs != `{"count":2}` {
		t.Fatalf("the dropped note: %+v", batch[0])
	}
	for i, want := range []string{"line 2", "line 3", "line 4"} {
		if batch[i+1].Message != want {
			t.Fatalf("batch[%d] = %q, want %q (oldest dropped first)", i+1, batch[i+1].Message, want)
		}
	}
	// 🔴 Reported ONCE: the next drain carries no second note.
	r.Push(line(9))
	again := r.Drain(10, 1<<20)
	if len(again) != 1 || again[0].Message != "line 9" {
		t.Fatalf("the note was reported twice: %+v", again)
	}
	if r.Dropped() != 2 {
		t.Fatalf("the lifetime count moved: %d", r.Dropped())
	}
}

func TestRingDropsTheOldestByBytes(t *testing.T) {
	one := line(0).size()
	r := NewRing(100, one*3, stamp)
	for i := 0; i < 5; i++ {
		r.Push(line(i))
	}
	if r.Len() != 3 || r.Bytes() > one*3 || r.Dropped() != 2 {
		t.Fatalf("len %d bytes %d dropped %d", r.Len(), r.Bytes(), r.Dropped())
	}
}

func TestRingRefusesALineLargerThanItsBudget(t *testing.T) {
	r := NewRing(10, 100, stamp)
	r.Push(Entry{Message: strings.Repeat("x", 200)})
	if r.Len() != 0 || r.Dropped() != 1 {
		t.Fatalf("len %d dropped %d", r.Len(), r.Dropped())
	}
}

func TestDrainHonoursItsBoundsAndNeverStalls(t *testing.T) {
	r := NewRing(10, 1<<20, stamp)
	for i := 0; i < 6; i++ {
		r.Push(line(i))
	}
	if got := r.Drain(2, 1<<20); len(got) != 2 {
		t.Fatalf("maxEntries: %d", len(got))
	}
	// A byte budget smaller than one line still yields that line.
	if got := r.Drain(10, 1); len(got) != 1 {
		t.Fatalf("a tiny budget stalled the batch: %d", len(got))
	}
	if got := r.Drain(10, line(0).size()*2); len(got) != 2 {
		t.Fatalf("maxBytes: %d", len(got))
	}
	if r.Drain(0, 100) != nil || r.Len() != 1 {
		t.Fatal("a zero-entry drain took something")
	}
	r.Drain(10, 1<<20)
	if r.Drain(10, 1<<20) != nil {
		t.Fatal("an empty ring drained something")
	}
}

func TestClearForgetsWithoutCountingADrop(t *testing.T) {
	r := NewRing(2, 1<<20, stamp)
	for i := 0; i < 3; i++ {
		r.Push(line(i))
	}
	r.Clear()
	if r.Len() != 0 || r.Bytes() != 0 || r.Drain(10, 1<<20) != nil {
		t.Fatal("clear left something behind")
	}
	if r.Dropped() != 1 {
		t.Fatalf("clear changed the lifetime count: %d", r.Dropped())
	}
}

func TestNewRingDefaults(t *testing.T) {
	r := NewRing(0, 0, stamp)
	if r.maxEntries != DefaultMaxEntries || r.maxBytes != DefaultMaxBytes {
		t.Fatalf("defaults %d %d", r.maxEntries, r.maxBytes)
	}
}

// Run under -race: pushes and drains from several goroutines lose nothing the
// bounds did not drop, and never corrupt an entry.
func TestRingIsSafeForConcurrentUse(t *testing.T) {
	r := NewRing(200, 1<<20, stamp)
	var wg sync.WaitGroup
	var mu sync.Mutex
	drained := 0
	for w := 0; w < 4; w++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				r.Push(line(i))
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				for _, e := range r.Drain(50, 1<<20) {
					if e.Message == "logs_dropped" {
						var note map[string]int
						if json.Unmarshal([]byte(e.Attrs), &note) != nil || note["count"] <= 0 {
							t.Errorf("a corrupt note: %q", e.Attrs)
						}
						continue
					}
					if !strings.HasPrefix(e.Message, "line ") {
						t.Errorf("a corrupt entry: %+v", e)
					}
					mu.Lock()
					drained++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	rest := 0
	for _, e := range r.Drain(10_000, 1<<30) {
		if e.Message != "logs_dropped" {
			rest++
		}
	}
	if drained+rest+r.Dropped() != 4*500 {
		t.Fatalf("drained %d + rest %d + dropped %d != %d", drained, rest, r.Dropped(), 4*500)
	}
}

// encoded is an entry as the gateway client puts it on the wire: JSON, HTML
// escaping off.
func encoded(t *testing.T, v any) int {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return buf.Len()
}

// 🔴 #2259 review: an entry's cost is its ENCODED size. Sized by its raw text, a
// quote- or control-heavy line encoded up to six times larger than it counted.
func TestAnEntryNeverCostsLessThanItsEncoding(t *testing.T) {
	for name, text := range map[string]string{
		"quotes":      strings.Repeat(`"`, 500),
		"backslashes": strings.Repeat(`\`, 500),
		"newlines":    strings.Repeat("\n\r\t", 200),
		"ansi colour": strings.Repeat("\x1b[31mred\x1b[0m", 100),
		"nul and del": strings.Repeat("\x00\x7f", 200),
		"invalid":     strings.Repeat("\xff", 300),
		"separators":  strings.Repeat("\u2028\u2029", 150),
		"html":        strings.Repeat("<a>&", 200),
		"multibyte":   strings.Repeat("é日🙂", 100),
	} {
		e := Entry{TS: stamp(), Source: SourceHarness, Level: "error", Logger: text[:min(len(text), 100)],
			Message: text, Attrs: `{"k":"` + strings.ReplaceAll(text, `"`, "") + `"}`, Redactions: 123456}
		if got, want := e.size(), encoded(t, e); got < want {
			t.Errorf("%s: size %d is under the %d bytes it encodes to", name, got, want)
		}
	}
}

// The bound the size exists for: a batch drained at BatchBytes from escape-heavy
// lines still encodes under the gateway's 256 KiB body, with room for the rest of
// the request.
func TestABatchOfEscapeHeavyLinesFitsTheGatewaysBody(t *testing.T) {
	r := NewRing(0, 0, stamp)
	for i := 0; i < 200; i++ {
		r.Push(Entry{TS: stamp(), Source: SourceHarness, Level: "warn", Message: strings.Repeat(`"`, MaxMessageBytes)})
	}
	batch := r.Drain(BatchEntries, BatchBytes)
	if len(batch) < 2 {
		t.Fatalf("drained %d lines; the bound is not being exercised", len(batch))
	}
	if got := encoded(t, batch); got > (256<<10)-4096 {
		t.Fatalf("a drained batch of %d lines encodes to %d bytes, past the gateway's body limit", len(batch), got)
	}
}
