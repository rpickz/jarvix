package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests pin the keyed entry editor (issue #163) to the same contract
// the array editor already meets: golden files prove that everything outside
// the one table being written — comments, the section's own scalars, sibling
// tables, ordering, formatting — survives byte-for-byte, across insert, edit
// and delete of both families the Providers section administers.

// keyedGolden is one byte-preservation case: an input document, the edit, and
// the exact bytes it must produce.
type keyedGolden struct {
	name   string
	family string
	// target is the table to replace or delete ("" creates).
	target string
	// entry is the whole draft, `name` carrying the table key. Nil deletes.
	entry map[string]any
	order []string
}

// TestKeyedEntryGolden drives the editor over hand-written documents and
// compares byte-for-byte.
func TestKeyedEntryGolden(t *testing.T) {
	endpointOrder := []string{"name", "base_url", "api_key_env"}
	advisorOrder := []string{"name", "binary", "args", "timeout_sec", "description"}
	cases := []keyedGolden{
		// Creating: the new table joins its section — after the last existing
		// [ai.*] block — rather than landing at the end of the file behind
		// unrelated families.
		{"ai_insert", "ai", "", map[string]any{
			"name": "openai", "base_url": "https://api.openai.com/v1",
			"api_key_env": "OPENAI_API_KEY",
		}, endpointOrder},
		// Editing in place: the block keeps its position and the comment glued
		// to its header — which documents it, and belongs to the file, not to
		// the entry.
		{"ai_edit", "ai", "openai", map[string]any{
			"name": "openai", "base_url": "https://api.openai.com/v1/",
			"api_key_env": "OPENAI_KEY",
		}, endpointOrder},
		// Deleting: the block and the comment glued to its header go together,
		// and the separator collapses so the document never gains a double
		// blank line.
		{"ai_delete", "ai", "openai", nil, endpointOrder},
		// The advisor family, same three motions. A created advisor with no
		// args of its own is rendered without an `args` key at all, because
		// that absence is what earns the shipped preset's tier (ADR 0016).
		{"advisor_insert", "advisors", "", map[string]any{
			"name": "gemini", "binary": "/usr/bin/gemini", "timeout_sec": int64(90),
		}, advisorOrder},
		{"advisor_edit", "advisors", "claude", map[string]any{
			"name": "claude", "binary": "/usr/bin/claude",
			"args": []string{"-p", "--model", "opus"}, "timeout_sec": int64(240),
			"description": "deep review",
		}, advisorOrder},
		{"advisor_delete", "advisors", "codex", nil, advisorOrder},
	}
	reserved := ReservedAIKeys()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := readGolden(t, tc.name+".input.toml")
			golden := readGolden(t, tc.name+".golden.toml")
			var got []byte
			var err error
			if tc.entry == nil {
				got, err = DeleteKeyedEntryTOML(input, tc.family, tc.target, reserved)
			} else {
				got, err = UpsertKeyedEntryTOML(input, tc.family, tc.target, tc.entry, tc.order, reserved)
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(golden) {
				t.Errorf("rewrite mismatch\n--- got ---\n%s\n--- want ---\n%s", got, golden)
			}
		})
	}
}

// readGolden loads one fixture from the shared entry testdata directory.
func readGolden(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "entry", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestKeyedEntryRenameRewritesTheHeader: for a keyed family the name IS the
// table key, so a rename is a header rewrite — and the old table must not
// survive alongside the new one.
func TestKeyedEntryRenameRewritesTheHeader(t *testing.T) {
	input := readGolden(t, "ai_edit.input.toml")
	out, err := UpsertKeyedEntryTOML(input, "ai", "openai", map[string]any{
		"name": "work", "base_url": "https://api.openai.com/v1",
	}, []string{"name", "base_url"}, ReservedAIKeys())
	if err != nil {
		t.Fatal(err)
	}
	names, err := KeyedEntryNames(out, "ai", ReservedAIKeys())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "ollama,work" {
		t.Errorf("names after rename = %v, want the renamed table and its sibling", names)
	}
	if strings.Contains(string(out), "[ai.openai]") {
		t.Errorf("the old table survived the rename:\n%s", out)
	}
}

// TestKeyedEntryAddressingIsExact: a keyed entry IS its table key, and TOML
// keys are case-sensitive — so addressing must be too. Matching "OpenAI" to
// "openai" would edit a different endpoint than the one ai.provider resolves,
// which is the one mistake a byte-preserving editor must not make.
func TestKeyedEntryAddressingIsExact(t *testing.T) {
	input := readGolden(t, "ai_edit.input.toml")
	if _, err := UpsertKeyedEntryTOML(input, "ai", "OpenAI", map[string]any{
		"name": "OpenAI", "base_url": "https://example.test/v1",
	}, []string{"name", "base_url"}, ReservedAIKeys()); err == nil {
		t.Error("a differently-cased name matched an existing table; it must not")
	}
	if _, err := DeleteKeyedEntryTOML(input, "ai", "OLLAMA", ReservedAIKeys()); err == nil {
		t.Error("a differently-cased name matched an existing table on delete")
	}
}

// TestKeyedEntryNamesSkipTheSectionsOwnScalars: [ai] holds the section's
// settings beside its endpoint tables, and only the tables are entries.
func TestKeyedEntryNamesSkipTheSectionsOwnScalars(t *testing.T) {
	names, err := KeyedEntryNames(readGolden(t, "ai_edit.input.toml"), "ai", ReservedAIKeys())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "ollama,openai" {
		t.Errorf("endpoint names = %v, want only the two tables", names)
	}
}

// TestKeyedEntryCreateWithNoSectionAppends: a family with no tables yet has
// nowhere to join, so the block lands at the end — still parsing, still
// leaving every existing byte alone.
func TestKeyedEntryCreateWithNoSectionAppends(t *testing.T) {
	doc := []byte("[ai]\nprovider = \"ollama\"\nmodel = \"llama3.2:3b\"\n")
	out, err := UpsertKeyedEntryTOML(doc, "advisors", "", map[string]any{
		"name": "claude", "binary": "/usr/bin/claude",
	}, []string{"name", "binary"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "[ai]\nprovider = \"ollama\"\nmodel = \"llama3.2:3b\"\n\n" +
		"[advisors.claude]\nbinary = \"/usr/bin/claude\"\n"
	if string(out) != want {
		t.Errorf("append mismatch\n--- got ---\n%s\n--- want ---\n%s", out, want)
	}
}

// TestKeyedEntryNameIsNeverStoredAsAKey: the draft carries `name` so one form
// shape serves both document shapes, but for a keyed family that key is the
// header. A stored copy could disagree with the header it sits under, and the
// loader would believe the header.
func TestKeyedEntryNameIsNeverStoredAsAKey(t *testing.T) {
	out, err := UpsertKeyedEntryTOML(nil, "advisors", "", map[string]any{
		"name": "claude", "binary": "/usr/bin/claude",
	}, []string{"name", "binary"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "name = ") {
		t.Errorf("the table stored its own key as a field:\n%s", out)
	}
	entry, ok, err := KeyedEntryValue(out, "advisors", "claude", nil)
	if err != nil || !ok {
		t.Fatalf("reading the written table back: ok=%v err=%v", ok, err)
	}
	if _, stored := entry["name"]; stored {
		t.Errorf("entry = %v, want no stored name", entry)
	}
}

// TestKeyedEntryRefusesAnUnusableName: a name the loader's own map lookup
// could not address is refused at render time rather than written as a quoted
// header nobody can select.
func TestKeyedEntryRefusesAnUnusableName(t *testing.T) {
	for _, name := range []string{"", "two words", "dots.inside", `quo"te`} {
		_, err := UpsertKeyedEntryTOML(nil, "advisors", "", map[string]any{
			"name": name, "binary": "/usr/bin/x",
		}, []string{"name", "binary"}, nil)
		if err == nil {
			t.Errorf("name %q was accepted; it cannot be a table key", name)
		}
	}
}

// TestKeyedEntryRefusesAnUnparsableDocument: the editor never guesses at a
// broken file — the same refusal the array editor gives.
func TestKeyedEntryRefusesAnUnparsableDocument(t *testing.T) {
	if _, err := KeyedEntryNames([]byte("[ai\nprovider ="), "ai", nil); err == nil {
		t.Error("a broken document was read as configuration")
	}
}

// TestTableHeaderPathAcceptsWhatTOMLAccepts: a hand-formatted header addresses
// the same table the loader decoded, or the editor would treat an existing
// endpoint as missing and write a duplicate the parser then rejects.
func TestTableHeaderPathAcceptsWhatTOMLAccepts(t *testing.T) {
	cases := map[string][]string{
		`[ai.openai]`:            {"ai", "openai"},
		`  [ai . openai ]  `:     {"ai", "openai"},
		`[ai."openai"] # a note`: {"ai", "openai"},
		`[advisors.claude]`:      {"advisors", "claude"},
	}
	for line, want := range cases {
		got, array, ok := tableHeaderPath(line)
		if !ok || array || strings.Join(got, ".") != strings.Join(want, ".") {
			t.Errorf("tableHeaderPath(%q) = %v, array=%v, ok=%v; want %v", line, got, array, ok, want)
		}
	}
	// An array header is recognised too — not as an entry, but as the boundary
	// that ends one. Without this a created endpoint would land after whatever
	// [[routines]] followed the section it belongs to.
	if got, array, ok := tableHeaderPath(`[[routines]]`); !ok || !array ||
		strings.Join(got, ".") != "routines" {
		t.Errorf("tableHeaderPath([[routines]]) = %v, array=%v, ok=%v", got, array, ok)
	}
	for _, line := range []string{`base_url = "x"`, ``, `[unclosed`} {
		if _, _, ok := tableHeaderPath(line); ok {
			t.Errorf("tableHeaderPath(%q) matched; it is not a table header", line)
		}
	}
}

// TestEndpointValidationNamesTheFieldThatIsWrong: the rules the Providers form
// pins to inputs. Each message carries the `ai.<name>.<key>` label the daemon
// strips to find the field, and none of them can quote a credential — there is
// no code path here that reads one.
func TestEndpointValidationNamesTheFieldThatIsWrong(t *testing.T) {
	cases := []struct {
		name string
		ep   Endpoint
		want string
	}{
		{"nourl", Endpoint{}, "ai.nourl.base_url is empty"},
		{"noscheme", Endpoint{BaseURL: "api.openai.com/v1"}, "ai.noscheme.base_url must start with http"},
		{"nohost", Endpoint{BaseURL: "http:///v1"}, "ai.nohost.base_url has no host"},
		{"badenv", Endpoint{BaseURL: "https://x.test/v1", APIKeyEnv: "my key"},
			"ai.badenv.api_key_env"},
	}
	for _, tc := range cases {
		cfg := Default()
		cfg.AI.Endpoints = map[string]Endpoint{tc.name: tc.ep, "ollama": {BaseURL: "http://127.0.0.1:11434/v1"}}
		cfg.AI.Provider = "ollama"
		problems := cfg.validateEndpoints()
		if !containsSubstring(problems, tc.want) {
			t.Errorf("%s problems = %v, want one containing %q", tc.name, problems, tc.want)
		}
	}
}

// TestEndpointNameCannotShadowTheSectionsOwnSettings: [ai.model] would be read
// as the model setting, not as an endpoint, so the name is refused with that
// reason rather than silently producing an endpoint nothing can select.
func TestEndpointNameCannotShadowTheSectionsOwnSettings(t *testing.T) {
	cfg := Default()
	cfg.AI.Endpoints = map[string]Endpoint{"model": {BaseURL: "https://x.test/v1"}}
	cfg.AI.Provider = "model"
	if !containsSubstring(cfg.validateEndpoints(), `endpoint name "model" is one of the [ai] section's own settings`) {
		t.Errorf("problems = %v, want the reserved-name refusal", cfg.validateEndpoints())
	}
}

// TestShippedEndpointsValidate: the presets Jarvix ships must pass the rules
// it enforces, or a fresh install would refuse to boot.
func TestShippedEndpointsValidate(t *testing.T) {
	if problems := Default().validateEndpoints(); len(problems) != 0 {
		t.Errorf("the shipped endpoints do not validate: %v", problems)
	}
}

// containsSubstring reports whether any message contains want.
func containsSubstring(messages []string, want string) bool {
	for _, m := range messages {
		if strings.Contains(m, want) {
			return true
		}
	}
	return false
}
