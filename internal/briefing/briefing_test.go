package briefing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// The return briefing's tests (#150, ADR 0050). Everything here is hermetic:
// an injected clock, scripted sources, a faked provider seam, and no sleeps
// anywhere — the one deadline test drives the budget down rather than waiting
// one out.

// fixedNow is the moment every test measures from: a Wednesday morning, so an
// eight-hour absence reads as the night it is meant to describe.
var fixedNow = time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)

// harness scripts the seams a Service is built on and records what it did.
type harness struct {
	mu sync.Mutex

	now time.Time
	set Settings

	// lines and errs script one source each, by name.
	lines map[string][]Line
	errs  map[string]error

	// reply and replyErr script the provider seam; block holds it on the
	// caller's context instead, so a deadline test never sleeps.
	reply    string
	replyErr error
	block    bool

	prompts []string
	reads   map[string]int
	events  []event

	svc *Service
}

type event struct {
	name string
	data map[string]any
}

// newHarness builds a Service over the scripted seams. sources names the
// sources to wire, in order — the ordering within a category is the
// declaration order, and several tests turn on that.
func newHarness(t *testing.T, sources ...string) *harness {
	t.Helper()
	h := &harness{
		now:   fixedNow,
		set:   Settings{Enabled: true, AfterHours: 8},
		lines: map[string][]Line{},
		errs:  map[string]error{},
		reads: map[string]int{},
	}
	opts := Options{
		Now:      func() time.Time { return h.clock() },
		Settings: func() Settings { return h.settings() },
		Publish: func(name string, data map[string]any) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.events = append(h.events, event{name: name, data: data})
		},
	}
	for _, name := range sources {
		opts.Sources = append(opts.Sources, Source{Name: name, Read: h.reader(name)})
	}
	h.svc = NewService(opts, slog.New(slog.DiscardHandler))
	return h
}

func (h *harness) clock() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

func (h *harness) settings() Settings {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.set
}

func (h *harness) reader(name string) func(context.Context, time.Time, time.Time) ([]Line, error) {
	return func(ctx context.Context, _, _ time.Time) ([]Line, error) {
		h.mu.Lock()
		h.reads[name]++
		err, lines := h.errs[name], h.lines[name]
		block := h.block
		h.mu.Unlock()
		if block {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		if err != nil {
			return nil, err
		}
		return lines, nil
	}
}

// withModel wires the provider seam. Done after construction, the daemon's
// own late-bind path, so the tests exercise the shape production uses.
func (h *harness) withModel() {
	h.svc.BindSummarise(func(_ context.Context, prompt string) (string, error) {
		h.mu.Lock()
		h.prompts = append(h.prompts, prompt)
		reply, err := h.reply, h.replyErr
		h.mu.Unlock()
		return reply, err
	})
}

// away moves the clock forward by d and reports the arrival, which is what
// the engine does at the top of a user-started exchange.
func (h *harness) away(d time.Duration) {
	h.mu.Lock()
	h.now = h.now.Add(d)
	now := h.now
	h.mu.Unlock()
	h.svc.Arrive(now)
}

// seen is the first arrival: it seeds the watermark without any absence.
func (h *harness) seen() { h.svc.Arrive(h.clock()) }

func (h *harness) set1(name string, lines ...Line) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lines[name] = lines
}

func (h *harness) readCount(name string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reads[name]
}

func (h *harness) prompted() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.prompts))
	copy(out, h.prompts)
	return out
}

func (h *harness) published() []event {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]event, len(h.events))
	copy(out, h.events)
	return out
}

// ------------------------------------------------------------- the absence

// TestAbsenceThresholdIsTheBoundary pins both sides of it. The threshold is
// the whole difference between a feature that speaks after a night and one
// that speaks after lunch, so "at least eight hours" is asserted at eight
// hours exactly, not merely somewhere past it.
func TestAbsenceThresholdIsTheBoundary(t *testing.T) {
	for _, tc := range []struct {
		name  string
		gap   time.Duration
		offer bool
	}{
		{"a second short of the threshold", 8*time.Hour - time.Second, false},
		{"exactly the threshold", 8 * time.Hour, true},
		{"well past it", 14 * time.Hour, true},
		{"a lunch break", 90 * time.Minute, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, SourceSessions)
			h.set1(SourceSessions, Line{Category: Awaiting, Text: "Something wants you."})
			h.seen()
			h.away(tc.gap)
			got := offerOf(h.svc)
			if tc.offer && got != offerSentence {
				t.Errorf("offer = %q, want the offer sentence", got)
			}
			if !tc.offer && got != "" {
				t.Errorf("offer = %q, want silence", got)
			}
		})
	}
}

// TestFirstEverExchangeIsNotAnAbsence: with no watermark and no seed, there is
// nothing to measure against, and an unmeasurable absence must not be
// invented — a fresh install must not greet its first user with a briefing
// about a machine that was not running.
func TestFirstEverExchangeIsNotAnAbsence(t *testing.T) {
	h := newHarness(t, SourceSessions)
	h.set1(SourceSessions, Line{Category: Awaiting, Text: "Something wants you."})
	h.away(30 * 24 * time.Hour)
	if got := offerOf(h.svc); got != "" {
		t.Errorf("offer on a first-ever exchange = %q", got)
	}
	if n := h.readCount(SourceSessions); n != 0 {
		t.Errorf("sources were read %d times with no absence to report on", n)
	}
}

// TestSeedMakesAnAbsenceSurviveARestart. The seed is the conversation
// archive's newest LastActive, which is why an overnight reboot still knows
// it was a night.
func TestSeedMakesAnAbsenceSurviveARestart(t *testing.T) {
	seed := fixedNow.Add(-12 * time.Hour)
	svc := NewService(Options{
		Now:      func() time.Time { return fixedNow },
		Settings: func() Settings { return Settings{Enabled: true, AfterHours: 8} },
		Seed:     func() (time.Time, bool) { return seed, true },
		Sources: []Source{{Name: SourceSessions,
			Read: func(context.Context, time.Time, time.Time) ([]Line, error) {
				return []Line{{Category: Completed, Text: "A session finished."}}, nil
			}}},
	}, slog.New(slog.DiscardHandler))
	svc.Arrive(fixedNow)
	if got := offerOf(svc); got != offerSentence {
		t.Errorf("offer after a restart = %q, want the offer sentence", got)
	}
}

// TestTheOfferIsMadeExactlyOnce. "Exactly one offer line" is a promise about
// the absence, not about the exchange: a second answer in the same morning
// must not repeat it, and the briefing must still be askable afterwards.
func TestTheOfferIsMadeExactlyOnce(t *testing.T) {
	h := newHarness(t, SourceSessions)
	h.set1(SourceSessions, Line{Category: Completed, Text: "A session finished."})
	h.seen()
	h.away(9 * time.Hour)

	if got := offerOf(h.svc); got != offerSentence {
		t.Fatalf("first offer = %q", got)
	}
	h.away(time.Minute)
	if got := offerOf(h.svc); got != "" {
		t.Errorf("second offer = %q, want silence", got)
	}
	spoken, err := h.svc.Briefing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(spoken, "A session finished.") {
		t.Errorf("the briefing stopped standing once the offer was spent: %q", spoken)
	}
}

// TestTheNextAbsenceSupersedesTheStandingOffer.
func TestTheNextAbsenceSupersedesTheStandingOffer(t *testing.T) {
	h := newHarness(t, SourceSessions)
	h.set1(SourceSessions, Line{Category: Completed, Text: "A session finished."})
	h.seen()
	h.away(9 * time.Hour)
	if got := offerOf(h.svc); got != offerSentence {
		t.Fatalf("first offer = %q", got)
	}
	h.away(10 * time.Hour)
	if got := offerOf(h.svc); got != offerSentence {
		t.Errorf("second night's offer = %q, want the offer sentence", got)
	}
}

// TestScheduledWorkNeverCountsAsBeingHere is asserted on the engine side
// (internal/session), but the service's half is that nothing but Arrive can
// move the watermark: there is no other way in, and this pins that the only
// two calls a briefing makes are reads.
func TestServiceHasNoWayToMarkTimeExceptArrive(t *testing.T) {
	h := newHarness(t, SourceSessions)
	h.set1(SourceSessions, Line{Category: Completed, Text: "A session finished."})
	h.seen()
	h.away(9 * time.Hour)
	// A briefing and an offer, in either order, must not move the watermark
	// — otherwise asking about a night would erase it.
	if _, err := h.svc.Briefing(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := offerOf(h.svc); got != offerSentence {
		t.Errorf("offer after a briefing = %q; asking must not consume the absence", got)
	}
}

// ---------------------------------------------------------------- honesty

// TestNothingHappenedIsNeverOfferedAndIsSaidPlainly is the honesty criterion
// in both directions at once: silence when nothing happened, and a plain
// answer to an explicit ask — never a manufactured briefing, and never a
// model call to manufacture one with.
func TestNothingHappenedIsNeverOfferedAndIsSaidPlainly(t *testing.T) {
	h := newHarness(t, SourceSessions, SourceReminders, SourceActivity)
	h.withModel()
	h.reply = "Plenty happened overnight and three sessions finished."
	h.seen()
	h.away(9 * time.Hour)

	if got := offerOf(h.svc); got != "" {
		t.Errorf("offer with nothing to report = %q", got)
	}
	spoken, err := h.svc.Briefing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(spoken, "Nothing while you were away") {
		t.Errorf("empty briefing = %q, want the plain nothing-happened sentence", spoken)
	}
	if strings.Contains(spoken, "three sessions") {
		t.Errorf("an empty night was given content by the model: %q", spoken)
	}
	if n := len(h.prompted()); n != 0 {
		t.Errorf("the model was consulted %d times about an empty night", n)
	}
}

// TestAnUnreadableSourceIsNamedNeverOmitted. A source that errors is the
// difference between "nothing happened there" and "I did not look", and only
// one of those is true.
func TestAnUnreadableSourceIsNamedNeverOmitted(t *testing.T) {
	h := newHarness(t, SourceSessions, SourceReminders)
	h.set1(SourceSessions, Line{Category: Completed, Text: "A session finished."})
	h.errs[SourceReminders] = errors.New("the store is unreadable")
	h.seen()
	h.away(9 * time.Hour)

	spoken, err := h.svc.Briefing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(spoken, "I couldn't check your reminders") {
		t.Errorf("the unreadable source is not named: %q", spoken)
	}
	view, err := h.svc.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Unavailable) != 1 || view.Unavailable[0] != SourceReminders {
		t.Errorf("Unavailable = %v", view.Unavailable)
	}
	if view.Empty {
		t.Error("a briefing with an unreadable source claimed nothing happened")
	}
}

// TestEverythingUnreadableIsNotSomethingToOffer. A briefing whose entire
// content is "I couldn't look" is not news; the honest ask still gets it.
func TestEverythingUnreadableIsNotSomethingToOffer(t *testing.T) {
	h := newHarness(t, SourceSessions)
	h.errs[SourceSessions] = errors.New("gone")
	h.seen()
	h.away(9 * time.Hour)
	if got := offerOf(h.svc); got != "" {
		t.Errorf("offer with only an unavailability to report = %q", got)
	}
	spoken, err := h.svc.Briefing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(spoken, "I couldn't check the AI sessions") {
		t.Errorf("ask = %q, want the named unavailability", spoken)
	}
}

// ----------------------------------------------------------- the ordering

// TestTheBriefingIsOrderedForTheListener pins the ticket's ordering:
// awaiting-you, then completed, then in-progress, then housekeeping, with
// the unavailability admissions last. It is asserted on the rendered
// sections and on the spoken text, because they are two renderings that must
// not be allowed to drift.
func TestTheBriefingIsOrderedForTheListener(t *testing.T) {
	h := newHarness(t, SourceActivity, SourceSessions, SourceReminders, SourceFocus)
	h.set1(SourceActivity, Line{Category: Housekeeping, Text: "Two schedules ran."})
	h.set1(SourceSessions,
		Line{Category: InProgress, Text: "One session is still working."},
		Line{Category: Awaiting, Text: "One session is waiting on you."},
		Line{Category: Completed, Text: "One session has finished."})
	h.errs[SourceReminders] = errors.New("unreadable")
	h.set1(SourceFocus, Line{Category: Awaiting, Text: "A timebox wants an answer."})
	h.seen()
	h.away(9 * time.Hour)

	view, err := h.svc.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantTitles := []string{"Waiting for you", "Finished", "Still going", "Housekeeping", "I couldn't check"}
	if len(view.Sections) != len(wantTitles) {
		t.Fatalf("sections = %+v", view.Sections)
	}
	for i, want := range wantTitles {
		if view.Sections[i].Title != want {
			t.Errorf("section %d = %q, want %q", i, view.Sections[i].Title, want)
		}
	}
	// Within a category, the source declaration order decides: the sessions
	// source is declared before focus, so its awaiting line comes first.
	if got := view.Sections[0].Lines; len(got) != 2 ||
		got[0] != "One session is waiting on you." || got[1] != "A timebox wants an answer." {
		t.Errorf("awaiting section = %v", got)
	}
	spoken := view.Spoken
	if !inOrder(spoken, "waiting on you", "has finished", "still working", "Two schedules ran", "I couldn't check") {
		t.Errorf("the spoken order does not match the sections: %q", spoken)
	}
}

// TestOneSourceGetsOneLinePerCategory. A source with two opinions about the
// same category is telling the listener the same thing twice, and the speech
// budget is not big enough for that.
func TestOneSourceGetsOneLinePerCategory(t *testing.T) {
	h := newHarness(t, SourceSessions)
	h.set1(SourceSessions,
		Line{Category: Awaiting, Text: "First."},
		Line{Category: Awaiting, Text: "Second."},
		Line{Category: Completed, Text: "Third."})
	h.seen()
	h.away(9 * time.Hour)
	view, err := h.svc.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Sections[0].Lines) != 1 || view.Sections[0].Lines[0] != "First." {
		t.Errorf("awaiting section = %v; a source may say one thing per category", view.Sections[0].Lines)
	}
	if strings.Contains(view.Spoken, "Second.") {
		t.Errorf("the second line of one category was spoken: %q", view.Spoken)
	}
}

// ------------------------------------------------------------ the length

// TestTheSpokenBriefingFitsTheSpeechBudget. The full version keeps
// everything; the spoken one stops and says so.
func TestTheSpokenBriefingFitsTheSpeechBudget(t *testing.T) {
	h := newHarness(t, SourceSessions, SourceReminders, SourceFocus, SourceActivity, SourceConversations)
	long := "This line is deliberately long enough that a handful of them will " +
		"overrun the spoken budget for one briefing and force the trim."
	h.set1(SourceSessions,
		Line{Category: Awaiting, Text: long},
		Line{Category: Completed, Text: long},
		Line{Category: InProgress, Text: long})
	h.set1(SourceReminders, Line{Category: Awaiting, Text: long})
	h.set1(SourceFocus, Line{Category: Housekeeping, Text: long})
	h.set1(SourceActivity, Line{Category: Housekeeping, Text: long})
	h.set1(SourceConversations, Line{Category: Housekeeping, Text: long})
	h.seen()
	h.away(9 * time.Hour)

	view, err := h.svc.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !view.Truncated {
		t.Fatal("seven long lines did not trigger the trim")
	}
	if got := words(view.Spoken); got > maxSpokenWords {
		t.Errorf("spoken briefing is %d words, over the %d-word budget", got, maxSpokenWords)
	}
	if !strings.HasSuffix(view.Spoken, windowPointer) {
		t.Errorf("a trimmed briefing did not point at the window: %q", view.Spoken)
	}
	if lineTotal(view) != 7 {
		t.Errorf("the window's version lost lines too: %d", lineTotal(view))
	}
}

// TestAFittingBriefingIsNeverTrimmedToAnnounceATrim is the two-pass rule: the
// pointer costs words only when it is going to be spoken.
func TestAFittingBriefingIsNeverTrimmedToAnnounceATrim(t *testing.T) {
	h := newHarness(t, SourceSessions)
	// Sized to fill the budget EXACTLY once the headline is counted, so a
	// pointer reserved unconditionally would push it over and trim a line
	// that fits. That is the whole difference the second pass buys.
	var one lineCounts
	one.byCategory[Completed], one.substantive = 1, 1
	filler := strings.TrimSpace(strings.Repeat("word ",
		maxSpokenWords-words(plainHeadline("nine hours ago", one))))
	h.set1(SourceSessions, Line{Category: Completed, Text: filler})
	h.seen()
	h.away(9 * time.Hour)
	view, err := h.svc.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if view.Truncated {
		t.Errorf("a briefing that fits was trimmed: %d words", words(view.Spoken))
	}
	if strings.Contains(view.Spoken, windowPointer) {
		t.Errorf("the pointer was spoken for a briefing that fits: %q", view.Spoken)
	}
}

// TestAnUnavailabilityIsNeverTrimmedAway. The trim takes the tail, and the
// admission lives in the tail — so the admission is exempt, or a shortened
// briefing would quietly become a dishonest one.
func TestAnUnavailabilityIsNeverTrimmedAway(t *testing.T) {
	h := newHarness(t, SourceSessions, SourceFocus, SourceActivity, SourceReminders)
	long := "A line long enough to make the spoken budget bite well before the " +
		"end of the list of things this briefing has to get through today."
	h.set1(SourceSessions,
		Line{Category: Awaiting, Text: long},
		Line{Category: Completed, Text: long},
		Line{Category: InProgress, Text: long})
	h.set1(SourceFocus, Line{Category: Housekeeping, Text: long})
	h.set1(SourceActivity, Line{Category: Housekeeping, Text: long})
	h.errs[SourceReminders] = errors.New("unreadable")
	h.seen()
	h.away(9 * time.Hour)

	view, err := h.svc.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !view.Truncated {
		t.Fatal("the fixture did not trigger the trim")
	}
	if !strings.Contains(view.Spoken, "I couldn't check your reminders") {
		t.Errorf("the trim dropped the unavailability admission: %q", view.Spoken)
	}
}

// --------------------------------------------------------------- the model

// TestNoInventedCompletions is the pinned fixture the ticket asks for. The
// facts contain nothing that finished; the provider claims two things did.
// The sentence is refused, the plain reading is spoken, and the invention
// reaches neither the speech nor the window.
func TestNoInventedCompletions(t *testing.T) {
	h := newHarness(t, SourceSessions)
	h.withModel()
	h.set1(SourceSessions, Line{Category: InProgress, Text: "The session on the ci refactor is still working."})
	h.reply = "Two of your sessions finished overnight and the deploy went through."
	h.seen()
	h.away(9 * time.Hour)

	view, err := h.svc.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if view.ModelOutcome != "refused" {
		t.Errorf("ModelOutcome = %q, want refused", view.ModelOutcome)
	}
	for _, invented := range []string{"finished", "deploy", "Two of your sessions"} {
		if strings.Contains(view.Spoken, invented) {
			t.Errorf("the invented claim %q was spoken: %q", invented, view.Spoken)
		}
	}
	if !strings.Contains(view.Spoken, "still working") {
		t.Errorf("the real fact did not survive the refusal: %q", view.Spoken)
	}
	// The model WAS consulted — the refusal is the contract working, not the
	// provider being skipped, and a test that passed either way would prove
	// nothing.
	if n := len(h.prompted()); n != 1 {
		t.Errorf("the model was consulted %d times, want once", n)
	}
}

// TestInventedCountsAreRefused. Every number in the headline has to be a
// number of things that are actually there.
func TestInventedCountsAreRefused(t *testing.T) {
	h := newHarness(t, SourceSessions)
	h.withModel()
	h.set1(SourceSessions,
		Line{Category: Completed, Text: "The session on the ci refactor has finished."})
	h.reply = "Four sessions finished while you were away."
	h.seen()
	h.away(9 * time.Hour)
	view, err := h.svc.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if view.ModelOutcome != "refused" || strings.Contains(view.Spoken, "Four") {
		t.Errorf("an invented count survived: outcome %q, spoken %q", view.ModelOutcome, view.Spoken)
	}
}

// TestAnHonestHeadlineIsSpoken. The guard must not be so eager that a true
// sentence never gets through — a contract that refuses everything is a
// contract that has quietly removed the feature.
func TestAnHonestHeadlineIsSpoken(t *testing.T) {
	h := newHarness(t, SourceSessions)
	h.withModel()
	h.set1(SourceSessions,
		Line{Category: Completed, Text: "The session on the ci refactor has finished."},
		Line{Category: Awaiting, Text: "The session on the docs pass is waiting on you."})
	h.reply = "One session finished and one is waiting on you."
	h.seen()
	h.away(9 * time.Hour)
	view, err := h.svc.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if view.ModelOutcome != "used" {
		t.Fatalf("an honest headline was refused: %q (%q)", view.Headline, view.ModelOutcome)
	}
	if !strings.HasPrefix(view.Spoken, "One session finished and one is waiting on you.") {
		t.Errorf("spoken = %q", view.Spoken)
	}
}

// TestAModelFailureReadsTheFactsPlainly. The recap chain's honest fallback,
// restated: a briefing that cannot be worded is still a briefing.
func TestAModelFailureReadsTheFactsPlainly(t *testing.T) {
	h := newHarness(t, SourceSessions)
	h.withModel()
	h.set1(SourceSessions, Line{Category: Completed, Text: "The session on the ci refactor has finished."})
	h.replyErr = errors.New("the provider is unreachable")
	h.seen()
	h.away(9 * time.Hour)
	view, err := h.svc.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if view.ModelOutcome != "refused" {
		t.Errorf("ModelOutcome = %q", view.ModelOutcome)
	}
	if !strings.HasPrefix(view.Spoken, "Since you were last here nine hours ago: one finished.") {
		t.Errorf("spoken = %q, want the plain reading", view.Spoken)
	}
	if !strings.Contains(view.Spoken, "The session on the ci refactor has finished.") {
		t.Errorf("the facts were lost with the wording: %q", view.Spoken)
	}
}

// TestNoProviderNeverReachesForOne.
func TestNoProviderNeverReachesForOne(t *testing.T) {
	h := newHarness(t, SourceSessions)
	h.set1(SourceSessions, Line{Category: Completed, Text: "A session finished."})
	h.seen()
	h.away(9 * time.Hour)
	view, err := h.svc.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if view.ModelOutcome != "off" {
		t.Errorf("ModelOutcome = %q, want off", view.ModelOutcome)
	}
	if !strings.Contains(view.Spoken, "A session finished.") {
		t.Errorf("spoken = %q", view.Spoken)
	}
}

// ---------------------------------------------------------------- the config

// TestDisabledPreparesNothing. briefing.enabled = false has to mean nothing
// is prepared, offered, or scheduled — so no source is read at all, on any
// path.
func TestDisabledPreparesNothing(t *testing.T) {
	h := newHarness(t, SourceSessions)
	h.set1(SourceSessions, Line{Category: Awaiting, Text: "Something wants you."})
	h.mu.Lock()
	h.set = Settings{Enabled: false, AfterHours: 8}
	h.mu.Unlock()
	h.seen()
	h.away(9 * time.Hour)

	if got := offerOf(h.svc); got != "" {
		t.Errorf("a disabled briefing offered itself: %q", got)
	}
	spoken, err := h.svc.Briefing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if spoken != "Return briefings are switched off." {
		t.Errorf("ask while disabled = %q", spoken)
	}
	view, err := h.svc.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !view.Disabled {
		t.Error("the window was not told the feature is off")
	}
	if n := h.readCount(SourceSessions); n != 0 {
		t.Errorf("a disabled briefing read its sources %d times", n)
	}
	if len(h.published()) != 0 {
		t.Errorf("a disabled briefing published events: %v", h.published())
	}
}

// TestSpeakOnReturnSpeaksInsteadOfOffering, and still says nothing when
// nothing happened: the switch changes what a return sounds like, never
// whether an empty night is worth interrupting for.
func TestSpeakOnReturnSpeaksInsteadOfOffering(t *testing.T) {
	h := newHarness(t, SourceSessions)
	h.mu.Lock()
	h.set = Settings{Enabled: true, AfterHours: 8, SpeakOnReturn: true}
	h.mu.Unlock()
	h.set1(SourceSessions, Line{Category: Completed, Text: "The session on the ci refactor has finished."})
	h.seen()
	h.away(9 * time.Hour)

	got, transient := h.svc.OfferLine(context.Background())
	if got == offerSentence || !strings.Contains(got, "The session on the ci refactor has finished.") {
		t.Errorf("speak_on_return returned %q, want the briefing itself", got)
	}
	if !transient {
		// The whole account is transient like a recap: spoken, never
		// recorded. The engine reads this flag to keep it out of the
		// conversation, so a briefing that reported itself as part of the
		// answer would quietly become conversation memory.
		t.Error("a spoken-on-return briefing did not report itself as transient")
	}

	h2 := newHarness(t, SourceSessions)
	h2.mu.Lock()
	h2.set = Settings{Enabled: true, AfterHours: 8, SpeakOnReturn: true}
	h2.mu.Unlock()
	h2.seen()
	h2.away(9 * time.Hour)
	if got := offerOf(h2.svc); got != "" {
		t.Errorf("speak_on_return greeted an empty night with %q", got)
	}
}

// TestAfterHoursIsReadLive. The setting is live class, and live means the
// value in force when the arrival happens — not the one at construction.
func TestAfterHoursIsReadLive(t *testing.T) {
	h := newHarness(t, SourceSessions)
	h.set1(SourceSessions, Line{Category: Completed, Text: "A session finished."})
	h.seen()
	h.mu.Lock()
	h.set.AfterHours = 2
	h.mu.Unlock()
	h.away(3 * time.Hour)
	if got := offerOf(h.svc); got != offerSentence {
		t.Errorf("a live threshold change did not land: %q", got)
	}
}

// -------------------------------------------------------------- the record

// TestBriefingContentNeverReachesTheEvent is the leak-salted criterion. The
// composed account exists in the spoken sentence and nowhere else: the event
// the activity row is built from carries counts, outcomes, and source names.
func TestBriefingContentNeverReachesTheEvent(t *testing.T) {
	const salt = "SECRET-BRIEFING-MARKER"
	h := newHarness(t, SourceSessions, SourceReminders)
	h.withModel()
	h.set1(SourceSessions, Line{Category: Completed, Text: "The session on " + salt + " has finished."})
	h.errs[SourceReminders] = errors.New(salt + " is unreadable")
	h.reply = "UNIQUE-HEADLINE-" + salt + ": one thing finished."
	h.seen()
	h.away(9 * time.Hour)

	if _, err := h.svc.Briefing(context.Background()); err != nil {
		t.Fatal(err)
	}
	events := h.published()
	if len(events) != 1 || events[0].name != "briefing.given" {
		t.Fatalf("events = %+v", events)
	}
	for key, value := range events[0].data {
		if s, ok := value.(string); ok && strings.Contains(s, salt) {
			t.Errorf("briefing.given %s carries content: %q", key, s)
		}
	}
	// And the counts that stand in for the content are actually there —
	// a record that says nothing at all is not privacy, it is a hole.
	for _, key := range []string{"reason", "lines", "sections", "truncated", "empty", "model", "away"} {
		if _, ok := events[0].data[key]; !ok {
			t.Errorf("briefing.given is missing %q: %v", key, events[0].data)
		}
	}
	if events[0].data["unavailable"] != SourceReminders {
		t.Errorf("unavailable = %v, want the source name", events[0].data["unavailable"])
	}
}

// TestEveryPathPublishesExactlyOneRow: the three ways a briefing can happen
// each leave one row, named by who asked.
func TestEveryPathPublishesExactlyOneRow(t *testing.T) {
	h := newHarness(t, SourceSessions)
	h.mu.Lock()
	h.set = Settings{Enabled: true, AfterHours: 8, SpeakOnReturn: true}
	h.mu.Unlock()
	h.set1(SourceSessions, Line{Category: Completed, Text: "A session finished."})
	h.seen()
	h.away(9 * time.Hour)

	offerOf(h.svc)
	if _, err := h.svc.Briefing(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.View(context.Background()); err != nil {
		t.Fatal(err)
	}
	var reasons []string
	for _, ev := range h.published() {
		reasons = append(reasons, fmt.Sprint(ev.data["reason"]))
	}
	want := []string{"return", "ask", "window"}
	if strings.Join(reasons, ",") != strings.Join(want, ",") {
		t.Errorf("reasons = %v, want %v", reasons, want)
	}
}

// TestTheOfferCheckSpendsNoModelCall. The offer is appended to an answer the
// user is still hearing, so it must not be able to add a provider round-trip
// to it.
func TestTheOfferCheckSpendsNoModelCall(t *testing.T) {
	h := newHarness(t, SourceSessions)
	h.withModel()
	h.reply = "should never be asked for"
	h.set1(SourceSessions, Line{Category: Completed, Text: "A session finished."})
	h.seen()
	h.away(9 * time.Hour)
	if got := offerOf(h.svc); got != offerSentence {
		t.Fatalf("offer = %q", got)
	}
	if n := len(h.prompted()); n != 0 {
		t.Errorf("the offer check made %d model calls", n)
	}
	if len(h.published()) != 0 {
		t.Errorf("the offer check published a briefing row: %v", h.published())
	}
}

// TestASlowSourceIsNamedRatherThanWaitedOut drives the budget down rather
// than sleeping: the source parks on the caller's context and the deadline
// releases it.
func TestASlowSourceIsNamedRatherThanWaitedOut(t *testing.T) {
	h := &harness{
		now:   fixedNow,
		set:   Settings{Enabled: true, AfterHours: 8},
		lines: map[string][]Line{},
		errs:  map[string]error{},
		reads: map[string]int{},
		block: true,
	}
	h.svc = NewService(Options{
		Now:      func() time.Time { return h.clock() },
		Settings: func() Settings { return h.settings() },
		Budget:   time.Millisecond,
		Sources:  []Source{{Name: SourceSessions, Read: h.reader(SourceSessions)}},
	}, slog.New(slog.DiscardHandler))

	h.seen()
	h.away(9 * time.Hour)
	spoken, err := h.svc.Briefing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(spoken, "I couldn't check the AI sessions") {
		t.Errorf("a source that ran out of time was not named: %q", spoken)
	}
}

// offerOf is OfferLine's line alone, for the tests that only care what was
// said. The transient half — spoken but not recorded — is asserted where it
// is the subject, in TestSpeakOnReturnSpeaksInsteadOfOffering and in the
// engine's own tests.
func offerOf(svc *Service) string {
	line, _ := svc.OfferLine(context.Background())
	return line
}

// inOrder reports whether every needle appears in text, in order.
func inOrder(text string, needles ...string) bool {
	at := 0
	for _, needle := range needles {
		idx := strings.Index(text[at:], needle)
		if idx < 0 {
			return false
		}
		at += idx + len(needle)
	}
	return true
}
