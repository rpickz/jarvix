// Package piper adapts the Piper neural TTS engine as a tts.Synthesizer.
//
// Piper runs as a short-lived external process per utterance: text goes in on
// stdin, raw s16le PCM comes out on stdout (--output-raw). The voice's sample
// rate is read from its .onnx.json sidecar. Killing the process implements
// cancellation, which keeps "interrupt Jarvix mid-sentence" immediate.
package piper

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/rpickz/jarvix/internal/tts"
)

// DefaultVoiceDirs are searched, in order, when the configured voice is a
// name rather than a path.
var DefaultVoiceDirs = []string{
	"/usr/share/piper-voices",
	"/usr/local/share/piper-voices",
}

// Synthesizer runs piper for each utterance.
type Synthesizer struct {
	Binary    string   // piper executable (piper-tts on Arch)
	Voice     string   // voice name or absolute .onnx path
	VoiceDirs []string // extra directories to search; DefaultVoiceDirs are always searched

	// resolved lazily
	modelPath  string
	sampleRate int
}

// Name implements tts.Synthesizer.
func (s *Synthesizer) Name() string { return "piper" }

// ResolveVoice locates the .onnx model for the configured voice and reads its
// sample rate. Idempotent; called automatically by Speak.
func (s *Synthesizer) ResolveVoice() error {
	if s.modelPath != "" {
		return nil
	}
	path, err := findVoice(s.Voice, append(s.VoiceDirs, DefaultVoiceDirs...))
	if err != nil {
		return err
	}
	rate, err := voiceSampleRate(path)
	if err != nil {
		return err
	}
	s.modelPath = path
	s.sampleRate = rate
	return nil
}

func findVoice(voice string, dirs []string) (string, error) {
	if voice == "" {
		return "", fmt.Errorf("no piper voice configured (tts.piper.voice)")
	}
	if filepath.IsAbs(voice) {
		if _, err := os.Stat(voice); err != nil {
			return "", fmt.Errorf("piper voice not found at %s", voice)
		}
		return voice, nil
	}
	want := voice
	if !strings.HasSuffix(want, ".onnx") {
		want += ".onnx"
	}
	for _, dir := range dirs {
		var found string
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || found != "" {
				return fs.SkipAll
			}
			if !d.IsDir() && d.Name() == want {
				found = path
				return fs.SkipAll
			}
			return nil
		})
		if found != "" {
			return found, nil
		}
	}
	return "", fmt.Errorf(
		"piper voice %q not found under %s; install a voice package (e.g. pacman -S piper-voices-en-us) or set tts.piper.voice to a model path",
		voice, strings.Join(dirs, ", "))
}

// voiceSampleRate reads audio.sample_rate from the voice's JSON sidecar.
func voiceSampleRate(modelPath string) (int, error) {
	data, err := os.ReadFile(modelPath + ".json")
	if err != nil {
		return 0, fmt.Errorf("piper voice config missing: %w", err)
	}
	var cfg struct {
		Audio struct {
			SampleRate int `json:"sample_rate"`
		} `json:"audio"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil || cfg.Audio.SampleRate == 0 {
		return 0, fmt.Errorf("piper voice config unreadable at %s.json", modelPath)
	}
	return cfg.Audio.SampleRate, nil
}

// Speak implements tts.Synthesizer.
func (s *Synthesizer) Speak(ctx context.Context, req tts.Request) (tts.Format, <-chan tts.Chunk, error) {
	if err := s.ResolveVoice(); err != nil {
		return tts.Format{}, nil, err
	}
	if _, err := exec.LookPath(s.Binary); err != nil {
		return tts.Format{}, nil, fmt.Errorf("piper binary %q not found; install piper-tts", s.Binary)
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		return tts.Format{}, nil, fmt.Errorf("nothing to speak")
	}

	cmd := exec.CommandContext(ctx, s.Binary,
		"--model", s.modelPath,
		"--output_raw",
		"--quiet",
	)
	cmd.Cancel = func() error { return cmd.Process.Kill() }
	// Piper treats each input line as one utterance; collapse newlines so the
	// whole response renders as a continuous stream.
	cmd.Stdin = strings.NewReader(strings.ReplaceAll(text, "\n", " ") + "\n")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return tts.Format{}, nil, err
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return tts.Format{}, nil, fmt.Errorf("start piper: %w", err)
	}

	format := tts.Format{SampleRate: s.sampleRate, Channels: 1}
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
			ch <- tts.Chunk{Err: fmt.Errorf("piper failed: %w", err)}
		}
	}()
	return format, ch, nil
}
