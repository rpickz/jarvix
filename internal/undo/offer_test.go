package undo

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// Offer is the eligibility question a surface has to ask before it draws a
// control (#210), and the only property worth testing about it is that it
// answers the same way Apply does. A listing built on a second, private idea of
// what can be reversed would eventually offer a control that refused when
// pressed — "the button did nothing", the shrug ADR 0064 exists to replace.
//
// So each case below asks BOTH: what the account would offer, and what actually
// happens when the reversal is attempted. Neither answer is derived from the
// other; the claim is that two independent observations agree.

// TestOfferAndApplyAgreeOnEveryRecordTheAccountCanHold walks the three states
// a record can be in when a surface asks about it.
func TestOfferAndApplyAgreeOnEveryRecordTheAccountCanHold(t *testing.T) {
	t.Run("a reversible record is offered and is reversed", func(t *testing.T) {
		store := newTestStore(t)
		path := filepath.Join(t.TempDir(), "config.toml")
		writeFile(t, path, "before\n")
		rec := recordFileChange(t, store, path, "after\n", `saved the routine "morning"`)
		undoer := NewUndoer(store, nil, nil)

		offered, why := undoer.Offer(rec)
		if !offered {
			t.Fatalf("a reversible record was not offered: %q", why)
		}
		if why != "" {
			t.Errorf("an offered record carries a reason not to: %q", why)
		}

		out, err := undoer.Apply(context.Background(), rec.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !out.Done {
			t.Fatalf("the offer stood and the reversal did not happen: %q", out.Spoken)
		}
		if got := readFile(t, path); got != "before\n" {
			t.Errorf("the file was not put back: %q", got)
		}
	})

	t.Run("an irreversible record is withheld with its own reason", func(t *testing.T) {
		store := newTestStore(t)
		ctx := WithRecorder(context.Background(), store)
		rec := Note(ctx, Action{Tool: "shell.run", Summary: "ran rm -rf ./build",
			Restore: OneWay("shell.run")})
		undoer := NewUndoer(store, nil, nil)

		offered, why := undoer.Offer(rec)
		if offered {
			t.Fatal("a shell command that has run was offered as reversible")
		}
		if why != rec.Why() {
			t.Errorf("the withheld reason %q is not the record's own %q", why, rec.Why())
		}

		out, err := undoer.Apply(context.Background(), rec.ID)
		if err != nil {
			t.Fatal(err)
		}
		if out.Done || !out.Refused {
			t.Fatalf("the offer was withheld and the reversal ran anyway: %+v", out)
		}
		if !strings.Contains(out.Spoken, why) {
			t.Errorf("the refusal %q does not carry the reason the listing showed: %q",
				out.Spoken, why)
		}
	})

	// The gate is the case a client could not have worked out for itself: the
	// record is perfectly reversible and the policy still says no. A listing
	// that read `reversible` as the offer would draw a button here.
	t.Run("a reversal the gate denies is withheld before it is pressed", func(t *testing.T) {
		store := newTestStore(t)
		path := filepath.Join(t.TempDir(), "config.toml")
		writeFile(t, path, "before\n")
		rec := recordFileChange(t, store, path, "after\n", `saved the routine "morning"`)
		undoer := NewUndoer(store, denyingGate{deny: "config.write_entry"}, nil)

		if !rec.Reversible() {
			t.Fatal("the fixture no longer tests what it claims to")
		}
		offered, why := undoer.Offer(rec)
		if offered {
			t.Fatal("a reversal the policy denies was offered as a control")
		}
		if !strings.Contains(why, "turned off") {
			t.Errorf("the withheld reason %q does not say the policy is what is in the way", why)
		}

		out, err := undoer.Apply(context.Background(), rec.ID)
		if err != nil {
			t.Fatal(err)
		}
		if out.Done {
			t.Fatal("a denied tool identity was reversed anyway")
		}
		// One clause, one source: the sentence the listing showed is the
		// sentence the refusal carries, so the two cannot describe the same
		// standing instruction differently.
		if !strings.Contains(out.Spoken, why) {
			t.Errorf("the refusal %q does not carry the listing's clause %q", out.Spoken, why)
		}
		if got := readFile(t, path); got != "after\n" {
			t.Errorf("the file was changed despite the gate: %q", got)
		}
	})

	t.Run("a record already put back is withheld and says so", func(t *testing.T) {
		store := newTestStore(t)
		path := filepath.Join(t.TempDir(), "config.toml")
		writeFile(t, path, "before\n")
		rec := recordFileChange(t, store, path, "after\n", `saved the routine "morning"`)
		undoer := NewUndoer(store, nil, nil)
		if _, err := undoer.Apply(context.Background(), rec.ID); err != nil {
			t.Fatal(err)
		}

		again, err := store.Get(rec.ID)
		if err != nil {
			t.Fatal(err)
		}
		offered, why := undoer.Offer(again)
		if offered {
			t.Fatal("a record that has already been put back was offered again")
		}
		if !strings.Contains(why, "already put that back") {
			t.Errorf("the withheld reason %q does not say it has already been done", why)
		}

		// And the press agrees, which is this file's whole claim and is worth
		// asking of THIS state in particular (#219): the row's standing here is
		// written by the reversal itself, so it is the one of the three that a
		// change to how a reversal is recorded could quietly break. A second
		// window that had drawn its control a moment before the mark landed
		// would arrive exactly here.
		out, err := undoer.Apply(context.Background(), again.ID)
		if err != nil {
			t.Fatal(err)
		}
		if out.Done {
			t.Fatal("a row that had already been put back was reversed a second time")
		}
		if !strings.Contains(out.Spoken, why) {
			t.Errorf("the refusal %q does not carry the reason the listing showed: %q",
				out.Spoken, why)
		}
		if got := readFile(t, path); got != "before\n" {
			t.Errorf("the second press changed the file: %q", got)
		}
	})
}

// A nil Undoer — the construction a daemon with no reverser makes — answers
// from the record alone rather than panicking. Everything else in this package
// degrades that way and a listing must not be the exception.
func TestOfferOnANilUndoerAnswersFromTheRecord(t *testing.T) {
	var undoer *Undoer
	offered, why := undoer.Offer(Record{ID: "a1",
		Action: Action{Tool: "shell.run", Summary: "ran a command",
			Restore: OneWay("shell.run")}})
	if offered {
		t.Error("an irreversible record was offered by a nil reverser")
	}
	if why == "" {
		t.Error("a withheld record carries no reason")
	}
}
