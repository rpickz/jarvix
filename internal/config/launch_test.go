package config

import (
	"strings"
	"testing"
)

// The launch overrides (#194). They exist because the classification behind
// "Claude runs in a terminal, Firefox opens a window" is an inference, and an
// inference is exactly the thing the person who owns the machine must be able
// to correct.

// Empty on a machine nobody has corrected, and empty is the working state:
// the feature must not need configuration to work, or it is not a default.
func TestTheLaunchOverridesAreEmptyByDefault(t *testing.T) {
	launch := Default().Tools.Launch
	if len(launch.TerminalPrograms) != 0 || len(launch.GraphicalPrograms) != 0 {
		t.Errorf("[tools.launch] = %+v, want nothing until the user says otherwise", launch)
	}
}

func TestTheLaunchOverridesAreRead(t *testing.T) {
	cfg, err := ParseBytes([]byte("[tools.launch]\n" +
		"terminal_programs = [\"claude\", \"opencode\"]\n" +
		"graphical_programs = [\"obsidian\"]\n"))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	if len(cfg.Tools.Launch.TerminalPrograms) != 2 ||
		cfg.Tools.Launch.TerminalPrograms[0] != "claude" {
		t.Errorf("terminal_programs = %v", cfg.Tools.Launch.TerminalPrograms)
	}
	if len(cfg.Tools.Launch.GraphicalPrograms) != 1 ||
		cfg.Tools.Launch.GraphicalPrograms[0] != "obsidian" {
		t.Errorf("graphical_programs = %v", cfg.Tools.Launch.GraphicalPrograms)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("a well-formed override list was refused: %v", err)
	}
}

// The contradiction is the one substantive check: a name in both lists is not
// a preference expressed twice, it is two incompatible instructions, and
// resolving it silently would mean choosing one of the user's own sentences
// to ignore.
func TestTheLaunchOverridesRefuseAContradiction(t *testing.T) {
	cfg := Default()
	cfg.Tools.Launch.TerminalPrograms = []string{"claude"}
	cfg.Tools.Launch.GraphicalPrograms = []string{"Claude"}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("a program named in both lists was accepted")
	}
	if !strings.Contains(err.Error(), "both") || !strings.Contains(err.Error(), "one list only") {
		t.Errorf("problem = %q, want it to say what is wrong and what to do", err)
	}
}

// An override names a program, so it is bounded by what a program name may
// be. A path or a command line here would be a value the launcher can never
// match against anything, silently.
func TestTheLaunchOverridesRefuseAnythingThatIsNotAProgramName(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"", "empty entry"},
		{"   ", "empty entry"},
		{"/usr/bin/claude", "not a path"},
		{"claude --resume", "not a path"},
	} {
		cfg := Default()
		cfg.Tools.Launch.TerminalPrograms = []string{tc.name}
		err := cfg.Validate()
		if err == nil {
			t.Errorf("terminal_programs = %q was accepted", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), "tools.launch.terminal_programs") ||
			!strings.Contains(err.Error(), tc.want) {
			t.Errorf("problem for %q = %q, want it to name the key and the fix", tc.name, err)
		}
	}
}

// Editable in the window like every other family (ADR 0054): a correction
// about your own computer is exactly what the settings screen is for, and the
// registry is what puts it there without anyone editing QML.
func TestTheLaunchOverridesAreEditableSettings(t *testing.T) {
	for _, key := range []string{"tools.launch.terminal_programs", "tools.launch.graphical_programs"} {
		setting, ok := SettingFor(key)
		if !ok {
			t.Fatalf("%s is not in the settings registry", key)
		}
		if setting.Type != TypeStringList {
			t.Errorf("%s is a %s, want a list of program names", key, setting.Type)
		}
		if !setting.Dangerous {
			t.Errorf("%s must always confirm: it decides what actually runs", key)
		}
		cfg := Default()
		if err := setting.Apply(&cfg, []string{"claude"}); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if got := setting.Get(cfg).([]string); len(got) != 1 || got[0] != "claude" {
			t.Errorf("%s round trip = %v", key, got)
		}
	}
}
