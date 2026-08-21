package desktop

import (
	"math"
	"regexp"
	"strings"
)

// Secret redaction for captured context.
//
// The clipboard and the primary selection are the two places on a Linux
// desktop where a private key or an API token spends its life: copied out of
// a password manager, highlighted in a .env file, pasted between terminals.
// Jarvix gathers both automatically, which means the user is not there to
// decide — so the decision is made here, before anything leaves the machine.
//
// Two rules govern the design:
//
//   - Fail closed, and closed means the *whole* value. A key spread over
//     twenty lines cannot be partially redacted safely, and a heuristic that
//     tries to blank out just the token will one day blank out all but the
//     last eight characters of one. Text that looks like it contains a
//     secret is replaced entirely, and the marker tells the model something
//     was withheld — which is a better answer than a leak and an honest one.
//   - Heuristics, honestly labelled. This is pattern matching, not
//     classification: it catches the shapes credentials actually have
//     (vendor prefixes, PEM headers, labelled assignments, high-entropy
//     tokens) and it will miss an unlabelled password that looks like a word.
//     It is the last line of defence, not the only one — the first is that
//     the clipboard is off until the user turns it on.

// RedactedMarker replaces text that looks like it holds a secret. It is
// deliberately a sentence rather than a row of asterisks: the model reads it,
// and "not shared" is the fact it needs.
const RedactedMarker = "[looks like a secret — not shared]"

// Redact reports whether text looks like it holds a credential and, when it
// does, replaces it wholesale.
func Redact(text string) (string, bool) {
	if looksLikeSecret(text) {
		return RedactedMarker, true
	}
	return text, false
}

// secretPatterns are the shapes credentials announce themselves with. Vendor
// prefixes are exact because vendors publish them; the labelled-assignment
// rule is the one that catches a .env file, where the value is random but the
// key names it.
var secretPatterns = []*regexp.Regexp{
	// PEM private keys of every flavour (RSA, EC, OPENSSH, PGP, PKCS#8).
	regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY`),
	regexp.MustCompile(`-----BEGIN PGP PRIVATE KEY BLOCK`),
	// OpenAI and Anthropic.
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`),
	// GitHub: personal access, OAuth, user-to-server, server-to-server, refresh.
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{16,}`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`),
	// GitLab, Slack, Google, Hugging Face, Docker Hub.
	regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`),
	regexp.MustCompile(`\bhf_[A-Za-z0-9]{16,}`),
	regexp.MustCompile(`\bdckr_pat_[A-Za-z0-9_-]{16,}`),
	// AWS access key ids. The secret key beside them has no distinguishing
	// shape at all, which is exactly what the entropy rule below is for.
	regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`),
	// A labelled assignment: `api_key = "…"`, `PASSWORD: …`. The label does
	// the work the value's shape cannot — a good password looks like nothing
	// in particular. Twelve characters of value is the floor, so prose
	// ("token: expired") stays prose while a real credential does not.
	regexp.MustCompile(`(?i)\b(?:api[_-]?key|secret|access[_-]?token|auth[_-]?token|` +
		`bearer|token|password|passwd|passphrase)\b["']?\s*[:=]\s*["']?[^\s"']{12,}`),
}

// High-entropy token thresholds. A random credential and a long identifier
// are both "a run of letters and digits"; these are the three properties that
// separate them, and all three must hold.
const (
	// minTokenLen is the length below which a token is not worth suspecting.
	// Real credentials are longer; words and identifiers are usually shorter.
	minTokenLen = 32
	// minTokenEntropy is Shannon entropy per character. English prose sits
	// near 3.0–3.5; base64 of random bytes sits near 5.5.
	minTokenEntropy = 3.5
	// minTransitionRate is how often the character class (upper, lower,
	// digit, other) changes between neighbours. Random tokens switch
	// constantly; CamelCaseIdentifiers and /file/path/segments barely do.
	// This is what keeps a long Java class name out of the redactor.
	minTransitionRate = 0.35
)

// looksLikeSecret is the whole heuristic: named shapes first (cheap, exact),
// then the entropy scan for credentials that have no announced shape.
func looksLikeSecret(text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	for _, re := range secretPatterns {
		if re.MatchString(text) {
			return true
		}
	}
	return hasHighEntropyToken(text)
}

// tokenChar reports whether b can appear inside a credential token. The set
// is base64 plus base64url plus the separators keys are written with; every
// other byte splits one token from the next, so a URL, a sentence, or a
// dotted JWT is examined piece by piece.
func tokenChar(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '+', b == '/', b == '=', b == '_', b == '-':
		return true
	}
	return false
}

// hasHighEntropyToken looks for a run of token characters that behaves like
// random data rather than like language.
func hasHighEntropyToken(text string) bool {
	for start := 0; start < len(text); {
		if !tokenChar(text[start]) {
			start++
			continue
		}
		end := start
		for end < len(text) && tokenChar(text[end]) {
			end++
		}
		if tokenLooksRandom(text[start:end]) {
			return true
		}
		start = end
	}
	return false
}

// tokenLooksRandom applies the three thresholds, plus one exclusion: a token
// beginning with "/" is a filesystem path, and paths are long, mixed-case,
// and entirely unsecret.
func tokenLooksRandom(tok string) bool {
	if len(tok) < minTokenLen || strings.HasPrefix(tok, "/") {
		return false
	}
	var upper, lower, digit int
	for i := 0; i < len(tok); i++ {
		switch c := tok[i]; {
		case c >= 'A' && c <= 'Z':
			upper++
		case c >= 'a' && c <= 'z':
			lower++
		case c >= '0' && c <= '9':
			digit++
		}
	}
	// All three classes present: a credential mixes them, a hex digest or a
	// lowercase slug does not. (A git SHA is not a secret, and redacting one
	// would break "what does this commit do?".)
	if upper == 0 || lower == 0 || digit == 0 {
		return false
	}
	if transitionRate(tok) < minTransitionRate {
		return false
	}
	return shannon(tok) >= minTokenEntropy
}

// charClass buckets a byte for the transition count.
func charClass(b byte) int {
	switch {
	case b >= 'A' && b <= 'Z':
		return 1
	case b >= 'a' && b <= 'z':
		return 2
	case b >= '0' && b <= '9':
		return 3
	}
	return 4
}

// transitionRate is the fraction of neighbouring character pairs that change
// class — a cheap stand-in for "does this look written or generated?".
func transitionRate(tok string) float64 {
	if len(tok) < 2 {
		return 0
	}
	changes := 0
	for i := 1; i < len(tok); i++ {
		if charClass(tok[i]) != charClass(tok[i-1]) {
			changes++
		}
	}
	return float64(changes) / float64(len(tok)-1)
}

// shannon is the Shannon entropy of the byte distribution, in bits per
// character.
func shannon(s string) float64 {
	if s == "" {
		return 0
	}
	var counts [256]int
	for i := 0; i < len(s); i++ {
		counts[s[i]]++
	}
	n := float64(len(s))
	h := 0.0
	for _, c := range counts {
		if c == 0 {
			continue
		}
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h
}
