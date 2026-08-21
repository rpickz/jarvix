package voice

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The catalog is exercised against a real zip archive built here, not against
// the 27 MB one setup-kokoro.sh downloads: the format is a zip of one entry
// per voice, and a four-entry zip proves the reader as well as a 54-entry one.
// Nothing in this package may need the installed engine, and nothing here does.

// writeVoicesArchive builds a stand-in for voices-v1.0.bin.
func writeVoicesArchive(t *testing.T, entries ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "voices-v1.0.bin")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	for _, name := range entries {
		e, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.Write([]byte("embedding")); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestKokoroArchiveListsInstalledVoicesGroupedByLanguage(t *testing.T) {
	path := writeVoicesArchive(t,
		"bm_george.npy", "af_heart.npy", "bf_emma.npy", "ff_siwis.npy", "am_adam.npy",
		// Not a voice: skipped rather than guessed at.
		"README.txt",
	)
	a := &KokoroArchive{Path: path}
	voices, err := a.Voices()
	if err != nil {
		t.Fatal(err)
	}
	if len(voices) != 5 {
		t.Fatalf("got %d voices: %+v", len(voices), voices)
	}

	groups := Grouped(voices)
	var got []string
	for _, g := range groups {
		ids := make([]string, 0, len(g.Voices))
		for _, v := range g.Voices {
			ids = append(ids, v.ID+"/"+v.Gender.String())
		}
		got = append(got, g.Language.Name+": "+strings.Join(ids, ","))
	}
	want := []string{
		"English (American): af_heart/female,am_adam/male",
		"English (British): bf_emma/female,bm_george/male",
		"French: ff_siwis/female",
	}
	if len(got) != len(want) {
		t.Fatalf("groups = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("group %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// The performance requirement in one assertion: enumeration reads the voices
// archive and nothing else. A 310 MB ONNX model sitting next to it is never
// opened, so listing voices costs a directory read rather than a model load.
func TestKokoroArchiveNeverTouchesTheModel(t *testing.T) {
	path := writeVoicesArchive(t, "af_heart.npy")
	model := filepath.Join(filepath.Dir(path), "kokoro-v1.0.onnx")
	if err := os.WriteFile(model, []byte("not a real model"), 0o000); err != nil {
		t.Fatal(err)
	}
	// The model file is unreadable; a catalog that opened it would fail.
	if _, err := (&KokoroArchive{Path: path}).Voices(); err != nil {
		t.Fatalf("enumeration touched something it should not: %v", err)
	}
}

// One read per catalog, however many times it is asked — the daemon holds one
// for its lifetime and a settings screen may ask on every render.
func TestKokoroArchiveCachesForItsLifetime(t *testing.T) {
	path := writeVoicesArchive(t, "af_heart.npy", "bf_emma.npy")
	a := &KokoroArchive{Path: path}
	first, err := a.Voices()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	second, err := a.Voices()
	if err != nil {
		t.Fatalf("a cached catalog must survive the file going away: %v", err)
	}
	if len(second) != len(first) {
		t.Errorf("second listing = %d voices, first = %d", len(second), len(first))
	}
}

func TestMissingArchiveExplainsTheFix(t *testing.T) {
	a := &KokoroArchive{Path: filepath.Join(t.TempDir(), "nope.bin")}
	_, err := a.Voices()
	if err == nil || !strings.Contains(err.Error(), "setup-kokoro.sh") {
		t.Errorf("err = %v; a missing archive must name the script that installs it", err)
	}
}

func TestUnreadableArchiveIsNotSilentlyEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "voices-v1.0.bin")
	if err := os.WriteFile(path, []byte("this is not a zip"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := (&KokoroArchive{Path: path}).Voices()
	if err == nil || !strings.Contains(err.Error(), "setup-kokoro.sh") {
		t.Errorf("err = %v", err)
	}
}

func TestPiperDirListsInstalledVoicesByLocale(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{
		"en/en_GB/alba/medium/en_GB-alba-medium.onnx",
		"en/en_US/amy/medium/en_US-amy-medium.onnx",
		"en/en_US/amy/medium/en_US-amy-medium.onnx.json",
		"xx/unknown-voice.onnx",
	} {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	voices, err := (&PiperDir{Dirs: []string{root, filepath.Join(root, "does-not-exist")}}).Voices()
	if err != nil {
		t.Fatal(err)
	}
	if len(voices) != 2 {
		t.Fatalf("voices = %+v", voices)
	}
	// American before British, matching Languages' display order.
	if voices[0].ID != "en_US-amy-medium" || voices[0].Language.Code != "en-us" {
		t.Errorf("first = %+v", voices[0])
	}
	if voices[1].ID != "en_GB-alba-medium" || voices[1].Name != "Alba (medium)" {
		t.Errorf("second = %+v", voices[1])
	}
	// Piper names carry no gender, and inventing one would be worse than
	// admitting it.
	if voices[0].Gender != GenderUnknown {
		t.Errorf("piper voice claimed a gender: %+v", voices[0])
	}
}

func TestPiperDirWithNoVoicesNamesAPackage(t *testing.T) {
	_, err := (&PiperDir{Dirs: []string{t.TempDir()}}).Voices()
	if err == nil || !strings.Contains(err.Error(), "piper-voices") {
		t.Errorf("err = %v", err)
	}
}

// Suggestions are the substance of the "that voice is not installed" message:
// a user who typed a British id wants the other British voices first.
func TestSuggestPrefersTheLanguageTheWantedIDImplies(t *testing.T) {
	installed := FakeKokoro("af_heart", "am_adam", "bf_emma", "bf_alice", "bm_george").List
	got := Suggest(installed, "bf_emily", 3)
	for _, id := range got {
		if id[0] != 'b' {
			t.Fatalf("suggestions for a British id = %v", got)
		}
	}
	if len(got) != 3 {
		t.Errorf("suggestions = %v, want 3", got)
	}
}

func TestSuggestFallsBackWhenTheIDImpliesNothing(t *testing.T) {
	installed := FakeKokoro("af_heart", "bf_emma").List
	got := Suggest(installed, "gibberish", 5)
	if len(got) != 2 {
		t.Errorf("suggestions = %v; every installed voice should be offered", got)
	}
	if Suggest(installed, "gibberish", 0) != nil {
		t.Error("asking for zero suggestions must produce none")
	}
}

func TestFakeIsSortedAndCanSimulateAnEmptyMachine(t *testing.T) {
	f := FakeKokoro("bm_george", "af_heart")
	voices, err := f.Voices()
	if err != nil {
		t.Fatal(err)
	}
	if voices[0].ID != "af_heart" {
		t.Errorf("fake is unsorted: %+v", voices)
	}
	if !Has(voices, "bm_george") || Has(voices, "nope") {
		t.Error("Has disagrees with the list")
	}
	if _, err := (Fake{Err: os.ErrNotExist}).Voices(); err == nil {
		t.Error("a fake with an error must report it")
	}
}

func TestKokoroVoicesFileIsTheOneDefinitionOfThePath(t *testing.T) {
	got := KokoroVoicesFile("/data/jarvix")
	want := filepath.Join("/data/jarvix", "models", "kokoro", "voices-v1.0.bin")
	if got != want {
		t.Errorf("KokoroVoicesFile = %q, want %q", got, want)
	}
}

func TestInAndFindSelectWithinALanguage(t *testing.T) {
	installed := FakeKokoro("af_heart", "bf_emma", "bm_george").List
	gb, _ := LanguageByCode("en-gb")
	if got := In(installed, gb); len(got) != 2 {
		t.Errorf("In(en-gb) = %+v", got)
	}
	if v, ok := Find(installed, "bf_emma"); !ok || v.Name != "Emma" {
		t.Errorf("Find = %+v, %v", v, ok)
	}
	if _, ok := Find(installed, "missing"); ok {
		t.Error("Find invented a voice")
	}
}
