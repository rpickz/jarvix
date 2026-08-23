package session

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/intent"
	"github.com/rpickz/jarvix/internal/tools"
)

// The script tests cover the engine half of ADR 0030: a script phrase is
// claimed by the router, gated under script.run — whose default is ASK, the
// inversion of routine.run's allow, because a script is an arbitrary
// executable behind a possibly-misheard phrase — executed through the
// injected runner (never the real one — no test here runs a file), and
// acknowledged per the report mode with failures always spoken. The provider
// is a fake so the headline assertion — zero model calls on a hit — is a
// count, not a hope.
//
// Several of these are deliberate mutation checks: each names the single
// code change it exists to catch, so the refusal properties cannot rot
// silently.

// fakeScripts is a scripted ScriptRunner.
type fakeScripts struct {
	mu   sync.Mutex
	line string
	err  error
	path string
	// block, when set, parks Run until the context is cancelled — the
	// deterministic stand-in for a script mid-run. started is closed when
	// Run has the context, so a test can cancel *during* the run without
	// polling.
	block   bool
	started chan struct{}

	runs    []string
	lastCtx context.Context
}

func (f *fakeScripts) Run(ctx context.Context, name string) (string, error) {
	f.mu.Lock()
	f.runs = append(f.runs, name)
	f.lastCtx = ctx
	block, line, err := f.block, f.line, f.err
	started := f.started
	f.mu.Unlock()
	if started != nil {
		close(started)
	}
	if block {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return line, err
}

func (f *fakeScripts) Path(string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.path, f.path != ""
}

func (f *fakeScripts) ran() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.runs...)
}

const testScriptPath = "/home/user/bin/backup-notes.sh"

// newScriptHarness wires an engine whose router knows one script and whose
// runner is the fake. A nil policyCfg installs no registry, which for
// script.run means the engine's own fallback: ask — an ungated arbitrary
// executable must never run silently.
func newScriptHarness(t *testing.T, runner *fakeScripts, policyCfg *tools.PolicyConfig) *harness {
	t.Helper()
	if runner.path == "" {
		runner.path = testScriptPath
	}
	router, err := intent.New(intent.Options{Scripts: []intent.ScriptPhrases{
		{Name: "backup notes", Phrases: []string{"backup my notes", "back up my notes"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, Options{})
	var registry *tools.Registry
	if policyCfg != nil {
		policy, err := tools.NewPolicy(*policyCfg)
		if err != nil {
			t.Fatal(err)
		}
		registry = tools.NewRegistry(nil)
		registry.SetPolicy(policy)
	}
	h.tools = registry
	bus := NewBus(nil)
	h.events, h.cancel = bus.Subscribe()
	t.Cleanup(h.cancel)
	h.engine = NewEngine(h.provider, h.stt, h.tts, h.recorder, h.player, registry, nil, bus, nil, Options{
		Model: "m", SpeakResponses: true, HistoryTurns: 8,
		ConfirmTimeout: 5 * time.Second,
		Intents:        router, IntentRunner: &intent.FakeRunner{},
		Scripts: runner,
	})
	return h
}

func sayScript(t *testing.T, h *harness, text string) map[string]Event {
	t.Helper()
	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit(text); err != nil {
		t.Fatal(err)
	}
	seen := h.collectUntil(t, "session.finished")
	h.waitIdle(t)
	return seen
}

// TestScriptPhraseAsksBeforeRunningByDefault is the gate acceptance
// criterion end to end: with nothing configured, the phrase produces a
// confirmation naming the script AND its absolute path, the runner is not
// consulted until the user says yes, no provider is ever called, and the
// runner's line is the one thing spoken.
func TestScriptPhraseAsksBeforeRunningByDefault(t *testing.T) {
	runner := &fakeScripts{line: "Backup notes finished."}
	h := newScriptHarness(t, runner, nil)

	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit("backup my notes"); err != nil {
		t.Fatal(err)
	}
	ev := h.waitFor(t, "tool.confirmation_required")
	if ev.Data["tool"] != tools.ScriptToolName {
		t.Errorf("confirmation tool = %v", ev.Data["tool"])
	}
	command, _ := ev.Data["command"].(string)
	if !strings.Contains(command, "backup notes") || !strings.Contains(command, testScriptPath) {
		t.Errorf("confirmation command %q does not name the script and its path", command)
	}
	summary, _ := ev.Data["summary"].(string)
	if !strings.Contains(summary, testScriptPath) {
		t.Errorf("spoken confirmation %q does not name the path; substitution must be audible", summary)
	}
	if len(runner.ran()) != 0 {
		t.Fatal("the script ran before it was confirmed")
	}
	if err := h.engine.Confirm(true); err != nil {
		t.Fatal(err)
	}
	seen := h.collectUntil(t, "session.finished")
	h.waitIdle(t)

	if len(h.provider.Requests) != 0 {
		t.Fatalf("the provider was called %d times for a script phrase", len(h.provider.Requests))
	}
	if ran := runner.ran(); len(ran) != 1 || ran[0] != "backup notes" {
		t.Fatalf("runner ran %v", ran)
	}
	ev, ok := seen["intent.executed"]
	if !ok {
		t.Fatal("no intent.executed event")
	}
	if ev.Data["intent"] != "script.run" || ev.Data["script"] != "backup notes" {
		t.Errorf("event = %v", ev.Data)
	}
	if ev.Data["source"] != "script" || ev.Data["status"] != "ok" {
		t.Errorf("event = %v", ev.Data)
	}
	if h.tts.LastRequest.Text != "Backup notes finished." {
		t.Errorf("spoken line = %q", h.tts.LastRequest.Text)
	}
}

// TestScriptDeclinedNeverRuns: "no" at the gate means the runner is never
// consulted — the mutation check for any change that would consult it first
// and ask afterwards.
func TestScriptDeclinedNeverRuns(t *testing.T) {
	runner := &fakeScripts{line: "never"}
	h := newScriptHarness(t, runner, nil)
	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit("backup my notes"); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, "tool.confirmation_required")
	if err := h.engine.Confirm(false); err != nil {
		t.Fatal(err)
	}
	seen := h.collectUntil(t, "session.finished")
	h.waitIdle(t)
	if len(runner.ran()) != 0 {
		t.Fatalf("a declined script ran: %v", runner.ran())
	}
	if seen["intent.executed"].Data["acknowledgement"] != "Cancelled." {
		t.Errorf("acknowledgement = %v", seen["intent.executed"].Data["acknowledgement"])
	}
	if len(h.provider.Requests) != 0 {
		t.Errorf("a declined script still made %d provider calls", len(h.provider.Requests))
	}
}

// TestScriptDeniedByPolicyNeverRuns: deny — explicit, or via the global
// default, which must reach scripts even though a global allow does not —
// means the runner is never consulted and the audit trail says so.
func TestScriptDeniedByPolicyNeverRuns(t *testing.T) {
	configs := map[string]*tools.PolicyConfig{
		"explicit deny": {Tools: map[string]tools.PolicyDecision{
			tools.ScriptToolName: tools.PolicyDeny}},
		"global default deny": {Default: tools.PolicyDeny},
	}
	for name, cfg := range configs {
		t.Run(name, func(t *testing.T) {
			runner := &fakeScripts{line: "should never be heard"}
			h := newScriptHarness(t, runner, cfg)

			seen := sayScript(t, h, "backup my notes")

			if len(runner.ran()) != 0 {
				t.Fatalf("a denied script ran: %v", runner.ran())
			}
			if _, denied := seen["tool.denied"]; !denied {
				t.Error("a denied script must reach the audit trail")
			}
			if ev := seen["intent.executed"]; ev.Data["status"] != "failed" {
				t.Errorf("status = %v", ev.Data["status"])
			}
		})
	}
}

// TestGlobalAllowDefaultStillAsksForScripts is the gate-bypass mutation
// check: `[tools.policy] default = "allow"` silences most tools, and the one
// code change that would extend it to script.run — dropping the explicit
// branch in ToolDecision — must fail here, loudly.
func TestGlobalAllowDefaultStillAsksForScripts(t *testing.T) {
	runner := &fakeScripts{line: "Backup notes finished."}
	h := newScriptHarness(t, runner, &tools.PolicyConfig{Default: tools.PolicyAllow})
	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit("backup my notes"); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, "tool.confirmation_required")
	if len(runner.ran()) != 0 {
		t.Fatal("a global allow default ran a script without asking")
	}
	if err := h.engine.Confirm(true); err != nil {
		t.Fatal(err)
	}
	h.collectUntil(t, "session.finished")
	h.waitIdle(t)
}

// TestScriptExplicitAllowRunsSilently: naming the tool is the sentence a
// user has to mean, and it removes the question.
func TestScriptExplicitAllowRunsSilently(t *testing.T) {
	runner := &fakeScripts{line: "Backup notes finished."}
	h := newScriptHarness(t, runner, &tools.PolicyConfig{
		Tools: map[string]tools.PolicyDecision{tools.ScriptToolName: tools.PolicyAllow},
	})

	seen := sayScript(t, h, "backup my notes")

	if _, asked := seen["tool.confirmation_required"]; asked {
		t.Error("an explicitly allowed script still asked")
	}
	if ran := runner.ran(); len(ran) != 1 {
		t.Fatalf("runner ran %v", ran)
	}
	if len(h.provider.Requests) != 0 {
		t.Errorf("provider called %d times", len(h.provider.Requests))
	}
	if seen["intent.executed"].Data["status"] != "ok" {
		t.Errorf("status = %v", seen["intent.executed"].Data["status"])
	}
}

// TestScriptSilentSuccessSpeaksNothingButIsRecorded: a silent report mode
// returns "" from the runner; the engine must not speak, must still report
// the turn as ok, and must not stage an empty assistant message (providers
// reject those) — the record says "Done." while the ear hears nothing.
func TestScriptSilentSuccessSpeaksNothingButIsRecorded(t *testing.T) {
	runner := &fakeScripts{line: ""}
	h := newScriptHarness(t, runner, &tools.PolicyConfig{
		Tools: map[string]tools.PolicyDecision{tools.ScriptToolName: tools.PolicyAllow},
	})

	seen := sayScript(t, h, "backup my notes")

	if h.tts.LastRequest.Text != "" {
		t.Errorf("a silent success spoke %q", h.tts.LastRequest.Text)
	}
	ev := seen["intent.executed"]
	if ev.Data["status"] != "ok" || ev.Data["acknowledgement"] != "" {
		t.Errorf("event = %v", ev.Data)
	}
	// The recorded turn carries a non-empty assistant side.
	turns := h.engine.Conversation()
	if len(turns) < 2 {
		t.Fatalf("conversation = %+v", turns)
	}
	last := turns[len(turns)-1]
	if last.Role != "assistant" || last.Text == "" {
		t.Errorf("a silent success recorded %+v; the history must not hold an empty assistant turn", last)
	}
}

// TestScriptFailureIsSpoken: the runner reports failures as errors precisely
// so the engine speaks them on every path — a silent report mode has no say
// in it. The fake returns the composed failure the real runner would.
func TestScriptFailureIsSpoken(t *testing.T) {
	runner := &fakeScripts{err: errors.New("backup notes failed — exit 2: disk full")}
	h := newScriptHarness(t, runner, &tools.PolicyConfig{
		Tools: map[string]tools.PolicyDecision{tools.ScriptToolName: tools.PolicyAllow},
	})

	seen := sayScript(t, h, "backup my notes")

	ev := seen["intent.executed"]
	if ev.Data["status"] != "failed" {
		t.Errorf("status = %v", ev.Data["status"])
	}
	if ev.Data["acknowledgement"] != "Sorry, backup notes failed — exit 2: disk full." {
		t.Errorf("acknowledgement = %v", ev.Data["acknowledgement"])
	}
	if h.tts.LastRequest.Text == "" {
		t.Error("a failed script was not spoken")
	}
}

// TestScriptHonoursSessionCancellation: the runner receives the session's
// context, so "stop" — or any interruption — aborts a running script
// (composing with #54's cancel path), and the cancelled run says nothing.
func TestScriptHonoursSessionCancellation(t *testing.T) {
	runner := &fakeScripts{block: true, started: make(chan struct{})}
	h := newScriptHarness(t, runner, &tools.PolicyConfig{
		Tools: map[string]tools.PolicyDecision{tools.ScriptToolName: tools.PolicyAllow},
	})

	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit("backup my notes"); err != nil {
		t.Fatal(err)
	}
	<-runner.started
	if err := h.engine.Cancel(); err != nil {
		t.Fatal(err)
	}
	seen := h.collectUntil(t, "session.cancelled")
	h.waitIdle(t)

	runner.mu.Lock()
	ctx := runner.lastCtx
	runner.mu.Unlock()
	select {
	case <-ctx.Done():
	default:
		t.Fatal("cancelling the session did not cancel the script's context")
	}
	if _, executed := seen["intent.executed"]; executed {
		t.Error("a cancelled script still reported an outcome; the cancel path owns the events")
	}
}
