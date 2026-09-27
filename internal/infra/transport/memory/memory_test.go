package memory

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
)

// The transport is the seam every other test in this module stands on, so it
// needs its own: a broken pipe here would look like a broken surface everywhere.

func TestItSatisfiesTheContracts(_ *testing.T) {
	var _ contracts.Listener = (*Listener)(nil)
	var _ contracts.Conn = (*Conn)(nil)
}

func TestAFrameSentIsTheFrameReceived(t *testing.T) {
	ctx := context.Background()
	listener := NewListener("memory://test")
	t.Cleanup(func() { _ = listener.Close() })

	client, err := listener.Dial()
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	server, err := listener.Accept(ctx)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}

	want := []byte(`{"v":1,"type":"post","requestId":"r","text":"hi"}`)
	if err := client.Send(ctx, want); err != nil {
		t.Fatalf("send: %v", err)
	}
	got, err := server.Receive(ctx)
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("got %q, want %q — the transport must not touch the bytes", got, want)
	}
}

func TestEitherSideClosingEndsBothAndKeepsTheReason(t *testing.T) {
	ctx := context.Background()
	listener := NewListener("memory://test")
	t.Cleanup(func() { _ = listener.Close() })

	client, _ := listener.Dial()
	server, err := listener.Accept(ctx)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}

	// 🔴 The close REASON is the only thing a refused plugin learns, so it must
	// survive the close rather than being swallowed with the connection.
	if err := server.Close("unauthorized"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := client.CloseReason(); got != "unauthorized" {
		t.Fatalf("the peer sees close reason %q, want %q", got, "unauthorized")
	}

	// 🔴 Repeated on fresh pairs, because a single attempt is not a guard here.
	// Both channels are buffered, so an implementation that offers `done`
	// alongside a ready channel in one select refuses only about half the time —
	// one attempt would pass on most runs and read as a flake on the rest.
	for attempt := range 64 {
		peer, _ := listener.Dial()
		other, acceptErr := listener.Accept(ctx)
		if acceptErr != nil {
			t.Fatalf("accept %d: %v", attempt, acceptErr)
		}
		// 🔴 A frame is PUT IN the peer's inbox first, and that is what makes the
		// Receive assertion below mean anything: with an empty inbox the old
		// select had only `done` ready and took it every time, so the assertion
		// passed against the unfixed code and proved nothing. It also pins the
		// deliberate consequence — a frame buffered at close is DROPPED, never
		// served after the connection ended.
		if err := other.Send(ctx, []byte(`{"v":1}`)); err != nil {
			t.Fatalf("priming the inbox %d: %v", attempt, err)
		}
		if err := other.Close("unauthorized"); err != nil {
			t.Fatalf("close %d: %v", attempt, err)
		}
		if _, err := peer.Receive(ctx); !errors.Is(err, ErrClosed) {
			t.Fatalf("attempt %d: receive after close returned %v, want ErrClosed", attempt, err)
		}
		if err := peer.Send(ctx, []byte("{}")); !errors.Is(err, ErrClosed) {
			t.Fatalf("attempt %d: send after close returned %v, want ErrClosed", attempt, err)
		}
	}
	select {
	case <-client.Closed():
	case <-time.After(time.Second):
		t.Fatal("Closed() never fired, so a test would have to poll")
	}
}

func TestReceiveHonoursTheContext(t *testing.T) {
	listener := NewListener("memory://test")
	t.Cleanup(func() { _ = listener.Close() })
	client, _ := listener.Dial()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Receive(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("receive with a cancelled context returned %v, want context.Canceled", err)
	}
}

func TestAcceptHonoursTheContextRatherThanBlockingForever(t *testing.T) {
	listener := NewListener("memory://test")
	t.Cleanup(func() { _ = listener.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := listener.Accept(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("accept with no pending connection returned %v, want DeadlineExceeded", err)
	}
}

func TestDialAfterCloseIsRefused(t *testing.T) {
	listener := NewListener("memory://test")
	if err := listener.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := listener.Dial(); !errors.Is(err, ErrClosed) {
		t.Fatalf("dial after close returned %v, want ErrClosed", err)
	}
	// Closing twice must be safe: shutdown paths call it from more than one place.
	if err := listener.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestTheListenerAndConnDescribeThemselves(t *testing.T) {
	listener := NewListener("memory://described")
	t.Cleanup(func() { _ = listener.Close() })
	if listener.Addr() != "memory://described" {
		t.Fatalf("Addr() is %q", listener.Addr())
	}
	client, err := listener.Dial()
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	// The hint names the PLACE, never the peer: it reaches log lines.
	if hint := client.RemoteHint(); hint == "" || !contains(hint, "memory://described") {
		t.Fatalf("RemoteHint() is %q", hint)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(needle) == 0 || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
