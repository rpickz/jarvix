package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/script"
)

// The [[scripts]] config tests use stub files in test-owned temp dirs —
// never a user's — because validation deliberately stats the path: a missing
// or non-executable script must be a load-time message, not a spoken apology.

// stubScriptFile writes an executable stub and returns its absolute path.
func stubScriptFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "backup-notes.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func backupScriptTOML(path string) string {
	return fmt.Sprintf(`
[[scripts]]
name = "backup notes"
phrases = ["backup my notes", "back up my notes"]
path = %q
timeout_sec = 120
report = "summary"
`, path)
}

// TestScriptsParseAndConvert: the documented shape parses, validates, and
// converts with the defaults applied where the file is silent.
func TestScriptsParseAndConvert(t *testing.T) {
	path := stubScriptFile(t)
	cfg := parseValid(t, backupScriptTOML(path))
	defs := cfg.ScriptDefinitions()
	if len(defs) != 1 {
		t.Fatalf("defs = %+v", defs)
	}
	def := defs[0]
	if def.Name != "backup notes" || len(def.Phrases) != 2 || def.Path != path {
		t.Errorf("def = %+v", def)
	}
	if def.Timeout != 120*time.Second || def.Report != script.ReportSummary {
		t.Errorf("def = %+v", def)
	}
	// The router knows the phrases.
	opts := cfg.IntentOptions()
	if len(opts.Scripts) != 1 || opts.Scripts[0].Name != "backup notes" {
		t.Errorf("router options = %+v", opts.Scripts)
	}
}

// TestScriptDefaultsApply: an entry that says nothing about timeout or
// report gets the shipped defaults, and no field exists that could carry
// arguments — the TOML shape is the enforcement.
func TestScriptDefaultsApply(t *testing.T) {
	path := stubScriptFile(t)
	cfg := parseValid(t, fmt.Sprintf(`
[[scripts]]
name = "backup notes"
phrases = ["backup my notes"]
path = %q
`, path))
	def := cfg.ScriptDefinitions()[0]
	if def.Timeout != script.DefaultTimeout || def.Report != script.ReportSummary {
		t.Errorf("defaults not applied: %+v", def)
	}
}

// TestScriptValidationSpeaksTheFileLanguage: the problems a user can write
// come back naming the table to fix, through Config.Validate like every
// other configuration mistake — including the filesystem facts and the
// cross-family phrase collisions.
func TestScriptValidationSpeaksTheFileLanguage(t *testing.T) {
	path := stubScriptFile(t)
	tests := []struct {
		name string
		doc  string
		want string
	}{
		{"relative path", `
[[scripts]]
name = "backup notes"
phrases = ["backup my notes"]
path = "bin/backup.sh"
`, "is not absolute"},
		{"missing file", `
[[scripts]]
name = "backup notes"
phrases = ["backup my notes"]
path = "/definitely/not/here/backup.sh"
`, "does not exist"},
		{"bad report", backupScriptTOML(path)[:len(backupScriptTOML(path))-len("report = \"summary\"\n")] + "report = \"shout\"\n",
			"is not a mode"},
		{"phrase collides with a built-in", fmt.Sprintf(`
[[scripts]]
name = "hush"
phrases = ["mute"]
path = %q
`, path), `already the built-in intent "volume.mute"`},
		{"phrase collides with a routine", fmt.Sprintf(`
[[routines]]
name = "morning"
phrases = ["set up my desk"]
[[routines.steps]]
app = "firefox"
workspace = 1
[[scripts]]
name = "desk backup"
phrases = ["set up my desk"]
path = %q
`, path), `the trigger for routine "morning"`},
		{"placeholder in a phrase", fmt.Sprintf(`
[[scripts]]
name = "backup notes"
phrases = ["backup workspace {workspace}"]
path = %q
`, path), "contains a placeholder"},
		{"timeout out of range", fmt.Sprintf(`
[[scripts]]
name = "backup notes"
phrases = ["backup my notes"]
path = %q
timeout_sec = -5
`, path), "timeout_sec must be between"},
		{"intents disabled", fmt.Sprintf(`
[intents]
enabled = false
[[scripts]]
name = "backup notes"
phrases = ["backup my notes"]
path = %q
`, path), "intents.enabled is false"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := parse([]byte(tt.doc), Default())
			if err != nil {
				t.Fatal(err)
			}
			err = cfg.Validate()
			if err == nil {
				t.Fatal("validated despite the problem")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
		})
	}
}

// TestScriptsSurviveASettingsRewrite: [[scripts]] is hand-edited TOML
// outside the settings registry, so a config.set of an ordinary key must
// leave the tables byte-for-byte intact — the rewrite is surgical (ADR 0015).
func TestScriptsSurviveASettingsRewrite(t *testing.T) {
	path := stubScriptFile(t)
	doc := "ai_model_placeholder = false\n" + backupScriptTOML(path)
	setting, ok := SettingFor("conversation.speak_responses")
	if !ok {
		t.Fatal("setting missing")
	}
	rewritten, err := RewriteTOML([]byte(doc), map[string]any{setting.Key: false})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rewritten), `phrases = ["backup my notes", "back up my notes"]`) {
		t.Error("the rewrite lost the script tables")
	}
	cfg, err := ParseBytes(rewritten)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Scripts) != 1 || cfg.Scripts[0].Path != path {
		t.Errorf("scripts after rewrite = %+v", cfg.Scripts)
	}
}
