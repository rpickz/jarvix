package focus

import (
	"context"
	"sort"
	"time"

	"github.com/rpickz/jarvix/internal/knowledge"
)

// The Focus tab's read surface. Everything the window shows is decided here,
// daemon-side (ADR 0013): the ordering (active first, then most recent
// activity), the spoken-style ages, and whether an anchor is gone — the tab
// renders fields and invents nothing.

// View is one whole snapshot of the store for focus.list.
type View struct {
	Threads []ThreadView
	// Active is the active thread's id, "" when none.
	Active string
	// Session is the live focus session, nil when none runs.
	Session *SessionView
}

// ThreadView is one thread with its display-only annotations.
type ThreadView struct {
	Thread
	Active bool
	// AnchorsGone marks, per anchor, a window that no longer exists. All
	// false when the desktop could not be read — an unreadable inventory is
	// never reported as a vanished window.
	AnchorsGone []bool
	// LastActivitySpoken is the shared spoken-style age ("four minutes
	// ago"), one scale for every surface.
	LastActivitySpoken string
}

// SessionView is the live timebox as the tab and the bar's activity ring
// show it.
type SessionView struct {
	ThreadID   string
	ThreadName string
	Started    time.Time
	Minutes    int
	// Phase is "running" while the timebox counts down, "closing" while the
	// continue-or-break question is on the floor.
	Phase string
	// RemainingSec is the countdown, 0 once closing.
	RemainingSec int
}

// Snapshot reads the whole store for display, anchor liveness included.
func (s *Service) Snapshot(ctx context.Context) View {
	alive := s.goneAnchors(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	now := s.now()
	st := clone(s.st)

	ordered := st.threads
	sort.SliceStable(ordered, func(i, j int) bool {
		if (ordered[i].ID == st.active) != (ordered[j].ID == st.active) {
			return ordered[i].ID == st.active
		}
		return ordered[i].LastActivity.After(ordered[j].LastActivity)
	})

	view := View{Active: st.active, Threads: make([]ThreadView, 0, len(ordered))}
	for _, th := range ordered {
		tv := ThreadView{
			Thread:             th,
			Active:             th.ID == st.active,
			LastActivitySpoken: knowledge.SpokenAge(now, th.LastActivity),
		}
		if len(th.Anchors) > 0 {
			tv.AnchorsGone = make([]bool, len(th.Anchors))
			if alive != nil {
				for i, a := range th.Anchors {
					tv.AnchorsGone[i] = !alive[a.Address]
				}
			}
		}
		view.Threads = append(view.Threads, tv)
	}

	if st.session.live() {
		th, _ := threadByID(st.threads, st.session.ThreadID)
		sv := SessionView{
			ThreadID: st.session.ThreadID, ThreadName: th.Name,
			Started: st.session.Started, Minutes: st.session.Minutes,
			Phase: "running",
		}
		if st.session.Closing || !now.Before(st.session.end()) {
			sv.Phase = "closing"
		} else {
			sv.RemainingSec = int(st.session.end().Sub(now) / time.Second)
		}
		view.Session = &sv
	}
	return view
}
