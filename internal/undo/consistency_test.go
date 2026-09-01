package undo

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
)

// #219. `undo.changed` is a "look again", and everything it invites a reader
// to look at has to be true by the time it fires.
//
// It was not. A reversal used to append its own row — publishing the event on
// that write — and only then mark the row it reversed, so a subscriber that
// re-read on the event could see the reversal listed beside a row that still
// reported itself reversible and still offered a control. The window that
// pressed the button never saw it, because it also re-reads from its own apply
// reply, which arrives after the mark. A second window did.
//
// The tests below are about the account being consistent AT the announcement
// and AT rest, which are the same claim once the two writes are one.

// accountSeen is what one subscriber found when it looked, having been told
// nothing except that something changed.
type accountSeen struct {
	who string
	// reversalListed is whether the reversal's own row was in the account.
	reversalListed bool
	// stillStands is whether the row that was reversed still reported itself
	// reversible — the half of the torn state that made the account wrong.
	stillStands bool
	// offered is whether a listing built on Offer would still have drawn an
	// Undo control for it, which is the same wrongness as the user sees it.
	offered bool
	why     string
}

// lookAtTheAccount is one subscriber's whole reaction to the event: read the
// account, and report what it says about the row and about the reversal.
func lookAtTheAccount(who string, store *Store, undoer *Undoer, reversed string) accountSeen {
	seen := accountSeen{who: who}
	for _, r := range store.List().Records {
		if r.ID == reversed {
			seen.stillStands = r.Reversible()
			seen.offered, seen.why = undoer.Offer(r)
			continue
		}
		if strings.HasPrefix(r.Summary, "undid ") {
			seen.reversalListed = true
		}
	}
	return seen
}

// TestTheAccountIsConsistentTheMomentItAnnouncesItself drives TWO
// subscriptions over one reversal, which is the ticket's own instruction:
// asserting on one and inferring the other would be assuming exactly the
// property under test.
//
// Both look from inside the publish, which is the earliest any subscriber can
// possibly look and therefore the tightest probe there is — a real bus hands
// the event to a socket and the reader gets there later, so a store that
// satisfies this satisfies every real client. And they look through two
// different readers on purpose: one through the store that did the work, one
// through a freshly-opened account over the same file, which is what a
// `jarvix undo list` in another process actually is. The second is also the
// restart case, asked at the worst possible instant rather than at a
// convenient one.
func TestTheAccountIsConsistentTheMomentItAnnouncesItself(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "undo.toml")
	target := filepath.Join(dir, "config.toml")
	writeFile(t, target, "before\n")

	var (
		seen     []accountSeen
		store    *Store
		undoer   *Undoer
		reversed string
	)
	// Nothing here is concurrent and nothing sleeps: Store.emit calls Publish
	// on the caller's own goroutine, after the account is settled and with no
	// lock held, so this closure runs between the write and Apply's return.
	publish := func(event string, _ map[string]any) {
		if event != "undo.changed" || reversed == "" {
			return // the recording's own event; the reversal is what is under test
		}
		seen = append(seen,
			lookAtTheAccount("the window that did not press it", store, undoer, reversed))
		fresh := NewStore(path, StoreOptions{Now: fixedClock()}, quietLog())
		seen = append(seen,
			lookAtTheAccount("a jarvix undo list in another process",
				fresh, NewUndoer(fresh, nil, nil), reversed))
	}
	store = NewStore(path, StoreOptions{Now: fixedClock(), Publish: publish}, quietLog())
	undoer = NewUndoer(store, nil, nil)

	rec := recordFileChange(t, store, target, "after\n", `saved the routine "morning"`)
	reversed = rec.ID

	out, err := undoer.Apply(context.Background(), rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Done {
		t.Fatalf("the reversal did not happen, so nothing was announced: %q", out.Spoken)
	}
	if len(seen) != 2 {
		t.Fatalf("%d subscribers looked, want the 2 the event was published to", len(seen))
	}
	for _, s := range seen {
		// The event's own row is there. The fix orders nothing after the
		// announcement — it puts both halves before it — so an account that
		// announced a reversal it had not yet written would be just as wrong
		// in the other direction.
		if !s.reversalListed {
			t.Errorf("%s: the reversal that caused the event is not in the account it was "+
				"told to re-read", s.who)
		}
		if s.stillStands {
			t.Errorf("%s: the row that was just put back still reports itself reversible",
				s.who)
		}
		if s.offered {
			t.Errorf("%s: the account still offers an Undo control for a row it has "+
				"already put back", s.who)
		}
		if !strings.Contains(s.why, "already put that back") {
			t.Errorf("%s: the withheld reason is %q, want it to say the row is already back",
				s.who, s.why)
		}
	}
}

// TestAReversalSurvivesARestartWholeOrNotAtAll is the interruption criterion.
//
// The honest answer to "what does a daemon that dies between the two writes
// leave on disk?" is that there is no between: the reversal's row and the mark
// on the row it reversed are one `persisted` value through one
// temp-write-and-rename, so the file is either the account before the reversal
// or the account after it. What this test can prove hermetically is the half
// that is checkable — that the state reached is the whole one, read back off
// disk by a store that was not there when it was written.
func TestAReversalSurvivesARestartWholeOrNotAtAll(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "undo.toml")
	target := filepath.Join(dir, "config.toml")
	writeFile(t, target, "before\n")

	store := NewStore(path, StoreOptions{Now: fixedClock()}, quietLog())
	rec := recordFileChange(t, store, target, "after\n", `saved the routine "morning"`)

	// Counted at the disk seam, because "one write" is the whole argument and
	// an argument that only lives in a comment is one somebody will
	// helpfully re-split into two ordered ones.
	writes := 0
	store.write = func(path string, p persisted) error {
		writes++
		return writeStore(path, p)
	}
	out, err := NewUndoer(store, nil, nil).Apply(context.Background(), rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Done {
		t.Fatalf("the reversal did not happen: %q", out.Spoken)
	}
	if writes != 1 {
		t.Errorf("the reversal touched the file %d times, want 1 — anything more is a "+
			"state a crash or a subscriber can land in between", writes)
	}

	restarted := NewStore(path, StoreOptions{Now: fixedClock()}, quietLog())
	back, err := restarted.Get(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if back.UndoneBy == "" {
		t.Error("after a restart the reversed row carries no mark, so the account would " +
			"offer to undo something it has already undone")
	}
	if back.UndoneBy != out.Reversal.ID {
		t.Errorf("the mark names %q, want the reversal's own row %q", back.UndoneBy, out.Reversal.ID)
	}
	if back.UndoneAt.IsZero() {
		t.Error("the mark carries no time, so the account cannot say when it was put back")
	}
	if _, err := restarted.Get(out.Reversal.ID); err != nil {
		t.Errorf("the reversal's own row did not survive the restart: %v", err)
	}
}

// TestAReversalTheAccountCouldNotWriteIsReportedRatherThanDenied is the
// ledger discipline applied to this store's one remaining failure.
//
// The file HAS been put back by the time the account is written, and a write
// that fails cannot un-put it. So the outcome says the reversal happened and
// says the account of it did not — the jobs runner's rule, that a step whose
// outcome could not be confirmed is reported honestly rather than as done or
// as never-started (internal/jobs, ADR 0065), applied to a step whose outcome
// was certain and whose record was not. Returning an error instead would
// describe a reversal that plainly did happen as one that did not.
//
// The other half is that nothing partial reaches memory or disk: the mark and
// the reversal row go through one save, so a save that fails costs exactly
// nothing — the write-failure contract internal/storefault holds every store
// in this repository to, on the one write path that suite cannot reach through
// its shared Add/Forget interface.
func TestAReversalTheAccountCouldNotWriteIsReportedRatherThanDenied(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "undo.toml")
	target := filepath.Join(dir, "config.toml")
	writeFile(t, target, "before\n")

	store := NewStore(path, StoreOptions{Now: fixedClock()}, quietLog())
	rec := recordFileChange(t, store, target, "after\n", `saved the routine "morning"`)

	var announced int
	store.publish = func(string, map[string]any) { announced++ }
	full := errors.New("no space left on device")
	store.write = func(string, persisted) error { return full }

	out, err := NewUndoer(store, nil, nil).Apply(context.Background(), rec.ID)
	if err != nil {
		t.Fatalf("a reversal whose account could not be written came back as an error, "+
			"which reads as a reversal that never started: %v", err)
	}
	if !out.Done {
		t.Fatalf("the file was put back and the outcome says it was not: %+v", out)
	}
	if got := readFile(t, target); got != "before\n" {
		t.Errorf("the file reads %q, want the reversal that was reported to have happened", got)
	}
	if !strings.Contains(out.Spoken, "couldn't write that down") {
		t.Errorf("spoken = %q, does not disclose that the account was not updated", out.Spoken)
	}
	if announced != 0 {
		t.Errorf("%d events announced a change that was never written", announced)
	}

	// Nothing half-landed. Not a reversal row without a mark, and not a mark
	// without a reversal row — in memory or on the file a restart would read.
	for _, s := range []struct {
		who   string
		store *Store
	}{
		{"the running store", store},
		{"a restart", NewStore(path, StoreOptions{Now: fixedClock()}, quietLog())},
	} {
		view := s.store.List()
		if len(view.Records) != 1 {
			t.Fatalf("%s holds %d rows after a failed write, want only the original action",
				s.who, len(view.Records))
		}
		if got := view.Records[0]; got.ID != rec.ID || got.UndoneBy != "" {
			t.Errorf("%s holds %+v, want the original row untouched and unmarked", s.who, got)
		}
	}
}

// quietLog keeps a deliberately-failed write's warning out of the test output.
// The warning itself is the store's business and the fault suite's; a passing
// test that prints a stack of them trains the reader to skim the log.
func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
