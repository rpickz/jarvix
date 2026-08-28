// Package doctor inspects the environment Jarvix depends on and produces
// actionable output. Voice/audio/AI integration fails for many environmental
// reasons; doctor turns those into clear next steps instead of raw errors.
package doctor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/rpickz/jarvix/internal/ai/openaicompat"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/hotkey"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/stt/whispercpp"
	"github.com/rpickz/jarvix/internal/tools"
	"github.com/rpickz/jarvix/internal/tts/kokoro"
	"github.com/rpickz/jarvix/internal/tts/piper"
)

// Status classifies a check outcome.
type Status int

// Check outcomes.
const (
	OK Status = iota
	Warn
	Fail
)

// Result is one completed check.
type Result struct {
	Status Status
	Name   string // short label, e.g. "PipeWire available"
	Detail string // extra context shown after the label
	Fix    string // what to do about a Warn/Fail; multi-line allowed
}

// Run executes every check. Failures never abort the run: the point is a
// complete picture.
func Run(cfg config.Config, paths config.Paths) []Result {
	checks := []func(config.Config, config.Paths) Result{
		checkConfig,
		checkPipeWire,
		checkMicrophone,
		checkOutput,
		checkAudioDevices,
		checkWhisperBinary,
		checkWhisperModel,
		// The probe sits beside the existence checks it corrects for: an
		// installed binary and a present model file can still add up to an
		// engine that aborts on every call (issue #113).
		checkSTTProbe,
		checkNameRecognition,
		checkVocabularyBias,
		checkTTS,
		checkTTSProbe,
		checkVoiceLanguage,
		checkSpeechLanguage,
		checkKokoroHelperLanguage,
		checkArtifactRenderer,
		checkIntentBinaries,
		checkContextSources,
		checkConversationSearch,
		checkWindowControl,
		checkTyping,
		checkDaemon,
		checkWarmEngines,
		checkProviderConfigured,
		checkProviderReachable,
		checkContextFloor,
		checkPlugin,
		checkKeybindings,
		checkPushToTalk,
		checkWakeWord,
	}
	results := make([]Result, 0, len(checks))
	for _, check := range checks {
		results = append(results, check(cfg, paths))
	}
	// One result per configured advisor, so a CLI that moved is visible
	// before someone asks Jarvix to consult it (ADR 0016).
	results = append(results, advisorChecks(cfg)...)
	// And one per configured script, so a file that moved or lost its
	// execute bit is named here before its phrase is ever spoken (ADR 0030).
	results = append(results, scriptChecks(cfg)...)
	// The feed cache (ADR 0031): per-feed command availability, plus the
	// running scheduler's health — fresh, stale, failing since when.
	return append(results, knowledgeChecks(cfg, paths)...)
}

// Healthy reports whether no check failed (warnings are tolerated).
func Healthy(results []Result) bool {
	for _, r := range results {
		if r.Status == Fail {
			return false
		}
	}
	return true
}

func checkConfig(cfg config.Config, paths config.Paths) Result {
	if err := cfg.Validate(); err != nil {
		return Result{Status: Fail, Name: "configuration valid",
			Detail: err.Error(),
			Fix:    "Edit " + paths.ConfigFile()}
	}
	return Result{Status: OK, Name: "configuration valid"}
}

func checkPipeWire(config.Config, config.Paths) Result {
	for _, bin := range []string{"pw-record", "pw-play"} {
		if _, err := exec.LookPath(bin); err != nil {
			return Result{Status: Fail, Name: "PipeWire available",
				Detail: bin + " not found",
				Fix:    "Install pipewire tools: sudo pacman -S pipewire pipewire-audio"}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "pw-cli", "info", "0").Run(); err != nil {
		return Result{Status: Fail, Name: "PipeWire available",
			Detail: "pw-cli cannot reach the PipeWire daemon",
			Fix:    "Start it: systemctl --user start pipewire wireplumber"}
	}
	return Result{Status: OK, Name: "PipeWire available"}
}

// wpctlSection extracts one section ("Sources:" / "Sinks:") of wpctl status.
func wpctlSection(section string) (deviceLine string, found bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "wpctl", "status").Output()
	if err != nil {
		return "", false
	}
	inSection := false
	for _, line := range strings.Split(string(out), "\n") {
		trimmed := strings.TrimLeft(line, " │├─└")
		if strings.HasPrefix(trimmed, section) {
			inSection = true
			continue
		}
		if inSection {
			if strings.TrimSpace(trimmed) == "" || strings.HasSuffix(strings.TrimSpace(trimmed), ":") {
				break // next section
			}
			if strings.Contains(trimmed, "*") {
				return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(trimmed), "*")), true
			}
			if deviceLine == "" {
				deviceLine = strings.TrimSpace(trimmed)
			}
		}
	}
	return deviceLine, deviceLine != ""
}

func checkMicrophone(config.Config, config.Paths) Result {
	device, ok := wpctlSection("Sources:")
	if !ok {
		return Result{Status: Fail, Name: "microphone detected",
			Detail: "no audio sources found",
			Fix:    "Connect a microphone, or check `wpctl status`"}
	}
	return Result{Status: OK, Name: "microphone detected", Detail: device}
}

func checkOutput(config.Config, config.Paths) Result {
	device, ok := wpctlSection("Sinks:")
	if !ok {
		return Result{Status: Fail, Name: "audio output available",
			Detail: "no audio sinks found",
			Fix:    "Check `wpctl status` for playback devices"}
	}
	return Result{Status: OK, Name: "audio output available", Detail: device}
}

// checkAudioDevices names the nodes Jarvix will actually bind, right now —
// not whether *a* device exists (checkMicrophone/checkOutput say that) but
// *which one* speech will come out of and capture will listen to (issue
// #142). Names, not descriptions, on the binding lines: node.name is the
// exact string an audio.input_device / audio.output_device pin uses, so this
// line is also how a user checks a pin is spelled right.
//
// The unpinned default is called out as such because it is the load-bearing
// choice: a default-following stream is one WirePlumber moves live when the
// default changes, which is what makes switching headsets mid-sentence work.
func checkAudioDevices(cfg config.Config, _ config.Paths) Result {
	const name = "audio devices"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sink, sinkErr := audio.DefaultSink(ctx)
	source, sourceErr := audio.DefaultSource(ctx)
	if sinkErr != nil || sourceErr != nil {
		err := sinkErr
		if err == nil {
			err = sourceErr
		}
		return Result{Status: Warn, Name: name,
			Detail: "cannot read the current defaults: " + err.Error(),
			Fix:    "Check `wpctl status` (sudo pacman -S wireplumber if wpctl is missing)"}
	}
	speaks := fmt.Sprintf("speaks to %s (the default sink)", sink.Name)
	if cfg.Audio.OutputDevice != "" {
		speaks = fmt.Sprintf("speaks to %s (pinned by audio.output_device; default sink is %s)",
			cfg.Audio.OutputDevice, sink.Name)
	}
	listens := fmt.Sprintf("listens to %s (the default source)", source.Name)
	if cfg.Audio.InputDevice != "" {
		listens = fmt.Sprintf("listens to %s (pinned by audio.input_device; default source is %s)",
			cfg.Audio.InputDevice, source.Name)
	}
	return Result{Status: OK, Name: name, Detail: speaks + "; " + listens}
}

func checkWhisperBinary(cfg config.Config, _ config.Paths) Result {
	if _, err := exec.LookPath(cfg.STT.Whisper.Binary); err != nil {
		return Result{Status: Fail, Name: "whisper.cpp installed",
			Detail: cfg.STT.Whisper.Binary + " not found in PATH",
			Fix:    "Install it: sudo pacman -S whisper-cpp"}
	}
	return Result{Status: OK, Name: "whisper.cpp installed"}
}

func checkWhisperModel(cfg config.Config, paths config.Paths) Result {
	path := whispercpp.ResolveModelPath(cfg.STT.Whisper.Model, paths.WhisperModelDir())
	if _, err := os.Stat(path); err != nil {
		return Result{Status: Fail, Name: "Whisper model available",
			Detail: "expected " + path,
			Fix:    "Download it: jarvix setup whisper"}
	}
	return Result{Status: OK, Name: "Whisper model available", Detail: cfg.STT.Whisper.Model}
}

func checkTTS(cfg config.Config, _ config.Paths) Result {
	if cfg.TTS.Provider == "kokoro" {
		k := &kokoro.Synthesizer{Voice: cfg.TTS.Kokoro.Voice, Speed: cfg.TTS.Kokoro.Speed}
		if err := k.Ready(); err != nil {
			return Result{Status: Fail, Name: "Kokoro TTS ready", Detail: err.Error(),
				Fix: "Install it: scripts/setup-kokoro.sh"}
		}
		return Result{Status: OK, Name: "Kokoro TTS ready", Detail: "voice " + cfg.TTS.Kokoro.Voice}
	}
	// Piper (default).
	if _, err := exec.LookPath(cfg.TTS.Piper.Binary); err != nil {
		return Result{Status: Fail, Name: "Piper installed",
			Detail: cfg.TTS.Piper.Binary + " not found in PATH",
			Fix:    "Install it: sudo pacman -S piper-tts-bin (AUR) or pip install piper-tts"}
	}
	s := &piper.Synthesizer{Binary: cfg.TTS.Piper.Binary, Voice: cfg.TTS.Piper.Voice}
	if err := s.ResolveVoice(); err != nil {
		return Result{Status: Fail, Name: "Piper voice available",
			Detail: err.Error(),
			Fix:    "Install voices: sudo pacman -S piper-voices-en-us (AUR),\nor set tts.piper.voice to a downloaded .onnx path"}
	}
	return Result{Status: OK, Name: "Piper voice available", Detail: cfg.TTS.Piper.Voice}
}

// checkArtifactRenderer is a Warn, not a Fail, when mmdc is missing: the
// artifact tool degrades to prose answers on its own, so a missing renderer
// costs a feature, not the assistant.
func checkArtifactRenderer(cfg config.Config, _ config.Paths) Result {
	if !cfg.Tools.Artifacts {
		return Result{Status: OK, Name: "diagram rendering",
			Detail: "disabled ([tools] artifacts = false)"}
	}
	r := &tools.MermaidRenderer{}
	if err := r.Available(); err != nil {
		return Result{Status: Warn, Name: "diagram renderer installed",
			Detail: err.Error(),
			Fix:    "Install mermaid-cli: " + tools.MermaidInstallHint}
	}
	return Result{Status: OK, Name: "diagram renderer installed",
		Detail: "mmdc → " + cfg.Artifacts.Dir}
}

func checkDaemon(_ config.Config, paths config.Paths) Result {
	client, err := ipc.Dial(paths.Socket)
	if err != nil {
		return Result{Status: Fail, Name: "jarvixd running",
			Detail: "socket not reachable at " + paths.Socket,
			Fix:    "Start it: systemctl --user start jarvixd\nor run in the foreground: jarvixd"}
	}
	defer func() { _ = client.Close() }()
	var status map[string]any
	if err := client.Call("status.get", nil, &status); err != nil {
		return Result{Status: Fail, Name: "jarvixd running",
			Detail: "socket reachable but status.get failed: " + err.Error(),
			Fix:    "Restart it: systemctl --user restart jarvixd"}
	}
	return Result{Status: OK, Name: "jarvixd running",
		Detail: fmt.Sprintf("version %v, state %v", status["version"], status["state"])}
}

// checkWarmEngines reports the supervised engine processes and what they cost
// in memory (ADR 0018). The daemon is the only place that knows: warm workers
// are its children, and their state is not visible on disk. A worker that is
// merely cold is not a problem — it warms on the next question — but one that
// keeps restarting is, and that is what this surfaces.
func checkWarmEngines(cfg config.Config, paths config.Paths) Result {
	const name = "warm engines"
	if !cfg.Performance.WarmEngines {
		return Result{Status: OK, Name: name,
			Detail: "disabled (performance.warm_engines = false); every session pays a cold start"}
	}
	client, err := ipc.Dial(paths.Socket)
	if err != nil {
		return Result{Status: Warn, Name: name,
			Detail: "enabled, but jarvixd is not running so nothing is warm",
			Fix:    "Start it: systemctl --user start jarvixd"}
	}
	defer func() { _ = client.Close() }()
	var status map[string]any
	if err := client.Call("status.get", nil, &status); err != nil {
		return Result{Status: Warn, Name: name, Detail: "jarvixd did not answer: " + err.Error()}
	}
	workers, _ := status["warm"].([]any)
	if len(workers) == 0 {
		return Result{Status: Warn, Name: name,
			Detail: "enabled, but the running daemon has no warm workers",
			Fix:    "Reload the configuration: jarvix config reload"}
	}

	var parts []string
	var restarting []string
	totalMB := 0.0
	for _, entry := range workers {
		w, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		label, _ := w["name"].(string)
		running, _ := w["running"].(bool)
		rss := jsonNumber(w["rss_mb"])
		restarts := jsonNumber(w["restarts"])
		switch {
		case running:
			totalMB += rss
			parts = append(parts, fmt.Sprintf("%s warm (%.0f MB)", label, rss))
		default:
			parts = append(parts, label+" cold")
		}
		if restarts >= warmRestartConcern {
			detail, _ := w["last_error"].(string)
			restarting = append(restarting, fmt.Sprintf("%s restarted %.0f times (%s)", label, restarts, detail))
		}
	}
	detail := strings.Join(parts, ", ")
	if totalMB > 0 {
		detail += fmt.Sprintf("; %.0f MB resident, cap %d MB, reaped after %ds idle",
			totalMB, cfg.Performance.WarmMemoryCapMB, cfg.Performance.WarmIdleReapSec)
	}
	if len(restarting) > 0 {
		return Result{Status: Warn, Name: name,
			Detail: detail + " — " + strings.Join(restarting, "; "),
			Fix: "Check the engine with journalctl --user -u jarvixd -g warm, " +
				"or turn warm mode off: jarvix config set performance.warm_engines=false"}
	}
	return Result{Status: OK, Name: name, Detail: detail}
}

// warmRestartConcern is how many restarts of one worker stop being routine
// (an idle reap counts as one) and start being worth a warning.
const warmRestartConcern = 3

// jsonNumber reads a JSON-decoded number whichever way it landed.
func jsonNumber(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	case int:
		return float64(n)
	}
	return 0
}

func checkProviderConfigured(cfg config.Config, paths config.Paths) Result {
	ep, ok := cfg.Endpoint()
	if !ok {
		return Result{Status: Fail, Name: "AI provider configured",
			Detail: fmt.Sprintf("provider %q has no endpoint", cfg.AI.Provider),
			Fix:    "Add an [ai." + cfg.AI.Provider + "] table with base_url to " + paths.ConfigFile()}
	}
	local := strings.Contains(ep.BaseURL, "127.0.0.1") || strings.Contains(ep.BaseURL, "localhost")
	if !local && ep.Key() == "" {
		hint := ep.APIKeyEnv
		if hint == "" {
			hint = "the provider's API key variable"
		}
		return Result{Status: Fail, Name: "AI provider configured",
			Detail: fmt.Sprintf("%s (%s) needs an API key and none is set", cfg.AI.Provider, ep.BaseURL),
			Fix:    "Export " + hint + " in your environment (and in jarvixd's:\nsystemctl --user set-environment " + hint + "=... && systemctl --user restart jarvixd)"}
	}
	return Result{Status: OK, Name: "AI provider configured",
		Detail: fmt.Sprintf("%s → %s (model %s)", cfg.AI.Provider, ep.BaseURL, cfg.AI.Model)}
}

func checkProviderReachable(cfg config.Config, _ config.Paths) Result {
	ep, ok := cfg.Endpoint()
	if !ok {
		return Result{Status: Warn, Name: "provider authentication",
			Detail: "skipped: no endpoint configured"}
	}
	client := openaicompat.New(cfg.AI.Provider, ep.BaseURL, ep.Key())
	if err := client.Probe(context.Background()); err != nil {
		fix := "Check the endpoint and your network"
		if strings.Contains(ep.BaseURL, "11434") {
			fix = "Start Ollama: systemctl start ollama (or run: ollama serve)\nthen pull the model: ollama pull " + cfg.AI.Model
		}
		return Result{Status: Fail, Name: "provider authentication",
			Detail: err.Error(), Fix: fix}
	}
	return Result{Status: OK, Name: "provider authentication succeeded"}
}

func checkPushToTalk(cfg config.Config, _ config.Paths) Result {
	if len(cfg.Activation.PTTChord) == 0 {
		return Result{Status: OK, Name: "push-to-talk",
			Detail: "daemon-side chord disabled; keybindings drive activation"}
	}
	if _, err := hotkey.ResolveChord(cfg.Activation.PTTChord); err != nil {
		return Result{Status: Fail, Name: "push-to-talk chord valid", Detail: err.Error(),
			Fix: "Fix activation.ptt_chord in the config"}
	}
	if !hotkey.Accessible() {
		return Result{Status: Warn, Name: "push-to-talk (hold the chord)",
			Detail: "input devices not readable, so hold-to-talk falls back to tap-to-toggle",
			Fix:    "Grant the daemon read access to keyboards: jarvix setup input"}
	}
	return Result{Status: OK, Name: "push-to-talk (hold the chord)",
		Detail: strings.Join(cfg.Activation.PTTChord, "+")}
}

func checkPlugin(_ config.Config, _ config.Paths) Result {
	home, _ := os.UserHomeDir()
	manifest := filepath.Join(home, ".config", "omarchy", "plugins", "jarvix", "manifest.json")
	if _, err := os.Stat(manifest); err != nil {
		return Result{Status: Warn, Name: "Omarchy plugin installed",
			Detail: "not found at " + filepath.Dir(manifest),
			Fix:    "Install it: make install-plugin (the CLI still works without it)"}
	}
	return Result{Status: OK, Name: "Omarchy plugin installed"}
}
