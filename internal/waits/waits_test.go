package waits

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSleepWaitsOrEndsWithTheContext(t *testing.T) {
	if err := Sleep(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("a short sleep: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	if err := Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("an ended context: %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("the sleep outlived its context")
	}
}
