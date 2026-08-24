package config

import (
	"fmt"
	"strings"
)

// This file is the assistant's identity (issue #103): the single place the
// name the user chose — and the spellings whisper mishears it as — is
// defined. Everything that hears, parses, or speaks the name derives from
// here: the STT bias sentence (stt.go), the wake-transcript strip in
// internal/session, the wake detector's word (WakeDetectorWord), and the
// default system prompt's self-reference (DefaultSystemPrompt). None of those
// call-sites may carry its own copy of the name; a grep-guard test in this
// package holds them to that.
//
// Product branding deliberately does not follow: the window title, the bar
// tooltip, the docs, and the binary/socket/service names stay "Jarvix"
// whatever the user calls their assistant, because those name the product on
// disk, not the persona in conversation.

// Assistant is the [assistant] table: what the assistant is called.
type Assistant struct {
	// Name is what the assistant answers to and refers to itself as. It may
	// be more than one word ("Mister Smith"); the transcript strip matches
	// word sequences, and the bias prompt presents it as a proper noun.
	Name string `toml:"name"`
	// Aliases are the spellings the wake-transcript strip accepts as the
	// name, in addition to the name itself. Whisper only writes words it
	// knows, so even a correctly *detected* summons is often *transcribed*
	// as a nearby real word (issue #83) — aliases are how those transcripts
	// still count as the summons. Transcript-side only: the acoustic wake
	// gate never sees them.
	//
	// Unset (absent from the file) means "the shipped mishearings of the
	// default name — or nothing, for a custom name": see EffectiveAliases.
	// The distinction between unset and explicitly empty is why this is a
	// nil-able slice rather than a value with a baked-in default.
	Aliases []string `toml:"aliases"`
}

// EffectiveAliases resolves the alias list the strip should use. Explicitly
// configured aliases (including an explicit empty list) win. Unset aliases
// fall back to the shipped mishearing list — but only while the name is the
// default one: the shipped list was tuned against what whisper's English
// models write when they hear the *default* name, and inheriting it for
// "Hal" would strip words that have nothing to do with Hal.
func (a Assistant) EffectiveAliases() []string {
	if a.Aliases != nil {
		return a.Aliases
	}
	if a.CustomName() {
		return nil
	}
	return defaultAssistantAliases()
}

// CustomName reports whether the configured name differs from the shipped
// default (case-insensitively — restating the default in different case is
// not choosing a new name).
func (a Assistant) CustomName() bool {
	return !strings.EqualFold(strings.TrimSpace(a.Name), defaultAssistantName)
}

// WakeDetectorWord is what the wake detector helper is handed. The word (or
// model path) in activation.wake_word wins when set — openWakeWord ships no
// model for most names, so pointing the detector at a bundled word or a
// self-trained .onnx file while keeping the chosen name everywhere else is a
// legitimate, documented setup. Unset, the detector follows the assistant's
// name, lowercased: model lookups are spelled lowercase, and the previous
// default ("jarvix") was too, so the derivation is byte-identical for the
// shipped configuration.
func (c Config) WakeDetectorWord() string {
	if w := strings.TrimSpace(c.Activation.WakeWord); w != "" {
		return w
	}
	return strings.ToLower(strings.TrimSpace(c.Assistant.Name))
}

// assistantProblems validates the [assistant] table. Every message starts
// with the dotted key so the settings screen can pin it to its field.
func (c Config) assistantProblems() []string {
	var problems []string
	name := foldWords(c.Assistant.Name)
	if name == "" {
		problems = append(problems, fmt.Sprintf(
			"assistant.name is empty; set the name the assistant answers to and calls itself (e.g. %q)",
			defaultAssistantName))
	}
	// Duplicates are compared case-insensitively and whitespace-normalised,
	// because that is exactly how the transcript strip compares them: two
	// entries the strip cannot tell apart are one entry written twice.
	seen := map[string]bool{}
	for _, alias := range c.Assistant.Aliases {
		folded := foldWords(alias)
		switch {
		case folded == "":
			problems = append(problems,
				"assistant.aliases contains an empty entry; each one must be a spelling the name arrives as in transcripts (e.g. \"jarvis\")")
		case folded == name:
			problems = append(problems, fmt.Sprintf(
				"assistant.aliases entry %q is the name itself; the strip already accepts the name, so list only its mishearings", alias))
		case seen[folded]:
			problems = append(problems, fmt.Sprintf(
				"assistant.aliases lists %q twice; aliases are matched case-insensitively, so one entry per spelling", alias))
		}
		seen[folded] = true
	}
	return problems
}

// foldWords normalises a name or alias for comparison the way the transcript
// strip does: case-insensitively, one space between words.
func foldWords(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// DefaultSystemPrompt composes the shipped base prompt around the assistant's
// name. It carries the honesty rule alongside the speech style because it
// binds even with every tool switched off: a session that cannot act must say
// so, never narrate an action it has no way to perform. The wording is pinned
// by TestSystemPromptPinsTheHonestyRule — a live session narrated launches
// and window moves with tool_calls=0 (issue #71), and this sentence is the
// standing instruction against that.
func DefaultSystemPrompt(name string) string {
	return "You are " + name + ", a voice assistant built into the user's Linux computer. " +
		"Your responses are spoken aloud, so answer concisely in plain prose: no markdown, " +
		"no lists, no code blocks, no preamble. Get straight to the point. " +
		"Never say you have done something, or are doing it, unless you really did it; if you cannot " +
		"do something, say so plainly instead of describing it as done."
}

// AssistantSystemPrompt is the system prompt the engine runs with: the
// configured base plus the instructions for each enabled tool. The tool
// flags decide the suffixes because the tool registry is built from them —
// on a reload the running (booted) tool flags are what matter, not the file.
// It lives here rather than in the daemon so doctor's context-floor check
// (issue #71) measures the same prompt the daemon sends, from one copy.
func AssistantSystemPrompt(cfg Config) string {
	prompt := cfg.AI.SystemPrompt
	// The untouched default follows the configured name: it is shipped text,
	// not the user's words, so its self-reference belongs to assistant.name.
	// A hand-written ai.system_prompt is sent verbatim — whatever it calls
	// the assistant is what the user chose to write.
	if prompt == DefaultSystemPrompt(defaultAssistantName) {
		prompt = DefaultSystemPrompt(strings.TrimSpace(cfg.Assistant.Name))
	}
	if cfg.Tools.Shell {
		prompt += ToolSystemPrompt
	}
	if cfg.Tools.Artifacts {
		prompt += ArtifactSystemPrompt
	}
	if cfg.Tools.Desktop {
		prompt += DesktopSystemPrompt
	}
	if cfg.Tools.Typing.Enable {
		prompt += TypingSystemPrompt
	}
	if len(cfg.Advisors) > 0 {
		prompt += AdvisorSystemPrompt
	}
	if cfg.Memory.Enabled {
		prompt += MemorySystemPrompt
	}
	if len(cfg.Knowledge.Feeds) > 0 {
		prompt += KnowledgeSystemPrompt
	}
	return prompt
}
