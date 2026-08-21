package desktop

import (
	"fmt"
	"sort"
	"strings"
)

//go:generate go run genbarstate.go

// This file is the bar widget's brain.
//
// The Omarchy bar widget (plugin/omarchy/JarvixBar.qml) has one job: say what
// Jarvix is doing right now, at a glance. Deciding *what* to say — which
// glyph, which words, whether it is alarming — is a table of rules, and rules
// belong in Go where they can be tested (ADR 0013: QML stays display-only).
//
// QML cannot call Go, so the table is compiled to a tiny JavaScript library,
// plugin/omarchy/BarState.js, by `go generate ./internal/desktop`. The
// checked-in file is guarded two ways in barstatus_test.go: byte-for-byte
// against RenderBarStateJS(), and — where node is available — by running the
// generated JavaScript over every case and comparing its answers to
// BarStatusFor's. The QML then only draws what it is handed.

// BarState is everything the bar draws for one moment of daemon state: a
// glyph, the same thing in words (accessibility requires text, not colour
// alone), and the two presentation switches the bar widget honours.
type BarState struct {
	// Key is the stable identifier for this state — the value #4's privacy
	// indicator and any future test will assert on, rather than the prose.
	Key string
	// Glyph is a Nerd Font (Material Design) character. Each state gets a
	// distinguishable *shape*, so the widget still reads correctly to someone
	// who cannot tell the theme's urgent colour from its foreground.
	Glyph string
	// Label is the short headline: "Listening", "Jarvix is not running".
	Label string
	// Detail is the second line — what to do about it, or what is happening.
	Detail string
	// Urgent asks the bar to paint the icon in the theme's urgent colour
	// (BarIconButton.active). Reserved for states the user must act on.
	Urgent bool
	// Pulse animates the icon's opacity. Matched to the overlay's existing
	// rule (JarvixOverlay.qml pulses on listening/thinking) so the two
	// surfaces never disagree about when Jarvix looks busy.
	Pulse bool
	// Dim renders the icon faded: present, but plainly not live. Only the
	// daemon-down state uses it — an absent icon must never be ambiguous
	// between "stopped" and "not installed" (issue #31).
	Dim bool
}

// Tooltip is the hover/accessible text: the label, plus the detail when there
// is one. Composed here rather than in QML so the widget's tooltip and any
// future CLI rendering of the same state cannot drift.
func (s BarState) Tooltip() string {
	if s.Detail == "" {
		return s.Label
	}
	return s.Label + " — " + s.Detail
}

// The glyphs, written as escapes with their Material Design names, because a
// bare Nerd Font character in source is unreviewable — every one of these is
// an unrenderable box in most diff viewers. Codepoints verified present in
// the shell's resolved monospace family (JetBrainsMono Nerd Font).
const (
	// Jarvix's resting identity. Deliberately not md-robot: a robot is the
	// generic "an AI lives here" glyph, so it collides with every other
	// assistant in the bar — the Agents widget next door is Claude Code —
	// and says nothing about what Jarvix is. An orbiting mark reads as a
	// presence rather than a machine, and nothing else in a default Omarchy
	// bar looks like it. The activity glyphs below carry the meaning; this
	// one only has to be unmistakably Jarvix.
	glyphOrbit = "\U000F0018" // md-orbit
	// Stopped. md-orbit has no struck-through variant, and reusing the
	// robot here would put the glyph we just removed back in the bar, so
	// the dormant state gets its own unambiguous mark.
	glyphSleep      = "\U000F04B2" // md-sleep
	glyphMicrophone = "\U000F036C" // md-microphone
	glyphWaveform   = "\U000F147D" // md-waveform
	glyphBrain      = "\U000F09D1" // md-brain
	glyphMessage    = "\U000F0369" // md-message_text
	glyphFlash      = "\U000F0241" // md-flash
	glyphHelp       = "\U000F02D7" // md-help_circle
	glyphVolume     = "\U000F057E" // md-volume_high
	glyphCancel     = "\U000F073A" // md-cancel
	glyphAlert      = "\U000F0026" // md-alert
	glyphDots       = "\U000F01D8" // md-dots_horizontal

	glyphWindow = "\U000F05B2" // md-window_restore
	glyphPlus   = "\U000F0419" // md-plus_circle_outline
	glyphCog    = "\U000F0493" // md-cog
	glyphPower  = "\U000F0425" // md-power
	glyphFolder = "\U000F0770" // md-folder_open

	glyphDiagram     = "\U000F04AA" // md-sitemap
	glyphDocument    = "\U000F09EE" // md-file_document_outline
	glyphSpreadsheet = "\U000F0C7F" // md-file_table_outline
	glyphSketch      = "\U000F0F49" // md-draw
	glyphSource      = "\U000F0169" // md-code_braces
	glyphFile        = "\U000F0224" // md-file_outline
)

// Keys for the states that are not simply a daemon state name. The daemon's
// own vocabulary (docs/ipc.md) supplies the rest.
const (
	// BarKeyNotRunning is shown whenever the widget has no socket. It is not
	// a daemon state — it is the absence of one.
	BarKeyNotRunning = "not-running"
	// BarKeyError outlives the session that caused it: it stands until the
	// next session starts, matching the conversation window's banner rule.
	BarKeyError = "error"
	// BarKeyUnknown catches a state this build has never heard of. docs/ipc.md
	// promises new states may be added without a protocol bump, and "Working"
	// is the honest thing to say about one — far better than reporting the
	// last state we understood, or claiming Jarvix is ready when it is busy.
	BarKeyUnknown = "working"
)

// barStates is the vocabulary, keyed by the `state` field of a `state.changed`
// event (docs/ipc.md). Sorted output everywhere below keeps the generated
// JavaScript stable across Go's map iteration order.
var barStates = map[string]BarState{
	"idle": {
		Key: "idle", Glyph: glyphOrbit,
		Label:  "Jarvix is ready",
		Detail: "Click to open the conversation",
	},
	"listening": {
		Key: "listening", Glyph: glyphMicrophone,
		Label:  "Listening",
		Detail: "The microphone is open",
		Pulse:  true,
	},
	"transcribing": {
		Key: "transcribing", Glyph: glyphWaveform,
		Label:  "Transcribing",
		Detail: "Turning what you said into text",
	},
	"thinking": {
		Key: "thinking", Glyph: glyphBrain,
		Label:  "Thinking",
		Detail: "Working out an answer",
		Pulse:  true,
	},
	"responding": {
		Key: "responding", Glyph: glyphMessage,
		Label:  "Responding",
		Detail: "Writing the answer",
	},
	"awaiting_confirmation": {
		Key: "awaiting_confirmation", Glyph: glyphHelp,
		Label:  "Waiting for your answer",
		Detail: "A tool call needs confirming",
		Urgent: true, Pulse: true,
	},
	"acting": {
		Key: "acting", Glyph: glyphFlash,
		Label:  "Running a command",
		Detail: "A matched intent is executing",
	},
	"speaking": {
		Key: "speaking", Glyph: glyphVolume,
		Label:  "Speaking",
		Detail: "Reading the answer aloud",
	},
	"cancelling": {
		Key: "cancelling", Glyph: glyphCancel,
		Label:  "Cancelling",
		Detail: "Stopping the current turn",
	},
	BarKeyError: {
		Key: BarKeyError, Glyph: glyphAlert,
		Label:  "Jarvix hit a problem",
		Detail: defaultErrorDetail,
		Urgent: true,
	},
	BarKeyNotRunning: {
		Key: BarKeyNotRunning, Glyph: glyphSleep,
		Label:  "Jarvix is not running",
		Detail: StartHint,
		Dim:    true,
	},
	BarKeyUnknown: {
		Key: BarKeyUnknown, Glyph: glyphDots,
		Label:  "Working",
		Detail: "Jarvix is busy",
	},
}

// StartHint is the one thing worth saying when the daemon is down, and the
// same sentence internal/doctor and internal/ipc already say.
const StartHint = "Start it: systemctl --user start jarvixd"

// defaultErrorDetail stands in when the error event carried no message —
// rare, but "Jarvix hit a problem —" with nothing after it is worse than a
// generic sentence.
const defaultErrorDetail = "The last turn failed"

// BarStatusFor resolves what the bar should show. The three inputs are
// exactly what the widget knows: whether its socket is up, the last
// `state.changed` it saw, and the error message it is holding (empty once a
// new session clears it).
//
// Precedence is deliberate and is the part worth testing:
//  1. No socket wins over everything. A state read from a dead connection is
//     stale by definition, and "not running" is the more useful truth.
//  2. A held error wins over the state. The daemon returns to idle after a
//     failed turn, so trusting the state alone would erase the failure the
//     instant it happened — the widget would go quiet about the one thing
//     the user needs to know.
//  3. Otherwise the state's own row, with an unknown state falling back to
//     "Working" rather than to "ready".
func BarStatusFor(connected bool, state string, errMessage string) BarState {
	if !connected {
		return barStates[BarKeyNotRunning]
	}
	if strings.TrimSpace(errMessage) != "" {
		s := barStates[BarKeyError]
		s.Detail = strings.TrimSpace(errMessage)
		return s
	}
	if state == "" {
		return barStates["idle"]
	}
	if s, ok := barStates[state]; ok {
		return s
	}
	return barStates[BarKeyUnknown]
}

// BarAction is one entry in the widget's panel: something worth reaching for
// without speaking. Every action is a command line that already exists —
// either the plugin's own IPC surface (`omarchy-shell jarvix …`, the same
// handler `jarvix window` and a clicked notification go through) or the CLI.
// The widget runs it and holds no session logic of its own (ADR 0013).
type BarAction struct {
	Key    string
	Glyph  string
	Label  string
	Detail string
	// Command is run by the bar exactly as written.
	Command string
	// OnlyWhenDown limits the action to a stopped daemon. Offering "start
	// Jarvix" beside a running one is noise; offering it beside a stopped one
	// is the entire point of the "not running" state.
	OnlyWhenDown bool
}

// barActions is the panel's menu, in the order it is drawn.
//
// Toggling the window through `omarchy-shell jarvix toggleWindow` rather than
// through a second window implementation is a requirement, not a shortcut:
// the window is a single instance owned by the plugin's panel entry point,
// and a widget that opened its own would give the user two of them.
var barActions = []BarAction{
	{
		Key: "start", Glyph: glyphPower,
		Label:        "Start Jarvix",
		Detail:       "systemctl --user start jarvixd",
		Command:      "systemctl --user start jarvixd",
		OnlyWhenDown: true,
	},
	{
		Key: "window", Glyph: glyphWindow,
		Label:   "Conversation window",
		Detail:  "Show or hide the full conversation",
		Command: "omarchy-shell jarvix toggleWindow",
	},
	{
		Key: "new", Glyph: glyphPlus,
		Label:   "New conversation",
		Detail:  "Forget the current thread and start fresh",
		Command: "jarvix new",
	},
	{
		Key: "settings", Glyph: glyphCog,
		Label:   "Settings",
		Detail:  "Voice, activation, AI, and advisors",
		Command: "omarchy-shell jarvix openSettings",
	},
}

// BarActionsFor returns the panel's actions for the current connection state.
// The daemon-down action leads, because when Jarvix is not running that is
// the only thing the user wants from this panel.
func BarActionsFor(connected bool) []BarAction {
	out := make([]BarAction, 0, len(barActions))
	for _, a := range barActions {
		if a.OnlyWhenDown && connected {
			continue
		}
		out = append(out, a)
	}
	return out
}

// artifactGlyphs maps the kinds `jarvix artifacts --json` reports onto icons
// for the panel's recent-artifacts list. Kept here with the rest of the
// vocabulary so a new artifact kind is one edit, in Go, with a test.
var artifactGlyphs = map[string]string{
	"diagram":     glyphDiagram,
	"document":    glyphDocument,
	"spreadsheet": glyphSpreadsheet,
	"sketch":      glyphSketch,
	"source":      glyphSource,
}

// BarArtifactGlyph is the icon for one artifact kind. An unknown kind — the
// CLI reports the bare extension for anything it has no name for — gets the
// generic file glyph rather than nothing, so a row never renders with a hole
// where its icon should be.
func BarArtifactGlyph(kind string) string {
	if glyph, ok := artifactGlyphs[kind]; ok {
		return glyph
	}
	return glyphFile
}

// BarArtifactKinds lists the named kinds, sorted, for the generator.
func BarArtifactKinds() []string {
	kinds := make([]string, 0, len(artifactGlyphs))
	for kind := range artifactGlyphs {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return kinds
}

// BarStateKeys lists every key in the table, sorted — the generator's
// iteration order and the tests' case list, from one place.
func BarStateKeys() []string {
	keys := make([]string, 0, len(barStates))
	for key := range barStates {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// RenderBarStateJS compiles the table above into the QML-importable library
// the bar widget reads. It emits the same precedence rules as BarStatusFor;
// barstatus_test.go proves the two agree rather than trusting the comment.
func RenderBarStateJS() string {
	var b strings.Builder
	// `.pragma library` has to be the first line: it tells the QML engine this
	// file is a stateless shared library rather than a per-component script,
	// and the engine only honours it at the top of the file.
	b.WriteString(`.pragma library

// Code generated by internal/desktop/genbarstate.go. DO NOT EDIT.
//
// The Jarvix bar widget's state vocabulary, compiled from the Go table in
// internal/desktop/barstatus.go — the source of truth, and the only place
// these rules are tested (ADR 0013 keeps QML display-only). Regenerate with:
//
//     go generate ./internal/desktop
//
// Glyphs are Nerd Font (Material Design) characters; each state has its own
// shape so the widget never depends on colour alone.

// One record per state. Keys match the ` + "`state`" + ` field of a state.changed
// event (docs/ipc.md), plus the three the widget synthesises: not-running,
// error, and working.
var states = {
`)
	keys := BarStateKeys()
	for i, key := range keys {
		s := barStates[key]
		fmt.Fprintf(&b, "  %s: { key: %s, glyph: %s, label: %s, detail: %s, urgent: %t, pulse: %t, dim: %t }%s\n",
			jsString(key), jsString(s.Key), jsString(s.Glyph), jsString(s.Label),
			jsString(s.Detail), s.Urgent, s.Pulse, s.Dim, jsSeparator(i, len(keys)))
	}
	b.WriteString(`}

// statusFor mirrors desktop.BarStatusFor. Precedence: a dead socket beats any
// state (a state read from a dead connection is stale); a held error beats the
// state (the daemon returns to idle after a failed turn, so the state alone
// would erase the failure); an unrecognised state reads as "Working" rather
// than "ready".
function statusFor(connected, state, errorMessage) {
  if (!connected) return states["not-running"]
  var message = String(errorMessage || "").trim()
  if (message !== "") {
    var failed = states["error"]
    return {
      key: failed.key, glyph: failed.glyph, label: failed.label,
      detail: message, urgent: failed.urgent, pulse: failed.pulse, dim: failed.dim
    }
  }
  var name = String(state || "")
  if (name === "") return states["idle"]
  return states[name] || states["working"]
}

// tooltip is the hover and accessible text: label, plus detail when there is
// one. Mirrors BarState.Tooltip().
function tooltip(status) {
  if (!status) return ""
  if (!status.detail) return status.label
  return status.label + " — " + status.detail
}

// The panel's menu, in draw order. Every command already exists: the plugin's
// own IPC surface — the same handler "jarvix window" and a clicked
// notification go through — or the CLI. Mirrors desktop.BarActionsFor.
var allActions = [
`)
	for i, a := range barActions {
		fmt.Fprintf(&b, "  { key: %s, glyph: %s, label: %s, detail: %s, command: %s, onlyWhenDown: %t }%s\n",
			jsString(a.Key), jsString(a.Glyph), jsString(a.Label),
			jsString(a.Detail), jsString(a.Command), a.OnlyWhenDown, jsSeparator(i, len(barActions)))
	}
	b.WriteString(`]

function actions(connected) {
  return allActions.filter(function (a) { return !(a.onlyWhenDown && connected) })
}

// Icons for the kinds ` + "`jarvix artifacts --json`" + ` reports. Anything else —
// the CLI falls back to the bare extension — gets the generic file glyph, so
// a row never draws with a hole where its icon should be.
var artifactGlyphs = {
`)
	kinds := BarArtifactKinds()
	for i, kind := range kinds {
		fmt.Fprintf(&b, "  %s: %s%s\n", jsString(kind), jsString(artifactGlyphs[kind]), jsSeparator(i, len(kinds)))
	}
	b.WriteString(`}

var fileGlyph = ` + jsString(glyphFile) + `
var folderGlyph = ` + jsString(glyphFolder) + `

function artifactGlyph(kind) {
  return artifactGlyphs[String(kind || "")] || fileGlyph
}
`)
	return b.String()
}

// jsSeparator is the comma between literal members — omitted after the last
// one. A dangling comma in an object or array literal is legal in ES5, but
// relying on it buys nothing and would make the generated file fussier to
// paste into a stricter engine than it needs to be.
func jsSeparator(index, length int) string {
	if index == length-1 {
		return ""
	}
	return ","
}

// jsString quotes a Go string as a JavaScript double-quoted literal. The
// values here are our own prose and Nerd Font glyphs, never user input, but
// escaping them properly is what keeps the generated file safe to eval and
// stable byte-for-byte.
func jsString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
