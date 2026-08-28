package placement

import (
	"strings"
	"testing"
)

// The two monitors this file measures against are the ones the vocabulary was
// designed on and the live verification script runs against, so the arithmetic
// in the tests and the arithmetic in scripts/verify-window-placement.sh are
// checking the same numbers: a 3440-wide ultrawide above a 5120-wide one, both
// with a 26-pixel bar reserved at the top.
func topMonitor() Monitor {
	return Monitor{Name: "HDMI-A-1", X: 840, Y: 0, Width: 3440, Height: 1440,
		Scale: 1, Reserved: [4]int{0, 26, 0, 0}, Focused: true, ActiveWorkspace: 1}
}

func bottomMonitor() Monitor {
	return Monitor{Name: "DP-2", X: 0, Y: 1440, Width: 5120, Height: 1440,
		Scale: 1, Reserved: [4]int{0, 26, 0, 0}, ActiveWorkspace: 2}
}

// TestUsableAreaExcludesWhatTheBarsTook: a percentage means a share of the
// part of the screen a window can occupy, not of the output — a window sized
// against the whole output overhangs the bar by exactly the bar's height on
// every monitor the user owns.
func TestUsableAreaExcludesWhatTheBarsTook(t *testing.T) {
	usable := topMonitor().Usable()
	if usable.X != 840 || usable.Y != 26 || usable.Width != 3440 || usable.Height != 1414 {
		t.Errorf("usable = %+v, want the output minus the 26-pixel top bar", usable)
	}
}

// TestUsableAreaDividesByScale: every coordinate in a dispatch is logical, so
// a scaled output's usable area is its mode divided by the scale. Reasoned
// from Hyprland's coordinate model rather than probed (both monitors on the
// machine this was written against run unscaled), which is why the live
// script prints both numbers.
func TestUsableAreaDividesByScale(t *testing.T) {
	m := Monitor{Width: 3840, Height: 2160, Scale: 2, Reserved: [4]int{0, 26, 0, 0}}
	if usable := m.Usable(); usable.Width != 1920 || usable.Height != 1054 {
		t.Errorf("usable = %+v, want the logical size minus the bar", usable)
	}
}

// TestExtentAcceptsPercentAndPixels: both forms parse, round-trip through the
// spelling written into config.toml, and resolve against a real screen.
func TestExtentAcceptsPercentAndPixels(t *testing.T) {
	tests := []struct {
		in       string
		want     Extent
		resolved int
	}{
		{"66%", Percent(66), 2270},   // 66% of the usable 3440
		{"100%", Percent(100), 3440}, // the whole usable width
		{"50%", Percent(50), 1720},
		{"1200px", Pixels(1200), 1200},
		{"1200", Pixels(1200), 1200}, // the bare form every existing routine's size used
		{" 66 % ", Percent(66), 2270},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseExtent(tt.in)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("parsed %q as %+v, want %+v", tt.in, got, tt.want)
			}
			pixels, err := got.Resolve(topMonitor().Usable().Width)
			if err != nil || pixels != tt.resolved {
				t.Errorf("%q resolved to %d (err %v), want %d", tt.in, pixels, err, tt.resolved)
			}
		})
	}
}

// TestExtentRefusesWhatCannotBeHonoured: every one of these is a load-time
// and form-time answer, not an eight-second silence at run time.
func TestExtentRefusesWhatCannotBeHonoured(t *testing.T) {
	tests := []struct{ in, want string }{
		{"150%", "more than the whole screen"},
		{"0%", "bigger than nothing"},
		{"-40px", "bigger than nothing"},
		{"99999px", "not a window size in pixels"},
		{"two thirds", "write a percentage of the screen"},
		{"66 percent", "write a percentage of the screen"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			_, err := ParseExtent(tt.in)
			if err == nil {
				t.Fatalf("%q was accepted", tt.in)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
		})
	}
}

// TestPixelSizeIsCheckedAgainstTheScreenItLandsOn: a size that fits nowhere
// is named with the number, because a clamp would place the window and lie
// about it.
func TestPixelSizeIsCheckedAgainstTheScreenItLandsOn(t *testing.T) {
	small := Monitor{Name: "eDP-1", Width: 1024, Height: 768, Scale: 1}
	p := Placement{Mode: ModeFloating, Width: Pixels(1200), Height: Pixels(600), Workspace: 1}
	_, problems := p.ResolveSize(small)
	if len(problems) != 1 || problems[0].Field != FieldWidth {
		t.Fatalf("problems = %+v, want one on the width field", problems)
	}
	if !strings.Contains(problems[0].Message, "1200px is wider than the screen's usable 1024 pixels") {
		t.Errorf("message = %q", problems[0].Message)
	}
	// The same size on a screen that can hold it is not a problem.
	if _, problems := p.ResolveSize(topMonitor()); len(problems) != 0 {
		t.Errorf("problems on a big screen = %+v", problems)
	}
}

// TestPixelSizeSurvivesAnUnknownScreen: a compositor that will not list its
// outputs must not turn "1200 by 800" into a refusal. There is nothing to
// check the number against, so it goes through unchecked; a PERCENTAGE, which
// genuinely cannot be worked out without a screen, does not.
func TestPixelSizeSurvivesAnUnknownScreen(t *testing.T) {
	if got, err := Pixels(1200).Resolve(0); err != nil || got != 1200 {
		t.Errorf("pixels against an unknown screen = %d, %v", got, err)
	}
	if _, err := Percent(66).Resolve(0); err == nil {
		t.Error("a percentage of an unknown screen was accepted")
	}
}

// TestModeProblemsAreKeyedToTheirField: every message names the form control
// where the fix happens, because the daemon's field-keyed classifier reads the
// leading token of the message to place it (entry_admin.go).
func TestModeProblemsAreKeyedToTheirField(t *testing.T) {
	tests := []struct {
		name  string
		p     Placement
		field string
		want  string
	}{
		{"share on a mode with no size",
			Placement{Workspace: 1, Mode: ModeMaximised, Width: Percent(50)},
			FieldWidth, "means nothing in maximised mode"},
		{"share with no mode at all",
			Placement{Workspace: 1, Width: Percent(50)},
			FieldWidth, "width needs a mode"},
		{"position on a tiled window",
			Placement{Workspace: 1, Mode: ModeTiled, HasPosition: true},
			FieldPosition, "the layout owns where a tiled window sits"},
		{"arrangement on a floating window",
			Placement{Workspace: 1, Mode: ModeFloating, PlaceNext: PlaceNextRight},
			FieldPlaceNext, "place_next arranges tiled windows"},
		{"master on a floating window",
			Placement{Workspace: 1, Mode: ModeFloating, Master: true},
			FieldMaster, "master promotes a tiled window"},
		{"a monitor name nothing could be called",
			Placement{Workspace: 1, Monitor: "the big one!"},
			FieldMonitor, "is not a screen name"},
		{"a workspace that does not exist",
			Placement{Workspace: 100},
			FieldWorkspace, "workspaces are numbered 1 to 99"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			problems := tt.p.Problems(true)
			if len(problems) != 1 {
				t.Fatalf("problems = %+v, want exactly one", problems)
			}
			if problems[0].Field != tt.field {
				t.Errorf("field = %q, want %q", problems[0].Field, tt.field)
			}
			if !strings.Contains(problems[0].Message, tt.want) {
				t.Errorf("message %q does not contain %q", problems[0].Message, tt.want)
			}
			// String() leads with the field, which is what the daemon's
			// classifier keys on — a message that leads with anything else
			// lands in the form's general area instead of on the control.
			if !strings.HasPrefix(problems[0].String(), tt.field) {
				t.Errorf("String() = %q, want it to lead with %q", problems[0].String(), tt.field)
			}
		})
	}
}

// TestWorkspaceIsRequiredOfARoutineAndNotOfATool: a routine step describes a
// desktop, so "wherever" is not a description; a tool call places the window
// the user is looking at, so leaving it where it is legitimate.
func TestWorkspaceIsRequiredOfARoutineAndNotOfATool(t *testing.T) {
	p := Placement{Mode: ModeFloating}
	if problems := p.Problems(true); len(problems) != 1 || problems[0].Field != FieldWorkspace {
		t.Errorf("routine problems = %+v, want one on the workspace", problems)
	}
	if problems := p.Problems(false); len(problems) != 0 {
		t.Errorf("tool problems = %+v, want none", problems)
	}
}

// TestUnsupportedModesExplainThemselves: a state the compositor has and this
// vocabulary declines comes back with the reason, not with "unknown". A bare
// refusal is what makes someone ask for the same thing again next month.
func TestUnsupportedModesExplainThemselves(t *testing.T) {
	if len(UnsupportedModes()) == 0 {
		t.Fatal("nothing is recorded as unsupported, which cannot be true")
	}
	for _, u := range UnsupportedModes() {
		if strings.TrimSpace(u.Reason) == "" {
			t.Errorf("%q is declined with no reason", u.Name)
		}
		_, err := ParseMode(u.Name)
		if err == nil {
			t.Fatalf("%q parsed as a mode", u.Name)
		}
		if !strings.Contains(err.Error(), u.Reason) {
			t.Errorf("refusing %q said %q, which does not carry its recorded reason", u.Name, err)
		}
	}
	// An unknown value that is not on the list gets the list of what is.
	_, err := ParseMode("sideways")
	if err == nil || !strings.Contains(err.Error(), strings.Join(ModeNames(), ", ")) {
		t.Errorf("unknown mode error = %v", err)
	}
}

// TestModeSpellingsAmericanAndSuperseded: Hyprland's own documentation says
// "maximized" and the superseded routine schema said "split"; a user typing
// either should not be told it does not exist.
func TestModeSpellingsAmericanAndSuperseded(t *testing.T) {
	for in, want := range map[string]Mode{
		"maximized": ModeMaximised, "maximised": ModeMaximised,
		"float": ModeFloating, "split": ModeTiled, "tile": ModeTiled,
		"TILED": ModeTiled, " floating ": ModeFloating,
	} {
		got, err := ParseMode(in)
		if err != nil || got != want {
			t.Errorf("ParseMode(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

// TestPlaceNextAcceptsHowPeopleSayIt: "down" and "below" are the same
// direction, and refusing one of them teaches nothing.
func TestPlaceNextAcceptsHowPeopleSayIt(t *testing.T) {
	for in, want := range map[string]PlaceNext{
		"right": PlaceNextRight, "left": PlaceNextLeft,
		"below": PlaceNextBelow, "down": PlaceNextBelow, "beneath": PlaceNextBelow,
		"above": PlaceNextAbove, "up": PlaceNextAbove, "": PlaceNextNone,
	} {
		got, err := ParsePlaceNext(in)
		if err != nil || got != want {
			t.Errorf("ParsePlaceNext(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParsePlaceNext("diagonally"); err == nil {
		t.Error("diagonally was accepted as a direction")
	}
}
