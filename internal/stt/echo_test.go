package stt

import "testing"

// The bias prompt exactly as config.STTBiasPromptWith composes it: the name
// sentence (#83/#107) and the taught hard-to-hear phrases (#129). The tests
// below never hard-code a sentence of their own — the rule under test is
// "compare against what was sent", and a literal here would test the opposite.
const biasBoth = "The assistant is called Jarvix. Conversations may mention: Hyprland, kubectl."

func TestPromptEcho(t *testing.T) {
	tests := []struct {
		name       string
		transcript string
		prompt     string
		want       bool
	}{
		// The reproduction from issue #191, byte for byte: whisper-cli's own
		// leading space, and the terminal full stop it chose.
		{"the reproduction", " The assistant is called Jarvix.",
			"The assistant is called Jarvix.", true},
		{"whole prompt echoed", " The assistant is called Jarvix. Conversations may mention: Hyprland, kubectl.",
			biasBoth, true},
		// Whisper is as likely to echo one sentence as both, and the second
		// sentence is the one that carries the taught phrases — the case a
		// hard-coded name check would miss entirely.
		{"only the name sentence", "The assistant is called Jarvix.", biasBoth, true},
		{"only the vocabulary sentence", "Conversations may mention: Hyprland, kubectl.", biasBoth, true},
		// The user renamed the assistant. Nothing in this package knows the
		// old name, and nothing should.
		{"a renamed assistant", " The assistant is called Friday.",
			"The assistant is called Friday.", true},
		{"case and punctuation folded", "THE ASSISTANT IS CALLED JARVIX",
			"The assistant is called Jarvix.", true},
		{"trailing and leading space folded", "\n  the assistant is called jarvix  \n",
			"The assistant is called Jarvix.", true},

		// The pin the acceptance criteria name: a genuine utterance that
		// merely contains the name, and shares most of the prompt's words.
		{"a real question containing the name", "Jarvix, what is the assistant called?",
			"The assistant is called Jarvix.", false},
		{"a real question about the name", "What is the assistant called?",
			"The assistant is called Jarvix.", false},
		{"the prompt plus a real question", "The assistant is called Jarvix, what time is it?",
			"The assistant is called Jarvix.", false},
		{"a taught phrase spoken on purpose", "Restart Hyprland",
			biasBoth, false},
		{"just the name", "Jarvix", biasBoth, false},

		// Degenerate inputs. An empty transcript is the caller's no-speech
		// path and must keep its own reason; an unbiased engine has no echo
		// to detect.
		{"empty transcript", "", biasBoth, false},
		{"whitespace transcript", "   ", biasBoth, false},
		{"punctuation-only transcript", "...", biasBoth, false},
		{"no prompt configured", "The assistant is called Jarvix.", "", false},
		{"nothing on either side", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsPromptEcho(tc.transcript, tc.prompt); got != tc.want {
				t.Errorf("IsPromptEcho(%q, %q) = %v, want %v",
					tc.transcript, tc.prompt, got, tc.want)
			}
		})
	}
}

func TestNormaliseForEcho(t *testing.T) {
	tests := []struct{ in, want string }{
		{" The assistant is called Jarvix. ", "the assistant is called jarvix"},
		{"Conversations may mention: a, b, c.", "conversations may mention a b c"},
		// Punctuation inside a word does not split it, so a possessive or a
		// hyphenated term compares as the one word it is.
		{"Jarvix's hard-to-hear", "jarvixs hardtohear"},
		{"\tmixed\n\nwhitespace  ", "mixed whitespace"},
		{"!!!", ""},
	}
	for _, tc := range tests {
		if got := normaliseForEcho(tc.in); got != tc.want {
			t.Errorf("normaliseForEcho(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
