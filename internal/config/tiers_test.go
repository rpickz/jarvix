package config

import (
	"strings"
	"testing"
)

// tierDoc is a config document with the endpoints and advisor a tier table
// would point at, so a test only has to write the part it is about.
func tierDoc(tiers string) string {
	return `
[ai]
provider = "ollama"
model = "llama3.2:3b"

[ai.fireworks]
base_url = "https://api.fireworks.ai/inference/v1"
api_key_env = "FIREWORKS_API_KEY"

[advisors.claude]
binary = "claude"
` + tiers
}

func parseTierDoc(t *testing.T, doc string) Config {
	t.Helper()
	cfg, err := ParseBytes([]byte(doc))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	return cfg
}

// The shipped state: no [ai.tiers] anywhere. Tiering is off, and nothing about
// the section exists to be reasoned about.
func TestNoTiersSectionMeansTieringIsOff(t *testing.T) {
	cfg := parseTierDoc(t, tierDoc(""))
	if cfg.AI.Tiers.Enabled() {
		t.Error("tiering is on with no [ai.tiers] table")
	}
	if names := cfg.AI.Tiers.Names(); len(names) != 0 {
		t.Errorf("tier names = %v, want none", names)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
	// The default ships as medium so the settings row reads sensibly, but it
	// configures nothing on its own.
	if got := cfg.AI.Tiers.Default; got != "medium" {
		t.Errorf("default = %q, want medium", got)
	}
}

// A `default` with no tier tables is still not tiering: it names a preference
// about a feature nobody has switched on.
func TestTiersDefaultAloneIsNotTiering(t *testing.T) {
	cfg := parseTierDoc(t, tierDoc("\n[ai.tiers]\ndefault = \"deep\"\n"))
	if cfg.AI.Tiers.Enabled() {
		t.Error("tiering is on with only a default")
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v — a default without tables is a preference, not an error", err)
	}
}

func TestTiersLoadAsTiersRatherThanEndpoints(t *testing.T) {
	cfg := parseTierDoc(t, tierDoc(`
[ai.tiers]
default = "instant"

[ai.tiers.instant]
provider = "lmstudio"
model = "qwen3-1.7b"
history_turns = 4

[ai.tiers.deep]
advisor = "claude"
`))
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !cfg.AI.Tiers.Enabled() {
		t.Fatal("tiering is off with two tier tables configured")
	}
	// The regression this guards: [ai.tiers] harvested as an [ai.<name>]
	// endpoint, which then fails validation for having no base_url.
	if _, ok := cfg.AI.Endpoints["tiers"]; ok {
		t.Error("[ai.tiers] was harvested as an endpoint")
	}
	instant := cfg.AI.Tiers.Tiers["instant"]
	if instant.Provider != "lmstudio" || instant.Model != "qwen3-1.7b" || instant.HistoryTurns != 4 {
		t.Errorf("instant = %+v", instant)
	}
	if got := cfg.AI.Tiers.Tiers["deep"].Advisor; got != "claude" {
		t.Errorf("deep advisor = %q, want claude", got)
	}
	if got := cfg.AI.Tiers.Default; got != "instant" {
		t.Errorf("default = %q, want instant", got)
	}
}

// An endpoint may not be called "tiers": the two would be indistinguishable in
// the document, and the loader would have to guess.
func TestAnEndpointCannotBeCalledTiers(t *testing.T) {
	if !ReservedAIKeys()["tiers"] {
		t.Fatal(`"tiers" is not a reserved [ai] key; [ai.tiers.instant] would parse as an endpoint's key`)
	}
}

func TestTierValidationNamesTheFieldThatIsWrong(t *testing.T) {
	cases := map[string]struct{ tiers, want string }{
		"a provider with no endpoint": {
			tiers: "[ai.tiers.instant]\nprovider = \"nowhere\"\nmodel = \"m\"\n",
			want:  "ai.tiers.instant.provider",
		},
		"a provider with no model": {
			tiers: "[ai.tiers.instant]\nprovider = \"fireworks\"\n",
			want:  "ai.tiers.instant.model",
		},
		"an advisor that is not configured": {
			tiers: "[ai.tiers.deep]\nadvisor = \"gemini\"\n",
			want:  "ai.tiers.deep.advisor",
		},
		"both shapes at once": {
			tiers: "[ai.tiers.deep]\nadvisor = \"claude\"\nprovider = \"fireworks\"\nmodel = \"m\"\n",
			want:  "never both",
		},
		"a negative context budget": {
			tiers: "[ai.tiers.instant]\nprovider = \"fireworks\"\nmodel = \"m\"\nhistory_turns = -1\n",
			want:  "ai.tiers.instant.history_turns",
		},
		"a tier table with only a budget": {
			tiers: "[ai.tiers.instant]\nhistory_turns = 4\n",
			want:  "ai.tiers.instant.provider",
		},
		"a name that is not a tier": {
			tiers: "[ai.tiers.fast]\nprovider = \"fireworks\"\nmodel = \"m\"\n",
			want:  `tier name "fast"`,
		},
		"a default that is not a tier": {
			tiers: "[ai.tiers]\ndefault = \"turbo\"\n\n[ai.tiers.instant]\nprovider = \"fireworks\"\nmodel = \"m\"\n",
			want:  "ai.tiers.default",
		},
		"a default naming a tier that has no table": {
			tiers: "[ai.tiers]\ndefault = \"deep\"\n\n[ai.tiers.instant]\nprovider = \"fireworks\"\nmodel = \"m\"\n",
			want:  "there is no [ai.tiers.deep] table",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := parseTierDoc(t, tierDoc("\n"+tc.tiers))
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %s", name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %q", err.Error(), tc.want)
			}
		})
	}
}

// Defaulting to medium is always serviceable, even with no [ai.tiers.medium]
// table: medium with no table of its own *is* the [ai] brain.
func TestDefaultingToMediumNeedsNoMediumTable(t *testing.T) {
	cfg := parseTierDoc(t, tierDoc(`
[ai.tiers]
default = "medium"

[ai.tiers.instant]
provider = "fireworks"
model = "m"
`))
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestTierDefaultIsASettingAndOutOfTheAssistantsReach(t *testing.T) {
	s, ok := SettingFor("ai.tiers.default")
	if !ok {
		t.Fatal("ai.tiers.default is not a registered setting")
	}
	if s.Type != TypeString || s.Reload != ReloadIdle {
		t.Errorf("type/reload = %q/%q", s.Type, s.Reload)
	}
	want := map[string]bool{"instant": true, "medium": true, "deep": true}
	if len(s.Enum) != len(want) {
		t.Fatalf("enum = %v", s.Enum)
	}
	for _, v := range s.Enum {
		if !want[v] {
			t.Errorf("enum has %q", v)
		}
	}
	// Everything under [ai] is: the assistant does not re-point its own
	// brains, and a tier is a brain.
	if _, excluded := AssistantExcludedSettingReason("ai.tiers.default"); !excluded {
		t.Error("the assistant may change its own default tier")
	}
}
