package config

import (
	"strings"
	"testing"
)

// TestAdvisorFromWizardConfigIsComplete pins the compatibility contract with
// `jarvix setup`: the wizard writes nothing but a binary, and that must be a
// working advisor.
func TestAdvisorFromWizardConfigIsComplete(t *testing.T) {
	cfg := writeAndLoad(t, `
[advisors.claude]
binary = "/usr/bin/claude"
`)
	a, ok := cfg.Advisors["claude"]
	if !ok {
		t.Fatalf("advisors = %v", cfg.Advisors)
	}
	if a.Binary != "/usr/bin/claude" {
		t.Errorf("binary = %q", a.Binary)
	}
	if len(a.Args) == 0 {
		t.Error("preset argv should fill in for a table that only names a binary")
	}
	if a.TimeoutSec != DefaultAdvisorTimeoutSec {
		t.Errorf("timeout_sec = %d, want %d", a.TimeoutSec, DefaultAdvisorTimeoutSec)
	}
	if a.Description == "" {
		t.Error("the model needs a description of what the advisor is for")
	}
	if !a.ReadOnly {
		t.Error("an untouched read-only preset must keep its read-only claim")
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("wizard config must validate: %v", err)
	}
}

func TestAdvisorOverridesAndCustomAdvisors(t *testing.T) {
	cfg := writeAndLoad(t, `
[advisors.claude]
binary = "/opt/claude"
args = ["--print", "{question}"]
timeout_sec = 30

[advisors.house]
binary = "/usr/local/bin/house-llm"
description = "the local research box"
`)
	claude := cfg.Advisors["claude"]
	if len(claude.Args) != 2 || claude.Args[1] != AdvisorQuestionPlaceholder {
		t.Errorf("args = %v", claude.Args)
	}
	if claude.TimeoutSec != 30 {
		t.Errorf("timeout_sec = %d", claude.TimeoutSec)
	}
	// A hand-written argv is unaudited, so it loses the silent tier — this
	// is the security-relevant half of the defaulting.
	if claude.ReadOnly {
		t.Error("a config-supplied argv must not keep the read-only claim")
	}

	house := cfg.Advisors["house"]
	if house.ReadOnly {
		t.Error("an advisor with no preset must never be read-only")
	}
	if house.Description != "the local research box" {
		t.Errorf("description = %q", house.Description)
	}
	if len(house.Args) != 0 {
		t.Errorf("an unknown advisor gets no argv guesses: %v", house.Args)
	}
	if names := cfg.AdvisorNames(); strings.Join(names, ",") != "claude,house" {
		t.Errorf("AdvisorNames = %v, want sorted", names)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestAdvisorBinaryDefaultsToName(t *testing.T) {
	cfg := writeAndLoad(t, "[advisors.gemini]\n")
	if got := cfg.Advisors["gemini"].Binary; got != "gemini" {
		t.Errorf("binary = %q, want the advisor's own name (resolved on PATH)", got)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestAdvisorValidationIsActionable(t *testing.T) {
	tests := []struct {
		name   string
		toml   string
		expect string
	}{
		{
			name:   "question embedded in a larger argument",
			toml:   "[advisors.claude]\nargs = [\"--ask={question}\"]\n",
			expect: "argument of its own",
		},
		{
			name:   "question placed twice",
			toml:   "[advisors.claude]\nargs = [\"{question}\", \"{question}\"]\n",
			expect: "at most once",
		},
		{
			name:   "non-positive timeout",
			toml:   "[advisors.claude]\ntimeout_sec = -1\n",
			expect: "timeout_sec must be positive",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := writeAndLoad(t, tt.toml).Validate()
			if err == nil || !strings.Contains(err.Error(), tt.expect) {
				t.Errorf("error = %v, want it to mention %q", err, tt.expect)
			}
		})
	}
}

func TestKnownAdvisorsMatchPresets(t *testing.T) {
	names := KnownAdvisors()
	if len(names) != len(AdvisorPresets) {
		t.Fatalf("KnownAdvisors = %v", names)
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Errorf("KnownAdvisors must be sorted: %v", names)
		}
	}
	for _, name := range names {
		preset := AdvisorPresets[name]
		if len(preset.Args) == 0 {
			t.Errorf("%s: a preset with no argv cannot be non-interactive", name)
		}
		if preset.Description == "" {
			t.Errorf("%s: preset needs a description for the tool schema", name)
		}
		placeholders := 0
		for _, arg := range preset.Args {
			if arg == AdvisorQuestionPlaceholder {
				placeholders++
			} else if strings.Contains(arg, AdvisorQuestionPlaceholder) {
				t.Errorf("%s: %q embeds the placeholder in a larger argument", name, arg)
			}
		}
		if placeholders > 1 {
			t.Errorf("%s: placeholder used %d times", name, placeholders)
		}
	}
	// The agents that edit files and run commands must never be shipped as
	// read-only: that flag is what lets a consultation happen silently.
	for _, name := range []string{"aider", "goose", "opencode"} {
		if AdvisorPresets[name].ReadOnly {
			t.Errorf("%s can act on the machine; it must not be a read-only preset", name)
		}
	}
}

func TestNoAdvisorsByDefault(t *testing.T) {
	if len(Default().Advisors) != 0 {
		t.Error("delegation must be off until an advisor is configured")
	}
}
