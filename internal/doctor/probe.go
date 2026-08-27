package doctor

// The voice-loop probes (issue #113): doctor runs the STT and TTS engines for
// real instead of trusting that an installed binary is a working one.
//
// The incident that forced this: on 2026-08-25 an Arch update split ggml's
// compute backends into separate packages and left none installed. Every
// whisper invocation aborted with `GGML_ASSERT(device) failed` — every voice
// session died at transcription — for two days, while doctor kept printing
// "[OK] whisper.cpp installed", because existence is all it checked. On a
// rolling-release distro "installed" and "functional" diverge routinely;
// these probes exist to name exactly that divergence in one line.
//
// Shape of both probes:
//   - the engine is really executed, cold, the way a session would run it —
//     the cold path is where load-time breakage (backends, models, venvs)
//     lives, and the warm servers link the same libraries;
//   - no mic, no speakers, no network: STT transcribes a wav generated right
//     here, TTS synthesizes a short phrase into a discarded sink;
//   - a FAIL quotes the engine's own stderr, because the engine already said
//     what is wrong and paraphrasing it loses the searchable string;
//   - a dependency that a *previous* check already failed (missing binary,
//     missing voice, kokoro never set up) makes the probe skip with a note
//     rather than fail twice — one cause, one FAIL. The one exception is the
//     model file, which the acceptance criteria want named here too;
//   - wall-time is bounded per probe, and the budget is printed so a slow
//     machine's user knows what they are waiting on.

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/stt/whispercpp"
	"github.com/rpickz/jarvix/internal/tts"
	"github.com/rpickz/jarvix/internal/tts/kokoro"
	"github.com/rpickz/jarvix/internal/tts/piper"
)

// probeTimeout bounds one engine probe. Generous on purpose: a cold large-v3
// on CPU or a cold Kokoro venv can take double-digit seconds legitimately,
// and the budget only ever bites a hung engine — which is itself a finding.
const probeTimeout = 30 * time.Second

// The probe wav: a quiet tone in whisper's native input format. A tone rather
// than silence so the engine demonstrably processes audio, and short because
// the point is the engine loading at all, not transcription quality.
const (
	probeRate     = 16000 // whisper.cpp's expected sample rate, mono
	probeToneHz   = 440
	probeToneSecs = 0.6
)

// probePhrase is what the TTS probe synthesizes. Short, and never played.
const probePhrase = "Jarvix voice check."

// probeTone renders the probe audio: 16 kHz mono s16le, a 440 Hz sine at a
// fifth of full scale.
func probeTone() []int16 {
	pcm := make([]int16, int(probeToneSecs*probeRate))
	for i := range pcm {
		pcm[i] = int16(0.2 * math.MaxInt16 * math.Sin(2*math.Pi*probeToneHz*float64(i)/probeRate))
	}
	return pcm
}

// writeProbeWAV materialises the probe tone as a RIFF/WAVE file at path.
func writeProbeWAV(path string) error {
	return audio.WriteWAV(path, probeTone(), probeRate, 1)
}

// probeScratch creates the directory probe artifacts live in, under the same
// tmpfs runtime root session recordings use, and returns the always-run
// cleanup. Nothing a probe writes may outlive the probe — not even on
// failure; a diagnostic that leaves litter behind is a bug of its own.
func probeScratch(paths config.Paths) (string, func(), error) {
	base := paths.Runtime
	if base == "" {
		base = os.TempDir()
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp(base, "doctor-probe-*")
	if err != nil {
		return "", nil, err
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

// checkSTTProbe proves whisper-cli can transcribe, not merely exist.
func checkSTTProbe(cfg config.Config, paths config.Paths) Result {
	return probeSTT(cfg, paths, probeTimeout)
}

func probeSTT(cfg config.Config, paths config.Paths, timeout time.Duration) Result {
	const name = "whisper.cpp transcribes"
	binary := cfg.STT.Whisper.Binary
	if _, err := exec.LookPath(binary); err != nil {
		return Result{Status: OK, Name: name,
			Detail: `skipped: ` + binary + ` is not installed (the "whisper.cpp installed" check has the fix)`}
	}
	model := whispercpp.ResolveModelPath(cfg.STT.Whisper.Model, paths.WhisperModelDir())
	if _, err := os.Stat(model); err != nil {
		return Result{Status: Fail, Name: name,
			Detail: "cannot probe: whisper model missing at " + model,
			Fix:    "Download it: jarvix setup whisper"}
	}

	dir, cleanup, err := probeScratch(paths)
	if err != nil {
		return Result{Status: Warn, Name: name,
			Detail: "could not create a scratch directory for the probe wav: " + err.Error()}
	}
	defer cleanup()
	wav := filepath.Join(dir, "probe.wav")
	if err := writeProbeWAV(wav); err != nil {
		return Result{Status: Warn, Name: name,
			Detail: "could not write the probe wav: " + err.Error()}
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// The same invocation the whispercpp adapter uses in a session, so what
	// this probe proves is the path a session actually takes.
	args := []string{"--model", model, "--file", wav, "--no-timestamps", "--no-prints"}
	if cfg.STT.Whisper.Language != "" {
		args = append(args, "--language", cfg.STT.Whisper.Language)
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(start)

	if ctx.Err() != nil {
		return Result{Status: Fail, Name: name,
			Detail: fmt.Sprintf("gave up after the %s probe budget: %s hung instead of transcribing", timeout, binary),
			Fix:    "Run it by hand against any wav and see where it stops:\n" + binary + " --model " + model + " --file <some.wav>"}
	}
	if runErr != nil {
		summary, fix := classifySTTFailure(stderr.String(), model, pacmanPresent())
		return Result{Status: Fail, Name: name,
			Detail: fmt.Sprintf("%s (%v): %s", summary, runErr, stderrTail(stderr.String())),
			Fix:    fix}
	}
	detail := fmt.Sprintf("transcribed a generated %.1fs wav in %.1fs (probe budget %s)",
		probeToneSecs, elapsed.Seconds(), timeout)
	if backend := backendLoadPath(stderr.String()); backend != "" {
		detail += "; backend " + backend
	}
	return Result{Status: OK, Name: name, Detail: detail}
}

// classifySTTFailure sorts a failed whisper-cli run into the cause classes a
// user can act on, from the engine's stderr. Backend breakage is tested first
// because its stderr (the 2026-08-25 incident) can also mention the model it
// was loading when it died.
func classifySTTFailure(stderr, model string, pacman bool) (summary, fix string) {
	switch {
	case strings.Contains(stderr, "GGML_ASSERT") ||
		(strings.Contains(stderr, "ggml_backend_load") && strings.Contains(stderr, "does not exist")) ||
		strings.Contains(stderr, "failed to load backend") ||
		strings.Contains(stderr, "no backends loaded"):
		fix = "The binary is installed but its ggml compute backend did not load —\n" +
			"likely missing or incompatible backend libraries after an update.\n" +
			"Reinstall whisper.cpp together with its ggml libraries."
		if pacman {
			fix += "\nOn pacman systems the ggml backends ship as separate packages (ggml-cpu,\n" +
				"ggml-vulkan, …): check `pacman -Qs ggml` and install a compute backend."
		}
		return "whisper-cli aborted loading its compute backend", fix
	case strings.Contains(stderr, "failed to load model") ||
		strings.Contains(stderr, "error loading model") ||
		strings.Contains(stderr, "invalid model"):
		return "whisper-cli could not load the model",
			"The file at " + model + " is unreadable or incompatible.\nRe-download it: jarvix setup whisper"
	default:
		return "whisper-cli failed on the probe wav",
			"Run it by hand against any wav and read the full output:\n" + "whisper-cli --model " + model + " --file <some.wav>"
	}
}

// pacmanPresent detects a pacman-based system, so the split-backend-package
// hint only appears where pacman exists to act on it. A LookPath, nothing
// distro-specific beyond that.
func pacmanPresent() bool {
	_, err := exec.LookPath("pacman")
	return err == nil
}

// stderrTail quotes the end of an engine's stderr on one line: the last few
// non-empty lines, capped, because the complaint that matters ("GGML_ASSERT
// (device) failed") is at the end and the banner above it is not.
func stderrTail(s string) string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	if len(lines) == 0 {
		return "(no stderr)"
	}
	if len(lines) > 3 {
		lines = lines[len(lines)-3:]
	}
	out := strings.Join(lines, " | ")
	if len(out) > 300 {
		out = "…" + out[len(out)-300:]
	}
	return out
}

// backendLoadPath extracts which compute backend ggml loaded, when its loader
// said so on stderr. Best effort by design: older builds print nothing here,
// and the probe's OK does not depend on it.
func backendLoadPath(stderr string) string {
	for _, line := range strings.Split(stderr, "\n") {
		if !strings.Contains(line, "load_backend: loaded") &&
			!strings.Contains(line, "ggml_backend_load_best: loading") {
			continue
		}
		if i := strings.LastIndex(line, " from "); i >= 0 {
			return strings.TrimSpace(line[i+len(" from "):])
		}
		fields := strings.Fields(line)
		if len(fields) > 0 && strings.HasPrefix(fields[len(fields)-1], "/") {
			return fields[len(fields)-1]
		}
	}
	return ""
}

// checkTTSProbe proves the configured TTS engine can synthesize, not merely
// resolve its files.
func checkTTSProbe(cfg config.Config, _ config.Paths) Result {
	return probeTTS(cfg, probeTimeout)
}

func probeTTS(cfg config.Config, timeout time.Duration) Result {
	if cfg.TTS.Provider == "kokoro" {
		const name = "kokoro synthesizes"
		k := &kokoro.Synthesizer{Voice: cfg.TTS.Kokoro.Voice, Speed: cfg.TTS.Kokoro.Speed}
		if err := k.Ready(); err != nil {
			return Result{Status: OK, Name: name,
				Detail: `skipped: kokoro is not set up (the "Kokoro TTS ready" check has the fix)`}
		}
		return speakProbe(name, k, timeout)
	}
	const name = "piper synthesizes"
	if _, err := exec.LookPath(cfg.TTS.Piper.Binary); err != nil {
		return Result{Status: OK, Name: name,
			Detail: `skipped: ` + cfg.TTS.Piper.Binary + ` is not installed (the "Piper installed" check has the fix)`}
	}
	s := &piper.Synthesizer{Binary: cfg.TTS.Piper.Binary, Voice: cfg.TTS.Piper.Voice}
	if err := s.ResolveVoice(); err != nil {
		return Result{Status: OK, Name: name,
			Detail: `skipped: no usable voice (the "Piper voice available" check has the fix)`}
	}
	return speakProbe(name, s, timeout)
}

// speakProbe synthesizes probePhrase through a real Synthesizer into a
// discarded sink — the audio is counted, never played — and reports the
// engine's own error when it cannot. The engines' failure errors carry their
// stderr tails since issue #113, so quoting the error is quoting the engine.
func speakProbe(name string, s tts.Synthesizer, timeout time.Duration) Result {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	start := time.Now()
	format, ch, err := s.Speak(ctx, tts.Request{Text: probePhrase})
	if err != nil {
		if ctx.Err() != nil {
			return timeoutResult(name, s.Name(), timeout)
		}
		return Result{Status: Fail, Name: name, Detail: err.Error(), Fix: ttsProbeFix(s.Name())}
	}
	var pcmBytes int
	var streamErr error
	for c := range ch {
		pcmBytes += len(c.PCM)
		if c.Err != nil {
			streamErr = c.Err
		}
	}
	elapsed := time.Since(start)
	if ctx.Err() != nil {
		return timeoutResult(name, s.Name(), timeout)
	}
	if streamErr != nil {
		return Result{Status: Fail, Name: name, Detail: streamErr.Error(), Fix: ttsProbeFix(s.Name())}
	}
	if pcmBytes == 0 {
		return Result{Status: Fail, Name: name,
			Detail: s.Name() + " exited cleanly but produced no audio for the probe phrase",
			Fix:    ttsProbeFix(s.Name())}
	}
	detail := fmt.Sprintf("spoke %q to a discarded sink in %.1fs (probe budget %s)",
		probePhrase, elapsed.Seconds(), timeout)
	if bytesPerSec := 2 * format.SampleRate * format.Channels; bytesPerSec > 0 {
		detail = fmt.Sprintf("spoke %q — %.1fs of audio to a discarded sink — in %.1fs (probe budget %s)",
			probePhrase, float64(pcmBytes)/float64(bytesPerSec), elapsed.Seconds(), timeout)
	}
	return Result{Status: OK, Name: name, Detail: detail}
}

func timeoutResult(name, engine string, timeout time.Duration) Result {
	return Result{Status: Fail, Name: name,
		Detail: fmt.Sprintf("gave up after the %s probe budget: %s hung instead of synthesizing", timeout, engine),
		Fix:    ttsProbeFix(engine)}
}

func ttsProbeFix(engine string) string {
	if engine == "kokoro" {
		return "Re-run scripts/setup-kokoro.sh; if it keeps failing, pipe a sentence\n" +
			"through the helper by hand and read its stderr."
	}
	return "Reinstall piper and its voice package, or pipe a sentence through\n" +
		"piper by hand (--model <voice.onnx> --output_raw) and read its stderr."
}
