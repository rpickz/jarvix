package conversations

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/storefault"
)

// The conversation archive's registration with the shared fault-injection
// suite (issue #173).
//
// The archive is the one store here that is a directory rather than a
// document, and the two places that shows are declared rather than worked
// around. It sets nothing aside when it finds damage — a damaged
// conversation is one record among many and stays exactly where the listing
// can report it, which is more visible than a file moved out of the archive
// — and its records are whole conversations, so one of the suite's records
// is one conversation with one exchange in it.
//
// The archive's own hazards — a torn line an append could bury, a library of
// thousands of turns, the caps — are in archive_test.go, because they are
// the archive's and not every store's.

func TestConversationArchiveKeepsItsPromisesUnderFault(t *testing.T) {
	storefault.Run(t, storefault.Subject{
		Name: "conversations",
		Open: openFaultArchive,
		// Nothing is moved aside, and that is the considered answer rather
		// than a missing feature: List reports an unparseable record beside
		// the readable rest, so the user is told it exists and where it is.
		// Moving it out of the archive directory would hide it, and
		// overwriting it — which is what no store may do — would destroy a
		// transcript that may be the only copy.
		MovedAsideSuffix: "",
	})
}

// faultClock advances a second per conversation. That is not decoration: a
// conversation's id is a UTC timestamp plus a random suffix, so a frozen
// clock would lean the whole never-reused-ids promise on two bytes of
// randomness and give the suite a one-in-five-hundred chance of colliding
// with itself. Moving the clock exercises the mechanism the store actually
// relies on.
type faultClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *faultClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Second)
	return c.t
}

func openFaultArchive(t *testing.T, dir string, faults *storefault.Faults) storefault.Store {
	t.Helper()
	clock := &faultClock{t: time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC)}
	return newFaultArchive(dir, faults, clock)
}

func newFaultArchive(dir string, faults *storefault.Faults, clock *faultClock) *faultArchive {
	store := &FileStore{Dir: dir, Now: clock.now}
	store.write = func(w diskWrite) error {
		if err := faults.Before(w.path); err != nil {
			return err
		}
		return commitWrite(w)
	}
	return &faultArchive{store: store, dir: dir, faults: faults, clock: clock}
}

type faultArchive struct {
	store  *FileStore
	dir    string
	faults *storefault.Faults
	clock  *faultClock
}

// Add archives one exchange as its own conversation, which is what a record
// means for a store whose unit is a conversation.
func (a *faultArchive) Add(content string) (string, error) {
	return a.store.Append("", []Turn{
		{Role: "user", Text: content},
		{Role: "assistant", Text: "an answer to " + content},
	})
}

func (a *faultArchive) Forget(id string) error { return a.store.Delete(id) }

// Records reads through both of the archive's readers: the listing for what
// exists, and a full read for what was said. A conversation the listing
// reported unreadable is not a record — it is disclosure, and Disclosure is
// where it belongs.
func (a *faultArchive) Records() []storefault.Record {
	metas, _, err := a.store.List()
	if err != nil {
		return nil
	}
	out := make([]storefault.Record, 0, len(metas))
	for _, m := range metas {
		conv, err := a.store.Read(m.ID)
		if err != nil || len(conv.Turns) == 0 {
			continue
		}
		out = append(out, storefault.Record{
			ID: m.ID, Content: conv.Turns[0].Text, Detail: conv.Turns[0].Role})
	}
	return out
}

func (a *faultArchive) Reload(*testing.T) storefault.Store {
	// The clock comes along: a restarted daemon does not go back in time,
	// and ids are minted from it.
	return newFaultArchive(a.dir, a.faults, a.clock)
}

// HandEdit is the archive edited with a text editor: one conversation's
// transcript rewritten, another written from nothing, and the record that
// was there removed by deleting its two files. All three are things a person
// can do to a directory of JSONL, and the store reads from disk on every
// call, so all three must land on the next one.
func (a *faultArchive) HandEdit(t *testing.T) []storefault.Record {
	t.Helper()
	entries, err := os.ReadDir(a.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if err := os.Remove(filepath.Join(a.dir, entry.Name())); err != nil {
			t.Fatal(err)
		}
	}
	write := func(id, text, lastActive string) {
		transcript := fmt.Sprintf("{\"schema\":1,\"id\":%q}\n{\"role\":\"user\",\"text\":%q,"+
			"\"ts\":\"2026-08-27T08:00:00Z\"}\n", id, text)
		meta := fmt.Sprintf("{\"schema\":1,\"id\":%q,\"started\":\"2026-08-27T08:00:00Z\","+
			"\"last_active\":%q,\"turns\":1,\"preview\":%q}", id, lastActive, text)
		if err := os.WriteFile(filepath.Join(a.dir, id+".jsonl"), []byte(transcript), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(a.dir, id+".json"), []byte(meta), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("20260827-080000-aaaa", "what did we decide about the deploy?", "2026-08-27T08:30:00Z")
	write("20260827-080000-bbbb", "and when is the standup?", "2026-08-27T08:10:00Z")
	// Newest last-active first, which is the listing's order.
	return []storefault.Record{
		{ID: "20260827-080000-aaaa", Content: "what did we decide about the deploy?", Detail: "user"},
		{ID: "20260827-080000-bbbb", Content: "and when is the standup?", Detail: "user"},
	}
}

// Damage truncates a conversation's metadata — the document the listing
// reads — as a torn write or an editor crash leaves it.
func (a *faultArchive) Damage(t *testing.T) (string, []byte) {
	t.Helper()
	metas, _, err := a.store.List()
	if err != nil || len(metas) == 0 {
		t.Fatalf("nothing to damage: %v", err)
	}
	path := a.store.metaPath(metas[0].ID)
	raw := []byte(`{"schema":1,"id":"` + metas[0].ID + `","star`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, raw
}

// Disclosure is the listing's unreadable report. The archive has no logger
// and wants none: a damaged record is not an event that happened once, it is
// a standing fact about the library, so it is reported every time the
// library is listed rather than warned about once and forgotten.
func (a *faultArchive) Disclosure() []string {
	_, unreadable, err := a.store.List()
	if err != nil {
		return []string{err.Error()}
	}
	out := make([]string, 0, len(unreadable))
	for _, u := range unreadable {
		out = append(out, u.ID+": "+u.Err)
	}
	return out
}
