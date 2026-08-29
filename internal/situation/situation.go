// Package situation composes the situation report (#196, ADR 0061): the one
// short, honest answer to "where are we?" — the question the manager of a
// machine asks most often and the one Jarvix could not answer.
//
// Nothing here observes anything. Every fact in a report is already held
// somewhere else for its own reasons — the AI-session classification on the
// focus threads (#137), the threads themselves (ADR 0041), the reminders (ADR
// 0046), the schedules (ADR 0032), the feeds (ADR 0031), Jarvix's own activity
// ring, the window inventory (ADR 0022). This package is composition, and the
// boundary ADR 0050 drew around the return briefing binds it identically: no
// keystroke record, no browsing history, no process inventory, not now and not
// as a small extension later. A report of everything the user did is a
// different product with a different consent conversation.
//
// Four stances shape the types below, and each is a decision rather than an
// implementation detail.
//
// **The report is about NOW.** That is the whole difference from the return
// briefing, which is about a stretch of time the user was not here. The
// briefing refuses to compose when it cannot measure a window ("I've no record
// of when you were last here"); a report that refused to say what is going on
// because it did not know when you last asked would be absurd. So a source is
// handed an Instant rather than a since/now pair, most sources ignore the
// backward-looking half of it entirely, and no reading of the clock can stop a
// report being given.
//
// **The ordering is a claim about the reader.** Needs-you, then in-progress,
// then finished-since-you-last-looked, then failing, then housekeeping. It is
// deliberately NOT the briefing's order, which puts what finished second: a
// person coming back from a night away wants to know what landed, and a person
// asking where things stand right now wants to know what is running. Same
// facts, different question, different order — and the order is pinned by a
// test because it is the feature.
//
// **Specifics, never categories.** Each Item is one thing, worded by the source
// that owns the fact, carrying a reference to the thing it is about so the
// window can link straight to it. "Two sessions are waiting on you" is a
// category; "Claude is waiting on you in the deploy thread" is the report.
//
// **A new source is an addition, not surgery.** The Source seam is a name and
// one function. Nothing in this package knows which sources exist, and the
// ordering is over ranks rather than over sources — so the jobs source of #195's
// next slice, and the remote-machine source of its last, drop in beside the rest
// without the composer, the ordering, or the speech budget changing. A test
// registers a stub source and proves it.
package situation

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/rpickz/jarvix/internal/provenance"
)

// Rank is where one fact sits in the report, and it is a claim about the
// reader rather than about the machine: what is stopped waiting for you comes
// before what is running without you, which comes before what landed while you
// were not looking, which comes before what broke, which comes before the
// housekeeping you would never have watched.
type Rank int

// The ranks, in the order they are always spoken and always rendered.
const (
	// NeedsYou is work that is stopped until the user does something. It
	// leads, and the AI-session classification (#137) is why the whole
	// feature is worth building: `needs_you` is the highest-value fact on
	// the machine and it is already computed.
	NeedsYou Rank = iota
	// InProgress is work running right now, without the user.
	InProgress
	// Finished is work that completed since the user last looked. It sits
	// below InProgress rather than above it — the briefing's order inverted —
	// because this report answers "where are we?" and the answer to that is
	// what is happening, not what has stopped happening.
	Finished
	// Failing is what is broken. Below Finished because a failure that has
	// been sitting there for an hour is not more urgent than a session that
	// wants an answer now, and above Housekeeping because it is news.
	Failing
	// Housekeeping is the shape of the machine — the desktop, the schedules,
	// the thread you are on. Real, and last.
	Housekeeping
	// Unavailable is a source that could not be read. It is a rank, not an
	// omission, because a report that quietly drops a source it failed to read
	// lies by shape: the listener hears "nothing is failing" when the truth is
	// "I could not look" (ADR 0050's discipline, held verbatim).
	Unavailable
)

// rankTitles are the section headings the window renders and the spoken form
// implies. Worded here, daemon-side, like every other Jarvix surface (ADR
// 0013): the QML tab renders these strings and composes none of its own.
var rankTitles = [...]string{
	NeedsYou:     "Needs you",
	InProgress:   "In progress",
	Finished:     "Finished since you last looked",
	Failing:      "Failing",
	Housekeeping: "Housekeeping",
	Unavailable:  "I couldn't check",
}

// Title is the section heading for a rank.
func (r Rank) Title() string {
	if r < 0 || int(r) >= len(rankTitles) {
		return ""
	}
	return rankTitles[r]
}

// ordered is every rank in speaking order — the one place the ordering is
// written down, so the spoken form, the window's sections and the tests cannot
// drift apart. TestTheOrderingIsPinned reads exactly this.
var ordered = [...]Rank{NeedsYou, InProgress, Finished, Failing, Housekeeping, Unavailable}

// Ordered is the speaking order, for tests and for anything that has to walk
// the ranks without guessing at their number.
func Ordered() []Rank { return ordered[:] }

// Item is one fact about the machine, already worded.
//
// Sources compose their own sentences because they are the only code that
// knows what the fact means; nothing downstream — not the model, not the
// window — invents wording from data.
type Item struct {
	Rank Rank
	// Source is the stable identifier of the source that produced this. It is
	// stamped by the reader rather than trusted from the source, is carried
	// for the event and the unavailability wording, and is never spoken.
	Source string
	Text   string
	// Where points at the thing this line is about, or nil when the line is
	// about no single thing — the shape of the desktop, a count of failures
	// the ring no longer holds the detail of.
	//
	// It is a provenance reference (#168, ADR 0055) rather than a navigation
	// scheme of this feature's own, so the window resolves and follows a
	// situation line with exactly the code that resolves and follows "what
	// went into this answer": one resolver, one liveness answer, one set of
	// buttons. The strength is always Returned — the source read the live
	// store and the line says what it read, which is mechanically causal in
	// the sense ADR 0055 reserves that word for.
	Where *provenance.Reference
}

// Instant is what a source is asked about. It is a struct rather than two time
// arguments for the reason the whole seam exists: a jobs source or a
// remote-machine source that needs to be told something more about the moment
// gets a new field here, and every source already written keeps compiling.
type Instant struct {
	// Now is the moment the report is about. Every source in one report is
	// handed the same Now, so a report cannot contradict itself between its
	// second line and its fifth.
	Now time.Time
	// Since is when the user last looked — the last report that was actually
	// composed, seeded at construction from the durable record of when they
	// were last here so that a restart does not erase it.
	//
	// It is ZERO when nobody has ever looked and there is no durable record to
	// seed from. A source whose news is interval-shaped ("this fired while you
	// were not watching") must then report NOTHING rather than reporting all
	// of its history: a fresh daemon reading out every reminder that ever
	// fired would be answering a question about now with an archive.
	//
	// Most sources ignore this field entirely. That is the point of the report
	// being about now: `needs_you`, `working`, a failing feed and an open
	// window are all current state, and only "finished since you last looked"
	// has a backward edge at all.
	Since time.Time
}

// Source is one corner of the machine. It is a name and one function, and that
// is deliberately the whole of it: the contract the next slice of #195 has to
// meet is small enough to read in one line.
//
// Read is called with a bounded context, concurrently with every other source.
// It must not block on anything it cannot cancel, and it must not write.
//
// An error is not a failure of the report: it becomes an Unavailable item
// naming this source, which is the honest outcome and the tested one.
type Source struct {
	Name string
	Read func(ctx context.Context, at Instant) ([]Item, error)
}

// The source identifiers the daemon's shipped adapters use. They are the event
// vocabulary and the argument to sourceNoun — declared here so the adapters and
// this package's wording cannot drift.
//
// A source added later does not have to appear in this list. It only has to
// have a name; sourceNoun falls back to the name itself, which reads acceptably
// for anything sensibly named and is better than a report that cannot say which
// source it failed to read.
const (
	SourceSessions  = "sessions"
	SourceFocus     = "focus"
	SourceReminders = "reminders"
	SourceSchedules = "schedules"
	SourceActivity  = "activity"
	SourceWindows   = "windows"
	// SourceJobs is the work that outlives a conversation (#200, ADR 0065).
	// It arrived exactly as this package's doc comment predicted it would: one
	// more entry in this list, one more Source in the daemon's bind, and not a
	// line of the composer, the ordering or the speech budget changed.
	SourceJobs = "jobs"
)

// Budget and cache bounds. Both are wall-clock ceilings on one user-visible
// moment and both are pinned rather than configurable, for the recap's reason
// (ADR 0043): they trade against nothing the user has an opinion about, and a
// knob on either would be a knob that can be turned into a bad report.
const (
	// DefaultBudget bounds one whole report: every source read, run in
	// parallel, plus the one model call that words the headline.
	DefaultBudget = 5 * time.Second
	// DefaultCacheFor is how long a composed report answers for. See the
	// caching rule on Service.
	DefaultCacheFor = 30 * time.Second
)

// Options configures a Service. Every seam that touches the world — the clock,
// the sources, the provider, the event bus — arrives here as a function, which
// is what makes the tests hermetic and why this package imports neither the
// daemon nor a provider.
type Options struct {
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	// Seed reports when the user last looked at the machine, before this
	// process started. It is called exactly once, during construction, off
	// every hot path. false means "no idea", which reads as "never looked" —
	// the conservative direction, because the alternative is a first report
	// that reads out history as news.
	Seed func() (time.Time, bool)
	// Sources are read in parallel; their items are ordered by rank first and
	// then by this declaration order, so the AI sessions lead the ranks they
	// share with anything else.
	Sources []Source
	// Summarise words the headline through the provider. Nil means the
	// deterministic headline is used and no model is ever consulted — which is
	// also what happens when the call fails or its answer is refused.
	Summarise func(ctx context.Context, prompt string) (string, error)
	// StartedAfter reports whether this process began after the given moment,
	// which is how a report knows that its own activity record cannot account
	// for the whole stretch it is reporting on. Nil means "no idea", which
	// reads as full coverage: an admission is only worth making when it is
	// demonstrably true.
	StartedAfter func(since time.Time) bool
	// Budget bounds one composition; CacheFor is how long the result answers
	// for. Zero means the defaults above.
	Budget   time.Duration
	CacheFor time.Duration
	// Publish emits the report's one event. It carries counts, outcomes and
	// source names — never a word of the report itself.
	Publish func(event string, data map[string]any)
}

// Service composes situation reports and remembers exactly two things: when the
// user last looked, and the last report it composed.
//
// **The caching rule**, which the ticket asks to be written down rather than
// merely implemented:
//
//	One report is composed at most once per CacheFor. Every ask inside that
//	window — voice, tool, or window — replays the same composition, at no
//	source read and no model call. The replay carries the moment it was
//	composed and says how old it is, so nothing pretends to be fresher than it
//	is, and the window's Refresh button forces a new composition for anyone
//	who doubts it.
//
// Thirty seconds is chosen against what the report can HONESTLY SAY rather than
// against a cost target, and the argument is worth stating because it is what
// makes the rule defensible rather than merely convenient.
//
// The shared spoken age scale (knowledge.SpokenAge, ADR 0013) bottoms out at
// "just now" for anything under a minute. So a report inside the cache window
// is not merely close enough to fresh — it is a report whose age Jarvix has no
// word to distinguish from a fresh one. Handing back the held composition and
// handing back a new one produce the same sentence about when it was read,
// which means the replay cannot mislead a listener in any vocabulary this
// daemon owns. Past the window that stops being true, so the cache expires.
//
// The other half is that "what's going on" asked twice in a row — because the
// speaker was interrupted, or the answer was missed — is a real and frequent
// thing, and it must not cost two compositor reads and two model calls.
//
// The cache is time-based only. Nothing invalidates it early, deliberately: an
// invalidation hook would be a second, quieter definition of "the machine
// changed", and the honest bound on staleness is the one the reader can see.
//
// It is not a store. Nothing here is written to disk, and a restart starts
// again from the seed.
type Service struct {
	now func() time.Time
	// sources, summarise and startedAfter are late-bindable and therefore
	// guarded by mu, like everything else the daemon completes after
	// construction.
	sources      []Source
	summarise    func(ctx context.Context, prompt string) (string, error)
	startedAfter func(since time.Time) bool

	budget   time.Duration
	cacheFor time.Duration
	publish  func(event string, data map[string]any)
	log      *slog.Logger

	mu sync.Mutex
	// lastLooked is when the user last had a report composed for them, seeded
	// from the durable record of when they were last here. It is the whole of
	// what "since you last looked" means, and it moves only on a real
	// composition — a replay from the cache is not a new look.
	lastLooked time.Time
	// cached is the last composed report and when it was composed. Zero at
	// cachedAt means nothing is held.
	cached   Report
	cachedAt time.Time
	// composing serialises compositions so that two asks arriving together
	// cost one read rather than two. It is a separate lock from mu because a
	// composition runs source reads and a model call, and mu is taken on every
	// cheap read of the watermark.
	composing sync.Mutex
}

// NewService builds the situation service and seeds its watermark.
func NewService(opts Options, log *slog.Logger) *Service {
	s := &Service{
		now:          opts.Now,
		sources:      opts.Sources,
		summarise:    opts.Summarise,
		startedAfter: opts.StartedAfter,
		budget:       opts.Budget,
		cacheFor:     opts.CacheFor,
		publish:      opts.Publish,
		log:          log,
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.budget <= 0 {
		s.budget = DefaultBudget
	}
	if s.cacheFor <= 0 {
		s.cacheFor = DefaultCacheFor
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if opts.Seed != nil {
		if seeded, ok := opts.Seed(); ok {
			s.lastLooked = seeded
		}
	}
	return s
}

// BindSources late-binds the source list. The daemon needs it: every source
// reads state that only exists once the daemon itself does, and the service has
// to exist before the engine that carries it. Called once during construction,
// single-threaded, exactly like the briefing service's BindSources.
func (s *Service) BindSources(sources ...Source) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sources = sources
}

// BindSummarise late-binds the provider seam, for the same construction-order
// reason as BindSources.
func (s *Service) BindSummarise(summarise func(ctx context.Context, prompt string) (string, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.summarise = summarise
}

// BindStartedAfter late-binds the process's own start-up moment, for the same
// construction-order reason as BindSources: the daemon knows when it began
// serving and this package must not learn it from a clock of its own.
func (s *Service) BindStartedAfter(startedAfter func(since time.Time) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startedAfter = startedAfter
}

// Situation is the report for the ear: the deterministic phrases and the
// model's tool alike. It never returns an error for having nothing to say —
// "nothing needs you" is an answer, and a spoken apology would imply a fault
// where there was only a quiet machine.
func (s *Service) Situation(ctx context.Context) (string, error) {
	r, err := s.compose(ctx, "ask", false)
	if err != nil {
		return "", err
	}
	return r.Spoken, nil
}

// View is the window's full version: every item, in order, untruncated, each
// with the reference the window resolves into a link. fresh skips the cache,
// which is what the Refresh button does and the only thing that does.
func (s *Service) View(ctx context.Context, fresh bool) (Report, error) {
	reason := "window"
	if fresh {
		reason = "refresh"
	}
	return s.compose(ctx, reason, fresh)
}

// compose is the one composition path, and the one place the caching rule is
// enforced. reason travels in the event so the activity feed can say a report
// was given without saying what was in it.
func (s *Service) compose(ctx context.Context, reason string, fresh bool) (Report, error) {
	if !fresh {
		if held, ok := s.replay(); ok {
			s.publishGiven(held, reason)
			return held, nil
		}
	}
	// One composition at a time. Two asks arriving together — the voice and a
	// window that opened on the same breath — then cost one set of reads, and
	// the second waits for the first rather than racing it to the compositor.
	s.composing.Lock()
	defer s.composing.Unlock()
	if !fresh {
		// Re-checked under the lock: whoever we queued behind has just filled
		// the cache, and reading the machine twice in that instant is exactly
		// what the rule exists to prevent.
		if held, ok := s.replay(); ok {
			s.publishGiven(held, reason)
			return held, nil
		}
	}
	r := s.composeNow(ctx)
	s.keep(r)
	s.publishGiven(r, reason)
	return r, nil
}

// replay answers from the cache when the held report is still inside CacheFor.
// The copy it returns has its age re-worded against the clock, so a replay
// twenty seconds later says twenty seconds rather than repeating "just now".
func (s *Service) replay() (Report, bool) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cachedAt.IsZero() || now.Sub(s.cachedAt) >= s.cacheFor || now.Before(s.cachedAt) {
		// A clock that went backwards drops the cache rather than holding it
		// for however long the jump was: composing again is cheap and being
		// wrong about how old an answer is is not.
		return Report{}, false
	}
	held := s.cached
	held.Cached = true
	held.AgeSpoken = spokenAge(now, s.cachedAt)
	return held, true
}

// keep stores a composed report and moves the watermark. Both happen here, and
// only here, because they are the same event: a report was actually composed,
// so it is what a repeat ask replays and it is what the NEXT report means by
// "since you last looked".
func (s *Service) keep(r Report) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cached, s.cachedAt = r, r.At
	s.lastLooked = r.At
}

// instant is the moment one report is about, read before any source runs so
// every source in it is handed the same pair.
func (s *Service) instant() Instant {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	return Instant{Now: now, Since: s.lastLooked}
}

// read runs every source in parallel under one context, turning a refusal into
// an Unavailable item rather than a hole.
//
// Parallel because the reads touch the compositor and the filesystem and there
// are six of them: run in sequence they would add up to a wait a person notices
// before the first word is spoken, and none of them depends on another. The
// budget is the ceiling on all of them together — a wedged compositor costs the
// report one named unavailable source, never the report.
//
// A source's items keep their declaration position: the results are collected
// into a slice indexed by source, and flattened in order afterwards. Sorting by
// completion order would make the report's wording depend on which disk was
// warm, which is exactly the kind of nondeterminism a pinned ordering exists to
// exclude.
func (s *Service) read(ctx context.Context, at Instant) ([]Item, []string) {
	s.mu.Lock()
	sources := s.sources
	s.mu.Unlock()

	type outcome struct {
		items []Item
		err   error
	}
	results := make([]outcome, len(sources))
	var wg sync.WaitGroup
	for i, src := range sources {
		if src.Read == nil {
			continue
		}
		wg.Add(1)
		go func(i int, src Source) {
			defer wg.Done()
			items, err := src.Read(ctx, at)
			results[i] = outcome{items: items, err: err}
		}(i, src)
	}
	wg.Wait()

	var items []Item
	var unavailable []string
	for i, src := range sources {
		if src.Read == nil {
			continue
		}
		got := results[i]
		if got.err != nil {
			unavailable = append(unavailable, src.Name)
			items = append(items, Item{Rank: Unavailable, Source: src.Name,
				Text: unavailableSentence(src.Name)})
			s.log.Info("situation source unavailable", "component", "situation",
				"source", src.Name, "error", got.err.Error())
			continue
		}
		for _, item := range got.items {
			if item.Text == "" || item.Rank == Unavailable {
				// A source may not word its own unavailability: that sentence
				// is this package's, so "I couldn't check" always reads the
				// same way and always means the same thing.
				continue
			}
			item.Source = src.Name
			items = append(items, item)
		}
	}
	return items, unavailable
}

// coverage is the report's one admission about ITSELF: this process began after
// the moment the report is measuring "since you last looked" from, so its own
// activity record — an in-memory ring that died with the previous process (#70)
// — cannot account for the whole of that stretch (#190).
//
// It matters most where it is least visible. The reminders, the focus threads
// and the AI-session transcripts are on disk, so after a restart they answer for
// the whole stretch with complete confidence. The ring does not, and a report
// whose window lies mostly before the restart therefore reads as a composed,
// confident "nothing is failing" with the one thing that could not be checked
// left unsaid. That is not an omission a listener can detect, which is the
// definition of the kind this package refuses to make.
//
// It is said up front — spoken second, rendered directly under the headline —
// rather than appended to the Failing lines, because a restart is exactly what
// deletes those lines.
func (s *Service) coverage(since time.Time) string {
	if since.IsZero() {
		// Nothing is being claimed about a past stretch at all, so there is no
		// coverage to fall short of. A caveat here would be a doubt with no
		// claim attached to it.
		return ""
	}
	s.mu.Lock()
	startedAfter := s.startedAfter
	s.mu.Unlock()
	if startedAfter == nil || !startedAfter(since) {
		return ""
	}
	return restartSentence
}

// restartSentence names the shortfall and, just as importantly, its edges: a
// listener told only "some of this is missing" has been handed a doubt rather
// than a fact, and cannot tell which half of the report to trust.
const restartSentence = "I restarted since you last looked, so my own record of what has failed " +
	"only goes back to then; your sessions, threads, reminders and schedules are read live, " +
	"so those are complete."
