package doctor

// This file holds the readiness checks for the settings surface: the
// offline, fast subset of doctor that tells a settings screen whether an
// option's external dependencies are in place (kokoro set up, whisper model
// downloaded, input access granted) — with the fix command shown, before the
// user commits to a value.

import "github.com/rpickz/jarvix/internal/config"

// String renders a Status for IPC/display.
func (s Status) String() string {
	switch s {
	case Warn:
		return "warn"
	case Fail:
		return "fail"
	default:
		return "ok"
	}
}

// ReadinessResult ties a doctor check to the setting it informs, so the
// settings screen can show readiness inline next to the right field.
type ReadinessResult struct {
	Result
	// Related is the dotted config key this check speaks to ("" = the whole
	// configuration).
	Related string
}

// SettingsChecks runs the checks a settings screen needs, and only those that
// answer fast without the network: no provider probe, no PipeWire round trip,
// no daemon dial (the caller *is* the daemon). Full diagnostics stay with
// `jarvix doctor`.
func SettingsChecks(cfg config.Config, paths config.Paths) []ReadinessResult {
	checks := []struct {
		run     func(config.Config, config.Paths) Result
		related string
	}{
		{checkConfig, ""},
		{checkProviderConfigured, "ai.provider"},
		{checkWhisperBinary, "stt.whisper.binary"},
		{checkWhisperModel, "stt.whisper.model"},
		{checkTTS, "tts.provider"},
		// The settings screen is where a voice is chosen, so it is where the
		// consequences of choosing one must appear: which language it speaks,
		// and whether speech recognition can serve that language.
		{checkVoiceLanguage, "tts.kokoro.voice"},
		{checkSpeechLanguage, "stt.whisper.language"},
		{checkArtifactRenderer, "tools.artifacts"},
		{checkContextSources, "context.window"},
		// Two local hyprctl calls, both bounded and neither on the network —
		// fast enough for a settings screen, and the one place the answer to
		// "why does nothing happen when I ask it to focus something?" lives.
		{checkWindowControl, "tools.desktop"},
		// Typing is off by default, so this check is usually one string compare;
		// with it on it is a local probe that presses nothing (ADR 0023).
		{checkTyping, "tools.typing.enable"},
		{checkPushToTalk, "activation.ptt_chord"},
	}
	results := make([]ReadinessResult, 0, len(checks))
	for _, c := range checks {
		results = append(results, ReadinessResult{Result: c.run(cfg, paths), Related: c.related})
	}
	return results
}
