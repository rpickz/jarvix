package intent

import (
	"strings"
	"testing"
)

// The focus grammar (#123): every action routes with its text, window count,
// and minutes parsed — and everything the table does not claim verbatim still
// falls through to the model, because ambiguity never belongs to this router.

func newTestRouter(t *testing.T, opts Options) *Router {
	t.Helper()
	r, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

func TestFocusPhrasesRoute(t *testing.T) {
	r := newTestRouter(t, Options{})
	cases := []struct {
		utterance string
		name      string
		action    FocusAction
		text      string
		windows   int
		slot      int
		hasSlot   bool
	}{
		{"new thread called the ci refactor", "focus.new", FocusNew, "the ci refactor", 0, 0, false},
		{"new thread deploy", "focus.new", FocusNew, "deploy", 0, 0, false},
		{"start a thread called reviews", "focus.new", FocusNew, "reviews", 0, 0, false},
		// The suffixed forms win over the bare slot: the anchoring words are
		// never eaten into the name.
		{"new thread called the ci refactor with this window", "focus.new", FocusNew, "the ci refactor", 1, 0, false},
		{"new thread deploy with these two windows", "focus.new", FocusNew, "deploy", 2, 0, false},
		{"anchor this window", "focus.anchor", FocusAnchor, "", 1, 0, false},
		{"anchor these two windows", "focus.anchor", FocusAnchor, "", 2, 0, false},
		{"switch to the deploy thread", "focus.switch", FocusSwitch, "deploy", 0, 0, false},
		{"go to the ci refactor thread", "focus.switch", FocusSwitch, "ci refactor", 0, 0, false},
		{"back to the deploy thread", "focus.switch", FocusSwitch, "deploy", 0, 0, false},
		{"later reply to dan", "focus.park", FocusPark, "reply to dan", 0, 0, false},
		{"park check the backup logs", "focus.park", FocusPark, "check the backup logs", 0, 0, false},
		{"what did i park", "focus.parked", FocusParked, "", 0, 0, false},
		{"where am i on everything", "focus.status", FocusStatus, "", 0, 0, false},
		{"where am i on the deploy thread", "focus.check", FocusCheck, "deploy", 0, 0, false},
		{"where am i on the ci refactor", "focus.check", FocusCheck, "the ci refactor", 0, 0, false},
		{"end the deploy thread", "focus.end", FocusEnd, "deploy", 0, 0, false},
		{"end this thread", "focus.end", FocusEnd, "", 0, 0, false},
		{"focus on the refactor for 25 minutes", "focus.timebox", FocusTimebox, "the refactor", 0, 25, true},
		{"focus on deploy for twenty five minutes", "focus.timebox", FocusTimebox, "deploy", 0, 25, true},
		{"end the focus session", "focus.timebox.end", FocusTimeboxEnd, "", 0, 0, false},
		{"stop focusing", "focus.timebox.end", FocusTimeboxEnd, "", 0, 0, false},
		{"focus session update", "focus.tick", FocusTick, "", 0, 0, false},
		{"take a break", "focus.break", FocusBreak, "", 0, 0, false},
		{"keep focusing", "focus.continue", FocusContinue, "", 0, 0, false},
		{"check in every 45 minutes", "focus.remind", FocusRemind, "", 0, 45, true},
		{"remind me where i am every forty five minutes", "focus.remind", FocusRemind, "", 0, 45, true},
		{"stop checking in", "focus.remind.stop", FocusRemindStop, "", 0, 0, false},
	}
	for _, tc := range cases {
		m, ok := r.Match(tc.utterance)
		if !ok {
			t.Errorf("%q did not route", tc.utterance)
			continue
		}
		if m.Name != tc.name || m.Focus != tc.action || m.FocusText != tc.text ||
			m.FocusWindows != tc.windows || m.Slot != tc.slot || m.HasSlot != tc.hasSlot {
			t.Errorf("%q = %+v; want name %q action %q text %q windows %d slot %d/%v",
				tc.utterance, m, tc.name, tc.action, tc.text, tc.windows, tc.slot, tc.hasSlot)
		}
		if m.Command != "" || len(m.Argv) > 0 || m.Desktop != DesktopNone {
			// The security posture: nothing spoken to the focus family can
			// ever reach a command line or the compositor from here.
			t.Errorf("%q carries an executable payload: %+v", tc.utterance, m)
		}
	}
}

// TestFocusTextIsAnchoredBySuffixWords pins the backtracking that makes a
// mid-utterance text slot deterministic: the fixed words after the slot claim
// their fields even when the name could have swallowed them.
func TestFocusTextIsAnchoredBySuffixWords(t *testing.T) {
	r := newTestRouter(t, Options{})
	m, ok := r.Match("focus on ci for docs for 25 minutes")
	if !ok || m.FocusText != "ci for docs" || m.Slot != 25 {
		t.Errorf("Match = %+v, %v; want text %q, minutes 25", m, ok, "ci for docs")
	}
}

// TestFocusMissesFallThrough: what the table does not claim verbatim goes to
// the model — a bare "later", an over-long thought, an out-of-range timebox.
func TestFocusMissesFallThrough(t *testing.T) {
	r := newTestRouter(t, Options{})
	for _, utterance := range []string{
		"later",                                // a thought needs content
		"new thread",                           // a thread needs a name
		"switch to the deploy",                 // no "thread" anchor: could mean a window
		"focus on x for 500 minutes",           // out of bounds is a miss, never a clamp
		"later " + strings.Repeat("word ", 13), // past the bound it is a sentence for the model
		"where am i",                           // no everything, no name: a model question
	} {
		if m, ok := r.Match(utterance); ok {
			t.Errorf("%q routed to %q; it belongs to the model", utterance, m.Name)
		}
	}
}

// TestFocusFixedPhrasesAreCollisionChecked: the fixed half of the family
// lives in the closed world every other phrase does — a custom intent or a
// routine claiming one is a config error naming both owners.
func TestFocusFixedPhrasesAreCollisionChecked(t *testing.T) {
	if _, err := New(Options{Custom: []Custom{{Match: "take a break", Run: "true"}}}); err == nil ||
		!strings.Contains(err.Error(), "focus.break") {
		t.Errorf("custom intent stole a focus phrase; err = %v", err)
	}
	if _, err := New(Options{Routines: []RoutinePhrases{
		{Name: "break time", Phrases: []string{"take a break"}},
	}}); err == nil || !strings.Contains(err.Error(), "focus.break") {
		t.Errorf("routine stole a focus phrase; err = %v", err)
	}
}

// TestLiteralRoutinePhraseBeatsFocusText: a routine whose literal phrase an
// utterance matches wins over the free-text slot, because literals always
// compile ahead of the {text} patterns.
func TestLiteralRoutinePhraseBeatsFocusText(t *testing.T) {
	r := newTestRouter(t, Options{Routines: []RoutinePhrases{
		{Name: "deploy context", Phrases: []string{"switch to the deploy thread"}},
	}})
	m, ok := r.Match("switch to the deploy thread")
	if !ok || m.Routine != "deploy context" || m.Focus != FocusNone {
		t.Errorf("Match = %+v, %v; want the routine", m, ok)
	}
	// A different name still reaches the focus family.
	m, ok = r.Match("switch to the reviews thread")
	if !ok || m.Focus != FocusSwitch || m.FocusText != "reviews" {
		t.Errorf("Match = %+v, %v; want focus.switch on reviews", m, ok)
	}
}
