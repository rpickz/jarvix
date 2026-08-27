package focus

import (
	"context"
	"time"
)

// The check-in clockwork: per-thread reminder intervals and the live
// timebox's midpoint and close. One loop, armed to the single next moment
// anything is due, recomputed whenever a mutation changes the schedule — the
// automation scheduler's discipline (ADR 0032) over interval events instead
// of wall-clock ones.
//
// The do-not-nag rule lives in two places by design. Here: while a focus
// session holds the floor, reminder ticks are skipped outright — a monotask
// stretch must not be interrupted by the multitask machinery — and a skipped
// tick reschedules one whole interval out, so ticks can never queue into a
// backlog. In the daemon: the firing is spoken through the scheduled-session
// path, which refuses while any Jarvix session is live or speech is playing,
// and a refused firing is likewise dropped, never retried into a pile-up.

// Start loads the store, reconciles a session that outlived the last daemon,
// and begins the loop. ctx is the service's lifetime: its cancellation
// reaches the loop and every in-flight firing, which is what makes Drain's
// deadline effective.
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

// bootScanLocked reconciles the persisted session with the clock, the
// missed-while-down stance of ADR 0032: a timebox that ran out while no
// daemon was up is reported in the log and closed quietly — never re-fired
// as a voice announcing a session from before the reboot — and one still in
// its window resumes counting with its remaining events intact. Reminder
// intervals adopt silently: each armed thread's next tick is one interval
// from now, because "you missed a check-in while I was off" is a false alarm
// by construction. Callers hold s.mu.
func (s *Service) bootScanLocked() {
	now := s.now()
	if s.st.session.live() {
		expired := s.st.session.Closing && now.Sub(s.st.session.ClosingAt) > closingAnswerWindow
		ranOut := !s.st.session.Closing && !now.Before(s.st.session.end()) &&
			now.Sub(s.st.session.end()) > closingAnswerWindow
		if expired || ranOut {
			next := clone(s.st)
			th, _ := threadByID(next.threads, next.session.ThreadID)
			next.session = Session{}
			if err := s.saveLocked(next); err == nil {
				s.log.Info("focus session ended while the daemon was off; closing quietly, not re-announcing",
					"component", "focus", "thread", th.ID)
			}
		}
	}
	for _, th := range s.st.threads {
		if th.RemindEveryMin > 0 {
			s.reminderNext[th.ID] = now.Add(time.Duration(th.RemindEveryMin) * time.Minute)
		}
	}
}

// Drain stops the clockwork and waits — bounded by ctx — for the loop and
// every in-flight firing to finish, so daemon shutdown treats the service as
// one more drained stage.
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

// Rearm wakes the loop to recompute its next event. Safe from any goroutine;
// a wake already pending is enough, so this never blocks.
func (s *Service) Rearm() {
	select {
	case s.rearm <- struct{}{}:
	default:
	}
}

// run is the loop: find the next due moment, wait for it (or for a rearm),
// dispatch what came due, repeat. Every wait goes through the timer seam and
// every firing through the generation context, so tests drive it
// deterministically and shutdown ends it promptly.
func (s *Service) run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		s.dispatchDue(ctx)
		wait, any := s.nextWait()
		if !any {
			// Nothing scheduled: sleep until a mutation says otherwise.
			select {
			case <-ctx.Done():
				return
			case <-s.rearm:
			}
			continue
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

// nextWait reports how long until the earliest scheduled moment, and whether
// one exists at all.
func (s *Service) nextWait() (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	now := s.now()
	var next time.Time
	consider := func(t time.Time) {
		if !t.IsZero() && (next.IsZero() || t.Before(next)) {
			next = t
		}
	}
	if sess := s.st.session; sess.live() {
		switch {
		case sess.Closing:
			consider(sess.ClosingAt.Add(closingAnswerWindow))
		default:
			if s.midpointEnabled() && !sess.MidpointDue && !sess.MidpointDone {
				consider(sess.midpoint())
			}
			consider(sess.end())
		}
	}
	for _, th := range s.st.threads {
		if th.RemindEveryMin <= 0 {
			continue
		}
		due, armed := s.reminderNext[th.ID]
		if !armed {
			// A reminder that appeared by hand-edit or verb without passing
			// Remind: adopt from now, the boot stance.
			due = now.Add(time.Duration(th.RemindEveryMin) * time.Minute)
			s.reminderNext[th.ID] = due
		}
		consider(due)
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

// dispatchDue fires everything whose moment has arrived. State changes are
// recorded here, under the lock, before any firing is spoken — a firing that
// cannot be spoken is a skipped announcement, never a lost state — and the
// speech itself runs on its own tracked goroutine so the loop keeps timing.
func (s *Service) dispatchDue(ctx context.Context) {
	s.mu.Lock()
	s.refreshLocked()
	now := s.now()
	var firings []Firing
	var events []struct {
		reason string
		data   map[string]any
	}

	if sess := s.st.session; sess.live() {
		switch {
		case sess.Closing && now.Sub(sess.ClosingAt) > closingAnswerWindow:
			// The continue-or-break question went unanswered for the whole
			// window: the user moved on, and the record follows them —
			// quietly, because a lapsed question re-asked an hour later is
			// the nagging this feature promises not to do.
			next := clone(s.st)
			th, _ := threadByID(next.threads, next.session.ThreadID)
			next.session = Session{}
			if err := s.saveLocked(next); err == nil {
				// A quiet end is still an end: check-ins silenced by the
				// lapsed session reschedule a whole interval out rather
				// than firing the moment the record clears.
				s.rescheduleSilencedLocked()
				s.log.Info("focus session close went unanswered; ended quietly",
					"component", "focus", "thread", th.ID)
				events = append(events, struct {
					reason string
					data   map[string]any
				}{"session_ended", map[string]any{"thread": th.ID}})
			}
		case !sess.Closing && !now.Before(sess.end()):
			// Time is up. Closing latches here, at dispatch, not when the
			// prompt is finally spoken: the countdown is over as a fact of
			// the record, and latching first is what stops a busy engine —
			// which refuses the spoken attempt — from turning this branch
			// into a re-firing loop. The prompt itself stays available for
			// the whole answer window, from Tick, however the firing fared.
			next := clone(s.st)
			next.session.Closing, next.session.ClosingAt = true, now
			if err := s.saveLocked(next); err == nil {
				th, _ := threadByID(next.threads, next.session.ThreadID)
				firings = append(firings, Firing{Kind: FiringClose, Thread: cloneThread(th)})
				events = append(events, struct {
					reason string
					data   map[string]any
				}{"session_closing", map[string]any{"thread": th.ID}})
			}
		case !sess.Closing && s.midpointEnabled() && !sess.MidpointDue && !sess.MidpointDone &&
			!now.Before(sess.midpoint()):
			next := clone(s.st)
			next.session.MidpointDue = true
			if err := s.saveLocked(next); err == nil {
				th, _ := threadByID(next.threads, next.session.ThreadID)
				firings = append(firings, Firing{Kind: FiringMidpoint, Thread: cloneThread(th)})
			}
		}
	}

	sessionLive := s.st.session.live()
	for _, th := range s.st.threads {
		if th.RemindEveryMin <= 0 {
			continue
		}
		due, armed := s.reminderNext[th.ID]
		if !armed || now.Before(due) {
			continue
		}
		// Rescheduled from the firing moment, not the completion: an
		// interval is a cadence, never a queue, so a skipped or slow tick
		// costs exactly one tick.
		s.reminderNext[th.ID] = now.Add(time.Duration(th.RemindEveryMin) * time.Minute)
		if sessionLive {
			// Do-not-nag, the monotask half: a live timebox silences every
			// check-in, its own thread's included — the whole point of the
			// stretch is that nothing interrupts it.
			s.log.Info("check-in skipped; a focus session is live",
				"component", "focus", "thread", th.ID)
			continue
		}
		firings = append(firings, Firing{Kind: FiringReminder, Thread: cloneThread(th)})
	}
	s.mu.Unlock()

	for _, ev := range events {
		s.emit(ev.reason, ev.data)
	}
	for _, f := range firings {
		firing := f
		s.group.Go(func() {
			if s.fire != nil {
				s.fire(ctx, firing)
			}
		})
	}
}

// midpointEnabled reads the config seam; nil means the shipped default, off.
func (s *Service) midpointEnabled() bool {
	return s.midpoint != nil && s.midpoint()
}

// rescheduleSilencedLocked applies the do-not-nag rule at the moment a focus
// session leaves the floor: a check-in whose due moment fell inside the
// silenced stretch costs one whole interval from now, exactly as if the loop
// had processed and skipped it mid-session. Without this, a due tick the
// loop had not yet dispatched escapes the silence — the session ends between
// the tick's delivery and its dispatch, and a check-in from inside the
// timebox pours out the instant it closes. Every session-end transition
// (EndSession, Break, an unanswered close, ending the session's thread)
// calls this under the lock, so the first interval heard after a session is
// always a whole one. Callers hold s.mu.
func (s *Service) rescheduleSilencedLocked() {
	now := s.now()
	for _, th := range s.st.threads {
		if th.RemindEveryMin <= 0 {
			continue
		}
		due, armed := s.reminderNext[th.ID]
		if !armed || due.After(now) {
			continue
		}
		s.reminderNext[th.ID] = now.Add(time.Duration(th.RemindEveryMin) * time.Minute)
	}
}
