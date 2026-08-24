package session

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rpickz/jarvix/internal/audio"
)

// This file is the engine's half of hands-free activation (ADR 0024).
//
// A wake-word session is a push-to-talk session with the two human gestures
// replaced: the chord going down becomes the detector firing, and the chord
// coming up becomes silence. Everything between and after — interruption,
// transcription, the intent router, the permission gate, history — is the
// same code, reached the same way, which is the point. Wake-word activation
// must not be a second pipeline with its own bugs.
//
// The one structural difference is where the audio comes from. StartVoice
// asks the engine's Recorder to open the microphone; by the time a wake word
// has been recognised the microphone is *already* open, in the wake
// listener's own supervised capture, and the request has largely been spoken.
// So the pair below takes the recording from the caller instead of creating
// one, and the engine's Recorder is left alone — two processes must never
// hold the microphone for the same utterance.

// StartWake begins a session for a wake-word activation. It is called the
// instant the detector fires, before the request has been captured, which is
// what makes "Jarvix, stop" feel instant: interruption happens on the wake
// word, not a sentence and an endpoint later.
//
// While a tool confirmation is pending it captures the user's answer instead,
// exactly as holding the push-to-talk chord does — the pending session keeps
// waiting and the transcript resolves it.
//
// It returns the session id, which FinishWake and AbortWake must quote: the
// gap between the wake word and the end of the sentence is seconds long, and
// anything at all may have interrupted in the meantime.
func (e *Engine) StartWake() (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Someone is already speaking into a capture of their own — a held chord,
	// `jarvix listen`. Two captures of one utterance is not a thing to
	// arbitrate, and the deliberate gesture wins: the wake word is ignored.
	if e.state == StateListening || e.state == StateTranscribing {
		return "", fmt.Errorf("a capture is already in progress")
	}

	if e.state == StateAwaitingConfirmation && e.pending != nil {
		s := e.current
		if err := e.setStateLocked(StateListening); err != nil {
			return "", err
		}
		e.pending.engaged = true
		s.replyCapture = true
		s.transcript = ""
		s.transcriptReady = false
		s.submitted = false
		s.voiceStarted = time.Now()
		e.publish(Event{Type: "recording.started", Data: map[string]any{"session_id": s.id}})
		return s.id, nil
	}

	id, err := e.startSessionLocked()
	if err != nil {
		return "", err
	}
	s := e.current
	if err := e.setStateLocked(StateListening); err != nil {
		e.failLocked(s, "session", err)
		return "", err
	}
	s.wake = true
	s.voiceStarted = time.Now()
	e.publish(Event{Type: "recording.started", Data: map[string]any{"session_id": s.id}})
	return id, nil
}

// FinishWake completes a wake-word session with the captured utterance and
// submits it. There is no key to release, so the silence the endpointer found
// *is* the submission: capture and submit are one decision here, taken under
// one lock, rather than the two calls push-to-talk makes.
//
// spoken is how much audio the capture holds; a capture shorter than
// Options.MinRecording is discarded as an accidental activation, the same
// guard a stray chord tap gets. discarded is true whenever nothing was
// transcribed — because the audio was too short, or because the session it
// belonged to is gone.
func (e *Engine) FinishWake(id string, rec audio.Recording, spoken time.Duration) (discarded bool, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.current

	// The session this capture belongs to was superseded while the user was
	// still talking — a chord press, a typed question, `jarvix cancel`. The
	// newer intent wins and this audio is deleted, unheard: capturing it was
	// unavoidable, keeping it is not.
	if s == nil || s.id != id || e.state != StateListening {
		rec.Cancel()
		return true, nil
	}
	if spoken < e.opts.MinRecording {
		e.log.Info("wake capture discarded as accidental", "component", "session",
			"session_id", s.id, "spoken_ms", spoken.Milliseconds(),
			"min_ms", e.opts.MinRecording.Milliseconds())
		rec.Cancel()
		e.cancelLocked(fmt.Sprintf("capture too short (%dms, minimum %dms)",
			spoken.Milliseconds(), e.opts.MinRecording.Milliseconds()))
		return true, nil
	}
	if err := e.setStateLocked(StateTranscribing); err != nil {
		return false, err
	}
	// The latency budget starts where the user stopped talking, which for a
	// hands-free turn is here — the same instant a chord release marks.
	s.timings.markCaptureStop()
	e.publish(Event{Type: "recording.stopped", Data: map[string]any{"session_id": s.id}})
	e.active.Add(1)
	go func() { defer e.active.Done(); e.transcribe(s, rec) }()

	s.submitted = true
	e.maybeThinkLocked(s)
	return false, nil
}

// stripWakeWord removes the assistant's name from the front of a hands-free
// transcript.
//
// It is deliberately narrow. Only a *leading* occurrence goes, and only when
// it stands as its own word(s) — "Jarvix, what's my disk usage?" loses its
// first word, "what did Jarvix say?" keeps all of them. Punctuation and case
// are whatever whisper decided, so both are ignored, and a filler that
// precedes the name ("hey Jarvix", "okay Jarvix") is handled by taking the
// name wherever it starts in the first two words rather than only at index
// zero.
//
// word is the configured assistant name (issue #103), and it may be more than
// one word ("Mister Smith"): a target is matched as a sequence of
// whitespace-delimited transcript words, each compared with punctuation and
// case ignored, so "Mister Smith, open the window" loses its first two words
// under the same leading-whole-word discipline a single-word name gets.
//
// aliases are the spellings the strip accepts *as* the name. The default name
// is out-of-vocabulary for whisper, so the detector fires on the right sound
// and the transcript still opens with a nearby real word (issue #83); a strip
// that only knows the true spelling then leaves the summons in place and the
// intent router never matches. Aliases get exactly the same leading-whole-word
// discipline — "tell me about Jarvis Cocker" mid-sentence is never touched —
// and they exist only here: the acoustic wake gate is unchanged.
//
// An utterance that is *only* the name is left alone: it becomes an empty
// transcript otherwise, and "I didn't catch that" is a better answer than a
// session that fails for no visible reason.
func stripWakeWord(transcript, word string, aliases []string) string {
	targets := make([][]string, 0, 1+len(aliases))
	if w := foldedWords(word); len(w) > 0 {
		targets = append(targets, w)
	}
	if len(targets) == 0 {
		return transcript
	}
	for _, alias := range aliases {
		if w := foldedWords(alias); len(w) > 0 {
			targets = append(targets, w)
		}
	}
	// Longest target first, so when one is a prefix of another ("Mister"
	// alongside "Mister Smith") the whole summons is stripped, not half of
	// it — leaving "Smith, open the window" would be worse than either.
	sort.SliceStable(targets, func(i, j int) bool { return len(targets[i]) > len(targets[j]) })

	fields := strings.Fields(transcript)
	for start := 0; start < len(fields) && start < 2; start++ {
		for _, target := range targets {
			if !wordsMatchAt(fields, start, target) {
				continue
			}
			rest := strings.Join(fields[start+len(target):], " ")
			if strings.TrimSpace(rest) == "" {
				return transcript
			}
			return rest
		}
	}
	return transcript
}

// wordsMatchAt reports whether the target's words appear in fields starting
// at start, comparing each transcript word with case and surrounding
// punctuation ignored — the same folding foldedWords applied to the target.
func wordsMatchAt(fields []string, start int, target []string) bool {
	if start+len(target) > len(fields) {
		return false
	}
	for k, want := range target {
		if strings.ToLower(trimWordPunctuation(fields[start+k])) != want {
			return false
		}
	}
	return true
}

// foldedWords splits a configured name or alias into the lowercased,
// punctuation-trimmed words the matcher compares — nil when nothing usable
// remains, so a blank entry can never match anything.
func foldedWords(s string) []string {
	fields := strings.Fields(s)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f = strings.ToLower(trimWordPunctuation(f)); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// trimWordPunctuation strips the punctuation whisper puts around a word, so a
// comparison is between words rather than between renderings of them.
func trimWordPunctuation(s string) string {
	return strings.Trim(s, ` .,!?;:"'“”‘’…`)
}

// AbortWake ends a wake-word session that produced nothing worth
// transcribing — a false activation followed by silence. Quoting the id keeps
// it from cancelling a session that has since replaced this one.
func (e *Engine) AbortWake(id, reason string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.current == nil || e.current.id != id || e.state != StateListening {
		return
	}
	e.cancelLocked(reason)
}
