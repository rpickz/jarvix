package routine

import (
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/placement"
)

// valid returns a well-formed definition tests mutate into invalidity.
func valid() Definition {
	return Definition{
		Name:    "morning setup",
		Phrases: []string{"morning setup"},
		Steps:   []Step{{App: "firefox", Placement: placement.Placement{Workspace: 2}}},
	}
}

// TestProblemsNamesEveryOffender: each structural rule fails with a message
// that names the routine and step to fix — validation is only worth having if
// the message says where to look.
func TestProblemsNamesEveryOffender(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Definition)
		want   string
	}{
		{"empty name", func(d *Definition) { d.Name = " " }, "routines[0]: name is empty"},
		{"no phrases", func(d *Definition) { d.Phrases = nil }, "it has no phrases"},
		{"no steps", func(d *Definition) { d.Steps = nil }, "it has no steps"},
		{"empty app", func(d *Definition) { d.Steps[0].App = "" }, "steps[0]: app is empty"},
		{"shell-shaped app", func(d *Definition) { d.Steps[0].App = "firefox; rm -rf ~" },
			"never through a shell"},
		{"workspace zero", func(d *Definition) { d.Steps[0].Workspace = 0 }, "workspace 0 does not exist"},
		{"workspace huge", func(d *Definition) { d.Steps[0].Workspace = 100 }, "workspace 100 does not exist"},
		{"size without a mode", func(d *Definition) {
			d.Steps[0].Width, d.Steps[0].Height = placement.Pixels(800), placement.Pixels(600)
		}, "width needs a mode"},
		{"share over the whole screen", func(d *Definition) {
			d.Steps[0].Mode = placement.ModeTiled
			d.Steps[0].Width = placement.Percent(150)
		}, "more than the whole screen"},
		{"size on a mode that has none", func(d *Definition) {
			d.Steps[0].Mode = placement.ModeFullscreen
			d.Steps[0].Width = placement.Percent(50)
		}, "means nothing in fullscreen mode"},
		{"negative size", func(d *Definition) {
			d.Steps[0].Mode = placement.ModeFloating
			d.Steps[0].Width = placement.Pixels(-1)
		}, "bigger than nothing"},
		{"position without a mode", func(d *Definition) { d.Steps[0].HasPosition = true },
			"position needs a mode"},
		{"position on a tiled window", func(d *Definition) {
			d.Steps[0].Mode, d.Steps[0].HasPosition = placement.ModeTiled, true
		}, "the layout owns where a tiled window sits"},
		{"position off the map", func(d *Definition) {
			d.Steps[0].Mode, d.Steps[0].HasPosition = placement.ModeFloating, true
			d.Steps[0].X = 32769
		}, "not on any plausible monitor"},
		{"unknown mode", func(d *Definition) { d.Steps[0].Mode = "stacked" },
			`"stacked" is not a placement mode`},
		{"arranging a floating window", func(d *Definition) {
			d.Steps[0].Mode, d.Steps[0].PlaceNext = placement.ModeFloating, placement.PlaceNextRight
		}, "place_next arranges tiled windows"},
		{"master on a floating window", func(d *Definition) {
			d.Steps[0].Mode, d.Steps[0].Master = placement.ModeFloating, true
		}, "master promotes a tiled window"},
		{"monitor that is not a name", func(d *Definition) { d.Steps[0].Monitor = "my screen!" },
			"is not a screen name"},
		{"unknown focus", func(d *Definition) { d.Steps[0].Focus = "sometimes" },
			"is not a focus choice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			def := valid()
			tt.mutate(&def)
			problems := Problems([]Definition{def})
			if len(problems) == 0 {
				t.Fatal("no problem reported")
			}
			joined := strings.Join(problems, "\n")
			if !strings.Contains(joined, tt.want) {
				t.Errorf("problems %q do not contain %q", joined, tt.want)
			}
			if !strings.Contains(joined, "routines[0]") {
				t.Errorf("problems %q do not name the routine", joined)
			}
		})
	}
}

func TestProblemsAcceptsTheWorkedExample(t *testing.T) {
	defs := []Definition{{
		Name:    "morning setup",
		Phrases: []string{"morning setup", "start my usual apps"},
		Steps: []Step{
			{App: "alacritty", Placement: placement.Placement{Workspace: 1}},
			{App: "firefox", Placement: placement.Placement{
				Workspace: 2, Mode: placement.ModeTiled, Master: true,
				Width: placement.Percent(66), PlaceNext: placement.PlaceNextRight}},
			{App: "code", Placement: placement.Placement{
				Workspace: 2, Mode: placement.ModeTiled, Monitor: "DP-2"}},
			{App: "signal", Placement: placement.Placement{
				Workspace: 9, Mode: placement.ModeFloating,
				Width: placement.Pixels(1200), Height: placement.Pixels(800),
				X: 100, Y: 100, HasPosition: true, Focus: placement.FocusFollow}},
		},
	}}
	if problems := Problems(defs); len(problems) != 0 {
		t.Errorf("the worked example is rejected: %v", problems)
	}
}

// TestProblemsCatchesAnArrangementWithNothingAfterIt: a preselection is
// one-shot, so a routine that sets one and opens nothing else on that
// workspace leaves it for whatever the user opens by hand ten minutes later —
// which they would experience as a window landing somewhere strange, with
// nothing on screen connecting it to the routine.
func TestProblemsCatchesAnArrangementWithNothingAfterIt(t *testing.T) {
	def := valid()
	def.Steps[0].Mode = placement.ModeTiled
	def.Steps[0].PlaceNext = placement.PlaceNextRight
	problems := Problems([]Definition{def})
	if len(problems) != 1 || !strings.Contains(problems[0], "has no step after it on workspace 2") {
		t.Fatalf("problems = %v", problems)
	}

	// A step on a DIFFERENT workspace does not count as following it: the
	// preselection belongs to the workspace it was set on.
	def.Steps = append(def.Steps, Step{App: "code",
		Placement: placement.Placement{Workspace: 3, Mode: placement.ModeTiled}})
	if problems := Problems([]Definition{def}); len(problems) != 1 {
		t.Errorf("a step on another workspace satisfied the arrangement: %v", problems)
	}

	// One on the same workspace does.
	def.Steps = append(def.Steps, Step{App: "alacritty",
		Placement: placement.Placement{Workspace: 2, Mode: placement.ModeTiled}})
	if problems := Problems([]Definition{def}); len(problems) != 0 {
		t.Errorf("problems = %v, want none once the arrangement is followed", problems)
	}
}

func TestProblemsRejectsDuplicateNames(t *testing.T) {
	a, b := valid(), valid()
	b.Name = "Morning Setup" // case-insensitive duplicate
	b.Phrases = []string{"another phrase"}
	problems := Problems([]Definition{a, b})
	if len(problems) != 1 || !strings.Contains(problems[0], "must be unique") {
		t.Errorf("problems = %v", problems)
	}
}
