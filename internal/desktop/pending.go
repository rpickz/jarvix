package desktop

import (
	"sort"
	"strings"

	"github.com/rpickz/jarvix/internal/ai"
)

// This file words *waiting* (issue #158).
//
// Jarvix used to say nothing about a wait in the one place the user was
// actually looking. A question was submitted, the message list showed the
// user's turn and then several seconds of blank space, and the assistant's
// bubble only appeared when the first token arrived — measured at ~6s on the
// current model stack. The header's "— Thinking" was too far from the list to
// be noticed and the bar swapped a monochrome glyph. Nothing said what was
// happening, and nothing said for how long.
//
// The fix is a *pending assistant turn*: a bubble that appears the moment a
// question is submitted, says what Jarvix is doing right now, counts once the
// wait is long enough to be worth quantifying, and then becomes the answer in
// place. What it says is decided here, in Go, where it is tested — QML renders
// the string and decides nothing (ADR 0013), reaching it through the generated
// BarState.js the same way the bar reaches its own vocabulary.
//
// The words are not new. A tool step is worded from the tool's own progress
// label where it has one ("Consulting claude…", already on the wire as
// tool.started's `detail`) and otherwise from the action-class table below —
// which is the same table the permission gate asks its short question with, so
// "May I run a shell command?" and "Running a shell command" can never drift
// into describing the same capability two ways. A state with no tool in flight
// is worded from barStates, the table the bar and the overlay already share.

// PendingElapsedThresholdSec is how long a wait must run before the pending
// turn starts saying how long it has been. Under it the answer usually arrives
// first, and a counter that appears and disappears inside two seconds is churn
// rather than information — it would make a *fast* answer feel busier than a
// slow one, which is exactly backwards.
const PendingElapsedThresholdSec = 2

// PendingTurnCancelled is how a pending turn resolves when the user cancelled.
// A wait must never simply stop updating: an indicator frozen mid-count reads
// as a hang, which is the failure this whole feature exists to remove.
const PendingTurnCancelled = "Cancelled"

// PendingTurnNothingHeard is how a pending turn resolves when the capture
// produced no words at all (issue #191) — the microphone was muted, the room
// was silent, or the only thing whisper offered was Jarvix's own bias prompt
// handed back.
//
// It is deliberately the ordinary sentence a person says when they did not
// hear you, and deliberately *not* PendingTurnFailed's wording: nothing broke.
// The reason — "the capture had no voiced audio (peak -inf dBFS…)" — is on the
// wire and lands in the activity feed, which is where a user debugging a
// microphone is looking. The conversation is not that place; a turn there is
// worth one honest line.
const PendingTurnNothingHeard = "I didn't catch that"

// toolPhrase is one tool's action in the two grammatical forms Jarvix needs:
// the infinitive clause the permission gate asks with ("May I run a shell
// command?"), and the present participle the pending turn shows while it is
// happening ("Running a shell command"). Both live on one row on purpose —
// they are one fact about a tool, and splitting them across two files is how
// the gate and the conversation would end up naming the same capability
// differently.
type toolPhrase struct {
	ask   string
	doing string
}

// toolPhrases is keyed by string literal rather than by the internal/tools
// constants because internal/tools imports this package — naming them here
// would be an import cycle. pending_tools_test.go is an external test that
// pins every literal to its constant, the same guard SummariseToolArgs's table
// carries, so a tool rename cannot silently demote a tool to its own name.
var toolPhrases = map[string]toolPhrase{
	"shell.run":            {ask: "run a shell command", doing: "Running a shell command"},
	"intent.run":           {ask: "run your custom command", doing: "Running your custom command"},
	"script.run":           {ask: "run one of your scripts", doing: "Running one of your scripts"},
	"routine.run":          {ask: "run one of your routines", doing: "Running one of your routines"},
	"advisor.ask":          {ask: "consult another assistant", doing: "Consulting another assistant"},
	"thinking.ask_deep":    {ask: "ask the stronger model", doing: "Thinking deeply"},
	"knowledge.refresh":    {ask: "refresh one of your feeds", doing: "Refreshing one of your feeds"},
	"typing.type_text":     {ask: "type on your keyboard", doing: "Typing on your keyboard"},
	"typing.press_key":     {ask: "type on your keyboard", doing: "Typing on your keyboard"},
	"memory.forget":        {ask: "forget one of your saved facts", doing: "Forgetting one of your saved facts"},
	"config.write_setting": {ask: "change one of your settings", doing: "Changing one of your settings"},
	"config.write_entry":   {ask: "save a configuration entry", doing: "Saving a configuration entry"},
	"config.delete_entry":  {ask: "delete a configuration entry", doing: "Deleting a configuration entry"},
}

// toolPhraseNames lists the table's tools, sorted — the generator's iteration
// order, so the emitted JavaScript is stable across Go's map ordering.
func toolPhraseNames() []string {
	names := make([]string, 0, len(toolPhrases))
	for name := range toolPhrases {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ToolActionAsk is the infinitive clause naming what a tool does, for the
// permission gate's short spoken question. A tool the table does not know
// names itself: a future tool must never be announced as something friendlier
// than its own name, because a question the user cannot map back to a
// capability is not a question they can answer (ADR 0014).
func ToolActionAsk(tool string) string {
	if p, ok := toolPhrases[tool]; ok {
		return p.ask
	}
	return "use the " + tool + " tool"
}

// ToolActionDoing is the same fact in the present tense, for a surface
// reporting a call that is already running. The unknown-tool fallback names
// the tool for the same reason ToolActionAsk's does.
func ToolActionDoing(tool string) string {
	if tool == "" {
		return ""
	}
	if p, ok := toolPhrases[tool]; ok {
		return p.doing
	}
	return "Running " + tool
}

// PendingTurnLabel words what Jarvix is doing right now for the conversation
// window's pending assistant turn, from exactly the three facts the window
// already holds off the event stream: the session state (state.changed), and
// the tool in flight with its progress label (tool.started / tool.finished).
//
// It returns "" for every state that is not a phase of a session — idle,
// error, and the wake rows. That empty string is load-bearing: it is how the
// window knows the pending turn must stop existing rather than sit there
// counting up forever after a turn ended.
//
// Precedence, and why:
//
//  1. Awaiting confirmation wins outright. The confirmation card is the
//     affordance and carries the verbatim command; the pending turn's only job
//     there is to say that a question is open, so it says exactly that and
//     nothing that could compete with the card.
//  2. Speaking with nothing running is not a wait, and says nothing. The
//     answer is already on the screen to be read; Jarvix reading it aloud
//     afterwards is not something the user is waiting through, and a row
//     saying "Speaking" underneath every completed answer would add a bubble
//     to every single turn. It stays a wait while a tool runs under the
//     narration, because that is a real one.
//  3. A tool's own progress label wins next. "Thinking" is true during a
//     two-minute advisor call but useless, and the label ("Consulting claude…")
//     is already on the wire and already the words the bar's tooltip and the
//     activity feed use for the same call. The trailing ellipsis is dropped:
//     it belongs to a spoken phrase, not to a line that may be followed by an
//     elapsed count.
//  4. Otherwise the tool's action class, so a tool with no progress label —
//     shell.run is the common one — still says what kind of thing is running
//     rather than falling back to a generic "Thinking".
//  5. Otherwise the state's own label from barStates, the table the bar and
//     the overlay already read.
func PendingTurnLabel(state, tool, toolDetail string) string {
	s, known := barStates[state]
	if !known || !busyBarState(s.Key) {
		return ""
	}
	if s.Key == "awaiting_confirmation" {
		return s.Label
	}
	tool, toolDetail = strings.TrimSpace(tool), strings.TrimSpace(toolDetail)
	if s.Key == "speaking" && tool == "" && toolDetail == "" {
		return ""
	}
	if detail := strings.TrimSuffix(toolDetail, "…"); strings.TrimSpace(detail) != "" {
		return strings.TrimSpace(detail)
	}
	if phrase := ToolActionDoing(tool); phrase != "" {
		return phrase
	}
	return s.Label
}

// PendingTurnLine is the whole pending turn's text: what is happening, and —
// once the wait has run past the threshold — how long it has been happening.
//
// The elapsed figure is a count of seconds the *daemon's* phase start decided
// (state.changed carries since_ms and conversation.get carries state_since_ms),
// not a client's guess at when it started asking. That is what lets a window
// opened five seconds into a long think say "5s" rather than starting its own
// clock from zero and telling the user a comfortable lie.
func PendingTurnLine(state, tool, toolDetail string, elapsedSec int) string {
	label := PendingTurnLabel(state, tool, toolDetail)
	if label == "" {
		return ""
	}
	if elapsedSec >= PendingElapsedThresholdSec {
		return label + " · " + formatActivityElapsed(elapsedSec)
	}
	return label
}

// PendingTurnFailed words a pending turn whose session failed, in the activity
// feed's own sentence for the same event so the transcript and the feed do not
// describe one failure two ways. A wait that ends badly has to *say* so in the
// place the user is looking; leaving the last "Thinking · 41s" on screen would
// claim Jarvix is still working on an answer that will never come.
func PendingTurnFailed(stage, message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		message = defaultErrorDetail
	}
	if stage = strings.TrimSpace(stage); stage == "" {
		return message
	}
	return activityErrorLabel(stage) + " — " + message
}

// BarChipLabel is the short status text the bar widget draws beside its glyph,
// and "" when it should draw the bare icon alone.
//
// The bar's problem was the mirror image of the window's: it said "busy" only
// by swapping one monochrome glyph for another in a dense bar, which is a
// colour-and-shape signal with no words at all unless you hover. So every
// phase of a session gets its word, and the elapsed count rides along on the
// states that already earn one in the tooltip.
//
// Nothing is added at rest. Idle, a stopped daemon, and both background
// listening rows return "" — they are states a user sits in for hours, and a
// permanent chip in the bar would be furniture within a day, which is the
// opposite of an indicator. The held error keeps its word because it is not a
// resting state: it stands until the next session and the user has to know.
func BarChipLabel(s BarState, elapsedSec int) string {
	if s.Short == "" {
		return ""
	}
	if busyBarState(s.Key) && elapsedSec > 0 {
		return s.Short + " " + formatActivityElapsed(elapsedSec)
	}
	return s.Short
}

// PendingTurnTierNote is the model tier's part of a pending turn's line
// (issue #159, on #158's surface): " · Deep" beside "Thinking · 6s", so the
// speed/quality trade the user just made is visible while they are paying for
// it rather than only afterwards in the record.
//
// It returns "" for an unknown or absent tier, which is every turn of a
// configuration with no tiers — the pending line is then exactly the line it
// has always been.
//
// The separator lives here rather than in QML for the reason the rest of this
// file does (ADR 0013): the window renders strings and decides nothing, and
// "decide whether a separator is needed" is a decision.
func PendingTurnTierNote(tier string) string {
	label := ai.TierLabel(ai.Tier(tier))
	if label == "" {
		return ""
	}
	return " · " + label
}
