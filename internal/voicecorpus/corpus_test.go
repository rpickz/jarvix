package voicecorpus

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/audio"
)

// The manifest these tests load against. Small and local, so a change to the
// shipped phrase list cannot break the loader's tests — and so each case names
// the exact ids it is about.
const testManifest = `
[[phrase]]
id = "01-yes"
say = "yes"
expect = { affirmative = true }

[[phrase]]
id = "02-workspace-four"
say = "workspace four"
noisy_take = true
expect = { intent = "workspace.switch", slot = 4 }
`

func loadTestManifest(t *testing.T) Manifest {
	t.Helper()
	m, err := ParseManifest(testManifest)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	return m
}

// writeSpeechLike writes a WAV that is comfortably above the voice-activity
// floor. It is not speech and is never transcribed by these tests — the point
// is only that Load's gate treats it as a recording with sound in it.
func writeSpeechLike(t *testing.T, path string) {
	t.Helper()
	const sampleRate = 16000
	pcm := make([]int16, sampleRate/2)
	for i := range pcm {
		// A 220 Hz tone at about a quarter of full scale: three orders of
		// magnitude above audio.SilenceFloorRMS, so this test asserts the
		// gate's verdict rather than sitting on its threshold.
		pcm[i] = int16(8000 * math.Sin(2*math.Pi*220*float64(i)/sampleRate))
	}
	if err := audio.WriteWAV(path, pcm, sampleRate, 1); err != nil {
		t.Fatalf("WriteWAV %s: %v", path, err)
	}
}

// writeSilence writes a WAV of digital silence — the shape that made whisper
// echo its own bias prompt back as a transcript (issue #191).
func writeSilence(t *testing.T, path string) {
	t.Helper()
	if err := audio.WriteWAV(path, make([]int16, 16000), 16000, 1); err != nil {
		t.Fatalf("WriteWAV %s: %v", path, err)
	}
}

func TestLoadWithNoDirectoryIsAnEmptyCorpusNotAnError(t *testing.T) {
	m := loadTestManifest(t)
	c, err := Load(filepath.Join(t.TempDir(), "never-recorded"), m)
	if err != nil {
		t.Fatalf("a corpus nobody has recorded yet must not be an error: %v", err)
	}
	if !c.Empty() {
		t.Errorf("Empty() is false for %d recordings", len(c.Recordings))
	}
	if len(c.Missing) != len(m.Phrases) {
		t.Errorf("Missing lists %d phrases, want all %d", len(c.Missing), len(m.Phrases))
	}
}

func TestLoadWithAnEmptyDirectoryIsAnEmptyCorpus(t *testing.T) {
	c, err := Load(t.TempDir(), loadTestManifest(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.Empty() {
		t.Errorf("Empty() is false for %d recordings", len(c.Recordings))
	}
}

func TestLoadPairsRecordingsWithTheirPhrasesAndTheirNoisyTakes(t *testing.T) {
	dir := t.TempDir()
	writeSpeechLike(t, filepath.Join(dir, "02-workspace-four.wav"))
	writeSpeechLike(t, filepath.Join(dir, "02-workspace-four-noisy.wav"))
	// Not audio, and none of this package's business.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("recordings live here"), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := Load(dir, loadTestManifest(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.Recordings) != 2 {
		t.Fatalf("loaded %d recordings, want 2: %+v", len(c.Recordings), c.Recordings)
	}
	quiet, noisy := c.Recordings[0], c.Recordings[1]
	if quiet.ID != "02-workspace-four" || quiet.Noisy {
		t.Errorf("first recording is %q (noisy %v)", quiet.ID, quiet.Noisy)
	}
	if noisy.ID != "02-workspace-four-noisy" || !noisy.Noisy {
		t.Errorf("second recording is %q (noisy %v)", noisy.ID, noisy.Noisy)
	}
	if noisy.Phrase.ID != "02-workspace-four" {
		t.Errorf("the noisy take is attached to phrase %q, not to the phrase it is a take of", noisy.Phrase.ID)
	}
	if !quiet.Level.Voiced() {
		t.Error("the recording's measured level did not survive onto the Recording")
	}
	if len(c.Missing) != 1 || c.Missing[0].ID != "01-yes" {
		t.Errorf("Missing is %+v, want just 01-yes", c.Missing)
	}
}

// TestLoadFailsLoudlyOnABrokenCorpus is the rule the house style asks for
// explicitly: a corpus directory that exists and holds something wrong is
// never a skip and never a quiet omission. Each case is a mistake somebody
// makes in a recording session.
func TestLoadFailsLoudlyOnABrokenCorpus(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, dir string)
		want  string
	}{
		{
			name: "a stem that matches no phrase",
			setup: func(t *testing.T, dir string) {
				writeSpeechLike(t, filepath.Join(dir, "99-typo.wav"))
			},
			want: `no phrase with id "99-typo"`,
		},
		{
			name: "a format whisper cannot read",
			setup: func(t *testing.T, dir string) {
				if err := os.WriteFile(filepath.Join(dir, "01-yes.m4a"), []byte("not a wav"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: "ffmpeg -i 01-yes.m4a -ar 16000 -ac 1",
		},
		{
			name: "a WAV that will not parse",
			setup: func(t *testing.T, dir string) {
				if err := os.WriteFile(filepath.Join(dir, "01-yes.wav"), []byte("RIFFnope"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: "cannot be read as a WAV",
		},
		{
			name: "a recording with nothing in it",
			setup: func(t *testing.T, dir string) {
				writeSilence(t, filepath.Join(dir, "01-yes.wav"))
			},
			want: "no voiced audio",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			c.setup(t, dir)
			got, err := Load(dir, loadTestManifest(t))
			if err == nil {
				t.Fatalf("Load accepted a broken corpus: %+v", got)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

// TestLoadReportsEveryProblemAtOnce: a recording session produces several
// mistakes together, and fixing them one run at a time wastes the only person
// who can fix them.
func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	dir := t.TempDir()
	writeSilence(t, filepath.Join(dir, "01-yes.wav"))
	writeSpeechLike(t, filepath.Join(dir, "99-typo.wav"))

	_, err := Load(dir, loadTestManifest(t))
	if err == nil {
		t.Fatal("Load accepted a corpus with two defects in it")
	}
	for _, want := range []string{"no voiced audio", "no phrase with id"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestLoadRefusesADirectoryItCannotRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not stop a read")
	}
	dir := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := Load(dir, loadTestManifest(t)); err == nil {
		t.Error("Load treated an unreadable directory as an empty corpus")
	}
}

func TestResolveDirDefaultsIntoTheSourceTree(t *testing.T) {
	t.Setenv(DirEnv, "")
	dir, explicit, err := ResolveDir()
	if err != nil {
		t.Fatalf("ResolveDir: %v", err)
	}
	if explicit {
		t.Error("ResolveDir reports the default path as explicitly chosen")
	}
	if !strings.HasSuffix(filepath.ToSlash(dir), "testdata/voicecorpus") {
		t.Errorf("default corpus directory is %q", dir)
	}
}

func TestResolveDirFollowsTheEnvironment(t *testing.T) {
	want := t.TempDir()
	t.Setenv(DirEnv, want)
	dir, explicit, err := ResolveDir()
	if err != nil {
		t.Fatalf("ResolveDir: %v", err)
	}
	if dir != want || !explicit {
		t.Errorf("ResolveDir = %q (explicit %v), want %q (explicit true)", dir, explicit, want)
	}
}

// TestResolveDirRefusesADeliberateButWrongPath: a mistyped override must not
// present as "nobody has recorded anything", which would hand back a green run
// for a corpus that was never opened.
func TestResolveDirRefusesADeliberateButWrongPath(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		t.Setenv(DirEnv, filepath.Join(t.TempDir(), "nowhere"))
		if _, _, err := ResolveDir(); err == nil {
			t.Error("ResolveDir accepted a path that does not exist")
		}
	})
	t.Run("not a directory", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "a-file")
		if err := os.WriteFile(file, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv(DirEnv, file)
		_, _, err := ResolveDir()
		if err == nil || !strings.Contains(err.Error(), "not a directory") {
			t.Errorf("ResolveDir on a file gave %v", err)
		}
	})
}
