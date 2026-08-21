package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRewriteGolden drives the surgical TOML editor over golden files:
// hand-edited documents whose comments, unknown keys, and layout must
// survive a settings change byte-for-byte outside the changed keys.
func TestRewriteGolden(t *testing.T) {
	cases := []struct {
		name    string
		changes map[string]any
	}{
		{"replace", map[string]any{
			"tts.provider": "kokoro",
			"ai.model":     "qwen2.5:7b",
		}},
		{"multiline", map[string]any{
			"activation.ptt_chord": []string{"leftmeta", "space"},
			"ai.model":             "llama3.1:8b",
		}},
		{"insert", map[string]any{
			"ui.notification_preview": false,
			"tts.kokoro.speed":        1.5,
		}},
		{"newtable", map[string]any{
			"log.level": "debug",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input, err := os.ReadFile(filepath.Join("testdata", "rewrite", tc.name+".input.toml"))
			if err != nil {
				t.Fatal(err)
			}
			golden, err := os.ReadFile(filepath.Join("testdata", "rewrite", tc.name+".golden.toml"))
			if err != nil {
				t.Fatal(err)
			}
			got, err := RewriteTOML(input, tc.changes)
			if err != nil {
				t.Fatalf("RewriteTOML: %v", err)
			}
			if string(got) != string(golden) {
				t.Errorf("rewrite mismatch\n--- got ---\n%s\n--- want ---\n%s", got, golden)
			}
			// The result must parse and carry the change (the guard asserts
			// this too; the test states the contract explicitly).
			cfg, err := ParseBytes(got)
			if err != nil {
				t.Fatalf("rewritten document does not parse: %v", err)
			}
			for key, want := range tc.changes {
				s, ok := SettingFor(key)
				if !ok {
					t.Fatalf("no setting for %q", key)
				}
				if !settingValuesEqual(s.Get(cfg), want) {
					t.Errorf("%s = %v after rewrite, want %v", key, s.Get(cfg), want)
				}
			}
		})
	}
}

// TestRewriteFromNothing creates a fresh document when no file exists yet.
func TestRewriteFromNothing(t *testing.T) {
	got, err := RewriteTOML(nil, map[string]any{"tts.provider": "kokoro"})
	if err != nil {
		t.Fatalf("RewriteTOML: %v", err)
	}
	want := "[tts]\nprovider = \"kokoro\"\n"
	if string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestRewriteIsIdempotent applies the same change twice; the second pass must
// be byte-identical, or every save would churn the user's file.
func TestRewriteIsIdempotent(t *testing.T) {
	input, err := os.ReadFile(filepath.Join("testdata", "rewrite", "replace.input.toml"))
	if err != nil {
		t.Fatal(err)
	}
	changes := map[string]any{"tts.provider": "kokoro"}
	once, err := RewriteTOML(input, changes)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := RewriteTOML(once, changes)
	if err != nil {
		t.Fatal(err)
	}
	if string(once) != string(twice) {
		t.Error("second rewrite of the same change altered the document")
	}
}

// TestRewriteRejectsUnknownKey: only registry settings may be rewritten;
// anything else stays hand-edit-only.
func TestRewriteRejectsUnknownKey(t *testing.T) {
	if _, err := RewriteTOML(nil, map[string]any{"ai.myserver.api_key": "sk-x"}); err == nil {
		t.Error("expected an error for a non-registry key")
	}
}

func TestEncodeTOMLValue(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{"plain", `"plain"`},
		{`say "hi"` + "\n", `"say \"hi\"\n"`},
		{true, "true"},
		{42, "42"},
		{1.5, "1.5"},
		{2.0, "2.0"}, // TOML floats need a fractional part
		{[]string{"a", "b"}, `["a", "b"]`},
		{[]string{}, `[]`},
	}
	for _, tc := range cases {
		got, err := encodeTOMLValue(tc.in)
		if err != nil {
			t.Errorf("encode %v: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("encode %v = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestFingerprint(t *testing.T) {
	a := Fingerprint([]byte("one"))
	b := Fingerprint([]byte("two"))
	if a == b {
		t.Error("different content must fingerprint differently")
	}
	if !strings.HasPrefix(a, "sha256:") {
		t.Errorf("fingerprint %q lacks its algorithm prefix", a)
	}
	fp, err := FingerprintFile(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if fp != FingerprintMissing {
		t.Errorf("missing file fingerprint = %q, want %q", fp, FingerprintMissing)
	}
}

// TestWriteFileAtomic checks the mode and that content lands whole.
func TestWriteFileAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.toml")
	if err := WriteFileAtomic(path, []byte("x = 1\n")); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "x = 1\n" {
		t.Errorf("content = %q", data)
	}
	// Overwrites replace, never append.
	if err := WriteFileAtomic(path, []byte("y = 2\n")); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if string(data) != "y = 2\n" {
		t.Errorf("after overwrite content = %q", data)
	}
}
