package daemon

// This file is the IPC surface and the voice seam of the taught vocabulary
// (issue #129), shaped after the memory files it sits beside:
//
//   - vocabulary.list / vocabulary.last mirror memory.list / memory.last
//     (ADR 0025): the store straight from disk for the window's section, and
//     the audit answer to "what taught words was the model just given".
//   - vocabulary.teach / vocabulary.update are the window form's writes,
//     UNGATED on memory.add's argument: typing a phrase into the form is the
//     user's explicit teaching, a wrong teach is undone with one forget, and
//     a re-teach supersedes onto the trail — nothing is destroyed.
//   - vocabulary.forget_gated is the section's Delete button: deletion
//     destroys the entry's taught history, so it routes through the
//     permission gate exactly as the Memory tab's Forget does (the ADR 0025
//     reversibility split, recorded for vocabulary in ADR 0042).
//   - vocabularyVoice is the engine's VocabularyTeacher seam: the spoken
//     sentences for "when i say X i mean Y", "listen for the word X", and
//     "what words have i taught you" live here, once, so voice and any
//     future surface confirm teaching in the same words.
//
// Contents travel here and nowhere else: it is the user's own vocabulary,
// asked for over their own 0600 socket. Events and logs carry ids and sizes.

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/session"
	"github.com/rpickz/jarvix/internal/vocabulary"
)

func (d *Daemon) registerVocabularyMethods() {
	// Registered even with vocabulary disabled, answering enabled=false, so
	// a client can tell "switched off" from "old daemon" without version
	// sniffing (the memory methods' arrangement).
	d.server.Handle("vocabulary.list", func(params json.RawMessage) (any, error) {
		if d.vocabulary == nil {
			return map[string]any{"enabled": false}, nil
		}
		p := struct {
			Query string `json:"query"`
		}{}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "vocabulary.list params: %v", err)
			}
		}
		entries := d.vocabulary.List(p.Query)
		count, max := d.vocabulary.Count()
		biasCount, biasMax := d.vocabulary.BiasCount()
		result := map[string]any{
			"enabled":    true,
			"path":       d.vocabulary.Path(),
			"count":      count,
			"max":        max,
			"bias_count": biasCount,
			"bias_max":   biasMax,
			"entries":    vocabularyReports(entries),
		}
		// The over-budget disclosure: the store decides whether entries are
		// being left out of the prompt, and the window shows the sentence
		// wherever the entries show — a trim is never silent (ADR 0037).
		if warning := d.vocabulary.InjectionWarning(d.speakBackEnabled()); warning != "" {
			result["warning"] = warning
		}
		return result, nil
	})

	d.server.Handle("vocabulary.last", func(json.RawMessage) (any, error) {
		if d.vocabulary == nil {
			return map[string]any{"enabled": false, "injected": false}, nil
		}
		inj, sessionID, ok := d.engine.LastVocabulary()
		if !ok {
			return map[string]any{"enabled": true, "injected": false}, nil
		}
		return map[string]any{
			"enabled":    true,
			"injected":   true,
			"session_id": sessionID,
			"entries":    vocabularyReports(inj.Entries),
			"trimmed":    inj.Trimmed,
			"total":      inj.Total,
			"est_tokens": inj.EstTokens,
		}, nil
	})

	// vocabulary.teach: the form's Add — and deliberately the same verb a
	// chat teach lands on, because the store's Teach IS the supersede rule:
	// an existing phrase updates in place, trail kept, never a second entry.
	d.server.Handle("vocabulary.teach", func(params json.RawMessage) (any, error) {
		if d.vocabulary == nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"vocabulary is disabled (vocabulary.enabled = false)")
		}
		var p struct {
			Phrase     string `json:"phrase"`
			Meaning    string `json:"meaning"`
			Note       string `json:"note"`
			HardToHear bool   `json:"hard_to_hear"`
		}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "vocabulary.teach params: %v", err)
			}
		}
		entry, warning, err := d.vocabulary.Teach(p.Phrase, p.Meaning, p.Note, "")
		if err != nil {
			return nil, vocabularyWriteError(err)
		}
		if p.HardToHear != entry.HardToHear {
			// The entry exists whatever happens next, so a refused flag is a
			// refused *flag*, not a failed teach: the reply carries the taught
			// entry and the refusal names the bias cap (never silent).
			flagged, biasWarning, flagErr := d.vocabulary.SetHardToHear(entry.ID, p.HardToHear)
			if flagErr != nil {
				return nil, vocabularyFlagError(entry, flagErr)
			}
			entry = flagged
			if warning == "" {
				warning = biasWarning
			}
		}
		d.publishVocabularyEntryChanged("taught", entry)
		result := map[string]any{"entry": vocabularyReport(entry)}
		if warning != "" {
			result["warning"] = warning
		}
		return result, nil
	})

	// vocabulary.update: the form's Edit, by id. Compares before writing so
	// an untouched entry costs nothing and a flag-only save manufactures no
	// revision (the memory.update discipline).
	d.server.Handle("vocabulary.update", func(params json.RawMessage) (any, error) {
		if d.vocabulary == nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"vocabulary is disabled (vocabulary.enabled = false)")
		}
		var p struct {
			ID         string `json:"id"`
			Phrase     string `json:"phrase"`
			Meaning    string `json:"meaning"`
			Note       string `json:"note"`
			HardToHear *bool  `json:"hard_to_hear"`
		}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "vocabulary.update params: %v", err)
			}
		}
		if p.ID == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "vocabulary.update needs an id")
		}
		entry, err := d.vocabulary.Update(p.ID, p.Phrase, p.Meaning, p.Note, "")
		if err != nil {
			return nil, vocabularyWriteError(err)
		}
		var warning string
		if p.HardToHear != nil && *p.HardToHear != entry.HardToHear {
			flagged, biasWarning, flagErr := d.vocabulary.SetHardToHear(entry.ID, *p.HardToHear)
			if flagErr != nil {
				return nil, vocabularyFlagError(entry, flagErr)
			}
			entry = flagged
			warning = biasWarning
		}
		d.publishVocabularyEntryChanged("edited", entry)
		result := map[string]any{"entry": vocabularyReport(entry)}
		if warning != "" {
			result["warning"] = warning
		}
		return result, nil
	})

	// The section's per-entry Delete (#129): through the gated tool path,
	// like the Memory tab's Forget — the standard confirmation card appears
	// in Chat naming the exact phrase, resolved from the store, and only an
	// approval deletes.
	d.server.Handle("vocabulary.forget_gated", func(params json.RawMessage) (any, error) {
		if d.vocabulary == nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"vocabulary is disabled (vocabulary.enabled = false)")
		}
		p := struct {
			ID string `json:"id"`
		}{}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "vocabulary.forget_gated params: %v", err)
			}
		}
		if p.ID == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "vocabulary.forget_gated needs an id")
		}
		// Resolved here so an unknown id is a crisp error, not a session that
		// starts only to apologise — and so the record names the entry.
		var description string
		for _, e := range d.vocabulary.List("") {
			if e.ID == p.ID {
				description = fmt.Sprintf("%q means %s", e.Phrase, e.Meaning)
				break
			}
		}
		if description == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"no taught entry has id %q; vocabulary.list shows what is taught", p.ID)
		}
		id, err := d.engine.ForgetVocabularyEntry(p.ID, description)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeSessionError, "%v", err)
		}
		return map[string]string{"session_id": id}, nil
	})
}

// speakBackEnabled reads the live vocabulary.speak_back switch — idle-class,
// so it follows the running config rather than the booted one.
func (d *Daemon) speakBackEnabled() bool {
	d.cfgMu.Lock()
	defer d.cfgMu.Unlock()
	return d.cfg.Vocabulary.SpeakBack
}

// vocabularyReport renders one entry for the wire, trail included,
// timestamps in RFC 3339 like every other IPC surface.
func vocabularyReport(e vocabulary.Entry) map[string]any {
	report := map[string]any{
		"id":           e.ID,
		"phrase":       e.Phrase,
		"meaning":      e.Meaning,
		"taught":       e.Taught.Format(time.RFC3339),
		"updated":      e.Updated.Format(time.RFC3339),
		"hard_to_hear": e.HardToHear,
	}
	if e.Note != "" {
		report["note"] = e.Note
	}
	if e.Source != "" {
		report["source"] = e.Source
	}
	if len(e.Previous) > 0 {
		previous := make([]map[string]any, 0, len(e.Previous))
		for _, p := range e.Previous {
			rev := map[string]any{
				"phrase":     p.Phrase,
				"meaning":    p.Meaning,
				"taught":     p.Taught.Format(time.RFC3339),
				"superseded": p.Superseded.Format(time.RFC3339),
			}
			if p.Note != "" {
				rev["note"] = p.Note
			}
			previous = append(previous, rev)
		}
		report["previous"] = previous
	}
	return report
}

// vocabularyReports renders an entry list, never nil, so clients always see
// an array.
func vocabularyReports(entries []vocabulary.Entry) []map[string]any {
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, vocabularyReport(e))
	}
	return out
}

// vocabularyWriteError places one store refusal for the form: empty phrase
// or meaning under their fields, a full store or a phrase collision in the
// general area, an unknown id as the crisp parameter error. The sentences
// are the store's own, verbatim; only the placement is decided here (the
// memoryWriteError arrangement).
func vocabularyWriteError(err error) error {
	problem := func(field string) error {
		return &ipc.Error{
			Code:    ipc.CodeConfigInvalid,
			Message: "the entry was rejected; nothing was written",
			Data: map[string]any{"problems": []entryProblem{
				{Field: field, Message: err.Error()}}},
		}
	}
	switch {
	case errors.Is(err, vocabulary.ErrNoPhrase):
		return problem("phrase")
	case errors.Is(err, vocabulary.ErrNoMeaning):
		return problem("meaning")
	case errors.Is(err, vocabulary.ErrDuplicatePhrase):
		return problem("phrase")
	case errors.Is(err, vocabulary.ErrStoreFull):
		return problem("")
	case errors.Is(err, vocabulary.ErrUnknownID):
		return ipc.Errorf(ipc.CodeInvalidParams, "%v; vocabulary.list shows what is taught", err)
	}
	return ipc.Errorf(ipc.CodeInternalError, "%v", err)
}

// vocabularyFlagError places a refused hard-to-hear flag. The entry itself
// was written — the error must say so, or the form would imply the teach
// failed and invite a retry that supersedes nothing.
func vocabularyFlagError(entry vocabulary.Entry, err error) error {
	if errors.Is(err, vocabulary.ErrBiasFull) {
		return &ipc.Error{
			Code: ipc.CodeConfigInvalid,
			Message: fmt.Sprintf("the entry was saved as %s, but it will not be listened for",
				entry.ID),
			Data: map[string]any{"problems": []entryProblem{
				{Field: "hard_to_hear", Message: err.Error()}}},
		}
	}
	return ipc.Errorf(ipc.CodeInternalError,
		"the entry was saved as %s but the listen flag failed: %v", entry.ID, err)
}

// publishVocabularyEntryChanged announces one save on the bus: the activity
// feed renders it into a row naming the entry by id, and any open window's
// Vocabulary section re-requests its listing. Id and size only, never the
// words — the memory privacy contract, held for vocabulary verbatim.
func (d *Daemon) publishVocabularyEntryChanged(action string, e vocabulary.Entry) {
	d.bus.Publish(session.Event{Type: "vocabulary.entry_changed", Data: map[string]any{
		"action": action, "id": e.ID,
		"chars": len([]rune(e.Phrase)) + len([]rune(e.Meaning)),
	}})
}

// vocabularyVoice is the engine's VocabularyTeacher seam (issue #129): the
// spoken sentences for the deterministic teach phrases, over the same store
// every other surface writes. Wording lives here once, so a voice teach and
// any future surface confirm in the same words.
type vocabularyVoice struct {
	store *vocabulary.Store
	// publish announces store changes so open windows refresh — the same
	// event the form verbs publish, because a voice teach changes the same
	// listing.
	publish func(action string, e vocabulary.Entry)
}

// TeachEntry implements session.VocabularyTeacher. The confirmation is soft
// and names both halves — phrase and meaning — because a mistranscribed
// teach corrected now costs one sentence, and discovered later costs a
// confusing answer some day.
func (v *vocabularyVoice) TeachEntry(phrase, meaning, source string) (string, error) {
	entry, warning, err := v.store.Teach(phrase, meaning, "", source)
	if err != nil {
		return "", spokenVocabularyError(err)
	}
	v.publish("taught", entry)
	spoken := fmt.Sprintf("Okay — %s means %s.", entry.Phrase, entry.Meaning)
	if len(entry.Previous) > 0 {
		previous := entry.Previous[len(entry.Previous)-1]
		if !strings.EqualFold(previous.Meaning, entry.Meaning) {
			spoken = fmt.Sprintf("Okay — %s now means %s; it used to mean %s.",
				entry.Phrase, entry.Meaning, previous.Meaning)
		}
	}
	if warning != "" {
		spoken += " " + capitaliseSentence(warning) + "."
	}
	return spoken, nil
}

// ListenFor implements session.VocabularyTeacher: flags an already-taught
// phrase for the STT bias. An untaught phrase is refused with the way
// forward rather than stored meaningless — an entry is a phrase AND a
// meaning, and inventing one to hold a flag would put words in the store the
// user never taught. Bare recognition terms belong in stt.vocabulary.
func (v *vocabularyVoice) ListenFor(phrase string) (string, error) {
	entry, found := v.store.ByPhrase(phrase)
	if !found {
		return "", fmt.Errorf("I have not been taught %s yet — teach it first: "+
			"when I say %s, I mean, and then the meaning", strings.TrimSpace(phrase),
			strings.TrimSpace(phrase))
	}
	if entry.HardToHear {
		return fmt.Sprintf("I am already listening for %s.", entry.Phrase), nil
	}
	flagged, warning, err := v.store.SetHardToHear(entry.ID, true)
	if err != nil {
		return "", spokenVocabularyError(err)
	}
	v.publish("flagged", flagged)
	spoken := fmt.Sprintf("I will listen for %s.", flagged.Phrase)
	if warning != "" {
		spoken += " " + capitaliseSentence(warning) + "."
	}
	return spoken, nil
}

// spokenListingCap bounds how many entries the voice listing reads in full.
// Past a handful the ear stops following; the window's Vocabulary section
// always shows everything, and the sentence says so.
const spokenListingCap = 8

// SpokenListing implements session.VocabularyTeacher: the one short spoken
// list behind "what words have i taught you".
func (v *vocabularyVoice) SpokenListing() (string, error) {
	entries := v.store.List("")
	if len(entries) == 0 {
		return "You have not taught me any words yet. Say, when I say quid, I mean pounds.", nil
	}
	shown := entries
	if len(shown) > spokenListingCap {
		shown = shown[:spokenListingCap]
	}
	parts := make([]string, 0, len(shown))
	for _, e := range shown {
		parts = append(parts, fmt.Sprintf("%s means %s", e.Phrase, e.Meaning))
	}
	spoken := fmt.Sprintf("You have taught me %s: %s.",
		countPhrase(len(entries)), strings.Join(parts, "; "))
	if len(entries) > len(shown) {
		spoken = fmt.Sprintf("You have taught me %s. The most recent are: %s. "+
			"The full list is in the window's Memory tab.",
			countPhrase(len(entries)), strings.Join(parts, "; "))
	}
	return spoken, nil
}

// countPhrase words an entry count for speech.
func countPhrase(n int) string {
	if n == 1 {
		return "one word"
	}
	return fmt.Sprintf("%d words", n)
}

// spokenVocabularyError rewords a store refusal for the ear: the sentinel
// sentences are written for forms and logs, and "an entry needs a meaning"
// read aloud after a mangled transcript would explain nothing.
func spokenVocabularyError(err error) error {
	switch {
	case errors.Is(err, vocabulary.ErrStoreFull):
		return errors.New("my vocabulary is full — forget a word you no longer use, " +
			"or raise vocabulary dot max entries")
	case errors.Is(err, vocabulary.ErrBiasFull):
		return errors.New("the listen-for list is full — I can only be tuned toward a few " +
			"words at once; unflag one in the window first")
	case errors.Is(err, vocabulary.ErrNoPhrase), errors.Is(err, vocabulary.ErrNoMeaning):
		return errors.New("I did not catch both the word and its meaning — " +
			"say, when I say quid, I mean pounds")
	}
	return err
}

// capitaliseSentence upper-cases a warning's first rune so it reads as its
// own sentence after the confirmation.
func capitaliseSentence(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	if r[0] >= 'a' && r[0] <= 'z' {
		r[0] = r[0] - 'a' + 'A'
	}
	return string(r)
}
