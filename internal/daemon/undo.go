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
		return undoViewReport(d.account.List()), nil
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
		return fmt.Errorf("%s is still working; stop it first and then I can put back what it did",
			job.Name)
	case jobs.Parked:
		if _, err := d.jobRunner.Stop(job.ID,
			"You asked me to undo what it had done, so I stopped it first."); err != nil {
			return err
		}
	}
	return nil
}

// undoViewReport renders the account for the wire.
//
// The disclosure travels as a sentence the daemon composed, not as two
// numbers a client is trusted to word (ADR 0013). It is the whole point of
// the bound being a bound rather than a silent truncation: every surface says
// the same thing about what is missing because there is only one sentence.
//
// The restore payload never travels. A listing carrying the previous contents
// of config.toml would put the user's api keys on every connected socket, and
// nothing that renders the account needs them — "this can be put back" is the
// fact a reader acts on, and the bytes stay in the file.
func undoViewReport(v undo.View) map[string]any {
	rows := make([]map[string]any, 0, len(v.Records))
	for _, r := range v.Records {
		row := map[string]any{
			"id":         r.ID,
			"at":         r.At.Format(time.RFC3339),
			"tool":       r.Tool,
			"summary":    r.Summary,
			"reversible": r.Reversible(),
		}
		if r.Target != "" {
			row["target"] = r.Target
		}
		if r.Job != "" {
			row["job"] = r.Job
		}
		if why := r.Why(); why != "" {
			row["why"] = why
		}
		if r.UndoneBy != "" {
			row["undone_by"] = r.UndoneBy
			row["undone_at"] = r.UndoneAt.Format(time.RFC3339)
		}
		if len(r.Provenance) > 0 {
			row["provenance"] = append([]string(nil), r.Provenance...)
		}
		rows = append(rows, row)
	}
	return map[string]any{
		"actions":    rows,
		"bound":      v.Bound,
		"forgotten":  v.Forgotten,
		"disclosure": v.Disclosure(),
		"path":       v.Path,
	}
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
