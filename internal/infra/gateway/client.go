package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/protocol"
)

// maxResponseBytes bounds what the companion reads from any answer. A read
// response is the largest the protocol has (a bounded transcript tail); an edge
// page past this is not worth reading to classify.
const maxResponseBytes = 8 << 20

// ticketRenewBefore is how long before its expiry a conversation ticket is
// re-minted, so a call never leaves with a ticket about to die in flight.
const ticketRenewBefore = 60 * time.Second

// client is the JSON control plane and the ticket mints, over HTTPS.
//
// 🔴 The agent key is used on the three mints and NOWHERE else: every other call
// carries a ticket (#2065). The key and every ticket are secret-class — they go
// into an `Authorization` header and never into a log line, an error string or
// a URL.
type client struct {
	base       *url.URL
	key        string
	instanceID string
	userAgent  string
	http       *http.Client
	now        func() time.Time

	mu      sync.Mutex
	tickets map[string]conversationTicket

	// inFlight and lastStart are the control plane's activity, read by the log
	// shipper through Host.Busy so it never competes with a call a person is
	// waiting on. Only the conversation client's are ever read.
	inFlight  atomic.Int32
	lastStart atomic.Int64
}

type conversationTicket struct {
	token     string
	expiresAt time.Time
}

// call is one HTTP exchange: a method, a path, the bearer, a body, a decoded answer.
func (c *client) call(ctx context.Context, method, path string, query url.Values, bearer string, body, out any) error {
	c.inFlight.Add(1)
	c.lastStart.Store(c.now().UnixNano())
	defer c.inFlight.Add(-1)
	target := c.base.JoinPath(path)
	if query != nil {
		target.RawQuery = query.Encode()
	}
	var reader io.Reader
	if body != nil {
		// No HTML escaping: a log line heavy in `<`, `>` or `&` would grow up to
		// six times on the wire and pass the body limit its size was built under.
		var raw bytes.Buffer
		encoder := json.NewEncoder(&raw)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(body); err != nil {
			return fmt.Errorf("gateway: encoding a request body: %w", err)
		}
		reader = &raw
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), reader)
	if err != nil {
		return fmt.Errorf("gateway: building a request: %w", err)
	}
	// Pairing's first call carries no credential at all (#1966), so no header.
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// 🔴 Never the transport error itself: it quotes the URL, and a DNS or
		// TLS message is detail for nobody the log reaches.
		return localError(ClassUnreachable, 0)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return localError(ClassUnreachable, resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return classifyResponse(resp.StatusCode, resp.Header.Get("Content-Type"), raw)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return localError(ClassPlatformError, resp.StatusCode)
	}
	return nil
}

// mintConnect mints a connection ticket — seconds-lived and single-use, so one
// per dial.
func (c *client) mintConnect(ctx context.Context, harness protocol.HarnessFacts, version string, plugin *protocol.PluginFacts, host *protocol.HostFacts) (string, error) {
	var answer protocol.TicketResponse
	err := c.call(ctx, http.MethodPost, "/v1/tickets/connect", nil, c.key, protocol.ConnectTicketRequest{
		InstanceID: c.instanceID,
		Client:     protocol.ClientFacts{Name: ClientName, Version: version},
		Harness:    harness,
		Plugin:     plugin,
		Host:       host,
	}, &answer)
	if err != nil {
		return "", err
	}
	if answer.Ticket == "" {
		return "", localError(ClassPlatformError, http.StatusOK)
	}
	return answer.Ticket, nil
}

// conversationTicket returns a live ticket for one conversation, minting one
// when there is none or the cached one is within a minute of expiring.
func (c *client) conversationTicket(ctx context.Context, conversationID string) (string, error) {
	c.mu.Lock()
	cached, ok := c.tickets[conversationID]
	c.mu.Unlock()
	if ok && c.now().Before(cached.expiresAt.Add(-ticketRenewBefore)) {
		return cached.token, nil
	}
	var answer protocol.ConversationTicketResponse
	err := c.call(ctx, http.MethodPost, "/v1/tickets/conversation", nil, c.key, protocol.ConversationTicketRequest{
		InstanceID: c.instanceID, ConversationID: conversationID,
	}, &answer)
	if err != nil {
		return "", err
	}
	if answer.Ticket == "" || answer.ConversationID != conversationID {
		return "", localError(ClassPlatformError, http.StatusOK)
	}
	c.mu.Lock()
	// Expired tickets go as a new one arrives: one entry per conversation ever
	// answered would otherwise live as long as the process.
	now := c.now()
	for id, ticket := range c.tickets {
		if !now.Before(ticket.expiresAt) {
			delete(c.tickets, id)
		}
	}
	c.tickets[conversationID] = conversationTicket{token: answer.Ticket, expiresAt: answer.ExpiresAt}
	c.mu.Unlock()
	return answer.Ticket, nil
}

// forget drops a conversation's ticket, so the next call mints a fresh one.
func (c *client) forget(conversationID string) {
	c.mu.Lock()
	delete(c.tickets, conversationID)
	c.mu.Unlock()
}

// ticketRefusals are the classes a fresh ticket cures.
var ticketRefusals = map[string]bool{
	"ticket_expired": true, "ticket_invalid": true, "ticket_spent": true, "ticket_audience": true,
}

// ticketed runs one control-plane call under the conversation's ticket.
//
// A refused ticket is re-minted ONCE and the call sent again; a retryable
// failure is sent again up to `attempts` times, honouring a named delay. The
// body is the same on every attempt, which is what makes a post safe to resend:
// its `postId` is the idempotency key.
func (c *client) ticketed(ctx context.Context, conversationID, method, path string, query url.Values, body, out any, attempts int, sleep sleeper) error {
	remint := true
	var last error
	for attempt := 0; attempt < attempts; attempt++ {
		ticket, err := c.conversationTicket(ctx, conversationID)
		if err == nil {
			err = c.call(ctx, method, path, query, ticket, body, out)
		}
		if err == nil {
			return nil
		}
		last = err
		if ticketRefusals[classOf(err)] && remint {
			remint = false
			c.forget(conversationID)
			attempt--
			continue
		}
		if !retryable(err) || attempt == attempts-1 {
			return err
		}
		delay := retryDelay(attempt)
		var classified *Error
		if errors.As(err, &classified) && classified.RetryAfter > delay {
			delay = classified.RetryAfter
		}
		if sleepErr := sleep(ctx, delay); sleepErr != nil {
			return sleepErr
		}
	}
	return last
}

// ticketedOnce sends one NON-IDEMPOTENT call under the conversation's ticket
// (#2349): the body leaves at most once per ticket the gateway accepted.
//
// What may be repeated is only what happened before the platform could act:
// the ticket mint (retried like any call, up to `attempts`), and a call the
// gateway refused for its ticket — it checks the ticket before it runs
// anything — which is re-minted ONCE and sent again. 🔴 Every other failure,
// a retryable one included, is returned as it came: a 503 or a lost connection
// after the body left may be a tool that ran.
func (c *client) ticketedOnce(ctx context.Context, conversationID, method, path string, body, out any, attempts int, sleep sleeper) error {
	remint := true
	for {
		ticket, err := c.mintWithRetries(ctx, conversationID, attempts, sleep)
		if err != nil {
			return err
		}
		err = c.call(ctx, method, path, nil, ticket, body, out)
		if err != nil && ticketRefusals[classOf(err)] && remint {
			remint = false
			c.forget(conversationID)
			continue
		}
		return err
	}
}

// mintWithRetries returns a live conversation ticket, retrying a retryable
// mint failure — a mint carries no tool and is safe to repeat.
func (c *client) mintWithRetries(ctx context.Context, conversationID string, attempts int, sleep sleeper) (string, error) {
	for attempt := 0; ; attempt++ {
		ticket, err := c.conversationTicket(ctx, conversationID)
		if err == nil || !retryable(err) || attempt >= attempts-1 {
			return ticket, err
		}
		delay := retryDelay(attempt)
		var classified *Error
		if errors.As(err, &classified) && classified.RetryAfter > delay {
			delay = classified.RetryAfter
		}
		if sleepErr := sleep(ctx, delay); sleepErr != nil {
			return "", sleepErr
		}
	}
}

// retryDelay is the pause before a control-plane retry: short, because a turn
// is waiting on it and the request has its own deadline behind that.
func retryDelay(attempt int) time.Duration {
	return time.Duration(250*(1<<attempt)) * time.Millisecond
}

// sleeper waits, or returns early when the context ends. Injected so tests do
// not sleep.
type sleeper func(ctx context.Context, d time.Duration) error

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
