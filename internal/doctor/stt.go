package doctor

import (
	"fmt"
	"strings"

	"github.com/rpickz/jarvix/internal/config"
)

// checkNameRecognition reports the two halves of issue #83 in one line:
// whether transcription is biased toward the assistant's name (and the user's
// extra vocabulary), and how many mishearing aliases the wake-transcript
// strip accepts. It exists because both mechanisms are invisible when they
// work — a user asking "why does it still write Jarvis?" needs to see whether
// the bias is even active before blaming the model.
//
// OK either way: an empty prompt only happens when the user has cleared the
// wake word and configured no vocabulary, which is a choice, not a fault.
func checkNameRecognition(cfg config.Config, _ config.Paths) Result {
	const name = "name recognition"
	prompt := cfg.STTBiasPrompt()
	if prompt == "" {
		return Result{Status: OK, Name: name,
			Detail: "bias off (no wake word and no stt.vocabulary configured); transcription runs unbiased"}
	}
	detail := "bias prompt active"
	if word := strings.TrimSpace(cfg.Activation.WakeWord); word != "" {
		detail += fmt.Sprintf(" (%q", word)
		if n := len(cfg.STT.Vocabulary); n > 0 {
			detail += fmt.Sprintf(" + %d vocabulary terms", n)
		}
		detail += ")"
	} else {
		detail += fmt.Sprintf(" (%d vocabulary terms)", len(cfg.STT.Vocabulary))
	}
	detail += fmt.Sprintf("; %d wake aliases accepted in transcripts", len(cfg.Activation.WakeAliases))
	return Result{Status: OK, Name: name, Detail: detail}
}
