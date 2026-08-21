package voice

import "testing"

// The mapping is the feature. Every voice Kokoro ships begins with its family
// letter, and getting one wrong means a voice speaking someone else's
// phonemes — which is the exact defect this package exists to make
// impossible. So the whole table is pinned, both directions, against the nine
// families the installed voices file actually contains.

func TestLanguageForKokoroVoiceCoversEveryFamily(t *testing.T) {
	cases := []struct {
		voice   string
		code    string
		name    string
		whisper string
		piper   string
		gender  Gender
	}{
		{"af_heart", "en-us", "English (American)", "en", "en_US", Female},
		{"am_adam", "en-us", "English (American)", "en", "en_US", Male},
		{"bf_emma", "en-gb", "English (British)", "en", "en_GB", Female},
		{"bm_george", "en-gb", "English (British)", "en", "en_GB", Male},
		{"ef_dora", "es", "Spanish", "es", "es_ES", Female},
		{"em_alex", "es", "Spanish", "es", "es_ES", Male},
		{"ff_siwis", "fr-fr", "French", "fr", "fr_FR", Female},
		{"hf_alpha", "hi", "Hindi", "hi", "hi_IN", Female},
		{"hm_omega", "hi", "Hindi", "hi", "hi_IN", Male},
		{"if_sara", "it", "Italian", "it", "it_IT", Female},
		{"im_nicola", "it", "Italian", "it", "it_IT", Male},
		{"jf_alpha", "ja", "Japanese", "ja", "ja_JP", Female},
		{"jm_kumo", "ja", "Japanese", "ja", "ja_JP", Male},
		{"pf_dora", "pt-br", "Portuguese (Brazilian)", "pt", "pt_BR", Female},
		{"pm_alex", "pt-br", "Portuguese (Brazilian)", "pt", "pt_BR", Male},
		{"zf_xiaobei", "zh", "Chinese (Mandarin)", "zh", "zh_CN", Female},
		{"zm_yunjian", "zh", "Chinese (Mandarin)", "zh", "zh_CN", Male},
	}
	for _, tc := range cases {
		t.Run(tc.voice, func(t *testing.T) {
			lang, ok := LanguageForKokoroVoice(tc.voice)
			if !ok {
				t.Fatalf("%s: no language derived", tc.voice)
			}
			if lang.Code != tc.code {
				t.Errorf("phonemiser code = %q, want %q", lang.Code, tc.code)
			}
			if lang.Name != tc.name {
				t.Errorf("name = %q, want %q", lang.Name, tc.name)
			}
			// whisper.cpp has no notion of accent: both English families must
			// resolve to plain "en", or transcription fails on a code it has
			// never heard of.
			if lang.Whisper != tc.whisper {
				t.Errorf("whisper code = %q, want %q", lang.Whisper, tc.whisper)
			}
			if lang.Piper != tc.piper {
				t.Errorf("piper locale = %q, want %q", lang.Piper, tc.piper)
			}
			if g := GenderForKokoroVoice(tc.voice); g != tc.gender {
				t.Errorf("gender = %v, want %v", g, tc.gender)
			}
		})
	}
}

// The two English families must be distinguishable, because "same language,
// different phonemiser" is precisely the case that was broken.
func TestBritishAndAmericanAreDifferentLanguagesToKokoroAndTheSameToWhisper(t *testing.T) {
	gb, _ := LanguageForKokoroVoice("bf_emma")
	us, _ := LanguageForKokoroVoice("af_heart")
	if gb.Code == us.Code {
		t.Fatalf("bf_emma and af_heart share phonemiser code %q", gb.Code)
	}
	if gb.Whisper != us.Whisper {
		t.Errorf("whisper codes differ: %q vs %q — whisper.cpp has no accents", gb.Whisper, us.Whisper)
	}
	if !gb.English() || !us.English() {
		t.Error("both English families must count as English for speech recognition")
	}
}

func TestEveryLanguageIsCompleteAndUnique(t *testing.T) {
	seenCode := map[string]bool{}
	seenPrefix := map[byte]bool{}
	for _, l := range Languages {
		if l.Code == "" || l.Name == "" || l.Whisper == "" || l.Piper == "" || l.Sample == "" {
			t.Errorf("incomplete language entry: %+v", l)
		}
		if seenCode[l.Code] {
			t.Errorf("duplicate language code %q", l.Code)
		}
		if seenPrefix[l.kokoroPrefix] {
			t.Errorf("duplicate Kokoro prefix %q", l.kokoroPrefix)
		}
		seenCode[l.Code], seenPrefix[l.kokoroPrefix] = true, true
		if _, ok := LanguageByCode(l.Code); !ok {
			t.Errorf("LanguageByCode cannot find %q", l.Code)
		}
	}
	if len(Languages) != 9 {
		t.Errorf("Languages has %d entries; Kokoro ships 9 families", len(Languages))
	}
}

func TestUnknownVoiceIdsDeriveNothing(t *testing.T) {
	for _, id := range []string{"", "x", "qf_nobody", "afheart", "af_", "AF_HEART"} {
		if lang, ok := LanguageForKokoroVoice(id); ok {
			t.Errorf("%q derived %q; an unrecognised id must constrain nothing", id, lang.Code)
		}
	}
}

// Falling back rather than refusing matters: an id that says nothing about its
// language is still a voice somebody installed, and silence is a worse answer
// than a default accent.
func TestDefaultLanguageIsTheDefaultVoicesLanguage(t *testing.T) {
	want, _ := LanguageForKokoroVoice("af_heart")
	if got := DefaultLanguage(); got.Code != want.Code {
		t.Errorf("DefaultLanguage = %q, want %q", got.Code, want.Code)
	}
}

func TestLanguageForPiperVoice(t *testing.T) {
	cases := []struct {
		in   string
		code string
		ok   bool
	}{
		{"en_US-amy-medium", "en-us", true},
		{"en_GB-alan-low", "en-gb", true},
		{"/usr/share/piper-voices/en/en_GB/alba/medium/en_GB-alba-medium.onnx", "en-gb", true},
		{"fr_FR-siwis-medium", "fr-fr", true},
		{"es_MX-claude-high", "es", true},   // a regional variant of a known family
		{"zh_CN-huayan-medium", "zh", true}, //nolint:misspell // a Piper voice name
		// No Kokoro family matches European Portuguese, and calling it
		// Brazilian to make the table total would put a wrong answer in front
		// of the user.
		{"pt_PT-tugao-medium", "", false},
		{"", "", false},
		{"nonsense", "", false},
	}
	for _, tc := range cases {
		lang, ok := LanguageForPiperVoice(tc.in)
		if ok != tc.ok || lang.Code != tc.code {
			t.Errorf("LanguageForPiperVoice(%q) = %q/%v, want %q/%v", tc.in, lang.Code, ok, tc.code, tc.ok)
		}
	}
}

func TestPiperPackageNamesTheInstallableThing(t *testing.T) {
	gb, _ := LanguageByCode("en-gb")
	if got := gb.PiperPackage(); got != "piper-voices-en-gb" {
		t.Errorf("PiperPackage = %q", got)
	}
}

func TestParseKokoroVoiceDescribesTheSpeaker(t *testing.T) {
	v, ok := ParseKokoroVoice("bm_george")
	if !ok {
		t.Fatal("bm_george did not parse")
	}
	if v.ID != "bm_george" || v.Name != "George" || v.Gender != Male || v.Language.Code != "en-gb" {
		t.Errorf("voice = %+v", v)
	}
}

func TestGenderString(t *testing.T) {
	for g, want := range map[Gender]string{Female: "female", Male: "male", GenderUnknown: "unknown"} {
		if got := g.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", g, got, want)
		}
	}
}
