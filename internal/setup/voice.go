package setup

// The voice step: language first, then accent, then hear it.
//
// Ordering language before voice is the whole design. A user does not want
// "bm_george" — they want British, and then a British voice they like the
// sound of. Presenting 54 raw ids and hoping the prefix convention is noticed
// is how the capability stayed invisible for as long as it did.
//
// The step is also the only place that can *fix* the speech-recognition half
// rather than merely refuse it. Config validation has to reject a non-English
// voice paired with the English-only base.en model, because silently
// downloading a different model is not a decision code should make for
// someone. Here it can simply be asked — which is why picking French in the
// wizard results in a working French assistant, while editing config.toml by
// hand results in an error message that says how to make one.

import (
	"fmt"
	"io"
	"strings"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/voice"
)

// VoiceDeps are the injected dependencies of the voice/language step. Every
// one of them is a seam a test fills: no archive is read, no model is loaded,
// and nothing is spoken unless Preview is wired.
type VoiceDeps struct {
	File   *File
	Out    io.Writer
	Prompt Prompter
	// Provider reports the effective tts.provider. It is a function, not a
	// value, because the engine step runs immediately before this one and may
	// have just switched engines — a wizard that then listed Piper voices for
	// the Kokoro the user just installed would be worse than no wizard.
	Provider func() string
	// Catalog enumerates the voices installed for one provider. Required.
	Catalog func(provider string) voice.Catalog
	// Current reports the configured voice id for one provider, the default
	// selection.
	Current func(provider string) string
	// WhisperModel is the effective stt.whisper.model, so the step knows
	// whether recognition can already serve a non-English choice.
	WhisperModel string
	// Preview speaks a sample in the given voice. nil skips the offer — the
	// daemon may not be running, and a wizard must never hang waiting for it.
	Preview func(voiceID string) error
	// DownloadModel fetches a whisper model by name (jarvix setup whisper).
	// nil turns the offer into printed instructions.
	DownloadModel func(model string) error
}

// VoiceStep configures the language Jarvix speaks and the voice it speaks it
// with, carrying the choice through to speech recognition.
func VoiceStep(d VoiceDeps) Step {
	return Step{
		Title: "Language and voice",
		Done: func() (bool, string) {
			provider := d.Provider()
			current := d.Current(provider)
			if current == "" {
				return false, ""
			}
			lang, ok := languageOf(provider, current)
			if !ok {
				return false, ""
			}
			// "Configured" is not "chosen": every install starts with the
			// shipped default voice, so treating the default as a decision
			// would skip the step for exactly the users who have never seen
			// it. Only a voice actually written to config.toml counts.
			if _, written := d.File.Get(voiceTable(provider), "voice"); !written {
				return false, ""
			}
			return true, fmt.Sprintf("%s — %s", lang.Name, current)
		},
		Run: func() error { return runVoiceStep(d) },
	}
}

func runVoiceStep(d VoiceDeps) error {
	provider := d.Provider()
	current := d.Current(provider)
	installed, err := d.Catalog(provider).Voices()
	if err != nil {
		// Not a step failure: the engine's own readiness is the previous
		// step's business, and failing here would list this step as broken
		// when the real problem is already reported above it.
		fprintf(d.Out, "No %s voices to choose from: %v\n", provider, err)
		return nil
	}

	lang, ok := chooseLanguage(d, voice.Grouped(installed), languageOrZero(provider, current))
	if !ok {
		return nil
	}
	choices := voice.In(installed, lang)
	if len(choices) == 0 {
		fprintf(d.Out, "No %s voice for %s is installed.\n", provider, lang.Name)
		fprintln(d.Out, installHint(provider, lang))
		return nil
	}

	selected := chooseVoice(d, lang, choices, current)
	setValue(d.File, d.Prompt, d.Out, voiceTable(provider), "voice", selected.ID)
	fprintf(d.Out, "Voice set to %s (%s, %s).\n", selected.ID, lang.Name, selected.Gender)
	return carrySpeechLanguage(d, lang)
}

// chooseLanguage offers every language Kokoro has a family for — not only the
// ones with a voice installed — so that "which languages can this thing
// speak?" has a visible answer, and choosing one that is missing produces the
// install instruction rather than silence.
func chooseLanguage(d VoiceDeps, groups []voice.Group, current voice.Language) (voice.Language, bool) {
	options := make([]string, 0, len(voice.Languages)+1)
	def := 0
	for i, l := range voice.Languages {
		count := 0
		for _, g := range groups {
			if g.Language.Code == l.Code {
				count = len(g.Voices)
			}
		}
		label := fmt.Sprintf("%s (%s) — %d voice(s) installed", l.Name, l.Code, count)
		if count == 0 {
			label = fmt.Sprintf("%s (%s) — none installed", l.Name, l.Code)
		}
		options = append(options, label)
		if l.Code == current.Code {
			def = i
		}
	}
	options = append(options, "keep the current voice")
	choice := d.Prompt.Choose("Which language should Jarvix speak?", options, def)
	if choice == len(options)-1 {
		fprintln(d.Out, "Keeping the current voice.")
		return voice.Language{}, false
	}
	return voice.Languages[choice], true
}

// chooseVoice offers the accents within a language, previewing aloud before
// committing. Hearing it is the only way to choose between four British
// voices, so the preview loops until the user is happy rather than being a
// one-shot confirmation they can only decline.
func chooseVoice(d VoiceDeps, lang voice.Language, choices []voice.Voice, current string) voice.Voice {
	options := make([]string, 0, len(choices))
	def := 0
	for i, v := range choices {
		label := v.ID
		if v.Name != "" {
			label += " — " + v.Name
		}
		if v.Gender != voice.GenderUnknown {
			label += " (" + v.Gender.String() + ")"
		}
		options = append(options, label)
		if v.ID == current {
			def = i
		}
	}
	for {
		choice := d.Prompt.Choose("Which "+lang.Name+" voice?", options, def)
		selected := choices[choice]
		if d.Preview == nil {
			return selected
		}
		if !d.Prompt.Confirm(fmt.Sprintf("Hear %s say something first?", selected.ID), true) {
			return selected
		}
		if err := d.Preview(selected.ID); err != nil {
			fprintf(d.Out, "Could not play a preview (%v) — choosing without one.\n", err)
			return selected
		}
		if d.Prompt.Confirm("Use "+selected.ID+"?", true) {
			return selected
		}
		def = choice
	}
}

// carrySpeechLanguage makes the choice reach whisper, which is the difference
// between an assistant that speaks French and one that also understands it.
func carrySpeechLanguage(d VoiceDeps, lang voice.Language) error {
	if lang.English() {
		// The shipped base.en already serves both English accents; whisper
		// has no notion of accent, so there is nothing to change.
		return nil
	}
	fprintf(d.Out, "\n%s also has to be understood, not only spoken.\n", lang.Name)
	model := d.WhisperModel
	if config.EnglishOnlyWhisperModel(model) {
		fprintf(d.Out, "The speech model in use (%s) transcribes English only, so questions in\n"+
			"%s would come back as English nonsense.\n", model, lang.Name)
		want := config.MultilingualWhisperModel
		if !d.Prompt.Confirm(fmt.Sprintf("Switch to the multilingual %q model (~148 MB download)?", want), true) {
			fprintln(d.Out, "Left as it is — Jarvix will speak "+lang.Name+" but keep listening in English.")
			fprintf(d.Out, "When you want to fix it:\n  jarvix setup whisper %s\n"+
				"  jarvix config set stt.whisper.model=%s stt.whisper.language=%s\n", want, want, lang.Whisper)
			return fmt.Errorf("speech recognition cannot serve %s yet — the two commands above finish the job", lang.Name)
		}
		if d.DownloadModel != nil {
			if err := d.DownloadModel(want); err != nil {
				fprintf(d.Out, "Download failed: %v — retry with `jarvix setup whisper %s`.\n", err, want)
			}
		} else {
			fprintf(d.Out, "Download it when convenient: jarvix setup whisper %s\n", want)
		}
		setValue(d.File, d.Prompt, d.Out, "stt.whisper", "model", want)
		model = want
	}
	setValue(d.File, d.Prompt, d.Out, "stt.whisper", "language", lang.Whisper)
	fprintf(d.Out, "Speech recognition set to %s with the %s model.\n", lang.Whisper, model)
	return nil
}

// voiceTable is the config table the chosen voice is written to.
func voiceTable(provider string) string {
	if provider == "kokoro" {
		return "tts.kokoro"
	}
	return "tts.piper"
}

// languageOf derives a voice's language for whichever engine names it.
func languageOf(provider, id string) (voice.Language, bool) {
	if provider == "kokoro" {
		return voice.LanguageForKokoroVoice(id)
	}
	return voice.LanguageForPiperVoice(id)
}

// languageOrZero is languageOf where "unknown" is an acceptable answer — the
// menu simply has no pre-selection to make.
func languageOrZero(provider, id string) voice.Language {
	l, _ := languageOf(provider, id)
	return l
}

// installHint names what to install for a language the engine cannot speak
// yet. Kokoro ships every language in one archive, so a missing one means the
// archive is missing; Piper ships one package per locale, so it names the
// package — the "or you are told which voice package to install" half of the
// requirement.
func installHint(provider string, l voice.Language) string {
	if provider == "kokoro" {
		return "  Re-run scripts/setup-kokoro.sh — the voices archive ships every language at once."
	}
	pkg := l.PiperPackage()
	if pkg == "" {
		return "  Download a voice from https://huggingface.co/rhasspy/piper-voices and set tts.piper.voice to its .onnx path."
	}
	return strings.Join([]string{
		"  Install the voice package:  sudo pacman -S " + pkg + "  (AUR)",
		"  or download one from https://huggingface.co/rhasspy/piper-voices",
		"  and set tts.piper.voice to its .onnx path.",
	}, "\n")
}
