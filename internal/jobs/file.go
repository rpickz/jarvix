package jobs

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

// This file is the disk half of the job store: one TOML document under the XDG
// state dir, written atomically with the fsync-and-rename discipline every
// durable store here keeps (ADR 0011), private to the user (0600 in a 0700
// directory).
//
// It is a file rather than memory for the reason that makes jobs different
// from everything before them: a parked job is state, not a paused goroutine,
// and "the daemon restarted" must cost a job nothing but the step that was in
// flight. Restart survival is an acceptance criterion, and a store is the only
// honest way to meet it.
//
// TOML, and hand-editable, for the reminder store's reason plus one of its
// own. A job is work being done in the user's name while they are not
// watching; being able to open the file and read the ledger — what was
// attempted, what happened, what it is waiting for — with no daemon running is
// the difference between delegation and hoping. Deleting a job's stanza is a
// legitimate way of saying "forget that", and setting `state = "ready"` by hand
// on a parked job is a legitimate way of saying "carry on", which the
// supervisor's idle sweep picks up without a restart (ADR 0049).

// document is the on-disk shape.
type document struct {
	Version int `toml:"version"`
	// NextID is the id high-water mark, persisted so an id is never reused —
	// not across a restart, and not after the job holding it was deleted by
	// hand. A report that once named j4 must never come to name a different
	// piece of work.
	NextID int         `toml:"next_id"`
	Jobs   []jobRecord `toml:"job"`
}

type jobRecord struct {
	ID      string `toml:"id"`
	Name    string `toml:"name"`
	Goal    string `toml:"goal"`
	State   string `toml:"state"`
	Steps   int    `toml:"steps,omitzero"`
	Closing string `toml:"closing,omitempty"`

	Started time.Time `toml:"started"`
	// Ended is a pointer so a live job writes no key at all: BurntSushi
	// renders a zero time.Time as 0001-01-01, which would put an end date on
	// every running job and read as one that had finished.
	Ended *time.Time `toml:"ended,omitempty"`

	Scope    scopeRecord     `toml:"scope"`
	Question *questionRecord `toml:"question,omitempty"`
	InFlight *stepRec        `toml:"in_flight,omitempty"`
	Ledger   []entryRecord   `toml:"step,omitempty"`
}

type scopeRecord struct {
	Tools []string `toml:"tools"`
	Roots []string `toml:"roots,omitempty"`
	Apps  []string `toml:"apps,omitempty"`
}

type questionRecord struct {
	Why  string    `toml:"why"`
	Ask  string    `toml:"ask"`
	At   time.Time `toml:"at"`
	Step stepRec   `toml:"step"`
}

type stepRec struct {
	Intent   string `toml:"intent,omitempty"`
	Tool     string `toml:"tool,omitempty"`
	Args     string `toml:"args,omitempty"`
	Finished bool   `toml:"finished,omitzero"`
	Question string `toml:"question,omitempty"`
}

type entryRecord struct {
	At       time.Time `toml:"at"`
	Intent   string    `toml:"intent,omitempty"`
	Tool     string    `toml:"tool,omitempty"`
	Said     string    `toml:"said,omitempty"`
	Verified bool      `toml:"verified"`
	Failed   bool      `toml:"failed,omitzero"`
	Undo     string    `toml:"undo,omitempty"`
}

// documentVersion is bumped when the shape changes incompatibly; an
// unrecognised version is treated like corruption (warn, serve an empty store,
// never overwrite) rather than guessed at.
const documentVersion = 1

// header is written at the top of every save — the file's own documentation,
// discoverable by the person the work is being done for without reading source.
const header = `# The work Jarvix is doing for you that outlives a conversation.
#
# One [[job]] per direction you gave. goal is your own words, kept verbatim.
# [job.scope] is the boundary the daemon enforces before every action: tools is
# the only tools it may use, roots the only directories it may touch, apps the
# only windows it may act on. Widening a scope here widens what that job may do,
# so treat it as you would treat handing someone a key.
#
# state is one of: ready (work left, nobody on it), running (a runner has it
# now), parked (waiting for you), done, stopped, failed. [job.question] is what
# a parked job is waiting for; why = "approval" or "decision" can be answered,
# and the job then resumes at [job.question.step] rather than starting again.
#
# [job.in_flight] is a step Jarvix had started and not yet finished. Finding one
# means the daemon went away mid-action: the step is written into the record as
# unverified, because whether it completed is genuinely not known.
#
# Each [[job.step]] is one thing that was attempted, in order. verified = false
# means Jarvix started that step and never saw how it ended — a stop mid-flight,
# or a daemon that went away between the action and its result. Those steps are
# reported as unknown, never as done.
#
# Edit freely; Jarvix picks up changes without a restart. Setting a parked job
# back to state = "ready" tells it to carry on. Deleting a [[job]] stanza
# forgets it; the id is not reissued. Jarvix rewrites this file whenever a job
# moves; comments are not preserved.

`

// persisted is everything one read or write of the store carries.
type persisted struct {
	jobs   []Job
	nextID int
}

// readStore loads the file. A parse failure, unknown version, or unknown key
// is an error the Store downgrades to a warning plus an empty store; content
// never travels inside the error.
func readStore(path string) (persisted, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return persisted{}, fmt.Errorf("read jobs: %w", err)
	}
	var doc document
	md, err := toml.Decode(string(data), &doc)
	if err != nil {
		return persisted{}, fmt.Errorf("parse jobs: %w", err)
	}
	// An unknown key is most likely a hand-edit typo ("gaol"), and silently
	// dropping the value it holds would look exactly like Jarvix forgetting
	// what it was asked to do. Refusing loudly gets the documented
	// degradation: a warning, an empty store, and a file never overwritten.
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return persisted{}, fmt.Errorf("jobs file has unknown key %q", undecoded[0].String())
	}
	if doc.Version != documentVersion {
		return persisted{}, fmt.Errorf("jobs file version %d is not supported", doc.Version)
	}
	p := persisted{nextID: doc.NextID}
	for _, r := range doc.Jobs {
		j := Job{
			ID: r.ID, Name: r.Name, Goal: r.Goal, State: State(r.State),
			Steps: r.Steps, Closing: r.Closing, Started: r.Started,
			Scope: Scope{
				Tools: append([]string(nil), r.Scope.Tools...),
				Roots: append([]string(nil), r.Scope.Roots...),
				Apps:  append([]string(nil), r.Scope.Apps...),
			},
		}
		if r.Ended != nil {
			j.Ended = *r.Ended
		}
		if f := r.InFlight; f != nil {
			j.InFlight = Step{Intent: f.Intent, Tool: f.Tool, Args: f.Args,
				Finished: f.Finished, Question: f.Question}
		}
		if q := r.Question; q != nil {
			j.Question = Question{Why: Why(q.Why), Ask: q.Ask, At: q.At, Step: Step{
				Intent: q.Step.Intent, Tool: q.Step.Tool, Args: q.Step.Args,
				Finished: q.Step.Finished, Question: q.Step.Question,
			}}
		}
		for _, e := range r.Ledger {
			// A conversion rather than a field-by-field copy: the on-disk row
			// and the in-memory entry are deliberately the same shape, and the
			// conversion is what makes adding a field to one without the other
			// a compile error rather than a silently dropped fact.
			j.Ledger = append(j.Ledger, Entry(e))
		}
		p.jobs = append(p.jobs, j)
	}
	return p, nil
}

// ValidateFile reports whether the jobs file at path would load: parseable
// TOML, no unknown keys, a supported schema version. `jarvix restore` (ADR
// 0045) proves a staged archive with it before swapping anything into place.
func ValidateFile(path string) error {
	_, err := readStore(path)
	return err
}

// writeStore persists the store atomically: temp file in the same directory,
// fsync, rename, fsync the directory — a crash mid-write leaves the old file or
// the new one, never a torn one, and the rename is durable rather than merely
// atomic (the ADR 0011 discipline, verbatim).
func writeStore(path string, p persisted) error {
	doc := document{
		Version: documentVersion,
		NextID:  p.nextID,
		Jobs:    make([]jobRecord, 0, len(p.jobs)),
	}
	for _, j := range p.jobs {
		out := jobRecord{
			ID: j.ID, Name: j.Name, Goal: j.Goal, State: string(j.State),
			Steps: j.Steps, Closing: j.Closing, Started: j.Started.UTC(),
			Scope: scopeRecord{
				Tools: append([]string(nil), j.Scope.Tools...),
				Roots: append([]string(nil), j.Scope.Roots...),
				Apps:  append([]string(nil), j.Scope.Apps...),
			},
		}
		if !j.Ended.IsZero() {
			at := j.Ended.UTC()
			out.Ended = &at
		}
		if j.InFlight.Tool != "" {
			out.InFlight = &stepRec{Intent: j.InFlight.Intent, Tool: j.InFlight.Tool,
				Args: j.InFlight.Args, Finished: j.InFlight.Finished,
				Question: j.InFlight.Question}
		}
		if j.Question.Why != "" {
			out.Question = &questionRecord{
				Why: string(j.Question.Why), Ask: j.Question.Ask, At: j.Question.At.UTC(),
				Step: stepRec{
					Intent: j.Question.Step.Intent, Tool: j.Question.Step.Tool,
					Args: j.Question.Step.Args, Finished: j.Question.Step.Finished,
					Question: j.Question.Step.Question,
				},
			}
		}
		for _, e := range j.Ledger {
			out.Ledger = append(out.Ledger, entryRecord{
				At: e.At.UTC(), Intent: e.Intent, Tool: e.Tool, Said: e.Said,
				Verified: e.Verified, Failed: e.Failed, Undo: e.Undo,
			})
		}
		doc.Jobs = append(doc.Jobs, out)
	}

	var buf bytes.Buffer
	buf.WriteString(header)
	if err := toml.NewEncoder(&buf).Encode(doc); err != nil {
		return fmt.Errorf("encode jobs: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	// MkdirAll applies its mode only to directories it creates, and this file
	// sits beside stores that are private, so 0700 is asserted on every write.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secure state dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".jobs-*.tmp")
	if err != nil {
		return fmt.Errorf("write jobs: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after a successful rename
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write jobs: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write jobs: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write jobs: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write jobs: %w", err)
	}
	// CreateTemp asks for 0600 but the umask can clear bits, and the rename
	// carries whatever the temp file ended up with. Reassert rather than hope:
	// this file holds a record of work done in the user's name.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secure jobs: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("write jobs: %w", err)
	}
	return nil
}

// syncDir fsyncs a directory so entries renamed into it survive a crash —
// atomic is not durable until the directory entry itself is on disk.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}
