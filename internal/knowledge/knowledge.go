// Package knowledge keeps user-configured feeds warm: fixed commands whose
// latest value the daemon fetches on a schedule (or on first use) and holds,
// so a question about changing data — a stock price, the weather — is
// answered from a value already sitting in memory, with its age spoken
// honestly (ADR 0031).
//
// It is the moving counterpart of the memory book (ADR 0025). Memory holds
// facts the user *stated*, edited by hand and injected each turn; feeds hold
// values a command the user *wrote* keeps current. The trust model is the
// same shape: nothing here is model-authored — the model chooses which feed
// to read, never what runs.
//
// The scheduler is a supervised component (the #74 lesson, ADR 0018): every
// goroutine it starts is registered with one tracked group from the moment it
// exists, a reload rebuilds the schedules without orphaning the old ones, and
// shutdown drains the group under the daemon's deadline — a stopping daemon
// never abandons a fetch mid-write.
package knowledge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/rpickz/jarvix/internal/quiesce"
)

// Mode is when a feed's value is fetched.
type Mode string

// Feed modes.
const (
	// ModeEager refreshes on a schedule, so the value is ready before it is
	// asked for — the headline of the feature.
	ModeEager Mode = "eager"
	// ModeLazy fetches on first use, then serves the cached value until the
	// ttl lapses.
	ModeLazy Mode = "lazy"
)

// Feed is one configured fetcher. Nothing here is model-controlled: the argv
// is fixed at configuration time and the model can only name the feed.
type Feed struct {
	// Name is what the model asks for and what every surface calls the feed.
	Name string
	// Description tells the model what this feed watches.
	Description string
	// Argv is the fixed command that prints the value on stdout: element
	// zero is the program, the rest its arguments. Never a shell line.
	Argv []string
	// Mode is eager (scheduled) or lazy (fetch on first use).
	Mode Mode
	// Interval is the eager refresh cadence.
	Interval time.Duration
	// TTL is how long a fetched value counts as fresh; past it the value is
	// disclosed as stale, and a lazy feed refetches on the next ask.
	TTL time.Duration
	// Timeout bounds one fetch; the process group is killed past it.
	Timeout time.Duration
	// Inject opts the cached value into every model turn, under the shared
	// injection budget.
	Inject bool
}

// maxFeedBackoff caps the failure backoff: a feed whose command is broken is
// retried at most hourly rather than never, so fixing the command does not
// also require a restart to notice.
const maxFeedBackoff = time.Hour

// Options configure a Service. Zero values take the defaults; the seams
// exist so tests control every timestamp and never sleep.
type Options struct {
	// Feeds are the configured fetchers, in declaration order.
	Feeds []Feed
	// MaxInjectedTokens caps the estimated token cost of one injection.
	MaxInjectedTokens int
	// RefreshAllowed reports whether background fetching is permitted — the
	// knowledge.refresh gate identity resolved to allow. When false, eager
	// schedules do not run: a background fetch has no way to ask a
	// confirmation question, so anything short of allow means tool-initiated
	// fetches only (which face the gate per call).
	RefreshAllowed bool
	// ScrubEnv names extra environment variables to withhold from feed
	// commands, on top of the built-in secret-name patterns.
	ScrubEnv []string
	// Now is the clock, injectable so tests control every timestamp.
	Now func() time.Time
	// Timer creates one shot of the scheduler's clock; injectable so tests
	// drive every refresh without a sleep.
	Timer func(d time.Duration) (<-chan time.Time, func())
	// Runner overrides fetch execution in tests; the real path is runFeed.
	Runner func(ctx context.Context, feed Feed, env []string) FetchResult
}

// Service owns the feeds: the cached values, the persistence, and the eager
// scheduler. All methods are safe for concurrent use.
type Service struct {
	path           string
	maxInjected    int
	refreshAllowed bool
	scrubEnv       []string
	now            func() time.Time
	timer          func(d time.Duration) (<-chan time.Time, func())
	runner         func(ctx context.Context, feed Feed, env []string) FetchResult
	log            *slog.Logger

	// group tracks every scheduler goroutine from the moment it starts —
	// never a bare `go` (the #74 lesson). Drain waits on it.
	group quiesce.Group

	mu     sync.Mutex
	feeds  []Feed
	states map[string]*feedState
	// base is the lifetime context Start was given; schedule generations
	// derive from it so Drain's cancel reaches every loop and every fetch.
	base context.Context
	// cancelGen stops the current generation of eager loops. Reconfigure
	// cancels it and starts a new generation; the old goroutines unwind into
	// the same tracked group, so a reload can never orphan one.
	cancelGen context.CancelFunc
	closed    bool
	loaded    bool
}

// feedState is everything remembered about one feed between fetches. Value
// may be sensitive: it travels to the model, to the persisted 0600 file, and
// over the user's own socket — never into a log line or an event.
type feedState struct {
	Value     string
	Truncated bool
	// FetchedAt is when Value was last successfully fetched; zero means no
	// value has ever arrived.
	FetchedAt time.Time
	// LastAttempt is when a fetch last ran, success or not — what the
	// failure backoff counts from.
	LastAttempt time.Time
	// Failures counts the current streak of failed fetches; zero after a
	// success. It drives the backoff and the "failing since" disclosure.
	Failures     int
	FailingSince time.Time
	// LastErr is operator material for logs, doctor and the journal — never
	// spoken and never sent to the model.
	LastErr string
	// fetching latches while a fetch for this feed is in flight, so the
	// scheduler and a tool call cannot run the command twice concurrently.
	fetching bool
}

// Reading is what one Get returns: the cached value and everything a caller
// needs to speak its freshness honestly.
type Reading struct {
	Feed Feed
	// HasValue reports whether any value has ever been fetched.
	HasValue bool
	Value    string
	// Truncated reports the value hit the output cap when fetched.
	Truncated bool
	FetchedAt time.Time
	// Age is how old the value is, measured at this Get.
	Age time.Duration
	// Stale reports the value has outlived the feed's ttl — served anyway,
	// but the caller must say so.
	Stale bool
	// Failing reports fetches have been failing since FailingSince.
	Failing      bool
	FailingSince time.Time
	Attempts     int
}

// FeedStatus is one feed's operational snapshot for doctor and the
// knowledge.status IPC method.
type FeedStatus struct {
	Name         string
	Mode         Mode
	Inject       bool
	HasValue     bool
	Value        string
	FetchedAt    time.Time
	Age          time.Duration
	Stale        bool
	Failing      bool
	FailingSince time.Time
	Attempts     int
	LastErr      string
}

// NewService builds the feed service over the values file at path. Persisted
// values are loaded on first use, so construction is free.
func NewService(path string, opts Options, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	s := &Service{
		path:           path,
		maxInjected:    opts.MaxInjectedTokens,
		refreshAllowed: opts.RefreshAllowed,
		scrubEnv:       append([]string(nil), opts.ScrubEnv...),
		now:            opts.Now,
		timer:          opts.Timer,
		runner:         opts.Runner,
		log:            log,
		feeds:          append([]Feed(nil), opts.Feeds...),
		states:         make(map[string]*feedState),
	}
	if s.maxInjected <= 0 {
		s.maxInjected = 300
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.timer == nil {
		s.timer = func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTimer(d)
			return t.C, func() { t.Stop() }
		}
	}
	if s.runner == nil {
		s.runner = runFeed
	}
	return s
}

// Path returns the values file, for doctor and the status surfaces to name.
func (s *Service) Path() string { return s.path }

// Feeds returns the configured feeds in declaration order.
func (s *Service) Feeds() []Feed {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Feed(nil), s.feeds...)
}

// Start loads the persisted values and begins the eager schedules. ctx is
// the service's lifetime: its cancellation reaches every loop and every
// in-flight fetch, which is what makes Drain's deadline effective.
func (s *Service) Start(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.base != nil {
		return
	}
	s.base = ctx
	s.loadLocked()
	s.startLocked()
}

// Reconfigure swaps in a new feed set — the reload path. Cached values
// survive by name (a renamed feed starts cold), the previous generation of
// loops is cancelled into the same tracked group, and the new schedules are
// started immediately.
func (s *Service) Reconfigure(feeds []Feed) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.feeds = append([]Feed(nil), feeds...)
	if s.base == nil {
		return // not started yet; Start will schedule the new set
	}
	if s.cancelGen != nil {
		s.cancelGen()
		s.cancelGen = nil
	}
	s.startLocked()
	s.log.Info("knowledge feeds reconfigured", "component", "knowledge",
		"feeds", len(feeds))
}

// Drain stops the schedules and waits — bounded by ctx — for every tracked
// goroutine to finish. After it returns nothing of the scheduler survives,
// which is what lets daemon shutdown treat it as one more drained stage.
func (s *Service) Drain(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	if s.cancelGen != nil {
		s.cancelGen()
		s.cancelGen = nil
	}
	s.mu.Unlock()
	return s.group.Wait(ctx)
}

// InFlight reports how many scheduler goroutines are still running, for the
// shutdown log when a drain gives up.
func (s *Service) InFlight() int { return s.group.InFlight() }

// startLocked spawns one loop per eager feed under a fresh generation
// context. Callers hold s.mu and have set s.base.
func (s *Service) startLocked() {
	eager := 0
	for _, f := range s.feeds {
		if f.Mode == ModeEager {
			eager++
		}
	}
	if eager == 0 {
		return
	}
	if !s.refreshAllowed {
		// Said once, at Warn: the user configured eager feeds and a policy
		// entry quietly disabling them must never be something they discover
		// by noticing every value is stale.
		s.log.Warn("eager feeds configured but background refresh is not allowed; "+
			"values will only be fetched when the knowledge.get tool asks",
			"component", "knowledge", "eager_feeds", eager,
			"fix", `set [tools.policy.tool]."knowledge.refresh" = "allow"`)
		return
	}
	ctx, cancel := context.WithCancel(s.base)
	s.cancelGen = cancel
	for _, f := range s.feeds {
		if f.Mode != ModeEager {
			continue
		}
		feed := f
		// Add before go, never inside the goroutine: a drain that started
		// between the two would otherwise return while a loop was starting.
		s.group.Go(func() { s.runEager(ctx, feed) })
	}
}

// runEager is one eager feed's loop: wait out the next delay, refresh,
// repeat. Every wait goes through the timer seam and every fetch through the
// generation context, so tests drive it deterministically and shutdown ends
// it promptly.
func (s *Service) runEager(ctx context.Context, feed Feed) {
	for {
		s.mu.Lock()
		delay := s.eagerDelayLocked(feed)
		s.mu.Unlock()
		fire, stop := s.timer(delay)
		select {
		case <-ctx.Done():
			stop()
			return
		case <-fire:
		}
		if ctx.Err() != nil {
			return
		}
		s.fetch(ctx, feed)
	}
}

// eagerDelayLocked computes how long an eager feed's loop should wait before
// its next fetch: immediately when there is nothing yet, the remainder of the
// interval when a persisted value already covers part of it (the boot-warm
// case), and the backoff distance while fetches are failing. Callers hold
// s.mu.
func (s *Service) eagerDelayLocked(feed Feed) time.Duration {
	st := s.states[feed.Name]
	now := s.now()
	var next time.Time
	switch {
	case st == nil || (st.FetchedAt.IsZero() && st.LastAttempt.IsZero()):
		return 0
	case st.Failures > 0:
		next = st.LastAttempt.Add(backoffDelay(feed.Interval, st.Failures))
	default:
		next = st.FetchedAt.Add(feed.Interval)
	}
	if d := next.Sub(now); d > 0 {
		return d
	}
	return 0
}

// backoffDelay is the wait after the failures-th consecutive failure: the
// feed's own cadence, doubled per further failure, capped at maxFeedBackoff
// (or the cadence itself, for feeds slower than the cap). Backing off from
// the cadence rather than from seconds keeps a broken command from being
// hammered while keeping the first retry no later than a normal refresh.
func backoffDelay(cadence time.Duration, failures int) time.Duration {
	ceiling := maxFeedBackoff
	if cadence > ceiling {
		ceiling = cadence
	}
	d := cadence
	for i := 1; i < failures && d < ceiling; i++ {
		d *= 2
	}
	if d > ceiling {
		d = ceiling
	}
	return d
}

// cadence is the rhythm the backoff scales from: the refresh interval for an
// eager feed, the ttl for a lazy one (whose natural refetch rhythm it is).
func cadence(feed Feed) time.Duration {
	if feed.Mode == ModeLazy {
		return feed.TTL
	}
	return feed.Interval
}

// Get returns the current reading for one feed, fetching first when this ask
// is what should trigger a fetch: a lazy feed past its ttl, or any feed that
// has no value yet — unless the failure backoff says not to retry yet, in
// which case the last good value (or the failure) serves as it stands. The
// second return is false when no feed has that name.
func (s *Service) Get(ctx context.Context, name string) (Reading, bool) {
	s.mu.Lock()
	feed, ok := s.feedLocked(name)
	if !ok {
		s.mu.Unlock()
		return Reading{}, false
	}
	s.loadLocked()
	if s.shouldSyncFetchLocked(feed) {
		st := s.stateLocked(feed.Name)
		st.fetching = true
		s.mu.Unlock()
		s.fetchNow(ctx, feed)
		s.mu.Lock()
	}
	r := s.readingLocked(feed)
	s.mu.Unlock()
	return r, true
}

// shouldSyncFetchLocked decides whether this Get pays for a fetch. Eager
// feeds normally never fetch here — the scheduler owns their cadence and a
// Get must stay instant, which is the point of the feature — except when no
// value exists at all (first boot, before the first refresh lands) or when
// background refresh is not allowed and a gate-approved tool call is the only
// fetch path there is. Callers hold s.mu.
func (s *Service) shouldSyncFetchLocked(feed Feed) bool {
	st := s.states[feed.Name]
	if st != nil && st.fetching {
		return false // a fetch is already in flight; serve what stands
	}
	now := s.now()
	if st != nil && st.Failures > 0 &&
		now.Before(st.LastAttempt.Add(backoffDelay(cadence(feed), st.Failures))) {
		return false // backing off; serve last-good with its age disclosed
	}
	if st == nil || st.FetchedAt.IsZero() {
		return true // nothing to serve; this ask is the trigger
	}
	scheduled := feed.Mode == ModeEager && s.refreshAllowed
	if scheduled {
		return false
	}
	return now.Sub(st.FetchedAt) > feed.TTL
}

// fetch runs one scheduled refresh, skipping if a tool call already has one
// in flight for the same feed.
func (s *Service) fetch(ctx context.Context, feed Feed) {
	s.mu.Lock()
	st := s.stateLocked(feed.Name)
	if st.fetching {
		s.mu.Unlock()
		return
	}
	st.fetching = true
	s.mu.Unlock()
	s.fetchNow(ctx, feed)
}

// fetchNow executes the feed's command and records the outcome. The caller
// has already latched state.fetching; this always clears it. Values never
// appear in log lines — names, sizes and durations only.
func (s *Service) fetchNow(ctx context.Context, feed Feed) {
	runCtx, cancel := context.WithTimeout(ctx, feed.Timeout)
	start := s.now()
	res := s.runner(runCtx, feed, scrubbedFeedEnv(s.scrubEnv))
	cancel()

	now := s.now()
	s.mu.Lock()
	st := s.stateLocked(feed.Name)
	st.fetching = false
	st.LastAttempt = now
	value := strings.TrimSpace(res.Stdout)
	switch {
	case ctx.Err() != nil:
		// Shutdown or reload cancelled the fetch mid-flight: not the feed's
		// fault, so it neither counts against the backoff nor overwrites the
		// last good value.
	case res.Err == nil && res.ExitCode == 0 && !res.TimedOut && value != "":
		st.Value, st.Truncated, st.FetchedAt = value, res.Truncated, now
		st.Failures, st.FailingSince, st.LastErr = 0, time.Time{}, ""
		s.log.Debug("feed refreshed", "component", "knowledge", "feed", feed.Name,
			"duration_ms", now.Sub(start).Milliseconds(), "bytes", len(value))
	default:
		st.Failures++
		if st.FailingSince.IsZero() {
			st.FailingSince = now
		}
		st.LastErr = fetchErrText(res, value)
		// One Warn per streak, like the warm supervisor: a broken command
		// must not fill the journal with a line per backoff attempt.
		line := s.log.Debug
		if st.Failures == 1 {
			line = s.log.Warn
		}
		line("feed fetch failed; serving the last good value until it recovers",
			"component", "knowledge", "feed", feed.Name, "failures", st.Failures,
			"error", st.LastErr)
	}
	s.saveLocked()
	s.mu.Unlock()
}

// fetchErrText summarises a failed fetch for the journal and doctor — exit
// codes and the first stderr line, never stdout, which is value territory.
func fetchErrText(res FetchResult, value string) string {
	switch {
	case res.TimedOut:
		return "timed out"
	case res.Err != nil:
		return res.Err.Error()
	case res.ExitCode != 0:
		msg := fmt.Sprintf("exit status %d", res.ExitCode)
		if line := firstStderrLine(res.Stderr); line != "" {
			msg += ": " + line
		}
		return msg
	case value == "":
		return "printed nothing"
	}
	return "failed"
}

// readingLocked snapshots one feed's reading at now. Callers hold s.mu.
func (s *Service) readingLocked(feed Feed) Reading {
	st := s.states[feed.Name]
	r := Reading{Feed: feed}
	if st == nil {
		return r
	}
	now := s.now()
	if !st.FetchedAt.IsZero() {
		r.HasValue = true
		r.Value = st.Value
		r.Truncated = st.Truncated
		r.FetchedAt = st.FetchedAt
		r.Age = now.Sub(st.FetchedAt)
		r.Stale = r.Age > feed.TTL
	}
	if st.Failures > 0 {
		r.Failing = true
		r.FailingSince = st.FailingSince
		r.Attempts = st.Failures
	}
	return r
}

// Status reports every feed for doctor and knowledge.status, in declaration
// order.
func (s *Service) Status() []FeedStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	out := make([]FeedStatus, 0, len(s.feeds))
	for _, f := range s.feeds {
		r := s.readingLocked(f)
		st := FeedStatus{
			Name: f.Name, Mode: f.Mode, Inject: f.Inject,
			HasValue: r.HasValue, Value: r.Value, FetchedAt: r.FetchedAt,
			Age: r.Age, Stale: r.Stale,
			Failing: r.Failing, FailingSince: r.FailingSince, Attempts: r.Attempts,
		}
		if state := s.states[f.Name]; state != nil {
			st.LastErr = state.LastErr
		}
		out = append(out, st)
	}
	return out
}

// feedLocked resolves a name against the configured feeds. Callers hold s.mu.
func (s *Service) feedLocked(name string) (Feed, bool) {
	for _, f := range s.feeds {
		if f.Name == name {
			return f, true
		}
	}
	return Feed{}, false
}

// stateLocked returns the state for a feed, creating it cold. Callers hold
// s.mu.
func (s *Service) stateLocked(name string) *feedState {
	st := s.states[name]
	if st == nil {
		st = &feedState{}
		s.states[name] = st
	}
	return st
}

// loadLocked reads the persisted values once. Failures degrade to an empty
// cache — a feed cache is reproducible, so unlike the memory store nothing
// here is worth refusing over. Callers hold s.mu.
func (s *Service) loadLocked() {
	if s.loaded {
		return
	}
	s.loaded = true
	states, err := readValues(s.path)
	if err != nil {
		s.log.Warn("feed values could not be loaded; starting cold",
			"component", "knowledge", "error", err.Error())
		return
	}
	for name, st := range states {
		s.states[name] = st
	}
}

// saveLocked persists the current values for the configured feeds — entries
// for feeds no longer configured are dropped here, which is the cache's whole
// deletion story. A failed write is a warning, not an error: the values are
// still served from memory and rewritten on the next fetch. Callers hold
// s.mu.
func (s *Service) saveLocked() {
	if err := writeValues(s.path, s.feeds, s.states); err != nil {
		s.log.Warn("feed values could not be persisted; the daemon will boot cold",
			"component", "knowledge", "error", err.Error())
	}
}
