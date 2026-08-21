package kokoro

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/tts"
)

// Kokoro is never required. The helper is a shell stub speaking both halves of
// kokoro_stream.py's contract — the one-shot protocol and the serve protocol —
// chosen by --serve exactly as the real script chooses, so the adapter's warm
// path, its cold fallback, and the switch between them are all pinned against
// the wire format rather than against Python.
//
// KOKORO_STUB_MODE selects the serve behaviour under test:
//
//	normal     one chunk then END, per utterance
//	abortable  utterance 1 emits one chunk then waits for a command: ABORT →
//	           ABORTED. Later utterances behave normally, so the test can show
//	           the same worker still answering after an interruption.
//	crash      exit on the first SPEAK, mid-conversation
//	oldscript  a helper predating the protocol: it rejects --serve
const kokoroDualStub = `#!/bin/sh
case " $* " in
  *" --serve "*)
    echo SERVE >> "$KOKORO_STUB_DIR/spawns"
    echo "$*" >> "$KOKORO_STUB_DIR/serve.args"
    if [ "$KOKORO_STUB_MODE" = "oldscript" ]; then
      echo "unrecognized arguments: --serve" >&2
      exit 2
    fi
    printf 'READY 1 24000\n'
    while IFS= read -r line; do
      verb=${line%% *}
      rest=${line#* }
      id=${rest%% *}
      case "$verb" in
        SPEAK)
          if [ "$KOKORO_STUB_MODE" = "crash" ]; then exit 9; fi
          printf 'CHUNK %s 4\n' "$id"
          printf 'PCM1'
          if [ "$KOKORO_STUB_MODE" = "abortable" ] && [ "$id" = "1" ]; then
            if IFS= read -r next; then
              case "$next" in
                "ABORT $id") printf 'ABORTED %s\n' "$id" ;;
                *) printf 'END %s\n' "$id" ;;
              esac
            fi
          else
            printf 'END %s\n' "$id"
          fi
          ;;
        QUIT) exit 0 ;;
      esac
    done
    ;;
  *)
    echo ONESHOT >> "$KOKORO_STUB_DIR/spawns"
    echo "$*" >> "$KOKORO_STUB_DIR/oneshot.args"
    cat > /dev/null
    echo "SAMPLE_RATE=24000" >&2
    printf 'COLD'
    ;;
esac
`

func warmTestLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// installWarmStub builds a WarmSynthesizer over the dual-mode stub and returns
// the directory the stub records its spawns in.
func installWarmStub(t *testing.T, mode string) (*WarmSynthesizer, string) {
	t.Helper()
	dir := t.TempDir()
	helper := filepath.Join(dir, "python-stub")
	if err := os.WriteFile(helper, []byte(kokoroDualStub), 0o755); err != nil {
		t.Fatal(err)
	}
	// The helper fixture declares --lang, as the real script does: the adapter
	// reads the installed script to decide whether it may pass the flag.
	for name, body := range map[string]string{
		"kokoro_stream.py": `parser.add_argument("--lang")`,
		"model.onnx":       "x",
		"voices.bin":       "x",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("KOKORO_STUB_DIR", dir)
	t.Setenv("KOKORO_STUB_MODE", mode)

	w := &WarmSynthesizer{
		Cold: &Synthesizer{
			Python:     helper,
			Script:     filepath.Join(dir, "kokoro_stream.py"),
			ModelPath:  filepath.Join(dir, "model.onnx"),
			VoicesPath: filepath.Join(dir, "voices.bin"),
		},
		Log:          warmTestLogger(),
		StartTimeout: 10 * time.Second,
	}
	t.Cleanup(func() { _ = w.Close() })
	return w, dir
}

// spawns reports how many helpers were started, in each mode.
func spawns(t *testing.T, dir string) (serve, oneShot int) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "spawns"))
	if os.IsNotExist(err) {
		return 0, 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "SERVE"), strings.Count(string(data), "ONESHOT")
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

func TestWarmSpeakStreamsPCMAndKeepsTheHelperBetweenUtterances(t *testing.T) {
	w, dir := installWarmStub(t, "normal")

	for i := range 3 {
		format, ch, err := w.Speak(context.Background(), tts.Request{Text: "hello there"})
		if err != nil {
			t.Fatal(err)
		}
		if format.SampleRate != 24000 || format.Channels != 1 {
			t.Errorf("format = %+v, want the helper's announced rate", format)
		}
		pcm, streamErr := drainWarm(t, ch)
		if streamErr != nil {
			t.Fatal(streamErr)
		}
		if string(pcm) != "PCM1" {
			t.Fatalf("utterance %d pcm = %q", i, pcm)
		}
	}
	// The whole point: three utterances, one interpreter, one model load.
	serve, oneShot := spawns(t, dir)
	if serve != 1 || oneShot != 0 {
		t.Errorf("spawns = %d serve / %d one-shot, want 1/0 — Python would boot per sentence", serve, oneShot)
	}
}

func TestWarmSpeakCollapsesNewlinesIntoOneUtteranceLine(t *testing.T) {
	w, _ := installWarmStub(t, "normal")
	_, ch, err := w.Speak(context.Background(), tts.Request{Text: "first\nsecond\n\nthird"})
	if err != nil {
		t.Fatal(err)
	}
	// A multi-line utterance would desynchronise the line protocol: the helper
	// would read "second" as a command and answer nothing.
	if _, streamErr := drainWarm(t, ch); streamErr != nil {
		t.Fatalf("multi-line text broke the protocol: %v", streamErr)
	}
}

func TestWarmSpeakAbortsMidUtteranceAndKeepsTheWorkerWarm(t *testing.T) {
	w, dir := installWarmStub(t, "abortable")

	ctx, cancel := context.WithCancel(context.Background())
	_, ch, err := w.Speak(ctx, tts.Request{Text: "a long sentence"})
	if err != nil {
		t.Fatal(err)
	}
	first := <-ch // audio flowing: the helper is mid-utterance
	if first.Err != nil || string(first.PCM) != "PCM1" {
		t.Fatalf("first chunk = %+v", first)
	}
	pidBefore := w.WarmStatus().PID

	cancel()
	if _, streamErr := drainWarm(t, ch); !errors.Is(streamErr, context.Canceled) {
		t.Errorf("stream err = %v, want context.Canceled", streamErr)
	}

	// The interruption must not have cost the model load: the same process
	// answers the next question.
	_, next, err := w.Speak(context.Background(), tts.Request{Text: "next question"})
	if err != nil {
		t.Fatal(err)
	}
	if _, streamErr := drainWarm(t, next); streamErr != nil {
		t.Fatal(streamErr)
	}
	if got := w.WarmStatus().PID; got != pidBefore {
		t.Errorf("helper pid changed from %d to %d — abort killed the worker", pidBefore, got)
	}
	if serve, _ := spawns(t, dir); serve != 1 {
		t.Errorf("serve spawns = %d, want 1: abort must not respawn the helper", serve)
	}
}

func TestWarmHelperCrashFallsBackColdAndCountsARestart(t *testing.T) {
	w, dir := installWarmStub(t, "crash")

	_, ch, err := w.Speak(context.Background(), tts.Request{Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	pcm, streamErr := drainWarm(t, ch)
	if streamErr != nil {
		t.Fatalf("a dead warm helper must not fail the session: %v", streamErr)
	}
	if string(pcm) != "COLD" {
		t.Errorf("pcm = %q, want the one-shot helper's output", pcm)
	}
	if got := w.WarmStatus().Restarts; got != 1 {
		t.Errorf("restarts = %d, want 1", got)
	}
	serve, oneShot := spawns(t, dir)
	if serve != 1 || oneShot != 1 {
		t.Errorf("spawns = %d serve / %d one-shot, want 1/1", serve, oneShot)
	}
}

func TestWarmSpeakFallsBackWhenTheHelperPredatesTheProtocol(t *testing.T) {
	w, dir := installWarmStub(t, "oldscript")

	_, ch, err := w.Speak(context.Background(), tts.Request{Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	pcm, streamErr := drainWarm(t, ch)
	if streamErr != nil {
		t.Fatal(streamErr)
	}
	if string(pcm) != "COLD" {
		t.Errorf("pcm = %q: an old kokoro_stream.py must degrade to one-shot, not hang", pcm)
	}
	if _, oneShot := spawns(t, dir); oneShot != 1 {
		t.Errorf("one-shot spawns = %d, want 1", oneShot)
	}
}

func TestWarmCloseKillsTheHelper(t *testing.T) {
	w, _ := installWarmStub(t, "normal")
	_, ch, err := w.Speak(context.Background(), tts.Request{Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if _, streamErr := drainWarm(t, ch); streamErr != nil {
		t.Fatal(streamErr)
	}
	pid := w.WarmStatus().PID
	if pid == 0 {
		t.Fatal("no warm helper to kill")
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	awaitExit(t, pid)
}

// awaitExit polls until a pid is gone. Polling on the real outcome, rather
// than sleeping a guessed interval, is what keeps this deterministic.
func awaitExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("helper (pid %d) is still running — jarvixd would leave an orphan", pid)
}

func TestWarmSpeakRejectsEmptyTextWithoutTouchingTheHelper(t *testing.T) {
	w, dir := installWarmStub(t, "normal")
	if _, _, err := w.Speak(context.Background(), tts.Request{Text: "   "}); err == nil {
		t.Error("blank text must not reach the helper")
	}
	if serve, oneShot := spawns(t, dir); serve != 0 || oneShot != 0 {
		t.Errorf("spawns = %d/%d, want none", serve, oneShot)
	}
}

func TestWarmSpeakSerialisesConcurrentUtterances(t *testing.T) {
	w, dir := installWarmStub(t, "normal")
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ch, err := w.Speak(context.Background(), tts.Request{Text: "concurrent"})
			if err != nil {
				return
			}
			pcm, streamErr := drainWarm(t, ch)
			if streamErr == nil && string(pcm) != "PCM1" {
				t.Errorf("frames interleaved between utterances: pcm = %q", pcm)
			}
		}()
	}
	wg.Wait()
	if serve, _ := spawns(t, dir); serve != 1 {
		t.Errorf("serve spawns = %d, want 1", serve)
	}
}

func TestReadHeaderRejectsGarbage(t *testing.T) {
	for _, line := range []string{"CHUNK 1 notanumber\n", "WAT 1\n", "CHUNK 1 999999999999\n"} {
		r := bufio.NewReader(strings.NewReader(line))
		if _, _, _, _, err := readHeader(r); err == nil {
			t.Errorf("readHeader(%q) accepted a malformed frame", line)
		}
	}
}
