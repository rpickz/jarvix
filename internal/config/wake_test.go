package config

import (
	"strings"
	"testing"
)

// wakeConfig is a valid default with background listening switched on.
func wakeConfig() Config {
	cfg := Default()
	cfg.Activation.Mode = ModeWakeWord
	return cfg
}

// The shipped defaults must be a working wake-word configuration, so turning
// the feature on is one setting rather than six.
func TestWakeWordDefaultsValidate(t *testing.T) {
	if err := wakeConfig().Validate(); err != nil {
		t.Fatalf("the defaults do not make a valid wake-word configuration: %v", err)
	}
	if Default().Activation.WakeWordEnabled() {
		t.Error("background listening is on by default; a microphone that opens itself must be opted into")
	}
}

// activation.mode is a closed set, and the message has to name the values —
// a user who typed "wake" needs to be told what to type instead.
func TestActivationModeIsValidated(t *testing.T) {
	cfg := Default()
	cfg.Activation.Mode = "wake"
	err := cfg.Validate()
	if err == nil {
		t.Fatal("an unknown activation.mode was accepted")
	}
	for _, want := range []string{ModePushToTalk, ModeWakeWord} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not offer %q: %v", want, err)
		}
	}
}

// The pre-roll ceiling is a privacy guarantee, so exceeding it is a
// validation failure rather than a value that is quietly clamped — a user who
// asked for ten seconds of look-back should be told they cannot have it, and
// why.
func TestPreRollCeilingIsEnforcedWithItsReason(t *testing.T) {
	cfg := wakeConfig()
	cfg.Activation.WakeRingMs = 10000
	err := cfg.Validate()
	if err == nil {
		t.Fatal("a ten-second pre-roll was accepted")
	}
	if !strings.Contains(err.Error(), "privacy") {
		t.Errorf("the message should say why the limit exists: %v", err)
	}

	// Zero is legitimate — and the most private setting there is.
	cfg.Activation.WakeRingMs = 0
	if err := cfg.Validate(); err != nil {
		t.Errorf("a zero pre-roll was rejected: %v", err)
	}
}

// The range checks that apply whatever the mode is. A bad value must not sit
// unnoticed in the file waiting to take effect the day someone switches
// background listening on.
func TestWakeRangesAreCheckedInEveryMode(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(*Config)
		want string
	}{
		{"sensitivity above one", func(c *Config) { c.Activation.WakeSensitivity = 2 }, "wake_sensitivity"},
		{"sensitivity below zero", func(c *Config) { c.Activation.WakeSensitivity = -1 }, "wake_sensitivity"},
		{"negative pre-roll", func(c *Config) { c.Activation.WakeRingMs = -1 }, "wake_ring_ms"},
		{"negative endpoint", func(c *Config) { c.Activation.EndpointSilenceMs = -1 }, "endpoint_silence_ms"},
		{"negative utterance cap", func(c *Config) { c.Activation.MaxUtteranceSec = -1 }, "max_utterance_sec"},
	} {
		cfg := Default() // push-to-talk: the wake settings are dormant
		c.edit(&cfg)
		err := cfg.Validate()
		if err == nil {
			t.Errorf("%s: accepted in push-to-talk mode", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: the error does not name %s: %v", c.name, c.want, err)
		}
	}
}

// The checks that only matter once the microphone is actually open. In
// push-to-talk mode they must not fire: an empty wake_command is irrelevant
// to someone who has never enabled the feature.
func TestWakeRequirementsApplyOnlyWhenEnabled(t *testing.T) {
	for _, c := range []struct {
		name string
		edit func(*Config)
		want string
	}{
		{"no detector", func(c *Config) { c.Activation.WakeCommand = nil }, "wake_command"},
		{"detector with a shell string", func(c *Config) {
			c.Activation.WakeCommand = []string{"jarvix-wake --model x"}
		}, "whitespace"},
		{"endpoint too short to be usable", func(c *Config) { c.Activation.EndpointSilenceMs = 10 }, "endpoint_silence_ms"},
		{"endpoint absurdly long", func(c *Config) { c.Activation.EndpointSilenceMs = 60000 }, "endpoint_silence_ms"},
		{"no utterance cap", func(c *Config) { c.Activation.MaxUtteranceSec = 0 }, "max_utterance_sec"},
	} {
		off := Default()
		c.edit(&off)
		if err := off.Validate(); err != nil {
			t.Errorf("%s: rejected while background listening is off: %v", c.name, err)
		}

		on := wakeConfig()
		c.edit(&on)
		err := on.Validate()
		if err == nil {
			t.Errorf("%s: accepted with background listening on", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: the error does not mention %s: %v", c.name, c.want, err)
		}
	}
}

// Every wake setting has to be reachable from `jarvix config set` and the
// settings screen, and every one of them is wired at daemon construction —
// so all of them are restart class. A setting that claimed to apply live
// would silently do nothing.
func TestWakeSettingsAreRegisteredAsRestartClass(t *testing.T) {
	want := []string{
		"activation.mode", "activation.wake_word", "activation.wake_command",
		"activation.wake_sensitivity", "activation.endpoint_silence_ms",
		"activation.wake_ring_ms", "activation.max_utterance_sec",
	}
	for _, key := range want {
		s, ok := SettingFor(key)
		if !ok {
			t.Errorf("%s is not in the settings registry, so nothing can change it", key)
			continue
		}
		if s.Reload != ReloadRestart {
			t.Errorf("%s is %s class; the wake listener is wired at boot", key, s.Reload)
		}
		if s.Label == "" {
			t.Errorf("%s has no label for the settings screen", key)
		}
	}
	mode, _ := SettingFor("activation.mode")
	if len(mode.Enum) != 2 {
		t.Errorf("activation.mode offers %v; it is a closed set of two", mode.Enum)
	}
}

// The mode round-trips through the registry's coercion, which is how
// `jarvix config set activation.mode=wake_word` reaches the file.
func TestWakeSettingsRoundTripThroughTheRegistry(t *testing.T) {
	cfg := Default()
	for key, value := range map[string]any{
		"activation.mode":                "wake_word",
		"activation.wake_word":           "computer",
		"activation.wake_sensitivity":    "0.7",
		"activation.endpoint_silence_ms": "600",
		"activation.wake_ring_ms":        "900",
		"activation.wake_command":        "jarvix-wake,--verbose",
	} {
		s, ok := SettingFor(key)
		if !ok {
			t.Fatalf("%s is missing", key)
		}
		if err := s.Apply(&cfg, value); err != nil {
			t.Fatalf("%s = %v: %v", key, value, err)
		}
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the applied configuration does not validate: %v", err)
	}
	a := cfg.Activation
	if a.Mode != ModeWakeWord || a.WakeWord != "computer" || a.WakeSensitivity != 0.7 ||
		a.EndpointSilenceMs != 600 || a.WakeRingMs != 900 {
		t.Errorf("values did not round-trip: %+v", a)
	}
	if len(a.WakeCommand) != 2 || a.WakeCommand[0] != "jarvix-wake" {
		t.Errorf("the detector command did not round-trip: %v", a.WakeCommand)
	}
}

// The durations the daemon actually hands the listener.
func TestActivationDurationHelpers(t *testing.T) {
	a := Activation{WakeRingMs: 1200, EndpointSilenceMs: 800, MaxUtteranceSec: 15}
	if got := a.WakeRing().Milliseconds(); got != 1200 {
		t.Errorf("WakeRing() is %dms", got)
	}
	if got := a.EndpointSilence().Milliseconds(); got != 800 {
		t.Errorf("EndpointSilence() is %dms", got)
	}
	if got := a.MaxUtterance().Seconds(); got != 15 {
		t.Errorf("MaxUtterance() is %vs", got)
	}
}
