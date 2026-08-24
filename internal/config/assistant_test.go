package config

import (
	"reflect"
	"strings"
	"testing"
)

// With no [assistant] table the identity is byte-identical to what shipped
// before it existed (issue #103): the default name, the tuned mishearing
// list, the exact bias sentence, and the lowercase detector word. This is the
// upgrade guarantee — an untouched config must not change behaviour by a
// byte.
func TestDefaultIdentityIsByteIdenticalToTheShippedOne(t *testing.T) {
	cfg := Default()
	if cfg.Assistant.Name != "Jarvix" {
		t.Errorf("default assistant.name = %q, want %q", cfg.Assistant.Name, "Jarvix")
	}
	wantAliases := []string{"jarvis", "javax", "jarvic", "jarvicks", "jarvex"}
	if got := cfg.Assistant.EffectiveAliases(); !reflect.DeepEqual(got, wantAliases) {
		t.Errorf("default effective aliases = %v, want %v", got, wantAliases)
	}
	if got := cfg.STTBiasPrompt(); got != "The assistant is called Jarvix." {
		t.Errorf("default bias sentence = %q", got)
	}
	if got := cfg.WakeDetectorWord(); got != "jarvix" {
		t.Errorf("default detector word = %q, want %q (the detector always got the lowercase word)", got, "jarvix")
	}
	if got, want := cfg.AI.SystemPrompt, "You are Jarvix, a voice assistant"; !strings.HasPrefix(got, want) {
		t.Errorf("default system prompt = %q, want it to open with %q", got, want)
	}
}

// The tuned alias list belongs to the default name, not to the aliases key:
// a custom name starts with none (whisper's mishearings of "jarvix" say
// nothing about "Hal"), while an explicit list — including an explicit empty
// one — always wins.
func TestEffectiveAliasesAreCoupledToTheName(t *testing.T) {
	for _, c := range []struct {
		name     string
		assist   Assistant
		want     []string
		wantsNil bool
	}{
		{"default name, unset aliases", Assistant{Name: "Jarvix"}, []string{"jarvis", "javax", "jarvic", "jarvicks", "jarvex"}, false},
		{"default name restated in lowercase", Assistant{Name: "jarvix"}, []string{"jarvis", "javax", "jarvic", "jarvicks", "jarvex"}, false},
		{"custom name, unset aliases", Assistant{Name: "Hal"}, nil, true},
		{"custom name, explicit aliases", Assistant{Name: "Hal", Aliases: []string{"hal", "howl"}}, []string{"hal", "howl"}, false},
		{"default name, explicitly cleared", Assistant{Name: "Jarvix", Aliases: []string{}}, []string{}, false},
	} {
		got := c.assist.EffectiveAliases()
		if c.wantsNil {
			if len(got) != 0 {
				t.Errorf("%s: effective aliases = %v, want none", c.name, got)
			}
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: effective aliases = %v, want %v", c.name, got, c.want)
		}
	}
}

// The loader refuses a broken identity with field-keyed messages — each one
// opens with the dotted key, which is how the settings screen pins a problem
// to its input (ADR 0015).
func TestAssistantValidationIsFieldKeyed(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(*Config)
		want string
	}{
		{"empty name", func(c *Config) { c.Assistant.Name = "" }, "assistant.name"},
		{"whitespace name", func(c *Config) { c.Assistant.Name = "   " }, "assistant.name"},
		{"blank alias", func(c *Config) { c.Assistant.Aliases = []string{"jarvis", "  "} }, "assistant.aliases"},
		{"duplicate alias", func(c *Config) { c.Assistant.Aliases = []string{"jarvis", "jarvis"} }, "twice"},
		{"duplicate alias in different case", func(c *Config) { c.Assistant.Aliases = []string{"jarvis", "Jarvis"} }, "twice"},
		{"alias equal to the name", func(c *Config) { c.Assistant.Aliases = []string{"jarvix"} }, "the name itself"},
		{"alias equal to a multi-word name", func(c *Config) {
			c.Assistant.Name = "Mister Smith"
			c.Assistant.Aliases = []string{"mister  smith"}
		}, "the name itself"},
	} {
		cfg := Default()
		c.edit(&cfg)
		err := cfg.Validate()
		if err == nil {
			t.Errorf("%s: accepted", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: the error does not carry %q: %v", c.name, c.want, err)
		}
	}

	// The identities the issue promises must all load: single-word,
	// multi-word, and multi-word aliases for a multi-word name. (The issue's
	// own example lists "hal" beside name "Hal"; that spelling is refused
	// above instead — the strip accepts the name case-insensitively already,
	// so a name-as-alias is a misunderstanding worth naming, not a wider
	// net.)
	for _, c := range []Assistant{
		{Name: "Hal", Aliases: []string{"howl", "hull"}},
		{Name: "Mister Smith", Aliases: []string{"mr smith", "missus smith"}},
		{Name: "Hal"}, // zero aliases is a doctor WARN, never a load error
	} {
		cfg := Default()
		cfg.Assistant = c
		if err := cfg.Validate(); err != nil {
			t.Errorf("%+v: rejected: %v", c, err)
		}
	}
}

// The detector's word derives from the name — lowercased, because model
// lookups are spelled that way — unless activation.wake_word overrides it,
// which is the documented setup for a name no acoustic model exists for.
func TestWakeDetectorWordDerivesFromTheNameUnlessOverridden(t *testing.T) {
	cfg := Default()
	cfg.Assistant.Name = "Hal"
	if got := cfg.WakeDetectorWord(); got != "hal" {
		t.Errorf("detector word = %q, want %q", got, "hal")
	}
	cfg.Activation.WakeWord = "hey_jarvis"
	if got := cfg.WakeDetectorWord(); got != "hey_jarvis" {
		t.Errorf("override ignored: detector word = %q", got)
	}
	cfg.Activation.WakeWord = "   "
	if got := cfg.WakeDetectorWord(); got != "hal" {
		t.Errorf("a whitespace override is not an override: detector word = %q", got)
	}
}

// The default system prompt's self-reference follows the configured name; a
// hand-written ai.system_prompt is sent verbatim, whatever it calls the
// assistant — the user's words are the user's.
func TestAssistantSystemPromptSelfRefersByTheConfiguredName(t *testing.T) {
	cfg := Default()
	cfg.Assistant.Name = "Hal"
	got := AssistantSystemPrompt(cfg)
	if !strings.HasPrefix(got, "You are Hal, a voice assistant") {
		t.Errorf("prompt does not self-refer by the configured name: %q", got)
	}
	if strings.Contains(got, "You are Jarvix") {
		t.Errorf("prompt still self-refers by the default name: %q", got)
	}

	cfg.AI.SystemPrompt = "You are Marvin, and you are not enjoying this."
	if got := AssistantSystemPrompt(cfg); !strings.HasPrefix(got, "You are Marvin") {
		t.Errorf("a hand-written prompt was rewritten: %q", got)
	}

	// Untouched everything: the composed base is the shipped prompt, byte
	// for byte, with the tool suffixes following exactly as before — the
	// other half of the upgrade guarantee.
	if got, want := AssistantSystemPrompt(Default()), Default().AI.SystemPrompt; !strings.HasPrefix(got, want) {
		t.Errorf("default composition changed: %q does not open with %q", got, want)
	}
}

// [assistant] loads from TOML the way the issue's examples write it, and a
// table that only names the assistant does not inherit another name's
// mishearings.
func TestAssistantLoadsFromTOML(t *testing.T) {
	cfg := writeAndLoad(t, `
[assistant]
name = "Hal"
aliases = ["howl", "hull"]
`)
	if cfg.Assistant.Name != "Hal" {
		t.Errorf("name = %q", cfg.Assistant.Name)
	}
	if got := cfg.Assistant.EffectiveAliases(); !reflect.DeepEqual(got, []string{"howl", "hull"}) {
		t.Errorf("aliases = %v", got)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("the documented shape does not validate: %v", err)
	}

	nameOnly := writeAndLoad(t, `
[assistant]
name = "Hal"
`)
	if got := nameOnly.Assistant.EffectiveAliases(); len(got) != 0 {
		t.Errorf("a custom name inherited the default name's mishearings: %v", got)
	}
}
