package voice

import (
	"archive/zip"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Voice is one voice installed on this machine, described well enough to
// choose from a list. The id alone is not: nobody picks "bm_george" on
// purpose without being told it is a British male voice called George.
type Voice struct {
	// ID is what goes in the config — the Kokoro voice id, or the Piper voice
	// name.
	ID string
	// Name is the speaker's name for display ("Emma").
	Name string
	// Language is the language and accent this voice speaks.
	Language Language
	// Gender is the speaker's voice type, Unknown where the engine's naming
	// does not encode it (every Piper voice).
	Gender Gender
}

// Catalog enumerates the voices actually installed. It is an interface for one
// reason above all: no test may need the 310 MB Kokoro model or the 27 MB
// voices archive to exercise the code that chooses between voices, so every
// consumer takes a Catalog and the tests hand it a Fake.
//
// A Catalog reports installed voices, never *possible* ones. Offering a voice
// the machine cannot speak turns a menu choice into a synthesis failure
// minutes later, which is the failure this whole feature exists to avoid.
type Catalog interface {
	// Voices lists the installed voices. The error says why the list could not
	// be read — most often that the engine is not installed yet — and is
	// phrased as the fix, because it is shown to the user verbatim.
	Voices() ([]Voice, error)
}

// KokoroArchive reads the voices Kokoro can actually speak out of its voices
// file.
//
// The voices file is a zip of one `<voice_id>.npy` style embedding per voice,
// so the whole catalog is in the archive's central directory: names only, no
// embeddings decoded, and above all no ONNX model loaded. That matters because
// listing voices is something a settings screen does casually and the model is
// 310 MB — reading it to answer "what voices do I have?" would make the
// question cost more than the answer.
//
// The result is cached for the lifetime of the value, which for the daemon is
// the lifetime of the daemon: the archive only changes when setup-kokoro.sh
// runs, and that is followed by a restart.
type KokoroArchive struct {
	// Path is the voices file (voices-v1.0.bin). Required.
	Path string

	once   sync.Once
	voices []Voice
	err    error
}

// Voices implements Catalog.
func (a *KokoroArchive) Voices() ([]Voice, error) {
	a.once.Do(func() { a.voices, a.err = readKokoroArchive(a.Path) })
	return a.voices, a.err
}

func readKokoroArchive(path string) ([]Voice, error) {
	if path == "" {
		return nil, fmt.Errorf("no Kokoro voices file configured")
	}
	r, err := zip.OpenReader(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("Kokoro voices file not found at %s; install it: scripts/setup-kokoro.sh", path)
		}
		return nil, fmt.Errorf("Kokoro voices file at %s is unreadable (%v); re-run scripts/setup-kokoro.sh", path, err)
	}
	defer func() { _ = r.Close() }()

	var voices []Voice
	for _, f := range r.File {
		// Entries are one embedding per voice, named for it ("bf_emma.npy").
		// The extension is stripped rather than required, because which one
		// the archive uses is kokoro-onnx's business; the id is the part that
		// is the contract. Anything that does not parse as a voice id is
		// skipped rather than guessed at.
		name := filepath.Base(f.Name)
		v, ok := ParseKokoroVoice(strings.TrimSuffix(name, filepath.Ext(name)))
		if !ok {
			continue
		}
		voices = append(voices, v)
	}
	if len(voices) == 0 {
		return nil, fmt.Errorf("no voices found in %s; re-run scripts/setup-kokoro.sh", path)
	}
	Sort(voices)
	return voices, nil
}

// KokoroVoicesFile names the voices archive inside a Jarvix data directory.
// It lives here so the one place that knows the file name is the one place
// that reads it — the adapter, config validation, doctor, and the CLI all
// resolve it through this rather than each spelling out the path.
func KokoroVoicesFile(dataDir string) string {
	return filepath.Join(dataDir, "models", "kokoro", "voices-v1.0.bin")
}

// PiperDir reads the Piper voices installed under a set of directories, so
// choosing an accent works on the zero-setup engine too and not only on
// Kokoro.
//
// Piper keeps one `<locale>-<speaker>-<quality>.onnx` per voice, which is all
// the naming there is: the locale gives the language, and nothing gives the
// gender, so Piper voices list as gender-unknown rather than pretending.
type PiperDir struct {
	// Dirs are searched in order, recursively. Missing directories are not an
	// error — most machines have only one of them.
	Dirs []string

	once   sync.Once
	voices []Voice
	err    error
}

// Voices implements Catalog.
func (p *PiperDir) Voices() ([]Voice, error) {
	p.once.Do(func() { p.voices, p.err = readPiperDirs(p.Dirs) })
	return p.voices, p.err
}

func readPiperDirs(dirs []string) ([]Voice, error) {
	seen := make(map[string]bool)
	var voices []Voice
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				// An unreadable subtree is skipped, not fatal: a voice
				// directory the user cannot read still leaves the others
				// listable, and a partial list beats no list.
				return nil
			}
			if d.IsDir() || !strings.HasSuffix(d.Name(), ".onnx") {
				return nil
			}
			id := strings.TrimSuffix(d.Name(), ".onnx")
			if seen[id] {
				return nil
			}
			lang, ok := LanguageForPiperVoice(id)
			if !ok {
				return nil
			}
			seen[id] = true
			voices = append(voices, Voice{ID: id, Name: piperSpeaker(id), Language: lang})
			return nil
		})
	}
	if len(voices) == 0 {
		return nil, fmt.Errorf("no Piper voices found under %s; install a voice package "+
			"(e.g. sudo pacman -S piper-voices-en-us) or point tts.piper.voice at a .onnx file",
			strings.Join(dirs, ", "))
	}
	Sort(voices)
	return voices, nil
}

// piperSpeaker pulls the speaker name out of a Piper voice name:
// "en_GB-alan-medium" → "Alan (medium)". The quality tier is kept because it
// is the difference between two entries that are otherwise the same voice.
func piperSpeaker(id string) string {
	parts := strings.Split(id, "-")
	if len(parts) < 2 {
		return id
	}
	name := displayName(parts[1])
	if len(parts) > 2 {
		name += " (" + strings.Join(parts[2:], " ") + ")"
	}
	return name
}

// Fake is a Catalog for tests: no archive, no filesystem, no engine. It is
// production code rather than a _test.go helper because packages beyond this
// one (config, doctor, setup, the CLI) all need a catalog to test against,
// and each rolling its own would let them drift from the real shape.
type Fake struct {
	// List is returned verbatim (after sorting) when Err is nil.
	List []Voice
	// Err simulates a machine with no voices installed.
	Err error
}

// Voices implements Catalog.
func (f Fake) Voices() ([]Voice, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	out := append([]Voice(nil), f.List...)
	Sort(out)
	return out, nil
}

// FakeKokoro builds a Fake from Kokoro voice ids, so a test can say what it
// means ("bf_emma", "am_adam") instead of spelling out Voice literals.
func FakeKokoro(ids ...string) Fake {
	list := make([]Voice, 0, len(ids))
	for _, id := range ids {
		if v, ok := ParseKokoroVoice(id); ok {
			list = append(list, v)
		}
	}
	return Fake{List: list}
}

// ---------------------------------------------------------------- grouping

// Sort orders voices the way every listing wants them: by language in the
// display order of Languages, then female before male (so the two halves of a
// family stay together), then by id.
func Sort(voices []Voice) {
	rank := make(map[string]int, len(Languages))
	for i, l := range Languages {
		rank[l.Code] = i
	}
	sort.SliceStable(voices, func(i, j int) bool {
		a, b := voices[i], voices[j]
		if ra, rb := rank[a.Language.Code], rank[b.Language.Code]; ra != rb {
			return ra < rb
		}
		if a.Gender != b.Gender {
			return a.Gender < b.Gender
		}
		return a.ID < b.ID
	})
}

// Group is one language's voices, the unit every listing displays.
type Group struct {
	Language Language
	Voices   []Voice
}

// Grouped buckets voices by language in display order — the shape `jarvix
// voices` prints and the wizard offers, so both agree on what "grouped by
// language and accent" means.
func Grouped(voices []Voice) []Group {
	byCode := make(map[string][]Voice)
	for _, v := range voices {
		byCode[v.Language.Code] = append(byCode[v.Language.Code], v)
	}
	groups := make([]Group, 0, len(byCode))
	for _, l := range Languages {
		if in := byCode[l.Code]; len(in) > 0 {
			Sort(in)
			groups = append(groups, Group{Language: l, Voices: in})
		}
	}
	return groups
}

// In returns the installed voices for one language.
func In(voices []Voice, l Language) []Voice {
	var out []Voice
	for _, v := range voices {
		if v.Language.Code == l.Code {
			out = append(out, v)
		}
	}
	return out
}

// Find looks an installed voice up by id.
func Find(voices []Voice, id string) (Voice, bool) {
	for _, v := range voices {
		if v.ID == id {
			return v, true
		}
	}
	return Voice{}, false
}

// Has reports whether the id is installed.
func Has(voices []Voice, id string) bool {
	_, ok := Find(voices, id)
	return ok
}

// Suggest names up to n installed alternatives to a voice id that is not
// installed.
//
// The choice of alternatives is the whole point of the message: a user who
// typed "bf_emily" wants the other British voices, not a list starting at
// af_alloy. So voices in the language the *wanted id implies* come first, and
// only then does the list fall back to the general catalog — which is also
// what makes the message useful when the id is nonsense and implies nothing.
func Suggest(voices []Voice, want string, n int) []string {
	if n <= 0 {
		return nil
	}
	var preferred, rest []Voice
	wanted, ok := LanguageForKokoroVoice(want)
	for _, v := range voices {
		if ok && v.Language.Code == wanted.Code {
			preferred = append(preferred, v)
			continue
		}
		rest = append(rest, v)
	}
	out := make([]string, 0, n)
	for _, v := range append(preferred, rest...) {
		if len(out) == n {
			break
		}
		out = append(out, v.ID)
	}
	return out
}
