package session

import (
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/memory"
	"github.com/rpickz/jarvix/internal/vocabulary"
)

// Engine-side taught vocabulary (issue #129): where the block lands in the
// message list, that a zero-entry prompt is byte-identical to a pre-feature
// one, which paths consult the store at all, how the injection is disclosed,
// and that the deterministic teach phrases go to the seam — never to the
// provider.

// fakeVocabInjector is a scripted VocabularyInjector that counts
// consultations, the fakeInjector pattern.
type fakeVocabInjector struct {
	mu        sync.Mutex
	injection vocabulary.Injection
	calls     int
}

func (f *fakeVocabInjector) Inject() vocabulary.Injection {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.injection
}

func (f *fakeVocabInjector) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeTeacher is a scripted VocabularyTeacher recording what the intents
// hand it.
type fakeTeacher struct {
	mu      sync.Mutex
	taught  [][2]string
	flagged []string
	listed  int
	err     error
	spoken  string
}

func (f *fakeTeacher) TeachEntry(phrase, meaning, source string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.taught = append(f.taught, [2]string{phrase, meaning})
	return f.spoken, f.err
}

func (f *fakeTeacher) ListenFor(phrase string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flagged = append(f.flagged, phrase)
	return f.spoken, f.err
}

func (f *fakeTeacher) SpokenListing() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listed++
	return f.spoken, f.err
}

// TestVocabularyReachesTheModel is the headline acceptance criterion: a
// taught phrase is in front of the model on a later, unrelated turn —
// asserted through the fake provider's recorded request, placement included:
// system prompt, memory, vocabulary, question.
func TestVocabularyReachesTheModel(t *testing.T) {
	injector := &fakeVocabInjector{injection: vocabulary.Injection{
		Message: "Taught vocabulary: words the user taught you.\n\n" +
			"- [w1, taught 2026-08-20] \"quid\" means: pounds",
		Total: 1,
	}}
	memoryInj := &fakeInjector{injection: memory.Injection{
		Message: "Remembered facts: block", Total: 1}}
	h := newHarness(t, Options{SystemPrompt: "You are Jarvix.",
		Memory: memoryInj, Vocabulary: injector})

	h.ask(t, "how much did I spend, in quid?")

	if injector.Calls() != 1 {
		t.Fatalf("injector consulted %d times, want once per model turn", injector.Calls())
	}
	msgs := h.provider.LastRequest.Messages
	if len(msgs) != 4 {
		t.Fatalf("messages = %d (%v), want system, memory, vocabulary, question", len(msgs), roles(msgs))
	}
	if msgs[1].Role != ai.RoleSystem || !strings.Contains(msgs[1].Content, "Remembered facts") {
		t.Errorf("message[1] = %+v, want the memory block first", msgs[1])
	}
	if msgs[2].Role != ai.RoleSystem || !strings.Contains(msgs[2].Content, "quid") {
		t.Errorf("message[2] = %+v, want the vocabulary block beside memory", msgs[2])
	}
	if msgs[3].Role != ai.RoleUser {
		t.Errorf("last message = %+v, want the question", msgs[3])
	}
}

// TestZeroEntryPromptIsByteIdentical is the pinned byte-identity criterion:
// with the feature enabled but nothing taught, the provider request is
// byte-for-byte the request a daemon without the feature sends — same
// messages, same order, nothing added anywhere.
func TestZeroEntryPromptIsByteIdentical(t *testing.T) {
	baseline := newHarness(t, Options{SystemPrompt: "You are Jarvix.", HistoryTurns: 4})
	baseline.ask(t, "hello")

	enabled := newHarness(t, Options{SystemPrompt: "You are Jarvix.", HistoryTurns: 4,
		Vocabulary: &fakeVocabInjector{}}) // enabled, zero entries
	enabled.ask(t, "hello")

	if !reflect.DeepEqual(baseline.provider.LastRequest, enabled.provider.LastRequest) {
		t.Errorf("zero-entry request differs from the pre-feature request:\nbase: %+v\nwith: %+v",
			baseline.provider.LastRequest, enabled.provider.LastRequest)
	}
	// The consultation is still recorded: "nothing was injected" is an audit
	// answer (vocabulary.last), even though the prompt carries nothing.
	if _, _, taken := enabled.engine.LastVocabulary(); !taken {
		t.Error("an empty injection was not retained for the audit")
	}
}

// TestVocabularyDisabledMeansAbsent: nil injector, no block, no event, no
// audit record — the feature off is the feature absent.
func TestVocabularyDisabledMeansAbsent(t *testing.T) {
	h := newHarness(t, Options{SystemPrompt: "sys"}) // Options.Vocabulary nil
	h.ask(t, "hello")
	for _, m := range h.provider.LastRequest.Messages {
		if strings.Contains(m.Content, "Taught vocabulary") {
			t.Errorf("a disabled vocabulary injected a block: %q", m.Content)
		}
	}
	if inj, _, taken := h.engine.LastVocabulary(); taken {
		t.Errorf("LastVocabulary = %+v, want nothing recorded with the feature disabled", inj)
	}
}

// TestVocabularyIsNotConsultedForRoutedIntents: the deterministic router
// answers without a model, so it must not pay even the stat(2).
func TestVocabularyIsNotConsultedForRoutedIntents(t *testing.T) {
	injector := &fakeVocabInjector{injection: vocabulary.Injection{Message: "Taught vocabulary: block"}}
	h := newIntentHarness(t, Options{Vocabulary: injector, HistoryTurns: 4})

	h.say(t, "volume thirty")
	if injector.Calls() != 0 {
		t.Fatalf("a matched intent consulted the vocabulary %d times; it must pay nothing",
			injector.Calls())
	}
	h.say(t, "why is my build failing?")
	if injector.Calls() != 1 {
		t.Fatalf("injector consulted %d times on the model path, want 1", injector.Calls())
	}
}

// TestVocabularyInjectedEventCarriesCountsOnly: the bus event is the public
// announcement, and phrase content must never ride on it.
func TestVocabularyInjectedEventCarriesCountsOnly(t *testing.T) {
	h := newHarness(t, Options{Vocabulary: &fakeVocabInjector{injection: vocabulary.Injection{
		Message:   "Taught vocabulary:\n- [w1] \"atlas\" means: the secret staging server",
		Entries:   []vocabulary.Entry{{ID: "w1", Phrase: "atlas", Meaning: "the secret staging server"}},
		Trimmed:   2,
		Total:     3,
		EstTokens: 21,
	}}})

	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit("hello"); err != nil {
		t.Fatal(err)
	}
	seen := h.collectUntil(t, "session.finished")
	h.waitIdle(t)

	ev, ok := seen["vocabulary.injected"]
	if !ok {
		t.Fatal("no vocabulary.injected event was published")
	}
	if ev.Data["entries"] != 1 || ev.Data["trimmed"] != 2 || ev.Data["total"] != 3 || ev.Data["est_tokens"] != 21 {
		t.Errorf("event data = %v, want the counts", ev.Data)
	}
	payload, err := json.Marshal(ev.Data)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "atlas") {
		t.Errorf("event leaked phrase content: %s", payload)
	}

	// And the audit surface holds the content, for the user, on request.
	inj, sessionID, taken := h.engine.LastVocabulary()
	if !taken || sessionID == "" || len(inj.Entries) != 1 || inj.Entries[0].Phrase != "atlas" {
		t.Errorf("LastVocabulary = (%+v, %q, %v), want the turn's injection", inj, sessionID, taken)
	}
}

// TestTeachIntentGoesToTheSeamNotTheModel: "when i say X i mean Y" is a
// deterministic turn — the seam is called with both slots, the confirmation
// is spoken, and the provider is never involved.
func TestTeachIntentGoesToTheSeamNotTheModel(t *testing.T) {
	teacher := &fakeTeacher{spoken: "Okay — quid means pounds."}
	h := newIntentHarness(t, Options{VocabularyTeacher: teacher, SpeakResponses: true, HistoryTurns: 4})

	seen := h.say(t, "when I say quid I mean pounds")

	if len(h.provider.Requests) != 0 {
		t.Fatalf("a teach reached the provider %d times", len(h.provider.Requests))
	}
	teacher.mu.Lock()
	taught := append([][2]string(nil), teacher.taught...)
	teacher.mu.Unlock()
	if len(taught) != 1 || taught[0] != [2]string{"quid", "pounds"} {
		t.Fatalf("seam taught %v, want quid → pounds", taught)
	}
	ev := seen["intent.executed"]
	if ev.Data["intent"] != "vocabulary.teach" || ev.Data["acknowledgement"] != teacher.spoken {
		t.Errorf("intent.executed = %v", ev.Data)
	}
	if !strings.Contains(h.tts.LastRequest.Text, "quid means pounds") {
		t.Errorf("the confirmation was not spoken: %+v", h.tts.LastRequest)
	}
}

// TestListenAndListIntentsRouteToTheSeam covers the other two phrases.
func TestListenAndListIntentsRouteToTheSeam(t *testing.T) {
	teacher := &fakeTeacher{spoken: "I will listen for quid."}
	h := newIntentHarness(t, Options{VocabularyTeacher: teacher, HistoryTurns: 4})

	h.say(t, "listen for the word quid")
	h.say(t, "what words have I taught you")

	teacher.mu.Lock()
	defer teacher.mu.Unlock()
	if len(teacher.flagged) != 1 || teacher.flagged[0] != "quid" {
		t.Errorf("seam flagged %v, want [quid]", teacher.flagged)
	}
	if teacher.listed != 1 {
		t.Errorf("seam listed %d times, want 1", teacher.listed)
	}
	if len(h.provider.Requests) != 0 {
		t.Errorf("the provider was called %d times", len(h.provider.Requests))
	}
}

// TestTeachWithoutSeamRefusesHonestly: a matched phrase on a daemon with the
// feature disabled is a spoken refusal, never a silent drop.
func TestTeachWithoutSeamRefusesHonestly(t *testing.T) {
	h := newIntentHarness(t, Options{HistoryTurns: 4}) // no VocabularyTeacher
	seen := h.say(t, "when I say quid I mean pounds")
	ev := seen["intent.executed"]
	if ev.Data["status"] != "failed" {
		t.Fatalf("intent.executed = %v, want an honest failure", ev.Data)
	}
	if ack, _ := ev.Data["acknowledgement"].(string); !strings.Contains(ack, "not available") {
		t.Errorf("acknowledgement = %q, want the refusal", ack)
	}
}
