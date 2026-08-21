package doctor

// The language checks. Everything else doctor reports is a dependency that is
// either present or missing; language is a property of a *working* install
// that can still be wrong, and wrong in a way the user experiences as the
// assistant being broken rather than misconfigured.
//
// Two things are reported, because two things can be wrong independently.
// What Jarvix speaks: the voice, the accent it will be phonemised with, and
// whether that voice is installed at all. And what Jarvix hears: whether the
// whisper model in use can serve the same language, or is the English-only
// base.en that would transcribe French as nonsense English words.

import (
	"fmt"
	"os"
	"strings"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/tts/kokoro"
	"github.com/rpickz/jarvix/internal/voice"
)

// checkVoiceLanguage reports the active voice and the language it speaks.
func checkVoiceLanguage(cfg config.Config, paths config.Paths) Result {
	const name = "voice language"
	id := cfg.TTS.Kokoro.Voice
	key := "tts.kokoro.voice"
	if cfg.TTS.Provider != "kokoro" {
		id, key = cfg.TTS.Piper.Voice, "tts.piper.voice"
	}
	lang, known := cfg.SpokenLanguage()

	// An installed catalog turns "the voice is wrong" from a synthesis-time
	// surprise into a named problem with named alternatives. Kokoro only:
	// its voices are a closed set in one archive, while Piper's are whatever
	// packages and downloads the machine has, and checkTTS already resolves
	// that one against the filesystem with the package to install.
	if cfg.TTS.Provider == "kokoro" && id != "" {
		catalog := cfg.Voices
		if catalog == nil {
			catalog = cfg.InstalledVoices(paths)
		}
		if installed, err := catalog.Voices(); err == nil && len(installed) > 0 && !voice.Has(installed, id) {
			return Result{Status: Fail, Name: name,
				Detail: fmt.Sprintf("%s is %q, which is not installed", key, id),
				Fix: "Pick an installed voice — `jarvix voices` lists them — e.g.\n  jarvix config set " +
					key + "=" + firstOr(voice.Suggest(installed, id, 1), kokoro.DefaultVoice)}
		}
	}

	if !known {
		return Result{Status: Warn, Name: name,
			Detail: fmt.Sprintf("cannot tell which language %s %q speaks, so %s will be used",
				key, id, voice.DefaultLanguage().Name),
			Fix: "Choose a voice whose language Jarvix knows: jarvix voices"}
	}
	detail := fmt.Sprintf("%s — %s via %s", lang.Name, id, cfg.TTS.Provider)
	if cfg.TTS.Provider == "kokoro" {
		// The phonemiser code is the whole point of the feature, so it is
		// shown: this line is how a user confirms the accent is not being
		// applied on top of American pronunciation.
		detail += fmt.Sprintf(" (phonemiser %s, %s)", lang.Code, voice.GenderForKokoroVoice(id))
	}
	return Result{Status: OK, Name: name, Detail: detail}
}

// checkSpeechLanguage reports whether speech recognition can serve the
// language the voice speaks. It is a Fail rather than a Warn when it cannot:
// an assistant that answers questions it mis-heard is not degraded, it is
// wrong.
func checkSpeechLanguage(cfg config.Config, _ config.Paths) Result {
	const name = "speech recognition language"
	lang, known := cfg.SpokenLanguage()
	if !known {
		lang = voice.DefaultLanguage()
	}
	model := cfg.STT.Whisper.Model
	if !lang.English() && config.EnglishOnlyWhisperModel(model) {
		return Result{Status: Fail, Name: name,
			Detail: fmt.Sprintf("the voice speaks %s but stt.whisper.model %q transcribes English only",
				lang.Name, model),
			Fix: fmt.Sprintf("Install a multilingual model and switch to it together:\n"+
				"  jarvix setup whisper %s\n  jarvix config set stt.whisper.model=%s stt.whisper.language=%s",
				config.MultilingualWhisperModel, config.MultilingualWhisperModel, lang.Whisper)}
	}
	have := strings.ToLower(strings.TrimSpace(cfg.STT.Whisper.Language))
	if !lang.English() && have != lang.Whisper && have != "auto" {
		return Result{Status: Fail, Name: name,
			Detail: fmt.Sprintf("the voice speaks %s but stt.whisper.language is %q",
				lang.Name, cfg.STT.Whisper.Language),
			Fix: "Match it to the voice: jarvix config set stt.whisper.language=" + lang.Whisper}
	}
	return Result{Status: OK, Name: name,
		Detail: fmt.Sprintf("%s (%s) transcribes %s", model, displayLanguage(have), lang.Name)}
}

// checkKokoroHelperLanguage catches the one upgrade hazard this feature
// introduces: an installed kokoro_stream.py older than the --lang flag.
//
// The helper lives outside the repo, copied to ~/.local/share/jarvix by
// setup-kokoro.sh, so upgrading Jarvix does not upgrade it — and a packaged
// upgrade refreshes /usr/share/jarvix, which is not the file the adapter
// runs. The adapter degrades rather than breaking when it finds a stale one
// (it drops --lang and speaks as it always did), which is the kind thing to
// do and also the silent thing: the accent is simply wrong. Doctor is where
// that stops being silent.
func checkKokoroHelperLanguage(cfg config.Config, _ config.Paths) Result {
	const name = "Kokoro helper supports accents"
	if cfg.TTS.Provider != "kokoro" {
		return Result{Status: OK, Name: name, Detail: "not applicable (tts.provider is " + cfg.TTS.Provider + ")"}
	}
	script := (&kokoro.Synthesizer{}).ScriptPath()
	data, err := os.ReadFile(script)
	if err != nil {
		// The helper being absent is already checkTTS's Fail; saying it twice
		// helps nobody.
		return Result{Status: OK, Name: name, Detail: "helper not installed yet"}
	}
	if !strings.Contains(string(data), "--lang") {
		lang, known := cfg.SpokenLanguage()
		detail := script + " predates language selection, so the voice is spoken with " +
			voice.DefaultLanguage().Name + " pronunciation"
		if known && !lang.English() {
			detail = script + " predates language selection, so " + lang.Name +
				" is spoken with " + voice.DefaultLanguage().Name + " pronunciation"
		}
		return Result{Status: Fail, Name: name, Detail: detail,
			Fix: "Re-install the helper: scripts/setup-kokoro.sh"}
	}
	return Result{Status: OK, Name: name, Detail: script}
}

// displayLanguage renders the configured whisper language for the report,
// spelling out what an empty setting actually does.
func displayLanguage(code string) string {
	switch code {
	case "":
		return "language unset, whisper defaults to English"
	case "auto":
		return "auto-detected"
	default:
		return "language " + code
	}
}

func firstOr(list []string, fallback string) string {
	if len(list) > 0 {
		return list[0]
	}
	return fallback
}
