package overlay

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/desktop"
)

// Compose is the whole overlay decision surface, so these tests are the
// issue's acceptance criteria restated as table rows: enrolment, badge fill,
// the AI-state slot, the clean-by-default rule, fullscreen, workspaces, and
// the occlusion honesty rules — all without a compositor anywhere.

// win builds one inventory entry with geometry; the focused window names the
// visible workspace throughout.
func win(addr string, ws, x, y, w, h int, focused bool) desktop.Window {
	return desktop.Window{Address: addr, Class: "app", Title: "t", Workspace: ws,
		X: x, Y: y, Width: w, Height: h, Focused: focused}
}

func TestComposeBadgesFollowEnrolmentAndActivity(t *testing.T) {
	windows := []desktop.Window{
		win("0xa", 1, 0, 0, 800, 600, true),      // anchored to the active thread
		win("0xb", 1, 800, 0, 800, 600, false),   // anchored to an inactive thread
		win("0xc", 1, 0, 600, 800, 400, false),   // nickname only
		win("0xd", 1, 800, 600, 800, 400, false), // enrolled in nothing
	}
	threads := []Thread{
		{Name: "ci refactor", Active: true, Anchors: []string{"0xa"}},
		{Name: "invoices", Active: false, Anchors: []string{"0xb"}},
	}
	rows := Compose(true, windows, threads, map[string]string{"0xc": "notes"}, nil)
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3 (the unenrolled window must stay clean): %+v", len(rows), rows)
	}
	byPos := map[[2]int]Row{}
	for _, r := range rows {
		byPos[[2]int{r.X, r.Y}] = r
	}
	active := byPos[[2]int{0, 0}]
	if active.Badge == nil || !active.Badge.Active || active.Badge.Thread != "ci refactor" {
		t.Errorf("active anchor badge = %+v, want filled ci refactor", active.Badge)
	}
	inactive := byPos[[2]int{800, 0}]
	if inactive.Badge == nil || inactive.Badge.Active || inactive.Badge.Thread != "invoices" {
		t.Errorf("inactive anchor badge = %+v, want hollow invoices", inactive.Badge)
	}
	tagged := byPos[[2]int{0, 600}]
	if tagged.Badge != nil || tagged.Tag != "notes" {
		t.Errorf("nickname-only row = %+v, want tag notes and no badge", tagged)
	}
	if tagged.AIState != "" {
		t.Errorf("nickname-only row carries ai_state %q; the dot rides the badge only", tagged.AIState)
	}
}

// The AI-state slot (#137): only the three published tokens travel; absent
// and unknown states alike produce no dot, so a build older than the
// classifier's vocabulary degrades to absence rather than a guessed colour.
func TestComposeAIStateAdmitsOnlyTheKnownVocabulary(t *testing.T) {
	windows := []desktop.Window{win("0xa", 1, 0, 0, 800, 600, true)}
	for state, want := range map[string]string{
		StateWorking:  StateWorking,
		StateNeedsYou: StateNeedsYou,
		StateDone:     StateDone,
		"":            "",
		"unknown":     "",
		"confused":    "",
	} {
		threads := []Thread{{Name: "th", Active: true, Anchors: []string{"0xa"}, AIState: state}}
		rows := Compose(true, windows, threads, nil, nil)
		if len(rows) != 1 {
			t.Fatalf("state %q: rows = %d, want 1", state, len(rows))
		}
		if rows[0].AIState != want {
			t.Errorf("state %q: ai_state = %q, want %q", state, rows[0].AIState, want)
		}
	}
}

func TestComposeDisabledOrUnfocusedIsEmpty(t *testing.T) {
	windows := []desktop.Window{win("0xa", 1, 0, 0, 800, 600, true)}
	threads := []Thread{{Name: "th", Active: true, Anchors: []string{"0xa"}}}
	if rows := Compose(false, windows, threads, nil, nil); rows != nil {
		t.Errorf("disabled: rows = %+v, want none — overlays.enabled is the one global off switch", rows)
	}
	// No focused window: no workspace is knowably visible, so nothing may
	// claim to be on screen.
	blurred := []desktop.Window{win("0xa", 1, 0, 0, 800, 600, false)}
	if rows := Compose(true, blurred, threads, nil, nil); rows != nil {
		t.Errorf("no focus: rows = %+v, want none", rows)
	}
}

func TestComposeFullscreenSilencesTheWorkspace(t *testing.T) {
	full := win("0xf", 1, 0, 0, 1920, 1080, true)
	full.Fullscreen = true
	windows := []desktop.Window{full, win("0xa", 1, 0, 0, 800, 600, false)}
	threads := []Thread{
		{Name: "th", Active: true, Anchors: []string{"0xa"}},
		{Name: "video", Active: false, Anchors: []string{"0xf"}},
	}
	if rows := Compose(true, windows, threads, nil, nil); rows != nil {
		t.Errorf("fullscreen workspace: rows = %+v, want none — an overlay must not float over "+
			"a covering window, and the fullscreen window's own overlay hides by design", rows)
	}
}

func TestComposeOverlaysOnlyTheFocusedWorkspace(t *testing.T) {
	windows := []desktop.Window{
		win("0xa", 1, 0, 0, 800, 600, true),
		win("0xb", 2, 0, 0, 800, 600, false), // enrolled, but elsewhere
	}
	threads := []Thread{{Name: "th", Active: true, Anchors: []string{"0xa", "0xb"}}}
	rows := Compose(true, windows, threads, nil, nil)
	if len(rows) != 1 || rows[0].X != 0 || rows[0].Y != 0 {
		t.Fatalf("rows = %+v, want only the focused workspace's window", rows)
	}
}

// Occlusion honesty: floating draws above tiled, so a floating window over a
// tiled window's corner suppresses that overlay; between two floating
// windows only the focused one is knowably on top.
func TestComposeSuppressesOverlaysUnderFloatingWindows(t *testing.T) {
	floater := win("0xfl", 1, 700, 0, 400, 300, true)
	floater.Floating = true
	covered := win("0xa", 1, 0, 0, 800, 600, false) // corner under the floater
	clear := win("0xb", 1, 0, 600, 800, 400, false) // nowhere near it
	windows := []desktop.Window{floater, covered, clear}
	threads := []Thread{{Name: "th", Active: true, Anchors: []string{"0xa", "0xb"}}}
	rows := Compose(true, windows, threads, map[string]string{"0xfl": "float"}, nil)
	positions := map[[2]int]bool{}
	for _, r := range rows {
		positions[[2]int{r.X, r.Y}] = true
	}
	if positions[[2]int{0, 0}] {
		t.Error("tiled window under a floating window kept its overlay; the chip would draw over the floater")
	}
	if !positions[[2]int{0, 600}] {
		t.Error("uncovered tiled window lost its overlay")
	}
	// The focused floating window is knowably topmost: its own tag stays.
	if !positions[[2]int{700, 0}] {
		t.Error("focused floating window lost its overlay; raise-on-focus makes it the one knowable top")
	}

	// Two overlapping floating windows, candidate not focused: unknowable
	// stacking, so the safe answer is no overlay.
	other := win("0xfl2", 1, 900, 20, 400, 300, false)
	other.Floating = true
	unfocused := win("0xfl3", 1, 700, 0, 400, 300, false)
	unfocused.Floating = true
	tiledFocus := win("0xt", 1, 0, 600, 600, 400, true)
	windows = []desktop.Window{tiledFocus, other, unfocused}
	threads = []Thread{{Name: "th", Active: true, Anchors: []string{"0xfl3"}}}
	rows = Compose(true, windows, threads, nil, nil)
	for _, r := range rows {
		if r.X == 700 && r.Y == 0 {
			t.Errorf("unfocused floating window under another floater kept its overlay: %+v", r)
		}
	}
}

// Two threads anchoring the same window: the active thread owns the badge;
// between inactive ones the first in snapshot order does. Deterministic, so
// the badge cannot flap between polls.
func TestComposeActiveThreadWinsASharedAnchor(t *testing.T) {
	windows := []desktop.Window{win("0xa", 1, 0, 0, 800, 600, true)}
	threads := []Thread{
		{Name: "first inactive", Active: false, Anchors: []string{"0xa"}},
		{Name: "the active one", Active: true, Anchors: []string{"0xa"}, AIState: StateNeedsYou},
	}
	rows := Compose(true, windows, threads, nil, nil)
	if len(rows) != 1 || rows[0].Badge == nil {
		t.Fatalf("rows = %+v, want one badged row", rows)
	}
	if rows[0].Badge.Thread != "the active one" || !rows[0].Badge.Active {
		t.Errorf("badge = %+v, want the active thread's", rows[0].Badge)
	}
	if rows[0].AIState != StateNeedsYou {
		t.Errorf("ai_state = %q, want the winning thread's %q", rows[0].AIState, StateNeedsYou)
	}
	// Inactive-only: first in order wins, every time.
	threads[1].Active = false
	for range 5 {
		rows = Compose(true, windows, threads, nil, nil)
		if rows[0].Badge.Thread != "first inactive" {
			t.Fatalf("badge thread = %q, want the first inactive thread deterministically", rows[0].Badge.Thread)
		}
	}
}

func TestComposeSkipsWindowsWithoutGeometry(t *testing.T) {
	// A compositor that reports no geometry reports zeros; a chip pinned at
	// 0,0 would decorate the wrong corner of the wrong monitor.
	flat := win("0xa", 1, 0, 0, 0, 0, true)
	windows := []desktop.Window{flat}
	threads := []Thread{{Name: "th", Active: true, Anchors: []string{"0xa"}}}
	if rows := Compose(true, windows, threads, nil, nil); rows != nil {
		t.Errorf("rows = %+v, want none for zero-sized geometry", rows)
	}
}

// The wire shape: rows must never leak a window address or anything
// compositor-internal (ADR 0022) — geometry, tag, badge, and state are the
// whole vocabulary.
func TestComposedRowsNeverCarryAddresses(t *testing.T) {
	windows := []desktop.Window{win("0xdeadbeef", 1, 0, 0, 800, 600, true)}
	threads := []Thread{{Name: "th", Active: true, Anchors: []string{"0xdeadbeef"}, AIState: StateWorking}}
	rows := Compose(true, windows, threads, map[string]string{"0xdeadbeef": "builds"}, nil)
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	wire := string(raw)
	for _, leak := range []string{"0xdeadbeef", "address", "class", "title", "pid"} {
		if strings.Contains(wire, leak) {
			t.Errorf("wire payload %q carries %q; addresses and window identity never travel (ADR 0022)", wire, leak)
		}
	}
}
