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
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/rpickz/jarvix/internal/tts"
	"github.com/rpickz/jarvix/internal/voice"
)

// Default install locations, matching scripts/setup-kokoro.sh — including its
// XDG_DATA_HOME handling, without which the adapter looks for the models in
// ~/.local/share on a machine where the script put them somewhere else.
func defaultDir() string {
	if data := os.Getenv("XDG_DATA_HOME"); data != "" {
		return filepath.Join(data, "jarvix")
	}
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
	return voice.KokoroVoicesFile(defaultDir())
}

func (s *Synthesizer) voice() string {
	if s.Voice != "" {
		return s.Voice
	}
	return DefaultVoice
}

// lang is the phonemiser language the helper is driven with, derived from the
// voice and never configured next to it.
//
// This is the fix for the defect that motivated the whole feature: the helper
// used to be handed lang="en-us" for every voice, so a British voice spoke
// British-sounding American English — rhotic R's, T's flapped to D's — and no
// amount of config could change it. Deriving the language from the voice id
// makes that combination unrepresentable.
//
// A voice whose id says nothing about its language (a custom embedding
// somebody added to the archive) falls back to the default rather than
// refusing to speak: silence is a worse answer than an accent.
func (s *Synthesizer) lang() string {
	if l, ok := voice.LanguageForKokoroVoice(s.voice()); ok {
		return l.Code
	}
	return voice.DefaultLanguage().Code
}

// DefaultVoice is the voice Kokoro speaks with when none is configured.
const DefaultVoice = "af_heart"

// langArgs is the --lang flag for the helper, omitted when the installed
// helper predates it.
//
// The helper is copied out of the repo into ~/.local/share/jarvix by
// setup-kokoro.sh, so upgrading Jarvix does not upgrade it — a packaged
// upgrade refreshes /usr/share/jarvix, which is not the file the adapter
// runs. Handing --lang to a helper that has never heard of it makes argparse
// exit 2, which would take the user's voice away entirely as the price of an
// upgrade they did not know changed anything. Degrading to the old behaviour
// costs them the accent until they re-run the script, and `jarvix doctor`
// says exactly that in the meantime.
//
// This mirrors how the serve protocol already handles a stale helper: detect,
// degrade, and explain — never fail hard on someone else's upgrade timing.
// Reading a 9 KB file is free next to spawning a Python interpreter, so it is
// checked per spawn rather than cached behind a lock.
func (s *Synthesizer) langArgs() []string {
	if !helperSupportsLang(s.script()) {
		return nil
	}
	return []string{"--lang", s.lang()}
}

// helperSupportsLang reports whether the installed helper accepts --lang. An
// unreadable helper is assumed current: a missing file is Ready's problem to
// report, and guessing "stale" there would deny a working helper its language.
func helperSupportsLang(script string) bool {
	data, err := os.ReadFile(script)
	if err != nil {
		return true
	}
	return bytes.Contains(data, []byte("--lang"))
}

func (s *Synthesizer) speed() float64 {
	if s.Speed > 0 {
		return s.Speed
	}
	return 1.0
}

// Name implements tts.Synthesizer.
func (s *Synthesizer) Name() string { return "kokoro" }

// ScriptPath reports the helper this synthesizer runs, so doctor can inspect
// the *installed* helper rather than assuming it matches the repo's copy —
// setup-kokoro.sh copies it out to the data directory, and upgrading Jarvix
// does not upgrade the copy.
func (s *Synthesizer) ScriptPath() string { return s.script() }

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
	args := append([]string{s.script(),
		"--voice", s.voice(),
		"--speed", strconv.FormatFloat(s.speed(), 'f', 2, 64)},
		s.langArgs()...)
	cmd := exec.CommandContext(ctx, s.python(), args...)
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
	return 0, fmt.Errorf("helper exited before producing audio " +
		"(is kokoro-onnx installed in the venv, and is the installed helper current? re-run scripts/setup-kokoro.sh)")
}

func drain(r io.Reader) {
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
	}
}
