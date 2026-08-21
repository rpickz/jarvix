package routine

import (
	"strings"
	"testing"
)

// valid returns a well-formed definition tests mutate into invalidity.
func valid() Definition {
	return Definition{
		Name:    "morning setup",
		Phrases: []string{"morning setup"},
		Steps:   []Step{{App: "firefox", Workspace: 2}},
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
		{"size without float", func(d *Definition) { d.Steps[0].Width, d.Steps[0].Height = 800, 600 },
			"size needs float = true"},
		{"negative size", func(d *Definition) {
			d.Steps[0].Float = true
			d.Steps[0].Width, d.Steps[0].Height = -1, 600
		}, "not a window size"},
		{"position without float", func(d *Definition) { d.Steps[0].HasPosition = true },
			"position needs float = true"},
		{"position off the map", func(d *Definition) {
			d.Steps[0].Float, d.Steps[0].HasPosition = true, true
			d.Steps[0].X = maxPixel + 1
		}, "not on any plausible monitor"},
		{"unknown tile", func(d *Definition) { d.Steps[0].Tile = "stacked" }, `tile "stacked" is not an arrangement`},
		{"float and tile", func(d *Definition) { d.Steps[0].Float, d.Steps[0].Tile = true, TileSplit },
			"mutually exclusive"},
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
			{App: "alacritty", Workspace: 1},
			{App: "firefox", Workspace: 2, Tile: TileMaster},
			{App: "code", Workspace: 2, Tile: TileSplit},
			{App: "signal", Workspace: 9, Float: true, Width: 1200, Height: 800, X: 100, Y: 100, HasPosition: true},
		},
	}}
	if problems := Problems(defs); len(problems) != 0 {
		t.Errorf("the worked example is rejected: %v", problems)
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
