package piper

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/tts"
)

// Piper is never required: the binary is a stub shell script that records its
// argv/stdin and emits scripted PCM, exactly like piper's --output_raw mode.

const piperStub = `#!/bin/sh
printf '%s\n' "$@" > "$PIPER_STUB_DIR/piper.args"
cat > "$PIPER_STUB_DIR/piper.stdin"
printf 'PIPER-PCM-STREAM'
exit "${PIPER_STUB_EXIT:-0}"
`

// installPiperStub creates the stub binary, a voice model with its JSON
// sidecar, and returns a ready Synthesizer plus the stub capture dir.
func installPiperStub(t *testing.T) (*Synthesizer, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "piper-stub")
	if err := os.WriteFile(bin, []byte(piperStub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIPER_STUB_DIR", dir)

	voice := filepath.Join(dir, "en_US-test-medium.onnx")
	if err := os.WriteFile(voice, []byte("onnx"), 0o644); err != nil {
		t.Fatal(err)
	}
	sidecar, _ := json.Marshal(map[string]any{"audio": map[string]any{"sample_rate": 22050}})
	if err := os.WriteFile(voice+".json", sidecar, 0o644); err != nil {
		t.Fatal(err)
	}
	return &Synthesizer{Binary: bin, Voice: voice}, dir
}

func drainChunks(t *testing.T, ch <-chan tts.Chunk) (pcm []byte, streamErr error) {
	t.Helper()
	for c := range ch {
		if c.Err != nil {
			streamErr = c.Err
		}
		pcm = append(pcm, c.PCM...)
	}
	return pcm, streamErr
}

func TestSpeakStreamsPCMWithVoiceFormat(t *testing.T) {
	s, dir := installPiperStub(t)
	format, ch, err := s.Speak(context.Background(), tts.Request{Text: "hello\nthere"})
	if err != nil {
		t.Fatal(err)
	}
	if format.SampleRate != 22050 || format.Channels != 1 {
		t.Errorf("format = %+v, want the sidecar's sample rate", format)
	}
	pcm, streamErr := drainChunks(t, ch)
	if streamErr != nil {
		t.Fatal(streamErr)
	}
	if string(pcm) != "PIPER-PCM-STREAM" {
		t.Errorf("pcm = %q", pcm)
	}

	args, err := os.ReadFile(filepath.Join(dir, "piper.args"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--model", s.modelPath, "--output_raw", "--quiet"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("argv %q missing %q", args, want)
		}
	}
	// Newlines collapse so multi-line text renders as one utterance.
	stdin, err := os.ReadFile(filepath.Join(dir, "piper.stdin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(stdin) != "hello there\n" {
		t.Errorf("stdin = %q", stdin)
	}
}

func TestSpeakRejectsEmptyText(t *testing.T) {
	s, _ := installPiperStub(t)
	if _, _, err := s.Speak(context.Background(), tts.Request{Text: "   "}); err == nil {
		t.Error("blank text must not spawn piper")
	}
}

func TestSpeakMissingBinaryIsActionable(t *testing.T) {
	s, _ := installPiperStub(t)
	s.Binary = filepath.Join(t.TempDir(), "nope")
	_, _, err := s.Speak(context.Background(), tts.Request{Text: "hi"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %v", err)
	}
}

func TestSpeakSurfacesProcessFailure(t *testing.T) {
	s, _ := installPiperStub(t)
	t.Setenv("PIPER_STUB_EXIT", "3")
	_, ch, err := s.Speak(context.Background(), tts.Request{Text: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	_, streamErr := drainChunks(t, ch)
	if streamErr == nil || !strings.Contains(streamErr.Error(), "piper failed") {
		t.Errorf("stream err = %v", streamErr)
	}
}

func TestSpeakCancellationEndsStream(t *testing.T) {
	dir := t.TempDir()
	// This stub emits one chunk then blocks, so cancellation lands
	// mid-stream. exec replaces the shell with the sleeper so the kill on
	// cancellation reaches the long-lived process itself and nothing
	// outlives the test.
	bin := filepath.Join(dir, "piper-stub")
	script := `#!/bin/sh
cat > /dev/null
printf 'FIRST-CHUNK'
exec sleep 60
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	voice := filepath.Join(dir, "v.onnx")
	if err := os.WriteFile(voice, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(voice+".json", []byte(`{"audio":{"sample_rate":22050}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Synthesizer{Binary: bin, Voice: voice}

	ctx, cancel := context.WithCancel(context.Background())
	_, ch, err := s.Speak(ctx, tts.Request{Text: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	first := <-ch // audio flowing: the process is alive
	if first.Err != nil {
		t.Fatal(first.Err)
	}
	cancel()
	_, streamErr := drainChunks(t, ch)
	if !errors.Is(streamErr, context.Canceled) {
		t.Errorf("stream err = %v, want context.Canceled", streamErr)
	}
}
