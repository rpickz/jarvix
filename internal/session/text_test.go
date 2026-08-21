package session

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/tools"
)

// Typing and speaking must be one conversation, not two. A typed question is
// committed to history exactly like a spoken one, so the spoken follow-up
// that comes next carries it — which is the whole point of typing the URL
// rather than reading it aloud.
func TestTypedTurnIsCommittedToHistoryForTheNextSpokenTurn(t *testing.T) {
	h := newHarness(t, Options{HistoryTurns: 4})
	const typed = "summarise https://example.com/some/long/path"

	result, err := h.engine.SubmitText(typed)
	if err != nil {
		t.Fatal(err)
	}
	if result.SessionID == "" {
		t.Error("a typed turn must report the session it started")
	}
	if result.Confirmation {
		t.Error("nothing was pending; this was a question, not an answer")
	}
	seen := h.collectUntil(t, "session.finished")
	h.waitIdle(t)

	if seen["transcript.final"].Data["text"] != typed {
		t.Errorf("transcript.final = %v; the window renders the user turn from this event",
			seen["transcript.final"].Data["text"])
	}
	if seen["assistant.finished"].Data["content"] != h.provider.Response {
		t.Errorf("typed question was answered with %v", seen["assistant.finished"].Data["content"])
	}

	turns := h.engine.Conversation()
	if len(turns) != 2 || turns[0].Role != "user" || turns[0].Text != typed {
		t.Fatalf("conversation after a typed turn = %+v", turns)
	}

	// Now speak. The fake transcriber answers "explain recursion"; what
	// matters is that the model is shown the typed exchange before it.
	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.StartVoice(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.StopVoice(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit(""); err != nil {
		t.Fatal(err)
	}
	h.collectUntil(t, "session.finished")
	h.waitIdle(t)

	msgs := h.provider.LastRequest.Messages
	var carried bool
	for _, m := range msgs {
		if m.Content == typed {
			carried = true
		}
	}
	if !carried {
		t.Errorf("the spoken follow-up did not carry the typed turn as context: %+v", msgs)
	}
}

// Typing over a running answer is the same contract as speaking over it: the
// session in flight is interrupted and the new turn begins. Anything else
// would make the composer a second-class way to talk to Jarvix.
func TestTypedTurnInterruptsTheSessionInFlight(t *testing.T) {
	h := newHarness(t, Options{SpeakResponses: true})
	// Slow enough that the first session is provably still mid-answer when
	// the second arrives, fast enough not to pad the suite.
	h.provider.Delay = 50 * time.Millisecond

	first, err := h.engine.SubmitText("first question")
	if err != nil {
		t.Fatal(err)
	}
	// The first *delta*, not assistant.started: a delta is proof the provider
	// call is open and streaming, which is the moment the AC is about.
	h.waitFor(t, "assistant.delta")
	if state, _ := h.engine.State(); state == StateIdle {
		t.Fatal("the first session was already over; nothing was interrupted")
	}

	second, err := h.engine.SubmitText("second question")
	if err != nil {
		t.Fatal(err)
	}
	if second.SessionID == first.SessionID {
		t.Fatalf("the interrupting turn reused session %s; it must be a new one", second.SessionID)
	}
	cancelled := h.waitFor(t, "session.cancelled")
	if cancelled.Data["session_id"] != first.SessionID {
		t.Errorf("cancelled session = %v, want the interrupted %s",
			cancelled.Data["session_id"], first.SessionID)
	}

	// The interrupting turn runs to completion on its own session. Asserted
	// from the events rather than from the fake provider: the cancelled
	// session's goroutine is still draining, and the fake records requests
	// without a lock.
	seen := h.collectUntil(t, "session.finished")
	h.waitIdle(t)
	if seen["transcript.final"].Data["text"] != "second question" {
		t.Errorf("the answered question was %v", seen["transcript.final"].Data["text"])
	}
	if seen["session.finished"].Data["session_id"] != second.SessionID {
		t.Errorf("session %v finished; expected the interrupting %s",
			seen["session.finished"].Data["session_id"], second.SessionID)
	}
}

// With a tool call waiting on the user, "yes" is an answer, not a new
// question. Starting a session here would cancel the very tool call the user
// just approved — the confirmation would be silently abandoned.
func TestTypedYesResolvesAPendingConfirmationWithoutStartingASession(t *testing.T) {
	rec := &namedTool{name: "shell.run", result: "removed"}
	h := newGateHarness(t, Options{}, rec, tools.PolicyConfig{})
	scriptShellCall(h, "rm -rf ./build", "Build directory removed.")

	asked, err := h.engine.SubmitText("clean the build dir")
	if err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, "tool.confirmation_required")

	answered, err := h.engine.SubmitText("  yes  ")
	if err != nil {
		t.Fatal(err)
	}
	if !answered.Confirmation || !answered.Approved {
		t.Fatalf("typed answer = %+v, want an approved confirmation", answered)
	}
	if answered.SessionID != asked.SessionID {
		t.Errorf("the answer moved to session %s; it belongs to %s",
			answered.SessionID, asked.SessionID)
	}

	counts := h.countUntil(t, "session.finished")
	h.waitIdle(t)
	if counts["session.cancelled"] != 0 {
		t.Error("answering a confirmation must not interrupt the session that asked")
	}
	if counts["tool.confirmed"] != 1 {
		t.Errorf("tool.confirmed = %d, want 1", counts["tool.confirmed"])
	}
	if rec.calls != 1 {
		t.Errorf("tool ran %d times, want 1", rec.calls)
	}
}

// The negative half of the same gate, through the same parser: anything that
// is not a clear affirmative declines, and nothing runs.
func TestTypedNoDeclinesThePendingConfirmation(t *testing.T) {
	rec := &namedTool{name: "shell.run", result: "removed"}
	h := newGateHarness(t, Options{}, rec, tools.PolicyConfig{})
	scriptShellCall(h, "rm -rf ./build", "Nothing was deleted.")

	if _, err := h.engine.SubmitText("clean the build dir"); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, "tool.confirmation_required")

	answered, err := h.engine.SubmitText("no, leave it alone")
	if err != nil {
		t.Fatal(err)
	}
	if !answered.Confirmation {
		t.Fatal("a reply to a pending confirmation must be reported as one")
	}
	if answered.Approved {
		t.Error("\"no, leave it alone\" was read as approval")
	}

	counts := h.countUntil(t, "session.finished")
	h.waitIdle(t)
	if counts["tool.declined"] != 1 {
		t.Errorf("tool.declined = %d, want 1", counts["tool.declined"])
	}
	if rec.calls != 0 {
		t.Errorf("a declined command ran %d times", rec.calls)
	}
}

// Enter on an empty field must be inert. A blank turn is not free: it would
// interrupt whatever Jarvix was saying and spend a provider request asking
// nothing.
func TestEmptyTypedInputStartsNothing(t *testing.T) {
	h := newHarness(t, Options{})
	for _, text := range []string{"", "   ", "\t\n ", " "} {
		result, err := h.engine.SubmitText(text)
		if !errors.Is(err, ErrEmptyText) {
			t.Errorf("SubmitText(%q) error = %v, want ErrEmptyText", text, err)
		}
		if result.SessionID != "" {
			t.Errorf("SubmitText(%q) started session %s", text, result.SessionID)
		}
		if state, id := h.engine.State(); state != StateIdle || id != "" {
			t.Fatalf("SubmitText(%q) left the engine at %s/%s", text, state, id)
		}
	}
	select {
	case ev := <-h.events:
		t.Fatalf("an empty submission published %q", ev.Type)
	default:
	}
}

// Surrounding whitespace is a typing artefact, not part of the question: the
// model, the history, and the window should all see the same clean string.
func TestTypedTextIsTrimmedBeforeItBecomesATurn(t *testing.T) {
	h := newHarness(t, Options{})
	if _, err := h.engine.SubmitText("  what is a monad?\n"); err != nil {
		t.Fatal(err)
	}
	seen := h.collectUntil(t, "session.finished")
	h.waitIdle(t)
	if got := seen["transcript.final"].Data["text"]; got != "what is a monad?" {
		t.Errorf("transcript.final = %q, want the trimmed question", got)
	}
	last := h.provider.LastRequest.Messages
	if got := last[len(last)-1].Content; strings.TrimSpace(got) != got {
		t.Errorf("the model was sent untrimmed text %q", got)
	}
}
