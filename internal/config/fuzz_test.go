package config

import (
	"testing"
)

// FuzzConfigParse throws arbitrary TOML at the config parser, including the
// loose [ai.<name>] endpoint tables that bypass typed struct decoding.
// Invariants: no panic, and any accepted document yields a config that can be
// validated and redacted without blowing up.
func FuzzConfigParse(f *testing.F) {
	f.Add(`
[activation]
mode = "push_to_talk"
ptt_chord = ["leftmeta", "leftalt", "v"]

[ai]
provider = "ollama"
model = "llama3.2:3b"
max_tokens = 512
temperature = 0.7
`)
	f.Add(`
[ai]
provider = "corp"

[ai.corp]
base_url = "https://llm.corp.internal/v1"
api_key_env = "CORP_KEY"
`)
	f.Add(`
[ai.openai]
api_key = "sk-inline-key"
`)
	f.Add(`[ai]` + "\n" + `provider = 42`)      // wrong type
	f.Add(`[ai.weird]` + "\n" + `base_url = 1`) // wrong type in a loose table
	f.Add(`[[ai]]`)                             // array where a table belongs
	f.Add(`ai = "not a table"`)
	f.Add(`[tools]` + "\n" + `shell = true`)
	f.Add(`
[advisors.claude]
binary = "/usr/bin/claude"

[advisors.house]
args = ["--ask", "{question}"]
timeout_sec = 30
`)
	f.Add(`[advisors]` + "\n" + `claude = "not a table"`)
	f.Add("\x00\xff not toml at all")
	f.Fuzz(func(t *testing.T, doc string) {
		cfg, err := parse([]byte(doc), Default())
		if err != nil {
			return // rejected input is fine; panicking is not
		}
		// Accepted documents must produce a usable config.
		if cfg.AI.Endpoints == nil {
			t.Fatal("accepted config lost its endpoint map")
		}
		_ = cfg.Validate() // must not panic, valid or not
		red := cfg.Redact()
		for name, ep := range red.AI.Endpoints {
			if ep.APIKey != "" && ep.APIKey != "[redacted]" {
				t.Fatalf("endpoint %q leaked an inline api key: %q", name, ep.APIKey)
			}
		}
	})
}
