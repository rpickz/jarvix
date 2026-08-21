package piper

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/tts"
)

// piper is never required. The stub speaks both halves of the real CLI's
// contract — --output_raw for the cold path, --output_dir for the warm one —
// chosen by argv exactly as piper does, so the adapter is pinned against
// piper's own protocol.
//
// PIPER_STUB_MODE selects the warm behaviour:
//
//	normal  one WAV per input line, path printed on stdout
//	slow    the first utterance is answered, later ones never arrive (a worker
//	        that wedges once it is already warm)
//	crash   exit on the first utterance
const piperDualStub = `#!/bin/sh
outdir=""
raw=""
prev=""
for arg in "$@"; do
  case "$prev" in --output_dir) outdir="$arg" ;; esac
  case "$arg" in --output_raw) raw=1 ;; esac
  prev="$arg"
done
if [ -n "$raw" ]; then
  echo COLD >> "$PIPER_STUB_DIR/spawns"
  cat > /dev/null
  printf 'COLD-PCM-STREAM'
  exit 0
fi
echo WARM >> "$PIPER_STUB_DIR/spawns"
n=0
while IFS= read -r line; do
  n=$((n+1))
  if [ "$PIPER_STUB_MODE" = "crash" ]; then exit 9; fi
  if [ "$PIPER_STUB_MODE" = "slow" ] && [ "$n" != "1" ]; then
    # A worker that never finishes the utterance it was given.
    while IFS= read -r _; do :; done
    exit 0
  fi
  out="$outdir/utt-$n.wav"
  printf '%s' "$line" > "$outdir/utt-$n.txt"
  # A minimal RIFF/WAVE file whose data chunk is the eight bytes "WARMPCM1".
  printf 'RIFF' > "$out"
  printf '\054\000\000\000WAVEfmt \020\000\000\000\001\000\001\000\102\254\000\000\104\254\000\000\002\000\020\000data\010\000\000\000WARMPCM1' >> "$out"
  printf '%s\n' "$out"
done
`

func warmTestLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// installWarmStub builds a WarmSynthesizer over the dual-mode stub plus a
// voice fixture, and returns the directory the stub records its spawns in.
func installWarmStub(t *testing.T, mode string) (*WarmSynthesizer, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "piper-stub")
	if err := os.WriteFile(bin, []byte(piperDualStub), 0o755); err != nil {
		t.Fatal(err)
	}
	voice := filepath.Join(dir, "en_US-test-medium.onnx")
	if err := os.WriteFile(voice, []byte("onnx"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(voice+".json", []byte(`{"audio":{"sample_rate":22050}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIPER_STUB_DIR", dir)
	t.Setenv("PIPER_STUB_MODE", mode)

	w := &WarmSynthesizer{
		Cold:       &Synthesizer{Binary: bin, Voice: voice},
		Dir:        dir,
		Log:        warmTestLogger(),
		AbortDrain: 200 * time.Millisecond,
	}
	t.Cleanup(func() { _ = w.Close() })
	return w, dir
}

func spawns(t *testing.T, dir string) (warmStarts, coldStarts int) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "spawns"))
	if os.IsNotExist(err) {
		return 0, 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "WARM"), strings.Count(string(data), "COLD")
}

func drainWarm(t *testing.T, ch <-chan tts.Chunk) (pcm []byte, streamErr error) {
	t.Helper()
	for c := range ch {
		if c.Err != nil {
			streamErr = c.Err
		}
		pcm = append(pcm, c.PCM...)
	}
	return pcm, streamErr
}

func TestWarmSpeakReusesOnePiperAcrossResponses(t *testing.T) {
	w, dir := installWarmStub(t, "normal")

	for i := range 3 {
		format, ch, err := w.Speak(context.Background(), tts.Request{Text: "hello there"})
		if err != nil {
			t.Fatal(err)
		}
		if format.SampleRate != 22050 || format.Channels != 1 {
			t.Errorf("format = %+v, want the sidecar's rate", format)
		}
		pcm, streamErr := drainWarm(t, ch)
		if streamErr != nil {
			t.Fatal(streamErr)
		}
		if string(pcm) != "WARMPCM1" {
			t.Fatalf("response %d pcm = %q", i, pcm)
		}
	}
	warmStarts, coldStarts := spawns(t, dir)
	if warmStarts != 1 || coldStarts != 0 {
		t.Errorf("spawns = %d warm / %d cold, want 1/0 — the voice would reload per response",
			warmStarts, coldStarts)
	}
}

func TestWarmSpeakSendsOneLinePerUtterance(t *testing.T) {
	w, dir := installWarmStub(t, "normal")
	_, ch, err := w.Speak(context.Background(), tts.Request{Text: "first\nsecond"})
	if err != nil {
		t.Fatal(err)
	}
	if _, streamErr := drainWarm(t, ch); streamErr != nil {
		t.Fatal(streamErr)
	}
	// Newlines must collapse: piper treats each line as its own utterance, so
	// a two-line request would leave a reply nobody reads in the pipe and
	// desynchronise every response after it.
	// The stub records each utterance next to the WAV it wrote, inside the
	// worker's scratch directory.
	written, err := filepath.Glob(filepath.Join(dir, "jarvix-piper-*", "utt-1.txt"))
	if err != nil || len(written) != 1 {
		t.Fatalf("utterance file not found: %v %v", written, err)
	}
	text, err := os.ReadFile(written[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(text) != "first second" {
		t.Errorf("utterance = %q, want the lines collapsed", text)
	}
}

func TestWarmSpeakCancelledBeforeAudioKeepsTheWorker(t *testing.T) {
	w, dir := installWarmStub(t, "normal")

	// Warm the worker so the cancellation lands on an established process.
	_, ch, err := w.Speak(context.Background(), tts.Request{Text: "warm up"})
	if err != nil {
		t.Fatal(err)
	}
	if _, streamErr := drainWarm(t, ch); streamErr != nil {
		t.Fatal(streamErr)
	}
	pidBefore := w.WarmStatus().PID

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // interrupted before piper produced anything
	_, ch, err = w.Speak(ctx, tts.Request{Text: "interrupted"})
	if err != nil {
		t.Fatal(err)
	}
	pcm, streamErr := drainWarm(t, ch)
	if !errors.Is(streamErr, context.Canceled) {
		t.Errorf("stream err = %v, want context.Canceled", streamErr)
	}
	if len(pcm) != 0 {
		t.Errorf("a cancelled utterance emitted %d bytes; silence must be immediate", len(pcm))
	}

	// The abandoned utterance is drained in the background, so the next
	// response comes from the same warm worker.
	_, ch, err = w.Speak(context.Background(), tts.Request{Text: "next"})
	if err != nil {
		t.Fatal(err)
	}
	if _, streamErr := drainWarm(t, ch); streamErr != nil {
		t.Fatal(streamErr)
	}
	if got := w.WarmStatus().PID; got != pidBefore {
		t.Errorf("worker pid changed from %d to %d — cancellation killed it", pidBefore, got)
	}
	if warmStarts, _ := spawns(t, dir); warmStarts != 1 {
		t.Errorf("warm spawns = %d, want 1", warmStarts)
	}
	// Nothing of the abandoned utterance is left on disk.
	leftovers, _ := filepath.Glob(filepath.Join(dir, "jarvix-piper-*", "*.wav"))
	if len(leftovers) != 0 {
		t.Errorf("abandoned WAVs left behind: %v", leftovers)
	}
}

func TestWarmWorkerCrashFallsBackToAFreshProcess(t *testing.T) {
	w, dir := installWarmStub(t, "crash")

	_, ch, err := w.Speak(context.Background(), tts.Request{Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	pcm, streamErr := drainWarm(t, ch)
	if streamErr != nil {
		t.Fatalf("a dead warm worker must not fail the session: %v", streamErr)
	}
	if string(pcm) != "COLD-PCM-STREAM" {
		t.Errorf("pcm = %q, want the fresh process's output", pcm)
	}
	if got := w.WarmStatus().Restarts; got != 1 {
		t.Errorf("restarts = %d, want 1", got)
	}
	warmStarts, coldStarts := spawns(t, dir)
	if warmStarts != 1 || coldStarts != 1 {
		t.Errorf("spawns = %d warm / %d cold, want 1/1", warmStarts, coldStarts)
	}
}

func TestWedgedWorkerIsReplacedAfterTheAbortDeadline(t *testing.T) {
	w, _ := installWarmStub(t, "slow")

	// Warm the worker first: the wedge only means anything once a live process
	// is holding the model.
	_, ch, err := w.Speak(context.Background(), tts.Request{Text: "warm up"})
	if err != nil {
		t.Fatal(err)
	}
	if _, streamErr := drainWarm(t, ch); streamErr != nil {
		t.Fatal(streamErr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, ch, err = w.Speak(ctx, tts.Request{Text: "never finishes"})
	if err != nil {
		t.Fatal(err)
	}
	if _, streamErr := drainWarm(t, ch); !errors.Is(streamErr, context.Canceled) {
		t.Fatalf("stream err = %v", streamErr)
	}
	// piper cannot be told to stop, so a worker that never delivers the
	// abandoned utterance is killed and respawned — the documented fallback.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if w.WarmStatus().Restarts == 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("a wedged worker was never retired (status %+v)", w.WarmStatus())
}

func TestWarmCloseKillsTheWorkerAndRemovesItsScratchDirectory(t *testing.T) {
	w, dir := installWarmStub(t, "normal")
	_, ch, err := w.Speak(context.Background(), tts.Request{Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if _, streamErr := drainWarm(t, ch); streamErr != nil {
		t.Fatal(streamErr)
	}
	pid := w.WarmStatus().PID
	if pid == 0 {
		t.Fatal("no warm worker to kill")
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	// Asserted the instant Close returns, with no grace loop: Close waits for
	// the teardown it started, so "closed" is a fact on return rather than
	// something that becomes true shortly afterwards. A retry loop here would
	// have hidden the defect this test exists for — the scratch check below
	// never had one, which is why it was the half that flaked under load.
	if syscall.Kill(pid, 0) == nil {
		t.Fatalf("worker (pid %d) survived Close — jarvixd would leave an orphan", pid)
	}
	scratch, _ := filepath.Glob(filepath.Join(dir, "jarvix-piper-*"))
	if len(scratch) != 0 {
		t.Errorf("scratch directories left behind: %v", scratch)
	}
}

func TestWarmSpeakRejectsEmptyTextWithoutStartingAWorker(t *testing.T) {
	w, dir := installWarmStub(t, "normal")
	if _, _, err := w.Speak(context.Background(), tts.Request{Text: "  "}); err == nil {
		t.Error("blank text must not reach piper")
	}
	if warmStarts, coldStarts := spawns(t, dir); warmStarts != 0 || coldStarts != 0 {
		t.Errorf("spawns = %d/%d, want none", warmStarts, coldStarts)
	}
}

func TestReadWAVDataWalksChunks(t *testing.T) {
	dir := t.TempDir()
	// A LIST chunk before the data chunk: a fixed 44-byte skip would render it
	// as noise, which is why the reader parses instead of assuming.
	path := filepath.Join(dir, "with-list.wav")
	var body []byte
	body = append(body, []byte("RIFF")...)
	body = append(body, 0, 0, 0, 0)
	body = append(body, []byte("WAVE")...)
	chunk := func(id string, payload []byte) {
		body = append(body, []byte(id)...)
		size := make([]byte, 4)
		binary.LittleEndian.PutUint32(size, uint32(len(payload)))
		body = append(body, size...)
		body = append(body, payload...)
	}
	chunk("LIST", []byte("INFOxxxx"))
	chunk("data", []byte("PAYLOAD!"))
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	pcm, err := readWAVData(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(pcm) != "PAYLOAD!" {
		t.Errorf("pcm = %q", pcm)
	}

	bad := filepath.Join(dir, "not-a-wav")
	if err := os.WriteFile(bad, []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readWAVData(bad); err == nil {
		t.Error("a non-WAVE file must be reported, not played")
	}
}
