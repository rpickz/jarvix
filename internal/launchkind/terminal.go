package launchkind

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// This file is the per-terminal table: how each terminal emulator spells "run
// this command in a new window", and how — if at all — it lets the caller
// choose the window's class.
//
// It is a table and not a rule for the same reason routine's identity table is
// (ADR 0058): there is no convention here to derive from. `-e` came from
// xterm and most terminals imitate it, but kitty and foot take the command as
// trailing positional arguments, gnome-terminal deprecated `-e` in favour of
// `--`, and wezterm puts the whole thing behind a `start` sub-command. A
// spelling guessed wrong is not a near miss: it is an argument the terminal
// rejects at start-up, so the user asks for Claude and gets a terminal that
// flashes an unknown-option error and exits — the same confident nothing this
// ticket exists to end.
//
// So every row below is a spelling read out of that terminal's own
// documentation, and the source is recorded in ADR 0061. A terminal that is
// not in the table is refused by name, with the list of the ones that are.
// Guessing `-e` at it would be the same failure with an extra step.

// Spelling is how one terminal is asked to run a command in a new window.
//
// The four fields are applied in a fixed order, which is the order every
// terminal in the table needs: the sub-command first, then the identity, then
// whatever introduces the command, then the command itself. `-e` is last in
// every row that has it because alacritty documents it as "must be last
// argument" and ghostty makes everything after it part of the command — so a
// construction that put anything after the command would be wrong for two of
// the seven and confusing for the rest.
type Spelling struct {
	// Prefix are the arguments that come before everything else. Only wezterm
	// needs one ("start"); it is a sub-command, not a flag.
	Prefix []string
	// IdentityFlag sets the window's class, and always carries its value in
	// the same argument, so it ends with "=". Empty means this terminal
	// offers no such flag — see AppID for the one that offers a restricted
	// one, and the table's comments for the ones that offer none.
	IdentityFlag string
	// AppID says the class must be a GTK application id (reverse-DNS, at
	// least one dot) rather than a free-form token. Ghostty is the only row
	// that sets it, and honouring it is the difference between a window that
	// carries our class and a terminal that refuses to start.
	AppID bool
	// Command introduces the program to run. Empty means the terminal takes
	// the command as trailing positional arguments, which is what kitty and
	// foot document.
	Command []string
}

// terminalSpellings is the table. Each row's source is recorded in ADR 0061;
// the two rows for terminals installed on the machine this was written for
// (ghostty, foot) were read from the man pages on that machine, and the rest
// from each project's own published manual.
var terminalSpellings = map[string]Spelling{
	// ghostty(1): "Ghostty supports the common -e flag for executing a command
	// with arguments." --class "controls the class field of the WM_CLASS X11
	// property …, the Wayland application ID …", and "the class name must
	// follow the requirements defined in the GTK documentation" — hence AppID.
	// -e also forces gtk-single-instance=false, so the window we class is a
	// new process rather than a tab in the one the user is already using.
	"ghostty": {IdentityFlag: "--class=", AppID: true, Command: []string{"-e"}},

	// alacritty(1): "-e, --command <COMMAND>… Command and args to execute
	// (must be last argument)"; "--class <GENERAL> | <GENERAL>,<INSTANCE>
	// Defines the window class hint on Linux". The attached form is what
	// routine's identity table already ships for alacritty.
	"alacritty": {IdentityFlag: "--class=", Command: []string{"-e"}},

	// kitty(1): "kitty [options] [program-to-run …]" — the command is
	// positional, and there is no -e. "--class … On Wayland set the
	// application id. On X11 set the class part of the WM_CLASS window
	// property."
	"kitty": {IdentityFlag: "--class="},

	// foot(1): "foot [OPTIONS] <command> [COMMAND OPTIONS] — All trailing
	// (non-option) arguments are treated as a command"; "-a,--app-id=ID Value
	// to set the app-id property on the Wayland window to". foot does accept
	// -e, but its own manual says it is "Ignored; for compatibility with
	// xterm -e", so the positional form is the one it actually documents.
	"foot": {IdentityFlag: "--app-id="},

	// wezterm(1) / wezterm.org "wezterm start": the command goes after a `--`
	// separator ("wezterm start -- bash -l"), and --class "overrides the
	// default windowing system class" — the X11 window class, the Wayland
	// app_id.
	"wezterm": {Prefix: []string{"start"}, IdentityFlag: "--class=", Command: []string{"--"}},

	// gnome-terminal(1): "use -- to terminate the options, and put the program
	// and arguments to execute after it: … prefer to use gnome-terminal --
	// python3 -q" over the deprecated -e.
	//
	// No identity: gnome-terminal documents --class ("Program class as used by
	// the window manager"), but it is the legacy GTK/X11 toolkit option, and
	// under Wayland GTK takes the application id from the application itself
	// and ignores it. Promising a class we would only sometimes deliver is
	// worse than saying there is none, so the row says there is none.
	"gnome-terminal": {Command: []string{"--"}},

	// konsole(1) / the Konsole handbook's command-line options: "-e command —
	// Execute command instead of the normal shell", and it catches every
	// following argument, so it goes last. Konsole publishes no option for the
	// window class.
	"konsole": {Command: []string{"-e"}},

	// xterm(1), the original: "-e program [arguments]" — the flag every other
	// terminal's -e imitates, and the one foot's manual names when explaining
	// why it accepts and ignores it.
	//
	// No identity: xterm's -class is an X toolkit option that takes its value
	// as a separate argument, a second argument shape this table deliberately
	// does not model — one shape, applied the same way to every row, is what
	// makes the table readable. An xterm window is found by its own class,
	// XTerm, instead.
	"xterm": {Command: []string{"-e"}},
}

// UnknownTerminalError is a configured terminal this table has no spelling
// for. It names the terminal and the ones that are known, because the fix is
// a one-word configuration change and a refusal that did not name the
// alternatives would make the user go looking for this file.
type UnknownTerminalError struct {
	Name  string
	Known []string
}

func (e *UnknownTerminalError) Error() string {
	return fmt.Sprintf("I do not know how to run a command inside %s — the terminals I know are %s",
		e.Name, strings.Join(e.Known, ", "))
}

// LookupTerminal returns the spelling for a configured terminal. The argument
// may be a bare name or an absolute path ([intents] terminal accepts both);
// only the base name decides.
func LookupTerminal(terminal string) (Spelling, error) {
	name := TerminalName(terminal)
	if spelling, ok := terminalSpellings[name]; ok {
		return spelling, nil
	}
	return Spelling{}, &UnknownTerminalError{Name: name, Known: KnownTerminals()}
}

// TerminalName is the table's key for a configured terminal: the base name,
// lower-cased. Exported because the daemon logs it and the refusal names it,
// and a second copy of "the base name decides" is how those would drift.
func TerminalName(terminal string) string {
	return strings.ToLower(filepath.Base(strings.TrimSpace(terminal)))
}

// KnownTerminals lists the terminals this table can run a command inside,
// sorted, for the refusal that has to name them.
func KnownTerminals() []string {
	out := make([]string, 0, len(terminalSpellings))
	for name := range terminalSpellings {
		out = append(out, name)
	}
	// Sorted so the same machine always words the refusal the same way.
	slices.Sort(out)
	return out
}

// IdentityFlag reports the flag a terminal accepts for setting its window's
// class, and whether it accepts one at all.
//
// Exported so routine's identity table can defer to this one rather than keep
// a second copy of the same three flags: two tables that both claim to know
// what `foot` accepts are two tables that will eventually disagree.
func IdentityFlag(terminal string) (string, bool) {
	spelling, ok := terminalSpellings[TerminalName(terminal)]
	if !ok || spelling.IdentityFlag == "" {
		return "", false
	}
	return spelling.IdentityFlag, true
}

// appIDPrefix is the reverse-DNS namespace Jarvix classes a terminal window
// under when the terminal insists on a GTK application id. It has to be a
// namespace we own rather than the terminal's own (com.mitchellh.ghostty…),
// because the whole point of the class is that it is nobody else's.
const appIDPrefix = "dev.jarvix."

// Identity renders the class to give a window that will run program.
//
// Two forms, and which one is used is the terminal's decision, not ours: a
// free-form token for the terminals that accept one, and a GTK application id
// for ghostty, which validates the value and would refuse to start on a bare
// word. Both end in the program's own name, so the window is findable by the
// name the user said.
func (s Spelling) Identity(program string) string {
	name := strings.ToLower(strings.TrimSpace(program))
	if s.IdentityFlag == "" {
		return ""
	}
	if !s.AppID {
		return sanitise(name, "._-")
	}
	// A GTK application id may hold letters, digits, underscores, hyphens and
	// the dots that separate its elements, and may not begin with a digit —
	// which the prefix guarantees, whatever the program is called.
	return appIDPrefix + sanitise(name, "-")
}

// sanitise keeps letters, digits and the punctuation the caller says is safe,
// and drops everything else. The names reaching it have already passed the
// launcher's own program-name check, so this is a second bound rather than
// the first one.
func sanitise(name, keep string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case strings.ContainsRune(keep, r):
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "program"
	}
	return b.String()
}

// Wrap builds the argv that runs command inside this terminal.
//
// terminalPath is the resolved terminal binary — resolved, not searched again
// here, so what runs is decided once. identity is the class from Identity, or
// empty to leave the window with the terminal's own class.
//
// Nothing in here is ever rendered into a string. The command arrives as an
// argv and leaves as a longer argv, so a program name containing a semicolon
// is a program name that does not resolve, never a second command.
func (s Spelling) Wrap(terminalPath, identity string, command []string) []string {
	argv := make([]string, 0, 1+len(s.Prefix)+1+len(s.Command)+len(command))
	argv = append(argv, terminalPath)
	argv = append(argv, s.Prefix...)
	if s.IdentityFlag != "" && identity != "" {
		argv = append(argv, s.IdentityFlag+identity)
	}
	argv = append(argv, s.Command...)
	return append(argv, command...)
}
