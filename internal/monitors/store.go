package monitors

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

	"github.com/rpickz/jarvix/internal/placement"
	"github.com/rpickz/jarvix/internal/statehold"
)

// DefaultMaxNicknames bounds the store. It is generous rather than tight —
// nobody has thirty-two screens — and exists for the reason every store here
// has a cap: a file that can grow without limit is a file a stuck loop can
// fill, and a refusal naming the limit is a better failure than a state dir
// that will not fit on the disk.
const DefaultMaxNicknames = 32

// Sentinels the IPC surface maps to wire codes. Everything else a caller sees
// is a Refusal (below), which carries the field a form should show it on.
var (
	// ErrUnknownNickname is returned by Forget for a name nothing holds.
	ErrUnknownNickname = errors.New("unknown nickname")
	// ErrStoreFull is returned by Assign at MaxNicknames.
	ErrStoreFull = errors.New("monitor store is full")
)

// FieldName and FieldConnector are the form fields a refusal is keyed to.
// They are the wire keys of the window's screen-name form and nothing else —
// a monitor nickname is not a placement key, so they deliberately do not
// borrow placement.FieldMonitor, which names the *reference* in a routine
// step rather than the name being assigned.
const (
	FieldName      = "name"
	FieldConnector = "connector"
)

// Refusal is an assignment that could not happen, keyed to the field a form
// would show it on — the same field-keyed shape the placement vocabulary and
// the config entry forms use (placement.Problem, daemon entryProblem), so one
// message written once lands on the right control in the window, reads
// correctly in a CLI error, and is speakable as it stands.
//
// Message is written lowercase and complete, the window-nickname discipline
// (ADR 0040): the caller frames it ("Sorry, …") without rewording it.
type Refusal struct {
	Problem placement.Problem
}

// Error renders the refusal as the sentence a person hears.
func (r *Refusal) Error() string { return r.Problem.Message }

// refuse builds a Refusal keyed to a field.
func refuse(field, format string, args ...any) error {
	return &Refusal{Problem: placement.Problem{Field: field, Message: fmt.Sprintf(format, args...)}}
}

// StoreOptions configure a Store.
type StoreOptions struct {
	// MaxNicknames overrides DefaultMaxNicknames.
	MaxNicknames int
	// Now is the clock, injected so tests are deterministic.
	Now func() time.Time
	// Gate is the backup write barrier (ADR 0045); nil is never held.
	Gate *statehold.Gate
}

// Store is the monitor-nickname store: one TOML file, read through a stat so
// a hand-edit is live on the next resolution, written atomically.
//
// It holds no compositor and no inventory. That separation is the whole
// reason a nickname survives a cable move: the store knows only that "top"
// means `HDMI-A-1`, and whether `HDMI-A-1` is plugged in is a question asked
// at the moment a routine runs, by placement.Resolver, against the live
// inventory. Nothing here is ever resolved in advance.
type Store struct {
	path string
	max  int
	now  func() time.Time
	gate *statehold.Gate
	log  *slog.Logger
	// write is the disk seam: writeStore in production, a failing or
	// counting stub in tests.
	write func(path string, names []Nickname) error

	mu      sync.Mutex
	names   []Nickname
	loaded  bool
	mod     time.Time
	size    int64
	corrupt bool
}

// NewStore builds a store over path. Construction reads nothing: the first
// operation stats the file, so a daemon that never asks about screens never
// touches the disk.
func NewStore(path string, opts StoreOptions, log *slog.Logger) *Store {
	s := &Store{
		path:  path,
		max:   opts.MaxNicknames,
		now:   opts.Now,
		gate:  opts.Gate,
		log:   log,
		write: writeStore,
	}
	if s.max <= 0 {
		s.max = DefaultMaxNicknames
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s
}

// Path returns the file the nicknames live in, so every surface can tell the
// user where to edit them by hand.
func (s *Store) Path() string { return s.path }

// Lookup answers one nickname, and is exactly the function
// placement.Resolver.Nicknames wants. A nil store answers "not known", so a
// daemon built without one behaves byte-for-byte like a pre-#180 daemon.
//
// It re-reads through a stat on every call rather than caching, and that is
// the point rather than an oversight: a routine resolves its monitors at run
// time, and a snapshot taken when the runner was built would make a hand-edit
// (or a nickname assigned by voice thirty seconds ago) invisible until a
// restart.
func (s *Store) Lookup(name string) (connector string, known bool) {
	if s == nil {
		return "", false
	}
	key := nicknameKey(name)
	if key == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	for _, n := range s.names {
		if n.Name == key {
			return n.Connector, true
		}
	}
	return "", false
}

// Resolver returns the placement resolver backed by this store — the one
// seam every consumer of a monitor reference goes through (ADR 0056). A nil
// store yields the zero resolver, which is the pre-nickname behaviour.
func (s *Store) Resolver() placement.Resolver {
	if s == nil {
		// Spelled out rather than left to the zero value, and the drift guard
		// in the placement package is why: a bare Resolver literal is how a
		// consumer silently loses screen names, so the ONE place that means
		// to have no table says so.
		return placement.Resolver{Nicknames: nil}
	}
	return placement.Resolver{Nicknames: s.Lookup}
}

// List returns every nickname, sorted by name so every surface lists them
// identically.
func (s *Store) List() []Nickname {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	return append([]Nickname(nil), s.names...)
}

// Count reports how many nicknames are held and the cap.
func (s *Store) Count() (n, max int) {
	if s == nil {
		return 0, DefaultMaxNicknames
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	return len(s.names), s.max
}

// Assign gives a screen a nickname, judged against the outputs that are
// plugged in right now.
//
// present is the live inventory. It is a parameter rather than something the
// store fetches because the store has no compositor by design — and because
// the caller has already read the inventory to work out which screen "this
// monitor" is.
//
// A name another screen already answers to is a REFUSAL here, not a silent
// re-point, and that is the #130 rule applied where it matters most: "call
// this monitor top" said while sitting at the wrong desk would otherwise
// change what every routine mentioning `top` means, with nothing said. Moving
// a name to another screen is Repoint — a deliberate act, on a name the user
// named — and the refusal says so.
//
// The refusal order is the #130 collision discipline applied to screens, and
// it puts the most specific owner first (validateName):
//
//  1. nothing to use as a name at all;
//  2. a name equal to a screen that is plugged in, naming that screen with
//     its size — the owner a user is most likely to have meant;
//  3. a name spelled like a connector even when nothing is plugged into it —
//     a present output always outranks a nickname (ADR 0056), so such a name
//     would work until the cable it is named after arrived, and then quietly
//     mean something else;
//  4. more than one word — a nickname is a single word so it stays easy to
//     say, and a multi-word handle is the fragile reference nicknames exist
//     to replace;
//  5. a spelling the placement vocabulary would refuse to resolve;
//  6. a reserved word (`current`, `primary`), naming what owns it;
//  7. a name another screen already answers to, naming that screen.
//
// The connector checks come BEFORE the single-word rule deliberately: "DP-2"
// normalises to two words, so a single-word rule applied first would refuse
// the most likely collision of all with "try just dp" instead of explaining
// that a connector name cannot be a nickname.
//
// Deliberately NOT refused: a name that is also a window nickname, or that is
// verbatim an intent phrase. Window nicknames are refused against the intent
// grammar because a bare window reference IS an utterance the router could
// take; a monitor nickname never is — it is only ever read as the value of a
// `monitor` key or the tail of "call this monitor …" — so the two namespaces
// cannot collide in any sentence, and refusing there would take words the
// user is entitled to.
func (s *Store) Assign(spoken, connector string, present []placement.Monitor) (Nickname, error) {
	if s == nil {
		return Nickname{}, refuse(FieldName, "screen names are not available")
	}
	name, err := validateName(spoken, present)
	if err != nil {
		return Nickname{}, err
	}
	target, err := validateConnector(connector, present)
	if err != nil {
		return Nickname{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	for _, held := range s.names {
		if held.Name != name {
			continue
		}
		if strings.EqualFold(held.Connector, target) {
			return held, nil // already true; a re-assert writes nothing
		}
		return Nickname{}, refuse(FieldName, "%q is already the name of %s; "+
			"forget it first if you want to move the name, or choose a different one",
			name, describe(held.Connector, present))
	}
	if len(s.names) >= s.max {
		return Nickname{}, fmt.Errorf("%w: %d screen names is the limit; "+
			"forget one before naming another", ErrStoreFull, s.max)
	}
	now := s.now().UTC()
	fresh := Nickname{Name: name, Connector: target, Named: now, Updated: now}
	next := append(append([]Nickname(nil), s.names...), fresh)
	sortNicknames(next)
	if err := s.saveLocked(next); err != nil {
		return Nickname{}, err
	}
	return fresh, nil
}

// Repoint moves an existing nickname to another screen — the cable-moved
// case, and the ONE thing this store exists to make cheap. It returns the
// updated nickname and the connector the name used to mean.
//
// It is a separate verb from Assign rather than a branch inside it because
// the two acts are different: naming a screen is something the user is
// looking at, while re-pointing a name silently changes what every routine
// mentioning it does. Making it its own call means the window's Edit button
// and an explicit `jarvix monitors repoint` can do it, and a misheard "call
// this monitor top" cannot.
func (s *Store) Repoint(spoken, connector string, present []placement.Monitor) (Nickname, string, error) {
	if s == nil {
		return Nickname{}, "", ErrUnknownNickname
	}
	name := nicknameKey(spoken)
	target, err := validateConnector(connector, present)
	if err != nil {
		return Nickname{}, "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	for i, held := range s.names {
		if held.Name != name {
			continue
		}
		if strings.EqualFold(held.Connector, target) {
			return held, "", nil // already true; writes nothing
		}
		updated := Nickname{Name: name, Connector: target,
			Named: held.Named, Updated: s.now().UTC()}
		next := append([]Nickname(nil), s.names...)
		next[i] = updated
		if err := s.saveLocked(next); err != nil {
			return Nickname{}, "", err
		}
		return updated, held.Connector, nil
	}
	return Nickname{}, "", fmt.Errorf("%w: no screen is called %q", ErrUnknownNickname, name)
}

// describe renders a connector for a refusal: with its size when it is
// plugged in, by name alone when it is not — a name the user cannot see on a
// desk is still the honest answer to "who owns this word".
func describe(connector string, present []placement.Monitor) string {
	for _, m := range present {
		if strings.EqualFold(m.Name, connector) {
			return m.Describe()
		}
	}
	return connector
}

// Forget drops a nickname, returning what it meant. A name nothing holds is
// ErrUnknownNickname rather than a silent success: "forget the monitor called
// top" answered with "done" when nothing was called top is the shrug this
// whole feature exists to replace.
func (s *Store) Forget(spoken string) (Nickname, error) {
	if s == nil {
		return Nickname{}, ErrUnknownNickname
	}
	key := nicknameKey(spoken)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked()
	for i, held := range s.names {
		if held.Name != key {
			continue
		}
		next := append(append([]Nickname(nil), s.names[:i]...), s.names[i+1:]...)
		if err := s.saveLocked(next); err != nil {
			return Nickname{}, err
		}
		return held, nil
	}
	return Nickname{}, fmt.Errorf("%w: no screen is called %q", ErrUnknownNickname, key)
}

// validateName runs a spoken name through the whole collision matrix and
// returns the normalised key that would be stored.
func validateName(spoken string, present []placement.Monitor) (string, error) {
	raw := strings.TrimSpace(spoken)
	if raw == "" {
		return "", refuse(FieldName, "I did not catch a name to use")
	}
	words := nicknameWords(raw)
	// Both spellings are judged for the connector collisions, and the RAW one
	// is judged first: "DP-2" normalises to two words ("dp", "2"), so a
	// single-word rule applied before the connector rule would refuse the
	// most likely collision of all with "try just dp" instead of saying why
	// a connector name cannot be a nickname.
	candidates := []string{raw}
	if len(words) == 1 {
		candidates = append(candidates, words[0])
	}
	for _, candidate := range candidates {
		for _, m := range present {
			if strings.EqualFold(m.Name, candidate) {
				return "", refuse(FieldName, "%q is already the name of %s; choose a different name",
					candidate, m.Describe())
			}
		}
	}
	for _, candidate := range candidates {
		if placement.MonitorRef(candidate).Kind() != placement.RefConnector {
			continue
		}
		// Refused even when nothing is plugged into it today. A present
		// output always outranks a nickname (ADR 0056), so this name would
		// work right up until the cable it is named after arrives — and then
		// silently mean something else, which is the failure mode nicknames
		// exist to remove rather than to reproduce one level up.
		return "", refuse(FieldName, "%q is how a screen names itself — a connector name — and a "+
			"screen that is plugged in always wins that name; choose a word like \"top\"", candidate)
	}
	switch {
	case len(words) == 0:
		// Punctuation only — the name survived the trim but nothing in it is
		// a letter or a digit, so there is no word to store.
		return "", refuse(FieldName, "I did not catch a name to use")
	case len(words) > 1:
		return "", refuse(FieldName,
			"a screen name is a single word, so it stays easy to say — try just %q", words[0])
	}
	name := words[0]
	// The vocabulary decides what can be spelled as a monitor reference. A
	// name it would refuse could be stored and listed and would then never
	// resolve, which is worse than the refusal it replaced.
	if problem := placement.MonitorRef(name).Problem(); problem != "" {
		return "", refuse(FieldName, "%s", problem)
	}
	if owner, taken := placement.ReservedMonitorWord(name); taken {
		return "", refuse(FieldName,
			"%q already means something when you name a screen — %s; choose a different name",
			name, owner)
	}
	return name, nil
}

// validateConnector checks the screen a nickname is being pointed at.
//
// The connector must be plugged in. That is a stricter rule than resolution
// applies — a nickname naming an absent output resolves to an honest "no
// monitor is called top right now" and the run carries on — and it is
// deliberate: at ASSIGNMENT the user is looking at their screens, so a
// connector nobody can see is a typo, and accepting it would store a name
// that never worked and say nothing.
func validateConnector(connector string, present []placement.Monitor) (string, error) {
	trimmed := strings.TrimSpace(connector)
	if trimmed == "" {
		return "", refuse(FieldConnector, "I did not catch which screen to name")
	}
	for _, m := range present {
		if strings.EqualFold(m.Name, trimmed) {
			return m.Name, nil // the compositor's own spelling, never the caller's
		}
	}
	if len(present) == 0 {
		return "", refuse(FieldConnector, "the window manager reports no monitors")
	}
	names := make([]string, 0, len(present))
	for _, m := range present {
		names = append(names, m.Name)
	}
	sort.Strings(names)
	return "", refuse(FieldConnector, "no monitor is called %q right now; the screens plugged in are %s",
		trimmed, strings.Join(names, ", "))
}

// refreshLocked brings the in-memory nicknames up to date with the file.
// Callers hold s.mu. Every failure degrades: a missing file is no nicknames,
// an unreadable or unparseable one is a warning plus no nicknames — never an
// error to the caller, never a crash (ADR 0011's precedent, via the memory
// book). Degrading rather than failing matters more here than in the other
// stores: a broken nickname file must cost the user their nicknames, not
// their morning routine.
func (s *Store) refreshLocked() {
	info, err := os.Stat(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		// Deleting the file is a legitimate hand-edit: deletion is deletion.
		s.names, s.corrupt = nil, false
		s.loaded, s.mod, s.size = true, time.Time{}, 0
		return
	}
	if err != nil {
		if !s.corrupt {
			s.log.Warn("monitor store could not be read; continuing with no screen names",
				"component", "monitors", "error", err.Error())
		}
		s.names, s.corrupt = nil, true
		s.loaded = true
		return
	}
	if s.loaded && info.ModTime().Equal(s.mod) && info.Size() == s.size {
		return // unchanged since the last load or write — the common case
	}
	names, err := readStore(s.path)
	s.loaded, s.mod, s.size = true, info.ModTime(), info.Size()
	if err != nil {
		// Warned per corruption event, not per resolution: the mtime/size
		// check above keeps this branch quiet until the file changes again.
		if !s.corrupt {
			s.log.Warn("monitor store could not be parsed; continuing with no screen names "+
				"(the file is left alone until something is saved)",
				"component", "monitors", "path", s.path, "error", err.Error())
		}
		s.names, s.corrupt = nil, true
		return
	}
	s.names, s.corrupt = normalize(names), false
}

// saveLocked writes the nicknames and only then commits them to memory, so a
// failed write leaves the store describing what is actually on disk.
func (s *Store) saveLocked(names []Nickname) error {
	defer s.gate.Enter()()
	if s.corrupt {
		// Never overwrite a file we could not read: the user's hand-edit may
		// be one typo away from correct, and it is theirs.
		backup := s.path + ".corrupt"
		if err := os.Rename(s.path, backup); err == nil {
			s.log.Warn("unparseable monitor store moved aside before writing",
				"component", "monitors", "path", s.path, "backup", backup)
		}
		s.corrupt = false
	}
	if err := s.write(s.path, names); err != nil {
		return err
	}
	s.names = names
	if info, err := os.Stat(s.path); err == nil {
		s.loaded, s.mod, s.size = true, info.ModTime(), info.Size()
	}
	return nil
}

// normalize makes a hand-edited file behave like a written one: names folded
// to their lookup key, blank or duplicate entries dropped (first wins, the
// order the file reads in), timestamps filled in, and the whole list sorted.
// Nothing is written back — normalising on read means a file that is merely
// untidy still works, and only a real change rewrites it.
func normalize(names []Nickname) []Nickname {
	seen := make(map[string]bool, len(names))
	out := make([]Nickname, 0, len(names))
	for _, n := range names {
		key := nicknameKey(n.Name)
		connector := strings.TrimSpace(n.Connector)
		if key == "" || connector == "" || seen[key] {
			continue
		}
		seen[key] = true
		if n.Updated.IsZero() {
			n.Updated = n.Named
		}
		out = append(out, Nickname{Name: key, Connector: connector,
			Named: n.Named, Updated: n.Updated})
	}
	sortNicknames(out)
	return out
}

func sortNicknames(names []Nickname) {
	sort.Slice(names, func(i, j int) bool { return names[i].Name < names[j].Name })
}

// nicknameKey reduces a spoken name to the single token it is stored and
// looked up by. Identical in shape to the window registry's normalisation
// (internal/desktop/nickname.go), for the same reason: the name assigned must
// be byte-for-byte the token a lookup will ask for, however it was heard or
// capitalised.
func nicknameKey(s string) string {
	words := nicknameWords(s)
	if len(words) != 1 {
		return ""
	}
	return words[0]
}

// nicknameWords lower-cases and splits on anything that is not a letter or a
// digit, so "Top", "top." and "top" are one name.
func nicknameWords(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}
