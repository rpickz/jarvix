// Package launchkind knows how each program on this machine is started: a
// graphical application directly, a terminal program inside the user's
// terminal.
//
// Why it exists (issue #194). Asked to launch Claude, Jarvix resolved `claude`
// on PATH, executed it bare through the compositor, and said "it is opening;
// its window will appear on its own". `claude` is a command-line program: with
// no terminal and no stdin it exited at once, so nothing opened and the user
// was told, confidently, that something had. That is the exact failure the
// honesty rules (#71) exist to prevent, and rewording the sentence would not
// have touched it — there was one notion of "launch", exec a binary and expect
// a window, and it is wrong for a whole class of programs.
//
// The signal this package turns into a decision was already on the machine and
// unused: a graphical application ships an XDG desktop entry, and a
// command-line tool does not. On the machine this was written for, `claude`,
// `opencode`, `codex` and `grok` are all on PATH with no entry of their own,
// and that alone identifies them. The desktop-entry specification also carries
// `Terminal=true` outright, which internal/desktopentry has parsed since #186
// and the routine loader refused; the refusal's reasoning was right — a
// terminal program launched with no terminal maps no window — and its remedy
// is what this package is.
//
// Three properties are the design:
//
//   - It classifies and reads; it launches nothing. Lookup answers "what is
//     this and how does it start", Command answers "what argv would start
//     it", and the caller decides whether to run it. That is what lets a
//     refusal be spoken before anything happens.
//   - The absence of an entry is only evidence when entries were looked for.
//     On a machine with no desktop entries at all, "it has no entry" is a fact
//     about the search, not about the program, so the answer is KindUnknown
//     and the honest thing to do is ask.
//   - Every inference is overridable. The classification is a strong default
//     rather than a law, and the user's own statement about their own machine
//     is the one thing that outranks it.
package launchkind

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rpickz/jarvix/internal/desktopentry"
)

// Kind is how a program is started.
type Kind int

// The kinds.
const (
	// KindUnknown: nothing on this machine says which of the other two it is,
	// so the honest move is to ask rather than to launch hopefully.
	KindUnknown Kind = iota
	// KindGraphical: started directly; a window appears on its own.
	KindGraphical
	// KindTerminal: started inside the configured terminal, because on its own
	// it has nowhere to write and nothing to read.
	KindTerminal
)

// String renders a kind for a log line. The spoken wording is the caller's —
// what the user hears is a sentence, not an enum.
func (k Kind) String() string {
	switch k {
	case KindGraphical:
		return "graphical"
	case KindTerminal:
		return "terminal"
	default:
		return "unknown"
	}
}

// Reason is where a classification came from. It is carried rather than
// discarded because every message this feeds — the spoken answer, the journal
// line, the question asked about an ambiguous name — is better for being able
// to say *why*, and because a wrong classification is then one sentence away
// from being diagnosed instead of a mystery.
type Reason int

// The reasons, in the precedence order Lookup applies them.
const (
	// ReasonOverride: the user said so in configuration. Outranks everything,
	// because the person who owns the machine knows it better than a rule.
	ReasonOverride Reason = iota
	// ReasonEntryTerminal: a desktop entry with Terminal=true — the
	// specification's own explicit statement that this needs a terminal.
	ReasonEntryTerminal
	// ReasonEntryWindow: a desktop entry that does not ask for a terminal.
	ReasonEntryWindow
	// ReasonNoEntry: on PATH, and nothing on this machine ships a desktop
	// entry for it. The default that identifies claude, opencode, codex and
	// grok.
	ReasonNoEntry
	// ReasonUnsurveyed: on PATH, and this machine has no desktop entries at
	// all — so "it has no entry" says nothing about the program.
	ReasonUnsurveyed
)

// Because renders the reason as a clause a sentence can be built from. Written
// to be said out loud, because it ends up in the tool result the model speaks
// from when it has to ask.
func (r Reason) Because() string {
	switch r {
	case ReasonOverride:
		return "you told me it is"
	case ReasonEntryTerminal:
		return "its application entry says it runs in a terminal"
	case ReasonEntryWindow:
		return "it has an application entry"
	case ReasonNoEntry:
		return "it is a command with no application entry"
	default:
		return "this computer has no application entries at all, so I cannot tell"
	}
}

// Program is one launchable thing on this machine, classified.
type Program struct {
	// Name is what to call it: the desktop entry's id, or the program's name
	// on PATH. It is the name a person would say and the name a launch is
	// asked for by.
	Name string
	// Display is the entry's own display name ("Claude Desktop"), empty for a
	// program that has no entry. It is for the sentence, not for matching.
	Display string
	// Kind is how it starts.
	Kind Kind
	// Reason is why Kind says that.
	Reason Reason
	// Entry is the desktop entry id this came from, empty for a program known
	// only from PATH.
	Entry string
	// Argv is what to run, before any terminal is wrapped round it. Empty in
	// a listing: a catalogue answers "what is there and how does it start",
	// and working out an argv for every program on PATH would make reading
	// the catalogue as expensive as launching everything in it.
	Argv []string
}

// InTerminal reports whether starting this program means opening a terminal.
func (p Program) InTerminal() bool { return p.Kind == KindTerminal }

// maxCandidates bounds how many programs one spoken name may resolve to. The
// list ends up inside a question the user has to answer out loud, so it is
// bounded by what fits in one sentence, exactly as the launcher's near-match
// suggestions are.
const maxCandidates = 3

// defaultRecheck is how often the source directories are re-examined for
// staleness. It is a floor on the cost of asking, not a claim about
// freshness: what decides whether the catalogue is rebuilt is whether those
// directories have changed, and this only decides how often we look.
const defaultRecheck = 2 * time.Second

// maxPathEntries bounds the PATH inventory. A listing is for a person to read
// through a spoken answer; a machine with a pathological PATH must cost a
// bounded scan, not an unbounded one.
const maxPathEntries = 8192

// Options configure a Catalogue. Every field is a seam with a real default, so
// production passes none of them and a test passes all of them and sees a
// machine it wrote itself.
type Options struct {
	// Load reads the desktop entries. Nil uses desktopentry.Default, which
	// reads this machine's XDG search path.
	Load func() *desktopentry.Index
	// EntryDirs are the directories Load reads. They are watched for change,
	// so installing an application invalidates the catalogue. Nil uses
	// desktopentry.SearchDirs.
	EntryDirs func() []string
	// PathDirs are the directories the PATH inventory is built from. Nil
	// splits $PATH.
	PathDirs func() []string
	// LookPath resolves one bare program name. Nil uses exec.LookPath. This
	// is the seam the launcher and routine capture already keep, so a test
	// can say what is installed without installing anything.
	LookPath func(name string) (string, error)
	// TerminalPrograms and GraphicalPrograms are the user's overrides: the
	// names they have said start one way rather than the other.
	TerminalPrograms  []string
	GraphicalPrograms []string
	// Recheck overrides defaultRecheck.
	Recheck time.Duration
	// Now overrides time.Now, so a test can drive the recheck clock.
	Now func() time.Time
}

// Catalogue is what this machine can launch, and how.
//
// It is built lazily on the first question and then reused, because the scan
// behind it — every desktop entry under the XDG search path — is exactly the
// thing that must not happen on every launch. It is rebuilt when the
// directories it was drawn from have changed, which is an honest invalidation
// rule in a way a timer is not: a five-minute TTL would go on claiming the
// picture is current for five minutes after an install, and would redraw it
// pointlessly for ever on a machine nobody installs anything on. What
// changes the answer is a directory changing, so that is what is watched.
type Catalogue struct {
	load       func() *desktopentry.Index
	entryDirs  func() []string
	pathDirs   func() []string
	lookPath   func(name string) (string, error)
	terminal   map[string]bool
	graphical  map[string]bool
	recheck    time.Duration
	now        func() time.Time
	mu         sync.Mutex
	built      bool
	checkedAt  time.Time
	stamps     []stamp
	entries    []desktopentry.Entry
	byID       map[string]desktopentry.Entry
	byExec     map[string][]string
	pathNames  []string
	pathWalked bool
}

// stamp is one watched directory as it was when the catalogue was built. Not
// a hash of its contents: a desktop entry edited in place changes the file's
// mtime and the directory's, and an installed or removed one changes the
// directory's too, which is the whole set of events that can change an answer.
type stamp struct {
	dir     string
	exists  bool
	modTime time.Time
	size    int64
}

// New builds a catalogue. Nothing is read until the first question.
func New(opts Options) *Catalogue {
	c := &Catalogue{
		load:      opts.Load,
		entryDirs: opts.EntryDirs,
		pathDirs:  opts.PathDirs,
		lookPath:  opts.LookPath,
		terminal:  nameSet(opts.TerminalPrograms),
		graphical: nameSet(opts.GraphicalPrograms),
		recheck:   opts.Recheck,
		now:       opts.Now,
	}
	if c.entryDirs == nil {
		c.entryDirs = desktopentry.SearchDirs
	}
	if c.load == nil {
		c.load = func() *desktopentry.Index { return desktopentry.Load(c.entryDirs()...) }
	}
	if c.pathDirs == nil {
		c.pathDirs = func() []string { return filepath.SplitList(os.Getenv("PATH")) }
	}
	if c.lookPath == nil {
		c.lookPath = exec.LookPath
	}
	if c.recheck <= 0 {
		c.recheck = defaultRecheck
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c
}

// nameSet lower-cases an override list into a set. Case-insensitive because
// the user typed it and the entry ids they were copying from are not
// consistently cased ("ChatGPT", "signal-desktop").
func nameSet(names []string) map[string]bool {
	if len(names) == 0 {
		return nil
	}
	set := make(map[string]bool, len(names))
	for _, name := range names {
		if trimmed := strings.ToLower(strings.TrimSpace(name)); trimmed != "" {
			set[trimmed] = true
		}
	}
	return set
}

// Invalidate drops the catalogue, so the next question rebuilds it. Called on
// a configuration reload: the overrides are part of the picture, and a
// reloaded daemon that kept the old one would answer from configuration the
// user has replaced.
func (c *Catalogue) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.built, c.pathWalked = false, false
}

// snapshot returns the current picture, rebuilding it when the directories it
// was drawn from have changed.
func (c *Catalogue) snapshot() {
	if c.built && !c.stale() {
		return
	}
	c.entries = c.load().All()
	c.byID = make(map[string]desktopentry.Entry, len(c.entries))
	for _, entry := range c.entries {
		c.byID[entry.ID] = entry
	}
	c.byExec = execIndex(c.entries)
	c.stamps = c.stampDirs()
	c.built, c.pathWalked = true, false
	c.checkedAt = c.now()
}

// stale reports whether any watched directory has changed since the catalogue
// was built. The recheck interval bounds how often the stat calls happen; it
// does not bound how long a stale answer survives, because a directory that
// has not changed cannot produce one.
func (c *Catalogue) stale() bool {
	now := c.now()
	if now.Sub(c.checkedAt) < c.recheck {
		return false
	}
	c.checkedAt = now
	return !slices.Equal(c.stamps, c.stampDirs())
}

// stampDirs records every watched directory: the applications directories and
// the PATH directories, in a fixed order so two stamp lists compare.
func (c *Catalogue) stampDirs() []stamp {
	dirs := append([]string(nil), c.entryDirs()...)
	dirs = append(dirs, c.pathDirs()...)
	stamps := make([]stamp, 0, len(dirs))
	for _, dir := range dirs {
		s := stamp{dir: dir}
		if info, err := os.Stat(dir); err == nil {
			s.exists, s.modTime, s.size = true, info.ModTime(), info.Size()
		}
		stamps = append(stamps, s)
	}
	return stamps
}

// execIndex maps a program name to the desktop entries that launch it.
//
// This is the half of "a graphical application ships a desktop entry" that
// makes the rule survive contact with real packaging. An entry's id is very
// often not its binary's name — Telegram installs
// `org.telegram.desktop.desktop` and a `telegram-desktop` binary — so asking
// only "is there an entry called telegram-desktop?" would answer no and send
// a graphical application into a terminal. Asking "does any entry launch
// telegram-desktop?" answers yes, which is the fact the rule is about.
func execIndex(entries []desktopentry.Entry) map[string][]string {
	index := map[string][]string{}
	for _, entry := range entries {
		argv, err := desktopentry.ParseExec(entry.Exec)
		if err != nil || len(argv) == 0 {
			continue
		}
		program := strings.ToLower(filepath.Base(argv[0]))
		index[program] = append(index[program], entry.ID)
		if try := strings.TrimSpace(entry.TryExec); try != "" {
			key := strings.ToLower(filepath.Base(try))
			if key != program {
				index[key] = append(index[key], entry.ID)
			}
		}
	}
	for _, ids := range index {
		sort.Strings(ids)
	}
	return index
}

// Lookup returns every program this machine has that answers to name,
// classified, at most maxCandidates of them.
//
// Several results is not a failure: it is the honest state of a machine that
// has both a `claude` command and a Claude Desktop application, and the caller
// is expected to ask rather than pick. None means nothing here is called that.
func (c *Catalogue) Lookup(name string) []Program {
	query := strings.ToLower(strings.TrimSpace(name))
	if query == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snapshot()

	var (
		exact, prefixed []Program
		claimed         = map[string]bool{}
	)
	// The entries whose own name is what was asked for, then the ones that
	// merely begin with it. Split so an exact answer is never buried under a
	// longer one that happens to start the same way.
	for _, id := range c.entryIDs() {
		switch {
		case matchesEntry(id, c.displayName(id), query):
			if p, ok := c.entryProgram(id); ok {
				exact, claimed[id] = append(exact, p), true
			}
		case prefixesEntry(id, c.displayName(id), query):
			if p, ok := c.entryProgram(id); ok {
				prefixed, claimed[id] = append(prefixed, p), true
			}
		}
	}

	// Then the program on PATH, if there is one under exactly that name. An
	// entry that launches it is the better description of the same thing —
	// it carries the Terminal key and the full command line — so the entry
	// answers for it, and only a program no entry launches is a program known
	// from PATH alone.
	if path, err := c.lookPath(query); err == nil {
		owned := false
		for _, id := range c.byExec[query] {
			if claimed[id] {
				owned = true
				continue
			}
			if p, ok := c.entryProgram(id); ok {
				exact, claimed[id], owned = append(exact, p), true, true
			}
		}
		if !owned {
			exact = append(exact, c.pathProgram(query, path))
		}
	}

	out := append(exact, prefixed...)
	if len(out) > maxCandidates {
		out = out[:maxCandidates]
	}
	return out
}

// Describe classifies a program the caller has already resolved to a path —
// the shape an allow list of absolute paths produces, where the name a person
// says never reached PATH at all.
//
// It is Lookup's tail without Lookup's search: the entry that launches this
// program answers for it if there is one, and otherwise the PATH rule applies
// with the same condition on it.
func (c *Catalogue) Describe(name, path string) Program {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snapshot()
	lower := strings.ToLower(filepath.Base(strings.TrimSpace(name)))
	for _, id := range c.byExec[lower] {
		if p, ok := c.entryProgram(id); ok {
			return p
		}
	}
	return c.pathProgram(lower, path)
}

// entryIDs is the entry ids in a fixed order, so the same machine always
// answers the same question the same way.
func (c *Catalogue) entryIDs() []string {
	ids := make([]string, 0, len(c.entries))
	for _, entry := range c.entries {
		ids = append(ids, entry.ID)
	}
	return ids
}

// displayName is an entry's Name key, for matching what a person would say.
func (c *Catalogue) displayName(id string) string { return c.byID[id].Name }

// matchesEntry reports whether an entry is what was asked for by name.
// Spaces in a display name become dashes because that is the only spelling
// the launcher accepts: a program name may not contain whitespace, so "Claude
// Desktop" reaches us as "claude-desktop" or not at all.
func matchesEntry(id, display, query string) bool {
	return strings.ToLower(id) == query || normaliseName(display) == query
}

// prefixesEntry reports whether an entry's name begins with what was asked
// for, at a word boundary: "claude" finds "claude-desktop", and does not find
// "claudia". The boundary is what keeps this from being a substring search,
// which would make half the machine ambiguous with the other half.
func prefixesEntry(id, display, query string) bool {
	return hasNamePrefix(strings.ToLower(id), query) || hasNamePrefix(normaliseName(display), query)
}

func hasNamePrefix(name, query string) bool {
	if len(name) <= len(query) || !strings.HasPrefix(name, query) {
		return false
	}
	switch name[len(query)] {
	case '-', '.', '_':
		return true
	}
	return false
}

// normaliseName folds a display name into the spelling a launch can be asked
// for by.
func normaliseName(display string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(display), " ", "-"))
}

// entryProgram classifies one desktop entry, and reports whether this machine
// can actually launch it. An entry left behind by an uninstall — its TryExec
// or its Exec naming a program that is gone — is not a candidate: offering it
// would turn "not installed" into a question about two things, one of which
// does not exist.
func (c *Catalogue) entryProgram(id string) (Program, bool) {
	entry, ok := c.byID[id]
	if !ok {
		return Program{}, false
	}
	argv, err := entry.Command()
	if err != nil {
		return Program{}, false
	}
	if try := strings.TrimSpace(entry.TryExec); try != "" {
		if _, err := c.lookPath(try); err != nil {
			return Program{}, false
		}
	}
	path, err := c.lookPath(argv[0])
	if err != nil {
		return Program{}, false
	}
	// The resolved path, not the name: what runs is decided here and not
	// searched again on PATH by the exec that follows.
	argv[0] = path

	p := Program{Name: entry.ID, Display: entry.Name, Entry: entry.ID, Argv: argv}
	switch {
	case c.overridden(entry.ID, &p):
	case entry.Terminal:
		p.Kind, p.Reason = KindTerminal, ReasonEntryTerminal
	default:
		p.Kind, p.Reason = KindGraphical, ReasonEntryWindow
	}
	return p, true
}

// pathProgram classifies a program known only from PATH.
//
// This is the ticket's rule, and the condition on it is the ticket's honesty.
// A graphical application ships a desktop entry and a command-line tool does
// not, so a program with no entry is a command-line tool — but only on a
// machine that has entries to not be among. On one with none, the same
// observation is a fact about the search, and answering KindTerminal from it
// would send every application on the machine into a terminal window with
// exactly the confidence this package exists to remove.
func (c *Catalogue) pathProgram(name, path string) Program {
	p := Program{Name: name, Argv: []string{path}}
	switch {
	case c.overridden(name, &p):
	case len(c.entryIDs()) == 0:
		p.Kind, p.Reason = KindUnknown, ReasonUnsurveyed
	default:
		p.Kind, p.Reason = KindTerminal, ReasonNoEntry
	}
	return p
}

// overridden applies the user's own answer, and reports whether they gave one.
// It runs before every inference because it is the one source that is not an
// inference: the person who owns the machine has said what this program is.
func (c *Catalogue) overridden(name string, p *Program) bool {
	lower := strings.ToLower(name)
	switch {
	case c.terminal[lower]:
		p.Kind, p.Reason = KindTerminal, ReasonOverride
	case c.graphical[lower]:
		p.Kind, p.Reason = KindGraphical, ReasonOverride
	default:
		return false
	}
	return true
}

// List is the catalogue the model consults: what can be launched here, and
// how each one starts.
//
// match narrows it; an empty match lists the applications this machine has
// entries for, which is what a person means by "what can you open?". The
// programs are returned without an argv — see Program.Argv for why — and
// total is how many matched, so an answer can say what it left out instead of
// implying the list is the machine.
func (c *Catalogue) List(match string, limit int) (found []Program, total int) {
	query := strings.ToLower(strings.TrimSpace(match))
	if limit <= 0 {
		limit = maxCandidates
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snapshot()

	add := func(p Program) {
		total++
		if len(found) < limit {
			// Without the argv. A listing is read out, not run: carrying the
			// command line for every program on the machine into a spoken
			// answer would be a launch's worth of detail per row, and the
			// name is what a launch is asked for by anyway.
			p.Argv = nil
			found = append(found, p)
		}
	}
	for _, id := range c.entryIDs() {
		if query != "" && !strings.Contains(strings.ToLower(id), query) &&
			!strings.Contains(normaliseName(c.displayName(id)), query) {
			continue
		}
		if p, ok := c.entryProgram(id); ok {
			add(p)
		}
	}
	// PATH programs join the listing only when something was asked for. With
	// no query the answer is "your applications", and appending several
	// thousand commands to it would bury them.
	if query == "" {
		return found, total
	}
	for _, name := range c.pathInventory() {
		if !strings.Contains(name, query) || len(c.byExec[name]) > 0 {
			continue
		}
		path, err := c.lookPath(name)
		if err != nil {
			continue
		}
		add(c.pathProgram(name, path))
	}
	return found, total
}

// pathInventory is every program name on PATH, sorted and de-duplicated.
//
// Built lazily and separately from the entries, because it is needed only by
// a listing: classifying one named program costs a single PATH lookup, and
// making a launch pay for a walk of every PATH directory is precisely the
// expensive-scan-per-launch this must not be.
func (c *Catalogue) pathInventory() []string {
	if c.pathWalked {
		return c.pathNames
	}
	seen := map[string]bool{}
	var names []string
	for _, dir := range c.pathDirs() {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		items, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, item := range items {
			if item.IsDir() {
				continue
			}
			name := strings.ToLower(item.Name())
			if seen[name] {
				continue
			}
			seen[name] = true
			names = append(names, name)
			if len(names) >= maxPathEntries {
				break
			}
		}
		if len(names) >= maxPathEntries {
			break
		}
	}
	sort.Strings(names)
	c.pathNames, c.pathWalked = names, true
	return names
}

// ErrNoProgram is Command asked to start something with no argv — a program
// that came out of a listing rather than out of Lookup.
var ErrNoProgram = errors.New("there is nothing to start")

// Command returns the argv that starts p, and the class its window will
// carry.
//
// A graphical program is its own argv. A terminal program is that argv inside
// the configured terminal, with an identity where the terminal supports one,
// so the window it opens can be found afterwards by the routines and the
// window tools (#186's mechanism, applied to the terminal instead of to the
// program it hosts). An unclassified program has no command at all: the point
// of KindUnknown is that we do not know what starting it would mean.
func (c *Catalogue) Command(p Program, terminal string) (argv []string, identity string, err error) {
	if len(p.Argv) == 0 {
		return nil, "", ErrNoProgram
	}
	if p.Kind != KindTerminal {
		return append([]string(nil), p.Argv...), "", nil
	}
	spelling, err := LookupTerminal(terminal)
	if err != nil {
		return nil, "", err
	}
	path, err := c.lookPath(terminal)
	if err != nil {
		return nil, "", &TerminalMissingError{Name: TerminalName(terminal)}
	}
	identity = spelling.Identity(p.Name)
	return spelling.Wrap(path, identity, p.Argv), identity, nil
}

// TerminalMissingError is a terminal this table knows the spelling for but
// the machine does not have. A different failure from an unknown terminal,
// and a different fix — install it, or configure the one that is here — so it
// is a different sentence.
type TerminalMissingError struct{ Name string }

func (e *TerminalMissingError) Error() string {
	return e.Name + " is not installed, so there is no terminal to run it in"
}
