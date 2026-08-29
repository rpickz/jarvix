package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/managed"
)

// This file is the managed-window surface (#197, ADR 0062) on the window
// tools' shared state: handing a window over, taking it back, and saying
// which ones Jarvix has.
//
// Everything here judges against the same inventory the other window verbs
// use and resolves through the same resolveWindow call, so "this terminal"
// and "builds" mean the same window when handing it over as when focusing it.
//
// The asymmetry between the two verbs is the design, not an accident of
// tiering:
//
//   - **Acquiring asks.** It is a grant, so the user hears the window named
//     before it happens, and the answer is never remembered for the next one
//     (ManageWindowToolName is in neverSilent, which is also what
//     RememberableApproval keys on). A global `default = "allow"` does not
//     reach it; a global `deny` still does, because tightening is never the
//     thing to override.
//   - **Releasing does not.** Giving up power needs no permission, so
//     release is allow-tier, immediate, and has no confirmation to skip.
//
// And the thing acquisition does NOT grant is the reason this feature was
// written the way it was. Management is access to a window. It is not
// permission to run anything: text typed into a managed terminal faces the
// shell classifier and the verbatim confirmation card exactly as `shell.run`
// does (typing.go's commandVerdict / Refuse / Escalate), every time, whatever is
// recorded here.

// Tool names. One tool per verb, because the permission gate keys on the tool
// name: "handing a window over asks, taking it back does not" is then a fact
// about the registry rather than a special case inside a tool (ADR 0014).
const (
	ManageWindowToolName  = "desktop.manage_window"
	ReleaseWindowToolName = "desktop.release_window"
	ListManagedToolName   = "desktop.list_managed"
)

// managedVerb is which of the three things this file's tools do.
type managedVerb int

const (
	verbManage managedVerb = iota
	verbRelease
	verbListManaged
)

// managedTool is one verb. Like windowTool it holds no state of its own:
// everything lives on the shared Desktop, so managing a window and focusing
// it see one inventory and one cache.
type managedTool struct {
	d    *Desktop
	verb managedVerb
}

// ManagedTools returns the three managed-window tools, read first.
func (d *Desktop) ManagedTools() []Tool {
	return []Tool{
		&managedTool{d: d, verb: verbListManaged},
		&managedTool{d: d, verb: verbManage},
		&managedTool{d: d, verb: verbRelease},
	}
}

// Name implements Tool.
func (t *managedTool) Name() string {
	switch t.verb {
	case verbManage:
		return ManageWindowToolName
	case verbRelease:
		return ReleaseWindowToolName
	default:
		return ListManagedToolName
	}
}

// Description implements Tool. Written for a small local model, so each one
// states the thing that would otherwise be discovered by trying — and the
// managing one states, in the description itself, that it is not permission
// to run commands, because that is the misunderstanding with consequences.
func (t *managedTool) Description() string {
	switch t.verb {
	case verbManage:
		return "Take control of a window the user points at, so Jarvix may read it, place it, " +
			"type into it, and run work there. Use it when they say something like \"take control " +
			"of this terminal\" or \"manage the builds window\". It does NOT give permission to run " +
			"commands: anything typed into a managed terminal is still confirmed command by " +
			"command, exactly as running a shell command is. The user is asked before a window is " +
			"taken over."
	case verbRelease:
		return "Stop managing a window — Jarvix keeps its hands off it again. Use it when the user " +
			"says \"let this go\", \"stop managing this\", or names a window they want back. It " +
			"happens immediately and is never refused."
	default:
		return "List the windows Jarvix currently manages: the ones it opened and the ones the user " +
			"handed over. Use it when they ask what you manage, what you have control of, or which " +
			"windows you can work in."
	}
}

// Schema implements Tool.
func (t *managedTool) Schema() json.RawMessage {
	if t.verb == verbListManaged {
		return json.RawMessage(`{"type": "object", "properties": {}}`)
	}
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"window": {
				"type": "string",
				"description": "Which window, as the user described it: a nickname they gave it, an application name, words from its title, or \"this\" for the one they are in. Leave it out for the focused window."
			}
		}
	}`)
}

// Execute implements Tool. Every refusal is a tool result rather than an
// error — a window that closed, or a name that means two windows, is
// something to say in a sentence, not a failed session.
func (t *managedTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var args windowArgs
	if err := unmarshalWindowArgs(input, &args); err != nil {
		return "", fmt.Errorf("invalid %s arguments: %w", t.Name(), err)
	}
	switch t.verb {
	case verbManage:
		return t.d.acquireForModel(ctx, args.Window)
	case verbRelease:
		return t.d.releaseForModel(ctx, args.Window)
	default:
		spoken, err := t.d.ManagedListing(ctx)
		if err != nil {
			return fmt.Sprintf("The managed windows could not be listed: %v. Tell the user in one "+
				"short sentence, and do not retry.", err), nil
		}
		return spoken + " Read it back to the user in one short sentence.", nil
	}
}

// Confirmation implements Confirmable for the managing verb: the ask tier's
// question, built from the live inventory rather than from the model's words.
//
// The window is named because that is the whole of what the user is
// approving, and the sentence says what management does and does not mean in
// the same breath — a confirmation that says "take control" without saying
// "commands are still confirmed" is a confirmation people would answer
// wrongly. The resolution behind it is held, so approving *this* window
// cannot hand over a different one that matched the same words a moment
// later.
func (t *managedTool) Confirmation(input json.RawMessage) (command, summary string, ok bool) {
	if t.verb != verbManage {
		return "", "", false // releasing never asks; listing has nothing to approve
	}
	var args windowArgs
	if err := unmarshalWindowArgs(input, &args); err != nil {
		return "", "", false
	}
	// The gate has no context of its own — it is a synchronous decision on
	// the session's think goroutine — so bound the look tightly, the typing
	// tools' stance.
	ctx, cancel := context.WithTimeout(context.Background(), t.d.timeout)
	defer cancel()
	res, err := t.d.resolve(ctx, args.Window)
	if err != nil || res.Kind != resolveOne {
		return "", "", false // Execute will explain; a generic question is better than a wrong one
	}
	t.d.holdPending(t.Name(), args, res.Window)
	where := res.Window.Describe()
	return "manage " + where,
		fmt.Sprintf("I want to take control of %s, so I can read it, place it and type in it%s. "+
			"Anything I type there is still confirmed command by command. Should I go ahead?",
			where, t.d.managedTypingClause(res.Window)), true
}

// managedTypingClause is the honest half of the confirmation when typing is
// switched off in configuration.
//
// The acceptance criterion is that acquisition still works for reading and
// placement, and that the refusal to type says exactly that rather than
// failing obscurely. Saying it in the confirmation is the earliest honest
// moment: a user who is handing over a terminal in order to have things typed
// into it should hear, before they answer, that this build will not type.
func (d *Desktop) managedTypingClause(w desktop.Window) string {
	if d.typingEnabled == nil || d.typingEnabled() {
		return ""
	}
	if !d.isManagedTerminal(w) {
		return " — though typing is switched off in your configuration, so reading and placing is all it will do"
	}
	return " — though typing is switched off in your configuration ([tools.typing] enable), " +
		"so I will be able to read it and move it but not type in it"
}

// acquireForModel is the managing verb's Execute half.
func (d *Desktop) acquireForModel(ctx context.Context, reference string) (string, error) {
	spoken, err := d.AcquireWindow(ctx, reference)
	if err != nil {
		return fmt.Sprintf("Nothing was taken over: %v. Tell the user in one short sentence, and "+
			"do not retry the same way.", err), nil
	}
	return spoken + " Confirm it to the user in one short sentence.", nil
}

// releaseForModel is the releasing verb's Execute half.
func (d *Desktop) releaseForModel(ctx context.Context, reference string) (string, error) {
	spoken, err := d.ReleaseWindow(ctx, reference)
	if err != nil {
		return fmt.Sprintf("Nothing was released: %v. Tell the user in one short sentence, and do "+
			"not retry the same way.", err), nil
	}
	return spoken + " Confirm it to the user in one short sentence.", nil
}

// AcquireWindow hands a window over. reference is the window as a person
// would describe it ("" or "this" means the focused one), and it is also the
// key the confirmation's resolution was held under, so the window the user
// was asked about is the window that is taken.
//
// The spoken result is a whole sentence, ready to be said as it stands; err
// is a spoken-ready refusal that starts lowercase, so "Sorry, %s." frames it
// without rewording.
func (d *Desktop) AcquireWindow(ctx context.Context, reference string) (spoken string, err error) {
	if d.managed == nil {
		return "", fmt.Errorf("managed windows are switched off on this daemon")
	}
	target, windows, err := d.resolveForManagement(ctx, ManageWindowToolName,
		windowArgs{Window: reference})
	if err != nil {
		return "", err
	}
	rec, fresh, err := d.managed.Acquire(target, windows)
	if err != nil {
		d.publishRefusal("manage", target.Describe(), err.Error())
		return "", err
	}
	if !fresh {
		// Already managed — said plainly rather than reported as a fresh
		// take-over, so "take control of this" twice does not sound like two
		// different things happened.
		return fmt.Sprintf("I already have %s%s.", target.Describe(), managedSince(rec)), nil
	}
	d.log.Info("window managed", "component", "tools", "class", target.Class,
		"address", target.Address, "pid", target.PID, "source", string(rec.Source))
	d.publish("manage", target.Describe())
	return fmt.Sprintf("Okay — I have %s now. I can read it, place it%s.",
		target.Describe(), d.managedGrantClause(target)), nil
}

// managedGrantClause finishes the acquisition sentence with what management
// actually allows here — and, when typing is off, with what it does not.
func (d *Desktop) managedGrantClause(w desktop.Window) string {
	if d.typingEnabled != nil && !d.typingEnabled() {
		return ", but not type in it — typing is switched off in your configuration"
	}
	if d.isManagedTerminal(w) {
		return " and type in it, and anything I type there is confirmed command by command"
	}
	return " and type in it"
}

// ReleaseWindow takes a window back. Ungated everywhere: there is no
// confirmation on this path, in the tool or in the daemon verb, because
// giving up power needs no permission.
func (d *Desktop) ReleaseWindow(ctx context.Context, reference string) (spoken string, err error) {
	if d.managed == nil {
		return "", fmt.Errorf("managed windows are switched off on this daemon")
	}
	target, windows, err := d.resolveForManagement(ctx, "", windowArgs{Window: reference})
	if err != nil {
		return "", err
	}
	rec, held, err := d.managed.Release(target, windows)
	if err != nil {
		d.publishRefusal("release", target.Describe(), err.Error())
		return "", err
	}
	if !held {
		// "Done" would be a lie of the exact kind this feature exists to
		// remove: the user believes something changed and nothing did.
		return "", fmt.Errorf("I was not managing %s, so there was nothing to let go", target.Describe())
	}
	d.log.Info("window released", "component", "tools", "class", target.Class,
		"address", target.Address, "source", string(rec.Source))
	d.publish("release", target.Describe())
	return fmt.Sprintf("Okay — I have let %s go. It is yours again.", target.Describe()), nil
}

// resolveForManagement resolves a reference to one window, preferring the
// resolution the confirmation was built from when there is one.
//
// tool is the pending-resolution key; empty skips the memo entirely, which is
// what an ungated verb wants — a release was never asked about, so there is
// nothing held to honour.
func (d *Desktop) resolveForManagement(ctx context.Context, tool string, args windowArgs) (desktop.Window, []desktop.Window, error) {
	if tool != "" {
		if held, ok := d.takePending(tool, args); ok {
			// Look again: the window the user approved must still be there,
			// and must still be that window (verify checks the address, the
			// compositor's stable id and the class together).
			d.invalidate()
			current, alive, err := d.verify(ctx, held)
			if err != nil {
				return desktop.Window{}, nil, fmt.Errorf("I cannot see the windows right now")
			}
			if !alive {
				return desktop.Window{}, nil, fmt.Errorf("%s has closed since you were asked about it",
					held.Describe())
			}
			windows, err := d.windows(ctx)
			if err != nil {
				return desktop.Window{}, nil, fmt.Errorf("I cannot see the windows right now")
			}
			return current, windows, nil
		}
	}
	windows, err := d.windows(ctx)
	if err != nil {
		d.log.Warn("compositor unavailable", "component", "tools", "error", err.Error())
		return desktop.Window{}, nil, fmt.Errorf("I cannot see the windows right now")
	}
	res := resolveWindow(args.Window, windows, d.names)
	switch res.Kind {
	case resolveOne:
		return res.Window, windows, nil
	case resolveMany:
		return desktop.Window{}, nil, fmt.Errorf("several windows match %q — %s — so say which one you mean",
			res.Query, describeCandidates(res.Candidates))
	case resolveReleased:
		return desktop.Window{}, nil, fmt.Errorf("nothing is called %q right now — that window has closed", res.Query)
	default:
		what := res.Query
		if what == "" {
			return desktop.Window{}, nil, fmt.Errorf("no window has focus right now, so there is nothing to point at")
		}
		return desktop.Window{}, nil, fmt.Errorf("no window matches %q", what)
	}
}

// managedSince renders how long a window has been managed, for the "I already
// have that one" sentence. Rounded to something a person would say.
func managedSince(rec managed.Record) string {
	if rec.Since.IsZero() {
		return ""
	}
	switch d := time.Since(rec.Since); {
	case d < time.Minute:
		return ""
	case d < time.Hour:
		return fmt.Sprintf(" — since %s ago", plural(int(d/time.Minute), "minute", "minutes"))
	case d < 24*time.Hour:
		return fmt.Sprintf(" — since %s ago", plural(int(d/time.Hour), "hour", "hours"))
	default:
		return " — since before today"
	}
}

// ManagedListing answers "what do you manage" in one spoken sentence, from
// the same inventory a resolution would use.
func (d *Desktop) ManagedListing(ctx context.Context) (string, error) {
	if d.managed == nil {
		return "", fmt.Errorf("managed windows are switched off on this daemon")
	}
	windows, err := d.windows(ctx)
	if err != nil {
		d.log.Warn("compositor unavailable", "component", "tools", "error", err.Error())
		return "", fmt.Errorf("I cannot see the windows right now")
	}
	live := d.managed.List(windows)
	if len(live) == 0 {
		return "I am not managing any windows. Say take control of this terminal to hand one over.", nil
	}
	nicknames := d.NicknamesByAddress(windows)
	parts := make([]string, 0, len(live))
	for _, item := range live {
		label := item.Window.Describe()
		if nick := nicknames[item.Window.Address]; nick != "" {
			label = nick + " — " + label
		}
		parts = append(parts, fmt.Sprintf("%s on workspace %s", label, workspaceLabel(item.Window)))
	}
	return fmt.Sprintf("%s: %s.", plural(len(live), "window is managed", "windows are managed"),
		strings.Join(parts, "; ")), nil
}

// ManagedWindowListing is one managed window as the daemon's windows.managed
// verb serves it: the facts a person-facing list needs and nothing
// compositor-internal — no address ever travels (ADR 0022).
type ManagedWindowListing struct {
	// Reference is how to name this window back to the daemon — the handle
	// the window's Release button sends. It is a spoken reference, resolved
	// through the same seam every other window reference goes through, and it
	// is "" when nothing distinguishes this window in words (see
	// managedReference): a row with no reference is a row whose Release
	// button the window hides, which is honest, where a button that released
	// the wrong window would not be.
	Reference string `json:"reference,omitempty"`
	App       string `json:"app"`
	Title     string `json:"title"`
	Workspace string `json:"workspace"`
	Focused   bool   `json:"focused"`
	// Nickname is the window's nickname (#130), "" when it has none.
	Nickname string `json:"nickname,omitempty"`
	// Source is "launched" (Jarvix opened it) or "acquired" (handed over).
	Source string `json:"source"`
	// Program is what a launched window was opened to run, "" otherwise.
	Program string `json:"program,omitempty"`
	// Since is when management began, RFC3339.
	Since string `json:"since,omitempty"`
	// Terminal marks a window whose contents are a command line — the one
	// place management and the permission gate meet, so the surfaces can say
	// so rather than leave the user to know it.
	Terminal bool `json:"terminal"`
}

// ManagedWindowListings renders the managed set for the daemon's
// windows.managed verb, in the store's stable order.
func (d *Desktop) ManagedWindowListings(ctx context.Context) ([]ManagedWindowListing, error) {
	if d.managed == nil {
		return nil, fmt.Errorf("managed windows are switched off on this daemon")
	}
	windows, err := d.windows(ctx)
	if err != nil {
		return nil, err
	}
	nicknames := d.NicknamesByAddress(windows)
	live := d.managed.List(windows)
	out := make([]ManagedWindowListing, 0, len(live))
	for _, item := range live {
		row := ManagedWindowListing{
			App:       desktop.AppName(item.Window.Class),
			Title:     strings.TrimSpace(item.Window.Title),
			Workspace: workspaceLabel(item.Window),
			Focused:   item.Window.Focused,
			Nickname:  nicknames[item.Window.Address],
			Source:    string(item.Record.Source),
			Program:   item.Record.Program,
			Terminal:  d.isManagedTerminal(item.Window),
		}
		if !item.Record.Since.IsZero() {
			row.Since = item.Record.Since.UTC().Format(time.RFC3339)
		}
		row.Reference = managedReference(item.Window, windows, row.Nickname)
		out = append(out, row)
	}
	return out, nil
}

// managedReference works out how a listing row can name its window back to
// the daemon without an address travelling (ADR 0022).
//
// It is the ambiguity discipline of the window matcher, run forwards: pick
// the shortest thing that resolves to exactly this window, and pick nothing
// at all when nothing does. A nickname is best — the user chose it. Failing
// that the application name, when only one window has that class, then the
// title, when only one window has that title. A window that is one of three
// identical terminals with no nickname gets no reference, and the surfaces
// say so rather than offering a button that would release a sibling.
func managedReference(w desktop.Window, windows []desktop.Window, nickname string) string {
	if nickname != "" {
		return nickname
	}
	app := strings.TrimSpace(desktop.AppName(w.Class))
	if app != "" && uniqueBy(windows, func(o desktop.Window) bool {
		return strings.EqualFold(desktop.AppName(o.Class), app)
	}) {
		return app
	}
	title := strings.TrimSpace(w.Title)
	if title != "" && uniqueBy(windows, func(o desktop.Window) bool {
		return strings.EqualFold(strings.TrimSpace(o.Title), title)
	}) {
		return title
	}
	return ""
}

// uniqueBy reports whether exactly one window in the inventory satisfies the
// predicate.
func uniqueBy(windows []desktop.Window, pred func(desktop.Window) bool) bool {
	n := 0
	for _, w := range windows {
		if pred(w) {
			n++
			if n > 1 {
				return false
			}
		}
	}
	return n == 1
}

// ManagedByAddress indexes the managed windows by address for the renderers
// that walk an inventory — the window overlays' feed (#127), which needs
// "which window carries the managed mark" against the same inventory it read
// the geometry from. The address is a lookup key only; it never enters
// anything rendered or anything on the wire (ADR 0022).
func (d *Desktop) ManagedByAddress(windows []desktop.Window) map[string]bool {
	if d.managed == nil {
		return nil
	}
	held := d.managed.ByAddress(windows)
	if len(held) == 0 {
		return nil
	}
	out := make(map[string]bool, len(held))
	for address := range held {
		out[address] = true
	}
	return out
}

// ManagedCount reports how many windows are managed, without consulting the
// compositor — the overlay feed's enrolment gate (#127). See
// managed.Store.Count for why the un-reconciled answer is the right trade.
func (d *Desktop) ManagedCount() int {
	if d.managed == nil {
		return 0
	}
	return d.managed.Count()
}

// ManagedStorePath is the file management lives in, so every surface can tell
// the user where to edit it by hand.
func (d *Desktop) ManagedStorePath() string {
	if d.managed == nil {
		return ""
	}
	return d.managed.Path()
}

// ErrNotManaged is what a job-shaped caller gets for a window Jarvix does not
// manage. A sentinel because the refusal is a decision other packages will
// key on, not a string they should match.
var ErrNotManaged = fmt.Errorf("that window is not one I manage")

// RequireManaged is the seam a job runs through (#195's next slice): resolve
// a window reference, and refuse unless Jarvix manages it.
//
// It exists here, as one exported function, so that "a job runs in a managed
// window and cannot act in an unmanaged one" is a single place rather than a
// rule each caller remembers. The refusal is spoken-ready and names the
// window and the way out, because "not managed" on its own tells the user
// nothing they can act on.
//
// It answers only the question it is named for. A managed terminal is still
// not permission to run anything: a job that types into one goes through the
// same classification and confirmation as `shell.run` (ADR 0062).
func (d *Desktop) RequireManaged(ctx context.Context, reference string) (desktop.Window, error) {
	if d.managed == nil {
		return desktop.Window{}, fmt.Errorf("%w: managed windows are switched off on this daemon", ErrNotManaged)
	}
	target, windows, err := d.resolveForManagement(ctx, "", windowArgs{Window: reference})
	if err != nil {
		return desktop.Window{}, err
	}
	if _, ok := d.managed.Managed(target, windows); !ok {
		return desktop.Window{}, fmt.Errorf("%w: %s is yours, not mine — say take control of it first",
			ErrNotManaged, target.Describe())
	}
	return target, nil
}

// isManagedTerminal reports whether a window's contents are a command line —
// the same question, through the same function, the typing gate asks
// (isTerminalClass). One definition, so a terminal Jarvix recognises when
// something is typed into it is a terminal it recognises when it is handed
// over; two would drift, and the drift would be a window described as safe
// and treated as a shell, or the reverse.
func (d *Desktop) isManagedTerminal(w desktop.Window) bool {
	return isTerminalClass(w, d.terminals)
}
