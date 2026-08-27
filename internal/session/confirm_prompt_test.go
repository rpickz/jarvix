package session

import (
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/intent"
	"github.com/rpickz/jarvix/internal/tools"
)

// This file covers issue #119: the spoken half of a permission ask. Two
// modes — the default short prompt that names the action class and points at
// the screen, and the confirmations.speak_details read-out that keeps the old
// verbatim question — and the continuity requirement: a confirmation resolved
// while its question is still being said stops the rest of the read-out and
// resumes the turn immediately, on both the speaker path (a model tool call)
// and the direct path (a user-defined intent).
//
// Display is asserted alongside audio on purpose: whatever the spoken mode,
// the events must still carry the full summary and the verbatim command,
// because the short prompt's "the details are on screen" is only honest while
// that stays true (ADR 0014).

// TestShortPromptIsSpokenByDefault pins the default wording: the action
// class, then the pointer at the screen — never the command text. The event
// stream is unchanged by the mode, so the card and overlay still show the
// full question and the verbatim command.
func TestShortPromptIsSpokenByDefault(t *testing.T) {
	rec := &namedTool{name: "shell.run", result: "removed"}
	h := newGateHarness(t, Options{SpeakResponses: true}, rec, tools.PolicyConfig{})
	scriptShellCall(h, "rm -rf ./build", "Understood.")

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("clean the build dir")
	required := h.waitFor(t, "tool.confirmation_required")
	// The visual surfaces are mode-independent: full question, verbatim
	// command, on the bus exactly as before.
	if required.Data["command"] != "rm -rf ./build" {
		t.Errorf("event command = %v, want it verbatim whatever is spoken", required.Data["command"])
	}
	if s, _ := required.Data["summary"].(string); !strings.Contains(s, "rm -rf ./build") {
		t.Errorf("event summary %q no longer quotes the command; the card would go blind", s)
	}
	// The deadline event is published only after the question has been asked
	// aloud, so once it arrives the prompt's synthesis request is on record.
	h.waitFor(t, "tool.confirmation_deadline")
	if got, want := h.tts.LastRequest.Text, "May I run a shell command? The details are on screen."; got != want {
		t.Errorf("spoken prompt = %q, want the short default %q", got, want)
	}

	_ = h.engine.Confirm(false)
	h.countUntil(t, "session.finished")
	h.waitIdle(t)
}

// TestVerbatimPromptWhenSpeakDetailsIsOn pins the opt-in: with
// confirmations.speak_details the spoken question is the full generated
// summary — today's behaviour, exactly, quoting the command.
func TestVerbatimPromptWhenSpeakDetailsIsOn(t *testing.T) {
	rec := &namedTool{name: "shell.run", result: "removed"}
	h := newGateHarness(t, Options{SpeakResponses: true, SpeakConfirmationDetails: true},
		rec, tools.PolicyConfig{})
	scriptShellCall(h, "rm -rf ./build", "Understood.")

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("clean the build dir")
	required := h.waitFor(t, "tool.confirmation_required")
	summary, _ := required.Data["summary"].(string)
	h.waitFor(t, "tool.confirmation_deadline")
	if got := h.tts.LastRequest.Text; got != summary {
		t.Errorf("spoken prompt = %q, want the full summary %q", got, summary)
	}
	if !strings.Contains(h.tts.LastRequest.Text, "rm -rf ./build") {
		t.Errorf("spoken prompt %q does not quote the command; speak_details must restore the verbatim read-out",
			h.tts.LastRequest.Text)
	}

	_ = h.engine.Confirm(false)
	h.countUntil(t, "session.finished")
	h.waitIdle(t)
}

// TestShortPromptWording pins every action-class template, so a wording
// change is a reviewed decision — these sentences are the product's voice —
// and so an unmapped tool provably names itself rather than borrowing a
// friendlier class.
func TestShortPromptWording(t *testing.T) {
	cases := map[string]string{
		"shell.run":                      "May I run a shell command? The details are on screen.",
		tools.IntentToolName:             "May I run your custom command? The details are on screen.",
		tools.ScriptToolName:             "May I run one of your scripts? The details are on screen.",
		tools.RoutineToolName:            "May I run one of your routines? The details are on screen.",
		tools.AdvisorToolName:            "May I consult another assistant? The details are on screen.",
		tools.KnowledgeRefreshToolName:   "May I refresh one of your feeds? The details are on screen.",
		tools.TypeTextToolName:           "May I type on your keyboard? The details are on screen.",
		tools.PressKeyToolName:           "May I type on your keyboard? The details are on screen.",
		tools.MemoryForgetToolName:       "May I forget one of your saved facts? The details are on screen.",
		tools.ConfigWriteSettingToolName: "May I change one of your settings? The details are on screen.",
		tools.ConfigWriteEntryToolName:   "May I save a configuration entry? The details are on screen.",
		tools.ConfigDeleteEntryToolName:  "May I delete a configuration entry? The details are on screen.",
		"mystery.op":                     "May I use the mystery.op tool? The details are on screen.",
	}
	for tool, want := range cases {
		if got := shortConfirmationPrompt(tool); got != want {
			t.Errorf("shortConfirmationPrompt(%q) = %q, want %q", tool, got, want)
		}
	}
}

// TestResolutionMidReadoutResumesTheTurn is the continuity requirement on the
// speaker path: the user answers while the question is still being said (the
// synthesizer is parked on the hold gate), and the turn must resume at once —
// the tool starts without the read-out ever finishing, and no deadline event
// goes out because the clock never got to start.
func TestResolutionMidReadoutResumesTheTurn(t *testing.T) {
	rec := &namedTool{name: "shell.run", result: "removed"}
	h := newGateHarness(t, Options{SpeakResponses: true}, rec, tools.PolicyConfig{})
	scriptShellCall(h, "rm -rf ./build", "Done, the build directory is gone.")
	hold := make(chan struct{})
	h.tts.SetHold(hold)

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("clean the build dir")
	h.waitFor(t, "tool.confirmation_required")
	// The question really is mid read-out: its synthesis has begun and is
	// parked on the gate, exactly like a long sentence still being said.
	waitUntil(t, "the question reaches the synthesizer", func() bool { return h.tts.Speaks() > 0 })

	if err := h.engine.Confirm(true); err != nil {
		t.Fatal(err)
	}
	// tool.started before the hold is released is the whole point: the
	// approved command begins while the question's audio is still parked, so
	// the turn provably did not wait out the read-out.
	h.waitFor(t, "tool.started")
	close(hold) // let the answer be spoken

	counts := h.countUntil(t, "session.finished")
	h.waitIdle(t)
	if rec.calls != 1 {
		t.Errorf("tool ran %d times, want 1", rec.calls)
	}
	if counts["error"] != 0 {
		t.Errorf("the session failed instead of finishing: %d error events", counts["error"])
	}
	// Resolved while the question was still being spoken: the countdown never
	// started, so no deadline may be announced for a confirmation that no
	// longer exists — the overlay and card would show a ghost timer.
	if counts["tool.confirmation_deadline"] != 0 {
		t.Errorf("tool.confirmation_deadline published %d times after a mid-read-out resolution, want 0",
			counts["tool.confirmation_deadline"])
	}
}

// TestResolutionSkipsAQueuedPrompt covers the question that never got its
// turn: raised while an answer sentence is still playing (the #52 shape), it
// is queued behind that sentence — and if the user answers before the queue
// reaches it, it must be skipped entirely, not said to a user who has already
// decided. The answer's own audio is untouched.
func TestResolutionSkipsAQueuedPrompt(t *testing.T) {
	ss := startSpeakingSession(t, nil, tools.PolicyConfig{},
		"rm -rf ./build", "Okay, that is dealt with.")
	ss.waitFor(t, "tool.confirmation_required")

	// The preamble is still held in the synthesizer, so the question is
	// queued behind it and cannot have been spoken. Answer now.
	if err := ss.engine.Confirm(true); err != nil {
		t.Fatal(err)
	}
	close(ss.hold) // the preamble finishes; the cancelled question is skipped

	counts := ss.countUntil(t, "session.finished")
	ss.waitIdle(t)
	if counts["error"] != 0 {
		t.Errorf("the session failed instead of finishing: %d error events", counts["error"])
	}
	if ss.tool.calls != 1 {
		t.Errorf("tool ran %d times, want 1", ss.tool.calls)
	}
	// Preamble and answer only: the skipped question never reached the
	// synthesizer. (The #52 baseline with the question spoken is 3 — see
	// assertOneVoiceAtAtime's speaks count.)
	if speaks := ss.tts.Speaks(); speaks != 2 {
		t.Errorf("sentences synthesized = %d, want 2 (preamble, answer): the resolved question must be skipped", speaks)
	}
	// And still one voice at a time: everything shared the one stream.
	if _, plays := ss.player.Played(); plays != 1 {
		t.Errorf("playback streams opened = %d, want 1 for the whole turn", plays)
	}
	if counts["tool.confirmation_deadline"] != 0 {
		t.Errorf("tool.confirmation_deadline published %d times for a question resolved before it was asked, want 0",
			counts["tool.confirmation_deadline"])
	}
}

// TestResolutionMidReadoutStopsADirectPrompt is the same continuity on the
// direct path: a user-defined intent asks before its turn has a voice, so the
// question plays outside any speaker — and answering mid read-out must stop
// that audio and let the intent proceed, exactly as on the speaker path.
func TestResolutionMidReadoutStopsADirectPrompt(t *testing.T) {
	h := newIntentHarness(t, Options{SpeakResponses: true},
		intent.Custom{Match: "tidy the downloads", Run: "rm -rf ~/Downloads/tmp", Say: "Tidied."})
	hold := make(chan struct{})
	h.tts.SetHold(hold)

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("tidy the downloads")
	h.waitFor(t, "tool.confirmation_required")
	waitUntil(t, "the question reaches the synthesizer", func() bool { return h.tts.Speaks() > 0 })

	if err := h.engine.Confirm(true); err != nil {
		t.Fatal(err)
	}
	// The command running while the question's audio is still parked on the
	// hold gate proves the resolution cut the read-out short.
	waitUntil(t, "the approved command runs", func() bool { return h.runner.Shell() != nil })
	close(hold) // let the acknowledgement be spoken

	counts := h.countUntil(t, "session.finished")
	h.waitIdle(t)
	if counts["error"] != 0 {
		t.Errorf("the session failed instead of finishing: %d error events", counts["error"])
	}
	if counts["tool.confirmation_deadline"] != 0 {
		t.Errorf("tool.confirmation_deadline published %d times after a mid-read-out resolution, want 0",
			counts["tool.confirmation_deadline"])
	}
}
