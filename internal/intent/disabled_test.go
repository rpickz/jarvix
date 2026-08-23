package intent

import (
	"strings"
	"testing"
)

// The disabled switch on routine and script phrases (issue #93): a parked
// entry's phrases leave the grammar — the utterance falls through to the
// assistant — while the entry's own shape is still validated, and only the
// collision check relaxes. These are the mutation checks for the skip: if it
// were removed, the phrases would match again and the coexistence cases would
// fail compilation.

// TestDisabledRoutinePhrasesLeaveTheGrammar: the same phrase routes while the
// routine is enabled and falls through while it is disabled — the property
// itself, both directions, so neither half can rot alone.
func TestDisabledRoutinePhrasesLeaveTheGrammar(t *testing.T) {
	enabled, err := New(Options{Routines: []RoutinePhrases{
		{Name: "morning setup", Phrases: []string{"morning setup"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if m, ok := enabled.Match("morning setup"); !ok || m.Routine != "morning setup" {
		t.Fatalf("the enabled phrase did not route: %+v (matched %v)", m, ok)
	}

	disabled, err := New(Options{Routines: []RoutinePhrases{
		{Name: "morning setup", Phrases: []string{"morning setup"}, Disabled: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if m, ok := disabled.Match("morning setup"); ok {
		t.Errorf("a disabled routine's phrase still matched: %+v", m)
	}
	for _, name := range disabled.Names() {
		if name == "routine:morning setup" {
			t.Error("a disabled routine is still listed as routable")
		}
	}
}

// TestDisabledScriptPhrasesLeaveTheGrammar: the script half of the same
// property — for the family whose phrase reaches an executable, the skip is
// the one thing standing between "parked" and "runs anyway".
func TestDisabledScriptPhrasesLeaveTheGrammar(t *testing.T) {
	enabled, err := New(Options{Scripts: []ScriptPhrases{
		{Name: "backup notes", Phrases: []string{"backup my notes"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if m, ok := enabled.Match("backup my notes"); !ok || m.Script != "backup notes" {
		t.Fatalf("the enabled phrase did not route: %+v (matched %v)", m, ok)
	}

	disabled, err := New(Options{Scripts: []ScriptPhrases{
		{Name: "backup notes", Phrases: []string{"backup my notes"}, Disabled: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if m, ok := disabled.Match("backup my notes"); ok {
		t.Errorf("a disabled script's phrase still matched: %+v", m)
	}
}

// TestDisabledEntriesAreStillValidated: parking an entry does not park its
// checks — a rotten phrase or a missing name is a compile error even while
// disabled, so re-enabling can never surprise with a per-entry problem.
func TestDisabledEntriesAreStillValidated(t *testing.T) {
	tests := []struct {
		name string
		opts Options
		want string
	}{
		{"disabled routine with a placeholder", Options{Routines: []RoutinePhrases{
			{Name: "morning setup", Phrases: []string{"setup {workspace}"}, Disabled: true}}},
			"contains a placeholder"},
		{"disabled routine with no phrases", Options{Routines: []RoutinePhrases{
			{Name: "morning setup", Disabled: true}}},
			"it has no phrases"},
		{"disabled script with an empty name", Options{Scripts: []ScriptPhrases{
			{Phrases: []string{"go"}, Disabled: true}}},
			"name is empty"},
		{"disabled script with an empty phrase", Options{Scripts: []ScriptPhrases{
			{Name: "backup notes", Phrases: []string{"  "}, Disabled: true}}},
			"pattern is empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.opts)
			if err == nil {
				t.Fatal("compiled despite the problem")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
		})
	}
}

// TestDisabledEntriesLeaveTheCollisionSet: a disabled entry's phrase may be
// held by an enabled one — that is exactly the state a disable leaves behind
// — and re-enabling it (the same options with Disabled flipped) fails with
// the load error naming both owners. The coexistence and the refusal are one
// test on purpose: together they pin "the collision is caught at re-enable,
// never earlier and never silently".
func TestDisabledEntriesLeaveTheCollisionSet(t *testing.T) {
	shared := []string{"do the thing"}
	coexisting := Options{
		Routines: []RoutinePhrases{{Name: "old thing", Phrases: shared, Disabled: true}},
		Scripts:  []ScriptPhrases{{Name: "new thing", Phrases: shared}},
	}
	r, err := New(coexisting)
	if err != nil {
		t.Fatalf("a disabled routine's phrase blocked an enabled script: %v", err)
	}
	if m, ok := r.Match("do the thing"); !ok || m.Script != "new thing" {
		t.Fatalf("the enabled owner did not route: %+v (matched %v)", m, ok)
	}

	reenabled := coexisting
	reenabled.Routines = []RoutinePhrases{{Name: "old thing", Phrases: shared}}
	if _, err := New(reenabled); err == nil ||
		!strings.Contains(err.Error(), `the trigger for routine "old thing"`) {
		t.Errorf("re-enable error = %v, want the collision naming both owners", err)
	}
}
