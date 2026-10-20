package gateway_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/infra/gateway"
	"github.com/uffici-ai/uffici-companion/internal/infra/gateway/gatewaytest"
)

const grantValue = "grant-value-do-not-log"

// filesHost is a gateway host whose files routes are served by handler.
func filesHost(t *testing.T, handler http.HandlerFunc) *gateway.Host {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	host, err := gateway.New(gateway.Options{
		BaseURL: server.URL, Key: gatewaytest.Key, InstanceID: instanceID,
		Sleep: func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return host
}

func TestADownloadNamesItsRangeAndItsGrant(t *testing.T) {
	var seen *http.Request
	host := filesHost(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r
		w.Header().Set("Content-Range", "bytes 4-9/10")
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Repr-Digest", "crc32c=:AAAAAA==:")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "456789")
	})
	meta, body, err := host.OpenDownload(context.Background(), contracts.TransferGrant{Grant: grantValue}, 4, `"v1"`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = body.Close() }()
	data, _ := io.ReadAll(body)
	if meta.Status != 206 || meta.Start != 4 || meta.Total != 10 || meta.ETag != `"v1"` || meta.CRC32C != "AAAAAA==" || string(data) != "456789" {
		t.Fatalf("meta %+v, body %q", meta, data)
	}
	if seen.URL.Path != "/v1/files/download" || seen.Header.Get("Authorization") != "Bearer "+grantValue ||
		seen.Header.Get("Range") != "bytes=4-" || seen.Header.Get("If-Range") != `"v1"` {
		t.Fatalf("asked %s %v", seen.URL.Path, seen.Header)
	}
}

func TestADownloadsRefusalsAreClassified(t *testing.T) {
	cases := map[string]struct {
		handler   http.HandlerFunc
		class     string
		retryable bool
	}{
		"a range past the file": {func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		}, gateway.ClassRangeNotSatisfiable, true},
		"a refused grant": {func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, gatewaytest.Envelope("grant_refused", false, nil))
		}, "grant_refused", false},
		"an outage naming its delay": {func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "4")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, gatewaytest.Envelope("platform_unavailable", false, nil))
		}, "platform_unavailable", true},
		"an edge page": {func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, "<html>challenge</html>")
		}, gateway.ClassEdgeChallenge, false},
		"a 206 with no range": {func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusPartialContent)
		}, gateway.ClassPlatformError, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			host := filesHost(t, tc.handler)
			_, body, err := host.OpenDownload(context.Background(), contracts.TransferGrant{Grant: grantValue}, 3, "")
			var classified *gateway.Error
			if body != nil || !errors.As(err, &classified) || classified.Class != tc.class || classified.Retryable != tc.retryable {
				t.Fatalf("answered %v", err)
			}
			if strings.Contains(err.Error(), grantValue) {
				t.Fatal("the error names the grant")
			}
			if name == "an outage naming its delay" && classified.RetryAfter != 4*time.Second {
				t.Fatalf("the delay was dropped: %v", classified.RetryAfter)
			}
		})
	}
}

func TestADownloadThatNeverReachedTheServerIsUnreachable(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	host, err := gateway.New(gateway.Options{BaseURL: server.URL, Key: gatewaytest.Key, InstanceID: instanceID})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = host.OpenDownload(context.Background(), contracts.TransferGrant{}, 0, "")
	var classified *gateway.Error
	if !errors.As(err, &classified) || classified.Class != gateway.ClassUnreachable {
		t.Fatalf("answered %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := host.OpenDownload(ctx, contracts.TransferGrant{}, 0, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("an ended context answered %v", err)
	}
	// An upload that cannot reach the server gives up after its attempts.
	err = host.UploadFile(context.Background(), contracts.TransferGrant{JTI: "j"}, bytes.NewReader([]byte("abc")),
		contracts.UploadSpec{Size: 3, ChunkBytes: 1, Attempts: 2})
	if !errors.As(err, &classified) || classified.Class != gateway.ClassUnreachable {
		t.Fatalf("an unreachable upload answered %v", err)
	}
	if err := host.UploadFile(context.Background(), contracts.TransferGrant{}, bytes.NewReader(nil),
		contracts.UploadSpec{ChunkBytes: 0}); err == nil {
		t.Fatal("an upload with no chunk size was accepted")
	}
}

func TestAGrantIsAskedOnlyForARungRequestAndCheckedOnTheWayBack(t *testing.T) {
	r := newRig(t)
	if _, err := r.host.GrantTransfer(context.Background(), requestID, contracts.TransferAsk{Op: contracts.TransferDownload, Path: "x"}); !errors.Is(err, contracts.ErrUnknownRequest) {
		t.Fatalf("an unrung request answered %v", err)
	}
	if _, err := r.host.CompleteTransfer(context.Background(), requestID, "j"); !errors.Is(err, contracts.ErrUnknownRequest) {
		t.Fatalf("an unrung completion answered %v", err)
	}
	if _, ok := r.host.ConversationOf(requestID); ok {
		t.Fatal("a request never rung has a conversation")
	}
	r.open(t)
	r.ring(t)
	if conversation, ok := r.host.ConversationOf(requestID); !ok || conversation != conversationID {
		t.Fatalf("ConversationOf = %q, %v", conversation, ok)
	}
	// An answer without its grant is not the protocol.
	r.double.Inject("transfers", gatewaytest.Injected{Status: http.StatusOK,
		Body: `{"op":"download","jti":"","grant":"","expiresAt":"2026-10-12T10:00:00Z","callUrn":"urn:uffici:tool_call:1","file":{"name":"a","size":1}}`})
	_, err := r.host.GrantTransfer(context.Background(), requestID, contracts.TransferAsk{Op: contracts.TransferDownload, Path: "x"})
	var classified *gateway.Error
	if !errors.As(err, &classified) || classified.Class != gateway.ClassPlatformError {
		t.Fatalf("a grantless answer read as %v", err)
	}
	// The platform's refusal comes back as its own, and is sent once.
	r.double.Inject("transfers", gatewaytest.Injected{Status: http.StatusForbidden, Body: gatewaytest.Envelope("transfer_denied", false, nil)})
	if _, err := r.host.GrantTransfer(context.Background(), requestID, contracts.TransferAsk{
		Op: contracts.TransferUpload, Filename: "a.txt", Size: 1, CRC32C: "AAAAAA==", ContentType: "text/plain",
	}); !errors.As(err, &classified) || classified.Class != "transfer_denied" {
		t.Fatalf("a refusal read as %v", err)
	}
	r.double.Inject("transfers", gatewaytest.Injected{Status: http.StatusOK, Body: `{}`})
	if _, err := r.host.CompleteTransfer(context.Background(), requestID, "0b9d2f4e-3c1a-4e8b-9f7d-5a6c8e0b1d23"); !errors.As(err, &classified) ||
		classified.Class != gateway.ClassPlatformError {
		t.Fatalf("an empty completion read as %v", err)
	}
	r.double.Inject("transfers", gatewaytest.Injected{Status: http.StatusConflict, Body: gatewaytest.Envelope("transfer_incomplete", false, nil)})
	if _, err := r.host.CompleteTransfer(context.Background(), requestID, "0b9d2f4e-3c1a-4e8b-9f7d-5a6c8e0b1d23"); !errors.As(err, &classified) ||
		classified.Class != "transfer_incomplete" {
		t.Fatalf("an incomplete upload read as %v", err)
	}
	if strings.Contains(r.logs.String(), "grant-") {
		t.Fatal("a grant reached the log")
	}
	if got := gateway.RetryDelay(1); got != 500*time.Millisecond {
		t.Fatalf("RetryDelay(1) = %v", got)
	}
}
