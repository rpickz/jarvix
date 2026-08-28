// Package overlay composes the tiny top-right window overlays (#127): for
// every window the user has deliberately enrolled — anchored to a focus
// thread (#123) or given a nickname (#126) — one static row saying where the
// window is and what, at most, may be drawn on it: a thread badge (filled
// when its thread is active, hollow otherwise), an optional AI-session state
// (#124/#137), and the nickname tag. Windows enrolled in neither way get no
// row at all: clean by default is an acceptance criterion, not a styling
// preference.
//
// All of the deciding happens here, in Go, where it is tested (ADR 0013):
// which windows earn an overlay, which thread owns a shared anchor, whether
// an overlay would lie about stacking, and when a fullscreen window silences
// a whole workspace. The QML surface (plugin/omarchy/JarvixWindowOverlays.qml)
// draws rows verbatim and decides nothing.
//
// Two honesty rules shape the feed, both worth stating because they are
// deliberate narrowings of the issue's "the overlay tracks the window":
//
//   - Only the focused workspace is overlaid. The compositor seam reports
//     every mapped window but not which workspace each *monitor* is showing,
//     so the one workspace whose visibility is certain is the focused
//     window's own. Overlaying a workspace we merely believe is visible
//     would pin badges over whatever is actually on that monitor.
//   - An overlay must never float over an unrelated covering window. Layer
//     surfaces draw above every toplevel, so the suppression has to be
//     decided from the inventory: a fullscreen window silences its whole
//     workspace, and a floating window (always above the tiled layer)
//     suppresses any overlay whose corner it covers. Between two floating
//     windows the stacking order is not in the inventory at all, so only the
//     focused one — the one raise-on-focus makes knowably topmost — keeps
//     its overlay under an overlap. Suppressed is always the safe answer:
//     these are ambient marks, and a missing mark misleads nobody.
package overlay

import (
	"sort"

	"github.com/rpickz/jarvix/internal/desktop"
)

// The AI-session states the feed will carry (#124). This is the vocabulary
// contract with #137's classifier: exactly these three tokens travel; the
// empty string means "no state", and anything else is treated as unknown and
// dropped before the wire — the QML must never be handed a state it would
// have to invent a colour for.
const (
	StateWorking  = "working"
	StateNeedsYou = "needs_you"
	StateDone     = "done"
)

// RegionWidth and RegionHeight bound the on-screen footprint of one overlay
// chip, in logical pixels: a small strip inside the window's top-right
// corner. They exist for the occlusion decision above — "does that floating
// window cover this overlay?" needs a rectangle to test — and they are the
// contract the QML keeps by clamping its chip to the same box (the constants
// are named in JarvixWindowOverlays.qml). Conservative on purpose: judging
// occlusion against a box slightly larger than the drawn chip can only
// suppress a fraction more, never draw over something.
const (
	RegionWidth  = 280
	RegionHeight = 44
)

// Thread is one focus thread as the feed consumes it — the few facts the
// overlay rules need, restated here rather than importing focus.View so the
// feed's tests stay hermetic and the daemon adapter (internal/daemon) is the
// single place the two shapes meet.
type Thread struct {
	// Name is what the badge stands for; it rides the wire so a future
	// surface can say which thread a badge means without a round trip.
	Name string
	// Active marks the active thread: its badges render filled, the rest
	// hollow.
	Active bool
	// Anchors are the compositor addresses of the thread's anchored windows
	// (at most two, #123). Addresses are matching keys only — they never
	// enter a Row and never travel (ADR 0022).
	Anchors []string
	// AIState is the AI-session classification for this thread's anchored
	// session window: StateWorking, StateNeedsYou, StateDone, or "" for
	// none. This is the slot #137 fills; until the focus payloads carry a
	// state, every caller passes "" and no dot renders anywhere.
	AIState string
}

// Badge is the focus-thread mark on one window.
type Badge struct {
	// Thread is the owning thread's name.
	Thread string `json:"thread"`
	// Active selects the filled rendering; hollow otherwise.
	Active bool `json:"active"`
}

// Row is one window's overlay, exactly as it goes over the wire in
// overlays.get and overlays.changed. Geometry is the window's own rectangle
// in global logical pixels, straight from the compositor seam: the QML pins
// its chip inside the top-right corner and applies no arithmetic beyond
// subtracting its screen's origin. There is deliberately no window address,
// class, or title here — addresses never travel (ADR 0022), and the overlay
// needs to say nothing the enrolment did not already say.
type Row struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
	// Tag is the window's nickname (#126), "" for none (omitted).
	Tag string `json:"tag,omitempty"`
	// Badge is the focus-thread mark, absent for a nickname-only window.
	Badge *Badge `json:"badge,omitempty"`
	// AIState is the AI-session dot: working, needs_you, or done. Absent
	// means no dot — the #137 seam's off state, and the permanent state of
	// every thread that is not an AI session.
	AIState string `json:"ai_state,omitempty"`
}

// Compose decides the whole overlay surface for one moment of desktop state.
// It is pure — inventory, threads, and tags in, rows out — which is what
// makes every rule above testable without a compositor. tags maps window
// address to nickname; rows come back in a stable order (top-left first) so
// callers can compare successive results byte-for-byte.
func Compose(enabled bool, windows []desktop.Window, threads []Thread, tags map[string]string) []Row {
	if !enabled {
		return nil
	}
	// The focused window names the one workspace whose visibility is
	// certain. No focused window — an empty desktop, or an inventory read
	// mid-transition — means no honest answer, and no overlays.
	var focused *desktop.Window
	for i := range windows {
		if windows[i].Focused {
			focused = &windows[i]
			break
		}
	}
	if focused == nil {
		return nil
	}
	workspace := focused.Workspace
	// A fullscreen window covers everything beside it, including whatever an
	// overlay would annotate; and the fullscreen window's own overlay is
	// hidden by the issue's acceptance criteria. Both collapse to: a
	// workspace with a fullscreen window shows nothing.
	for _, w := range windows {
		if w.Workspace == workspace && w.Fullscreen {
			return nil
		}
	}

	badges, states := badgesByAddress(threads)
	var rows []Row
	for _, w := range windows {
		if w.Workspace != workspace {
			continue
		}
		if w.Width <= 0 || w.Height <= 0 {
			// A compositor that reports no geometry reports every window at
			// zero; a chip drawn there would sit in the corner of the wrong
			// monitor. Degrade to nothing, the seam's own stance.
			continue
		}
		badge, hasBadge := badges[w.Address]
		tag := tags[w.Address]
		if !hasBadge && tag == "" {
			continue // unenrolled windows stay completely clean
		}
		if occluded(w, windows, workspace) {
			continue
		}
		row := Row{X: w.X, Y: w.Y, Width: w.Width, Height: w.Height, Tag: tag}
		if hasBadge {
			b := badge
			row.Badge = &b
			row.AIState = states[w.Address]
		}
		rows = append(rows, row)
	}
	// Stable order: top-left first, then the tag as a tiebreaker for the
	// pathological case of two windows sharing a corner. The service's
	// publish-on-change compares whole results, so the order has to be a
	// function of the content, never of map iteration.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Y != rows[j].Y {
			return rows[i].Y < rows[j].Y
		}
		if rows[i].X != rows[j].X {
			return rows[i].X < rows[j].X
		}
		return rows[i].Tag < rows[j].Tag
	})
	return rows
}

// badgesByAddress resolves which thread's badge each anchored window wears.
// Two threads may anchor the same window; the active thread always wins the
// spot, and between inactive ones the first in the given order does — the
// daemon hands threads in the focus snapshot's own order (active first, then
// most recent activity), so the answer is deterministic and matches what the
// Focus tab lists first. The AI state travels with the winning badge: a dot
// without its badge would be a mark on a window the user never enrolled.
func badgesByAddress(threads []Thread) (map[string]Badge, map[string]string) {
	badges := make(map[string]Badge)
	states := make(map[string]string)
	for _, th := range threads {
		for _, addr := range th.Anchors {
			if addr == "" {
				continue
			}
			if held, taken := badges[addr]; taken && (held.Active || !th.Active) {
				continue
			}
			badges[addr] = Badge{Thread: th.Name, Active: th.Active}
			states[addr] = knownState(th.AIState)
		}
	}
	return badges, states
}

// knownState admits exactly the three published states. Anything else —
// including a future token this build has never heard of — becomes "no dot":
// an unknown state must degrade to absence, never to a colour chosen by a
// display surface (the issue's absent/unknown rule).
func knownState(state string) string {
	switch state {
	case StateWorking, StateNeedsYou, StateDone:
		return state
	default:
		return ""
	}
}

// occluded reports whether window w's overlay corner is covered by a window
// that is known to draw above it. Only floating windows can cover anything
// here: tiled windows never overlap each other, and the fullscreen case has
// already silenced the workspace. Hyprland draws the floating layer above
// the tiled one, so a floating overlap always covers a tiled window's
// corner; between two floating windows the inventory carries no stacking
// order, so only the focused one is knowably on top and everything else
// under an overlap is suppressed (the safe direction — see the package
// comment).
func occluded(w desktop.Window, windows []desktop.Window, workspace int) bool {
	rx := w.X + w.Width - min(w.Width, RegionWidth)
	ry := w.Y
	rw := min(w.Width, RegionWidth)
	rh := min(w.Height, RegionHeight)
	for _, o := range windows {
		if o.Address == w.Address || o.Workspace != workspace || !o.Floating {
			continue
		}
		if !intersects(rx, ry, rw, rh, o.X, o.Y, o.Width, o.Height) {
			continue
		}
		if !w.Floating {
			return true // floating draws above tiled, always
		}
		if !w.Focused {
			return true // floating-over-floating order is unknowable
		}
	}
	return false
}

// intersects is axis-aligned rectangle overlap, exclusive of touching edges:
// a floating window sitting exactly beside the corner covers nothing.
func intersects(ax, ay, aw, ah, bx, by, bw, bh int) bool {
	return ax < bx+bw && bx < ax+aw && ay < by+bh && by < ay+ah
}
