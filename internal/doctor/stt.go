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
// A custom name with no aliases WARNs (issue #103). Aliases are not garnish:
// whisper only writes words it knows, so a novel name usually arrives as a
// nearby real word — the default name kept arriving as "Jarvis" and "JavaX"
// until exactly those spellings were accepted — and a strip that only knows
// the true spelling leaves the summons in the transcript, where it breaks
// intent matching. The shipped aliases cover the shipped name; a chosen name
// starts with none, and this check is where the user learns to add them.
func checkNameRecognition(cfg config.Config, _ config.Paths) Result {
	const name = "name recognition"
	assistant := strings.TrimSpace(cfg.Assistant.Name)
	aliases := cfg.Assistant.EffectiveAliases()

	if cfg.Assistant.CustomName() && assistant != "" && len(aliases) == 0 {
		return Result{Status: Warn, Name: name,
			Detail: fmt.Sprintf("assistant.name is %q but it has no aliases; whisper only writes words it "+
				"knows, so a custom name usually arrives in transcripts as a nearby real word — the default "+
				"name kept arriving as \"Jarvis\" and \"JavaX\" until those spellings were accepted (issue #83) "+
				"— and without aliases the wake-word matcher and strip only fire on the exact spelling", assistant),
			Fix: fmt.Sprintf("Say the name, read what the transcript actually wrote, and accept those spellings:\n"+
				"  [assistant]\n"+
				"  name = %q\n"+
				"  aliases = [\"…the spellings the transcript showed…\"]\n"+
				"(or: jarvix config set assistant.aliases=spelling1,spelling2)", assistant)}
	}

	prompt := cfg.STTBiasPrompt()
	if prompt == "" {
		// Only reachable with the name cleared and no vocabulary — an
		// invalid document nowadays (assistant.name must be set), but doctor
		// reports what *is* rather than assuming validation already ran.
		return Result{Status: OK, Name: name,
			Detail: "bias off (no assistant name and no stt.vocabulary configured); transcription runs unbiased"}
	}
	detail := "bias prompt active"
	if assistant != "" {
		detail += fmt.Sprintf(" (%q", assistant)
		if n := len(cfg.STT.Vocabulary); n > 0 {
			detail += fmt.Sprintf(" + %d vocabulary terms", n)
		}
		detail += ")"
	} else {
		detail += fmt.Sprintf(" (%d vocabulary terms)", len(cfg.STT.Vocabulary))
	}
	detail += fmt.Sprintf("; %d name aliases accepted in transcripts", len(aliases))
	return Result{Status: OK, Name: name, Detail: detail}
}
