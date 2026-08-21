package kokoro

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/tts"
)

// Kokoro (and its Python venv) is never required: the "interpreter" is a stub
// script speaking the helper protocol — SAMPLE_RATE on stderr, PCM on stdout.

const kokoroStub = `#!/bin/sh
printf '%s\n' "$@" > "$KOKORO_STUB_DIR/kokoro.args"
cat > /dev/null
if [ -n "$KOKORO_STUB_SILENT" ]; then
  echo "Traceback: kokoro_onnx not installed" >&2
  exit 1
fi
echo "loading model..." >&2
echo "SAMPLE_RATE=24000" >&2
printf 'KOKORO-PCM'
exit "${KOKORO_STUB_EXIT:-0}"
`

func installKokoroStub(t *testing.T) (*Synthesizer, string) {
	t.Helper()
	dir := t.TempDir()
	python := filepath.Join(dir, "python-stub")
	if err := os.WriteFile(python, []byte(kokoroStub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KOKORO_STUB_DIR", dir)
	mkfile := func(name string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	return &Synthesizer{
		Python:     python,
		Script:     mkfile("kokoro_stream.py"),
		ModelPath:  mkfile("model.onnx"),
		VoicesPath: mkfile("voices.bin"),
	}, dir
}

func TestSpeakStreamsPCMAtReportedRate(t *testing.T) {
	s, dir := installKokoroStub(t)
	format, ch, err := s.Speak(context.Background(), tts.Request{Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	// The rate comes from the helper's SAMPLE_RATE line, never hardcoded.
	if format.SampleRate != 24000 || format.Channels != 1 {
		t.Errorf("format = %+v", format)
	}
	var pcm []byte
	for c := range ch {
		if c.Err != nil {
			t.Fatal(c.Err)
		}
		pcm = append(pcm, c.PCM...)
	}
	if string(pcm) != "KOKORO-PCM" {
		t.Errorf("pcm = %q", pcm)
	}
	args, err := os.ReadFile(filepath.Join(dir, "kokoro.args"))
	if err != nil {
		t.Fatal(err)
	}
	// Defaults apply when voice/speed are unset.
	for _, want := range []string{s.Script, "--voice", "af_heart", "--speed", "1.00"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("argv %q missing %q", args, want)
		}
	}
}

func TestSpeakPassesConfiguredVoiceAndSpeed(t *testing.T) {
	s, dir := installKokoroStub(t)
	s.Voice = "af_bella"
	s.Speed = 1.5
	_, ch, err := s.Speak(context.Background(), tts.Request{Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	args, _ := os.ReadFile(filepath.Join(dir, "kokoro.args"))
	for _, want := range []string{"af_bella", "1.50"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("argv %q missing %q", args, want)
		}
	}
}

func TestSpeakHelperDyingBeforeAudioIsActionable(t *testing.T) {
	s, _ := installKokoroStub(t)
	t.Setenv("KOKORO_STUB_SILENT", "1")
	_, _, err := s.Speak(context.Background(), tts.Request{Text: "hello"})
	if err == nil || !strings.Contains(err.Error(), "helper exited before producing audio") {
		t.Errorf("err = %v", err)
	}
}

func TestSpeakSurfacesHelperFailure(t *testing.T) {
	s, _ := installKokoroStub(t)
	t.Setenv("KOKORO_STUB_EXIT", "2")
	_, ch, err := s.Speak(context.Background(), tts.Request{Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	var streamErr error
	for c := range ch {
		if c.Err != nil {
			streamErr = c.Err
		}
	}
	if streamErr == nil || !strings.Contains(streamErr.Error(), "kokoro helper failed") {
		t.Errorf("stream err = %v", streamErr)
	}
}

func TestSpeakRejectsEmptyText(t *testing.T) {
	s, _ := installKokoroStub(t)
	if _, _, err := s.Speak(context.Background(), tts.Request{Text: " \n "}); err == nil {
		t.Error("blank text must not spawn the helper")
	}
}

func TestSpeakRefusesWhenNotReady(t *testing.T) {
	s, _ := installKokoroStub(t)
	s.ModelPath = filepath.Join(t.TempDir(), "missing.onnx")
	_, _, err := s.Speak(context.Background(), tts.Request{Text: "hello"})
	if err == nil || !strings.Contains(err.Error(), "setup-kokoro") {
		t.Errorf("err = %v", err)
	}
}
