package desktop

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The pending assistant turn (issue #158) exists because several seconds of
// blank message list read as nothing happening. Its whole value is in what it
// says, so what it says is decided here and tested here; the window renders
// the string (ADR 0013).

func TestPendingTurnSaysWhatIsHappening(t *testing.T) {
	cases := []struct {
		name       string
		state      string
		tool       string
		toolDetail string
		want       string
	}{
		{"thinking with no tool", "thinking", "", "", "Thinking"},
		{"listening", "listening", "", "", "Listening"},
		{"transcribing", "transcribing", "", "", "Transcribing"},
		{"responding", "responding", "", "", "Responding"},
		{"cancelling", "cancelling", "", "", "Cancelling"},
		// Reading the finished answer aloud is not a wait: the words are
		// already on screen. A "Speaking" row under every completed answer
		// would add a bubble to every turn.
		{"speaking with nothing running", "speaking", "", "", ""},
		// It is a wait again the moment something is running under the
		// narration — a tool round narrated aloud is exactly the case where
		// the user most needs telling what is going on.
		{"speaking over a tool round", "speaking", "shell.run", "", "Running a shell command"},
		{"speaking over a labelled tool", "speaking", "advisor.ask", "Consulting claude…",
			"Consulting claude"},
		// A tool the model called mid-think: the pending turn says which kind
		// of thing is running, not a generic "Thinking".
		{"shell round", "thinking", "shell.run", "", "Running a shell command"},
		{"script round", "thinking", "script.run", "", "Running one of your scripts"},
		{"typing round", "thinking", "typing.type_text", "", "Typing on your keyboard"},
		// A tool that publishes its own progress label wins: it is more
		// specific than the class, and it is already the wording the bar's
		// tooltip and the activity feed show for the same call.
		{"labelled tool", "thinking", "advisor.ask", "Consulting claude…", "Consulting claude"},
		{"labelled tool while responding", "responding", "knowledge.get", "Checking the news feed…",
			"Checking the news feed"},
		// A tool this build has never heard of names itself rather than
		// borrowing a friendlier word for what it might be doing.
		{"unknown tool", "thinking", "future.tool", "", "Running future.tool"},
		// The confirmation card is the affordance; the pending turn only says
		// a question is open, and never repeats the command the card shows
		// verbatim.
		{"awaiting confirmation", "awaiting_confirmation", "", "", "Waiting for your answer"},
		{"awaiting confirmation ignores a stale tool", "awaiting_confirmation", "shell.run",
			"Consulting claude…", "Waiting for your answer"},
		// Nothing to wait for: the window closes the pending turn on "".
		{"idle", "idle", "", "", ""},
		{"error", "error", "", "", ""},
		{"empty", "", "", "", ""},
		{"unknown state", "dreaming", "", "", ""},
		{"wake armed is not a session phase", BarKeyWakeArmed, "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := PendingTurnLabel(c.state, c.tool, c.toolDetail); got != c.want {
				t.Errorf("PendingTurnLabel(%q, %q, %q) = %q, want %q",
					c.state, c.tool, c.toolDetail, got, c.want)
			}
		})
	}
}

// The elapsed count is the second half of "waiting is legible": a wait that
// says what it is doing but not how long it has been doing it still leaves the
// user guessing. It appears only past the threshold, so an answer that starts
// streaming quickly never shows a counter that flashes in and straight back
// out — which would make a fast answer look busier than a slow one.
func TestPendingTurnCountsOnlyPastTheThreshold(t *testing.T) {
	cases := []struct {
		elapsed int
		want    string
	}{
		{-1, "Thinking"},
		{0, "Thinking"},
		{1, "Thinking"},
		{PendingElapsedThresholdSec, "Thinking · 2s"},
		{6, "Thinking · 6s"},
		{59, "Thinking · 59s"},
		{60, "Thinking · 1m00s"},
		{125, "Thinking · 2m05s"},
	}
	for _, c := range cases {
		if got := PendingTurnLine("thinking", "", "", c.elapsed); got != c.want {
			t.Errorf("PendingTurnLine at %ds = %q, want %q", c.elapsed, got, c.want)
		}
	}
	// A state with nothing to say says nothing, however long it has been
	// saying it: the elapsed count must never resurrect a closed pending turn.
	if got := PendingTurnLine("idle", "", "", 90); got != "" {
		t.Errorf("idle with 90s elapsed = %q, want nothing at all", got)
	}
	if got := PendingTurnLine("thinking", "advisor.ask", "Consulting claude…", 12); got != "Consulting claude · 12s" {
		t.Errorf("labelled tool line = %q", got)
	}
}

// A wait that ends must say so. Freezing on the last count is the failure mode
// this feature removes, not one it is allowed to introduce.
func TestPendingTurnResolvesHonestly(t *testing.T) {
	if PendingTurnCancelled != "Cancelled" {
		t.Errorf("cancelled wording = %q", PendingTurnCancelled)
	}
	cases := []struct {
		stage, message, want string
	}{
		{"assistant", "the provider timed out", "Failed at assistant — the provider timed out"},
		{"stt", "no speech recognised", "Failed at stt — no speech recognised"},
		// The feed's own sentence for the same event, so one failure is never
		// worded two ways across two surfaces.
		{"tts", "boom", ActivityRowsFor("error", map[string]any{
			"stage": "tts", "message": "boom"})[0].Label + " — boom"},
		// A failure that arrived with nothing to say still says something:
		// "Failed at assistant —" with a hole after it is worse than a
		// generic sentence.
		{"assistant", "   ", "Failed at assistant — " + defaultErrorDetail},
		{"", "the socket closed", "the socket closed"},
		{"", "", defaultErrorDetail},
	}
	for _, c := range cases {
		if got := PendingTurnFailed(c.stage, c.message); got != c.want {
			t.Errorf("PendingTurnFailed(%q, %q) = %q, want %q", c.stage, c.message, got, c.want)
		}
	}
}

// The gate's question and the pending turn's narration are one fact about a
// tool in two grammatical forms. They come off one table so they cannot drift
// into naming the same capability differently — "May I run a shell command?"
// answered, then "Running a shell command" while it runs.
func TestToolActionPhrasesAreOneTableInTwoTenses(t *testing.T) {
	for tool, phrase := range toolPhrases {
		if phrase.ask == "" || phrase.doing == "" {
			t.Errorf("%s: both tenses are required", tool)
		}
		if got := ToolActionAsk(tool); got != phrase.ask {
			t.Errorf("%s: ask = %q, want %q", tool, got, phrase.ask)
		}
		if got := ToolActionDoing(tool); got != phrase.doing {
			t.Errorf("%s: doing = %q, want %q", tool, got, phrase.doing)
		}
		if strings.ToUpper(phrase.doing[:1]) != phrase.doing[:1] {
			t.Errorf("%s: %q starts a line in the transcript and must be capitalised",
				tool, phrase.doing)
		}
		if strings.ToUpper(phrase.ask[:1]) == phrase.ask[:1] {
			t.Errorf("%s: %q sits inside \"May I …?\" and must not be capitalised",
				tool, phrase.ask)
		}
	}
	// The unknown-tool fallbacks name the tool. A future tool announced as
	// something friendlier than its own name is a question the user cannot
	// map back to a capability (ADR 0014).
	if got := ToolActionAsk("future.tool"); got != "use the future.tool tool" {
		t.Errorf("unknown ask = %q", got)
	}
	if got := ToolActionDoing("future.tool"); got != "Running future.tool" {
		t.Errorf("unknown doing = %q", got)
	}
	if got := ToolActionDoing(""); got != "" {
		t.Errorf("no tool = %q, want nothing", got)
	}
}

// The bar's problem was the mirror image of the window's: a monochrome glyph
// swap in a dense bar says nothing without a hover. Every phase of a session
// now carries its word; every resting state carries none, because a permanent
// chip stops being an indicator within a day.
func TestBarChipSpeaksWhileBusyAndVanishesAtRest(t *testing.T) {
	cases := []struct {
		name    string
		status  BarState
		elapsed int
		want    string
	}{
		{"thinking", BarStatusFor(true, "thinking", "", WakeOff), 4, "Thinking 4s"},
		{"thinking under a second", BarStatusFor(true, "thinking", "", WakeOff), 0, "Thinking"},
		{"a long round", BarStatusFor(true, "thinking", "", WakeOff), 95, "Thinking 1m35s"},
		{"listening", BarStatusFor(true, "listening", "", WakeOff), 2, "Listening 2s"},
		{"awaiting confirmation", BarStatusFor(true, "awaiting_confirmation", "", WakeOff), 3, "Confirm? 3s"},
		{"an unknown state", BarStatusFor(true, "dreaming", "", WakeOff), 7, "Working 7s"},
		// A held error is not a resting state — it stands until the next
		// session and the user has to know — but it is not a phase either, so
		// it carries no counter.
		{"held error", BarStatusFor(true, "idle", "it went wrong", WakeOff), 40, "Problem"},
		// Nothing at rest: the bar is the user's, and normal use adds nothing.
		{"idle", BarStatusFor(true, "idle", "", WakeOff), 0, ""},
		{"idle for a long time", BarStatusFor(true, "idle", "", WakeOff), 900, ""},
		{"wake armed", BarStatusFor(true, "idle", "", WakeArmed), 900, ""},
		{"wake muted", BarStatusFor(true, "idle", "", WakeMuted), 900, ""},
		{"daemon down", BarStatusFor(false, "thinking", "", WakeOff), 5, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := BarChipLabel(c.status, c.elapsed); got != c.want {
				t.Errorf("BarChipLabel(%s, %d) = %q, want %q", c.status.Key, c.elapsed, got, c.want)
			}
		})
	}
}

// Every state that is a phase of a session needs a chip word: one that fell
// through would be a busy bar saying nothing again, and the gap would only
// show up on the state nobody happened to sit in during review.
func TestEveryBusyStateHasAChipWord(t *testing.T) {
	for _, key := range busyBarStateKeys {
		s := barStates[key]
		if s.Short == "" {
			t.Errorf("%s is a phase of a session but the bar has no word for it", key)
		}
		if len([]rune(s.Short)) > 12 {
			t.Errorf("%s: chip word %q is too long for a bar", key, s.Short)
		}
	}
	// And the resting states carry none, so nothing is added to the bar in
	// normal use.
	for _, key := range []string{"idle", BarKeyNotRunning, BarKeyWakeArmed, BarKeyWakeMuted} {
		if got := barStates[key].Short; got != "" {
			t.Errorf("%s is a resting state but adds %q to the bar permanently", key, got)
		}
	}
}

// The generated library has to decide the same way the Go does. Only running
// it proves that; node is not a build dependency, so the check skips where it
// is absent, exactly like the statusFor mirror above it.
func TestBarStateJSMirrorsPendingWording(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; skipping the JavaScript mirror check")
	}

	type pendingCase struct {
		State      string `json:"state"`
		Tool       string `json:"tool"`
		ToolDetail string `json:"toolDetail"`
		Elapsed    int    `json:"elapsed"`
	}
	var cases []pendingCase
	for _, key := range BarStateKeys() {
		for _, elapsed := range []int{0, 1, 2, 12, 61} {
			cases = append(cases,
				pendingCase{State: key, Elapsed: elapsed},
				pendingCase{State: key, Tool: "shell.run", Elapsed: elapsed},
				pendingCase{State: key, Tool: "advisor.ask", ToolDetail: "Consulting claude…", Elapsed: elapsed},
				pendingCase{State: key, Tool: "future.tool", Elapsed: elapsed})
		}
	}
	cases = append(cases,
		pendingCase{State: "", Elapsed: 5},
		pendingCase{State: "dreaming", Elapsed: 5},
		pendingCase{State: "thinking", ToolDetail: "   ", Elapsed: 3},
		pendingCase{State: "thinking", Tool: "  shell.run  ", Elapsed: 3},
		pendingCase{State: "thinking", Elapsed: -4})
	encoded, err := json.Marshal(cases)
	if err != nil {
		t.Fatal(err)
	}

	type failCase struct {
		Stage   string `json:"stage"`
		Message string `json:"message"`
	}
	failures := []failCase{
		{"assistant", "the provider timed out"},
		{"", "the socket closed"},
		{"tts", "  "},
		{"", ""},
	}
	encodedFailures, err := json.Marshal(failures)
	if err != nil {
		t.Fatal(err)
	}

	library, err := os.ReadFile(barStateJSPath(t))
	if err != nil {
		t.Fatal(err)
	}
	script := strings.Replace(string(library), ".pragma library", "", 1) + `
var cases = ` + string(encoded) + `
var failures = ` + string(encodedFailures) + `
console.log(JSON.stringify({
  lines: cases.map(function (c) {
    return pendingTurnLine(c.state, c.tool, c.toolDetail, c.elapsed)
  }),
  chips: cases.map(function (c) {
    return chipLabel(statusFor(true, c.state, "", "off"), c.elapsed)
  }),
  failures: failures.map(function (f) { return pendingTurnFailed(f.stage, f.message) }),
  cancelled: pendingTurnCancelled,
  nothingHeard: pendingTurnNothingHeard,
  threshold: pendingElapsedThresholdSec
}))
`
	file := filepath.Join(t.TempDir(), "pending.js")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, file).Output()
	if err != nil {
		t.Fatalf("running the generated library under node failed: %v", err)
	}
	var answers struct {
		Lines        []string `json:"lines"`
		Chips        []string `json:"chips"`
		Failures     []string `json:"failures"`
		Cancelled    string   `json:"cancelled"`
		NothingHeard string   `json:"nothingHeard"`
		Threshold    int      `json:"threshold"`
	}
	if err := json.Unmarshal(out, &answers); err != nil {
		t.Fatalf("decoding the library's answers: %v", err)
	}
	if len(answers.Lines) != len(cases) || len(answers.Chips) != len(cases) {
		t.Fatalf("library answered %d lines and %d chips for %d cases",
			len(answers.Lines), len(answers.Chips), len(cases))
	}
	for i, c := range cases {
		want := PendingTurnLine(c.State, c.Tool, c.ToolDetail, c.Elapsed)
		if answers.Lines[i] != want {
			t.Errorf("pendingTurnLine(%q, %q, %q, %d) = %q in JS, %q in Go",
				c.State, c.Tool, c.ToolDetail, c.Elapsed, answers.Lines[i], want)
		}
		wantChip := BarChipLabel(BarStatusFor(true, c.State, "", WakeOff), c.Elapsed)
		if answers.Chips[i] != wantChip {
			t.Errorf("chipLabel(%q, %d) = %q in JS, %q in Go",
				c.State, c.Elapsed, answers.Chips[i], wantChip)
		}
	}
	for i, f := range failures {
		if want := PendingTurnFailed(f.Stage, f.Message); answers.Failures[i] != want {
			t.Errorf("pendingTurnFailed(%q, %q) = %q in JS, %q in Go",
				f.Stage, f.Message, answers.Failures[i], want)
		}
	}
	if answers.Cancelled != PendingTurnCancelled {
		t.Errorf("cancelled wording = %q in JS, %q in Go", answers.Cancelled, PendingTurnCancelled)
	}
	if answers.NothingHeard != PendingTurnNothingHeard {
		t.Errorf("nothing-heard wording = %q in JS, %q in Go",
			answers.NothingHeard, PendingTurnNothingHeard)
	}
	if answers.Threshold != PendingElapsedThresholdSec {
		t.Errorf("threshold = %d in JS, %d in Go", answers.Threshold, PendingElapsedThresholdSec)
	}
}

// The window must reach the pending turn's wording through the generated
// library rather than growing a switch of its own. A second copy in QML is
// exactly what ADR 0013 forbids, and it would be untested — the drift would
// only show up as the transcript and the bar disagreeing on screen.
//
// Kept after the QML suite landed (#174). tst_pendingturn.qml executes the
// row's whole life and deliberately asserts only that it says *something* —
// because who owns the words is this guard's question, not that one's. A
// local switch that happened to produce the same sentences would pass every
// behavioural test and drift the first time Go changed one.
func TestConversationWindowRendersThePendingTurnFromTheGeneratedLibrary(t *testing.T) {
	source, err := os.ReadFile(pluginFilePath(t, "JarvixWindow.qml"))
	if err != nil {
		t.Fatal(err)
	}
	qml := string(source)
	for _, want := range []string{
		`import "BarState.js" as BarState`,
		"BarState.pendingTurnLine(",
		"BarState.pendingTurnFailed(",
		"BarState.pendingTurnCancelled",
		// A capture that produced nothing resolves the pending row too
		// (issue #191): a wait that simply stops updating reads as a hang,
		// and a row that vanishes reads as the question having been lost.
		"BarState.pendingTurnNothingHeard",
	} {
		if !strings.Contains(qml, want) {
			t.Errorf("JarvixWindow.qml does not use %s; the pending turn's wording must come from Go", want)
		}
	}
	// The counter must be derived from the daemon's phase start, not from
	// when this window happened to start watching (issue #158): a window
	// opened five seconds into a think has to agree with one that was open.
	if !strings.Contains(qml, "state_since_ms") || !strings.Contains(qml, "since_ms") {
		t.Error("JarvixWindow.qml does not read the daemon's phase start; the elapsed count would be a client guess")
	}
}

// The overlay says the same nothing in the same words (issue #191). It is the
// surface a push-to-talk user is actually looking at, and it is the one that
// must not reach for errorMessage — that property is what paints the urgent
// colour, holds the longer linger and prints "Jarvix hit a problem", none of
// which a muted microphone has earned.
func TestOverlayWordsANothingHeardWithoutTheErrorStyling(t *testing.T) {
	source, err := os.ReadFile(pluginFilePath(t, "JarvixOverlay.qml"))
	if err != nil {
		t.Fatal(err)
	}
	qml := string(source)
	for _, want := range []string{
		`import "BarState.js" as BarState`,
		`case "session.nothing_heard":`,
		"BarState.pendingTurnNothingHeard",
	} {
		if !strings.Contains(qml, want) {
			t.Errorf("JarvixOverlay.qml does not contain %s", want)
		}
	}
	// The handler must set the notice, never the error. Checked on the
	// statement itself rather than on the whole file, because errorMessage
	// legitimately appears elsewhere.
	handler := qml[strings.Index(qml, `case "session.nothing_heard":`):]
	if end := strings.Index(handler, "break"); end > 0 {
		handler = handler[:end]
	}
	if strings.Contains(handler, "errorMessage =") {
		t.Errorf("the nothing-heard handler assigns errorMessage:\n%s", handler)
	}
}

// The bar's chip is the same rule: words from Go, drawn by QML.
func TestBarWidgetRendersTheChipFromTheGeneratedLibrary(t *testing.T) {
	source, err := os.ReadFile(pluginFilePath(t, "JarvixBar.qml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), "BarState.chipLabel(") {
		t.Error("JarvixBar.qml does not use BarState.chipLabel; the chip's words must come from Go")
	}
}
