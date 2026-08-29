package voicecorpus

import (
	"fmt"
	"time"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/intent"
	"github.com/rpickz/jarvix/internal/session"
	"github.com/rpickz/jarvix/internal/vocabulary"
)

// Rig is the downstream half of the pipeline, built from one configuration:
// everything a transcript meets after whisper hands it over.
//
// It is assembled from the live configuration rather than from defaults
// because that is the only version of the question worth asking. The user's
// own config renames the terminal, adds routines and scripts with their own
// trigger phrases, and can rename the assistant outright; a corpus graded
// against the shipped defaults would keep passing while the machine in front
// of them stopped working.
type Rig struct {
	// Router is the compiled intent table, exactly as the daemon compiles it
	// (config.IntentOptions).
	Router *intent.Router
	// WakeWord and WakeAliases are what the engine strips from the front of a
	// hands-free transcript (daemon/collaborators.go wires the same two).
	WakeWord    string
	WakeAliases []string
	// BiasPrompt resolves the prompt whisper decodes under, per call, over
	// the live vocabulary store. Held as a function rather than a string for
	// the reason config.STTBiasPromptFunc exists: a phrase taught between two
	// recordings must bias the second one.
	BiasPrompt func() string
}

// BuildRig assembles the rig from a configuration and its paths.
//
// The vocabulary store is opened read-only over the daemon's own file, the way
// doctor's bias-budget check opens it: the store is stat-fresh by design, so
// this reads exactly what the running daemon would read, without going through
// it and without a second copy of the file's meaning.
func BuildRig(cfg config.Config, paths config.Paths) (Rig, error) {
	router, err := intent.New(cfg.IntentOptions())
	if err != nil {
		return Rig{}, fmt.Errorf("compile the live intent table: %w", err)
	}
	var taught func() []string
	if cfg.Vocabulary.Enabled {
		store := vocabulary.NewStore(paths.VocabularyFile(), vocabulary.StoreOptions{
			MaxEntries:        cfg.Vocabulary.MaxEntries,
			MaxInjectedTokens: cfg.Vocabulary.MaxInjectedTokens,
		}, nil)
		taught = store.HardToHear
	}
	return Rig{
		Router:      router,
		WakeWord:    cfg.Assistant.Name,
		WakeAliases: cfg.Assistant.EffectiveAliases(),
		BiasPrompt:  cfg.STTBiasPromptFunc(taught),
	}, nil
}

// Result is one recording's verdict.
type Result struct {
	// Recording is the take this verdict is about.
	Recording Recording
	// Transcript is what whisper wrote, verbatim. Reported so a failure can be
	// read and understood; never asserted.
	Transcript string
	// Stripped is the transcript with the summons removed — what the router
	// and the model actually see.
	Stripped string
	// Reason is the adapter's explanation for a transcript that is
	// deliberately empty: no voiced audio, or nothing but the bias prompt
	// echoed back (issue #191). Non-empty is always a failure here, and
	// saying which of the two happened is the difference between "your
	// microphone is off" and "whisper heard nothing in a file that has sound
	// in it".
	Reason string
	// Score is Score(phrase.Say, Transcript); see score.go for what it is and
	// is not.
	Score float64
	// Failures is why this recording did not pass, one sentence each. Empty
	// means it passed.
	Failures []string
	// Elapsed is how long the transcription took, for the report.
	Elapsed time.Duration
}

// Pass reports whether every expectation held.
func (r Result) Pass() bool { return len(r.Failures) == 0 }

// Evaluate applies a phrase's expectations to what the pipeline produced.
//
// transcript and reason come straight from the stt.TranscriptEvent the real
// adapter emitted; everything after that is the real downstream code
// (session.StripWakeWord, session.IsAffirmative, the compiled intent.Router).
// Nothing here re-implements a decision that exists elsewhere, which is the
// point: a change to the alias list or the affirmative vocabulary has to be
// able to fail this.
func Evaluate(rec Recording, transcript, reason string, elapsed time.Duration, rig Rig) Result {
	r := Result{
		Recording:  rec,
		Transcript: transcript,
		Reason:     reason,
		Elapsed:    elapsed,
		Score:      Score(rec.Phrase.Say, transcript),
	}
	if reason != "" {
		// The pipeline declined to produce a transcript. That is the right
		// behaviour for silence and for a prompt echo, and it is never a pass
		// for a recording of somebody speaking — but which one it is decides
		// whether the fix is the recording or the code, so the reason travels.
		r.Failures = append(r.Failures, "the pipeline produced no transcript: "+reason)
		return r
	}
	if transcript == "" {
		r.Failures = append(r.Failures, "whisper returned an empty transcript with no reason given")
		return r
	}
	r.Stripped = session.StripWakeWord(transcript, rig.WakeWord, rig.WakeAliases)

	exp := rec.Phrase.Expect
	leads := session.WakeWordLeads(transcript, rig.WakeWord, rig.WakeAliases)
	switch exp.Wake {
	case WakeName:
		if !leads {
			r.Failures = append(r.Failures, fmt.Sprintf(
				"the transcript does not open with %q or any accepted alias %v — add the spelling whisper wrote to assistant.aliases",
				rig.WakeWord, rig.WakeAliases))
		}
	case WakeStrip:
		switch {
		case !leads:
			r.Failures = append(r.Failures, fmt.Sprintf(
				"the summons was not recognised, so it stays in the transcript and the router never sees a clean utterance (name %q, aliases %v)",
				rig.WakeWord, rig.WakeAliases))
		case r.Stripped == transcript:
			// Recognised but not removed: the only way that happens is a
			// transcript that is nothing BUT the name, which for a phrase
			// expecting a remainder means the rest of the sentence was lost.
			r.Failures = append(r.Failures,
				"the summons was recognised but nothing was left after it; the rest of the utterance did not survive")
		}
	}

	if exp.Intent != "" || exp.NoIntent {
		match, ok := rig.Router.Match(r.Stripped)
		switch {
		case exp.NoIntent && ok:
			r.Failures = append(r.Failures, fmt.Sprintf(
				"the router claimed this utterance as %q; it is meant to reach the assistant untouched", match.Name))
		case exp.Intent != "" && !ok:
			r.Failures = append(r.Failures, fmt.Sprintf(
				"the router matched nothing, so %q would have gone to the model instead of running locally", exp.Intent))
		case exp.Intent != "" && match.Name != exp.Intent:
			r.Failures = append(r.Failures, fmt.Sprintf(
				"the router matched %q, not %q", match.Name, exp.Intent))
		case exp.Intent != "" && exp.Slot != nil && (!match.HasSlot || match.Slot != *exp.Slot):
			r.Failures = append(r.Failures, fmt.Sprintf(
				"%s matched but its slot is %d (present: %v), not %d",
				match.Name, match.Slot, match.HasSlot, *exp.Slot))
		}
	}

	for _, w := range exp.Words {
		if !containsWord(transcript, w) {
			r.Failures = append(r.Failures, fmt.Sprintf("the word %q did not survive into the transcript", w))
		}
	}

	if exp.Affirmative != nil {
		if got := session.IsAffirmative(r.Stripped); got != *exp.Affirmative {
			r.Failures = append(r.Failures, fmt.Sprintf(
				"the confirmation gate read this as %v, not %v", verdict(got), verdict(*exp.Affirmative)))
		}
	}
	return r
}

// verdict names a confirmation decision the way the user would.
func verdict(approved bool) string {
	if approved {
		return "approval"
	}
	return "refusal"
}
