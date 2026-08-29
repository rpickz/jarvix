package config

import (
	"fmt"
	"net/url"
	"strings"
)

// This file owns the `[ai.<name>]` tables: one OpenAI-compatible endpoint per
// table, keyed by the name `ai.provider` selects it with.
//
// It exists because #163 gave the window a form for them, and a form needs the
// loader to have an opinion: until now a mistyped base URL was discovered
// mid-conversation, as a stream that would not start. The rules below are the
// ones a user can act on — a name the loader can address, a URL an HTTP client
// can dial, an environment variable a shell can export — and every message
// names the table and the key so the form can pin it to the field that is
// wrong. None of them ever quotes a credential.

// ReservedAIKeys names the scalar keys that share the [ai] table with the
// endpoint sub-tables. They are the section's own settings, never endpoints,
// and both the loader (parse) and the window's entry editor read this one set
// so the two can never disagree about what `[ai.model]` would mean.
// "tiers" is here for a different reason than the rest: it is not a scalar
// but the [ai.tiers] sub-table family (issue #159, tiers.go), which the loader
// reads with its own decoder. It has to be reserved all the same, and for the
// same purpose — an endpoint may not be called "tiers", because then
// [ai.tiers.instant] would be ambiguous between an endpoint's key and a tier.
func ReservedAIKeys() map[string]bool {
	return map[string]bool{
		"provider": true, "model": true, "system_prompt": true,
		"max_tokens": true, "temperature": true, tiersTableKey: true,
	}
}

// EndpointNames lists the configured endpoint names, sorted — the order the
// provider picker, the doctor, and every message uses.
func (c Config) EndpointNames() []string { return c.endpointNames() }

// validateEndpoints reports endpoint configuration a user must fix. Messages
// are labelled `ai.<name>.<key>` so a whole-document validation can be keyed
// back to the form field that owns it, matching the shape validateAdvisors
// already uses for `advisors.<name>.<key>`.
//
// The api_key value is never read here, let alone quoted: whether a key is
// present is a question for the surface that asks, and a validation problem is
// a string that travels to logs, events, and screens.
func (c Config) validateEndpoints() []string {
	reserved := ReservedAIKeys()
	var problems []string
	for _, name := range c.endpointNames() {
		ep := c.AI.Endpoints[name]
		if reserved[name] {
			problems = append(problems, fmt.Sprintf(
				"endpoint name %q is one of the [ai] section's own settings; "+
					"choose another name for the [ai.%s] table", name, name))
			continue
		}
		if !plainTableKey(name) {
			problems = append(problems, fmt.Sprintf(
				"endpoint name %q is invalid; use letters, digits, dashes or underscores "+
					"(the table is [ai.<name>], e.g. [ai.openai])", name))
		}
		problems = append(problems, endpointURLProblems(name, ep.BaseURL)...)
		if ep.APIKeyEnv != "" && !plainEnvName(ep.APIKeyEnv) {
			problems = append(problems, fmt.Sprintf(
				"ai.%s.api_key_env %q is not an environment variable name; "+
					"use letters, digits and underscores, e.g. OPENAI_API_KEY", name, ep.APIKeyEnv))
		}
	}
	return problems
}

// endpointURLProblems checks one base URL: present, absolute, and dialable
// over HTTP. A relative or scheme-less URL is the mistake this catches — it
// parses fine and then fails as a connection refused nobody can explain.
func endpointURLProblems(name, base string) []string {
	trimmed := strings.TrimSpace(base)
	if trimmed == "" {
		return []string{fmt.Sprintf(
			"ai.%s.base_url is empty; set the endpoint's API root, "+
				"e.g. base_url = \"https://api.openai.com/v1\"", name)}
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return []string{fmt.Sprintf(
			"ai.%s.base_url is not a URL; set the endpoint's API root, "+
				"e.g. base_url = \"https://api.openai.com/v1\"", name)}
	}
	switch {
	case parsed.Scheme != "http" && parsed.Scheme != "https":
		return []string{fmt.Sprintf(
			"ai.%s.base_url must start with http:// or https://; got %q", name, trimmed)}
	case parsed.Host == "":
		return []string{fmt.Sprintf(
			"ai.%s.base_url has no host; set the endpoint's API root, "+
				"e.g. base_url = \"http://127.0.0.1:11434/v1\"", name)}
	}
	return nil
}

// plainEnvName reports whether s can be exported by a shell as-is. The check
// is deliberately narrow: an api_key_env that a shell cannot set is a setting
// that can never resolve, and saying so at save time beats an endpoint that
// silently authenticates with nothing.
func plainEnvName(s string) bool {
	for i, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return s != ""
}
