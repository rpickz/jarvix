package daemon

// This file wires the account of what Jarvix did — and putting it back —
// into jarvixd (#201, ADR 0064): the store's construction, the gate and the
// window placer the reverser needs, and the undo.* IPC methods behind
// `jarvix actions` and `jarvix undo`.
//
// The store is installed on the engine's tool context (session.Options.Undo),
// which is the whole of "the recording belongs where the action happens": the
// tools that change the machine write their own rows, and nothing here
// watches the activity feed and guesses.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/jobs"
	"github.com/rpickz/jarvix/internal/session"
	"github.com/rpickz/jarvix/internal/statehold"
	"github.com/rpickz/jarvix/internal/tools"
	"github.com/rpickz/jarvix/internal/undo"
)

// newUndoStore builds the account. Always present, like the reminder store
// and for the same reason: there is no configuration switch for "keep no
// record of what you did in my name". A user who wants the file gone deletes
// it, which the store reads as an empty account and refills from the next
// action — an honest fresh start rather than a way to act unaccountably.
func newUndoStore(paths config.Paths, bus *session.Bus, gate *statehold.Gate,
	logger *slog.Logger) *undo.Store {
	return undo.NewStore(paths.UndoFile(), undo.StoreOptions{
		Gate: gate,
		Publish: func(event string, data map[string]any) {
			bus.Publish(session.Event{Type: event, Data: data})
		},
	}, logger)
}

// undoGate judges a reversal under the identity of the action it reverses.
//
// It reads the live policy off the registry rather than a snapshot, so a
// `[tools.policy]` change or a config.reload applies to the next undo the way
// it applies to the next tool call. With no policy installed — a construction
// only tests make — everything is allowed, which is that construction's
// documented meaning everywhere else in this repository.
type undoGate struct{ registry *tools.Registry }

// Judge implements undo.Gate.
func (g undoGate) Judge(tool string) undo.Decision {
	if g.registry == nil {
		return undo.DecisionAllow
	}
	policy := g.registry.Policy()
	if policy == nil {
		return undo.DecisionAllow
	}
	switch policy.ToolDecision(tool) {
	case tools.PolicyDeny:
		return undo.DecisionDeny
	case tools.PolicyAsk:
		return undo.DecisionAsk
	default:
		return undo.DecisionAllow
	}
}

// undoPlacer puts a window back through the same compositor seam the window
// tools dispatch through (ADR 0022 allows exactly one).
type undoPlacer struct {
	comp    desktop.Compositor
	timeout time.Duration
}

// Restore implements undo.Placer.
//
// The identity check comes first and is the window half of
// refuse-rather-than-clobber: a compositor address is a handle the window
// manager recycles, so a reversal that dispatched on the address alone would
// eventually move a stranger's window into the geometry of one that has since
// closed. All four facts must agree — the managed-window store's rule (ADR
// 0062), applied to a reversal.
//
// The order of the dispatches matters and is not arbitrary. Fullscreen comes
// off first, because a fullscreen window ignores geometry; then the
// workspace, so the window is on the right screen before it is sized for it;
// then floating, because position and size mean different things in and out
// of the tiling layer; then size, then position. Each is a set rather than a
// toggle, so a reversal applied twice converges instead of undoing itself.
func (p undoPlacer) Restore(ctx context.Context, want undo.WindowState) error {
	if p.comp == nil {
		return &undo.Refusal{Spoken: "I can't reach the window manager, so I can't put that window back."}
	}
	timeout := p.timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	windows, err := p.comp.Windows(callCtx)
	if err != nil {
		return &undo.Refusal{Spoken: "I couldn't read the window list, so I've left that window alone."}
	}
	var live desktop.Window
	found := false
	for _, w := range windows {
		if w.Address != want.Address || !strings.EqualFold(w.Class, want.Class) {
			continue
		}
		if want.StableID != "" && w.StableID != want.StableID {
			continue
		}
		if want.PID != 0 && w.PID != want.PID {
			continue
		}
		live, found = w, true
		break
	}
	if !found {
		name := want.Describe
		if strings.TrimSpace(name) == "" {
			name = "that window"
		}
		return &undo.Refusal{Spoken: fmt.Sprintf(
			"I can't put %s back: it isn't on the desktop any more.", name)}
	}

	if live.Fullscreen != want.Fullscreen {
		if err := p.comp.SetFullscreen(callCtx, live.Address,
			desktop.FullscreenWhole, want.Fullscreen); err != nil {
			return err
		}
	}
	if live.Workspace != want.Workspace && want.Workspace > 0 {
		if err := p.comp.MoveToWorkspace(callCtx, live.Address, want.Workspace); err != nil {
			return err
		}
	}
	if live.Floating != want.Floating {
		if err := p.comp.SetFloating(callCtx, live.Address, want.Floating); err != nil {
			return err
		}
	}
	// Geometry is restored only when it was recorded. A compositor that
	// reports no geometry writes zeroes, and resizing a window to nothing
	// because we never knew its size would be a far worse outcome than
	// leaving it where the workspace and layer put it.
	if want.Width > 0 && want.Height > 0 {
		if err := p.comp.ResizeWindow(callCtx, live.Address, want.Width, want.Height); err != nil {
			return err
		}
	}
	if want.Floating && (want.X != 0 || want.Y != 0) {
		if err := p.comp.PositionWindow(callCtx, live.Address, want.X, want.Y); err != nil {
			return err
		}
	}
	return nil
}

// registerUndoMethods adds the undo.* verbs: the account and its reversal.
//
// undo.list is a read. undo.apply and undo.last perform the reversal, and
// they do it on the manager's own instruction — `jarvix undo`, or the review
// pane a follow-up issue will add — so they are not behind a confirmation
// card. The gate still applies where the gate is the user's standing
// instruction rather than a question: a tool identity the policy denies
// refuses here too, and the clobber guard refuses on every path, because
// "restoring this would destroy newer work" is not a thing the person asking
// can be assumed to know.
func (d *Daemon) registerUndoMethods() {
	d.server.Handle("undo.list", func(json.RawMessage) (any, error) {
		return undoViewReport(d.undoAccount()), nil
	})

	d.server.Handle("undo.apply", func(params json.RawMessage) (any, error) {
		p := struct {
			ID  string `json:"id"`
			Job string `json:"job"`
		}{}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "undo.apply params: %v", err)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if strings.TrimSpace(p.Job) != "" {
			if err := d.settleBeforeUndo(p.Job); err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
			}
			out, err := d.undoer.JobActions(ctx, p.Job)
			if err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
			}
			return undoJobReport(out), nil
		}
		if strings.TrimSpace(p.ID) == "" {
			out, err := d.undoer.Last(ctx)
			if err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
			}
			return undoOutcomeReport(out), nil
		}
		out, err := d.undoer.Apply(ctx, p.ID)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
		}
		return undoOutcomeReport(out), nil
	})
}

// settleBeforeUndo answers the second question ADR 0064 left to #200: **may a
// job that is still running be undone?**
//
// No. A reversal that ran underneath a live runner would be racing the thing it
// is reversing — restoring a file the next step is about to rewrite, moving a
// window the job is about to move back — and the account would end up
// describing a state the machine was never in. "I can't tell whether that
// stuck" is exactly the answer this feature refuses to produce, so the request
// is refused instead, with the thing to do about it.
//
// A PARKED job is different and is allowed, because it is not acting: it is
// waiting for a person, and that person has just said something better than an
// answer. Undoing it stops it, and must — resuming from a checkpoint whose
// effects have been reversed would be resuming into a world the checkpoint does
// not describe.
//
// A job id nothing recognises passes straight through: the account is asked, and
// answers honestly that it holds nothing for it.
func (d *Daemon) settleBeforeUndo(ref string) error {
	if d.jobStore == nil {
		return nil
	}
	job, err := d.jobStore.Find(ref)
	if err != nil {
		return nil
	}
	switch job.State {
	case jobs.Ready, jobs.Running:
		return errors.New(undoJobBusy(job.Name))
	case jobs.Parked:
		if _, err := d.jobRunner.Stop(job.ID,
			"You asked me to undo what it had done, so I stopped it first."); err != nil {
			return err
		}
	}
	return nil
}

// undoAccount is the account plus the two things the wire report needs that
// the account file itself does not hold: whether the gate would allow each
// reversal, and what the job a row belongs to is called and doing.
//
// A struct of seams rather than a *Daemon, so every sentence below can be
// exercised against a fixed clock and a canned gate. The report is where the
// window's whole vocabulary is decided (ADR 0013), and a vocabulary only
// reachable through a wired daemon on a socket is one nobody writes cases for.
type undoAccount struct {
	view undo.View
	// offer is the Undoer's own eligibility answer, so the control this
	// listing draws and the reversal it would run cannot disagree. Nil falls
	// back to the record's own reversibility, which is what a daemon
	// constructed without a reverser honestly knows.
	offer func(undo.Record) (bool, string)
	// job names one job, and reports false for an id the job store no longer
	// holds — a job's own bound can drop it while its actions are still in the
	// account, and the group must still be nameable when that happens.
	job func(id string) (jobs.Job, bool)
}

// undoAccount assembles the report's inputs from the running daemon.
func (d *Daemon) undoAccount() undoAccount {
	a := undoAccount{view: d.account.List()}
	if d.undoer != nil {
		a.offer = d.undoer.Offer
	}
	if d.jobStore != nil {
		a.job = func(id string) (jobs.Job, bool) {
			j, err := d.jobStore.Find(id)
			if err != nil {
				return jobs.Job{}, false
			}
			return j, true
		}
	}
	return a
}

// undoViewReport renders the account for the wire.
//
// The disclosure travels as a sentence the daemon composed, not as two
// numbers a client is trusted to word (ADR 0013). It is the whole point of
// the bound being a bound rather than a silent truncation: every surface says
// the same thing about what is missing because there is only one sentence.
// `empty` is the same promise for the other end of the range — the account
// with nothing in it still has something to say, and it says it once.
//
// The restore payload never travels. A listing carrying the previous contents
// of config.toml would put the user's api keys on every connected socket, and
// nothing that renders the account needs them — "this can be put back" is the
// fact a reader acts on, and the bytes stay in the file.
//
// `actions` and `groups` carry the same rows twice, on purpose, for two
// readers with two different questions (#210, ADR 0066). `actions` is the flat
// chronological account `jarvix actions` prints. `groups` is that account
// arranged as work — one group per job, everything else standing alone — which
// is the arrangement the window shows and therefore a decision that belongs
// here rather than in QML. They cannot drift apart: both are built from the
// same row function over the same slice, in the same pass.
func undoViewReport(a undoAccount) map[string]any {
	v := a.view
	rows := make([]map[string]any, 0, len(v.Records))
	for _, r := range v.Records {
		rows = append(rows, undoRowReport(r, v.Now, a.offer))
	}
	return map[string]any{
		"actions":    rows,
		"groups":     undoGroups(v.Records, rows, a),
		"bound":      v.Bound,
		"forgotten":  v.Forgotten,
		"disclosure": v.Disclosure(),
		"empty":      undoEmptySentence,
		"path":       v.Path,
	}
}

// undoEmptySentence is what the account says when it holds nothing. First
// person and in one place, like the disclosure beside it: "nothing has been
// changed in your name" is a claim about the machine, and a claim worded
// separately by the CLI and by the window would be two claims.
const undoEmptySentence = "I haven't changed anything on this machine."

// undoRowReport renders one record.
//
// Everything a surface shows about a row is composed here, including when it
// happened and what its standing is. That is not decoration: a client that
// formatted a timestamp would be measuring another machine's clock with its
// own, and a client that decided which of "can be put back" / "can't be put
// back" / "already put back" to write would be wording an eligibility it did
// not compute. Both come back as strings so the surface can only render them.
func undoRowReport(r undo.Record, now time.Time, offer func(undo.Record) (bool, string)) map[string]any {
	canUndo, why := r.Reversible(), r.Why()
	if offer != nil {
		canUndo, why = offer(r)
	}
	row := map[string]any{
		"id":   r.ID,
		"at":   r.At.Format(time.RFC3339),
		"when": undo.Ago(now, r.At),
		"tool": r.Tool,
		// Reversible is the record's own property — this action left something
		// behind that would restore it — and can_undo is whether the offer
		// stands right now, which the gate can withhold from a record that is
		// perfectly reversible. Two facts, two fields: collapsing them would
		// lose the difference between "there is nothing to put back" and "you
		// have told me not to".
		"summary":    r.Summary,
		"reversible": r.Reversible(),
		"can_undo":   canUndo,
		"state":      undoRowState(r, now, canUndo, why),
	}
	if r.Target != "" {
		row["target"] = r.Target
	}
	if r.Job != "" {
		row["job"] = r.Job
	}
	if why != "" {
		row["why"] = why
	}
	if r.UndoneBy != "" {
		row["undone_by"] = r.UndoneBy
		row["undone_at"] = r.UndoneAt.Format(time.RFC3339)
	}
	if len(r.Provenance) > 0 {
		row["provenance"] = append([]string(nil), r.Provenance...)
		row["sources"] = undoSources(r.Provenance)
	}
	return row
}

// undoRowState is the row's standing in one sentence.
//
// Three states and no more, matching the three a reader acts on differently:
// it has already been put back (and by what), it can be put back, or it cannot
// (and why). The reason is the same string `why` carries so the sentence and
// the field cannot disagree; it is spelled out here as well because a surface
// showing only `why` would have to supply its own lead-in, and a lead-in is
// wording.
func undoRowState(r undo.Record, now time.Time, canUndo bool, why string) string {
	if r.UndoneBy != "" {
		if r.UndoneAt.IsZero() {
			return "I put this back — that reversal is " + r.UndoneBy + "."
		}
		return "I put this back " + undo.Ago(now, r.UndoneAt) +
			" — that reversal is " + r.UndoneBy + "."
	}
	if !canUndo {
		if strings.TrimSpace(why) == "" {
			return "I can't put this back."
		}
		return "I can't put this back — " + why + "."
	}
	return "I can put this back."
}

// undoSources splits the account's stored provenance references into the shape
// provenance.resolve takes.
//
// The account stores a reference as "kind:ref" — one string, because that is
// what fits a hand-editable TOML line. Splitting it here rather than in the
// window is the same rule as everything else in this file: an encoding is the
// daemon's, and a client that learned to parse one would keep parsing it after
// the daemon stopped writing it. What comes back is exactly what the answer
// panel already sends, so the account's sources are resolved and opened by the
// verbs that already do it (#168) rather than by a second path.
//
// A reference with no colon is dropped rather than guessed at. A malformed
// line in a file the user may edit by hand is not a source, and inventing a
// kind for it would produce a row in the panel that resolves to nothing.
func undoSources(refs []string) []map[string]any {
	out := make([]map[string]any, 0, len(refs))
	for _, ref := range refs {
		kind, id, ok := strings.Cut(ref, ":")
		if !ok || strings.TrimSpace(kind) == "" || strings.TrimSpace(id) == "" {
			continue
		}
		out = append(out, map[string]any{"kind": kind, "ref": id})
	}
	return out
}

// undoGroups arranges the account the way somebody reviewing work reads it:
// grouped by job where a job exists, chronological otherwise (#210).
//
// A job's group is placed where its NEWEST action falls, and holds every action
// of that job in the same newest-first order the flat list uses. That keeps one
// reading order for the whole page — nothing jumps backwards in time between
// groups — while putting a job's twelve steps under one heading instead of
// scattering them through the eleven other things that happened while it ran.
//
// An action that belonged to no job is a group of one with no heading, rather
// than a separate list beside the grouped ones. Two lists would force a reader
// to merge them by eye to answer "what happened last", which is the question
// the account exists to answer.
func undoGroups(records []undo.Record, rows []map[string]any, a undoAccount) []map[string]any {
	groups := make([]map[string]any, 0, len(records))
	at := map[string]int{}
	for i, r := range records {
		if i >= len(rows) {
			break
		}
		if r.Job == "" {
			groups = append(groups, map[string]any{
				"job": "", "heading": "", "can_undo": false,
				"actions": []map[string]any{rows[i]},
			})
			continue
		}
		index, seen := at[r.Job]
		if !seen {
			groups = append(groups, map[string]any{"job": r.Job,
				"actions": []map[string]any{}})
			index = len(groups) - 1
			at[r.Job] = index
		}
		held, _ := groups[index]["actions"].([]map[string]any)
		groups[index]["actions"] = append(held, rows[i])
	}
	// The heading and the whole-job offer need the group's size and its
	// members' eligibility, so they are settled once every row has landed
	// rather than guessed at as the first one arrives.
	for _, g := range groups {
		id, _ := g["job"].(string)
		if id == "" {
			continue
		}
		held, _ := g["actions"].([]map[string]any)
		job, known := jobs.Job{}, false
		if a.job != nil {
			job, known = a.job(id)
		}
		g["heading"] = undoJobHeading(id, job, known, len(held))
		canUndo, why, note := undoJobOffer(job, known, held)
		g["can_undo"] = canUndo
		if why != "" {
			g["why"] = why
		}
		if note != "" {
			g["note"] = note
		}
	}
	return groups
}

// undoJobHeading names a group. The job's own name where the store still has
// it, and the id where it does not — the account outlives the job list, and a
// group headed by nothing would be a heading that hid which work it was.
func undoJobHeading(id string, job jobs.Job, known bool, n int) string {
	name := id
	if known && strings.TrimSpace(job.Name) != "" {
		name = job.Name
	}
	unit := "actions"
	if n == 1 {
		unit = "action"
	}
	return fmt.Sprintf("The job %q — %d %s", name, n, unit)
}

// undoJobOffer decides whether the whole-job control is offered, and says what
// pressing it will also do when that is more than a reversal.
//
// The refusal a running job earns is the one settleBeforeUndo raises on the
// apply path, and it is the same sentence because it is the same function: a
// control offered here and refused there would be exactly the dead affordance
// this surface exists to avoid. A PARKED job may be undone and saying so is not
// enough — undoing it stops it, because resuming from a checkpoint whose
// effects have been reversed would be resuming into a world the checkpoint does
// not describe (ADR 0065). That consequence rides the group as a note, before
// the press rather than after it, on the confirmation card's own argument.
func undoJobOffer(job jobs.Job, known bool, rows []map[string]any) (can bool, why, note string) {
	if known {
		switch job.State {
		case jobs.Ready, jobs.Running:
			return false, undoJobBusy(job.Name), ""
		}
	}
	for _, row := range rows {
		if offered, _ := row["can_undo"].(bool); offered {
			if known && job.State == jobs.Parked {
				return true, "", fmt.Sprintf(
					"%q is waiting on you; putting its work back stops it.", job.Name)
			}
			return true, "", ""
		}
	}
	return false, "there is nothing left in this job that I can put back", ""
}

// undoJobBusy is why a live job cannot be reversed, worded once. Read by the
// listing that withholds the control and by the apply path that refuses the
// call, so the two cannot describe the same state differently.
func undoJobBusy(name string) string {
	return name + " is still working; stop it first and then I can put back what it did"
}

// undoOutcomeReport renders one reversal.
func undoOutcomeReport(o undo.Outcome) map[string]any {
	out := map[string]any{"done": o.Done, "refused": o.Refused, "spoken": o.Spoken}
	if o.Record.ID != "" {
		out["id"] = o.Record.ID
		out["summary"] = o.Record.Summary
	}
	if o.Reversal.ID != "" {
		out["reversal_id"] = o.Reversal.ID
	}
	return out
}

// undoJobReport renders a job-scoped reversal: the report, plus every action
// it tried, so a caller can show which were put back and which were not
// without re-deriving it from prose.
func undoJobReport(o undo.JobOutcome) map[string]any {
	rows := make([]map[string]any, 0, len(o.Outcomes))
	for _, one := range o.Outcomes {
		rows = append(rows, undoOutcomeReport(one))
	}
	return map[string]any{"job": o.Job, "spoken": o.Spoken, "actions": rows}
}
