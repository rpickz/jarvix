package daemon

// The speech.replay verb over a real socket (issue #122): the wire shape, the
// activity row, the busy refusal, and address stability across the record
// rebuild a conversation.open performs. The engine-level policy (precedence,
// supersession, cancel) is pinned in internal/session/replay_test.go; these
// tests cover what only the daemon adds.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/tts"
)

func TestSpeechReplayOverSocket(t *testing.T) {
	f := startConvDaemon(t, config.RetentionOn)
	f.ask(t, "hello archive")

	// No params: the daemon resolves the newest assistant turn itself — the
	// `jarvix say-again` shape.
	var out struct {
		Turn int    `json:"turn"`
		Role string `json:"role"`
	}
	if err := f.client.Call("speech.replay", nil, &out); err != nil {
		t.Fatal(err)
	}
	if out.Turn != 2 || out.Role != "assistant" {
		t.Fatalf("replay resolved (%d, %s), want (2, assistant)", out.Turn, out.Role)
	}
	waitForEvent(t, f.client, "tts.started")
	replayed := waitForEvent(t, f.client, "speech.replayed")
	if got, _ := replayed["turn"].(float64); int(got) != 2 {
		t.Errorf("speech.replayed turn = %v", replayed["turn"])
	}

	// The activity ring shows the replay row, worded daemon-side. Waited for
	// as its own push: rows are derived by the feed's subscriber and may
	// trail the engine events (docs/ipc.md), but they do arrive on the bus.
	deadline := time.After(5 * time.Second)
	for {
		var row map[string]any
		select {
		case ev := <-f.client.Events():
			if ev.Type != "activity.row" {
				continue
			}
			row = ev.Data
		case <-deadline:
			t.Fatal("no replay row reached the activity feed")
		}
		if label, _ := row["label"].(string); label == "Spoke turn 2 again" {
			if detail, _ := row["detail"].(string); detail != "Jarvix's message" {
				t.Errorf("row detail = %q", detail)
			}
			break
		}
	}

	// The record is untouched: still one exchange, no replay turn.
	turns := conversationTurns(t, f.client)
	if len(turns) != 2 {
		t.Fatalf("replay changed the record: %d turns", len(turns))
	}
}

func TestSpeechReplayRefusedMidTurn(t *testing.T) {
	synth := &tts.Fake{}
	hold := make(chan struct{})
	synth.SetHold(hold)
	defer close(hold)
	f := startConvDaemonSpeaking(t, config.RetentionOn, nil, synth)

	if err := f.client.Call("session.text", map[string]any{"text": "talk to me"}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, f.client, "tts.started")

	err := f.client.Call("speech.replay", map[string]any{"turn": 1, "role": "user"}, nil)
	if err == nil {
		t.Fatal("replay accepted while the conversation was speaking")
	}
	var rpcErr *ipc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != ipc.CodeSessionError {
		t.Fatalf("err = %v, want the session-error code", err)
	}
	if !strings.Contains(rpcErr.Message, "busy") {
		t.Errorf("refusal message %q should say busy", rpcErr.Message)
	}
}

func TestSpeechReplayBadAddressIsInvalidParams(t *testing.T) {
	f := startConvDaemon(t, config.RetentionOn)
	f.ask(t, "hello")
	err := f.client.Call("speech.replay", map[string]any{"turn": 9}, nil)
	if err == nil {
		t.Fatal("out-of-range address accepted")
	}
	var rpcErr *ipc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != ipc.CodeInvalidParams {
		t.Fatalf("err = %v, want the invalid-params code", err)
	}
}

func TestSpeechReplayWorksAfterReopen(t *testing.T) {
	// The rebuilt-history acceptance criterion end to end: archive a
	// conversation, end the thread, reopen the record over the socket, and
	// replay by the same positional address the reopened snapshot shows —
	// #118's records give positional identity, and the conversation.get
	// snapshot is the address space.
	f := startConvDaemon(t, config.RetentionOn)
	f.ask(t, "remember this answer")

	reopened := f.list(t).ActiveID
	if reopened == "" {
		t.Fatal("no active conversation to reopen")
	}
	if err := f.client.Call("conversation.new", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Call("conversation.open", map[string]any{"id": reopened}, nil); err != nil {
		t.Fatal(err)
	}

	var out struct {
		Turn int    `json:"turn"`
		Role string `json:"role"`
	}
	if err := f.client.Call("speech.replay", map[string]any{"turn": 2, "role": "assistant"}, &out); err != nil {
		t.Fatal(err)
	}
	if out.Turn != 2 || out.Role != "assistant" {
		t.Fatalf("reopened replay resolved (%d, %s)", out.Turn, out.Role)
	}
	waitForEvent(t, f.client, "speech.replayed")
	waitForEvent(t, f.client, "session.finished")
}
