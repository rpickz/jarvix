package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/routine"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/tts"
)

// These tests drive the capture service (#62) against a FakeCompositor and a
// temp-dir config file — never the developer's desktop or config. They cover
// the composition the unit tests cannot: plan → (replace) → atomic write →
// provenance on disk → runnable entry, including the round-trip acceptance
// criterion: a captured routine, run on the same fake, converges to the
// captured layout.

// captureLookPath fakes binary resolution for the daemon-side tests.
func captureLookPath(installed ...string) func(string) (string, error) {
	set := make(map[string]bool, len(installed))
	for _, name := range installed {
		set[name] = true
	}
	return func(name string) (string, error) {
		if set[name] {
			return "/usr/bin/" + name, nil
		}
		return "", fmt.Errorf("%s: not found", name)
	}
}

// newTestCapturer builds the service over a temp config dir and a canned
// clock. existing, when not empty, is written as the pre-existing config.toml.
func newTestCapturer(t *testing.T, comp *desktop.FakeCompositor, existing string,
	installed ...string) (*layoutCapturer, string) {
	t.Helper()
	dir := t.TempDir()
	paths := config.Paths{Config: dir, Data: dir, State: dir}
	path := paths.ConfigFile()
	if existing != "" {
		if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c := newLayoutCapturer(paths, comp, nil)
	c.lookPath = captureLookPath(installed...)
	c.now = func() time.Time { return time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC) }
	return c, path
}

// morningInventory is the issue's own scenario: seven windows the rules keep,
// across three workspaces, plus surfaces the rules exclude.
func morningInventory() []desktop.Window {
	return []desktop.Window{
		{Address: "0x1", Class: "alacritty", Workspace: 1, AcceptsInput: true, Focused: true},
		{Address: "0x2", Class: "alacritty", Workspace: 1, AcceptsInput: true},
		{Address: "0x3", Class: "firefox", Workspace: 2, AcceptsInput: true},
		{Address: "0x4", Class: "code", Workspace: 2, AcceptsInput: true},
		{Address: "0x5", Class: "md.obsidian.Obsidian", Workspace: 2, AcceptsInput: true},
		{Address: "0x6", Class: "Signal", Workspace: 9, AcceptsInput: true,
			Floating: true, X: 100, Y: 120, Width: 1200, Height: 800},
		{Address: "0x7", Class: "spotify", Workspace: 9, AcceptsInput: true},
		// Excluded: the Jarvix surface, a splash that takes no input, a
		// special-workspace scratchpad.
		{Address: "0xa", Class: "omarchy-shell", Workspace: 1, AcceptsInput: true},
		{Address: "0xb", Class: "firefox", Title: "Splash", Workspace: 2},
		{Address: "0xc", Class: "mpv", Workspace: -98, WorkspaceName: "special:video", AcceptsInput: true},
	}
}

func morningBinaries() []string {
	return []string{"alacritty", "firefox", "code", "obsidian", "signal-desktop", "spotify"}
}

// planAndCommit runs the two-phase flow, failing the test on either error.
func planAndCommit(t *testing.T, c *layoutCapturer, name string) string {
	t.Helper()
	plan, err := c.Plan(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	spoken, err := plan.Commit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return spoken
}

// TestCaptureIsReadOnlyAgainstTheDesktop: planning reads the inventory once
// and dispatches nothing — capture can never move a window.
func TestCaptureIsReadOnlyAgainstTheDesktop(t *testing.T) {
	comp := desktop.NewFakeCompositor(morningInventory()...)
	c, path := newTestCapturer(t, comp, "", morningBinaries()...)

	spoken := planAndCommit(t, c, "morning setup")

	if reads := comp.Reads(); reads != 1 {
		t.Errorf("inventory read %d times, want exactly one", reads)
	}
	if actions := comp.Actions(); len(actions) != 0 {
		t.Fatalf("capture dispatched %v; it must be read-only", actions)
	}
	if spoken != "Seven windows across three workspaces, saved as morning setup." {
		t.Errorf("spoken = %q", spoken)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config not written: %v", err)
	}
}

// TestCaptureWritesProvenanceAndAValidEntry: the written entry carries its
// provenance comment, parses, validates, and describes the desktop.
func TestCaptureWritesProvenanceAndAValidEntry(t *testing.T) {
	comp := desktop.NewFakeCompositor(morningInventory()...)
	c, path := newTestCapturer(t, comp, "# my hand-written file\n[audio]\nmax_recording_sec = 25\n",
		morningBinaries()...)

	planAndCommit(t, c, "morning setup")

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "# captured 2026-08-21\n[[routines]]") {
		t.Fatalf("provenance missing:\n%s", raw)
	}
	if !strings.HasPrefix(string(raw), "# my hand-written file\n") {
		t.Fatalf("hand-written content disturbed:\n%s", raw)
	}
	cfg, err := config.ParseBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("captured config must validate: %v", err)
	}
	if len(cfg.Routines) != 1 || len(cfg.Routines[0].Steps) != 7 {
		t.Fatalf("routines = %+v", cfg.Routines)
	}
	if got := cfg.Routines[0].Phrases; len(got) != 1 || got[0] != "morning setup" {
		t.Errorf("generated phrases = %v", got)
	}
	// The Signal float kept its geometry; the class≠binary windows carry
	// match overrides.
	var signal *config.RoutineStep
	for i := range cfg.Routines[0].Steps {
		if cfg.Routines[0].Steps[i].App == "signal-desktop" {
			signal = &cfg.Routines[0].Steps[i]
		}
	}
	// Written in the current placement vocabulary (ADR 0056), never in the
	// superseded `float`/`size` spelling: a captured entry is what a user
	// opens in the editor, so it must read like one they wrote today.
	if signal == nil || signal.Mode != "floating" || signal.Match != "Signal" ||
		signal.Width != "1200px" || signal.Height != "800px" ||
		len(signal.Position) != 2 || signal.Position[1] != 120 {
		t.Errorf("signal step = %+v", signal)
	}
}

// TestCapturePlaceholderIsSavedAndSpoken: an underivable command is a saved
// partial capture — placeholder plus TODO in the file, named in the one
// spoken line, marked incomplete in the listing surfaces.
func TestCapturePlaceholderIsSavedAndSpoken(t *testing.T) {
	comp := desktop.NewFakeCompositor(
		desktop.Window{Address: "0x1", Class: "firefox", Workspace: 1, AcceptsInput: true},
		desktop.Window{Address: "0x2", Class: "chrome-web.whatsapp.com__-Default", Workspace: 3, AcceptsInput: true},
	)
	c, path := newTestCapturer(t, comp, "", "firefox")

	spoken := planAndCommit(t, c, "chat setup")

	want := "Two windows across two workspaces, saved as chat setup. " +
		"One of them needs a hand: I could not work out how to launch " +
		"chrome-web.whatsapp.com__-Default — config.toml marks it."
	if spoken != want {
		t.Errorf("spoken = %q\nwant     %q", spoken, want)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), routine.PlaceholderApp) ||
		!strings.Contains(string(raw), "# TODO: no installed command matched") {
		t.Fatalf("placeholder or note missing:\n%s", raw)
	}
	cfg, err := config.ParseBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Routines[0].Incomplete() {
		t.Error("a placeholder capture must list as incomplete")
	}
}

// TestCaptureReplaceAsksAndKeepsCuratedPhrases: an existing name plans as a
// replace — the question the engine asks — and committing keeps the curated
// trigger phrases while replacing the steps wholesale.
func TestCaptureReplaceAsksAndKeepsCuratedPhrases(t *testing.T) {
	existing := `[[routines]]
name = "morning setup"
phrases = ["morning setup", "start my day"]

  [[routines.steps]]
  app = "mpv"
  workspace = 7
`
	comp := desktop.NewFakeCompositor(morningInventory()...)
	c, path := newTestCapturer(t, comp, existing, morningBinaries()...)

	plan, err := c.Plan(context.Background(), "morning setup")
	if err != nil {
		t.Fatal(err)
	}
	question, replaces := plan.ReplaceQuestion()
	if !replaces || !strings.Contains(question, "morning setup") {
		t.Fatalf("ReplaceQuestion = %q, %v", question, replaces)
	}
	if _, err := plan.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.ParseBytes(readFile(t, path))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Routines) != 1 {
		t.Fatalf("routines = %+v, want the one replaced entry", cfg.Routines)
	}
	got := cfg.Routines[0]
	if len(got.Phrases) != 2 || got.Phrases[1] != "start my day" {
		t.Errorf("curated phrases lost: %v", got.Phrases)
	}
	if len(got.Steps) != 7 || got.Steps[0].App == "mpv" {
		t.Errorf("steps not replaced wholesale: %+v", got.Steps)
	}
}

// TestCaptureCommitRefusesAConcurrentlyAppearedName: a name that appeared in
// the file between plan and commit — a hand edit during the thirty-second
// confirmation window — is never overwritten by an approval that predates it.
func TestCaptureCommitRefusesAConcurrentlyAppearedName(t *testing.T) {
	comp := desktop.NewFakeCompositor(morningInventory()...)
	c, path := newTestCapturer(t, comp, "", morningBinaries()...)

	plan, err := c.Plan(context.Background(), "morning setup")
	if err != nil {
		t.Fatal(err)
	}
	interloper := "[[routines]]\nname = \"morning setup\"\nphrases = [\"morning setup\"]\n\n" +
		"  [[routines.steps]]\n  app = \"mpv\"\n  workspace = 7\n"
	if err := os.WriteFile(path, []byte(interloper), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Commit(context.Background()); err == nil {
		t.Fatal("commit overwrote an entry nobody was asked about")
	}
	if string(readFile(t, path)) != interloper {
		t.Error("the refused commit still changed the file")
	}
}

// TestCaptureRefusesACollidingName: a name whose phrase the router already
// owns fails at plan time, spoken, with nothing written.
func TestCaptureRefusesACollidingName(t *testing.T) {
	comp := desktop.NewFakeCompositor(morningInventory()...)
	c, path := newTestCapturer(t, comp, "", morningBinaries()...)

	_, err := c.Plan(context.Background(), "mute")
	if err == nil || !strings.Contains(err.Error(), "mute") {
		t.Fatalf("err = %v, want a spoken refusal naming the collision", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Error("a refused capture still wrote config.toml")
	}
}

// TestCaptureFailedWriteLeavesConfigUntouched: when the atomic write cannot
// happen, the original file survives byte-for-byte and the error is a
// sentence.
func TestCaptureFailedWriteLeavesConfigUntouched(t *testing.T) {
	existing := "# precious\n[audio]\nmax_recording_sec = 25\n"
	comp := desktop.NewFakeCompositor(morningInventory()...)
	c, path := newTestCapturer(t, comp, existing, morningBinaries()...)

	plan, err := c.Plan(context.Background(), "morning setup")
	if err != nil {
		t.Fatal(err)
	}
	// Make the config directory unwritable so the temp-file create fails
	// before anything could touch the real file.
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := plan.Commit(context.Background()); err == nil {
		t.Fatal("commit reported success without writing")
	}
	_ = os.Chmod(dir, 0o700)
	if string(readFile(t, path)) != existing {
		t.Error("a failed write changed config.toml")
	}
}

// TestCaptureRoundTripConvergesOnTheFake is the issue's round-trip criterion:
// capture a canned inventory, run the routine the file now holds against the
// same fake, and the desktop ends exactly where it started — every window
// deduped (nothing spawned), every placement a re-assertion of the captured
// state.
func TestCaptureRoundTripConvergesOnTheFake(t *testing.T) {
	inventory := morningInventory()
	comp := desktop.NewFakeCompositor(inventory...)
	c, path := newTestCapturer(t, comp, "", morningBinaries()...)

	planAndCommit(t, c, "morning setup")

	cfg, err := config.ParseBytes(readFile(t, path))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the captured entry must be immediately runnable: %v", err)
	}
	runner := routine.New(routine.Options{
		Compositor:  comp,
		Definitions: cfg.RoutineDefinitions(),
	})
	summary, err := runner.Run(context.Background(), "morning setup")
	if err != nil {
		t.Fatal(err)
	}
	if summary != "Morning setup: all seven apps placed." {
		t.Errorf("summary = %q", summary)
	}

	// Fold the dispatches into a final state per window and compare with the
	// captured desktop: convergence means the run re-asserts exactly what
	// was on screen.
	type state struct {
		workspace     int
		floating      bool
		x, y          int
		width, height int
	}
	final := make(map[string]state, len(inventory))
	for _, w := range inventory {
		final[w.Address] = state{workspace: w.Workspace, floating: w.Floating,
			x: w.X, y: w.Y, width: w.Width, height: w.Height}
	}
	for _, a := range comp.Actions() {
		s := final[a.Address]
		switch a.Verb {
		case "spawn":
			t.Fatalf("the run spawned %q; every captured window was already on screen", a.Program)
		case "move":
			s.workspace = a.Workspace
		case "float":
			s.floating = a.Floating
		case "resize":
			s.width, s.height = a.Width, a.Height
		case "position":
			s.x, s.y = a.X, a.Y
		}
		final[a.Address] = s
	}
	for _, w := range inventory {
		got := final[w.Address]
		if captureExcludedForTest(w) {
			if got != (state{workspace: w.Workspace, floating: w.Floating, x: w.X, y: w.Y, width: w.Width, height: w.Height}) {
				t.Errorf("excluded window %s was touched: %+v", w.Address, got)
			}
			continue
		}
		if got.workspace != w.Workspace || got.floating != w.Floating {
			t.Errorf("window %s (%s) ended at %+v, want workspace %d floating %v",
				w.Address, w.Class, got, w.Workspace, w.Floating)
		}
		if w.Floating && (got.x != w.X || got.y != w.Y || got.width != w.Width || got.height != w.Height) {
			t.Errorf("floating window %s ended at %+v, want its captured geometry", w.Address, got)
		}
	}
}

// captureExcludedForTest mirrors the documented exclusion rules for the
// round-trip assertion's benefit only.
func captureExcludedForTest(w desktop.Window) bool {
	class := strings.ToLower(desktop.AppName(w.Class))
	return class == "omarchy-shell" || class == "jarvix" || !w.AcceptsInput ||
		w.Workspace < 1 || w.Workspace > 99
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestIdleClassChangedSeesStructuredTables: the router compiles from
// [[routines]] and [[intents.custom]], which live outside the settings
// registry — a reload after a capture (or a hand edit) must still rebuild
// the engine, or the phrase never works until a restart.
func TestIdleClassChangedSeesStructuredTables(t *testing.T) {
	running := testConfig()
	next := testConfig()
	if idleClassChanged(running, next) {
		t.Fatal("identical configs reported an idle-class change")
	}
	next.Routines = []config.Routine{{Name: "morning setup", Phrases: []string{"morning setup"},
		Steps: []config.RoutineStep{{App: "sh", Workspace: 1}}}}
	if !idleClassChanged(running, next) {
		t.Error("a routines change must rebuild the engine's collaborators")
	}
	next = testConfig()
	next.Intents.Custom = []config.CustomIntent{{Match: "lock it", Run: "loginctl lock-session"}}
	if !idleClassChanged(running, next) {
		t.Error("a custom-intent change must rebuild the engine's collaborators")
	}
}

// TestSpokenCaptureMakesTheRoutineImmediatelyRunnable is the whole loop over
// a live daemon: the phrase arrives as a session turn, the entry lands in
// config.toml and routines.list at once, the deferred reload (announced by
// config.changed) recompiles the router after the capture session finishes,
// and routines.run then executes the captured routine with zero provider
// calls. The window's class is "sh" so the real PATH lookup resolves on any
// machine the suite runs on.
func TestSpokenCaptureMakesTheRoutineImmediatelyRunnable(t *testing.T) {
	dir := t.TempDir()
	paths := config.Paths{
		Config: dir, Data: dir, State: dir, Runtime: dir,
		Socket: filepath.Join(dir, "j.sock"),
	}
	cfg := testConfig()
	cfg.Audio.MinRecordingMs = 0
	provider := &ai.Fake{Response: "should never be needed"}
	comp := desktop.NewFakeCompositor(
		desktop.Window{Address: "0xs", Class: "sh", Workspace: 2, AcceptsInput: true, Focused: true},
	)
	d, err := New(cfg, paths, nil, Deps{
		Provider:    provider,
		Transcriber: &stt.Fake{Text: "unused"},
		Synthesizer: &tts.Fake{},
		Recorder:    &audio.FakeRecorder{Clip: audio.Clip{WAVPath: dir + "/r.wav"}},
		Player:      &audio.FakePlayer{},
		Notifier:    &desktop.FakeNotifier{},
		OpenWindow:  func(context.Context) error { return nil },
		Compositor:  comp,
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDaemon(t, d)
	client := dialDaemon(t, paths.Socket)

	if err := client.Call("session.text", map[string]string{"text": "save this as morning setup"}, nil); err != nil {
		t.Fatal(err)
	}
	ev := waitForEvent(t, client, "intent.executed")
	if ev["intent"] != "routine.capture" || ev["status"] != "ok" {
		t.Fatalf("intent.executed = %v", ev)
	}
	if ev["acknowledgement"] != "One window on one workspace, saved as morning setup." {
		t.Errorf("acknowledgement = %v", ev["acknowledgement"])
	}
	waitForEvent(t, client, "session.finished")
	// The engine could not be rebuilt under the capture session; the session
	// watcher does it now and announces it the way every config move is
	// announced.
	waitForEvent(t, client, "config.changed")

	var listed struct {
		Routines []struct {
			Name       string `json:"name"`
			Incomplete bool   `json:"incomplete"`
		} `json:"routines"`
	}
	if err := client.Call("routines.list", nil, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Routines) != 1 || listed.Routines[0].Name != "morning setup" ||
		listed.Routines[0].Incomplete {
		t.Fatalf("routines.list = %+v", listed.Routines)
	}

	if err := client.Call("routines.run", map[string]string{"name": "morning setup"}, nil); err != nil {
		t.Fatal(err)
	}
	fin := waitForEvent(t, client, "routine.finished")
	if fin["routine"] != "morning setup" {
		t.Errorf("routine.finished = %v", fin)
	}
	waitForEvent(t, client, "session.finished")
	if len(provider.Requests) != 0 {
		t.Errorf("the captured loop made %d provider calls", len(provider.Requests))
	}
}
