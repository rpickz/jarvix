package audio

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The streaming path exists so background listening never writes audio to
// disk (ADR 0024). These tests cover the one place it deliberately does —
// turning a captured request into a file whisper can read — and nothing here
// starts pw-record: opening the user's microphone is not something a test
// suite gets to do.

// whisper.cpp reads the header, not our intentions. A wrong byte rate or
// block align produces a file that decodes to noise or to nothing, and the
// only symptom would be a transcript that says something the user did not.
func TestWriteWAVProducesACanonicalHeader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clip.wav")
	pcm := []int16{0, -1, 32767, -32768}
	if err := WriteWAV(path, pcm, 16000, 1); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != wavHeaderSize+len(pcm)*2 {
		t.Fatalf("file is %d bytes, want %d", len(raw), wavHeaderSize+len(pcm)*2)
	}
	for _, c := range []struct {
		label  string
		offset int
		want   string
	}{
		{"RIFF", 0, "RIFF"}, {"WAVE", 8, "WAVE"}, {"fmt ", 12, "fmt "}, {"data", 36, "data"},
	} {
		if got := string(raw[c.offset : c.offset+4]); got != c.want {
			t.Errorf("%s marker is %q", c.label, got)
		}
	}
	for _, c := range []struct {
		label  string
		offset int
		want   uint32
	}{
		{"riff size", 4, uint32(wavHeaderSize - 8 + len(pcm)*2)},
		{"fmt chunk size", 16, 16},
		{"sample rate", 24, 16000},
		{"byte rate", 28, 32000},
		{"data size", 40, uint32(len(pcm) * 2)},
	} {
		if got := binary.LittleEndian.Uint32(raw[c.offset : c.offset+4]); got != c.want {
			t.Errorf("%s is %d, want %d", c.label, got, c.want)
		}
	}
	for _, c := range []struct {
		label  string
		offset int
		want   uint16
	}{
		{"format tag (PCM)", 20, 1},
		{"channels", 22, 1},
		{"block align", 32, 2},
		{"bits per sample", 34, 16},
	} {
		if got := binary.LittleEndian.Uint16(raw[c.offset : c.offset+2]); got != c.want {
			t.Errorf("%s is %d, want %d", c.label, got, c.want)
		}
	}
	// Samples are little-endian two's complement, in order.
	want := []byte{0x00, 0x00, 0xff, 0xff, 0xff, 0x7f, 0x00, 0x80}
	if got := raw[wavHeaderSize:]; string(got) != string(want) {
		t.Errorf("samples % x, want % x", got, want)
	}
}

// A capture that turned out to be nothing still has to produce a valid file
// rather than a truncated one: the caller decides whether to transcribe it,
// and it must not have to guess whether the file is intact.
func TestWriteWAVHandlesAnEmptyCapture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.wav")
	if err := WriteWAV(path, nil, 16000, 1); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != wavHeaderSize {
		t.Errorf("an empty capture wrote %d bytes, want a bare %d-byte header", info.Size(), wavHeaderSize)
	}
}

// An impossible format is a programming error, and failing loudly beats
// writing a header that claims zero channels.
func TestWriteWAVRefusesAnImpossibleFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.wav")
	if err := WriteWAV(path, []int16{1}, 0, 1); err == nil {
		t.Error("a zero sample rate was accepted")
	}
	if err := WriteWAV(path, []int16{1}, 16000, 0); err == nil {
		t.Error("a zero channel count was accepted")
	}
}

// SaveClip is the bridge between audio held in memory and the session engine,
// which only knows how to transcribe files. The file it produces has to be
// the shape the engine expects, and readable by nobody else — it holds
// somebody's speech until transcription deletes it.
func TestSaveClipHandsAudioToTheEngineAsARecording(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runtime")
	rec, err := SaveClip(dir, []int16{1, 2, 3, 4})
	if err != nil {
		t.Fatal(err)
	}
	clip, err := rec.Stop()
	if err != nil {
		t.Fatal(err)
	}
	if clip.SampleRate != captureRate || clip.Channels != captureChannels {
		t.Errorf("clip is %d Hz / %d channels, want %d / %d",
			clip.SampleRate, clip.Channels, captureRate, captureChannels)
	}
	if !strings.HasPrefix(filepath.Base(clip.WAVPath), "wake-") {
		t.Errorf("the file is named %q; wake captures should be identifiable", filepath.Base(clip.WAVPath))
	}
	info, err := os.Stat(clip.WAVPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("the capture is mode %o; audio must not be world-readable while it waits", perm)
	}

	// Cancel is the unwanted-capture path: the audio was recorded
	// unavoidably, and stops existing the moment it is not wanted.
	rec.Cancel()
	if _, err := os.Stat(clip.WAVPath); err == nil {
		t.Error("a cancelled capture is still on disk")
	}
}

// The runtime directory is created on demand and only for its owner: it holds
// captured audio, briefly, and tmpfs does not make that anyone else's
// business.
func TestSaveClipCreatesAPrivateRuntimeDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "runtime")
	if _, err := SaveClip(dir, []int16{1}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("the recording directory is mode %o, want 700", perm)
	}
}
