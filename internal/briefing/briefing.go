// Package briefing composes the return briefing (#150, ADR 0050): the short
// account Jarvix can give of a stretch of time the user was not here.
//
// The boundary is the feature. A briefing reports **only what Jarvix already
// participates in** — the AI sessions anchored to its focus threads, the
// routines and scripts it ran, the reminders it holds, the threads it keeps,
// and how many exchanges there were. Nothing here watches the machine. No
// keystroke, window-history, browsing, or process record may ever be added to
// enrich it, now or as a "small extension": a log of everything the user did
// is a different product with a different consent conversation, and this
// package must not become its way in. That line is the reason the feature
// exists at all, and it is restated in ADR 0050 because a boundary that lives
// only in a ticket is a boundary that erodes.
//
// Two more stances shape everything below.
//
// **Offered, not ambushed.** After a long enough absence, and only when at
// least one source actually has something, one sentence is appended to the
// answer the user came back and asked for. The briefing itself follows on
// request. `briefing.speak_on_return` turns that into the whole account
// spoken at the same moment, and it is off by default.
//
// **Prepared lazily, never on a clock.** There is no scheduler in this
// package and no goroutine — nothing summarises a machine nobody is using.
// Everything is read at the moment the user is demonstrably back, which is
// also the only moment the answer could be wanted. That is why ADR 0049's
// "a scheduler loop never parks" has nothing to say here: there is no loop.
//
// The composed briefing is transient. It is not written to any store, it
// carries no content into any event, and the deterministic reading is not
// committed to the conversation — the activity row says a briefing was given
// and nothing about what was in it.
package briefing

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Category is the ordering of a briefing, and it is a claim about the reader
// rather than about the machine: what is blocked on you comes before what
// finished, which comes before what is still running, which comes before the
// housekeeping you would never have watched anyway. A briefing read in any
// other order makes the listener wait for the part they needed.
type Category int

// The categories, in the order they are always spoken.
const (
	// Awaiting is work that is stopped until the user does something.
	Awaiting Category = iota
	// Completed is work that finished while they were away.
	Completed
	// InProgress is work that is still running.
	InProgress
	// Housekeeping is what the machine did on its own behalf.
	Housekeeping
	// Unavailable is a source that could not be read. It is a category, not
	// an omission, because a briefing that quietly drops a source it failed
	// to read is a briefing that lies by shape: the listener hears "nothing
	// from the reminders" when the truth is "I could not look".
	Unavailable
)

// categoryTitles are the section headings the window renders and the spoken
// form implies. Worded here, daemon-side, like every other Jarvix surface
// (ADR 0013): the QML tab renders these strings and composes none of its own.
var categoryTitles = [...]string{
	Awaiting:     "Waiting for you",
	Completed:    "Finished",
	InProgress:   "Still going",
	Housekeeping: "Housekeeping",
	Unavailable:  "I couldn't check",
}

// Title is the section heading for a category.
func (c Category) Title() string {
	if c < 0 || int(c) >= len(categoryTitles) {
		return ""
	}
	return categoryTitles[c]
}

// ordered is every category in speaking order — the one place the ordering is
// written down, so the spoken form, the window's sections, and the tests
// cannot drift apart.
var ordered = [...]Category{Awaiting, Completed, InProgress, Housekeeping, Unavailable}

// Line is one fact, already worded. Sources compose their own sentences
// because they are the only code that knows what the fact means; nothing
// downstream — not the model, not the window — invents wording from data.
type Line struct {
	Category Category
	// Source is the stable identifier of the source that produced the line,
	// carried for logging and for the "at most one line per source per
	// category" rule. It is never spoken.
	Source string
	Text   string
}

// Source is one corner of what Jarvix already participates in. Read is given
// the moment the absence began and the moment it ended, and returns at most
// one line per category — a source with three different kinds of news gets to
// say all three, but never twice about the same one.
//
// An error is not a failure of the briefing: it becomes an Unavailable line
// naming this source, which is the honest outcome and the tested one.
type Source struct {
	Name string
	Read func(ctx context.Context, since, now time.Time) ([]Line, error)
}

// Settings is the live configuration a briefing reads at the moment it acts
// — never at construction, so "stop offering me briefings" lands on the very
// next answer.
type Settings struct {
	Enabled       bool
	AfterHours    int
	SpeakOnReturn bool
}

// Default bounds. Both are wall-clock ceilings on one user-visible moment,
// and they are deliberately different sizes: the offer check runs while the
// answer the user actually asked for is still draining out of the speaker, so
// it must be over before they notice; the full briefing was explicitly asked
// for and may spend a model call.
const (
	// DefaultOfferBudget bounds the "is there anything at all?" check. No
	// model call and no window capture happens inside it — it is the same
	// source reads the briefing does, and nothing more.
	DefaultOfferBudget = 2 * time.Second
	// DefaultBudget bounds one whole briefing: every source read plus the
	// one model call that words its headline.
	DefaultBudget = 6 * time.Second
	// defaultAfterHours mirrors config's default so a Service built without
	// a Settings func is still usable rather than silently never firing.
	defaultAfterHours = 8
)

// Options configures a Service. Every seam that touches the world — the
// clock, the sources, the provider, the event bus — arrives here as a
// function, which is what makes the tests hermetic and why this package
// imports neither the daemon nor a provider.
type Options struct {
	// Now is the clock. Nil means time.Now.
	Now func() time.Time
	// Seed reports when the daemon last saw the user before this process
	// started, so an absence that spans a restart is still an absence. It is
	// called exactly once, during construction, off every hot path. false
	// means "no idea", which reads as "not away" — the conservative
	// direction: a briefing is never invented for an absence we cannot
	// demonstrate.
	Seed func() (time.Time, bool)
	// Settings reads the live configuration. Nil means the shipped defaults.
	Settings func() Settings
	// Sources are read in this order, and their lines are spoken in this
	// order within a category.
	Sources []Source
	// Summarise words the headline through the provider. Nil means the
	// deterministic headline is used and no model is ever consulted — which
	// is also what happens when the call fails.
	Summarise func(ctx context.Context, prompt string) (string, error)
	// Budget bounds one full briefing; OfferBudget bounds the offer check.
	// Zero means the defaults above.
	Budget      time.Duration
	OfferBudget time.Duration
	// Publish emits the briefing's one event. It carries counts, outcomes and
	// source names — never a word of the briefing itself.
	Publish func(event string, data map[string]any)
}

// Service holds the one thing this feature has to remember: when the user was
// last here, and whether a standing absence still owes them an offer.
//
// It is not a store. Nothing here is written to disk, and a restart starts
// again from the seed — which is exactly right, because the seed is the
// durable record (the conversation archive) and this is a cache of it.
type Service struct {
	now func() time.Time
	// settingsFn, sources and summarise are late-bindable and therefore
	// guarded by mu, like everything else the daemon completes after
	// construction.
	settingsFn  func() Settings
	sources     []Source
	summarise   func(ctx context.Context, prompt string) (string, error)
	budget      time.Duration
	offerBudget time.Duration
	publish     func(event string, data map[string]any)
	log         *slog.Logger

	mu sync.Mutex
	// lastSeen is the last moment a user-started exchange reached the engine.
	// Scheduled sessions — a reminder speaking at three in the morning — do
	// not touch it, because the daemon talking to itself is not the user
	// being here, and counting it would erase the very absence this measures.
	lastSeen time.Time
	// absenceSince is the start of the standing absence: the lastSeen value
	// the arrival superseded. Zero means there is no absence to report.
	absenceSince time.Time
	// offerDue is the one-shot half. It is set when an absence is detected
	// and cleared the first time an answer could carry the offer, whether or
	// not there turned out to be anything to offer — "exactly one offer line"
	// means the question is asked once per absence, not once per exchange.
	// absenceSince outlives it, so the briefing itself stays askable.
	offerDue bool
}

// NewService builds the briefing service and seeds its watermark.
func NewService(opts Options, log *slog.Logger) *Service {
	s := &Service{
		now:         opts.Now,
		settingsFn:  opts.Settings,
		sources:     opts.Sources,
		summarise:   opts.Summarise,
		budget:      opts.Budget,
		offerBudget: opts.OfferBudget,
		publish:     opts.Publish,
		log:         log,
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.settingsFn == nil {
		s.settingsFn = func() Settings {
			return Settings{Enabled: true, AfterHours: defaultAfterHours}
		}
	}
	if s.budget <= 0 {
		s.budget = DefaultBudget
	}
	if s.offerBudget <= 0 {
		s.offerBudget = DefaultOfferBudget
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if opts.Seed != nil {
		if seeded, ok := opts.Seed(); ok {
			s.lastSeen = seeded
		}
	}
	return s
}

// settings reads the live configuration through whatever is currently bound.
func (s *Service) settings() Settings {
	s.mu.Lock()
	fn := s.settingsFn
	s.mu.Unlock()
	return fn()
}

// BindSources late-binds the source list. The daemon needs it: half the
// sources read state that only exists once the daemon itself does, and the
// service has to exist before the engine that carries it. Called once during
// construction, single-threaded, exactly like the focus service's BindRecap.
func (s *Service) BindSources(sources ...Source) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sources = sources
}

// BindSettings late-binds the live configuration reader, for the same
// construction-order reason as BindSources: the daemon's running config only
// exists once the daemon does, and reading a construction-time snapshot would
// quietly make three live-class settings restart-class.
func (s *Service) BindSettings(settings func() Settings) {
	if settings == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settingsFn = settings
}

// BindSummarise late-binds the provider seam, for the same construction-order
// reason as BindSources.
func (s *Service) BindSummarise(summarise func(ctx context.Context, prompt string) (string, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.summarise = summarise
}

// Arrive records that the user is demonstrably back: a user-started exchange
// has reached the engine at now. It is pure arithmetic over one mutex — no
// I/O, no source read, nothing that could make an ordinary turn wait — which
// is what lets the engine call it on every exchange.
//
// A qualifying gap supersedes any standing offer rather than stacking with
// it. That is the ticket's rule read literally: an offer belongs to one
// absence, and after a second night the news is the second night's.
func (s *Service) Arrive(now time.Time) {
	set := s.settings()
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.lastSeen
	s.lastSeen = now
	if !set.Enabled {
		// Nothing is prepared, offered, or scheduled — but the watermark
		// keeps moving, so switching the feature back on does not
		// immediately claim the whole time it was off as an absence.
		return
	}
	if previous.IsZero() || !now.After(previous) {
		// No previous sighting, or a clock that went backwards. Either way
		// there is no absence we can demonstrate, and an undemonstrable
		// absence is not one.
		return
	}
	if now.Sub(previous) < afterDuration(set) {
		return
	}
	s.absenceSince = previous
	s.offerDue = true
}

// afterDuration is the configured absence threshold, floored so a
// hand-edited zero cannot turn every exchange into a return.
func afterDuration(set Settings) time.Duration {
	hours := set.AfterHours
	if hours < 1 {
		hours = defaultAfterHours
	}
	return time.Duration(hours) * time.Hour
}

// OfferLine reports the one sentence to append to the answer of the first
// exchange after an absence, or "" for the overwhelmingly common case of no
// absence at all — which costs one mutex and returns before anything is read.
//
// With briefing.speak_on_return set it returns the whole briefing instead.
// "Unprompted" there means "without you asking for the briefing"; Jarvix
// still waits until the user is demonstrably back, because nothing in this
// package runs on a clock.
//
// transient reports which of the two came back. The offer sentence is part of
// the answer and belongs in the record with it; a whole briefing does not —
// it is transient like a recap, and committing it here would smuggle into
// conversation memory exactly what the explicit-ask path is careful to keep
// out of it.
func (s *Service) OfferLine(ctx context.Context) (line string, transient bool) {
	set := s.settings()
	s.mu.Lock()
	due, since := s.offerDue, s.absenceSince
	if due {
		// Consumed whatever the answer turns out to be: the check runs once
		// per absence, so a machine that did nothing overnight pays for one
		// source read and is never asked again.
		s.offerDue = false
	}
	s.mu.Unlock()
	if !due || !set.Enabled || since.IsZero() {
		return "", false
	}
	if set.SpeakOnReturn {
		spoken, _ := s.compose(ctx, since, s.budget, "return")
		if spoken.Empty {
			// Nothing happened, and the user did not ask. Silence is the
			// honest answer: "nothing while you were away" is what an
			// explicit ask earns, not what a return is greeted with.
			return "", false
		}
		return spoken.Spoken, true
	}
	composed, err := s.probe(ctx, since)
	if err != nil || !composed {
		return "", false
	}
	return offerSentence, false
}

// The two sentences a non-briefing gets. They are here rather than at each
// call site because both surfaces say them — the voice through Briefing, the
// window through View's headline — and a surface that worded its own would be
// composing, which ADR 0013 puts daemon-side for exactly this reason.
const (
	DisabledSentence  = "Return briefings are switched off."
	NoAbsenceSentence = "You haven't been away long enough for a briefing."
)

// offerSentence is the whole ambush-avoidance contract in one line: it names
// what is on offer, says it is on offer, and asks for nothing. It is a fixed
// sentence rather than a model call because an offer that cost a provider
// round-trip would be a briefing prepared in advance, which is the thing this
// design refuses.
const offerSentence = "I've got a briefing on what happened while you were away, whenever you want it."

// probe is the cheap "is there anything at all?" check: the same source reads
// the briefing does, under a tighter budget, with no model call and no window
// capture. It reports true only when a source produced something substantive
// — a briefing whose entire content is "I couldn't check the reminders" is
// not news, and offering it would be the noise the offer line exists to avoid.
func (s *Service) probe(ctx context.Context, since time.Time) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, s.offerBudget)
	defer cancel()
	lines, _ := s.read(ctx, since, s.now())
	for _, line := range lines {
		if line.Category != Unavailable {
			return true, nil
		}
	}
	return false, ctx.Err()
}

// Briefing is the spoken briefing, for the deterministic phrases and the
// model's tool alike. It never returns an error for having nothing to say:
// "nothing while you were away" is an answer, and a spoken apology would
// imply a fault where there was only a quiet night. The error is reserved for
// a service that cannot brief at all.
func (s *Service) Briefing(ctx context.Context) (string, error) {
	set := s.settings()
	if !set.Enabled {
		return DisabledSentence, nil
	}
	since, ok := s.standing()
	if !ok {
		return NoAbsenceSentence, nil
	}
	composed, err := s.compose(ctx, since, s.budget, "ask")
	if err != nil {
		return "", err
	}
	return composed.Spoken, nil
}

// View is the window's full version: every line, in order, untruncated. It
// composes independently rather than replaying a cached briefing — the facts
// are re-read, so a tab opened ten minutes after the spoken version tells the
// truth about ten minutes later rather than a remembered truth. Nothing is
// cached precisely because nothing is persisted.
func (s *Service) View(ctx context.Context) (Composed, error) {
	set := s.settings()
	if !set.Enabled {
		return Composed{Disabled: true, Headline: DisabledSentence}, nil
	}
	since, ok := s.standing()
	if !ok {
		return Composed{NoAbsence: true, Headline: NoAbsenceSentence}, nil
	}
	return s.compose(ctx, since, s.budget, "window")
}

// standing reports the absence a briefing would be about. It survives the
// offer being spent: the offer is one sentence, the absence is the subject,
// and the subject stands until the next absence replaces it.
func (s *Service) standing() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.absenceSince.IsZero() {
		return time.Time{}, false
	}
	return s.absenceSince, true
}

// read runs every source under one context, turning a refusal into an
// Unavailable line rather than a hole. A source may return at most one line
// per category; a second is dropped, so no source can crowd the others out of
// a briefing bounded by how long a person will listen.
func (s *Service) read(ctx context.Context, since, now time.Time) ([]Line, []string) {
	s.mu.Lock()
	sources := s.sources
	s.mu.Unlock()

	var lines []Line
	var unavailable []string
	for _, src := range sources {
		if src.Read == nil {
			continue
		}
		got, err := src.Read(ctx, since, now)
		if err != nil {
			unavailable = append(unavailable, src.Name)
			lines = append(lines, Line{Category: Unavailable, Source: src.Name,
				Text: unavailableSentence(src.Name)})
			s.log.Info("briefing source unavailable", "component", "briefing",
				"source", src.Name, "error", err.Error())
			continue
		}
		seen := map[Category]bool{}
		for _, line := range got {
			if line.Text == "" || seen[line.Category] || line.Category == Unavailable {
				continue
			}
			seen[line.Category] = true
			line.Source = src.Name
			lines = append(lines, line)
		}
	}
	return lines, unavailable
}
