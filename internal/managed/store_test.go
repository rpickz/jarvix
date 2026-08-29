package managed

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/desktop"
)

// The managed-window store's tests. Everything here is hermetic: a temp
// directory, an injected clock, and window literals — no compositor, no
// daemon, no sleeping.

func newTestStore(t *testing.T, now func() time.Time) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "managed.toml")
	return NewStore(path, StoreOptions{Now: now}, quietLogger(t)), path
}

func win(address, class string, pid int) desktop.Window {
	return desktop.Window{Address: address, Class: class, PID: pid,
		StableID: "id-" + address, Title: class + " window", AcceptsInput: true}
}

func TestAcquireMakesAWindowManaged(t *testing.T) {
	store, _ := newTestStore(t, nil)
	term := win("0xaa", "ghostty", 4242)
	inventory := []desktop.Window{term, win("0xbb", "firefox", 7)}

	rec, fresh, err := store.Acquire(term, inventory)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if !fresh {
		t.Fatal("the first acquisition should be fresh")
	}
	if rec.Source != SourceAcquired {
		t.Fatalf("source = %q, want %q", rec.Source, SourceAcquired)
	}
	if _, ok := store.Managed(term, inventory); !ok {
		t.Fatal("the acquired window should be managed")
	}
	if _, ok := store.Managed(inventory[1], inventory); ok {
		t.Fatal("an untouched window must never become managed")
	}
}

func TestAcquireTwiceWritesNothingNewAndIsNotAnError(t *testing.T) {
	store, _ := newTestStore(t, nil)
	term := win("0xaa", "ghostty", 4242)
	inventory := []desktop.Window{term}
	if _, _, err := store.Acquire(term, inventory); err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	writes := 0
	store.write = func(path string, r []Record, c []Claim) error { writes++; return writeStore(path, r, c) }

	rec, fresh, err := store.Acquire(term, inventory)
	if err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	if fresh {
		t.Fatal("re-acquiring an already managed window is not a fresh acquisition")
	}
	if rec.Address != term.Address {
		t.Fatalf("the existing record should come back, got %+v", rec)
	}
	if writes != 0 {
		t.Fatalf("re-acquiring wrote %d times; it should write nothing", writes)
	}
}

func TestReleaseGivesTheWindowBackImmediately(t *testing.T) {
	store, _ := newTestStore(t, nil)
	term := win("0xaa", "ghostty", 4242)
	inventory := []desktop.Window{term}
	if _, _, err := store.Acquire(term, inventory); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	rec, held, err := store.Release(term, inventory)
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if !held {
		t.Fatal("releasing a managed window should report that it was held")
	}
	if rec.Source != SourceAcquired {
		t.Fatalf("the released record should be the one acquired, got %+v", rec)
	}
	if _, ok := store.Managed(term, inventory); ok {
		t.Fatal("a released window must not still be managed")
	}
}

func TestReleasingAnUnmanagedWindowSaysSoRatherThanSucceeding(t *testing.T) {
	store, _ := newTestStore(t, nil)
	term := win("0xaa", "ghostty", 4242)
	_, held, err := store.Release(term, []desktop.Window{term})
	if err != nil {
		t.Fatalf("Release: %v", err)
	}
	if held {
		t.Fatal("releasing a window that was never managed must not report a release")
	}
}

func TestManagementSurvivesADaemonRestart(t *testing.T) {
	store, path := newTestStore(t, nil)
	term := win("0xaa", "ghostty", 4242)
	inventory := []desktop.Window{term}
	if _, _, err := store.Acquire(term, inventory); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// A restart is a second store over the same file and nothing else: the
	// window outlived the daemon, so its management must have too.
	restarted := NewStore(path, StoreOptions{}, quietLogger(t))
	if _, ok := restarted.Managed(term, inventory); !ok {
		t.Fatal("management should survive a restart")
	}
}

func TestManagementEndsWhenTheWindowCloses(t *testing.T) {
	store, path := newTestStore(t, nil)
	term := win("0xaa", "ghostty", 4242)
	if _, _, err := store.Acquire(term, []desktop.Window{term}); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// The window is gone from the inventory: nothing may be left claiming it.
	if got := store.List(nil); len(got) != 0 {
		t.Fatalf("a closed window is still listed: %+v", got)
	}
	// And the record is gone from the FILE, not merely from the answer — a
	// restart must not resurrect it.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	if strings.Contains(string(data), "0xaa") {
		t.Fatalf("the closed window is still on disk:\n%s", data)
	}
}

func TestARecycledAddressDoesNotInheritManagement(t *testing.T) {
	store, _ := newTestStore(t, nil)
	term := win("0xaa", "ghostty", 4242)
	if _, _, err := store.Acquire(term, []desktop.Window{term}); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// The compositor hands the same pointer to a different window: same
	// address, different process and different stable id. Matching on the
	// address alone would hand a stranger's window the user's grant.
	stranger := desktop.Window{Address: "0xaa", Class: "ghostty", PID: 9999, StableID: "id-new"}
	if _, ok := store.Managed(stranger, []desktop.Window{stranger}); ok {
		t.Fatal("a recycled address must not inherit management")
	}
}

func TestADifferentClassOnTheSameAddressIsADifferentWindow(t *testing.T) {
	store, _ := newTestStore(t, nil)
	term := win("0xaa", "ghostty", 4242)
	if _, _, err := store.Acquire(term, []desktop.Window{term}); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	other := term
	other.Class = "firefox"
	if _, ok := store.Managed(other, []desktop.Window{other}); ok {
		t.Fatal("the class is part of the identity; a mismatch is a different window")
	}
}

func TestALaunchedWindowIsManagedFromBirth(t *testing.T) {
	clock := time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC)
	store, _ := newTestStore(t, func() time.Time { return clock })
	if err := store.ClaimLaunch("dev.jarvix.claude", "claude"); err != nil {
		t.Fatalf("ClaimLaunch: %v", err)
	}

	// Nothing is managed until the window exists — a claim is a promise, not
	// a record.
	if got := store.List(nil); len(got) != 0 {
		t.Fatalf("a claim alone manages something: %+v", got)
	}

	opened := win("0xcc", "dev.jarvix.claude", 555)
	rec, ok := store.Managed(opened, []desktop.Window{opened})
	if !ok {
		t.Fatal("a window Jarvix launched should be managed the moment it appears")
	}
	if rec.Source != SourceLaunched {
		t.Fatalf("source = %q, want %q", rec.Source, SourceLaunched)
	}
	if rec.Program != "claude" {
		t.Fatalf("program = %q, want %q", rec.Program, "claude")
	}
}

func TestALaunchClaimNothingMatchesExpires(t *testing.T) {
	clock := time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC)
	store, _ := newTestStore(t, func() time.Time { return clock })
	if err := store.ClaimLaunch("dev.jarvix.claude", "claude"); err != nil {
		t.Fatalf("ClaimLaunch: %v", err)
	}
	if store.Count() == 0 {
		t.Fatal("a fresh claim should keep the store awake")
	}

	// Long past the grace period, a window turns up wearing the class. The
	// launch it was promised for never happened, so it is the user's window.
	clock = clock.Add(DefaultClaimGrace + time.Minute)
	late := win("0xcc", "dev.jarvix.claude", 555)
	if _, ok := store.Managed(late, []desktop.Window{late}); ok {
		t.Fatal("an expired claim must not adopt a window that turns up later")
	}
	if got := store.Count(); got != 0 {
		t.Fatalf("count = %d after the claim expired, want 0", got)
	}
}

func TestClaimLaunchIgnoresAnEmptyIdentity(t *testing.T) {
	store, _ := newTestStore(t, nil)
	// A graphical launch issues no class at all. Claiming one would be
	// claiming to recognise a window Jarvix cannot recognise.
	if err := store.ClaimLaunch("  ", "firefox"); err != nil {
		t.Fatalf("ClaimLaunch: %v", err)
	}
	if got := store.Count(); got != 0 {
		t.Fatalf("count = %d, want 0 — an empty identity claims nothing", got)
	}
}

func TestOneClaimAdoptsOneWindow(t *testing.T) {
	store, _ := newTestStore(t, nil)
	if err := store.ClaimLaunch("dev.jarvix.claude", "claude"); err != nil {
		t.Fatalf("ClaimLaunch: %v", err)
	}
	first := win("0xc1", "dev.jarvix.claude", 501)
	second := win("0xc2", "dev.jarvix.claude", 502)
	live := store.List([]desktop.Window{first, second})
	if len(live) != 1 {
		t.Fatalf("one launch adopted %d windows; it should adopt exactly one", len(live))
	}
}

func TestAHandEditIsPickedUpWithoutARestart(t *testing.T) {
	store, path := newTestStore(t, nil)
	term := win("0xaa", "ghostty", 4242)
	inventory := []desktop.Window{term}
	if _, _, err := store.Acquire(term, inventory); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if _, ok := store.Managed(term, inventory); !ok {
		t.Fatal("precondition: the window should be managed")
	}

	// Deleting the stanza by hand is how a window is released in a text
	// editor, and it must not need a restart.
	handEdited := "version = 1\n"
	writeAt(t, path, handEdited)
	if _, ok := store.Managed(term, inventory); ok {
		t.Fatal("a hand-edit that removed the window should release it")
	}
}

func TestAnUnparseableStoreManagesNothingAndIsMovedAsideBeforeWriting(t *testing.T) {
	store, path := newTestStore(t, nil)
	writeAt(t, path, "this is not toml {{{")
	term := win("0xaa", "ghostty", 4242)
	inventory := []desktop.Window{term}

	if _, ok := store.Managed(term, inventory); ok {
		t.Fatal("an unparseable store must manage nothing")
	}
	if _, _, err := store.Acquire(term, inventory); err != nil {
		t.Fatalf("Acquire over a corrupt store: %v", err)
	}
	moved, err := os.ReadFile(path + ".corrupt")
	if err != nil {
		t.Fatalf("the unreadable file should have been moved aside: %v", err)
	}
	if !strings.Contains(string(moved), "not toml") {
		t.Fatalf("the moved-aside file is not the user's: %q", moved)
	}
	if _, ok := store.Managed(term, inventory); !ok {
		t.Fatal("the acquisition after the move-aside should have taken effect")
	}
}

func TestAnUnknownKeyIsTreatedAsCorruptionRatherThanDroppedSilently(t *testing.T) {
	store, path := newTestStore(t, nil)
	writeAt(t, path, "version = 1\n\n[[window]]\naddress = \"0xaa\"\nclass = \"ghostty\"\nsource = \"acquired\"\nstabel_id = \"typo\"\n")
	term := win("0xaa", "ghostty", 4242)
	if _, ok := store.Managed(term, []desktop.Window{term}); ok {
		t.Fatal("a file with an unknown key must not be half-read")
	}
}

func TestAnUnsupportedVersionManagesNothing(t *testing.T) {
	store, path := newTestStore(t, nil)
	writeAt(t, path, "version = 99\n")
	term := win("0xaa", "ghostty", 4242)
	if _, ok := store.Managed(term, []desktop.Window{term}); ok {
		t.Fatal("a future schema must degrade to nothing managed, never be guessed at")
	}
}

func TestAHandWrittenStanzaMayOmitTheFactsAUserCannotSee(t *testing.T) {
	store, path := newTestStore(t, nil)
	writeAt(t, path, "version = 1\n\n[[window]]\naddress = \"0xaa\"\nclass = \"ghostty\"\nsource = \"acquired\"\n")
	term := win("0xaa", "ghostty", 4242)
	if _, ok := store.Managed(term, []desktop.Window{term}); !ok {
		t.Fatal("a hand-written stanza with the facts a user can see should be honoured")
	}
}

func TestTheStoreRefusesPastItsLimit(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(filepath.Join(dir, "managed.toml"),
		StoreOptions{MaxWindows: 2}, quietLogger(t))
	inventory := []desktop.Window{win("0x1", "ghostty", 1), win("0x2", "ghostty", 2), win("0x3", "ghostty", 3)}
	for i := 0; i < 2; i++ {
		if _, _, err := store.Acquire(inventory[i], inventory); err != nil {
			t.Fatalf("Acquire %d: %v", i, err)
		}
	}
	if _, _, err := store.Acquire(inventory[2], inventory); err == nil {
		t.Fatal("acquiring past the limit should refuse")
	}
}

func TestReconcilingAQuietDesktopWritesNothing(t *testing.T) {
	store, _ := newTestStore(t, nil)
	term := win("0xaa", "ghostty", 4242)
	inventory := []desktop.Window{term}
	if _, _, err := store.Acquire(term, inventory); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	writes := 0
	store.write = func(path string, r []Record, c []Claim) error { writes++; return writeStore(path, r, c) }
	for i := 0; i < 5; i++ {
		store.ByAddress(inventory)
	}
	if writes != 0 {
		t.Fatalf("a quiet desktop cost %d writes; the poll must be free", writes)
	}
}

func TestListNamesTheLiveWindow(t *testing.T) {
	store, _ := newTestStore(t, nil)
	term := win("0xaa", "ghostty", 4242)
	if _, _, err := store.Acquire(term, []desktop.Window{term}); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// The title moved on since the acquisition; a listing must report the
	// window as it is now, not as it was.
	renamed := term
	renamed.Title = "go test ./..."
	live := store.List([]desktop.Window{renamed})
	if len(live) != 1 {
		t.Fatalf("List returned %d rows, want 1", len(live))
	}
	if live[0].Window.Title != "go test ./..." {
		t.Fatalf("title = %q, want the live one", live[0].Window.Title)
	}
}

func TestANilStoreAnswersLikeADaemonWithoutOne(t *testing.T) {
	var store *Store
	if _, ok := store.Managed(win("0xaa", "ghostty", 1), nil); ok {
		t.Fatal("a nil store manages nothing")
	}
	if got := store.Count(); got != 0 {
		t.Fatalf("count = %d, want 0", got)
	}
	if got := store.List(nil); got != nil {
		t.Fatalf("List = %+v, want nil", got)
	}
	if err := store.ClaimLaunch("x", "y"); err != nil {
		t.Fatalf("ClaimLaunch on a nil store: %v", err)
	}
	if _, held, err := store.Release(win("0xaa", "ghostty", 1), nil); err != nil || held {
		t.Fatalf("Release on a nil store: held=%v err=%v", held, err)
	}
}

func writeAt(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	// The store's hand-edit pickup keys on mtime and size, and a test that
	// rewrites a file inside one filesystem timestamp tick would look like no
	// change at all. Push the timestamp forward explicitly rather than sleep.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

// quietLogger keeps the store's warnings out of the test output while still
// exercising the code that writes them.
func quietLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
