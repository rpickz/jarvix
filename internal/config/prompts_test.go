package config

import (
	"strings"
	"testing"
)

// These tests pin the honesty steering verbatim, the way the archive search's
// no-match wording is pinned (#67): the sentences are the only standing
// instruction against a model narrating actions it never performed — a live
// session claimed launches and window moves with tool_calls=0 (issue #71) —
// and a well-meaning rewrite must not soften them unreviewed.

func TestSystemPromptPinsTheHonestyRule(t *testing.T) {
	// Pinned verbatim: the base prompt binds even with every tool off, when
	// "I've opened it" cannot ever be true.
	want := "Never say you have done something, or are doing it, unless you really did it; " +
		"if you cannot do something, say so plainly instead of describing it as done."
	if got := Default().AI.SystemPrompt; !strings.Contains(got, want) {
		t.Errorf("default system prompt lost the honesty rule:\n%q\nmust contain\n%q", got, want)
	}
}

func TestToolSystemPromptPinsTheHonestyRule(t *testing.T) {
	// Pinned verbatim: the cardinal rule of the tool loop — an action exists
	// only as the tool call that performs it, made in this turn.
	want := "The cardinal rule: an action only happens when you make the tool call that " +
		"performs it, in this turn. Never describe an action as done or underway unless you are " +
		"making that call; if you did not call the tool, the action did not happen — say plainly " +
		"that you have not done it."
	if !strings.Contains(ToolSystemPrompt, want) {
		t.Errorf("tool system prompt lost the cardinal rule:\n%q\nmust contain\n%q",
			ToolSystemPrompt, want)
	}
}

func TestConfigSystemPromptPinsTheContract(t *testing.T) {
	// Pinned verbatim (issue #105): the self-configuration honesty rule — a
	// validation failure is corrected or reported, never papered over with a
	// claimed success — and the off-limits statement, so the model refuses
	// the excluded space in words instead of discovering it by calling.
	for _, want := range []string{
		"fix exactly what they name and retry, or tell the user " +
			"the real problem — never claim a change you did not make",
		"The tool permission policy, advisors, and AI provider settings are off limits to you",
		"confirm in one short sentence what actually changed, using the values the tool result " +
			"reports — never a paraphrase of the request",
		"read it with config.get_entry",
	} {
		if !strings.Contains(ConfigSystemPrompt, want) {
			t.Errorf("config system prompt lost its contract:\n%q\nmust contain\n%q",
				ConfigSystemPrompt, want)
		}
	}
	// Always present: the tools are always registered, so the prompt is too.
	if got := AssistantSystemPrompt(Default()); !strings.Contains(got, ConfigSystemPrompt) {
		t.Error("the assembled system prompt does not carry the config tool guidance")
	}
}
