package gateway_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/infra/gateway"
	"github.com/uffici-ai/uffici-companion/internal/infra/gateway/gatewaytest"
)

// seeingTransport records every path it carries, and announces each request's
// start on a channel so a test can wait for a push to be IN FLIGHT.
type seeingTransport struct {
	inner   http.RoundTripper
	mu      sync.Mutex
	paths   []string
	started chan string
}

func (s *seeingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	s.paths = append(s.paths, r.URL.Path)
	s.mu.Unlock()
	if s.started != nil {
		s.started <- r.URL.Path
	}
	return s.inner.RoundTrip(r)
}

func (s *seeingTransport) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths...)
}

func logsHost(t *testing.T, double *gatewaytest.Double, control, logs *seeingTransport, now func() time.Time) *gateway.Host {
	t.Helper()
	host, err := gateway.New(gateway.Options{
		BaseURL: double.URL(), Key: gatewaytest.Key, InstanceID: instanceID, Version: "1.2.3",
		HTTPClient:     &http.Client{Transport: control},
		LogsHTTPClient: &http.Client{Transport: logs},
		Now:            now,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close() })
	return host
}

func batch(lines ...string) contracts.LogBatch {
	b := contracts.LogBatch{Client: contracts.LogClient{Name: gateway.ClientName, Version: "1.2.3", HarnessKind: "hermes"}}
	for _, line := range lines {
		b.Entries = append(b.Entries, contracts.LogEntry{
			TS: "2026-09-29T12:00:00.000Z", Source: "companion", Level: "info", Message: line,
		})
	}
	return b
}

// 🔴 A push never shares the conversation client's connection pool: each
// transport sees only its own routes.
func TestLogTrafficRidesItsOwnTransport(t *testing.T) {
	double := gatewaytest.New()
	t.Cleanup(double.Close)
	double.Seed(conversationID, gatewaytest.Conversation{Offset: "1040"})
	control := &seeingTransport{inner: double.Client().Transport}
	logs := &seeingTransport{inner: double.Client().Transport}
	host := logsHost(t, double, control, logs, nil)
	ctx := context.Background()

	shipping, err := host.MintLogsTicket(ctx)
	if err != nil || !shipping.Enabled || shipping.Ticket == "" || shipping.HarnessLevel != "warn" ||
		shipping.Recheck != 15*time.Minute || shipping.ExpiresAt.IsZero() {
		t.Fatalf("MintLogsTicket: %+v %v", shipping, err)
	}
	result, err := host.PushLogs(ctx, shipping.Ticket, batch("one", "two"))
	if err != nil || result.Accepted != 2 {
		t.Fatalf("PushLogs: %+v %v", result, err)
	}
	if _, err := host.ReadSince(ctx, conversationID, "1040", "1040", 0); err != nil {
		t.Fatalf("ReadSince: %v", err)
	}

	for _, path := range control.seen() {
		if strings.Contains(path, "/logs") {
			t.Fatalf("the control plane's transport carried %s", path)
		}
	}
	logPaths := logs.seen()
	if len(logPaths) != 2 {
		t.Fatalf("the log transport carried %v", logPaths)
	}
	for _, path := range logPaths {
		if path != "/v1/tickets/logs" && path != "/v1/logs" {
			t.Fatalf("the log transport carried %s", path)
		}
	}
	if got := double.LogBatches(); len(got) != 1 || got[0].Client.HarnessKind != "hermes" {
		t.Fatalf("the double recorded %+v", got)
	}
	if refusals := double.Refusals(); len(refusals) != 0 {
		t.Fatalf("off-contract bodies: %v", refusals)
	}
}

// 🔴 No wall clock: a push HELD in flight at the gateway does not stop a
// control-plane call from completing.
func TestAHeldPushNeverBlocksTheControlPlane(t *testing.T) {
	double := gatewaytest.New()
	t.Cleanup(double.Close)
	double.Seed(conversationID, gatewaytest.Conversation{Offset: "1040"})
	control := &seeingTransport{inner: double.Client().Transport}
	logs := &seeingTransport{inner: double.Client().Transport, started: make(chan string, 4)}
	host := logsHost(t, double, control, logs, nil)
	ctx := context.Background()

	shipping, err := host.MintLogsTicket(ctx)
	if err != nil {
		t.Fatal(err)
	}
	<-logs.started
	release := double.HoldLogPushes()
	pushed := make(chan error, 1)
	go func() {
		_, err := host.PushLogs(ctx, shipping.Ticket, batch("held"))
		pushed <- err
	}()
	if path := <-logs.started; path != "/v1/logs" {
		t.Fatalf("expected the push in flight, saw %s", path)
	}
	if _, err := host.ReadSince(ctx, conversationID, "1040", "1040", 0); err != nil {
		t.Fatalf("the control plane waited on a held push: %v", err)
	}
	select {
	case err := <-pushed:
		t.Fatalf("the push finished before it was released: %v", err)
	default:
	}
	release()
	if err := <-pushed; err != nil {
		t.Fatalf("the released push: %v", err)
	}
}

func TestADisabledAgentGetsThePolicyAndNoTicket(t *testing.T) {
	double := gatewaytest.New()
	t.Cleanup(double.Close)
	double.SetLogsShipping(gatewaytest.LogsShipping{Enabled: false, HarnessLevel: "error"})
	control := &seeingTransport{inner: double.Client().Transport}
	logs := &seeingTransport{inner: double.Client().Transport}
	shipping, err := logsHost(t, double, control, logs, nil).MintLogsTicket(context.Background())
	if err != nil || shipping.Enabled || shipping.Ticket != "" || shipping.HarnessLevel != "error" {
		t.Fatalf("got %+v %v", shipping, err)
	}
}

func TestAnEnabledAnswerWithNoTicketIsAPlatformError(t *testing.T) {
	double := gatewaytest.New()
	t.Cleanup(double.Close)
	double.Inject("logs-ticket", gatewaytest.Injected{
		Status: http.StatusOK, ContentType: "application/json",
		Body: `{"shipping":{"enabled":true,"harnessLevel":"warn","recheckSeconds":900}}`,
	})
	control := &seeingTransport{inner: double.Client().Transport}
	logs := &seeingTransport{inner: double.Client().Transport}
	_, err := logsHost(t, double, control, logs, nil).MintLogsTicket(context.Background())
	var classified *gateway.Error
	if !errors.As(err, &classified) || classified.Class != gateway.ClassPlatformError {
		t.Fatalf("got %v", err)
	}
}

func TestARefusedPushCarriesItsClassAndTheNamedDelay(t *testing.T) {
	double := gatewaytest.New()
	t.Cleanup(double.Close)
	double.Inject("logs",
		gatewaytest.Injected{Status: http.StatusServiceUnavailable, ContentType: "application/json",
			Body: gatewaytest.Envelope("shed", true, map[string]string{"retryAfterSeconds": "30"})},
		gatewaytest.Injected{Status: http.StatusServiceUnavailable, ContentType: "text/html", Body: "<html>busy</html>"},
	)
	control := &seeingTransport{inner: double.Client().Transport}
	logs := &seeingTransport{inner: double.Client().Transport}
	host := logsHost(t, double, control, logs, nil)
	ctx := context.Background()
	shipping, _ := host.MintLogsTicket(ctx)

	_, err := host.PushLogs(ctx, shipping.Ticket, batch("x"))
	var named contracts.RetryAfterError
	if !errors.As(err, &named) || named.RetryAfterDelay() != 30*time.Second {
		t.Fatalf("shed: %v", err)
	}
	var classified contracts.ClassifiedError
	if !errors.As(err, &classified) || classified.ErrorClass() != "shed" {
		t.Fatalf("shed class: %v", err)
	}
	_, err = host.PushLogs(ctx, shipping.Ticket, batch("x"))
	if !errors.As(err, &classified) || classified.ErrorClass() != gateway.ClassEdgeChallenge {
		t.Fatalf("an edge page: %v", err)
	}
}

// Busy is the control plane's activity: a call in flight, or one started within
// the last second — never the log client's.
func TestBusyTracksTheControlPlaneOnly(t *testing.T) {
	double := gatewaytest.New()
	t.Cleanup(double.Close)
	double.Seed(conversationID, gatewaytest.Conversation{Offset: "1040"})
	clock := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return clock
	}
	control := &seeingTransport{inner: double.Client().Transport}
	logs := &seeingTransport{inner: double.Client().Transport}
	host := logsHost(t, double, control, logs, now)
	ctx := context.Background()
	if host.Busy() {
		t.Fatal("busy before any call")
	}
	shipping, _ := host.MintLogsTicket(ctx)
	if _, err := host.PushLogs(ctx, shipping.Ticket, batch("x")); err != nil {
		t.Fatal(err)
	}
	if host.Busy() {
		t.Fatal("log traffic made the control plane look busy")
	}
	if _, err := host.ReadSince(ctx, conversationID, "1040", "1040", 0); err != nil {
		t.Fatal(err)
	}
	if !host.Busy() {
		t.Fatal("not busy right after a control-plane call")
	}
	mu.Lock()
	clock = clock.Add(2 * time.Second)
	mu.Unlock()
	if host.Busy() {
		t.Fatal("still busy two seconds after the call ended")
	}
}

// bodyTransport records every request body it carries.
type bodyTransport struct {
	inner  http.RoundTripper
	mu     sync.Mutex
	bodies map[string]string
}

func (b *bodyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body != nil {
		raw, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(raw))
		b.mu.Lock()
		b.bodies[r.URL.Path] = string(raw)
		b.mu.Unlock()
	}
	return b.inner.RoundTrip(r)
}

// #2259 review: a line heavy in `<`, `>` and `&` goes on the wire as written —
// HTML escaping would grow it up to six times, past the limit its batch was sized for.
func TestAPushIsNotHTMLEscaped(t *testing.T) {
	double := gatewaytest.New()
	t.Cleanup(double.Close)
	logs := &bodyTransport{inner: double.Client().Transport, bodies: map[string]string{}}
	host, err := gateway.New(gateway.Options{
		BaseURL: double.URL(), Key: gatewaytest.Key, InstanceID: instanceID, Version: "1.2.3",
		HTTPClient: double.Client(), LogsHTTPClient: &http.Client{Transport: logs},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close() })
	ctx := context.Background()
	shipping, err := host.MintLogsTicket(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := host.PushLogs(ctx, shipping.Ticket, batch("<tag attr=\"x\">a & b</tag>")); err != nil {
		t.Fatal(err)
	}
	body := logs.bodies["/v1/logs"]
	if !strings.Contains(body, `<tag attr=\"x\">a & b</tag>`) || strings.Contains(body, "\\u003c") {
		t.Fatalf("the push body was escaped: %s", body)
	}
}
