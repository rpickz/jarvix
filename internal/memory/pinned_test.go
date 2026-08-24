package memory

import (
	"strings"
	"testing"
	"time"
)

// Pinning (issue #104) is presentation-of-memory state riding the book's own
// discipline: these tests pin that a pin round-trips the TOML file, survives
// hand-edits, and never disturbs the parts of a fact that mean something
// else — timestamps, the supersede trail, the injection order.

func TestSetPinnedPersistsAcrossReopen(t *testing.T) {
	b, _, path := newTestBook(t, BookOptions{})
	f := b.mustAdd(t, "the staging server is called atlas")
	if _, err := b.SetPinned(f.ID, true); err != nil {
		t.Fatal(err)
	}
	facts := NewBook(path, BookOptions{}, nil).List("")
	if len(facts) != 1 || !facts[0].Pinned {
		t.Fatalf("after reopen facts = %+v, want the fact pinned", facts)
	}
	// And back: unpinning is the exact inverse.
	if _, err := b.SetPinned(f.ID, false); err != nil {
		t.Fatal(err)
	}
	if facts := NewBook(path, BookOptions{}, nil).List(""); facts[0].Pinned {
		t.Error("fact still pinned after unpin")
	}
}

// TestSetPinnedTouchesNothingElse: a pin is not a content change — Updated
// (the trim priority), Stored, and the trail must be exactly what they were,
// or pinning would silently reorder the injection.
func TestSetPinnedTouchesNothingElse(t *testing.T) {
	b, clock, _ := newTestBook(t, BookOptions{})
	f := b.mustAdd(t, "the staging server is called atlas")
	clock.advance(time.Hour)
	if _, err := b.Update(f.ID, "the staging server is called helios", ""); err != nil {
		t.Fatal(err)
	}
	before := b.List("")[0]
	clock.advance(time.Hour)

	pinned, err := b.SetPinned(f.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if !pinned.Updated.Equal(before.Updated) || !pinned.Stored.Equal(before.Stored) {
		t.Errorf("pin moved timestamps: %v/%v, want %v/%v",
			pinned.Stored, pinned.Updated, before.Stored, before.Updated)
	}
	if len(pinned.Previous) != 1 || pinned.Previous[0].Content != "the staging server is called atlas" {
		t.Errorf("pin disturbed the trail: %+v", pinned.Previous)
	}
	if pinned.Content != before.Content {
		t.Errorf("pin changed content to %q", pinned.Content)
	}
}

func TestSetPinnedUnknownID(t *testing.T) {
	b, _, _ := newTestBook(t, BookOptions{})
	if _, err := b.SetPinned("m9", true); err == nil {
		t.Fatal("pinning an unknown id succeeded")
	}
}

// TestPinnedFileStaysCleanForHandEdits pins the omitempty contract: an
// unpinned, never-retrieved fact writes no pinned / times_retrieved /
// last_retrieved lines at all — the file a pre-#104 user knows is the file
// they keep — while a pinned fact writes exactly `pinned = true`, which a
// hand-edit can flip or add.
func TestPinnedFileStaysCleanForHandEdits(t *testing.T) {
	b, _, path := newTestBook(t, BookOptions{})
	b.mustAdd(t, "the staging server is called atlas")
	data := mustRead(t, path)
	// The header's documentation mentions the keys, so scan the document
	// body only — everything from `version =` down.
	body := data[strings.Index(data, "version ="):]
	for _, absent := range []string{"pinned", "times_retrieved", "last_retrieved"} {
		if strings.Contains(body, absent+" = ") {
			t.Errorf("untouched fact wrote a %q line:\n%s", absent, body)
		}
	}

	// The user pins by hand; the very next operation sees it (no restart).
	writeHandEdit(t, path, strings.Replace(data,
		"content = ", "pinned = true\ncontent = ", 1))
	facts := b.List("")
	if len(facts) != 1 || !facts[0].Pinned {
		t.Fatalf("hand-edited pin not picked up: %+v", facts)
	}
}

// TestHandEditedStatsAreRepairedNotFabricated: normalize's stats repair —
// nonsense counts go to never-retrieved, a bare last_retrieved implies the
// one retrieval that stamped it, and an untouched fact stays untouched.
func TestHandEditedStatsAreRepairedNotFabricated(t *testing.T) {
	b, _, path := newTestBook(t, BookOptions{})
	b.mustAdd(t, "the staging server is called atlas")
	writeHandEdit(t, path, mustRead(t, path)+`
[[fact]]
content = "the user's editor is neovim"
times_retrieved = -3

[[fact]]
content = "the user's terminal is Ghostty"
last_retrieved = 2026-08-01T09:00:00Z
`)
	byContent := map[string]Fact{}
	for _, f := range b.List("") {
		byContent[strings.Fields(f.Content)[2]] = f
	}
	if f := byContent["server"]; f.TimesRetrieved != 0 || !f.LastRetrieved.IsZero() {
		t.Errorf("untouched fact grew stats: %+v", f)
	}
	if f := byContent["editor"]; f.TimesRetrieved != 0 {
		t.Errorf("negative count repaired to %d, want 0", f.TimesRetrieved)
	}
	if f := byContent["terminal"]; f.TimesRetrieved != 1 || f.LastRetrieved.IsZero() {
		t.Errorf("bare last_retrieved repaired to %+v, want one retrieval kept", f)
	}
}
