package tools

import (
	"strings"
	"testing"
)

// The acceptance criteria of issue #194, at the tool boundary: what the model
// is told after asking to launch something, for each kind of thing it can be.
//
// The failure these pin is not a wording bug. Asked to launch Claude, the
// launcher resolved `claude` on PATH, exec'd it bare, and returned "it is
// opening; its window will appear on its own" — a confident description of
// something that did not happen. Every assertion below is about the sentence
// the user ends up hearing being the sentence that is true.

// terminalMachine is a computer with applications on it (so "no entry" means
// something), a terminal, and whatever else the test named on PATH.
func terminalMachine(t *testing.T, h *harness, onPath ...string) {
	t.Helper()
	stubApp(t, append([]string{"ghostty", "firefox"}, onPath...)...)
	// One real application, so the machine has entries and the absence of one
	// is evidence rather than a fact about the search.
	h.install(t, "firefox", "Exec=firefox")
}

func TestLaunchingACommandOpensItInATerminal(t *testing.T) {
	h := newHarness(t)
	terminalMachine(t, h, "claude")

	out := h.run(t, LaunchAppToolName, map[string]any{"app": "claude"})
	if !strings.Contains(out, "Started claude inside ghostty") {
		t.Errorf("launch = %q, want it to say where claude is running", out)
	}
	if !strings.Contains(out, "running in a terminal") {
		t.Errorf("launch = %q, want the spoken wording for a terminal program", out)
	}
	// The sentence that made this ticket exist must not survive here. It may
	// only appear as the instruction NOT to say it.
	if strings.Contains(out, "it is opening; its window will appear") {
		t.Errorf("launch = %q, still promises a window that nothing will open", out)
	}
	if !strings.Contains(out, "Do not say a window will appear on its own") {
		t.Errorf("launch = %q, want the promise ruled out explicitly", out)
	}
	calls := h.launcher.calls()
	if len(calls) != 1 {
		t.Fatalf("launched %v, want exactly one", calls)
	}
	for _, want := range []string{"ghostty", "--class=dev.jarvix.claude", "-e", "claude"} {
		if !strings.Contains(calls[0], want) {
			t.Errorf("argv = %q, missing %q", calls[0], want)
		}
	}
	if got := h.firedEvents(); len(got) != 1 || got[0] != "launch:claude" {
		t.Errorf("events = %v, want the launch named by the program", got)
	}
}

// The identity is what makes the terminal window findable afterwards, so a
// routine or a window tool can act on it (#186's mechanism, applied to the
// terminal rather than to the program it hosts).
func TestATerminalLaunchGivesItsWindowAnIdentity(t *testing.T) {
	h := newHarness(t)
	terminalMachine(t, h, "opencode")
	h.run(t, LaunchAppToolName, map[string]any{"app": "opencode"})
	if calls := h.launcher.calls(); len(calls) != 1 ||
		!strings.Contains(calls[0], "--class=dev.jarvix.opencode") {
		t.Errorf("argv = %v, want the window classed so it can be found", calls)
	}
}

// A graphical application is unchanged: started directly, and the sentence
// still says a window is coming, because one genuinely is.
func TestLaunchingAGraphicalApplicationIsUnchanged(t *testing.T) {
	h := newHarness(t)
	terminalMachine(t, h, "firefox")
	out := h.run(t, LaunchAppToolName, map[string]any{"app": "firefox"})
	if !strings.Contains(out, "Started firefox") ||
		!strings.Contains(out, "window will appear on its own") {
		t.Errorf("launch = %q, want the unchanged windowed wording", out)
	}
	if calls := h.launcher.calls(); len(calls) != 1 || strings.Contains(calls[0], "ghostty") {
		t.Errorf("argv = %v, want the application started directly", calls)
	}
}

// The desktop entry's own Terminal=true, which #186 parsed and refused.
func TestAnEntryThatAsksForATerminalGetsOne(t *testing.T) {
	h := newHarness(t)
	terminalMachine(t, h, "htop")
	h.install(t, "htop", "Exec=htop", "Terminal=true")
	out := h.run(t, LaunchAppToolName, map[string]any{"app": "htop"})
	if !strings.Contains(out, "running in a terminal") {
		t.Errorf("launch = %q, want the entry's own Terminal=true honoured", out)
	}
	if calls := h.launcher.calls(); len(calls) != 1 || !strings.Contains(calls[0], "ghostty") {
		t.Errorf("argv = %v, want it wrapped in the configured terminal", calls)
	}
}

// The ticket's own example: a `claude` command and a Claude Desktop
// application. Jarvix asks which, in one short sentence, and does not guess.
func TestTwoCandidatesAcrossKindsAreAskedAboutRatherThanGuessed(t *testing.T) {
	h := newHarness(t)
	terminalMachine(t, h, "claude", "claude-desktop")
	h.install(t, "claude-desktop", "Exec=claude-desktop")

	out := h.run(t, LaunchAppToolName, map[string]any{"app": "claude"})
	if !strings.Contains(out, "Several applications match") {
		t.Fatalf("launch = %q, want the question", out)
	}
	for _, want := range []string{"claude, which runs in a terminal",
		"claude-desktop", "which opens a window", "Do not guess"} {
		if !strings.Contains(out, want) {
			t.Errorf("launch = %q, missing %q", out, want)
		}
	}
	if calls := h.launcher.calls(); len(calls) != 0 {
		t.Errorf("launched %v; the question must come first", calls)
	}
}

// A program nothing on this machine can classify: say what is not known and
// ask, rather than launching hopefully. The harness's applications directory
// is left empty here, which is the condition — with no entries anywhere, "it
// has no entry" is a fact about the search rather than about the program.
func TestAnUnclassifiableProgramIsAskedAboutRatherThanLaunched(t *testing.T) {
	stubApp(t, "somethingorother")
	h := newHarness(t)
	out := h.run(t, LaunchAppToolName, map[string]any{"app": "somethingorother"})
	for _, want := range []string{"I cannot tell whether it opens a window",
		"no application entries", "Ask the user", "do not describe anything as opened"} {
		if !strings.Contains(out, want) {
			t.Errorf("launch = %q, missing %q", out, want)
		}
	}
	if calls := h.launcher.calls(); len(calls) != 0 {
		t.Errorf("launched %v; an unclassified program must not be started", calls)
	}
	if got := h.firedRefusals(); len(got) != 1 ||
		got[0] != "launch:somethingorother:I cannot tell how it starts" {
		t.Errorf("refusals = %v, want the reason on the activity feed", got)
	}
}

// A terminal Jarvix has no spelling for is refused with the reason. Guessing
// -e at it would be the same silent failure with an extra step.
func TestAnUnknownTerminalIsRefusedWithTheReason(t *testing.T) {
	h := newHarnessWith(t, DesktopOptions{Terminal: "st"})
	stubApp(t, "st", "claude")
	h.install(t, "firefox", "Exec=firefox")
	out := h.run(t, LaunchAppToolName, map[string]any{"app": "claude"})
	for _, want := range []string{"claude runs in a terminal",
		"do not know how to run a command inside st", "ghostty"} {
		if !strings.Contains(out, want) {
			t.Errorf("launch = %q, missing %q", out, want)
		}
	}
	if calls := h.launcher.calls(); len(calls) != 0 {
		t.Errorf("launched %v; an unknown terminal must start nothing", calls)
	}
}

// The user's own answer about their own machine outranks the classification.
func TestTheConfiguredOverrideDecidesHowAProgramStarts(t *testing.T) {
	h := newHarnessWith(t, DesktopOptions{})
	stubApp(t, "ghostty", "claude", "firefox")
	h.install(t, "firefox", "Exec=firefox")
	// Rebuilt with the override in force, the way a config reload does it.
	h.d = NewDesktop(DesktopOptions{
		Compositor: h.comp, launcher: h.launcher, Terminal: "ghostty",
		// claude is windowed and firefox needs a terminal, because the user
		// said so — the opposite of what the machine itself suggests.
		Catalogue: launchkindCatalogue(h.appsDir, []string{"claude"}, []string{"firefox"}),
	})
	if out := h.run(t, LaunchAppToolName, map[string]any{"app": "claude"}); !strings.Contains(out,
		"Started claude. ") {
		t.Errorf("launch = %q, want the override to make a command windowed", out)
	}
	if out := h.run(t, LaunchAppToolName, map[string]any{"app": "firefox"}); !strings.Contains(out,
		"Started firefox inside ghostty") {
		t.Errorf("launch = %q, want the override to win over the entry", out)
	}
}

// The confirmation card says which kind, because "open Claude" and "open
// Claude in a terminal" are two different things to say yes to (ADR 0014).
func TestTheConfirmationSaysWhetherATerminalIsInvolved(t *testing.T) {
	h := newHarness(t)
	terminalMachine(t, h, "claude")
	tool, ok := h.tool(t, LaunchAppToolName).(Confirmable)
	if !ok {
		t.Fatal("the launch tool must be confirmable")
	}
	_, summary, ready := tool.Confirmation([]byte(`{"app":"claude"}`))
	if !ready || !strings.Contains(summary, "open claude in a terminal") {
		t.Errorf("confirmation = %q (%v), want the terminal named in the question", summary, ready)
	}
	_, summary, ready = tool.Confirmation([]byte(`{"app":"firefox"}`))
	if !ready || strings.Contains(summary, "terminal") {
		t.Errorf("confirmation = %q (%v), want the unchanged wording for an application",
			summary, ready)
	}
}

// The catalogue the model consults, so "launch Claude" does not rest on its
// idea of what a Linux machine usually has.
func TestTheModelCanReadWhatIsLaunchableAndHow(t *testing.T) {
	h := newHarness(t)
	terminalMachine(t, h, "claude")
	out := h.run(t, ListAppsToolName, map[string]any{"match": "claude"})
	if !strings.Contains(out, "claude — runs in a terminal") {
		t.Errorf("list = %q, want the kind alongside the name", out)
	}
	if !strings.Contains(out, LaunchAppToolName) {
		t.Errorf("list = %q, want it to say what these names are for", out)
	}
	// With no query, the applications — what a person means by "what can you
	// open?".
	if out := h.run(t, ListAppsToolName, nil); !strings.Contains(out, "firefox — opens a window") {
		t.Errorf("list = %q, want this machine's applications", out)
	}
	if out := h.run(t, ListAppsToolName, map[string]any{"match": "nothinglikethis"}); !strings.Contains(
		out, "no programs matching") {
		t.Errorf("list = %q, want an honest empty answer", out)
	}
	if calls := h.launcher.calls(); len(calls) != 0 {
		t.Errorf("launched %v; reading the catalogue starts nothing", calls)
	}
}

// With an allow list configured, the allow list IS the catalogue: offering
// the model a name the launcher would then refuse is inviting a call that
// cannot work.
func TestTheCatalogueNeverOffersWhatTheAllowListForbids(t *testing.T) {
	h := newHarnessWith(t, DesktopOptions{Apps: []string{"firefox"}})
	stubApp(t, "ghostty", "firefox", "claude")
	h.install(t, "firefox", "Exec=firefox")

	out := h.run(t, ListAppsToolName, nil)
	if !strings.Contains(out, "firefox") {
		t.Errorf("list = %q, want the permitted application", out)
	}
	if strings.Contains(out, "claude") {
		t.Errorf("list = %q, must not offer what the launcher would refuse", out)
	}
	if out := h.run(t, ListAppsToolName, map[string]any{"match": "claude"}); !strings.Contains(
		out, "no programs matching") {
		t.Errorf("list = %q, want nothing outside the allow list", out)
	}
}
