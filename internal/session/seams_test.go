package session

import "testing"

// The exported seams are thin, so these tests are thin too. What they pin is
// the one property that is not obvious from the wrappers: WakeWordLeads and
// StripWakeWord answer DIFFERENT questions, and the corpus harness needs both
// (issue #143). A refactor that made WakeWordLeads "did StripWakeWord change
// anything" would compile, pass every other test in this package, and quietly
// stop the corpus from being able to test the bare-name recording at all.

func TestWakeWordLeadsSeesANameStripWakeWordLeavesAlone(t *testing.T) {
	const name = "Jarvix"
	if got := StripWakeWord("Jarvix", name, nil); got != "Jarvix" {
		t.Errorf("StripWakeWord(%q) = %q; a name-only utterance is left whole", "Jarvix", got)
	}
	if !WakeWordLeads("Jarvix.", name, nil) {
		t.Error("WakeWordLeads cannot see the name in a transcript that is only the name")
	}
	if WakeWordLeads("what time is it", name, nil) {
		t.Error("WakeWordLeads found the name in an utterance that does not contain it")
	}
}

func TestWakeWordLeadsAcceptsTheConfiguredAliases(t *testing.T) {
	if !WakeWordLeads("Jarvis, open the window", "Jarvix", []string{"Jarvis"}) {
		t.Error("an accepted mishearing was not recognised as the name")
	}
	if WakeWordLeads("Jarvis, open the window", "Jarvix", nil) {
		t.Error("a mishearing was accepted with no alias configured for it")
	}
	if !WakeWordLeads("hey Jarvix, open the window", "Jarvix", nil) {
		t.Error("a filler before the name hid it from the matcher")
	}
}

func TestStripWakeWordSeamMatchesTheEngines(t *testing.T) {
	if got := StripWakeWord("Jarvix, open the window", "Jarvix", nil); got != "open the window" {
		t.Errorf("StripWakeWord = %q", got)
	}
}

func TestIsAffirmativeSeamMatchesTheGates(t *testing.T) {
	if !IsAffirmative("yes, do it") {
		t.Error("IsAffirmative refused a clear approval")
	}
	if IsAffirmative("no, don't") {
		t.Error("IsAffirmative approved a refusal")
	}
}
