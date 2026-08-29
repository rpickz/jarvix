package approvals

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/storefault"
)

// The approval ledger's registration with the shared fault-injection suite
// (issue #173). This is the store the ticket predicted would fail, and it
// did: it was the one durable store whose discipline was assumed rather than
// asserted, and four of the six promises were broken on the first run, by
// three separate mechanisms — a write committed to memory before the disk
// took it, an unreadable file overwritten rather than set aside, and a load
// that ran once per process so a hand edit was never seen. All three are
// fixed in approvals.go and pinned individually in approvals_test.go; this
// file is only the registration.
//
// What counts as a "record" here needs saying, because the ledger is the odd
// one out and the model is deliberate. Which patterns EXIST is decided by
// config.toml and nothing else — that is the whole design (see the package
// doc), and it is why List folds the ledger onto the configured list on
// every read. So the thing this file holds, and the thing the suite drives,
// is not membership: it is the HISTORY of a pattern the card agreed to.
// Records therefore reports the card-sourced rows, which are exactly the
// rows the ledger itself created and the only rows a lost or damaged file
// can cost the user.

func TestApprovalLedgerKeepsItsPromisesUnderFault(t *testing.T) {
	storefault.Run(t, storefault.Subject{
		Name:             "approvals",
		Open:             openFaultApprovals,
		MovedAsideSuffix: ".corrupt",
		NoIDsBecause: "a row's identity IS its pattern, and the pattern comes from config.toml — " +
			"the ledger mints nothing and could not reuse what it does not issue",
	})
}

func openFaultApprovals(t *testing.T, dir string, faults *storefault.Faults) storefault.Store {
	t.Helper()
	log, disclosure := storefault.Log()
	path := filepath.Join(dir, "approvals.toml")
	store := NewStore(path, log)
	store.write = func(path string, records map[string]*record) error {
		if err := faults.Before(path); err != nil {
			return err
		}
		return writeLedger(path, records)
	}
	return &faultApprovals{store: store, dir: dir, path: path, faults: faults, disclosure: disclosure}
}

// faultApprovals adapts the ledger. configured stands in for config.toml's
// shell_allow list, and it is read in the same critical section as the
// ledger because in the daemon the two are read together too — the
// configured list is taken under the daemon's own configuration lock and
// handed to List whole. Reading a stale list beside a fresh ledger would
// have reconcile delete rows that are not missing, which is an artefact of
// splitting them, not a property of either.
type faultApprovals struct {
	store      *Store
	dir        string
	path       string
	faults     *storefault.Faults
	disclosure func() []string

	mu         sync.Mutex
	configured []string
}

func faultApprovalsNow() time.Time {
	return time.Date(2026, 8, 28, 14, 2, 0, 0, time.UTC)
}

// Add is the card writing a pattern. The configured list gains it only once
// the ledger has taken it, so a refused write leaves neither half claiming
// the grant — and so the reconcile that every List performs has nothing new
// to write, which would otherwise touch the file the suite is watching.
//
// The ledger reports a failed write by logging and nothing else, on purpose:
// the count is a convenience for deciding what to revoke, and losing one
// must never cost the user the command they were running. So the warning is
// what comes back here, which is the suite's rule — the failure has to be
// surfaced honestly, not through any particular return value.
func (a *faultApprovals) Add(content string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	before := len(a.disclosure())
	a.store.Added(content, faultApprovalsNow())
	if said := a.disclosure(); len(said) > before {
		return "", errors.New(said[len(said)-1])
	}
	a.configured = append(a.configured, content)
	return content, nil
}

func (a *faultApprovals) Forget(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	kept := a.configured[:0]
	for _, p := range a.configured {
		if p != id {
			kept = append(kept, p)
		}
	}
	a.configured = kept
	a.store.Forget(id)
	return nil
}

func (a *faultApprovals) Records() []storefault.Record {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []storefault.Record{}
	for _, e := range a.store.List(a.configured) {
		if e.Source != SourceCard {
			continue // a configured pattern the ledger has no history for
		}
		out = append(out, storefault.Record{
			ID: e.Pattern, Content: e.Pattern, Detail: strconv.Itoa(e.Uses)})
	}
	return out
}

func (a *faultApprovals) Reload(t *testing.T) storefault.Store {
	t.Helper()
	a.mu.Lock()
	configured := append([]string(nil), a.configured...)
	a.mu.Unlock()
	next, ok := openFaultApprovals(t, a.dir, a.faults).(*faultApprovals)
	if !ok {
		t.Fatal("the approvals adapter changed shape")
	}
	// config.toml survives a restart; the ledger is what is being reloaded.
	next.configured = configured
	return next
}

// HandEdit is the ledger edited in place: two patterns with the use counts
// only this file holds. Membership is not what changes — both patterns are
// configured either way — so the counts are the whole proof that the edit
// was read rather than reconstructed.
func (a *faultApprovals) HandEdit(t *testing.T) []storefault.Record {
	t.Helper()
	doc := `version = 1

[[approval]]
pattern = "docker ps"
source = "card"
added = 2026-08-20T09:30:00Z
uses = 7
last_used = 2026-08-28T08:00:00Z

[[approval]]
pattern = "git status"
source = "card"
added = 2026-08-21T11:00:00Z
uses = 2
last_used = 2026-08-28T09:00:00Z
`
	if err := os.WriteFile(a.path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.configured = []string{"docker ps", "git status"}
	a.mu.Unlock()
	return []storefault.Record{
		{ID: "docker ps", Content: "docker ps", Detail: "7"},
		{ID: "git status", Content: "git status", Detail: "2"},
	}
}

func (a *faultApprovals) Damage(t *testing.T) (string, []byte) {
	t.Helper()
	raw := []byte("version = 1\n\n[[approval]]\npattern = \"cut off mid-p")
	if err := os.WriteFile(a.path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return a.path, raw
}

func (a *faultApprovals) Disclosure() []string { return a.disclosure() }
