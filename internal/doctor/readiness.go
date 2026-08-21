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
		{checkArtifactRenderer, "tools.artifacts"},
		{checkPushToTalk, "activation.ptt_chord"},
	}
	results := make([]ReadinessResult, 0, len(checks))
	for _, c := range checks {
		results = append(results, ReadinessResult{Result: c.run(cfg, paths), Related: c.related})
	}
	return results
}
