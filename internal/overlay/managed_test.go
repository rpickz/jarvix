package overlay

import (
	"context"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/desktop"
)

// The managed mark's half of the feed (#197, ADR 0062). Management is
// enrolment in its own right — a mark that only appeared on windows the user
// had ALSO nicknamed would answer "can Jarvix act in here?" for the wrong
// half of the desktop — while the clean-by-default rule is untouched.

func managedDesktop() []desktop.Window {
	return []desktop.Window{
		{Address: "0xa", Class: "Alacritty", Title: "go test", Workspace: 1,
			Focused: true, X: 0, Y: 0, Width: 800, Height: 600},
		{Address: "0xb", Class: "firefox", Title: "GitHub", Workspace: 1,
			X: 900, Y: 0, Width: 800, Height: 600},
	}
}

func TestAManagedWindowEarnsAChipWithNothingElseOnIt(t *testing.T) {
	rows := Compose(true, managedDesktop(), nil, nil, map[string]bool{"0xa": true})
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want exactly the managed window", rows)
	}
	if !rows[0].Managed {
		t.Errorf("row = %+v, want the managed mark", rows[0])
	}
	if rows[0].Tag != "" || rows[0].Badge != nil {
		t.Errorf("row = %+v, want no tag and no badge — nothing else is enrolled", rows[0])
	}
}

func TestUnmanagedUnenrolledWindowsStayCompletelyClean(t *testing.T) {
	rows := Compose(true, managedDesktop(), nil, nil, map[string]bool{"0xa": true})
	for _, row := range rows {
		if row.X == 900 {
			t.Errorf("the unmanaged window got a chip: %+v", row)
		}
	}
	if rows := Compose(true, managedDesktop(), nil, nil, nil); rows != nil {
		t.Errorf("rows = %+v, want nothing at all when nothing is enrolled", rows)
	}
}

// The mark rides alongside the marks that were already there rather than
// replacing them: a managed, nicknamed, thread-anchored window carries all
// three, because they answer three different questions.
func TestTheManagedMarkSitsBesideTheBadgeAndTheTag(t *testing.T) {
	threads := []Thread{{Name: "ship it", Active: true, Anchors: []string{"0xa"}, AIState: StateWorking}}
	rows := Compose(true, managedDesktop(), threads, map[string]string{"0xa": "builds"},
		map[string]bool{"0xa": true})
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want one", rows)
	}
	row := rows[0]
	if !row.Managed || row.Tag != "builds" || row.Badge == nil || row.AIState != StateWorking {
		t.Errorf("row = %+v, want the managed mark, the tag, the badge and the dot together", row)
	}
}

// The enrolment gate: a managed window keeps the poll awake on its own, so
// the mark converges when the window moves. Nothing managed, nothing named
// and nothing anchored still parks the loop.
func TestTheFeedPollsForAManagedWindowAlone(t *testing.T) {
	windows := managedDesktop()
	reads := 0
	svc := NewService(Options{
		Windows: func(context.Context) ([]desktop.Window, error) {
			reads++
			return windows, nil
		},
		Threads:     func(context.Context) []Thread { return nil },
		Managed:     func([]desktop.Window) map[string]bool { return map[string]bool{"0xa": true} },
		ManagedHeld: func() bool { return true },
		Enabled:     func() bool { return true },
	}, nil)

	rows := svc.Current(context.Background())
	if len(rows) != 1 || !rows[0].Managed {
		t.Fatalf("rows = %+v, want the managed window marked", rows)
	}
	if reads == 0 {
		t.Fatal("the feed should have read the inventory for a managed window")
	}

	parked := NewService(Options{
		Windows: func(context.Context) ([]desktop.Window, error) {
			t.Error("the feed read the compositor with nothing enrolled")
			return nil, nil
		},
		Enabled: func() bool { return true },
	}, nil)
	if rows := parked.Current(context.Background()); rows != nil {
		t.Errorf("rows = %+v, want nothing", rows)
	}
}

// The published payload keeps the managed flag, so the QML surface can draw
// the mark without asking anything.
func TestTheManagedFlagIsPublished(t *testing.T) {
	published := make(chan []Row, 1)
	tick := make(chan time.Time)
	svc := NewService(Options{
		Windows:     func(context.Context) ([]desktop.Window, error) { return managedDesktop(), nil },
		Managed:     func([]desktop.Window) map[string]bool { return map[string]bool{"0xa": true} },
		ManagedHeld: func() bool { return true },
		Enabled:     func() bool { return true },
		Publish:     func(rows []Row) { published <- rows },
		Timer:       func(time.Duration) (<-chan time.Time, func()) { return tick, func() {} },
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Start(ctx)

	select {
	case rows := <-published:
		if len(rows) != 1 || !rows[0].Managed {
			t.Fatalf("published %+v, want the managed window marked", rows)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("nothing was published")
	}
	cancel()
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer drainCancel()
	if err := svc.Drain(drainCtx); err != nil {
		t.Fatalf("Drain: %v", err)
	}
}
