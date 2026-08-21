package setup

import (
	"errors"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/voice"
)

// The wizard is the "switches" half of the requirement: config validation
// refuses a non-English voice paired with an English-only model, and this is
// where it can instead be fixed, because here the user can simply be asked.
// Every dependency is injected, so nothing here reads an archive or loads a
// model.

// kokoroVoiceDeps builds a step over a fake catalog of the British and
// American families plus one French voice.
func kokoroVoiceDeps(t *testing.T, f *File, out *strings.Builder, p Prompter) VoiceDeps {
	t.Helper()
	catalog := voice.FakeKokoro("af_heart", "am_adam", "bf_emma", "bf_alice", "bm_george", "ff_siwis")
	return VoiceDeps{
		File: f, Out: out, Prompt: p,
		Provider:     func() string { return "kokoro" },
		Catalog:      func(string) voice.Catalog { return catalog },
		Current:      func(string) string { return "af_heart" },
		WhisperModel: "base.en",
	}
}

// The headline flow: language, then accent, then it is written.
func TestVoiceStepChoosesLanguageThenAccent(t *testing.T) {
	f := loadString(t, "")
	var out strings.Builder
	// Choice 1: English (British) is second in the language menu.
	// Choice 2: the second British voice offered.
	p := &fakePrompter{choices: []int{1, 1}}
	if err := VoiceStep(kokoroVoiceDeps(t, f, &out, p)).Run(); err != nil {
		t.Fatal(err)
	}
	got, ok := f.Get("tts.kokoro", "voice")
	if !ok || !strings.HasPrefix(got, "b") {
		t.Fatalf("voice = %q, want a British one", got)
	}
	if !strings.Contains(p.asked[0], "language") {
		t.Errorf("language was not asked first: %q", p.asked[0])
	}
	if !strings.Contains(p.asked[1], "English (British)") {
		t.Errorf("accent was not scoped to the chosen language: %q", p.asked[1])
	}
	// A British voice is still English to whisper, so nothing about speech
	// recognition should have been touched.
	if _, changed := f.Get("stt.whisper", "model"); changed {
		t.Error("a British voice must not demand a new speech model")
	}
}

// The menu is the discovery surface: every voice, with its gender, under the
// language it belongs to.
func TestVoiceStepOffersGendersAndCounts(t *testing.T) {
	f := loadString(t, "")
	var out strings.Builder
	p := &recordingPrompter{fakePrompter: fakePrompter{choices: []int{1, 0}}}
	if err := VoiceStep(kokoroVoiceDeps(t, f, &out, p)).Run(); err != nil {
		t.Fatal(err)
	}
	languageMenu := strings.Join(p.options[0], "\n")
	for _, want := range []string{"English (American) (en-us) — 2 voice(s)", "English (British) (en-gb) — 3 voice(s)", "Hindi (hi) — none installed"} {
		if !strings.Contains(languageMenu, want) {
			t.Errorf("language menu missing %q:\n%s", want, languageMenu)
		}
	}
	voiceMenu := strings.Join(p.options[1], "\n")
	for _, want := range []string{"bf_alice — Alice (female)", "bm_george — George (male)"} {
		if !strings.Contains(voiceMenu, want) {
			t.Errorf("voice menu missing %q:\n%s", want, voiceMenu)
		}
	}
}

// Hearing it is the only way to choose between four British voices, so the
// preview loops until the user says yes rather than being a confirmation they
// can only decline.
func TestVoiceStepPreviewsAloudAndRepeatsUntilAccepted(t *testing.T) {
	f := loadString(t, "")
	var out strings.Builder
	var previewed []string
	p := &fakePrompter{
		choices: []int{1, 0, 2},
		// hear it? yes / use it? no / hear it? yes / use it? yes
		confirms: []bool{true, false, true, true},
	}
	d := kokoroVoiceDeps(t, f, &out, p)
	d.Preview = func(id string) error {
		previewed = append(previewed, id)
		return nil
	}
	if err := VoiceStep(d).Run(); err != nil {
		t.Fatal(err)
	}
	if len(previewed) != 2 {
		t.Fatalf("previewed = %v; a rejected preview must lead back to the menu", previewed)
	}
	got, _ := f.Get("tts.kokoro", "voice")
	if got != previewed[1] {
		t.Errorf("wrote %q but last previewed %q", got, previewed[1])
	}
}

func TestVoiceStepSurvivesAPreviewThatCannotPlay(t *testing.T) {
	f := loadString(t, "")
	var out strings.Builder
	p := &fakePrompter{choices: []int{1, 0}, confirms: []bool{true}}
	d := kokoroVoiceDeps(t, f, &out, p)
	d.Preview = func(string) error { return errors.New("no audio sink") }
	if err := VoiceStep(d).Run(); err != nil {
		t.Fatal(err)
	}
	if got, ok := f.Get("tts.kokoro", "voice"); !ok || got == "" {
		t.Error("a failed preview must not block the choice")
	}
	if !strings.Contains(out.String(), "no audio sink") {
		t.Errorf("the reason was not shown: %s", out.String())
	}
}

// The whole second-order value of the ticket: picking French in the wizard
// leaves a working French assistant, not one that speaks French and listens
// in English.
func TestNonEnglishChoiceSwitchesSpeechRecognitionToo(t *testing.T) {
	f := loadString(t, "")
	var out strings.Builder
	// Language menu index 3 is French; then the only French voice; then
	// "yes" to the multilingual model.
	p := &fakePrompter{choices: []int{3, 0}, confirms: []bool{true}}
	d := kokoroVoiceDeps(t, f, &out, p)
	downloaded := ""
	d.DownloadModel = func(model string) error {
		downloaded = model
		return nil
	}
	if err := VoiceStep(d).Run(); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.Get("tts.kokoro", "voice"); got != "ff_siwis" {
		t.Fatalf("voice = %q", got)
	}
	if got, _ := f.Get("stt.whisper", "model"); got != "base" {
		t.Errorf("whisper model = %q, want the multilingual base", got)
	}
	if got, _ := f.Get("stt.whisper", "language"); got != "fr" {
		t.Errorf("whisper language = %q, want fr", got)
	}
	if downloaded != "base" {
		t.Errorf("the model was not fetched: %q", downloaded)
	}
}

// Declining is allowed, but it must not look like it worked: the step fails
// so the wizard's closing summary lists it, and it prints the two commands
// that finish the job.
func TestDecliningTheMultilingualModelFailsLoudly(t *testing.T) {
	f := loadString(t, "")
	var out strings.Builder
	p := &fakePrompter{choices: []int{3, 0}, confirms: []bool{false}}
	err := VoiceStep(kokoroVoiceDeps(t, f, &out, p)).Run()
	if err == nil || !strings.Contains(err.Error(), "French") {
		t.Fatalf("err = %v", err)
	}
	printed := out.String()
	for _, want := range []string{"jarvix setup whisper base", "stt.whisper.language=fr"} {
		if !strings.Contains(printed, want) {
			t.Errorf("output missing %q:\n%s", want, printed)
		}
	}
	if _, changed := f.Get("stt.whisper", "language"); changed {
		t.Error("a declined change must not be written anyway")
	}
}

// A language the engine cannot speak yet is a chance to say what to install,
// which is the Piper half of the requirement.
func TestPiperLanguageWithNoInstalledVoiceNamesThePackage(t *testing.T) {
	f := loadString(t, "")
	var out strings.Builder
	p := &fakePrompter{choices: []int{1}} // English (British)
	d := VoiceDeps{
		File: f, Out: &out, Prompt: p,
		Provider: func() string { return "piper" },
		Catalog: func(string) voice.Catalog {
			return voice.Fake{List: []voice.Voice{{ID: "en_US-amy-medium", Name: "Amy", Language: usLanguage(t)}}}
		},
		Current:      func(string) string { return "en_US-amy-medium" },
		WhisperModel: "base.en",
	}
	if err := VoiceStep(d).Run(); err != nil {
		t.Fatal(err)
	}
	printed := out.String()
	if !strings.Contains(printed, "piper-voices-en-gb") {
		t.Errorf("the voice package was not named:\n%s", printed)
	}
	if _, written := f.Get("tts.piper", "voice"); written {
		t.Error("a voice that is not installed must not be written")
	}
}

// Kokoro ships every language in one archive, so a missing language there
// means the archive is missing — a different instruction entirely.
func TestKokoroMissingLanguagePointsAtTheSetupScript(t *testing.T) {
	f := loadString(t, "")
	var out strings.Builder
	p := &fakePrompter{choices: []int{4}} // Hindi, absent from the fake catalog
	if err := VoiceStep(kokoroVoiceDeps(t, f, &out, p)).Run(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "setup-kokoro.sh") {
		t.Errorf("output = %s", out.String())
	}
}

func TestVoiceStepWithNoCatalogExplainsItselfWithoutFailing(t *testing.T) {
	f := loadString(t, "")
	var out strings.Builder
	d := kokoroVoiceDeps(t, f, &out, &fakePrompter{})
	d.Catalog = func(string) voice.Catalog { return voice.Fake{Err: errors.New("run scripts/setup-kokoro.sh")} }
	// Not an error: the engine's own readiness is the previous step's job,
	// and failing twice for one cause helps nobody.
	if err := VoiceStep(d).Run(); err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(out.String(), "setup-kokoro.sh") {
		t.Errorf("output = %s", out.String())
	}
}

// "Keep the current voice" is the last option and changes nothing.
func TestVoiceStepCanBeDeclinedEntirely(t *testing.T) {
	f := loadString(t, "")
	var out strings.Builder
	p := &fakePrompter{choices: []int{len(voice.Languages)}}
	if err := VoiceStep(kokoroVoiceDeps(t, f, &out, p)).Run(); err != nil {
		t.Fatal(err)
	}
	if _, written := f.Get("tts.kokoro", "voice"); written {
		t.Error("declining wrote a voice anyway")
	}
}

// The shipped default is not a decision: a step that counted it as one would
// be skipped for exactly the users who have never seen it.
func TestVoiceStepIsOnlyDoneOnceAVoiceIsWritten(t *testing.T) {
	var out strings.Builder
	unwritten := kokoroVoiceDeps(t, loadString(t, ""), &out, &fakePrompter{})
	if done, _ := VoiceStep(unwritten).Done(); done {
		t.Error("an untouched config counted as configured")
	}
	written := kokoroVoiceDeps(t, loadString(t, "[tts.kokoro]\nvoice = \"bf_emma\"\n"), &out, &fakePrompter{})
	written.Current = func(string) string { return "bf_emma" }
	done, detail := VoiceStep(written).Done()
	if !done || !strings.Contains(detail, "English (British)") {
		t.Errorf("done = %v, detail = %q", done, detail)
	}
}

func usLanguage(t *testing.T) voice.Language {
	t.Helper()
	l, ok := voice.LanguageByCode("en-us")
	if !ok {
		t.Fatal("no en-us language")
	}
	return l
}

// recordingPrompter also keeps the options it was shown, so a test can assert
// what the menu actually said rather than only which index was picked.
type recordingPrompter struct {
	fakePrompter
	options [][]string
}

func (r *recordingPrompter) Choose(question string, options []string, def int) int {
	r.options = append(r.options, append([]string(nil), options...))
	return r.fakePrompter.Choose(question, options, def)
}
