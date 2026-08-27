package focus

import (
	"context"
	"fmt"
	"strings"

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
	if next.session.ThreadID == ended.ID {
		next.session = Session{}
	}
	if err := s.saveLocked(next); err != nil {
		s.mu.Unlock()
		return "", err
	}
	delete(s.reminderNext, ended.ID)
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
	if s.windows == nil {
		return nil
	}
	inventory, err := s.windows(ctx)
	if err != nil {
		return nil
	}
	alive := make(map[string]bool, len(inventory))
	for _, w := range inventory {
		alive[w.Address] = true
	}
	return alive
}
