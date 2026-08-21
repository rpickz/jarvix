package main

import (
	"archive/zip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/config"
)

// `jarvix voices` is the discovery surface: the voices were always on disk,
// and the reason nobody used them is that nothing listed them. These tests
// build a stand-in voices archive — a zip of named entries, which is what the
// real file is — so no 27 MB download and no 310 MB model is involved.

// installVoices writes a Kokoro voices archive into the hermetic data dir and
// points the config at the Kokoro engine.
func installVoices(t *testing.T, ids ...string) {
	t.Helper()
	paths := config.DefaultPaths()
	path := paths.KokoroVoicesFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	for _, id := range ids {
		e, err := w.Create(id + ".npy")
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
}

func writeConfig(t *testing.T, body string) {
	t.Helper()
	path := config.DefaultPaths().ConfigFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRunVoicesListsThemGroupedByLanguageWithGender(t *testing.T) {
	hermeticEnv(t)
	installVoices(t, "af_heart", "am_adam", "bf_emma", "bm_george", "ff_siwis")
	writeConfig(t, "[tts]\nprovider = \"kokoro\"\n\n[tts.kokoro]\nvoice = \"bf_emma\"\n")

	stdout, _ := capture(t, func() {
		if code := run([]string{"voices"}); code != 0 {
			t.Errorf("exit code = %d", code)
		}
	})
	for _, want := range []string{
		"English (American)", "English (British)", "French",
		"bf_emma", "female", "bm_george", "male",
		// The active voice is marked, so "which one am I hearing?" is
		// answered by the same command that answers "what else is there?".
		"* bf_emma",
		// And changing it is a copyable command, not an instruction to go
		// and edit TOML.
		"jarvix config set tts.kokoro.voice=",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("listing missing %q:\n%s", want, stdout)
		}
	}
	// Grouped means grouped: every American voice before the British header.
	if strings.Index(stdout, "am_adam") > strings.Index(stdout, "English (British)") {
		t.Errorf("voices are not grouped by language:\n%s", stdout)
	}
	// A non-English choice cannot be made without speech recognition
	// following, and the listing says so where the choice is made.
	if !strings.Contains(stdout, "stt.whisper.model=") {
		t.Errorf("listing did not mention speech recognition:\n%s", stdout)
	}
}

func TestRunVoicesJSONIsParseable(t *testing.T) {
	hermeticEnv(t)
	installVoices(t, "af_heart", "bf_emma")
	writeConfig(t, "[tts]\nprovider = \"kokoro\"\n\n[tts.kokoro]\nvoice = \"bf_emma\"\n")

	stdout, _ := capture(t, func() {
		if code := run([]string{"voices", "--json"}); code != 0 {
			t.Errorf("exit code = %d", code)
		}
	})
	var got []struct {
		ID       string `json:"id"`
		Language string `json:"language"`
		Code     string `json:"code"`
		Whisper  string `json:"whisper_language"`
		Gender   string `json:"gender"`
		Active   bool   `json:"active"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries: %s", len(got), stdout)
	}
	for _, v := range got {
		if v.ID == "bf_emma" {
			if v.Code != "en-gb" || v.Whisper != "en" || v.Gender != "female" || !v.Active {
				t.Errorf("bf_emma = %+v", v)
			}
		}
	}
}

// A missing archive is the common case on a Piper machine, and the message
// has to be the fix rather than a stack of paths.
func TestRunVoicesWithoutKokoroSaysHowToInstallIt(t *testing.T) {
	hermeticEnv(t)
	writeConfig(t, "[tts]\nprovider = \"kokoro\"\n")
	_, stderr := capture(t, func() {
		if code := run([]string{"voices"}); code != 1 {
			t.Errorf("exit code = %d", code)
		}
	})
	if !strings.Contains(stderr, "setup-kokoro.sh") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestRunVoicesRejectsUnknownFlags(t *testing.T) {
	hermeticEnv(t)
	_, stderr := capture(t, func() {
		if code := run([]string{"voices", "--all"}); code != 1 {
			t.Errorf("exit code = %d", code)
		}
	})
	if !strings.Contains(stderr, "usage: jarvix voices") {
		t.Errorf("stderr = %q", stderr)
	}
}

// The command is listed where a user would look for it.
func TestUsageMentionsVoices(t *testing.T) {
	if !strings.Contains(usage, "jarvix voices") {
		t.Error("usage does not mention jarvix voices")
	}
}
