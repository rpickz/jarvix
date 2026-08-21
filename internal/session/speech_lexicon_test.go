package session

import (
	"strings"
	"testing"
)

// TestLexiconRules pins the matching rules: shipped defaults apply, matching
// ignores case, and a term never corrupts a longer word that contains it.
func TestLexiconRules(t *testing.T) {
	lex := newSpeechLexicon(nil)
	cases := map[string]struct{ in, want string }{
		// The reported mispronunciation: "Golang" with the vowel of posh.
		"shipped default":        {"Golang is fast", "go lang is fast"},
		"case insensitive":       {"GOLANG and golang", "go lang and go lang"},
		"mid sentence":           {"deploy to Kubernetes today", "deploy to koo ber net eez today"},
		"longer word is safe":    {"I finished the sudoku", "I finished the sudoku"},
		"longer word suffix":     {"a Waylander", "a Waylander"},
		"hyphen is a boundary":   {"nginx-proxy", "engine ex-proxy"},
		"underscore is not":      {"sudo_helper", "sudo_helper"},
		"multiple terms":         {"nginx on PostgreSQL", "engine ex on post gres queue ell"},
		"untouched prose":        {"the disk is full", "the disk is full"},
		"punctuation boundaries": {"(Hyprland).", "(hyper land)."},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := lex.apply(c.in); got != c.want {
				t.Errorf("apply(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// A user entry adds a word, and overrides a shipped default for the same term
// whatever case it is written in.
func TestLexiconUserEntries(t *testing.T) {
	lex := newSpeechLexicon(map[string]string{
		"GoLang":     "gee oh lang", // overrides the default, different casing
		"Excalidraw": "ex calli draw",
	})
	if got := lex.apply("Golang and Excalidraw"); got != "gee oh lang and ex calli draw" {
		t.Errorf("user entries not applied: %q", got)
	}
	if got := lex.apply("Kubernetes"); got != "koo ber net eez" {
		t.Errorf("user entries dropped the shipped defaults: %q", got)
	}
}

// A term is matched by the longest entry that fits, so an entry can never be
// shadowed by a shorter one that starts the same way.
func TestLexiconPrefersTheLongerTerm(t *testing.T) {
	lex := newSpeechLexicon(map[string]string{
		"post":       "posted",
		"postgresql": "the database",
	})
	if got := lex.apply("PostgreSQL"); got != "the database" {
		t.Errorf("shorter term shadowed the longer one: %q", got)
	}
}

// Whatever is written in config.toml, markdown glyphs never reach the engine:
// a spoken form is sanitised when the lexicon is compiled.
func TestLexiconStripsMarkdownFromSpokenForms(t *testing.T) {
	lex := newSpeechLexicon(map[string]string{"jarvix": "*jar* `vix`"})
	got := lex.apply("jarvix")
	if strings.ContainsAny(got, "`*") {
		t.Errorf("markdown reached the spoken form: %q", got)
	}
}

// A pathological entry must not take the daemon down: an empty term would
// match at every position, so it is ignored rather than compiled.
func TestLexiconIgnoresEmptyTerms(t *testing.T) {
	lex := newSpeechLexicon(map[string]string{"   ": "boom", "ok": "okay"})
	if got := lex.apply("ok then"); got != "okay then" {
		t.Errorf("apply = %q", got)
	}
}

// The engine speaks with the configured lexicon, and a reconfigure swaps it
// without a restart — the settings-surface contract (ADR 0015).
func TestEngineSpokenFormUsesConfiguredLexicon(t *testing.T) {
	e := &Engine{speech: newSpeechNormalizer(map[string]string{"jarvix": "jarviks"})}
	// The trailing full stop is markdownProse's: a line without terminal
	// punctuation gets one so the engine pauses between items.
	if got := e.spokenForm("jarvix says 9.2 million"); got != "jarviks says nine point two million." {
		t.Errorf("spokenForm = %q", got)
	}

	// An engine that was never given a normalizer still speaks, with the
	// shipped defaults.
	var bare Engine
	if got := bare.spokenForm("Golang"); got != "go lang." {
		t.Errorf("bare engine spokenForm = %q", got)
	}
}
