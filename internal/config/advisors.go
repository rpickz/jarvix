package config

import (
	"fmt"
	"sort"
	"strings"
)

// This file owns the `[advisors.<name>]` tables: the assistant CLIs already
// installed and authenticated on the user's machine that Jarvix can hand a
// question to when the local model is the wrong tool for it (ADR 0016).
//
// The shape is deliberately the one `jarvix setup` already writes — a table
// per advisor whose only required key is `binary` — so a config produced by
// the wizard works with no further editing. Everything else comes from a
// shipped preset for the known CLIs.

// Advisor is one assistant CLI Jarvix can delegate a question to.
//
// The argv template lives here and only here: the model chooses *which*
// advisor and *what to ask*, never the binary, the flags, or their order.
// The question reaches the child either on stdin (the default) or as the one
// argv element written as "{question}" — never through a shell.
type Advisor struct {
	// Binary is the CLI to run: an absolute path (what the wizard records),
	// or a bare name resolved on PATH at call time. Empty means the advisor's
	// own name.
	Binary string `toml:"binary"`
	// Args is the argv template passed to Binary. Exactly one element may be
	// the literal "{question}", which is replaced by the question as a single
	// argument; with no such element the question goes to the child's stdin.
	// Unset means the shipped preset for this advisor name.
	Args []string `toml:"args"`
	// TimeoutSec bounds one consultation. The process group is killed past
	// it. Zero means DefaultAdvisorTimeoutSec.
	TimeoutSec int `toml:"timeout_sec"`
	// Description tells the model what this advisor is good for; it appears
	// in the tool schema. Unset means the preset's description.
	Description string `toml:"description"`

	// ReadOnly reports whether this advisor runs a shipped preset, unchanged,
	// that only reads and answers. It is computed from the preset table, never
	// read from the config file — a user-supplied argv is something Jarvix has
	// not audited, so it loses the read-only claim and its calls need
	// confirmation (ADR 0016).
	ReadOnly bool `toml:"-"`
}

// DefaultAdvisorTimeoutSec bounds one consultation. Two minutes is long
// enough for a real code review on a large repo and short enough that a hung
// CLI does not hold a voice session hostage.
const DefaultAdvisorTimeoutSec = 120

// AdvisorQuestionPlaceholder is the argv element replaced by the user's
// question. Whole-element only: the question is never interpolated into a
// larger string, so it cannot grow flags or a second command.
const AdvisorQuestionPlaceholder = "{question}"

// AdvisorPreset is the shipped configuration for a known assistant CLI, so
// recording `binary = "…"` (all `jarvix setup` writes) is enough to use it.
type AdvisorPreset struct {
	// Args is the non-interactive argv for a single question.
	Args []string
	// ReadOnly marks a preset that answers questions without acting on the
	// machine: a one-shot print/exec mode with no file-editing or
	// command-running powers granted by default. It decides the advisor's
	// default permission tier — allow for these, ask for the rest.
	ReadOnly bool
	// Description is what the model is told this advisor is good for.
	Description string
}

// AdvisorPresets covers the CLIs `jarvix setup` detects. The argv choices
// are each a documented non-interactive mode; a CLI whose flags move on is
// fixed by setting `args` in the config, which also (deliberately) demotes
// the advisor to the ask tier.
var AdvisorPresets = map[string]AdvisorPreset{
	// Print mode answers one prompt and exits. It cannot prompt for tool
	// permission, so file edits and commands are refused rather than run.
	"claude": {
		Args:        []string{"-p"},
		ReadOnly:    true,
		Description: "Claude Code — deep reasoning, code review, long-context analysis",
	},
	// `exec` is the non-interactive mode; the explicit read-only sandbox is
	// what earns the allow tier, and "-" reads the prompt from stdin.
	"codex": {
		Args:        []string{"exec", "--sandbox", "read-only", "-"},
		ReadOnly:    true,
		Description: "OpenAI Codex CLI — code reasoning and review in a read-only sandbox",
	},
	// -p is Gemini's one-shot prompt flag; it takes the prompt as its value,
	// so this preset uses the single-argument form.
	"gemini": {
		Args:        []string{"-p", AdvisorQuestionPlaceholder},
		ReadOnly:    true,
		Description: "Google Gemini CLI — broad general knowledge and long-context reasoning",
	},
	// The remaining three are coding agents: their whole purpose is editing
	// files and running commands, so they are never read-only, whatever the
	// flags say. Delegating to one is a real action and asks first.
	"aider": {
		Args:        []string{"--message", AdvisorQuestionPlaceholder, "--no-pretty", "--no-auto-commits"},
		Description: "Aider — pair-programming agent that edits files in the current repository",
	},
	"goose": {
		Args:        []string{"run", "--text", AdvisorQuestionPlaceholder},
		Description: "Goose — agent that can run commands and change files on this machine",
	},
	"opencode": {
		Args:        []string{"run", AdvisorQuestionPlaceholder},
		Description: "OpenCode — terminal coding agent that can change files in the current project",
	},
}

// KnownAdvisors lists the preset advisor names, sorted. `jarvix setup` looks
// for exactly these on PATH, so the wizard and the runtime can never drift.
func KnownAdvisors() []string {
	names := make([]string, 0, len(AdvisorPresets))
	for name := range AdvisorPresets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// AdvisorSystemPrompt is appended to the system prompt when at least one
// advisor is configured. Local-first is a prompt-level rule because it is a
// judgement, not a pattern: the tool cannot tell "what time is it?" from
// "review this architecture", but the model can, and the cost of getting it
// wrong is two minutes of silence.
const AdvisorSystemPrompt = " A stronger assistant is installed on this computer and you can " +
	"consult it with the advisor.ask tool. Answer everything you can yourself — the time, quick " +
	"facts, conversions, anything about this computer — and consult an advisor only when the " +
	"question genuinely exceeds you (deep reasoning, reviewing or planning a lot of material, " +
	"knowledge you do not have) or when the user asks you to ask it by name. A consultation " +
	"takes up to two minutes of silence, so it must earn the wait. When an advisor answers, give " +
	"the user that answer: stay faithful to it, keep it short enough to listen to, and never read " +
	"out file paths, URLs, or code. If a consultation fails, say so in one sentence and do not " +
	"retry it."

// applyAdvisorDefaults fills each configured advisor from its preset, so a
// table holding nothing but `binary` is complete and usable.
func applyAdvisorDefaults(cfg *Config) {
	for name, a := range cfg.Advisors {
		preset := AdvisorPresets[name]
		if a.Binary == "" {
			a.Binary = name // resolved on PATH at call time
		}
		if a.Args == nil {
			// Only an untouched preset argv earns the read-only claim; a
			// config-supplied argv (including an explicitly empty one) is
			// unaudited by definition.
			a.Args = preset.Args
			a.ReadOnly = preset.ReadOnly
		}
		// Only an unset timeout is defaulted: a negative one is a typo, and
		// Validate should say so rather than quietly mean two minutes.
		if a.TimeoutSec == 0 {
			a.TimeoutSec = DefaultAdvisorTimeoutSec
		}
		if a.Description == "" {
			a.Description = preset.Description
		}
		if a.Description == "" {
			a.Description = fmt.Sprintf("%s — a stronger assistant CLI on this computer", name)
		}
		cfg.Advisors[name] = a
	}
}

// AdvisorNames lists the configured advisor names, sorted, so tool schemas,
// doctor output, and error messages are deterministic.
func (c Config) AdvisorNames() []string {
	names := make([]string, 0, len(c.Advisors))
	for name := range c.Advisors {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// validateAdvisors reports configuration a user must fix. Messages name the
// offending table and what is accepted.
func (c Config) validateAdvisors() []string {
	var problems []string
	for _, name := range c.AdvisorNames() {
		a := c.Advisors[name]
		if !validAdvisorName(name) {
			problems = append(problems, fmt.Sprintf(
				"advisor name %q is invalid; use letters, digits, dashes or underscores "+
					"(the table is [advisors.<name>], e.g. [advisors.claude])", name))
		}
		// Defaulting fills an unset binary with the advisor's own name, so
		// this only fires for a Config built in code — belt and braces.
		if strings.TrimSpace(a.Binary) == "" {
			problems = append(problems, fmt.Sprintf(
				"advisors.%s.binary is empty; set the CLI to run, e.g. binary = \"/usr/bin/%s\"", name, name))
		}
		placeholders := 0
		for _, arg := range a.Args {
			if arg == AdvisorQuestionPlaceholder {
				placeholders++
				continue
			}
			if strings.Contains(arg, AdvisorQuestionPlaceholder) {
				problems = append(problems, fmt.Sprintf(
					"advisors.%s.args entry %q embeds %s in a larger argument; it must be an "+
						"argument of its own, so the question can never become a flag",
					name, arg, AdvisorQuestionPlaceholder))
			}
		}
		if placeholders > 1 {
			problems = append(problems, fmt.Sprintf(
				"advisors.%s.args repeats %s %d times; use it at most once (omit it entirely "+
					"to send the question on stdin)", name, AdvisorQuestionPlaceholder, placeholders))
		}
		if a.TimeoutSec <= 0 {
			problems = append(problems, fmt.Sprintf(
				"advisors.%s.timeout_sec must be positive (seconds before the advisor is killed)", name))
		}
	}
	return problems
}

// validAdvisorName keeps advisor names to something a model can name back
// exactly and a human can read in a spoken confirmation.
func validAdvisorName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}
