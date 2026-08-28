package routine

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/placement"
)

// lookPathFor fakes binary resolution: only the listed names are installed.
func lookPathFor(installed ...string) func(string) (string, error) {
	set := make(map[string]bool, len(installed))
	for _, name := range installed {
		set[name] = true
	}
	return func(name string) (string, error) {
		if set[name] {
			return "/usr/bin/" + name, nil
		}
		return "", fmt.Errorf("%s: not found", name)
	}
}

// TestSnapshotExcludesByTheDocumentedRules pins each exclusion rule with one
// window, and pins that the counts — the spoken confirmation's numbers —
// cover only what was kept.
func TestSnapshotExcludesByTheDocumentedRules(t *testing.T) {
	windows := []desktop.Window{
		{Address: "0x1", Class: "firefox", Workspace: 2, AcceptsInput: true},
		{Address: "0x2", Class: "jarvix", Workspace: 1, AcceptsInput: true},                     // Jarvix itself
		{Address: "0x3", Class: "omarchy-shell", Workspace: 1, AcceptsInput: true},              // the shell hosting the window
		{Address: "0x4", Class: "firefox", Title: "Splash", Workspace: 2},                       // accepts no input: transient
		{Address: "0x5", Class: "", Title: "Untitled dialog", Workspace: 2, AcceptsInput: true}, // classless transient
		{Address: "0x6", Class: "spotify", Workspace: -98, WorkspaceName: "special:music", AcceptsInput: true},
		{Address: "0x7", Class: "alacritty", Workspace: 1, AcceptsInput: true},
	}
	snap := Snapshot("morning setup", windows, CaptureOptions{LookPath: lookPathFor("firefox", "alacritty")})

	if snap.Kept != 2 || snap.Excluded != 5 {
		t.Fatalf("kept %d, excluded %d, want 2 and 5", snap.Kept, snap.Excluded)
	}
	if snap.Workspaces != 2 {
		t.Errorf("workspaces = %d, want 2", snap.Workspaces)
	}
	var apps []string
	for _, s := range snap.Definition.Steps {
		apps = append(apps, s.App)
	}
	if !reflect.DeepEqual(apps, []string{"alacritty", "firefox"}) {
		t.Errorf("steps = %v, want the kept windows ordered by workspace", apps)
	}
	if problems := Problems([]Definition{snap.Definition}); len(problems) != 0 {
		t.Errorf("a snapshot must validate as written: %v", problems)
	}
}

// TestSnapshotDerivesCommandsAndMatches: the class→command derivation, the
// match override when class and binary differ, and the "-desktop" packaging
// convention — each verified back through the real dedupe matcher, so what
// capture writes is what the runner will find.
func TestSnapshotDerivesCommandsAndMatches(t *testing.T) {
	windows := []desktop.Window{
		{Address: "0x1", Class: "firefox", Workspace: 1, AcceptsInput: true},
		{Address: "0x2", Class: "md.obsidian.Obsidian", Workspace: 2, AcceptsInput: true},
		{Address: "0x3", Class: "Signal", Workspace: 3, AcceptsInput: true},
	}
	snap := Snapshot("apps", windows, CaptureOptions{
		LookPath: lookPathFor("firefox", "obsidian", "signal-desktop"),
	})
	steps := snap.Definition.Steps
	if len(steps) != 3 || len(snap.Placeholders) != 0 {
		t.Fatalf("steps = %+v, placeholders = %v", steps, snap.Placeholders)
	}
	want := []Step{
		{App: "firefox", Placement: placement.Placement{Workspace: 1, Mode: placement.ModeTiled}},
		{App: "obsidian", Match: "md.obsidian.Obsidian",
			Placement: placement.Placement{Workspace: 2, Mode: placement.ModeTiled}},
		{App: "signal-desktop", Match: "Signal",
			Placement: placement.Placement{Workspace: 3, Mode: placement.ModeTiled}},
	}
	for i := range want {
		if !reflect.DeepEqual(steps[i], want[i]) {
			t.Errorf("steps[%d] = %+v, want %+v", i, steps[i], want[i])
		}
		if !steps[i].Claims(windows[i]) {
			t.Errorf("steps[%d] would not claim its own window through the dedupe matcher", i)
		}
	}
}

// TestSnapshotWritesAPlaceholderNeverDrops: an underivable launch command —
// here a browser web-app window whose class is a URL, not a program — still
// becomes a step: placeholder app, match on the class so dedupe keeps
// working, a note for the file, and a name for the spoken warning.
func TestSnapshotWritesAPlaceholderNeverDrops(t *testing.T) {
	w := desktop.Window{Address: "0x1", Class: "chrome-web.whatsapp.com__-Default",
		Workspace: 3, AcceptsInput: true}
	snap := Snapshot("chat", []desktop.Window{w}, CaptureOptions{LookPath: lookPathFor()})

	if snap.Kept != 1 || len(snap.Definition.Steps) != 1 {
		t.Fatalf("a partial capture must be saved, never dropped: %+v", snap)
	}
	step := snap.Definition.Steps[0]
	if step.App != PlaceholderApp || step.Match != w.Class {
		t.Errorf("step = %+v, want the placeholder with a match on the class", step)
	}
	if !step.Claims(w) {
		t.Error("the placeholder step no longer claims the live window")
	}
	if len(snap.Placeholders) != 1 {
		t.Fatalf("placeholders = %v", snap.Placeholders)
	}
	if len(snap.Notes) != 1 || snap.Notes[0] == "" {
		t.Errorf("notes = %v, want a TODO for the file", snap.Notes)
	}
	if got := snap.Definition.IncompleteSteps(); !reflect.DeepEqual(got, []int{0}) {
		t.Errorf("IncompleteSteps = %v, want [0]", got)
	}
	if problems := Problems([]Definition{snap.Definition}); len(problems) != 0 {
		t.Errorf("a placeholder entry must still validate: %v", problems)
	}
}

// TestSnapshotRecordsFloatingGeometryAndTiledSplits: floats carry their size
// and position in the placement vocabulary, tiled windows are captured tiled
// and without a proportion (master and share are hand edits — the inventory
// cannot say which window owns the layout, and a replayed share would move
// splits in the wrong order), and impossible geometry is left to the layout
// rather than written as an entry the next load rejects.
func TestSnapshotRecordsFloatingGeometryAndTiledSplits(t *testing.T) {
	windows := []desktop.Window{
		{Address: "0x1", Class: "firefox", Workspace: 1, AcceptsInput: true},
		{Address: "0x2", Class: "signal", Workspace: 1, AcceptsInput: true,
			Floating: true, X: 100, Y: 120, Width: 1200, Height: 800},
		{Address: "0x3", Class: "mpv", Workspace: 2, AcceptsInput: true,
			Floating: true, X: 90000, Y: 0, Width: 0, Height: 0},
	}
	snap := Snapshot("layout", windows, CaptureOptions{LookPath: lookPathFor("firefox", "signal", "mpv")})
	steps := snap.Definition.Steps

	if steps[0].Mode != placement.ModeTiled || steps[0].Sized() {
		t.Errorf("tiled step = %+v, want mode %q with no share", steps[0], placement.ModeTiled)
	}
	float := steps[1]
	if float.Mode != placement.ModeFloating ||
		float.Width != placement.Pixels(1200) || float.Height != placement.Pixels(800) ||
		!float.HasPosition || float.X != 100 || float.Y != 120 {
		t.Errorf("floating step = %+v, want the captured geometry", float)
	}
	offscreen := steps[2]
	if offscreen.Mode != placement.ModeFloating || offscreen.Sized() || offscreen.HasPosition {
		t.Errorf("impossible geometry step = %+v, want float with no directives", offscreen)
	}
	if problems := Problems([]Definition{snap.Definition}); len(problems) != 0 {
		t.Errorf("geometry capture must validate: %v", problems)
	}
}

// TestSnapshotOrdersDeterministically: same inventory, same steps, every
// time — workspace order first, tiled before floating within one, focus
// order after that. A capture that shuffled would make the round-trip test a
// coin toss and the written file churn on every save.
func TestSnapshotOrdersDeterministically(t *testing.T) {
	windows := []desktop.Window{
		{Address: "0x1", Class: "mpv", Workspace: 2, AcceptsInput: true, Floating: true},
		{Address: "0x2", Class: "code", Workspace: 2, AcceptsInput: true},
		{Address: "0x3", Class: "alacritty", Workspace: 1, AcceptsInput: true},
		{Address: "0x4", Class: "firefox", Workspace: 2, AcceptsInput: true},
	}
	opts := CaptureOptions{LookPath: lookPathFor("mpv", "code", "alacritty", "firefox")}
	first := Snapshot("x", windows, opts)
	second := Snapshot("x", windows, opts)
	if !reflect.DeepEqual(first, second) {
		t.Fatal("two snapshots of one inventory differ")
	}
	var order []string
	for _, s := range first.Definition.Steps {
		order = append(order, s.App)
	}
	if !reflect.DeepEqual(order, []string{"alacritty", "code", "firefox", "mpv"}) {
		t.Errorf("order = %v", order)
	}
}

// TestSnapshotWithoutLookupDerivesNothing: no resolver means every step is a
// placeholder — derivation never falls back to guessing that a class is a
// command.
func TestSnapshotWithoutLookupDerivesNothing(t *testing.T) {
	snap := Snapshot("x", []desktop.Window{
		{Address: "0x1", Class: "firefox", Workspace: 1, AcceptsInput: true},
	}, CaptureOptions{})
	if got := snap.Definition.Steps[0].App; got != PlaceholderApp {
		t.Errorf("app = %q, want the placeholder", got)
	}
}
