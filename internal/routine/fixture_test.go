package routine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/desktopentry"
	"github.com/rpickz/jarvix/internal/placement"
)

// This file is the user's own case, made a fixture (issues #176 and #175): a
// personal browser at two thirds of the top monitor, X above ChatGPT in the
// remaining third at half each, a work browser filling the bottom monitor. It
// is the arrangement they set out to build, could not, and worked around with
// a shell script (~/.local/bin/jarvix-workspace-setup) — so it is the
// arrangement the vocabulary has to be able to express, and the dispatch
// sequence it produces is asserted exactly.
//
// Exactly, not approximately, because the sequence IS the feature. A tiling
// compositor decides where a window lands the moment it maps, from the
// focused window and the preselection standing at that instant, so
// "launch, preselect, launch, preselect, launch, then size" is not one
// possible ordering of the work — it is the only ordering that produces this
// desktop. A test that counted the dispatches, or checked them as a set,
// would pass on a run that opened all three windows in a column.
//
// #175 made the LAUNCHING half real, and this fixture is now the machine's
// actual shape rather than a stand-in for it. Both browsers are `chromium`
// with a different `--profile-directory`; X and ChatGPT are desktop entries
// whose Exec is `omarchy-launch-webapp <url>`, a Chromium `--app=` wrapper,
// and their windows take a class derived from the host. The two profiles are
// given identities because nothing else can tell them apart: Chromium runs
// every profile in one process, so class, PID and /proc/<pid>/cmdline are
// identical for both, and only a class chosen before the launch distinguishes
// them.

// theMonitors is the user's arrangement: a 3440-wide ultrawide above a
// 5120-wide one, each with a 26-pixel bar reserved at the top, and neither
// currently showing the workspace the routine wants on it.
func theMonitors() []placement.Monitor {
	return []placement.Monitor{
		{Name: "HDMI-A-1", X: 840, Y: 0, Width: 3440, Height: 1440, Scale: 1,
			Reserved: [4]int{0, 26, 0, 0}, Focused: true, ActiveWorkspace: 3},
		{Name: "DP-2", X: 0, Y: 1440, Width: 5120, Height: 1440, Scale: 1,
			Reserved: [4]int{0, 26, 0, 0}, ActiveWorkspace: 5},
	}
}

// morningSetup is the routine, written the way the user would write it — the
// four steps that replace every line of their stopgap shell script.
func morningSetup() Definition {
	return Definition{
		Name:    "morning setup",
		Phrases: []string{"morning setup", "good morning jarvix"},
		Steps: []Step{
			// The personal browser: two thirds of the top screen, and the
			// next window goes to its right, which is what leaves the
			// remaining third for the stack. The profile is an ARGUMENT —
			// there is no separate binary for it — and the identity is what
			// makes the window findable afterwards, because the work profile
			// below is the same binary in the same process.
			{App: "chromium", Identity: "personal-browser",
				Args: []string{"--profile-directory=Profile 3", "--restore-last-session"},
				Placement: placement.Placement{
					Workspace: 1, Monitor: "HDMI-A-1", Mode: placement.ModeTiled,
					Width: placement.Percent(66), Height: placement.Percent(100),
					PlaceNext: placement.PlaceNextRight,
				}},
			// X takes the remaining third, and the window after it goes
			// below. It is a desktop entry, not a binary: there is no `x` on
			// PATH, and the entry's Exec is the web-app wrapper. Its window
			// takes a class derived from the site, so the match names that
			// rather than the entry.
			{DesktopEntry: "X", Match: "chrome-x.com", Placement: placement.Placement{
				Workspace: 1, Monitor: "HDMI-A-1", Mode: placement.ModeTiled,
				PlaceNext: placement.PlaceNextBelow,
			}},
			// ChatGPT lands below X and takes half of what they share. The
			// same shape, and the same reason it could not be written before.
			{DesktopEntry: "ChatGPT.desktop", Match: "chrome-chatgpt.com",
				Placement: placement.Placement{
					Workspace: 1, Monitor: "HDMI-A-1", Mode: placement.ModeTiled,
					Height: placement.Percent(50),
				}},
			// The work browser fills the bottom screen on its own: the same
			// binary as step one, a different profile, a different identity.
			{App: "chromium", Identity: "work-browser",
				Args: []string{"--profile-directory=Default", "--restore-last-session"},
				Placement: placement.Placement{
					Workspace: 2, Monitor: "DP-2", Mode: placement.ModeTiled,
				}},
		},
	}
}

// theApplications is the machine the fixture runs against: chromium on PATH,
// and the two web-app desktop entries exactly as this desktop writes them —
// `Exec=omarchy-launch-webapp <url>`, with no binary of their own.
//
// The entries are written to a real directory and read by the real index,
// which is hermetic in the way that matters: nothing is launched, and the
// parser under test is the one the daemon runs.
func theApplications(t *testing.T) *Resolver {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "applications")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, url := range map[string]string{
		"X":       "https://x.com/",
		"ChatGPT": "https://chatgpt.com/",
	} {
		body := "[Desktop Entry]\nVersion=1.0\nName=" + name +
			"\nExec=omarchy-launch-webapp " + url + "\nTerminal=false\nType=Application\n"
		if err := os.WriteFile(filepath.Join(dir, name+".desktop"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	installed := map[string]bool{"chromium": true, "omarchy-launch-webapp": true}
	return &Resolver{
		Entries: desktopentry.Load(dir),
		LookPath: func(name string) (string, error) {
			if installed[name] {
				return "/usr/bin/" + name, nil
			}
			return "", errors.New("not found in $PATH")
		},
	}
}

// theWindows is what the four launches produce on this desktop, verified
// against the running machine: the two Chromium profiles take the class their
// `--class` flag asked for, and the two web apps take the class Chromium
// derives from the host and profile.
func theWindows() []desktop.Window {
	return []desktop.Window{
		{Address: "0xa", Class: "personal-browser", Workspace: 1, Width: 3440, Height: 1414},
		{Address: "0xb", Class: "chrome-x.com__-Profile_3", Workspace: 1, Width: 1170, Height: 1414},
		{Address: "0xc", Class: "chrome-chatgpt.com__-Profile_3", Workspace: 1, Width: 1170, Height: 707},
		{Address: "0xd", Class: "work-browser", Workspace: 2, Width: 5120, Height: 1414},
	}
}

// runMorningSetup drives the fixture against a fake compositor, making each
// launched window appear after one poll — the way a real launch lands while
// the routine is already waiting.
func runMorningSetup(t *testing.T) (*desktop.FakeCompositor, *fakeLauncher, string) {
	t.Helper()
	comp := desktop.NewFakeCompositor()
	comp.Outputs = theMonitors()
	appearing := theWindows()
	r, _, launcher := newTestRunnerOn(comp, []Definition{morningSetup()}, nil, func(poll int) {
		// One window per poll, in step order: the launches are serialised, so
		// step two's window cannot appear before step one's has been placed.
		if poll >= 1 && poll <= len(appearing) {
			comp.SetWindows(appearing[:poll]...)
		}
	}, theApplications(t))
	summary, err := r.Run(context.Background(), "morning setup")
	if err != nil {
		t.Fatal(err)
	}
	return comp, launcher, summary
}

// TestTheMorningSetupProducesItsExactDispatchSequence is the acceptance
// criterion: the user's own example, expressed in the vocabulary, produces
// the placement they asked for.
func TestTheMorningSetupProducesItsExactDispatchSequence(t *testing.T) {
	comp, _, summary := runMorningSetup(t)
	if summary != "Morning setup: all four apps placed." {
		t.Errorf("summary = %q", summary)
	}

	want := []string{
		// Both workspaces are put on their screens before anything launches:
		// a workspace that arrives on the right monitor after its windows
		// have opened has already shown the user the wrong screen, and on a
		// tiling layout it re-tiles them on the way.
		"workspace_monitor", "workspace_monitor",
		// The personal browser. The view goes to its workspace first, because
		// a new window maps onto whatever is in view; then the window is
		// placed as a whole state (out of fullscreen, then tiled); then it is
		// focused and the layout is told the next window goes to its right.
		//
		// There is no "spawn" in this sequence any more, and that is the
		// change #175 made: every step here carries arguments or comes from a
		// desktop entry, and neither can be expressed through a dispatcher
		// that hands its argument to a shell. They are started directly, as
		// an argv — see TestTheMorningSetupLaunchesWhatTheDesktopWould, which
		// pins exactly what each one runs.
		"workspace", "move", "fullscreen", "float", "focus", "preselect",
		// X lands in that preselected space, and preselects downwards itself.
		"workspace", "move", "fullscreen", "float", "focus", "preselect",
		// ChatGPT lands below X. Nothing follows it, so it preselects nothing.
		"workspace", "move", "fullscreen", "float",
		// The work browser, alone on the bottom screen.
		"workspace", "move", "fullscreen", "float",
		// The proportions, LAST — a tiled resize moves the split the window
		// sits in, so it only means anything once the windows it shares that
		// split with exist.
		"focus", "resize", "focus", "resize",
		// And the run ends on the first step's workspace.
		"workspace",
	}
	got := verbs(comp.Actions())
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("dispatches =\n  %v\nwant\n  %v", got, want)
	}
}

// TestTheMorningSetupPutsEachWindowWhereItWasAsked reads the payloads of the
// sequence above: the screens, the preselections, and — the number this whole
// ticket exists for — the two-thirds share, resolved against the top
// monitor's USABLE area rather than its output size.
func TestTheMorningSetupPutsEachWindowWhereItWasAsked(t *testing.T) {
	comp, _, _ := runMorningSetup(t)
	actions := comp.Actions()

	byVerb := func(verb string) []desktop.FakeAction {
		var out []desktop.FakeAction
		for _, a := range actions {
			if a.Verb == verb {
				out = append(out, a)
			}
		}
		return out
	}

	screens := byVerb("workspace_monitor")
	if len(screens) != 2 ||
		screens[0].Workspace != 1 || screens[0].Monitor != "HDMI-A-1" ||
		screens[1].Workspace != 2 || screens[1].Monitor != "DP-2" {
		t.Errorf("workspaces went to %+v", screens)
	}

	arrangement := byVerb("preselect")
	if len(arrangement) != 2 ||
		arrangement[0].Direction != desktop.PreselectRight ||
		arrangement[1].Direction != desktop.PreselectDown {
		t.Errorf("arrangement = %+v, want right then down — two thirds on the left, "+
			"the rest stacked", arrangement)
	}

	sizes := byVerb("resize")
	if len(sizes) != 2 {
		t.Fatalf("resizes = %+v, want the browser's share and ChatGPT's half", sizes)
	}
	// 66% of the top monitor's usable width (3440, the bar takes height only)
	// is 2270; the height is the whole usable 1414, not the output's 1440.
	if sizes[0].Address != "0xa" || sizes[0].Width != 2270 || sizes[0].Height != 1414 {
		t.Errorf("the personal browser was sized %+v, want two thirds of the usable area", sizes[0])
	}
	// ChatGPT named only a height: the width it already has is sent, because
	// the compositor's resize verb wants both numbers and "leave it alone"
	// has to be spelled as the number it already is.
	if sizes[1].Address != "0xc" || sizes[1].Width != 1170 || sizes[1].Height != 707 {
		t.Errorf("ChatGPT was sized %+v, want half the usable height at its own width", sizes[1])
	}
}

// TestTheMorningSetupConvergesOnASecondRun is ADR 0026's set-not-toggle rule
// on the whole fixture: run it again with the windows already open and the
// same desktop comes out — no second copy launched, and every directive still
// a set, so nothing oscillates.
func TestTheMorningSetupConvergesOnASecondRun(t *testing.T) {
	open := theWindows()
	open[0].Width, open[0].Height = 2270, 1414
	comp := desktop.NewFakeCompositor(open...)
	comp.Outputs = theMonitors()
	r, _, launcher := newTestRunnerOn(comp, []Definition{morningSetup()}, nil, nil, theApplications(t))
	if _, err := r.Run(context.Background(), "morning setup"); err != nil {
		t.Fatal(err)
	}
	// Nothing was started, by either route: the compositor's spawn verb was
	// never dispatched and the launcher recorded no argv. Both halves matter
	// now that a step can take either — a re-run that opened a second copy
	// through the new path would be exactly the old defect wearing new
	// clothes.
	if started := launcher.launches(); len(started) != 0 {
		t.Fatalf("a re-run launched %+v; every window was already open", started)
	}
	for _, a := range comp.Actions() {
		if a.Verb == "spawn" {
			t.Fatalf("a re-run launched %q; every window was already open", a.Program)
		}
		if a.Verb == "float" && a.Floating {
			t.Fatalf("a re-run floated a tiled window: %+v", a)
		}
	}
	// The sizes are re-sent, and re-sent exactly: an exact resize applied
	// twice lands in the same place, which is the whole reason the seam never
	// sends a delta.
	var resizes []desktop.FakeAction
	for _, a := range comp.Actions() {
		if a.Verb == "resize" {
			resizes = append(resizes, a)
		}
	}
	if len(resizes) != 2 || resizes[0].Width != 2270 || resizes[0].Height != 1414 {
		t.Errorf("second-run resizes = %+v", resizes)
	}
}

// TestARefusedDispatchIsNotAPlacedStep is issue #177's defect, pinned: the
// compositor declines part of a placement, and the run says so instead of
// counting the step. Before this, a refused resize left the move and the
// float succeeding, and the routine reported the step placed — which is how a
// resize verb that had never worked survived two years of green tests.
func TestARefusedDispatchIsNotAPlacedStep(t *testing.T) {
	comp := desktop.NewFakeCompositor(
		desktop.Window{Address: "0xa", Class: "personal-browser", Workspace: 1, Width: 3440, Height: 1414},
	)
	comp.Outputs = theMonitors()
	// The shape a real refusal takes: hyprctl exits 0 and the compositor
	// explains itself on stdout, so the seam judges the reply (runDispatch).
	comp.FailVerb = map[string]error{
		"resize": errors.New("hyprctl dispatch: unrecognized arguments"),
	}
	def := Definition{Name: "morning setup", Phrases: []string{"morning setup"},
		Steps: []Step{morningSetup().Steps[0]}}
	def.Steps[0].PlaceNext = placement.PlaceNextNone // one step, so nothing follows it
	r, _, _ := newTestRunnerOn(comp, []Definition{def}, nil, nil, theApplications(t))
	summary, err := r.Run(context.Background(), "morning setup")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "could not be sized") {
		t.Errorf("summary = %q, want it to name the resize as what failed", summary)
	}
	if strings.Contains(summary, "placed") && !strings.Contains(summary, "nothing could be placed") {
		t.Errorf("summary = %q, want the step NOT reported as placed", summary)
	}
}

// TestAnUnpluggedMonitorIsNamedAndTheRunContinues is the #180 contract this
// vocabulary already has to honour: a step naming a screen that is not there
// fails with THAT reason, and the rest of the routine still runs.
func TestAnUnpluggedMonitorIsNamedAndTheRunContinues(t *testing.T) {
	comp := desktop.NewFakeCompositor(
		desktop.Window{Address: "0xa", Class: "personal-browser", Workspace: 1, Width: 3440, Height: 1414},
		desktop.Window{Address: "0xd", Class: "work-browser", Workspace: 2, Width: 5120, Height: 1414},
	)
	// The bottom screen was unplugged since the routine was written.
	comp.Outputs = theMonitors()[:1]
	def := morningSetup()
	def.Steps = []Step{def.Steps[0], def.Steps[3]}
	def.Steps[0].PlaceNext = placement.PlaceNextNone
	r, _, _ := newTestRunnerOn(comp, []Definition{def}, nil, nil, theApplications(t))
	summary, err := r.Run(context.Background(), "morning setup")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, `no monitor is called "DP-2" right now`) {
		t.Errorf("summary = %q, want it to name the screen that is not there", summary)
	}
	if !strings.Contains(summary, "HDMI-A-1") {
		t.Errorf("summary = %q, want it to say which screens are plugged in", summary)
	}
	if !strings.Contains(summary, "one app placed") {
		t.Errorf("summary = %q, want the other step still placed", summary)
	}
}

// TestTheMorningSetupLaunchesWhatTheDesktopWould is issue #175's acceptance
// criterion on the same fixture: the four steps produce the four argvs the
// user's shell script produces, and nothing in any of them is a command line.
//
// Read the expected argv as the answer to the ticket. The profile is an
// argument containing a SPACE and arrives as one element. The two web apps
// resolve through their desktop entries to the wrapper the entry names, with
// the URL as its own argument. The identity flag is appended by the routine,
// not written by the user, and it is what makes the two Chromium windows
// distinguishable at all.
func TestTheMorningSetupLaunchesWhatTheDesktopWould(t *testing.T) {
	_, launcher, _ := runMorningSetup(t)

	want := [][]string{
		{"/usr/bin/chromium", "--profile-directory=Profile 3", "--restore-last-session",
			"--class=personal-browser"},
		{"/usr/bin/omarchy-launch-webapp", "https://x.com/"},
		{"/usr/bin/omarchy-launch-webapp", "https://chatgpt.com/"},
		{"/usr/bin/chromium", "--profile-directory=Default", "--restore-last-session",
			"--class=work-browser"},
	}
	got := launcher.launches()
	if len(got) != len(want) {
		t.Fatalf("launched %d things, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if !slices.Equal(got[i].Argv, want[i]) {
			t.Errorf("step %d launched\n  %q\nwant\n  %q", i+1, got[i].Argv, want[i])
		}
	}
}

// TestTheMorningSetupIsExpressibleAsConfiguredTOML closes the loop the ticket
// opened: the routine above is not a Go literal a test invented, it is what
// the user writes in config.toml, and it loads with no problems.
//
// This is the fixture the ticket asked for — the whole morning layout,
// expressible, validated by the real loader. The stopgap script at
// ~/.local/bin/jarvix-workspace-setup has no line this does not say.
func TestTheMorningSetupIsExpressibleAsConfiguredTOML(t *testing.T) {
	const written = `
[[routines]]
name = "morning setup"
phrases = ["morning setup", "good morning jarvix"]

  [[routines.steps]]
  app = "chromium"
  args = ["--profile-directory=Profile 3", "--restore-last-session"]
  identity = "personal-browser"
  workspace = 1
  monitor = "HDMI-A-1"
  mode = "tiled"
  width = "66%"
  height = "100%"
  place_next = "right"

  [[routines.steps]]
  desktop_entry = "X"
  match = "chrome-x.com"
  workspace = 1
  monitor = "HDMI-A-1"
  mode = "tiled"
  place_next = "below"

  [[routines.steps]]
  desktop_entry = "ChatGPT.desktop"
  match = "chrome-chatgpt.com"
  workspace = 1
  monitor = "HDMI-A-1"
  mode = "tiled"
  height = "50%"

  [[routines.steps]]
  app = "chromium"
  args = ["--profile-directory=Default", "--restore-last-session"]
  identity = "work-browser"
  workspace = 2
  monitor = "DP-2"
  mode = "tiled"
`
	var doc struct {
		Routines []struct {
			Name    string `toml:"name"`
			Phrases []string
			Steps   []struct {
				App          string   `toml:"app"`
				DesktopEntry string   `toml:"desktop_entry"`
				Args         []string `toml:"args"`
				Identity     string   `toml:"identity"`
				Match        string   `toml:"match"`
			} `toml:"steps"`
		} `toml:"routines"`
	}
	if err := toml.Unmarshal([]byte(written), &doc); err != nil {
		t.Fatalf("the worked example does not parse: %v", err)
	}
	if len(doc.Routines) != 1 || len(doc.Routines[0].Steps) != 4 {
		t.Fatalf("parsed %+v", doc)
	}
	// The launching half, read back off the file, is the fixture's own.
	def := morningSetup()
	for i, s := range doc.Routines[0].Steps {
		want := def.Steps[i]
		if s.App != want.App || s.DesktopEntry != want.DesktopEntry ||
			s.Identity != want.Identity || s.Match != want.Match ||
			!slices.Equal(s.Args, want.Args) {
			t.Errorf("step %d read back as %+v, want the fixture's %+v", i, s, want)
		}
	}
	// And it validates — against the machine's entries, so a missing entry is
	// caught here rather than eight seconds into a run.
	resolver := theApplications(t)
	if problems := ProblemsWith([]Definition{def}, *resolver); len(problems) != 0 {
		t.Errorf("the user's whole morning layout is refused: %v", problems)
	}
	if problems := InstallProblems(def, *resolver); len(problems) != 0 {
		t.Errorf("the user's whole morning layout cannot be launched here: %+v", problems)
	}
}
