package daemon

import (
	"context"
	"os"
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

// Issue #118: approvals are part of the record. The window is a thin client
// (ADR 0013) — everything these tests do rides the socket, and "the window
// was closed and reopened" is a fresh connection reading conversation.get,
// which is also exactly what the #108 kill-rebuild path does. The tests pin:
// a resolved confirmation survives reopen as a turn at its position; declined
// and timed-out are never conflated; a pending confirmation reappears live
// and is not doubled by a history entry; and conversation.open restores
// records like turns.

// startRecordDaemon runs a wired daemon whose real shell gate will ask about
// `command`. confirmTimer, when non-nil, replaces the confirmation-timeout
// clock (the timeout variant fires it instantly; every other test leaves the
// real 30s timer, which never fires within a test's life).
func startRecordDaemon(t *testing.T, command string,
	confirmTimer func(time.Duration) (<-chan time.Time, func())) (*ipc.Client, string) {
	t.Helper()
	dir := t.TempDir()
	paths := config.Paths{
		Config: dir, Data: dir, State: dir, Runtime: dir,
		Socket: filepath.Join(dir, "j.sock"),
	}
	cfg := testConfig()
	cfg.Audio.MinRecordingMs = 0
	cfg.Tools.Shell = true // the real gate decides; unlisted commands are ask-tier
	provider := &ai.Fake{Response: "All done."}
	provider.ToolCallsByRound = [][]ai.ToolCall{
		{{ID: "c1", Name: "shell.run", Arguments: `{"command":"` + command + `"}`}},
	}
	d, err := New(cfg, paths, nil, Deps{
		Provider:     provider,
		Transcriber:  &stt.Fake{Text: "unused"},
		Synthesizer:  &tts.Fake{},
		Recorder:     &audio.FakeRecorder{Clip: audio.Clip{WAVPath: dir + "/r.wav"}},
		Player:       &audio.FakePlayer{},
		Notifier:     &desktop.FakeNotifier{},
		OpenWindow:   func(context.Context) error { return nil },
		ConfirmTimer: confirmTimer,
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDaemon(t, d)
	return dialDaemon(t, paths.Socket), paths.Socket
}

// firedTimer is a confirmation clock that has already expired: the pending
// confirmation times out the moment the engine starts waiting — no sleeps.
func firedTimer(time.Duration) (<-chan time.Time, func()) {
	c := make(chan time.Time, 1)
	c <- time.Time{}
	return c, func() {}
}

// confirmationEntry finds the record turns in a conversation.get / .read turn
// list.
func confirmationEntries(turns []map[string]any) []map[string]any {
	var recs []map[string]any
	for _, turn := range turns {
		if turn["role"] == "confirmation" {
			recs = append(recs, turn)
		}
	}
	return recs
}

// recordOutcome digs the structured outcome out of one record turn.
func recordOutcome(t *testing.T, turn map[string]any) string {
	t.Helper()
	rec, ok := turn["confirmation"].(map[string]any)
	if !ok {
		t.Fatalf("record turn carries no confirmation payload: %v", turn)
	}
	outcome, _ := rec["outcome"].(string)
	return outcome
}

// The headline criterion: request → approve → close → reopen shows the
// confirmation at its position with request details and outcome. The reopened
// window is a second connection whose only knowledge is the snapshot — and
// the archive agrees with it immediately (the #116 barrier: the resolution
// was acknowledged, so conversation.read sees it).
func TestApprovedConfirmationSurvivesWindowReopen(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "approved-ran")
	command := "mkdir -p " + marker
	client, socket := startRecordDaemon(t, command, nil)

	if err := client.Call("session.text", map[string]string{"text": "make the marker"}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "tool.confirmation_required")
	if err := client.Call("session.confirm", map[string]bool{"approved": true}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "tool.confirmed")
	waitForEvent(t, client, "session.finished")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("approved command did not run: %v", err)
	}

	// The reopened window: a fresh connection that saw none of the events.
	late := dialDaemon(t, socket)
	var snapshot struct {
		Turns        []map[string]any `json:"turns"`
		Confirmation map[string]any   `json:"confirmation"`
	}
	if err := late.Call("conversation.get", nil, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Confirmation != nil {
		t.Errorf("resolved confirmation still pending in the snapshot: %v", snapshot.Confirmation)
	}
	if len(snapshot.Turns) != 3 {
		t.Fatalf("reopened snapshot has %d turns, want user/record/answer", len(snapshot.Turns))
	}
	if snapshot.Turns[0]["role"] != "user" || snapshot.Turns[1]["role"] != "confirmation" ||
		snapshot.Turns[2]["role"] != "assistant" {
		t.Fatalf("reopened order = %v/%v/%v, want the record between its turns",
			snapshot.Turns[0]["role"], snapshot.Turns[1]["role"], snapshot.Turns[2]["role"])
	}
	rec, _ := snapshot.Turns[1]["confirmation"].(map[string]any)
	if rec == nil {
		t.Fatal("record turn carries no structured payload")
	}
	if rec["command"] != command {
		t.Errorf("record command = %v, want it verbatim", rec["command"])
	}
	if rec["outcome"] != "approved" {
		t.Errorf("record outcome = %v, want approved", rec["outcome"])
	}
	if summary, _ := snapshot.Turns[1]["text"].(string); summary == "" {
		t.Error("record carries no question text to show")
	}

	// The archive already agrees: the turn's session.finished was seen, so
	// the read behind the barrier holds the record at the same position.
	var listing struct {
		ActiveID string `json:"active_id"`
	}
	if err := late.Call("conversation.list", nil, &listing); err != nil {
		t.Fatal(err)
	}
	if listing.ActiveID == "" {
		t.Fatal("no active conversation after an acknowledged turn (the #116 barrier failed)")
	}
	var read struct {
		Turns []map[string]any `json:"turns"`
	}
	if err := late.Call("conversation.read", map[string]string{"id": listing.ActiveID}, &read); err != nil {
		t.Fatal(err)
	}
	if len(read.Turns) != 3 || read.Turns[1]["role"] != "confirmation" {
		t.Fatalf("archived turns = %v, want the record between the halves", read.Turns)
	}
	if got := recordOutcome(t, read.Turns[1]); got != "approved" {
		t.Errorf("archived outcome = %q, want approved", got)
	}
}

// Declined is declined on reopen — never "resolved", never approved.
func TestDeclinedConfirmationSurvivesWindowReopen(t *testing.T) {
	client, socket := startRecordDaemon(t, "rm -rf ./never-exists", nil)

	if err := client.Call("session.text", map[string]string{"text": "clean up"}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "tool.confirmation_required")
	if err := client.Call("session.confirm", map[string]bool{"approved": false}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "tool.declined")
	waitForEvent(t, client, "session.finished")

	late := dialDaemon(t, socket)
	var snapshot struct {
		Turns []map[string]any `json:"turns"`
	}
	if err := late.Call("conversation.get", nil, &snapshot); err != nil {
		t.Fatal(err)
	}
	recs := confirmationEntries(snapshot.Turns)
	if len(recs) != 1 {
		t.Fatalf("reopened snapshot has %d records, want 1", len(recs))
	}
	if got := recordOutcome(t, recs[0]); got != "declined" {
		t.Errorf("outcome = %q, want declined", got)
	}
}

// A timeout is its own outcome on reopen, distinct from a spoken no. The
// injected clock has already expired, so the daemon declines by timeout the
// moment the countdown starts — deterministically, with no waiting.
func TestTimedOutConfirmationSurvivesWindowReopen(t *testing.T) {
	client, socket := startRecordDaemon(t, "rm -rf ./never-exists", firedTimer)

	if err := client.Call("session.text", map[string]string{"text": "clean up"}, nil); err != nil {
		t.Fatal(err)
	}
	declined := waitForEvent(t, client, "tool.declined")
	if declined["source"] != "timeout" {
		t.Fatalf("declined source = %v, want timeout", declined["source"])
	}
	waitForEvent(t, client, "session.finished")

	late := dialDaemon(t, socket)
	var snapshot struct {
		Turns []map[string]any `json:"turns"`
	}
	if err := late.Call("conversation.get", nil, &snapshot); err != nil {
		t.Fatal(err)
	}
	recs := confirmationEntries(snapshot.Turns)
	if len(recs) != 1 {
		t.Fatalf("reopened snapshot has %d records, want 1", len(recs))
	}
	if got := recordOutcome(t, recs[0]); got != "timed_out" {
		t.Errorf("outcome = %q, want timed_out — a timeout must never read as a spoken no", got)
	}
	rec, _ := recs[0]["confirmation"].(map[string]any)
	if rec["timeout_sec"] != float64(30) {
		t.Errorf("timeout_sec = %v, want the window that applied", rec["timeout_sec"])
	}
}

// The issue's reopen sequence, end to end: request → close → reopen shows the
// LIVE card only (the existing #76 reappear path, not doubled by a history
// entry) → resolve → reopen again shows the static record only.
func TestPendingConfirmationReopensLiveThenBecomesTheRecord(t *testing.T) {
	client, socket := startRecordDaemon(t, "rm -rf ./never-exists", nil)

	if err := client.Call("session.text", map[string]string{"text": "clean up"}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "tool.confirmation_required")

	// Reopen mid-wait: the live card rides the snapshot's confirmation
	// field; the turns carry no record of it — one card, no duplicate.
	midWait := dialDaemon(t, socket)
	var pending struct {
		State        string           `json:"state"`
		Turns        []map[string]any `json:"turns"`
		Confirmation map[string]any   `json:"confirmation"`
	}
	if err := midWait.Call("conversation.get", nil, &pending); err != nil {
		t.Fatal(err)
	}
	if pending.State != "awaiting_confirmation" || pending.Confirmation == nil {
		t.Fatalf("mid-wait snapshot = state %q confirmation %v; the live card must reappear",
			pending.State, pending.Confirmation)
	}
	if recs := confirmationEntries(pending.Turns); len(recs) != 0 {
		t.Fatalf("pending confirmation doubled by %d history record(s)", len(recs))
	}

	// Resolve from the reopened window, exactly as its buttons would.
	if err := midWait.Call("session.confirm", map[string]bool{"approved": false}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "tool.declined")
	waitForEvent(t, client, "session.finished")

	// Reopen again: the live card is gone, the static record stands.
	again := dialDaemon(t, socket)
	var resolved struct {
		Turns        []map[string]any `json:"turns"`
		Confirmation map[string]any   `json:"confirmation"`
	}
	if err := again.Call("conversation.get", nil, &resolved); err != nil {
		t.Fatal(err)
	}
	if resolved.Confirmation != nil {
		t.Errorf("resolved confirmation still offered as live: %v", resolved.Confirmation)
	}
	if recs := confirmationEntries(resolved.Turns); len(recs) != 1 {
		t.Fatalf("final snapshot has %d records, want exactly 1", len(recs))
	}
}

// conversation.open restores confirmation records like turns: end the thread,
// reopen it from the archive, and the record is back at its position in the
// live snapshot.
func TestConversationOpenRestoresConfirmationRecords(t *testing.T) {
	client, _ := startRecordDaemon(t, "rm -rf ./never-exists", nil)

	if err := client.Call("session.text", map[string]string{"text": "clean up"}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "tool.confirmation_required")
	if err := client.Call("session.confirm", map[string]bool{"approved": false}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "session.finished")

	// The thread's id, behind the barrier, then end the thread.
	var listing struct {
		ActiveID string `json:"active_id"`
	}
	if err := client.Call("conversation.list", nil, &listing); err != nil {
		t.Fatal(err)
	}
	if listing.ActiveID == "" {
		t.Fatal("no active conversation to reopen")
	}
	if err := client.Call("conversation.new", nil, nil); err != nil {
		t.Fatal(err)
	}
	var empty struct {
		Turns []map[string]any `json:"turns"`
	}
	if err := client.Call("conversation.get", nil, &empty); err != nil {
		t.Fatal(err)
	}
	if len(empty.Turns) != 0 {
		t.Fatalf("new thread starts with %d turns, want none", len(empty.Turns))
	}

	if err := client.Call("conversation.open", map[string]string{"id": listing.ActiveID}, nil); err != nil {
		t.Fatal(err)
	}
	var reopened struct {
		Turns []map[string]any `json:"turns"`
	}
	if err := client.Call("conversation.get", nil, &reopened); err != nil {
		t.Fatal(err)
	}
	if len(reopened.Turns) != 3 || reopened.Turns[1]["role"] != "confirmation" {
		t.Fatalf("reopened turns = %v, want the record restored between its turns", reopened.Turns)
	}
	if got := recordOutcome(t, reopened.Turns[1]); got != "declined" {
		t.Errorf("restored outcome = %q, want declined", got)
	}
}
