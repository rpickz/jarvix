package session

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/conversations"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/knowledge"
	"github.com/rpickz/jarvix/internal/memory"
	"github.com/rpickz/jarvix/internal/provenance"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/tools"
	"github.com/rpickz/jarvix/internal/tts"
	"github.com/rpickz/jarvix/internal/vocabulary"
)

// Engine-side answer provenance (issue #168): what each collection point
// contributes, what it deliberately does not, and — the criterion the privacy
// rule turns on — that nothing any of them saw is written anywhere.

// --- per-source-kind derivation ------------------------------------------

func TestMemorySourcesNameFactsByID(t *testing.T) {
	refs := memorySources(memory.Injection{Facts: []memory.Fact{
		{ID: "m1", Content: "the staging server is called atlas"},
		{ID: "m7", Content: "the deploy key rotates monthly"},
	}})
	if len(refs) != 2 {
		t.Fatalf("refs = %+v", refs)
	}
	for i, want := range []string{"m1", "m7"} {
		if refs[i].Kind != provenance.KindFact || refs[i].Ref != want {
			t.Errorf("refs[%d] = %+v", i, refs[i])
		}
		// Injection is not use: the weaker claim, always.
		if refs[i].Strength != provenance.Available {
			t.Errorf("refs[%d] overstated the claim: %q", i, refs[i].Strength)
		}
		// References, never content.
		if refs[i].Subject != "" {
			t.Errorf("refs[%d] carried a subject: %q", i, refs[i].Subject)
		}
	}
}

func TestVocabularySourcesNameEntriesByID(t *testing.T) {
	refs := vocabularySources(vocabulary.Injection{Entries: []vocabulary.Entry{
		{ID: "w2", Phrase: "the box", Meaning: "the home server"},
	}})
	if len(refs) != 1 || refs[0].Kind != provenance.KindVocabulary || refs[0].Ref != "w2" {
		t.Fatalf("refs = %+v", refs)
	}
	if refs[0].Strength != provenance.Available {
		t.Errorf("strength = %q", refs[0].Strength)
	}
}

func TestKnowledgeSourcesNameFeedsAndNeverValues(t *testing.T) {
	refs := knowledgeSources(knowledge.Injection{
		Message: "- prices: 41231 (as of a minute ago)",
		Feeds:   1,
		Names:   []string{"prices"},
	})
	if len(refs) != 1 || refs[0].Kind != provenance.KindFeed || refs[0].Ref != "prices" {
		t.Fatalf("refs = %+v", refs)
	}
	for _, r := range refs {
		if strings.Contains(r.Ref+r.Subject, "41231") {
			t.Errorf("a feed value reached the reference: %+v", r)
		}
	}
}

// TestContextSourcesNameTheSourceAndNeverTheCapture is the privacy line for
// desktop context. The active-window capture's whole text *is* the window's
// identity line, so naming the window would be recording the capture.
func TestContextSourcesNameTheSourceAndNeverTheCapture(t *testing.T) {
	refs := contextSources(desktop.Snapshot{Items: []desktop.Item{
		{Source: desktop.SourceWindow, Text: "Alacritty — SECRET-WINDOW-MARKER"},
		{Source: desktop.SourceClipboard, Text: "SECRET-CLIPBOARD-MARKER"},
	}})
	if len(refs) != 2 {
		t.Fatalf("refs = %+v", refs)
	}
	if refs[0].Ref != string(desktop.SourceWindow) || refs[1].Ref != string(desktop.SourceClipboard) {
		t.Errorf("sources not named: %+v", refs)
	}
	for _, r := range refs {
		if strings.Contains(r.Ref+r.Subject, "MARKER") {
			t.Errorf("captured text reached the reference: %+v", r)
		}
	}
}

// --- tool derivation ------------------------------------------------------

func TestToolSourceNamesTheToolAndItsSubject(t *testing.T) {
	call := ai.ToolCall{Name: "shell.run", Arguments: `{"command":"git status"}`}
	refs := toolSource(call, "On branch main", nil)
	if len(refs) != 1 {
		t.Fatalf("refs = %+v", refs)
	}
	if refs[0].Kind != provenance.KindTool || refs[0].Tool != "shell.run" {
		t.Fatalf("refs[0] = %+v", refs[0])
	}
	if refs[0].Subject != "git status" {
		t.Errorf("subject = %q", refs[0].Subject)
	}
	// A tool ran and returned output: mechanically causal, so the stronger
	// claim — and only here.
	if refs[0].Strength != provenance.Returned {
		t.Errorf("strength = %q", refs[0].Strength)
	}
}

// TestToolSourceNeverCarriesAQuery: a search query can quote the very fact it
// is looking for, which is why the Activity pane refuses to show one either.
func TestToolSourceNeverCarriesAQuery(t *testing.T) {
	for _, name := range []string{"memory.search", "conversations.search"} {
		call := ai.ToolCall{Name: name, Arguments: `{"query":"SECRET-QUERY-MARKER"}`}
		refs := toolSource(call, "Found 1 matching passage", nil)
		if len(refs) != 1 {
			t.Fatalf("%s refs = %+v", name, refs)
		}
		if strings.Contains(refs[0].Subject+refs[0].Ref, "MARKER") {
			t.Errorf("%s carried its query: %+v", name, refs[0])
		}
	}
}

// TestToolSourceIgnoresAFailedCall: a call that returned an error returned no
// output, so claiming it went into the answer would be the overstatement this
// feature exists to avoid.
func TestToolSourceIgnoresAFailedCall(t *testing.T) {
	call := ai.ToolCall{Name: "shell.run", Arguments: `{"command":"git status"}`}
	if refs := toolSource(call, "error: unknown tool", nil); refs != nil {
		t.Errorf("a failed call became a source: %+v", refs)
	}
}

// TestToolSourceLetsATOolSpeakForItself: what a tool reports about itself is
// the call's provenance, replacing the generic tool line — "the artifact
// chart.png" is the thing to press, and "artifact.create" beside it is noise.
func TestToolSourceLetsAToolSpeakForItself(t *testing.T) {
	call := ai.ToolCall{Name: "artifact.create", Arguments: `{"title":"Q3"}`}
	refs := toolSource(call, "The artifact is now open.", []provenance.Reference{
		{Kind: provenance.KindArtifact, Ref: "/tmp/2026-08-28-q3.png", Subject: "diagram"},
	})
	if len(refs) != 1 {
		t.Fatalf("refs = %+v", refs)
	}
	if refs[0].Kind != provenance.KindArtifact || refs[0].Ref != "/tmp/2026-08-28-q3.png" {
		t.Fatalf("refs[0] = %+v", refs[0])
	}
	if refs[0].Tool != "artifact.create" || refs[0].Strength != provenance.Returned {
		t.Errorf("refs[0] = %+v", refs[0])
	}
}

// TestKnowledgeToolIsAFeedNotAnAnonymousCall: a feed the model read navigates
// to the Knowledge tab exactly like an injected one; only the strength
// differs, because this one actually ran.
func TestKnowledgeToolIsAFeedNotAnAnonymousCall(t *testing.T) {
	call := ai.ToolCall{Name: "knowledge.get", Arguments: `{"feed":"prices"}`}
	refs := toolSource(call, "prices: 41231", nil)
	if len(refs) != 1 || refs[0].Kind != provenance.KindFeed || refs[0].Ref != "prices" {
		t.Fatalf("refs = %+v", refs)
	}
	if refs[0].Strength != provenance.Returned {
		t.Errorf("strength = %q", refs[0].Strength)
	}
}

func TestToolSourceSurvivesMalformedArguments(t *testing.T) {
	call := ai.ToolCall{Name: "shell.run", Arguments: `{"command":`}
	refs := toolSource(call, "ok", nil)
	if len(refs) != 1 || refs[0].Tool != "shell.run" {
		t.Fatalf("refs = %+v", refs)
	}
	if refs[0].Subject != "" {
		t.Errorf("a subject was invented from unparseable arguments: %q", refs[0].Subject)
	}
}

// --- end to end through the engine ---------------------------------------

// scriptedMemory hands out one injection per turn, so a test can change what
// a turn is given without writing to the engine's options while it runs.
type scriptedMemory struct {
	mu     sync.Mutex
	rounds []memory.Injection
}

func (s *scriptedMemory) Inject() memory.Injection {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.rounds) == 0 {
		return memory.Injection{}
	}
	inj := s.rounds[0]
	s.rounds = s.rounds[1:]
	return inj
}

type fakeKnowledgeInjector struct{ injection knowledge.Injection }

func (f *fakeKnowledgeInjector) Inject() knowledge.Injection { return f.injection }

// provenanceHarness is the standard harness with a tool registry attached
// before the engine is built, plus every injection wired. Built here rather
// than reusing newHarness because the registry has to exist at construction
// time — the engine holds it, and swapping it afterwards would leave the
// shutdown cleanup pointed at a discarded engine.
func provenanceHarness(t *testing.T, opts Options, registered tools.Tool) *harness {
	t.Helper()
	h := &harness{
		provider: &ai.Fake{Response: "Atlas, through the staging pipeline."},
		stt:      &stt.Fake{Text: "where do I deploy?"},
		tts:      &tts.Fake{},
		recorder: &audio.FakeRecorder{Clip: audio.Clip{WAVPath: t.TempDir() + "/rec.wav",
			SampleRate: 16000, Channels: 1}},
		player: &audio.FakePlayer{},
		tools:  tools.NewRegistry(nil),
	}
	if registered != nil {
		h.tools.Register(registered)
	}
	if opts.Model == "" {
		opts.Model = "test-model"
	}
	bus := NewBus(nil)
	h.events, h.cancel = bus.Subscribe()
	t.Cleanup(h.cancel)
	h.engine = NewEngine(h.provider, h.stt, h.tts, h.recorder, h.player, h.tools, nil, bus, nil, opts)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := h.engine.Shutdown(ctx); err != nil {
			t.Errorf("engine had not quiesced by the end of the test: %v", err)
		}
	})
	return h
}

// TestTurnRecordsWhatWentIntoIt is the headline: a turn that was given a
// fact, a feed value and a capture, and that ran a tool, carries all of them
// on its assistant half — the injections as the weaker claim, the tool as the
// stronger one.
func TestTurnRecordsWhatWentIntoIt(t *testing.T) {
	fake := conversations.NewFake()
	opts := Options{
		Model: "test-model", HistoryTurns: 5, FollowUpWindow: time.Hour,
		Archive: fake,
		Context: &desktop.FakeCollector{Snapshot: desktop.Snapshot{
			Items: []desktop.Item{{Source: desktop.SourceClipboard, Text: "a url"}},
		}},
		Memory: &scriptedMemory{rounds: []memory.Injection{{
			Message: "Remembered facts:\n- [m1, stored 2026-08-01] atlas is the staging host",
			Facts:   []memory.Fact{{ID: "m1", Content: "atlas is the staging host"}},
			Total:   1,
		}}},
		Knowledge: &fakeKnowledgeInjector{injection: knowledge.Injection{
			Message: "Live feed values:\n- prices: 41231",
			Feeds:   1,
			Names:   []string{"prices"},
		}},
	}
	h := provenanceHarness(t, opts, &recordingTool{result: "On branch main"})
	h.provider.ToolCallsByRound = [][]ai.ToolCall{
		{{ID: "c1", Name: "run", Arguments: `{"command":"git status"}`}},
		nil,
	}

	h.ask(t, "where do I deploy?")
	awaitAppend(t, fake)

	rec := lastArchivedProvenance(t, fake)
	claims := map[string]string{}
	for _, src := range rec.Sources {
		key := src.Kind + ":" + src.Ref
		if src.Kind == provenance.KindTool {
			key = src.Kind + ":" + src.Tool
		}
		claims[key] = src.Strength
	}
	for key, want := range map[string]string{
		"fact:m1":           provenance.Available,
		"feed:prices":       provenance.Available,
		"desktop:clipboard": provenance.Available,
		"tool:run":          provenance.Returned,
	} {
		got, ok := claims[key]
		if !ok {
			t.Errorf("%s missing from %+v", key, rec.Sources)
			continue
		}
		if got != want {
			t.Errorf("%s claimed %q, want %q", key, got, want)
		}
	}

	// The live view carries the same record on the same turn.
	turns := h.engine.Conversation()
	answer := turns[len(turns)-1]
	if answer.Role != string(ai.RoleAssistant) || answer.Provenance == nil {
		t.Fatalf("the live answer carries no provenance: %+v", answer)
	}
	if len(answer.Provenance.Sources) != len(rec.Sources) {
		t.Errorf("live view and archive disagree: %d vs %d",
			len(answer.Provenance.Sources), len(rec.Sources))
	}
	// A user turn never carries provenance.
	if turns[len(turns)-2].Provenance != nil {
		t.Error("the question carried provenance")
	}
	// And "where did that come from?" reads the same record.
	last, ok := h.engine.LastProvenance()
	if !ok || last == nil || len(last.Sources) != len(rec.Sources) {
		t.Errorf("LastProvenance = %+v (ok=%v)", last, ok)
	}
}

// TestATurnThatConsumedNothingCarriesNoProvenance: absence is information,
// and an affordance that is always there says nothing.
func TestATurnThatConsumedNothingCarriesNoProvenance(t *testing.T) {
	fake := conversations.NewFake()
	h := newHarness(t, archiveOptions(fake, 5))

	h.ask(t, "say something")
	awaitAppend(t, fake)

	for _, turn := range fake.Turns(fake.Active()) {
		if turn.Provenance != nil {
			t.Errorf("a turn that used nothing recorded %+v", turn.Provenance)
		}
	}
	for _, turn := range h.engine.Conversation() {
		if turn.Provenance != nil {
			t.Errorf("the live view invented provenance: %+v", turn.Provenance)
		}
	}
	if rec, ok := h.engine.LastProvenance(); ok {
		t.Errorf("LastProvenance found something to cite: %+v", rec)
	}
}

// TestCapturedContentNeverReachesTheRecordOrAnEvent is #168's leak-salted
// criterion, and the reason provenance stores references and nothing else: a
// turn given a captured window, a remembered fact and a feed value writes
// none of their content to the archive, and publishes none of it on the bus.
// The transient rule of ADR 0043/0047 stands — what Jarvix saw exists in the
// prompt of that one turn and nowhere durable.
func TestCapturedContentNeverReachesTheRecordOrAnEvent(t *testing.T) {
	fake := conversations.NewFake()
	opts := Options{
		Model: "test-model", HistoryTurns: 5, FollowUpWindow: time.Hour,
		Archive: fake,
		Context: &desktop.FakeCollector{Snapshot: desktop.Snapshot{
			Items: []desktop.Item{
				{Source: desktop.SourceWindow, Text: "Alacritty — SECRET-WINDOW-MARKER"},
				{Source: desktop.SourceClipboard, Text: "SECRET-CLIPBOARD-MARKER"},
			},
		}},
		Memory: &scriptedMemory{rounds: []memory.Injection{{
			Message: "Remembered facts:\n- [m1, stored 2026-08-01] SECRET-FACT-MARKER",
			Facts:   []memory.Fact{{ID: "m1", Content: "SECRET-FACT-MARKER"}},
			Total:   1,
		}}},
		Knowledge: &fakeKnowledgeInjector{injection: knowledge.Injection{
			Message: "Live feed values:\n- prices: SECRET-VALUE-MARKER",
			Feeds:   1,
			Names:   []string{"prices"},
		}},
	}
	h := provenanceHarness(t, opts, nil)

	// Every event of the exchange is collected before anything is asserted:
	// collectUntil returns only once session.finished has been seen, so the
	// ordering this test relies on is one it establishes, not the
	// scheduler's.
	seen := h.collectAll(t, "what is on screen?")
	awaitAppend(t, fake)

	archived, err := json.Marshal(fake.Turns(fake.Active()))
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{
		"SECRET-WINDOW-MARKER", "SECRET-CLIPBOARD-MARKER",
		"SECRET-FACT-MARKER", "SECRET-VALUE-MARKER",
	} {
		if strings.Contains(string(archived), marker) {
			t.Errorf("transient content %q reached the archive", marker)
		}
	}
	for _, ev := range seen {
		payload, err := json.Marshal(ev.Data)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(payload), "MARKER") {
			t.Errorf("event %s carries content: %s", ev.Type, payload)
		}
	}
	// The provenance itself is there — the leak test must not pass by the
	// feature simply not working.
	rec := lastArchivedProvenance(t, fake)
	if len(rec.Sources) == 0 {
		t.Fatal("nothing was recorded; the leak test proves nothing")
	}
}

// TestProvenanceFollowsItsTurnUnderTheCap: the retention cap slides the head,
// and what went into an answer must slide with the answer, never onto another
// one.
func TestProvenanceFollowsItsTurnUnderTheCap(t *testing.T) {
	fake := conversations.NewFake()
	opts := Options{
		Model: "test-model", HistoryTurns: 1, FollowUpWindow: time.Hour,
		Archive: fake,
		// One injection, then nothing: the first turn has provenance and the
		// second does not, scripted rather than swapped mid-flight.
		Memory: &scriptedMemory{rounds: []memory.Injection{{
			Message: "Remembered facts:\n- [m1, stored 2026-08-01] atlas",
			Facts:   []memory.Fact{{ID: "m1", Content: "atlas"}},
		}}},
	}
	h := provenanceHarness(t, opts, nil)

	h.ask(t, "first question")
	awaitAppend(t, fake)
	h.ask(t, "second question")
	awaitAppend(t, fake)

	turns := h.engine.Conversation()
	if len(turns) != 2 {
		t.Fatalf("the cap kept %d turns, want the last exchange only", len(turns))
	}
	for _, turn := range turns {
		if turn.Provenance != nil {
			t.Errorf("the trimmed turn's provenance landed on %q: %+v", turn.Role, turn.Provenance)
		}
	}
	// The archive keeps everything the cap dropped, provenance included.
	archived := fake.Turns(fake.Active())
	if archived[1].Provenance == nil || archived[1].Provenance.Sources[0].Ref != "m1" {
		t.Errorf("the archive lost the first answer's provenance: %+v", archived[1].Provenance)
	}
}

// collectAll drives one exchange and returns every event it published, in
// order. It stands in for harness.ask in tests that must inspect the whole
// event stream: ask drains the bus itself, so by the time an assertion looked
// the events would be gone.
func (h *harness) collectAll(t *testing.T, text string) []Event {
	t.Helper()
	if _, err := h.engine.StartSession(); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Submit(text); err != nil {
		t.Fatal(err)
	}
	var seen []Event
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-h.events:
			seen = append(seen, ev)
			if ev.Type == "session.finished" {
				h.waitIdle(t)
				return seen
			}
		case <-deadline:
			t.Fatalf("timed out; saw %d events", len(seen))
		}
	}
}

// lastArchivedProvenance reads the newest archived answer's provenance.
func lastArchivedProvenance(t *testing.T, fake *conversations.Fake) *provenance.Record {
	t.Helper()
	turns := fake.Turns(fake.Active())
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].Role == string(ai.RoleAssistant) {
			if turns[i].Provenance == nil {
				t.Fatalf("the archived answer carries no provenance: %+v", turns[i])
			}
			return turns[i].Provenance
		}
	}
	t.Fatal("no assistant turn was archived")
	return nil
}
