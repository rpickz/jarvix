package launchkind

import (
	"slices"
	"strings"
	"testing"
)

// The per-terminal table (ADR 0061). A wrong spelling here is not a near
// miss: it is an argument the terminal rejects at start-up, so the user asks
// for Claude and gets a window that flashes an unknown-option error and
// exits. These tests pin what the table says against what each terminal's own
// documentation says, and pin that an unfamiliar terminal is refused by name
// rather than guessed at.

func TestTheTerminalTableSpellsEachTerminalTheWayItsOwnManualDoes(t *testing.T) {
	for _, tc := range []struct {
		terminal string
		want     []string
		why      string
	}{
		// ghostty(1): "Ghostty supports the common -e flag"; --class must be
		// a GTK application id, so the class is namespaced rather than bare.
		{"ghostty", []string{"/usr/bin/ghostty", "--class=dev.jarvix.claude", "-e", "claude"},
			"ghostty(1) on the machine this was written for"},
		// alacritty(1): -e must be last, --class takes the general class.
		{"alacritty", []string{"/usr/bin/alacritty", "--class=claude", "-e", "claude"},
			"alacritty(1)"},
		// kitty(1): "kitty [options] [program-to-run ...]" — positional.
		{"kitty", []string{"/usr/bin/kitty", "--class=claude", "claude"}, "kitty(1)"},
		// foot(1): trailing non-option arguments are the command; --app-id.
		{"foot", []string{"/usr/bin/foot", "--app-id=claude", "claude"}, "foot(1)"},
		// wezterm: a sub-command, then --class, then -- before the program.
		{"wezterm", []string{"/usr/bin/wezterm", "start", "--class=claude", "--", "claude"},
			"wezterm start"},
		// gnome-terminal(1) recommends -- over the deprecated -e, and its
		// --class is the legacy X11 toolkit option, so no identity is claimed.
		{"gnome-terminal", []string{"/usr/bin/gnome-terminal", "--", "claude"},
			"gnome-terminal(1)"},
		// konsole publishes -e and no window-class option.
		{"konsole", []string{"/usr/bin/konsole", "-e", "claude"}, "konsole(1)"},
		// xterm, the original -e, and no identity: its -class takes a separate
		// argument, a shape the table deliberately does not model.
		{"xterm", []string{"/usr/bin/xterm", "-e", "claude"}, "xterm(1)"},
	} {
		t.Run(tc.terminal, func(t *testing.T) {
			spelling, err := LookupTerminal(tc.terminal)
			if err != nil {
				t.Fatalf("%s is not in the table: %v", tc.terminal, err)
			}
			got := spelling.Wrap("/usr/bin/"+tc.terminal,
				spelling.Identity("claude"), []string{"claude"})
			if !slices.Equal(got, tc.want) {
				t.Errorf("argv = %v, want %v (%s)", got, tc.want, tc.why)
			}
		})
	}
}

// An absolute path in [intents] terminal is a spelling the setting accepts,
// so the table has to key on the base name or the whole feature stops for a
// user who wrote one.
func TestAPathToATerminalIsStillThatTerminal(t *testing.T) {
	spelling, err := LookupTerminal("/usr/local/bin/ghostty")
	if err != nil {
		t.Fatalf("a path to a known terminal was refused: %v", err)
	}
	if !slices.Equal(spelling.Command, []string{"-e"}) {
		t.Errorf("command = %v, want ghostty's row", spelling.Command)
	}
	if got := TerminalName("/usr/local/bin/Ghostty"); got != "ghostty" {
		t.Errorf("TerminalName = %q, want the lower-cased base name", got)
	}
}

// An unknown terminal is refused with the reason and the alternatives.
// Guessing -e at it would turn "I do not know your terminal" into a launch
// that fails for a reason the user never wrote.
func TestAnUnknownTerminalIsRefusedByName(t *testing.T) {
	_, err := LookupTerminal("st")
	if err == nil {
		t.Fatal("an unknown terminal was accepted")
	}
	var unknown *UnknownTerminalError
	if !asUnknown(err, &unknown) {
		t.Fatalf("error = %T, want an UnknownTerminalError the caller can recognise", err)
	}
	if unknown.Name != "st" {
		t.Errorf("name = %q, want the terminal that was configured", unknown.Name)
	}
	for _, want := range []string{"do not know how to run a command inside st", "ghostty", "kitty"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %q, missing %q", err.Error(), want)
		}
	}
}

// asUnknown is errors.As without the import, kept local so the assertion
// reads as what it is checking.
func asUnknown(err error, target **UnknownTerminalError) bool {
	u, ok := err.(*UnknownTerminalError)
	if ok {
		*target = u
	}
	return ok
}

// The identity is what makes a launched terminal window findable afterwards,
// so its shape is the terminal's rule, not ours: ghostty validates its class
// as a GTK application id and would refuse to start on a bare word.
func TestTheIdentityTakesTheFormTheTerminalDemands(t *testing.T) {
	ghostty, _ := LookupTerminal("ghostty")
	if got := ghostty.Identity("claude"); got != "dev.jarvix.claude" {
		t.Errorf("ghostty identity = %q, want a GTK application id", got)
	}
	if got := ghostty.Identity("google-chrome"); !strings.HasPrefix(got, "dev.jarvix.") ||
		strings.ContainsAny(got, " +") {
		t.Errorf("ghostty identity = %q, want a valid application id", got)
	}
	foot, _ := LookupTerminal("foot")
	if got := foot.Identity("claude"); got != "claude" {
		t.Errorf("foot identity = %q, want the free-form token it accepts", got)
	}
	konsole, _ := LookupTerminal("konsole")
	if got := konsole.Identity("claude"); got != "" {
		t.Errorf("konsole identity = %q, want none — it publishes no such flag", got)
	}
}

// Nothing a terminal wraps may become syntax. The command arrives as an argv
// and leaves as a longer argv, so a name full of shell metacharacters is a
// name that does not resolve — never a second command.
func TestWrappingNeverBuildsACommandLine(t *testing.T) {
	spelling, _ := LookupTerminal("ghostty")
	argv := spelling.Wrap("/usr/bin/ghostty", spelling.Identity("claude"),
		[]string{"/usr/bin/claude", "; rm -rf ~", "$(id)"})
	want := []string{"/usr/bin/ghostty", "--class=dev.jarvix.claude", "-e",
		"/usr/bin/claude", "; rm -rf ~", "$(id)"}
	if !slices.Equal(argv, want) {
		t.Errorf("argv = %v, want each element kept whole: %v", argv, want)
	}
}

// KnownTerminals is what a refusal names, so it has to be stable and
// non-empty — a machine must word the same refusal the same way every time.
func TestKnownTerminalsIsSortedAndNamesTheRequiredOnes(t *testing.T) {
	known := KnownTerminals()
	if !slices.IsSorted(known) {
		t.Errorf("KnownTerminals() = %v, want a stable order", known)
	}
	for _, want := range []string{"alacritty", "foot", "ghostty", "gnome-terminal",
		"kitty", "konsole", "wezterm", "xterm"} {
		if !slices.Contains(known, want) {
			t.Errorf("KnownTerminals() = %v, missing %q", known, want)
		}
	}
}

// The identity flags are shared with routine's table rather than copied, so
// this is the one place that claims to know what foot accepts.
func TestIdentityFlagAnswersOnlyForTerminalsThatHaveOne(t *testing.T) {
	for terminal, want := range map[string]string{
		"ghostty":   "--class=",
		"alacritty": "--class=",
		"kitty":     "--class=",
		"foot":      "--app-id=",
		"wezterm":   "--class=",
	} {
		if got, ok := IdentityFlag(terminal); !ok || got != want {
			t.Errorf("IdentityFlag(%q) = %q,%v; want %q", terminal, got, ok, want)
		}
	}
	for _, terminal := range []string{"konsole", "gnome-terminal", "xterm", "st"} {
		if flag, ok := IdentityFlag(terminal); ok {
			t.Errorf("IdentityFlag(%q) = %q; that terminal publishes no such flag", terminal, flag)
		}
	}
}
