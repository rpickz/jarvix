// Package whispercpp adapts whisper.cpp's whisper-cli as an stt.Transcriber.
//
// The engine runs as a short-lived external process per transcription rather
// than being linked into the daemon (ADR 0002): whisper-cli is packaged for
// Arch, upgrades independently, and a crash in native code cannot take the
// daemon down. The adapter is the only code that knows whisper.cpp exists.
package whispercpp

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/rpickz/jarvix/internal/stt"
)

// KnownModels maps short model names to their ggml file names as distributed
// on Hugging Face (ggerganov/whisper.cpp).
var KnownModels = map[string]string{
	"tiny":           "ggml-tiny.bin",
	"tiny.en":        "ggml-tiny.en.bin",
	"base":           "ggml-base.bin",
	"base.en":        "ggml-base.en.bin",
	"small":          "ggml-small.bin",
	"small.en":       "ggml-small.en.bin",
	"medium":         "ggml-medium.bin",
	"large-v3":       "ggml-large-v3.bin",
	"large-v3-turbo": "ggml-large-v3-turbo.bin",
}

// ModelURL returns the download URL for a known model name.
func ModelURL(name string) (string, bool) {
	file, ok := KnownModels[name]
	if !ok {
		return "", false
	}
	return "https://huggingface.co/ggerganov/whisper.cpp/resolve/main/" + file, true
}

// Transcriber runs whisper-cli against recorded WAV files.
type Transcriber struct {
	Binary    string // whisper-cli executable
	ModelPath string // absolute path to the ggml model file
	Language  string // e.g. "en"; "auto" for detection
	// Prompt is the initial prompt whisper decodes under (--prompt): a
	// vocabulary bias, not a transcript prefix. It exists because "Jarvix" is
	// an out-of-vocabulary word that otherwise rounds to "Jarvis" (issue #83);
	// config.STTBiasPrompt composes it from the wake word and stt.vocabulary.
	// Empty passes no prompt at all.
	Prompt string
}

// ResolveModelPath turns a configured model value into a file path. Absolute
// paths are used as-is; short names resolve inside modelDir.
func ResolveModelPath(model, modelDir string) string {
	if strings.ContainsRune(model, os.PathSeparator) {
		return model
	}
	file, ok := KnownModels[model]
	if !ok {
		// Unknown short name: assume the user placed a matching ggml file.
		file = "ggml-" + model + ".bin"
	}
	return modelDir + string(os.PathSeparator) + file
}

// Name implements stt.Transcriber.
func (t *Transcriber) Name() string { return "whisper.cpp" }

// Transcribe implements stt.Transcriber. whisper.cpp processes the whole
// recording in one pass, so the stream carries a single final event (or an
// error). Partial events will come from a future streaming engine.
func (t *Transcriber) Transcribe(ctx context.Context, input stt.AudioInput) (<-chan stt.TranscriptEvent, error) {
	if _, err := os.Stat(t.ModelPath); err != nil {
		return nil, fmt.Errorf("whisper model not found at %s (run: jarvix setup whisper)", t.ModelPath)
	}
	if _, err := exec.LookPath(t.Binary); err != nil {
		return nil, fmt.Errorf("whisper-cli not found (%q); install whisper.cpp (pacman -S whisper.cpp)", t.Binary)
	}

	args := []string{
		"--model", t.ModelPath,
		"--file", input.WAVPath,
		"--no-timestamps",
		"--no-prints",
	}
	if t.Language != "" {
		args = append(args, "--language", t.Language)
	}
	if t.Prompt != "" {
		args = append(args, "--prompt", t.Prompt)
	}
	cmd := exec.CommandContext(ctx, t.Binary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// Kill the whole process promptly on cancellation.
	cmd.Cancel = func() error { return cmd.Process.Kill() }

	ch := make(chan stt.TranscriptEvent)
	go func() {
		defer close(ch)
		err := cmd.Run()
		if ctx.Err() != nil {
			ch <- stt.TranscriptEvent{Type: stt.EventError, Err: ctx.Err()}
			return
		}
		if err != nil {
			detail := strings.TrimSpace(stderr.String())
			if len(detail) > 300 {
				detail = detail[len(detail)-300:]
			}
			ch <- stt.TranscriptEvent{Type: stt.EventError,
				Err: fmt.Errorf("whisper-cli failed: %w: %s", err, detail)}
			return
		}
		text := strings.TrimSpace(stdout.String())
		ch <- stt.TranscriptEvent{Type: stt.EventFinal, Text: text}
	}()
	return ch, nil
}
