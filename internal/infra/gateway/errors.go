package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/protocol"
)

// The companion's own classes, for failures the platform never got to classify.
const (
	// ClassEdgeChallenge — something between the companion and the gateway
	// answered instead of it: an HTML page, or a 403 on a JSON route. In
	// practice the CDN's bot protection.
	ClassEdgeChallenge = "edge_challenge"
	// ClassUnreachable — DNS, TCP or TLS failed; the gateway was never reached.
	ClassUnreachable = "unreachable"
	// ClassPlatformError — the gateway answered 2xx with a body this companion
	// cannot read as the protocol's shape.
	ClassPlatformError = "platform_error"
	// ClassRequestClosed — the platform's word for a request that is over.
	ClassRequestClosed = "request_closed"
)

// localInstructions are decision 32's sentences for the companion's own
// classes. The platform writes the instruction for every class it owns.
var localInstructions = map[string]string{
	ClassEdgeChallenge: "The network between this machine and the platform refused the companion's request " +
		"before the platform saw it; tell the person something went wrong, and check the path to the agents host.",
	ClassUnreachable: "The companion cannot reach the platform; tell the person something went wrong, " +
		"and check the network and the agents host address.",
	ClassPlatformError: "The platform answered in a shape this companion does not understand; tell the person " +
		"something went wrong, and update the companion.",
}

// Error is a classified failure: a class for the machine and an instruction the
// agent relays to the person (#1958 decision 32).
//
// 🔴 It carries NO body, URL, header or ticket. Its text is built from the
// class and the status alone, so an Error is always safe in a log line — the
// classified replacement logging.md asks for instead of an upstream error.
type Error struct {
	Class       string
	Instruction string
	Retryable   bool
	// RetryAfter is the delay a 429 named, or zero.
	RetryAfter time.Duration
	// Status is the HTTP status, or zero when none was received.
	Status int
}

func (e *Error) Error() string {
	if e.Status == 0 {
		return "gateway: " + e.Class
	}
	return fmt.Sprintf("gateway: %s (HTTP %d)", e.Class, e.Status)
}

// ErrorClass is the class, for callers that relay it without importing this
// package (`contracts.ClassifiedError`).
func (e *Error) ErrorClass() string { return e.Class }

// ErrorInstruction is the sentence the agent relays.
func (e *Error) ErrorInstruction() string { return e.Instruction }

// RetryAfterDelay is the delay the platform named, or zero
// (`contracts.RetryAfterError`).
func (e *Error) RetryAfterDelay() time.Duration { return e.RetryAfter }

func localError(class string, status int) *Error {
	return &Error{
		Class: class, Instruction: localInstructions[class], Status: status,
		// Transport failures heal on their own; an edge challenge and a body
		// this build cannot read do not.
		Retryable: class == ClassUnreachable,
	}
}

// classifyResponse turns a non-2xx answer into an Error.
//
// 🔴 The body is PARSED, never kept: an HTML page from a challenge can be tens
// of kilobytes of someone else's markup, and ledger class 18 is exactly an
// upstream body reaching a log line.
func classifyResponse(status int, contentType string, body []byte) *Error {
	var envelope protocol.ErrorEnvelope
	isJSON := strings.Contains(strings.ToLower(contentType), "json")
	if !isJSON || json.Unmarshal(body, &envelope) != nil || envelope.Error.Class == "" {
		// Not the gateway speaking. A 403 or any HTML is the edge; anything else
		// unreadable is the platform failing in a shape it did not mean to.
		if status == http.StatusForbidden || !isJSON {
			return localError(ClassEdgeChallenge, status)
		}
		return localError(ClassPlatformError, status)
	}
	out := &Error{
		Class:       envelope.Error.Class,
		Instruction: envelope.Error.Instruction,
		Retryable:   envelope.Error.Retryable,
		Status:      status,
	}
	if raw, ok := envelope.Error.Details["retryAfterSeconds"]; ok {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
			out.RetryAfter = time.Duration(seconds) * time.Second
		}
	}
	return out
}

// classOf reads the class of any error the host returns.
func classOf(err error) string {
	var classified *Error
	if errors.As(err, &classified) {
		return classified.Class
	}
	return ""
}

// retryable reports whether a failed call may be sent again as it was.
func retryable(err error) bool {
	var classified *Error
	return errors.As(err, &classified) && classified.Retryable
}
