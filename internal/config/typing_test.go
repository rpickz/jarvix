package config

import (
	"strings"
	"testing"
	"time"
)

// TestTypingIsDisabledByDefault is the acceptance criterion the whole feature
// hangs off: a user who never heard of typing must not have a machine that
// types. It is asserted on the shipped defaults *and* on a config file that
// enables everything else, because "off unless asked for" has to survive
// somebody turning the other tools on.
func TestTypingIsDisabledByDefault(t *testing.T) {
	if Default().Tools.Typing.Enable {
		t.Fatal("[tools.typing] enable must default to false")
	}
	cfg, err := ParseBytes([]byte("[tools]\nshell = true\ndesktop = true\nartifacts = true\n"))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	if cfg.Tools.Typing.Enable {
		t.Fatal("enabling the other tools must not enable typing")
	}
}

// TestTypingSystemPromptOnlyWhenEnabled: the model is not told it can type
// unless it can. A prompt that describes a tool the registry does not hold is
// a model inventing calls that fail.
func TestTypingSystemPromptOnlyWhenEnabled(t *testing.T) {
	if strings.Contains(Default().AI.SystemPrompt, "type into the window") {
		t.Error("the base system prompt must not mention typing")
	}
	if !strings.Contains(TypingSystemPrompt, "Typing never submits") {
		t.Error("the typing prompt must state that typing does not submit")
	}
}

func TestTypingDefaults(t *testing.T) {
	tp := Default().Tools.Typing
	if tp.MaxChars != 500 || tp.RateLimit != 6 || tp.RateWindowSec != 60 {
		t.Fatalf("defaults = %+v, want 500 chars, 6 per 60s", tp)
	}
	if got := tp.RateWindow(); got != time.Minute {
		t.Errorf("RateWindow() = %v, want 1m", got)
	}
}

// TestTypingValidation: every blast-radius control refuses an unusable value
// and says what to write instead. A cap that can be set to anything is not a
// cap, and a message that does not name the key is not actionable.
func TestTypingValidation(t *testing.T) {
	cases := []struct {
		name string
		set  func(*Config)
		want string
	}{
		{"max_chars of zero", func(c *Config) { c.Tools.Typing.MaxChars = 0 },
			"tools.typing.max_chars"},
		{"max_chars beyond the ceiling", func(c *Config) { c.Tools.Typing.MaxChars = 1_000_000 },
			"tools.typing.max_chars"},
		{"rate_limit of zero", func(c *Config) { c.Tools.Typing.RateLimit = 0 },
			"tools.typing.rate_limit"},
		{"rate window of zero", func(c *Config) { c.Tools.Typing.RateWindowSec = 0 },
			"tools.typing.rate_window_sec"},
		{"rate window beyond an hour", func(c *Config) { c.Tools.Typing.RateWindowSec = 7200 },
			"tools.typing.rate_window_sec"},
		{"an empty terminal class", func(c *Config) { c.Tools.Typing.TerminalClasses = []string{"kitty", " "} },
			"tools.typing.terminal_classes"},
		{"a binary with arguments in it", func(c *Config) { c.Tools.Typing.Binary = "wtype --fast" },
			"tools.typing.binary"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.set(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("%s should be refused", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to name %s", err, tc.want)
			}
		})
	}
}

func TestTypingValidationAcceptsTheDefaults(t *testing.T) {
	cfg := Default()
	cfg.Tools.Typing.Enable = true
	cfg.Tools.Typing.TerminalClasses = []string{"alacritty", "myterm"}
	cfg.Tools.Typing.Binary = "/usr/bin/wtype"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// TestTypingSettingsAreEditable: the switch has to be reachable from the
// settings surface, because turning it *off* must be as easy as one command —
// and easier than editing TOML while something is typing.
func TestTypingSettingsAreEditable(t *testing.T) {
	for _, key := range []string{
		"tools.typing.enable", "tools.typing.max_chars",
		"tools.typing.rate_limit", "tools.typing.rate_window_sec",
		"tools.typing.terminal_classes",
	} {
		if _, ok := SettingFor(key); !ok {
			t.Errorf("%s is not an editable setting", key)
		}
	}
}

// TestTypingRewriteCreatesTheTable: the [tools.typing] table does not exist in
// a config written before this feature, so the surgical rewriter has to add it
// rather than fail the save.
func TestTypingRewriteCreatesTheTable(t *testing.T) {
	doc := []byte("[tools]\nshell = false\n")
	out, err := RewriteTOML(doc, map[string]any{"tools.typing.enable": true})
	if err != nil {
		t.Fatalf("RewriteTOML: %v", err)
	}
	cfg, err := ParseBytes(out)
	if err != nil {
		t.Fatalf("ParseBytes: %v\n%s", err, out)
	}
	if !cfg.Tools.Typing.Enable {
		t.Fatalf("the rewrite did not take effect:\n%s", out)
	}
	// And back off again: the direction that has to work is the one somebody
	// reaches for in a hurry.
	out, err = RewriteTOML(out, map[string]any{"tools.typing.enable": false})
	if err != nil {
		t.Fatalf("RewriteTOML off: %v", err)
	}
	cfg, err = ParseBytes(out)
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	if cfg.Tools.Typing.Enable {
		t.Fatalf("typing could not be switched off:\n%s", out)
	}
}
