package desktop

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// barStateJSPath returns the checked-in generated library, found by walking up
// to the checkout root so the test does not care where it is run from.
func barStateJSPath(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, "plugin", "omarchy", "BarState.js")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the working directory")
		}
		dir = parent
	}
}

// A dead socket is the loudest fact the widget has: whatever state it last
// saw came from a connection that is now gone, and "not running" is both the
// truth and the actionable thing to say. The AC is explicit that the icon
// must not simply vanish, because an absent icon cannot be told apart from a
// plugin that was never installed.
func TestBarStatusDaemonDownBeatsEverything(t *testing.T) {
	for _, state := range []string{"", "idle", "listening", "thinking", "banana"} {
		got := BarStatusFor(false, state, "the assistant failed", WakeArmed)
		if got.Key != BarKeyNotRunning {
			t.Errorf("state %q with the socket down: got %q, want %q", state, got.Key, BarKeyNotRunning)
		}
		if !strings.Contains(got.Tooltip(), "systemctl --user start jarvixd") {
			t.Errorf("the not-running tooltip must carry the start hint, got %q", got.Tooltip())
		}
		if !got.Dim {
			t.Error("a daemon that is down should read as present-but-inert, not as a live state")
		}
	}
}

// The daemon returns to idle after a failed turn, so a widget that trusted
// `state` alone would erase the failure in the same instant it happened. The
// error stands until the next session starts — the window's banner rule, and
// the widget must not disagree with the window.
func TestBarStatusHeldErrorBeatsTheState(t *testing.T) {
	got := BarStatusFor(true, "idle", "whisper.cpp exited with status 1", WakeOff)
	if got.Key != BarKeyError {
		t.Fatalf("got %q, want %q", got.Key, BarKeyError)
	}
	if got.Detail != "whisper.cpp exited with status 1" {
		t.Errorf("the error detail must be the daemon's message, got %q", got.Detail)
	}
	if !got.Urgent {
		t.Error("a failed turn is the one thing the user has to notice")
	}

	// Cleared error: straight back to the state's own row. This is what makes
	// "until the next session starts" work — the widget clears the message,
	// and the table stops reporting an error without being told twice.
	if got := BarStatusFor(true, "listening", "", WakeOff); got.Key != "listening" {
		t.Errorf("with the error cleared: got %q, want listening", got.Key)
	}
	// Whitespace is not a message. An `error` event with a blank body would
	// otherwise pin the widget in a red state saying nothing.
	if got := BarStatusFor(true, "idle", "   \n", WakeOff); got.Key != "idle" {
		t.Errorf("a blank error message must not hold the widget in the error state, got %q", got.Key)
	}
}

// docs/ipc.md promises new states can be added without bumping the protocol,
// so an older widget will meet states it has never heard of. Reporting "ready"
// while Jarvix is plainly busy is the failure worth guarding against.
func TestBarStatusUnknownStateReadsAsBusyNotReady(t *testing.T) {
	got := BarStatusFor(true, "dreaming", "", WakeOff)
	if got.Key != BarKeyUnknown {
		t.Fatalf("got %q, want %q", got.Key, BarKeyUnknown)
	}
	if got.Glyph == barStates["idle"].Glyph {
		t.Error("an unknown state must not borrow the idle glyph — it is not idle")
	}
	// An empty state is different: that is the widget before its first event,
	// and idle is exactly right.
	if got := BarStatusFor(true, "", "", WakeOff); got.Key != "idle" {
		t.Errorf("no state yet should read as idle, got %q", got.Key)
	}
}

// Accessibility: state is conveyed by shape and by words, never by colour
// alone. Two states that shared a glyph would be indistinguishable to anyone
// who cannot pick the theme's urgent colour out of its foreground, and an
// empty label would leave the tooltip and the screen reader with nothing.
func TestBarStateVocabularyIsDistinguishableWithoutColour(t *testing.T) {
	glyphs := map[string]string{}
	labels := map[string]string{}
	for _, key := range BarStateKeys() {
		s := barStates[key]
		if s.Key != key {
			t.Errorf("%s: row key is %q; the map key and the record must agree", key, s.Key)
		}
		if s.Label == "" || s.Detail == "" {
			t.Errorf("%s: every state needs a label and a detail — the tooltip is the accessible text", key)
		}
		if s.Glyph == "" {
			t.Errorf("%s: missing glyph", key)
		}
		if other, clash := glyphs[s.Glyph]; clash {
			t.Errorf("%s and %s share a glyph; shape is what distinguishes them without colour", key, other)
		}
		if other, clash := labels[s.Label]; clash {
			t.Errorf("%s and %s share a label", key, other)
		}
		glyphs[s.Glyph] = key
		labels[s.Label] = key
		if want := s.Label + " — " + s.Detail; s.Tooltip() != want {
			t.Errorf("%s: tooltip %q, want %q", key, s.Tooltip(), want)
		}
	}
}

// Every state the daemon can publish (docs/ipc.md) needs a row. A state with
// no row falls through to "Working", which is safe but says nothing useful —
// and the gap would be invisible until someone hit that state on the bar.
func TestBarStateCoversEveryDocumentedDaemonState(t *testing.T) {
	documented := []string{
		"idle", "listening", "transcribing", "thinking", "responding",
		"awaiting_confirmation", "acting", "speaking", "cancelling", "error",
	}
	for _, state := range documented {
		if got := BarStatusFor(true, state, "", WakeOff); got.Key != state {
			t.Errorf("daemon state %q has no row of its own (fell back to %q)", state, got.Key)
		}
	}
}

// The panel's promise is that nothing in it is a new implementation: every
// action is a command that already exists. Toggling the window in particular
// must go through the plugin's own IpcHandler — a widget that opened its own
// window would hand the user two of them.
func TestBarActionsReuseExistingEntryPoints(t *testing.T) {
	byKey := map[string]BarAction{}
	for _, a := range BarActionsFor(false, WakeOff) {
		if a.Command == "" {
			t.Errorf("%s: an action with no command is a dead menu row", a.Key)
		}
		if a.Label == "" || a.Detail == "" || a.Glyph == "" {
			t.Errorf("%s: actions need a label, a detail, and a glyph", a.Key)
		}
		if _, dup := byKey[a.Key]; dup {
			t.Errorf("%s: duplicate action key", a.Key)
		}
		byKey[a.Key] = a
	}

	// The four the acceptance criteria name, plus the start hint.
	for _, want := range []struct{ key, command string }{
		{"window", "omarchy-shell jarvix toggleWindow"},
		{"new", "jarvix new"},
		{"settings", "omarchy-shell jarvix openSettings"},
		{"start", "systemctl --user start jarvixd"},
	} {
		got, ok := byKey[want.key]
		if !ok {
			t.Errorf("the panel must offer %q", want.key)
			continue
		}
		if got.Command != want.command {
			t.Errorf("%s runs %q, want %q", want.key, got.Command, want.command)
		}
	}
}

// "Start Jarvix" beside a running daemon is noise; beside a stopped one it is
// the whole reason the panel opened. Nothing else changes with the socket.
func TestBarActionsOfferTheStartHintOnlyWhenTheDaemonIsDown(t *testing.T) {
	up := BarActionsFor(true, WakeOff)
	for _, a := range up {
		if a.Key == "start" {
			t.Fatal("a running daemon must not be offered a start command")
		}
	}
	down := BarActionsFor(false, WakeOff)
	if len(down) != len(up)+1 {
		t.Fatalf("down offers %d actions, up offers %d — only the start action should differ", len(down), len(up))
	}
	if down[0].Key != "start" {
		t.Errorf("with the daemon down the start action must lead, got %q", down[0].Key)
	}
	if !strings.Contains(down[0].Detail, StartHint[len("Start it: "):]) {
		t.Errorf("the start action should show the command it runs, got %q", down[0].Detail)
	}
}

// Every kind `jarvix artifacts --json` can report needs an icon, and an
// unrecognised one must still get something — the CLI falls back to the bare
// file extension for kinds it has no name for.
func TestBarArtifactGlyphAlwaysResolves(t *testing.T) {
	seen := map[string]string{}
	for _, kind := range BarArtifactKinds() {
		glyph := BarArtifactGlyph(kind)
		if glyph == glyphFile {
			t.Errorf("%s: a named kind should have its own glyph, not the generic file one", kind)
		}
		if other, clash := seen[glyph]; clash {
			t.Errorf("%s and %s share a glyph", kind, other)
		}
		seen[glyph] = kind
	}
	for _, unknown := range []string{"", "xlsx", "wat"} {
		if got := BarArtifactGlyph(unknown); got != glyphFile {
			t.Errorf("kind %q: got %q, want the generic file glyph", unknown, got)
		}
	}
	// The CLI's own kind vocabulary (cmd/jarvix artifactKind) — a kind it can
	// emit that has no icon here would draw the generic file glyph forever
	// without anyone noticing.
	for _, kind := range []string{"source", "diagram", "document", "spreadsheet", "sketch"} {
		if BarArtifactGlyph(kind) == glyphFile {
			t.Errorf("the CLI reports kind %q; the panel has no icon for it", kind)
		}
	}
}

// The generated library is the widget's only copy of the table, and it is
// checked in so the plugin works from a git clone with no Go toolchain. That
// makes drift possible, and silent: the bar would keep showing yesterday's
// labels. Regenerate with `go generate ./internal/desktop`.
func TestBarStateJSIsUpToDate(t *testing.T) {
	path := barStateJSPath(t)
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != RenderBarStateJS() {
		t.Errorf("%s is stale — run: go generate ./internal/desktop", path)
	}
}

// Byte equality proves the file matches the generator; it does not prove the
// generator's JavaScript decides the same way the Go does. Only running it
// does. node is not a build dependency — the test skips without it, and the
// mirror is checked wherever node exists (CI runners, most dev machines).
func TestBarStateJSMirrorsGo(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping the JavaScript mirror check")
	}

	type jsCase struct {
		Connected bool   `json:"connected"`
		State     string `json:"state"`
		Error     string `json:"error"`
		Wake      string `json:"wake"`
	}
	var cases []jsCase
	for _, key := range BarStateKeys() {
		for _, wake := range []string{WakeOff, WakeArmed, WakeMuted, "who knows"} {
			cases = append(cases,
				jsCase{Connected: true, State: key, Wake: wake},
				jsCase{Connected: false, State: key, Wake: wake},
				jsCase{Connected: true, State: key, Error: "the assistant failed", Wake: wake},
			)
		}
	}
	cases = append(cases,
		jsCase{Connected: true, State: ""},
		jsCase{Connected: true, State: "", Wake: WakeArmed},
		jsCase{Connected: true, State: "", Wake: WakeMuted},
		jsCase{Connected: true, State: "dreaming"},
		jsCase{Connected: true, State: "dreaming", Wake: WakeArmed},
		jsCase{Connected: true, State: "idle", Error: "   \n"},
		jsCase{Connected: true, State: "idle", Error: "   \n", Wake: WakeArmed},
		jsCase{Connected: false, State: "", Error: "boom", Wake: WakeMuted},
	)
	encoded, err := json.Marshal(cases)
	if err != nil {
		t.Fatal(err)
	}

	library, err := os.ReadFile(barStateJSPath(t))
	if err != nil {
		t.Fatal(err)
	}
	// `.pragma library` is a QML engine directive, not JavaScript; node would
	// choke on it. Everything after it is plain script.
	script := strings.Replace(string(library), ".pragma library", "", 1) + `
var cases = ` + string(encoded) + `
var out = cases.map(function (c) {
  var status = statusFor(c.connected, c.state, c.error, c.wake)
  return {
    key: status.key, glyph: status.glyph, label: status.label,
    detail: status.detail, urgent: status.urgent, pulse: status.pulse,
    dim: status.dim, tooltip: tooltip(status)
  }
})
console.log(JSON.stringify({
  statuses: out,
  actionsUp: actions(true, "off").map(function (a) { return a.key + " " + a.command }),
  actionsDown: actions(false, "off").map(function (a) { return a.key + " " + a.command }),
  actionsArmed: actions(true, "armed").map(function (a) { return a.key + " " + a.command }),
  actionsMuted: actions(true, "muted").map(function (a) { return a.key + " " + a.command }),
  glyphs: ["diagram", "document", "spreadsheet", "sketch", "source", "wat", ""]
    .map(function (k) { return artifactGlyph(k) })
}))
`
	file := filepath.Join(t.TempDir(), "mirror.js")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, file).Output()
	if err != nil {
		t.Fatalf("running the generated library under node failed: %v", err)
	}

	type jsStatus struct {
		Key     string `json:"key"`
		Glyph   string `json:"glyph"`
		Label   string `json:"label"`
		Detail  string `json:"detail"`
		Urgent  bool   `json:"urgent"`
		Pulse   bool   `json:"pulse"`
		Dim     bool   `json:"dim"`
		Tooltip string `json:"tooltip"`
	}
	var answers struct {
		Statuses     []jsStatus `json:"statuses"`
		ActionsUp    []string   `json:"actionsUp"`
		ActionsDown  []string   `json:"actionsDown"`
		ActionsArmed []string   `json:"actionsArmed"`
		ActionsMuted []string   `json:"actionsMuted"`
		Glyphs       []string   `json:"glyphs"`
	}
	if err := json.Unmarshal(out, &answers); err != nil {
		t.Fatalf("decoding the library's answers: %v\n%s", err, out)
	}

	goActions := func(connected bool, wake string) []string {
		var list []string
		for _, a := range BarActionsFor(connected, wake) {
			list = append(list, a.Key+" "+a.Command)
		}
		return list
	}
	for _, c := range []struct {
		label     string
		js        []string
		connected bool
		wake      string
	}{
		{"daemon up", answers.ActionsUp, true, WakeOff},
		{"daemon down", answers.ActionsDown, false, WakeOff},
		{"listening in the background", answers.ActionsArmed, true, WakeArmed},
		{"muted", answers.ActionsMuted, true, WakeMuted},
	} {
		if strings.Join(c.js, "|") != strings.Join(goActions(c.connected, c.wake), "|") {
			t.Errorf("actions (%s):\n  js: %v\n  go: %v", c.label, c.js, goActions(c.connected, c.wake))
		}
	}
	for i, kind := range []string{"diagram", "document", "spreadsheet", "sketch", "source", "wat", ""} {
		if answers.Glyphs[i] != BarArtifactGlyph(kind) {
			t.Errorf("artifact glyph for %q: js %q, go %q", kind, answers.Glyphs[i], BarArtifactGlyph(kind))
		}
	}

	got := answers.Statuses
	if len(got) != len(cases) {
		t.Fatalf("got %d answers for %d cases", len(got), len(cases))
	}
	for i, c := range cases {
		want := BarStatusFor(c.Connected, c.State, c.Error, c.Wake)
		have := got[i]
		if have.Key != want.Key || have.Glyph != want.Glyph || have.Label != want.Label ||
			have.Detail != want.Detail || have.Urgent != want.Urgent ||
			have.Pulse != want.Pulse || have.Dim != want.Dim || have.Tooltip != want.Tooltip() {
			t.Errorf("connected=%t state=%q error=%q wake=%q:\n  js: %+v\n  go: %+v (tooltip %q)",
				c.Connected, c.State, c.Error, c.Wake, have, want, want.Tooltip())
		}
	}
}

// pluginFilePath resolves a file in plugin/omarchy the same way
// barStateJSPath does, by walking up to the module root.
func pluginFilePath(t *testing.T, name string) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, "plugin", "omarchy", name)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the working directory")
		}
		dir = parent
	}
}

// A QML file that declares a FloatingWindow must not import Quickshell.Wayland.
//
// This looks like a style rule and is not: with that import present,
// FloatingWindow stops producing a Wayland toplevel while every other symptom
// still looks healthy. The plugin loads, the IPC handlers answer, `openWindow`
// returns "open", `visible` reads back true — and there is simply no window,
// with nothing anywhere logging a complaint. It was added once on the
// reasonable-sounding theory that FloatingWindow lived in that module, and it
// silently disabled the conversation window for every user until someone
// clicked the bar icon and noticed nothing happened.
//
// FloatingWindow comes from Quickshell itself; Omarchy's own dev-gallery
// plugin imports Quickshell alone. The import is entirely legitimate
// elsewhere — JarvixOverlay.qml needs it for WlrLayershell — so the rule is
// scoped to the files that actually declare a window. Nothing else in the
// suite can catch this: the failure is invisible to Go, to the QML parser,
// and to `omarchy plugin validate` alike.
func TestFloatingWindowFilesDoNotImportQuickshellWayland(t *testing.T) {
	names := []string{"JarvixWindow.qml", "JarvixOverlay.qml", "JarvixBar.qml", "JarvixSettings.qml"}
	checked := 0
	for _, name := range names {
		source, err := os.ReadFile(pluginFilePath(t, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		text := string(source)
		if !strings.Contains(text, "FloatingWindow {") {
			continue // no window declared here; the import may be needed for other types
		}
		checked++
		for _, line := range strings.Split(text, "\n") {
			if strings.TrimSpace(line) == "import Quickshell.Wayland" {
				t.Errorf("%s declares a FloatingWindow and imports Quickshell.Wayland: "+
					"the window will never map, with no error anywhere — see this test's comment", name)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no FloatingWindow declaration found in the plugin; this guard is no longer watching anything")
	}
}
