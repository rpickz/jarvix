package focus

import (
	"context"
	"fmt"
	"time"
)

// The timeboxed focus session (monotasking) and the per-thread check-in
// interval. One session at a time, riding the same store as the threads so a
// daemon restart mid-timebox resumes the countdown instead of forgetting it.

// StartSession begins a timebox on the referenced thread, which also becomes
// the active thread — focusing on something *is* switching to it. A session
// already running is replaced: the new stretch is the user's word that the
// old one is over.
func (s *Service) StartSession(ctx context.Context, ref string, minutes int) (string, error) {
	if minutes <= 0 {
		return "", fmt.Errorf("a focus session needs a length in minutes")
	}
	s.mu.Lock()
	s.refreshLocked()
	i, err := s.resolveLocked(ref)
	if err != nil {
		s.mu.Unlock()
		return "", err
	}
	next := clone(s.st)
	now := s.now()
	next.threads[i].LastSwitched = now
	next.threads[i].LastActivity = now
	next.active = next.threads[i].ID
	next.session = Session{ThreadID: next.threads[i].ID, Started: now, Minutes: minutes}
	th := next.threads[i]
	if err := s.saveLocked(next); err != nil {
		s.mu.Unlock()
		return "", err
	}
	s.mu.Unlock()
	s.log.Info("focus session started", "component", "focus",
		"thread", th.ID, "minutes", minutes)
	s.emit("session_started", map[string]any{"thread": th.ID, "minutes": minutes})
	s.Rearm()
	return sessionStartAck(th, minutes), nil
}

// Tick reports the live session — remaining time mid-way, the midpoint line
// when its firing latched one, the continue-or-break prompt once time is up.
// The scheduler's firings and a spoken "focus session update" both land
// here, so the clock and the user always hear the same sentence for the
// same state.
func (s *Service) Tick() (string, error) {
	s.mu.Lock()
	s.refreshLocked()
	if !s.st.session.live() {
		s.mu.Unlock()
		return "", ErrNoSession
	}
	next := clone(s.st)
	sess := &next.session
	th, ok := threadByID(next.threads, sess.ThreadID)
	if !ok {
		// normalize prevents this; belt and braces for a mid-operation edit.
		s.mu.Unlock()
		return "", ErrNoSession
	}
	now := s.now()
	switch {
	case sess.Closing || !now.Before(sess.end()):
		// Time is up (or the close already latched and went unanswered):
		// the prompt repeats until answered or the window lapses — asking
		// again is honest, silently expiring mid-question is not.
		line := closePrompt(th, sess.Minutes)
		if !sess.Closing {
			sess.Closing, sess.ClosingAt = true, now
			if err := s.saveLocked(next); err != nil {
				s.mu.Unlock()
				return "", err
			}
			s.mu.Unlock()
			s.emit("session_closing", map[string]any{"thread": th.ID})
			s.Rearm()
			return line, nil
		}
		s.mu.Unlock()
		return line, nil
	case sess.MidpointDue && !sess.MidpointDone:
		sess.MidpointDone = true
		line := midpointLine(th, now, sess.end())
		if err := s.saveLocked(next); err != nil {
			s.mu.Unlock()
			return "", err
		}
		s.mu.Unlock()
		return line, nil
	default:
		line := remainingLine(th, now, sess.end())
		s.mu.Unlock()
		return line, nil
	}
}

// EndSession ends the timebox early, by voice or by verb. It works in every
// phase — counting down or waiting on continue-or-break — because "end the
// focus session" must always mean what it says.
func (s *Service) EndSession() (string, error) {
	return s.clearSession(func(th Thread, elapsed int) string {
		return endSessionAck(th, elapsed)
	})
}

// Break answers the close prompt (or ends a live session) with a rest.
func (s *Service) Break() (string, error) {
	return s.clearSession(func(th Thread, _ int) string {
		return breakAck(th)
	})
}

// clearSession removes the session record and speaks about it.
func (s *Service) clearSession(ack func(th Thread, elapsedMinutes int) string) (string, error) {
	s.mu.Lock()
	s.refreshLocked()
	if !s.st.session.live() {
		s.mu.Unlock()
		return "", ErrNoSession
	}
	next := clone(s.st)
	th, _ := threadByID(next.threads, next.session.ThreadID)
	elapsed := int(s.now().Sub(next.session.Started) / time.Minute)
	if max := next.session.Minutes; elapsed > max {
		elapsed = max // a session that sat closing did not "run" the wait
	}
	next.session = Session{}
	if err := s.saveLocked(next); err != nil {
		s.mu.Unlock()
		return "", err
	}
	// The session has left the floor: a check-in whose due moment fell
	// inside it is rescheduled a whole interval out, never fired at the
	// boundary (the do-not-nag rule's edge, rescheduleSilencedLocked).
	s.rescheduleSilencedLocked()
	s.mu.Unlock()
	s.log.Info("focus session ended", "component", "focus",
		"thread", th.ID, "elapsed_min", elapsed)
	s.emit("session_ended", map[string]any{"thread": th.ID})
	s.Rearm()
	return ack(th, elapsed), nil
}

// Continue answers the close prompt with another round: same thread, same
// minutes. It only applies while the prompt is live — a session still
// counting down needs no continuing, and none at all has nothing to
// continue.
func (s *Service) Continue() (string, error) {
	s.mu.Lock()
	s.refreshLocked()
	if !s.st.session.live() {
		s.mu.Unlock()
		return "", ErrNoSession
	}
	if !s.st.session.Closing {
		th, _ := threadByID(s.st.threads, s.st.session.ThreadID)
		line := remainingLine(th, s.now(), s.st.session.end())
		s.mu.Unlock()
		return line, nil
	}
	next := clone(s.st)
	now := s.now()
	minutes := next.session.Minutes
	next.session = Session{ThreadID: next.session.ThreadID, Started: now, Minutes: minutes}
	th, _ := threadByID(next.threads, next.session.ThreadID)
	i, _ := threadIndexByID(next.threads, th.ID)
	next.threads[i].LastActivity = now
	if err := s.saveLocked(next); err != nil {
		s.mu.Unlock()
		return "", err
	}
	s.mu.Unlock()
	s.log.Info("focus session continued", "component", "focus",
		"thread", th.ID, "minutes", minutes)
	s.emit("session_started", map[string]any{"thread": th.ID, "minutes": minutes})
	s.Rearm()
	return continueAck(th, minutes), nil
}

// Remind sets the active thread's check-in interval; 0 clears it. The
// engine's grammar only produces positive minutes — 0 arrives from the verb
// surface and the RemindStop phrase.
func (s *Service) Remind(minutes int) (string, error) {
	return s.remind("", minutes)
}

// RemindThread is Remind for a named or id'd thread — the Focus tab edits
// any row, not just the active one. Same rule, different resolution.
func (s *Service) RemindThread(ref string, minutes int) (string, error) {
	return s.remind(ref, minutes)
}

func (s *Service) remind(ref string, minutes int) (string, error) {
	if minutes < 0 {
		return "", fmt.Errorf("a check-in interval cannot be negative")
	}
	s.mu.Lock()
	s.refreshLocked()
	var i int
	var err error
	if ref == "" {
		if i = s.activeLocked(); i < 0 {
			s.mu.Unlock()
			return "", ErrNoActive
		}
	} else if i, err = s.resolveLocked(ref); err != nil {
		s.mu.Unlock()
		return "", err
	}
	next := clone(s.st)
	next.threads[i].RemindEveryMin = minutes
	next.threads[i].LastActivity = s.now()
	th := next.threads[i]
	if err := s.saveLocked(next); err != nil {
		s.mu.Unlock()
		return "", err
	}
	if minutes > 0 {
		s.reminderNext[th.ID] = s.now().Add(time.Duration(minutes) * time.Minute)
	} else {
		delete(s.reminderNext, th.ID)
	}
	s.mu.Unlock()
	s.log.Info("thread check-in changed", "component", "focus",
		"thread", th.ID, "every_min", minutes)
	reason := "reminder_set"
	if minutes == 0 {
		reason = "reminder_cleared"
	}
	s.emit(reason, map[string]any{"thread": th.ID})
	s.Rearm()
	if minutes == 0 {
		return remindStopAck(th), nil
	}
	return remindAck(th, minutes), nil
}

// RemindStop clears the active thread's check-in interval.
func (s *Service) RemindStop() (string, error) {
	return s.Remind(0)
}

// CheckByID is Check for callers holding an id (the reminder firing), which
// must never re-resolve by name: the thread the user configured is the
// thread that speaks, even if another has since taken a similar name.
func (s *Service) CheckByID(ctx context.Context, id string) (string, error) {
	return s.Check(ctx, id)
}

func threadByID(threads []Thread, id string) (Thread, bool) {
	if i, ok := threadIndexByID(threads, id); ok {
		return threads[i], true
	}
	return Thread{}, false
}

func threadIndexByID(threads []Thread, id string) (int, bool) {
	for i, th := range threads {
		if th.ID == id {
			return i, true
		}
	}
	return -1, false
}
