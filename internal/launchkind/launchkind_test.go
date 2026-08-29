package launchkind

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/desktopentry"
)

// Every test here runs against a machine the test wrote: an applications
// directory it filled and a PATH it declared. Nothing is launched, and the
// index doing the classifying is the one the daemon runs — the same doctrine
// internal/routine's fixtures follow, and the reason the machine running the
// tests decides nothing about whether `claude` is a command.

// machine is a computer described in a test: what is on PATH, what desktop
// entries exist, and what the user said about it.
type machine struct {
	onPath   []string
	entries  map[string]string // id → the keys after Type and Name
	terminal []string
	windowed []string
}

// build writes the machine out and returns a catalogue over it.
func (m machine) build(t *testing.T) *Catalogue {
	t.Helper()
	apps := filepath.Join(t.TempDir(), "applications")
	if err := os.MkdirAll(apps, 0o755); err != nil {
		t.Fatal(err)
	}
	for id, keys := range m.entries {
		body := "[Desktop Entry]\nType=Application\nName=" + id + "\n" + keys + "\n"
		if err := os.WriteFile(filepath.Join(apps, id+".desktop"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// PATH is two seams and the fixture says both: a directory listing, which
	// is what a catalogue listing walks, and a per-name resolution, which is
	// what a launch does. Written out as real files so the walk is the real
	// one, resolved to /usr/bin so an assertion reads as what it is checking.
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	installed := make(map[string]bool, len(m.onPath))
	for _, name := range m.onPath {
		installed[name] = true
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return New(Options{
		EntryDirs:         func() []string { return []string{apps} },
		PathDirs:          func() []string { return []string{bin} },
		TerminalPrograms:  m.terminal,
		GraphicalPrograms: m.windowed,
		LookPath: func(name string) (string, error) {
			if installed[filepath.Base(name)] {
				return "/usr/bin/" + filepath.Base(name), nil
			}
			return "", errors.New(name + ": executable file not found in $PATH")
		},
	})
}

// one is the single candidate a lookup must have produced, or a failure
// naming what it produced instead.
func one(t *testing.T, c *Catalogue, name string) Program {
	t.Helper()
	found := c.Lookup(name)
	if len(found) != 1 {
		t.Fatalf("Lookup(%q) = %d candidates (%v), want exactly one", name, len(found), found)
	}
	return found[0]
}

// The rule the ticket is built on, and the four programs it was written for.
// On this machine `claude`, `opencode`, `codex` and `grok` are on PATH with no
// entry of their own, and that alone identifies them as terminal programs.
func TestAProgramOnPathWithNoEntryIsACommand(t *testing.T) {
	c := machine{
		onPath:  []string{"claude", "opencode", "codex", "grok", "firefox"},
		entries: map[string]string{"firefox": "Exec=firefox"},
	}.build(t)
	for _, name := range []string{"claude", "opencode", "codex", "grok"} {
		p := one(t, c, name)
		if p.Kind != KindTerminal || p.Reason != ReasonNoEntry {
			t.Errorf("%s = %v (%v), want a terminal program with no entry", name, p.Kind, p.Reason)
		}
		if !slices.Equal(p.Argv, []string{"/usr/bin/" + name}) {
			t.Errorf("%s argv = %v, want the resolved path", name, p.Argv)
		}
	}
}

// The specification's own explicit statement, which #186 parsed and refused.
func TestAnEntryThatAsksForATerminalGetsOne(t *testing.T) {
	c := machine{
		onPath:  []string{"htop"},
		entries: map[string]string{"htop": "Exec=htop\nTerminal=true"},
	}.build(t)
	p := one(t, c, "htop")
	if p.Kind != KindTerminal || p.Reason != ReasonEntryTerminal {
		t.Errorf("htop = %v (%v), want the entry's own Terminal=true honoured", p.Kind, p.Reason)
	}
}

// An entry that does not ask for a terminal is unchanged: it opens a window.
func TestAnEntryWithoutTerminalOpensAWindow(t *testing.T) {
	c := machine{
		onPath:  []string{"chromium"},
		entries: map[string]string{"ChatGPT": "Exec=chromium --app=https://chatgpt.com/\nTerminal=false"},
	}.build(t)
	p := one(t, c, "chatgpt")
	if p.Kind != KindGraphical || p.Reason != ReasonEntryWindow {
		t.Errorf("ChatGPT = %v (%v), want a windowed application", p.Kind, p.Reason)
	}
	want := []string{"/usr/bin/chromium", "--app=https://chatgpt.com/"}
	if !slices.Equal(p.Argv, want) {
		t.Errorf("argv = %v, want the entry's own command %v", p.Argv, want)
	}
}

// The half of the rule that makes it survive real packaging: an entry's id is
// very often not its binary's name. Telegram installs
// org.telegram.desktop.desktop and a telegram-desktop binary, and asking only
// "is there an entry called telegram-desktop?" would send a graphical
// application into a terminal.
func TestAnEntryAnswersForTheProgramItLaunches(t *testing.T) {
	c := machine{
		onPath:  []string{"telegram-desktop"},
		entries: map[string]string{"org.telegram.desktop": "Exec=telegram-desktop -- %u"},
	}.build(t)
	p := one(t, c, "telegram-desktop")
	if p.Kind != KindGraphical {
		t.Errorf("telegram-desktop = %v (%v), want the entry that launches it to answer",
			p.Kind, p.Reason)
	}
	if p.Entry != "org.telegram.desktop" {
		t.Errorf("entry = %q, want the entry whose Exec names it", p.Entry)
	}
}

// The ticket's own example: a `claude` command and a Claude Desktop
// application both answer to "claude", and picking one would be right half
// the time and confidently wrong the other half.
func TestOneNameWithTwoAnswersIsTwoCandidates(t *testing.T) {
	c := machine{
		onPath:  []string{"claude", "claude-desktop"},
		entries: map[string]string{"claude-desktop": "Exec=claude-desktop"},
	}.build(t)
	found := c.Lookup("claude")
	if len(found) != 2 {
		t.Fatalf("Lookup(\"claude\") = %v, want both the command and the application", found)
	}
	kinds := map[Kind]string{}
	for _, p := range found {
		kinds[p.Kind] = p.Name
	}
	if kinds[KindTerminal] != "claude" || kinds[KindGraphical] != "claude-desktop" {
		t.Errorf("candidates = %v, want the command and the application distinguished", found)
	}
	// The exact name comes first, so a caller that resolves to one answer
	// resolves to the one that was actually asked for.
	if found[0].Name != "claude" {
		t.Errorf("first candidate = %q, want the exact match", found[0].Name)
	}
}

// A word boundary, not a substring: "claude" finds "claude-desktop" and does
// not find "claudia", or half the machine would be ambiguous with the other
// half.
func TestAPrefixMatchStopsAtAWordBoundary(t *testing.T) {
	c := machine{
		onPath:  []string{"claude", "claudia"},
		entries: map[string]string{"claudia": "Exec=claudia"},
	}.build(t)
	p := one(t, c, "claude")
	if p.Name != "claude" || p.Kind != KindTerminal {
		t.Errorf("Lookup(\"claude\") = %+v, want only the command", p)
	}
}

// The honesty condition on the whole rule. "It has no desktop entry" is
// evidence only on a machine that has desktop entries: on one with none, the
// same observation is a fact about the search, and answering from it would
// send every application into a terminal window.
func TestWithNoEntriesAnywhereNothingIsClassified(t *testing.T) {
	c := machine{onPath: []string{"firefox"}}.build(t)
	p := one(t, c, "firefox")
	if p.Kind != KindUnknown || p.Reason != ReasonUnsurveyed {
		t.Errorf("firefox = %v (%v), want an honest \"I cannot tell\"", p.Kind, p.Reason)
	}
	if !strings.Contains(p.Reason.Because(), "no application entries") {
		t.Errorf("reason = %q, want it to say what is missing", p.Reason.Because())
	}
}

// The classification is a strong default, not a law: the person who owns the
// machine outranks it, in both directions.
func TestTheUsersOwnAnswerOutranksEveryInference(t *testing.T) {
	c := machine{
		onPath:   []string{"claude", "obsidian", "htop", "firefox"},
		entries:  map[string]string{"obsidian": "Exec=obsidian", "htop": "Exec=htop\nTerminal=true"},
		terminal: []string{"obsidian"},
		windowed: []string{"claude", "htop"},
	}.build(t)
	for name, want := range map[string]Kind{
		"claude":   KindGraphical, // a command the user says opens a window
		"obsidian": KindTerminal,  // an application the user says needs one
		"htop":     KindGraphical, // even the entry's own Terminal=true yields
	} {
		p := one(t, c, name)
		if p.Kind != want || p.Reason != ReasonOverride {
			t.Errorf("%s = %v (%v), want %v because the user said so", name, p.Kind, p.Reason, want)
		}
	}
	// And the override still works where nothing else can classify at all.
	bare := machine{onPath: []string{"claude"}, terminal: []string{"claude"}}.build(t)
	if p := one(t, bare, "claude"); p.Kind != KindTerminal || p.Reason != ReasonOverride {
		t.Errorf("claude = %v (%v), want the override to answer on an unsurveyed machine",
			p.Kind, p.Reason)
	}
}

// An entry left behind by an uninstall is not a candidate: offering it would
// turn "not installed" into a question about two things, one of which does
// not exist.
func TestAnEntryWhoseProgramIsGoneIsNotOffered(t *testing.T) {
	c := machine{
		onPath: []string{"chromium"},
		entries: map[string]string{
			"chromium": "Exec=chromium",
			"gone":     "Exec=gone",
			"stale":    "Exec=chromium\nTryExec=uninstalled-helper",
		},
	}.build(t)
	for _, name := range []string{"gone", "stale"} {
		if found := c.Lookup(name); len(found) != 0 {
			t.Errorf("Lookup(%q) = %v, want nothing — this machine cannot launch it", name, found)
		}
	}
}

// Command is the whole of "how it starts", and it is the terminal's own
// spelling that decides the shape.
func TestCommandWrapsATerminalProgramAndLeavesAWindowedOneAlone(t *testing.T) {
	c := machine{
		onPath:  []string{"claude", "ghostty", "firefox"},
		entries: map[string]string{"firefox": "Exec=firefox"},
	}.build(t)

	argv, identity, err := c.Command(one(t, c, "claude"), "ghostty")
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	want := []string{"/usr/bin/ghostty", "--class=dev.jarvix.claude", "-e", "/usr/bin/claude"}
	if !slices.Equal(argv, want) {
		t.Errorf("argv = %v, want %v", argv, want)
	}
	if identity != "dev.jarvix.claude" {
		t.Errorf("identity = %q, want the class the window will carry", identity)
	}

	argv, identity, err = c.Command(one(t, c, "firefox"), "ghostty")
	if err != nil {
		t.Fatalf("Command: %v", err)
	}
	if !slices.Equal(argv, []string{"/usr/bin/firefox"}) || identity != "" {
		t.Errorf("argv = %v, identity = %q; a windowed application is its own argv", argv, identity)
	}
}

// A terminal Jarvix has no spelling for, and a terminal that is not
// installed, are two different failures with two different fixes — so they
// are two different errors, and neither is a guessed -e.
func TestATerminalProgramNeedsATerminalThatIsKnownAndInstalled(t *testing.T) {
	c := machine{onPath: []string{"claude", "firefox"},
		entries: map[string]string{"firefox": "Exec=firefox"}}.build(t)
	program := one(t, c, "claude")
	if !program.InTerminal() {
		t.Fatalf("claude = %v, want a terminal program", program.Kind)
	}

	if _, _, err := c.Command(program, "st"); err == nil {
		t.Error("an unknown terminal was accepted")
	} else if !strings.Contains(err.Error(), "do not know how to run a command inside st") {
		t.Errorf("error = %q, want the unknown-terminal refusal", err)
	}
	if _, _, err := c.Command(program, "kitty"); err == nil {
		t.Error("a terminal that is not installed was accepted")
	} else if !strings.Contains(err.Error(), "kitty is not installed") {
		t.Errorf("error = %q, want the not-installed refusal", err)
	}
}

// The catalogue the model consults: what is here, and how each one starts.
func TestTheCatalogueSaysWhatIsHereAndHowItStarts(t *testing.T) {
	c := machine{
		onPath: []string{"claude", "firefox", "obsidian"},
		entries: map[string]string{
			"firefox":  "Exec=firefox",
			"obsidian": "Exec=obsidian",
		},
	}.build(t)
	// With no query, the applications: what a person means by "what can you
	// open?". A PATH command is not one of them.
	found, total := c.List("", 10)
	if total != 2 || len(found) != 2 {
		t.Fatalf("List(\"\") = %v (%d), want the two applications", found, total)
	}
	for _, p := range found {
		if p.Kind != KindGraphical {
			t.Errorf("%s = %v, want a windowed application", p.Name, p.Kind)
		}
	}
	// With a query, the commands join in — which is the point: this is how
	// the model discovers that claude is a terminal program.
	found, total = c.List("claude", 10)
	if total != 1 || len(found) != 1 || found[0].Name != "claude" ||
		found[0].Kind != KindTerminal {
		t.Fatalf("List(\"claude\") = %v (%d), want the claude command", found, total)
	}
	// A listing answers "what and how", not "run it": the argv is worked out
	// when something is actually launched, so listing the machine does not
	// cost a resolution per program.
	if found[0].Argv != nil {
		t.Errorf("listed program carries an argv (%v); a listing must not price one",
			found[0].Argv)
	}
	// The bound is honest about what it left out.
	found, total = c.List("", 1)
	if len(found) != 1 || total != 2 {
		t.Errorf("List(\"\", 1) = %v (%d), want one shown and two counted", found, total)
	}
}

// The invalidation rule. The catalogue is a picture of the directories it was
// drawn from, and it is redrawn when one of them changes — not on a timer
// that goes on claiming to be current after an install, and not on every
// launch.
func TestTheCatalogueIsRebuiltWhenTheApplicationsDirectoryChanges(t *testing.T) {
	apps := filepath.Join(t.TempDir(), "applications")
	if err := os.MkdirAll(apps, 0o755); err != nil {
		t.Fatal(err)
	}
	// The directory's own mtime is the signal, and two writes a microsecond
	// apart on a filesystem with coarse timestamps would report no change at
	// all — so each write stamps the directory at a time of its own.
	stamped := time.Now().Add(-time.Hour)
	write := func(id, body string) {
		t.Helper()
		path := filepath.Join(apps, id+".desktop")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		stamped = stamped.Add(time.Minute)
		if err := os.Chtimes(apps, stamped, stamped); err != nil {
			t.Fatal(err)
		}
	}
	write("firefox", "[Desktop Entry]\nType=Application\nName=Firefox\nExec=firefox\n")

	loads := 0
	now := time.Now()
	// The Load seam counts the scans, which is the property under test: a
	// launch must not pay for one.
	c := New(Options{
		Load:      func() *desktopentry.Index { loads++; return desktopentry.Load(apps) },
		EntryDirs: func() []string { return []string{apps} },
		PathDirs:  func() []string { return nil },
		Recheck:   time.Second,
		Now:       func() time.Time { return now },
		LookPath:  func(name string) (string, error) { return "/usr/bin/" + name, nil },
	})

	if p := one(t, c, "firefox"); p.Kind != KindGraphical {
		t.Fatalf("firefox = %v", p.Kind)
	}
	for range 5 {
		c.Lookup("firefox")
	}
	if loads != 1 {
		t.Errorf("scans = %d, want the catalogue built once and reused", loads)
	}

	// An application installed while the daemon runs: the directory changes,
	// so the next question past the recheck interval sees it.
	write("obsidian", "[Desktop Entry]\nType=Application\nName=Obsidian\nExec=obsidian\n")
	now = now.Add(2 * time.Second)
	if p := one(t, c, "obsidian"); p.Kind != KindGraphical {
		t.Errorf("obsidian = %v, want the newly installed application seen", p.Kind)
	}
	if loads != 2 {
		t.Errorf("scans = %d, want exactly one rebuild", loads)
	}

	// Nothing changed since: no further scan, however often it is asked.
	now = now.Add(time.Hour)
	for range 5 {
		c.Lookup("firefox")
	}
	if loads != 2 {
		t.Errorf("scans = %d, want no rebuild for directories that did not change", loads)
	}

	// A configuration reload replaces the overrides, so it drops the picture
	// outright rather than waiting for a directory to move.
	c.Invalidate()
	c.Lookup("firefox")
	if loads != 3 {
		t.Errorf("scans = %d, want Invalidate to force a rebuild", loads)
	}
}

// An allow list of absolute paths never reaches PATH, so Describe is the same
// classification without the search.
func TestDescribeClassifiesAProgramAlreadyResolvedToAPath(t *testing.T) {
	c := machine{
		onPath:  []string{"obsidian", "claude"},
		entries: map[string]string{"obsidian": "Exec=obsidian"},
	}.build(t)
	if p := c.Describe("/opt/obsidian/obsidian", "/opt/obsidian/obsidian"); p.Kind != KindGraphical {
		t.Errorf("obsidian = %v (%v), want the entry that launches it to answer", p.Kind, p.Reason)
	}
	if p := c.Describe("/opt/claude/claude", "/opt/claude/claude"); p.Kind != KindTerminal {
		t.Errorf("claude = %v (%v), want the no-entry rule to apply", p.Kind, p.Reason)
	}
}
