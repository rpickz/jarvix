package session

import (
	"regexp"
	"strings"
)

// Markdown that would be read aloud literally by TTS. The overlay still shows
// the original formatted text; only the spoken form is normalized.
var (
	reCodeFence  = regexp.MustCompile("(?s)```.*?```")
	reInlineCode = regexp.MustCompile("`([^`]*)`")
	reBold       = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	reItalic     = regexp.MustCompile(`\*([^*]+)\*|_([^_]+)_`)
	reHeading    = regexp.MustCompile(`(?m)^#{1,6}\s*`)
	reBullet     = regexp.MustCompile(`(?m)^\s*[-*+]\s+`)
	reNumbered   = regexp.MustCompile(`(?m)^\s*\d+\.\s+`)
	reLink       = regexp.MustCompile(`\[([^\]]+)\]\([^)]+\)`)
	reMultiSpace = regexp.MustCompile(`[ \t]+`)
	reBlankLines = regexp.MustCompile(`\n{2,}`)
)

// defaultSpeech normalizes with the shipped lexicon and no user entries —
// what speechText uses, and the fallback for any path without an engine to
// carry the configured one.
var defaultSpeech = newSpeechNormalizer(nil)

// speechText turns assistant text into the form Jarvix speaks: markdown
// stripped, mispronounced terms respelled, numbers expanded. The overlay
// still shows the original text; only the spoken form is normalized.
func speechText(s string) string { return defaultSpeech.text(s) }

// markdownProse strips assistant markdown down to plain prose. Spoken output
// must never contain backticks, asterisks, or bullet glyphs — engines read
// them literally. List items become sentences so the rhythm stays natural.
func markdownProse(s string) string {
	s = reCodeFence.ReplaceAllString(s, " ")
	s = reLink.ReplaceAllString(s, "$1")
	s = reInlineCode.ReplaceAllString(s, "$1")
	s = reBold.ReplaceAllString(s, "$1")
	s = reItalic.ReplaceAllStringFunc(s, func(m string) string {
		return strings.Trim(m, "*_")
	})
	s = reHeading.ReplaceAllString(s, "")
	// Turn list markers into sentence breaks so items are read as a list,
	// not run together, but without speaking the bullet character.
	s = reBullet.ReplaceAllString(s, "")
	s = reNumbered.ReplaceAllString(s, "")
	// The regexes above only strip paired/anchored markers; an unpaired
	// backtick or asterisk ("2 * 3", a stray ` in prose) would survive and be
	// read aloud literally. Found by FuzzSpeechText — strip the stragglers.
	s = stripSpeechMarkers(s)
	s = reMultiSpace.ReplaceAllString(s, " ")
	s = reBlankLines.ReplaceAllString(s, "\n")

	// Join list lines into sentences: a line that does not end with
	// terminal punctuation gets a period so TTS pauses between items.
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasSuffix(line, ".") && !strings.HasSuffix(line, "!") &&
			!strings.HasSuffix(line, "?") && !strings.HasSuffix(line, ":") {
			line += "."
		}
		lines[i] = line
	}
	return strings.TrimSpace(strings.Join(lines, " "))
}

// stripSpeechMarkers removes the markdown glyphs a TTS engine would read
// aloud. It is applied to lexicon replacements too, so a spoken form written
// into config.toml cannot reintroduce what the rest of this file removes.
func stripSpeechMarkers(s string) string {
	s = strings.ReplaceAll(s, "`", " ")
	return strings.ReplaceAll(s, "*", " ")
}
