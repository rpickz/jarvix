package config

import (
	"strings"
	"testing"
)

// The [memory] table (ADR 0025): defaults, validation, and the settings
// registry entries the CLI and settings screen drive it through.

func TestMemoryDefaults(t *testing.T) {
	cfg := Default()
	if !cfg.Memory.Enabled {
		t.Error("memory defaults off; it should default on — nothing is stored without an explicit ask")
	}
	if cfg.Memory.MaxFacts != 200 || cfg.Memory.MaxInjectedTokens != 500 {
		t.Errorf("memory defaults = %+v", cfg.Memory)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("defaults do not validate: %v", err)
	}
}

func TestMemoryValidation(t *testing.T) {
	cases := []struct {
		name  string
		shape func(*Config)
		want  string
	}{
		{"max_facts zero", func(c *Config) { c.Memory.MaxFacts = 0 }, "memory.max_facts"},
		{"max_facts negative", func(c *Config) { c.Memory.MaxFacts = -1 }, "memory.max_facts"},
		{"injection budget below the floor",
			func(c *Config) { c.Memory.MaxInjectedTokens = MinMemoryInjectedTokens - 1 },
			"memory.max_injected_tokens"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := Default()
			c.shape(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("Validate() = %v, want a problem naming %s", err, c.want)
			}
		})
	}
	// The floor itself is fine.
	cfg := Default()
	cfg.Memory.MaxInjectedTokens = MinMemoryInjectedTokens
	if err := cfg.Validate(); err != nil {
		t.Errorf("the documented minimum failed validation: %v", err)
	}
}

// TestMemorySettingsAreRegistered: every [memory] key is drivable through
// config.set, and all are restart-class — the store and the tools are wired
// at daemon construction.
func TestMemorySettingsAreRegistered(t *testing.T) {
	want := map[string]bool{
		"memory.enabled":             false,
		"memory.max_facts":           false,
		"memory.max_injected_tokens": false,
	}
	for _, s := range Settings() {
		if _, ok := want[s.Key]; !ok {
			continue
		}
		want[s.Key] = true
		if s.Reload != ReloadRestart {
			t.Errorf("%s reload = %s, want restart", s.Key, s.Reload)
		}
	}
	for key, seen := range want {
		if !seen {
			t.Errorf("setting %s is not registered", key)
		}
	}

	// One round trip through the registry, the way config.set applies it.
	cfg := Default()
	s, ok := SettingFor("memory.enabled")
	if !ok {
		t.Fatal("no setting for memory.enabled")
	}
	if err := s.Apply(&cfg, "false"); err != nil {
		t.Fatal(err)
	}
	if cfg.Memory.Enabled {
		t.Error("Apply(false) did not disable memory")
	}
	if got := s.Get(cfg); got != false {
		t.Errorf("Get = %v after Apply(false)", got)
	}
}
