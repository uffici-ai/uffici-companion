package gateway_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/infra/gateway"
	"github.com/uffici-ai/uffici-companion/internal/infra/gateway/gatewaytest"
	"github.com/uffici-ai/uffici-companion/internal/protocol"
)

const (
	conversationID = "a1b2c3d4-e5f6-4789-8abc-def012345678"
	requestID      = "6f1c0f7a-2f0e-4f2e-9b1e-0d6a0b5c7e21"
	ringID         = "0b9d2f4e-3c1a-4e8b-9f7d-5a6c8e0b1d23"
	instanceID     = "d4c3b2a1-f6e5-4d7c-8b9a-0f1e2d3c4b5a"
	// challengeMarker stands for an edge's HTML page. It must never reach a log.
	challengeMarker = "cf-challenge-marker-do-not-log"
)

// rig is one host against one double, with every log line and every backoff
// delay captured.
type rig struct {
	double *gatewaytest.Double
	host   *gateway.Host
	logs   *syncBuffer

	mu      sync.Mutex
	delays  []time.Duration
	skew    time.Duration
	rings   chan contracts.Ring
	cancels chan [2]string
	ended   chan string
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newRig(t *testing.T, configure ...func(*gatewaytest.Double)) *rig {
	t.Helper()
	return newRigWith(t, func() (string, string) { return "hermes", "0.21.3" }, configure...)
}

// newRigWith is newRig with the attached harness's facts under the test's control.
func newRigWith(t *testing.T, harness func() (string, string), configure ...func(*gatewaytest.Double)) *rig {
	t.Helper()
	double := gatewaytest.New()
	for _, apply := range configure {
		apply(double)
	}
	t.Cleanup(double.Close)
	double.Seed(conversationID, gatewaytest.Conversation{Offset: "1040", Entries: []protocol.Entry{
		entry("1040", "user", "u-1", false, "before the range"),
		entry("1041", "agent", "a-self", true, "my own answer"),
		entry("1042", "external", "c-1", false, "@smith can you triage the printer card?"),
		entry("1043", "agent", "a-2", false, "a colleague"),
	}})
	r := &rig{
		double: double, logs: &syncBuffer{},
		rings: make(chan contracts.Ring, 16), cancels: make(chan [2]string, 16), ended: make(chan string, 64),
	}
	host, err := gateway.New(gateway.Options{
		BaseURL: double.URL(), Key: gatewaytest.Key, InstanceID: instanceID, Version: "1.2.3",
		Harness:    harness,
		Plugin:     func() string { return "0.4.0" },
		HostFacts:  &protocol.HostFacts{OS: "linux", Arch: "amd64", OSRelease: "Debian GNU/Linux 13 (trixie)"},
		HTTPClient: double.Client(),
		Logger:     slog.New(slog.NewJSONHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Sleep:      r.sleep,
		Now:        r.now,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.host = host
	t.Cleanup(func() {
		_ = host.Close()
		// 🔴 The secret-and-challenge marker test runs on EVERY rig: no test in
		// this file may put the key, a ticket or an edge's page into a log line.
		for _, secret := range []string{gatewaytest.Key, "connect-ticket-", "conversation-ticket-", challengeMarker} {
			if strings.Contains(r.logs.String(), secret) {
				t.Errorf("a log line carries %q:\n%s", secret, r.logs.String())
			}
		}
	})
	return r
}

// now is the wall clock moved forward by whatever the test advanced it.
func (r *rig) now() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return time.Now().Add(r.skew)
}

func (r *rig) advance(d time.Duration) {
	r.mu.Lock()
	r.skew += d
	r.mu.Unlock()
}

// sleep records a delay and returns at once — except that backoff delays past
// the tenth block until the claim closes, so a loop that would spin forever
// against a refusing double is held still for the assertions.
func (r *rig) sleep(ctx context.Context, d time.Duration) error {
	r.mu.Lock()
	r.delays = append(r.delays, d)
	count := len(r.delays)
	r.mu.Unlock()
	if count > 10 {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (r *rig) backoffDelays() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []time.Duration
	for _, d := range r.delays {
		if d >= gateway.BackoffFloor/2 {
			out = append(out, d)
		}
	}
	return out
}

func (r *rig) open(t *testing.T) contracts.DoorbellClaim {
	t.Helper()
	claim, err := r.host.OpenDoorbell(context.Background(), contracts.Handlers{
		OnRing:       func(ring contracts.Ring) { r.rings <- ring },
		OnCancel:     func(id string, reason contracts.CancelReason) { r.cancels <- [2]string{id, string(reason)} },
		OnDisconnect: func(reason string) { r.ended <- reason },
	})
	if err != nil {
		t.Fatalf("OpenDoorbell: %v", err)
	}
	t.Cleanup(func() { _ = claim.Close() })
	return claim
}

func (r *rig) connected(t *testing.T) {
	t.Helper()
	waitFor(t, "the doorbell to connect", r.double.Connected)
}

func (r *rig) ring(t *testing.T) contracts.Ring {
	t.Helper()
	r.connected(t)
	if err := r.double.Ring(protocol.Ring{
		RingID: ringID, RequestID: requestID, ConversationID: conversationID, Reason: "mention",
		Cause:    protocol.RingCause{EntryID: "1042", Author: protocol.RingAuthor{Type: "external", ID: "c-1"}},
		Deadline: time.Now().Add(5 * time.Minute).UTC(), Since: "1040", Through: "1042",
	}); err != nil {
		t.Fatalf("Ring: %v", err)
	}
	select {
	case ring := <-r.rings:
		return ring
	case <-time.After(5 * time.Second):
		t.Fatal("OnRing was never called")
	}
	return contracts.Ring{}
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func nextEnd(t *testing.T, r *rig) string {
	t.Helper()
	select {
	case reason := <-r.ended:
		return reason
	case <-time.After(10 * time.Second):
		t.Fatal("OnDisconnect was never called")
	}
	return ""
}

func entry(cursor, authorType, authorID string, self bool, text string) protocol.Entry {
	return protocol.Entry{
		ID: cursor, Cursor: cursor, At: time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC), Kind: "message", Text: text,
		Author:      protocol.Author{Type: authorType, ID: authorID, Label: "Someone", Self: self},
		Attachments: []protocol.Attachment{},
	}
}

func classOf(err error) string {
	var classified contracts.ClassifiedError
	if errors.As(err, &classified) {
		return classified.ErrorClass()
	}
	return ""
}

// ── The acceptance journey ──────────────────────────────────────────────────

func TestTheWholeTurnAgainstTheDouble(t *testing.T) {
	r := newRig(t)
	r.open(t)
	ctx := context.Background()

	ring := r.ring(t)
	if ring.RingID != ringID || ring.RequestID != requestID || ring.ConversationID != conversationID {
		t.Fatalf("the ring arrived as %+v", ring)
	}
	if ring.Cause.AuthorType != "external" || ring.Cause.AuthorID != "c-1" || ring.Cause.EntryID != "1042" {
		t.Fatalf("the cause arrived as %+v", ring.Cause)
	}
	if ring.SinceCursor != "1040" || ring.ThroughCursor != "1042" {
		t.Fatalf("the range arrived as (%s, %s]", ring.SinceCursor, ring.ThroughCursor)
	}

	if err := r.host.Ack(ctx, ring.RingID); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	waitFor(t, "the ack", func() bool { return len(r.double.Acks()) == 1 })
	if got := r.double.Acks()[0]; got != ringID {
		t.Fatalf("acked %q, want %q", got, ringID)
	}

	delta, bounded, err := r.host.ReadSince(ctx, conversationID, ring.SinceCursor, ring.ThroughCursor)
	if err != nil || bounded {
		t.Fatalf("ReadSince: %v bounded=%v", err, bounded)
	}
	// 1041 is the agent's own; 1043 is past `through`.
	if len(delta) != 1 || delta[0].ID != "1042" {
		t.Fatalf("the delta is %+v, want only 1042", delta)
	}
	if delta[0].Author.Type != "correspondent" {
		t.Fatalf("an external author reached the wire as %q, want correspondent", delta[0].Author.Type)
	}

	opener, err := r.host.ReadOpener(ctx, conversationID, ring.ThroughCursor, 48000)
	if err != nil {
		t.Fatalf("ReadOpener: %v", err)
	}
	if len(opener) != 3 || !opener[1].Author.Self || opener[0].Author.Type != "member" {
		t.Fatalf("the opener is %+v, want 1040–1042 with the agent's own", opener)
	}

	messageID, err := r.host.Post(ctx, requestID, conversationID, "On it.", nil)
	if err != nil || messageID == "" {
		t.Fatalf("Post: %q %v", messageID, err)
	}
	if err := r.host.CloseRequest(ctx, requestID, contracts.OutcomeAnswered, ""); err != nil {
		t.Fatalf("CloseRequest: %v", err)
	}
	if offset, _ := r.double.OffsetOf(conversationID); offset != "1042" {
		t.Fatalf("the offset is %s after answering, want 1042", offset)
	}
	// One conversation ticket served every call: the cache is not re-minting.
	if got := r.double.Mints("conversation"); got != 1 {
		t.Fatalf("%d conversation tickets minted, want 1", got)
	}
	if refusals := r.double.Refusals(); len(refusals) != 0 {
		t.Fatalf("the double refused off-contract bodies: %v", refusals)
	}
	facts := r.double.ConnectFacts()
	if len(facts) == 0 || facts[0].InstanceID != instanceID || facts[0].Client.Name != "uffici-companion" ||
		facts[0].Client.Version != "1.2.3" || facts[0].Harness.Kind != "hermes" {
		t.Fatalf("the connection ticket was minted with %+v", facts)
	}
	// 🔴 #2236: the plugin's version and the machine's kind ride the same ticket —
	// the facts the Connect tab shows and an out-of-date notice will read.
	if facts[0].Plugin == nil || facts[0].Plugin.Version != "0.4.0" ||
		facts[0].Host == nil || facts[0].Host.OS != "linux" || facts[0].Host.OSRelease != "Debian GNU/Linux 13 (trixie)" {
		t.Fatalf("the connection ticket carried plugin %+v and host %+v", facts[0].Plugin, facts[0].Host)
	}
}

// 🔴 #2232: a question rides the post in the protocol's shape — the double
// validates every body against protocol/v1 and refuses one that is off-contract.
func TestAQuestionRidesThePostInTheProtocolsShape(t *testing.T) {
	r := newRig(t)
	r.open(t)
	r.ring(t)
	question := &contracts.Question{ID: "clarify_1", Choices: nil, MultiSelect: false}
	if _, err := r.host.Post(context.Background(), requestID, conversationID, "Anything else?", question); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if refusals := r.double.Refusals(); len(refusals) != 0 {
		t.Fatalf("the double refused the question post: %v", refusals)
	}
	posts := r.double.Posts()
	if len(posts) != 1 || posts[0].Question == nil || posts[0].Question.ID != "clarify_1" ||
		posts[0].Question.Choices == nil {
		t.Fatalf("the platform received %+v, want the question with an (empty) choice list", posts)
	}
}

func TestAPostWhoseAnswerWasLostIsOneMessage(t *testing.T) {
	r := newRig(t)
	r.open(t)
	r.ring(t)
	// The post lands; its answer does not. The retry carries the same postId.
	r.double.Inject("posts", gatewaytest.Injected{
		Status: http.StatusServiceUnavailable, Body: gatewaytest.Envelope("platform_unavailable", true, nil), AfterEffect: true,
	})
	messageID, err := r.host.Post(context.Background(), requestID, conversationID, "once", nil)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	posts := r.double.Posts()
	if len(posts) != 1 {
		t.Fatalf("%d messages recorded for one post, want 1 — the retry minted a new postId", len(posts))
	}
	if posts[0].MessageID != messageID {
		t.Fatalf("the retry answered %q, the recorded message is %q", messageID, posts[0].MessageID)
	}
}

func TestTwoPostsAreTwoMessages(t *testing.T) {
	// The other half of idempotency: the key is per CALL, not per request.
	r := newRig(t)
	r.open(t)
	r.ring(t)
	for range 2 {
		if _, err := r.host.Post(context.Background(), requestID, conversationID, "part", nil); err != nil {
			t.Fatalf("Post: %v", err)
		}
	}
	if got := len(r.double.Posts()); got != 2 {
		t.Fatalf("%d messages for two posts, want 2", got)
	}
}

func TestACancelReachesOnCancelInTheProtocolsWords(t *testing.T) {
	r := newRig(t)
	r.open(t)
	r.ring(t)
	if err := r.double.Cancel(requestID, "stop"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-r.cancels:
		if got[0] != requestID || contracts.CancelReason(got[1]) != contracts.CancelStop {
			t.Fatalf("OnCancel(%q, %q)", got[0], got[1])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnCancel was never called")
	}
}

func TestCloseMapsEveryClassOntoThePlatformsVocabulary(t *testing.T) {
	for _, testCase := range []struct{ companion, platform string }{
		{"model_error", "harness_error"},
		{"timeout", "harness_error"},
		{"redelivery_exhausted", "harness_error"},
		{"refused", "refused"},
		{"post_cap_exceeded", "over_budget"},
		{"platform_unavailable", "harness_error"},
	} {
		t.Run(testCase.companion, func(t *testing.T) {
			r := newRig(t)
			r.open(t)
			r.ring(t)
			if err := r.host.CloseRequest(context.Background(), requestID, contracts.OutcomeError, testCase.companion); err != nil {
				t.Fatalf("CloseRequest: %v (the platform refuses a class outside its vocabulary)", err)
			}
			closes := r.double.Closes()
			if len(closes) != 1 || closes[0].ErrorClass != testCase.platform {
				t.Fatalf("closed as %+v, want class %s", closes, testCase.platform)
			}
			if offset, _ := r.double.OffsetOf(conversationID); offset != "1040" {
				t.Fatalf("an error moved the offset to %s", offset)
			}
		})
	}
}

func TestACancelledCloseSendsNoClass(t *testing.T) {
	r := newRig(t)
	r.open(t)
	r.ring(t)
	if err := r.host.CloseRequest(context.Background(), requestID, contracts.OutcomeCancelled, "superseded"); err != nil {
		t.Fatalf("CloseRequest: %v", err)
	}
	if closes := r.double.Closes(); len(closes) != 1 || closes[0].ErrorClass != "" {
		t.Fatalf("closed as %+v, want no class on a cancel", closes)
	}
}

func TestClosingARequestThePlatformClosedIsNotAnError(t *testing.T) {
	r := newRig(t)
	r.open(t)
	r.ring(t)
	r.double.Inject("close", gatewaytest.Injected{Status: http.StatusConflict, Body: gatewaytest.Envelope("request_closed", false, nil)})
	if err := r.host.CloseRequest(context.Background(), requestID, contracts.OutcomeCancelled, ""); err != nil {
		t.Fatalf("closing an already-closed request failed: %v", err)
	}
}

func TestPostingToAClosedRequestIsClassified(t *testing.T) {
	r := newRig(t)
	r.open(t)
	r.ring(t)
	ctx := context.Background()
	if err := r.host.CloseRequest(ctx, requestID, contracts.OutcomeAnswered, ""); err != nil {
		t.Fatal(err)
	}
	_, err := r.host.Post(ctx, requestID, conversationID, "late", nil)
	if classOf(err) != contracts.ClassRequestClosed {
		t.Fatalf("a post to a closed request answered %v, want class request_closed", err)
	}
}

func TestAnUnrungRequestCannotBeClosed(t *testing.T) {
	r := newRig(t)
	if err := r.host.CloseRequest(context.Background(), requestID, contracts.OutcomeAnswered, ""); err == nil {
		t.Fatal("a close with no conversation ticket to carry it was accepted")
	}
}

// ── Errors are classes, relayed ─────────────────────────────────────────────

func TestAnEdgeChallengeIsClassifiedAndItsBodyNeverLogged(t *testing.T) {
	for name, injected := range map[string]gatewaytest.Injected{
		"an HTML page": {Status: http.StatusServiceUnavailable, ContentType: "text/html",
			Body: "<html><body>" + challengeMarker + "</body></html>"},
		"a 403 on a JSON route": {Status: http.StatusForbidden, ContentType: "application/json",
			Body: `{"message":"` + challengeMarker + `"}`},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.double.Inject("entries", injected)
			_, _, err := r.host.ReadSince(context.Background(), conversationID, "1040", "1042")
			if classOf(err) != gateway.ClassEdgeChallenge {
				t.Fatalf("answered %v, want edge_challenge", err)
			}
			var classified contracts.ClassifiedError
			if errors.As(err, &classified) && classified.ErrorInstruction() == "" {
				t.Fatal("an edge challenge carries no instruction to relay")
			}
			if strings.Contains(err.Error(), challengeMarker) {
				t.Fatal("the error string quotes the edge's page")
			}
		})
	}
}

func TestThePlatformsClassAndInstructionAreRelayedVerbatim(t *testing.T) {
	r := newRig(t)
	r.double.Inject("entries", gatewaytest.Injected{Status: http.StatusBadGateway, Body: gatewaytest.Envelope("platform_error", false, nil)})
	_, _, err := r.host.ReadSince(context.Background(), conversationID, "1040", "1042")
	var classified contracts.ClassifiedError
	if !errors.As(err, &classified) {
		t.Fatalf("answered %v, want a classified error", err)
	}
	if classified.ErrorClass() != "platform_error" || classified.ErrorInstruction() != "instruction for platform_error" {
		t.Fatalf("relayed %q / %q", classified.ErrorClass(), classified.ErrorInstruction())
	}
}

func TestAnUnreachableHostIsClassified(t *testing.T) {
	host, err := gateway.New(gateway.Options{
		BaseURL: "https://127.0.0.1:1", Key: gatewaytest.Key, InstanceID: instanceID,
		Sleep: func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = host.ReadSince(context.Background(), conversationID, "1", "2")
	if classOf(err) != gateway.ClassUnreachable {
		t.Fatalf("answered %v, want unreachable", err)
	}
	if strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatal("the error quotes the URL")
	}
}

func TestARateLimitIsWaitedOutForTheDelayItNames(t *testing.T) {
	r := newRig(t)
	r.double.Inject("entries", gatewaytest.Injected{
		Status: http.StatusTooManyRequests,
		Body:   gatewaytest.Envelope("rate_limited", true, map[string]string{"retryAfterSeconds": "12"}),
	})
	if _, _, err := r.host.ReadSince(context.Background(), conversationID, "1040", "1042"); err != nil {
		t.Fatalf("ReadSince after a rate limit: %v", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.delays) != 1 || r.delays[0] != 12*time.Second {
		t.Fatalf("waited %v, want exactly the 12s the platform named", r.delays)
	}
}

func TestAnExpiredTicketIsReMintedOnce(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if _, _, err := r.host.ReadSince(ctx, conversationID, "1040", "1042"); err != nil {
		t.Fatal(err)
	}
	r.double.Inject("entries",
		gatewaytest.Injected{Status: http.StatusUnauthorized, Body: gatewaytest.Envelope("ticket_expired", true, nil)})
	if _, _, err := r.host.ReadSince(ctx, conversationID, "1040", "1042"); err != nil {
		t.Fatalf("ReadSince after an expired ticket: %v", err)
	}
	if got := r.double.Mints("conversation"); got != 2 {
		t.Fatalf("%d conversation tickets minted, want 2 (the first, and one re-mint)", got)
	}
	// And only once: two refusals in a row are an answer, not a loop.
	r.double.Inject("entries",
		gatewaytest.Injected{Status: http.StatusUnauthorized, Body: gatewaytest.Envelope("ticket_invalid", false, nil)},
		gatewaytest.Injected{Status: http.StatusUnauthorized, Body: gatewaytest.Envelope("ticket_invalid", false, nil)})
	if _, _, err := r.host.ReadSince(ctx, conversationID, "1040", "1042"); classOf(err) != "ticket_invalid" {
		t.Fatalf("a second refusal answered %v, want ticket_invalid", err)
	}
}

func TestATicketCloseToExpiryIsReMintedBeforeUse(t *testing.T) {
	r := newRig(t, func(d *gatewaytest.Double) { d.ConversationTTL = 30 * time.Second })
	ctx := context.Background()
	for range 2 {
		if _, _, err := r.host.ReadSince(ctx, conversationID, "1040", "1042"); err != nil {
			t.Fatal(err)
		}
	}
	// 30 s of life is inside the 60 s renewal window: every call mints.
	if got := r.double.Mints("conversation"); got != 2 {
		t.Fatalf("%d mints for two calls under a 30s ticket, want 2", got)
	}
}

func TestAReadWithoutItsIdentityBlockIsRefused(t *testing.T) {
	r := newRig(t)
	r.double.Inject("entries", gatewaytest.Injected{Status: http.StatusOK, Body: `{"entries":[],"bounded":false}`})
	if _, _, err := r.host.ReadSince(context.Background(), conversationID, "1040", "1042"); classOf(err) != gateway.ClassPlatformError {
		t.Fatalf("a brief without its identity floor answered %v, want platform_error", err)
	}
}

// ── The doorbell's lifecycle and the reconnect policy ───────────────────────

func TestAnInstanceConflictIsRelayedAndHeldAtTheCeiling(t *testing.T) {
	r := newRig(t, func(d *gatewaytest.Double) { d.SetConflict(true) })
	r.open(t)
	for range 3 {
		if reason := nextEnd(t, r); reason != "instance_conflict" {
			t.Fatalf("OnDisconnect(%q), want instance_conflict", reason)
		}
	}
	waitFor(t, "three backoff delays", func() bool { return len(r.backoffDelays()) >= 3 })
	delays := r.backoffDelays()
	// 🔴 #2094: the FIRST 4409 may be this instance's own reconnect losing the
	// gate lock for two seconds — retried on the ordinary step. A repeat is a
	// real conflict and waits at the ceiling.
	if delays[0] >= gateway.BackoffCeiling {
		t.Fatalf("the first conflict waited %v — a gate-lock timeout would be stuck for a minute", delays[0])
	}
	for _, d := range delays[1:3] {
		if d != gateway.BackoffCeiling {
			t.Fatalf("a repeated conflict waited %v, want the %v ceiling (delays %v)", d, gateway.BackoffCeiling, delays)
		}
	}
}

func TestAConflictThatClearsReconnects(t *testing.T) {
	r := newRig(t, func(d *gatewaytest.Double) { d.SetConflict(true) })
	r.open(t)
	nextEnd(t, r)
	r.double.SetConflict(false)
	r.connected(t)
}

func TestARevokedKeyWaitsAtTheCeiling(t *testing.T) {
	r := newRig(t)
	r.open(t)
	r.connected(t)
	if err := r.double.Disconnect("key_revoked", 4403); err != nil {
		t.Fatal(err)
	}
	if reason := nextEnd(t, r); reason != "key_revoked" {
		t.Fatalf("OnDisconnect(%q)", reason)
	}
	waitFor(t, "a backoff delay", func() bool { return len(r.backoffDelays()) >= 1 })
	if d := r.backoffDelays()[0]; d != gateway.BackoffCeiling {
		t.Fatalf("a revoked key waited %v, want the ceiling", d)
	}
}

func TestARefusedKeyWaitsAtTheCeiling(t *testing.T) {
	r := newRig(t)
	r.double.Inject("connect", gatewaytest.Injected{Status: http.StatusUnauthorized, Body: gatewaytest.Envelope("unauthorized", false, nil)})
	r.open(t)
	if reason := nextEnd(t, r); reason != "unauthorized" {
		t.Fatalf("OnDisconnect(%q)", reason)
	}
	waitFor(t, "a backoff delay", func() bool { return len(r.backoffDelays()) >= 1 })
	if d := r.backoffDelays()[0]; d != gateway.BackoffCeiling {
		t.Fatalf("a refused key waited %v, want the ceiling", d)
	}
	r.connected(t)
}

func TestTheCloseCodeClassifiesAnEndWithNoDisconnectFrame(t *testing.T) {
	for _, testCase := range []struct {
		code   websocket.StatusCode
		reason string
		want   string
	}{
		{4409, "instance_conflict", "instance_conflict"},
		{4403, "key_revoked", "key_revoked"},
		{4401, "ticket_expired", gateway.DisconnectUnauthorized},
		{4408, "idle", "idle"},
		{4400, "protocol_error", "protocol_error"},
		{websocket.StatusGoingAway, "server_shutdown", "server_shutdown"},
		{websocket.StatusNormalClosure, "replaced", gateway.DisconnectReplaced},
		{websocket.StatusInternalError, "internal_error", gateway.DisconnectConnectionLost},
	} {
		t.Run(testCase.want+"/"+testCase.reason, func(t *testing.T) {
			r := newRig(t)
			r.open(t)
			r.connected(t)
			if err := r.double.CloseSocket(testCase.code, testCase.reason); err != nil {
				t.Fatal(err)
			}
			if got := nextEnd(t, r); got != testCase.want {
				t.Fatalf("close %d %q reported %q, want %q", testCase.code, testCase.reason, got, testCase.want)
			}
		})
	}
}

func TestAServerShutdownReconnectsPromptly(t *testing.T) {
	r := newRig(t)
	r.open(t)
	r.connected(t)
	if err := r.double.Disconnect("server_shutdown", websocket.StatusGoingAway); err != nil {
		t.Fatal(err)
	}
	nextEnd(t, r)
	waitFor(t, "a backoff delay", func() bool { return len(r.backoffDelays()) >= 1 })
	if d := r.backoffDelays()[0]; d != gateway.BackoffFloor {
		t.Fatalf("a drain waited %v, want %v", d, gateway.BackoffFloor)
	}
	waitFor(t, "the second connection", func() bool { return r.double.Upgrades() >= 2 && r.double.Connected() })
}

func TestASilentSocketIsAbandonedClosedAndReplaced(t *testing.T) {
	r := newRig(t, func(d *gatewaytest.Double) { d.HeartbeatSeconds = 1 })
	r.open(t)
	r.connected(t)
	r.double.SetSilent(true)
	if reason := nextEnd(t, r); reason != gateway.DisconnectConnectionLost {
		t.Fatalf("a silent socket reported %q", reason)
	}
	r.double.SetSilent(false)
	waitFor(t, "the reconnection", func() bool { return r.double.Upgrades() >= 2 && r.double.Connected() })
	// 🔴 #2094: the abandoned socket was CLOSED, not left for its replica to
	// time out — one live entry for this instance, never two.
	waitFor(t, "exactly one live socket", func() bool { return r.double.LiveSessions() == 1 })
}

func TestEveryConnectionMintsAFreshTicket(t *testing.T) {
	r := newRig(t)
	r.open(t)
	r.connected(t)
	if err := r.double.CloseSocket(websocket.StatusInternalError, "boom"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the reconnection", func() bool { return r.double.Upgrades() >= 2 && r.double.Connected() })
	if got := r.double.Mints("connect"); got < 2 {
		t.Fatalf("%d connection tickets for two connections — a spent ticket was reused", got)
	}
}

func TestAnEdgeRefusedUpgradeIsClassified(t *testing.T) {
	r := newRig(t)
	r.double.Inject("doorbell", gatewaytest.Injected{Status: http.StatusForbidden, ContentType: "text/html",
		Body: "<html>" + challengeMarker + "</html>"})
	r.open(t)
	if reason := nextEnd(t, r); reason != gateway.ClassEdgeChallenge {
		t.Fatalf("OnDisconnect(%q), want edge_challenge", reason)
	}
}

func TestADoorbellIsClaimedOnce(t *testing.T) {
	r := newRig(t)
	claim := r.open(t)
	if _, err := r.host.OpenDoorbell(context.Background(), contracts.Handlers{
		OnRing: func(contracts.Ring) {}, OnCancel: func(string, contracts.CancelReason) {}, OnDisconnect: func(string) {},
	}); err == nil {
		t.Fatal("a second doorbell was opened")
	}
	if err := claim.Close(); err != nil {
		t.Fatal(err)
	}
	if err := claim.Close(); err != nil {
		t.Fatalf("closing a claim twice: %v", err)
	}
}

func TestAnAckWithNoSocketIsClassified(t *testing.T) {
	r := newRig(t)
	if classOf(r.host.Ack(context.Background(), ringID)) != gateway.ClassUnreachable {
		t.Fatal("an ack with no doorbell connected was not classified unreachable")
	}
	if r.host.Ack(context.Background(), "") == nil {
		t.Fatal("an ack with no ring id was accepted")
	}
}

func TestNewRefusesAnIncompleteConfiguration(t *testing.T) {
	for name, options := range map[string]gateway.Options{
		"no URL":         {Key: "k", InstanceID: instanceID},
		"not a URL":      {BaseURL: "agents.uffici.dev", Key: "k", InstanceID: instanceID},
		"a wrong scheme": {BaseURL: "ftp://agents.uffici.dev", Key: "k", InstanceID: instanceID},
		"no key":         {BaseURL: "https://agents.uffici.dev", InstanceID: instanceID},
		"no instance":    {BaseURL: "https://agents.uffici.dev", Key: "k"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := gateway.New(options); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// 🔴 #2077 review: the ack goes out from the read loop, on the socket the ring
// came on — not behind a handler. A handler stuck on I/O used to delay it past
// the gateway's ack timeout, and the redelivery came back as a supersede.
func TestARingIsAckedWhileItsHandlerIsStillBusy(t *testing.T) {
	r := newRig(t)
	stuck := make(chan struct{})
	claim, err := r.host.OpenDoorbell(context.Background(), contracts.Handlers{
		OnRing:       func(contracts.Ring) { <-stuck },
		OnCancel:     func(string, contracts.CancelReason) {},
		OnDisconnect: func(string) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = claim.Close() })
	// Registered AFTER the claim's close, so it runs FIRST (cleanups are LIFO): the
	// close waits for the handler, and the handler waits for this.
	t.Cleanup(func() { close(stuck) })
	r.connected(t)
	if err := r.double.Ring(protocol.Ring{
		RingID: ringID, RequestID: requestID, ConversationID: conversationID, Reason: "mention",
		Cause:    protocol.RingCause{EntryID: "1042", Author: protocol.RingAuthor{Type: "user", ID: "u-1"}},
		Deadline: time.Now().Add(time.Minute).UTC(), Since: "1040", Through: "1042",
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the ack on receipt", func() bool { return len(r.double.Acks()) == 1 })
	// The companion's own on-receipt Ack of the same ring sends nothing twice.
	if err := r.host.Ack(context.Background(), ringID); err != nil {
		t.Fatalf("Ack of an acked ring: %v", err)
	}
	if got := len(r.double.Acks()); got != 1 {
		t.Fatalf("%d acks for one ring", got)
	}
}

// 🔴 Two processes on one instance id replace each other in turn: each reaches
// hello, then is `replaced`. The streak must survive the hello, or the pair
// swaps about once a second for ever (#2077 review).
func TestAReplacementFlapGoesToTheCeiling(t *testing.T) {
	r := newRig(t)
	r.open(t)
	for range 2 {
		waitFor(t, "a live socket", func() bool { return r.double.LiveSessions() == 1 })
		// The companion may tear this session down before the close lands (that
		// is the flap); either way it is gone, which is what this asks for.
		if err := r.double.CloseSocket(websocket.StatusNormalClosure, "replaced"); err != nil && !gatewaytest.IsGone(err) {
			t.Fatal(err)
		}
		if reason := nextEnd(t, r); reason != gateway.DisconnectReplaced {
			t.Fatalf("OnDisconnect(%q)", reason)
		}
	}
	waitFor(t, "two backoff delays", func() bool { return len(r.backoffDelays()) >= 2 })
	if d := r.backoffDelays()[1]; d != gateway.BackoffCeiling {
		t.Fatalf("a second replacement right after hello waited %v, want the ceiling", d)
	}
}

// 🔴 The other half of the flap rule: a connection that held for a whole
// heartbeat ENDS the streak, so a later, unrelated replacement takes the
// ordinary step. Read through the injected clock (#2106 review) — on the wall
// clock this path could not be exercised without waiting a heartbeat.
func TestAConnectionThatHeldAHeartbeatEndsTheConflictStreak(t *testing.T) {
	r := newRig(t)
	r.open(t)
	for i := range 2 {
		waitFor(t, "a live socket", func() bool { return r.double.LiveSessions() == 1 })
		if i == 1 {
			waitFor(t, "the second hello", func() bool { return strings.Count(r.logs.String(), "the doorbell is connected") >= 2 })
			r.advance(31 * time.Second)
		}
		// The companion may tear this session down before the close lands (that
		// is the flap); either way it is gone, which is what this asks for.
		if err := r.double.CloseSocket(websocket.StatusNormalClosure, "replaced"); err != nil && !gatewaytest.IsGone(err) {
			t.Fatal(err)
		}
		if reason := nextEnd(t, r); reason != gateway.DisconnectReplaced {
			t.Fatalf("OnDisconnect(%q)", reason)
		}
	}
	waitFor(t, "two backoff delays", func() bool { return len(r.backoffDelays()) >= 2 })
	if d := r.backoffDelays()[1]; d >= gateway.BackoffCeiling {
		t.Fatalf("a replacement after a connection held a heartbeat waited %v — the streak survived it", d)
	}
}

// 🔴 #2105: the daemon opens the doorbell before any plugin says hello, so the
// first connection reports the harness as `unknown`. Once the facts become
// known the doorbell reconnects ONCE to report them — presence then names the
// harness a person sees answering — and the handlers never hear of it.
func TestAPluginAttachingAfterTheDoorbellIsReportedOnceKnown(t *testing.T) {
	var mu sync.Mutex
	kind, version := "", ""
	harness := func() (string, string) {
		mu.Lock()
		defer mu.Unlock()
		return kind, version
	}
	r := newRigWith(t, harness, func(d *gatewaytest.Double) { d.HeartbeatSeconds = 1 })
	r.open(t)
	r.connected(t)
	if facts := r.double.ConnectFacts(); len(facts) != 1 || facts[0].Harness.Kind != "unknown" {
		t.Fatalf("the first connection reported %+v, want the harness unknown", facts)
	}

	mu.Lock()
	kind, version = "hermes", "0.21.4"
	mu.Unlock()
	waitFor(t, "a connection naming the harness", func() bool {
		facts := r.double.ConnectFacts()
		last := facts[len(facts)-1]
		return len(facts) == 2 && last.Harness.Kind == "hermes" && last.Harness.Version == "0.21.4" &&
			r.double.Connected()
	})
	// Once, not on every heartbeat: two more heartbeats pass with the one connection.
	time.Sleep(2500 * time.Millisecond)
	if got := r.double.Upgrades(); got != 2 {
		t.Fatalf("%d connections, want 2 — the harness was reported more than once", got)
	}
	select {
	case reason := <-r.ended:
		t.Fatalf("OnDisconnect heard %q — reporting the harness is not a disconnect", reason)
	default:
	}
	for _, d := range r.backoffDelays() {
		t.Fatalf("reporting the harness waited a backoff of %v", d)
	}
}
