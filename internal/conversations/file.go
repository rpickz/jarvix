package conversations

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rpickz/jarvix/internal/statehold"
)

// FileStore is a Store backed by one directory: two files per conversation,
// plus one pointer file.
//
//	<id>.jsonl   the transcript — one JSON header line, then one JSON object
//	             per turn ({"role","text","ts"}), appended as turns complete.
//	<id>.json    the metadata — id, timestamps, turn count, preview —
//	             rewritten atomically on every append, so listing a library
//	             of any size reads only these small documents and never a
//	             transcript.
//	active       the id of the conversation the live head belongs to.
//
// JSONL rather than the memory store's TOML (ADR 0025) on purpose: memory is
// a document the user is invited to open and edit, while an archive is an
// append-mostly machine record — per-turn appends are one written line, a
// crash can tear at most the line being written, and the search ticket can
// index records line-by-line without parsing whole documents. The atomic
// temp+rename discipline for the metadata file follows internal/history
// (ADR 0011) exactly.
//
// Only one daemon writes at a time; the CLI writes to the same directory only
// when the daemon is down (mirroring how `jarvix new` treats history.json).
type FileStore struct {
	// Dir is the archive directory, conventionally
	// $XDG_STATE_HOME/jarvix/conversations.
	Dir string
	// Gate is the backup write barrier (ADR 0045); nil — the CLI, tests —
	// means writes are never held. Only the daemon threads one through. It
	// matters most here: a transcript append plus its metadata rewrite is
	// the one multi-file mutation in the state root, held as a unit.
	Gate *statehold.Gate
	// Now is the clock, injectable so tests control every timestamp. Nil
	// means time.Now. It only backstops turns that arrive without their own
	// timestamp; the engine stamps turns as they complete.
	Now func() time.Time
	// NewID mints conversation ids, injectable so the golden-file test can
	// pin one. Nil uses a UTC timestamp plus random suffix — sortable in a
	// directory listing, unique without coordination.
	NewID func(time.Time) string

	mu sync.Mutex
}

// header is the first line of every transcript file, so a transcript found on
// its own — copied somewhere, indexed by search — still says what it is.
type header struct {
	Schema int    `json:"schema"`
	ID     string `json:"id"`
}

// activeFile is the pointer file's name inside Dir. No extension, so the
// listing scan never mistakes it for a conversation.
const activeFile = "active"

func (s *FileStore) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *FileStore) newID(t time.Time) string {
	if s.NewID != nil {
		return s.NewID(t)
	}
	return defaultID(t)
}

// defaultID mints "20060102-150405-xxxx": the timestamp makes ids humane to
// type and time-sortable at a glance, the random suffix makes two
// conversations in one second (or a deleted id's second coming) collide with
// probability a user will never meet.
func defaultID(t time.Time) string {
	var b [2]byte
	_, _ = rand.Read(b[:]) // crypto/rand never fails on Linux
	return t.UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}

// metaPath and turnsPath place a conversation's two files. Ids are validated
// before either is called.
func (s *FileStore) metaPath(id string) string  { return filepath.Join(s.Dir, id+".json") }
func (s *FileStore) turnsPath(id string) string { return filepath.Join(s.Dir, id+".jsonl") }

// ensureDir creates the archive directory and asserts its privacy. Chmod
// runs even when the directory already existed, for the same reason history
// does it (ADR 0011): MkdirAll's mode only applies to directories it makes,
// and the transcript of someone's home must not inherit a permissive umask.
func (s *FileStore) ensureDir() error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("create conversations dir: %w", err)
	}
	if err := os.Chmod(s.Dir, 0o700); err != nil {
		return fmt.Errorf("secure conversations dir: %w", err)
	}
	return nil
}

// Append implements Store. The transcript grows by exactly the lines being
// added — a crash can tear at most the final line, which Read tolerates — and
// the metadata is rewritten atomically afterwards, so a listing never sees a
// torn document.
func (s *FileStore) Append(id string, turns []Turn) (string, error) {
	if len(turns) == 0 {
		return id, nil
	}
	defer s.Gate.Enter()()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureDir(); err != nil {
		return id, err
	}

	// Resolve which conversation the turns land in. An id whose metadata has
	// gone — deleted underneath a running daemon — gets a fresh conversation
	// rather than a resurrection: deletion is deletion, and nothing may
	// quietly rebuild a record the user removed.
	meta := Meta{Schema: SchemaVersion}
	create := id == ""
	if !create {
		if err := validID(id); err != nil {
			return id, err
		}
		loaded, err := readMetaFile(s.metaPath(id))
		switch {
		case err == nil:
			meta = loaded
		case errors.Is(err, os.ErrNotExist):
			create = true
		default:
			// The metadata is unreadable. Leave the damaged record where the
			// user can see it in the listing and start a fresh conversation —
			// overwriting it here would destroy evidence of what went wrong.
			create = true
		}
	}
	last := turns[len(turns)-1].Time
	if last.IsZero() {
		last = s.now()
	}
	if create {
		id = s.newID(last)
		meta = Meta{Schema: SchemaVersion, ID: id, Started: firstTime(turns, last)}
	}

	if err := s.appendTurnsLocked(id, turns); err != nil {
		return id, err
	}
	meta.LastActive = last
	meta.TurnCount += len(turns)
	if meta.Preview == "" {
		meta.Preview = preview(turns)
	}
	if err := s.writeMetaLocked(meta); err != nil {
		return id, err
	}
	if err := s.writeActiveLocked(id); err != nil {
		return id, err
	}
	return id, nil
}

// firstTime returns the earliest usable timestamp for a new conversation.
func firstTime(turns []Turn, fallback time.Time) time.Time {
	if !turns[0].Time.IsZero() {
		return turns[0].Time
	}
	return fallback
}

// appendTurnsLocked writes turn lines to the transcript, creating it with its
// header line when absent. The write is flushed to disk before the metadata
// is updated, so the count in a listing never promises turns the transcript
// does not durably hold.
func (s *FileStore) appendTurnsLocked(id string, turns []Turn) error {
	path := s.turnsPath(id)
	_, statErr := os.Stat(path)
	fresh := errors.Is(statErr, os.ErrNotExist)

	var buf strings.Builder
	if fresh {
		line, err := json.Marshal(header{Schema: SchemaVersion, ID: id})
		if err != nil {
			return fmt.Errorf("encode conversation header: %w", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	for _, t := range turns {
		if t.Time.IsZero() {
			t.Time = s.now()
		}
		line, err := json.Marshal(t)
		if err != nil {
			return fmt.Errorf("encode conversation turn: %w", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("write conversation: %w", err)
	}
	if _, err := f.WriteString(buf.String()); err != nil {
		_ = f.Close()
		return fmt.Errorf("write conversation: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("write conversation: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write conversation: %w", err)
	}
	// OpenFile asks for 0600 but the umask can clear bits; reassert rather
	// than hope, exactly as history does for its file.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secure conversation: %w", err)
	}
	if fresh {
		// A new directory entry is durable only once the directory itself is
		// synced (the fsync+rename lesson from ADR 0011's review).
		if err := syncDir(s.Dir); err != nil {
			return fmt.Errorf("write conversation: %w", err)
		}
	}
	return nil
}

// writeMetaLocked rewrites a conversation's metadata atomically.
func (s *FileStore) writeMetaLocked(meta Meta) error {
	data, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("encode conversation metadata: %w", err)
	}
	return writeFileAtomic(s.Dir, s.metaPath(meta.ID), data)
}

// writeActiveLocked points the live head at id, atomically. An unchanged
// pointer is rewritten anyway: the write is one tiny rename, and skipping it
// would need a read whose failure modes buy nothing.
func (s *FileStore) writeActiveLocked(id string) error {
	return writeFileAtomic(s.Dir, filepath.Join(s.Dir, activeFile), []byte(id+"\n"))
}

// Active implements Store. Every failure degrades to "": a fresh conversation
// is always a safe answer, and the pointer is a convenience, not a record.
func (s *FileStore) Active() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(filepath.Join(s.Dir, activeFile))
	if err != nil {
		return ""
	}
	id := strings.TrimSpace(string(data))
	if validID(id) != nil {
		return ""
	}
	if _, err := os.Stat(s.metaPath(id)); err != nil {
		return "" // the conversation it named is gone; the pointer is stale
	}
	return id
}

// SetActive implements Store.
func (s *FileStore) SetActive(id string) error {
	if err := validID(id); err != nil {
		return err
	}
	defer s.Gate.Enter()()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureDir(); err != nil {
		return err
	}
	return s.writeActiveLocked(id)
}

// List implements Store. It reads only the metadata files — the transcript
// files are deliberately never opened, which is what keeps a library of
// hundreds of conversations listable in the time one small read takes each.
func (s *FileStore) List() ([]Meta, []Unreadable, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return []Meta{}, []Unreadable{}, nil // no archive yet: an empty library
	}
	if err != nil {
		return nil, nil, fmt.Errorf("list conversations: %w", err)
	}

	metas := []Meta{}
	unreadable := []Unreadable{}
	seen := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		seen[id] = true
		meta, err := readMetaFile(filepath.Join(s.Dir, name))
		if err != nil {
			// The error is reported without file contents: the message names
			// what is wrong, never what was said.
			unreadable = append(unreadable, Unreadable{ID: id, Err: err.Error()})
			continue
		}
		metas = append(metas, meta)
	}
	// A transcript whose metadata has gone is still a conversation the user
	// owns; surfacing it beats a file invisibly haunting the state directory.
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(name, ".jsonl")
		if !seen[id] {
			unreadable = append(unreadable, Unreadable{ID: id, Err: "conversation metadata is missing"})
		}
	}

	sort.Slice(metas, func(i, j int) bool {
		if !metas[i].LastActive.Equal(metas[j].LastActive) {
			return metas[i].LastActive.After(metas[j].LastActive)
		}
		if !metas[i].Started.Equal(metas[j].Started) {
			return metas[i].Started.After(metas[j].Started)
		}
		return metas[i].ID > metas[j].ID
	})
	sort.Slice(unreadable, func(i, j int) bool { return unreadable[i].ID < unreadable[j].ID })
	return metas, unreadable, nil
}

// readMetaFile parses one metadata document, rejecting versions this build
// does not understand rather than guessing at them (the history precedent).
func readMetaFile(path string) (Meta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Meta{}, err
	}
	var meta Meta
	if err := json.Unmarshal(data, &meta); err != nil {
		return Meta{}, fmt.Errorf("parse conversation metadata: %w", err)
	}
	if meta.Schema != SchemaVersion {
		return Meta{}, fmt.Errorf("conversation schema version %d is not supported", meta.Schema)
	}
	return meta, nil
}

// Read implements Store. The returned TurnCount is what actually parsed, not
// what the metadata promised — on a full read the transcript is the truth.
//
// A final line that fails to parse is dropped, not fatal: appends land whole
// lines, so only the line being written when power died can be torn, and
// costing the user their entire conversation over its own last half-written
// turn would be the archive failing at its one job. A bad line anywhere
// *before* the end cannot be torn-write damage, so it is corruption and is
// reported as such.
func (s *FileStore) Read(id string) (Conversation, error) {
	if err := validID(id); err != nil {
		return Conversation{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	meta, err := readMetaFile(s.metaPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return Conversation{}, fmt.Errorf("no conversation has id %q", id)
	}
	if err != nil {
		return Conversation{}, err
	}
	data, err := os.ReadFile(s.turnsPath(id))
	if err != nil {
		return Conversation{}, fmt.Errorf("read conversation %q: %w", id, err)
	}
	lines := strings.Split(string(data), "\n")
	// A complete file ends in "\n"; drop the empty element that split leaves.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return Conversation{}, fmt.Errorf("conversation %q has an empty transcript", id)
	}
	var h header
	if err := json.Unmarshal([]byte(lines[0]), &h); err != nil {
		return Conversation{}, fmt.Errorf("parse conversation %q: bad header", id)
	}
	if h.Schema != SchemaVersion {
		return Conversation{}, fmt.Errorf("conversation schema version %d is not supported", h.Schema)
	}
	turns := make([]Turn, 0, len(lines)-1)
	for i, line := range lines[1:] {
		var t Turn
		if err := json.Unmarshal([]byte(line), &t); err != nil {
			if i == len(lines)-2 {
				break // the torn final line of an interrupted append
			}
			return Conversation{}, fmt.Errorf("parse conversation %q: bad turn at line %d", id, i+2)
		}
		turns = append(turns, t)
	}
	meta.TurnCount = len(turns)
	return Conversation{Meta: meta, Turns: turns}, nil
}

// Delete implements Store. Both files go, the active pointer is cleared if it
// named this conversation, and the directory is synced so the deletion is as
// durable as the writes were — removed means removed, even across a crash.
func (s *FileStore) Delete(id string) error {
	if err := validID(id); err != nil {
		return err
	}
	defer s.Gate.Enter()()
	s.mu.Lock()
	defer s.mu.Unlock()
	metaErr := os.Remove(s.metaPath(id))
	turnsErr := os.Remove(s.turnsPath(id))
	if errors.Is(metaErr, os.ErrNotExist) && errors.Is(turnsErr, os.ErrNotExist) {
		return fmt.Errorf("no conversation has id %q", id)
	}
	for _, err := range []error{metaErr, turnsErr} {
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("delete conversation %q: %w", id, err)
		}
	}
	s.clearActiveLocked(id)
	if err := syncDir(s.Dir); err != nil {
		return fmt.Errorf("delete conversation %q: %w", id, err)
	}
	return nil
}

// clearActiveLocked removes the active pointer when it names id, so a deleted
// conversation cannot be pointed at. Best-effort: a pointer that survives is
// caught by Active's existence check anyway.
func (s *FileStore) clearActiveLocked(id string) {
	path := filepath.Join(s.Dir, activeFile)
	data, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(data)) != id {
		return
	}
	_ = os.Remove(path)
}

// DeleteAll implements Store. Every conversation goes — readable or not: the
// unreadable ones are exactly the records a user wiping the archive would
// least want left behind.
func (s *FileStore) DeleteAll() (int, error) {
	defer s.Gate.Enter()()
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("delete conversations: %w", err)
	}
	ids := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			continue
		}
		switch {
		case strings.HasSuffix(name, ".json"):
			ids[strings.TrimSuffix(name, ".json")] = true
		case strings.HasSuffix(name, ".jsonl"):
			ids[strings.TrimSuffix(name, ".jsonl")] = true
		case name == activeFile:
		default:
			continue
		}
		if err := os.Remove(filepath.Join(s.Dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return 0, fmt.Errorf("delete conversations: %w", err)
		}
	}
	if len(ids) > 0 {
		if err := syncDir(s.Dir); err != nil {
			return 0, fmt.Errorf("delete conversations: %w", err)
		}
	}
	return len(ids), nil
}

// writeFileAtomic writes data to path via a temp file and rename in dir,
// asserting 0600 and syncing the directory — the exact discipline
// internal/history established (ADR 0011), duplicated here rather than
// exported from it so neither package's crash-safety story depends on the
// other's internals.
func writeFileAtomic(dir, path string, data []byte) error {
	tmp, err := os.CreateTemp(dir, ".conversation-*.tmp")
	if err != nil {
		return fmt.Errorf("write conversation metadata: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write conversation metadata: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write conversation metadata: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write conversation metadata: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write conversation metadata: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secure conversation metadata: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("write conversation metadata: %w", err)
	}
	return nil
}

// syncDir fsyncs a directory so entries created, renamed, or removed inside
// it survive a crash — the same portable spelling internal/history uses.
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
