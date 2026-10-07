package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/infra/gateway"
	"github.com/uffici-ai/uffici-companion/internal/pairing"
	"github.com/uffici-ai/uffici-companion/internal/protocol"
)

// pairer is what `uffici pair` asks of the agents host.
type pairer interface {
	Open(ctx context.Context, harness protocol.HarnessFacts, version, publicKey, code string) (protocol.PairingResponse, error)
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
	// Code is a pairing code the portal issued (#2209) — the one in the command
	// an admin was given. With it nobody approves anything and no question is
	// asked: the admin who asked for the command chose the agent. Secret-class:
	// never printed, never logged. Empty is the typed-code path.
	Code string
}

// codeLifetime bounds how long a portal-issued code is worth retrying: the
// platform keeps it ten minutes, and nothing here knows when it was minted.
const codeLifetime = 10 * time.Minute

// pairingCodeShape is the portal's code: `XXXX-XXXX` over the alphabet with no
// 0/O, 1/I/L — the protocol's PAIRING_CODE_RE.
var pairingCodeShape = regexp.MustCompile(`^[ABCDEFGHJKMNPQRSTUVWXYZ23456789]{4}-[ABCDEFGHJKMNPQRSTUVWXYZ23456789]{4}$`)

// ParsePairingCode checks a code given on the command line before anything
// touches the network: case is forgiven, the shape and the alphabet are not.
// The error never repeats the value — a mistyped code is still a secret.
func ParsePairingCode(value string) (string, error) {
	code := strings.ToUpper(strings.TrimSpace(value))
	if !pairingCodeShape.MatchString(code) {
		return "", errors.New("is not a pairing code: eight letters and digits as XXXX-XXXX, from the command the portal gave you")
	}
	return code, nil
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
	withCode := options.Code != ""
	var opened protocol.PairingResponse
	if withCode {
		say("uffici pair")
		say("")
		say("  Pairing this machine with the code from the portal…")
		var exit int
		if opened, exit = openWithCode(ctx, agentsHost, harness, config.Version, pairing.PublicKey(key), options.Code, now, sleep, say); exit != ExitOK {
			return exit
		}
	} else {
		opened, err = agentsHost.Open(ctx, harness, config.Version, pairing.PublicKey(key), "")
		if err != nil {
			say("uffici pair: %s", instructionOf(err))
			return ExitMiss
		}
	}
	expiresAt, err := time.Parse(time.RFC3339, opened.ExpiresAt)
	if err != nil {
		expiresAt = now().Add(10 * time.Minute)
	}
	if !withCode {
		say("uffici pair")
		say("")
		say("  Code:  %s", opened.Code)
		say("")
		say("  An administrator enters this code on the agent's Advanced tab in the portal")
		say("  (Pair a harness). It expires at %s. Waiting for the approval…", expiresAt.Local().Format("15:04"))
	}

	interval := time.Duration(opened.PollAfterSeconds) * time.Second
	if interval <= 0 {
		interval = 2 * time.Second
	}
	for {
		if !now().Before(expiresAt) {
			say("")
			if withCode {
				// The portal may not have answered the claim in time and never sealed the key.
				say("uffici pair: the pairing did not complete in time — get a new command from the agent's page in the portal.")
			} else {
				say("uffici pair: the code expired before it was approved. Run `uffici pair` again for a new one.")
			}
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
		// With a portal-issued code the admin who asked for the command chose the
		// agent, and the code was bound to it: there is nothing left to ask.
		if !options.AssumeYes && !withCode && !confirm(options, out, "  Install it on this machine? Only say yes if that is the agent you meant to pair. [y/N] ") {
			say("  Not installed. Nothing was written; ask an administrator to revoke that key if you did not expect it.")
			return ExitMiss
		}
		if err := WriteAgentKey(config.Home, agentKey); err != nil {
			say("")
			say("uffici pair: %v", err)
			return ExitConfig
		}
		say("")
		say("  Paired. The agent key ••••%s is in %s (mode 600).", agentKey[len(agentKey)-4:], config.Home)
		say("  Start the companion: uffici — or, if it already runs as a service, restart it: the")
		say("  running process holds the key it started with (a key it replaced keeps working for a short overlap).")
		return ExitOK
	}
}

// openWithCode opens a pairing with a portal-issued code (#2209). A code the
// platform does not know — expired, used, or never issued — ends the run at
// once with the one instruction that helps; a platform that cannot answer is
// retried on a doubling delay until the code could no longer be alive.
func openWithCode(
	ctx context.Context, agentsHost pairer, harness protocol.HarnessFacts, version, publicKey, code string,
	now func() time.Time, sleep func(context.Context, time.Duration) error, say func(string, ...any),
) (protocol.PairingResponse, int) {
	deadline := now().Add(codeLifetime)
	delay := gateway.BackoffFloor
	for {
		opened, err := agentsHost.Open(ctx, harness, version, publicKey, code)
		if err == nil {
			return opened, ExitOK
		}
		var classified *gateway.Error
		isClassified := errors.As(err, &classified)
		if isClassified && classified.Class == "not_found" {
			say("")
			say("uffici pair: this pairing code is not valid any more — get a new command from the agent's page in the portal.")
			return protocol.PairingResponse{}, ExitConfig
		}
		// An unclassified failure is the network; a classified one says itself
		// whether sending it again can help.
		if isClassified && !classified.Retryable {
			say("")
			say("uffici pair: %s", instructionOf(err))
			return protocol.PairingResponse{}, ExitMiss
		}
		wait := delay
		if isClassified && classified.RetryAfter > wait {
			wait = classified.RetryAfter
		}
		if !now().Add(wait).Before(deadline) {
			say("")
			say("uffici pair: the platform could not be reached while the pairing code was valid. %s", instructionOf(err))
			say("  Get a new command from the agent's page in the portal and run it again.")
			return protocol.PairingResponse{}, ExitMiss
		}
		// One line per retry, so a person watching knows it is still trying — never the code.
		say("  The agents host is not answering; trying again in %s…", wait.Round(time.Second))
		if err := sleep(ctx, wait); err != nil {
			say("")
			say("uffici pair: stopped before the pairing completed.")
			return protocol.PairingResponse{}, ExitMiss
		}
		delay = min(delay*2, gateway.BackoffCeiling)
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
