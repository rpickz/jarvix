package config

import (
	"strings"
	"testing"
)

func TestContextDefaults(t *testing.T) {
	c := Default().Context
	// The privacy default, stated as a test so it cannot drift: a title bar
	// and a selection are already on screen; the clipboard is not.
	if !c.Window || !c.Selection {
		t.Errorf("window/selection = %v/%v, want both on", c.Window, c.Selection)
	}
	if c.Clipboard {
		t.Error("clipboard defaults on; it is the highest-risk source and must be opt-in")
	}
	if c.MaxChars != 2000 {
		t.Errorf("max_chars = %d, want 2000", c.MaxChars)
	}
	if c.TimeoutMs != MaxContextTimeoutMs {
		t.Errorf("timeout_ms = %d, want %d", c.TimeoutMs, MaxContextTimeoutMs)
	}
	if err := Default().Validate(); err != nil {
		t.Fatalf("the defaults must validate: %v", err)
	}
}

func TestContextEnabledSources(t *testing.T) {
	cases := []struct {
		in   Context
		want string
		any  bool
	}{
		{Context{}, "", false},
		{Context{Window: true}, "window", true},
		{Context{Window: true, Selection: true}, "window,selection", true},
		{Context{Window: true, Selection: true, Clipboard: true}, "window,selection,clipboard", true},
		{Context{Clipboard: true}, "clipboard", true},
	}
	for _, c := range cases {
		if got := strings.Join(c.in.EnabledSources(), ","); got != c.want {
			t.Errorf("EnabledSources(%+v) = %q, want %q", c.in, got, c.want)
		}
		if got := c.in.AnySource(); got != c.any {
			t.Errorf("AnySource(%+v) = %v, want %v", c.in, got, c.any)
		}
	}
}

func TestContextValidation(t *testing.T) {
	cases := map[string]struct {
		mutate func(*Config)
		want   string
	}{
		"zero cap":          {func(c *Config) { c.Context.MaxChars = 0 }, "context.max_chars must be positive"},
		"negative cap":      {func(c *Config) { c.Context.MaxChars = -1 }, "context.max_chars must be positive"},
		"zero timeout":      {func(c *Config) { c.Context.TimeoutMs = 0 }, "context.timeout_ms must be positive"},
		"timeout too large": {func(c *Config) { c.Context.TimeoutMs = 1000 }, "must not exceed 300"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := Default()
			c.mutate(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("%s validated", name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %v, want it to mention %q", err, c.want)
			}
		})
	}
	// Lowering the budget is always allowed; only raising it past the hard
	// ceiling is refused.
	cfg := Default()
	cfg.Context.TimeoutMs = 50
	if err := cfg.Validate(); err != nil {
		t.Errorf("a lowered budget was rejected: %v", err)
	}
}

func TestContextParsesFromTOML(t *testing.T) {
	cfg, err := ParseBytes([]byte(`
[context]
window = false
selection = true
clipboard = true
max_chars = 500
timeout_ms = 120
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Context.Window || !cfg.Context.Selection || !cfg.Context.Clipboard {
		t.Errorf("sources = %+v", cfg.Context)
	}
	if cfg.Context.MaxChars != 500 || cfg.Context.TimeoutMs != 120 {
		t.Errorf("limits = %+v", cfg.Context)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}

	// A file that says nothing about context keeps the defaults, so an
	// existing config.toml is unaffected by the feature landing.
	cfg, err = ParseBytes([]byte("[ai]\nmodel = \"llama3.2:3b\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Context.Window || !cfg.Context.Selection || cfg.Context.Clipboard {
		t.Errorf("context = %+v, want the defaults", cfg.Context)
	}
}
