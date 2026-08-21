package setup

import (
	"os"
	"path/filepath"
	"testing"
)

func loadString(t *testing.T, content string) *File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if content != "" {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func saved(t *testing.T, f *File) string {
	t.Helper()
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(f.Path())
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSetCreatesFileAndTable(t *testing.T) {
	f := loadString(t, "")
	f.Set("tts", "provider", "piper")
	got := saved(t, f)
	want := "[tts]\nprovider = \"piper\"\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestSetReplacesExistingKeyPreservingEverythingElse(t *testing.T) {
	content := "# my config\n\n[ai]\nprovider = \"openai\" # hand-picked\nmodel = \"gpt-4o\"\n\n[tts]\nprovider = \"piper\"\n"
	f := loadString(t, content)
	f.Set("ai", "provider", "ollama")
	got := saved(t, f)
	want := "# my config\n\n[ai]\nprovider = \"ollama\"\nmodel = \"gpt-4o\"\n\n[tts]\nprovider = \"piper\"\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestSetAppendsKeyToExistingTable(t *testing.T) {
	f := loadString(t, "[ai]\nprovider = \"ollama\"\n\n[log]\nlevel = \"info\"\n")
	f.Set("ai", "model", "llama3.2:3b")
	got := saved(t, f)
	want := "[ai]\nprovider = \"ollama\"\nmodel = \"llama3.2:3b\"\n\n[log]\nlevel = \"info\"\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestSetAppendsNewTableWithBlankSeparator(t *testing.T) {
	f := loadString(t, "[ai]\nprovider = \"ollama\"\n")
	f.Set("advisors.claude", "binary", "/usr/bin/claude")
	got := saved(t, f)
	want := "[ai]\nprovider = \"ollama\"\n\n[advisors.claude]\nbinary = \"/usr/bin/claude\"\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestSetSameValueIsANoOp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[tts]\nprovider = \"piper\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f.Set("tts", "provider", "piper")
	if f.dirty {
		t.Fatal("setting the same value must not dirty the file")
	}
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "[tts]\nprovider = \"piper\"\n" {
		t.Fatalf("file changed by a no-op set: %q", data)
	}
}

func TestGetReadsQuotedAndBareValues(t *testing.T) {
	f := loadString(t, "[ai]\nprovider = \"ollama\" # comment\nmax_tokens = 1024\n")
	if v, ok := f.Get("ai", "provider"); !ok || v != "ollama" {
		t.Fatalf("provider: got %q, %v", v, ok)
	}
	if v, ok := f.Get("ai", "max_tokens"); !ok || v != "1024" {
		t.Fatalf("max_tokens: got %q, %v", v, ok)
	}
	if _, ok := f.Get("ai", "model"); ok {
		t.Fatal("missing key must not be found")
	}
	if _, ok := f.Get("tts", "provider"); ok {
		t.Fatal("missing table must not be found")
	}
}

func TestGetDoesNotBleedIntoSubtables(t *testing.T) {
	f := loadString(t, "[ai]\nmodel = \"llama3.2:3b\"\n\n[ai.openai]\nbase_url = \"https://api.openai.com/v1\"\n")
	if _, ok := f.Get("ai", "base_url"); ok {
		t.Fatal("[ai] lookup must stop at the [ai.openai] header")
	}
	if v, ok := f.Get("ai.openai", "base_url"); !ok || v != "https://api.openai.com/v1" {
		t.Fatalf("subtable lookup: got %q, %v", v, ok)
	}
}

func TestTablesWithPrefix(t *testing.T) {
	f := loadString(t, "[advisors.claude]\nbinary = \"/usr/bin/claude\"\n\n[advisors.aider]\nbinary = \"/usr/bin/aider\"\n")
	if !f.HasTablePrefix("advisors.") {
		t.Fatal("expected advisors tables to be found")
	}
	names := f.TablesWithPrefix("advisors.")
	if len(names) != 2 || names[0] != "claude" || names[1] != "aider" {
		t.Fatalf("got %v", names)
	}
	if f.HasTablePrefix("tools.") {
		t.Fatal("unexpected tools tables")
	}
}

// config.toml is user state: it records local paths, the machine's advisor
// binaries, and the shell-tool policy. Every other file Jarvix writes is
// 0600, and a pre-existing 0644 config (written by an earlier build, or by
// hand) must be tightened rather than preserved
// (raised in review of #20).
func TestSaveWritesConfigPrivate(t *testing.T) {
	for name, seed := range map[string]string{
		"new file":               "",
		"pre-existing 0644 file": "[tts]\nprovider = \"piper\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			f := loadString(t, seed)
			f.Set("ai", "provider", "ollama")
			if err := f.Save(); err != nil {
				t.Fatal(err)
			}
			fi, err := os.Stat(f.Path())
			if err != nil {
				t.Fatal(err)
			}
			if perm := fi.Mode().Perm(); perm != 0o600 {
				t.Errorf("config mode = %o, want 0600", perm)
			}
		})
	}
}
