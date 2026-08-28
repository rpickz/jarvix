package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/monitors"
	"github.com/rpickz/jarvix/internal/placement"
)

// The screen-name tests (#180) pin the seam every consumer shares: the store
// behind placement.Resolver, the deictic assignment, the honest miss when a
// screen is unplugged, and — the one that matters most — that filling the
// resolver's one field lit nicknames up in the window tools without any
// per-tool nickname code.

func fakeScreens() []placement.Monitor {
	return []placement.Monitor{
		{Name: "HDMI-A-1", X: 840, Y: 0, Width: 3440, Height: 1440, Scale: 1,
			Reserved: [4]int{0, 26, 0, 0}, Focused: true, ActiveWorkspace: 1},
		{Name: "DP-2", X: 0, Y: 1440, Width: 5120, Height: 1440, Scale: 1,
			Reserved: [4]int{0, 26, 0, 0}, ActiveWorkspace: 2},
	}
}

// screenHarness is the window tools over a fake compositor with the user's
// two screens and a real nickname store in a temp dir. The store is real
// rather than a stub on purpose: the collision matrix and the atomic write
// are the behaviour under test everywhere else, and a stub here would let
// the seam and the store drift.
func screenHarness(t *testing.T) (*Desktop, *desktop.FakeCompositor, *monitors.Store) {
	t.Helper()
	comp := desktop.NewFakeCompositor(desktop.Window{
		Address: "0x1", Class: "firefox", Title: "GitHub", Workspace: 1,
		AcceptsInput: true, Focused: true, Width: 1600, Height: 900,
	})
	comp.Outputs = fakeScreens()
	store := monitors.NewStore(filepath.Join(t.TempDir(), "monitors.toml"), monitors.StoreOptions{
		Now: func() time.Time { return time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC) },
	}, nil)
	d := NewDesktop(DesktopOptions{Compositor: comp, InventoryTTL: time.Nanosecond, Screens: store})
	return d, comp, store
}

// TestCallThisMonitorNamesTheFocusedScreen: the deictic assignment, which is
// the only form the voice surface ever sends.
func TestCallThisMonitorNamesTheFocusedScreen(t *testing.T) {
	d, _, store := screenHarness(t)
	spoken, err := d.AssignMonitorNickname(t.Context(), "top", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(spoken, "HDMI-A-1 (3440 by 1440) is now called top") {
		t.Errorf("spoken = %q", spoken)
	}
	if connector, ok := store.Lookup("top"); !ok || connector != "HDMI-A-1" {
		t.Errorf("the store holds %q (%v)", connector, ok)
	}
	// Naming the other screen by connector works from the window's form.
	if _, err := d.AssignMonitorNickname(t.Context(), "bottom", "DP-2"); err != nil {
		t.Fatal(err)
	}
	listing, err := d.MonitorNicknameListing(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"2 screens have names", "bottom is DP-2 (5120 by 1440)",
		"top is HDMI-A-1 (3440 by 1440)"} {
		if !strings.Contains(listing, want) {
			t.Errorf("listing %q is missing %q", listing, want)
		}
	}
}

// TestOneFieldLitNicknamesUpInTheWindowTools is the seam claim made
// mechanical: nothing in the move tool knows what a nickname is, and yet
// `monitor = "top"` places the window on the screen the store points at. The
// proof is the dispatch the compositor received, not an internal call count.
func TestOneFieldLitNicknamesUpInTheWindowTools(t *testing.T) {
	d, comp, _ := screenHarness(t)
	if _, err := d.AssignMonitorNickname(t.Context(), "bottom", "DP-2"); err != nil {
		t.Fatal(err)
	}
	out := runMove(t, d, map[string]any{"window": "firefox", "monitor": "bottom"})
	if strings.Contains(strings.ToLower(out), "no monitor is called") {
		t.Fatalf("the move tool did not resolve the nickname: %s", out)
	}
	moved := monitorMoves(comp)
	if len(moved) != 1 || moved[0] != "0x1:DP-2" {
		t.Fatalf("the compositor was told %v; want the window sent to DP-2", moved)
	}
	// And the guarantee that makes nicknames safe, restated where the tools
	// use it: a request naming a real connector is never redirected, however
	// the nickname table is spelled.
	shadow := placement.Resolver{Nicknames: func(string) (string, bool) { return "DP-2", true }}
	if m, err := shadow.Resolve("HDMI-A-1", fakeScreens()); err != nil || m.Name != "HDMI-A-1" {
		t.Errorf("a present output was redirected by a nickname: %q, %v", m.Name, err)
	}
}

// monitorMoves returns the "send this window to that screen" dispatches the
// fake received, as "address:monitor".
func monitorMoves(comp *desktop.FakeCompositor) []string {
	var out []string
	for _, a := range comp.Actions() {
		if a.Verb == "window_monitor" {
			out = append(out, a.Address+":"+a.Monitor)
		}
	}
	return out
}

// TestAnUnpluggedScreenIsNamedByTheMoveTool: the window tools report the
// disappearance with that reason, so the model and the user hear what to fix.
func TestAnUnpluggedScreenIsNamedByTheMoveTool(t *testing.T) {
	d, comp, _ := screenHarness(t)
	if _, err := d.AssignMonitorNickname(t.Context(), "bottom", "DP-2"); err != nil {
		t.Fatal(err)
	}
	comp.Outputs = fakeScreens()[:1] // the dock came out

	out := runMove(t, d, map[string]any{"window": "firefox", "monitor": "bottom"})
	// The whole sentence survives the trip through PlacementSentence: what
	// the name means is the half that tells the user which cable to plug in.
	for _, want := range []string{`no monitor is called "bottom" right now`,
		"it means DP-2, which is not plugged in", "the screens plugged in are HDMI-A-1"} {
		if !strings.Contains(out, want) {
			t.Errorf("the move tool said %q, which is missing %q", out, want)
		}
	}
	if moved := monitorMoves(comp); len(moved) != 0 {
		t.Errorf("a window was moved anyway: %v", moved)
	}
}

// TestForgettingAScreenNameIsHonestAboutAMiss.
func TestForgettingAScreenNameIsHonestAboutAMiss(t *testing.T) {
	d, _, _ := screenHarness(t)
	if _, err := d.AssignMonitorNickname(t.Context(), "top", ""); err != nil {
		t.Fatal(err)
	}
	spoken, err := d.ForgetMonitorNickname(t.Context(), "top")
	if err != nil || !strings.Contains(spoken, "no longer called top") {
		t.Fatalf("Forget = %q, %v", spoken, err)
	}
	_, err = d.ForgetMonitorNickname(t.Context(), "bottom")
	if err == nil || !strings.Contains(err.Error(), `no monitor is called "bottom" right now`) {
		t.Errorf("forgetting a name nothing holds = %v", err)
	}
}

// TestTheListingCarriesScreensAndNamesForThePicker: monitors.list's data,
// including a name whose screen is absent — a nickname the user cannot see is
// a nickname they cannot correct.
func TestTheListingCarriesScreensAndNamesForThePicker(t *testing.T) {
	d, comp, _ := screenHarness(t)
	for name, connector := range map[string]string{"top": "HDMI-A-1", "bottom": "DP-2"} {
		if _, err := d.AssignMonitorNickname(t.Context(), name, connector); err != nil {
			t.Fatal(err)
		}
	}
	comp.Outputs = fakeScreens()[:1]

	screens, names, err := d.MonitorListings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(screens) != 1 || screens[0].Connector != "HDMI-A-1" || screens[0].Nickname != "top" ||
		!screens[0].Focused || screens[0].Describe != "HDMI-A-1 (3440 by 1440)" {
		t.Fatalf("screens = %+v", screens)
	}
	if len(names) != 2 {
		t.Fatalf("names = %+v", names)
	}
	for _, n := range names {
		want := n.Name == "top"
		if n.Present != want {
			t.Errorf("%s present = %v, want %v", n.Name, n.Present, want)
		}
		if n.Named == "" || n.Updated == "" {
			t.Errorf("%s carries no timestamps: %+v", n.Name, n)
		}
	}
}

// TestWithoutAStoreEverythingIsExactlyAsItWas is the pinned baseline: window
// tools built without a screen store resolve connector names and "current"
// and nothing else, byte-for-byte as they did before #180.
func TestWithoutAStoreEverythingIsExactlyAsItWas(t *testing.T) {
	comp := desktop.NewFakeCompositor(desktop.Window{
		Address: "0x1", Class: "firefox", Workspace: 1, AcceptsInput: true, Focused: true,
	})
	comp.Outputs = fakeScreens()
	d := NewDesktop(DesktopOptions{Compositor: comp, InventoryTTL: time.Nanosecond})

	if out := runMove(t, d, map[string]any{"window": "firefox", "monitor": "DP-2"}); strings.Contains(
		strings.ToLower(out), "no monitor is called") {
		t.Errorf("a connector stopped resolving: %s", out)
	}
	out := runMove(t, d, map[string]any{"window": "firefox", "monitor": "top"})
	if !strings.Contains(out, `no monitor is called "top" right now`) {
		t.Errorf("a name nothing holds said %q", out)
	}
	if d.MonitorNicknamePath() != "" {
		t.Errorf("a daemon with no store reported a path (%q)", d.MonitorNicknamePath())
	}
	if _, err := d.AssignMonitorNickname(t.Context(), "top", ""); err == nil {
		t.Error("a daemon with no store accepted an assignment")
	}
}

// runMove executes desktop.move_window and returns what the assistant is
// told — the same door a model call comes through.
func runMove(t *testing.T, d *Desktop, args map[string]any) string {
	t.Helper()
	for _, tool := range d.Tools() {
		if tool.Name() != MoveWindowToolName {
			continue
		}
		input, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		out, err := tool.Execute(context.Background(), input)
		if err != nil {
			t.Fatalf("%s returned an error rather than a sentence: %v", tool.Name(), err)
		}
		return out
	}
	t.Fatalf("no %s tool is registered", MoveWindowToolName)
	return ""
}
