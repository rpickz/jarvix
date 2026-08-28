package monitors

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/placement"
)

// The user's own arrangement, which is the acceptance case of issue #180:
// HDMI-A-1 above at 3440x1440, DP-2 below at 5120x1440, called "top" and
// "bottom". Every test here judges against a fake inventory rather than a
// compositor — the store holds no compositor by design, so an inventory is
// just a slice.
func topMonitor() placement.Monitor {
	return placement.Monitor{Name: "HDMI-A-1", X: 840, Y: 0, Width: 3440, Height: 1440,
		Scale: 1, Reserved: [4]int{0, 26, 0, 0}, Focused: true, ActiveWorkspace: 1}
}

func bottomMonitor() placement.Monitor {
	return placement.Monitor{Name: "DP-2", X: 0, Y: 1440, Width: 5120, Height: 1440,
		Scale: 1, Reserved: [4]int{0, 26, 0, 0}, ActiveWorkspace: 2}
}

func bothMonitors() []placement.Monitor {
	return []placement.Monitor{topMonitor(), bottomMonitor()}
}

// newStore builds a store over a fresh temp file with a fixed clock, so
// timestamps are assertions rather than noise.
func newStore(t *testing.T) *Store {
	t.Helper()
	at := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)
	return NewStore(filepath.Join(t.TempDir(), "monitors.toml"), StoreOptions{
		Now: func() time.Time { return at },
	}, nil)
}

// TestTheUsersOwnArrangementResolves is the acceptance case: name the two
// screens the way their owner talks about them, and the placement vocabulary
// answers with the right outputs — through the resolver every consumer uses,
// not through a lookup written for this test.
func TestTheUsersOwnArrangementResolves(t *testing.T) {
	s := newStore(t)
	for name, connector := range map[string]string{"top": "HDMI-A-1", "bottom": "DP-2"} {
		if _, err := s.Assign(name, connector, bothMonitors()); err != nil {
			t.Fatalf("Assign(%q, %q) = %v", name, connector, err)
		}
	}
	for ref, want := range map[placement.MonitorRef]string{
		"top": "HDMI-A-1", "bottom": "DP-2",
		// The forms that already worked must keep working beside them.
		"DP-2": "DP-2", placement.MonitorCurrent: "HDMI-A-1", "": "HDMI-A-1",
	} {
		got, err := s.Resolver().Resolve(ref, bothMonitors())
		if err != nil || got.Name != want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", ref, got.Name, err, want)
		}
	}
}

// TestACableMoveIsOneEdit is the whole point of the feature: the routine says
// "top" and never changes, and re-pointing the name is what makes the run
// land on the other screen.
func TestACableMoveIsOneEdit(t *testing.T) {
	s := newStore(t)
	if _, err := s.Assign("top", "HDMI-A-1", bothMonitors()); err != nil {
		t.Fatal(err)
	}
	stored, previous, err := s.Repoint("top", "DP-2", bothMonitors())
	if err != nil {
		t.Fatal(err)
	}
	if previous != "HDMI-A-1" {
		t.Errorf("re-pointing top reported previous %q, want HDMI-A-1", previous)
	}
	if stored.Connector != "DP-2" {
		t.Errorf("top now means %q, want DP-2", stored.Connector)
	}
	got, err := s.Resolver().Resolve("top", bothMonitors())
	if err != nil || got.Name != "DP-2" {
		t.Errorf("after the move Resolve(top) = %q, %v", got.Name, err)
	}
	// Re-asserting what is already true is not a collision and writes
	// nothing, so a routine that re-runs its own setup is idempotent.
	if _, previous, err := s.Repoint("top", "DP-2", bothMonitors()); err != nil || previous != "" {
		t.Errorf("re-asserting top = %q, %v", previous, err)
	}
	// Assign still refuses the name, because moving it is not what "call this
	// monitor top" means — a misheard utterance must not redirect a routine.
	if _, err := s.Assign("top", "HDMI-A-1", bothMonitors()); err == nil {
		t.Error("assigning a name another screen already answers to was accepted")
	}
}

// TestAnAbsentScreenIsNamedRatherThanGuessedAt: the disappearance contract.
// The nickname still exists — a dock is unplugged, not deleted — and
// resolution says what it means and that it is not there.
func TestAnAbsentScreenIsNamedRatherThanGuessedAt(t *testing.T) {
	s := newStore(t)
	if _, err := s.Assign("top", "HDMI-A-1", bothMonitors()); err != nil {
		t.Fatal(err)
	}
	onlyBottom := []placement.Monitor{bottomMonitor()}
	_, err := s.Resolver().Resolve("top", onlyBottom)
	if err == nil {
		t.Fatal("a nickname whose screen is unplugged resolved to something")
	}
	for _, want := range []string{`no monitor is called "top" right now`,
		"it means HDMI-A-1, which is not plugged in", "the screens plugged in are DP-2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not carry %q", err, want)
		}
	}
	// The nickname is still held: the user has not stopped calling that
	// screen "top" just because it is in a bag.
	if connector, ok := s.Lookup("top"); !ok || connector != "HDMI-A-1" {
		t.Errorf("Lookup(top) after unplugging = %q, %v", connector, ok)
	}
}

// TestTheCollisionMatrix walks every refusal the ticket names, and checks
// each one says who owns the word — a refusal that does not name the owner is
// a refusal the user cannot act on.
func TestTheCollisionMatrix(t *testing.T) {
	s := newStore(t)
	if _, err := s.Assign("top", "HDMI-A-1", bothMonitors()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		spoken string
		field  string
		want   string
	}{
		{"a connector that is plugged in", "DP-2", FieldName, "already the name of DP-2 (5120 by 1440)"},
		{"the other connector", "HDMI-A-1", FieldName, "already the name of HDMI-A-1 (3440 by 1440)"},
		{"a connector nothing is plugged into", "DP-9", FieldName, "how a screen names itself"},
		{"the reserved word current", "current", FieldName, "it is the screen you are on"},
		{"the reserved word primary", "primary", FieldName, "it is kept for the main screen"},
		{"another nickname", "top", FieldName, "already the name of HDMI-A-1 (3440 by 1440)"},
		{"two words", "top left", FieldName, `try just "top"`},
		{"nothing at all", "  ", FieldName, "did not catch a name"},
		{"punctuation only", "!!", FieldName, "did not catch a name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// "top" is assigned to HDMI-A-1, so naming DP-2 "top" is the
			// nickname collision; everything else is judged against DP-2.
			target := "DP-2"
			_, err := s.Assign(tc.spoken, target, bothMonitors())
			var refusal *Refusal
			if !errors.As(err, &refusal) {
				t.Fatalf("Assign(%q) = %v, want a field-keyed refusal", tc.spoken, err)
			}
			if refusal.Problem.Field != tc.field {
				t.Errorf("refusal keyed to %q, want %q", refusal.Problem.Field, tc.field)
			}
			if !strings.Contains(refusal.Error(), tc.want) {
				t.Errorf("refusal %q does not name the owner (%q)", refusal, tc.want)
			}
		})
	}
	// Nothing above was stored: a refused assignment must leave the store
	// exactly as it was.
	if names := s.List(); len(names) != 1 || names[0].Name != "top" {
		t.Errorf("after nine refusals the store holds %+v", names)
	}
}

// TestANicknameNeverOutranksAScreenThatIsPluggedIn restates the vocabulary's
// precedence through this store, because the refusal above is only half the
// guarantee: even a nickname smuggled in by hand-editing the file must not
// redirect a routine that named a real connector.
func TestANicknameNeverOutranksAScreenThatIsPluggedIn(t *testing.T) {
	s := newStore(t)
	writeByHand(t, s.Path(), `version = 1

[[nickname]]
name = "dp2"
connector = "HDMI-A-1"
`)
	// "dp2" is not "DP-2" — the store cannot hold the latter — so the two
	// spellings are checked separately: the connector resolves to itself.
	got, err := s.Resolver().Resolve("DP-2", bothMonitors())
	if err != nil || got.Name != "DP-2" {
		t.Errorf("Resolve(DP-2) = %q, %v; a present output must win", got.Name, err)
	}
	if got, err := s.Resolver().Resolve("dp2", bothMonitors()); err != nil || got.Name != "HDMI-A-1" {
		t.Errorf("Resolve(dp2) = %q, %v", got.Name, err)
	}
}

// TestForgettingIsHonestAboutWhatItDidNotFind: removal by name, and a name
// nothing holds is said so rather than answered with "done".
func TestForgettingIsHonestAboutWhatItDidNotFind(t *testing.T) {
	s := newStore(t)
	if _, err := s.Assign("top", "HDMI-A-1", bothMonitors()); err != nil {
		t.Fatal(err)
	}
	gone, err := s.Forget("Top.")
	if err != nil || gone.Connector != "HDMI-A-1" {
		t.Fatalf("Forget(top) = %+v, %v", gone, err)
	}
	if _, ok := s.Lookup("top"); ok {
		t.Error("a forgotten nickname still resolves")
	}
	if _, err := s.Forget("bottom"); !errors.Is(err, ErrUnknownNickname) {
		t.Errorf("Forget(bottom) = %v, want ErrUnknownNickname", err)
	}
}

// TestNoNicknamesConfiguredIsTodaysBehaviourExactly is the pinned baseline:
// with nothing named — and with no store at all — every reference behaves
// byte-for-byte as it did before this feature existed.
func TestNoNicknamesConfiguredIsTodaysBehaviourExactly(t *testing.T) {
	empty := newStore(t)
	var absent *Store // a daemon built without a store at all
	for _, r := range []placement.Resolver{
		placement.Resolver{}, // the pre-#180 resolver, verbatim
		empty.Resolver(),
		absent.Resolver(),
	} {
		for ref, want := range map[placement.MonitorRef]string{
			"DP-2": "DP-2", "HDMI-A-1": "HDMI-A-1",
			placement.MonitorCurrent: "HDMI-A-1", "": "HDMI-A-1",
		} {
			got, err := r.Resolve(ref, bothMonitors())
			if err != nil || got.Name != want {
				t.Errorf("Resolve(%q) = %q, %v; want %q", ref, got.Name, err, want)
			}
		}
		_, err := r.Resolve("top", bothMonitors())
		if err == nil || !strings.Contains(err.Error(), `no monitor is called "top" right now`) {
			t.Errorf("Resolve(top) with nothing named = %v", err)
		}
	}
	// The zero store also costs no file: a daemon nobody names a screen on
	// leaves the state dir exactly as it found it.
	if _, err := os.Stat(empty.Path()); !os.IsNotExist(err) {
		t.Errorf("an untouched store wrote %s (%v)", empty.Path(), err)
	}
}

// TestAHandEditIsLiveOnTheNextResolution: the file is the user's, and
// correcting it in a text editor works as well as saying it. The stat-per-
// operation refresh is what makes that true without a restart.
func TestAHandEditIsLiveOnTheNextResolution(t *testing.T) {
	s := newStore(t)
	if _, err := s.Assign("top", "HDMI-A-1", bothMonitors()); err != nil {
		t.Fatal(err)
	}
	writeByHand(t, s.Path(), `version = 1

[[nickname]]
name = "Top"
connector = "DP-2"

[[nickname]]
name = "bottom"
connector = "HDMI-A-1"
`)
	if connector, ok := s.Lookup("top"); !ok || connector != "DP-2" {
		t.Errorf("after the hand-edit top means %q (%v), want DP-2", connector, ok)
	}
	// The edit is normalised on the way in — "Top" is looked up as "top" —
	// and the file itself is left exactly as the user wrote it.
	if names := s.List(); len(names) != 2 || names[0].Name != "bottom" || names[1].Name != "top" {
		t.Errorf("hand-edited store lists %+v", names)
	}
	raw, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `name = "Top"`) {
		t.Error("reading the store rewrote the user's file")
	}
}

// TestACorruptFileCostsNicknamesAndNothingElse: a broken file must not take
// the morning routine with it, must never be silently overwritten, and is
// moved aside the moment something is genuinely saved.
func TestACorruptFileCostsNicknamesAndNothingElse(t *testing.T) {
	s := newStore(t)
	writeByHand(t, s.Path(), "version = 1\n[[nickname]]\nname = \"top\"\nconector = \"DP-2\"\n")
	if names := s.List(); len(names) != 0 {
		t.Errorf("a corrupt store served %+v", names)
	}
	// A connector reference still resolves: the failure costs the user their
	// nicknames, not their placement.
	if got, err := s.Resolver().Resolve("DP-2", bothMonitors()); err != nil || got.Name != "DP-2" {
		t.Errorf("Resolve(DP-2) against a corrupt store = %q, %v", got.Name, err)
	}
	if _, err := s.Assign("top", "DP-2", bothMonitors()); err != nil {
		t.Fatal(err)
	}
	moved, err := os.ReadFile(s.Path() + ".corrupt")
	if err != nil {
		t.Fatalf("the unreadable file was overwritten rather than moved aside: %v", err)
	}
	if !strings.Contains(string(moved), "conector") {
		t.Errorf("the moved-aside file is not the user's: %q", moved)
	}
	if connector, ok := s.Lookup("top"); !ok || connector != "DP-2" {
		t.Errorf("after recovery top means %q (%v)", connector, ok)
	}
}

// TestAFailedWriteLeavesTheStoreDescribingTheDisk: memory is committed only
// after the write succeeds, so a full disk does not produce a daemon that
// answers with nicknames the file does not hold.
func TestAFailedWriteLeavesTheStoreDescribingTheDisk(t *testing.T) {
	s := newStore(t)
	s.write = func(string, []Nickname) error { return errors.New("no space left on device") }
	if _, err := s.Assign("top", "HDMI-A-1", bothMonitors()); err == nil {
		t.Fatal("a failed write reported success")
	}
	if _, ok := s.Lookup("top"); ok {
		t.Error("a nickname that was never written resolves")
	}
}

// TestTheStoreIsSafeUnderConcurrentUse: the resolver is called from the
// routine runner and the window tools while the window and voice assign
// names, so every path takes the same lock. Meaningful only under -race.
func TestTheStoreIsSafeUnderConcurrentUse(t *testing.T) {
	s := newStore(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); _, _ = s.Assign("top", "HDMI-A-1", bothMonitors()) }()
		go func() { defer wg.Done(); _, _ = s.Lookup("top") }()
		go func() { defer wg.Done(); _ = s.List() }()
	}
	wg.Wait()
	if connector, ok := s.Lookup("top"); !ok || connector != "HDMI-A-1" {
		t.Errorf("after the concurrent run top means %q (%v)", connector, ok)
	}
}

// TestTheStoreIsCapped: a bounded file, refused loudly at the limit.
func TestTheStoreIsCapped(t *testing.T) {
	s := newStore(t)
	s.max = 2
	present := bothMonitors()
	for _, name := range []string{"top", "bottom"} {
		if _, err := s.Assign(name, "DP-2", present); err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.Assign("middle", "DP-2", present)
	if !errors.Is(err, ErrStoreFull) {
		t.Fatalf("the third name = %v, want ErrStoreFull", err)
	}
	if !strings.Contains(err.Error(), "2 screen names is the limit") {
		t.Errorf("the refusal does not name the limit: %v", err)
	}
	if n, max := s.Count(); n != 2 || max != 2 {
		t.Errorf("Count() = %d/%d", n, max)
	}
}

// TestAScreenThatIsNotThereCannotBeNamed: assignment is stricter than
// resolution on purpose — at assignment the user is looking at their screens,
// so a connector nobody can see is a typo.
func TestAScreenThatIsNotThereCannotBeNamed(t *testing.T) {
	s := newStore(t)
	_, err := s.Assign("top", "DP-9", bothMonitors())
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("Assign to an absent screen = %v", err)
	}
	if refusal.Problem.Field != FieldConnector {
		t.Errorf("keyed to %q, want %q", refusal.Problem.Field, FieldConnector)
	}
	if !strings.Contains(refusal.Error(), "the screens plugged in are DP-2, HDMI-A-1") {
		t.Errorf("refusal %q does not list what is plugged in", refusal)
	}
	if _, err := s.Assign("top", "", bothMonitors()); err == nil {
		t.Error("naming no screen at all was accepted")
	}
	// The compositor's spelling is what gets stored, never the caller's.
	stored, err := s.Assign("top", "hdmi-a-1", bothMonitors())
	if err != nil || stored.Connector != "HDMI-A-1" {
		t.Errorf("Assign(top, hdmi-a-1) stored %q, %v", stored.Connector, err)
	}
}

// TestValidateFileProvesAnArchiveBeforeItIsRestored: the `jarvix restore`
// hook (ADR 0045), so a staged archive carrying a broken store is refused
// before anything is swapped into place.
func TestValidateFileProvesAnArchiveBeforeItIsRestored(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.toml")
	writeByHand(t, good, "version = 1\n\n[[nickname]]\nname = \"top\"\nconnector = \"DP-2\"\n")
	if err := ValidateFile(good); err != nil {
		t.Errorf("a sound store was refused: %v", err)
	}
	bad := filepath.Join(dir, "bad.toml")
	writeByHand(t, bad, "version = 99\n")
	if err := ValidateFile(bad); err == nil {
		t.Error("a store from an unsupported version was accepted")
	}
}

// writeByHand writes a file the way a user with a text editor would.
func writeByHand(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	// The refresh compares mtime and size, and a same-second rewrite of the
	// same length would look unchanged on a filesystem with second
	// granularity. Stamp the file into the past so the next stat is
	// unambiguous — an assertion about the file, not about the clock.
	past := time.Now().Add(-time.Duration(len(content)+1) * time.Second)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatal(err)
	}
}
