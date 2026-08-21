package config

// This file makes language one setting instead of two unrelated ones.
//
// Jarvix has two engines with an opinion about language: the synthesizer,
// whose voice determines what comes out, and whisper.cpp, whose model and
// language setting determine what goes in. Nothing used to connect them. A
// user who found `bf_emma` in the voices file got British speech and American
// phonemisation; a user who found `ff_siwis` got French speech and an
// assistant still transcribing their French as English — which does not look
// like a misconfiguration, it looks like a broken assistant.
//
// So the language is derived from the voice (never configured beside it,
// never hardcoded), and the configuration is rejected when speech recognition
// cannot serve that language. Refusal is deliberately preferred to silent
// correction: changing someone's whisper model behind their back would
// download hundreds of megabytes they did not ask for. The message names the
// model to install and the exact command that sets both halves at once.

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/rpickz/jarvix/internal/voice"
)

// KokoroVoicesFile returns the Kokoro voices archive under this installation's
// data directory — the file that says which voices the machine can speak.
func (p Paths) KokoroVoicesFile() string { return voice.KokoroVoicesFile(p.Data) }

// InstalledVoices builds the catalog for the configured TTS engine. It reads
// nothing until asked and caches the result for its own lifetime, so handing
// one to a long-lived daemon costs a single archive read, not one per
// validation.
func (c Config) InstalledVoices(paths Paths) voice.Catalog {
	if c.TTS.Provider == "kokoro" {
		return &voice.KokoroArchive{Path: paths.KokoroVoicesFile()}
	}
	return &voice.PiperDir{Dirs: PiperVoiceDirs()}
}

// PiperVoiceDirs are the directories Piper voice packages install into. It
// mirrors piper.DefaultVoiceDirs; config cannot import the adapter without
// dragging the whole TTS stack into every consumer of configuration, and two
// short lists are cheaper than that coupling.
func PiperVoiceDirs() []string {
	return []string{"/usr/share/piper-voices", "/usr/local/share/piper-voices"}
}

// SpokenLanguage reports the language the configured voice actually speaks,
// derived from the voice id itself for whichever engine is selected.
//
// The second return is false when the voice says nothing about its language —
// a Piper locale outside the table, a hand-installed Kokoro voice, an absolute
// model path with a name of its own. Unknown means "impose no constraint":
// Jarvix will not reject a configuration it cannot reason about.
func (c Config) SpokenLanguage() (voice.Language, bool) {
	if c.TTS.Provider == "kokoro" {
		return voice.LanguageForKokoroVoice(c.TTS.Kokoro.Voice)
	}
	return voice.LanguageForPiperVoice(c.TTS.Piper.Voice)
}

// voiceProblems validates the voice and the language it implies. It is called
// from Validate alongside the other section validators.
func (c Config) voiceProblems() []string {
	var problems []string
	problems = append(problems, c.installedVoiceProblems()...)
	if lang, ok := c.SpokenLanguage(); ok {
		problems = append(problems, c.speechLanguageProblems(lang)...)
	}
	return problems
}

// installedVoiceProblems rejects a voice this machine does not have, naming
// alternatives it does.
//
// The value of catching it here rather than at synthesis time is the timing: a
// wrong voice id otherwise surfaces as a failed answer, seconds after a
// question, with the helper's error rather than the user's mistake in it. And
// the value of naming alternatives is that the ids are unguessable — nobody
// invents "bm_george" from first principles.
//
// A missing catalog is not a problem. Validation runs on machines where Kokoro
// was never installed (the Piper default) and inside tests that must not read
// a 27 MB archive, so "cannot tell" always means "do not object" — doctor is
// where a missing engine is reported.
//
// Only Kokoro is checked. Its voices are a closed set shipped in one archive,
// so "installed or not" is a fact. Piper's are open: any .onnx from the
// upstream collection, at any path the user points tts.piper.voice at, from
// packages that differ by distribution. Whitelisting against whatever happens
// to be in /usr/share/piper-voices would refuse working configurations more
// often than it caught broken ones — so Piper keeps resolving its voice from
// the filesystem, and doctor reports a miss with the package to install.
func (c Config) installedVoiceProblems() []string {
	if c.Voices == nil || c.TTS.Provider != "kokoro" {
		return nil
	}
	id, key := c.TTS.Kokoro.Voice, "tts.kokoro.voice"
	if id == "" {
		return nil
	}
	installed, err := c.Voices.Voices()
	if err != nil || len(installed) == 0 {
		return nil
	}
	if voice.Has(installed, id) {
		return nil
	}
	alternatives := voice.Suggest(installed, id, 4)
	return []string{fmt.Sprintf(
		"%s %q is not installed on this machine; installed voices include %s (`jarvix voices` lists all %d)",
		key, id, strings.Join(alternatives, ", "), len(installed))}
}

// speechLanguageProblems refuses a configuration where Jarvix would speak one
// language and listen in another.
//
// Two ways that happens. The whisper model may be English-only — `base.en` is
// the shipped default, and it does not become multilingual by being asked
// nicely — in which case no language setting can save it and a different model
// must be installed. Or the model is multilingual but `stt.whisper.language`
// still says "en", which whisper obeys: it will transcribe French audio into
// English words rather than detect the mistake.
func (c Config) speechLanguageProblems(lang voice.Language) []string {
	if lang.English() {
		// An English voice with the default English recognition is the
		// overwhelmingly common case and needs no further opinion; a user who
		// deliberately speaks another language to an English voice is not
		// making a mistake this code should overrule.
		return nil
	}
	model := strings.TrimSpace(c.STT.Whisper.Model)
	if EnglishOnlyWhisperModel(model) {
		return []string{fmt.Sprintf(
			"the configured voice speaks %s, but stt.whisper.model %q only transcribes English, "+
				"so Jarvix would answer questions it mis-heard; install a multilingual model "+
				"(`jarvix setup whisper %s`) and set both together: "+
				"`jarvix config set stt.whisper.model=%s stt.whisper.language=%s`",
			lang.Name, model, MultilingualWhisperModel, MultilingualWhisperModel, lang.Whisper)}
	}
	have := strings.ToLower(strings.TrimSpace(c.STT.Whisper.Language))
	if have == lang.Whisper || have == "auto" {
		return nil
	}
	if have == "" {
		return []string{fmt.Sprintf(
			"the configured voice speaks %s, but stt.whisper.language is empty, so whisper.cpp "+
				"defaults to English; set it: `jarvix config set stt.whisper.language=%s` (or \"auto\")",
			lang.Name, lang.Whisper)}
	}
	return []string{fmt.Sprintf(
		"the configured voice speaks %s, but stt.whisper.language is %q, so Jarvix would speak one "+
			"language and listen in another; set it: `jarvix config set stt.whisper.language=%s` (or \"auto\")",
		lang.Name, have, lang.Whisper)}
}

// MultilingualWhisperModel is the model recommended when a non-English
// language is selected: the smallest whisper.cpp model that is not
// English-only, so the recommendation costs a download of the same order as
// the base.en it replaces rather than a gigabyte.
const MultilingualWhisperModel = "base"

// EnglishOnlyWhisperModel reports whether a whisper.cpp model can only
// transcribe English.
//
// whisper.cpp encodes this in the name and nowhere else: the ".en" models
// (tiny.en, base.en, small.en) are trained on English alone, and every other
// model is multilingual. The check covers absolute paths too, because
// stt.whisper.model may name a ggml file directly and "ggml-base.en.bin" is
// exactly as English-only as "base.en".
func EnglishOnlyWhisperModel(model string) bool {
	name := strings.ToLower(filepath.Base(strings.TrimSpace(model)))
	if name == "" {
		return false
	}
	name = strings.TrimSuffix(name, ".bin")
	return strings.HasSuffix(name, ".en")
}
