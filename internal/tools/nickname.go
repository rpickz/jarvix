package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/rpickz/jarvix/internal/desktop"
)

// This file is the window-nickname surface (#126) on the window tools'
// shared state: assignment, the spoken listing, and the one public
// resolution seam. Everything here judges against the same inventory the
// five window verbs use, and resolves through the same resolveWindow call —
// there is deliberately no per-consumer nickname code anywhere else.
//
// Assignment is deliberately ungated. It changes nothing on screen, enters
// nothing anywhere, and the opposite assignment undoes it — the same
// reversibility reading that lets focus run without a question (ADR 0014).

// AssignNickname resolves reference (empty means the focused window) and
// gives that window the spoken nickname. spoken is the confirmation to say —
// soft, with the common-word caution suffixed when one applies — and err is
// a spoken-ready refusal that starts lowercase, so "Sorry, %s." frames it
// without rewording.
//
// Every surface assigns through here: the deterministic "call this window
// builds" intent, the model's desktop.name_window tool, and the daemon's
// windows.name verb. One implementation is what makes the collision and
// normalisation rules impossible to disagree about.
func (d *Desktop) AssignNickname(ctx context.Context, reference, name string) (spoken string, err error) {
	windows, err := d.windows(ctx)
	if err != nil {
		d.log.Warn("compositor unavailable", "component", "tools", "error", err.Error())
		return "", fmt.Errorf("I cannot see the windows right now, so nothing was named")
	}
	res := resolveWindow(reference, windows, d.names)
	switch res.Kind {
	case resolveOne:
	case resolveMany:
		return "", fmt.Errorf("several windows match %q — %s — so nothing was named; say which one you mean",
			res.Query, describeCandidates(res.Candidates))
	case resolveReleased:
		return "", fmt.Errorf("nothing is called %q right now — that window has closed — so nothing was named", res.Query)
	default:
		what := res.Query
		if what == "" {
			// A deictic (or empty) reference with no focused window in the
			// inventory: there is nothing "this" can mean.
			return "", fmt.Errorf("no window has focus right now, so nothing was named")
		}
		return "", fmt.Errorf("no window matches %q, so nothing was named", what)
	}

	assigned, previous, warning, err := d.names.Assign(name, res.Window, windows)
	if err != nil {
		// The refusal reasons are composed to be safe on the bus: collision
		// owners and reserved-word descriptions, never addresses.
		d.publishRefusal("name", res.Window.Describe(), err.Error())
		return "", err
	}
	app := desktop.AppName(res.Window.Class)
	if app == "" {
		app = "that"
	}
	spoken = fmt.Sprintf("Okay — the %s window is now called %s.", app, assigned)
	if previous != "" {
		spoken += fmt.Sprintf(" It is no longer called %s.", previous)
	}
	if warning != "" {
		spoken += " " + warning
	}
	// Class, nickname and address for the journal; the bus gets the window
	// as a person would name it, like every other desktop action.
	d.log.Info("window named", "component", "tools", "class", res.Window.Class,
		"address", res.Window.Address, "nickname", assigned)
	d.publish("name", res.Window.Describe()+" → "+assigned)
	return spoken, nil
}

// nameWindow is the desktop.name_window tool's Execute half: the same
// assignment, framed for the model. Refusals are results, never errors —
// a name the user cannot have is a thing to say in one sentence.
func (d *Desktop) nameWindow(ctx context.Context, reference, name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("%s: empty nickname", NameWindowToolName)
	}
	spoken, err := d.AssignNickname(ctx, reference, name)
	if err != nil {
		return fmt.Sprintf("Nothing was named: %v. Tell the user in one short sentence, and do not "+
			"retry with the same name.", err), nil
	}
	return spoken + " Confirm it to the user in one short sentence.", nil
}

// NicknameListing answers "what are my windows called" in one spoken
// sentence, from the same inventory a resolution would use. Behind the
// deterministic listing intent and the daemon's windows.list verb alike.
func (d *Desktop) NicknameListing(ctx context.Context) (string, error) {
	windows, err := d.windows(ctx)
	if err != nil {
		d.log.Warn("compositor unavailable", "component", "tools", "error", err.Error())
		return "", fmt.Errorf("I cannot see the windows right now")
	}
	named := d.names.List(windows)
	if len(named) == 0 {
		return "No windows have names right now. Say call this window and then a name to give one.", nil
	}
	parts := make([]string, 0, len(named))
	for _, nw := range named {
		parts = append(parts, fmt.Sprintf("%s is %s", nw.Name, nw.Window.Describe()))
	}
	return fmt.Sprintf("%s: %s.", plural(len(named), "window has a name", "windows have names"),
		strings.Join(parts, "; ")), nil
}

// ResolveReference is the one window-resolution seam (#126): every consumer
// that accepts a spoken window reference — the window verbs and typing
// already, focus-thread anchors (#123) and session recaps (#124) next —
// resolves through it, so a nickname means the same window everywhere.
// Nicknames are consulted before any fuzzy app/title matching; the
// precedence is pinned by TestNicknameOutranksEveryMatchingTier.
//
// ok false comes with explain: one spoken-ready sentence saying why —
// ambiguity naming the candidates, a released nickname, or a plain miss.
// The window returned is live inventory: act on its Address promptly, and
// re-verify before anything destructive, as the window verbs do.
func (d *Desktop) ResolveReference(ctx context.Context, reference string) (w desktop.Window, ok bool, explain string) {
	windows, err := d.windows(ctx)
	if err != nil {
		d.log.Warn("compositor unavailable", "component", "tools", "error", err.Error())
		return desktop.Window{}, false, "I cannot see the windows right now."
	}
	res := resolveWindow(reference, windows, d.names)
	switch res.Kind {
	case resolveOne:
		return res.Window, true, ""
	case resolveMany:
		return desktop.Window{}, false, fmt.Sprintf("Several windows match %q: %s.",
			res.Query, describeCandidates(res.Candidates))
	case resolveReleased:
		return desktop.Window{}, false, fmt.Sprintf("Nothing is called %q right now — that window has closed.", res.Query)
	default:
		what := res.Query
		if what == "" {
			what = "that"
		}
		return desktop.Window{}, false, fmt.Sprintf("Nothing like %q is open.", what)
	}
}

// WindowListing is one window as the daemon's windows.list verb serves it:
// the facts a person-facing list needs and nothing compositor-internal — no
// address ever travels (ADR 0022).
type WindowListing struct {
	App       string `json:"app"`
	Title     string `json:"title"`
	Workspace string `json:"workspace"`
	Focused   bool   `json:"focused"`
	// Nickname is the window's nickname (#126), "" when it has none.
	Nickname string `json:"nickname,omitempty"`
}

// WindowListings renders the live inventory for the daemon's windows.list
// verb, nicknames included, in the inventory's own order (most recently
// focused first).
func (d *Desktop) WindowListings(ctx context.Context) ([]WindowListing, error) {
	windows, err := d.windows(ctx)
	if err != nil {
		return nil, err
	}
	nicknames := d.NicknamesByAddress(windows)
	out := make([]WindowListing, 0, len(windows))
	for _, w := range windows {
		out = append(out, WindowListing{
			App:       desktop.AppName(w.Class),
			Title:     strings.TrimSpace(w.Title),
			Workspace: workspaceLabel(w),
			Focused:   w.Focused,
			Nickname:  nicknames[w.Address],
		})
	}
	return out, nil
}

// NicknameCount reports how many nicknames are held, without consulting the
// compositor — the overlay feed's enrolment gate (#127). See
// desktop.Nicknames.Count for why the un-pruned answer is the right trade.
func (d *Desktop) NicknameCount() int {
	return d.names.Count()
}

// NicknamesByAddress indexes the live nicknames by window address for the
// renderers that walk an inventory — windows.list above, and the window
// overlays' feed (#127), which needs "which window wears which tag" against
// the same inventory it read the geometry from. The address is a lookup key
// only; it never enters anything rendered or anything on the wire (ADR 0022).
func (d *Desktop) NicknamesByAddress(windows []desktop.Window) map[string]string {
	named := d.names.List(windows)
	if len(named) == 0 {
		return nil
	}
	out := make(map[string]string, len(named))
	for _, nw := range named {
		out[nw.Window.Address] = nw.Name
	}
	return out
}
