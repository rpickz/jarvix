package daemon

// The account over the real IPC surface (#201, ADR 0064): the verbs behind
// `jarvix actions` and `jarvix undo`, driven through a fully wired daemon on
// a real socket.
//
// These are deliberately end-to-end rather than unit tests of the report
// functions. The thing worth proving here is not that a map has the right
// keys — it is that a change made through the daemon's own store lands in the
// account, that the disclosure the client prints is the daemon's own
// sentence, and that a reversal actually rewrites the file.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/tts"
	"github.com/rpickz/jarvix/internal/undo"
)

// undoHarness is a wired daemon plus a client and the account it writes to.
type undoHarness struct {
	d       *Daemon
	client  *ipc.Client
	account *undo.Store
	dir     string
	// socket is kept so a test can open a SECOND connection — a second
	// window on the Account tab, which is the only observer the ordering of
	// the account's writes was ever visible to (#219).
	socket string
}

func startUndoDaemon(t *testing.T) *undoHarness {
	t.Helper()
	dir := t.TempDir()
	paths := config.Paths{Config: dir, Data: dir, State: dir, Runtime: dir,
		Socket: filepath.Join(dir, "j.sock")}
	cfg := testConfig()
	cfg.Audio.MinRecordingMs = 0
	d, err := New(cfg, paths, nil, Deps{
		Provider:    &ai.Fake{Response: "Done."},
		Transcriber: &stt.Fake{Text: "hello computer"},
		Synthesizer: &tts.Fake{},
		Recorder:    &audio.FakeRecorder{Clip: audio.Clip{WAVPath: dir + "/r.wav"}},
		Player:      &audio.FakePlayer{},
		Notifier:    &desktop.FakeNotifier{},
		OpenWindow:  func(context.Context) error { return nil },
		Compositor:  desktop.NewFakeCompositor(),
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDaemon(t, d)
	return &undoHarness{d: d, client: dialDaemon(t, paths.Socket), account: d.account,
		dir: dir, socket: paths.Socket}
}

// recordFileChange puts one reversible file change into the daemon's own
// account, exactly as a tool does: snapshot, mutate, note. Going through the
// real store rather than hand-writing a row is what makes the reversal below
// a test of the whole path.
func (h *undoHarness) recordFileChange(t *testing.T, name, before, after, summary string) undo.Record {
	t.Helper()
	path := filepath.Join(h.dir, name)
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := undo.WithRecorder(context.Background(), h.account)
	snap := undo.Snapshot(ctx, path)
	if err := os.WriteFile(path, []byte(after), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := snap.Note(ctx, undo.Action{Tool: "config.write_entry", Summary: summary})
	if rec.ID == "" {
		t.Fatal("the change was not recorded")
	}
	return rec
}

// TestTheAccountIsListedWithItsBoundDisclosed. The listing is what `jarvix
// actions` prints, and the disclosure travels as one daemon-composed sentence
// so no client invents its own wording for the bound (ADR 0013).
func TestTheAccountIsListedWithItsBoundDisclosed(t *testing.T) {
	h := startUndoDaemon(t)
	h.recordFileChange(t, "config.toml", "before\n", "after\n", `saved the routine "morning"`)
	if _, err := h.account.Append(undo.Action{Tool: "shell.run",
		Summary: "ran rm -rf ./build", Restore: undo.OneWay("shell.run")}); err != nil {
		t.Fatal(err)
	}

	var view struct {
		Actions []struct {
			ID         string `json:"id"`
			Tool       string `json:"tool"`
			Summary    string `json:"summary"`
			Reversible bool   `json:"reversible"`
			Why        string `json:"why"`
		} `json:"actions"`
		Bound      int    `json:"bound"`
		Disclosure string `json:"disclosure"`
		Path       string `json:"path"`
	}
	if err := h.client.Call("undo.list", nil, &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Actions) != 2 {
		t.Fatalf("undo.list returned %d rows, want 2", len(view.Actions))
	}
	// Newest first.
	if view.Actions[0].Tool != "shell.run" {
		t.Errorf("the first row is %q, want the newest action", view.Actions[0].Tool)
	}
	if view.Actions[0].Reversible {
		t.Error("the shell command is listed as reversible")
	}
	if !strings.Contains(view.Actions[0].Why, "has run") {
		t.Errorf("why = %q, does not say why the command cannot be taken back", view.Actions[0].Why)
	}
	if !view.Actions[1].Reversible {
		t.Errorf("the config write is listed as irreversible: %q", view.Actions[1].Why)
	}
	if view.Bound != undo.MaxActions {
		t.Errorf("bound = %d, want %d", view.Bound, undo.MaxActions)
	}
	if !strings.Contains(view.Disclosure, "I keep the last") {
		t.Errorf("disclosure = %q, want the daemon's own sentence about the bound", view.Disclosure)
	}
	if view.Path == "" {
		t.Error("the listing does not say where the file is, so nobody can go and read it")
	}
}

// TestUndoThatReversesTheMostRecentReversibleAction is the ticket's headline
// criterion, over the wire: no id, the last reversible change goes back, and
// the answer names it plainly.
func TestUndoThatReversesTheMostRecentReversibleAction(t *testing.T) {
	h := startUndoDaemon(t)
	h.recordFileChange(t, "config.toml", "before\n", "after\n", `saved the routine "morning"`)

	var res struct {
		Done       bool   `json:"done"`
		Refused    bool   `json:"refused"`
		Spoken     string `json:"spoken"`
		ReversalID string `json:"reversal_id"`
	}
	if err := h.client.Call("undo.apply", map[string]any{}, &res); err != nil {
		t.Fatal(err)
	}
	if !res.Done || res.Refused {
		t.Fatalf("undo.apply = %+v, want it done", res)
	}
	if !strings.Contains(res.Spoken, "config.toml") {
		t.Errorf("spoken = %q, does not name what was put back", res.Spoken)
	}
	body, err := os.ReadFile(filepath.Join(h.dir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "before\n" {
		t.Errorf("the file reads %q after the undo, want it back the way it was", body)
	}
	if res.ReversalID == "" {
		t.Error("the reversal earned no row of its own")
	}
}

// TestAnUndoOverTheWireRefusesRatherThanClobbering: the guard applies on
// every path, including the manager's own `jarvix undo`. "Restoring this
// would destroy newer work" is not something the person asking can be assumed
// to know, so it is refused rather than confirmed.
func TestAnUndoOverTheWireRefusesRatherThanClobbering(t *testing.T) {
	h := startUndoDaemon(t)
	h.recordFileChange(t, "config.toml", "before\n", "after\n", `saved the routine "morning"`)
	newer := "after, and then a hand edit\n"
	if err := os.WriteFile(filepath.Join(h.dir, "config.toml"), []byte(newer), 0o600); err != nil {
		t.Fatal(err)
	}

	var res struct {
		Done    bool   `json:"done"`
		Refused bool   `json:"refused"`
		Spoken  string `json:"spoken"`
	}
	if err := h.client.Call("undo.apply", map[string]any{}, &res); err != nil {
		t.Fatal(err)
	}
	if res.Done {
		t.Fatal("the undo overwrote a hand edit")
	}
	if !res.Refused || !strings.Contains(res.Spoken, "has changed since") {
		t.Errorf("undo.apply = %+v, want a refusal that says what it found", res)
	}
	body, err := os.ReadFile(filepath.Join(h.dir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != newer {
		t.Errorf("the hand edit was changed anyway: %q", body)
	}
}

// TestAnUnknownActionIsRefusedRatherThanIgnored: an id nothing answers to is
// a JSON-RPC error, not a quiet success that would let a script believe it
// had put something back.
func TestAnUnknownActionIsRefusedRatherThanIgnored(t *testing.T) {
	h := startUndoDaemon(t)
	var res map[string]any
	err := h.client.Call("undo.apply", map[string]any{"id": "a999"}, &res)
	if err == nil {
		t.Fatal("undoing an unknown id reported success")
	}
	if !strings.Contains(err.Error(), "a999") {
		t.Errorf("the refusal %q does not name the id that was asked for", err)
	}
}

// TestTheAccountSurvivesADaemonRestart: the record is on disk, so a manager
// can review what was done in their name yesterday. The clock is only used to
// prove the row that comes back is the row that went in.
func TestTheAccountSurvivesADaemonRestart(t *testing.T) {
	h := startUndoDaemon(t)
	rec := h.recordFileChange(t, "config.toml", "before\n", "after\n", `saved the routine "morning"`)

	reopened := undo.NewStore(h.account.Path(), undo.StoreOptions{
		Now: func() time.Time { return time.Now() }}, nil)
	got, err := reopened.Get(rec.ID)
	if err != nil {
		t.Fatalf("the account did not survive: %v", err)
	}
	if got.Summary != rec.Summary || !got.Reversible() {
		t.Errorf("after a restart the row reads %+v, want the one that was written", got)
	}
}

// TestASecondWindowSeesTheRowUndoneOnTheEventAlone is #219's acceptance
// criterion over the real socket, with two subscriptions and one press.
//
// The window that presses the button was never the one at risk: it re-reads
// from its own `undo.apply` reply, which arrives after everything the reversal
// writes. Every other subscriber has only the event, and the event used to fire
// when the reversal's own row was written and before the row it reversed was
// marked — so a second window re-reading on it saw a reversal listed beside a
// row that still offered an Undo control for something already undone.
//
// So the second connection is driven for real rather than inferred from the
// first. It is dialled AFTER the change is recorded, so the only `undo.changed`
// it can see is the reversal's, and through dialDaemon's own round trip, which
// is what makes its subscription certain rather than merely likely (#179).
//
// **What this test is and is not.** It is the end-to-end half: two real
// subscriptions, and the wire report — `can_undo`, `why`, `state`, `undone_by`
// — read across a connection that was told nothing but the event. It is NOT
// the test that would have caught the defect, and saying so is the point. The
// old ordering left a window of one file write between the announcement and
// the mark, and a socket round trip is not a barrier against a write on
// another goroutine: run against the two-write version this passes twenty
// times out of twenty, because the marking write wins the race on any machine
// that is not starved. A test that catches a fault only when the runner is
// loaded is the shape internal/testdiscipline exists to ban, so the ordering
// itself is pinned where it can be pinned exactly — at the publish seam, with
// no scheduling in it at all, in
// undo.TestTheAccountIsConsistentTheMomentItAnnouncesItself.
func TestASecondWindowSeesTheRowUndoneOnTheEventAlone(t *testing.T) {
	h := startUndoDaemon(t)
	rec := h.recordFileChange(t, "config.toml", "before\n", "after\n", `saved the routine "morning"`)
	second := dialDaemon(t, h.socket)

	var applied struct {
		Done       bool   `json:"done"`
		ReversalID string `json:"reversal_id"`
	}
	if err := h.client.Call("undo.apply", map[string]any{"id": rec.ID}, &applied); err != nil {
		t.Fatal(err)
	}
	if !applied.Done || applied.ReversalID == "" {
		t.Fatalf("undo.apply = %+v, want it done with a row of its own", applied)
	}

	for _, window := range []struct {
		name   string
		client *ipc.Client
	}{
		{"the window that pressed it", h.client},
		{"the second window", second},
	} {
		// The event is the whole of what this window is told. Nothing below
		// consults the reply, and nothing waits on anything else.
		waitForUndoChanged(t, window.client, applied.ReversalID)

		var view struct {
			Actions []struct {
				ID         string `json:"id"`
				Summary    string `json:"summary"`
				Reversible bool   `json:"reversible"`
				CanUndo    bool   `json:"can_undo"`
				Why        string `json:"why"`
				State      string `json:"state"`
				UndoneBy   string `json:"undone_by"`
			} `json:"actions"`
		}
		if err := window.client.Call("undo.list", nil, &view); err != nil {
			t.Fatal(err)
		}
		var reversed, reversal bool
		for _, row := range view.Actions {
			switch row.ID {
			case rec.ID:
				reversed = true
				if row.Reversible || row.CanUndo {
					t.Errorf("%s: the row it has just put back still offers a control "+
						"(reversible=%v can_undo=%v)", window.name, row.Reversible, row.CanUndo)
				}
				if row.UndoneBy != applied.ReversalID {
					t.Errorf("%s: the row names %q as what reversed it, want %q",
						window.name, row.UndoneBy, applied.ReversalID)
				}
				if !strings.Contains(row.Why, "already put that back") {
					t.Errorf("%s: why = %q, want it to say the row is already back",
						window.name, row.Why)
				}
				if !strings.Contains(row.State, "put") {
					t.Errorf("%s: state = %q, want the sentence for a row already put back",
						window.name, row.State)
				}
			case applied.ReversalID:
				reversal = true
			}
		}
		// Both halves, in the same read. An account that had delayed its
		// announcement past its own row would be wrong in the other direction:
		// a window told to look and shown nothing new.
		if !reversed {
			t.Errorf("%s: the row that was reversed is not in the account", window.name)
		}
		if !reversal {
			t.Errorf("%s: the reversal that caused the event has no row of its own", window.name)
		}
	}
}

// waitForUndoChanged drains until the account announces the given row.
//
// Matched on the id rather than taking the first undo.changed, because a
// connection open since before the change was recorded has one of its own
// waiting, and a test that read the account on that one would be asking about
// a moment nobody is claiming anything about.
func waitForUndoChanged(t *testing.T, client *ipc.Client, id string) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev := <-client.Events():
			if ev.Type == "undo.changed" && ev.Data["id"] == id {
				return
			}
		case <-timeout:
			t.Fatalf("no undo.changed for %s", id)
		}
	}
}
