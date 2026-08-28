package desktop

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rpickz/jarvix/internal/placement"
)

// Placer applies the window-placement vocabulary (internal/placement, ADR
// 0056) to a real window through the compositor seam. It is the *only*
// implementation of "what does mode = pinned actually dispatch?", and that is
// the point: the routine runner and the window tools both place windows, and
// before this existed they would have had to agree by copying each other.
//
// Two properties are the design:
//
//   - Every mode is expressed as the whole state, never as a difference from
//     whatever the window happens to be in. A floating placement also unpins;
//     a tiled placement also leaves fullscreen. That is what makes a routine's
//     second run land where its first did (ADR 0026's set-not-toggle rule)
//     even when the user moved things by hand in between.
//   - A refusal is an error, not a shrug. Hyprland answers "ok" with exit
//     status zero for a dispatch it declined to act on, so the seam judges the
//     reply rather than the status (runDispatch) and every step of a placement
//     is checked. A step reported as placed when part of it was refused is the
//     defect this whole family of code was rewritten to end (#177).
//
// Sizing a TILED window is deliberately not part of Apply. A tiled size moves
// the split the window sits in, which only means anything once the windows it
// shares that split with exist, so it is a separate call (Proportion) the
// caller makes after everything is open.
type Placer struct {
	// Comp is the seam. Required.
	Comp Compositor
	// Timeout bounds one dispatch. Zero means DefaultCompositorTimeout.
	Timeout time.Duration
}

// call runs one dispatch under the per-call bound, refusing to start once the
// caller's context is done so a cancelled run stops mid-placement rather than
// after it.
func (p Placer) call(ctx context.Context, f func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = DefaultCompositorTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return f(callCtx)
}

// Apply puts one window where a placement says, against the monitor the
// caller resolved for it. It does not move the workspace between monitors —
// that is a decision about a whole workspace and belongs to whoever is
// placing the group (see MoveWorkspaceToMonitor).
func (p Placer) Apply(ctx context.Context, w Window, want placement.Placement, mon placement.Monitor) error {
	if want.Workspace != 0 {
		if err := p.call(ctx, func(c context.Context) error {
			return p.Comp.MoveToWorkspace(c, w.Address, want.Workspace)
		}); err != nil {
			return err
		}
	}
	if err := p.applyMode(ctx, w, want, mon); err != nil {
		return err
	}
	if want.Master {
		if err := p.call(ctx, func(c context.Context) error {
			return p.Comp.PromoteMaster(c, w.Address)
		}); err != nil {
			return err
		}
	}
	if want.Focus == placement.FocusFollow {
		return p.call(ctx, func(c context.Context) error { return p.Comp.Focus(c, w.Address) })
	}
	return nil
}

// applyMode puts the window into the placement's mode, as a whole state.
func (p Placer) applyMode(ctx context.Context, w Window, want placement.Placement,
	mon placement.Monitor) error {
	if want.Mode == "" {
		return nil
	}
	float := func(on bool) error {
		return p.call(ctx, func(c context.Context) error { return p.Comp.SetFloating(c, w.Address, on) })
	}
	pin := func(on bool) error {
		return p.call(ctx, func(c context.Context) error { return p.Comp.SetPinned(c, w.Address, on) })
	}
	full := func(mode FullscreenMode, on bool) error {
		return p.call(ctx, func(c context.Context) error {
			return p.Comp.SetFullscreen(c, w.Address, mode, on)
		})
	}
	switch want.Mode {
	case placement.ModeTiled:
		if err := full(FullscreenWhole, false); err != nil {
			return err
		}
		return float(false)
	case placement.ModeFloating, placement.ModePinned:
		if err := full(FullscreenWhole, false); err != nil {
			return err
		}
		if err := float(true); err != nil {
			return err
		}
		// Size before position: a compositor that keeps a floating window
		// inside its monitor would otherwise nudge it back after the move.
		if size, problems := want.ResolveSize(mon); len(problems) == 0 && size.Set() {
			if err := p.Resize(ctx, w, size); err != nil {
				return err
			}
		}
		if want.HasPosition {
			if err := p.call(ctx, func(c context.Context) error {
				return p.Comp.PositionWindow(c, w.Address, want.X, want.Y)
			}); err != nil {
				return err
			}
		}
		return pin(want.Mode == placement.ModePinned)
	case placement.ModeFullscreen:
		return full(FullscreenWhole, true)
	case placement.ModeMaximised:
		return full(FullscreenMaximised, true)
	}
	// Unreachable: the mode was validated before it got here. A sentence
	// rather than a silent skip, because a mode nobody honours must never be
	// reported as a window that was placed.
	return fmt.Errorf("%q is not a placement mode", want.Mode)
}

// Resize sets a window's size, filling in the axis the placement did not
// mention from the window's own current extent — the compositor's resize verb
// wants both numbers, so "leave the height alone" has to be spelled as the
// height it already has.
func (p Placer) Resize(ctx context.Context, w Window, size placement.Size) error {
	width, height := size.Width, size.Height
	if width <= 0 {
		width = w.Width
	}
	if height <= 0 {
		height = w.Height
	}
	if width <= 0 || height <= 0 {
		return fmt.Errorf("the window manager reports no size for this window, so it cannot be resized")
	}
	return p.call(ctx, func(c context.Context) error {
		return p.Comp.ResizeWindow(c, w.Address, width, height)
	})
}

// Proportion applies a tiled window's share of its workspace, once the
// windows it shares a split with exist.
//
// The window is focused first. On a tiled window an exact resize moves the
// split rather than the window, and the empirical shape this was written from
// — the shell script the user was reduced to before this vocabulary existed —
// focused before resizing. Keeping that is worth more than saving a dispatch:
// the point of this change is that placement stops being something we hope
// works.
func (p Placer) Proportion(ctx context.Context, w Window, want placement.Placement,
	mon placement.Monitor) error {
	size, problems := want.ResolveSize(mon)
	if len(problems) > 0 {
		return fmt.Errorf("%s", problems[0].Message)
	}
	if !size.Set() {
		return nil
	}
	if err := p.call(ctx, func(c context.Context) error { return p.Comp.Focus(c, w.Address) }); err != nil {
		return err
	}
	return p.Resize(ctx, w, size)
}

// Preselect tells the layout where the NEXT window goes, by focusing this one
// and sending the layout message — which carries no window selector on either
// dialect, so becoming the window is the only way to name it.
//
// It is dispatched between launches rather than after them, and that is the
// mechanism rather than a detail: a dwindle-family layout decides where a
// window lands at the moment it maps and never revisits it, so an arrangement
// asked for once everything is open is an arrangement that cannot happen.
func (p Placer) Preselect(ctx context.Context, w Window, dir placement.PlaceNext) error {
	if dir == placement.PlaceNextNone {
		return nil
	}
	direction, ok := preselectDirections[dir]
	if !ok {
		return fmt.Errorf("%q is not a direction for the next window", dir)
	}
	if err := p.call(ctx, func(c context.Context) error { return p.Comp.Focus(c, w.Address) }); err != nil {
		return err
	}
	return p.call(ctx, func(c context.Context) error { return p.Comp.Preselect(c, direction) })
}

// preselectDirections translates the vocabulary's arrangement words into the
// compositor's single-letter spelling. One table, in one place, so the words
// a user writes and the letters a compositor takes cannot drift apart.
var preselectDirections = map[placement.PlaceNext]PreselectDirection{
	placement.PlaceNextRight: PreselectRight,
	placement.PlaceNextLeft:  PreselectLeft,
	placement.PlaceNextBelow: PreselectDown,
	placement.PlaceNextAbove: PreselectUp,
}

// PlacementSentence returns the part of a placement error that was written
// for a person to hear, or "" when the error is a compositor diagnostic that
// belongs in the log and nowhere else.
//
// The distinction is the seam's own: two refusals — a layout with no master
// pane, a preselection on a layout that has none — are rewritten into the
// vocabulary's words as they leave the seam, and a monitor that is not
// attached is named by the resolver. Everything else Hyprland says is
// operator material.
func PlacementSentence(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	for _, known := range placementSentences {
		if i := strings.Index(msg, known); i >= 0 {
			return msg[i:]
		}
	}
	return ""
}

// placementSentences are the prefixes that mark an error as speakable. They
// are values rather than string literals at the point of use so the seam and
// the callers cannot disagree about the wording.
var placementSentences = []string{
	placement.MasterUnsupported,
	"this workspace's layout cannot arrange windows that way",
	"no monitor is called",
	"the window manager reports no monitors",
	"I cannot see which screens are attached",
}
