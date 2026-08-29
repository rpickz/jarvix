// Package desktopentry reads the XDG desktop entries this machine already
// has, so a routine step can launch what the application menu launches.
//
// Why it exists (issue #175): on this desktop the things the user wants
// opened are not bare binaries. `ChatGPT.desktop` is
// `Exec=omarchy-launch-webapp https://chatgpt.com/`, `signal` exists only as
// an entry, and `discord` and `whatsapp` have no binary on PATH at all. A
// step that could only say `app = "chatgpt"` could never open any of them —
// it launched nothing and then waited eight seconds for a window, which is
// the silence this ticket exists to end.
//
// Three properties are the design:
//
//   - Reading is all it does. Nothing in this package executes anything;
//     Command() returns an argv and the caller decides whether to run it.
//     That is what lets the loader and the form ask "could this be launched?"
//     without launching it.
//   - The `Exec` value is parsed by the specification's own rules, not by a
//     shell. The spec's quoting is a small, closed grammar (double quotes,
//     backslash escapes for " ` $ \) and the result is an argv, so nothing an
//     entry contains can become syntax at a second level. A desktop file is
//     writable by anything the user installs, so "we hand it to sh" would be
//     a much larger promise than "we launch what it names".
//   - A missing entry is an error with a name in it, produced at the moment
//     the routine is loaded or saved. Discovering it at run time is what the
//     eight-second wait was.
package desktopentry

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Suffix is the file extension every desktop entry carries. A step may name
// an entry with or without it ("ChatGPT" and "ChatGPT.desktop" are the same
// entry), because both spellings are what people have in front of them.
const Suffix = ".desktop"

// maxEntryBytes bounds one desktop file. Entries are a few hundred bytes;
// anything past this is not a desktop entry and reading it whole would be the
// only unbounded read in the launch path.
const maxEntryBytes = 64 << 10

// Entry is one desktop entry, reduced to the keys a launch needs.
//
// The group is `[Desktop Entry]` only: actions (`[Desktop Action …]`) are a
// separate feature and a step naming one would be naming something this
// package deliberately does not model yet.
type Entry struct {
	// ID is the entry's identifier without the .desktop suffix — the name a
	// step writes ("ChatGPT").
	ID string
	// Path is the file the entry was read from, for error messages and %k.
	Path string
	// Name is the entry's display name, for %c and for spoken sentences.
	Name string
	// Exec is the raw Exec value, unparsed. Command() is what turns it into
	// an argv; keeping the raw value means an error can quote what the file
	// actually says.
	Exec string
	// Icon is the entry's icon name, for %i.
	Icon string
	// TryExec, when set, is the binary whose presence decides whether the
	// entry is installed. The specification's own "is this really here?"
	// key, and the reason a stale entry left behind by an uninstall can be
	// reported as not installed rather than launched into nothing.
	TryExec string
	// Terminal is the entry's own statement that it needs a terminal window.
	Terminal bool
	// Hidden is the specification's "deleted" flag: an entry carrying it must
	// be treated as absent.
	Hidden bool
	// NoDisplay hides an entry from menus. It does NOT hide it from a step —
	// a NoDisplay entry is still launchable, and several web-app wrappers are
	// written that way — so it is recorded and not acted on.
	NoDisplay bool
	// Type is the entry's `Type` key. Only "Application" can be launched.
	Type string
}

// NotFoundError is a step naming an entry this machine does not have. It
// carries the closest installed spellings so the message can end with the fix
// rather than with a shrug — the same discipline desktop.launch_app's
// near-match suggestion follows.
type NotFoundError struct {
	ID string
	// Near are installed entry ids that look like what was asked for, at
	// most maxSuggestions of them.
	Near []string
}

func (e *NotFoundError) Error() string {
	if len(e.Near) == 0 {
		return fmt.Sprintf("there is no %s desktop entry on this computer", e.ID)
	}
	return fmt.Sprintf("there is no %s desktop entry on this computer; the closest installed are %s",
		e.ID, strings.Join(e.Near, ", "))
}

// maxSuggestions bounds a near-match list to what fits in one sentence.
const maxSuggestions = 3

// Index is the set of desktop entries found under a list of directories.
//
// It is read once and held, rather than re-scanned per lookup, because a
// routine validates every step at load and a re-scan per step would stat the
// same few hundred files repeatedly. The cost of that choice is that an
// application installed while the daemon is running is not seen until the
// next config reload — which is the same staleness every other configured
// fact has, and a reload is what a newly installed application needs anyway.
type Index struct {
	// entries is id → entry, first-wins in directory precedence order (the
	// specification's rule: an entry in XDG_DATA_HOME shadows one in
	// /usr/share).
	entries map[string]Entry
	// order is the ids in sorted order, so a suggestion list is the same on
	// every run of the same machine.
	order []string
}

// SearchDirs returns the directories desktop entries are read from, in the
// specification's precedence order: $XDG_DATA_HOME/applications first, then
// each of $XDG_DATA_DIRS/applications.
//
// The defaults are the specification's own ($HOME/.local/share and
// /usr/local/share:/usr/share), which is what makes a test hermetic: point
// XDG_DATA_HOME and XDG_DATA_DIRS at a temporary directory and this package
// sees exactly the entries the test wrote and nothing of the real machine.
func SearchDirs() []string {
	var dirs []string
	home := strings.TrimSpace(os.Getenv("XDG_DATA_HOME"))
	if home == "" {
		if userHome, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(userHome, ".local", "share")
		}
	}
	if home != "" {
		dirs = append(dirs, filepath.Join(home, "applications"))
	}
	shared := strings.TrimSpace(os.Getenv("XDG_DATA_DIRS"))
	if shared == "" {
		shared = "/usr/local/share:/usr/share"
	}
	for _, dir := range strings.Split(shared, ":") {
		if dir = strings.TrimSpace(dir); dir != "" {
			dirs = append(dirs, filepath.Join(dir, "applications"))
		}
	}
	return dirs
}

// Load reads every entry under dirs, in precedence order.
//
// A directory that does not exist is not an error — most machines have only
// two of the four — and neither is a file that will not parse: one broken
// entry dropped by a package is not a reason for a routine to refuse to load,
// and the step that names it gets "there is no such entry", which is true.
func Load(dirs ...string) *Index {
	idx := &Index{entries: map[string]Entry{}}
	for _, dir := range dirs {
		idx.scan(dir)
	}
	idx.order = make([]string, 0, len(idx.entries))
	for id := range idx.entries {
		idx.order = append(idx.order, id)
	}
	sort.Strings(idx.order)
	return idx
}

// Default reads the machine's own entries, from SearchDirs.
func Default() *Index { return Load(SearchDirs()...) }

// scan walks one applications directory. Sub-directories are part of the
// identifier with their separators turned into dashes, which is the
// specification's own id rule ("kde/foo.desktop" is "kde-foo").
func (i *Index) scan(dir string) {
	root := os.DirFS(dir)
	_ = fs.WalkDir(root, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable subtree is skipped rather than fatal: a
			// permission-denied directory somewhere in /usr/share must not
			// cost the user every entry after it.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(path, Suffix) {
			return nil
		}
		id := strings.TrimSuffix(filepath.ToSlash(path), Suffix)
		id = strings.ReplaceAll(id, "/", "-")
		if _, taken := i.entries[id]; taken {
			return nil
		}
		entry, err := parseFile(filepath.Join(dir, filepath.FromSlash(path)))
		if err != nil {
			return nil
		}
		entry.ID = id
		if entry.Hidden {
			return nil
		}
		i.entries[id] = entry
		return nil
	})
}

// IDs returns every entry id the index holds, sorted.
func (i *Index) IDs() []string {
	if i == nil {
		return nil
	}
	return append([]string(nil), i.order...)
}

// All returns every entry the index holds, in id order.
//
// Separate from Lookup because Lookup is the *step's* question — it trims a
// `.desktop` suffix, so an id that genuinely ends in ".desktop"
// (`org.telegram.desktop`, and every other reverse-DNS id) cannot be fetched
// through it by its own id. A caller walking the index has already got the
// ids and must not be made to guess which of them that rule mangles.
func (i *Index) All() []Entry {
	if i == nil {
		return nil
	}
	out := make([]Entry, 0, len(i.order))
	for _, id := range i.order {
		out = append(out, i.entries[id])
	}
	return out
}

// Lookup finds the entry a step named.
//
// The id is matched exactly first and case-insensitively second. The
// specification says ids are case-sensitive, and they are — but the name a
// person types comes off a menu entry ("ChatGPT") or off a file listing
// ("chatgpt.desktop"), and refusing the second spelling would be pedantry
// with a real cost. Exact-first means a machine that genuinely has two
// entries differing only in case still resolves them separately.
func (i *Index) Lookup(id string) (Entry, error) {
	want := strings.TrimSpace(id)
	if want == "" {
		return Entry{}, errors.New("no desktop entry was named")
	}
	want = strings.TrimSuffix(want, Suffix)
	if i == nil {
		return Entry{}, &NotFoundError{ID: want}
	}
	if entry, ok := i.entries[want]; ok {
		return entry, nil
	}
	lower := strings.ToLower(want)
	for _, candidate := range i.order {
		if strings.ToLower(candidate) == lower {
			return i.entries[candidate], nil
		}
	}
	return Entry{}, &NotFoundError{ID: want, Near: i.near(lower)}
}

// near names installed entries whose id contains — or is contained by — what
// was asked for. Substring rather than an edit distance because the misses
// that happen in practice are "signal" for "signal-desktop" and "chatgpt" for
// "ChatGPT", not typos.
func (i *Index) near(lower string) []string {
	var out []string
	for _, candidate := range i.order {
		c := strings.ToLower(candidate)
		if strings.Contains(c, lower) || strings.Contains(lower, c) {
			out = append(out, candidate)
			if len(out) == maxSuggestions {
				break
			}
		}
	}
	return out
}

// Command turns the entry's Exec into the argv to run.
//
// Field codes are expanded per the specification with no files and no URLs to
// substitute, which is what a launch from a routine is: %f, %F, %u and %U
// disappear, %i becomes `--icon <Icon>` when there is one, %c the entry's
// name, %k its path, the deprecated codes disappear, and %% is a literal
// percent.
func (e Entry) Command() ([]string, error) {
	if kind := strings.TrimSpace(e.Type); kind != "" && kind != "Application" {
		return nil, fmt.Errorf("the %s desktop entry is a %s, not an application", e.ID, kind)
	}
	if strings.TrimSpace(e.Exec) == "" {
		return nil, fmt.Errorf("the %s desktop entry has no Exec line, so there is nothing to launch", e.ID)
	}
	argv, err := ParseExec(e.Exec)
	if err != nil {
		return nil, fmt.Errorf("the %s desktop entry's Exec line cannot be read: %w", e.ID, err)
	}
	argv = e.expandFieldCodes(argv)
	if len(argv) == 0 {
		return nil, fmt.Errorf("the %s desktop entry's Exec line names no program", e.ID)
	}
	return argv, nil
}

// expandFieldCodes applies the specification's % codes to an already-split
// argv.
func (e Entry) expandFieldCodes(argv []string) []string {
	out := make([]string, 0, len(argv))
	for _, arg := range argv {
		switch arg {
		case "%f", "%F", "%u", "%U", "%d", "%D", "%n", "%N", "%v", "%m":
			// Nothing to substitute — a routine launches an application, not
			// a file — and the deprecated codes are removed outright, which
			// is what the specification requires of a reader that sees them.
			continue
		case "%i":
			if e.Icon != "" {
				out = append(out, "--icon", e.Icon)
			}
			continue
		}
		out = append(out, expandInline(arg, e))
	}
	return out
}

// expandInline handles the codes that can legitimately appear inside a larger
// argument (%c, %k, %%), and strips any remaining file/URL code that was
// written attached to other text.
func expandInline(arg string, e Entry) string {
	var b strings.Builder
	for i := 0; i < len(arg); i++ {
		if arg[i] != '%' || i+1 >= len(arg) {
			b.WriteByte(arg[i])
			continue
		}
		switch arg[i+1] {
		case '%':
			b.WriteByte('%')
		case 'c':
			b.WriteString(e.Name)
		case 'k':
			b.WriteString(e.Path)
		case 'f', 'F', 'u', 'U', 'i', 'd', 'D', 'n', 'N', 'v', 'm':
			// Removed: there is nothing to put here.
		default:
			// Not a field code at all — a percent in a URL, say — so it is
			// text and stays text.
			b.WriteByte('%')
			b.WriteByte(arg[i+1])
		}
		i++
	}
	return b.String()
}

// ParseExec splits an Exec value into an argv by the specification's quoting
// rules, which are NOT a shell's.
//
// The whole grammar: arguments are separated by spaces or tabs; an argument
// may be wrapped in double quotes, inside which a backslash escapes one of
// " ` $ \ and nothing else. There is no word splitting of a quoted value, no
// variable expansion, no command substitution, no globbing — so a desktop
// file containing `$(rm -rf ~)` yields an argument that says exactly that and
// is passed to execve, where it is a string.
//
// Exported because the argv it produces is what the security argument is
// about, and a test pins the metacharacter cases against it directly.
func ParseExec(exec string) ([]string, error) {
	var (
		argv    []string
		current strings.Builder
		started bool
	)
	flush := func() {
		if started {
			argv = append(argv, current.String())
			current.Reset()
			started = false
		}
	}
	for i := 0; i < len(exec); i++ {
		c := exec[i]
		switch c {
		case ' ', '\t':
			flush()
		case '"':
			started = true
			closed := false
			for i++; i < len(exec); i++ {
				if exec[i] == '\\' && i+1 < len(exec) {
					switch exec[i+1] {
					case '"', '`', '$', '\\':
						current.WriteByte(exec[i+1])
						i++
						continue
					}
					current.WriteByte('\\')
					continue
				}
				if exec[i] == '"' {
					closed = true
					break
				}
				current.WriteByte(exec[i])
			}
			if !closed {
				return nil, fmt.Errorf("a quoted argument is never closed")
			}
		default:
			started = true
			current.WriteByte(c)
		}
	}
	flush()
	return argv, nil
}

// parseFile reads one desktop file's [Desktop Entry] group.
func parseFile(path string) (Entry, error) {
	f, err := os.Open(path) //nolint:gosec // path comes from walking a configured applications directory
	if err != nil {
		return Entry{}, err
	}
	defer func() { _ = f.Close() }()

	// Bounded: a desktop file is a few hundred bytes, and the cap is what
	// keeps a pathological file in an applications directory from being read
	// whole into a daemon that only wanted an Exec line.
	entry := Entry{Path: path}
	scanner := bufio.NewScanner(io.LimitReader(f, maxEntryBytes))
	inGroup := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			inGroup = line == "[Desktop Entry]"
			continue
		}
		if !inGroup {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		// Localised keys ("Name[de]") are skipped: the launch uses the
		// unlocalised value, and picking a locale here would be a second,
		// worse copy of the locale rules for no gain.
		if strings.Contains(key, "[") {
			continue
		}
		switch key {
		case "Type":
			entry.Type = value
		case "Name":
			entry.Name = value
		case "Exec":
			entry.Exec = value
		case "Icon":
			entry.Icon = value
		case "TryExec":
			entry.TryExec = value
		case "Terminal":
			entry.Terminal = value == "true"
		case "Hidden":
			entry.Hidden = value == "true"
		case "NoDisplay":
			entry.NoDisplay = value == "true"
		}
	}
	if err := scanner.Err(); err != nil {
		return Entry{}, err
	}
	return entry, nil
}
