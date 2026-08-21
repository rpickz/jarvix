package setup

import (
	"fmt"
	"io"
	"strings"
)

// Step is one wizard stage. Detection and action are separate so a re-run
// shows finished steps as done instead of redoing them.
type Step struct {
	// Title names the step in the progress header.
	Title string
	// Done reports whether the step is already satisfied, with a short
	// detail for the "done" line. nil means the step always runs.
	Done func() (bool, string)
	// Run performs the step. A returned error must name the fix; it is
	// printed and the wizard continues with the remaining steps.
	Run func() error
}

// Wizard runs steps in order, saving configuration after each one so an
// interrupted run keeps everything completed so far.
type Wizard struct {
	Out    io.Writer
	Prompt Prompter
	Steps  []Step
	// Save persists pending config changes; called after every step. nil
	// disables saving (tests that only exercise flow).
	Save func() error
}

// Run executes the wizard. It never aborts on a step failure — every step
// gets its chance — and returns an error naming the steps that still need
// attention, or nil when everything is configured.
func (w *Wizard) Run() error {
	var failed []string
	for i, s := range w.Steps {
		fprintf(w.Out, "\n── Step %d of %d: %s\n", i+1, len(w.Steps), s.Title)
		if s.Done != nil {
			if done, detail := s.Done(); done {
				if detail != "" {
					fprintf(w.Out, "already set up: %s\n", detail)
				} else {
					fprintln(w.Out, "already set up")
				}
				if !w.Prompt.Confirm("Revisit this step anyway?", false) {
					continue
				}
			}
		}
		if err := s.Run(); err != nil {
			failed = append(failed, s.Title)
			fprintf(w.Out, "✗ %s: %v\n  (continuing with the remaining steps)\n", s.Title, err)
		}
		if w.Save != nil {
			if err := w.Save(); err != nil {
				return fmt.Errorf("save configuration: %w", err)
			}
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("these steps still need attention: %s — the fixes are printed above, and `jarvix doctor` re-checks everything", strings.Join(failed, ", "))
	}
	return nil
}

// setValue writes table.key = value through the config file editor, asking
// before overwriting a different existing value: a hand-edited config is
// never clobbered silently.
func setValue(f *File, p Prompter, out io.Writer, table, key, value string) {
	if cur, ok := f.Get(table, key); ok && cur != value {
		q := fmt.Sprintf("config.toml already sets %s.%s = %q — change it to %q?", table, key, cur, value)
		if !p.Confirm(q, false) {
			fprintf(out, "keeping %s.%s = %q\n", table, key, cur)
			return
		}
	}
	f.Set(table, key, value)
}
