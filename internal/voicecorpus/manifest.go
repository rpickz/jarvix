package voicecorpus

import (
	_ "embed"
	"fmt"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

// phrasesTOML is the shipped manifest, embedded rather than read from disk.
//
// Embedding is what lets `jarvix doctor` say something true about the corpus
// from an installed binary that has no source tree behind it (doctor.go's
// summary line). The recordings themselves are emphatically NOT embedded —
// they are personal audio, they live in testdata/voicecorpus, and nothing in
// this package ever copies them anywhere.
//
//go:embed phrases.toml
var phrasesTOML string

// Wake expectations: what the assistant's own name must do to a transcript.
const (
	// WakeNone makes no claim about the name. The default for a phrase
	// recorded without saying it.
	WakeNone = ""
	// WakeName requires that the transcript opens with something the strip
	// accepts as the name — the assistant's name itself, or one of the
	// spellings configured as an alias. This is the assertion for a recording
	// that is *only* the summons: whisper must have written a word this
	// machine recognises, whatever spelling it chose (issue #83).
	WakeName = "name"
	// WakeStrip requires WakeName and, beyond it, that removing the summons
	// leaves a real utterance behind. This is the assertion for "Jarvix, what
	// time is it?": the wake word must come off, and what remains is what the
	// router and the model see.
	WakeStrip = "strip"
)

// Expect is what one phrase claims about the pipeline's behaviour — the
// downstream outcome, never the words.
//
// Every field that is set must hold; fields left out are not checked. A
// phrase with no expectation at all is refused by Validate, because a
// recording nobody asserts anything about is a file that can only ever pass.
type Expect struct {
	// Wake is WakeNone, WakeName or WakeStrip; see above.
	Wake string `toml:"wake"`
	// Intent is the intent name the router must return for this utterance,
	// e.g. "workspace.switch". The router is asked about the transcript with
	// the wake word already stripped, exactly as the engine asks it.
	Intent string `toml:"intent"`
	// Slot is the integer the intent must have parsed out of the utterance —
	// 4 for "workspace four", 25 for "focus on deploy for twenty five
	// minutes". A pointer because 0 is a legitimate slot value ("volume
	// zero") and "no slot expected" has to be distinguishable from it.
	Slot *int `toml:"slot"`
	// NoIntent asserts that the router deliberately does NOT claim this
	// utterance, so it reaches the assistant untouched. That is a real
	// outcome worth pinning: the router's whole discipline is that ambiguity
	// belongs to the model, and an over-eager new pattern that swallowed
	// "what did we talk about yesterday" would be a regression no other test
	// in the tree would notice.
	NoIntent bool `toml:"no_intent"`
	// Words are content words that must survive into the transcript, matched
	// whole and with case and punctuation folded away. This is how a taught
	// vocabulary term, a window nickname or a thread name is asserted — the
	// word Jarvix was biased toward has to actually come out the other end.
	// It is not an exact-transcript assertion and must never be allowed to
	// grow into one: put the one or two words that carry the meaning here,
	// not the sentence.
	Words []string `toml:"words"`
	// Affirmative, when set, is what the confirmation gate must decide about
	// this utterance — true for "yes do it", false for "no don't". Misheard
	// confirmations are the failure mode with the largest blast radius in the
	// whole product, which is why the corpus tests them in the user's own
	// voice rather than only on typed strings.
	Affirmative *bool `toml:"affirmative"`
}

// empty reports whether this expectation asserts nothing at all.
func (e Expect) empty() bool {
	return e.Wake == WakeNone && e.Intent == "" && !e.NoIntent &&
		len(e.Words) == 0 && e.Affirmative == nil
}

// Phrase is one thing to say, and what must happen when it is said.
type Phrase struct {
	// ID is the recording's stem: two digits, a dash, and a lowercase slug —
	// "07-workspace-four", recorded as 07-workspace-four.wav. The number
	// keeps the corpus in the order it is meant to be read and recorded in;
	// the slug is what a failure names.
	ID string `toml:"id"`
	// Say is the phrase to speak, written as a person would say it. It is the
	// script for the recording session and the yardstick Score measures a
	// transcript against — it is NOT asserted, and nothing in this package
	// ever compares a transcript to it for equality.
	Say string `toml:"say"`
	// Note is guidance for whoever records it: how to say it, or why the
	// expectation is what it is. Printed in the report beside a failure.
	Note string `toml:"note"`
	// Noisy marks the phrases worth recording a second time in a noisy room
	// (the starred items in issue #143). The second take is a sibling file,
	// 07-workspace-four-noisy.wav, and is scored and baselined separately
	// under its own id — the same phrase, a harder day.
	Noisy bool `toml:"noisy_take"`
	// Expect is the downstream outcome.
	Expect Expect `toml:"expect"`
}

// Manifest is the corpus definition: every phrase, in recording order.
type Manifest struct {
	Phrases []Phrase `toml:"phrase"`
}

// NoisySuffix marks a second, noisy-room take of a phrase in its file name.
const NoisySuffix = "-noisy"

// idPattern is the file-stem grammar: "NN-lowercase-slug".
//
// Strict on purpose. The corpus is a directory of files a person drags in, and
// the two mistakes that directory invites — a stray file, and a typo'd stem
// that silently matches no phrase — are both mistakes that would otherwise
// present as "the harness passed". Anything not matching this is reported by
// name (see Load).
var idPattern = regexp.MustCompile(`^[0-9]{2}-[a-z0-9]+(-[a-z0-9]+)*$`)

// Phrases returns the shipped manifest, or an error if the embedded file has
// been broken. Both failure modes are pinned by a hermetic test, so in a
// built binary this only ever succeeds.
func Phrases() (Manifest, error) {
	return ParseManifest(phrasesTOML)
}

// ParseManifest reads and validates a manifest document.
func ParseManifest(document string) (Manifest, error) {
	var m Manifest
	if _, err := toml.Decode(document, &m); err != nil {
		return Manifest{}, fmt.Errorf("voice corpus manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// Validate reports every problem in the manifest at once, because a manifest
// is edited in bulk and fixing typos one run at a time is a waste of a person.
func (m Manifest) Validate() error {
	if len(m.Phrases) == 0 {
		return fmt.Errorf("voice corpus manifest: no phrases defined")
	}
	var problems []string
	seen := make(map[string]bool, len(m.Phrases))
	for i, p := range m.Phrases {
		where := fmt.Sprintf("phrase[%d]", i)
		if p.ID != "" {
			where = p.ID
		}
		switch {
		case p.ID == "":
			problems = append(problems, where+": no id")
		case !idPattern.MatchString(p.ID):
			problems = append(problems, fmt.Sprintf(
				"%s: id must be two digits, a dash and a lowercase slug (e.g. 07-workspace-four)", where))
		case strings.HasSuffix(p.ID, NoisySuffix):
			// Reserved: a "-noisy" stem is how a second take of an existing
			// phrase is recognised, so a phrase claiming that stem would make
			// one recording ambiguous between two phrases.
			problems = append(problems, fmt.Sprintf(
				"%s: %q is reserved for the second take of another phrase; choose another slug", where, NoisySuffix))
		case seen[p.ID]:
			problems = append(problems, where+": duplicate id")
		}
		seen[p.ID] = true
		if strings.TrimSpace(p.Say) == "" {
			problems = append(problems, where+": no phrase to say")
		}
		problems = append(problems, p.Expect.problems(where)...)
	}
	if len(problems) > 0 {
		return fmt.Errorf("voice corpus manifest:\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

// problems reports what is wrong with one expectation, prefixed with where it
// was found.
func (e Expect) problems(where string) []string {
	var out []string
	switch e.Wake {
	case WakeNone, WakeName, WakeStrip:
	default:
		out = append(out, fmt.Sprintf("%s: wake = %q; use %q or %q, or leave it out",
			where, e.Wake, WakeName, WakeStrip))
	}
	if e.Intent != "" && e.NoIntent {
		out = append(out, where+": expect.intent and expect.no_intent contradict each other")
	}
	if e.Slot != nil && e.Intent == "" {
		out = append(out, where+": expect.slot needs an expect.intent to belong to")
	}
	for _, w := range e.Words {
		if strings.TrimSpace(w) == "" {
			out = append(out, where+": expect.words contains an empty entry")
			break
		}
		if len(strings.Fields(w)) > 1 {
			out = append(out, fmt.Sprintf(
				"%s: expect.words entry %q is more than one word; list the words separately so each is matched on its own",
				where, w))
			break
		}
	}
	if e.empty() {
		// The one rule that makes the corpus mean anything: a recording with
		// nothing asserted about it is a file that passes whatever whisper
		// does to it.
		out = append(out, where+": no expectation; a phrase nothing is asserted about can only ever pass")
	}
	return out
}

// Find returns the phrase with the given id.
func (m Manifest) Find(id string) (Phrase, bool) {
	for _, p := range m.Phrases {
		if p.ID == id {
			return p, true
		}
	}
	return Phrase{}, false
}
