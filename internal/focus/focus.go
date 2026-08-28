// Package focus owns focus threads (#123, ADR 0041): named pieces of work a
// user switches between by voice, each with an optional window anchor, its
// own parked thoughts, an optional check-in interval, and — one at a time — a
// timeboxed focus session. Monotasking and multi-tasking are the same model:
// a timebox is just one thread holding the floor for a fixed stretch.
//
// The package is deliberately three disciplines this repository already
// trusts, restated over a new noun:
//
//   - Storage is the memory book's (ADR 0025): one hand-editable TOML file
//     under the XDG state dir, atomic fsync-and-rename writes, a stat-based
//     change detector so a hand edit lands on the very next operation,
//     normalize-repair for what an edit leaves out, and a corrupt latch that
//     serves an empty store and never overwrites a file mid-fix.
//   - Scheduling is the automation scheduler's (ADR 0032): an injected clock
//     and timer so no test ever sleeps, every goroutine tracked in one
//     quiesce.Group from the moment it exists, and a missed-while-down policy
//     of adopt-and-report, never re-fire. It is a third sibling of that
//     scheduler and the knowledge feeds' — not an extraction — because its
//     events (interval check-ins, one timebox's midpoint and close) fit
//     neither wall-clock moments nor fetch backoff.
//   - Every spoken sentence is templated here, daemon-side, from the thread's
//     own record (ADR 0013): a recap can be wrong only if the record is,
//     never because something was invented. The one exception is deliberate
//     and fenced: a thread anchored to an AI session earns a model-composed
//     summary of what is visible in that window (#124, ADR 0043, recap.go),
//     and every failure of that path falls back to the templated record
//     behind a pinned honest admission.
package focus

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/quiesce"
)

// Model types. Everything is plain data; the Service owns all mutation.

// Anchor is one window a thread is tied to, captured at anchor time. The
// address is the compositor's stable handle and the only thing ever compared
// against the live inventory; app and title are the human words recaps speak.
type Anchor struct {
	Address  string
	StableID string
	App      string
	Title    string
}

// Parked is one thought parked into a thread ("later: reply to Dan").
type Parked struct {
	ID   string
	Text string
	At   time.Time
}

// Thread is one named piece of work.
type Thread struct {
	ID      string
	Name    string
	Created time.Time
	// LastSwitched is when the thread last became active; zero for a thread
	// never switched into, which is what makes "fresh thread" honest.
	LastSwitched time.Time
	// LastActivity is the most recent touch of any kind.
	LastActivity time.Time
	// RemindEveryMin is the check-in interval in minutes, 0 for none.
	RemindEveryMin int
	// Recap is the AI-session recap trigger (#124): RecapAuto (the empty
	// default — read the anchored window only when it is a terminal),
	// RecapAlways, or RecapNever. Hand-editable as the thread's `recap` key.
	Recap string
	// Anchors holds at most two windows.
	Anchors []Anchor
	Parked  []Parked
}

// Session is one timeboxed focus session. The zero value means none is live;
// ThreadID is the discriminator.
type Session struct {
	ThreadID string
	Started  time.Time
	Minutes  int
	// MidpointDue latches when the midpoint firing has been dispatched;
	// MidpointDone when the line has actually been spoken.
	MidpointDue  bool
	MidpointDone bool
	// Closing latches when the timebox has run out: the close prompt is owed
	// and the countdown is over. The record stays until the user answers or
	// the answer window lapses.
	Closing   bool
	ClosingAt time.Time
}

// live reports a session record that still holds the floor.
func (s Session) live() bool { return s.ThreadID != "" }

// end is the moment the timebox runs out.
func (s Session) end() time.Time { return s.Started.Add(time.Duration(s.Minutes) * time.Minute) }

// midpoint is the moment the optional halfway check-in is due.
func (s Session) midpoint() time.Time {
	return s.Started.Add(time.Duration(s.Minutes) * time.Minute / 2)
}

// maxAnchors is how many windows a thread may be tied to. Two is the issue's
// own bound: a couple of windows and an AI session is a front, more is a
// workspace — and workspaces already have a feature.
const maxAnchors = 2

// maxThreads caps the store. A dozen live fronts is already far past what
// switching can serve; the cap exists so a store nobody prunes cannot grow
// without bound, and the refusal names the fix.
const maxThreads = 20

// closingAnswerWindow is how long the continue-or-break question stays
// answerable after the close. Past it the session record expires quietly:
// the user plainly moved on, and "keep focusing" an hour later should not
// resurrect a timebox from before lunch.
const closingAnswerWindow = 15 * time.Minute

// The Service's refusals, as matchable sentinels (the memory book's pattern):
// callers place the message, the rule lives here.
var (
	// ErrNoName refuses a thread with nothing to call it.
	ErrNoName = errors.New("a thread needs a name")
	// ErrUnknownThread refuses a reference no thread answers to.
	ErrUnknownThread = errors.New("no thread is called")
	// ErrAmbiguous refuses a reference several threads answer to.
	ErrAmbiguous = errors.New("more than one thread matches")
	// ErrNoActive refuses an operation that needs an active thread.
	ErrNoActive = errors.New("no thread is active")
	// ErrNoSession refuses a session operation with no live session.
	ErrNoSession = errors.New("no focus session is running")
	// ErrStoreFull refuses a store at the thread cap.
	ErrStoreFull = errors.New("the focus store is full")
)

// Firing is one scheduled speech moment the Service asks the daemon to carry
// out: a per-thread check-in, a timebox midpoint, or a timebox close. The
// daemon owns how it is spoken (the scheduled-session path, ADR 0032); the
// Service owns when, and has already recorded the state change the firing
// announces — so a firing that cannot be spoken is a skipped announcement,
// never a lost state.
type Firing struct {
	Kind   FiringKind
	Thread Thread
}

// FiringKind names what a firing announces.
type FiringKind string

// The firing kinds.
const (
	FiringReminder FiringKind = "reminder"
	FiringMidpoint FiringKind = "midpoint"
	FiringClose    FiringKind = "close"
)

// Options configure a Service. The seams exist so tests control every
// timestamp and never sleep, and so no test in this package can touch a real
// compositor.
type Options struct {
	// Windows reads the live window inventory, for anchors — the same seam
	// the window tools resolve against (ADR 0022). Nil means anchoring and
	// anchor-gone checks degrade gracefully: threads work, anchors refuse.
	Windows func(ctx context.Context) ([]desktop.Window, error)
	// Fire carries out one scheduled speech moment. It blocks until the
	// attempt is over; it runs on a goroutine the Service tracks. Nil drops
	// firings (a daemon built without speech).
	Fire func(ctx context.Context, f Firing)
	// Publish emits focus.changed events for the Focus tab and the overlay.
	// Nil publishes nothing.
	Publish func(event string, data map[string]any)
	// Midpoint reports whether the optional halfway check-in is enabled
	// (config focus.midpoint_checkin), read at fire time so a reload lands
	// without a restart. Nil means disabled — the shipped default.
	Midpoint func() bool
	// Capture reads what is visible in one anchored window for the
	// AI-session recap (#124), through the desktop-context capture seam:
	// bounded, redacted, and honouring ctx. ErrRecapUnavailable means the
	// window source is switched off and the recap skips silently. Nil
	// disables the model-composed recap entirely (with Summarise).
	Capture func(ctx context.Context, a Anchor) (Capture, error)
	// Summarise asks the model for the pinned-style session summary; the
	// prompt is composed here (RecapPrompt) so every provider answers the
	// same contract. Nil disables the model-composed recap (with Capture).
	Summarise func(ctx context.Context, prompt string) (string, error)
	// Classify reads one anchored window's deterministic session state
	// (#137) for the Snapshot — working / needs_you / done, "" when unknown.
	// No model call is ever involved: the daemon reads the session's
	// transcript structure, under the same consent gate as Capture. The
	// window rides along because the daemon needs its process and class, and
	// trigger is the thread's recap mode so an opted-in ("always")
	// non-terminal is classified and an opted-out thread never is. Nil
	// disables classification; focus.list then simply omits the field.
	Classify func(ctx context.Context, a Anchor, w desktop.Window, trigger string) (string, error)
	// RecapBudget bounds one capture-plus-summary attempt; zero means
	// DefaultRecapBudget. Tests shrink it so a deadline fires without a
	// sleep.
	RecapBudget time.Duration
	// Now is the clock; Timer creates one shot of it.
	Now   func() time.Time
	Timer func(d time.Duration) (<-chan time.Time, func())
}

// Service owns the thread store and the check-in clockwork. All methods are
// safe for concurrent use, and every one of them begins by checking whether
// the file changed on disk — a hand-edit is picked up on the very next
// operation, no restart, no watcher.
type Service struct {
	path     string
	windows  func(ctx context.Context) ([]desktop.Window, error)
	fire     func(ctx context.Context, f Firing)
	publish  func(string, map[string]any)
	midpoint func() bool
	// capture and summarise are the AI-session recap's two halves (#124);
	// both nil means every recap is the templated base. classify is the
	// Snapshot's session-state read (#137), nil when the daemon has no
	// transcript reader. Bound once, before Start, like fire — never mutated
	// after.
	capture     func(ctx context.Context, a Anchor) (Capture, error)
	summarise   func(ctx context.Context, prompt string) (string, error)
	classify    func(ctx context.Context, a Anchor, w desktop.Window, trigger string) (string, error)
	recapBudget time.Duration
	now         func() time.Time
	timer       func(d time.Duration) (<-chan time.Time, func())
	log         *slog.Logger
	// write persists the store; always writeStore outside tests. A field for
	// the memory book's reason: the write-failure contract (a failed write
	// must cost exactly nothing in memory) needs a disk that fails on
	// command, hermetically.
	write func(path string, p persisted) error

	// group tracks the scheduler loop and every in-flight firing from the
	// moment it exists, never a bare `go` (the #74 lesson).
	group quiesce.Group

	mu sync.Mutex
	st persisted
	// loaded, mod and size are the change detector: the file is re-read when
	// its mtime or size no longer matches what was last loaded or written.
	loaded bool
	mod    time.Time
	size   int64
	// corrupt latches when the on-disk file could not be parsed. While set,
	// the Service serves an empty store, and the first write moves the
	// unparseable file aside instead of overwriting it.
	corrupt bool
	// reminderNext is each thread's next check-in moment, in memory only:
	// intervals resume from the store at boot (adopt silently, never
	// back-fire — the ADR 0032 stance), so persisting the tick would only
	// manufacture missed-while-down noise.
	reminderNext map[string]time.Time
	// base and cancelGen are the scheduler's lifetime and generation, the
	// automation service's shape.
	base      context.Context
	cancelGen context.CancelFunc
	closed    bool
	// rearm wakes the scheduler loop to recompute its next event after a
	// mutation changed the schedule. Buffered so a mutation never blocks on
	// the loop being mid-fire.
	rearm chan struct{}
}

// NewService opens the thread store at path. Nothing is read until the first
// operation, so construction is free.
func NewService(path string, opts Options, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	s := &Service{
		path:        path,
		windows:     opts.Windows,
		fire:        opts.Fire,
		publish:     opts.Publish,
		midpoint:    opts.Midpoint,
		capture:     opts.Capture,
		summarise:   opts.Summarise,
		classify:    opts.Classify,
		recapBudget: opts.RecapBudget,
		now:         opts.Now,
		timer:       opts.Timer,
		log:         log,
		write:       writeStore,
		rearm:       make(chan struct{}, 1),
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
	s.reminderNext = make(map[string]time.Time)
	// Ids are 1-based; the marks only ever move up from here.
	s.st.nextThread, s.st.nextParked = 1, 1
	return s
}

// Path returns the store file, for logs and doctor to name.
func (s *Service) Path() string { return s.path }

// Bind installs the daemon's halves after construction: the firing path (the
// scheduled-session speech entry) and the live midpoint switch. The service
// is built before the daemon exists — the engine needs its runner — and the
// daemon needs the engine, so this is the capture service's late-bind
// pattern: wired once, single-threaded, before Start ever runs.
func (s *Service) Bind(fire func(ctx context.Context, f Firing), midpoint func() bool) {
	s.fire = fire
	s.midpoint = midpoint
}

// BindRecap installs the AI-session recap's three daemon halves after
// construction, the same late-bind pattern as Bind: wired once,
// single-threaded, before Start ever runs. The capture half reaches the
// desktop and the transcript store (#124, #137), the summarise half reaches
// the provider, and the classify half reads session state for focus.list —
// all of which belong to the daemon.
func (s *Service) BindRecap(capture func(ctx context.Context, a Anchor) (Capture, error),
	summarise func(ctx context.Context, prompt string) (string, error),
	classify func(ctx context.Context, a Anchor, w desktop.Window, trigger string) (string, error)) {
	s.capture = capture
	s.summarise = summarise
	s.classify = classify
}

// ---------------------------------------------------------------- storage

// refreshLocked brings the in-memory store up to date with the file. Callers
// hold s.mu. Every failure degrades: a missing file is an empty store, an
// unreadable or unparseable one is a warning plus an empty store — never an
// error to the caller, never a crash (the ADR 0011 precedent).
func (s *Service) refreshLocked() {
	info, err := os.Stat(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		// Deleting the file is a legitimate hand-edit: deletion is deletion.
		nextThread, nextParked := s.st.nextThread, s.st.nextParked
		s.st = persisted{nextThread: nextThread, nextParked: nextParked}
		s.corrupt = false
		s.loaded, s.mod, s.size = true, time.Time{}, 0
		return
	}
	if err != nil {
		if !s.corrupt {
			s.log.Warn("focus store could not be read; continuing with an empty store",
				"component", "focus", "error", err.Error())
		}
		s.st.threads, s.st.active, s.st.session = nil, "", Session{}
		s.corrupt, s.loaded = true, true
		return
	}
	if s.loaded && info.ModTime().Equal(s.mod) && info.Size() == s.size {
		return // unchanged since last load or write — the common case
	}
	p, err := readStore(s.path)
	s.loaded, s.mod, s.size = true, info.ModTime(), info.Size()
	if err != nil {
		// Warned per corruption event, not per operation: the mtime/size
		// check keeps this branch from re-running until the file changes
		// again, and content never appears in the message.
		s.log.Warn("focus store could not be parsed; continuing with an empty store "+
			"(fix the file by hand — it will not be overwritten)",
			"component", "focus", "path", s.path, "error", err.Error())
		s.st.threads, s.st.active, s.st.session = nil, "", Session{}
		s.corrupt = true
		return
	}
	// The high-water marks ratchet: the persisted values, the highest ids in
	// use, and whatever this Service already promised all hold them up, so a
	// hand-edit that drops them cannot cause an id to be reissued.
	if p.nextThread < s.st.nextThread {
		p.nextThread = s.st.nextThread
	}
	if p.nextParked < s.st.nextParked {
		p.nextParked = s.st.nextParked
	}
	s.st = s.normalize(p)
	s.corrupt = false
	s.log.Debug("focus store loaded", "component", "focus", "threads", len(s.st.threads))
}

// normalize repairs what a hand-edit may have left out: missing or duplicate
// ids get fresh ones, missing timestamps become now, an active pointer at a
// vanished thread clears, anchors past two are trimmed, and a session on an
// unknown thread ends. The repair never fabricates content — an empty name or
// an empty parked thought is dropped, because there is nothing to repair it
// into.
func (s *Service) normalize(p persisted) persisted {
	now := s.now()
	seenThread := make(map[string]bool, len(p.threads))
	seenParked := make(map[string]bool)
	threads := p.threads[:0]
	for _, th := range p.threads {
		th.Name = strings.TrimSpace(th.Name)
		if th.Name == "" {
			continue // a nameless [[thread]] cannot be spoken about
		}
		if th.ID == "" || seenThread[th.ID] {
			th.ID = fmt.Sprintf("t%d", p.nextThread)
			p.nextThread++
		}
		seenThread[th.ID] = true
		if th.Created.IsZero() {
			th.Created = now
		}
		if th.LastActivity.IsZero() {
			th.LastActivity = th.Created
		}
		if th.RemindEveryMin < 0 {
			th.RemindEveryMin = 0
		}
		if th.Recap != RecapAlways && th.Recap != RecapNever {
			// An unrecognised trigger word repairs to the conservative
			// default rather than being guessed at.
			th.Recap = RecapAuto
		}
		if len(th.Anchors) > maxAnchors {
			th.Anchors = th.Anchors[:maxAnchors]
		}
		parked := th.Parked[:0]
		for _, pk := range th.Parked {
			pk.Text = strings.TrimSpace(pk.Text)
			if pk.Text == "" {
				continue
			}
			if pk.ID == "" || seenParked[pk.ID] {
				pk.ID = fmt.Sprintf("p%d", p.nextParked)
				p.nextParked++
			}
			seenParked[pk.ID] = true
			if pk.At.IsZero() {
				pk.At = now
			}
			parked = append(parked, pk)
		}
		th.Parked = parked
		threads = append(threads, th)
	}
	p.threads = threads
	if p.active != "" && !seenThread[p.active] {
		p.active = ""
	}
	if p.session.live() && (!seenThread[p.session.ThreadID] || p.session.Minutes <= 0) {
		p.session = Session{}
	}
	return p
}

// saveLocked writes the store to disk and commits it to memory only on
// success, so a failed write can never leave the Service claiming a state the
// file does not hold. Callers hold s.mu.
func (s *Service) saveLocked(p persisted) error {
	if s.corrupt {
		// The file on disk is one the user may be mid-way through fixing.
		// Move it aside rather than overwrite it: the write proceeds, and
		// the unparseable content survives next to it.
		backup := s.path + ".corrupt"
		if err := os.Rename(s.path, backup); err == nil {
			s.log.Warn("unparseable focus store moved aside before writing",
				"component", "focus", "backup", backup)
		}
		s.corrupt = false
	}
	if err := s.write(s.path, p); err != nil {
		return err
	}
	s.st = p
	// Record the write's own stat so it is not mistaken for a hand-edit and
	// pointlessly re-read on the next operation.
	if info, err := os.Stat(s.path); err == nil {
		s.loaded, s.mod, s.size = true, info.ModTime(), info.Size()
	}
	return nil
}

// clone deep-copies the store so callers can never mutate the Service's
// slices through a returned value.
func clone(p persisted) persisted {
	out := p
	out.threads = make([]Thread, len(p.threads))
	for i, th := range p.threads {
		out.threads[i] = cloneThread(th)
	}
	return out
}

func cloneThread(th Thread) Thread {
	th.Anchors = append([]Anchor(nil), th.Anchors...)
	th.Parked = append([]Parked(nil), th.Parked...)
	return th
}

// -------------------------------------------------------------- resolution

// resolveLocked finds the thread a spoken or typed reference means: an id
// ("t3"), an exact name (case-insensitive, articles dropped), or a name every
// significant word of the reference appears in. Looseness produces ties, and
// a tie is never broken by guessing — several matches is an ErrAmbiguous
// naming the candidates, the window-matching stance (issue #37's honesty
// rule) applied to threads.
func (s *Service) resolveLocked(ref string) (int, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return -1, fmt.Errorf("%w %q", ErrUnknownThread, ref)
	}
	for i, th := range s.st.threads {
		if th.ID == ref {
			return i, nil
		}
	}
	want := nameKey(ref)
	var exact, loose []int
	for i, th := range s.st.threads {
		key := nameKey(th.Name)
		if key == want {
			exact = append(exact, i)
			continue
		}
		if allWordsIn(want, key) {
			loose = append(loose, i)
		}
	}
	winners := exact
	if len(winners) == 0 {
		winners = loose
	}
	switch len(winners) {
	case 1:
		return winners[0], nil
	case 0:
		return -1, fmt.Errorf("%w %q", ErrUnknownThread, strings.TrimSpace(ref))
	default:
		names := make([]string, 0, len(winners))
		for _, i := range winners {
			names = append(names, s.st.threads[i].Name)
		}
		return -1, fmt.Errorf("%w %q: %s", ErrAmbiguous, ref, strings.Join(names, ", "))
	}
}

// nameKey normalises a thread name or reference for comparison: lower case,
// letters and digits only, leading articles dropped — so "the CI refactor",
// "CI refactor" and "ci refactor?" are the same thread.
func nameKey(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte(' ')
		}
	}
	words := strings.Fields(b.String())
	for len(words) > 1 && (words[0] == "the" || words[0] == "a" || words[0] == "my") {
		words = words[1:]
	}
	return strings.Join(words, " ")
}

// allWordsIn reports whether every word of want appears in have as a word
// prefix — "refactor" finding "the big refactoring", never the reverse of a
// single letter claiming everything.
func allWordsIn(want, have string) bool {
	wantWords := strings.Fields(want)
	haveWords := strings.Fields(have)
	if len(wantWords) == 0 {
		return false
	}
	for _, w := range wantWords {
		found := false
		for _, h := range haveWords {
			if strings.HasPrefix(h, w) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// activeLocked returns the active thread's index, or -1.
func (s *Service) activeLocked() int {
	if s.st.active == "" {
		return -1
	}
	for i, th := range s.st.threads {
		if th.ID == s.st.active {
			return i
		}
	}
	return -1
}

// emit publishes one focus.changed event, if anyone is listening. Never
// called with s.mu held — Publish reaches the bus, and a report must never
// hold the store's lock while it waits.
func (s *Service) emit(reason string, extra map[string]any) {
	if s.publish == nil {
		return
	}
	s.mu.Lock()
	data := map[string]any{
		"reason":  reason,
		"active":  s.st.active,
		"session": s.st.session.live(),
	}
	if i := s.activeLocked(); i >= 0 {
		data["active_name"] = s.st.threads[i].Name
	}
	s.mu.Unlock()
	for k, v := range extra {
		data[k] = v
	}
	s.publish("focus.changed", data)
}
