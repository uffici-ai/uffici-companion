package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	tus "github.com/eventials/go-tus"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
)

func TestContentRangeIsRead(t *testing.T) {
	cases := map[string][3]int64{
		"bytes 10-99/100":                {10, 100, 1},
		"bytes 0-0/*":                    {0, -1, 1},
		"bytes 5-9":                      {0, 0, 0},
		"items 0-1/2":                    {0, 0, 0},
		"":                               {0, 0, 0},
		"bytes 99999999999999999999-1/2": {0, 0, 0},
		"bytes 0-1/99999999999999999999": {0, 0, 0},
	}
	for value, want := range cases {
		start, total, ok := contentRange(value)
		if ok != (want[2] == 1) || (ok && (start != want[0] || total != want[1])) {
			t.Errorf("contentRange(%q) = %d, %d, %v", value, start, total, ok)
		}
	}
}

func TestReprDigestNamesTheCRC32C(t *testing.T) {
	cases := map[string]string{
		"crc32c=:4waSgw==:":                 "4waSgw==",
		"sha-256=:abc=:, crc32c=:AAAAAA==:": "AAAAAA==",
		"sha-256=:abc=:":                    "",
		"crc32c=:not-base64:":               "",
		"":                                  "",
	}
	for value, want := range cases {
		if got := reprDigestCRC32C(value); got != want {
			t.Errorf("reprDigestCRC32C(%q) = %q, want %q", value, got, want)
		}
	}
}

// Every way go-tus or the transport can fail is classified — and only the
// classes that heal are retried.
func TestTusErrorsAreClassified(t *testing.T) {
	cases := []struct {
		err       error
		class     string
		retryable bool
	}{
		{tus.ErrOffsetMismatch, ClassPlatformError, true},
		{tus.ErrLargeUpload, ClassFileTooLarge, false},
		{tus.ErrUploadNotFound, ClassUploadGone, false},
		{tus.ErrVersionMismatch, ClassPlatformError, false},
		{errors.New("dial tcp: connection refused"), ClassUnreachable, true},
		{tus.ClientError{Code: 503, Body: []byte(`{"error":{"class":"platform_unavailable","instruction":"x","retryable":false}}`)}, "platform_unavailable", true},
		{tus.ClientError{Code: 460, Body: []byte(`{"error":{"class":"checksum_mismatch","instruction":"x","retryable":false}}`)}, "checksum_mismatch", false},
		{tus.ClientError{Code: 403, Body: []byte(`<html>challenge</html>`)}, ClassEdgeChallenge, false},
		{tus.ClientError{Code: 429, Body: []byte(`{"error":{"class":"rate_limited","instruction":"x","retryable":true,"details":{"retryAfterSeconds":"3"}}}`)}, "rate_limited", true},
		{&Error{Class: "already", Retryable: true}, "already", true},
	}
	for _, tc := range cases {
		got := tusError(tc.err)
		if got.Class != tc.class || got.Retryable != tc.retryable {
			t.Errorf("tusError(%v) = %s retryable=%v, want %s %v", tc.err, got.Class, got.Retryable, tc.class, tc.retryable)
		}
	}
	if got := tusError(tus.ClientError{Code: 429, Body: []byte(`{"error":{"class":"rate_limited","instruction":"x","retryable":true,"details":{"retryAfterSeconds":"3"}}}`)}); got.RetryAfter != 3*time.Second {
		t.Fatalf("the platform's delay was dropped: %v", got.RetryAfter)
	}
	if !(&Error{Retryable: true}).CanRetry() {
		t.Fatal("CanRetry")
	}
}

func TestATransferRetriesOnlyWhatHeals(t *testing.T) {
	var slept []time.Duration
	h := &FilesClient{Sleep: func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }}
	if h.retryTransfer(context.Background(), tus.ErrLargeUpload, 1, 4) {
		t.Fatal("a refusal was retried")
	}
	if h.retryTransfer(context.Background(), tus.ErrOffsetMismatch, 4, 4) {
		t.Fatal("the attempts were not bounded")
	}
	limited := tus.ClientError{Code: http.StatusTooManyRequests, Body: []byte(`{"error":{"class":"rate_limited","instruction":"x","retryable":true,"details":{"retryAfterSeconds":"9"}}}`)}
	if !h.retryTransfer(context.Background(), limited, 1, 4) || slept[len(slept)-1] != 9*time.Second {
		t.Fatalf("slept %v", slept)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if h.retryTransfer(ctx, tus.ErrOffsetMismatch, 1, 4) {
		t.Fatal("an ended context was retried")
	}
	if err := h.transferFailure(ctx, tus.ErrLargeUpload); !errors.Is(err, context.Canceled) {
		t.Fatalf("an ended upload answered %v", err)
	}
}

// 🔴 #2673 self-review L12: go-tus parks one goroutine per Uploader for the
// life of the process, holding the client and its header — so the grant is
// deleted from that header when the upload returns.
func TestAnUploadLeavesNoGrantInWhatGoTusKeeps(t *testing.T) {
	var sawGrant atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer grant-secret" {
			sawGrant.Store(true)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	var kept *tus.Config
	original := newTusClient
	newTusClient = func(url string, config *tus.Config) (*tus.Client, error) {
		kept = config
		return original(url, config)
	}
	t.Cleanup(func() { newTusClient = original })
	host, err := New(Options{
		BaseURL: server.URL, Key: "k", InstanceID: "i",
		Sleep: func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	err = host.UploadFile(context.Background(), contracts.TransferGrant{JTI: "j", Grant: "grant-secret"},
		bytes.NewReader([]byte("abc")), contracts.UploadSpec{Size: 3, ChunkBytes: 3, Attempts: 2})
	if err == nil || !sawGrant.Load() {
		t.Fatalf("the upload answered %v, grant sent %v", err, sawGrant.Load())
	}
	if kept == nil || kept.Header.Get("Authorization") != "" {
		t.Fatal("the grant outlived the upload in go-tus's client")
	}
}

// 🔴 #2673 self-review H4: a download whose server stops sending fails its
// attempt at the idle bound, and one whose headers never come at the header
// bound — never a hang until the request's deadline.
func TestADownloadThatGoesSilentFailsItsAttempt(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			// A resume whose answer never begins: no headers at all.
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("0123456789"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	// After the close in registration, so BEFORE it when run: a handler still
	// waiting would hold the server's Close forever.
	t.Cleanup(func() { close(release) })
	host, err := New(Options{
		BaseURL: server.URL, Key: "k", InstanceID: "i",
		FilesHeaderTimeout: 200 * time.Millisecond, FilesIdleTimeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	within(t, "a silent body", func() {
		_, body, err := host.OpenDownload(context.Background(), contracts.TransferGrant{Grant: "g"}, 0, "")
		if err != nil {
			t.Error(err)
			return
		}
		data, readErr := io.ReadAll(body)
		_ = body.Close()
		if readErr == nil || string(data) != "0123456789" {
			t.Errorf("a silent body read %q, %v", data, readErr)
		}
	})
	within(t, "silent headers", func() {
		if _, _, err := host.OpenDownload(context.Background(), contracts.TransferGrant{Grant: "g"}, 5, `"v"`); err == nil {
			t.Error("silent headers answered")
		}
	})
}

// within fails the test when fn has not returned in five seconds — a bound
// that never fired would otherwise hang the suite instead of failing it.
func within(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s was not bounded", what)
	}
}
