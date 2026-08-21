package config

import "fmt"

// Context configures what Jarvix may look at on the desktop before it answers
// (ADR 0018): the focused window, the text the user has highlighted, and the
// clipboard. Every source is opt-in on its own, because they carry very
// different amounts of the user's life:
//
//   - window is a title bar — public, already on screen, and the difference
//     between "what does this error mean?" being answerable and not;
//   - selection is what the user is pointing at as they speak;
//   - clipboard is whatever they last copied, for any purpose, which is why
//     it is the one source that defaults off.
//
// A source switched off here is not gathered at all — no subprocess runs, so
// its content never exists to be leaked, logged, or sent.
type Context struct {
	// Window offers the active window's app and title.
	Window bool `toml:"window"`
	// Selection offers the Wayland primary selection (highlighted text).
	Selection bool `toml:"selection"`
	// Clipboard offers the regular clipboard. Off by default.
	Clipboard bool `toml:"clipboard"`
	// MaxChars caps how much of each source reaches the model; longer content
	// is truncated with a marker.
	MaxChars int `toml:"max_chars"`
	// TimeoutMs bounds gathering — both per source and in total, since
	// sources are gathered in parallel. It may be lowered but never raised
	// past MaxContextTimeoutMs: context is never worth more latency than that.
	TimeoutMs int `toml:"timeout_ms"`
}

// MaxContextTimeoutMs is the hard ceiling on context gathering, in
// milliseconds. It is a constant rather than a preference: the feature's
// premise is that context is free, and a configuration that could make the
// assistant hesitate for a second would quietly trade the product for it.
const MaxContextTimeoutMs = 300

// AnySource reports whether any context source is enabled. False is the
// zero-cost path: no collector is built and no session pays anything.
func (c Context) AnySource() bool {
	return c.Window || c.Selection || c.Clipboard
}

// EnabledSources lists the enabled sources in gathering order, for doctor and
// for diagnostics.
func (c Context) EnabledSources() []string {
	var out []string
	if c.Window {
		out = append(out, "window")
	}
	if c.Selection {
		out = append(out, "selection")
	}
	if c.Clipboard {
		out = append(out, "clipboard")
	}
	return out
}

// contextProblems validates the [context] table.
func (c Config) contextProblems() []string {
	var problems []string
	if c.Context.MaxChars <= 0 {
		problems = append(problems,
			"context.max_chars must be positive (characters of each source offered to the model)")
	}
	switch {
	case c.Context.TimeoutMs <= 0:
		problems = append(problems,
			"context.timeout_ms must be positive (the time desktop context may take to gather)")
	case c.Context.TimeoutMs > MaxContextTimeoutMs:
		problems = append(problems, fmt.Sprintf(
			"context.timeout_ms is %d; it must not exceed %d — desktop context is never worth "+
				"more latency than that", c.Context.TimeoutMs, MaxContextTimeoutMs))
	}
	return problems
}
