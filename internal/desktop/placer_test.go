package desktop

import (
	"context"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/placement"
)

// These tests cover the half of ADR 0056 that lives in the seam: reading the
// outputs a percentage resolves against, and turning the two refusals the
// compositor can only express as a layout complaint into sentences a run can
// report. Everything here is against stubs — no test in this tree may require
// a running Hyprland — with scripts/verify-window-placement.sh as the live
// counterpart.

// TestParseMonitorsReadsWhatAPercentageNeeds: the geometry, the bars'
// reservation, and which workspace each output is showing. Disabled outputs
// are dropped, because resolving a monitor to a screen that is switched off
// would put the user's morning windows somewhere they cannot see.
func TestParseMonitorsReadsWhatAPercentageNeeds(t *testing.T) {
	const out = `[
	  {"name":"HDMI-A-1","x":840,"y":0,"width":3440,"height":1440,"scale":1,
	   "reserved":[0,26,0,0],"focused":true,"disabled":false,
	   "activeWorkspace":{"id":1,"name":"1"}},
	  {"name":"DP-2","x":0,"y":1440,"width":5120,"height":1440,"scale":1,
	   "reserved":[0,26,0,0],"focused":false,"disabled":false,
	   "activeWorkspace":{"id":2,"name":"2"}},
	  {"name":"DP-3","x":0,"y":0,"width":1920,"height":1080,"scale":1,
	   "reserved":[0,0,0,0],"focused":false,"disabled":true,
	   "activeWorkspace":{"id":0,"name":""}}
	]`
	monitors, err := parseMonitors(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(monitors) != 2 {
		t.Fatalf("monitors = %+v, want the two that are switched on", monitors)
	}
	top := monitors[0]
	if top.Name != "HDMI-A-1" || !top.Focused || top.ActiveWorkspace != 1 {
		t.Errorf("top monitor = %+v", top)
	}
	if usable := top.Usable(); usable.Y != 26 || usable.Height != 1414 || usable.Width != 3440 {
		t.Errorf("usable = %+v, want the output minus the 26-pixel bar", usable)
	}
}

// TestParseMonitorsIgnoresAReservationItCannotRead: a compositor reporting
// some other number of edges is reporting something this code does not
// understand, and guessing which edges it meant would shrink windows by the
// wrong amount on every screen.
func TestParseMonitorsIgnoresAReservationItCannotRead(t *testing.T) {
	monitors, err := parseMonitors(
		`[{"name":"DP-1","width":1920,"height":1080,"scale":1,"reserved":[0,26]}]`)
	if err != nil {
		t.Fatal(err)
	}
	if got := monitors[0].Usable(); got.Width != 1920 || got.Height != 1080 {
		t.Errorf("usable = %+v, want the whole output when the reservation is unreadable", got)
	}
}

// TestALayoutRefusalBecomesASentence is the "supported, or recorded as
// unsupported with the reason" rule where it bites hardest: preselection is a
// dwindle-family message and master promotion a master-family one, and the
// compositor's only way of saying so is "Unknown <layout> layoutmsg" — with
// exit status zero. Left alone that is a step counted as placed.
func TestALayoutRefusalBecomesASentence(t *testing.T) {
	bin := writeStub(t, "hyprctl", `#!/bin/sh
case "$2" in
  hl.dsp.no_op*) printf 'ok\n' ;;
  *layout*)      printf 'error: Unknown master layoutmsg: preselect\n' ;;
  *)             printf 'ok\n' ;;
esac
`)
	h := &Hyprland{Binary: bin}
	err := h.Preselect(context.Background(), PreselectRight)
	if err == nil {
		t.Fatal("a refused preselection was reported as done")
	}
	if !strings.Contains(err.Error(), "this workspace's layout cannot arrange windows that way") {
		t.Errorf("preselect error = %q, want the vocabulary's own words", err)
	}
	if PlacementSentence(err) == "" {
		t.Error("the refusal is not recognised as speakable, so a run would say " +
			"\"could not be arranged\" instead of why")
	}

	err = h.PromoteMaster(context.Background(), "0x1")
	if err == nil {
		t.Fatal("a refused master promotion was reported as done")
	}
	if !strings.Contains(err.Error(), placement.MasterUnsupported) {
		t.Errorf("master error = %q, want %q", err, placement.MasterUnsupported)
	}
}

// TestPlacementSentenceKeepsCompositorDiagnosticsDaemonSide: the operator's
// material stays in the log line. Anything returned to a caller here may be
// read aloud, and "hyprctl dispatch: invalid handle 0x0" is not a sentence.
func TestPlacementSentenceKeepsCompositorDiagnosticsDaemonSide(t *testing.T) {
	if got := PlacementSentence(errNoSuchThing); got != "" {
		t.Errorf("PlacementSentence(%v) = %q, want it withheld", errNoSuchThing, got)
	}
	if PlacementSentence(nil) != "" {
		t.Error("a nil error produced a sentence")
	}
}

// TestPreselectRefusesADirectionItCannotSend: the seam bounds the value
// before it is rendered, the same discipline the address and monitor patterns
// keep — nothing a caller supplies becomes dispatch syntax unchecked.
func TestPreselectRefusesADirectionItCannotSend(t *testing.T) {
	bin := writeStub(t, "hyprctl", "#!/bin/sh\nprintf 'ok\\n'\n")
	err := (&Hyprland{Binary: bin}).Preselect(context.Background(), "sideways\") --")
	if err == nil || !strings.Contains(err.Error(), "unknown direction") {
		t.Errorf("err = %v, want a refusal naming the direction", err)
	}
}

// TestMonitorNamesAreBoundedBeforeTheyBecomeSyntax: the Lua dialect wraps a
// monitor name in a string literal, so a quote would be syntax at two levels
// rather than a screen that does not exist — the spawnPattern argument,
// applied to the one other value this seam interpolates.
func TestMonitorNamesAreBoundedBeforeTheyBecomeSyntax(t *testing.T) {
	bin := writeStub(t, "hyprctl", "#!/bin/sh\nprintf 'ok\\n'\n")
	h := &Hyprland{Binary: bin}
	for _, name := range []string{`DP-2"); os.execute("rm -rf ~`, "", strings.Repeat("D", 65)} {
		if err := h.MoveWorkspaceToMonitor(context.Background(), 1, name); err == nil {
			t.Errorf("workspace move accepted monitor %q", name)
		}
		if err := h.MoveToMonitor(context.Background(), "0x1", name); err == nil {
			t.Errorf("window move accepted monitor %q", name)
		}
	}
}

// TestPlacerAppliesAModeAsAWholeState is the set-not-toggle rule at the level
// it is implemented: a floating placement leaves fullscreen and UNPINS, so a
// window the user pinned by hand yesterday does not stay pinned through a
// routine that says it floats.
func TestPlacerAppliesAModeAsAWholeState(t *testing.T) {
	comp := NewFakeCompositor(Window{Address: "0x1", Class: "firefox", Width: 800, Height: 600})
	p := Placer{Comp: comp}
	w := Window{Address: "0x1", Width: 800, Height: 600}
	err := p.Apply(context.Background(), w, placement.Placement{
		Workspace: 2, Mode: placement.ModeFloating}, placement.Monitor{})
	if err != nil {
		t.Fatal(err)
	}
	var pinned *FakeAction
	for i, a := range comp.Actions() {
		if a.Verb == "pin" {
			pinned = &comp.Actions()[i]
		}
	}
	if pinned == nil {
		t.Fatal("a floating placement sent no pin directive at all")
	}
	if pinned.Pinned {
		t.Error("a floating placement pinned the window")
	}
}

// errNoSuchThing stands in for a raw compositor diagnostic.
var errNoSuchThing = &compositorDiagnostic{"hyprctl dispatch: invalid handle 0x0"}

type compositorDiagnostic struct{ msg string }

func (e *compositorDiagnostic) Error() string { return e.msg }
