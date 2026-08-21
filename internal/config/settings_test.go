package config

import (
	"reflect"
	"testing"
)

// sampleValue returns a valid, non-default value for a setting type.
func sampleValue(t SettingType) any {
	switch t {
	case TypeString:
		return "sample"
	case TypeInt:
		return 42
	case TypeFloat:
		return 1.25
	case TypeBool:
		return true
	case TypeStringList:
		return []string{"a", "b"}
	}
	return nil
}

// TestEverySettingRoundTrips applies a value through each registry entry and
// reads it back — a stale Get or set closure fails here, not in production.
func TestEverySettingRoundTrips(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range Settings() {
		if seen[s.Key] {
			t.Errorf("duplicate setting key %q", s.Key)
		}
		seen[s.Key] = true
		switch s.Reload {
		case ReloadLive, ReloadIdle, ReloadRestart:
		default:
			t.Errorf("%s: invalid reload class %q", s.Key, s.Reload)
		}
		if s.Label == "" {
			t.Errorf("%s: no label", s.Key)
		}

		cfg := Default()
		want := sampleValue(s.Type)
		if s.Type == TypeBool {
			// Flip the default so the round trip proves a write happened.
			want = !s.Get(cfg).(bool)
		}
		if err := s.Apply(&cfg, want); err != nil {
			t.Errorf("%s: Apply: %v", s.Key, err)
			continue
		}
		if got := s.Get(cfg); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: Get after Apply = %v, want %v", s.Key, got, want)
		}
	}
}

// TestSettingsRoundTripThroughRewrite writes every setting into a TOML
// document and loads it back — proving registry, encoder, and parser agree
// on every key's location and type.
func TestSettingsRoundTripThroughRewrite(t *testing.T) {
	changes := make(map[string]any)
	for _, s := range Settings() {
		v := sampleValue(s.Type)
		if s.Type == TypeBool {
			v = !s.Get(Default()).(bool)
		}
		changes[s.Key] = v
	}
	doc, err := RewriteTOML(nil, changes)
	if err != nil {
		t.Fatalf("RewriteTOML: %v", err)
	}
	cfg, err := ParseBytes(doc)
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	for _, s := range Settings() {
		if got := s.Get(cfg); !reflect.DeepEqual(got, changes[s.Key]) {
			t.Errorf("%s = %v after round trip, want %v", s.Key, got, changes[s.Key])
		}
	}
}

func TestCoerce(t *testing.T) {
	cases := []struct {
		typ  SettingType
		in   any
		want any
		fail bool
	}{
		{TypeString, "x", "x", false},
		{TypeString, 3, nil, true},
		{TypeInt, "42", 42, false},
		{TypeInt, float64(42), 42, false},
		{TypeInt, float64(1.5), nil, true},
		{TypeInt, "abc", nil, true},
		{TypeFloat, "1.5", 1.5, false},
		{TypeFloat, float64(2), 2.0, false},
		{TypeBool, "true", true, false},
		{TypeBool, "off", false, false},
		{TypeBool, "maybe", nil, true},
		{TypeStringList, "a, b", []string{"a", "b"}, false},
		{TypeStringList, "", []string{}, false},
		{TypeStringList, []any{"a"}, []string{"a"}, false},
		{TypeStringList, []any{1}, nil, true},
	}
	for _, tc := range cases {
		s := Setting{Key: "test", Type: tc.typ}
		got, err := s.Coerce(tc.in)
		if tc.fail {
			if err == nil {
				t.Errorf("%s coerce %v: expected error, got %v", tc.typ, tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s coerce %v: %v", tc.typ, tc.in, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s coerce %v = %v, want %v", tc.typ, tc.in, got, tc.want)
		}
	}
}
