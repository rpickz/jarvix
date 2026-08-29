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
	return buildHarness(t, fixedNow, nil, sources...)
}

// newSeededHarness builds the state a restart leaves behind: a watermark that
// came from the conversation archive rather than from an arrival this process
// witnessed, and a clock wherever the test needs it. It is the only way to
// reach the state #188 was reported in, because that state contains no
// arrival at all — which was exactly the bug.
func newSeededHarness(t *testing.T, seed, now time.Time, sources ...string) *harness {
	t.Helper()
	return buildHarness(t, now, func() (time.Time, bool) { return seed, true }, sources...)
}

func buildHarness(t *testing.T, now time.Time, seed func() (time.Time, bool), sources ...string) *harness {
	t.Helper()
	h := &harness{
		now:   now,
		set:   Settings{Enabled: true, AfterHours: 8},
		lines: map[string][]Line{},
		errs:  map[string]error{},
		reads: map[string]int{},
	}
	opts := Options{
		Now:      func() time.Time { return h.clock() },
		Seed:     seed,
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

// restartedAt binds the seam the up-front coverage caveat is read through:
// this process began serving at started, so a window that opens before that
// cannot be fully accounted for by the in-memory activity ring. Bound after
// construction, the daemon's own late-bind path.
func (h *harness) restartedAt(started time.Time) {
	h.svc.BindStartedAfter(func(since time.Time) bool { return started.After(since) })
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

// pass moves the clock with nobody here: no arrival, no sighting, just time
// going by. That is what an absence actually is, and the harness needs its own
// verb for it because `away` conflates the time passing with the user coming
// back — which is the conflation #188 was about.
func (h *harness) pass(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.now = h.now.Add(d)
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

// ------------------------------------------- an absence nobody witnessed

// The reported case (#188), in the numbers it was reported in. Taken off the
// user's machine on the morning of the 29th:
//
//   - last user exchange 2026-08-28 16:16:56 — the archive's newest
//     LastActive, and so the value the seed puts in the watermark;
//   - daemon started 20:51:12 that evening and still running;
//   - sessions started since that start-up: zero;
//   - the button pressed at 07:57 the next morning, with after_hours at 8.
//
// Fifteen hours and forty minutes, and briefing.get answered no_absence. The
// zone is UTC here only because the arithmetic is a difference and a test that
// depends on the reader's zone is a worse test; the gap is the reported one to
// the second.
var (
	reportedLastSeen = time.Date(2026, 8, 28, 16, 16, 56, 0, time.UTC)
	reportedNow      = time.Date(2026, 8, 29, 7, 57, 0, 0, time.UTC)
)

// TestAnAbsenceNobodyWitnessedIsStillAnAbsence is the regression test for the
// report. Nothing has arrived since the daemon started, so under the old
// event-shaped model there was nothing stored and every reader said "you
// haven't been away long enough" — to a user who had been away all night. The
// daemon knew the whole time: the seed had put 16:16:56 in the watermark.
func TestAnAbsenceNobodyWitnessedIsStillAnAbsence(t *testing.T) {
	h := newSeededHarness(t, reportedLastSeen, reportedNow, SourceSessions)
	h.set1(SourceSessions,
		Line{Category: Awaiting, Text: "The session on the ci refactor is waiting on you."})

	// No Arrive anywhere in this test. That is the reproduction.
	view, err := h.svc.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if view.NoRecord {
		t.Fatalf("the window's button reported no absence %v after the last exchange: %+v",
			reportedNow.Sub(reportedLastSeen), view)
	}
	if !view.Since.Equal(reportedLastSeen) {
		t.Errorf("Since = %v, want the last exchange at %v", view.Since, reportedLastSeen)
	}
	if view.AwaySpoken == "" {
		t.Error("the absence was reported without saying how long it was")
	}
	if !strings.Contains(view.Spoken, "waiting on you") {
		t.Errorf("the briefing was not composed: %q", view.Spoken)
	}

	// The voice ask reaches the same absence through the same door. It arrives
	// first in production — the engine's own ordering — but the service must
	// not depend on that, which is the whole point.
	spoken, err := h.svc.Briefing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if spoken == NoRecordSentence {
		t.Errorf("the spoken ask still denied the absence: %q", spoken)
	}
}

// TestTheButtonAndTheVoiceAskAgreeAtEveryMoment. The two surfaces are the
// same question asked twice, and #188's bug was precisely that they disagreed:
// one of them happened to follow an arrival and the other could not. They are
// driven here through one state, in one order, and compared at every step.
//
// Since #190 the steps cover the SHORT gaps too — a moment, a minute, four
// hours — because those are no longer a refusal on either surface but a real
// window that both must read the same way. Agreement is asserted on the window
// itself rather than on a flag: the spoken form carries the interval its
// headline names, so a voice ask that composed over a different stretch from
// the button's could not contain the button's own spoken age.
func TestTheButtonAndTheVoiceAskAgreeAtEveryMoment(t *testing.T) {
	h := newSeededHarness(t, reportedLastSeen, reportedLastSeen, SourceSessions)
	h.set1(SourceSessions, Line{Category: Completed, Text: "A session finished."})

	steps := []struct {
		name string
		do   func()
		away string
	}{
		{"the moment the user left", func() {}, "just now"},
		{"a minute later", func() { h.pass(time.Minute) }, "a minute ago"},
		{"four hours later", func() { h.pass(4*time.Hour - time.Minute) }, "four hours ago"},
		{"exactly at the threshold", func() { h.pass(4 * time.Hour) }, "eight hours ago"},
		{"still nobody here, hours on", func() { h.pass(7 * time.Hour) }, "fifteen hours ago"},
		{"they come back", func() { h.seen() }, "fifteen hours ago"},
		{"and stay a while", func() { h.pass(time.Hour); h.seen() }, "sixteen hours ago"},
	}
	for _, step := range steps {
		step.do()
		view, err := h.svc.View(context.Background())
		if err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		spoken, err := h.svc.Briefing(context.Background())
		if err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if view.NoRecord || spoken == NoRecordSentence {
			t.Errorf("%s: an ask went unanswered — button %+v, voice %q", step.name, view, spoken)
		}
		if view.AwaySpoken != step.away {
			t.Errorf("%s: the button reported the window as %q, want %q",
				step.name, view.AwaySpoken, step.away)
		}
		if !strings.Contains(spoken, step.away) {
			t.Errorf("%s: the voice ask named a different window: %q", step.name, spoken)
		}
		if !strings.Contains(spoken, "A session finished.") ||
			!strings.Contains(view.Spoken, "A session finished.") {
			t.Errorf("%s: the sources were not read over the window: button %q, voice %q",
				step.name, view.Spoken, spoken)
		}
	}
	// And none of those fourteen reads spent the offer the one arrival armed
	// (#189's rule, on the path #190 widened): it is still owed, once.
	if got := offerOf(h.svc); got != offerSentence {
		t.Errorf("offer after the reads = %q, want the offer sentence", got)
	}
	if got := offerOf(h.svc); got != "" {
		t.Errorf("the same absence was offered twice: %q", got)
	}
}

// TestAnAbsenceIsReportedOncePerAbsenceHoweverItWasObserved. Reading an
// absence must not create a second one, and it must not spend or re-arm the
// offer: a read is a read. The absence the arrival stores is the same absence
// the earlier read described — same start — so the user is never told about
// one night twice, and the offer that rides the answer is still made exactly
// once.
func TestAnAbsenceIsReportedOncePerAbsenceHoweverItWasObserved(t *testing.T) {
	h := newSeededHarness(t, reportedLastSeen, reportedNow, SourceSessions)
	h.set1(SourceSessions, Line{Category: Completed, Text: "A session finished."})

	first, err := h.svc.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.NoRecord {
		t.Fatalf("the read did not find the absence: %+v", first)
	}

	// The user now speaks. The arrival ends the absence, keeps it askable, and
	// owes the one offer.
	h.seen()
	if got := offerOf(h.svc); got != offerSentence {
		t.Errorf("the arrival after a read made no offer: %q", got)
	}
	if got := offerOf(h.svc); got != "" {
		t.Errorf("the same absence was offered twice: %q", got)
	}

	// An hour of being here. The ended absence is still the subject, and it is
	// the SAME one — a read must not resurrect it as a fresh night.
	h.pass(time.Hour)
	h.seen()
	later, err := h.svc.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if later.NoRecord || !later.Since.Equal(first.Since) {
		t.Errorf("a later read reported %+v, want the same absence since %v",
			later.Since, first.Since)
	}
	if got := offerOf(h.svc); got != "" {
		t.Errorf("a read re-armed the offer: %q", got)
	}
}

// TestSpeakThenAskAndAskThenSpeakReachTheSameAbsence. The feature's behaviour
// used to depend on the order of the user's first two actions after a night:
// speaking first worked, asking first did not. Both orders are driven from one
// state and must land on the same absence and the same single offer.
func TestSpeakThenAskAndAskThenSpeakReachTheSameAbsence(t *testing.T) {
	line := Line{Category: Completed, Text: "A session finished."}

	speakThenAsk := newSeededHarness(t, reportedLastSeen, reportedNow, SourceSessions)
	speakThenAsk.set1(SourceSessions, line)
	speakThenAsk.seen()
	spokeFirst := offerOf(speakThenAsk.svc)
	viewA, err := speakThenAsk.svc.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	askThenSpeak := newSeededHarness(t, reportedLastSeen, reportedNow, SourceSessions)
	askThenSpeak.set1(SourceSessions, line)
	viewB, err := askThenSpeak.svc.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	askThenSpeak.seen()
	askedFirst := offerOf(askThenSpeak.svc)

	if viewA.NoRecord || viewB.NoRecord || !viewA.Since.Equal(viewB.Since) {
		t.Errorf("speak-then-ask saw %+v and ask-then-speak saw %+v", viewA.Since, viewB.Since)
	}
	if spokeFirst != offerSentence || askedFirst != offerSentence {
		t.Errorf("offers differed by order: speak-first %q, ask-first %q", spokeFirst, askedFirst)
	}
	for _, h := range []*harness{speakThenAsk, askThenSpeak} {
		if got := offerOf(h.svc); got != "" {
			t.Errorf("a second offer for the same absence: %q", got)
		}
	}
}

// TestASecondNightWithNobodyHereSupersedesTheFirst. Deriving must supersede
// the same way arriving does: after the user has been and gone again, the
// absence the button reports is the current one, not the one the last arrival
// happened to store.
func TestASecondNightWithNobodyHereSupersedesTheFirst(t *testing.T) {
	h := newSeededHarness(t, reportedLastSeen, reportedNow, SourceSessions)
	h.set1(SourceSessions, Line{Category: Completed, Text: "A session finished."})
	h.seen() // the arrival that ends and stores the first night
	back := h.clock()

	h.pass(9 * time.Hour)
	view, err := h.svc.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if view.NoRecord || !view.Since.Equal(back) {
		t.Errorf("Since = %v, want the second absence starting at %v", view.Since, back)
	}
}

// ------------------------------------------------------ asking always answers

// TestAskingAlwaysAnswersOverWhateverWindowThereIs is #190 in one table. The
// threshold decides when Jarvix VOLUNTEERS; it was never a rule about when an
// account exists, and consulting it here meant an explicit ask was refused
// without a single source being read — while the daemon sat on the routine
// that ran, the reminder that fired and the session that finished over lunch.
//
// Every row is the same question over a different gap, on both surfaces, and
// every row must read the sources and name the interval it read them over.
func TestAskingAlwaysAnswersOverWhateverWindowThereIs(t *testing.T) {
	line := Line{Category: Completed, Text: "The session on the ci refactor has finished."}
	for _, tc := range []struct {
		name    string
		gap     time.Duration
		content bool
		want    string
	}{
		{"a minute", time.Minute, true, "Since you were last here a minute ago:"},
		{"an hour, with something in it", 90 * time.Minute, true, "Since you were last here an hour ago:"},
		{"a short gap with nothing in it", 90 * time.Minute, false,
			"Nothing since you were last here, an hour ago."},
		{"a second short of the threshold", 8*time.Hour - time.Second, true,
			"Since you were last here seven hours ago:"},
		{"a night, unchanged", 9 * time.Hour, true, "Since you were last here nine hours ago:"},
		{"twice in a minute, with nobody having been anywhere", time.Second, false,
			"Nothing since you were last here, just now."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newSeededHarness(t, reportedLastSeen, reportedLastSeen.Add(tc.gap), SourceSessions)
			if tc.content {
				h.set1(SourceSessions, line)
			}
			view, err := h.svc.View(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if view.NoRecord {
				t.Fatalf("an ask over a %v gap was refused: %+v", tc.gap, view)
			}
			if !strings.HasPrefix(view.Headline, tc.want) {
				t.Errorf("headline = %q, want it to start %q", view.Headline, tc.want)
			}
			if n := h.readCount(SourceSessions); n != 1 {
				t.Errorf("the sources were read %d times, want once over the window", n)
			}
			if tc.content && !strings.Contains(view.Spoken, line.Text) {
				t.Errorf("the window's content was not reported: %q", view.Spoken)
			}
			// Both surfaces, one state, one answer — and asking twice inside
			// the same minute must cost the same and say the same, because a
			// read moves nothing.
			spoken, err := h.svc.Briefing(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if spoken != view.Spoken {
				t.Errorf("the voice said %q and the button said %q", spoken, view.Spoken)
			}
		})
	}
}

// TestAskingSpendsNoOfferHoweverShortTheGap. The ask path was widened, not the
// offer path: reading over a two-minute window must not arm an offer, must not
// consume the one a night armed, and must not move the watermark that would
// end the night (#189's rule, on the surface #190 opened up).
func TestAskingSpendsNoOfferHoweverShortTheGap(t *testing.T) {
	h := newSeededHarness(t, reportedLastSeen, reportedLastSeen.Add(2*time.Minute), SourceSessions)
	h.set1(SourceSessions, Line{Category: Completed, Text: "A session finished."})

	for range 3 {
		if _, err := h.svc.Briefing(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := h.svc.View(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := offerOf(h.svc); got != "" {
		t.Errorf("six asks over a two-minute window armed an offer: %q", got)
	}

	// The night that follows is still the whole night: the reads left the
	// watermark exactly where the seed put it.
	h.pass(9 * time.Hour)
	h.seen()
	if got := offerOf(h.svc); got != offerSentence {
		t.Errorf("offer after the night = %q, want the offer sentence", got)
	}
	view, err := h.svc.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !view.Since.Equal(reportedLastSeen) {
		t.Errorf("Since = %v, want the absence to start at the seed %v", view.Since, reportedLastSeen)
	}
}

// TestTheOfferStillWaitsForTheThreshold is the other half of the same change,
// and the one that would be easy to lose: an ask over lunch now answers, and
// lunch must still not make Jarvix volunteer anything. The threshold's whole
// job is the difference between speaking after a night and speaking after a
// sandwich.
func TestTheOfferStillWaitsForTheThreshold(t *testing.T) {
	h := newHarness(t, SourceSessions)
	h.set1(SourceSessions, Line{Category: Completed, Text: "A session finished."})
	h.seen()
	h.away(90 * time.Minute)

	if got := offerOf(h.svc); got != "" {
		t.Errorf("a ninety-minute gap volunteered %q", got)
	}
	// And the same state, asked rather than offered, reports the lunch. The
	// arrival has already moved the watermark by the time this runs — the
	// engine's own ordering — so this is also the case that would report an
	// empty two microseconds if the ask read lastSeen alone.
	spoken, err := h.svc.Briefing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(spoken, "A session finished.") {
		t.Errorf("the ask after lunch = %q, want the lunch reported", spoken)
	}
	if !strings.Contains(spoken, "an hour ago") {
		t.Errorf("the ask after lunch did not name the interval: %q", spoken)
	}
}

// TestAStretchNobodyClosedBeatsTheOneAnArrivalDid. The plain window has two
// candidates and the longer wins (see window). Hours after an arrival, with
// nobody here, the open stretch is the news — reporting the one the arrival
// closed instead would say "since you were last here, eight hours ago" to
// someone who was demonstrably here five hours ago.
func TestAStretchNobodyClosedBeatsTheOneAnArrivalDid(t *testing.T) {
	h := newHarness(t, SourceSessions)
	h.set1(SourceSessions, Line{Category: Completed, Text: "A session finished."})
	h.seen()              // 09:00
	h.away(3 * time.Hour) // 12:00: a three-hour stretch, closed by an arrival
	h.pass(5 * time.Hour) // 17:00: five hours with nobody here, still open
	back := h.clock().Add(-5 * time.Hour)

	view, err := h.svc.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !view.Since.Equal(back) {
		t.Errorf("Since = %v, want the open five-hour stretch from %v", view.Since, back)
	}
	if view.AwaySpoken != "five hours ago" {
		t.Errorf("AwaySpoken = %q, want the open stretch", view.AwaySpoken)
	}
}

// TestAnUnknownWatermarkInventsNoWindow. With no archive to seed from and
// nobody ever here, there is nothing to measure against — and the derivation
// must not turn "I don't know" into "you've been away for ages". A fresh
// install must not greet its first user with a briefing about a machine that
// was not running, however long ago its clock thinks the epoch was.
//
// This is the ONLY state left in which an ask does not compose (#190). It is a
// claim about the daemon's own records rather than about the length of a gap,
// and the sentence says so.
func TestAnUnknownWatermarkInventsNoWindow(t *testing.T) {
	h := newHarness(t, SourceSessions)
	h.set1(SourceSessions, Line{Category: Completed, Text: "A session finished."})
	h.pass(30 * 24 * time.Hour)

	view, err := h.svc.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !view.NoRecord {
		t.Errorf("an absence was invented from an unknown watermark: %+v", view)
	}
	spoken, err := h.svc.Briefing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if spoken != NoRecordSentence {
		t.Errorf("spoken = %q, want the no-record sentence", spoken)
	}
	if n := h.readCount(SourceSessions); n != 0 {
		t.Errorf("sources were read %d times with no absence to report on", n)
	}
}

// TestReadingAnAbsenceNeverMovesTheWatermark. The derivation is arithmetic on
// state it does not own: only an arrival ends an absence. If a read moved
// lastSeen, asking about a night would erase it — and the second ask, or the
// user's own first word, would be told there had been no night.
func TestReadingAnAbsenceNeverMovesTheWatermark(t *testing.T) {
	h := newSeededHarness(t, reportedLastSeen, reportedNow, SourceSessions)
	h.set1(SourceSessions, Line{Category: Completed, Text: "A session finished."})

	for i := range 3 {
		view, err := h.svc.View(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if view.NoRecord || !view.Since.Equal(reportedLastSeen) {
			t.Fatalf("read %d = %+v; asking must not consume the absence", i, view.Since)
		}
	}
	// And the arrival that follows still measures the whole night, because the
	// reads left the watermark where the seed put it.
	h.seen()
	if got := offerOf(h.svc); got != offerSentence {
		t.Errorf("offer after three reads = %q, want the offer sentence", got)
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

// ------------------------------------------------ what the ring cannot cover

// TestAWindowThatPredatesStartUpSaysSoUpFront is the second honesty gap #190
// names. Four of the five sources are durable and answer for the whole window
// with complete confidence after a restart; the fifth is an in-memory ring
// that died with the previous process. A briefing whose WHOLE window predates
// the restart therefore composed a confident "nothing happened" out of the
// four that survived — and the one admission that existed was appended to the
// activity line, which in that very case has nothing to say and so is not
// there at all.
//
// So it is said up front, it names which half is lossy and which half is not,
// and it sits where the trim cannot reach it.
func TestAWindowThatPredatesStartUpSaysSoUpFront(t *testing.T) {
	h := newSeededHarness(t, reportedLastSeen, reportedNow, SourceActivity)
	// The daemon came up four hours into the night, so the whole first stretch
	// of the window is before anything this process could have observed.
	h.restartedAt(reportedLastSeen.Add(4 * time.Hour))

	view, err := h.svc.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if view.Caveat == "" {
		t.Fatalf("a window opening before start-up carried no caveat: %+v", view)
	}
	// Up front means second: after the headline that says what shape this is
	// in, and before the first fact.
	if !strings.HasPrefix(view.Spoken, view.Headline+" "+view.Caveat) {
		t.Errorf("the caveat is not up front: %q", view.Spoken)
	}
	// And it cannot be mistaken for "nothing happened", which is exactly what
	// the sentence it follows says.
	if !view.Empty || !strings.HasPrefix(view.Headline, "Nothing since you were last here") {
		t.Fatalf("this test no longer covers the mistakable case: %+v", view)
	}
	for _, want := range []string{"I restarted", "my own record of what I ran",
		"your reminders, focus threads, conversations and AI sessions", "complete"} {
		if !strings.Contains(view.Caveat, want) {
			t.Errorf("the caveat does not say %q: %q", want, view.Caveat)
		}
	}
	// The record says a briefing could not cover its own window, and says it
	// as an outcome rather than by carrying the sentence.
	events := h.published()
	if len(events) != 1 || events[0].data["partial"] != true {
		t.Errorf("the event did not report the shortfall: %+v", events)
	}
}

// TestAWindowInsideThisProcessCarriesNoCaveat. The admission is only worth
// making when it is true; made unconditionally it would be noise, and noise is
// what a listener learns to ignore.
func TestAWindowInsideThisProcessCarriesNoCaveat(t *testing.T) {
	h := newSeededHarness(t, reportedLastSeen, reportedNow, SourceActivity)
	h.restartedAt(reportedLastSeen.Add(-time.Hour))

	view, err := h.svc.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if view.Caveat != "" {
		t.Errorf("a fully covered window carried a caveat: %q", view.Caveat)
	}
	if events := h.published(); len(events) != 1 || events[0].data["partial"] != false {
		t.Errorf("the event reported a shortfall that was not there: %+v", events)
	}
}

// TestTheCaveatIsNeverTrimmedAway. The trim takes the tail, and an admission
// in the tail is an admission that disappears exactly when the briefing is
// busiest — which is the same reasoning that exempts the unavailability lines.
func TestTheCaveatIsNeverTrimmedAway(t *testing.T) {
	h := newSeededHarness(t, reportedLastSeen, reportedNow, SourceSessions, SourceReminders, SourceFocus)
	h.restartedAt(reportedNow.Add(-time.Hour))
	long := "This line is deliberately long enough that a handful of them will " +
		"overrun the spoken budget for one briefing and force the trim."
	h.set1(SourceSessions,
		Line{Category: Awaiting, Text: long},
		Line{Category: Completed, Text: long},
		Line{Category: InProgress, Text: long})
	h.set1(SourceReminders, Line{Category: Awaiting, Text: long})
	h.set1(SourceFocus, Line{Category: Housekeeping, Text: long})

	view, err := h.svc.View(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !view.Truncated {
		t.Fatal("five long lines did not trigger the trim")
	}
	if !strings.Contains(view.Spoken, view.Caveat) || view.Caveat == "" {
		t.Errorf("the trim dropped the caveat: %q", view.Spoken)
	}
	if got := words(view.Spoken); got > maxSpokenWords {
		t.Errorf("spoken briefing is %d words, over the %d-word budget", got, maxSpokenWords)
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
	if !strings.HasPrefix(spoken, "Nothing since you were last here, nine hours ago.") {
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
