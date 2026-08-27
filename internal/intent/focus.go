package intent

// This file is the router half of focus threads (#123): the fixed phrases
// that create, switch, park into, and report on named threads of work, plus
// the timeboxed focus session and its check-in vocabulary. Like every other
// family in this table the phrases are a short, literal list — a near-synonym
// is a code change with a test — and like the capture patterns (#62) the ones
// that carry a thread name or a parked thought end that free text against
// literal words, so the grammar stays deterministic: the router decides
// *whether* an utterance is a focus action, and the focus service owns what
// the action means.
//
// Nothing spoken ever becomes part of a command line here: a focus match
// carries an action name, at most one bounded free-text value (a thread name
// or a parked thought), and at most one bounds-checked integer (minutes).
// The engine hands the whole match to the focus runner, which acts on
// Jarvix's own thread store and nowhere else.

// FocusAction names what a matched focus phrase asks for. The engine performs
// none of these itself — it hands the match to the focus runner — so the
// constants are the entire contract between the grammar and the service.
type FocusAction string

// The focus actions the built-in table uses.
const (
	// FocusNone is an intent that is not a focus action.
	FocusNone FocusAction = ""
	// FocusNew creates a thread named by the free text, optionally anchored
	// to Match.FocusWindows windows.
	FocusNew FocusAction = "new"
	// FocusAnchor anchors Match.FocusWindows windows to the active thread.
	FocusAnchor FocusAction = "anchor"
	// FocusSwitch makes the named thread active and speaks its recap.
	FocusSwitch FocusAction = "switch"
	// FocusPark parks the free text as a thought on the active thread.
	FocusPark FocusAction = "park"
	// FocusParked reads the active thread's parked thoughts.
	FocusParked FocusAction = "parked"
	// FocusStatus speaks one line per thread, active thread first.
	FocusStatus FocusAction = "status"
	// FocusCheck speaks the named thread's recap without switching to it —
	// also the utterance a per-thread reminder replays through the session
	// path, so a check-in and the question it answers are the same sentence.
	FocusCheck FocusAction = "check"
	// FocusEnd ends the named thread ("this thread" ends the active one).
	FocusEnd FocusAction = "end"
	// FocusTimebox starts a timeboxed session on the named thread for
	// Match.Slot minutes.
	FocusTimebox FocusAction = "timebox"
	// FocusTimeboxEnd ends the live focus session early, by voice.
	FocusTimeboxEnd FocusAction = "timebox_end"
	// FocusTick reports the live focus session: remaining time mid-way, the
	// midpoint line at the midpoint firing, the continue-or-break close once
	// time is up. One action for all three so the scheduler's firings and
	// the user's own "focus session update" are indistinguishable.
	FocusTick FocusAction = "tick"
	// FocusBreak answers the close prompt: end the session and rest.
	FocusBreak FocusAction = "break"
	// FocusContinue answers the close prompt: another round, same thread,
	// same minutes.
	FocusContinue FocusAction = "continue"
	// FocusRemind sets the active thread's check-in interval to Match.Slot
	// minutes.
	FocusRemind FocusAction = "remind"
	// FocusRemindStop clears the active thread's check-in interval.
	FocusRemindStop FocusAction = "remind_stop"
)

// Minutes slot bounds. Out of bounds is a miss, never a clamp — "focus on it
// for five hundred minutes" belongs to the model, exactly like an
// out-of-range volume.
const (
	minMinutes = 1
	maxMinutes = 240
)

// MaxFocusNameWords is how many words a thread name may be and still ride
// the grammar — maxNameWords, exported so the daemon's reminder firing can
// refuse to replay a phrase the router could not claim (a hand-edited name
// past the bound must be skipped, never fall through to the model).
const MaxFocusNameWords = maxNameWords

// maxParkedWords bounds how many words a parked thought may be. Longer than a
// thread name's bound (maxNameWords) because a thought is a sentence fragment
// ("reply to Dan about the rota"), but still bounded: past a dozen words the
// utterance is far more likely a question for the model than a note, and an
// unbounded slot would claim it.
const maxParkedWords = 12

// focusBuiltin is one entry of the focus grammar: fixed phrases with no free
// text. The ack is deliberately absent from the table — every focus
// acknowledgement is composed by the focus service from the thread's own
// record (never invented, ADR 0041), so the grammar carries only the action.
type focusBuiltin struct {
	name     string
	action   FocusAction
	windows  int
	patterns []string
}

// focusFixedTable is the half of the focus grammar with no free text in it.
// These compile with the built-ins and enter the collision set, so a routine
// or custom intent claiming "take a break" is a config error naming both
// owners, never a coin toss.
func focusFixedTable() []focusBuiltin {
	return []focusBuiltin{
		{
			name: "focus.anchor", action: FocusAnchor, windows: 1,
			patterns: []string{"anchor this window"},
		},
		{
			name: "focus.anchor", action: FocusAnchor, windows: 2,
			patterns: []string{"anchor these two windows"},
		},
		{
			name: "focus.parked", action: FocusParked,
			patterns: []string{
				"what did i park", "what have i parked", "whats parked",
				"read my parked thoughts",
			},
		},
		{
			name: "focus.status", action: FocusStatus,
			patterns: []string{"where am i on everything", "where do i stand"},
		},
		{
			name: "focus.end", action: FocusEnd,
			patterns: []string{"end this thread", "close this thread"},
		},
		{
			name: "focus.timebox.end", action: FocusTimeboxEnd,
			patterns: []string{
				"end the focus session", "stop the focus session", "stop focusing",
			},
		},
		{
			name: "focus.tick", action: FocusTick,
			patterns: []string{"focus session update", "hows my focus session"},
		},
		{
			name: "focus.break", action: FocusBreak,
			patterns: []string{"take a break"},
		},
		{
			name: "focus.continue", action: FocusContinue,
			patterns: []string{"keep focusing"},
		},
		{
			name: "focus.remind", action: FocusRemind,
			patterns: []string{
				"check in every {minutes} minutes",
				"remind me where i am every {minutes} minutes",
			},
		},
		{
			name: "focus.remind.stop", action: FocusRemindStop,
			patterns: []string{"stop checking in"},
		},
	}
}

// focusTextEntry is one entry of the focus grammar that carries free text: a
// thread name or a parked thought, written as {text} in the pattern.
type focusTextEntry struct {
	name    string
	action  FocusAction
	windows int
	// textCap bounds how many words the {text} slot may swallow.
	textCap  int
	patterns []string
}

// focusTextTable is the free-text half of the focus grammar. Like the capture
// patterns these compile last, so every literal phrase — built-in, custom,
// routine, or script trigger — that happens to share a prefix wins over the
// text slot: only utterances no literal phrase claims reach it.
//
// Entry order is load-bearing, twice over. Rules are tried in insertion
// order, and a {text} slot with no suffix would happily swallow a sibling
// pattern's anchoring words ("new thread called ci with this window" must be
// the one-window form with the name "ci", never the bare form with the name
// "ci with this window") — so within the family, the most-suffixed pattern
// always compiles first, and the bare form is the last resort. The suffixes
// themselves ("… with this window", "the {text} thread") are what keep a
// mid-utterance text slot deterministic in the first place: the matcher tries
// the shortest text and backtracks, so the fixed words around the slot anchor
// exactly one reading.
func focusTextTable() []focusTextEntry {
	return []focusTextEntry{
		{
			name: "focus.new", action: FocusNew, windows: 2, textCap: maxNameWords,
			patterns: []string{
				"new thread called {text} with these two windows",
				"new thread {text} with these two windows",
			},
		},
		{
			name: "focus.new", action: FocusNew, windows: 1, textCap: maxNameWords,
			patterns: []string{
				"new thread called {text} with this window",
				"new thread {text} with this window",
			},
		},
		{
			name: "focus.new", action: FocusNew, textCap: maxNameWords,
			patterns: []string{
				"new thread called {text}",
				"new thread {text}",
				"start a thread called {text}",
			},
		},
		{
			name: "focus.switch", action: FocusSwitch, textCap: maxNameWords,
			patterns: []string{
				"switch to the {text} thread",
				"go to the {text} thread",
				"back to the {text} thread",
			},
		},
		{
			name: "focus.park", action: FocusPark, textCap: maxParkedWords,
			patterns: []string{"later {text}", "park {text}"},
		},
		{
			name: "focus.check", action: FocusCheck, textCap: maxNameWords,
			patterns: []string{
				"where am i on the {text} thread",
				"where am i on {text}",
			},
		},
		{
			name: "focus.end", action: FocusEnd, textCap: maxNameWords,
			patterns: []string{
				"end the {text} thread",
				"close the {text} thread",
			},
		},
		{
			name: "focus.timebox", action: FocusTimebox, textCap: maxNameWords,
			patterns: []string{"focus on {text} for {minutes} minutes"},
		},
	}
}
