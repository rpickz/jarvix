package daemon

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/tts"
)

// These tests drive the conversation window's composer the only way it can be
// driven from Go: through the daemon's IPC surface, exactly as the QML sends
// it (issue #35). The window itself is display-only (ADR 0013), so this is
// where the behaviour behind Enter is pinned down.

// startTypingDaemon runs a wired daemon over a real socket, like startDaemon,
// but lets a test shape the configuration first — typed turns care about
// conversation memory and about the tool gate, both of which the shared
// helper deliberately turns off.
func startTypingDaemon(t *testing.T, shape func(*config.Config)) (*ipc.Client, *ai.Fake) {
	t.Helper()
	dir := t.TempDir()
	paths := config.Paths{
		Config: dir, Data: dir, State: dir, Runtime: dir,
		Socket: filepath.Join(dir, "j.sock"),
	}
	cfg := testConfig()
	cfg.Audio.MinRecordingMs = 0
	if shape != nil {
		shape(&cfg)
	}
	provider := &ai.Fake{Response: "Streaming works."}
	d, err := New(cfg, paths, nil, Deps{
		Provider:    provider,
		Transcriber: &stt.Fake{Text: "and what about the second one"},
		Synthesizer: &tts.Fake{},
		Recorder:    &audio.FakeRecorder{Clip: audio.Clip{WAVPath: dir + "/r.wav"}},
		Player:      &audio.FakePlayer{},
		Notifier:    &desktop.FakeNotifier{},
		OpenWindow:  func(context.Context) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDaemon(t, d)
	return dialDaemon(t, paths.Socket), provider
}

// waitForEvent drains until the wanted event arrives, failing on any error
// event on the way — an unexpected failure is more useful than a timeout.
func waitForEvent(t *testing.T, client *ipc.Client, want string) map[string]any {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev := <-client.Events():
			if ev.Type == want {
				return ev.Data
			}
			if ev.Type == "error" && want != "error" {
				t.Fatalf("waiting for %q, got error event: %v", want, ev.Data)
			}
		case <-timeout:
			t.Fatalf("no %s event", want)
		}
	}
}

// conversationTurns reads the window's own snapshot method, which is how the
// conversation window renders history.
func conversationTurns(t *testing.T, client *ipc.Client) []map[string]any {
	t.Helper()
	var snapshot struct {
		Turns []map[string]any `json:"turns"`
	}
	if err := client.Call("conversation.get", nil, &snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot.Turns
}

// A typed question is answered exactly as a spoken one, and lands in the same
// conversation: the spoken turn that follows finds it already there. This is
// the AC that makes typing and speaking one conversation rather than two.
func TestTypedTurnIsAnsweredAndSharedWithTheNextSpokenTurn(t *testing.T) {
	client, _ := startTypingDaemon(t, nil)
	const typed = "summarise https://example.com/some/long/path"

	var result struct {
		SessionID    string `json:"session_id"`
		Confirmation bool   `json:"confirmation"`
	}
	if err := client.Call("session.text", map[string]string{"text": typed}, &result); err != nil {
		t.Fatal(err)
	}
	if result.SessionID == "" {
		t.Error("session.text reported no session")
	}
	if result.Confirmation {
		t.Error("nothing was pending; this was a question")
	}

	if got := waitForEvent(t, client, "transcript.final")["text"]; got != typed {
		t.Errorf("transcript.final = %v; the window draws the user turn from this", got)
	}
	if got := waitForEvent(t, client, "assistant.finished")["content"]; got != "Streaming works." {
		t.Errorf("typed question answered with %v", got)
	}
	waitForEvent(t, client, "session.finished")

	turns := conversationTurns(t, client)
	if len(turns) != 2 || turns[0]["role"] != "user" || turns[0]["text"] != typed {
		t.Fatalf("conversation after a typed turn = %v", turns)
	}

	// Now speak, the ordinary way, and check the typed exchange is still
	// there underneath the new one.
	if err := client.Call("session.start", nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"voice.start", "voice.stop", "session.submit"} {
		if err := client.Call(method, nil, nil); err != nil {
			t.Fatalf("%s: %v", method, err)
		}
	}
	waitForEvent(t, client, "session.finished")

	turns = conversationTurns(t, client)
	if len(turns) != 4 {
		t.Fatalf("conversation after typing then speaking = %v", turns)
	}
	if turns[0]["text"] != typed {
		t.Errorf("the typed turn was lost: %v", turns)
	}
	if turns[2]["text"] != "and what about the second one" {
		t.Errorf("the spoken turn did not join the same conversation: %v", turns)
	}
}

// Typing while Jarvix is answering interrupts it, exactly as speaking over it
// does. The interrupted session is cancelled and the typed turn gets a fresh
// one.
func TestTypedTurnInterruptsTheAnswerInFlight(t *testing.T) {
	client, provider := startTypingDaemon(t, nil)
	provider.Delay = 50 * time.Millisecond

	var first, second struct {
		SessionID string `json:"session_id"`
	}
	if err := client.Call("session.text", map[string]string{"text": "the long one"}, &first); err != nil {
		t.Fatal(err)
	}
	// A delta proves the provider call is open and streaming — the state the
	// AC calls "mid-answer".
	waitForEvent(t, client, "assistant.delta")

	if err := client.Call("session.text", map[string]string{"text": "no, this instead"}, &second); err != nil {
		t.Fatal(err)
	}
	if second.SessionID == first.SessionID {
		t.Fatalf("the interrupting turn reused session %s", second.SessionID)
	}
	cancelled := waitForEvent(t, client, "session.cancelled")
	if cancelled["session_id"] != first.SessionID {
		t.Errorf("cancelled %v, want the interrupted %s", cancelled["session_id"], first.SessionID)
	}
	if got := waitForEvent(t, client, "transcript.final")["text"]; got != "no, this instead" {
		t.Errorf("the new turn's question = %v", got)
	}
}

// With a tool call waiting on the user, "yes" answers it. Starting a session
// here would cancel the very call being approved (ADR 0014), so the daemon
// must read the state and the text together — which is why the decision is
// not left to the window.
func TestTypedYesResolvesAPendingConfirmation(t *testing.T) {
	client, provider := startTypingDaemon(t, func(cfg *config.Config) {
		cfg.Tools.Shell = true // the real gate; nothing may run without a yes
	})
	provider.Response = "Done."
	provider.ToolCallsByRound = [][]ai.ToolCall{
		{{ID: "c1", Name: "shell.run", Arguments: `{"command":"rm -rf ./build"}`}},
	}

	var asked struct {
		SessionID string `json:"session_id"`
	}
	if err := client.Call("session.text", map[string]string{"text": "clean up"}, &asked); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "tool.confirmation_required")

	var answer struct {
		SessionID    string `json:"session_id"`
		Confirmation bool   `json:"confirmation"`
		Approved     bool   `json:"approved"`
	}
	if err := client.Call("session.text", map[string]string{"text": "yes"}, &answer); err != nil {
		t.Fatal(err)
	}
	if !answer.Confirmation || !answer.Approved {
		t.Fatalf("typed answer = %+v, want an approved confirmation", answer)
	}
	if answer.SessionID != asked.SessionID {
		t.Errorf("the answer started session %s; it belongs to %s", answer.SessionID, asked.SessionID)
	}
	waitForEvent(t, client, "tool.confirmed")
}

// The other half: anything that is not a clear affirmative declines, using
// the same parser a spoken reply goes through — there is one reading of
// "no", not two.
func TestTypedNoDeclinesThePendingConfirmation(t *testing.T) {
	client, provider := startTypingDaemon(t, func(cfg *config.Config) {
		cfg.Tools.Shell = true
	})
	provider.Response = "Nothing was deleted."
	provider.ToolCallsByRound = [][]ai.ToolCall{
		{{ID: "c1", Name: "shell.run", Arguments: `{"command":"rm -rf ./build"}`}},
	}

	if err := client.Call("session.text", map[string]string{"text": "clean up"}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "tool.confirmation_required")

	var answer struct {
		Confirmation bool `json:"confirmation"`
		Approved     bool `json:"approved"`
	}
	if err := client.Call("session.text", map[string]string{"text": "no, don't"}, &answer); err != nil {
		t.Fatal(err)
	}
	if !answer.Confirmation {
		t.Fatal("a reply to a pending confirmation must be reported as one")
	}
	if answer.Approved {
		t.Error(`"no, don't" was read as approval`)
	}
	declined := waitForEvent(t, client, "tool.declined")
	if declined["source"] != "text" {
		t.Errorf("decline source = %v, want text", declined["source"])
	}
}

// Enter on an empty field must do nothing at all — no session, no
// interruption, no provider request. Whitespace is empty too.
func TestEmptyTypedInputStartsNoSession(t *testing.T) {
	client, _ := startTypingDaemon(t, nil)
	for _, text := range []string{"", "   ", "\t\n"} {
		err := client.Call("session.text", map[string]string{"text": text}, nil)
		rpcErr, ok := err.(*ipc.Error)
		if !ok || rpcErr.Code != ipc.CodeInvalidParams {
			t.Errorf("session.text(%q) error = %v, want invalid params", text, err)
		}
		var status map[string]any
		if err := client.Call("status.get", nil, &status); err != nil {
			t.Fatal(err)
		}
		if status["state"] != "idle" || status["session_id"] != "" {
			t.Fatalf("session.text(%q) left the daemon at %v/%v", text, status["state"], status["session_id"])
		}
	}
	// Missing params entirely is the same thing as an empty string.
	if err := client.Call("session.text", nil, nil); err == nil {
		t.Error("session.text with no params was accepted")
	}
}

// Malformed params must be rejected as params, not blamed on the session.
func TestTypedInputWithBadParamsIsInvalidParams(t *testing.T) {
	client, _ := startTypingDaemon(t, nil)
	err := client.Call("session.text", json.RawMessage(`"not an object"`), nil)
	rpcErr, ok := err.(*ipc.Error)
	if !ok || rpcErr.Code != ipc.CodeInvalidParams {
		t.Errorf("err = %v, want invalid params", err)
	}
}
