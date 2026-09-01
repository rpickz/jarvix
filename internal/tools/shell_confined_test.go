package tools

// The shell tool inside a job's boundary (#222, ADR 0068).
//
// internal/confine has the tests that prove the kernel actually refuses an
// escape; these prove the wiring — that a job's command reaches that boundary
// and that a job's command WITHOUT one does not reach a shell at all. The
// second is the more important of the two, because it is the property that
// makes the arrangement fail closed: forgetting to install a boundary must
// produce a command that does not run, never a command that runs unheld.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/confine"
	"github.com/rpickz/jarvix/internal/undo"
)

// TestMain makes this test binary its own confinement helper. See
// confine.Reexec: a confined command is this program, re-executed and then
// exec'd into the command, so under `go test` the program is the test binary.
func TestMain(m *testing.M) {
	confine.Reexec()
	os.Exit(m.Run())
}

// confinableOrSkip refuses to let a green run mean nothing.
func confinableOrSkip(t *testing.T) {
	t.Helper()
	if s := confine.Available(); !s.OK {
		t.Skipf("THE BOUNDARY WAS NOT EXERCISED and this test proved nothing: %s "+
			"(kernel reported Landlock ABI %d)", s.Because, s.ABI)
	}
}

// jobRoot builds a directory a job may write in and one it may not.
func jobRoot(t *testing.T) (root, outside string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, outside = filepath.Join(base, "in"), filepath.Join(base, "out")
	for _, dir := range []string{root, outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root, outside
}

// commandThrough runs one command through the real shell tool on the given
// context, which is where the boundary and the job id both live.
func commandThrough(t *testing.T, ctx context.Context, command string) (string, error) {
	t.Helper()
	input, _ := json.Marshal(map[string]string{"command": command})
	return (&Shell{}).Execute(ctx, input)
}

// TestAJobsCommandWithNoBoundaryIsRefused is the fail-closed rule, and the one
// test that would catch a future change quietly dropping the confinement.
//
// The context carries a job id — installed by the runner for the account — and
// no boundary. The tool does not fall back to running it the way a session's
// command runs: it refuses, because a command it cannot hold inside a scope is
// a command with no scope, and the job that proposed it was given one.
//
// The assertion is the FILE. A refusal that returned an error while still
// having run the command would satisfy any check on the error alone.
func TestAJobsCommandWithNoBoundaryIsRefused(t *testing.T) {
	root, _ := jobRoot(t)
	landed := filepath.Join(root, "it-ran.txt")
	ctx := undo.WithJob(context.Background(), "j7")
	_, err := commandThrough(t, ctx, "echo ran > "+landed)
	if err == nil {
		t.Fatal("a job's command ran with no boundary around it")
	}
	if !strings.Contains(err.Error(), "boundary") {
		t.Errorf("refusal = %q, want it to say what was missing", err.Error())
	}
	if _, statErr := os.Stat(landed); statErr == nil {
		t.Fatalf("the command ran anyway — it wrote %s", landed)
	}
}

// TestASessionsCommandIsUnchangedByTheBoundary. shell.run outside a job is the
// user's own authority, exercised while they are present and asked about at the
// gate. Nothing in #222 narrows it, and a change that started confining a
// session's commands would be a change to what the tool IS — so the absence of
// a boundary on an ordinary call is pinned rather than assumed.
func TestASessionsCommandIsUnchangedByTheBoundary(t *testing.T) {
	out, err := commandThrough(t, context.Background(), "echo plain")
	if err != nil {
		t.Fatalf("a session's own command was refused: %v", err)
	}
	if !strings.Contains(out, "plain") {
		t.Errorf("output = %q, want the command's own output", out)
	}
}

// TestAJobsCommandRunsInsideItsBoundary: the boundary permits the work. A wall
// that refused everything would pass every escape test in this package and be
// useless.
func TestAJobsCommandRunsInsideItsBoundary(t *testing.T) {
	confinableOrSkip(t)
	root, _ := jobRoot(t)
	made := filepath.Join(root, "made.txt")
	ctx := confine.With(undo.WithJob(context.Background(), "j7"), confine.Spec{Roots: []string{root}})
	if _, err := commandThrough(t, ctx, "echo hello > "+made); err != nil {
		t.Fatalf("a write inside the job's root was refused: %v", err)
	}
	got, err := os.ReadFile(made)
	if err != nil || strings.TrimSpace(string(got)) != "hello" {
		t.Errorf("file inside the root = %q, %v; want the command's own write", got, err)
	}
}

// TestAJobsCommandCannotReachOutsideItsBoundary, through the real tool rather
// than through internal/confine's own runner — so the wiring is proved and not
// only the mechanism.
func TestAJobsCommandCannotReachOutsideItsBoundary(t *testing.T) {
	confinableOrSkip(t)
	root, outside := jobRoot(t)
	victim := filepath.Join(outside, "victim.txt")
	if err := os.WriteFile(victim, []byte("ORIGINAL"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := confine.With(undo.WithJob(context.Background(), "j7"), confine.Spec{Roots: []string{root}})
	if _, err := commandThrough(t, ctx, "echo PWNED > "+victim); err != nil {
		t.Fatalf("the tool reported the command unrunnable rather than letting it fail: %v", err)
	}
	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("the file outside the boundary is gone: %v", err)
	}
	if string(got) != "ORIGINAL" {
		t.Errorf("the file outside the boundary now reads %q — a job's command reached "+
			"through the wall", got)
	}
}

// TestAConfinedCommandStillWritesItsAccountRow. Every command a job runs has to
// appear in the account with its provenance (#210), and it is recorded as
// one-way for the reason it always was: Jarvix has no idea what the command did
// and an offer to put it back would be one it could not keep.
func TestAConfinedCommandStillWritesItsAccountRow(t *testing.T) {
	confinableOrSkip(t)
	root, _ := jobRoot(t)
	base, store := recordingCtx(t)
	ctx := confine.With(undo.WithJob(base, "j7"), confine.Spec{Roots: []string{root}})
	if _, err := commandThrough(t, ctx, "echo hello"); err != nil {
		t.Fatal(err)
	}
	record := onlyRecord(t, store)
	switch {
	case record.Job != "j7":
		t.Errorf("record = %+v, want it filed under the job that ran it", record)
	case !strings.Contains(record.Summary, "echo hello"):
		t.Errorf("summary = %q, want the command verbatim", record.Summary)
	case record.Reversible():
		t.Errorf("record = %+v, a command is never offered as undoable", record)
	}
}

// TestACommandThatWasNeverRunLeavesNoAccountRow.
//
// This is #71's rule pointed at this package's own bookkeeping. The account is
// what a job's report is built from, so a row saying "ran ..." for a command
// that was refused before it started would put a false sentence into the
// report by construction — the report would be reading the ledger honestly and
// the ledger would be wrong.
func TestACommandThatWasNeverRunLeavesNoAccountRow(t *testing.T) {
	base, store := recordingCtx(t)
	ctx := undo.WithJob(base, "j7") // a job, and deliberately no boundary
	if _, err := commandThrough(t, ctx, "echo hello"); err == nil {
		t.Fatal("the command was not refused")
	}
	if view := store.List(); len(view.Records) != 0 {
		t.Errorf("the account holds %d rows for a command that never ran, want 0",
			len(view.Records))
	}
}
