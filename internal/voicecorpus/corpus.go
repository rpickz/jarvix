package voicecorpus

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rpickz/jarvix/internal/audio"
)

// DirEnv names an environment variable holding the corpus directory.
//
// It exists for one specific and foreseeable reason. The recordings are the
// user's voice; this repository is private today, and the issue that asked for
// the corpus said in as many words that the files may need to move out if it
// ever opens. Pointing the harness at a directory outside the working tree has
// to be a supported, one-variable operation on that day, not a rewrite.
const DirEnv = "JARVIX_VOICE_CORPUS"

// defaultDir is where the recordings live in a source checkout, relative to
// this package's own directory — which is where `go test` runs. It is only
// ever meaningful to the harness; nothing in an installed binary looks here.
const defaultDir = "../../testdata/voicecorpus"

// wavExt is the only format whisper-cli reads.
const wavExt = ".wav"

// convertibleExts are the audio formats a person plausibly drops in the corpus
// directory instead of a WAV. Named individually so the error can say what to
// do about them rather than "unexpected file".
var convertibleExts = map[string]bool{
	".mp3": true, ".m4a": true, ".ogg": true, ".opus": true,
	".flac": true, ".aac": true, ".wma": true, ".webm": true, ".mp4": true,
}

// Recording is one file on disk, matched to the phrase it records.
type Recording struct {
	// Phrase is the manifest entry this recording is a take of.
	Phrase Phrase
	// ID is the file stem, which is the phrase id for a normal take and the
	// phrase id plus "-noisy" for a second, noisy-room take. Results and
	// baseline entries are keyed on this, not on the phrase id: the two takes
	// of one phrase pass and fail independently and are worth tracking that
	// way.
	ID string
	// Noisy marks the second take.
	Noisy bool
	// Path is the absolute-or-relative path handed to whisper.
	Path string
	// Level is what the recording measured, by the same voice-activity rule
	// the daemon applies before it will ask whisper anything (issue #191).
	Level audio.CaptureLevel
}

// Corpus is a loaded corpus directory: the recordings that are there, and the
// phrases that are not yet.
type Corpus struct {
	// Dir is the directory that was read.
	Dir string
	// Recordings are the usable takes, in phrase order, noisy take after its
	// quiet sibling.
	Recordings []Recording
	// Missing are the manifest phrases with no recording at all. Not an
	// error: an unrecorded corpus is the state this feature ships in.
	Missing []Phrase
}

// Empty reports whether there is nothing to run.
func (c Corpus) Empty() bool { return len(c.Recordings) == 0 }

// ResolveDir decides which directory the corpus lives in.
//
// explicit is true when the environment named it. The distinction changes what
// a missing directory means: the default path being absent is the ordinary
// "nobody has recorded anything yet" state and the harness skips gracefully,
// whereas a path somebody deliberately set and mistyped is an error — silently
// skipping it would hand back a green run for a corpus that was never opened.
func ResolveDir() (dir string, explicit bool, err error) {
	if set := strings.TrimSpace(os.Getenv(DirEnv)); set != "" {
		info, err := os.Stat(set)
		if err != nil {
			return set, true, fmt.Errorf("%s is set to %q, which cannot be read: %w", DirEnv, set, err)
		}
		if !info.IsDir() {
			return set, true, fmt.Errorf("%s is set to %q, which is not a directory", DirEnv, set)
		}
		return set, true, nil
	}
	return defaultDir, false, nil
}

// Load reads a corpus directory against a manifest.
//
// A directory that does not exist, or holds no recordings, is an empty corpus
// and not an error — that is the graceful path, and the caller turns it into a
// skip with a note. Everything else that is wrong IS an error, reported all at
// once and by file name:
//
//   - a file whose name is not a phrase id (a typo'd stem matches no phrase,
//     and would otherwise vanish silently from the run);
//   - a recording in a format whisper cannot read;
//   - a WAV that will not parse;
//   - a WAV with no voiced audio in it.
//
// That last one is the important one, and it is deliberately stricter than the
// daemon. In production an unmeasurable or silent capture errs towards asking
// whisper anyway, because dropping a real question is the worse mistake. Here
// the opposite is true: a silent file in the corpus is a recording that failed,
// and if it were sent to whisper the engine would either return nothing or
// echo the bias prompt back (issue #191) — so the harness would be grading the
// hallucination-suppression path while reporting on speech recognition. Naming
// it as a corpus defect, with the level it measured, is the only honest answer.
func Load(dir string, m Manifest) (Corpus, error) {
	c := Corpus{Dir: dir}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			c.Missing = append(c.Missing, m.Phrases...)
			return c, nil
		}
		return c, fmt.Errorf("voice corpus %s: %w", dir, err)
	}

	var problems []string
	found := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		ext := strings.ToLower(filepath.Ext(name))
		path := filepath.Join(dir, name)
		switch {
		case ext == wavExt:
			// Fall through to the checks below.
		case convertibleExts[ext]:
			problems = append(problems, fmt.Sprintf(
				"%s: whisper-cli reads 16 kHz mono WAV only; convert it with\n"+
					"      ffmpeg -i %s -ar 16000 -ac 1 -c:a pcm_s16le %s",
				name, name, strings.TrimSuffix(name, filepath.Ext(name))+wavExt))
			continue
		default:
			// Notes, a README, an editor's leavings: not audio, not this
			// package's business.
			continue
		}

		stem := strings.TrimSuffix(name, filepath.Ext(name))
		id, noisy := strings.CutSuffix(stem, NoisySuffix)
		phrase, ok := m.Find(id)
		if !ok {
			problems = append(problems, fmt.Sprintf(
				"%s: no phrase with id %q in the manifest (internal/voicecorpus/phrases.toml)", name, id))
			continue
		}
		if found[stem] {
			// Two files differing only in extension case, on a case-sensitive
			// filesystem. Rare, but it would silently run one and drop the other.
			problems = append(problems, fmt.Sprintf("%s: a second recording claims the id %q", name, stem))
			continue
		}
		found[stem] = true

		level, err := audio.MeasureWAV(path)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: cannot be read as a WAV: %v", name, err))
			continue
		}
		if !level.Voiced() {
			problems = append(problems, fmt.Sprintf(
				"%s: no voiced audio (peak %.0f dBFS, against a floor of %.0f dBFS) — "+
					"the daemon would not ask whisper about this capture at all, so it cannot be scored; re-record it",
				name, level.DBFS(), audio.DBFS(audio.SilenceFloorRMS)))
			continue
		}
		c.Recordings = append(c.Recordings, Recording{
			Phrase: phrase, ID: stem, Noisy: noisy, Path: path, Level: level,
		})
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return c, fmt.Errorf("voice corpus %s:\n  %s", dir, strings.Join(problems, "\n  "))
	}

	for _, p := range m.Phrases {
		if !found[p.ID] {
			c.Missing = append(c.Missing, p)
		}
	}
	sort.Slice(c.Recordings, func(i, j int) bool { return c.Recordings[i].ID < c.Recordings[j].ID })
	return c, nil
}
