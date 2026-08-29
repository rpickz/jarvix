package desktop

import (
	"fmt"
	"sort"
	"strings"

	"github.com/rpickz/jarvix/internal/ai"
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
	// Short is the same fact in the fewest words that still say it — the chip
	// the bar widget draws beside its glyph (issue #158), where a full label
	// would crowd out the user's actual bar. Empty means "draw the bare icon":
	// the resting states carry no chip, because a permanent one stops being an
	// indicator and becomes furniture. See BarChipLabel (pending.go).
	Short string
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
	// The two background-listening marks (#4). Deliberately the same family
	// as the session's filled microphone and deliberately not the same shape:
	// hollow means the microphone is open but nobody is being listened *to*
	// yet, struck through means it is shut. A user must be able to tell the
	// three apart across a room, without reading the tooltip and without
	// being able to pick the theme's urgent colour out of its foreground.
	glyphMicrophoneOutline = "\U000F036E" // md-microphone_outline
	glyphMicrophoneOff     = "\U000F036D" // md-microphone_off
	glyphWaveform          = "\U000F147D" // md-waveform
	glyphBrain             = "\U000F09D1" // md-brain
	glyphMessage           = "\U000F0369" // md-message_text
	glyphFlash             = "\U000F0241" // md-flash
	glyphHelp              = "\U000F02D7" // md-help_circle
	glyphVolume            = "\U000F057E" // md-volume_high
	glyphCancel            = "\U000F073A" // md-cancel
	glyphAlert             = "\U000F0026" // md-alert
	glyphDots              = "\U000F01D8" // md-dots_horizontal

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
	// BarKeyWakeArmed is the privacy indicator (#4, ADR 0024): background
	// wake-word listening is running and a capture process is open on the
	// microphone. It is shown between sessions, which is exactly when nothing
	// else on screen would say so — the whole reason this widget exists.
	BarKeyWakeArmed = "wake-armed"
	// BarKeyWakeMuted is the same feature with `jarvix mute` in force: the
	// capture process has been killed. It is a state of its own rather than a
	// silent fall back to "ready" because a user who muted must be able to
	// confirm that they did.
	BarKeyWakeMuted = "wake-muted"
)

// Wake states, as the daemon publishes them in `wake.changed` and in
// `status.get`'s `wake_state`. They are a second dimension alongside the
// session state: a session state describes a turn in progress, and these
// describe the microphone between turns.
const (
	// WakeOff — background listening is not running. The bar shows the
	// session state alone, exactly as it did before the feature existed.
	WakeOff = "off"
	// WakeArmed — a capture process is running.
	WakeArmed = "armed"
	// WakeMuted — the user muted; nothing is capturing.
	WakeMuted = "muted"
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
		Short:  "Listening",
		Detail: "The microphone is open",
		Pulse:  true,
	},
	"transcribing": {
		Key: "transcribing", Glyph: glyphWaveform,
		Label:  "Transcribing",
		Short:  "Transcribing",
		Detail: "Turning what you said into text",
	},
	"thinking": {
		Key: "thinking", Glyph: glyphBrain,
		Label:  "Thinking",
		Short:  "Thinking",
		Detail: "Working out an answer",
		Pulse:  true,
	},
	"responding": {
		Key: "responding", Glyph: glyphMessage,
		Label:  "Responding",
		Short:  "Responding",
		Detail: "Writing the answer",
	},
	"awaiting_confirmation": {
		Key: "awaiting_confirmation", Glyph: glyphHelp,
		Label:  "Waiting for your answer",
		Short:  "Confirm?",
		Detail: "A tool call needs confirming",
		Urgent: true, Pulse: true,
	},
	"acting": {
		Key: "acting", Glyph: glyphFlash,
		Label:  "Running a command",
		Short:  "Running",
		Detail: "A matched intent is executing",
	},
	"speaking": {
		Key: "speaking", Glyph: glyphVolume,
		Label:  "Speaking",
		Short:  "Speaking",
		Detail: "Reading the answer aloud",
	},
	"cancelling": {
		Key: "cancelling", Glyph: glyphCancel,
		Label:  "Cancelling",
		Short:  "Cancelling",
		Detail: "Stopping the current turn",
	},
	BarKeyError: {
		Key: BarKeyError, Glyph: glyphAlert,
		Label: "Jarvix hit a problem",
		// The one non-session state that earns a chip: a held error stands
		// until the next session starts, and it is the thing the user most
		// needs told in words rather than in the theme's urgent colour.
		Short:  "Problem",
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
		Short:  "Working",
		Detail: "Jarvix is busy",
	},
	BarKeyWakeArmed: {
		Key: BarKeyWakeArmed, Glyph: glyphMicrophoneOutline,
		Label: "Listening for the wake word",
		// Says what is true rather than what is reassuring: the microphone is
		// open. The second half is what makes that acceptable, and it is the
		// claim the rest of the feature has to keep.
		Detail: "The microphone is open; audio stays on this machine until you say the wake word",
		// No pulse. A permanent state that animates permanently is not an
		// indicator, it is a distraction, and one the user would learn to
		// stop seeing — which is the opposite of the point.
	},
	BarKeyWakeMuted: {
		Key: BarKeyWakeMuted, Glyph: glyphMicrophoneOff,
		Label:  "Microphone muted",
		Detail: "Background listening is off; run jarvix unmute to resume",
	},
}

// StartHint is the one thing worth saying when the daemon is down, and the
// same sentence internal/doctor and internal/ipc already say.
const StartHint = "Start it: systemctl --user start jarvixd"

// defaultErrorDetail stands in when the error event carried no message —
// rare, but "Jarvix hit a problem —" with nothing after it is worse than a
// generic sentence.
const defaultErrorDetail = "The last turn failed"

// BarStatusFor resolves what the bar should show. The four inputs are exactly
// what the widget knows: whether its socket is up, the last `state.changed`
// it saw, the error message it is holding (empty once a new session clears
// it), and the last `wake.changed` — off, armed, or muted.
//
// Precedence is deliberate and is the part worth testing:
//  1. No socket wins over everything. A state read from a dead connection is
//     stale by definition, and "not running" is the more useful truth.
//  2. A held error wins over the state. The daemon returns to idle after a
//     failed turn, so trusting the state alone would erase the failure the
//     instant it happened — the widget would go quiet about the one thing
//     the user needs to know.
//  3. An active session wins over the wake state. During a turn the session
//     states already say the microphone is open, and in more detail.
//  4. Between sessions, the wake state. This is the privacy indicator: idle
//     with a capture process running is *not* the same thing as idle, and a
//     bar that showed "Jarvix is ready" for both would be telling a user
//     their microphone is closed when it is not.
//  5. Otherwise the state's own row, with an unknown state falling back to
//     "Working" rather than to "ready".
func BarStatusFor(connected bool, state string, errMessage string, wake string) BarState {
	if !connected {
		return barStates[BarKeyNotRunning]
	}
	if strings.TrimSpace(errMessage) != "" {
		s := barStates[BarKeyError]
		s.Detail = strings.TrimSpace(errMessage)
		return s
	}
	if state == "" || state == "idle" {
		switch wake {
		case WakeArmed:
			return barStates[BarKeyWakeArmed]
		case WakeMuted:
			return barStates[BarKeyWakeMuted]
		}
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
	// OnlyWhenWake limits the action to one wake state ("armed", "muted").
	// Empty means always. It is how the microphone control appears in the
	// panel only for users who have background listening on: muting is the
	// action a privacy indicator has to be one click away from, and offering
	// it to someone with no microphone open would be noise of the same kind
	// as offering "start Jarvix" to a running daemon.
	OnlyWhenWake string
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
		Key: "mute", Glyph: glyphMicrophoneOff,
		Label:        "Mute the microphone",
		Detail:       "Stop listening for the wake word",
		Command:      "jarvix mute",
		OnlyWhenWake: WakeArmed,
	},
	{
		Key: "unmute", Glyph: glyphMicrophoneOutline,
		Label:        "Resume background listening",
		Detail:       "Listen for the wake word again",
		Command:      "jarvix unmute",
		OnlyWhenWake: WakeMuted,
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

// BarActionsFor returns the panel's actions for the current connection and
// wake states. The daemon-down action leads, because when Jarvix is not
// running that is the only thing the user wants from this panel; the
// microphone control comes next when there is a microphone to control.
func BarActionsFor(connected bool, wake string) []BarAction {
	out := make([]BarAction, 0, len(barActions))
	for _, a := range barActions {
		if a.OnlyWhenDown && connected {
			continue
		}
		if a.OnlyWhenWake != "" && (!connected || a.OnlyWhenWake != wake) {
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
// event (docs/ipc.md), plus the three the widget synthesises — not-running,
// error, and working — and the two background-listening rows the wake state
// selects. ` + "`short`" + ` is the bar chip's word, empty where a state carries no
// chip at all.
var states = {
`)
	keys := BarStateKeys()
	for i, key := range keys {
		s := barStates[key]
		fmt.Fprintf(&b, "  %s: { key: %s, glyph: %s, label: %s, short: %s, detail: %s, urgent: %t, pulse: %t, dim: %t }%s\n",
			jsString(key), jsString(s.Key), jsString(s.Glyph), jsString(s.Label),
			jsString(s.Short), jsString(s.Detail), s.Urgent, s.Pulse, s.Dim,
			jsSeparator(i, len(keys)))
	}
	b.WriteString(`}

// statusFor mirrors desktop.BarStatusFor. Precedence: a dead socket beats any
// state (a state read from a dead connection is stale); a held error beats the
// state (the daemon returns to idle after a failed turn, so the state alone
// would erase the failure); an active session beats the wake state (the
// session states already say the microphone is open); between sessions the
// wake state decides, so "idle with a capture process running" never draws as
// plain "ready"; an unrecognised state reads as "Working" rather than "ready".
function statusFor(connected, state, errorMessage, wake) {
  if (!connected) return states["not-running"]
  var message = String(errorMessage || "").trim()
  if (message !== "") {
    var failed = states["error"]
    return {
      key: failed.key, glyph: failed.glyph, label: failed.label,
      short: failed.short, detail: message, urgent: failed.urgent,
      pulse: failed.pulse, dim: failed.dim
    }
  }
  var name = String(state || "")
  if (name === "" || name === "idle") {
    var listening = String(wake || "")
    if (listening === ` + jsString(WakeArmed) + `) return states[` + jsString(BarKeyWakeArmed) + `]
    if (listening === ` + jsString(WakeMuted) + `) return states[` + jsString(BarKeyWakeMuted) + `]
    return states["idle"]
  }
  return states[name] || states["working"]
}

// tooltip is the hover and accessible text: label, plus detail when there is
// one. Mirrors BarState.Tooltip().
function tooltip(status) {
  if (!status) return ""
  if (!status.detail) return status.label
  return status.label + " — " + status.detail
}

// The states that are phases of a session — the ones whose tooltip earns an
// elapsed counter and live tool detail. Mirrors desktop.busyBarStateKeys.
var busyStates = {
`)
	for i, key := range busyBarStateKeys {
		fmt.Fprintf(&b, "  %s: true%s\n", jsString(key), jsSeparator(i, len(busyBarStateKeys)))
	}
	b.WriteString(`}

// formatElapsed mirrors desktop.formatActivityElapsed: "12s", then "1m05s"
// past a minute.
function formatElapsed(sec) {
  sec = Math.max(0, Math.floor(sec || 0))
  if (sec < 60) return sec + "s"
  var s = sec % 60
  return Math.floor(sec / 60) + "m" + (s < 10 ? "0" : "") + s + "s"
}

// liveTooltip mirrors desktop.LiveTooltip: the state's label plus the most
// informative detail of the moment — the confirmation question while one is
// pending, the running tool's own label or name while one is in flight, the
// static detail otherwise — prefixed with how long this phase has run.
// Non-session states keep their plain tooltip.
function liveTooltip(status, elapsedSec, tool, toolDetail, question) {
  if (!status) return ""
  if (!busyStates[status.key]) return tooltip(status)
  var detail = status.detail
  var q = String(question || "")
  var td = String(toolDetail || "")
  var tn = String(tool || "")
  if (status.key === "awaiting_confirmation" && q !== "") detail = q
  else if (td !== "") detail = td.replace(/…$/, "")
  else if (tn !== "") detail = "running " + tn
  if (Math.floor(elapsedSec || 0) > 0) detail = formatElapsed(elapsedSec) + " · " + detail
  if (detail === "") return status.label
  return status.label + " — " + detail
}

// chipLabel mirrors desktop.BarChipLabel: the short words the bar draws beside
// its glyph, with the elapsed count on the states that earn one, and "" where
// the widget should draw the bare icon alone (issue #158).
function chipLabel(status, elapsedSec) {
  if (!status || !status.short) return ""
  if (busyStates[status.key] && Math.floor(elapsedSec || 0) > 0) {
    return status.short + " " + formatElapsed(elapsedSec)
  }
  return status.short
}

// The conversation window's pending assistant turn (issue #158). Everything
// below mirrors internal/desktop/pending.go, which is where it is tested; the
// window renders the string and decides nothing (ADR 0013).

// The present-tense action class per tool, mirroring desktop.toolPhrases —
// the same table the permission gate asks its short question from, so the two
// surfaces cannot name one capability two ways.
var toolDoing = {
`)
	for i, tool := range toolPhraseNames() {
		fmt.Fprintf(&b, "  %s: %s%s\n", jsString(tool),
			jsString(toolPhrases[tool].doing), jsSeparator(i, len(toolPhrases)))
	}
	b.WriteString(`}

// toolActionDoing mirrors desktop.ToolActionDoing. An unlisted tool names
// itself rather than borrowing a friendlier word.
function toolActionDoing(tool) {
  var name = String(tool || "")
  if (name === "") return ""
  return toolDoing[name] || ("Running " + name)
}

// pendingTurnLabel mirrors desktop.PendingTurnLabel: what Jarvix is doing, from
// the session state and the tool in flight. "" means there is no pending turn —
// which is how the window knows to stop showing one rather than leave it
// counting up after the turn ended.
function pendingTurnLabel(state, tool, toolDetail) {
  var s = states[String(state || "")]
  if (!s || !busyStates[s.key]) return ""
  if (s.key === "awaiting_confirmation") return s.label
  var name = String(tool || "").trim()
  var td = String(toolDetail || "").trim()
  if (s.key === "speaking" && name === "" && td === "") return ""
  var detail = td.replace(/…$/, "").trim()
  if (detail !== "") return detail
  var phrase = toolActionDoing(name)
  if (phrase !== "") return phrase
  return s.label
}

// How long a wait must run before it starts saying how long. Mirrors
// desktop.PendingElapsedThresholdSec.
var pendingElapsedThresholdSec = ` + fmt.Sprint(PendingElapsedThresholdSec) + `

// pendingTurnLine mirrors desktop.PendingTurnLine. elapsedSec is measured from
// the daemon's own phase start (state.changed's since_ms, conversation.get's
// state_since_ms), never from when this window happened to start watching.
function pendingTurnLine(state, tool, toolDetail, elapsedSec) {
  var label = pendingTurnLabel(state, tool, toolDetail)
  if (label === "") return ""
  var sec = Math.floor(elapsedSec || 0)
  if (sec >= pendingElapsedThresholdSec) return label + " · " + formatElapsed(sec)
  return label
}

// pendingTurnTierNote mirrors desktop.PendingTurnTierNote: the model tier
// appended to a pending line, separator included, "" when there is no tier.
// The labels are the Go table's, so the window cannot invent a fourth word for
// a level (issue #159).
var tierLabels = {`)
	for _, tier := range ai.TierOrder() {
		fmt.Fprintf(&b, "\n  %s: %s,", jsString(string(tier)), jsString(ai.TierLabel(tier)))
	}
	b.WriteString(`
}

function pendingTurnTierNote(tier) {
  var label = tierLabels[String(tier || "")]
  if (!label) return ""
  return " · " + label
}

// How a pending turn resolves when the user cancelled. Mirrors
// desktop.PendingTurnCancelled.
var pendingTurnCancelled = ` + jsString(PendingTurnCancelled) + `

// How a pending turn resolves when the capture produced no words (issue #191).
// Mirrors desktop.PendingTurnNothingHeard — an honest nothing, worded like a
// person and styled like nothing, never like the failure it is not.
var pendingTurnNothingHeard = ` + jsString(PendingTurnNothingHeard) + `

// pendingTurnFailed mirrors desktop.PendingTurnFailed: the activity feed's own
// sentence for the same failure, so one error is never worded twice.
function pendingTurnFailed(stage, message) {
  var text = String(message || "").trim()
  if (text === "") text = ` + jsString(defaultErrorDetail) + `
  var at = String(stage || "").trim()
  if (at === "") return text
  return ` + jsString(activityErrorLabel("")) + ` + at + " — " + text
}

// The panel's menu, in draw order. Every command already exists: the plugin's
// own IPC surface — the same handler "jarvix window" and a clicked
// notification go through — or the CLI. Mirrors desktop.BarActionsFor.
var allActions = [
`)
	for i, a := range barActions {
		fmt.Fprintf(&b, "  { key: %s, glyph: %s, label: %s, detail: %s, command: %s, onlyWhenDown: %t, onlyWhenWake: %s }%s\n",
			jsString(a.Key), jsString(a.Glyph), jsString(a.Label),
			jsString(a.Detail), jsString(a.Command), a.OnlyWhenDown,
			jsString(a.OnlyWhenWake), jsSeparator(i, len(barActions)))
	}
	b.WriteString(`]

function actions(connected, wake) {
  var listening = String(wake || "")
  return allActions.filter(function (a) {
    if (a.onlyWhenDown && connected) return false
    if (a.onlyWhenWake !== "" && (!connected || a.onlyWhenWake !== listening)) return false
    return true
  })
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
