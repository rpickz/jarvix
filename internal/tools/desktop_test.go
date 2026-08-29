package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/launchkind"
)

// Every test here runs against a fake compositor and a fake launcher: nothing
// in this file needs a running Hyprland, a Wayland session, or anything on
// screen, and nothing it does could act on the machine it runs on.

// fakeLauncher records what would have been executed.
type fakeLauncher struct {
	mu       sync.Mutex
	launched []string
	err      error
}

// Launch records the whole argv, joined for readability. Joined here and
// nowhere else: what a test asserts on is the elements it was handed, and
// nothing in the daemon ever turns an argv back into a command line.
func (f *fakeLauncher) Launch(_ context.Context, argv []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launched = append(f.launched, strings.Join(argv, " "))
	return f.err
}

func (f *fakeLauncher) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.launched...)
}

// testWindows is a small desktop: a focused editor, two firefox windows, a
// terminal.
func testWindows() []desktop.Window {
	return []desktop.Window{
		{Address: "0x1", Class: "code", Title: "engine.go", Workspace: 1, WorkspaceName: "1",
			Focused: true, StableID: "s1", AcceptsInput: true},
		{Address: "0x2", Class: "firefox", Title: "GitHub", Workspace: 1, WorkspaceName: "1",
			StableID: "s2", AcceptsInput: true},
		{Address: "0x3", Class: "firefox", Title: "Fastmail", Workspace: 2, WorkspaceName: "2",
			StableID: "s3", AcceptsInput: true},
		{Address: "0x4", Class: "Alacritty", Title: "go test", Workspace: 2, WorkspaceName: "2",
			StableID: "s4", AcceptsInput: true},
	}
}

type harness struct {
	d        *Desktop
	comp     *desktop.FakeCompositor
	launcher *fakeLauncher
	// appsDir is this test's applications directory. Every harness gets one,
	// empty, so a launch test sees the machine it wrote and never the one it
	// is running on — the desktop entries a launch classifies from are as
	// much a fixture as the window list is.
	appsDir  string
	events   []string
	refusals []string
	mu       sync.Mutex
}

func newHarness(t *testing.T, windows ...desktop.Window) *harness {
	t.Helper()
	if len(windows) == 0 {
		windows = testWindows()
	}
	h := &harness{
		comp:     desktop.NewFakeCompositor(windows...),
		launcher: &fakeLauncher{},
		appsDir:  t.TempDir(),
	}
	h.d = NewDesktop(DesktopOptions{
		Compositor: h.comp,
		launcher:   h.launcher,
		Catalogue:  h.catalogue(nil),
		Terminal:   "ghostty",
		OnAction: func(verb, target string) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.events = append(h.events, verb+":"+target)
		},
		OnRefusal: func(verb, target, reason string) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.refusals = append(h.refusals, verb+":"+target+":"+reason)
		},
	})
	return h
}

// catalogue builds the launch catalogue over this harness's own machine: the
// applications directory it owns, and whatever PATH the test set up.
func (h *harness) catalogue(lookPath func(string) (string, error)) *launchkind.Catalogue {
	dir := h.appsDir
	return launchkind.New(launchkind.Options{
		EntryDirs: func() []string { return []string{dir} },
		PathDirs:  func() []string { return filepath.SplitList(os.Getenv("PATH")) },
		LookPath:  lookPath,
	})
}

// launchkindCatalogue builds a catalogue over one applications directory and
// the live PATH, with the user's overrides applied — the shape a config
// reload produces.
func launchkindCatalogue(appsDir string, windowed, terminal []string) *launchkind.Catalogue {
	return launchkind.New(launchkind.Options{
		EntryDirs:         func() []string { return []string{appsDir} },
		PathDirs:          func() []string { return filepath.SplitList(os.Getenv("PATH")) },
		TerminalPrograms:  terminal,
		GraphicalPrograms: windowed,
	})
}

// newHarnessWith builds the window tools with extra options, over the same
// hermetic machine every harness gets: its own fake compositor, its own fake
// launcher, its own applications directory, and a terminal that is a fixture
// rather than whatever this computer happens to have configured.
func newHarnessWith(t *testing.T, opts DesktopOptions) *harness {
	t.Helper()
	h := &harness{
		comp:     desktop.NewFakeCompositor(testWindows()...),
		launcher: &fakeLauncher{},
		appsDir:  t.TempDir(),
	}
	opts.Compositor, opts.launcher = h.comp, h.launcher
	if opts.Catalogue == nil {
		opts.Catalogue = h.catalogue(opts.lookPath)
	}
	if opts.Terminal == "" {
		opts.Terminal = "ghostty"
	}
	h.d = NewDesktop(opts)
	return h
}

// install writes one desktop entry into the harness's applications directory.
// A real file read by the real parser, which is hermetic in the way that
// matters: nothing is launched, and the index under test is the one the
// daemon runs.
func (h *harness) install(t *testing.T, id string, keys ...string) {
	t.Helper()
	body := "[Desktop Entry]\nType=Application\nName=" + id + "\n" +
		strings.Join(keys, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(h.appsDir, id+".desktop"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) firedRefusals() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.refusals...)
}

func (h *harness) tool(t *testing.T, name string) Tool {
	t.Helper()
	for _, tool := range h.d.Tools() {
		if tool.Name() == name {
			return tool
		}
	}
	t.Fatalf("no tool named %q", name)
	return nil
}

// run executes one tool with the given arguments and returns the model-facing
// result, failing on an infrastructure error.
func (h *harness) run(t *testing.T, name string, args map[string]any) string {
	t.Helper()
	input, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	out, err := h.tool(t, name).Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("%s(%v): %v", name, args, err)
	}
	// Nothing a tool hands back may carry a window address: the model reads
	// these results out loud.
	if strings.Contains(out, "0x") {
		t.Errorf("%s result leaks an address: %q", name, out)
	}
	return out
}

func (h *harness) firedEvents() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.events...)
}

func TestListWindowsSummarisesWithoutIdentifiers(t *testing.T) {
	h := newHarness(t)
	out := h.run(t, ListWindowsToolName, nil)
	for _, want := range []string{"4 windows are open", "firefox", "Alacritty", "workspace 2"} {
		if !strings.Contains(out, want) {
			t.Errorf("list = %q, missing %q", out, want)
		}
	}
	if got := h.firedEvents(); len(got) != 1 || got[0] != "list:4 windows" {
		t.Errorf("events = %v", got)
	}
}

func TestListWindowsOnAnEmptyDesktop(t *testing.T) {
	h := newHarness(t, desktop.Window{Address: "", Class: "", Title: ""})
	h.comp.SetWindows()
	if out := h.run(t, ListWindowsToolName, nil); !strings.Contains(out, "Nothing is open") {
		t.Errorf("list = %q", out)
	}
}

func TestFocusDispatchesTheResolvedAddress(t *testing.T) {
	h := newHarness(t)
	out := h.run(t, FocusWindowToolName, map[string]any{"window": "alacritty"})
	if !strings.Contains(out, "Switched to Alacritty") {
		t.Errorf("focus = %q", out)
	}
	action, ok := h.comp.LastAction()
	if !ok || action.Verb != "focus" || action.Address != "0x4" {
		t.Errorf("action = %+v, want a focus of 0x4", action)
	}
	if got := h.firedEvents(); len(got) != 1 || !strings.HasPrefix(got[0], "focus:Alacritty") {
		t.Errorf("events = %v", got)
	}
}

func TestFocusWithNoWindowNamedTakesTheFocusedOne(t *testing.T) {
	h := newHarness(t)
	h.run(t, FocusWindowToolName, map[string]any{})
	action, _ := h.comp.LastAction()
	if action.Address != "0x1" {
		t.Errorf("action = %+v, want the focused window", action)
	}
}

func TestAmbiguousReferenceAsksInsteadOfGuessing(t *testing.T) {
	h := newHarness(t)
	out := h.run(t, FocusWindowToolName, map[string]any{"window": "firefox"})
	for _, want := range []string{"Several windows match", "GitHub", "Fastmail", "Do not guess"} {
		if !strings.Contains(out, want) {
			t.Errorf("result = %q, missing %q", out, want)
		}
	}
	if _, acted := h.comp.LastAction(); acted {
		t.Error("an ambiguous reference must never be dispatched")
	}
}

func TestNoMatchIsSpeakableAndNotAnError(t *testing.T) {
	h := newHarness(t)
	out := h.run(t, FocusWindowToolName, map[string]any{"window": "photoshop"})
	if !strings.Contains(out, "No window matches") {
		t.Errorf("result = %q", out)
	}
	if _, acted := h.comp.LastAction(); acted {
		t.Error("nothing may be dispatched when nothing matched")
	}
}

func TestCompositorUnavailableIsSpeakable(t *testing.T) {
	h := newHarness(t)
	h.comp.Err = desktop.ErrNoCompositor
	for _, name := range []string{ListWindowsToolName, FocusWindowToolName, CloseWindowToolName} {
		out := h.run(t, name, map[string]any{"window": "firefox", "workspace": 1})
		if !strings.Contains(out, "window manager is not available") {
			t.Errorf("%s = %q", name, out)
		}
	}
}

func TestMoveChecksTheWorkspaceRange(t *testing.T) {
	h := newHarness(t)
	// Zero is no longer out of range — it means "leave the window on the
	// workspace it is on", which is legitimate once a call can also name a
	// monitor or a mode. A call that says NOTHING is still refused, and
	// without dispatching, which is what the last assertion of the loop pins.
	out := h.run(t, MoveWindowToolName, map[string]any{"window": "alacritty", "workspace": 0})
	if !strings.Contains(out, "said nothing about where the window should go") {
		t.Errorf("empty placement = %q", out)
	}
	for _, ws := range []int{-1, 100, 1 << 20} {
		out := h.run(t, MoveWindowToolName, map[string]any{"window": "alacritty", "workspace": ws})
		if !strings.Contains(out, "does not exist") {
			t.Errorf("workspace %d = %q", ws, out)
		}
		if _, acted := h.comp.LastAction(); acted {
			t.Fatalf("workspace %d was dispatched", ws)
		}
	}
	out = h.run(t, MoveWindowToolName, map[string]any{"window": "alacritty", "workspace": 3})
	if !strings.Contains(out, "Moved Alacritty") {
		t.Errorf("move = %q", out)
	}
	action, _ := h.comp.LastAction()
	if action.Verb != "move" || action.Address != "0x4" || action.Workspace != 3 {
		t.Errorf("action = %+v", action)
	}
}

func TestCloseDispatchesAndReportsARefusal(t *testing.T) {
	h := newHarness(t)
	if out := h.run(t, CloseWindowToolName, map[string]any{"window": "go test"}); !strings.Contains(out, "Closed Alacritty") {
		t.Errorf("close = %q", out)
	}
	action, _ := h.comp.LastAction()
	if action.Verb != "close" || action.Address != "0x4" {
		t.Errorf("action = %+v", action)
	}

	h.comp.FailAction = desktop.ErrNoCompositor
	out := h.run(t, CloseWindowToolName, map[string]any{"window": "github"})
	if !strings.Contains(out, "would not close") {
		t.Errorf("refused close = %q", out)
	}
}

// The heart of the design: what was resolved is what is dispatched. A window
// appearing, closing, or being renamed between resolution and dispatch can
// make an action fail, but can never redirect it.
func TestResolvedAddressIsNeverRedirectedByAChangedInventory(t *testing.T) {
	t.Run("a new better match does not steal the dispatch", func(t *testing.T) {
		h := newHarness(t)
		tool := h.tool(t, CloseWindowToolName).(*windowTool)
		args := json.RawMessage(`{"window":"go test"}`)

		// Resolution happens while the confirmation question is built.
		command, summary, ok := tool.Confirmation(args)
		if !ok || !strings.Contains(summary, "Alacritty") || !strings.Contains(command, "Alacritty") {
			t.Fatalf("confirmation = %q / %q / %v", command, summary, ok)
		}
		// While the user is answering, a new window appears that would match
		// the same words better.
		h.comp.SetWindows(append(testWindows(), desktop.Window{
			Address: "0x9", Class: "go test", Title: "go test", Workspace: 1, StableID: "s9"})...)

		out, err := tool.Execute(context.Background(), args)
		if err != nil {
			t.Fatal(err)
		}
		action, _ := h.comp.LastAction()
		if action.Address != "0x4" {
			t.Fatalf("dispatched %s (%q), want the window the user was asked about", action.Address, out)
		}
	})

	t.Run("a window that has gone is refused, not re-resolved", func(t *testing.T) {
		h := newHarness(t)
		tool := h.tool(t, CloseWindowToolName).(*windowTool)
		args := json.RawMessage(`{"window":"go test"}`)
		if _, _, ok := tool.Confirmation(args); !ok {
			t.Fatal("expected a resolution")
		}
		// The window closes itself while the user is answering.
		h.comp.SetWindows(testWindows()[:3]...)

		out, err := tool.Execute(context.Background(), args)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "no longer there") {
			t.Errorf("result = %q", out)
		}
		if _, acted := h.comp.LastAction(); acted {
			t.Error("a window that has gone must not be dispatched to")
		}
	})

	t.Run("a reused address is not the same window", func(t *testing.T) {
		h := newHarness(t)
		tool := h.tool(t, CloseWindowToolName).(*windowTool)
		args := json.RawMessage(`{"window":"go test"}`)
		if _, _, ok := tool.Confirmation(args); !ok {
			t.Fatal("expected a resolution")
		}
		// The terminal closes and something else is allocated its address —
		// the failure mode an address alone cannot see.
		reused := testWindows()[:3]
		reused = append(reused, desktop.Window{Address: "0x4", Class: "banking",
			Title: "Transfer £5000", Workspace: 2, StableID: "s9"})
		h.comp.SetWindows(reused...)

		out, err := tool.Execute(context.Background(), args)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "no longer there") {
			t.Errorf("result = %q", out)
		}
		if _, acted := h.comp.LastAction(); acted {
			t.Error("a recycled address must never inherit an approval")
		}
	})

	t.Run("the inventory moving under a direct call cannot redirect it", func(t *testing.T) {
		h := newHarness(t)
		// Swap the whole desktop out at the moment of dispatch: the address
		// was captured before, and it is all that is used.
		h.comp.BeforeAction = func(string) {
			h.comp.SetWindows(desktop.Window{Address: "0x8", Class: "Alacritty",
				Title: "go test", Workspace: 1, StableID: "s8"})
		}
		h.run(t, FocusWindowToolName, map[string]any{"window": "alacritty"})
		action, _ := h.comp.LastAction()
		if action.Address != "0x4" {
			t.Errorf("dispatched %s, want the address resolved before the change", action.Address)
		}
	})
}

func TestConfirmationNamesTheWindowFromTheInventory(t *testing.T) {
	h := newHarness(t)
	tests := []struct {
		tool          string
		args          string
		wantInSummary []string
	}{
		{CloseWindowToolName, `{"window":"github"}`, []string{"close firefox", "titled GitHub", "Should I go ahead?"}},
		{MoveWindowToolName, `{"window":"github","workspace":4}`, []string{"move firefox", "workspace 4"}},
	}
	for _, tt := range tests {
		tool := h.tool(t, tt.tool).(*windowTool)
		command, summary, ok := tool.Confirmation(json.RawMessage(tt.args))
		if !ok {
			t.Fatalf("%s: no confirmation offered", tt.tool)
		}
		for _, want := range tt.wantInSummary {
			if !strings.Contains(summary, want) {
				t.Errorf("%s summary = %q, missing %q", tt.tool, summary, want)
			}
		}
		if strings.Contains(summary, "0x") || strings.Contains(command, "0x") {
			t.Errorf("%s confirmation leaks an address: %q / %q", tt.tool, command, summary)
		}
	}
}

func TestConfirmationDeclinesToInventWhenNothingResolves(t *testing.T) {
	h := newHarness(t)
	tool := h.tool(t, CloseWindowToolName).(*windowTool)
	for _, args := range []string{`{"window":"photoshop"}`, `{"window":"firefox"}`, `not json`} {
		if _, _, ok := tool.Confirmation(json.RawMessage(args)); ok {
			t.Errorf("Confirmation(%s) claimed a window; no match and an ambiguity have none", args)
		}
	}
}

// The read verbs are never asked about, so they must not resolve anything —
// a Confirmation call for them would be a compositor round trip inside the
// permission gate for nothing.
func TestReadVerbsOfferNoConfirmation(t *testing.T) {
	h := newHarness(t)
	for _, name := range []string{ListWindowsToolName, FocusWindowToolName} {
		if _, _, ok := h.tool(t, name).(*windowTool).Confirmation(json.RawMessage(`{"window":"firefox"}`)); ok {
			t.Errorf("%s offered a confirmation", name)
		}
	}
	if h.comp.Reads() != 0 {
		t.Errorf("reads = %d, want the compositor left alone", h.comp.Reads())
	}
}

func TestInventoryIsCapturedOncePerTurn(t *testing.T) {
	h := newHarness(t)
	h.run(t, ListWindowsToolName, nil)
	h.run(t, FocusWindowToolName, map[string]any{"window": "alacritty"})
	// List, then resolve, then verify before dispatch: one capture, reused.
	if got := h.comp.Reads(); got != 1 {
		t.Errorf("inventory captured %d times in one turn, want 1", got)
	}
}

func TestArgumentsThatAreNotJSONAreAProgrammingError(t *testing.T) {
	h := newHarness(t)
	if _, err := h.tool(t, FocusWindowToolName).Execute(context.Background(), json.RawMessage(`nonsense`)); err == nil {
		t.Error("malformed arguments must be an error")
	}
	// An absent argument object is not malformed: it means "this window".
	out, err := h.tool(t, FocusWindowToolName).Execute(context.Background(), nil)
	if err != nil || !strings.Contains(out, "Switched to") {
		t.Errorf("empty arguments: %q, %v", out, err)
	}
}

// stubApp builds a PATH containing exactly the named executables, so the
// launch tests see the same desktop everywhere: what is installed on the
// machine running them must not decide whether "open a browser" is ambiguous.
func stubApp(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	return dir
}

func TestLaunchResolvesThroughPath(t *testing.T) {
	dir := stubApp(t, "jarvix-test-app")
	h := newHarness(t)
	// An application: it ships a desktop entry, so it opens a window of its
	// own and is started directly.
	h.install(t, "jarvix-test-app", "Exec=jarvix-test-app")
	out := h.run(t, LaunchAppToolName, map[string]any{"app": "jarvix-test-app"})
	if !strings.Contains(out, "Started jarvix-test-app") {
		t.Errorf("launch = %q", out)
	}
	if got := h.launcher.calls(); len(got) != 1 || got[0] != filepath.Join(dir, "jarvix-test-app") {
		t.Errorf("launched %v, want the resolved absolute path", got)
	}
	if got := h.firedEvents(); len(got) != 1 || got[0] != "launch:jarvix-test-app" {
		t.Errorf("events = %v", got)
	}
}

func TestLaunchRefusesAnythingThatIsNotAProgramName(t *testing.T) {
	stubApp(t, "jarvix-test-app")
	h := newHarness(t)
	for _, app := range []string{
		"jarvix-test-app; rm -rf /", "jarvix-test-app && id", "/bin/sh", "../../bin/sh",
		"$(id)", "echo hi", "app`id`", "-rf",
	} {
		out := h.run(t, LaunchAppToolName, map[string]any{"app": app})
		if !strings.Contains(out, "cannot be started") {
			t.Errorf("launch(%q) = %q, want a refusal", app, out)
		}
	}
	if got := h.launcher.calls(); len(got) != 0 {
		t.Errorf("launched %v; none of those may run", got)
	}
}

func TestLaunchHonoursTheAllowList(t *testing.T) {
	dir := stubApp(t, "jarvix-allowed", "jarvix-forbidden")
	h := newHarnessWith(t, DesktopOptions{Apps: []string{"jarvix-allowed"}})
	h.install(t, "jarvix-allowed", "Exec=jarvix-allowed")
	h.install(t, "jarvix-forbidden", "Exec=jarvix-forbidden")
	if out := h.run(t, LaunchAppToolName, map[string]any{"app": "jarvix-forbidden"}); !strings.Contains(out, "not on the allowed list") {
		t.Errorf("forbidden = %q", out)
	}
	if out := h.run(t, LaunchAppToolName, map[string]any{"app": "jarvix-allowed"}); !strings.Contains(out, "Started") {
		t.Errorf("allowed = %q", out)
	}
	if got := h.launcher.calls(); len(got) != 1 || got[0] != filepath.Join(dir, "jarvix-allowed") {
		t.Errorf("launched %v", got)
	}
}

func TestLaunchAsksWhenACategoryMatchesSeveralInstalledApps(t *testing.T) {
	stubApp(t, "firefox", "chromium")
	h := newHarness(t)
	h.install(t, "firefox", "Exec=firefox")
	h.install(t, "chromium", "Exec=chromium")
	out := h.run(t, LaunchAppToolName, map[string]any{"app": "browser"})
	if !strings.Contains(out, "Several applications match") || !strings.Contains(out, "firefox") {
		t.Errorf("launch = %q", out)
	}
	if got := h.launcher.calls(); len(got) != 0 {
		t.Errorf("launched %v, want the question asked first", got)
	}
}

func TestLaunchResolvesACategoryWithOneInstalledApp(t *testing.T) {
	stubApp(t, "jarvix-test-app", "logseq")
	h := newHarness(t)
	h.install(t, "logseq", "Exec=logseq")
	if out := h.run(t, LaunchAppToolName, map[string]any{"app": "notes"}); !strings.Contains(out, "Started logseq") {
		t.Errorf("launch = %q", out)
	}
}

func TestLaunchNeedsAnApplication(t *testing.T) {
	h := newHarness(t)
	if _, err := h.tool(t, LaunchAppToolName).Execute(context.Background(), json.RawMessage(`{"app":"  "}`)); err == nil {
		t.Error("an empty application name must be an error")
	}
}

// nearMatchHarness builds the window tools over a canned PATH, through the
// injectable lookPath — the same seam routine capture uses — so what is
// installed on the machine running the tests decides nothing.
func nearMatchHarness(t *testing.T, installed []string, apps ...string) *harness {
	t.Helper()
	onPath := make(map[string]bool, len(installed))
	for _, name := range installed {
		onPath[name] = true
	}
	return newHarnessWith(t, DesktopOptions{
		Apps: apps,
		lookPath: func(name string) (string, error) {
			if onPath[name] {
				return "/usr/bin/" + name, nil
			}
			return "", errors.New(name + ": executable file not found in $PATH")
		},
	})
}

// TestLaunchRefusalNamesAnInstalledNearMatch pins issue #71's recovery: a
// refusal for a program that is not installed names an installed near-match
// when one exists — the alias table, the shared "-desktop" convention, or a
// category sibling — and stays quiet when nothing near is installed, so the
// suggestion is always something that would actually launch.
func TestLaunchRefusalNamesAnInstalledNearMatch(t *testing.T) {
	tests := []struct {
		name      string
		app       string
		installed []string
		apps      []string // launch allow list; empty allows anything
		want      []string // fragments the refusal must carry
		quiet     bool     // plain refusal: no suggestion offered
	}{
		{name: "alias: chrome finds chromium (the live incident's hint)",
			app: "chrome", installed: []string{"chromium"},
			want: []string{`"chrome" is not installed, but chromium is`, "call this tool again"}},
		{name: "alias: google-chrome finds the -stable packaging",
			app: "google-chrome", installed: []string{"google-chrome-stable"},
			want: []string{"google-chrome-stable is"}},
		{name: "alias: code finds code-oss (which the editor category does not list)",
			app: "code", installed: []string{"code-oss"},
			want: []string{"code-oss is"}},
		{name: "packaging convention: signal finds signal-desktop",
			app: "signal", installed: []string{"signal-desktop"},
			want: []string{"signal-desktop is"}},
		{name: "packaging convention alone: a name in no table still finds its -desktop binary",
			app: "vesktop", installed: []string{"vesktop-desktop"},
			want: []string{"vesktop-desktop is"}},
		{name: "category sibling: firefox finds chromium (the live refusal)",
			app: "firefox", installed: []string{"chromium"},
			want: []string{`"firefox" is not installed, but chromium is`}},
		{name: "several near-matches are named together",
			app: "chrome", installed: []string{"chromium", "google-chrome-stable"},
			want: []string{"chromium", "google-chrome-stable", "are", "one of them"}},
		{name: "allow list: the suggestion is permitted",
			app: "chrome", installed: []string{"chromium"}, apps: []string{"chrome", "chromium"},
			want: []string{"chromium is"}},
		{name: "quiet: nothing near is installed",
			app: "chrome", installed: []string{"gimp"}, quiet: true},
		{name: "quiet: an unknown name has no near-match to offer",
			app: "hopfenschnitzel", installed: []string{"chromium"}, quiet: true},
		{name: "quiet: a suggestion outside the allow list is never offered",
			app: "chrome", installed: []string{"chromium"}, apps: []string{"chrome", "firefox"},
			quiet: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := nearMatchHarness(t, tt.installed, tt.apps...)
			out := h.run(t, LaunchAppToolName, map[string]any{"app": tt.app})
			if tt.quiet {
				if !strings.Contains(out, "cannot be started") || strings.Contains(out, "instead") {
					t.Errorf("launch(%q) = %q, want a plain refusal with no suggestion", tt.app, out)
				}
			} else {
				for _, want := range tt.want {
					if !strings.Contains(out, want) {
						t.Errorf("launch(%q) = %q, missing %q", tt.app, out, want)
					}
				}
				if !strings.Contains(out, "only if they say yes") {
					t.Errorf("launch(%q) = %q; the suggestion must route through the user", tt.app, out)
				}
			}
			if got := h.launcher.calls(); len(got) != 0 {
				t.Errorf("launched %v; a refusal must start nothing", got)
			}
		})
	}
}

// TestInstalledNearMatchesAreBoundedAndDeduplicated: the refusal is one
// spoken sentence, so the suggestions are capped and never repeat.
func TestInstalledNearMatchesAreBoundedAndDeduplicated(t *testing.T) {
	h := nearMatchHarness(t, []string{"chromium", "google-chrome-stable", "google-chrome",
		"firefox", "brave", "vivaldi"})
	got := h.d.installedNearMatches("chrome")
	// Pinned at three, not at the constant: the bound is the spoken-sentence
	// budget, and a test that compares the cap to itself checks nothing.
	if len(got) > 3 {
		t.Errorf("near matches = %v, want at most 3", got)
	}
	seen := map[string]bool{}
	for _, name := range got {
		if seen[name] {
			t.Errorf("near matches repeat %q: %v", name, got)
		}
		seen[name] = true
		if name == "chrome" {
			t.Errorf("near matches suggest the very name that failed: %v", got)
		}
	}
}

// The gate's tiers are a fact about the registry: the reads run silently and
// the changes ask, without either being a special case inside a tool.
func TestPolicyTiersForTheWindowTools(t *testing.T) {
	h := newHarness(t)
	registry := NewRegistry(nil)
	for _, tool := range h.d.Tools() {
		registry.Register(tool)
	}
	policy, err := NewPolicy(PolicyConfig{})
	if err != nil {
		t.Fatal(err)
	}
	registry.SetPolicy(policy)

	tests := []struct {
		tool string
		args string
		want PolicyDecision
	}{
		{ListWindowsToolName, `{}`, PolicyAllow},
		{FocusWindowToolName, `{"window":"firefox"}`, PolicyAllow},
		{MoveWindowToolName, `{"window":"github","workspace":3}`, PolicyAsk},
		{CloseWindowToolName, `{"window":"github"}`, PolicyAsk},
		{LaunchAppToolName, `{"app":"firefox"}`, PolicyAsk},
	}
	for _, tt := range tests {
		verdict := registry.Check(ai.ToolCall{Name: tt.tool, Arguments: tt.args})
		if verdict.Decision != tt.want {
			t.Errorf("%s decided %q, want %q", tt.tool, verdict.Decision, tt.want)
		}
	}

	// The ask tier's question is built from the inventory, not from the
	// model's arguments: it names the window that is actually about to close.
	verdict := registry.Check(ai.ToolCall{Name: CloseWindowToolName,
		Arguments: `{"window":"github"}`})
	if !strings.Contains(verdict.Summary, "firefox") || !strings.Contains(verdict.Summary, "GitHub") {
		t.Errorf("summary = %q, want the resolved window named", verdict.Summary)
	}
	if verdict.Command != "close firefox — GitHub" {
		t.Errorf("command = %q", verdict.Command)
	}
}

// A user who disagrees with a default says so in configuration, and that is
// the whole mechanism — there is no second one.
func TestPolicyOverridesApplyToTheWindowTools(t *testing.T) {
	policy, err := NewPolicy(PolicyConfig{Tools: map[string]PolicyDecision{
		CloseWindowToolName: PolicyDeny,
		MoveWindowToolName:  PolicyAllow,
		FocusWindowToolName: PolicyAsk,
	}})
	if err != nil {
		t.Fatal(err)
	}
	for tool, want := range map[string]PolicyDecision{
		CloseWindowToolName: PolicyDeny,
		MoveWindowToolName:  PolicyAllow,
		FocusWindowToolName: PolicyAsk,
		ListWindowsToolName: PolicyAllow,
	} {
		if got := policy.ToolDecision(tool); got != want {
			t.Errorf("%s = %q, want %q", tool, got, want)
		}
	}
}

func TestToolSchemasAreValidJSON(t *testing.T) {
	h := newHarness(t)
	for _, tool := range h.d.Tools() {
		var schema map[string]any
		if err := json.Unmarshal(tool.Schema(), &schema); err != nil {
			t.Errorf("%s schema: %v", tool.Name(), err)
		}
		if tool.Description() == "" {
			t.Errorf("%s has no description", tool.Name())
		}
	}
	if got := len(h.d.Names()); got != 7 {
		t.Errorf("Names() = %v", h.d.Names())
	}
}

func TestLaunchSchemaNamesTheAllowList(t *testing.T) {
	d := NewDesktop(DesktopOptions{Compositor: desktop.NewFakeCompositor(), Apps: []string{"firefox", "spotify"}})
	for _, tool := range d.Tools() {
		if tool.Name() != LaunchAppToolName {
			continue
		}
		if !strings.Contains(string(tool.Schema()), "spotify") {
			t.Errorf("schema = %s, want the allowed applications named", tool.Schema())
		}
	}
}
