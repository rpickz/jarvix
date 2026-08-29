package situation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/provenance"
)

// The tests in this file are hermetic by construction: every seam a report
// touches — the clock, the sources, the provider, the process's own start-up
// moment, the event bus — is a function on Options, so nothing here starts a
// daemon, reads a disk, or talks to a compositor.

// fixed is a clock that only moves when a test moves it.
type fixed struct{ at atomic.Int64 }

func newFixed(t time.Time) *fixed {
	f := &fixed{}
	f.at.Store(t.UnixNano())
	return f
}

func (f *fixed) now() time.Time      { return time.Unix(0, f.at.Load()) }
func (f *fixed) add(d time.Duration) { f.at.Add(int64(d)) }

// stub builds a source that answers with the given items, counting its reads so
// a test can assert that a cached ask read nothing.
func stub(name string, reads *atomic.Int32, items ...Item) Source {
	return Source{Name: name, Read: func(context.Context, Instant) ([]Item, error) {
		if reads != nil {
			reads.Add(1)
		}
		return items, nil
	}}
}

// broken is a source that cannot be read.
func broken(name string) Source {
	return Source{Name: name, Read: func(context.Context, Instant) ([]Item, error) {
		return nil, errors.New("the store would not open")
	}}
}

func item(rank Rank, text string) Item { return Item{Rank: rank, Text: text} }

func newTestService(t *testing.T, clock *fixed, opts Options) *Service {
	t.Helper()
	opts.Now = clock.now
	return NewService(opts, nil)
}

// TestTheOrderingIsPinned. The ordering IS the feature — a report read in any
// other order makes the listener wait through the news they did not need for
// the news they did — so it is pinned here rather than left to whichever order
// the sources happen to be declared in.
//
// Note what this test does not do: it does not name a source. Every rank below
// is filled by ONE stub source declared in a single deliberately-wrong order,
// so a pass proves the composer sorts by rank and not by arrival.
func TestTheOrderingIsPinned(t *testing.T) {
	clock := newFixed(time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC))
	s := newTestService(t, clock, Options{Sources: []Source{
		// Deliberately declared upside down.
		stub("one", nil,
			item(Housekeeping, "housekeeping."),
			item(Failing, "failing."),
			item(Finished, "finished."),
			item(InProgress, "in progress."),
			item(NeedsYou, "needs you."),
		),
		broken("two"),
	}})

	r, err := s.View(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, sec := range r.Sections {
		titles = append(titles, sec.Title)
	}
	want := []string{"Needs you", "In progress", "Finished since you last looked",
		"Failing", "Housekeeping", "I couldn't check"}
	if strings.Join(titles, "|") != strings.Join(want, "|") {
		t.Errorf("section order = %v, want %v", titles, want)
	}
	// And the spoken form says them in exactly that order too, which is the
	// half a listener actually experiences. Searched past the headline, whose
	// own summary names the same ranks.
	body := r.Spoken[len(r.Headline):]
	wantSpoken := []string{"needs you.", "in progress.", "finished.", "failing.", "housekeeping."}
	at := -1
	for _, phrase := range wantSpoken {
		idx := strings.Index(body, phrase)
		if idx <= at {
			t.Fatalf("%q is out of order in the spoken report: %q", phrase, r.Spoken)
		}
		at = idx
	}
}

// TestTheOrderingIsNotTheBriefings guards the one thing most likely to be
// "corrected" by somebody reading the two packages side by side. The return
// briefing puts what finished second; this report puts what is running second,
// because it answers a different question. Inverting them here would be a
// silent regression that every other test would still pass.
func TestTheOrderingIsNotTheBriefings(t *testing.T) {
	if InProgress >= Finished {
		t.Fatal("in-progress must come before finished: the report is about now")
	}
	if NeedsYou >= InProgress || Finished >= Failing || Failing >= Housekeeping {
		t.Fatal("the rank order has drifted from needs-you → in-progress → finished → failing → housekeeping")
	}
	if Unavailable != ordered[len(ordered)-1] {
		t.Fatal("the admissions must come last")
	}
}

// TestNeedsYouLeadsWhateverElseIsTrue. The AI-session classification is the
// highest-value fact on the machine and the acceptance criteria say it is said
// first. It gets there by rank, not by a special case, so a source that only
// ever produces housekeeping cannot displace it however it is declared.
func TestNeedsYouLeadsWhateverElseIsTrue(t *testing.T) {
	clock := newFixed(time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC))
	s := newTestService(t, clock, Options{Sources: []Source{
		stub("windows", nil, item(Housekeeping, "Nine windows are open.")),
		stub("sessions", nil, item(NeedsYou, "The AI session on deploy is waiting on you.")),
	}})

	r, err := s.View(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if r.Sections[0].Title != "Needs you" {
		t.Fatalf("first section = %q", r.Sections[0].Title)
	}
	if !strings.Contains(r.Sections[0].Lines[0].Text, "deploy") {
		t.Errorf("the session was not named: %q", r.Sections[0].Lines[0].Text)
	}
}

// TestAQuietMachineSaysSoAndNoModelIsAsked. "Nothing needs you" has to be the
// answer rather than a manufactured list, and it has to be a property of the
// code rather than a hope about the prompt — so the provider is a fixture that
// fails the test if it is called at all.
func TestAQuietMachineSaysSoAndNoModelIsAsked(t *testing.T) {
	clock := newFixed(time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC))
	s := newTestService(t, clock, Options{
		Sources: []Source{stub("sessions", nil), stub("reminders", nil)},
		Summarise: func(context.Context, string) (string, error) {
			t.Error("a model was asked to word a quiet machine")
			return "Everything is going great.", nil
		},
	})

	r, err := s.View(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Quiet {
		t.Error("a machine with nothing on it was not reported quiet")
	}
	if r.Spoken != QuietSentence {
		t.Errorf("spoken = %q, want %q", r.Spoken, QuietSentence)
	}
	if r.ModelOutcome != "off" {
		t.Errorf("model outcome = %q, want off", r.ModelOutcome)
	}
	if len(r.Sections) != 0 {
		t.Errorf("a quiet report invented sections: %v", r.Sections)
	}
}

// TestHousekeepingAloneIsStillQuiet. The desktop always has something on it, so
// a Quiet that any housekeeping line defeated would be a Quiet that never
// happened. Housekeeping is reported and is not news, and the headline says so.
func TestHousekeepingAloneIsStillQuiet(t *testing.T) {
	clock := newFixed(time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC))
	s := newTestService(t, clock, Options{
		Sources: []Source{stub("windows", nil, item(Housekeeping, "Nine windows are open."))},
		Summarise: func(context.Context, string) (string, error) {
			t.Error("a model was asked to word housekeeping")
			return "", nil
		},
	})

	r, err := s.View(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Quiet {
		t.Error("housekeeping alone defeated the quiet answer")
	}
	if r.Headline != QuietSentence {
		t.Errorf("headline = %q", r.Headline)
	}
	// The housekeeping is still reported. Quiet is a claim about news, not a
	// reason to withhold what was read.
	if !strings.Contains(r.Spoken, "Nine windows are open.") {
		t.Errorf("the housekeeping was dropped: %q", r.Spoken)
	}
}

// TestASourceThatCannotBeReadIsNamed. The listener has to be able to tell
// "nothing is failing" from "I did not look", so a refusal becomes a named line
// and never a silent omission — and it defeats the quiet answer, because
// "nothing needs you" from a daemon that could not read two of its sources is a
// claim it has not earned.
func TestASourceThatCannotBeReadIsNamed(t *testing.T) {
	clock := newFixed(time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC))
	s := newTestService(t, clock, Options{Sources: []Source{
		stub("sessions", nil),
		broken(SourceReminders),
	}})

	r, err := s.View(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if r.Quiet {
		t.Error("a report that could not read a source called itself quiet")
	}
	if len(r.Unavailable) != 1 || r.Unavailable[0] != SourceReminders {
		t.Fatalf("unavailable = %v", r.Unavailable)
	}
	last := r.Sections[len(r.Sections)-1]
	if last.Title != "I couldn't check" {
		t.Fatalf("the admission is not the last section: %q", last.Title)
	}
	if !strings.Contains(last.Lines[0].Text, "your reminders") {
		t.Errorf("the source was not named in English: %q", last.Lines[0].Text)
	}
	if !strings.Contains(r.Spoken, "your reminders") {
		t.Errorf("the admission was not spoken: %q", r.Spoken)
	}
}

// TestASourceThatCannotWordItsOwnAdmission. "I couldn't check" is this
// package's sentence, so a source that tried to file one of its own is dropped
// — otherwise the one wording a listener has learned to trust would have as
// many variants as there are adapters.
func TestASourceThatCannotWordItsOwnAdmission(t *testing.T) {
	clock := newFixed(time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC))
	s := newTestService(t, clock, Options{Sources: []Source{
		stub("sneaky", nil, item(Unavailable, "everything is fine, honest.")),
	}})

	r, err := s.View(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.Spoken, "honest") {
		t.Errorf("a source worded its own admission: %q", r.Spoken)
	}
	if len(r.Unavailable) != 0 {
		t.Errorf("a source that did not fail was named unavailable: %v", r.Unavailable)
	}
}

// TestASourceAddedLaterNeedsNoChangeToTheComposer is the design constraint the
// ticket cares about more than any single feature: the jobs source of #195's
// next slice, and the remote-machine source of its last, must be additions
// rather than surgery.
//
// The demonstration is deliberately blunt. A source this package has never
// heard of is registered with a name that appears nowhere in it, produces items
// in four ranks, and the report orders them, ranks them, links them, speaks
// them and — when it fails — names it, with no line of situation.go, compose.go
// or prompt.go mentioning it. If a later slice ever has to touch the composer
// to add a source, this test is what will have stopped compiling first.
func TestASourceAddedLaterNeedsNoChangeToTheComposer(t *testing.T) {
	clock := newFixed(time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC))
	jobRef := provenance.Reference{Kind: provenance.KindThread,
		Strength: provenance.Returned, Ref: "job-7"}
	s := newTestService(t, clock, Options{Sources: []Source{
		stub("sessions", nil, item(NeedsYou, "The AI session on deploy is waiting on you.")),
		// The stranger.
		Source{Name: "jobs", Read: func(context.Context, Instant) ([]Item, error) {
			return []Item{
				{Rank: NeedsYou, Text: "The job \"get CI green\" needs a decision.", Where: &jobRef},
				{Rank: InProgress, Text: "The job \"tidy downloads\" is on step two of four."},
				{Rank: Finished, Text: "The job \"set up the laptop\" finished."},
				{Rank: Failing, Text: "The job \"nightly backup\" gave up."},
			}, nil
		}},
	}})

	r, err := s.View(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	// Its items land in the right sections, in the right order, behind the
	// source declared before it.
	if got := r.Sections[0].Title; got != "Needs you" {
		t.Fatalf("first section = %q", got)
	}
	if len(r.Sections[0].Lines) != 2 {
		t.Fatalf("needs-you lines = %d, want 2", len(r.Sections[0].Lines))
	}
	if !strings.Contains(r.Sections[0].Lines[0].Text, "deploy") {
		t.Errorf("declaration order was not honoured inside the rank: %v", r.Sections[0].Lines)
	}
	if !strings.Contains(r.Sections[0].Lines[1].Text, "get CI green") {
		t.Errorf("the new source's item is missing: %v", r.Sections[0].Lines)
	}
	// Its link is resolvable through the same array every other line uses.
	link := r.Sections[0].Lines[1].Link
	if link < 0 || link >= len(r.Sources) || r.Sources[link] != jobRef {
		t.Errorf("the new source's line does not point at its own subject: link=%d sources=%v",
			link, r.Sources)
	}
	// And when it fails it is named, in its own name, with no entry for it
	// anywhere in sourceNoun.
	broke := newTestService(t, clock, Options{Sources: []Source{broken("jobs")}})
	fail, err := broke.View(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fail.Spoken, "jobs") {
		t.Errorf("an unknown source did not name itself when it failed: %q", fail.Spoken)
	}
}

// TestEveryLineCarriesItsOwnLink. The window sends Sources to
// provenance.resolve verbatim and reads each line's item back at its Link, so
// the indices have to be right for every line, in render order, with the
// unlinked ones marked rather than defaulted to zero.
func TestEveryLineCarriesItsOwnLink(t *testing.T) {
	clock := newFixed(time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC))
	refs := []provenance.Reference{
		{Kind: provenance.KindThread, Strength: provenance.Returned, Ref: "t1"},
		{Kind: provenance.KindReminder, Strength: provenance.Returned, Ref: "r1"},
		{Kind: provenance.KindFeed, Strength: provenance.Returned, Ref: "prices"},
	}
	s := newTestService(t, clock, Options{Sources: []Source{
		stub("sessions", nil,
			Item{Rank: NeedsYou, Text: "one.", Where: &refs[0]},
			// No link: a line about a group, not a thing.
			Item{Rank: NeedsYou, Text: "and two more."},
		),
		stub("reminders", nil, Item{Rank: NeedsYou, Text: "three.", Where: &refs[1]}),
		stub("activity", nil, Item{Rank: Failing, Text: "four.", Where: &refs[2]}),
	}})

	r, err := s.View(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Sources) != 3 {
		t.Fatalf("sources = %v", r.Sources)
	}
	var seen []string
	for _, sec := range r.Sections {
		for _, line := range sec.Lines {
			if line.Link < 0 {
				seen = append(seen, line.Text+"→none")
				continue
			}
			if line.Link >= len(r.Sources) {
				t.Fatalf("line %q links past the end: %d", line.Text, line.Link)
			}
			seen = append(seen, line.Text+"→"+r.Sources[line.Link].Ref)
		}
	}
	want := "one.→t1|and two more.→none|three.→r1|four.→prices"
	if got := strings.Join(seen, "|"); got != want {
		t.Errorf("links = %s, want %s", got, want)
	}
}

// TestTheSpokenReportIsBoundedAndSaysWhatItDropped. The bound is seconds of
// speech; the trim takes whole lines from the tail; and a shortened report that
// did not say it was shortened would be the same lie as one that dropped a
// source silently.
func TestTheSpokenReportIsBoundedAndSaysWhatItDropped(t *testing.T) {
	clock := newFixed(time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC))
	var items []Item
	for i := range 12 {
		items = append(items, item(Housekeeping,
			fmt.Sprintf("Housekeeping line number %d has quite a few words in it indeed.", i)))
	}
	items = append([]Item{item(NeedsYou, "The AI session on deploy is waiting on you.")}, items...)
	s := newTestService(t, clock, Options{Sources: []Source{stub("many", nil, items...)}})

	r, err := s.View(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Truncated {
		t.Fatal("a thirteen-line report was not truncated")
	}
	if !strings.HasSuffix(r.Spoken, windowPointer) {
		t.Errorf("the drop was not disclosed: %q", r.Spoken)
	}
	if got := len(strings.Fields(r.Spoken)); got > maxSpokenWords {
		t.Errorf("spoken words = %d, over the %d budget", got, maxSpokenWords)
	}
	// The trim takes the TAIL, so what needed the user survives it.
	if !strings.Contains(r.Spoken, "deploy") {
		t.Errorf("the trim ate the needs-you line: %q", r.Spoken)
	}
	// The window still holds everything.
	total := 0
	for _, sec := range r.Sections {
		total += len(sec.Lines)
	}
	if total != 13 {
		t.Errorf("the window version lost lines: %d", total)
	}
}

// TestTheAdmissionsSurviveTheTrim. The trim takes the tail and the admissions
// live there, so a shortened report that quietly lost "I couldn't check the
// reminders" has become a dishonest one.
func TestTheAdmissionsSurviveTheTrim(t *testing.T) {
	clock := newFixed(time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC))
	var items []Item
	for i := range 12 {
		items = append(items, item(Housekeeping,
			fmt.Sprintf("Housekeeping line number %d has quite a few words in it indeed.", i)))
	}
	s := newTestService(t, clock, Options{Sources: []Source{
		stub("many", nil, items...),
		broken(SourceReminders),
	}})

	r, err := s.View(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Truncated {
		t.Fatal("the report was not truncated, so this proves nothing")
	}
	if !strings.Contains(r.Spoken, "your reminders") {
		t.Errorf("the admission was trimmed away: %q", r.Spoken)
	}
}

// TestAskingTwiceInsideTheWindowReadsNothing is the caching rule. Asking twice
// in a minute must be cheap, and "cheap" means no source read at all — not a
// faster one.
func TestAskingTwiceInsideTheWindowReadsNothing(t *testing.T) {
	clock := newFixed(time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC))
	var reads atomic.Int32
	var calls atomic.Int32
	s := newTestService(t, clock, Options{
		Sources: []Source{stub("sessions", &reads,
			item(NeedsYou, "The AI session on deploy is waiting on you."))},
		Summarise: func(context.Context, string) (string, error) {
			calls.Add(1)
			return "One thing is waiting on you.", nil
		},
	})

	first, err := s.Situation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	clock.add(20 * time.Second)
	second, err := s.Situation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if reads.Load() != 1 {
		t.Errorf("source reads = %d, want 1", reads.Load())
	}
	if calls.Load() != 1 {
		t.Errorf("model calls = %d, want 1", calls.Load())
	}
	if first != second {
		t.Errorf("a replay said something different:\n%q\n%q", first, second)
	}

	// Past the window it is composed again, because a reading of "now" that is
	// a minute old has stopped being one.
	clock.add(DefaultCacheFor)
	if _, err := s.Situation(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reads.Load() != 2 {
		t.Errorf("source reads after the window lapsed = %d, want 2", reads.Load())
	}
}

// TestTheCacheNeverOutlivesWhatTheAgeScaleCanSay is the caching rule's honesty
// argument, pinned. The shared spoken age scale bottoms out at "just now" under
// a minute, and the cache window sits inside that floor — so a replay is a
// report whose age Jarvix has no word to distinguish from a fresh one, rather
// than a stale answer dressed up as a current one. If the window were ever
// raised past the floor this test is what would notice.
func TestTheCacheNeverOutlivesWhatTheAgeScaleCanSay(t *testing.T) {
	if DefaultCacheFor >= time.Minute {
		t.Fatalf("the cache window is %v, past the point where the age scale can "+
			"still call a replay \"just now\"", DefaultCacheFor)
	}
}

// TestAReplayIsMarkedAndKeepsItsOwnMoment. The cache is honest about itself:
// the window is handed the moment the report was actually composed, and a flag
// saying it is a replay, so no surface can imply a freshness it does not have.
func TestAReplayIsMarkedAndKeepsItsOwnMoment(t *testing.T) {
	clock := newFixed(time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC))
	var reads atomic.Int32
	s := newTestService(t, clock, Options{
		Sources: []Source{stub("sessions", &reads, item(NeedsYou, "waiting."))},
	})

	fresh, err := s.View(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Cached {
		t.Error("a first composition called itself cached")
	}
	clock.add(20 * time.Second)
	held, err := s.View(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !held.Cached {
		t.Fatal("a replay did not say it was one")
	}
	if !held.At.Equal(fresh.At) {
		t.Errorf("a replay moved its own timestamp: %v vs %v", held.At, fresh.At)
	}
	if reads.Load() != 1 {
		t.Errorf("source reads = %d, want 1", reads.Load())
	}
}

// TestRefreshComposesAgain. The Refresh button is the escape hatch that makes
// the cache safe: anyone who doubts a replay can force a new reading.
func TestRefreshComposesAgain(t *testing.T) {
	clock := newFixed(time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC))
	var reads atomic.Int32
	s := newTestService(t, clock, Options{
		Sources: []Source{stub("sessions", &reads, item(NeedsYou, "waiting."))},
	})

	if _, err := s.View(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	r, err := s.View(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if reads.Load() != 2 {
		t.Errorf("source reads = %d, want 2 — refresh did not bypass the cache", reads.Load())
	}
	if r.Cached {
		t.Error("a forced composition called itself cached")
	}
}

// TestSinceYouLastLookedMovesOnlyOnARealComposition. A replay is not a new
// look: if it moved the watermark, a second ask thirty seconds later would
// report nothing as having finished since a report the user was only shown a
// copy of.
func TestSinceYouLastLookedMovesOnlyOnARealComposition(t *testing.T) {
	clock := newFixed(time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC))
	seen := make(chan Instant, 8)
	s := newTestService(t, clock, Options{
		Seed: func() (time.Time, bool) {
			return time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC), true
		},
		Sources: []Source{{Name: "spy", Read: func(_ context.Context, at Instant) ([]Item, error) {
			seen <- at
			return []Item{item(NeedsYou, "waiting.")}, nil
		}}},
	})

	if _, err := s.View(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	first := <-seen
	if !first.Since.Equal(time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("the seed was not used as the first window: %v", first.Since)
	}

	// A replay inside the cache window reads nothing at all, so nothing is
	// sent on the channel and the watermark cannot have moved from it.
	clock.add(time.Second)
	if _, err := s.View(context.Background(), false); err != nil {
		t.Fatal(err)
	}

	clock.add(DefaultCacheFor)
	if _, err := s.View(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	second := <-seen
	if !second.Since.Equal(first.Now) {
		t.Errorf("the second window opened at %v, want the first composition's moment %v",
			second.Since, first.Now)
	}
}

// TestAFirstEverReportHasNoBackwardEdge. With no seed and nobody having ever
// looked, Since is zero — the signal a source uses to report nothing
// interval-shaped rather than reading out its whole history as news. The report
// itself is still given: it is about now, and "now" does not need a window.
func TestAFirstEverReportHasNoBackwardEdge(t *testing.T) {
	clock := newFixed(time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC))
	seen := make(chan Instant, 4)
	s := newTestService(t, clock, Options{
		Sources: []Source{{Name: "spy", Read: func(_ context.Context, at Instant) ([]Item, error) {
			seen <- at
			return []Item{item(Housekeeping, "Nine windows are open.")}, nil
		}}},
		StartedAfter: func(time.Time) bool { return true },
	})

	r, err := s.View(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	at := <-seen
	if !at.Since.IsZero() {
		t.Errorf("a first-ever report invented a window: %v", at.Since)
	}
	if r.Spoken == "" {
		t.Error("a first-ever report refused to answer")
	}
	// And no caveat, because nothing is being claimed about a past stretch —
	// a doubt with no claim attached to it is worse than no doubt at all.
	if r.Caveat != "" {
		t.Errorf("a report with no window admitted a coverage gap: %q", r.Caveat)
	}
}

// TestARestartIsAdmittedUpFront. The activity ring dies with the process, so a
// report whose "since you last looked" opens before this process started is
// composed from sources that answer confidently for the whole stretch and one
// that could not have seen the start of it. It is said about the report itself,
// spoken second, and never trimmed (#190's correction, applied here from the
// beginning).
func TestARestartIsAdmittedUpFront(t *testing.T) {
	clock := newFixed(time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC))
	lastLooked := time.Date(2026, 8, 28, 18, 0, 0, 0, time.UTC)
	started := time.Date(2026, 8, 29, 9, 30, 0, 0, time.UTC)
	s := newTestService(t, clock, Options{
		Seed:         func() (time.Time, bool) { return lastLooked, true },
		Sources:      []Source{stub("activity", nil)},
		StartedAfter: func(since time.Time) bool { return started.After(since) },
	})

	r, err := s.View(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if r.Caveat == "" {
		t.Fatal("a report whose window predates the process made no admission")
	}
	// Spoken second: after the headline and before anything else.
	head := strings.Index(r.Spoken, r.Headline)
	caveat := strings.Index(r.Spoken, r.Caveat)
	if head != 0 || caveat <= head {
		t.Errorf("the caveat is not spoken second: %q", r.Spoken)
	}
	// It names both halves, so the listener is handed a fact rather than a
	// doubt they cannot size.
	for _, half := range []string{"only goes back to then", "read live"} {
		if !strings.Contains(r.Caveat, half) {
			t.Errorf("the caveat does not name %q: %q", half, r.Caveat)
		}
	}
	// Quiet describes the LINES, so it is true beside a caveat — read the two
	// together, which is how both renderings present them.
	if !r.Quiet {
		t.Error("a quiet report with a caveat was not reported quiet")
	}
}

// TestNoRestartMeansNoCaveat. An admission is only worth making when it is
// demonstrably true; a report from a process that has been up the whole time
// claims full coverage because it has it.
func TestNoRestartMeansNoCaveat(t *testing.T) {
	clock := newFixed(time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC))
	s := newTestService(t, clock, Options{
		Seed:         func() (time.Time, bool) { return time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC), true },
		Sources:      []Source{stub("activity", nil)},
		StartedAfter: func(time.Time) bool { return false },
	})

	r, err := s.View(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if r.Caveat != "" {
		t.Errorf("an unbroken process admitted a gap it does not have: %q", r.Caveat)
	}
}

// TestEverySourceIsReadEvenWhenOneIsSlow. The reads are parallel, so a source
// that takes most of the budget must not cost the ones after it their answers.
// The stub below blocks until every other source has been entered, which can
// only happen if they run concurrently — sequentially it would deadlock, and
// the test would fail on the budget rather than silently pass.
func TestEverySourceIsReadEvenWhenOneIsSlow(t *testing.T) {
	clock := newFixed(time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC))
	entered := make(chan struct{}, 3)
	release := make(chan struct{})
	blocking := Source{Name: "slow", Read: func(ctx context.Context, _ Instant) ([]Item, error) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return []Item{item(Housekeeping, "slow.")}, nil
	}}
	quick := func(name, text string) Source {
		return Source{Name: name, Read: func(context.Context, Instant) ([]Item, error) {
			entered <- struct{}{}
			return []Item{item(Housekeeping, text)}, nil
		}}
	}
	s := newTestService(t, clock, Options{Sources: []Source{
		blocking, quick("a", "quick a."), quick("b", "quick b."),
	}})

	done := make(chan Report, 1)
	go func() {
		r, err := s.View(context.Background(), false)
		if err != nil {
			t.Error(err)
		}
		done <- r
	}()
	// All three sources have been entered while the first is still blocked:
	// the reads overlap.
	for range 3 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("not every source was entered while one was blocked — the reads are sequential")
		}
	}
	close(release)
	r := <-done
	for _, want := range []string{"slow.", "quick a.", "quick b."} {
		if !strings.Contains(r.Spoken, want) {
			t.Errorf("%q is missing from %q", want, r.Spoken)
		}
	}
}

// TestTheEventCarriesNoWordOfTheReport. The activity feed says a report was
// given and stops there. The salt below appears in every line and every source
// name; finding it in the event at all is the failure.
func TestTheEventCarriesNoWordOfTheReport(t *testing.T) {
	const salt = "zarquon"
	clock := newFixed(time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC))
	events := make(chan map[string]any, 4)
	s := newTestService(t, clock, Options{
		Sources: []Source{
			stub("sessions", nil, item(NeedsYou, "The AI session on "+salt+" is waiting on you.")),
			broken(salt + "-store"),
		},
		Summarise: func(context.Context, string) (string, error) {
			return "One " + salt + " thing is waiting on you.", nil
		},
		Publish: func(_ string, data map[string]any) { events <- data },
	})

	r, err := s.View(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Spoken, salt) {
		t.Fatal("the salt is not in the report, so this proves nothing")
	}
	data := <-events
	// The source NAME is allowed through — naming an unreadable source is the
	// point of the field — so it is scrubbed before the sweep.
	delete(data, "unavailable")
	for k, v := range data {
		if text, ok := v.(string); ok && strings.Contains(text, salt) {
			t.Errorf("the event leaked the report in %q: %q", k, text)
		}
	}
}
