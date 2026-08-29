package undo

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests are the ticket's acceptance criteria, one function each: a
// reversal per reversible kind, an irreversible action marked at the time it
// happens, the refuse-rather-than-clobber case, the bound's disclosure, and
// the gate on an undo that would itself be consequential.
//
// Everything is hermetic. The file reversals run against a temp directory,
// the window reversal against a placer that records what it was asked for,
// the clock is frozen, and nothing sleeps: the store's own promises under a
// failing disk are the fault suite's business (faults_test.go), and what is
// under test here is whether Jarvix puts the right thing back and refuses the
// right thing.

// fixedClock is a monotonic frozen clock, so records sort in the order they
// were appended without any test depending on wall time.
func fixedClock() func() time.Time {
	at := time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC)
	return func() time.Time {
		at = at.Add(time.Minute)
		return at
	}
}

// newTestStore opens an account in a fresh temp directory.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(filepath.Join(t.TempDir(), "undo.toml"),
		StoreOptions{Now: fixedClock()}, nil)
}

// writeFile writes a file the test then changes through the account.
func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// readFile reads one back.
func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// recordFileChange performs one file mutation through the recorder exactly as
// a tool does: snapshot, mutate, note.
func recordFileChange(t *testing.T, store *Store, path, after, summary string) Record {
	t.Helper()
	ctx := WithRecorder(context.Background(), store)
	before := Snapshot(ctx, path)
	writeFile(t, path, after)
	rec := before.Note(ctx, Action{Tool: "config.write_entry", Summary: summary})
	if rec.ID == "" {
		t.Fatal("the change was not recorded at all")
	}
	return rec
}

// TestAConfigWriteIsPutBackByteForByte is the first acceptance criterion and
// the one the whole file kind exists for: what would restore a config write
// is the previous bytes, comments and spacing included, because that is what
// the byte-preserving editor deliberately kept and what a re-serialisation
// would silently lose.
func TestAConfigWriteIsPutBackByteForByte(t *testing.T) {
	store := newTestStore(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	// A comment and an odd blank line, because those are exactly what an undo
	// that re-serialised the parsed document would quietly drop.
	const original = "# my own note, which nothing may eat\n\n[tts]\nvoice  =  \"alba\"\n"
	writeFile(t, path, original)

	recordFileChange(t, store, path, "[tts]\nvoice = \"jenny\"\n", `changed the setting tts.voice`)

	out, err := NewUndoer(store, nil, nil).Last(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !out.Done {
		t.Fatalf("the undo did not happen: %s", out.Spoken)
	}
	if got := readFile(t, path); got != original {
		t.Errorf("the file was not restored byte for byte:\n got %q\nwant %q", got, original)
	}
	if !strings.Contains(out.Spoken, "config.toml") {
		t.Errorf("the sentence %q does not name what was put back", out.Spoken)
	}
}

// TestAnUndoIsItselfRecordedAndTheOriginalIsMarked pins the account's own
// bookkeeping: a reversal is a thing that happened, so it earns a row, and
// the row it reversed says so rather than disappearing.
func TestAnUndoIsItselfRecordedAndTheOriginalIsMarked(t *testing.T) {
	store := newTestStore(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "before\n")
	rec := recordFileChange(t, store, path, "after\n", "saved the routine \"morning\"")

	out, err := NewUndoer(store, nil, nil).Apply(context.Background(), rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Done {
		t.Fatalf("the undo did not happen: %s", out.Spoken)
	}
	original, err := store.Get(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if original.UndoneBy != out.Reversal.ID {
		t.Errorf("the reversed action says undone_by %q, want the reversal's own id %q",
			original.UndoneBy, out.Reversal.ID)
	}
	if original.Reversible() {
		t.Error("an action that has been put back still offers to be put back")
	}
	reversal, err := store.Get(out.Reversal.ID)
	if err != nil {
		t.Fatalf("the reversal earned no row of its own: %v", err)
	}
	if !strings.HasPrefix(reversal.Summary, "undid ") {
		t.Errorf("the reversal's row reads %q, which does not say it was a reversal", reversal.Summary)
	}
}

// TestCreatingAFileIsUndoneByRemovingIt is the artifact kind: "what would put
// this back" for something that did not exist is that it did not exist.
func TestCreatingAFileIsUndoneByRemovingIt(t *testing.T) {
	store := newTestStore(t)
	path := filepath.Join(t.TempDir(), "diagram.png")
	writeFile(t, path, "a rendered diagram")

	ctx := WithRecorder(context.Background(), store)
	rec := Note(ctx, Action{
		Tool: "artifact.create", Summary: `made the mermaid artifact "diagram.png"`,
		Restore: Restore{Kind: KindFile, File: &FileRestore{
			Path: path, Existed: false, AfterDigest: DigestOf(path)}},
	})

	out, err := NewUndoer(store, nil, nil).Apply(context.Background(), rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Done {
		t.Fatalf("the undo did not happen: %s", out.Spoken)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the created file is still there after the undo (%v)", err)
	}
}

// fakePlacer records what a window reversal asked the compositor for. Its
// fields are unexported and read through an accessor under the same mutex the
// write takes — the fake-field discipline (#149, internal/testdiscipline) —
// even though this one is only ever driven from the test goroutine.
type fakePlacer struct {
	asked []WindowState
	err   error
}

func (p *fakePlacer) Restore(_ context.Context, want WindowState) error {
	if p.err != nil {
		return p.err
	}
	p.asked = append(p.asked, want)
	return nil
}

func (p *fakePlacer) lastAsk() (WindowState, bool) {
	if len(p.asked) == 0 {
		return WindowState{}, false
	}
	return p.asked[len(p.asked)-1], true
}

// TestAWindowIsPutBackWhereItWas is the one reversal that is not a file: the
// compositor holds the state, so the record carries all of it and the placer
// is asked for exactly that.
func TestAWindowIsPutBackWhereItWas(t *testing.T) {
	store := newTestStore(t)
	placer := &fakePlacer{}
	ctx := WithRecorder(context.Background(), store)
	want := WindowState{
		Address: "0x1", StableID: "7", Class: "firefox", PID: 42,
		Describe: "Firefox — GitHub", Workspace: 2, Floating: true,
		X: 100, Y: 200, Width: 800, Height: 600,
	}
	rec := Note(ctx, Action{
		Tool: "desktop.move_window", Summary: "moved Firefox — GitHub to workspace 5",
		Restore: Restore{Kind: KindWindow, Window: &want},
	})

	out, err := NewUndoer(store, nil, placer).Apply(context.Background(), rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Done {
		t.Fatalf("the undo did not happen: %s", out.Spoken)
	}
	got, ok := placer.lastAsk()
	if !ok {
		t.Fatal("the placer was never asked to put the window back")
	}
	if got != want {
		t.Errorf("the placer was asked for %+v, want the state before the move %+v", got, want)
	}
	if !strings.Contains(out.Spoken, "Firefox — GitHub") {
		t.Errorf("the sentence %q does not name the window", out.Spoken)
	}
}

// TestAShellCommandIsRecordedAndNeverPromisedAsUndoable is the ticket's
// spine. The command is in the account, verbatim; asking to undo it says
// exactly why it cannot be; and something adjacent that CAN be undone is
// named rather than leaving the user at a dead end.
func TestAShellCommandIsRecordedAndNeverPromisedAsUndoable(t *testing.T) {
	store := newTestStore(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "before\n")
	recordFileChange(t, store, path, "after\n", `saved the routine "morning"`)

	ctx := WithRecorder(context.Background(), store)
	shell := Note(ctx, Action{
		Tool: "shell.run", Summary: "ran rm -rf ./build", Restore: OneWay("shell.run")})
	if shell.Reversible() {
		t.Fatal("a shell command was recorded as something Jarvix could put back")
	}

	// "Undo that" must not silently reach past the command to the config
	// write: it says what the last thing was, and that it cannot be undone.
	out, err := NewUndoer(store, nil, nil).Last(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !out.Done {
		t.Fatalf("the reversible action behind the command was not put back: %s", out.Spoken)
	}
	for _, want := range []string{
		"ran rm -rf ./build",
		"a command that has run has run",
	} {
		if !strings.Contains(out.Spoken, want) {
			t.Errorf("the answer %q does not say %q", out.Spoken, want)
		}
	}
	if got := readFile(t, path); got != "before\n" {
		t.Errorf("the config write behind the command was not put back: %q", got)
	}

	// Asked about the command directly, the refusal names the reason and
	// something adjacent that can be undone.
	direct, err := NewUndoer(store, nil, nil).Apply(context.Background(), shell.ID)
	if err != nil {
		t.Fatal(err)
	}
	if direct.Done || !direct.Refused {
		t.Fatalf("undoing a shell command reported %+v, want a refusal", direct)
	}
	if !strings.Contains(direct.Spoken, "a command that has run has run") {
		t.Errorf("the refusal %q does not say why", direct.Spoken)
	}
}

// TestAnUndoRefusesRatherThanClobberingNewerWork is the acceptance criterion
// with the sharpest edge. The file changed after Jarvix wrote it — the user
// in an editor, another machine, anything — and there is no way from here to
// know whether that change matters. So: no write, a reason, and the account
// unchanged so the offer still stands once the person has looked.
func TestAnUndoRefusesRatherThanClobberingNewerWork(t *testing.T) {
	store := newTestStore(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "before\n")
	rec := recordFileChange(t, store, path, "after\n", `saved the routine "morning"`)

	const newer = "after, and then the user's own edit\n"
	writeFile(t, path, newer)

	out, err := NewUndoer(store, nil, nil).Apply(context.Background(), rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if out.Done {
		t.Fatal("the undo overwrote work that arrived after the change")
	}
	if !out.Refused {
		t.Error("the outcome does not report itself as a refusal")
	}
	if got := readFile(t, path); got != newer {
		t.Errorf("the newer file was changed anyway: %q", got)
	}
	if !strings.Contains(out.Spoken, "has changed since") {
		t.Errorf("the refusal %q does not say what it found", out.Spoken)
	}
	// The offer still stands: refusing is not consuming.
	again, err := store.Get(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Reversible() {
		t.Error("a refused undo marked the action as no longer reversible")
	}
}

// TestAnUndoRefusesWhenItCannotTellWhatTheFileLooksLike covers the other
// half of "when it cannot tell": the file is gone. Jarvix does not know what
// removed it, so it does not put one back.
func TestAnUndoRefusesWhenItCannotTellWhatTheFileLooksLike(t *testing.T) {
	store := newTestStore(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	writeFile(t, path, "before\n")
	rec := recordFileChange(t, store, path, "after\n", `saved the routine "morning"`)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	out, err := NewUndoer(store, nil, nil).Apply(context.Background(), rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if out.Done || !out.Refused {
		t.Fatalf("undoing into a missing file reported %+v, want a refusal", out)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the undo recreated a file it could not account for")
	}
}

// denyingGate refuses one tool identity, the way a user's `[tools.policy]`
// does.
type denyingGate struct{ deny string }

func (g denyingGate) Judge(tool string) Decision {
	if tool == g.deny {
		return DecisionDeny
	}
	return DecisionAllow
}

// TestADangerousUndoGoesThroughTheGate is the criterion that an undo is not
// a back door. Putting a config write back IS a config write, so it faces
// the tier config writes face: a user who turned them off gets them off,
// including under another name.
func TestADangerousUndoGoesThroughTheGate(t *testing.T) {
	store := newTestStore(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	writeFile(t, path, "before\n")
	rec := recordFileChange(t, store, path, "after\n", `saved the routine "morning"`)

	gate := denyingGate{deny: "config.write_entry"}
	out, err := NewUndoer(store, gate, nil).Apply(context.Background(), rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if out.Done {
		t.Fatal("a denied tool identity was reversed anyway")
	}
	if !strings.Contains(out.Spoken, "turned off") {
		t.Errorf("the refusal %q does not say the gate refused it", out.Spoken)
	}
	if got := readFile(t, path); got != "after\n" {
		t.Errorf("the file was changed despite the gate: %q", got)
	}
}

// TestAJobIsReversedNewestStepFirst pins #200's contract. Nothing sets a job
// id yet, so the id is set by hand here — which is precisely the point: the
// day jobs land, the only new code is the code that fills the field in.
func TestAJobIsReversedNewestStepFirst(t *testing.T) {
	store := newTestStore(t)
	dir := t.TempDir()
	ctx := WithRecorder(context.Background(), store)

	var order []string
	for _, name := range []string{"one", "two", "three"} {
		path := filepath.Join(dir, name+".toml")
		writeFile(t, path, "before "+name+"\n")
		before := Snapshot(ctx, path)
		writeFile(t, path, "after "+name+"\n")
		before.Note(ctx, Action{
			Tool: "config.write_entry", Job: "deploy", Summary: "saved " + name})
		order = append(order, name)
	}
	// One irreversible step in the middle of the job, so the report has to
	// have both halves to be true.
	Note(ctx, Action{Tool: "shell.run", Job: "deploy",
		Summary: "ran ./deploy.sh", Restore: OneWay("shell.run")})

	out, err := NewUndoer(store, nil, nil).JobActions(context.Background(), "deploy")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Outcomes) != 4 {
		t.Fatalf("the job reversal tried %d actions, want all 4", len(out.Outcomes))
	}
	// Newest step first: the shell command, then three, two, one.
	if !out.Outcomes[0].Refused {
		t.Error("the newest step was not attempted first — the shell command should have refused")
	}
	for i, name := range []string{"three", "two", "one"} {
		got := out.Outcomes[i+1]
		if !got.Done {
			t.Errorf("step %q was not put back: %s", name, got.Spoken)
		}
		if !strings.Contains(got.Record.Summary, name) {
			t.Errorf("outcome %d is %q, want the step that saved %q", i+1, got.Record.Summary, name)
		}
	}
	for _, want := range []string{"I undid", "I couldn't undo", "ran ./deploy.sh"} {
		if !strings.Contains(out.Spoken, want) {
			t.Errorf("the report %q does not say %q", out.Spoken, want)
		}
	}
	for _, name := range order {
		if got := readFile(t, filepath.Join(dir, name+".toml")); got != "before "+name+"\n" {
			t.Errorf("%s was not restored: %q", name, got)
		}
	}
}

// TestNothingIsRecordedWithoutARecorder is the seam's own promise: a tool
// exercised outside a turn — a unit test, a CLI invocation — behaves exactly
// as it did before this feature existed, and reads no files it did not have
// to read.
func TestNothingIsRecordedWithoutARecorder(t *testing.T) {
	ctx := context.Background()
	before := Snapshot(ctx, filepath.Join(t.TempDir(), "config.toml"))
	if rec := before.Note(ctx, Action{Tool: "config.write_entry", Summary: "saved it"}); rec.ID != "" {
		t.Errorf("a call with no recorder installed produced the record %+v", rec)
	}
	if rec := Note(ctx, Action{Tool: "shell.run", Summary: "ran ls"}); rec.ID != "" {
		t.Errorf("a bare Note with no recorder produced the record %+v", rec)
	}
}
