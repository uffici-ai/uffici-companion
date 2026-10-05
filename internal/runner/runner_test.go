package runner

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/infra/fixture"
	"github.com/uffici-ai/uffici-companion/internal/requests"
)

// A script with one conversation and one member entry to answer.
func script(steps []fixture.Step, assertions fixture.Assertions) *fixture.Script {
	return &fixture.Script{
		V: 1,
		Conversations: map[string]*fixture.Conversation{
			"invoices": {
				ID: "conv_1", Offset: "c0001",
				Entries: []fixture.ScriptEntry{
					{ID: "e1", Cursor: "c0001", Kind: "message", Text: "first",
						Author: fixture.ScriptAuthor{Type: "member", ID: "m1", Label: "Ana"}},
					{ID: "e2", Cursor: "c0002", Kind: "message", Text: "@smith please look",
						Author: fixture.ScriptAuthor{Type: "member", ID: "m1", Label: "Ana"}},
				},
			},
		},
		Steps:  steps,
		Assert: assertions,
	}
}

// rig wires a runner to a fixture host and a manager, with a plugin that
// answers automatically — standing in for a harness.
type rig struct {
	runner  *Runner
	host    *fixture.Host
	manager *requests.Manager
}

// autoPlugin acks, posts and closes as soon as it receives a brief.
type autoPlugin struct {
	manager *requests.Manager
	session requests.Session
	answer  string
	// question makes the post a question (#2232).
	question *contracts.Question
	silent   bool
	// noClose posts and then stops, leaving the request open for a cancel.
	noClose bool
	// noAck never acknowledges: a plugin that drops before it could (#2290).
	// An ack moves the request past `delivered`, which is all a reconnect
	// redelivers, so a scenario about redelivery must not race one.
	noAck bool
	// beforePost runs between the ack and the post — a turn calling a tool (#2349).
	beforePost func(ctx context.Context, requestID string)

	briefsMu sync.Mutex
	// briefs is every brief the plugin was handed, in order (#2390).
	briefs []requests.Brief
}

func (a *autoPlugin) seen() []requests.Brief {
	a.briefsMu.Lock()
	defer a.briefsMu.Unlock()
	return append([]requests.Brief(nil), a.briefs...)
}

func (a *autoPlugin) SendBrief(ctx context.Context, brief requests.Brief) error {
	a.briefsMu.Lock()
	a.briefs = append(a.briefs, brief)
	a.briefsMu.Unlock()
	if a.noAck {
		return nil
	}
	go func() {
		_ = a.manager.OnAck(ctx, "conn-1", brief.RequestID, requests.AckAccepted, a.session)
		if a.silent {
			return
		}
		if a.beforePost != nil {
			a.beforePost(ctx, brief.RequestID)
		}
		_ = a.manager.OnPost(ctx, "conn-1", brief.RequestID, a.answer, extrasFor(a.question))
		if a.noClose {
			return
		}
		_ = a.manager.OnDone(ctx, "conn-1", brief.RequestID, contracts.OutcomeAnswered, "")
	}()
	return nil
}

func (a *autoPlugin) SendOpener(context.Context, string, []contracts.Entry) error { return nil }
func (a *autoPlugin) SendCancel(context.Context, string, contracts.CancelReason) error {
	return nil
}
func (a *autoPlugin) SendFailure(context.Context, string, string, string) error { return nil }

func newRig(t *testing.T, s *fixture.Script, plugin *autoPlugin) *rig {
	t.Helper()
	host, err := fixture.New(s)
	if err != nil {
		t.Fatalf("fixture.New: %v", err)
	}
	runner, err := New(s, host, nil)
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	manager := requests.New(host, requests.Options{Observer: runner.Observer()})
	plugin.manager = manager
	manager.Attach("conn-1", plugin, 48000)

	ctx := context.Background()
	claim, err := host.OpenDoorbell(ctx, contracts.Handlers{
		OnRing:       func(ring contracts.Ring) { _ = manager.OnRing(ctx, ring) },
		OnCancel:     func(id string, r contracts.CancelReason) { _ = manager.OnPlatformCancel(ctx, id, r) },
		OnDisconnect: func(string) {},
	})
	if err != nil {
		t.Fatalf("OpenDoorbell: %v", err)
	}
	t.Cleanup(func() { _ = claim.Close() })
	return &rig{runner: runner, host: host, manager: manager}
}

func TestAWholeScriptPasses(t *testing.T) {
	s := script([]fixture.Step{
		{Step: "ring", Conversation: "invoices", Reason: "mentioned", CauseEntry: "e2", ThroughCursor: "c0002"},
		{Step: "expectAck", Outcome: "accepted", Session: "fresh", WithinMs: 1000},
		{Step: "expectOpener", WithinMs: 1000},
		{Step: "expectPost", Matcher: "contains", Value: "filed", WithinMs: 1000},
		{Step: "expectClose", Outcome: "answered", WithinMs: 1000},
		{Step: "expectOffset", Conversation: "invoices", Cursor: "c0002"},
	}, fixture.Assertions{RingsDelivered: 1, OpenersSent: 1, Posts: 1})

	r := newRig(t, s, &autoPlugin{session: requests.SessionFresh, answer: "the invoice is filed"})
	result, err := r.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.OK() {
		t.Fatalf("the script failed: %v", result.Unmet)
	}
}

// 🔴 THE FLOOR. A script that rings nothing meets every expectation vacuously.
// Without the second half of OK(), `--once` would exit 0 having proved only
// that the daemon starts.
func TestAScriptThatRingsNothingIsNotAPass(t *testing.T) {
	result := Result{Unmet: nil, RingsDelivered: 0}
	if result.OK() {
		t.Fatal("a run with no ring reported OK — every expectation held over nothing at all")
	}
	if !(Result{RingsDelivered: 1}).OK() {
		t.Fatal("a run with a ring and no misses must pass")
	}
	if (Result{RingsDelivered: 1, Unmet: []string{"a miss"}}).OK() {
		t.Fatal("a run with an unmet expectation reported OK")
	}
}

// 🔴 The resumed-session rule, as a SCRIPT step: `expectNoOpener` is what makes
// the free local hop checkable from a fixture rather than only from a unit test.
func TestExpectNoOpenerCatchesAResumedSessionPayingForOne(t *testing.T) {
	steps := []fixture.Step{
		{Step: "ring", Conversation: "invoices", Reason: "mentioned", CauseEntry: "e2", ThroughCursor: "c0002"},
		{Step: "expectAck", Outcome: "accepted", Session: "resumed", WithinMs: 1000},
		{Step: "expectClose", Outcome: "answered", WithinMs: 1000},
		{Step: "expectNoOpener"},
	}
	r := newRig(t, script(steps, fixture.Assertions{RingsDelivered: 1}),
		&autoPlugin{session: requests.SessionResumed, answer: "done"})
	result, err := r.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.OK() {
		t.Fatalf("a resumed session should send no opener: %v", result.Unmet)
	}
	if result.Counts.Openers != 0 {
		t.Fatalf("%d opener(s) for a resumed session", result.Counts.Openers)
	}
}

func TestAnUnmetExpectationIsReportedWithItsStep(t *testing.T) {
	steps := []fixture.Step{
		{Step: "ring", Conversation: "invoices", Reason: "mentioned", CauseEntry: "e2", ThroughCursor: "c0002"},
		// The plugin never posts, so this must miss.
		{Step: "expectPost", Matcher: "contains", Value: "never happens", WithinMs: 100},
	}
	r := newRig(t, script(steps, fixture.Assertions{}), &autoPlugin{session: requests.SessionFresh, silent: true})
	result, err := r.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.OK() {
		t.Fatal("a script whose post never arrived reported OK")
	}
	if len(result.Unmet) != 1 || !strings.Contains(result.Unmet[0], "expectPost") {
		t.Fatalf("the miss does not name its step: %v", result.Unmet)
	}
}

// The terminal tally is a second, independent check: the steps can all pass
// while the run did more than the script intended.
func TestTheTerminalTallyCatchesAnUnexpectedCount(t *testing.T) {
	steps := []fixture.Step{
		{Step: "ring", Conversation: "invoices", Reason: "mentioned", CauseEntry: "e2", ThroughCursor: "c0002"},
		{Step: "expectClose", Outcome: "answered", WithinMs: 1000},
	}
	// The script claims two rings; only one is played.
	r := newRig(t, script(steps, fixture.Assertions{RingsDelivered: 2}),
		&autoPlugin{session: requests.SessionFresh, answer: "done"})
	result, err := r.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.OK() {
		t.Fatal("the tally did not notice one ring where two were asserted")
	}
	if !strings.Contains(strings.Join(result.Unmet, " "), "ring(s) delivered") {
		t.Fatalf("the tally's miss is not about rings: %v", result.Unmet)
	}
}

func TestOffsetExpectationsReadTheStore(t *testing.T) {
	steps := []fixture.Step{
		{Step: "ring", Conversation: "invoices", Reason: "mentioned", CauseEntry: "e2", ThroughCursor: "c0002"},
		{Step: "expectClose", Outcome: "answered", WithinMs: 1000},
		// Wrong on purpose: the offset moved to c0002.
		{Step: "expectOffset", Conversation: "invoices", Cursor: "c0001"},
	}
	r := newRig(t, script(steps, fixture.Assertions{RingsDelivered: 1}),
		&autoPlugin{session: requests.SessionFresh, answer: "done"})
	result, err := r.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.OK() {
		t.Fatal("an offset expectation that does not hold reported OK")
	}
	if !strings.Contains(strings.Join(result.Unmet, " "), "the offset is") {
		t.Fatalf("the miss does not describe the offset: %v", result.Unmet)
	}
}

func TestAppendEntriesReachesTheNextDelta(t *testing.T) {
	steps := []fixture.Step{
		{Step: "appendEntries", Conversation: "invoices", Entries: []fixture.ScriptEntry{
			{ID: "e3", Cursor: "c0003", Kind: "message", Text: "and another",
				Author: fixture.ScriptAuthor{Type: "member", ID: "m1", Label: "Ana"}},
		}},
		{Step: "ring", Conversation: "invoices", Reason: "mentioned", CauseEntry: "e2", ThroughCursor: "c0003"},
		{Step: "expectClose", Outcome: "answered", WithinMs: 1000},
		{Step: "expectOffset", Conversation: "invoices", Cursor: "c0003"},
	}
	r := newRig(t, script(steps, fixture.Assertions{RingsDelivered: 1}),
		&autoPlugin{session: requests.SessionFresh, answer: "done"})
	result, err := r.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.OK() {
		t.Fatalf("appended entries did not reach the turn: %v", result.Unmet)
	}
}

func TestStepsNeedingAPluginSaySoWhenThereIsNone(t *testing.T) {
	for _, step := range []fixture.Step{{Step: "disconnectPlugin"}, {Step: "secondConnection"}} {
		steps := []fixture.Step{
			{Step: "ring", Conversation: "invoices", Reason: "mentioned", CauseEntry: "e2", ThroughCursor: "c0002"},
			step,
		}
		r := newRig(t, script(steps, fixture.Assertions{}), &autoPlugin{session: requests.SessionFresh, answer: "x"})
		result, err := r.runner.Run(context.Background())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if result.OK() {
			t.Fatalf("%s reported OK with no plugin control — it would silently do nothing", step.Step)
		}
	}
}

func TestNewRefusesAnIncompleteRunner(t *testing.T) {
	if _, err := New(nil, nil, nil); err == nil {
		t.Fatal("a runner was built with no script and no host")
	}
}

// A cancel injected after the first post is how a script exercises "a person
// pressed stop mid-turn" — the oversight control the platform depends on.
func TestInjectCancelAfterTheFirstPostWithdrawsTheRequest(t *testing.T) {
	steps := []fixture.Step{
		{Step: "ring", Conversation: "invoices", Reason: "mentioned", CauseEntry: "e2", ThroughCursor: "c0002"},
		{Step: "expectPost", Matcher: "nonEmpty", WithinMs: 1000},
		{Step: "injectCancel", Reason: "stopped", AfterFirstPost: true, WithinMs: 1000},
		// 🔴 The offset must NOT move: a stopped turn answered nothing, so its
		// entries have to stay in range for whoever answers next.
		{Step: "expectOffset", Conversation: "invoices", Cursor: "c0001"},
	}
	// A plugin that posts but never closes, so the cancel has something to stop.
	r := newRig(t, script(steps, fixture.Assertions{RingsDelivered: 1}),
		&autoPlugin{session: requests.SessionFresh, answer: "working on it", noClose: true})
	result, err := r.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.OK() {
		t.Fatalf("the cancel scenario failed: %v", result.Unmet)
	}
	if result.Counts.Cancels["stopped"] != 1 {
		t.Fatalf("cancels: %v, want one stopped", result.Counts.Cancels)
	}
}

// expectNoPost is a negative over a window: it must pass when the plugin stays
// quiet and fail when it does not.
func TestExpectNoPostBothWays(t *testing.T) {
	quiet := []fixture.Step{
		{Step: "ring", Conversation: "invoices", Reason: "mentioned", CauseEntry: "e2", ThroughCursor: "c0002"},
		{Step: "expectNoPost", ForMs: 50},
	}
	r := newRig(t, script(quiet, fixture.Assertions{RingsDelivered: 1}),
		&autoPlugin{session: requests.SessionFresh, silent: true})
	result, err := r.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.OK() {
		t.Fatalf("a silent plugin should satisfy expectNoPost: %v", result.Unmet)
	}

	noisy := []fixture.Step{
		{Step: "ring", Conversation: "invoices", Reason: "mentioned", CauseEntry: "e2", ThroughCursor: "c0002"},
		{Step: "expectNoPost", ForMs: 200},
	}
	r2 := newRig(t, script(noisy, fixture.Assertions{RingsDelivered: 1}),
		&autoPlugin{session: requests.SessionFresh, answer: "but I did post"})
	result2, err := r2.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result2.OK() {
		t.Fatal("a post arrived and expectNoPost still passed")
	}
}

// A `wait` step must actually wait, or timing-sensitive scripts are fiction.
func TestWaitTakesTime(t *testing.T) {
	steps := []fixture.Step{
		{Step: "ring", Conversation: "invoices", Reason: "mentioned", CauseEntry: "e2", ThroughCursor: "c0002"},
		{Step: "wait", Ms: 60},
	}
	r := newRig(t, script(steps, fixture.Assertions{RingsDelivered: 1}),
		&autoPlugin{session: requests.SessionFresh, answer: "done"})
	start := time.Now()
	if _, err := r.runner.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("the script finished in %v, so `wait` did not wait", elapsed)
	}
}

// A cancel with nothing open must be reported, not silently ignored.
func TestInjectCancelWithNothingOpenIsAMiss(t *testing.T) {
	steps := []fixture.Step{
		{Step: "ring", Conversation: "invoices", Reason: "mentioned", CauseEntry: "e2", ThroughCursor: "c0002"},
		{Step: "expectClose", Outcome: "answered", WithinMs: 1000},
		{Step: "injectCancel", Reason: "stopped"},
	}
	r := newRig(t, script(steps, fixture.Assertions{}),
		&autoPlugin{session: requests.SessionFresh, answer: "done"})
	result, err := r.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.OK() {
		t.Fatal("a cancel with no open request passed silently")
	}
}

// fakePlugin stands in for the daemon's control over the attached plugin.
type fakePlugin struct {
	disconnects  int
	secondReason string
	secondErr    error
	onDisconnect func()
	// detached makes Attached() report false — a plugin that never turns up.
	detached bool
}

func (f *fakePlugin) Disconnect() error {
	f.disconnects++
	if f.onDisconnect != nil {
		f.onDisconnect()
	}
	return nil
}

func (f *fakePlugin) DialSecond(context.Context) (string, error) {
	return f.secondReason, f.secondErr
}

func (f *fakePlugin) Attached() bool { return !f.detached }

// 🔴 Acceptance criterion: a plugin that goes away and comes back gets the
// brief AGAIN. Without redelivery a harness restart silently drops whatever was
// in flight, and the platform waits on a request nobody is answering.
func TestDisconnectAndRedelivery(t *testing.T) {
	steps := []fixture.Step{
		{Step: "ring", Conversation: "invoices", Reason: "mentioned", CauseEntry: "e2", ThroughCursor: "c0002"},
		{Step: "disconnectPlugin"},
		{Step: "expectRedelivery", WithinMs: 1000},
	}
	s := script(steps, fixture.Assertions{RingsDelivered: 1})
	host, err := fixture.New(s)
	if err != nil {
		t.Fatalf("fixture.New: %v", err)
	}
	plugin := &fakePlugin{}
	runner, err := New(s, host, plugin)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	manager := requests.New(host, requests.Options{Observer: runner.Observer()})
	// 🔴 A plugin that drops BEFORE acknowledging (#2290): the fake's ack runs on
	// a goroutine, and one that won the race to the disconnect left nothing to
	// redeliver — the release gate failed on exactly that.
	sink := &autoPlugin{manager: manager, session: requests.SessionFresh, silent: true, noAck: true}
	manager.Attach("conn-1", sink, 48000)
	// Disconnecting detaches and then redelivers, which is what the daemon does
	// when a plugin reconnects.
	plugin.onDisconnect = func() {
		manager.Detach("conn-1")
		manager.Attach("conn-2", sink, 48000)
		_ = manager.RedeliverOpen(context.Background())
	}

	ctx := context.Background()
	claim, err := host.OpenDoorbell(ctx, contracts.Handlers{
		OnRing:       func(ring contracts.Ring) { _ = manager.OnRing(ctx, ring) },
		OnCancel:     func(id string, r contracts.CancelReason) { _ = manager.OnPlatformCancel(ctx, id, r) },
		OnDisconnect: func(string) {},
	})
	if err != nil {
		t.Fatalf("OpenDoorbell: %v", err)
	}
	t.Cleanup(func() { _ = claim.Close() })

	result, err := runner.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.OK() {
		t.Fatalf("the reconnect scenario failed: %v", result.Unmet)
	}
	if plugin.disconnects != 1 {
		t.Fatalf("%d disconnect(s), want 1", plugin.disconnects)
	}
	if result.Counts.Redeliveries != 1 {
		t.Fatalf("%d redelivery/ies, want 1", result.Counts.Redeliveries)
	}
}

// 🔴 Acceptance criterion: a second plugin is refused with `already_attached`.
// Two plugins answering one agent's briefs would post twice per turn.
func TestSecondConnectionMustBeRefused(t *testing.T) {
	steps := []fixture.Step{
		{Step: "ring", Conversation: "invoices", Reason: "mentioned", CauseEntry: "e2", ThroughCursor: "c0002"},
		{Step: "secondConnection"},
	}
	for name, plugin := range map[string]*fakePlugin{
		"refused as it must be": {secondReason: "already_attached"},
		"wrongly admitted":      {secondReason: ""},
	} {
		t.Run(name, func(t *testing.T) {
			s := script(steps, fixture.Assertions{RingsDelivered: 1})
			host, err := fixture.New(s)
			if err != nil {
				t.Fatalf("fixture.New: %v", err)
			}
			runner, err := New(s, host, plugin)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			manager := requests.New(host, requests.Options{Observer: runner.Observer()})
			manager.Attach("conn-1", &autoPlugin{manager: manager, session: requests.SessionFresh, silent: true}, 48000)

			ctx := context.Background()
			claim, err := host.OpenDoorbell(ctx, contracts.Handlers{
				OnRing:       func(ring contracts.Ring) { _ = manager.OnRing(ctx, ring) },
				OnCancel:     func(string, contracts.CancelReason) {},
				OnDisconnect: func(string) {},
			})
			if err != nil {
				t.Fatalf("OpenDoorbell: %v", err)
			}
			t.Cleanup(func() { _ = claim.Close() })

			result, err := runner.Run(ctx)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			refused := plugin.secondReason == "already_attached"
			if result.OK() != refused {
				t.Fatalf("a second connection closed with %q gave OK=%v; the step must pass only on already_attached",
					plugin.secondReason, result.OK())
			}
		})
	}
}

// 🔴 A run does not start against nobody. Before this, the first step raced the
// plugin's own connection — and `secondConnection` does not merely race it, it
// TAKES the single-plugin claim, so the real plugin is refused and both sides
// wait for each other until the outer timeout, with a log that says only
// "already_attached" and nothing about why.
func TestARunWaitsForAPluginAndSaysSoWhenNoneComes(t *testing.T) {
	steps := []fixture.Step{
		{Step: "secondConnection"},
		{Step: "ring", Conversation: "invoices", Reason: "mentioned", CauseEntry: "e2", ThroughCursor: "c0002"},
	}
	s := script(steps, fixture.Assertions{RingsDelivered: 1})
	host, err := fixture.New(s)
	if err != nil {
		t.Fatalf("fixture.New: %v", err)
	}
	plugin := &fakePlugin{detached: true, secondReason: "already_attached"}
	runner, err := New(s, host, plugin)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	runner.attachWait = 50 * time.Millisecond

	result, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.OK() {
		t.Fatal("a run with no plugin reported OK, so a CI job would call it a pass")
	}
	if len(result.Unmet) != 1 || !strings.Contains(result.Unmet[0], "no plugin attached") {
		t.Fatalf("the misses are %v, want exactly one naming the missing plugin", result.Unmet)
	}
	// 🔴 And no step ran: a second connection dialled here would have taken the
	// claim the absent plugin still needs.
	if result.RingsDelivered != 0 {
		t.Fatalf("%d rings were delivered, want 0 — the script must not start", result.RingsDelivered)
	}
}

// 🔴 #2232: `question` is met only by a post that CARRIED a question — a harness
// that fell back to asking in plain text posts words, and the card never shows.
func TestTheQuestionMatcherNeedsAPostThatCarriesOne(t *testing.T) {
	steps := []fixture.Step{
		{Step: "ring", Conversation: "invoices", Reason: "mentioned", CauseEntry: "e2", ThroughCursor: "c0002"},
		{Step: "expectPost", Matcher: "question", WithinMs: 300},
	}
	asks := &autoPlugin{
		session: requests.SessionFresh, answer: "Which one?",
		question: &contracts.Question{ID: "clarify_1", Choices: []string{"a", "b"}},
	}
	r := newRig(t, script(steps, fixture.Assertions{}), asks)
	if result, err := r.runner.Run(context.Background()); err != nil || !result.OK() {
		t.Fatalf("a question post did not meet `question`: %v %v", err, result.Unmet)
	}

	plain := &autoPlugin{session: requests.SessionFresh, answer: "Which one? 1. a 2. b"}
	r = newRig(t, script(steps, fixture.Assertions{}), plain)
	result, err := r.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.OK() {
		t.Fatal("a plain-text question met `question` — the card would never have shown")
	}
}

// 🔴 #2235: `refuseNextQuestion` fails the question's post the way a platform that
// could not take it would, so the companion withdraws the request that asked — the
// one real path that cancels a turn waiting on a question. Only the NEXT question
// is refused: the ring after it is the one the scenario needs answered.
func TestRefuseNextQuestionFailsOnlyTheNextQuestion(t *testing.T) {
	steps := []fixture.Step{
		{Step: "refuseNextQuestion"},
		{Step: "ring", Conversation: "invoices", Reason: "mentioned", CauseEntry: "e2", ThroughCursor: "c0002"},
		{Step: "expectClose", Outcome: "error", WithinMs: 1000},
		{Step: "ring", Conversation: "invoices", Reason: "mentioned", CauseEntry: "e2", ThroughCursor: "c0002"},
		{Step: "expectPost", Matcher: "question", WithinMs: 1000},
	}
	asks := &autoPlugin{
		session: requests.SessionFresh, answer: "Which one?", noClose: true,
		question: &contracts.Question{ID: "clarify_1", Choices: []string{"a", "b"}},
	}
	r := newRig(t, script(steps, fixture.Assertions{}), asks)
	result, err := r.runner.Run(context.Background())
	if err != nil || !result.OK() {
		t.Fatalf("the refusal scenario failed: %v %v", err, result.Unmet)
	}
	closes := r.host.Closes()
	if len(closes) == 0 || closes[0].ErrorClass != "platform_unavailable" {
		t.Fatalf("the refused question closed as %+v, want an error of class platform_unavailable", closes)
	}
	if posts := r.host.Posts(); len(posts) != 1 || posts[0].RequestID != "req_3" {
		t.Fatalf("posts %+v, want only the second ring's question", posts)
	}
}

// restartRig is a rig whose driver answers `restartHarness` by reconnecting the
// plugin from the process `next` names.
func restartRig(t *testing.T, next string) *Runner {
	t.Helper()
	steps := []fixture.Step{
		{Step: "ring", Conversation: "invoices", Reason: "mentioned", CauseEntry: "e2", ThroughCursor: "c0002"},
		{Step: "expectAck", Outcome: "accepted", WithinMs: 1000},
		{Step: "restartHarness", WithinMs: 300},
	}
	r := newRig(t, script(steps, fixture.Assertions{RingsDelivered: 1}),
		&autoPlugin{session: requests.SessionFresh, silent: true})
	r.manager.BindProcess("process-1")
	r.runner.SetAnnounce(func(string) {
		sink := &autoPlugin{manager: r.manager, session: requests.SessionFresh, silent: true}
		r.manager.Detach("conn-1")
		r.manager.Attach("conn-2", sink, 48000)
		r.manager.BindProcess(next)
		_ = r.manager.RedeliverOpen(context.Background())
	})
	return r.runner
}

// 🔴 #2235: the step passes only when a NEW harness process had the acknowledged
// request redelivered (#2234) — a driver that reconnected the same process, or none
// at all, restarted nothing.
func TestRestartHarnessNeedsARedeliveryToANewProcess(t *testing.T) {
	runner := restartRig(t, "process-2")
	result, err := runner.Run(context.Background())
	if err != nil || !result.OK() {
		t.Fatalf("a restart into a new process did not pass: %v %v", err, result.Unmet)
	}
	if result.Counts.Redeliveries != 1 {
		t.Fatalf("%d redelivery/ies, want 1", result.Counts.Redeliveries)
	}

	runner = restartRig(t, "process-1")
	result, err = runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.OK() {
		t.Fatal("a reconnect of the SAME process passed as a restart")
	}
}

// attachedPlugin is a Plugin whose Attached answers what the test says.
type attachedPlugin struct {
	mu       sync.Mutex
	attached bool
}

func (p *attachedPlugin) Disconnect() error                          { return nil }
func (p *attachedPlugin) DialSecond(context.Context) (string, error) { return "", nil }
func (p *attachedPlugin) Attached() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.attached
}
func (p *attachedPlugin) set(attached bool) {
	p.mu.Lock()
	p.attached = attached
	p.mu.Unlock()
}

// 🔴 #2313: a restart's window runs from the ask to the moment the OLD plugin
// connection was seen gone — the span in which the dying harness's own warnings
// were logged — and stays open (To zero) when nobody saw it close, so the report
// counts that restart as silent rather than inventing an end.
func TestRestartHarnessRecordsItsWindowUntilTheOldConnectionCloses(t *testing.T) {
	runner := restartRig(t, "process-2")
	plugin := &attachedPlugin{attached: true}
	runner.plugin = plugin
	inner := runner.announce
	var asked time.Time
	runner.SetAnnounce(func(line string) {
		asked = time.Now()
		plugin.set(false)
		inner(line)
	})
	result, err := runner.Run(context.Background())
	if err != nil || !result.OK() {
		t.Fatalf("Run: %v %v", err, result.Unmet)
	}
	if len(result.Restarts) != 1 {
		t.Fatalf("%d restart window(s), want 1", len(result.Restarts))
	}
	window := result.Restarts[0]
	if window.From.After(asked) || window.To.IsZero() || window.To.Before(asked) {
		t.Fatalf("window %+v, want it to open by the ask (%v) and close after it", window, asked)
	}

	runner = restartRig(t, "process-2")
	runner.plugin = &attachedPlugin{attached: true}
	result, err = runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Restarts) != 1 || !result.Restarts[0].To.IsZero() {
		t.Fatalf("a close nobody saw ended the window: %+v", result.Restarts)
	}
}

func TestRestartHarnessAnnouncesThePublishedLine(t *testing.T) {
	runner := restartRig(t, "process-2")
	var got []string
	inner := runner.announce
	runner.SetAnnounce(func(line string) { got = append(got, line); inner(line) })
	if _, err := runner.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got) != 1 || got[0] != "restart the harness" {
		t.Fatalf("announced %q, want the one published line", got)
	}
}

func TestRestartHarnessWithNoDriverIsAMiss(t *testing.T) {
	steps := []fixture.Step{
		{Step: "ring", Conversation: "invoices", Reason: "mentioned", CauseEntry: "e2", ThroughCursor: "c0002"},
		{Step: "restartHarness", WithinMs: 100},
	}
	r := newRig(t, script(steps, fixture.Assertions{}), &autoPlugin{session: requests.SessionFresh, silent: true})
	result, err := r.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.OK() || !strings.Contains(strings.Join(result.Unmet, "\n"), "no driver") {
		t.Fatalf("a restart with nobody to perform it passed: %v", result.Unmet)
	}
}

// extrasFor wraps a scripted question as a post's extras.
func extrasFor(question *contracts.Question) *contracts.PostExtras {
	if question == nil {
		return nil
	}
	return &contracts.PostExtras{Question: question}
}

// 🔴 #2349: `expectToolCall` is met by a call of the named tool on the CURRENT
// request, and `argumentsContain` narrows it; the `toolCalls` tally counts every
// call that reached the host.
func TestExpectToolCallAndTheToolCallsTally(t *testing.T) {
	withTools := func(steps []fixture.Step, assertions fixture.Assertions) *fixture.Script {
		s := script(steps, assertions)
		s.Tools = []fixture.ScriptTool{{
			Name: "list_boards", Description: "List the boards.", Parameters: json.RawMessage(`{"type":"object"}`),
			Permission: "allow",
		}}
		s.Results = map[string]fixture.ScriptResult{
			"list_boards": {CallURN: "urn:uffici:tool_call:1", Result: json.RawMessage(`{"boards":[{"id":"b-1","name":"Support"}]}`)},
		}
		return s
	}
	ring := fixture.Step{Step: "ring", Conversation: "invoices", Reason: "mentioned", CauseEntry: "e2", ThroughCursor: "c0002"}
	one, two := 1, 2
	for name, tc := range map[string]struct {
		expect fixture.Step
		tally  *int
		pass   bool
	}{
		"the named tool":               {fixture.Step{Step: "expectToolCall", Name: "list_boards", WithinMs: 1000}, &one, true},
		"its arguments contain":        {fixture.Step{Step: "expectToolCall", Name: "list_boards", ArgumentsContain: `"lane":"todo"`, WithinMs: 1000}, nil, true},
		"another tool is a miss":       {fixture.Step{Step: "expectToolCall", Name: "delete_board", WithinMs: 200}, nil, false},
		"other arguments are a miss":   {fixture.Step{Step: "expectToolCall", Name: "list_boards", ArgumentsContain: "done", WithinMs: 200}, nil, false},
		"a tally that disagrees fails": {fixture.Step{Step: "expectToolCall", Name: "list_boards", WithinMs: 1000}, &two, false},
	} {
		t.Run(name, func(t *testing.T) {
			s := withTools([]fixture.Step{ring, tc.expect,
				{Step: "expectPost", Matcher: "contains", Value: "Support", WithinMs: 1000},
			}, fixture.Assertions{RingsDelivered: 1, ToolCalls: tc.tally})
			plugin := &autoPlugin{session: requests.SessionFresh, answer: "There is one board, Support."}
			r := newRig(t, s, plugin)
			plugin.beforePost = func(ctx context.Context, requestID string) {
				result, err := r.host.CallTool(ctx, requestID, "list_boards", json.RawMessage(`{"lane":"todo"}`))
				if err != nil || !strings.Contains(string(result.Result), "Support") {
					t.Errorf("CallTool = %+v, %v", result, err)
				}
			}
			result, err := r.runner.Run(context.Background())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if result.OK() != tc.pass {
				t.Fatalf("OK() = %v, want %v: %v", result.OK(), tc.pass, result.Unmet)
			}
			if result.Counts.ToolCalls != 1 {
				t.Fatalf("%d tool calls counted, want 1", result.Counts.ToolCalls)
			}
		})
	}
}

// A tool call on an EARLIER request does not meet the current request's step.
func TestExpectToolCallIsScopedToTheCurrentRequest(t *testing.T) {
	s := script([]fixture.Step{
		{Step: "ring", Conversation: "invoices", Reason: "mentioned", CauseEntry: "e2", ThroughCursor: "c0002"},
		{Step: "expectClose", Outcome: "answered", WithinMs: 1000},
		{Step: "ring", Conversation: "invoices", Reason: "mentioned", CauseEntry: "e2", ThroughCursor: "c0002"},
		{Step: "expectClose", Outcome: "answered", WithinMs: 1000},
		{Step: "expectToolCall", Name: "list_boards", WithinMs: 200},
	}, fixture.Assertions{RingsDelivered: 2})
	s.Tools = []fixture.ScriptTool{{Name: "list_boards", Permission: "allow"}}
	plugin := &autoPlugin{session: requests.SessionFresh, answer: "ok"}
	r := newRig(t, s, plugin)
	var once sync.Once
	plugin.beforePost = func(ctx context.Context, requestID string) {
		once.Do(func() { _, _ = r.host.CallTool(ctx, requestID, "list_boards", json.RawMessage(`{}`)) })
	}
	result, err := r.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.OK() {
		t.Fatal("turn one's tool call met turn two's expectation")
	}
}

// 🔴 #2386: `expectMemory` is met by a memory op of that kind on the CURRENT
// request, and the `memoryOps` tally asserts the ops it names.
func TestExpectMemoryAndTheMemoryOpsTally(t *testing.T) {
	ring := fixture.Step{Step: "ring", Conversation: "invoices", Reason: "mentioned", CauseEntry: "e2", ThroughCursor: "c0002"}
	for name, tc := range map[string]struct {
		expect fixture.Step
		tally  map[string]int
		pass   bool
	}{
		"the op on this request":       {fixture.Step{Step: "expectMemory", Op: "remember", WithinMs: 1000}, map[string]int{"remember": 1}, true},
		"another op is a miss":         {fixture.Step{Step: "expectMemory", Op: "forget", WithinMs: 200}, nil, false},
		"a tally that disagrees fails": {fixture.Step{Step: "expectMemory", Op: "remember", WithinMs: 1000}, map[string]int{"remember": 2}, false},
	} {
		t.Run(name, func(t *testing.T) {
			s := script([]fixture.Step{ring, tc.expect,
				{Step: "expectPost", Matcher: "nonEmpty", WithinMs: 1000},
			}, fixture.Assertions{RingsDelivered: 1, MemoryOps: tc.tally})
			plugin := &autoPlugin{session: requests.SessionFresh, answer: "Noted."}
			r := newRig(t, s, plugin)
			plugin.beforePost = func(ctx context.Context, requestID string) {
				if _, err := r.host.Memory(ctx, contracts.MemoryRemember, requestID, json.RawMessage(`{"text":"The forklift needs a service."}`)); err != nil {
					t.Errorf("remember: %v", err)
				}
			}
			result, err := r.runner.Run(context.Background())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if result.OK() != tc.pass {
				t.Fatalf("OK() = %v, want %v: %v", result.OK(), tc.pass, result.Unmet)
			}
			if result.Counts.MemoryOps["remember"] != 1 {
				t.Fatalf("memory ops counted %v, want one remember", result.Counts.MemoryOps)
			}
		})
	}
}

// 🔴 #2390: a ring starts where the platform would — at the offset the last
// answered close COMMITTED, never at the script's starting offset. The platform's
// `since` is the participant's committed position, so a brief after an answered
// turn carries only what came after it; one that re-carried the whole
// conversation drove a resumed harness session with messages it already held.
func TestARingAfterAnAnsweredCloseStartsAtTheCommittedOffset(t *testing.T) {
	s := script([]fixture.Step{
		{Step: "ring", Conversation: "invoices", Reason: "mentioned", CauseEntry: "e2", ThroughCursor: "c0002"},
		{Step: "expectClose", Conversation: "invoices", Outcome: "answered", WithinMs: 1000},
		{Step: "appendEntries", Conversation: "invoices", Entries: []fixture.ScriptEntry{
			{ID: "e3", Cursor: "c0003", Kind: "message", Text: "and one more thing",
				Author: fixture.ScriptAuthor{Type: "member", ID: "m1", Label: "Ana"}},
		}},
		{Step: "ring", Conversation: "invoices", Reason: "mentioned", CauseEntry: "e3", ThroughCursor: "c0003"},
		{Step: "expectClose", Conversation: "invoices", Outcome: "answered", WithinMs: 1000},
	}, fixture.Assertions{RingsDelivered: 2})
	plugin := &autoPlugin{session: requests.SessionFresh, answer: "ok"}
	r := newRig(t, s, plugin)
	result, err := r.runner.Run(context.Background())
	if err != nil || !result.OK() {
		t.Fatalf("Run: %v %v", err, result.Unmet)
	}
	briefs := plugin.seen()
	if len(briefs) != 2 {
		t.Fatalf("%d briefs, want 2", len(briefs))
	}
	if briefs[0].SinceCursor != "c0001" {
		t.Fatalf("the first ring starts at %q, want the script's offset c0001", briefs[0].SinceCursor)
	}
	if briefs[1].SinceCursor != "c0002" {
		t.Fatalf("the second ring starts at %q, want c0002 — what the answered close committed", briefs[1].SinceCursor)
	}
	if len(briefs[1].Delta) != 1 || briefs[1].Delta[0].ID != "e3" {
		t.Fatalf("the second brief re-carried earlier entries: %+v", briefs[1].Delta)
	}
}
