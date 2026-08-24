package desktop

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

//go:generate go run genactivity.go

// This file is the activity feed's brain (issue #70).
//
// The daemon broadcasts nearly everything it does on the event bus and then
// discards it: what Jarvix is *doing* was observable only through journalctl.
// The activity feed keeps it — a daemon-side ring of rendered rows (see
// internal/daemon/activity.go) served over `activity.get` and pushed live as
// `activity.row` events — and this file decides what each bus event becomes
// on screen. Deciding is a table of rules, and rules belong in Go where they
// can be tested (ADR 0013: QML stays display-only). The window's only
// vocabulary — which glyph a row kind draws — is compiled to
// plugin/omarchy/ActivityState.js by `go generate ./internal/desktop`,
// exactly as the bar's state table is compiled to BarState.js.
//
// The privacy contracts hold here or nowhere: rows carry counts for memory
// and desktop context (ADR 0019, ADR 0025), lengths for typed text (ADR
// 0023), and per-tool argument summaries that default to *nothing* for any
// tool this table does not know. activity_test.go feeds events salted with
// content that must never surface and fails if a row repeats it.

// Per-row caps. The ring is bounded in rows (ui.activity_rows) and each row
// is bounded here, so the feed's memory cost has a hard ceiling however
// verbose a transcript or an error message is.
const (
	activityLabelLimit  = 120
	activityDetailLimit = 240
)

// ActivityRow is one line of the activity feed: a stable kind (the window
// looks its glyph up in the generated table), a headline, a second line, and
// whether it reports a failure. Failure is carried in the words as well —
// "Declined", "Failed at" — so the row still reads correctly to someone who
// cannot tell the urgent colour from the foreground.
type ActivityRow struct {
	Kind   string
	Label  string
	Detail string
	Failed bool
}

// Row kinds. Stable identifiers, not prose: the glyph table, the tests, and
// any future filtering key on these.
const (
	ActivityKindWake       = "wake"
	ActivityKindYou        = "you"
	ActivityKindModel      = "model"
	ActivityKindAssistant  = "assistant"
	ActivityKindTurn       = "turn"
	ActivityKindTool       = "tool"
	ActivityKindGate       = "gate"
	ActivityKindRefusal    = "refusal"
	ActivityKindIntent     = "intent"
	ActivityKindRoutine    = "routine"
	ActivityKindScript     = "script"
	ActivityKindDesktop    = "desktop"
	ActivityKindTyping     = "typing"
	ActivityKindContext    = "context"
	ActivityKindMemory     = "memory"
	ActivityKindArtifact   = "artifact"
	ActivityKindTimings    = "timings"
	ActivityKindError      = "error"
	ActivityKindCancelled  = "cancelled"
	ActivityKindAutomation = "automation"
	ActivityKindKnowledge  = "knowledge"
)

// Glyphs for the kinds barstatus.go does not already name, written as escapes
// with their Material Design names for the same reason as the bar's: a bare
// Nerd Font character in source is unreviewable. Codepoints are in the
// Material Design range the shell's resolved monospace family ships.
const (
	glyphKeyboard = "\U000F030C" // md-keyboard
	glyphConsole  = "\U000F018D" // md-console
	glyphMonitor  = "\U000F0379" // md-monitor
	glyphBook     = "\U000F00BA" // md-book
	glyphClock    = "\U000F0150" // md-clock
	glyphStop     = "\U000F04DB" // md-stop
	glyphCalClock = "\U000F00F0" // md-calendar-clock
	glyphRss      = "\U000F046B" // md-rss
)

// activityGlyphs maps each row kind to its icon. Kinds reuse the bar's
// verified glyph constants where the meaning matches; every kind gets a
// distinguishable shape, because rows must read without colour.
var activityGlyphs = map[string]string{
	ActivityKindWake:       glyphMicrophoneOutline,
	ActivityKindYou:        glyphMicrophone,
	ActivityKindModel:      glyphBrain,
	ActivityKindAssistant:  glyphMessage,
	ActivityKindTurn:       glyphDots,
	ActivityKindTool:       glyphCog,
	ActivityKindGate:       glyphHelp,
	ActivityKindRefusal:    glyphCancel,
	ActivityKindIntent:     glyphFlash,
	ActivityKindRoutine:    glyphDiagram,
	ActivityKindScript:     glyphConsole,
	ActivityKindDesktop:    glyphWindow,
	ActivityKindTyping:     glyphKeyboard,
	ActivityKindContext:    glyphMonitor,
	ActivityKindMemory:     glyphBook,
	ActivityKindArtifact:   glyphFile,
	ActivityKindTimings:    glyphClock,
	ActivityKindError:      glyphAlert,
	ActivityKindCancelled:  glyphStop,
	ActivityKindAutomation: glyphCalClock,
	ActivityKindKnowledge:  glyphRss,
}

// ActivityGlyph resolves a row kind to its icon. An unknown kind — a newer
// daemon may add kinds an older plugin has never heard of — gets the dots
// glyph rather than a hole where the icon should be.
func ActivityGlyph(kind string) string {
	if glyph, ok := activityGlyphs[kind]; ok {
		return glyph
	}
	return glyphDots
}

// ActivityKinds lists the known kinds, sorted — the generator's iteration
// order and the tests' case list, from one place.
func ActivityKinds() []string {
	kinds := make([]string, 0, len(activityGlyphs))
	for kind := range activityGlyphs {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return kinds
}

// ActivityRowsFor renders one bus event into zero, one, or two feed rows.
// Zero is the common case — deltas, state changes, and the feed's own
// `activity.row` echo (which must map to nothing, or the daemon's subscriber
// would feed on its own output) all stay off the feed. Two happens once: a
// final answer produced without a single tool call also gets the explicit
// text-only marker, because "the model claimed it acted but called no tool"
// must be diagnosable at a glance, not by noticing an absence.
func ActivityRowsFor(eventType string, data map[string]any) []ActivityRow {
	one := func(r ActivityRow) []ActivityRow { return []ActivityRow{clipActivityRow(r)} }
	switch eventType {
	case "wake.detected":
		return one(ActivityRow{Kind: ActivityKindWake,
			Label: "Wake word heard", Detail: "Starting a session"})
	case "transcript.final":
		return one(ActivityRow{Kind: ActivityKindYou,
			Label: "You", Detail: activityString(data, "text")})
	case "assistant.started":
		return one(ActivityRow{Kind: ActivityKindModel,
			Label: "Asking " + activityString(data, "provider")})
	case "assistant.finished":
		return assistantFinishedRows(data)
	case "tool.started":
		return one(toolStartedRow(data))
	case "tool.finished":
		return one(toolFinishedRow(data))
	case "tool.confirmation_required":
		return one(ActivityRow{Kind: ActivityKindGate,
			Label:  "Waiting for your yes or no",
			Detail: activityString(data, "command")})
	case "tool.confirmed":
		return one(ActivityRow{Kind: ActivityKindGate,
			Label:  "Approved: " + activityString(data, "tool"),
			Detail: activityString(data, "command")})
	case "tool.declined":
		return one(ActivityRow{Kind: ActivityKindRefusal, Failed: true,
			Label:  "Declined: " + activityString(data, "tool"),
			Detail: joinActivity(declineReason(activityString(data, "source")), activityString(data, "command"))})
	case "tool.denied":
		return one(ActivityRow{Kind: ActivityKindRefusal, Failed: true,
			Label:  "Denied by policy: " + activityString(data, "tool"),
			Detail: joinActivity(activityString(data, "rule"), activityString(data, "command"))})
	case "typing.audit":
		return one(typingAuditRow(data))
	case "desktop.action":
		return one(desktopActionRow(data))
	case "desktop.refusal":
		return one(ActivityRow{Kind: ActivityKindRefusal, Failed: true,
			Label:  desktopRefusalLabel(activityString(data, "verb"), activityString(data, "target")),
			Detail: activityString(data, "reason")})
	case "intent.executed":
		return one(intentRow(data))
	case "routine.started":
		steps, _ := activityInt(data, "steps")
		return one(ActivityRow{Kind: ActivityKindRoutine,
			Label:  "Routine: " + activityString(data, "routine"),
			Detail: fmt.Sprintf("%d %s", steps, pluralActivity(steps, "step", "steps"))})
	case "routine.step":
		return one(routineStepRow(data))
	case "routine.finished":
		return one(ActivityRow{Kind: ActivityKindRoutine,
			Label:  "Routine finished: " + activityString(data, "routine"),
			Detail: activityString(data, "summary")})
	case "script.started":
		// The path in the feed on purpose (ADR 0030): it is exactly what the
		// gate's confirmation named, and the audit surface repeating it is
		// what makes a substituted file visible after the fact too. Output
		// never appears — the event does not carry it, and this row has no
		// field that could.
		return one(ActivityRow{Kind: ActivityKindScript,
			Label:  "Script: " + activityString(data, "script"),
			Detail: activityString(data, "path")})
	case "script.finished":
		return one(scriptFinishedRow(data))
	case "automation.fired":
		// A schedule fired (ADR 0032). The run itself reports through the
		// ordinary rows that follow; this row is the trigger's own record —
		// the clock, not a person, started what comes next.
		return one(ActivityRow{Kind: ActivityKindAutomation,
			Label:  "Schedule fired: " + activityString(data, "name"),
			Detail: activityString(data, "schedule")})
	case "automation.skipped":
		return one(ActivityRow{Kind: ActivityKindAutomation,
			Label:  "Schedule skipped: " + activityString(data, "name"),
			Detail: activityString(data, "reason")})
	case "automation.refused":
		return one(ActivityRow{Kind: ActivityKindRefusal, Failed: true,
			Label:  "Scheduled run refused: " + activityString(data, "name"),
			Detail: joinActivity(activityString(data, "reason"), activityString(data, "rule"))})
	case "automation.missed":
		return one(ActivityRow{Kind: ActivityKindAutomation,
			Label:  "Missed while off: " + activityString(data, "name"),
			Detail: joinActivity("was due "+activityString(data, "due"), "reported, never re-fired")})
	case "config.entry_changed":
		// A save through the entry pipeline (#99/#100/#105): one routine,
		// script, or knowledge feed created, edited, or deleted through
		// config.upsert_entry / config.delete_entry — by the window's form or
		// by the assistant's tool, and the row says which. The feed names the
		// entry — configuration changes are actions too, and a schedule that
		// appears or stops must be traceable to its save.
		return one(entryChangedRow(data))
	case "config.setting_changed":
		// One registry setting changed (issue #105): the settings equivalent
		// of the entry row above, from the settings screen, the CLI, or the
		// assistant's config.write_setting. Key and source only, never the
		// value — a value can be a whole system prompt, and the row exists to
		// say what moved and who moved it, not to republish content. The tool
		// kind (a cog) rather than a new kind: an older plugin would render
		// an unknown kind with the fallback glyph, and a settings turn *is*
		// the machinery being adjusted.
		return one(ActivityRow{Kind: ActivityKindTool,
			Label:  "Setting changed: " + activityString(data, "key"),
			Detail: "config.toml, " + configChangeSource(data)})
	case "memory.entry_changed":
		// A fact added or edited from the window's Memory tab (#100), through
		// the memory book's own write path. Id and size only, never the words
		// — the memory privacy contract (counts, not content) holds for
		// window saves exactly as for the memory.remember tool.
		return one(memoryEntryChangedRow(data))
	case "artifact.created":
		return one(ActivityRow{Kind: ActivityKindArtifact,
			Label:  "Artifact created",
			Detail: joinActivity(activityString(data, "type"), activityString(data, "path"))})
	case "context.captured":
		return contextCapturedRows(data)
	case "memory.injected":
		return one(memoryInjectedRow(data))
	case "session.timings":
		return timingsRows(data)
	case "error":
		return one(ActivityRow{Kind: ActivityKindError, Failed: true,
			Label:  "Failed at " + activityString(data, "stage"),
			Detail: activityString(data, "message")})
	case "session.cancelled":
		return one(ActivityRow{Kind: ActivityKindCancelled,
			Label: "Cancelled", Detail: activityString(data, "reason")})
	}
	return nil
}

// assistantFinishedRows renders the final answer, plus the text-only marker
// when the event says no tool was called. The marker is the row issue #70
// exists for: the daemon claimed launches and focus changes in prose while
// its logs showed tool_calls=0, and only journalctl could prove it.
func assistantFinishedRows(data map[string]any) []ActivityRow {
	content := activityString(data, "content")
	if content == "" {
		return nil // the empty answer fails the session; the error row explains
	}
	rows := []ActivityRow{clipActivityRow(ActivityRow{
		Kind: ActivityKindAssistant, Label: "Jarvix", Detail: content})}
	if calls, ok := activityInt(data, "tool_calls"); ok && calls == 0 {
		rows = append(rows, clipActivityRow(ActivityRow{
			Kind:  ActivityKindTurn,
			Label: "Text-only turn — no tools ran",
			Detail: "The model answered in words alone; " +
				"anything it claims to have done on the machine, it did not do"}))
	}
	return rows
}

func toolStartedRow(data map[string]any) ActivityRow {
	tool := activityString(data, "tool")
	detail := activityString(data, "detail")
	if detail == "" {
		detail = SummariseToolArgs(tool, activityString(data, "arguments"))
	}
	return ActivityRow{Kind: ActivityKindTool, Label: "Tool: " + tool, Detail: detail}
}

func toolFinishedRow(data map[string]any) ActivityRow {
	tool := activityString(data, "tool")
	outcome := activityString(data, "outcome")
	var dur string
	if ms, ok := activityInt(data, "duration_ms"); ok {
		dur = formatActivityDuration(ms)
	}
	if outcome == "error" {
		return ActivityRow{Kind: ActivityKindTool, Failed: true,
			Label: "Tool failed: " + tool, Detail: dur}
	}
	detail := dur
	if outcome != "" {
		detail = joinActivity(outcome, dur)
	}
	return ActivityRow{Kind: ActivityKindTool, Label: "Finished: " + tool, Detail: detail}
}

// declineReason words a tool.declined source for the feed. The daemon's
// vocabulary (docs/ipc.md) is closed; an unknown source is shown as itself
// rather than hidden, because a refusal must never lose its reason.
func declineReason(source string) string {
	switch source {
	case "cli", "text", "voice":
		return "you said no"
	case "timeout":
		return "no answer before the timeout"
	case "interrupted":
		return "the session was interrupted"
	case "error", "unavailable":
		return "you could not be asked"
	}
	return source
}

// typingAuditRow renders a typing decision (ADR 0023). The event never
// carries the typed text, and this row never invents a place for it: lengths
// and outcomes only, with the reason when the daemon refused.
func typingAuditRow(data map[string]any) ActivityRow {
	window := activityString(data, "window")
	target := "into " + window
	if b, ok := data["terminal"].(bool); ok && b {
		target += " (a terminal)"
	}
	if b, ok := data["approved"].(bool); ok && b {
		target += " · approved by you"
	}
	switch outcome := activityString(data, "outcome"); outcome {
	case "typed":
		chars, _ := activityInt(data, "chars")
		return ActivityRow{Kind: ActivityKindTyping,
			Label:  fmt.Sprintf("Typed %d %s", chars, pluralActivity(chars, "character", "characters")),
			Detail: target}
	case "pressed":
		return ActivityRow{Kind: ActivityKindTyping,
			Label: "Pressed " + activityString(data, "key"), Detail: target}
	default:
		reason := activityString(data, "reason")
		if reason == "" {
			reason = outcome
		}
		return ActivityRow{Kind: ActivityKindRefusal, Failed: true,
			Label: "Typing " + outcome, Detail: reason}
	}
}

func desktopActionRow(data map[string]any) ActivityRow {
	verbs := map[string]string{
		"focus": "Focused", "move": "Moved", "close": "Closed",
		"launch": "Launched", "list": "Listed windows",
	}
	verb := activityString(data, "verb")
	label, ok := verbs[verb]
	if !ok {
		label = "Desktop: " + verb
	}
	return ActivityRow{Kind: ActivityKindDesktop, Label: label,
		Detail: activityString(data, "target")}
}

// desktopRefusalLabel is the headline of the row the user needed on the day
// this feature was asked for: "Launch refused: firefox", with the daemon's
// actual reason on the second line.
func desktopRefusalLabel(verb, target string) string {
	verbs := map[string]string{
		"focus": "Focus refused", "move": "Move refused",
		"close": "Close refused", "launch": "Launch refused",
	}
	label, ok := verbs[verb]
	if !ok {
		label = verb + " refused"
	}
	if target == "" {
		return label
	}
	return label + ": " + target
}

func intentRow(data map[string]any) ActivityRow {
	name := activityString(data, "intent")
	if routine := activityString(data, "routine"); routine != "" {
		name += " (routine " + routine + ")"
	}
	if script := activityString(data, "script"); script != "" {
		name += " (script " + script + ")"
	}
	if activityString(data, "status") == "failed" {
		return ActivityRow{Kind: ActivityKindIntent, Failed: true,
			Label: "Intent failed: " + name, Detail: activityString(data, "error")}
	}
	label := "Intent: " + name
	var dur string
	if ms, ok := activityInt(data, "duration_ms"); ok {
		dur = formatActivityDuration(ms)
	}
	return ActivityRow{Kind: ActivityKindIntent, Label: label,
		Detail: joinActivity(activityString(data, "acknowledgement"), dur)}
}

// scriptFinishedRow renders one script run's ending: exit status and
// duration, always — a feed where only failures carried the exit code would
// make "did my backup actually run?" answerable only by trusting silence.
func scriptFinishedRow(data map[string]any) ActivityRow {
	name := activityString(data, "script")
	code, _ := activityInt(data, "exit_code")
	var dur string
	if ms, ok := activityInt(data, "duration_ms"); ok {
		dur = formatActivityDuration(ms)
	}
	if activityString(data, "status") == "failed" {
		detail := fmt.Sprintf("exit %d", code)
		if b, ok := data["timed_out"].(bool); ok && b {
			detail = "stopped at the timeout"
		}
		return ActivityRow{Kind: ActivityKindScript, Failed: true,
			Label: "Script failed: " + name, Detail: joinActivity(detail, dur)}
	}
	return ActivityRow{Kind: ActivityKindScript,
		Label: "Script finished: " + name, Detail: joinActivity("exit 0", dur)}
}

// entryChangedRow words one form save (#99, #100): a routine, script, or
// knowledge feed created, edited, or deleted from the window. The label
// names the entry — a schedule that appears or stops must be traceable to
// the save that did it — and the glyph follows the entry's kind so the row
// sits beside that entry's runs.
func entryChangedRow(data map[string]any) ActivityRow {
	kind := activityString(data, "kind")
	row := ActivityRow{Kind: ActivityKindAutomation}
	word := "Entry"
	switch kind {
	case "routine":
		row.Kind, word = ActivityKindRoutine, "Routine"
	case "script":
		row.Kind, word = ActivityKindScript, "Script"
	case "feed":
		row.Kind, word = ActivityKindKnowledge, "Feed"
	}
	action := activityString(data, "action")
	switch action {
	case "created", "edited", "deleted":
	default:
		action = "changed"
	}
	row.Label = word + " " + action + ": " + activityString(data, "name")
	verb := "saved"
	if action == "deleted" {
		verb = "removed"
	}
	// Who did it matters (issue #105): a self-administered change must be
	// auditable as such at a glance, so the assistant's saves say so, while
	// everything else keeps the window wording — the forms are the only other
	// client of these verbs.
	if activityString(data, "source") == "assistant" {
		row.Detail = "config.toml, " + verb + " by the assistant"
	} else {
		row.Detail = "config.toml, " + verb + " from the window"
	}
	return row
}

// configChangeSource words a settings change's source for the feed: the
// assistant's config.write_setting says so; every other writer of config.set
// — the settings screen, the CLI — is the user acting directly.
func configChangeSource(data map[string]any) string {
	if activityString(data, "source") == "assistant" {
		return "changed by the assistant"
	}
	return "changed by you"
}

// memoryEntryChangedRow words one memory-form save (#100): the fact named by
// its stable id ("m3" — the handle every memory surface shares), the action,
// and the content's size in place of the content itself.
func memoryEntryChangedRow(data map[string]any) ActivityRow {
	action := activityString(data, "action")
	switch action {
	case "added", "edited", "pinned", "unpinned":
	default:
		action = "changed"
	}
	chars, _ := activityInt(data, "chars")
	return ActivityRow{Kind: ActivityKindMemory,
		Label: "Fact " + action + ": " + activityString(data, "id"),
		Detail: fmt.Sprintf("memory.toml, saved from the window · %d %s (content not shown)",
			chars, pluralActivity(chars, "character", "characters"))}
}

func routineStepRow(data map[string]any) ActivityRow {
	step, _ := activityInt(data, "step")
	app := activityString(data, "app")
	if activityString(data, "status") == "failed" {
		return ActivityRow{Kind: ActivityKindRoutine, Failed: true,
			Label:  fmt.Sprintf("Step %d failed: %s", step, app),
			Detail: activityString(data, "detail")}
	}
	workspace, _ := activityInt(data, "workspace")
	detail := fmt.Sprintf("placed on workspace %d", workspace)
	if b, ok := data["launched"].(bool); ok && !b {
		detail += " · already open"
	}
	return ActivityRow{Kind: ActivityKindRoutine,
		Label: fmt.Sprintf("Step %d: %s", step, app), Detail: detail}
}

// contextCapturedRows summarises a desktop-context capture: source names,
// sizes, and flags — never text. The event itself already keeps content off
// the bus (ADR 0019); this function reads only the fields that cannot carry
// it, so a future field could not leak through the feed either.
func contextCapturedRows(data map[string]any) []ActivityRow {
	sources, _ := data["sources"].([]any)
	if len(sources) == 0 {
		return nil
	}
	var names []string
	var chars, truncated, redacted int
	for _, s := range sources {
		m, ok := s.(map[string]any)
		if !ok {
			continue
		}
		if name := activityString(m, "source"); name != "" {
			names = append(names, name)
		}
		if c, ok := activityInt(m, "chars"); ok {
			chars += c
		}
		if b, ok := m["truncated"].(bool); ok && b {
			truncated++
		}
		if b, ok := m["redacted"].(bool); ok && b {
			redacted++
		}
	}
	detail := fmt.Sprintf("%d %s · %d chars (%s)",
		len(sources), pluralActivity(len(sources), "source", "sources"), chars, strings.Join(names, ", "))
	if truncated > 0 {
		detail += fmt.Sprintf(" · %d truncated", truncated)
	}
	if redacted > 0 {
		detail += fmt.Sprintf(" · %d redacted", redacted)
	}
	return []ActivityRow{clipActivityRow(ActivityRow{
		Kind: ActivityKindContext, Label: "Desktop context gathered", Detail: detail})}
}

// memoryInjectedRow reports what the knowledge base offered the model: counts
// and estimates only, exactly what the event carries (ADR 0025). No field of
// this event is a fact, and no field of this row could hold one.
func memoryInjectedRow(data map[string]any) ActivityRow {
	facts, _ := activityInt(data, "facts")
	detail := fmt.Sprintf("%d %s", facts, pluralActivity(facts, "fact", "facts"))
	if tokens, ok := activityInt(data, "est_tokens"); ok && tokens > 0 {
		detail += fmt.Sprintf(" · ~%d tokens", tokens)
	}
	if trimmed, ok := activityInt(data, "trimmed"); ok && trimmed > 0 {
		detail += fmt.Sprintf(" · %d kept out by the token cap", trimmed)
	}
	// The unpinned rest under the split (#104): not a loss like the trim, so
	// the wording says where those facts went instead of that they were cut.
	if searchable, ok := activityInt(data, "searchable"); ok && searchable > 0 {
		detail += fmt.Sprintf(" · %d behind memory.search", searchable)
	}
	return ActivityRow{Kind: ActivityKindMemory, Label: "Remembered facts offered", Detail: detail}
}

// timingsRows renders the latency budget line. Stages that did not happen are
// absent from the event and stay absent here — a typed question honestly has
// no transcribe figure.
func timingsRows(data map[string]any) []ActivityRow {
	stages := []struct{ key, name string }{
		{"capture_to_transcript_ms", "transcribe"},
		{"context_ms", "context"},
		{"transcript_to_first_delta_ms", "model"},
		// The excluded spans (issue #72): time inside tool executions and
		// time spent waiting on the user's confirmation answer — the turn's
		// real length, attributed to neither Jarvix nor the model.
		{"tool_ms", "tools"},
		{"confirm_wait_ms", "confirming"},
		{"jarvix_ms", "jarvix"},
		{"release_to_first_audio_ms", "to audio"},
	}
	var parts []string
	for _, stage := range stages {
		if ms, ok := activityInt(data, stage.key); ok {
			parts = append(parts, stage.name+" "+formatActivityDuration(ms))
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return []ActivityRow{clipActivityRow(ActivityRow{
		Kind: ActivityKindTimings, Label: "Timings", Detail: strings.Join(parts, " · ")})}
}

// SummariseToolArgs is the one-line argument summary for a tool row, decided
// per tool. The default is the important rule: a tool this table does not
// know shows *no* arguments. Arguments are model-authored JSON that can carry
// anything — dictated text, memory content, file bodies — so the safe reading
// of "unknown" is silence, and each entry below states what is safe to show.
//
// Tool names are literals rather than the internal/tools constants because
// tools imports this package; activity_tools_test.go (an external test, which
// may import both) pins each literal to its constant so a rename cannot
// silently turn a summarised tool into an unknown one.
func SummariseToolArgs(tool, arguments string) string {
	var args map[string]any
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return ""
	}
	str := func(key string) string { return activityString(args, key) }
	runes := func(key string) int { return len([]rune(str(key))) }
	switch tool {
	case "shell.run":
		// The exact command: the confirmation gate already publishes it
		// verbatim, so the feed showing it reveals nothing new.
		return str("command")
	case "artifact.create":
		// Format and title only — the source can embed whole documents.
		return joinActivity(str("format"), str("title"))
	case "advisor.ask":
		// The advisor's name; the question is model-authored content.
		return "advisor " + str("advisor")
	case "desktop.launch_app":
		return str("app")
	case "desktop.focus_window", "desktop.close_window":
		if w := str("window"); w != "" {
			return w
		}
		return "the focused window"
	case "desktop.move_window":
		workspace, _ := activityInt(args, "workspace")
		return joinActivity(str("window"), fmt.Sprintf("workspace %d", workspace))
	case "desktop.list_windows":
		return ""
	case "typing.type_text":
		// Length only, never the characters (ADR 0023): the payload may be a
		// password the user dictated.
		n := runes("text")
		return fmt.Sprintf("%d %s (text not shown)", n, pluralActivity(n, "character", "characters"))
	case "typing.press_key":
		return str("key")
	case "memory.remember":
		// A fact's size, never the fact (ADR 0025): the feed reports that the
		// store was written, and `jarvix memory list` is where content lives.
		n := runes("content")
		return fmt.Sprintf("a fact of %d %s (content not shown)", n, pluralActivity(n, "character", "characters"))
	case "memory.search", "memory.forget":
		// The query can quote the fact it is looking for.
		return "query not shown"
	case "conversations.search":
		// Queries are conversation content; the search contract keeps them
		// out of logs, and the feed follows it (ADR 0028 / issue #59).
		n := runes("query")
		return fmt.Sprintf("query of %d %s (not shown)", n, pluralActivity(n, "character", "characters"))
	case "config.list_entries":
		return str("family")
	case "config.get_entry", "config.delete_entry":
		// Family and entry name — what is being read or removed; the entry's
		// content never travels in arguments for these two.
		return joinActivity(str("family"), str("name"))
	case "config.write_entry":
		// Family and name only, never the draft: the entry body carries
		// command-bearing fields, and the confirmation gate is the surface
		// that shows those verbatim (issue #105) — the feed's tool row just
		// says which entry was being written.
		name := str("name")
		if name == "" {
			var nested struct {
				Entry map[string]any `json:"entry"`
			}
			if json.Unmarshal([]byte(arguments), &nested) == nil {
				name = activityString(nested.Entry, "name")
			}
		}
		return joinActivity(str("family"), name)
	case "config.read_settings":
		return str("prefix")
	case "config.write_setting":
		// The key only, never the value: a value can be a whole system
		// prompt, and the gate's card already showed it verbatim where the
		// approval happened.
		return str("key")
	}
	return ""
}

// LiveTooltip composes the bar's hover text for the current moment: the
// state's label, then the most informative detail the widget holds — the
// confirmation question while one is pending, the running tool's own label
// ("Consulting claude…") or name while one is in flight, the state's static
// detail otherwise — prefixed with how long this phase has been going.
// "Thinking — 12s · consulting claude" from a glance, no journal required.
//
// Idle, error, not-running and the wake rows keep their plain tooltip: those
// states are not phases of a session, and a ticking counter on "Jarvix is
// ready" would be noise.
func LiveTooltip(s BarState, elapsedSec int, tool, toolDetail, question string) string {
	if !busyBarState(s.Key) {
		return s.Tooltip()
	}
	detail := s.Detail
	switch {
	case s.Key == "awaiting_confirmation" && question != "":
		detail = question
	case toolDetail != "":
		detail = strings.TrimSuffix(toolDetail, "…")
	case tool != "":
		detail = "running " + tool
	}
	if elapsedSec > 0 {
		detail = formatActivityElapsed(elapsedSec) + " · " + detail
	}
	if detail == "" {
		return s.Label
	}
	return s.Label + " — " + detail
}

// busyBarStateKeys are the bar states that are phases of a session — the ones
// whose tooltip earns an elapsed counter and live tool detail. Sorted, for
// the generator.
var busyBarStateKeys = []string{
	"acting", "awaiting_confirmation", "cancelling", "listening",
	"responding", "speaking", "thinking", "transcribing", BarKeyUnknown,
}

func busyBarState(key string) bool {
	for _, k := range busyBarStateKeys {
		if k == key {
			return true
		}
	}
	return false
}

// formatActivityElapsed renders whole seconds for the tooltip: "12s", then
// "1m05s" past a minute — long tool rounds are exactly when the counter earns
// its keep.
func formatActivityElapsed(sec int) string {
	if sec < 0 {
		sec = 0
	}
	if sec < 60 {
		return fmt.Sprintf("%ds", sec)
	}
	return fmt.Sprintf("%dm%02ds", sec/60, sec%60)
}

// FormatActivityDuration renders a millisecond figure the way a person reads
// one: milliseconds under a second, one decimal of seconds above. Exported
// for the Automations tab's last-run line (#93), so the tab and the activity
// feed can never word the same duration two ways.
func FormatActivityDuration(ms int) string {
	return formatActivityDuration(ms)
}

// formatActivityDuration is FormatActivityDuration's body, kept unexported
// for the wording helpers in this file.
func formatActivityDuration(ms int) string {
	if ms < 0 {
		ms = 0
	}
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	return fmt.Sprintf("%.1fs", float64(ms)/1000)
}

// clipActivityRow applies the per-row caps. Runes, not bytes: a transcript
// must never be cut mid-character.
func clipActivityRow(r ActivityRow) ActivityRow {
	r.Label = clipActivityText(r.Label, activityLabelLimit)
	r.Detail = clipActivityText(r.Detail, activityDetailLimit)
	return r
}

func clipActivityText(s string, limit int) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return strings.TrimSpace(string(runes[:limit])) + "…"
}

// joinActivity joins the non-empty parts of a detail line.
func joinActivity(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, " · ")
}

func pluralActivity(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// activityString reads a string field from event data, tolerating absence.
func activityString(data map[string]any, key string) string {
	s, _ := data[key].(string)
	return s
}

// activityInt reads a numeric field from event data. Bus events reach this
// code both in-process (native int/int64) and after a JSON round trip
// (float64), so all three spellings of a number are accepted.
func activityInt(data map[string]any, key string) (int, bool) {
	switch v := data[key].(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	}
	return 0, false
}

// RenderActivityJS compiles the window's activity vocabulary — the glyph per
// row kind — into the QML-importable library, the same arrangement as
// RenderBarStateJS. Row *text* is deliberately absent: rows arrive from the
// daemon already rendered (activity.get and activity.row), so the window
// never re-derives a sentence and the two surfaces cannot drift.
func RenderActivityJS() string {
	var b strings.Builder
	b.WriteString(`.pragma library

// Code generated by internal/desktop/genactivity.go. DO NOT EDIT.
//
// The Jarvix activity feed's display vocabulary, compiled from the Go table
// in internal/desktop/activity.go — the source of truth, and the only place
// it is tested (ADR 0013 keeps QML display-only). Rows arrive from the
// daemon already worded; this file only knows which glyph each row kind
// draws. Regenerate with:
//
//     go generate ./internal/desktop

var glyphs = {
`)
	kinds := ActivityKinds()
	for i, kind := range kinds {
		fmt.Fprintf(&b, "  %s: %s%s\n",
			jsString(kind), jsString(activityGlyphs[kind]), jsSeparator(i, len(kinds)))
	}
	b.WriteString(`}

var fallbackGlyph = ` + jsString(glyphDots) + `

// glyphFor mirrors desktop.ActivityGlyph: an unknown kind — a newer daemon
// may add kinds this build has never heard of — draws the dots glyph rather
// than a hole.
function glyphFor(kind) {
  return glyphs[String(kind || "")] || fallbackGlyph
}
`)
	return b.String()
}
