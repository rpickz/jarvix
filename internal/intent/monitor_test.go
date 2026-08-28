package intent

import (
	"strings"
	"testing"
)

// The monitor-name router tests (#180) mirror window_test.go exactly, because
// the grammars are twins and a difference between them would be a difference
// the user has to learn.

// TestMonitorNamePhrasesCarryTheSpokenName: each shipped phrase reaches the
// engine with the name and nothing else — no ack, because the seam composes
// what is said.
func TestMonitorNamePhrasesCarryTheSpokenName(t *testing.T) {
	r := newRouter(t)
	for utterance, want := range map[string]string{
		"call this monitor top":       "top",
		"call that monitor bottom":    "bottom",
		"call this screen top":        "top",
		"call that screen bottom":     "bottom",
		"name this monitor left":      "left",
		"name this screen right":      "right",
		"nickname this monitor upper": "upper",
	} {
		m, ok := r.Match(utterance)
		if !ok {
			t.Errorf("%q did not match", utterance)
			continue
		}
		if m.Name != MonitorNameIntentName {
			t.Errorf("%q matched %q", utterance, m.Name)
		}
		if m.MonitorName != want {
			t.Errorf("%q carried %q, want %q", utterance, m.MonitorName, want)
		}
		if m.Ack != "" {
			t.Errorf("%q carried an ack (%q); the seam owns what is said", utterance, m.Ack)
		}
	}
}

// TestMonitorForgetPhrasesCarryTheName: removal names the nickname, not the
// screen — the screen may be in a bag, which is often why it is being
// forgotten.
func TestMonitorForgetPhrasesCarryTheName(t *testing.T) {
	r := newRouter(t)
	for _, utterance := range []string{
		"forget the monitor called top",
		"forget the screen called top",
		"forget the monitor named top",
		"forget the screen named top",
		"stop calling a monitor top",
		"stop calling a screen top",
	} {
		m, ok := r.Match(utterance)
		if !ok {
			t.Errorf("%q did not match", utterance)
			continue
		}
		if m.Name != MonitorForgetIntentName || m.MonitorForget != "top" {
			t.Errorf("%q matched %q carrying %q", utterance, m.Name, m.MonitorForget)
		}
		if m.MonitorName != "" {
			t.Errorf("%q also carried an assignment name (%q)", utterance, m.MonitorName)
		}
	}
}

// TestMonitorNameSlotIsBounded: no name, an unbounded sentence, and the
// phrasings the table deliberately does not claim all fall through to the
// model rather than being swallowed.
func TestMonitorNameSlotIsBounded(t *testing.T) {
	r := newRouter(t)
	for _, utterance := range []string{
		"call this monitor",
		"call this monitor " + strings.Repeat("word ", maxNameWords+1),
		// No noun: indistinguishable from the window phrase, so neither
		// claims it and the model decides.
		"call this top",
		"call this a success",
		"what is my monitor called",
	} {
		if m, ok := r.Match(utterance); ok {
			t.Errorf("%q matched %q; it belongs to the model", utterance, m.Name)
		}
	}
}

// TestMonitorNamesListingPhrases: the listing is literal and whole-utterance.
func TestMonitorNamesListingPhrases(t *testing.T) {
	r := newRouter(t)
	for _, utterance := range monitorNamesPatterns {
		m, ok := r.Match(utterance)
		if !ok {
			t.Errorf("%q did not match", utterance)
			continue
		}
		if m.Name != MonitorNamesIntentName || !m.MonitorNames {
			t.Errorf("%q matched %q (MonitorNames=%v)", utterance, m.Name, m.MonitorNames)
		}
		if m.WindowNames {
			t.Errorf("%q also claimed the window listing", utterance)
		}
	}
}

// TestTheScreenAndWindowGrammarsDoNotOverlap is the guarantee the two nouns
// buy: no utterance ever means both, so which thing gets named can never
// depend on the order the tables were compiled in.
func TestTheScreenAndWindowGrammarsDoNotOverlap(t *testing.T) {
	r := newRouter(t)
	for _, utterance := range []string{"call this window builds", "what are my windows called"} {
		m, ok := r.Match(utterance)
		if !ok {
			t.Fatalf("%q did not match", utterance)
		}
		if m.MonitorName != "" || m.MonitorForget != "" || m.MonitorNames {
			t.Errorf("%q leaked into the screen grammar: %+v", utterance, m)
		}
	}
	for _, utterance := range []string{"call this monitor top", "what are my screens called"} {
		m, ok := r.Match(utterance)
		if !ok {
			t.Fatalf("%q did not match", utterance)
		}
		if m.WindowName != "" || m.WindowNames {
			t.Errorf("%q leaked into the window grammar: %+v", utterance, m)
		}
	}
}

// TestMonitorNamesPhrasesAreOwned: the listing phrases are owned like every
// built-in, so a routine or custom intent claiming one is refused at config
// load naming this owner — and Owner answers with the same wording.
func TestMonitorNamesPhrasesAreOwned(t *testing.T) {
	claim := monitorNamesPatterns[0]
	if _, err := New(Options{Custom: []Custom{{Match: claim, Run: "true"}}}); err == nil {
		t.Fatalf("a custom intent claimed %q", claim)
	} else if !strings.Contains(err.Error(), MonitorNamesIntentName) {
		t.Errorf("the refusal does not name the owner: %v", err)
	}
	if _, err := New(Options{Routines: []RoutinePhrases{{Name: "r", Phrases: []string{claim}}}}); err == nil {
		t.Fatalf("a routine claimed %q", claim)
	}
	owner, taken := newRouter(t).Owner(claim)
	if !taken || !strings.Contains(owner, MonitorNamesIntentName) {
		t.Errorf("Owner(%q) = %q, %v", claim, owner, taken)
	}
}
