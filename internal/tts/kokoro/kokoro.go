// Package kokoro adapts the Kokoro neural TTS engine as a tts.Synthesizer.
//
// Kokoro runs through a small Python helper (tts/kokoro/kokoro_stream.py) in
// a dedicated venv: text in on stdin, streamed s16le PCM out on stdout. Like
// the Piper adapter, the process is short-lived per utterance and killing it
// implements cancellation, so interrupting Jarvix mid-sentence stays instant.
// Kokoro's voice is markedly more natural than Piper's, at the cost of a
// heavier (ONNX + Python) setup — hence Piper remains the zero-setup default.
package kokoro

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/rpickz/jarvix/internal/tts"
)

// Default install locations, matching scripts/setup-kokoro.sh.
func defaultDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "jarvix")
}

// Synthesizer runs the Kokoro helper for each utterance.
type Synthesizer struct {
	// Python is the interpreter with kokoro-onnx installed. Empty uses the
	// venv at ~/.local/share/jarvix/kokoro-venv.
	Python string
	// Script is the helper path. Empty uses the installed default.
	Script string
	// ModelPath / VoicesPath point at the ONNX model and voices bin. Empty
	// uses the defaults under ~/.local/share/jarvix/models/kokoro.
	ModelPath  string
	VoicesPath string
	// Voice is the Kokoro voice id (e.g. "af_heart"). Empty uses af_heart.
	Voice string
	// Speed is the speech rate multiplier. Zero means 1.0.
	Speed float64
}

func (s *Synthesizer) python() string {
	if s.Python != "" {
		return s.Python
	}
	return filepath.Join(defaultDir(), "kokoro-venv", "bin", "python")
}

func (s *Synthesizer) script() string {
	if s.Script != "" {
		return s.Script
	}
	return filepath.Join(defaultDir(), "kokoro_stream.py")
}

func (s *Synthesizer) modelPath() string {
	if s.ModelPath != "" {
		return s.ModelPath
	}
	return filepath.Join(defaultDir(), "models", "kokoro", "kokoro-v1.0.onnx")
}

func (s *Synthesizer) voicesPath() string {
	if s.VoicesPath != "" {
		return s.VoicesPath
	}
	return filepath.Join(defaultDir(), "models", "kokoro", "voices-v1.0.bin")
}

// Name implements tts.Synthesizer.
func (s *Synthesizer) Name() string { return "kokoro" }

// Ready reports whether the interpreter, script, and model files exist, so
// jarvix doctor can explain a missing setup before a session needs speech.
func (s *Synthesizer) Ready() error {
	for label, path := range map[string]string{
		"python interpreter": s.python(),
		"helper script":      s.script(),
		"model":              s.modelPath(),
		"voices":             s.voicesPath(),
	} {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("kokoro %s missing at %s (run: scripts/setup-kokoro.sh)", label, path)
		}
	}
	return nil
}

// Speak implements tts.Synthesizer.
func (s *Synthesizer) Speak(ctx context.Context, req tts.Request) (tts.Format, <-chan tts.Chunk, error) {
	if err := s.Ready(); err != nil {
		return tts.Format{}, nil, err
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		return tts.Format{}, nil, fmt.Errorf("nothing to speak")
	}
	voice := s.Voice
	if voice == "" {
		voice = "af_heart"
	}
	speed := s.Speed
	if speed <= 0 {
		speed = 1.0
	}

	cmd := exec.CommandContext(ctx, s.python(), s.script(),
		"--voice", voice, "--speed", strconv.FormatFloat(speed, 'f', 2, 64))
	cmd.Cancel = func() error { return cmd.Process.Kill() }
	cmd.Env = append(os.Environ(),
		"JARVIX_KOKORO_MODEL="+s.modelPath(),
		"JARVIX_KOKORO_VOICES="+s.voicesPath(),
	)
	cmd.Stdin = strings.NewReader(strings.ReplaceAll(text, "\n", " ") + "\n")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return tts.Format{}, nil, err
	}
	// The helper prints SAMPLE_RATE=NNNN on stderr before audio; read it so
	// playback is configured from the engine, never hardcoded here.
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return tts.Format{}, nil, err
	}
	if err := cmd.Start(); err != nil {
		return tts.Format{}, nil, fmt.Errorf("start kokoro helper: %w", err)
	}

	sampleRate, rateErr := readSampleRate(stderr)
	if rateErr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return tts.Format{}, nil, fmt.Errorf("kokoro: %w", rateErr)
	}
	go drain(stderr) // keep the pipe from filling and blocking the helper

	format := tts.Format{SampleRate: sampleRate, Channels: 1}
	ch := make(chan tts.Chunk)
	go func() {
		defer close(ch)
		buf := make([]byte, 8192)
		for {
			n, readErr := stdout.Read(buf)
			if n > 0 {
				pcm := make([]byte, n)
				copy(pcm, buf[:n])
				select {
				case ch <- tts.Chunk{PCM: pcm}:
				case <-ctx.Done():
					_ = cmd.Wait()
					ch <- tts.Chunk{Err: ctx.Err()}
					return
				}
			}
			if readErr != nil {
				break
			}
		}
		err := cmd.Wait()
		if ctx.Err() != nil {
			ch <- tts.Chunk{Err: ctx.Err()}
			return
		}
		if err != nil {
			ch <- tts.Chunk{Err: fmt.Errorf("kokoro helper failed: %w", err)}
		}
	}()
	return format, ch, nil
}

// readSampleRate consumes stderr lines until the SAMPLE_RATE marker.
func readSampleRate(r io.Reader) (int, error) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if rate, ok := strings.CutPrefix(line, "SAMPLE_RATE="); ok {
			return strconv.Atoi(rate)
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("helper exited before producing audio (is kokoro-onnx installed in the venv?)")
}

func drain(r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
	}
}
