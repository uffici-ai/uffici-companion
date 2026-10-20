package requests

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
)

// withAttachment seeds the rig's conversation with an entry carrying an
// attachment — the only kind of delta a prefetch runs for.
func withAttachment(r *rig) {
	r.host.Seed("conv_1", "c0001", []contracts.Entry{
		{ID: "e1", Cursor: "c0001", Kind: "message", Text: "first", Author: contracts.Author{Type: "member", ID: "m1"}},
		{ID: "e2", Cursor: "c0002", Kind: "message", Text: "the scan", Author: contracts.Author{Type: "member", ID: "m1"},
			Attachments: []contracts.Attachment{{ID: "2.0", Name: "scan.pdf"}}},
	})
}

// The brief carries what the prefetch returned (#2673), and the prefetch runs
// under its own window, never the whole deadline.
func TestABriefCarriesWhatThePrefetchMarked(t *testing.T) {
	var window time.Duration
	r := newRig(t, Options{Prefetch: func(ctx context.Context, _ string, entries []contracts.Entry) []contracts.Entry {
		deadline, _ := ctx.Deadline()
		window = time.Until(deadline)
		out := append([]contracts.Entry(nil), entries...)
		out[0].Attachments = []contracts.Attachment{{ID: "a", LocalPath: "conversations/conv_1/a-x.pdf"}}
		return out
	}})
	withAttachment(r)
	envelope := ring("req_1", "c0001", "c0002")
	envelope.Deadline = time.Now().Add(time.Minute)
	r.ringWith(t, envelope)
	r.manager.background.Wait()
	if len(r.sink.briefs) != 1 || r.sink.briefs[0].Delta[0].Attachments[0].LocalPath != "conversations/conv_1/a-x.pdf" {
		t.Fatalf("briefed %+v", r.sink.briefs)
	}
	if window <= 0 || window > PrefetchMaxWindow {
		t.Fatalf("the prefetch ran for a window of %v", window)
	}
}

// A delta with nothing to fetch is briefed at once, on the ring's own goroutine.
func TestADeltaWithoutAttachmentsIsNotPrefetched(t *testing.T) {
	called := false
	r := newRig(t, Options{Prefetch: func(_ context.Context, _ string, entries []contracts.Entry) []contracts.Entry {
		called = true
		return entries
	}})
	r.ring(t, "req_1", "c0001", "c0002")
	if called || len(r.sink.briefs) != 1 {
		t.Fatalf("prefetched %v, briefed %d", called, len(r.sink.briefs))
	}
}

// 🔴 #2673 self-review H4: the window is a quarter of what is left of the
// request, and never more than PrefetchMaxWindow.
func TestThePrefetchWindowIsBounded(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	cases := map[string]struct {
		deadline time.Time
		want     time.Duration
	}{
		"no deadline":      {time.Time{}, PrefetchMaxWindow},
		"five minutes":     {now.Add(5 * time.Minute), PrefetchMaxWindow},
		"twenty seconds":   {now.Add(20 * time.Second), 5 * time.Second},
		"already past due": {now.Add(-time.Second), 0},
	}
	for name, tc := range cases {
		if got := PrefetchWindow(now, tc.deadline); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}

// 🔴 #2673 self-review H4: a stalled prefetch never holds the goroutine that
// delivered the ring — the one that carries the platform's cancel next — and a
// cancel handled meanwhile ends the prefetch and is never followed by a brief.
func TestAStalledPrefetchHoldsNothingAndACancelEndsIt(t *testing.T) {
	started := make(chan struct{})
	ended := make(chan struct{})
	r := newRig(t, Options{Prefetch: func(ctx context.Context, _ string, entries []contracts.Entry) []contracts.Entry {
		close(started)
		<-ctx.Done()
		close(ended)
		return entries
	}})
	withAttachment(r)
	returned := make(chan struct{})
	go func() {
		r.ring(t, "req_1", "c0001", "c0002")
		close(returned)
	}()
	<-started
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("the ring's delivery waited on the prefetch")
	}
	// The cancel arrives through the same doorbell, the way the platform's does.
	if err := r.host.DeliverCancel("req_1", contracts.CancelStop); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the cancel did not end the prefetch")
	}
	r.manager.background.Wait()
	if len(r.sink.briefs) != 0 || r.sink.cancels["req_1"] != contracts.CancelStop {
		t.Fatalf("briefed %d, cancels %v", len(r.sink.briefs), r.sink.cancels)
	}
}

// 🔴 A request withdrawn while its attachments were fetched is never briefed:
// the cancel already reached the plugin.
func TestARequestCancelledDuringThePrefetchIsNotBriefed(t *testing.T) {
	var r *rig
	r = newRig(t, Options{Prefetch: func(_ context.Context, requestID string, entries []contracts.Entry) []contracts.Entry {
		if err := r.manager.OnPlatformCancel(context.Background(), requestID, contracts.CancelStop); err != nil {
			t.Errorf("cancelling: %v", err)
		}
		return entries
	}})
	withAttachment(r)
	r.ring(t, "req_1", "c0001", "c0002")
	r.manager.background.Wait()
	if len(r.sink.briefs) != 0 {
		t.Fatalf("a cancelled request was briefed: %+v", r.sink.briefs)
	}
	if r.sink.cancels["req_1"] != contracts.CancelStop {
		t.Fatalf("the cancel did not reach the plugin: %v", r.sink.cancels)
	}
}

// A brief that fails to send after a prefetch is reported, not lost.
func TestABackgroundDeliveryFailureIsReported(t *testing.T) {
	var reported []string
	r := newRig(t, Options{
		Prefetch: func(_ context.Context, _ string, entries []contracts.Entry) []contracts.Entry { return entries },
		Report:   func(requestID string, _ error) { reported = append(reported, requestID) },
	})
	withAttachment(r)
	r.manager.Attach("conn-1", briefRefusingSink{newRecorder()}, 48000)
	r.ring(t, "req_1", "c0001", "c0002")
	r.manager.background.Wait()
	if len(reported) != 1 || reported[0] != "req_1" {
		t.Fatalf("reported %v", reported)
	}
}

type briefRefusingSink struct{ *recorder }

func (briefRefusingSink) SendBrief(context.Context, Brief) error {
	return errors.New("the plugin went away")
}
