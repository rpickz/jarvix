package undo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// This file puts things back.
//
// Three rules govern every reversal here, and they are the ticket's spine:
//
//  1. **An undo is judged as the action it performs.** Reversing a config
//     write IS a config write, so it faces the tier `config.write_entry`
//     faces. The gate is not consulted about a new "undo" capability nobody
//     configured — it is asked the question it already knows the answer to.
//     A tier of deny refuses the reversal outright: a user who turned config
//     writes off gets them off, including under another name.
//
//  2. **When it cannot tell, it refuses.** Every reversal carries a guard —
//     the digest of what the action left behind, the four facts that identify
//     a window — and a guard that does not match means something changed
//     since. Jarvix cannot know whose change it was or whether it mattered,
//     so it says what it found and does nothing. Restoring over newer work is
//     not an undo, it is a second loss.
//
//  3. **A reversal is itself an action.** It gets its own record, and the row
//     it reversed is marked. The account is what happened, and putting
//     something back is a thing that happened. Those two are one write, not
//     two ordered ones (#219): the account is consistent at the moment it
//     announces itself, and there is no intermediate state for a crash to
//     leave behind or for a second window to read.

// Decision is the gate's answer for one tool identity. It mirrors
// tools.PolicyDecision without importing it: internal/tools imports this
// package, so the dependency has to point the other way, and the three words
// are the gate's own.
type Decision int

const (
	// DecisionAllow means the reversal may proceed without asking.
	DecisionAllow Decision = iota
	// DecisionAsk means it must be confirmed. The caller owns the asking —
	// the tool path raises the ordinary confirmation card; the CLI path is
	// the manager's own hand and does not.
	DecisionAsk
	// DecisionDeny means it must not happen at all.
	DecisionDeny
)

// Gate judges a reversal under the identity of the action it reverses. The
// daemon implements it over tools.Policy.ToolDecision; a nil Gate allows
// everything, which is the construction tests use and never the daemon's.
type Gate interface {
	// Judge returns the tier configured for a tool identity.
	Judge(tool string) Decision
}

// Placer puts a window back. It is an interface, and WindowState is plain
// fields, so this package depends on no compositor: internal/desktop owns
// the one seam that talks to Hyprland (ADR 0022) and a second one here would
// be a second place deciding what is on screen.
type Placer interface {
	// Restore moves one window back to the state it held. It must verify the
	// window's identity against the live inventory first and return a refusal
	// naming what it found when the window has gone — the caller turns that
	// into the sentence the user hears.
	Restore(ctx context.Context, want WindowState) error
}

// Outcome is what one reversal did, worded for a person.
type Outcome struct {
	// Done is true when something was actually put back.
	Done bool
	// Record is the row that was reversed (zero when nothing was).
	Record Record
	// Reversal is the row the reversal itself earned (zero when nothing was
	// done).
	Reversal Record
	// Spoken is the sentence to say: "I've put your config back the way it
	// was", or the refusal and its reason. Always non-empty.
	Spoken string
	// Refused marks an outcome that was possible in principle and was
	// declined in this instance — the clobber guard, the gate, a window that
	// has gone. Distinguished from Done because "I didn't" and "I couldn't"
	// are different answers and a caller may want to exit non-zero on one.
	Refused bool
}

// JobOutcome is what reversing a whole job did.
type JobOutcome struct {
	// Job is the id that was asked for.
	Job string
	// Outcomes are per action, in the order they were attempted — reverse
	// chronological, because putting a piece of work back means undoing the
	// last step first.
	Outcomes []Outcome
	// Spoken is the report: which were reversed and which could not be.
	Spoken string
}

// Undoer reverses recorded actions.
type Undoer struct {
	store  *Store
	gate   Gate
	placer Placer
	// writeFile is the disk seam for a file restore, so the
	// refuse-rather-than-clobber tests can prove what was and was not
	// written without a real failure.
	writeFile func(path string, data []byte) error
}

// NewUndoer builds the reverser. gate nil allows everything; placer nil
// refuses window reversals with a stated reason rather than pretending.
func NewUndoer(store *Store, gate Gate, placer Placer) *Undoer {
	return &Undoer{store: store, gate: gate, placer: placer, writeFile: writeFileAtomic}
}

// Last reverses the most recent reversible action.
//
// When the most recent action of all cannot be undone, that is what the
// answer says first, and it names the reversible one behind it rather than
// silently reaching past — "the last thing I did was run a command, and I
// can't take that back" is the sentence the ticket asks for, and skipping
// straight to the config write would answer a question nobody asked.
func (u *Undoer) Last(ctx context.Context) (Outcome, error) {
	if u == nil || u.store == nil {
		return Outcome{Spoken: "I'm not keeping an account of what I've done, so there's nothing to undo."}, nil
	}
	reversible, last, ok := u.store.Latest()
	if !ok {
		return Outcome{Spoken: "I haven't done anything on this machine yet, so there's nothing to undo."}, nil
	}
	if reversible.ID == "" {
		return Outcome{
			Refused: true,
			Spoken: fmt.Sprintf("The last thing I did was %s, and I can't undo that — %s. "+
				"Nothing further back can be undone either.", last.Summary, last.Why()),
		}, nil
	}
	if reversible.ID != last.ID {
		out, err := u.Apply(ctx, reversible.ID)
		if err != nil {
			return out, err
		}
		out.Spoken = fmt.Sprintf("The last thing I did was %s, and I can't undo that — %s. %s",
			last.Summary, last.Why(), out.Spoken)
		return out, nil
	}
	return u.Apply(ctx, reversible.ID)
}

// Offer answers the question a surface listing the account has to ask before
// it draws a control: **would a reversal of this record be attempted at all,
// and if not, in what words?** (#210.)
//
// It lives here, beside Apply, rather than in whichever surface draws the
// button, because it has to answer with Apply's own reasons. The two checks
// below are the two Apply makes before it touches anything — the record's own
// reversibility, and the gate under the identity of the action being reversed
// — so a listing built on this cannot offer a control that refuses when
// pressed, nor withhold one that would have worked. "The button did nothing"
// is the shrug ADR 0064 exists to replace, and a second implementation of
// eligibility in a client is how a feature grows one.
//
// What it deliberately does NOT predict is the clobber guard. Whether the file
// still hashes to what the action left behind is a fact about the disk *at the
// moment of the press*, and a listing that had checked it a minute ago would be
// making a promise it had no way to keep. That refusal belongs at Apply, in
// words, and the account is left unchanged by it so the offer still stands once
// the person has looked.
func (u *Undoer) Offer(rec Record) (bool, string) {
	if !rec.Reversible() {
		return false, rec.Why()
	}
	if u != nil && u.gate != nil && u.gate.Judge(rec.Tool) == DecisionDeny {
		return false, gateRefusal(rec.Tool)
	}
	return true, ""
}

// gateRefusal is the clause a denied tool identity earns. Worded once so the
// listing that withholds a control and the reversal that declines to run
// cannot describe the same standing instruction in two different ways.
func gateRefusal(tool string) string {
	return "putting it back means another " + tool + ", and you have that turned off"
}

// Apply reverses one recorded action by id.
func (u *Undoer) Apply(ctx context.Context, id string) (Outcome, error) {
	if u == nil || u.store == nil {
		return Outcome{Spoken: "I'm not keeping an account of what I've done, so there's nothing to undo."}, nil
	}
	rec, err := u.store.Get(id)
	if err != nil {
		return Outcome{}, err
	}
	if !rec.Reversible() {
		return Outcome{
			Record: rec, Refused: true,
			Spoken: fmt.Sprintf("I can't undo %s — %s.%s", rec.Summary, rec.Why(), u.adjacent(rec)),
		}, nil
	}
	// The gate, under the identity of the action being reversed. Deny is
	// final here rather than a question, because the tier is the user's
	// standing instruction about that capability and an undo is not an
	// exception to it.
	if u.gate != nil && u.gate.Judge(rec.Tool) == DecisionDeny {
		return Outcome{
			Record: rec, Refused: true,
			Spoken: fmt.Sprintf("I can't undo %s: %s.", rec.Summary, gateRefusal(rec.Tool)),
		}, nil
	}

	var performed string
	switch rec.Restore.Kind {
	case KindFile:
		performed, err = u.restoreFile(rec)
	case KindWindow:
		performed, err = u.restoreWindow(ctx, rec)
	default:
		return Outcome{Record: rec, Refused: true,
			Spoken: fmt.Sprintf("I can't undo %s — %s.", rec.Summary, rec.Why())}, nil
	}
	if err != nil {
		var refusal *Refusal
		if errors.As(err, &refusal) {
			return Outcome{Record: rec, Refused: true, Spoken: refusal.Spoken}, nil
		}
		return Outcome{Record: rec, Refused: true,
			Spoken: fmt.Sprintf("I couldn't undo %s: %v.", rec.Summary, err)}, nil
	}

	// The reversal earns its own row and the row it reversed is marked, in one
	// write (#219). They used to be two, in this order, and the account was
	// observably torn between them: `undo.changed` fires on the first, so a
	// second window re-reading on that event saw the reversal listed beside a
	// row that still offered to undo something already undone. Ordering them
	// the other way would only have moved the window; one write removes it,
	// at rest as well as in flight — see Store.Reverse.
	reversal, recordErr := u.store.Reverse(rec.ID, Action{
		Tool:    rec.Tool,
		Summary: "undid " + rec.Summary,
		Target:  rec.Target,
		Job:     rec.Job,
		Restore: Restore{Kind: KindNone,
			Because: "undoing an undo is not something I offer; " +
				"ask for the change again if you want it back"},
	})
	if recordErr != nil {
		// The reversal HAPPENED — the file is back, the window has moved —
		// and only the account of it failed. So it is reported as done with
		// what could not be confirmed said out loud, never as an error: the
		// jobs ledger's rule, that a step whose outcome could not be
		// confirmed is reported as unverified rather than as done or as
		// never-started, applies just as much to a step whose outcome was
		// certain and whose record was not. Returning an error here would
		// have described a reversal that plainly did happen as one that did
		// not, which is the reading the whole account exists to prevent.
		//
		// The store has already logged the write failure with the path. The
		// row therefore still stands in the account and still offers a
		// control — which is honest, because the account genuinely does not
		// know, and pressing it again meets the clobber guard rather than a
		// second reversal.
		return Outcome{
			Done: true, Record: rec,
			Spoken: performed + " I couldn't write that down in my account, though, " +
				"so it will still be listed as something I can undo.",
		}, nil
	}
	return Outcome{Done: true, Record: rec, Reversal: reversal, Spoken: performed}, nil
}

// JobActions reverses every reversible action a job took, in reverse order,
// and reports which were reversed and which could not be.
//
// One confirmation covers the whole job, not one per step (#200, ADR 0065).
// The manager's decision is "put the deploy job back" — a single judgement
// about a single piece of work — and asking it twelve times converts one
// decision into a fatigue test whose twelfth answer is not a judgement. It
// widens nothing the gate protects: a tool identity the policy denies still
// refuses per action below, and the clobber guard still refuses per file.
//
// A job that is still RUNNING is refused before it gets here, by the caller
// that owns the job's state. See ADR 0065.
func (u *Undoer) JobActions(ctx context.Context, job string) (JobOutcome, error) {
	if u == nil || u.store == nil {
		return JobOutcome{Job: job, Spoken: "I'm not keeping an account of what I've done."}, nil
	}
	records := u.store.Job(job)
	if len(records) == 0 {
		return JobOutcome{Job: job,
			Spoken: fmt.Sprintf("I have nothing in the account for %s.", job)}, nil
	}
	out := JobOutcome{Job: job, Outcomes: make([]Outcome, 0, len(records))}
	var done, could []string
	// Backwards: the last step is undone first, because an earlier step may
	// be what a later one depended on and putting them back in the order they
	// happened would restore a state that never existed.
	for i := len(records) - 1; i >= 0; i-- {
		o, err := u.Apply(ctx, records[i].ID)
		if err != nil {
			return out, err
		}
		out.Outcomes = append(out.Outcomes, o)
		if o.Done {
			done = append(done, records[i].Summary)
			continue
		}
		could = append(could, records[i].Summary+" ("+records[i].Why()+")")
	}
	out.Spoken = jobReport(job, done, could)
	return out, nil
}

// jobReport words what a job-scoped undo managed and what it did not. Both
// halves always, because a report that named only the successes would read as
// if the job had been fully reversed.
func jobReport(job string, done, could []string) string {
	switch {
	case len(done) == 0:
		return fmt.Sprintf("I couldn't undo anything from %s: %s.", job, strings.Join(could, "; "))
	case len(could) == 0:
		return fmt.Sprintf("I undid everything %s did: %s.", job, strings.Join(done, "; "))
	default:
		return fmt.Sprintf("I undid %s. I couldn't undo %s.",
			strings.Join(done, "; "), strings.Join(could, "; "))
	}
}

// adjacent names something reversible near an action that is not, so a
// refusal is never a dead end. The ticket asks for exactly this: "asking to
// undo it says exactly why it cannot be, and names anything adjacent that
// *can* be."
func (u *Undoer) adjacent(rec Record) string {
	view := u.store.List()
	for _, r := range view.Records {
		if r.ID == rec.ID || !r.Reversible() {
			continue
		}
		return fmt.Sprintf(" I can undo %s (%s) if that helps.", r.Summary, r.ID)
	}
	return ""
}

// Refusal is a reversal Jarvix declined, carrying the sentence to say. It is
// an error type so the restore functions can refuse from anywhere without a
// second return value that every caller would have to remember to check.
type Refusal struct{ Spoken string }

func (r *Refusal) Error() string { return r.Spoken }

// restoreFile writes a file back to the bytes it held, or removes a file the
// action created.
//
// The guard is the whole of the refuse-rather-than-clobber promise. The
// record carries the digest of what the action LEFT BEHIND; if the file no
// longer hashes to that, somebody changed it since — the user in an editor,
// another action, a sync from another machine — and there is no way from here
// to tell whether that change matters. So it refuses, says what it found, and
// says where the file is, which is enough for a person to look and decide.
func (u *Undoer) restoreFile(rec Record) (string, error) {
	f := rec.Restore.File
	current, err := os.ReadFile(f.Path)
	switch {
	case err == nil:
		if f.AfterDigest == "" {
			return "", &Refusal{Spoken: fmt.Sprintf(
				"I won't undo %s: I didn't record what %s looked like straight after the change, "+
					"so I can't tell whether putting it back would overwrite newer work.",
				rec.Summary, f.Path)}
		}
		if digestBytes(current) != f.AfterDigest {
			return "", &Refusal{Spoken: fmt.Sprintf(
				"I won't undo %s: %s has changed since, so putting it back would overwrite "+
					"whatever that change was. Have a look and tell me if you still want it.",
				rec.Summary, f.Path)}
		}
	case os.IsNotExist(err):
		if !f.Existed {
			// The file the action created has already gone. Nothing to do,
			// and saying "done" would claim work nobody did.
			return "", &Refusal{Spoken: fmt.Sprintf(
				"There's nothing to undo: %s is already gone.", f.Path)}
		}
		return "", &Refusal{Spoken: fmt.Sprintf(
			"I won't undo %s: %s isn't there any more, so I can't tell what removed it.",
			rec.Summary, f.Path)}
	default:
		return "", &Refusal{Spoken: fmt.Sprintf(
			"I couldn't read %s to check it was safe to put back, so I've left it alone.", f.Path)}
	}

	if !f.Existed {
		if err := os.Remove(f.Path); err != nil {
			return "", err
		}
		return fmt.Sprintf("I've removed %s again — it wasn't there before.", f.Path), nil
	}
	if err := u.writeFile(f.Path, []byte(f.Previous)); err != nil {
		return "", err
	}
	return fmt.Sprintf("I've put %s back the way it was before I %s.",
		filepath.Base(f.Path), rec.Summary), nil
}

// restoreWindow puts a window back where it was.
func (u *Undoer) restoreWindow(ctx context.Context, rec Record) (string, error) {
	if u.placer == nil {
		return "", &Refusal{Spoken: fmt.Sprintf(
			"I can't undo %s: I can't reach the window manager from here.", rec.Summary)}
	}
	if err := u.placer.Restore(ctx, *rec.Restore.Window); err != nil {
		var refusal *Refusal
		if errors.As(err, &refusal) {
			return "", refusal
		}
		return "", err
	}
	what := rec.Restore.Window.Describe
	if strings.TrimSpace(what) == "" {
		what = "the window"
	}
	return fmt.Sprintf("I've put %s back where it was.", what), nil
}

// writeFileAtomic writes a restore through the same fsync-and-rename
// discipline the account itself uses. A restore that tore would be the worst
// possible outcome of an undo: the user asked for their file back and got
// half of it.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".undo-restore-*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// The restored file keeps the mode it had, not the mode CreateTemp and
	// the umask happen to produce. An undo that quietly widened the
	// permissions of config.toml — which holds api keys — would be a security
	// change wearing a reversal's name; one that narrowed them would break a
	// setup the user had chosen. 0600 only when the file is somehow gone,
	// which restoreFile has already ruled out before calling here.
	mode := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return syncDir(dir)
}
