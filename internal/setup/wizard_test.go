package setup

import (
	"errors"
	"strings"
	"testing"
)

// fakePrompter answers prompts from queues; exhausted queues return the
// default, matching the terminal prompter's EOF behaviour.
type fakePrompter struct {
	confirms []bool
	choices  []int
	inputs   []string
	asked    []string
}

func (f *fakePrompter) Confirm(question string, def bool) bool {
	f.asked = append(f.asked, question)
	if len(f.confirms) == 0 {
		return def
	}
	v := f.confirms[0]
	f.confirms = f.confirms[1:]
	return v
}

func (f *fakePrompter) Choose(question string, options []string, def int) int {
	f.asked = append(f.asked, question)
	if len(f.choices) == 0 {
		return def
	}
	v := f.choices[0]
	f.choices = f.choices[1:]
	return v
}

func (f *fakePrompter) Input(question, def string) string {
	f.asked = append(f.asked, question)
	if len(f.inputs) == 0 {
		return def
	}
	v := f.inputs[0]
	f.inputs = f.inputs[1:]
	return v
}

func TestWizardSkipsDoneStepsAndContinuesPastFailures(t *testing.T) {
	var out strings.Builder
	var ran []string
	saves := 0
	w := &Wizard{
		Out:    &out,
		Prompt: &fakePrompter{},
		Save:   func() error { saves++; return nil },
		Steps: []Step{
			{Title: "done step", Done: func() (bool, string) { return true, "already configured" },
				Run: func() error { ran = append(ran, "done step"); return nil }},
			{Title: "failing step", Run: func() error {
				ran = append(ran, "failing step")
				return errors.New("install X to fix it")
			}},
			{Title: "last step", Run: func() error { ran = append(ran, "last step"); return nil }},
		},
	}
	err := w.Run()
	if err == nil || !strings.Contains(err.Error(), "failing step") {
		t.Fatalf("want an error naming the failed step, got %v", err)
	}
	if len(ran) != 2 || ran[0] != "failing step" || ran[1] != "last step" {
		t.Fatalf("ran %v: the done step must be skipped, the rest must all run", ran)
	}
	if !strings.Contains(out.String(), "already set up: already configured") {
		t.Fatalf("done step not shown as done:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "install X to fix it") {
		t.Fatalf("failure fix not printed:\n%s", out.String())
	}
	if saves != 2 {
		t.Fatalf("config must be saved after every executed step, got %d saves", saves)
	}
}

func TestWizardCanRevisitADoneStep(t *testing.T) {
	var out strings.Builder
	ran := false
	w := &Wizard{
		Out:    &out,
		Prompt: &fakePrompter{confirms: []bool{true}}, // yes, revisit
		Steps: []Step{
			{Title: "done step", Done: func() (bool, string) { return true, "" },
				Run: func() error { ran = true; return nil }},
		},
	}
	if err := w.Run(); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("answering yes to the revisit prompt must re-run the step")
	}
}

func TestSetValueAsksBeforeClobberingAndKeepsOnDecline(t *testing.T) {
	f := loadString(t, "[ai]\nmodel = \"hand-picked\"\n")
	var out strings.Builder
	p := &fakePrompter{confirms: []bool{false}}
	setValue(f, p, &out, "ai", "model", "llama3.2:3b")
	if v, _ := f.Get("ai", "model"); v != "hand-picked" {
		t.Fatalf("declined overwrite must keep the hand-edited value, got %q", v)
	}
	if len(p.asked) != 1 || !strings.Contains(p.asked[0], "hand-picked") {
		t.Fatalf("must ask before clobbering, asked: %v", p.asked)
	}
}

func TestSetValueOverwritesOnConfirmAndWritesSilentlyWhenUnset(t *testing.T) {
	f := loadString(t, "[ai]\nmodel = \"hand-picked\"\n")
	var out strings.Builder
	p := &fakePrompter{confirms: []bool{true}}
	setValue(f, p, &out, "ai", "model", "llama3.2:3b")
	if v, _ := f.Get("ai", "model"); v != "llama3.2:3b" {
		t.Fatalf("confirmed overwrite must apply, got %q", v)
	}

	p2 := &fakePrompter{}
	setValue(f, p2, &out, "ai", "provider", "ollama")
	if len(p2.asked) != 0 {
		t.Fatalf("writing an unset key must not prompt, asked: %v", p2.asked)
	}
	if v, _ := f.Get("ai", "provider"); v != "ollama" {
		t.Fatalf("got %q", v)
	}
}
