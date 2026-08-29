// Package undo is the account of what Jarvix did in the user's name, and the
// machinery for putting it back (#201, ADR 0064, part of the operator
// direction #195).
//
// The permission gate (ADR 0014) answers *may I*. The activity feed answers
// *what happened*, one row at a time, in the past tense and with no handle on
// it. Nothing answered *what did you do* as a piece of work, and nothing
// answered *put it back*. Delegation without accountability is not
// delegation: a manager has to be able to see what was done in their name,
// judge it, and reverse it.
//
// Three ideas carry the whole package.
//
//   - **A record is written where the action happens.** Not inferred from the
//     feed, not reconstructed from a log line. The tool that changed
//     something is the only code that knows what it changed and what would
//     put it back, and it says so through the context seam below — the same
//     arrangement internal/provenance uses, and for the same reason: the
//     things with something to say say it, and every other tool pays nothing.
//
//   - **Reversible and irreversible are different words and are never
//     blurred.** A config write can be put back byte for byte. A shell
//     command that has run has run. The account says which, at the time the
//     action happens, and — this is the half that matters — the confirmation
//     card says it *before* approval. A manager should know which decisions
//     are one-way at the moment they make them, not afterwards.
//
//   - **An undo is itself a consequential action.** It goes through the gate
//     under the identity of the action it reverses, and when it cannot tell
//     whether restoring would destroy newer work it refuses rather than
//     clobbering. "I can't tell" is an answer; a silent overwrite is not.
//
// What is deliberately NOT here: reversing anything outside Jarvix's own
// actions. A general-purpose filesystem time machine is a different product,
// and undoing what the *user* did is not this feature's business. The account
// records Jarvix acting, which in practice means the assistant's tools — a
// person typing `jarvix config set` is the manager's own hand, and a manager
// does not need an account of themselves.
//
// The store is on the discipline every durable store here keeps (ADR 0011):
// one hand-editable TOML file under the XDG state dir, 0600 in a 0700
// directory, atomic fsync-and-rename writes, stat-based hand-edit pickup, a
// corrupt document moved aside rather than overwritten, ids that are never
// reused — proved rather than promised, by registration with
// internal/storefault's shared suite. It is bounded, and the bound is
// disclosed on every listing: an account that silently forgets is worse than
// one that says "I only keep the last hundred".
package undo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"time"
)

// MaxActions bounds the account. A hundred is chosen against what the bound
// is for rather than as a round number: "undo that" reaches the last one,
// "undo the deploy job" reaches a job's worth, and past a hundred nobody is
// reviewing — they are searching, which is a different feature. The cap
// exists because a file that can grow without limit is a file a stuck loop
// can fill, and — unlike every other store here — this one cannot refuse at
// the cap: refusing to record would mean an action that happened with nothing
// in the account, which is the one outcome the feature exists to prevent. So
// it evicts the oldest, counts what it evicted, and says so.
const MaxActions = 100

// MaxRestoreBytes caps the "what would restore it" one record may carry.
//
// For a file that is the previous bytes, and 64 KiB is far above every
// configuration and state file Jarvix writes (config.toml is a few kilobytes
// on a heavily-used machine). The cap is not a guess about typical size, it
// is a ceiling on the worst case: a hundred records each carrying an
// arbitrarily large file would be a state directory that does not fit on the
// disk, arrived at by an assistant doing its job.
//
// An action whose previous bytes exceed it is recorded as IRREVERSIBLE, with
// that as the stated reason — at the time it happens, and therefore visible
// in the account rather than discovered when somebody says "undo that". A
// half-kept copy would be worse than none: it would restore a truncated file
// over a whole one.
const MaxRestoreBytes = 64 * 1024

// Kind is how a record can be put back. Three, and no more, because three is
// what the reversals actually are: rewrite a file to bytes we kept, put a
// window back where it was, or say honestly that nothing here can be undone.
type Kind string

const (
	// KindFile restores one file to the bytes it held before the action.
	// Creation is the same kind with Existed false — restoring then means
	// removing the file, which is what "put it back" means for something
	// that did not exist before.
	//
	// It covers far more than configuration: every store Jarvix writes by
	// voice — the memory book, the taught vocabulary, the reminders — is one
	// TOML document rewritten whole, so "the previous bytes" is a complete
	// and exact answer for all of them, with no per-store reversal logic to
	// write, get wrong, and drift.
	KindFile Kind = "file"
	// KindWindow puts one window back where it was: workspace, floating,
	// fullscreen, and geometry. A file cannot describe this one — the
	// compositor holds the state — so it is the one bespoke reversal.
	KindWindow Kind = "window"
	// KindNone is an action that genuinely cannot be undone. It is recorded
	// all the same, with Because saying why in the words the user hears.
	// This is the honest half of the feature: a shell command the user
	// approved is recorded and described, never falsely promised as undoable.
	KindNone Kind = "none"
)

// Action is what a tool reports at the moment it changes something. It is the
// caller's half of a Record: the store fills in the id, the time, and the
// bookkeeping.
type Action struct {
	// Tool is the gate identity that acted — "config.write_entry",
	// "shell.run". It is the join between the account and everything else
	// that names a capability: the policy's tiers, the activity feed's
	// wording, the confirmation the user answered.
	Tool string
	// Summary is one plain sentence saying what changed, in the past tense
	// and without a subject: "saved the routine \"morning\"". The account
	// reads as a list of these, and "undo that" says one of them back.
	Summary string
	// Target names what was touched, for the account's provenance column: a
	// file path, a window's description, an artifact's name.
	Target string
	// Job is the piece of work this action belonged to (#200). Empty today,
	// because jobs do not exist yet — it is here so that grouping is a query
	// the day they land rather than a migration. See ADR 0064.
	Job string
	// Provenance are the #168 references the action touched, stored as
	// references and never as content, exactly as internal/provenance does.
	Provenance []string
	// Restore is how to put it back, or the honest refusal to promise. The
	// zero value is KindNone with no reason, which renders as "I can't say
	// how to undo that" — the safe default for a caller that forgets to
	// fill it in, because it promises nothing.
	Restore Restore
}

// Restore is the "what would restore it" half of a record.
type Restore struct {
	Kind Kind
	// Because says why an action cannot be undone, in the words the user
	// hears. Only meaningful for KindNone, and required there: "it cannot be
	// undone" without a reason is a shrug.
	Because string
	// File is set for KindFile.
	File *FileRestore
	// Window is set for KindWindow.
	Window *WindowState
}

// FileRestore is one file's previous state, plus the guard that decides
// whether putting it back is safe.
type FileRestore struct {
	// Path is the file the action wrote.
	Path string
	// Existed is false when the action created the file. Restoring then
	// removes it.
	Existed bool
	// Previous is the exact bytes the file held before the action. Exact
	// rather than a diff or a re-derivation: the byte-preserving rewriters in
	// internal/config keep comments, key order and spacing that no
	// re-serialisation would reproduce, and an undo that lost a user's
	// comments would be a second edit dressed as a reversal.
	Previous string
	// AfterDigest is the SHA-256 of the bytes the action LEFT BEHIND, hex,
	// unprefixed. It is the clobber guard and the whole of the
	// refuse-rather-than-clobber promise: if the file no longer hashes to
	// this, something changed it since — the user in an editor, another
	// action, another machine — and restoring would silently destroy that
	// newer work. Jarvix cannot tell whose work it is or whether it matters,
	// so it refuses and says what it found.
	//
	// A digest rather than the bytes because this half never needs to be
	// written back, only compared, and keeping a second copy of every file
	// would double a bound that exists to stay small.
	AfterDigest string
}

// WindowState is where a window was before it was placed, and everything
// needed to put it back.
//
// The identity is the managed-window store's four facts (ADR 0062), and for
// its reason: a compositor address is a pointer value that gets recycled, so
// a reversal matching on it alone would eventually move a stranger's window.
// All four must agree or the undo refuses — the same refuse-rather-than-guess
// answer the file guard gives.
type WindowState struct {
	Address  string
	StableID string
	Class    string
	PID      int
	// Describe is how the window read to a person when it was moved, kept so
	// the account can name it ("Firefox — GitHub") without a live inventory.
	// A description, never a title used as an identity.
	Describe string

	Workspace     int
	WorkspaceName string
	Floating      bool
	Fullscreen    bool
	X, Y          int
	Width, Height int
}

// Record is one entry in the account: an Action, plus what the store knows.
//
// This shape is #200's contract. A job id is a field the record already
// carries, so "undo the deploy job" is `Store.Job(id)` in reverse order the
// day jobs exist — nothing about the record changes, no migration runs, and
// the reversal machinery is the same machinery. What #200 has to supply is
// the id and the code that sets it; see ADR 0064's "What remains for #200".
type Record struct {
	// ID is "a17", minted by the store, never reused — including across a
	// restart, and including after the record holding it has been dropped.
	ID string
	// At is when the action happened, UTC.
	At time.Time
	Action
	// UndoneBy is the id of the record that reversed this one, empty while it
	// still stands. A reversal is itself an action and earns its own record,
	// so the account never has to be read backwards to know what is current.
	UndoneBy string
	// UndoneAt is when that happened.
	UndoneAt time.Time
}

// Reversible reports whether this record can be put back. It is false for
// KindNone, for a record already undone, and for a record whose restore
// payload is missing — a hand-edit that deleted the [action.file] stanza
// leaves a row that says what happened and cannot promise a reversal, which
// is the honest reading of a file the user edited.
func (r Record) Reversible() bool {
	if r.UndoneBy != "" {
		return false
	}
	switch r.Restore.Kind {
	case KindFile:
		return r.Restore.File != nil
	case KindWindow:
		return r.Restore.Window != nil
	default:
		return false
	}
}

// Why says why a record cannot be reversed, in one clause fit to be spoken.
// Empty when it can be.
func (r Record) Why() string {
	if r.Reversible() {
		return ""
	}
	if r.UndoneBy != "" {
		return "I already put that back"
	}
	if because := strings.TrimSpace(r.Restore.Because); because != "" {
		return because
	}
	return "I didn't keep anything that would restore it"
}

// ---------------------------------------------------------------------------
// The context seam
// ---------------------------------------------------------------------------

// Recorder is what the seam carries. It is an interface rather than the Store
// so that a caller with nothing to record — a CLI invocation, a test of a
// tool's wording — installs nothing and every Note becomes a no-op.
type Recorder interface {
	// Append writes one action to the account and returns the record it
	// became. A failure is the store's to report and log; callers do not
	// unwind their work over it, because an action that succeeded and was not
	// recorded is still an action that succeeded, and pretending otherwise
	// would make the account's failure mode "lose the user's change".
	Append(a Action) (Record, error)
}

type recorderKey struct{}

// WithRecorder returns a context whose tool calls record what they change.
//
// A context is the transport for internal/provenance's reason, restated: the
// code that knows what changed — the config bridge knows which file it wrote
// after the fingerprint round trip, the window tools know the geometry the
// compositor reported a moment ago — is reached through interfaces whose
// signatures belong to what they do. Threading a recorder through every one
// of them would put this feature in every tool's contract; a context lets
// exactly the tools that change the machine say so, and costs the rest
// nothing.
//
// A nil recorder installs nothing, so `WithRecorder(ctx, nil)` is the same
// context back and every Note below is a no-op.
func WithRecorder(ctx context.Context, r Recorder) context.Context {
	if r == nil {
		return ctx
	}
	return context.WithValue(ctx, recorderKey{}, r)
}

// RecorderFrom returns the recorder installed on ctx, nil when there is none.
func RecorderFrom(ctx context.Context) Recorder {
	if ctx == nil {
		return nil
	}
	r, _ := ctx.Value(recorderKey{}).(Recorder)
	return r
}

// Note records one action. Safe to call from anywhere: with no recorder
// installed it does nothing, so a tool exercised by a unit test or run
// outside a turn behaves exactly as it did before this feature existed.
//
// It returns the record so a caller that wants to name it ("that was a17")
// can, and the zero Record when nothing was recorded.
func Note(ctx context.Context, a Action) Record {
	r := RecorderFrom(ctx)
	if r == nil {
		return Record{}
	}
	rec, err := r.Append(a)
	if err != nil {
		// Deliberately swallowed here rather than returned: the store logs
		// its own failures, and a tool that reported "I changed it but could
		// not write that down" to the model would invite it to describe a
		// successful change as a failure. The account degrades; the work does
		// not.
		return Record{}
	}
	return rec
}

// ---------------------------------------------------------------------------
// Capturing a file change
// ---------------------------------------------------------------------------

// Before is a file's state captured ahead of a mutation. The zero value is
// safe: Note on it records the action with no restore, which is what a caller
// running without a recorder wants.
type Before struct {
	recorder Recorder
	path     string
	existed  bool
	previous string
	tooBig   bool
	unread   string
}

// Snapshot captures the file a mutation is about to change, so the record can
// carry the bytes that would restore it. Returns a zero Before — safe to Note
// on — when no recorder is installed, which keeps the read off every code
// path that is not recording.
//
// Read here rather than derived later because "later" does not exist: the
// moment the write lands the previous bytes are gone, and no amount of
// re-serialising the parsed document would reproduce the comments, key order
// and spacing a byte-preserving rewriter deliberately preserved.
func Snapshot(ctx context.Context, path string) *Before {
	r := RecorderFrom(ctx)
	if r == nil {
		return &Before{}
	}
	b := &Before{recorder: r, path: path}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		b.existed = true
		if len(data) > MaxRestoreBytes {
			b.tooBig = true
			break
		}
		b.previous = string(data)
	case os.IsNotExist(err):
		// Not an error and not an absence of information: "the file was not
		// there" is a complete answer, and restoring to it means removing
		// whatever the action created.
	default:
		b.unread = err.Error()
	}
	return b
}

// Note records the action the snapshot bracketed, reading the file again to
// take the clobber guard's digest. a.Restore is filled in from the snapshot
// and must be left zero by the caller.
//
// Every way the capture could not promise a reversal becomes an irreversible
// record with a stated reason rather than a silent omission, because a row
// that merely lacks a restore stanza is indistinguishable from a row nobody
// thought about.
func (b *Before) Note(ctx context.Context, a Action) Record {
	if b == nil || b.recorder == nil {
		return Record{}
	}
	switch {
	case b.unread != "":
		a.Restore = Restore{Kind: KindNone,
			Because: "I couldn't read " + b.path + " before the change, so I have nothing to put back"}
	case b.tooBig:
		a.Restore = Restore{Kind: KindNone,
			Because: "the file was larger than the " + describeBytes(MaxRestoreBytes) +
				" I keep a copy of, so I didn't keep one"}
	default:
		a.Restore = Restore{Kind: KindFile, File: &FileRestore{
			Path:        b.path,
			Existed:     b.existed,
			Previous:    b.previous,
			AfterDigest: digestFile(b.path),
		}}
	}
	if a.Target == "" {
		a.Target = b.path
	}
	return Note(ctx, a)
}

// DigestOf is digestFile for a caller outside this package: a tool that
// created a file and needs the guard for a record it is building by hand.
// An unreadable file digests to "", which the reverser reads as "I cannot
// tell" and refuses on.
func DigestOf(path string) string { return digestFile(path) }

// digestFile hashes a file for the clobber guard. An unreadable or missing
// file digests to the empty string, which the reverser treats as "I cannot
// tell" — the refusing direction, and the only safe one.
func digestFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return digestBytes(data)
}

// digestBytes is the guard's hash of some bytes.
func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// describeBytes words a byte cap for a sentence a person hears. Only ever
// called on the constants above, so the two cases it handles are the two that
// exist.
func describeBytes(n int) string {
	if n >= 1024 {
		return itoa(n/1024) + " KB"
	}
	return itoa(n) + " bytes"
}

// itoa avoids pulling strconv into a file that needs one integer rendered.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
