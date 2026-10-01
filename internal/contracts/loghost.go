package contracts

import (
	"context"
	"time"
)

// LogHost is where the companion ships its own log lines, its plugin's and its
// harness's.
//
// 🔴 A capability of its OWN, beside AgentsHost rather than inside it:
// AgentsHost's header promises its methods stay unchanged, and log shipping is
// the lowest-priority thing this program does — a separate interface is what
// lets it run on a separate connection and never be mistaken for a call a
// conversation waits on.
type LogHost interface {
	// MintLogsTicket asks for the agent's log policy, and a ticket to push
	// under when shipping is on.
	MintLogsTicket(ctx context.Context) (LogsShipping, error)
	// PushLogs sends one batch under a ticket MintLogsTicket issued.
	PushLogs(ctx context.Context, ticket string, batch LogBatch) (LogPushResult, error)
}

// LogsShipping is the agent's log policy as the platform answers it.
type LogsShipping struct {
	// Enabled is false when an administrator switched shipping off for the
	// agent: nothing is sent, and the ring is cleared.
	Enabled bool
	// HarnessLevel is the platform's floor for the harness's own lines —
	// debug, info, warn or error. The machine's floor may be stricter.
	HarnessLevel string
	// Recheck is how long to wait before asking again.
	Recheck time.Duration
	// Ticket is the credential a push carries, empty when shipping is off.
	// 🔴 SECRET-CLASS: never logged.
	Ticket string
	// ExpiresAt is when the ticket stops being accepted.
	ExpiresAt time.Time
}

// LogEntry is one line on its way out, already redacted and clipped.
type LogEntry struct {
	TS         string
	Source     string
	Level      string
	Logger     string
	Message    string
	Attrs      string
	Redactions int
}

// LogClient describes who is shipping: this companion, and the harness and
// plugin it speaks for when one is attached.
type LogClient struct {
	Name           string
	Version        string
	HarnessKind    string
	HarnessVersion string
	PluginVersion  string
}

// LogBatch is one push.
type LogBatch struct {
	Client  LogClient
	Entries []LogEntry
}

// LogPushResult is what the platform took from a push.
type LogPushResult struct {
	Accepted int
	Filtered int
}

// ControlPlaneActivity reports whether conversation traffic is using the
// platform right now.
//
// 🔴 The log shipper waits for this to read false before it sends: its pushes
// must never compete with a call a person is waiting on.
type ControlPlaneActivity interface {
	// Busy is true while any control-plane call is in flight, or one started
	// within the last second.
	Busy() bool
}

// RetryAfterError is a failure that names how long to wait before trying
// again — a platform refusal's `retryAfterSeconds`.
type RetryAfterError interface {
	error
	RetryAfterDelay() time.Duration
}
