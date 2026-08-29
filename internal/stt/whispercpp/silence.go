package whispercpp

import (
	"fmt"
	"log/slog"

	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/stt"
)

// Silence is where issue #191 is actually fixed, for both the cold whisper-cli
// path and the warm whisper-server one.
//
// Whisper never declines to answer. Given a capture with nothing in it, it
// returns its most likely continuation of whatever conditioned the decoder:
// with Jarvix's bias prompt that is the bias prompt itself, and without one it
// is the well-known " you" / "Thank you." / "Thanks for watching!" family. A
// hallucinated transcript starts a real exchange — the intent router, the
// provider, the arrival watermark, the archive — so the fix has to happen
// before any of that, and it has to happen in the one place that knows what
// was sent: this adapter.
//
// Two rules, in order of value:
//
//  1. Do not ask at all when the capture has no voiced audio. This removes the
//     whole family rather than one instance of it, and it is the cheaper fix
//     in every sense: one linear pass over a tmpfs file the daemon wrote
//     seconds ago, against several hundred milliseconds of decoding.
//  2. Discard a transcript that is wholly the injected prompt. The bias set is
//     composed per call from the configured assistant name and the taught
//     hard-to-hear phrases, so the comparison is against what this adapter
//     actually sent — never a literal.
//
// Rule 2 is not made redundant by rule 1. A microphone that is picking up a
// quiet room delivers real signal and passes the energy gate, and whisper will
// still echo the prompt at it; rule 1 only covers the input that produced
// nothing at all. Nor is rule 1 made redundant by rule 2: without it, silence
// with no prompt configured still produces " you".
//
// ADR 0060 records both rules, the thresholds, and the measured finding that
// whisper's own decoding flags (--no-speech-thold and friends) do not fix
// this and must not be relied on to.

// noVoiceReason returns the reason to skip transcription entirely, or "" to
// transcribe.
//
// Every uncertainty resolves to "transcribe": an unreadable clip, an
// unparseable one, one too short to measure. The engine's own no-speech path
// is a mild, honest outcome, but a question silently dropped because a WAV
// header confused a parser is not, so nothing here refuses a capture it did
// not manage to measure.
func noVoiceReason(input stt.AudioInput, log *slog.Logger) string {
	level, err := audio.MeasureWAV(input.WAVPath)
	if err != nil {
		// Not an error the user should ever see: the recording is about to be
		// transcribed anyway. It is worth a debug line because a parser that
		// stopped understanding pw-record's output would otherwise disable
		// this gate in total silence.
		log.Debug("could not measure capture level; transcribing anyway",
			"component", "stt", "error", err.Error())
		return ""
	}
	if level.Voiced() {
		return ""
	}
	// The raw levels, not the decibels: digital silence has no logarithm, and
	// a -Inf attribute is a value a JSON log handler cannot encode. The dBFS
	// figures are in the reason, where they are already a string.
	log.Info("capture had no voiced audio; not asking whisper",
		"component", "stt", "peak_rms", level.PeakFrameRMS,
		"floor_rms", audio.SilenceFloorRMS, "frames", level.Frames)
	return fmt.Sprintf("the capture had no voiced audio (peak %.0f dBFS, floor %.0f dBFS)",
		level.DBFS(), audio.DBFS(audio.SilenceFloorRMS))
}

// promptEchoReason returns the reason to discard a finished transcript, or ""
// to keep it.
func promptEchoReason(text, prompt string, log *slog.Logger) string {
	if !stt.IsPromptEcho(text, prompt) {
		return ""
	}
	// Logged at info with the text, because this is the one line that tells a
	// user reading the journal why their question vanished — and because the
	// text is, by construction, Jarvix's own sentence rather than anything
	// the user said.
	log.Info("discarded a transcript that was only the bias prompt",
		"component", "stt", "transcript", text)
	return "the transcript was only the bias prompt echoed back"
}

// nothing builds the final event for a capture that produced no speech: no
// text, and the reason it produced none. The session engine turns this into
// the ordinary "I didn't catch that" and a record that a capture happened,
// rather than an exchange.
func nothing(reason string) stt.TranscriptEvent {
	return stt.TranscriptEvent{Type: stt.EventFinal, Text: "", Reason: reason}
}
