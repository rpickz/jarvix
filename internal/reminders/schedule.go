package reminders

import (
	"context"
	"time"
)

// The one-shot clockwork: one loop, armed to the single next moment any
// pending reminder is due, recomputed whenever a mutation changes the
// schedule — the automation scheduler's discipline (ADR 0032) over one-shot
// moments instead of recurring ones, the focus clockwork's third sibling
// (ADR 0041).
//
// The do-not-nag rule takes its owed variant here (ADR 0046). A moment that
// arrives is given exactly ONE delivery attempt; an attempt the engine
// refuses — a live session, playing speech — parks the reminder as deferred,
// where it waits for a session boundary (FlushOwed) instead of being retried
// into a pile-up. The #136 lesson is applied at the mechanism: whichever
// wake finally wins the floor, the owed→delivered transition happens once,
// in ClaimDue, under the same lock that observed the reminders due — so a
// boundary can release a held reminder but can never double it, and two
// racing wakes can never speak it twice.

// Start loads the store, marks anything already due as a boot catch-up, and
// begins the loop. ctx is the service's lifetime: its cancellation reaches
// the loop and every in-flight attempt, which is what makes Drain's deadline
// effective.
func (s *Service) Start(ctx context.Context) {
	s.mu.Lock()
	if s.closed || s.base != nil {
		s.mu.Unlock()
		return
	}
	s.base = ctx
	s.refreshLocked()
	s.bootScanLocked()
	loopCtx, cancel := context.WithCancel(ctx)
	s.cancelGen = cancel
	// Add before go, never inside the goroutine: a drain that started
	// between the two would otherwise return while the loop was starting.
	s.group.Go(func() { s.run(loopCtx) })
	s.mu.Unlock()
}

// bootScanLocked reconciles the persisted store with the clock. This is the
// one deliberate deviation from the siblings' adopt-never-refire stance, and
// it is the feature's promise: a reminder that came due while no daemon ran
// is OWED — it fires once at boot, marked as the "While I was off" catch-up,
// never silently dropped and never a backlog storm (however many were
// missed, they arrive as one spoken announcement). Callers hold s.mu.
func (s *Service) bootScanLocked() {
	now := s.now()
	for _, r := range s.st.pending {
		if !r.Due.After(now) {
			s.bootLate[r.ID] = true
			s.log.Info("reminder came due while the daemon was off; firing once at boot, marked late",
				"component", "reminders", "id", r.ID,
				"due", r.Due.UTC().Format(time.RFC3339))
		}
	}
}

// Drain stops the clockwork and waits — bounded by ctx — for the loop and
// every in-flight attempt to finish, so daemon shutdown treats the service
// as one more drained stage.
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

// InFlight reports how many service goroutines are still running, for the
// shutdown log when a drain gives up.
func (s *Service) InFlight() int { return s.group.InFlight() }

// Rearm wakes the loop to recompute its next event. Safe from any
// goroutine; a wake already pending is enough, so this never blocks.
func (s *Service) Rearm() {
	select {
	case s.rearm <- struct{}{}:
	default:
	}
}

// FlushOwed is the boundary half of the owed contract: the daemon calls it
// when a session finishes or is cancelled, and every reminder parked as
// deferred becomes attemptable again — one more try, at the exact moment
// the floor is most likely free. A no-op when nothing is owed.
func (s *Service) FlushOwed() {
	s.mu.Lock()
	if len(s.deferred) == 0 {
		s.mu.Unlock()
		return
	}
	s.deferred = make(map[string]bool)
	s.mu.Unlock()
	s.Rearm()
}

// Owed reports how many reminders are past due and undelivered — for the
// daemon's boundary watcher and for tests to observe without racing.
func (s *Service) Owed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	now := s.now()
	n := 0
	for _, r := range s.st.pending {
		if !r.Due.After(now) {
			n++
		}
	}
	return n
}

// idleSweep is the longest the loop will go without looking at the store when
// nothing at all is schedulable. It is not a poll for due moments — every due
// moment has an arm of its own — it is the store's hand-edit promise applied
// to the one reader with no other way of hearing about a change (ADR 0049).
const idleSweep = time.Minute

// run is the loop: find the next due moment, wait for it (or for a rearm),
// dispatch what came due, repeat. Every wait goes through the timer seam
// and every attempt through the generation context, so tests drive it
// deterministically and shutdown ends it promptly.
//
// The loop is **always armed**. This scheduler shipped with a park branch —
// "nothing schedulable, sleep until somebody pokes me" — which the focus
// sibling had already been cured of (ADR 0049, #152) before this one was
// written; the two landed in parallel and the lesson missed the merge. The
// same two failures follow from a park, and both are real here:
//
//   - The loop stops reading the store. Every other entry point refreshes
//     from disk on its way in, which is what makes `reminders.toml`
//     hand-editable without a restart. A parked loop is the one reader that
//     never returns, so a reminder added by hand while nothing else is
//     pending arms nothing until some unrelated verb happens to Rearm.
//   - "Armed" becomes unobservable: a park drops `<-fire` from the select, so
//     a tick delivered to a parked loop is never received, and a caller
//     holding the timer waits forever rather than late.
//
// Deferred reminders still belong to a boundary, not to a timer — that
// stance is unchanged and lives where it is enforced, in nextWait (which
// skips them) and dispatchDue (which will not attempt them). Waking every
// minute cannot fire one early; it only lets the loop notice the file.
func (s *Service) run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		s.dispatchDue(ctx)
		wait, any := s.nextWait()
		if !any {
			wait = idleSweep
		}
		fire, stop := s.timer(wait)
		select {
		case <-ctx.Done():
			stop()
			return
		case <-s.rearm:
			stop()
		case <-fire:
		}
	}
}

// nextWait reports how long until the earliest attemptable moment, and
// whether one exists at all. A deferred reminder has no moment: it already
// had its attempt, and only a boundary re-arms it.
func (s *Service) nextWait() (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	now := s.now()
	var next time.Time
	for _, r := range s.st.pending {
		if s.deferred[r.ID] {
			continue
		}
		if next.IsZero() || r.Due.Before(next) {
			next = r.Due
		}
	}
	if next.IsZero() {
		return 0, false
	}
	wait := next.Sub(now)
	if wait < 0 {
		wait = 0
	}
	return wait, true
}

// dispatchDue starts one delivery attempt when any reminder's moment has
// arrived. The attempt itself is the daemon's — a scheduled session
// replaying the check phrase, whose claim runs under this Service's lock —
// and it runs on its own tracked goroutine so the loop keeps timing. One
// attempt at a time (attempt latches under the lock), and whatever an
// attempt leaves owed is parked as deferred under the same lock that
// observes the outcome: refused or interrupted, the next try belongs to a
// boundary, never to a retry loop.
func (s *Service) dispatchDue(ctx context.Context) {
	s.mu.Lock()
	s.refreshLocked()
	now := s.now()
	owed := false
	for _, r := range s.st.pending {
		if !r.Due.After(now) && !s.deferred[r.ID] {
			owed = true
			break
		}
	}
	if !owed || s.attempt || s.fire == nil {
		s.mu.Unlock()
		return
	}
	s.attempt = true
	s.mu.Unlock()

	s.group.Go(func() {
		delivered := s.fire(ctx)
		s.mu.Lock()
		s.attempt = false
		now := s.now()
		var stillOwed, parked int
		for _, r := range s.st.pending {
			if r.Due.After(now) || s.deferred[r.ID] {
				continue
			}
			stillOwed++
			if !delivered {
				s.deferred[r.ID] = true
				parked++
			}
		}
		s.mu.Unlock()
		switch {
		case parked > 0:
			// The floor was refused — a session is live or speech is playing
			// (the engine's word, not a guess here). Parked for the boundary,
			// never retried into a pile-up.
			s.log.Info("reminder delivery deferred; a session is live or speech is playing",
				"component", "reminders", "count", parked)
			s.emit("deferred", map[string]any{"count": parked})
		case delivered && stillOwed > 0:
			// The delivery session ran but its claim left these behind — it
			// was interrupted before the claim dispatched. Its own boundary
			// event has already raced past this bookkeeping, so parking now
			// could wait forever; try once more instead. No spin: another
			// round requires another successful start and another fresh
			// interruption, and an attempt the interrupter's session refuses
			// lands in the parked branch above.
			s.log.Info("reminder delivery was interrupted before its claim; trying again",
				"component", "reminders", "count", stillOwed)
			s.Rearm()
		}
	})
}
