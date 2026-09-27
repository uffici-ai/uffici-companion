package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/infra/gateway"
	"github.com/uffici-ai/uffici-companion/internal/pairing"
	"github.com/uffici-ai/uffici-companion/internal/protocol"
)

// pairer is what `uffici pair` asks of the agents host.
type pairer interface {
	Open(ctx context.Context, harness protocol.HarnessFacts, version, publicKey string) (protocol.PairingResponse, error)
	Poll(ctx context.Context, pairingID, pollToken string) (protocol.PairingPollResponse, error)
}

// PairOptions carries what a pairing run needs beyond the Config.
type PairOptions struct {
	// Harness is the harness this companion will serve, as it declares it to the
	// admin who approves — facts for a person, never authority.
	Harness protocol.HarnessFacts
	// Pairer overrides the agents host (tests); nil builds the gateway's.
	Pairer pairer
	// Sleep waits between polls; nil sleeps for real.
	Sleep func(ctx context.Context, d time.Duration) error
	// Now is the clock the expiry is judged by; nil is the wall clock.
	Now func() time.Time
	// Confirm asks the person whether to install the key for the agent and org the
	// delivery names. 🔴 Asked BEFORE the key is written (#1966 review): a code
	// someone else learned can be approved by an admin of ANOTHER org, and only the
	// person who ran `uffici pair` can tell. Nil reads a yes/no from In.
	Confirm func(question string) bool
	// In is where the default Confirm reads the answer; nil answers no.
	In io.Reader
	// AssumeYes skips the question (`--yes`), for a script that checks the output.
	AssumeYes bool
}

// Pair is `uffici pair` (#1966): ask the agents host for a pairing, show the
// person the code, wait for an admin to approve it in the portal, and put the
// key it delivers into `$UFFICI_HOME/agent-key`.
//
// 🔴 The key is never printed and never logged. It arrives sealed to a key pair
// made for this one run and held only in memory; it is opened here and written
// straight to the file. What the person sees is the code, and afterwards the
// key's last four characters — the same four the portal shows the admin.
func Pair(ctx context.Context, config Config, out io.Writer, options PairOptions) int {
	say := func(format string, args ...any) { _, _ = fmt.Fprintf(out, format+"\n", args...) }
	now := options.Now
	if now == nil {
		now = time.Now
	}
	sleep := options.Sleep
	if sleep == nil {
		sleep = sleepFor
	}
	harness := options.Harness
	// The request's schema needs both facts; an unstated one is `unknown`, never "".
	if harness.Kind == "" {
		harness.Kind = "unknown"
	}
	if harness.Version == "" {
		harness.Version = "unknown"
	}
	instanceID, err := EnsureInstanceID(config.Home)
	if err != nil {
		say("uffici pair: %v", err)
		return ExitConfig
	}
	agentsHost := options.Pairer
	if agentsHost == nil {
		httpClient, clientErr := HTTPClient(config.CAFile)
		if clientErr != nil {
			say("uffici pair: %v", clientErr)
			return ExitConfig
		}
		built, buildErr := gateway.NewPairer(gateway.PairerOptions{
			BaseURL: config.GatewayURL, InstanceID: instanceID, Version: config.Version, HTTPClient: httpClient,
		})
		if buildErr != nil {
			say("uffici pair: %v", buildErr)
			return ExitConfig
		}
		agentsHost = built
	}

	key, err := pairing.NewKey()
	if err != nil {
		say("uffici pair: making this pairing's key: %v", err)
		return ExitConfig
	}
	opened, err := agentsHost.Open(ctx, harness, config.Version, pairing.PublicKey(key))
	if err != nil {
		say("uffici pair: %s", instructionOf(err))
		return ExitMiss
	}
	expiresAt, err := time.Parse(time.RFC3339, opened.ExpiresAt)
	if err != nil {
		expiresAt = now().Add(10 * time.Minute)
	}
	say("uffici pair")
	say("")
	say("  Code:  %s", opened.Code)
	say("")
	say("  An administrator enters this code on the agent's Advanced tab in the portal")
	say("  (Pair a harness). It expires at %s. Waiting for the approval…", expiresAt.Local().Format("15:04"))

	interval := time.Duration(opened.PollAfterSeconds) * time.Second
	if interval <= 0 {
		interval = 2 * time.Second
	}
	for {
		if !now().Before(expiresAt) {
			say("")
			say("uffici pair: the code expired before it was approved. Run `uffici pair` again for a new one.")
			return ExitMiss
		}
		if err := sleep(ctx, interval); err != nil {
			say("")
			say("uffici pair: stopped before the pairing was approved.")
			return ExitMiss
		}
		answer, err := agentsHost.Poll(ctx, opened.PairingID, opened.PollToken)
		if err != nil {
			var classified *gateway.Error
			if errors.As(err, &classified) && classified.Retryable {
				continue
			}
			say("")
			say("uffici pair: %s", instructionOf(err))
			return ExitMiss
		}
		if answer.Status == protocol.PairingPending {
			if answer.RetryAfterSeconds > 0 {
				interval = time.Duration(answer.RetryAfterSeconds) * time.Second
			}
			continue
		}
		binding := pairing.Binding{
			PairingID: opened.PairingID, AgentID: answer.AgentID, OrgSlug: answer.OrgSlug, AgentSlug: answer.AgentSlug,
		}
		agentKey, err := pairing.Open(pairing.Sealed(*answer.Sealed), key, binding)
		// The last four shown are the opened key's own, never the delivery's word for them.
		if err != nil || len(agentKey) < 4 || agentKey[len(agentKey)-4:] != answer.KeyLast4 {
			say("")
			say("uffici pair: the key that arrived could not be opened — run `uffici pair` again.")
			return ExitMiss
		}
		say("")
		say("  Approved: a key for agent %q in organization %q.", answer.AgentSlug, answer.OrgSlug)
		if !options.AssumeYes && !confirm(options, out, "  Install it on this machine? Only say yes if that is the agent you meant to pair. [y/N] ") {
			say("  Not installed. Nothing was written; ask an administrator to revoke that key if you did not expect it.")
			return ExitMiss
		}
		if err := WriteAgentKey(config.Home, agentKey); err != nil {
			say("")
			say("uffici pair: %v", err)
			return ExitConfig
		}
		say("")
		say("  Paired. The agent key ••••%s is in %s (mode 600). Start the companion: uffici", agentKey[len(agentKey)-4:], config.Home)
		return ExitOK
	}
}

// confirm asks the question, through the caller's Confirm or a line read from In.
func confirm(options PairOptions, out io.Writer, question string) bool {
	if options.Confirm != nil {
		return options.Confirm(question)
	}
	if options.In == nil {
		return false
	}
	_, _ = fmt.Fprint(out, question)
	line, _ := bufio.NewReader(options.In).ReadString('\n')
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

// instructionOf is the sentence a failure carries for a person, never its detail.
func instructionOf(err error) string {
	var classified *gateway.Error
	if errors.As(err, &classified) && classified.Instruction != "" {
		return classified.Instruction
	}
	return "the agents host could not be reached; check the network and UFFICI_GATEWAY_URL."
}

// sleepFor waits `d`, or less when the context ends first.
func sleepFor(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
