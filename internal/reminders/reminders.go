// Package reminders owns one-shot spoken reminders (#141, ADR 0046):
// "remind me at three to call the pharmacy", parsed by code, stored in the
// user's own file, spoken once at the right moment, and never nagging.
//
// The package is deliberately the disciplines this repository already
// trusts, restated over a new noun:
//
//   - Storage is the memory book's (ADR 0025): one hand-editable TOML file
//     under the XDG state dir, atomic fsync-and-rename writes, a stat-based
//     change detector so a hand edit lands on the very next operation,
//     normalize-repair for what an edit leaves out, and a corrupt latch that
//     serves an empty store and never overwrites a file mid-fix.
//   - Scheduling is the automation scheduler's (ADR 0032): an injected clock
//     and timer so no test ever sleeps, every goroutine tracked in one
//     quiesce.Group from the moment it exists. It is the THIRD sibling of
//     that scheduler and the focus clockwork's (ADR 0041) — not an
//     extraction — because its events (one-shot moments with an owed-until-
//     delivered contract) fit neither recurring wall-clock rules nor
//     interval cadences.
//   - Time parsing is the intent grammar's (when.go): the same table the
//     router validated the {when} slot against resolves here, against this
//     service's clock, so the router needs no clock and the service needs no
//     second parser.
//
// One deliberate deviation from the siblings' missed-while-down stance, and
// it is the feature: a reminder that came due while nobody could speak it is
// OWED, not dropped. A moment missed behind a live session defers to that
// session's end; a moment missed while the daemon was down fires once at
// boot, marked late ("While I was off: …"). The #136 lesson is applied at
// the mechanism: the owed state changes exactly once, under the store's
// lock, when a delivery session's claim actually runs — so a reminder can
// be late but never lost, and delivered but never doubled.
package reminders

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rpickz/jarvix/internal/intent"
	"github.com/rpickz/jarvix/internal/quiesce"
)

// Reminder is one pending one-shot reminder.
type Reminder struct {
	ID   string
	Text string
	// Due is the resolved moment — next-occurrence arithmetic already done
	// at creation, so the store never holds an ambiguous "three".
	Due     time.Time
	Created time.Time
}

// Fired is one retained history entry: a reminder that fired or was
// cancelled, gone from listings but answerable for "what fired today".
type Fired struct {
	ID      string
	Text    string
	Due     time.Time
	At      time.Time
	Outcome string
	Late    bool
}

// The history outcomes.
const (
	OutcomeFired     = "fired"
	OutcomeCancelled = "cancelled"
)

// maxPending caps the store: past a few dozen simultaneous one-shots the
// feature being used is a task manager, and the refusal names the fix.
const maxPending = 50

// maxHistory caps the fired trail; the oldest entries fall off on write.
const maxHistory = 20

// lateGrace is how far past its moment a delivery may land and still be
// spoken plainly. Past it, the spoken line says the reminder was held —
// the acceptance criteria's two minutes.
const lateGrace = 2 * time.Minute

// maxSpokenList bounds how many reminders one listing reads aloud.
const maxSpokenList = 5

// The Service's refusals, as matchable sentinels (the memory book's
// pattern): callers place the message, the rule lives here.
var (
	// ErrNoText refuses a reminder with nothing to say.
	ErrNoText = errors.New("a reminder needs something to remind you of")
	// ErrBadTime refuses a time expression the table cannot read; the
	// message carries the spoken hint.
	ErrBadTime = errors.New("I couldn't make out the time")
	// ErrUnknownReminder refuses a reference no pending reminder answers to.
	ErrUnknownReminder = errors.New("no reminder matches")
	// ErrAmbiguous refuses a reference several pending reminders answer to.
	ErrAmbiguous = errors.New("more than one reminder matches")
	// ErrStoreFull refuses a store at the pending cap.
	ErrStoreFull = errors.New("the reminder store is full")
)

// Options configure a Service. The seams exist so tests control every
// timestamp and never sleep.
type Options struct {
	// Fire carries out one delivery attempt: start a scheduled session and
	// replay intent.ReminderCheckPhrase through the ordinary session path,
	// whose claim moves the owed reminders under this Service's own lock. It
	// returns false when the floor was refused — a live session or playing
	// speech — and the Service then holds the owed reminders for the next
	// boundary. It blocks until the attempt is over and runs on a goroutine
	// the Service tracks. Nil drops attempts (a daemon built without speech).
	Fire func(ctx context.Context) bool
	// Publish emits reminders.changed / reminders.deferred events for the
	// Automations tab and the activity feed. Nil publishes nothing.
	Publish func(event string, data map[string]any)
	// Now is the clock; Timer creates one shot of it.
	Now   func() time.Time
	Timer func(d time.Duration) (<-chan time.Time, func())
}

// Service owns the reminder store and its clockwork. All methods are safe
// for concurrent use, and every one of them begins by checking whether the
// file changed on disk — a hand-edit is picked up on the very next
// operation, no restart, no watcher.
type Service struct {
	path    string
	fire    func(ctx context.Context) bool
	publish func(string, map[string]any)
	now     func() time.Time
	timer   func(d time.Duration) (<-chan time.Time, func())
	log     *slog.Logger
	// write persists the store; always writeStore outside tests (the memory
	// book's failure-injection seam).
	write func(path string, p persisted) error

	// group tracks the scheduler loop and every in-flight delivery attempt
	// from the moment it exists, never a bare `go` (the #74 lesson).
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
	// deferred marks owed reminders whose one delivery attempt was refused:
	// they wait for a session boundary (FlushOwed) rather than being retried
	// into a pile-up — the do-not-nag rule, the owed-not-dropped variant.
	// In memory only: on disk, "pending with a past due" IS the owed state,
	// which is what makes a crash-then-boot delivery inevitable.
	deferred map[string]bool
	// bootLate marks reminders found already due at Start: their one fire
	// speaks the "While I was off" sentence instead of a plain late one.
	bootLate map[string]bool
	// attempt is true while one delivery attempt is in flight, so the loop
	// and a boundary flush can never race two spoken sessions at each other.
	attempt bool
	// base and cancelGen are the scheduler's lifetime and generation, the
	// automation service's shape.
	base      context.Context
	cancelGen context.CancelFunc
	closed    bool
	// rearm wakes the scheduler loop to recompute its next event after a
	// mutation changed the schedule. Buffered so a mutation never blocks.
	rearm chan struct{}
}

// NewService opens the reminder store at path. Nothing is read until the
// first operation, so construction is free.
func NewService(path string, opts Options, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	s := &Service{
		path:    path,
		fire:    opts.Fire,
		publish: opts.Publish,
		now:     opts.Now,
		timer:   opts.Timer,
		log:     log,
		write:   writeStore,
		rearm:   make(chan struct{}, 1),
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
	s.deferred = make(map[string]bool)
	s.bootLate = make(map[string]bool)
	// Ids are 1-based; the mark only ever moves up from here.
	s.st.nextID = 1
	return s
}

// Path returns the store file, for logs and doctor to name.
func (s *Service) Path() string { return s.path }

// Bind installs the daemon's delivery path after construction — the capture
// service's late-bind pattern: the service is built before the daemon exists
// (the engine's intent runner carries it), wired once, single-threaded,
// before Start ever runs.
func (s *Service) Bind(fire func(ctx context.Context) bool) {
	s.fire = fire
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
		nextID := s.st.nextID
		s.st = persisted{nextID: nextID}
		s.corrupt = false
		s.loaded, s.mod, s.size = true, time.Time{}, 0
		return
	}
	if err != nil {
		if !s.corrupt {
			s.log.Warn("reminder store could not be read; continuing with an empty store",
				"component", "reminders", "error", err.Error())
		}
		s.st.pending, s.st.fired = nil, nil
		s.corrupt, s.loaded = true, true
		return
	}
	if s.loaded && info.ModTime().Equal(s.mod) && info.Size() == s.size {
		return // unchanged since last load or write — the common case
	}
	p, err := readStore(s.path)
	s.loaded, s.mod, s.size = true, info.ModTime(), info.Size()
	if err != nil {
		s.log.Warn("reminder store could not be parsed; continuing with an empty store "+
			"(fix the file by hand — it will not be overwritten)",
			"component", "reminders", "path", s.path, "error", err.Error())
		s.st.pending, s.st.fired = nil, nil
		s.corrupt = true
		return
	}
	// The high-water mark ratchets: the persisted value, the highest id in
	// use, and whatever this Service already promised all hold it up, so a
	// hand-edit that drops it cannot cause an id to be reissued.
	if p.nextID < s.st.nextID {
		p.nextID = s.st.nextID
	}
	s.st = s.normalize(p)
	s.corrupt = false
	s.log.Debug("reminder store loaded", "component", "reminders",
		"pending", len(s.st.pending), "history", len(s.st.fired))
}

// normalize repairs what a hand-edit may have left out: missing or duplicate
// ids get fresh ones, a missing created time becomes now, history outcomes
// repair to "fired", and the history cap holds. The repair never fabricates
// content — a reminder without text or a due time is dropped, because there
// is nothing to repair it into.
func (s *Service) normalize(p persisted) persisted {
	now := s.now()
	// Every id the file mentions, so a repaired duplicate can never be
	// handed an id another entry already answers to.
	inFile := make(map[string]bool, len(p.pending)+len(p.fired))
	for _, r := range p.pending {
		inFile[r.ID] = true
	}
	for _, f := range p.fired {
		inFile[f.ID] = true
	}
	seen := make(map[string]bool, len(inFile))
	fresh := func() string {
		for {
			id := fmt.Sprintf("r%d", p.nextID)
			p.nextID++
			if !inFile[id] && !seen[id] {
				return id
			}
		}
	}
	pending := p.pending[:0]
	for _, r := range p.pending {
		r.Text = strings.TrimSpace(r.Text)
		if r.Text == "" || r.Due.IsZero() {
			continue // nothing to speak, or no moment to speak it
		}
		if r.ID == "" || seen[r.ID] {
			r.ID = fresh()
		}
		seen[r.ID] = true
		if r.Created.IsZero() {
			r.Created = now
		}
		pending = append(pending, r)
	}
	p.pending = pending
	fired := p.fired[:0]
	for _, f := range p.fired {
		f.Text = strings.TrimSpace(f.Text)
		if f.Text == "" {
			continue
		}
		if f.ID == "" || seen[f.ID] {
			f.ID = fresh()
		}
		seen[f.ID] = true
		if f.Outcome != OutcomeCancelled {
			f.Outcome = OutcomeFired
		}
		if f.At.IsZero() {
			f.At = now
		}
		fired = append(fired, f)
	}
	p.fired = fired
	if len(p.fired) > maxHistory {
		p.fired = append([]Fired(nil), p.fired[len(p.fired)-maxHistory:]...)
	}
	if len(p.pending) > maxPending {
		p.pending = append([]Reminder(nil), p.pending[:maxPending]...)
	}
	return p
}

// saveLocked writes the store to disk and commits it to memory only on
// success, so a failed write can never leave the Service claiming a state
// the file does not hold. Callers hold s.mu.
func (s *Service) saveLocked(p persisted) error {
	if len(p.fired) > maxHistory {
		p.fired = append([]Fired(nil), p.fired[len(p.fired)-maxHistory:]...)
	}
	if s.corrupt {
		// The file on disk is one the user may be mid-way through fixing:
		// move it aside rather than overwrite it.
		backup := s.path + ".corrupt"
		if err := os.Rename(s.path, backup); err == nil {
			s.log.Warn("unparseable reminder store moved aside before writing",
				"component", "reminders", "backup", backup)
		}
		s.corrupt = false
	}
	if err := s.write(s.path, p); err != nil {
		return err
	}
	s.st = p
	if info, err := os.Stat(s.path); err == nil {
		s.loaded, s.mod, s.size = true, info.ModTime(), info.Size()
	}
	return nil
}

// mintID takes the next unused id off the high-water mark, skipping any id a
// hand-edit already placed ahead of it — the mark only ever moves up, and an
// id is never reissued.
func mintID(p *persisted) string {
	used := make(map[string]bool, len(p.pending)+len(p.fired))
	for _, r := range p.pending {
		used[r.ID] = true
	}
	for _, f := range p.fired {
		used[f.ID] = true
	}
	for {
		id := fmt.Sprintf("r%d", p.nextID)
		p.nextID++
		if !used[id] {
			return id
		}
	}
}

// clone deep-copies the store so callers can never mutate the Service's
// slices through a returned value.
func clone(p persisted) persisted {
	out := p
	out.pending = append([]Reminder(nil), p.pending...)
	out.fired = append([]Fired(nil), p.fired...)
	return out
}

// ------------------------------------------------------------------ verbs

// Create parses and resolves one spoken time expression, stores the
// reminder, and returns the confirmation — which always says which reading
// of an ambiguous hour won ("Reminding you at three this afternoon: …").
// No confirmation card, no gate: the spoken sentence IS the authorisation,
// and a wrong reminder is undone with one cancel (the ADR 0025 stance).
func (s *Service) Create(when, text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", ErrNoText
	}
	w, ok := intent.ParseWhen(when)
	if !ok {
		return "", fmt.Errorf("%w %q — try a clock time like \"at three\" or \"at 15:00\", "+
			"or a delay like \"in twenty minutes\"", ErrBadTime, strings.TrimSpace(when))
	}
	s.mu.Lock()
	s.refreshLocked()
	if len(s.st.pending) >= maxPending {
		s.mu.Unlock()
		return "", fmt.Errorf("%w — cancel one first", ErrStoreFull)
	}
	now := s.now()
	due, say := w.Resolve(now)
	next := clone(s.st)
	r := Reminder{
		ID:      mintID(&next),
		Text:    text,
		Due:     due,
		Created: now,
	}
	next.pending = append(next.pending, r)
	if err := s.saveLocked(next); err != nil {
		s.mu.Unlock()
		return "", fmt.Errorf("the reminder could not be saved: %w", err)
	}
	s.mu.Unlock()
	s.emit("created", map[string]any{"id": r.ID})
	s.Rearm()
	return "Reminding you " + say + ": " + text + ".", nil
}

// ListSpoken reads the pending reminders, soonest first.
func (s *Service) ListSpoken() string {
	s.mu.Lock()
	s.refreshLocked()
	pending := sortedPending(s.st.pending)
	now := s.now()
	s.mu.Unlock()
	return listSpoken(pending, now)
}

// HistorySpoken answers "what fired today" from the capped history.
func (s *Service) HistorySpoken() string {
	s.mu.Lock()
	s.refreshLocked()
	fired := append([]Fired(nil), s.st.fired...)
	now := s.now()
	s.mu.Unlock()
	return historySpoken(fired, now)
}

// Cancel removes the pending reminder a spoken reference means — an id, or
// enough of its words (fuzzy, the thread-resolution stance: ties are asked
// about, never guessed at). The confirmation names what was cancelled.
func (s *Service) Cancel(ref string) (string, error) {
	s.mu.Lock()
	s.refreshLocked()
	i, err := s.resolveLocked(ref)
	if err != nil {
		s.mu.Unlock()
		return "", err
	}
	next := clone(s.st)
	r := next.pending[i]
	next.pending = append(next.pending[:i], next.pending[i+1:]...)
	next.fired = append(next.fired, Fired{
		ID: r.ID, Text: r.Text, Due: r.Due, At: s.now(), Outcome: OutcomeCancelled,
	})
	if err := s.saveLocked(next); err != nil {
		s.mu.Unlock()
		return "", fmt.Errorf("the cancellation could not be saved: %w", err)
	}
	delete(s.deferred, r.ID)
	delete(s.bootLate, r.ID)
	s.mu.Unlock()
	s.emit("cancelled", map[string]any{"id": r.ID})
	s.Rearm()
	return "Cancelled the reminder: " + r.Text + ".", nil
}

// ClaimDue moves every reminder whose moment has arrived into the fired
// history and returns the one spoken announcement for them all. This is the
// single owed→delivered transition, and it runs under the store's lock at
// the moment the delivery session's phrase dispatches — the #136 lesson
// applied to reminders: however many paths raced to start the session (the
// scheduler's own attempt, a boundary flush, the user saying "reminder
// check"), exactly one claim finds the owed reminders, so a reminder is
// never doubled; and until a claim runs they stay pending on disk, so a
// reminder is never lost.
func (s *Service) ClaimDue() (string, int) {
	s.mu.Lock()
	s.refreshLocked()
	now := s.now()
	next := clone(s.st)
	var claimed []Fired
	var boot []Fired
	pending := next.pending[:0]
	for _, r := range next.pending {
		if r.Due.After(now) {
			pending = append(pending, r)
			continue
		}
		f := Fired{
			ID: r.ID, Text: r.Text, Due: r.Due, At: now, Outcome: OutcomeFired,
			Late: now.Sub(r.Due) > lateGrace,
		}
		if s.bootLate[r.ID] {
			boot = append(boot, f)
		} else {
			claimed = append(claimed, f)
		}
	}
	if len(claimed) == 0 && len(boot) == 0 {
		s.mu.Unlock()
		return "No reminders are due.", 0
	}
	next.pending = pending
	next.fired = append(next.fired, append(boot, claimed...)...)
	if err := s.saveLocked(next); err != nil {
		// The write failed, so the store still owes them: refuse the claim
		// rather than speak a delivery the disk does not record.
		s.mu.Unlock()
		s.log.Warn("reminder claim could not be persisted; still owed",
			"component", "reminders", "error", err.Error())
		return "No reminders are due.", 0
	}
	for _, f := range append(boot, claimed...) {
		delete(s.deferred, f.ID)
		delete(s.bootLate, f.ID)
	}
	spoken := claimSpoken(boot, claimed, now)
	s.mu.Unlock()
	s.emit("fired", map[string]any{"count": len(boot) + len(claimed)})
	s.Rearm()
	return spoken, len(boot) + len(claimed)
}

// resolveLocked finds the pending reminder a reference means: an id ("r3"),
// or words of its text — every significant word of the reference appearing
// in the text as a word prefix. Ties are ErrAmbiguous naming the candidates
// (the window-matching honesty rule); no match is ErrUnknownReminder.
// Callers hold s.mu.
func (s *Service) resolveLocked(ref string) (int, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return -1, fmt.Errorf("%w %q", ErrUnknownReminder, ref)
	}
	for i, r := range s.st.pending {
		if r.ID == ref {
			return i, nil
		}
	}
	want := textKey(ref)
	var exact, loose []int
	for i, r := range s.st.pending {
		key := textKey(r.Text)
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
		return -1, fmt.Errorf("%w %q", ErrUnknownReminder, ref)
	default:
		texts := make([]string, 0, len(winners))
		for _, i := range winners {
			texts = append(texts, s.st.pending[i].Text)
		}
		return -1, fmt.Errorf("%w %q — which one: %s?", ErrAmbiguous, ref, strings.Join(texts, ", or "))
	}
}

// textKey normalises reminder text for comparison: lower case, letters and
// digits only, leading articles dropped (the focus resolution's key).
func textKey(text string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(text) {
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
// prefix — "pharmacy" finding "call the pharmacy", never one letter claiming
// everything.
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

// ----------------------------------------------------------------- views

// View is the IPC snapshot: pending soonest first, history newest first.
type View struct {
	Pending []PendingView
	History []FiredView
}

// PendingView is one pending reminder for the wire, its due moment
// pre-worded on the shared spoken scale (ADR 0013).
type PendingView struct {
	ID        string
	Text      string
	Due       time.Time
	DueSpoken string
	Created   time.Time
}

// FiredView is one history entry for the wire.
type FiredView struct {
	ID      string
	Text    string
	Due     time.Time
	At      time.Time
	Outcome string
	Late    bool
}

// Snapshot reads the store for the Automations tab.
func (s *Service) Snapshot() View {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	now := s.now()
	v := View{}
	for _, r := range sortedPending(s.st.pending) {
		v.Pending = append(v.Pending, PendingView{
			ID: r.ID, Text: r.Text, Due: r.Due,
			DueSpoken: dueSpoken(r.Due, now), Created: r.Created,
		})
	}
	for i := len(s.st.fired) - 1; i >= 0; i-- {
		f := s.st.fired[i]
		v.History = append(v.History, FiredView(f))
	}
	return v
}

// sortedPending orders reminders soonest first without mutating the store.
func sortedPending(pending []Reminder) []Reminder {
	out := append([]Reminder(nil), pending...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Due.Before(out[j].Due) })
	return out
}

// emit publishes one reminders.changed event, if anyone is listening. Never
// called with s.mu held — Publish reaches the bus, and a report must never
// hold the store's lock while it waits (the focus rule).
func (s *Service) emit(reason string, extra map[string]any) {
	if s.publish == nil {
		return
	}
	data := map[string]any{"reason": reason}
	for k, v := range extra {
		data[k] = v
	}
	s.publish("reminders.changed", data)
}
