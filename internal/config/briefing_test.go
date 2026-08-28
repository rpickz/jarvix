package config

import (
	"strings"
	"testing"
)

// The return briefing's configuration (#150, ADR 0050).

// TestBriefingDefaultsAreOfferNotAmbush pins the shipped stance. On by
// default is safe because a machine that did nothing is silent anyway;
// speaking without being asked is not, so it is opt-in.
func TestBriefingDefaultsAreOfferNotAmbush(t *testing.T) {
	c := Default()
	if !c.Briefing.Enabled {
		t.Error("briefing.enabled defaults to false; with nothing to report there is no offer anyway")
	}
	if c.Briefing.AfterHours != 8 {
		t.Errorf("briefing.after_hours = %d, want 8 — a night away", c.Briefing.AfterHours)
	}
	if c.Briefing.SpeakOnReturn {
		t.Error("briefing.speak_on_return defaults to true; the default contract is offered, not ambushed")
	}
	if err := c.Validate(); err != nil {
		t.Errorf("the shipped defaults do not validate: %v", err)
	}
}

// TestBriefingSettingsAreLiveAndAdjustableByVoice pins the registry rows:
// live class (the service reads them at the moment it decides, so "stop
// offering me briefings" lands on the next answer), not dangerous (the widest
// thing they do is make Jarvix say one more sentence about the user's own
// work), and inside AssistantSettings — which is what makes them voice-
// adjustable through config.write_setting for free.
func TestBriefingSettingsAreLiveAndAdjustableByVoice(t *testing.T) {
	assistant := map[string]bool{}
	for _, s := range AssistantSettings() {
		assistant[s.Key] = true
	}
	for key, want := range map[string]SettingType{
		"briefing.enabled":         TypeBool,
		"briefing.after_hours":     TypeInt,
		"briefing.speak_on_return": TypeBool,
	} {
		s, ok := SettingFor(key)
		if !ok {
			t.Errorf("%s: not in the settings registry", key)
			continue
		}
		if s.Type != want {
			t.Errorf("%s: type = %q, want %q", key, s.Type, want)
		}
		if s.Reload != ReloadLive {
			t.Errorf("%s: reload = %q, want live — the briefing reads it at the moment it decides", key, s.Reload)
		}
		if s.Dangerous {
			t.Errorf("%s: marked dangerous, but it widens nothing", key)
		}
		if !assistant[key] {
			t.Errorf("%s: excluded from AssistantSettings, so it cannot be adjusted by voice", key)
		}
	}
}

// TestBriefingAfterHoursIsBounded. The floor is what stops the offer becoming
// the interruption the feature removes; the ceiling is what catches a typo
// rather than silently switching the feature off.
func TestBriefingAfterHoursIsBounded(t *testing.T) {
	for _, tc := range []struct {
		hours int
		valid bool
	}{
		{0, false},
		{-1, false},
		{1, true},
		{8, true},
		{24 * 28, true},
		{24*28 + 1, false},
		{8000, false},
	} {
		c := Default()
		c.Briefing.AfterHours = tc.hours
		err := c.Validate()
		if tc.valid && err != nil {
			t.Errorf("after_hours %d rejected: %v", tc.hours, err)
		}
		if !tc.valid {
			if err == nil {
				t.Errorf("after_hours %d accepted", tc.hours)
			} else if !strings.Contains(err.Error(), "briefing.after_hours") {
				t.Errorf("after_hours %d: error does not name the key: %v", tc.hours, err)
			}
		}
	}
}

// TestBriefingSettingsRoundTripThroughTheRegistry: every row's Get and Apply
// agree, which is what the settings screen, the CLI and the assistant's tool
// all ride on.
func TestBriefingSettingsRoundTripThroughTheRegistry(t *testing.T) {
	c := Default()
	for key, value := range map[string]any{
		"briefing.enabled":         false,
		"briefing.after_hours":     12,
		"briefing.speak_on_return": true,
	} {
		s, ok := SettingFor(key)
		if !ok {
			t.Fatalf("%s: not in the registry", key)
		}
		if err := s.Apply(&c, value); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if got := s.Get(c); got != value {
			t.Errorf("%s: Get = %v, want %v", key, got, value)
		}
	}
	if c.Briefing.AfterHours != 12 || c.Briefing.Enabled || !c.Briefing.SpeakOnReturn {
		t.Errorf("the writes did not land on the struct: %+v", c.Briefing)
	}
}
