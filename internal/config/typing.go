package config

import (
	"fmt"
	"strings"
	"time"
)

// This file holds the [tools.typing] table's validation and the system prompt
// that goes with it (ADR 0023). It is separate from config.go because the
// capability is separate: everything about typing should be readable in one
// place, including the sentence that tells the model what it may and may not
// do with it.

// maxTypingChars is the largest payload cap that may be configured.
//
// A cap exists on the cap because the setting is a blast-radius control, and a
// control a user can set to a million is not one. Fifty thousand characters is
// already far more than anybody dictates in one breath; past that the number
// is a mistake or a misunderstanding, and it is worth saying so at startup
// rather than discovering it when something types a novel.
const maxTypingChars = 50_000

// maxTypingRateWindowSec bounds the rate limiter's window at an hour. Longer
// than that and a limit meant to stop a runaway loop becomes a limit that
// stops the user working.
const maxTypingRateWindowSec = 3600

// RateWindow returns the rate limiter's window as a duration.
func (t Typing) RateWindow() time.Duration {
	return time.Duration(t.RateWindowSec) * time.Second
}

// typingProblems validates the [tools.typing] table. Messages name the key and
// the accepted range, because a refused config is only actionable if it says
// what to write instead.
func (c Config) typingProblems() []string {
	t := c.Tools.Typing
	var problems []string
	if t.MaxChars < 1 || t.MaxChars > maxTypingChars {
		problems = append(problems, fmt.Sprintf(
			"tools.typing.max_chars is %d; it must be between 1 and %d characters",
			t.MaxChars, maxTypingChars))
	}
	if t.RateLimit < 1 {
		problems = append(problems, fmt.Sprintf(
			"tools.typing.rate_limit is %d; it must be at least 1 — use [tools.typing] enable = false to switch typing off",
			t.RateLimit))
	}
	if t.RateWindowSec < 1 || t.RateWindowSec > maxTypingRateWindowSec {
		problems = append(problems, fmt.Sprintf(
			"tools.typing.rate_window_sec is %d; it must be between 1 and %d seconds",
			t.RateWindowSec, maxTypingRateWindowSec))
	}
	for _, class := range t.TerminalClasses {
		if strings.TrimSpace(class) == "" {
			problems = append(problems,
				"tools.typing.terminal_classes contains an empty entry; each one must be a window class such as \"alacritty\"")
		}
	}
	if strings.ContainsAny(t.Binary, " \t") {
		problems = append(problems, fmt.Sprintf(
			"tools.typing.binary %q contains whitespace; it is executed directly, not through a shell, so it must be one program name or absolute path",
			t.Binary))
	}
	return problems
}

// TypingSystemPrompt is appended to the system prompt when the typing tools
// are enabled.
//
// Three things have to be said here rather than left to the tool descriptions,
// because all three are judgements the model makes before it calls anything:
// that dictation goes into whatever the user is working in, that the text is
// only the characters they want entered, and that sending is a separate act
// it must not assume it has been given. The last one matters most — a model
// that types a message and then presses enter "to be helpful" has sent
// something nobody approved.
const TypingSystemPrompt = " You can type into the window the user is working in. When they ask " +
	"you to write, enter or dictate something into what is on their screen, use the typing tool " +
	"with exactly the characters they want entered — no line breaks, no quotation marks you added, " +
	"and no commentary. Typing never submits: it does not press enter, send, or save. If the user " +
	"asked for something to be sent as well, that is a separate key-press call, and they will be " +
	"asked to approve it on its own — never assume approval to type was approval to send. Both are " +
	"confirmed out loud before anything happens; if a tool result says the window changed, that the " +
	"request was declined, or that a limit refused it, do not retry — say what happened in one " +
	"short sentence and wait. Never read the typed text back to the user: it is already on their " +
	"screen, and it may be private."
