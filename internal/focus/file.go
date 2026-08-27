package focus

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

// This file is the disk half of the thread store: one TOML document under the
// XDG state dir, written atomically with the fsync-and-rename discipline of
// conversation history (ADR 0011), private to the user (0600 in a 0700
// directory) — the memory book's storage contract (ADR 0025), applied to
// threads.
//
// TOML rather than JSONL for the memory book's reason: the user owns this
// file. Renaming a thread, deleting a stale parked thought, or fixing a
// mis-transcribed name with a text editor is a first-class operation, and the
// store is small, curated, and rewritten whole on every change — exactly the
// shape TOML serves and append-only JSONL does not.

// document is the on-disk shape.
type document struct {
	Version int `toml:"version"`
	// NextThreadID and NextParkedID are id high-water marks, persisted so an
	// id is never reused even across ends and restarts: a conversation that
	// once named a thread or a parked thought must never come to describe a
	// different one.
	NextThreadID int `toml:"next_thread_id"`
	NextParkedID int `toml:"next_parked_id"`
	// Active is the id of the active thread, "" when none is.
	Active string `toml:"active,omitempty"`
	// Session is the live timeboxed focus session, absent when none runs.
	Session *sessionRecord `toml:"session,omitempty"`
	Threads []threadRecord `toml:"thread"`
}

type threadRecord struct {
	ID      string    `toml:"id"`
	Name    string    `toml:"name"`
	Created time.Time `toml:"created"`
	// LastSwitched is when the thread last became active; omitted for a
	// thread never switched into — that absence is what makes the "fresh
	// thread" recap honest rather than derived from a fabricated zero.
	LastSwitched time.Time `toml:"last_switched,omitempty"`
	// LastActivity is the most recent touch of any kind: a switch, a parked
	// thought, a timebox, a reminder change.
	LastActivity time.Time `toml:"last_activity"`
	// RemindEveryMin is the check-in interval in minutes, 0 for none.
	RemindEveryMin int `toml:"remind_every_min,omitzero"`
	// Recap is the AI-session recap trigger (#124): "always", "never", or
	// absent for the default (only a terminal anchor is read).
	Recap   string         `toml:"recap,omitempty"`
	Anchors []anchorRecord `toml:"anchor,omitempty"`
	Parked  []parkedRecord `toml:"parked,omitempty"`
}

type anchorRecord struct {
	Address  string `toml:"address"`
	StableID string `toml:"stable_id,omitempty"`
	App      string `toml:"app"`
	Title    string `toml:"title,omitempty"`
}

type parkedRecord struct {
	ID   string    `toml:"id"`
	Text string    `toml:"text"`
	At   time.Time `toml:"at"`
}

type sessionRecord struct {
	Thread  string    `toml:"thread"`
	Started time.Time `toml:"started"`
	Minutes int       `toml:"minutes"`
	// MidpointDue latches when the midpoint firing has been dispatched;
	// MidpointDone when the midpoint line has actually been spoken. Two
	// flags, because the dispatch and the speech are separated by the
	// session path and either half may not happen (config off, speech busy).
	MidpointDue  bool `toml:"midpoint_due,omitempty"`
	MidpointDone bool `toml:"midpoint_done,omitempty"`
	// Closing latches when the timebox has run out: the close prompt is owed
	// (or has been spoken) and the session no longer counts down. The record
	// stays until the user answers — continue, break, end — or the answer
	// window expires.
	Closing bool `toml:"closing,omitempty"`
	// ClosingAt is when Closing latched, for the answer-window expiry.
	ClosingAt time.Time `toml:"closing_at,omitempty"`
}

// documentVersion is bumped when the shape changes incompatibly; an
// unrecognised version is treated like corruption (warn, serve empty, never
// overwrite) rather than guessed at.
const documentVersion = 1

// header is written at the top of every save. It is the file's own
// documentation — the format must be discoverable by the person the file
// belongs to, without them ever reading Jarvix's source.
const header = `# Jarvix's focus threads — the named pieces of work behind "new thread ...",
# "switch to the ... thread", and "later: ...".
#
# Edit freely; Jarvix picks up changes without a restart. Each [[thread]]
# needs a name; ids and timestamps are filled in when missing. [[thread.parked]]
# entries are the thoughts parked into that thread, [[thread.anchor]] entries
# the windows it is anchored to (at most two), and remind_every_min is the
# check-in interval in minutes (0 or absent for none). recap = "always" or
# "never" overrides when Jarvix reads an anchored AI session's window for a
# spoken summary (absent: only when the anchored window is a terminal).
# "active" names the thread you are on; [session] is a live timeboxed focus
# session.
# Jarvix rewrites this file whenever a thread changes; comments not preserved.

`

// readStore loads the thread file. A parse failure, unknown version, or
// unknown key is an error the Service downgrades to a warning plus an empty
// store; content never travels inside the error.
func readStore(path string) (persisted, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return persisted{}, fmt.Errorf("read focus store: %w", err)
	}
	var doc document
	md, err := toml.Decode(string(data), &doc)
	if err != nil {
		return persisted{}, fmt.Errorf("parse focus store: %w", err)
	}
	// An unknown key is most likely a hand-edit typo ("remnd_every_min"),
	// and silently dropping the value it holds would look exactly like
	// Jarvix forgetting. Refusing loudly gets the documented degradation
	// instead: a warning, an empty store, and a file never overwritten.
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		return persisted{}, fmt.Errorf("focus store has unknown key %q", undecoded[0].String())
	}
	if doc.Version != documentVersion {
		return persisted{}, fmt.Errorf("focus store version %d is not supported", doc.Version)
	}
	p := persisted{
		nextThread: doc.NextThreadID,
		nextParked: doc.NextParkedID,
		active:     doc.Active,
	}
	for _, r := range doc.Threads {
		th := Thread{
			ID: r.ID, Name: r.Name,
			Created: r.Created, LastSwitched: r.LastSwitched, LastActivity: r.LastActivity,
			RemindEveryMin: r.RemindEveryMin, Recap: r.Recap,
		}
		for _, a := range r.Anchors {
			th.Anchors = append(th.Anchors, Anchor(a))
		}
		for _, pk := range r.Parked {
			th.Parked = append(th.Parked, Parked(pk))
		}
		p.threads = append(p.threads, th)
	}
	if doc.Session != nil {
		p.session = Session{
			ThreadID: doc.Session.Thread, Started: doc.Session.Started,
			Minutes:      doc.Session.Minutes,
			MidpointDue:  doc.Session.MidpointDue,
			MidpointDone: doc.Session.MidpointDone,
			Closing:      doc.Session.Closing,
			ClosingAt:    doc.Session.ClosingAt,
		}
	}
	return p, nil
}

// persisted is everything one read or write of the store carries.
type persisted struct {
	threads    []Thread
	active     string
	session    Session
	nextThread int
	nextParked int
}

// writeStore persists the store atomically: temp file in the same directory,
// fsync, rename, fsync the directory — a crash mid-write leaves the old store
// or the new one, never a torn file, and the rename is durable rather than
// merely atomic (the ADR 0011 discipline, applied verbatim).
func writeStore(path string, p persisted) error {
	doc := document{
		Version:      documentVersion,
		NextThreadID: p.nextThread,
		NextParkedID: p.nextParked,
		Active:       p.active,
		Threads:      make([]threadRecord, 0, len(p.threads)),
	}
	if p.session.ThreadID != "" {
		s := sessionRecord{
			Thread: p.session.ThreadID, Started: p.session.Started.UTC(),
			Minutes:      p.session.Minutes,
			MidpointDue:  p.session.MidpointDue,
			MidpointDone: p.session.MidpointDone,
			Closing:      p.session.Closing,
		}
		if !p.session.ClosingAt.IsZero() {
			s.ClosingAt = p.session.ClosingAt.UTC()
		}
		doc.Session = &s
	}
	for _, th := range p.threads {
		r := threadRecord{
			ID: th.ID, Name: th.Name,
			Created: th.Created.UTC(), LastActivity: th.LastActivity.UTC(),
			RemindEveryMin: th.RemindEveryMin, Recap: th.Recap,
		}
		if !th.LastSwitched.IsZero() {
			r.LastSwitched = th.LastSwitched.UTC()
		}
		for _, a := range th.Anchors {
			r.Anchors = append(r.Anchors, anchorRecord(a))
		}
		for _, pk := range th.Parked {
			r.Parked = append(r.Parked, parkedRecord{ID: pk.ID, Text: pk.Text, At: pk.At.UTC()})
		}
		doc.Threads = append(doc.Threads, r)
	}
	var buf bytes.Buffer
	buf.WriteString(header)
	if err := toml.NewEncoder(&buf).Encode(doc); err != nil {
		return fmt.Errorf("encode focus store: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	// MkdirAll applies its mode only to directories it creates; an existing
	// state dir keeps whatever modes it had. The content is the user's work
	// in progress, so the 0700 requirement is asserted on every write, not
	// hoped for.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secure state dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".focus-*.tmp")
	if err != nil {
		return fmt.Errorf("write focus store: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after a successful rename
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write focus store: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write focus store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write focus store: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write focus store: %w", err)
	}
	// CreateTemp asks for 0600 but the umask can clear bits, and the rename
	// carries whatever the temp file ended up with. Reassert rather than hope.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secure focus store: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("write focus store: %w", err)
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
