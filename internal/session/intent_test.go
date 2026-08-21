package session

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/intent"
	"github.com/rpickz/jarvix/internal/tools"
)

// intentHarness is the standard harness plus a compiled router, a fake runner
// and a fake compositor, so an "executed" intent is a recorded argv or a
// recorded dispatch and nothing else — no test in this package may reach
// wpctl, hyprctl, or the developer's own workspaces.
type intentHarness struct {
	*harness
	runner *intent.FakeRunner
	comp   *desktop.FakeCompositor
}

func newIntentHarness(t *testing.T, opts Options, custom ...intent.Custom) *intentHarness {
	t.Helper()
	router, err := intent.New(intent.Options{Custom: custom})
	if err != nil {
		t.Fatalf("intent.New: %v", err)
	}
	runner := &intent.FakeRunner{}
	comp := desktop.NewFakeCompositor()
	opts.Intents = router
	opts.IntentRunner = runner
	if opts.Compositor == nil {
		opts.Compositor = comp
	}
	return &intentHarness{harness: newHarness(t, opts), runner: runner, comp: comp}
}

// say drives one text utterance through the engine to completion.
func (h *intentHarness) say(t *testing.T, text string) map[string]Event {
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

// TestIntentHitMakesNoProviderCall is the headline acceptance criterion: a
// matched intent executes without any model involvement at all.
func TestIntentHitMakesNoProviderCall(t *testing.T) {
	h := newIntentHarness(t, Options{SpeakResponses: true, HistoryTurns: 8})

	seen := h.say(t, "volume thirty")

	if len(h.provider.Requests) != 0 {
		t.Fatalf("the provider was called %d times for a deterministic intent", len(h.provider.Requests))
	}
	ev, ok := seen["intent.executed"]
	if !ok {
		t.Fatal("no intent.executed event")
	}
	if ev.Data["intent"] != "volume.set" {
		t.Errorf("intent = %v", ev.Data["intent"])
	}
	if ev.Data["slot"] != 30 {
		t.Errorf("slot = %v, want 30", ev.Data["slot"])
	}
	if ev.Data["source"] != "builtin" || ev.Data["status"] != "ok" {
		t.Errorf("event data = %v", ev.Data)
	}
	if ev.Data["acknowledgement"] != "Volume thirty" {
		t.Errorf("acknowledgement = %v", ev.Data["acknowledgement"])
	}

	// The fixed argv ran — with the transcript contributing only the integer.
	argv := h.runner.Argv()
	if len(argv) != 1 || strings.Join(argv[0], " ") != "wpctl set-volume -l 1.5 @DEFAULT_AUDIO_SINK@ 30%" {
		t.Fatalf("argv = %v", argv)
	}
	if h.runner.Shell() != nil {
		t.Errorf("a built-in intent must never reach a shell: %v", h.runner.Shell())
	}
	// And it was spoken, so the user knows it landed.
	if h.tts.LastRequest.Text != "Volume thirty." {
		t.Errorf("spoken acknowledgement = %q", h.tts.LastRequest.Text)
	}
}

// TestIntentLatencyBudget measures the criterion directly: transcript-final to
// acknowledgement in under 300ms.
func TestIntentLatencyBudget(t *testing.T) {
	h := newIntentHarness(t, Options{SpeakResponses: false})
	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := h.engine.Submit("mute"); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, "intent.executed")
	elapsed := time.Since(start)
	h.waitIdle(t)
	if elapsed > 300*time.Millisecond {
		t.Errorf("transcript → acknowledgement took %s, budget is 300ms", elapsed)
	}
	t.Logf("transcript → acknowledgement: %s", elapsed)
}

// TestIntentMissReachesTheModelUnchanged is the other half of the contract:
// an unrecognised utterance behaves exactly as it did before the router.
func TestIntentMissReachesTheModelUnchanged(t *testing.T) {
	h := newIntentHarness(t, Options{SpeakResponses: true})

	seen := h.say(t, "turn it up a bit")

	if _, routed := seen["intent.executed"]; routed {
		t.Fatal("a near-miss was claimed by the router")
	}
	if len(h.provider.Requests) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(h.provider.Requests))
	}
	last := h.provider.LastRequest.Messages
	if last[len(last)-1].Content != "turn it up a bit" {
		t.Errorf("the model saw %q", last[len(last)-1].Content)
	}
	if seen["assistant.finished"].Data["content"] != "Recursion is a function calling itself." {
		t.Errorf("assistant answer = %v", seen["assistant.finished"].Data["content"])
	}
	if h.runner.Argv() != nil {
		t.Errorf("nothing should have been executed: %v", h.runner.Argv())
	}
}

// TestIntentStateSequence proves a matched intent never passes through
// Thinking or Responding — those states mean an open provider request.
func TestIntentStateSequence(t *testing.T) {
	h := newIntentHarness(t, Options{SpeakResponses: true})
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("workspace four")

	var states []string
	deadline := time.After(5 * time.Second)
	for len(states) == 0 || states[len(states)-1] != "idle" {
		select {
		case ev := <-h.events:
			if ev.Type == "state.changed" {
				states = append(states, ev.Data["state"].(string))
			}
		case <-deadline:
			t.Fatalf("states so far: %v", states)
		}
	}
	want := []string{"acting", "speaking", "idle"}
	if strings.Join(states, ",") != strings.Join(want, ",") {
		t.Fatalf("states = %v, want %v", states, want)
	}
}

func TestIntentThroughVoiceCapture(t *testing.T) {
	h := newIntentHarness(t, Options{SpeakResponses: true})
	h.stt.Text = "volume 30"

	_, _ = h.engine.StartSession()
	_ = h.engine.StartVoice()
	_, _ = h.engine.StopVoice()
	_ = h.engine.Submit("")

	seen := h.collectUntil(t, "session.finished")
	h.waitIdle(t)
	if _, ok := seen["intent.executed"]; !ok {
		t.Fatal("a voice transcript must reach the router")
	}
	if len(h.provider.Requests) != 0 {
		t.Errorf("provider was called %d times", len(h.provider.Requests))
	}
	if argv := h.runner.Argv(); len(argv) != 1 || argv[0][len(argv[0])-1] != "30%" {
		t.Errorf("argv = %v", argv)
	}
}

// TestIntentRecordedInHistory is the follow-up criterion: the next question,
// which does reach the model, must know what just happened.
func TestIntentRecordedInHistory(t *testing.T) {
	h := newIntentHarness(t, Options{HistoryTurns: 8, FollowUpWindow: time.Hour})

	h.say(t, "volume thirty")
	h.say(t, "a bit louder")

	if len(h.provider.Requests) != 1 {
		t.Fatalf("provider calls = %d; only the follow-up should reach the model", len(h.provider.Requests))
	}
	var transcript []string
	for _, m := range h.provider.LastRequest.Messages {
		transcript = append(transcript, string(m.Role)+":"+m.Content)
	}
	joined := strings.Join(transcript, " | ")
	if !strings.Contains(joined, "user:volume thirty") {
		t.Errorf("the intent utterance is missing from context: %s", joined)
	}
	if !strings.Contains(joined, "assistant:Volume thirty") {
		t.Errorf("the acknowledgement is missing from context: %s", joined)
	}
	if !strings.Contains(joined, "user:a bit louder") {
		t.Errorf("the follow-up is missing: %s", joined)
	}

	// The conversation window shows it as an ordinary exchange, too.
	turns := h.engine.Conversation()
	if len(turns) < 2 || turns[0].Text != "volume thirty" || turns[1].Text != "Volume thirty" {
		t.Errorf("conversation = %+v", turns)
	}
}

// TestStopWhileSpeakingIsSilent covers the "stop" criterion end to end: the
// speech stops and nothing new is said.
func TestStopWhileSpeakingIsSilent(t *testing.T) {
	h := newIntentHarness(t, Options{SpeakResponses: true, HistoryTurns: 8})
	// Hold the first answer mid-utterance so "stop" always lands while
	// Jarvix is speaking.
	hold := make(chan struct{})
	h.tts.SetHold(hold)
	defer close(hold)

	first, _ := h.engine.StartSession()
	_ = h.engine.Submit("explain recursion")
	h.waitFor(t, "tts.started")
	speaksBefore := h.tts.Speaks()

	// The user interrupts and says "stop".
	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	ev := h.waitFor(t, "session.cancelled")
	if ev.Data["session_id"] != first {
		t.Errorf("cancelled %v, want the speaking session %s", ev.Data["session_id"], first)
	}
	h.tts.SetHold(nil) // an acknowledgement, if any, would now be free to run
	_ = h.engine.Submit("stop")

	seen := h.collectUntil(t, "session.finished")
	h.waitIdle(t)

	if _, ok := seen["intent.executed"]; !ok {
		t.Fatal("\"stop\" must be routed, not sent to the model")
	}
	if seen["intent.executed"].Data["acknowledgement"] != "" {
		t.Errorf("stop acknowledged out loud: %v", seen["intent.executed"].Data["acknowledgement"])
	}
	if len(h.provider.Requests) != 1 {
		t.Errorf("provider calls = %d; \"stop\" must not reach the model", len(h.provider.Requests))
	}
	if h.tts.Speaks() != speaksBefore {
		t.Errorf("speech syntheses %d → %d; silence is the confirmation",
			speaksBefore, h.tts.Speaks())
	}
	if h.runner.Argv() != nil {
		t.Errorf("stop must run no command: %v", h.runner.Argv())
	}
	// Nothing was appended to the conversation for an utterance with no reply.
	for _, turn := range h.engine.Conversation() {
		if turn.Text == "stop" {
			t.Error("\"stop\" was recorded as a conversation turn")
		}
	}
}

// TestStopCancelsSpeechDirectly exercises the CancelSpeech path from the
// router itself: the utterance arrives while the very session that is
// speaking is still current, which is what a wake word will produce.
func TestStopCancelsSpeechDirectly(t *testing.T) {
	h := newIntentHarness(t, Options{SpeakResponses: true})
	hold := make(chan struct{})
	h.tts.SetHold(hold)
	defer close(hold)
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("explain recursion")
	h.waitFor(t, "tts.started")

	if !h.engine.CancelSpeech() {
		t.Fatal("CancelSpeech reported nothing playing while speech was held mid-utterance")
	}
	ev := h.waitFor(t, "tts.finished")
	if ev.Data["interrupted"] != true {
		t.Errorf("tts.finished = %v", ev.Data)
	}
	h.waitFor(t, "session.finished")
	h.waitIdle(t)
}

func TestNewConversationIntentClearsHistory(t *testing.T) {
	h := newIntentHarness(t, Options{SpeakResponses: false, HistoryTurns: 8, FollowUpWindow: time.Hour})

	h.say(t, "explain recursion") // a real exchange, so there is context to clear
	h.say(t, "new conversation")

	if turns := h.engine.Conversation(); len(turns) != 0 {
		t.Fatalf("conversation = %+v, want empty", turns)
	}
	h.say(t, "what did I ask")
	last := h.provider.LastRequest.Messages
	for _, m := range last {
		if strings.Contains(m.Content, "recursion") {
			t.Errorf("the reset did not clear context: %+v", last)
		}
	}
}

// TestIntentCommandFailureSpeaksAndRecovers proves the reliability criterion:
// a missing wpctl produces one spoken line and a clean return to Idle.
func TestIntentCommandFailureSpeaksAndRecovers(t *testing.T) {
	h := newIntentHarness(t, Options{SpeakResponses: true, HistoryTurns: 8})
	h.runner.SetErr(errors.New("wpctl is not installed"))

	seen := h.say(t, "mute")

	ev := seen["intent.executed"]
	if ev.Data["status"] != "failed" {
		t.Errorf("status = %v, want failed", ev.Data["status"])
	}
	if ev.Data["error"] != "wpctl is not installed" {
		t.Errorf("error = %v", ev.Data["error"])
	}
	if _, isError := seen["error"]; isError {
		t.Error("a failing intent must not fail the session")
	}
	if !strings.Contains(h.tts.LastRequest.Text, "wpctl is not installed") {
		t.Errorf("the failure was not spoken: %q", h.tts.LastRequest.Text)
	}
	if state, _ := h.engine.State(); state != StateIdle {
		t.Errorf("state = %s, want idle", state)
	}

	// The engine keeps working: the next utterance still reaches the model.
	h.runner.SetErr(nil)
	h.say(t, "explain recursion")
	if len(h.provider.Requests) != 1 {
		t.Errorf("provider calls = %d", len(h.provider.Requests))
	}
}

// ------------------------------------------------------ compositor intents

// TestDesktopIntentsDispatchThroughTheCompositor is the regression for #47:
// "workspace four" and "open a terminal" reach the compositor seam as actions
// rather than as an `hyprctl dispatch …` command line the router wrote
// itself. The seam is what knows which dispatch dialect this machine speaks;
// a table that wrote its own was a Lua parse error on a current Omarchy
// desktop, and nothing moved.
func TestDesktopIntentsDispatchThroughTheCompositor(t *testing.T) {
	h := newIntentHarness(t, Options{SpeakResponses: true})

	seen := h.say(t, "workspace four")
	if ev := seen["intent.executed"]; ev.Data["intent"] != "workspace.switch" ||
		ev.Data["status"] != "ok" || ev.Data["slot"] != 4 {
		t.Fatalf("event data = %v", ev.Data)
	}
	h.say(t, "open a terminal")

	got := h.comp.Actions()
	if len(got) != 2 {
		t.Fatalf("dispatches = %+v, want a workspace switch and a spawn", got)
	}
	if got[0].Verb != "workspace" || got[0].Workspace != 4 || got[0].Address != "" {
		t.Errorf("first dispatch = %+v, want a switch to workspace 4", got[0])
	}
	if got[1].Verb != "spawn" || got[1].Program != intent.DefaultTerminal {
		t.Errorf("second dispatch = %+v, want the configured terminal spawned", got[1])
	}
	// And nothing was executed behind the seam's back.
	if argv := h.runner.Argv(); argv != nil {
		t.Errorf("a compositor intent ran a command line: %v", argv)
	}
}

// TestRefusedDesktopDispatchIsSpokenNotSilent is the deeper half of #47.
// hyprctl exits 0 for a dispatch the compositor refused, so before the seam
// owned the decision a refusal was indistinguishable from success: Jarvix
// said "Workspace four" and the screen did not move. It must now say what
// went wrong instead.
func TestRefusedDesktopDispatchIsSpokenNotSilent(t *testing.T) {
	h := newIntentHarness(t, Options{SpeakResponses: true})
	h.comp.FailAction = errors.New("hyprctl dispatch: workspace not found")

	seen := h.say(t, "workspace four")

	ev := seen["intent.executed"]
	if ev.Data["status"] != "failed" {
		t.Fatalf("status = %v, want failed for a refused dispatch", ev.Data["status"])
	}
	if ev.Data["acknowledgement"] == "Workspace four" {
		t.Error("a refused dispatch was acknowledged as though it had worked")
	}
	if !strings.Contains(h.tts.LastRequest.Text, "workspace not found") {
		t.Errorf("the refusal was not spoken: %q", h.tts.LastRequest.Text)
	}
	if _, isError := seen["error"]; isError {
		t.Error("a refused dispatch must not fail the session")
	}
	if state, _ := h.engine.State(); state != StateIdle {
		t.Errorf("state = %s, want idle", state)
	}
}

// TestDesktopIntentWithoutACompositorSaysSo covers the daemon started outside
// a graphical session. There is nothing to dispatch to, and the one thing
// Jarvix must not do is acknowledge the action anyway.
func TestDesktopIntentWithoutACompositorSaysSo(t *testing.T) {
	router, err := intent.New(intent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, Options{SpeakResponses: true, Intents: router,
		IntentRunner: &intent.FakeRunner{}}) // no compositor wired
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("open terminal")
	seen := h.collectUntil(t, "session.finished")
	h.waitIdle(t)

	if ev := seen["intent.executed"]; ev.Data["status"] != "failed" {
		t.Fatalf("status = %v, want failed with no compositor", ev.Data["status"])
	}
	if !strings.Contains(h.tts.LastRequest.Text, "window manager") {
		t.Errorf("spoken failure = %q, want it to name what is missing", h.tts.LastRequest.Text)
	}
}

func TestRouterDisabledSendsEverythingToTheModel(t *testing.T) {
	h := newHarness(t, Options{SpeakResponses: false}) // no router wired
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("volume thirty")
	h.collectUntil(t, "session.finished")
	h.waitIdle(t)
	if len(h.provider.Requests) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(h.provider.Requests))
	}
}

// ------------------------------------------- user-defined intents & the gate

// gatedIntentHarness wires a real permission gate so a user-defined intent
// faces the same classifier a model tool call does.
func newGatedIntentHarness(t *testing.T, policyCfg tools.PolicyConfig, custom ...intent.Custom) *intentHarness {
	t.Helper()
	router, err := intent.New(intent.Options{Custom: custom})
	if err != nil {
		t.Fatalf("intent.New: %v", err)
	}
	policy, err := tools.NewPolicy(policyCfg)
	if err != nil {
		t.Fatalf("tools.NewPolicy: %v", err)
	}
	registry := tools.NewRegistry(nil)
	registry.SetPolicy(policy)
	runner := &intent.FakeRunner{}

	h := newHarness(t, Options{})
	h.tools = registry
	bus := NewBus(nil)
	h.events, h.cancel = bus.Subscribe()
	h.engine = NewEngine(h.provider, h.stt, h.tts, h.recorder, h.player, registry, nil, bus, nil, Options{
		Model: "m", SpeakResponses: false, HistoryTurns: 8,
		ConfirmTimeout: 5 * time.Second,
		Intents:        router, IntentRunner: runner,
	})
	return &intentHarness{harness: h, runner: runner}
}

func TestUserIntentAllowedByPolicyRunsSilently(t *testing.T) {
	h := newGatedIntentHarness(t,
		tools.PolicyConfig{Default: tools.PolicyAsk, Tools: map[string]tools.PolicyDecision{
			tools.IntentToolName: tools.PolicyAllow,
		}},
		intent.Custom{Match: "lock the screen", Run: "hyprlock", Say: "Locking."})

	seen := h.say(t, "lock the screen")

	if _, asked := seen["tool.confirmation_required"]; asked {
		t.Error("an allow-tier intent must not ask")
	}
	if shell := h.runner.Shell(); len(shell) != 1 || shell[0] != "hyprlock" {
		t.Fatalf("shell = %v", shell)
	}
	if seen["intent.executed"].Data["source"] != "user" {
		t.Errorf("source = %v", seen["intent.executed"].Data["source"])
	}
	if len(h.provider.Requests) != 0 {
		t.Errorf("provider calls = %d", len(h.provider.Requests))
	}
}

func TestUserIntentAskTierWaitsForConfirmation(t *testing.T) {
	h := newGatedIntentHarness(t,
		tools.PolicyConfig{Default: tools.PolicyAsk},
		intent.Custom{Match: "tidy the downloads", Run: "rm -rf ~/Downloads/tmp", Say: "Tidied."})

	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit("tidy the downloads"); err != nil {
		t.Fatal(err)
	}
	ev := h.waitFor(t, "tool.confirmation_required")
	if ev.Data["tool"] != tools.IntentToolName {
		t.Errorf("tool = %v, want %s", ev.Data["tool"], tools.IntentToolName)
	}
	if ev.Data["command"] != "rm -rf ~/Downloads/tmp" {
		t.Errorf("command = %v", ev.Data["command"])
	}
	if state, _ := h.engine.State(); state != StateAwaitingConfirmation {
		t.Fatalf("state = %s, want awaiting_confirmation", state)
	}
	if h.runner.Shell() != nil {
		t.Fatal("the command ran before it was confirmed")
	}

	if err := h.engine.Confirm(true); err != nil {
		t.Fatal(err)
	}
	seen := h.collectUntil(t, "session.finished")
	h.waitIdle(t)
	if shell := h.runner.Shell(); len(shell) != 1 {
		t.Fatalf("shell = %v", shell)
	}
	if seen["intent.executed"].Data["status"] != "ok" {
		t.Errorf("status = %v", seen["intent.executed"].Data["status"])
	}
	if len(h.provider.Requests) != 0 {
		t.Errorf("a confirmed intent still made %d provider calls", len(h.provider.Requests))
	}
}

func TestUserIntentDeclinedRunsNothing(t *testing.T) {
	h := newGatedIntentHarness(t,
		tools.PolicyConfig{Default: tools.PolicyAsk},
		intent.Custom{Match: "tidy the downloads", Run: "rm -rf ~/Downloads/tmp"})

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("tidy the downloads")
	h.waitFor(t, "tool.confirmation_required")
	if err := h.engine.Confirm(false); err != nil {
		t.Fatal(err)
	}
	seen := h.collectUntil(t, "session.finished")
	h.waitIdle(t)

	if h.runner.Shell() != nil {
		t.Fatalf("a declined intent executed: %v", h.runner.Shell())
	}
	if seen["intent.executed"].Data["acknowledgement"] != "Cancelled." {
		t.Errorf("acknowledgement = %v", seen["intent.executed"].Data["acknowledgement"])
	}
	if _, declined := seen["tool.declined"]; !declined {
		t.Error("a decline must reach the audit trail")
	}
}

func TestUserIntentDeniedByPolicyNeverRuns(t *testing.T) {
	h := newGatedIntentHarness(t,
		tools.PolicyConfig{Default: tools.PolicyAsk},
		intent.Custom{Match: "wipe the disk", Run: "rm -rf /"})

	seen := h.say(t, "wipe the disk")

	if h.runner.Shell() != nil {
		t.Fatalf("a denied intent executed: %v", h.runner.Shell())
	}
	if _, denied := seen["tool.denied"]; !denied {
		t.Error("no tool.denied event")
	}
	if seen["intent.executed"].Data["status"] != "failed" {
		t.Errorf("status = %v", seen["intent.executed"].Data["status"])
	}
	if state, _ := h.engine.State(); state != StateIdle {
		t.Errorf("state = %s", state)
	}
}

func TestUserIntentConfirmationByVoiceReply(t *testing.T) {
	h := newGatedIntentHarness(t,
		tools.PolicyConfig{Default: tools.PolicyAsk},
		intent.Custom{Match: "tidy the downloads", Run: "rm -rf ~/Downloads/tmp"})
	h.stt.Text = "yes go ahead"

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("tidy the downloads")
	h.waitFor(t, "tool.confirmation_required")

	// The user answers by holding the key: the reply capture reuses the
	// ordinary Listening → Transcribing path and resolves back into Acting.
	if err := h.engine.StartVoice(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.StopVoice(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit(""); err != nil {
		t.Fatal(err)
	}
	seen := h.collectUntil(t, "session.finished")
	h.waitIdle(t)

	if shell := h.runner.Shell(); len(shell) != 1 {
		t.Fatalf("shell = %v", shell)
	}
	if seen["tool.confirmed"].Data["source"] != "voice" {
		t.Errorf("source = %v", seen["tool.confirmed"].Data["source"])
	}
	// The conversation remembers the request, not the word "yes".
	turns := h.engine.Conversation()
	if len(turns) < 1 || turns[0].Text != "tidy the downloads" {
		t.Errorf("conversation = %+v", turns)
	}
}

func TestUngatedEngineAsksBeforeRunningAUserIntent(t *testing.T) {
	// No registry at all: an ungated shell command must never run silently.
	router, err := intent.New(intent.Options{
		Custom: []intent.Custom{{Match: "lock the screen", Run: "hyprlock"}}})
	if err != nil {
		t.Fatal(err)
	}
	runner := &intent.FakeRunner{}
	h := newHarness(t, Options{
		SpeakResponses: false, ConfirmTimeout: 5 * time.Second,
		Intents: router, IntentRunner: runner,
	})
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("lock the screen")
	ev := h.waitFor(t, "tool.confirmation_required")
	if ev.Data["command"] != "hyprlock" {
		t.Errorf("command = %v", ev.Data["command"])
	}
	if err := h.engine.Confirm(false); err != nil {
		t.Fatal(err)
	}
	h.collectUntil(t, "session.finished")
	h.waitIdle(t)
	if runner.Shell() != nil {
		t.Errorf("an ungated command ran: %v", runner.Shell())
	}
}

// TestIntentCancelledMidConfirmationEndsCleanly proves the router cannot
// leave a session wedged in Acting.
func TestIntentCancelledMidConfirmationEndsCleanly(t *testing.T) {
	h := newGatedIntentHarness(t,
		tools.PolicyConfig{Default: tools.PolicyAsk},
		intent.Custom{Match: "tidy the downloads", Run: "rm -rf ~/Downloads/tmp"})

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("tidy the downloads")
	h.waitFor(t, "tool.confirmation_required")
	if err := h.engine.Cancel(); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, "session.cancelled")
	h.waitIdle(t)
	if h.runner.Shell() != nil {
		t.Errorf("a cancelled intent executed: %v", h.runner.Shell())
	}
}

// TestModelToolConfirmationStillReturnsToThinking guards the shared
// confirmation mechanism: adding a second caller must not change where a
// model's tool round resumes.
func TestModelToolConfirmationStillReturnsToThinking(t *testing.T) {
	router, err := intent.New(intent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := tools.NewPolicy(tools.PolicyConfig{Default: tools.PolicyAsk})
	if err != nil {
		t.Fatal(err)
	}
	registry := tools.NewRegistry(nil)
	registry.SetPolicy(policy)
	registry.Register(&recordingTool{result: "ok"})

	h := newHarness(t, Options{})
	bus := NewBus(nil)
	h.events, h.cancel = bus.Subscribe()
	h.engine = NewEngine(h.provider, h.stt, h.tts, h.recorder, h.player, registry, nil, bus, nil, Options{
		Model: "m", ConfirmTimeout: 5 * time.Second, Intents: router,
		IntentRunner: &intent.FakeRunner{},
	})
	h.provider.ToolCallsByRound = [][]ai.ToolCall{
		{{ID: "c1", Name: "run", Arguments: `{"command":"x"}`}},
	}

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("do the thing")
	h.waitFor(t, "tool.confirmation_required")
	if err := h.engine.Confirm(true); err != nil {
		t.Fatal(err)
	}
	ev := h.waitFor(t, "state.changed")
	for ev.Data["state"] == "awaiting_confirmation" {
		ev = h.waitFor(t, "state.changed")
	}
	if ev.Data["state"] != "thinking" {
		t.Errorf("a resolved tool confirmation resumed in %v, want thinking", ev.Data["state"])
	}
	h.collectUntil(t, "session.finished")
	h.waitIdle(t)
}
