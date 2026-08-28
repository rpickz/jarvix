package config

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/placement"
	"github.com/rpickz/jarvix/internal/routine"
)

// parseValid parses a document and requires it to validate.
func parseValid(t *testing.T, doc string) Config {
	t.Helper()
	cfg, err := parse([]byte(doc), Default())
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

const morningSetupTOML = `
[[routines]]
name = "morning setup"
phrases = ["morning setup", "start my usual apps"]

  [[routines.steps]]
  app = "alacritty"
  workspace = 1

  [[routines.steps]]
  app = "firefox"
  workspace = 2
  tile = "master"

  [[routines.steps]]
  app = "code"
  workspace = 2
  tile = "split"

  [[routines.steps]]
  app = "signal-desktop"
  match = "signal"
  workspace = 9
  float = true
  size = [1200, 800]
  position = [100, 100]
`

// TestRoutinesParseAndConvert: the worked morning-setup example — the
// documented shape — parses, validates, and converts field for field.
func TestRoutinesParseAndConvert(t *testing.T) {
	cfg := parseValid(t, morningSetupTOML)
	defs := cfg.RoutineDefinitions()
	if len(defs) != 1 || len(defs[0].Steps) != 4 {
		t.Fatalf("defs = %+v", defs)
	}
	def := defs[0]
	if def.Name != "morning setup" || len(def.Phrases) != 2 {
		t.Errorf("def = %+v", def)
	}
	// The superseded spellings translate into the vocabulary rather than
	// being refused: `tile = "master"` is a tiled window promoted to the
	// master pane, and `float = true` with a pixel `size` is a floating
	// window sized in pixels (ADR 0056).
	if s := def.Steps[1]; s.App != "firefox" || s.Workspace != 2 ||
		s.Mode != placement.ModeTiled || !s.Master {
		t.Errorf("step 2 = %+v", s)
	}
	floaty := def.Steps[3]
	if floaty.App != "signal-desktop" || floaty.Match != "signal" ||
		floaty.Mode != placement.ModeFloating {
		t.Errorf("step 4 = %+v", floaty)
	}
	if floaty.Width != placement.Pixels(1200) || floaty.Height != placement.Pixels(800) ||
		!floaty.HasPosition || floaty.X != 100 || floaty.Y != 100 {
		t.Errorf("step 4 geometry = %+v", floaty)
	}
	// The router knows the phrases.
	opts := cfg.IntentOptions()
	if len(opts.Routines) != 1 || opts.Routines[0].Name != "morning setup" {
		t.Errorf("router options = %+v", opts.Routines)
	}
}

// TestRoutineValidationSpeaksTheFileLanguage: the problems a user can write
// come back naming the table to fix, through Config.Validate like every
// other configuration mistake.
func TestRoutineValidationSpeaksTheFileLanguage(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want string
	}{
		{"phrase collides with a built-in", `
[[routines]]
name = "quiet"
phrases = ["mute"]
[[routines.steps]]
app = "firefox"
workspace = 1
`, `already the built-in intent "volume.mute"`},
		{"shell-shaped app", `
[[routines]]
name = "bad"
phrases = ["bad routine"]
[[routines.steps]]
app = "firefox; rm -rf ~"
workspace = 1
`, "never through a shell"},
		{"three-element size", `
[[routines]]
name = "bad"
phrases = ["bad routine"]
[[routines.steps]]
app = "firefox"
workspace = 1
float = true
size = [1, 2, 3]
`, "write it as [width, height]"},
		{"a share bigger than the screen", `
[[routines]]
name = "bad"
phrases = ["bad routine"]
[[routines.steps]]
app = "firefox"
workspace = 1
mode = "tiled"
width = "150%"
`, "more than the whole screen"},
		{"a mode nobody has", `
[[routines]]
name = "bad"
phrases = ["bad routine"]
[[routines.steps]]
app = "firefox"
workspace = 1
mode = "stacked"
`, "is not a placement mode"},
		{"a mode the compositor cannot deliver as a set", `
[[routines]]
name = "bad"
phrases = ["bad routine"]
[[routines.steps]]
app = "firefox"
workspace = 1
mode = "grouped"
`, "which only toggles"},
		{"one directive spelled two ways", `
[[routines]]
name = "bad"
phrases = ["bad routine"]
[[routines.steps]]
app = "firefox"
workspace = 1
mode = "tiled"
float = true
`, "the superseded spelling, so delete it"},
		{"intents disabled", `
[intents]
enabled = false
[[routines]]
name = "morning setup"
phrases = ["morning setup"]
[[routines.steps]]
app = "firefox"
workspace = 1
`, "intents.enabled is false"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := parse([]byte(tt.doc), Default())
			if err != nil {
				t.Fatal(err)
			}
			err = cfg.Validate()
			if err == nil {
				t.Fatal("validated despite the problem")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
		})
	}
}

// TestRoutinesSurviveASettingsRewrite: [[routines]] is hand-edited TOML
// outside the settings registry, so a config.set of an ordinary key must
// leave the tables byte-for-byte intact — the rewrite is surgical (ADR 0015).
func TestRoutinesSurviveASettingsRewrite(t *testing.T) {
	doc := "ai_model_placeholder = false\n" + morningSetupTOML
	setting, ok := SettingFor("conversation.speak_responses")
	if !ok {
		t.Fatal("setting missing")
	}
	rewritten, err := RewriteTOML([]byte(doc), map[string]any{setting.Key: false})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rewritten), `phrases = ["morning setup", "start my usual apps"]`) {
		t.Error("the rewrite lost the routine tables")
	}
	if !strings.Contains(string(rewritten), "size = [1200, 800]") {
		t.Error("the rewrite lost the step geometry")
	}
	cfg, err := ParseBytes(rewritten)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Routines) != 1 || len(cfg.Routines[0].Steps) != 4 {
		t.Errorf("routines after rewrite = %+v", cfg.Routines)
	}
}

// TestLaunchingKeysRoundTripThroughTheFile is issue #175's schema, read and
// written: the four new keys parse, convert into the runner's own Step, and
// come back out of the renderer as the same TOML.
//
// The round trip matters more than the parse. The window edits one step of a
// routine and saves the whole entry, so a key the conversion loses is a key
// the window deletes the first time anyone renames a step — which is how a
// user's profile argument would disappear without anyone touching it.
func TestLaunchingKeysRoundTripThroughTheFile(t *testing.T) {
	const doc = `
[[routines]]
name = "morning setup"
phrases = ["morning setup"]

  [[routines.steps]]
  app = "chromium"
  args = ["--profile-directory=Profile 3", "--restore-last-session"]
  identity = "personal-browser"
  launch = "always"
  workspace = 1

  [[routines.steps]]
  desktop_entry = "ChatGPT.desktop"
  match = "chrome-chatgpt.com"
  workspace = 2
`
	cfg, err := ParseBytes([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	steps := cfg.Routines[0].Steps
	if steps[0].App != "chromium" || steps[0].Identity != "personal-browser" ||
		steps[0].Launch != "always" ||
		!slices.Equal(steps[0].Args, []string{"--profile-directory=Profile 3", "--restore-last-session"}) {
		t.Fatalf("step one parsed as %+v", steps[0])
	}
	if steps[1].DesktopEntry != "ChatGPT.desktop" || steps[1].Match != "chrome-chatgpt.com" {
		t.Fatalf("step two parsed as %+v", steps[1])
	}

	// Into the runner's own shape and back, which is the conversion the
	// window's save runs through.
	defs := cfg.RoutineDefinitions()
	if got := defs[0].Steps[0]; got.Launch != routine.LaunchAlways ||
		got.Identity != "personal-browser" ||
		!slices.Equal(got.Args, []string{"--profile-directory=Profile 3", "--restore-last-session"}) {
		t.Fatalf("converted step one = %+v", got)
	}
	if got := defs[0].Steps[1]; got.DesktopEntry != "ChatGPT.desktop" {
		t.Fatalf("converted step two = %+v", got)
	}
	back := RoutineFromDefinition(defs[0])
	if !reflect.DeepEqual(back.Steps, cfg.Routines[0].Steps) {
		t.Errorf("round trip changed the steps:\n got %+v\nwant %+v", back.Steps, cfg.Routines[0].Steps)
	}

	// And the renderer writes exactly those keys back into the file.
	written, err := UpsertRoutineTOML([]byte(doc), back, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`args = ["--profile-directory=Profile 3", "--restore-last-session"]`,
		`identity = "personal-browser"`,
		`launch = "always"`,
		`desktop_entry = "ChatGPT.desktop"`,
	} {
		if !strings.Contains(string(written), want) {
			t.Errorf("the rewritten document does not carry %s:\n%s", want, written)
		}
	}
	// The default policy is written as absence, never as the word: a step
	// that never asked for anything comes back looking exactly as it went in.
	if strings.Contains(string(written), `launch = "if_missing"`) {
		t.Error("the renderer wrote the default launch policy into a step that never named it")
	}
}
