package desktopentry_test

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/desktopentry"
)

// These tests read desktop files off disk and never execute anything: the
// package has no exec in it, and Command() returns an argv for someone else to
// decide about. Writing real files is what makes them honest — the parser
// under test is the one the daemon runs, against the shapes this machine
// actually has.

// writeEntries builds an applications directory and returns its index.
func writeEntries(t *testing.T, files map[string]string) *desktopentry.Index {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "applications")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return desktopentry.Load(dir)
}

// TestTheWebAppEntriesOnThisDesktopResolveToTheirWrapper is the case the
// ticket was written from, byte for byte: on Omarchy, X, ChatGPT, WhatsApp
// and Discord are entries whose Exec is `omarchy-launch-webapp <url>`, and
// none of them has a binary of its own.
func TestTheWebAppEntriesOnThisDesktopResolveToTheirWrapper(t *testing.T) {
	idx := writeEntries(t, map[string]string{
		"ChatGPT.desktop": "[Desktop Entry]\nVersion=1.0\nName=ChatGPT\n" +
			"Exec=omarchy-launch-webapp https://chatgpt.com/\nTerminal=false\nType=Application\n",
		"WhatsApp.desktop": "[Desktop Entry]\nName=WhatsApp\n" +
			"Exec=omarchy-launch-webapp https://web.whatsapp.com/\nType=Application\n",
	})

	for _, want := range []struct {
		id   string
		argv []string
	}{
		{"ChatGPT", []string{"omarchy-launch-webapp", "https://chatgpt.com/"}},
		{"ChatGPT.desktop", []string{"omarchy-launch-webapp", "https://chatgpt.com/"}},
		{"chatgpt", []string{"omarchy-launch-webapp", "https://chatgpt.com/"}},
		{"WhatsApp", []string{"omarchy-launch-webapp", "https://web.whatsapp.com/"}},
	} {
		entry, err := idx.Lookup(want.id)
		if err != nil {
			t.Fatalf("looking up %q: %v", want.id, err)
		}
		argv, err := entry.Command()
		if err != nil {
			t.Fatalf("%q has no command: %v", want.id, err)
		}
		if !slices.Equal(argv, want.argv) {
			t.Errorf("%q runs %q, want %q", want.id, argv, want.argv)
		}
	}
}

// TestAMissingEntryIsNamedWithTheClosestInstalled: the whole point of doing
// this at load time is that the message is actionable. A shrug would be no
// better than the eight-second silence it replaces.
func TestAMissingEntryIsNamedWithTheClosestInstalled(t *testing.T) {
	idx := writeEntries(t, map[string]string{
		"signal-desktop.desktop": "[Desktop Entry]\nName=Signal\nExec=signal-desktop\nType=Application\n",
	})

	_, err := idx.Lookup("signal")
	if err == nil {
		t.Fatal("a missing entry resolved")
	}
	var missing *desktopentry.NotFoundError
	if !errors.As(err, &missing) {
		t.Fatalf("error is %T, want a NotFoundError so callers can classify it", err)
	}
	if missing.ID != "signal" {
		t.Errorf("the error names %q, want the id that was asked for", missing.ID)
	}
	if !strings.Contains(err.Error(), "signal-desktop") {
		t.Errorf("message = %q, want it to name what IS installed", err.Error())
	}
}

// TestExecQuotingIsTheSpecificationsAndNotAShells is the security pin for
// this package: a desktop file is writable by anything the user installs, and
// what comes out of it is an argv, not a command line. A shell would treat
// four of these five values as syntax; this parser treats all of them as
// characters.
func TestExecQuotingIsTheSpecificationsAndNotAShells(t *testing.T) {
	for _, tc := range []struct {
		name string
		exec string
		want []string
	}{
		{"plain", `firefox --new-window`, []string{"firefox", "--new-window"}},
		{"quoted argument keeps its space", `chromium "--profile-directory=Profile 3"`,
			[]string{"chromium", "--profile-directory=Profile 3"}},
		{"a semicolon is a character", `sh -c "echo hi; rm -rf ~"`,
			[]string{"sh", "-c", "echo hi; rm -rf ~"}},
		{"command substitution is a character", `weird "$(rm -rf /tmp/x)"`,
			[]string{"weird", "$(rm -rf /tmp/x)"}},
		{"backticks are characters", "weird \"`id`\"", []string{"weird", "`id`"}},
		{"escaped quote", `weird "say \"hello\""`, []string{"weird", `say "hello"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := desktopentry.ParseExec(tc.exec)
			if err != nil {
				t.Fatalf("ParseExec(%q): %v", tc.exec, err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("ParseExec(%q) = %q, want %q", tc.exec, got, tc.want)
			}
		})
	}
}

// TestFieldCodesExpandTheWayTheSpecificationSays: a routine launches an
// application with no files and no URLs to hand it, so the file and URL codes
// disappear rather than becoming an empty argument the program then complains
// about.
func TestFieldCodesExpandTheWayTheSpecificationSays(t *testing.T) {
	idx := writeEntries(t, map[string]string{
		"gedit.desktop": "[Desktop Entry]\nName=Text Editor\nIcon=accessories-text-editor\n" +
			"Exec=gedit %U %i --title=%c --deprecated=%v\nType=Application\n",
		"pct.desktop": "[Desktop Entry]\nName=Pct\nExec=pct --at=50%% --url=http://x/?a=%%b\nType=Application\n",
	})

	entry, err := idx.Lookup("gedit")
	if err != nil {
		t.Fatal(err)
	}
	argv, err := entry.Command()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"gedit", "--icon", "accessories-text-editor", "--title=Text Editor", "--deprecated="}
	if !slices.Equal(argv, want) {
		t.Errorf("argv = %q, want %q", argv, want)
	}

	// %% is a literal percent and nothing else, so a percentage in a flag and
	// a query string survive intact.
	entry, err = idx.Lookup("pct")
	if err != nil {
		t.Fatal(err)
	}
	argv, err = entry.Command()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"pct", "--at=50%", "--url=http://x/?a=%b"}; !slices.Equal(argv, want) {
		t.Errorf("argv = %q, want %q", argv, want)
	}
}

// TestAnEntryThatCannotBeLaunchedSaysWhy: three shapes a desktop file can
// take that are not launchable at all, each refused with the reason rather
// than started into nothing.
func TestAnEntryThatCannotBeLaunchedSaysWhy(t *testing.T) {
	idx := writeEntries(t, map[string]string{
		"link.desktop":   "[Desktop Entry]\nName=Bookmark\nType=Link\nURL=https://example.com\n",
		"noexec.desktop": "[Desktop Entry]\nName=Nothing\nType=Application\n",
	})

	for _, tc := range []struct{ id, want string }{
		{"link", "not an application"},
		{"noexec", "no Exec line"},
	} {
		entry, err := idx.Lookup(tc.id)
		if err != nil {
			t.Fatalf("%s: %v", tc.id, err)
		}
		_, err = entry.Command()
		if err == nil {
			t.Fatalf("%s produced a command", tc.id)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s refused with %q, want it to say %q", tc.id, err.Error(), tc.want)
		}
	}
}

// TestHiddenEntriesAreAbsentAndNoDisplayOnesAreNot: the specification's
// Hidden key means "deleted", so such an entry must not be found — while
// NoDisplay only hides it from a MENU, and several web-app wrappers are
// written that way, so a routine can still name one.
func TestHiddenEntriesAreAbsentAndNoDisplayOnesAreNot(t *testing.T) {
	idx := writeEntries(t, map[string]string{
		"gone.desktop":    "[Desktop Entry]\nName=Gone\nExec=gone\nType=Application\nHidden=true\n",
		"quiet.desktop":   "[Desktop Entry]\nName=Quiet\nExec=quiet\nType=Application\nNoDisplay=true\n",
		"kde/sub.desktop": "[Desktop Entry]\nName=Sub\nExec=sub\nType=Application\n",
	})

	if _, err := idx.Lookup("gone"); err == nil {
		t.Error("a Hidden entry was found; the specification says it is deleted")
	}
	if _, err := idx.Lookup("quiet"); err != nil {
		t.Errorf("a NoDisplay entry was refused: %v; it is hidden from menus, not from routines", err)
	}
	// A sub-directory becomes part of the id with a dash, which is the
	// specification's own rule.
	if _, err := idx.Lookup("kde-sub"); err != nil {
		t.Errorf("a nested entry was not found under its dashed id: %v", err)
	}
}

// TestTheFirstDirectoryWins pins the precedence the specification sets: an
// entry in the user's own data directory shadows the system one of the same
// id, which is how a user overrides a packaged application.
func TestTheFirstDirectoryWins(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home", "applications")
	system := filepath.Join(root, "system", "applications")
	for dir, exec := range map[string]string{home: "mine", system: "theirs"} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "[Desktop Entry]\nName=Editor\nExec=" + exec + "\nType=Application\n"
		if err := os.WriteFile(filepath.Join(dir, "editor.desktop"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	entry, err := desktopentry.Load(home, system).Lookup("editor")
	if err != nil {
		t.Fatal(err)
	}
	argv, err := entry.Command()
	if err != nil {
		t.Fatal(err)
	}
	if argv[0] != "mine" {
		t.Errorf("resolved to %q, want the user's own entry to shadow the system one", argv[0])
	}
}

// TestSearchDirsFollowsTheEnvironment: the reason every test in this tree can
// be hermetic. XDG_DATA_HOME and XDG_DATA_DIRS decide where entries are read
// from, so a test points them at a temporary directory and sees nothing of
// the real machine.
func TestSearchDirsFollowsTheEnvironment(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/tmp/jarvix-home")
	t.Setenv("XDG_DATA_DIRS", "/tmp/jarvix-a:/tmp/jarvix-b")

	want := []string{
		"/tmp/jarvix-home/applications",
		"/tmp/jarvix-a/applications",
		"/tmp/jarvix-b/applications",
	}
	if got := desktopentry.SearchDirs(); !slices.Equal(got, want) {
		t.Errorf("SearchDirs() = %q, want %q", got, want)
	}
}

// TestAMissingDirectoryIsNotAnError: most machines have two of the four
// search directories, and a routine must not fail to load because
// /usr/local/share/applications was never created.
func TestAMissingDirectoryIsNotAnError(t *testing.T) {
	idx := desktopentry.Load(filepath.Join(t.TempDir(), "nowhere"))
	if ids := idx.IDs(); len(ids) != 0 {
		t.Errorf("found %v under a directory that does not exist", ids)
	}
	if _, err := idx.Lookup("anything"); err == nil {
		t.Error("an entry resolved out of a directory that does not exist")
	}
}

// TestAMalformedExecIsRefusedRatherThanGuessedAt: an unclosed quote means the
// file's author meant something this parser cannot know, and inventing a
// closing quote would produce an argv nobody wrote. The step naming it is
// told, with the entry's id in the message.
func TestAMalformedExecIsRefusedRatherThanGuessedAt(t *testing.T) {
	if _, err := desktopentry.ParseExec(`weird "never closed`); err == nil {
		t.Error("an unclosed quote parsed")
	}
	idx := writeEntries(t, map[string]string{
		"broken.desktop": "[Desktop Entry]\nName=Broken\nExec=weird \"never closed\nType=Application\n",
	})
	entry, err := idx.Lookup("broken")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Command(); err == nil ||
		!strings.Contains(err.Error(), "broken desktop entry's Exec line cannot be read") {
		t.Errorf("Command() = %v, want a refusal naming the entry", err)
	}
	// An empty name is not a lookup at all.
	if _, err := idx.Lookup("   "); err == nil {
		t.Error("an empty entry name resolved")
	}
}
