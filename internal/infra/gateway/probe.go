package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/coder/websocket"

	"github.com/uffici-ai/uffici-companion/internal/protocol"
)

// ProbeResult is what `uffici doctor` reports about the doorbell.
type ProbeResult struct {
	// Connected is true when the gateway answered the upgrade with `hello`.
	Connected bool
	// ConnectionID and HeartbeatSeconds are the hello's, when connected.
	ConnectionID     string
	HeartbeatSeconds int
	// Reason is why the probe did not connect: an error class from the mint, or
	// the doorbell's disconnect reason.
	Reason string
	// Instruction is the sentence the platform (or the companion) gave for it.
	Instruction string
}

// Probe proves the doorbell from this machine: mint a connection ticket, open
// the doorbell, read the first frame, close. It never touches a request.
//
// 🔴 It connects with this companion's own instance id, so a daemon already
// running here is REPLACED for a moment — the gateway keeps one socket per
// instance — and reconnects on its own within seconds. Probing with another id
// would be refused `instance_conflict` by that same daemon, which would report
// a healthy machine as a broken one.
func (h *Host) Probe(ctx context.Context) ProbeResult {
	ticket, err := h.api.mintConnect(ctx, h.harness(), h.version)
	if err != nil {
		return failedProbe(err)
	}
	conn, resp, err := websocket.Dial(ctx, h.api.base.JoinPath(DoorbellPath).String(), &websocket.DialOptions{
		HTTPClient: h.api.http,
		HTTPHeader: http.Header{
			"Authorization": []string{"Bearer " + ticket},
			"User-Agent":    []string{h.api.userAgent},
		},
	})
	if err != nil {
		if resp != nil && resp.Body != nil {
			// Never read (an edge's page), always closed: one leaked connection
			// per failed `uffici doctor` otherwise (#2106 review).
			_ = resp.Body.Close()
		}
		if resp != nil && resp.StatusCode == http.StatusForbidden {
			return failedProbe(localError(ClassEdgeChallenge, resp.StatusCode))
		}
		return failedProbe(localError(ClassUnreachable, 0))
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "doctor") }()
	conn.SetReadLimit(maxFrameBytes)

	_, raw, err := conn.Read(ctx)
	if err != nil {
		_, _, reason := ended(err, nil)
		return ProbeResult{Reason: reason}
	}
	var envelope protocol.Envelope
	_ = json.Unmarshal(raw, &envelope)
	switch envelope.Type {
	case protocol.FrameHello:
		var hello protocol.Hello
		_ = json.Unmarshal(raw, &hello)
		return ProbeResult{Connected: true, ConnectionID: hello.ConnectionID, HeartbeatSeconds: hello.HeartbeatSeconds}
	case protocol.FrameDisconnect:
		var frame protocol.Disconnect
		_ = json.Unmarshal(raw, &frame)
		return ProbeResult{Reason: frame.Reason, Instruction: frame.Instruction}
	default:
		return ProbeResult{Reason: ClassPlatformError, Instruction: localInstructions[ClassPlatformError]}
	}
}

func failedProbe(err error) ProbeResult {
	class, instruction := ClassPlatformError, localInstructions[ClassPlatformError]
	var classified *Error
	if errors.As(err, &classified) {
		class, instruction = classified.Class, classified.Instruction
	}
	return ProbeResult{Reason: class, Instruction: instruction}
}
