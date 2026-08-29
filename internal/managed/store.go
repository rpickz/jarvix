package managed

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/statehold"
)

// DefaultMaxWindows bounds the store. Generous rather than tight — nobody
// hands over sixty-four terminals — and it exists for the reason every store
// here has a cap: a file that can grow without limit is a file a stuck loop
// can fill, and a refusal naming the limit is a better failure than a state
// dir that will not fit on the disk.
const DefaultMaxWindows = 64

// DefaultClaimGrace is how long a launch claim waits for its window.
//
// It is the one number in this package that is a guess, and it is deliberately
// generous: a cold Electron application on a loaded machine can take a while
// to map its first window, and a claim that expires early costs the user a
// window Jarvix opened and then disowned. Expiring at all is the point — a
// launch that failed, or a terminal that ignored the class flag, must not
// leave a standing offer to adopt whatever turns up wearing that name an hour
// later.
const DefaultClaimGrace = 2 * time.Minute

// ErrStoreFull is returned by Acquire at MaxWindows.
var ErrStoreFull = errors.New("managed-window store is full")

// StoreOptions configure a Store.
type StoreOptions struct {
	// MaxWindows overrides DefaultMaxWindows.
	MaxWindows int
	// ClaimGrace overrides DefaultClaimGrace.
	ClaimGrace time.Duration
	// Now is the clock, injected so tests are deterministic.
	Now func() time.Time
	// Gate is the backup write barrier (ADR 0045); nil is never held.
	Gate *statehold.Gate
}

// Store is the managed-window store: one TOML file, read through a stat so a
// hand-edit is live on the next question, written atomically.
//
// It holds no compositor. Every method that answers a question about windows
// takes the live inventory as a parameter, for the monitor store's reason
// inverted: this store's answers are only meaningful *against* an inventory,
// and a store that fetched its own would be a second place deciding what is
// on screen (ADR 0022 allows exactly one).
type Store struct {
	path  string
	max   int
	grace time.Duration
	now   func() time.Time
	gate  *statehold.Gate
	log   *slog.Logger
	// write is the disk seam: writeStore in production, a failing or
	// counting stub in tests.
	write func(path string, records []Record, claims []Claim) error

	mu      sync.Mutex
	records []Record
	claims  []Claim
	loaded  bool
	mod     time.Time
	size    int64
	corrupt bool
}

// NewStore builds a store over path. Construction reads nothing: the first
// question stats the file, so a daemon nobody ever hands a window to never
// touches the disk.
func NewStore(path string, opts StoreOptions, log *slog.Logger) *Store {
	s := &Store{
		path:  path,
		max:   opts.MaxWindows,
		grace: opts.ClaimGrace,
		now:   opts.Now,
		gate:  opts.Gate,
		log:   log,
		write: writeStore,
	}
	if s.max <= 0 {
		s.max = DefaultMaxWindows
	}
	if s.grace <= 0 {
		s.grace = DefaultClaimGrace
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s
}

// Path returns the file management lives in, so every surface can tell the
// user where to edit it by hand.
func (s *Store) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// Live is one managed window with the window as the inventory reports it now.
type Live struct {
	Record Record
	Window desktop.Window
}

// ByAddress reconciles the store against the live inventory and returns what
// is managed, keyed by window address.
//
// It is the one read every other question goes through, and reconciling is
// part of reading rather than a background chore for a reason worth stating:
// there is no window-created and no window-closed event seam anywhere in this
// repository (ADR 0048 says why the overlays poll), so "has that window
// gone?" can only be answered by looking at an inventory, and the honest
// moment to answer it is the moment someone asks.
//
// Two things happen here, and both write when they change something:
//
//   - a record whose window is not in the inventory is dropped — management
//     ends with the window, and nothing is left claiming one that has gone;
//   - a live window wearing a launch claim's class becomes a record, managed
//     from birth (#198), and the claim is consumed.
//
// The address is a lookup key in the returned map and nothing more; it never
// enters anything rendered or anything on the wire (ADR 0022).
func (s *Store) ByAddress(windows []desktop.Window) map[string]Record {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	s.reconcileLocked(windows)
	if len(s.records) == 0 {
		return nil
	}
	out := make(map[string]Record, len(s.records))
	for _, r := range s.records {
		for _, w := range windows {
			if matches(r, w) {
				out[w.Address] = r
				break
			}
		}
	}
	return out
}

// Managed reports whether one window is managed, judged against the same
// inventory it was read from.
func (s *Store) Managed(target desktop.Window, windows []desktop.Window) (Record, bool) {
	if s == nil {
		return Record{}, false
	}
	held, ok := s.ByAddress(windows)[target.Address]
	if !ok {
		return Record{}, false
	}
	// The map is keyed by address; prove the rest of the identity too, in
	// case the caller's window and the inventory's disagree about what that
	// address is (a stale capture from before a close-and-open).
	if !matches(held, target) {
		return Record{}, false
	}
	return held, true
}

// List returns every managed window with its live facts, ordered by
// application then title so every surface lists them identically.
func (s *Store) List(windows []desktop.Window) []Live {
	if s == nil {
		return nil
	}
	byAddress := s.ByAddress(windows)
	out := make([]Live, 0, len(byAddress))
	for _, w := range windows {
		if rec, ok := byAddress[w.Address]; ok {
			out = append(out, Live{Record: rec, Window: w})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := desktop.AppName(out[i].Window.Class), desktop.AppName(out[j].Window.Class)
		if !strings.EqualFold(a, b) {
			return strings.ToLower(a) < strings.ToLower(b)
		}
		return out[i].Window.Title < out[j].Window.Title
	})
	return out
}

// Count reports how many windows are managed right now, deliberately without
// reconciling — no inventory is consulted.
//
// It exists as the window-overlay feed's cheap enrolment gate (#127): "is
// there anything a poll could possibly mark?" must be answerable without a
// compositor call, or the gate would cost exactly what it exists to save.
// The price is honesty at the margin, and it is the same trade
// desktop.Nicknames.Count makes: a record whose window has closed still
// counts until the next reconciliation drops it — which the overlay poll
// itself performs, so an over-count converges to zero rather than polling for
// ever. Pending claims count too: a launch has happened and its window is
// expected, so the feed should be awake to mark it.
func (s *Store) Count() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	return len(s.records) + len(s.claims)
}

// Acquire hands a window over. fresh is false when it was already managed —
// re-acquiring writes nothing and is not an error, because "take control of
// this terminal" said twice is one wish stated twice.
//
// Acquisition is the gated act (the confirmation naming the window lives in
// the tool, ADR 0062); this store performs it and records nothing about
// permission, because acquisition grants none.
func (s *Store) Acquire(target desktop.Window, windows []desktop.Window) (rec Record, fresh bool, err error) {
	if s == nil {
		return Record{}, false, fmt.Errorf("managed windows are not available")
	}
	if strings.TrimSpace(target.Address) == "" {
		return Record{}, false, fmt.Errorf("that window has no handle I can hold on to")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	s.reconcileLocked(windows)
	for _, held := range s.records {
		if matches(held, target) {
			return held, false, nil
		}
	}
	if len(s.records) >= s.max {
		return Record{}, false, fmt.Errorf("%w: %d managed windows is the limit; "+
			"let one go before taking another", ErrStoreFull, s.max)
	}
	rec = Record{
		Address: target.Address, StableID: target.StableID, Class: target.Class,
		PID: target.PID, App: desktop.AppName(target.Class),
		Source: SourceAcquired, Since: s.now().UTC(),
	}
	if err := s.saveLocked(append(append([]Record(nil), s.records...), rec), s.claims); err != nil {
		return Record{}, false, err
	}
	return rec, true, nil
}

// Release gives a window back. held is false when it was not managed to begin
// with, which callers report as such rather than as a success: "let this go"
// answered with "done" when Jarvix never had it is the shrug the whole
// feature exists to replace.
//
// Releasing is never gated, anywhere in this feature, and this is the seam
// that makes that easy to keep true: there is no confirmation to skip,
// because giving up power needs no permission.
func (s *Store) Release(target desktop.Window, windows []desktop.Window) (rec Record, held bool, err error) {
	if s == nil {
		return Record{}, false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	s.reconcileLocked(windows)
	for i, r := range s.records {
		if !matches(r, target) {
			continue
		}
		next := append(append([]Record(nil), s.records[:i]...), s.records[i+1:]...)
		if err := s.saveLocked(next, s.claims); err != nil {
			return Record{}, false, err
		}
		return r, true, nil
	}
	return Record{}, false, nil
}

// ReleaseAddress is Release for a caller that has an address and no window —
// the window's Release button, which is handed a listing row rather than an
// inventory entry. It resolves the address against the inventory first, so
// the identity check is the same one every other path makes.
func (s *Store) ReleaseAddress(address string, windows []desktop.Window) (Record, bool, error) {
	for _, w := range windows {
		if w.Address == address {
			return s.Release(w, windows)
		}
	}
	return Record{}, false, nil
}

// ClaimLaunch records that Jarvix has just opened a window and what class it
// asked that window to carry (#198's launched-window identity).
//
// It is the managed-from-birth mechanism, and it is a claim rather than a
// record because at this instant there is no window: no address, no pid,
// nothing to match on. The first inventory showing a window wearing the class
// turns the claim into a record.
//
// An empty class is not an error and records nothing. That is the honest
// answer for a graphical launch, which carries no identity at all (only the
// terminal table issues one): Jarvix cannot recognise the window it just
// opened, so it must not claim to.
func (s *Store) ClaimLaunch(class, program string) error {
	if s == nil {
		return nil
	}
	class = strings.TrimSpace(class)
	if class == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	now := s.now().UTC()
	next := make([]Claim, 0, len(s.claims)+1)
	for _, c := range s.claims {
		// One claim per class: launching the same program twice before the
		// first window appears is one expectation, refreshed. Two stanzas
		// would adopt one window and then expire the other as unmatched.
		if strings.EqualFold(c.Class, class) {
			continue
		}
		next = append(next, c)
	}
	next = append(next, Claim{Class: class, Program: program, Issued: now})
	sortClaims(next)
	return s.saveLocked(s.records, next)
}

// matches reports whether a record names this window.
//
// All four facts together, and never the address alone: a compositor address
// is a pointer value that is reused after the window holding it is destroyed,
// so a record matching on it alone would eventually hand a stranger's window
// the management the user granted to one that has since closed.
//
// An empty stable id or a zero pid in the RECORD is a wildcard, and only
// there. Machine-written records always carry whatever the compositor
// reported, so the wildcard is reachable only from a hand-edit — where it is
// the right answer: the file is the user's, a stanza they wrote by hand names
// a window with the facts they could see, and refusing to honour it would
// make the file look broken rather than permissive.
func matches(r Record, w desktop.Window) bool {
	if r.Address == "" || r.Address != w.Address {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(r.Class), strings.TrimSpace(w.Class)) {
		return false
	}
	if r.StableID != "" && r.StableID != w.StableID {
		return false
	}
	if r.PID != 0 && r.PID != w.PID {
		return false
	}
	return true
}

// reconcileLocked drops records whose window has gone, adopts the windows a
// launch claim promised, and expires claims nothing matched. Callers hold
// s.mu and pass the inventory the answer will be judged against. It writes
// only when something actually changed, so the overlay's two-second poll
// costs no disk traffic on a quiet desktop.
func (s *Store) reconcileLocked(windows []desktop.Window) {
	now := s.now().UTC()
	kept := make([]Record, 0, len(s.records))
	closed := 0
	for _, r := range s.records {
		alive := false
		for _, w := range windows {
			if matches(r, w) {
				alive = true
				break
			}
		}
		if alive {
			kept = append(kept, r)
			continue
		}
		closed++
	}

	adopted := make([]Record, 0, len(s.claims))
	liveClaims := make([]Claim, 0, len(s.claims))
	expired := 0
	for _, c := range s.claims {
		// Expiry is judged BEFORE adoption, and the order is the rule rather
		// than an implementation detail: a claim that has run out has run
		// out, and a window turning up an hour later wearing the class is
		// the user's, not the launch's.
		if now.Sub(c.Issued) > s.grace {
			expired++
			continue
		}
		taken := false
		for _, w := range windows {
			if !strings.EqualFold(strings.TrimSpace(w.Class), strings.TrimSpace(c.Class)) {
				continue
			}
			if managedAlready(kept, adopted, w) {
				continue
			}
			if len(kept)+len(adopted) >= s.max {
				// At the cap, a claim is neither adopted nor dropped: the
				// window is open and Jarvix did open it, so the honest state
				// is "not managed yet", and letting one go makes room.
				break
			}
			adopted = append(adopted, Record{
				Address: w.Address, StableID: w.StableID, Class: w.Class, PID: w.PID,
				App: desktop.AppName(w.Class), Source: SourceLaunched,
				Program: c.Program, Since: now,
			})
			taken = true
			break
		}
		if !taken {
			// Still waiting: the window has not appeared yet, and the grace
			// period has not run out.
			liveClaims = append(liveClaims, c)
		}
	}

	if closed == 0 && expired == 0 && len(adopted) == 0 {
		return
	}
	next := append(kept, adopted...)
	sortRecords(next)
	sortClaims(liveClaims)
	if err := s.saveLocked(next, liveClaims); err != nil {
		// A store that cannot be written must still answer honestly about
		// what is on screen, so the reconciled view is taken in memory and
		// the disk is retried on the next change. The alternative — keeping a
		// closed window listed because the write failed — is the one thing
		// this function exists to prevent.
		s.records, s.claims = next, liveClaims
		s.log.Warn("managed-window store could not be written; the reconciled state is in memory only",
			"component", "managed", "path", s.path, "error", err.Error())
		return
	}
	if closed > 0 || len(adopted) > 0 {
		s.log.Info("managed windows reconciled", "component", "managed",
			"adopted", len(adopted), "closed", closed, "expired_claims", expired,
			"managed", len(next))
	}
}

// managedAlready reports whether a window is already covered, so one claim
// cannot adopt a window another record already names.
func managedAlready(kept, adopted []Record, w desktop.Window) bool {
	for _, set := range [][]Record{kept, adopted} {
		for _, r := range set {
			if matches(r, w) {
				return true
			}
		}
	}
	return false
}

// refreshLocked brings the in-memory state up to date with the file. Callers
// hold s.mu. Every failure degrades: a missing file means nothing is managed,
// an unreadable or unparseable one is a warning plus nothing managed — never
// an error to the caller, never a crash (ADR 0011's precedent, via the memory
// book).
//
// Degrading towards "nothing is managed" rather than "everything stays
// managed" is the safe direction here and it is chosen deliberately: the
// failure of a store that grants access must lose the grant, never invent
// one.
func (s *Store) refreshLocked() {
	info, err := os.Stat(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		// Deleting the file is a legitimate hand-edit, and it means exactly
		// what it looks like: Jarvix manages nothing.
		s.records, s.claims, s.corrupt = nil, nil, false
		s.loaded, s.mod, s.size = true, time.Time{}, 0
		return
	}
	if err != nil {
		if !s.corrupt {
			s.log.Warn("managed-window store could not be read; continuing with nothing managed",
				"component", "managed", "error", err.Error())
		}
		s.records, s.claims, s.corrupt = nil, nil, true
		s.loaded = true
		return
	}
	if s.loaded && info.ModTime().Equal(s.mod) && info.Size() == s.size {
		return // unchanged since the last load or write — the common case
	}
	records, claims, err := readStore(s.path)
	s.loaded, s.mod, s.size = true, info.ModTime(), info.Size()
	if err != nil {
		// Warned per corruption event, not per question: the mtime/size check
		// above keeps this branch quiet until the file changes again.
		if !s.corrupt {
			s.log.Warn("managed-window store could not be parsed; continuing with nothing managed "+
				"(the file is left alone until something is saved)",
				"component", "managed", "path", s.path, "error", err.Error())
		}
		s.records, s.claims, s.corrupt = nil, nil, true
		return
	}
	s.records, s.claims, s.corrupt = normalize(records), normalizeClaims(claims), false
}

// saveLocked writes the store and only then commits it to memory, so a failed
// write leaves the store describing what is actually on disk.
func (s *Store) saveLocked(records []Record, claims []Claim) error {
	defer s.gate.Enter()()
	if s.corrupt {
		// Never overwrite a file we could not read: the user's hand-edit may
		// be one typo away from correct, and it is theirs.
		backup := s.path + ".corrupt"
		if err := os.Rename(s.path, backup); err == nil {
			s.log.Warn("unparseable managed-window store moved aside before writing",
				"component", "managed", "path", s.path, "backup", backup)
		}
		s.corrupt = false
	}
	if err := s.write(s.path, records, claims); err != nil {
		return err
	}
	s.records, s.claims = records, claims
	if info, err := os.Stat(s.path); err == nil {
		s.loaded, s.mod, s.size = true, info.ModTime(), info.Size()
	}
	return nil
}

// normalize makes a hand-edited file behave like a written one: blank and
// duplicate entries dropped (first wins, the order the file reads in), an
// unrecognised source folded to "acquired" — the file says a window was
// handed over, and how is a detail beside that — and the whole list sorted.
// Nothing is written back: a file that is merely untidy still works, and only
// a real change rewrites it.
func normalize(records []Record) []Record {
	seen := make(map[string]bool, len(records))
	out := make([]Record, 0, len(records))
	for _, r := range records {
		r.Address = strings.TrimSpace(r.Address)
		r.Class = strings.TrimSpace(r.Class)
		if r.Address == "" || r.Class == "" || seen[recordKey(r)] {
			continue
		}
		seen[recordKey(r)] = true
		if r.Source != SourceLaunched && r.Source != SourceAcquired {
			r.Source = SourceAcquired
		}
		if strings.TrimSpace(r.App) == "" {
			r.App = desktop.AppName(r.Class)
		}
		out = append(out, r)
	}
	sortRecords(out)
	return out
}

// normalizeClaims applies the same tidying to the pending launches.
func normalizeClaims(claims []Claim) []Claim {
	seen := make(map[string]bool, len(claims))
	out := make([]Claim, 0, len(claims))
	for _, c := range claims {
		c.Class = strings.TrimSpace(c.Class)
		key := strings.ToLower(c.Class)
		if c.Class == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, c)
	}
	sortClaims(out)
	return out
}

// recordKey identifies one record for the duplicate check: the whole
// identity, because two stanzas differing only in pid are two claims about
// two different windows and dropping either would be a guess.
func recordKey(r Record) string {
	return strings.ToLower(r.Address) + "\x00" + r.StableID + "\x00" +
		strings.ToLower(r.Class) + "\x00" + fmt.Sprint(r.PID)
}

func sortRecords(records []Record) {
	sort.SliceStable(records, func(i, j int) bool { return recordKey(records[i]) < recordKey(records[j]) })
}

func sortClaims(claims []Claim) {
	sort.SliceStable(claims, func(i, j int) bool {
		return strings.ToLower(claims[i].Class) < strings.ToLower(claims[j].Class)
	})
}
