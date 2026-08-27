package config

import (
	"strings"
	"testing"
)

// Reading comfort (issue #121): the transcript's typography — line spacing,
// message text size, letter spacing — is registry-driven. These tests pin the
// contract: defaults identical to the window's previously hard-coded
// rendering, bounded ranges refused with the standard field problem, live
// reload class, and voice reachability through the assistant's settings view.

// TestReadingComfortDefaultsPinTheHardCodedRendering pins the defaults to the
// values JarvixWindow.qml rendered before the settings existed: line height
// ×1.0 (QML's own default), the design text size ×1.0 (Style.font.subtitle,
// unscaled), and no extra letter spacing. A config that never touches these
// keys must render pixel-identically to before — changing any of these
// defaults changes what every untouched install looks like, so it is a
// reviewed decision, not a drive-by.
func TestReadingComfortDefaultsPinTheHardCodedRendering(t *testing.T) {
	ui := Default().UI
	if ui.LineSpacing != 1.0 {
		t.Errorf("default ui.line_spacing = %g, want 1.0 (the window's hard-coded line height)", ui.LineSpacing)
	}
	if ui.TextSize != 1.0 {
		t.Errorf("default ui.text_size = %g, want 1.0 (the unscaled design text size)", ui.TextSize)
	}
	if ui.LetterSpacing != 0.0 {
		t.Errorf("default ui.letter_spacing = %g, want 0.0 (no extra letter spacing)", ui.LetterSpacing)
	}
}

// TestReadingComfortBounds walks each knob's range: both ends are accepted,
// and a step outside either end is refused with a problem naming the key —
// the standard field problem config.set pins next to the settings field.
func TestReadingComfortBounds(t *testing.T) {
	cases := []struct {
		key string
		set func(*Config, float64)
		ok  []float64
		bad []float64
	}{
		{"ui.line_spacing", func(c *Config, v float64) { c.UI.LineSpacing = v },
			[]float64{minLineSpacing, 1.0, 1.5, maxLineSpacing},
			[]float64{minLineSpacing - 0.1, 0, -1, maxLineSpacing + 0.1}},
		{"ui.text_size", func(c *Config, v float64) { c.UI.TextSize = v },
			[]float64{minTextSize, 1.0, 1.25, maxTextSize},
			[]float64{minTextSize - 0.1, 0, -1, maxTextSize + 0.1}},
		{"ui.letter_spacing", func(c *Config, v float64) { c.UI.LetterSpacing = v },
			[]float64{0, 0.12, maxLetterSpacing},
			[]float64{-0.05, maxLetterSpacing + 0.01}},
	}
	for _, tc := range cases {
		for _, v := range tc.ok {
			cfg := Default()
			tc.set(&cfg, v)
			if err := cfg.Validate(); err != nil && strings.Contains(err.Error(), tc.key) {
				t.Errorf("%s = %g must validate: %v", tc.key, v, err)
			}
		}
		for _, v := range tc.bad {
			cfg := Default()
			tc.set(&cfg, v)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Errorf("%s = %g must be refused with a problem naming the key, got %v", tc.key, v, err)
			}
		}
	}
}

// TestReadingComfortSettingsAreLiveFloatsTheAssistantMayAdjust pins the
// registry rows' shape: float type (so the settings screen renders a numeric
// field and the CLI coerces "1.5"), live reload class (a change spoken
// mid-session lands on the transcript being looked at, like the rest of
// [ui]), not dangerous (typography widens nothing), and present in the
// assistant's settings view — which is what makes "increase the line spacing
// a bit" adjustable by voice through the config.write_setting tool.
func TestReadingComfortSettingsAreLiveFloatsTheAssistantMayAdjust(t *testing.T) {
	assistant := map[string]bool{}
	for _, s := range AssistantSettings() {
		assistant[s.Key] = true
	}
	for _, key := range []string{"ui.line_spacing", "ui.text_size", "ui.letter_spacing"} {
		s, ok := SettingFor(key)
		if !ok {
			t.Errorf("%s: not in the settings registry", key)
			continue
		}
		if s.Type != TypeFloat {
			t.Errorf("%s: type = %q, want float", key, s.Type)
		}
		if s.Reload != ReloadLive {
			t.Errorf("%s: reload = %q, want live — the transcript re-renders without waiting for idle", key, s.Reload)
		}
		if s.Dangerous {
			t.Errorf("%s: marked dangerous, but typography widens nothing", key)
		}
		if !assistant[key] {
			t.Errorf("%s: excluded from AssistantSettings, so it cannot be adjusted by voice", key)
		}
	}
}
