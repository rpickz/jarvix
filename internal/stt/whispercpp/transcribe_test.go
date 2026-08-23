package whispercpp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/stt"
)

// whisper.cpp is never required: whisper-cli is a stub script that records
// its argv and prints a scripted transcript.

const whisperStub = `#!/bin/sh
printf '%s\n' "$@" > "$WHISPER_STUB_DIR/whisper.args"
if [ -n "$WHISPER_STUB_FAIL" ]; then
  echo "ggml backend blew up" >&2
  exit 1
fi
printf '  scripted transcript  \n'
`

func installWhisperStub(t *testing.T) (*Transcriber, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "whisper-cli-stub")
	if err := os.WriteFile(bin, []byte(whisperStub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WHISPER_STUB_DIR", dir)
	model := filepath.Join(dir, "ggml-base.en.bin")
	if err := os.WriteFile(model, []byte("model"), 0o644); err != nil {
		t.Fatal(err)
	}
	return &Transcriber{Binary: bin, ModelPath: model, Language: "en"}, dir
}

func TestTranscribeEmitsTrimmedFinalTranscript(t *testing.T) {
	tr, dir := installWhisperStub(t)
	ch, err := tr.Transcribe(context.Background(), stt.AudioInput{WAVPath: "/tmp/rec.wav"})
	if err != nil {
		t.Fatal(err)
	}
	var events []stt.TranscriptEvent
	for ev := range ch {
		events = append(events, ev)
	}
	if len(events) != 1 || events[0].Type != stt.EventFinal || events[0].Text != "scripted transcript" {
		t.Fatalf("events = %+v", events)
	}

	args, err := os.ReadFile(filepath.Join(dir, "whisper.args"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--model", tr.ModelPath, "--file", "/tmp/rec.wav",
		"--no-timestamps", "--no-prints", "--language", "en"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("argv %q missing %q", args, want)
		}
	}
}

// The bias prompt is the cold half of issue #83: without it, "Jarvix" is
// out-of-vocabulary and whisper rounds it to "Jarvis". The exact argv matters —
// --prompt is the flag whisper-cli documents for an initial prompt.
func TestTranscribeCarriesTheBiasPrompt(t *testing.T) {
	tr, dir := installWhisperStub(t)
	tr.Prompt = "The assistant is called Jarvix."
	ch, err := tr.Transcribe(context.Background(), stt.AudioInput{WAVPath: "/tmp/rec.wav"})
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	args, err := os.ReadFile(filepath.Join(dir, "whisper.args"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "--prompt\nThe assistant is called Jarvix.") {
		t.Errorf("argv %q missing --prompt with the bias text", args)
	}
}

func TestTranscribeOmitsThePromptWhenUnset(t *testing.T) {
	tr, dir := installWhisperStub(t)
	ch, err := tr.Transcribe(context.Background(), stt.AudioInput{WAVPath: "/tmp/rec.wav"})
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	args, _ := os.ReadFile(filepath.Join(dir, "whisper.args"))
	if strings.Contains(string(args), "--prompt") {
		t.Errorf("argv %q must not include --prompt when no bias is configured", args)
	}
}

func TestTranscribeOmitsLanguageWhenUnset(t *testing.T) {
	tr, dir := installWhisperStub(t)
	tr.Language = ""
	ch, err := tr.Transcribe(context.Background(), stt.AudioInput{WAVPath: "/tmp/rec.wav"})
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	args, _ := os.ReadFile(filepath.Join(dir, "whisper.args"))
	if strings.Contains(string(args), "--language") {
		t.Errorf("argv %q must not include --language", args)
	}
}

func TestTranscribeFailureCarriesStderrDetail(t *testing.T) {
	tr, _ := installWhisperStub(t)
	t.Setenv("WHISPER_STUB_FAIL", "1")
	ch, err := tr.Transcribe(context.Background(), stt.AudioInput{WAVPath: "/tmp/rec.wav"})
	if err != nil {
		t.Fatal(err)
	}
	var last stt.TranscriptEvent
	for ev := range ch {
		last = ev
	}
	if last.Type != stt.EventError || last.Err == nil {
		t.Fatalf("last = %+v, want an error event", last)
	}
	msg := last.Err.Error()
	if !strings.Contains(msg, "whisper-cli failed") || !strings.Contains(msg, "ggml backend blew up") {
		t.Errorf("err = %q, want the stderr detail", msg)
	}
}

func TestTranscribeCancellationReportsContextError(t *testing.T) {
	tr, _ := installWhisperStub(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ch, err := tr.Transcribe(ctx, stt.AudioInput{WAVPath: "/tmp/rec.wav"})
	if err != nil {
		t.Fatal(err)
	}
	var last stt.TranscriptEvent
	for ev := range ch {
		last = ev
	}
	if last.Type != stt.EventError || !errors.Is(last.Err, context.Canceled) {
		t.Fatalf("last = %+v, want a cancellation error", last)
	}
}

func TestTranscribeMissingBinaryIsActionable(t *testing.T) {
	tr, _ := installWhisperStub(t)
	tr.Binary = filepath.Join(t.TempDir(), "nope")
	_, err := tr.Transcribe(context.Background(), stt.AudioInput{WAVPath: "/tmp/rec.wav"})
	if err == nil || !strings.Contains(err.Error(), "whisper-cli not found") {
		t.Errorf("err = %v", err)
	}
}
