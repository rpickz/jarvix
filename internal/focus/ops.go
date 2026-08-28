package focus

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rpickz/jarvix/internal/desktop"
)

// The operations: everything a voice phrase or an IPC verb can do to the
// store. Each one follows the same shape — refresh, mutate a copy, persist,
// commit only on a successful write — and returns the one spoken sentence
// the action earns, composed by sentences.go from the record alone.

// Create makes a new thread, optionally anchored to the anchorWindows
// most-recently-focused windows, and makes it active. A duplicate name is a
// refusal, not a second thread: two threads answering to "the refactor"
// would make every later reference a coin toss.
func (s *Service) Create(ctx context.Context, name string, anchorWindows int) (Thread, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Thread{}, "", ErrNoName
	}
	anchors, anchorNote := s.captureAnchors(ctx, anchorWindows)
	s.mu.Lock()
	s.refreshLocked()
	if len(s.st.threads) >= maxThreads {
		s.mu.Unlock()
		return Thread{}, "", fmt.Errorf("%w (%d threads); end something finished first",
			ErrStoreFull, maxThreads)
	}
	key := nameKey(name)
	for _, th := range s.st.threads {
		if nameKey(th.Name) == key {
			s.mu.Unlock()
			return Thread{}, "", fmt.Errorf("a thread called %q already exists; switch to it, or end it first", th.Name)
		}
	}
	now := s.now()
	next := clone(s.st)
	th := Thread{
		ID:      fmt.Sprintf("t%d", next.nextThread),
		Name:    name,
		Created: now, LastSwitched: now, LastActivity: now,
		Anchors: anchors,
	}
	// Bumped before the save on purpose: a failed write may skip an id, but
	// no path can ever reuse one.
	next.nextThread++
	s.st.nextThread = next.nextThread
	next.threads = append(next.threads, th)
	next.active = th.ID
	if err := s.saveLocked(next); err != nil {
		s.mu.Unlock()
		return Thread{}, "", err
	}
	s.mu.Unlock()
	s.log.Info("thread created", "component", "focus", "thread", th.ID,
		"anchors", len(anchors))
	s.emit("created", map[string]any{"thread": th.ID})
	s.Rearm()
	return th, createAck(th, anchorWindows, anchorNote), nil
}

// captureAnchors reads the live inventory and takes the n most-recently
// focused windows as anchors. Degradation is graceful and disclosed: no
// compositor, an error, or fewer windows than asked for all reduce to what
// could be seen, with a note for the acknowledgement — the thread must never
// fail to exist because a window could not be read.
func (s *Service) captureAnchors(ctx context.Context, n int) ([]Anchor, string) {
	if n <= 0 {
		return nil, ""
	}
	if n > maxAnchors {
		n = maxAnchors
	}
	if s.windows == nil {
		return nil, "I cannot see windows on this desktop, so nothing is anchored"
	}
	inventory, err := s.windows(ctx)
	if err != nil || len(inventory) == 0 {
		return nil, "I could not see any windows, so nothing is anchored"
	}
	if n > len(inventory) {
		n = len(inventory)
	}
	anchors := make([]Anchor, 0, n)
	for _, w := range inventory[:n] {
		anchors = append(anchors, Anchor{
			Address: w.Address, StableID: w.StableID,
			App: desktop.AppName(w.Class), Title: strings.TrimSpace(w.Title),
		})
	}
	return anchors, ""
}

// Anchor ties the anchorWindows most-recently-focused windows to the active
// thread, replacing what was anchored before — "anchor this window" is a
// statement about now, not an append.
func (s *Service) Anchor(ctx context.Context, anchorWindows int) (string, error) {
	anchors, note := s.captureAnchors(ctx, anchorWindows)
	s.mu.Lock()
	s.refreshLocked()
	i := s.activeLocked()
	if i < 0 {
		s.mu.Unlock()
		return "", ErrNoActive
	}
	if note != "" {
		name := s.st.threads[i].Name
		s.mu.Unlock()
		return "", fmt.Errorf("%s; %s keeps its anchors", note, name)
	}
	next := clone(s.st)
	next.threads[i].Anchors = anchors
	next.threads[i].LastActivity = s.now()
	th := next.threads[i]
	if err := s.saveLocked(next); err != nil {
		s.mu.Unlock()
		return "", err
	}
	s.mu.Unlock()
	s.log.Info("thread anchored", "component", "focus", "thread", th.ID, "anchors", len(anchors))
	s.emit("anchored", map[string]any{"thread": th.ID})
	return anchorAck(th), nil
}

// Switch makes the referenced thread active and returns its recap — at most
// two sentences, every clause read from the record: last time here, parked
// count and newest, the anchor and whether it still exists. Never invented;
// a thread with no history says "fresh thread". A thread anchored to an AI
// session earns the model-composed recap instead (#124, recap.go): the
// switch itself has already committed by then, so a slow capture or model
// can only ever delay the sentence, never the switch — and only up to the
// recap budget.
func (s *Service) Switch(ctx context.Context, ref string) (Thread, string, error) {
	gone := s.goneAnchors(ctx)
	s.mu.Lock()
	s.refreshLocked()
	i, err := s.resolveLocked(ref)
	if err != nil {
		s.mu.Unlock()
		return Thread{}, "", err
	}
	next := clone(s.st)
	prior := cloneThread(next.threads[i]) // the recap speaks the record as it was
	now := s.now()
	next.threads[i].LastSwitched = now
	next.threads[i].LastActivity = now
	next.active = next.threads[i].ID
	th := next.threads[i]
	if err := s.saveLocked(next); err != nil {
		s.mu.Unlock()
		return Thread{}, "", err
	}
	s.mu.Unlock()
	s.log.Info("thread switched", "component", "focus", "thread", th.ID)
	s.emit("switched", map[string]any{"thread": th.ID})
	base := switchRecap(prior, gone, now)
	return th, s.enrich(ctx, th, base, gone), nil
}

// Park adds a thought to the active thread. The acknowledgement is a soft
// confirm by design — no read-back of the thought, because the point of
// parking is not breaking the stride the thought interrupted.
func (s *Service) Park(text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("there is nothing to park")
	}
	s.mu.Lock()
	s.refreshLocked()
	i := s.activeLocked()
	if i < 0 {
		s.mu.Unlock()
		return "", ErrNoActive
	}
	next := clone(s.st)
	now := s.now()
	pk := Parked{ID: fmt.Sprintf("p%d", next.nextParked), Text: text, At: now}
	next.nextParked++
	s.st.nextParked = next.nextParked
	next.threads[i].Parked = append(next.threads[i].Parked, pk)
	next.threads[i].LastActivity = now
	th := next.threads[i]
	if err := s.saveLocked(next); err != nil {
		s.mu.Unlock()
		return "", err
	}
	s.mu.Unlock()
	s.log.Info("thought parked", "component", "focus", "thread", th.ID,
		"parked", len(th.Parked), "chars", len(text))
	s.emit("parked", map[string]any{"thread": th.ID})
	return "Parked.", nil
}

// ParkedSpoken reads the active thread's parked thoughts, newest first.
func (s *Service) ParkedSpoken() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	i := s.activeLocked()
	if i < 0 {
		return "", ErrNoActive
	}
	return parkedSpoken(s.st.threads[i]), nil
}

// Status speaks one line per thread, active thread first, the rest by most
// recent activity — bounded so the whole answer stays inside ~15 seconds of
// speech however many threads exist.
func (s *Service) Status() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	return statusSpoken(clone(s.st), s.now())
}

// Check speaks the referenced thread's recap without switching to it — the
// same sentence a check-in reminder speaks, because a reminder is exactly
// this question asked by the clock. The AI-session enrichment (#124) applies
// here identically, clock or voice: the check-in is the user's own standing
// question, and the trigger and consent gates hold for it unchanged.
func (s *Service) Check(ctx context.Context, ref string) (string, error) {
	gone := s.goneAnchors(ctx)
	s.mu.Lock()
	s.refreshLocked()
	i, err := s.resolveLocked(ref)
	if err != nil {
		s.mu.Unlock()
		return "", err
	}
	th := cloneThread(s.st.threads[i])
	now := s.now()
	s.mu.Unlock()
	// Enrichment runs off the lock: a capture or a model call must never
	// hold the store against every other operation.
	base := checkRecap(th, gone, now)
	return s.enrich(ctx, th, base, gone), nil
}

// End removes the referenced thread — deletion is deletion, the memory
// book's stance — and says what went with it. An empty ref ends the active
// thread ("end this thread").
func (s *Service) End(ref string) (string, error) {
	s.mu.Lock()
	s.refreshLocked()
	var i int
	var err error
	if strings.TrimSpace(ref) == "" {
		if i = s.activeLocked(); i < 0 {
			s.mu.Unlock()
			return "", ErrNoActive
		}
	} else if i, err = s.resolveLocked(ref); err != nil {
		s.mu.Unlock()
		return "", err
	}
	next := clone(s.st)
	ended := next.threads[i]
	next.threads = append(next.threads[:i], next.threads[i+1:]...)
	if next.active == ended.ID {
		next.active = ""
	}
	endedSession := next.session.ThreadID == ended.ID
	if endedSession {
		next.session = Session{}
	}
	if err := s.saveLocked(next); err != nil {
		s.mu.Unlock()
		return "", err
	}
	delete(s.reminderNext, ended.ID)
	if endedSession {
		// Ending the thread ended its timebox too: the other threads'
		// silenced check-ins reschedule rather than fire at the boundary.
		s.rescheduleSilencedLocked()
	}
	s.mu.Unlock()
	s.log.Info("thread ended", "component", "focus", "thread", ended.ID,
		"parked", len(ended.Parked))
	s.emit("ended", map[string]any{"thread": ended.ID})
	s.Rearm()
	return endAck(ended), nil
}

// goneAnchors reads the live inventory once and returns the set of window
// addresses that still exist, nil when the desktop cannot be read. One
// capture per operation, outside the store lock — a compositor call must
// never hold it.
func (s *Service) goneAnchors(ctx context.Context) map[string]bool {
	live := s.liveWindows(ctx)
	if live == nil {
		return nil
	}
	alive := make(map[string]bool, len(live))
	for address := range live {
		alive[address] = true
	}
	return alive
}

// liveWindows reads the inventory once and indexes it by address, nil when
// the desktop cannot be read. The whole window travels — not just its
// existence — because the session classification (#137) needs the owning
// process and the class to decide whether and where to look for a
// transcript.
func (s *Service) liveWindows(ctx context.Context) map[string]desktop.Window {
	if s.windows == nil {
		return nil
	}
	inventory, err := s.windows(ctx)
	if err != nil {
		return nil
	}
	live := make(map[string]desktop.Window, len(inventory))
	for _, w := range inventory {
		live[w.Address] = w
	}
	return live
}

// ------------------------------------------------------------- the form save

// ThreadForm is one whole thread as a FORM describes it (#164): everything the
// voice path can set about a thread, in one draft, so the window can create or
// edit one without typing four sentences at a microphone.
//
// It exists because a thread's settings were reachable only in pieces and only
// by speaking: "new thread" then a name, "with this window" to anchor, "check
// in every twenty minutes" for the interval — and the recap mode was not
// reachable by voice at all, only by hand-editing focus.json. A form that
// applied those as four separate operations would be four separate writes, and
// a failure between two of them would leave a thread half-configured, which is
// the one thing the ticket forbids. So Save applies the whole draft to one
// cloned store and persists it once.
type ThreadForm struct {
	// Name is the thread's name. Required, and unique the same way Create
	// requires it: two threads answering to one name make every later spoken
	// reference a coin toss.
	Name string
	// AnchorWindows asks for the n most-recently-focused windows as anchors,
	// replacing whatever was anchored — the meaning "anchor this window"
	// already has. nil leaves the anchors exactly as they are, which is what an
	// edit that only renames must do; 0 clears them.
	AnchorWindows *int
	// RemindEveryMin is the check-in interval in minutes, 0 for none.
	RemindEveryMin int
	// Recap is the AI-session recap trigger: RecapAuto, RecapAlways, RecapNever.
	Recap string
}

// Save creates or edits one thread from a form draft, in a single write.
//
// ref "" creates and makes the thread active, exactly as Create does — a new
// thread is what you just started working on. A non-empty ref edits the thread
// it resolves and changes nothing about which thread is active: renaming a
// parked thread must not steal the floor from the one in front of you.
//
// Every rule it enforces is a rule the voice path already enforces, reached
// through the same fields: the name is required and unique (Create), the
// interval cannot be negative (remind), the recap mode is one of three
// (normalize), and the anchors come from the same captureAnchors that "with
// this window" uses — including its graceful degradation, which is reported
// rather than fatal, because a thread must never fail to exist because a
// compositor could not be read.
func (s *Service) Save(ctx context.Context, ref string, form ThreadForm) (Thread, string, error) {
	name := strings.TrimSpace(form.Name)
	if name == "" {
		return Thread{}, "", ErrNoName
	}
	if form.RemindEveryMin < 0 {
		return Thread{}, "", fmt.Errorf("a check-in interval cannot be negative")
	}
	recap := strings.TrimSpace(form.Recap)
	if recap != RecapAuto && recap != RecapAlways && recap != RecapNever {
		return Thread{}, "", fmt.Errorf(
			"recap %q is not a mode; use %q (read a terminal only), %q, or %q",
			form.Recap, RecapAuto, RecapAlways, RecapNever)
	}

	// The window inventory is read OUTSIDE the store lock, like every other
	// operation that needs it: a compositor call must never hold it.
	var anchors []Anchor
	anchorNote := ""
	wanted := 0
	if form.AnchorWindows != nil {
		wanted = *form.AnchorWindows
		anchors, anchorNote = s.captureAnchors(ctx, wanted)
	}

	s.mu.Lock()
	s.refreshLocked()
	creating := strings.TrimSpace(ref) == ""
	i := -1
	if !creating {
		var err error
		if i, err = s.resolveLocked(ref); err != nil {
			s.mu.Unlock()
			return Thread{}, "", err
		}
	} else if len(s.st.threads) >= maxThreads {
		s.mu.Unlock()
		return Thread{}, "", fmt.Errorf("%w (%d threads); end something finished first",
			ErrStoreFull, maxThreads)
	}
	key := nameKey(name)
	for j, th := range s.st.threads {
		if j != i && nameKey(th.Name) == key {
			s.mu.Unlock()
			return Thread{}, "", fmt.Errorf(
				"a thread called %q already exists; switch to it, or end it first", th.Name)
		}
	}

	now := s.now()
	next := clone(s.st)
	if creating {
		th := Thread{
			ID:      fmt.Sprintf("t%d", next.nextThread),
			Name:    name,
			Created: now, LastSwitched: now, LastActivity: now,
		}
		// Bumped before the save on purpose, as Create does it: a failed write
		// may skip an id, but no path can ever reuse one.
		next.nextThread++
		s.st.nextThread = next.nextThread
		next.threads = append(next.threads, th)
		next.active = th.ID
		i = len(next.threads) - 1
	}
	next.threads[i].Name = name
	next.threads[i].RemindEveryMin = form.RemindEveryMin
	next.threads[i].Recap = recap
	next.threads[i].LastActivity = now
	if form.AnchorWindows != nil {
		next.threads[i].Anchors = anchors
	}
	th := next.threads[i]
	if err := s.saveLocked(next); err != nil {
		s.mu.Unlock()
		return Thread{}, "", err
	}
	if form.RemindEveryMin > 0 {
		s.reminderNext[th.ID] = now.Add(time.Duration(form.RemindEveryMin) * time.Minute)
	} else {
		delete(s.reminderNext, th.ID)
	}
	s.mu.Unlock()

	reason := "edited"
	if creating {
		reason = "created"
	}
	s.log.Info("thread saved", "component", "focus", "thread", th.ID,
		"created", creating, "anchors", len(th.Anchors), "every_min", th.RemindEveryMin)
	s.emit(reason, map[string]any{"thread": th.ID})
	s.Rearm()
	return th, saveAck(th, creating, wanted, anchorNote), nil
}
