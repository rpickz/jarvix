package audio

import (
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// Fixtures are generated here rather than committed. A WAV of digital silence
// compresses to nothing and would be a fine file to check in, but the
// interesting cases are the ones *beside* silence — a dither floor, a quiet
// room, a quiet talker — and those are only meaningful if the level that
// produced them is written down in the same place as the assertion.

// tone fills a buffer with uniform noise of the given amplitude, which for a
// uniform distribution has RMS amplitude/sqrt(3). Uniform rather than a sine
// because it is what a noise floor is, and because internal/wake's fixtures
// describe room tone the same way.
func tone(samples int, amplitude int, seed int64) []int16 {
	rng := rand.New(rand.NewSource(seed)) //nolint:gosec // fixtures, not crypto
	out := make([]int16, samples)
	for i := range out {
		out[i] = int16(rng.Intn(2*amplitude+1) - amplitude)
	}
	return out
}

func writeClip(t *testing.T, pcm []int16) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "clip.wav")
	if err := WriteWAV(path, pcm, 16000, 1); err != nil {
		t.Fatal(err)
	}
	return path
}

// Two seconds of digital silence is the reproduction from issue #191: exactly
// what a muted source or the wrong capture device delivers, and exactly what
// whisper turns into "The assistant is called Jarvix."
func TestDigitalSilenceIsNotVoiced(t *testing.T) {
	level, err := MeasureWAV(writeClip(t, make([]int16, 2*16000)))
	if err != nil {
		t.Fatal(err)
	}
	if level.PeakFrameRMS != 0 {
		t.Errorf("peak = %v, want 0 for digital silence", level.PeakFrameRMS)
	}
	if level.Frames != 100 {
		t.Errorf("frames = %d, want 100 (two seconds of 20 ms frames)", level.Frames)
	}
	if level.Voiced() {
		t.Error("digital silence must not count as voiced audio")
	}
}

// Sub-LSB dither is what a live-but-silent capture chain produces instead of
// exact zeros. It must fall on the same side of the gate as silence, or the
// gate answers only the tidiest half of the failure.
func TestDitherFloorIsNotVoiced(t *testing.T) {
	level, err := MeasureWAV(writeClip(t, tone(2*16000, 1, 191)))
	if err != nil {
		t.Fatal(err)
	}
	if level.PeakFrameRMS > SilenceFloorRMS {
		t.Errorf("peak = %.2f, want at or below the %.1f floor", level.PeakFrameRMS, SilenceFloorRMS)
	}
	if level.Voiced() {
		t.Error("a ±1 LSB dither floor must not count as voiced audio")
	}
}

// The safety direction. Room tone alone — nobody speaking, just the fan —
// already sits well above the floor, which is the whole argument that a quiet
// person carrying a room with them is never gated out. ±60 is the amplitude
// internal/wake's fixtures use for exactly this, so the two packages agree on
// what "a quiet room" means.
func TestRoomToneIsVoiced(t *testing.T) {
	level, err := MeasureWAV(writeClip(t, tone(16000, 60, 83)))
	if err != nil {
		t.Fatal(err)
	}
	if !level.Voiced() {
		t.Errorf("room tone at peak %.1f must be transcribed, floor is %.1f",
			level.PeakFrameRMS, SilenceFloorRMS)
	}
	if level.PeakFrameRMS < 4*SilenceFloorRMS {
		t.Errorf("peak = %.1f: room tone should clear the floor by a wide margin, not squeak past it",
			level.PeakFrameRMS)
	}
}

// A single loud frame in an otherwise silent capture is voiced. This is the
// peak-not-mean rule, and it is the reason a short question inside a long
// press is never mistaken for silence.
func TestOneLoudFrameMakesTheCaptureVoiced(t *testing.T) {
	pcm := make([]int16, 5*16000)
	copy(pcm[16000:], tone(320, 4000, 129))
	level, err := MeasureWAV(writeClip(t, pcm))
	if err != nil {
		t.Fatal(err)
	}
	if !level.Voiced() {
		t.Errorf("peak %.1f in five seconds must count as voiced", level.PeakFrameRMS)
	}
}

// A clip shorter than one analysis frame is not evidence of anything.
func TestClipTooShortToMeasureIsVoiced(t *testing.T) {
	level, err := MeasureWAV(writeClip(t, make([]int16, 100)))
	if err != nil {
		t.Fatal(err)
	}
	if level.Frames != 0 {
		t.Errorf("frames = %d, want 0", level.Frames)
	}
	if !level.Voiced() {
		t.Error("a clip too short to measure must be transcribed, not discarded")
	}
}

// pw-record is killed mid-write when a capture is cancelled, so the header
// over-declares the data chunk. The tail that did land still has to be
// measurable: refusing the file would send a real question down the
// no-voice path.
func TestTruncatedDataChunkIsMeasuredNotRefused(t *testing.T) {
	path := writeClip(t, tone(16000, 4000, 7))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cut := filepath.Join(t.TempDir(), "cut.wav")
	if err := os.WriteFile(cut, raw[:len(raw)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	level, err := MeasureWAV(cut)
	if err != nil {
		t.Fatalf("a truncated recording must still measure: %v", err)
	}
	if !level.Voiced() {
		t.Errorf("peak %.1f: the half that survived is speech", level.PeakFrameRMS)
	}
}

// Anything unreadable or unparseable is an error, and the adapter turns an
// error into "transcribe it anyway". The test pins the error rather than the
// policy; internal/stt/whispercpp pins the policy.
func TestUnreadableAndUnparseableClipsError(t *testing.T) {
	if _, err := MeasureWAV(filepath.Join(t.TempDir(), "gone.wav")); err == nil {
		t.Error("a missing recording must report an error, not silence")
	}
	junk := filepath.Join(t.TempDir(), "junk.wav")
	if err := os.WriteFile(junk, []byte("this is not a RIFF file at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := MeasureWAV(junk); err == nil {
		t.Error("a non-WAV file must report an error, not silence")
	}
}

// The floor is a documented number, not an accident. If someone moves it, the
// dBFS figure in the comment above it — and in the reason a user reads — has
// to move with it.
func TestSilenceFloorIsWhereTheCommentSaysItIs(t *testing.T) {
	if got := DBFS(SilenceFloorRMS); math.Abs(got-(-72.2)) > 0.1 {
		t.Errorf("SilenceFloorRMS = %.1f is %.1f dBFS, but the reasoning is written for -72.2",
			SilenceFloorRMS, got)
	}
	if got := DBFS(0); !math.IsInf(got, -1) {
		t.Errorf("DBFS(0) = %v, want -Inf", got)
	}
}
