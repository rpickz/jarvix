// Package voice answers one question the rest of Jarvix keeps needing: what
// language does this voice actually speak?
//
// It exists because a voice is two things at once, and Jarvix used to conflate
// them. A voice has a *timbre* — whose vocal cords it imitates — and it has a
// *phonemisation*, the rules that turn letters into sounds. Kokoro takes them
// as separate arguments, and the helper passed `lang="en-us"` for every voice
// ever configured. Select the British `bf_emma` and you got British timbre
// speaking American English: rhotic R's, T's flapped to D's, "mobile" rhyming
// with "noble". Half-right is worse than either half, because it lands in the
// uncanny valley of an accent that does not exist.
//
// So language is *derived*, never configured next to the voice and never
// hardcoded. A Kokoro voice id encodes its family in the first character, so
// the derivation is total and cheap, and every consumer — the synthesis
// helper, config validation, whisper model selection, the setup wizard,
// doctor — reads it from here rather than inventing its own rule.
//
// The second-order reason this package is engine-agnostic: language is not a
// TTS concern alone. Whisper is pinned to the English-only `base.en` model by
// default, so choosing a French voice while leaving speech recognition on
// `base.en` produces an assistant that speaks French and hears English —
// which presents as broken, not as misconfigured. Both engines resolve their
// language through the same table, so they cannot silently disagree.
package voice

import (
	"path/filepath"
	"strings"
)

// Gender is the speaker's voice type, as encoded in a Kokoro voice id. It is
// listed because "bm_george" tells a user nothing; "George — male, British"
// tells them everything they need to choose.
type Gender int

// Voice genders. Unknown is not a failure: Piper voice names carry no gender
// at all, and a catalog that says so is more honest than one that guesses.
const (
	GenderUnknown Gender = iota
	Female
	Male
)

// String renders a gender for display and JSON.
func (g Gender) String() string {
	switch g {
	case Female:
		return "female"
	case Male:
		return "male"
	default:
		return "unknown"
	}
}

// Language is one language/accent Jarvix can speak and hear, with the code
// each engine wants for it.
//
// The three codes differ on purpose and must not be collapsed into one. Code
// is what Kokoro's phonemiser expects (`en-gb`), Whisper is the ISO-639-1 code
// whisper.cpp expects (`en` — it has no notion of accent, and asking it for
// "en-gb" fails), and Piper is the locale prefix that begins a Piper voice
// file name (`en_GB-alan-medium`). Deriving one from another by string surgery
// is how a pipeline ends up asking whisper.cpp for a language it has never
// heard of.
type Language struct {
	// Code is the phonemiser language code — the value that reaches Kokoro
	// as `lang=`, and the identity of this language everywhere in Jarvix.
	Code string
	// Name is the human label used by menus, doctor, and `jarvix voices`.
	Name string
	// Whisper is the speech-recognition language code (ISO 639-1).
	Whisper string
	// Piper is the locale prefix of the equivalent Piper voice names.
	Piper string
	// Sample is a short greeting in this language, spoken as the wizard's
	// preview. It is in-language deliberately: the point of the preview is to
	// hear the phonemiser, and English words rendered by a French phonemiser
	// prove nothing about how French will sound.
	Sample string
	// kokoroPrefix is the first character of a Kokoro voice id in this family.
	kokoroPrefix byte
}

// Valid reports whether this is a real language rather than the zero value,
// which is what a lookup miss returns.
func (l Language) Valid() bool { return l.Code != "" }

// English reports whether speech recognition can serve this language with an
// English-only whisper model.
func (l Language) English() bool { return l.Whisper == "en" }

// PiperPackage names the Arch/AUR package that ships Piper voices for this
// language, for the "install this to get that accent" message.
func (l Language) PiperPackage() string {
	if l.Piper == "" {
		return ""
	}
	return "piper-voices-" + strings.ToLower(strings.ReplaceAll(l.Piper, "_", "-"))
}

// Languages are the voice families Kokoro ships, in the order menus and
// listings present them: the two English accents first because they are what
// most users are choosing between, then the rest alphabetically by name.
//
// This is the whole mapping the ticket pins with a table test. Kokoro's voice
// ids begin with the family letter — a, b, e, f, h, i, j, p, z — and that
// letter is the only evidence of language a voice file carries, so it is the
// only place the derivation can come from.
var Languages = []Language{
	{kokoroPrefix: 'a', Code: "en-us", Name: "English (American)", Whisper: "en", Piper: "en_US",
		Sample: "Hello, I'm Jarvix. Ask me anything."},
	{kokoroPrefix: 'b', Code: "en-gb", Name: "English (British)", Whisper: "en", Piper: "en_GB",
		Sample: "Hello, I'm Jarvix. Shall we get started?"},
	{kokoroPrefix: 'z', Code: "zh", Name: "Chinese (Mandarin)", Whisper: "zh", Piper: "zh_CN",
		Sample: "你好，我是贾维斯。"},
	{kokoroPrefix: 'f', Code: "fr-fr", Name: "French", Whisper: "fr", Piper: "fr_FR",
		Sample: "Bonjour, je suis Jarvix. Comment puis-je vous aider ?"},
	{kokoroPrefix: 'h', Code: "hi", Name: "Hindi", Whisper: "hi", Piper: "hi_IN",
		Sample: "नमस्ते, मैं जार्विक्स हूँ।"},
	{kokoroPrefix: 'i', Code: "it", Name: "Italian", Whisper: "it", Piper: "it_IT",
		Sample: "Ciao, sono Jarvix. Come posso aiutarti?"},
	{kokoroPrefix: 'j', Code: "ja", Name: "Japanese", Whisper: "ja", Piper: "ja_JP",
		Sample: "こんにちは、ジャービックスです。"},
	{kokoroPrefix: 'p', Code: "pt-br", Name: "Portuguese (Brazilian)", Whisper: "pt", Piper: "pt_BR",
		Sample: "Olá, eu sou o Jarvix. Como posso ajudar?"},
	{kokoroPrefix: 'e', Code: "es", Name: "Spanish", Whisper: "es", Piper: "es_ES",
		Sample: "Hola, soy Jarvix. ¿En qué puedo ayudarte?"},
}

// DefaultLanguage is what Jarvix speaks when nothing says otherwise — the
// language of the shipped default voice (af_heart), so the derivation and the
// default can never drift apart.
func DefaultLanguage() Language {
	lang, _ := LanguageByCode("en-us")
	return lang
}

// LanguageByCode looks a language up by its phonemiser code ("en-gb").
func LanguageByCode(code string) (Language, bool) {
	for _, l := range Languages {
		if l.Code == code {
			return l, true
		}
	}
	return Language{}, false
}

// LanguageForKokoroVoice derives the phonemiser language from a Kokoro voice
// id. This is the fix for the core defect: the language reaching the helper is
// a function of the voice, so a British voice cannot be phonemised as
// American however the config is edited.
//
// Kokoro ids are `<language><gender>_<name>` — "bf_emma" is British, female,
// Emma. An id that does not fit that shape yields no language rather than a
// guess: callers treat "unknown" as "impose no constraint", which is what
// keeps a hand-installed custom voice from being rejected out of hand.
func LanguageForKokoroVoice(id string) (Language, bool) {
	if !looksLikeKokoroID(id) {
		return Language{}, false
	}
	for _, l := range Languages {
		if l.kokoroPrefix == id[0] {
			return l, true
		}
	}
	return Language{}, false
}

// GenderForKokoroVoice reads the gender character of a Kokoro voice id.
func GenderForKokoroVoice(id string) Gender {
	if !looksLikeKokoroID(id) {
		return GenderUnknown
	}
	switch id[1] {
	case 'f':
		return Female
	case 'm':
		return Male
	}
	return GenderUnknown
}

// looksLikeKokoroID reports whether id has the `xy_name` shape the family and
// gender characters live in.
func looksLikeKokoroID(id string) bool {
	return len(id) > 3 && id[2] == '_'
}

// ParseKokoroVoice turns a voice id into a described Voice. It never touches
// the filesystem — it is pure naming — so it is also what the archive reader
// uses on each entry it finds.
func ParseKokoroVoice(id string) (Voice, bool) {
	lang, ok := LanguageForKokoroVoice(id)
	if !ok {
		return Voice{}, false
	}
	return Voice{
		ID:       id,
		Name:     displayName(id[3:]),
		Language: lang,
		Gender:   GenderForKokoroVoice(id),
	}, true
}

// LanguageForPiperVoice derives the language of a Piper voice from its name or
// model path, so the choice of accent is not a Kokoro-only feature.
//
// Piper names are `<locale>-<speaker>-<quality>` ("en_GB-alan-medium"), and
// `tts.piper.voice` may also be an absolute path to the .onnx file, which is
// why the base name is taken first. A locale outside the table — pt_PT, say,
// which no Kokoro family matches — yields no language rather than the nearest
// one: pretending pt_PT is Brazilian would quietly hand whisper the wrong
// hint, and "unknown" costs nothing because callers impose no constraint.
func LanguageForPiperVoice(nameOrPath string) (Language, bool) {
	name := strings.TrimSuffix(filepath.Base(strings.TrimSpace(nameOrPath)), ".onnx")
	locale, _, _ := strings.Cut(name, "-")
	if locale == "" {
		return Language{}, false
	}
	for _, l := range Languages {
		if strings.EqualFold(l.Piper, locale) {
			return l, true
		}
	}
	// Regional variants of a language Kokoro *does* have a family for still
	// resolve to it: es_MX is Spanish, and recognising that is the difference
	// between a working config and a spurious validation failure. Variants of
	// an accent Kokoro has no family for (en_AU) are deliberately absent —
	// calling an Australian voice British to make the table total would put a
	// wrong answer in front of the user in `jarvix doctor`, and "unknown"
	// costs nothing.
	if code, ok := piperVariants[strings.ToLower(locale)]; ok {
		return LanguageByCode(code)
	}
	return Language{}, false
}

// piperVariants maps regional Piper locales onto the language family they
// belong to. Keys are lower-cased for case-insensitive lookup.
var piperVariants = map[string]string{
	"es_mx": "es", "es_ar": "es", "es_419": "es",
	"fr_ca": "fr-fr", "fr_be": "fr-fr",
	"zh_tw": "zh",
}

// displayName turns the name half of a voice id into something a menu can
// show: "gongitsune" → "Gongitsune".
func displayName(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
