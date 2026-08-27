package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/desktop"
)

// The nickname tests (#126) pin the seam every consumer shares: precedence
// over every matching tier, the assignment flows (deictic default,
// refusals as speakable results, the common-word caution), release on close
// through lazy revalidation, and the listings.

// nicknameHarness is newHarness with the two knobs these tests need: a
// phrase owner for the intent-collision refusal, and an inventory TTL of one
// nanosecond so a window "closing" in the fake is seen on the very next
// look — the release mechanism is revalidation, and these tests revalidate.
func nicknameHarness(t *testing.T, owner func(string) (string, bool), windows ...desktop.Window) *harness {
	t.Helper()
	if len(windows) == 0 {
		windows = testWindows()
	}
	h := &harness{
		comp:     desktop.NewFakeCompositor(windows...),
		launcher: &fakeLauncher{},
	}
	h.d = NewDesktop(DesktopOptions{
		Compositor:   h.comp,
		launcher:     h.launcher,
		InventoryTTL: time.Nanosecond,
		PhraseOwner:  owner,
		OnAction: func(verb, target string) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.events = append(h.events, verb+":"+target)
		},
		OnRefusal: func(verb, target, reason string) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.refusals = append(h.refusals, verb+":"+target+":"+reason)
		},
	})
	return h
}

func mustAssign(t *testing.T, h *harness, reference, name string) string {
	t.Helper()
	spoken, err := h.d.AssignNickname(context.Background(), reference, name)
	if err != nil {
		t.Fatalf("AssignNickname(%q, %q): %v", reference, name, err)
	}
	return spoken
}

// TestNicknameOutranksEveryMatchingTier is the precedence pin the issue asks
// for: a nickname resolves before ANY fuzzy matching — an exact application
// name (the strongest tier there is) and a title word alike. "focus builds"
// must be deterministic the day after "call this window builds", whatever
// the other windows say about themselves.
func TestNicknameOutranksEveryMatchingTier(t *testing.T) {
	h := nicknameHarness(t, nil)
	// The code editor takes the name of an application that is actually
	// open: the nickname must still win over the exact class match.
	mustAssign(t, h, "the code window", "firefox")
	out := h.run(t, FocusWindowToolName, map[string]any{"window": "firefox"})
	if !strings.Contains(out, "Switched to code") {
		t.Fatalf("focus firefox = %q, want the nicknamed window, not the firefox windows", out)
	}
	// And over a title word: firefox's title contains "github", but the
	// terminal is *called* github.
	mustAssign(t, h, "alacritty", "github")
	out = h.run(t, FocusWindowToolName, map[string]any{"window": "github"})
	if !strings.Contains(out, "Switched to Alacritty") {
		t.Fatalf("focus github = %q, want the nicknamed terminal, not the title match", out)
	}
}

func TestAssignDefaultsToTheFocusedWindow(t *testing.T) {
	h := nicknameHarness(t, nil)
	spoken := mustAssign(t, h, "", "builds")
	if !strings.Contains(spoken, "the code window is now called builds") {
		t.Errorf("spoken = %q, want the focused window named", spoken)
	}
	if got := h.firedEvents(); len(got) != 1 || !strings.HasPrefix(got[0], "name:code — engine.go") {
		t.Errorf("events = %v, want one name action", got)
	}
	out := h.run(t, MoveWindowToolName, map[string]any{"window": "builds", "workspace": 3})
	if !strings.Contains(out, "Moved code — engine.go to workspace 3") {
		t.Errorf("move builds = %q", out)
	}
}

// TestClosedWindowNicknameAnswersHonestly: the named window closes, and
// referring to it says "nothing is called builds right now" — released, not
// unknown, and certainly not some other window.
func TestClosedWindowNicknameAnswersHonestly(t *testing.T) {
	h := nicknameHarness(t, nil)
	mustAssign(t, h, "alacritty", "builds")
	windows := testWindows()
	h.comp.SetWindows(windows[:3]...) // the terminal is gone
	out := h.run(t, FocusWindowToolName, map[string]any{"window": "builds"})
	if !strings.Contains(out, `Nothing is called "builds" right now`) {
		t.Fatalf("focus builds = %q, want the released-nickname answer", out)
	}
	if got := h.firedEvents(); len(got) != 1 {
		t.Errorf("events = %v, want only the assignment — nothing was focused", got)
	}
}

// TestNicknameRefusalsAreSpeakableResults: the model tool reports refusals
// as results to say in one sentence — never errors — and the refusal
// reaches the activity feed with its reason.
func TestNicknameRefusalsAreSpeakableResults(t *testing.T) {
	h := nicknameHarness(t, nil)
	out := h.run(t, NameWindowToolName, map[string]any{"name": "the build terminal"})
	if !strings.Contains(out, "Nothing was named") || !strings.Contains(out, "single word") {
		t.Fatalf("name_window = %q, want the single-word refusal with guidance", out)
	}
	refusals := h.firedRefusals()
	if len(refusals) != 1 || !strings.Contains(refusals[0], "name:") ||
		!strings.Contains(refusals[0], "single word") {
		t.Errorf("refusals = %v, want the reason on the bus", refusals)
	}
}

func TestNicknameCollisionNamesTheIntentOwner(t *testing.T) {
	h := nicknameHarness(t, func(phrase string) (string, bool) {
		if phrase == "mute" {
			return `the built-in intent "volume.mute"`, true
		}
		return "", false
	})
	out := h.run(t, NameWindowToolName, map[string]any{"name": "mute"})
	if !strings.Contains(out, `the built-in intent "volume.mute"`) {
		t.Errorf("name_window = %q, want the intent owner named", out)
	}
}

func TestNicknameCommonWordWarnsInTheConfirmation(t *testing.T) {
	h := nicknameHarness(t, nil)
	spoken := mustAssign(t, h, "", "work")
	if !strings.Contains(spoken, "now called work") || !strings.Contains(spoken, "common word") {
		t.Errorf("spoken = %q, want the assignment with the caution suffixed", spoken)
	}
}

func TestListWindowsCarriesNicknames(t *testing.T) {
	h := nicknameHarness(t, nil)
	mustAssign(t, h, "alacritty", "builds")
	out := h.run(t, ListWindowsToolName, nil)
	if !strings.Contains(out, "the user calls it builds") {
		t.Errorf("list = %q, want the nickname alongside its window", out)
	}
}

func TestNicknameListingSpeaksTheNames(t *testing.T) {
	h := nicknameHarness(t, nil)
	empty, err := h.d.NicknameListing(context.Background())
	if err != nil || !strings.Contains(empty, "No windows have names") {
		t.Fatalf("listing = %q, %v", empty, err)
	}
	mustAssign(t, h, "alacritty", "builds")
	mustAssign(t, h, "the code window", "notes2") // "notes" is reserved; a digit keeps it a valid name
	spoken, err := h.d.NicknameListing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"2 windows have names", "builds is Alacritty — go test", "notes2 is code — engine.go"} {
		if !strings.Contains(spoken, want) {
			t.Errorf("listing = %q, missing %q", spoken, want)
		}
	}
}

// TestResolveReferenceIsTheSharedSeam: the public resolution seam behaves
// exactly as the window verbs do — one window, or a spoken-ready explanation
// — so #123/#124 consumers inherit nicknames without code of their own.
func TestResolveReferenceIsTheSharedSeam(t *testing.T) {
	h := nicknameHarness(t, nil)
	mustAssign(t, h, "alacritty", "builds")
	if w, ok, _ := h.d.ResolveReference(context.Background(), "builds"); !ok || w.Class != "Alacritty" {
		t.Errorf("ResolveReference(builds) = %+v, %v", w, ok)
	}
	if _, ok, explain := h.d.ResolveReference(context.Background(), "firefox"); ok ||
		!strings.Contains(explain, "Several windows match") {
		t.Errorf("ResolveReference(firefox) explain = %q, want the ambiguity named", explain)
	}
	windows := testWindows()
	h.comp.SetWindows(windows[:3]...)
	if _, ok, explain := h.d.ResolveReference(context.Background(), "builds"); ok ||
		!strings.Contains(explain, `Nothing is called "builds" right now`) {
		t.Errorf("ResolveReference after close explain = %q, want the released answer", explain)
	}
	if _, ok, explain := h.d.ResolveReference(context.Background(), "xyzzy"); ok ||
		!strings.Contains(explain, "Nothing like") {
		t.Errorf("ResolveReference(xyzzy) explain = %q", explain)
	}
}

// TestWindowListingsNeverCarryAddresses: the daemon verb's rows are
// person-facing facts only — the nickname travels, the address never does.
func TestWindowListingsNeverCarryAddresses(t *testing.T) {
	h := nicknameHarness(t, nil)
	mustAssign(t, h, "alacritty", "builds")
	listings, err := h.d.WindowListings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(listings) != len(testWindows()) {
		t.Fatalf("listings = %d rows, want %d", len(listings), len(testWindows()))
	}
	var builds, focused bool
	for _, l := range listings {
		if l.Nickname == "builds" && strings.EqualFold(l.App, "alacritty") {
			builds = true
		}
		if l.Focused && l.App == "code" {
			focused = true
		}
	}
	if !builds || !focused {
		t.Errorf("listings = %+v, want the nickname and the focus marked", listings)
	}
	raw, err := json.Marshal(listings)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "0x") {
		t.Errorf("listings leak an address: %s", raw)
	}
}
