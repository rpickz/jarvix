package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/ai"
)

// The self-configuration tools (issue #105, ADR 0036) over a scripted admin:
// the exclusion wall, the verbatim confirmation cards, the validation-retry
// shape, the fingerprint-conflict handling, and the read-before-edit rule —
// each with its mutation check: every refused or surfaced path must leave the
// admin's write counters untouched.

// upsertCall records one write the fake admin received.
type upsertCall struct {
	family, name, fingerprint string
	entry                     map[string]any
}

// fakeConfigAdmin is a deterministic ConfigAdmin: entries in a map, scripted
// errors consumed in order, every write recorded.
type fakeConfigAdmin struct {
	entries     map[string]map[string]any // family+"\x00"+name → entry
	fingerprint string

	upserts    []upsertCall
	deletes    []upsertCall
	upsertErrs []error // consumed per UpsertEntry call; nil → success
	deleteErrs []error

	settings      []ConfigSettingView
	settingWrites []upsertCall // key in name, value in entry["value"]
	settingErr    error
	receipt       ConfigWriteReceipt
	settingRes    ConfigSettingReceipt
	// path is the file the fake claims to write. Empty in most tests: the
	// account snapshots it before every write, and a path nothing exists at
	// records the honest "there was no file", which is all these tests need.
	path string
}

// Path implements ConfigAdmin.
func (f *fakeConfigAdmin) Path() string { return f.path }

func newFakeConfigAdmin() *fakeConfigAdmin {
	return &fakeConfigAdmin{
		entries:     map[string]map[string]any{},
		fingerprint: "fp1",
		receipt:     ConfigWriteReceipt{Applied: true},
		settingRes:  ConfigSettingReceipt{Applied: true},
	}
}

func (f *fakeConfigAdmin) put(family, name string, entry map[string]any) {
	f.entries[family+"\x00"+strings.ToLower(name)] = entry
}

func (f *fakeConfigAdmin) ListEntries(family string) ([]ConfigEntrySummary, error) {
	var out []ConfigEntrySummary
	for key := range f.entries {
		if strings.HasPrefix(key, family+"\x00") {
			out = append(out, ConfigEntrySummary{Name: strings.SplitN(key, "\x00", 2)[1]})
		}
	}
	return out, nil
}

func (f *fakeConfigAdmin) GetEntry(family, name string) (ConfigEntry, error) {
	entry, ok := f.entries[family+"\x00"+strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return ConfigEntry{}, &ConfigAdminError{NotFound: true, Message: "no entry named " + name}
	}
	return ConfigEntry{Family: family, Name: name, Entry: entry, Fingerprint: f.fingerprint}, nil
}

func (f *fakeConfigAdmin) UpsertEntry(family, name string, entry map[string]any, fingerprint string) (ConfigWriteReceipt, error) {
	f.upserts = append(f.upserts, upsertCall{family: family, name: name, fingerprint: fingerprint, entry: entry})
	if len(f.upsertErrs) > 0 {
		err := f.upsertErrs[0]
		f.upsertErrs = f.upsertErrs[1:]
		if err != nil {
			return ConfigWriteReceipt{}, err
		}
	}
	written, _ := entry["name"].(string)
	if written == "" {
		written = name
	}
	f.put(family, written, entry)
	return f.receipt, nil
}

func (f *fakeConfigAdmin) DeleteEntry(family, name, fingerprint string) (ConfigWriteReceipt, error) {
	f.deletes = append(f.deletes, upsertCall{family: family, name: name, fingerprint: fingerprint})
	if len(f.deleteErrs) > 0 {
		err := f.deleteErrs[0]
		f.deleteErrs = f.deleteErrs[1:]
		if err != nil {
			return ConfigWriteReceipt{}, err
		}
	}
	delete(f.entries, family+"\x00"+strings.ToLower(name))
	return f.receipt, nil
}

func (f *fakeConfigAdmin) Settings() []ConfigSettingView { return f.settings }

func (f *fakeConfigAdmin) WriteSetting(key string, value any) (ConfigSettingReceipt, error) {
	f.settingWrites = append(f.settingWrites, upsertCall{name: key, entry: map[string]any{"value": value}})
	if f.settingErr != nil {
		return ConfigSettingReceipt{}, f.settingErr
	}
	return f.settingRes, nil
}

func (f *fakeConfigAdmin) ExcludedSetting(key string) (string, bool) {
	// The fake mirrors the real wall's shape for the prefixes the tests use.
	for _, prefix := range []string{"ai.", "tools.policy", "advisors", "intents.custom"} {
		if strings.HasPrefix(key, prefix) {
			return "the assistant may not change " + prefix + " configuration", true
		}
	}
	return "", false
}

// toolByName finds one of the family's tools.
func toolByName(t *testing.T, c *ConfigTools, name string) Tool {
	t.Helper()
	for _, tool := range c.Tools() {
		if tool.Name() == name {
			return tool
		}
	}
	t.Fatalf("no tool named %q", name)
	return nil
}

// gatedRegistry builds a registry holding the family under a real policy.
func gatedRegistry(t *testing.T, c *ConfigTools, cfg PolicyConfig) *Registry {
	t.Helper()
	r := NewRegistry(nil)
	for _, tool := range c.Tools() {
		r.Register(tool)
	}
	p, err := NewPolicy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.SetPolicy(p)
	return r
}

// ------------------------------------------------------- the exclusion wall

// TestExclusionWallRefusesForbiddenFamiliesUnderGlobalAllow is the test the
// design requirement names: a policy of `default = "allow"` plus a direct
// attempt at an excluded family or key still refuses, before the gate, with
// a spoken-ready reason.
func TestExclusionWallRefusesForbiddenFamiliesUnderGlobalAllow(t *testing.T) {
	c := NewConfigTools(ConfigToolsOptions{Admin: newFakeConfigAdmin()})
	r := gatedRegistry(t, c, PolicyConfig{Default: PolicyAllow})
	cases := []struct {
		tool, args, want string
	}{
		{ConfigWriteEntryToolName, `{"family":"advisors","entry":{"name":"x"}}`, "off limits"},
		{ConfigWriteEntryToolName, `{"family":"intents.custom","entry":{"name":"x"}}`, "off limits"},
		{ConfigDeleteEntryToolName, `{"family":"tools.policy","name":"x"}`, "off limits"},
		{ConfigWriteSettingToolName, `{"key":"ai.model","value":"other"}`, "may not change"},
		{ConfigWriteSettingToolName, `{"key":"tools.policy.default","value":"allow"}`, "may not change"},
		{ConfigWriteSettingToolName, `{"key":"advisors.claude.command","value":["x"]}`, "may not change"},
	}
	for _, tc := range cases {
		v := r.Check(ai.ToolCall{Name: tc.tool, Arguments: tc.args})
		if v.Decision != PolicyDeny {
			t.Errorf("%s %s: decision = %q, want deny", tc.tool, tc.args, v.Decision)
		}
		if !strings.Contains(v.Rule, tc.want) {
			t.Errorf("%s %s: rule %q does not carry the spoken-ready reason", tc.tool, tc.args, v.Rule)
		}
	}
}

// TestExclusionWallHoldsWithoutAPolicy: the wall must not depend on a gate
// being installed — "no policy" is a construction-time test convenience, and
// even there the excluded space stays shut.
func TestExclusionWallHoldsWithoutAPolicy(t *testing.T) {
	c := NewConfigTools(ConfigToolsOptions{Admin: newFakeConfigAdmin()})
	r := NewRegistry(nil)
	for _, tool := range c.Tools() {
		r.Register(tool)
	}
	v := r.Check(ai.ToolCall{Name: ConfigWriteEntryToolName,
		Arguments: `{"family":"advisors","entry":{"name":"x"}}`})
	if v.Decision != PolicyDeny {
		t.Errorf("decision without a policy = %q, want deny", v.Decision)
	}
}

// TestExclusionWallHoldsAtExecuteToo: defence in depth — even a call that
// somehow reaches Execute writes nothing and reports the refusal.
func TestExclusionWallHoldsAtExecuteToo(t *testing.T) {
	admin := newFakeConfigAdmin()
	c := NewConfigTools(ConfigToolsOptions{Admin: admin})
	write := toolByName(t, c, ConfigWriteEntryToolName)
	result, err := write.Execute(context.Background(),
		[]byte(`{"family":"advisors","entry":{"name":"x"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "Refused") {
		t.Errorf("result %q does not refuse", result)
	}
	set := toolByName(t, c, ConfigWriteSettingToolName)
	result, err = set.Execute(context.Background(), []byte(`{"key":"ai.model","value":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "Refused") {
		t.Errorf("result %q does not refuse", result)
	}
	if len(admin.upserts) != 0 || len(admin.settingWrites) != 0 {
		t.Errorf("the excluded calls reached the admin: %d upserts, %d setting writes",
			len(admin.upserts), len(admin.settingWrites))
	}
}

// ---------------------------------------------- dangerous-setting escalation

// TestDangerousSettingEscalatesEvenUnderGlobalAllow: the always-confirm floor
// for registry-flagged dangerous keys is per-call escalation, so it holds
// against a global allow AND an explicit allow on the tool.
func TestDangerousSettingEscalatesEvenUnderGlobalAllow(t *testing.T) {
	admin := newFakeConfigAdmin()
	admin.settings = []ConfigSettingView{
		{Key: "tools.typing.enable", Label: "Assistant may type on your keyboard", Type: "bool", Dangerous: true},
		{Key: "tts.kokoro.speed", Label: "Kokoro speed", Type: "float"},
	}
	c := NewConfigTools(ConfigToolsOptions{Admin: admin})
	for _, cfg := range []PolicyConfig{
		{Default: PolicyAllow},
		{Tools: map[string]PolicyDecision{ConfigWriteSettingToolName: PolicyAllow}},
	} {
		r := gatedRegistry(t, c, cfg)
		v := r.Check(ai.ToolCall{Name: ConfigWriteSettingToolName,
			Arguments: `{"key":"tools.typing.enable","value":true}`})
		if v.Decision != PolicyAsk {
			t.Errorf("dangerous key under %+v: decision = %q, want ask", cfg, v.Decision)
		}
		if !strings.Contains(v.Command, "tools.typing.enable") || !strings.Contains(v.Command, "true") {
			t.Errorf("card %q does not carry the exact key and value", v.Command)
		}
		// The benign key honours the allow the user configured.
		v = r.Check(ai.ToolCall{Name: ConfigWriteSettingToolName,
			Arguments: `{"key":"tts.kokoro.speed","value":1.3}`})
		if v.Decision != PolicyAllow {
			t.Errorf("benign key under %+v: decision = %q, want allow", cfg, v.Decision)
		}
	}
}

// TestUnknownSettingEscalates: a key the registry does not know cannot be
// classified, and the safe failure mode is the question.
func TestUnknownSettingEscalates(t *testing.T) {
	c := NewConfigTools(ConfigToolsOptions{Admin: newFakeConfigAdmin()})
	r := gatedRegistry(t, c, PolicyConfig{Default: PolicyAllow})
	v := r.Check(ai.ToolCall{Name: ConfigWriteSettingToolName,
		Arguments: `{"key":"no.such.key","value":1}`})
	if v.Decision != PolicyAsk {
		t.Errorf("unknown key: decision = %q, want ask", v.Decision)
	}
}

// --------------------------------------------------- the confirmation cards

// TestWriteEntryCardShowsCommandBearingFieldsVerbatim pins the card for each
// family: name, phrases, schedule, and every command-bearing field, verbatim
// — the shell.run discipline at authoring time.
func TestWriteEntryCardShowsCommandBearingFieldsVerbatim(t *testing.T) {
	c := NewConfigTools(ConfigToolsOptions{Admin: newFakeConfigAdmin()})
	write := toolByName(t, c, ConfigWriteEntryToolName).(Confirmable)

	command, summary, ok := write.Confirmation([]byte(`{"family":"scripts","entry":{
		"name":"deploy","phrases":["ship it"],"path":"/home/u/bin/deploy.sh","schedule":"every day 08:30"}}`))
	if !ok {
		t.Fatal("no confirmation for a script draft")
	}
	for _, want := range []string{
		"create script \"deploy\"",
		"phrases: \"ship it\"",
		"schedule: every day 08:30",
		"runs file (verbatim): /home/u/bin/deploy.sh",
	} {
		if !strings.Contains(command, want) {
			t.Errorf("script card %q missing %q", command, want)
		}
	}
	if !strings.Contains(summary, "/home/u/bin/deploy.sh") || !strings.Contains(summary, "Should I go ahead?") {
		t.Errorf("script summary %q does not speak the path", summary)
	}

	command, _, ok = write.Confirmation([]byte(`{"family":"knowledge.feeds","entry":{
		"name":"amd","command":["curl","-s","https://x"],"interval_sec":300}}`))
	if !ok {
		t.Fatal("no confirmation for a feed draft")
	}
	// Element-by-element quoting: joining would hide argv boundaries.
	if !strings.Contains(command, `runs command (verbatim): "curl" "-s" "https://x"`) {
		t.Errorf("feed card %q does not show the argv verbatim", command)
	}

	command, _, ok = write.Confirmation([]byte(`{"family":"routines","name":"morning setup","entry":{
		"name":"morning setup","phrases":["morning setup"],"schedule":"weekdays 07:30",
		"steps":[{"app":"firefox","workspace":2},{"app":"alacritty"}]}}`))
	if !ok {
		t.Fatal("no confirmation for a routine draft")
	}
	for _, want := range []string{
		"edit routine \"morning setup\"",
		`step 1 (verbatim): launch "firefox"`,
		`step 2 (verbatim): launch "alacritty"`,
		"schedule: weekdays 07:30",
	} {
		if !strings.Contains(command, want) {
			t.Errorf("routine card %q missing %q", command, want)
		}
	}
}

// TestDeleteEntryCardNamesTheEntryFromDisk: the delete question is resolved
// from the file, not the model's words, and carries what will stop running.
func TestDeleteEntryCardNamesTheEntryFromDisk(t *testing.T) {
	admin := newFakeConfigAdmin()
	admin.put("scripts", "deploy", map[string]any{
		"name": "deploy", "phrases": []any{"ship it"}, "path": "/home/u/bin/deploy.sh"})
	c := NewConfigTools(ConfigToolsOptions{Admin: admin})
	del := toolByName(t, c, ConfigDeleteEntryToolName).(Confirmable)
	command, summary, ok := del.Confirmation([]byte(`{"family":"scripts","name":"deploy"}`))
	if !ok {
		t.Fatal("no confirmation for an existing entry")
	}
	if !strings.Contains(command, "delete script \"deploy\"") ||
		!strings.Contains(command, "runs file (verbatim): /home/u/bin/deploy.sh") {
		t.Errorf("delete card %q does not name the entry and its command", command)
	}
	if !strings.Contains(summary, "permanently remove your deploy script") {
		t.Errorf("delete summary %q does not name the entry", summary)
	}
	if _, _, ok := del.Confirmation([]byte(`{"family":"scripts","name":"ghost"}`)); ok {
		t.Error("a confirmation was offered for an entry that does not exist")
	}
}

// ------------------------------------------------- validation-retry surface

// TestWriteEntryValidationProblemsComeBackFieldKeyed pins requirement 4's
// shape: the field-keyed problems, nothing-written stated, and exactly the
// two legal continuations (fix and retry, or report) — never claim success.
func TestWriteEntryValidationProblemsComeBackFieldKeyed(t *testing.T) {
	admin := newFakeConfigAdmin()
	admin.upsertErrs = []error{&ConfigAdminError{Invalid: true,
		Message: "the entry was rejected by validation; nothing was written",
		Problems: []ConfigProblem{
			{Field: "phrases[0]", Message: `phrase "ship it" is already used by routines[0] ("shipping")`},
			{Message: "it has no steps"},
		}}}
	c := NewConfigTools(ConfigToolsOptions{Admin: admin})
	write := toolByName(t, c, ConfigWriteEntryToolName)
	result, err := write.Execute(context.Background(), []byte(`{"family":"scripts","entry":{
		"name":"deploy","phrases":["ship it"],"path":"/x"}}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"NOTHING was written",
		`- phrases[0]: phrase "ship it" is already used`,
		"- it has no steps",
		"Fix exactly what each problem names",
		"never say the change was made",
	} {
		if !strings.Contains(result, want) {
			t.Errorf("problem result %q missing %q", result, want)
		}
	}
}

// -------------------------------------------------- fingerprint discipline

// scriptEntry is the edit fixture: the entry as stored, and a changed draft.
func scriptEntry(path string) map[string]any {
	return map[string]any{"name": "deploy", "phrases": []any{"ship it"}, "path": path}
}

// readThenEdit primes the read cache the way a well-behaved model does.
func readThenEdit(t *testing.T, c *ConfigTools) {
	t.Helper()
	get := toolByName(t, c, ConfigGetEntryToolName)
	if _, err := get.Execute(context.Background(), []byte(`{"family":"scripts","name":"deploy"}`)); err != nil {
		t.Fatal(err)
	}
}

// TestWriteEntryRefusesABlindEdit: an edit with no prior read writes nothing
// and hands back the current entry — the refusal IS the read.
func TestWriteEntryRefusesABlindEdit(t *testing.T) {
	admin := newFakeConfigAdmin()
	admin.put("scripts", "deploy", scriptEntry("/old"))
	c := NewConfigTools(ConfigToolsOptions{Admin: admin})
	write := toolByName(t, c, ConfigWriteEntryToolName)
	result, err := write.Execute(context.Background(),
		[]byte(`{"family":"scripts","name":"deploy","entry":{"name":"deploy","path":"/new"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "you have not read this entry") || !strings.Contains(result, "/old") {
		t.Errorf("blind edit result %q does not refuse with the current entry", result)
	}
	if len(admin.upserts) != 0 {
		t.Errorf("a blind edit reached the write path: %d upserts", len(admin.upserts))
	}
	// The refusal counted as the read: the corrected retry may now write.
	if _, err := write.Execute(context.Background(),
		[]byte(`{"family":"scripts","name":"deploy","entry":{"name":"deploy","path":"/new"}}`)); err != nil {
		t.Fatal(err)
	}
	if len(admin.upserts) != 1 {
		t.Errorf("the corrected retry did not write: %d upserts", len(admin.upserts))
	}
}

// TestWriteEntryConflictRetriesOnceWhenTheEntryIsUntouched: a fingerprint
// conflict caused by a change ELSEWHERE in the file retries exactly once,
// internally, under the fresh fingerprint.
func TestWriteEntryConflictRetriesOnceWhenTheEntryIsUntouched(t *testing.T) {
	admin := newFakeConfigAdmin()
	admin.put("scripts", "deploy", scriptEntry("/old"))
	admin.upsertErrs = []error{&ConfigAdminError{Conflict: true, Fingerprint: "fp2"}}
	c := NewConfigTools(ConfigToolsOptions{Admin: admin})
	readThenEdit(t, c)
	admin.fingerprint = "fp2" // the file moved; the entry did not
	write := toolByName(t, c, ConfigWriteEntryToolName)
	result, err := write.Execute(context.Background(),
		[]byte(`{"family":"scripts","name":"deploy","entry":{"name":"deploy","path":"/new"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "Saved") {
		t.Errorf("retried write did not save: %q", result)
	}
	if len(admin.upserts) != 2 {
		t.Fatalf("upserts = %d, want the original and exactly one retry", len(admin.upserts))
	}
	if admin.upserts[1].fingerprint != "fp2" {
		t.Errorf("retry fingerprint = %q, want the re-read fp2", admin.upserts[1].fingerprint)
	}
}

// TestWriteEntryConflictSurfacesWhenTheEntryChanged: the hand-edited-mid-
// exchange criterion — the model's view is stale, so the write refuses, the
// current entry comes back, and nothing lands.
func TestWriteEntryConflictSurfacesWhenTheEntryChanged(t *testing.T) {
	admin := newFakeConfigAdmin()
	admin.put("scripts", "deploy", scriptEntry("/old"))
	c := NewConfigTools(ConfigToolsOptions{Admin: admin})
	readThenEdit(t, c)
	// The hand edit lands after the model's read.
	admin.put("scripts", "deploy", scriptEntry("/hand-edited"))
	admin.fingerprint = "fp2"
	write := toolByName(t, c, ConfigWriteEntryToolName)
	result, err := write.Execute(context.Background(),
		[]byte(`{"family":"scripts","name":"deploy","entry":{"name":"deploy","path":"/new"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "Not written") || !strings.Contains(result, "/hand-edited") {
		t.Errorf("stale edit result %q does not surface the conflict with the current entry", result)
	}
	if len(admin.upserts) != 0 {
		t.Errorf("a stale edit reached the write path: %d upserts", len(admin.upserts))
	}
}

// TestDeleteEntryConflictRetriesOnceWhenTheEntryIsUntouched mirrors the
// write's retry arm for the delete verb.
func TestDeleteEntryConflictRetriesOnceWhenTheEntryIsUntouched(t *testing.T) {
	admin := newFakeConfigAdmin()
	admin.put("scripts", "deploy", scriptEntry("/old"))
	admin.deleteErrs = []error{&ConfigAdminError{Conflict: true, Fingerprint: "fp2"}}
	c := NewConfigTools(ConfigToolsOptions{Admin: admin})
	del := toolByName(t, c, ConfigDeleteEntryToolName)
	result, err := del.Execute(context.Background(), []byte(`{"family":"scripts","name":"deploy"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "Deleted") {
		t.Errorf("retried delete did not delete: %q", result)
	}
	if len(admin.deletes) != 2 {
		t.Errorf("deletes = %d, want the original and exactly one retry", len(admin.deletes))
	}
}

// ----------------------------------------------------------- success wording

// TestWriteEntrySuccessWordsTheWrittenEntry: the confirmation the model is
// told to speak comes from a re-read of the file, never from the request.
func TestWriteEntrySuccessWordsTheWrittenEntry(t *testing.T) {
	admin := newFakeConfigAdmin()
	c := NewConfigTools(ConfigToolsOptions{Admin: admin})
	write := toolByName(t, c, ConfigWriteEntryToolName)
	result, err := write.Execute(context.Background(), []byte(`{"family":"scripts","entry":{
		"name":"deploy","phrases":["ship it"],"path":"/home/u/bin/deploy.sh"}}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Saved", "/home/u/bin/deploy.sh",
		"using these saved values", "never your request"} {
		if !strings.Contains(result, want) {
			t.Errorf("success result %q missing %q", result, want)
		}
	}
}

// TestWriteEntryReportsAnUnappliedWriteHonestly: applied=false travels to the
// model with its reason and an instruction to relay it.
func TestWriteEntryReportsAnUnappliedWriteHonestly(t *testing.T) {
	admin := newFakeConfigAdmin()
	admin.receipt = ConfigWriteReceipt{Applied: false,
		Reason: "the first knowledge feed needs a daemon restart to start fetching"}
	c := NewConfigTools(ConfigToolsOptions{Admin: admin})
	write := toolByName(t, c, ConfigWriteEntryToolName)
	result, err := write.Execute(context.Background(), []byte(`{"family":"knowledge.feeds","entry":{
		"name":"amd","command":["curl","-s","https://x"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "NOT yet in effect") || !strings.Contains(result, "daemon restart") {
		t.Errorf("unapplied result %q does not carry the honest status", result)
	}
}

// TestWriteSettingSuccessAndRestartHonesty: the saved value is what the
// confirmation states, and a restart-class change says so plainly.
func TestWriteSettingSuccessAndRestartHonesty(t *testing.T) {
	admin := newFakeConfigAdmin()
	admin.settings = []ConfigSettingView{{Key: "tts.kokoro.speed", Label: "Kokoro speed", Type: "float"}}
	admin.settingRes = ConfigSettingReceipt{Value: 1.3, Applied: true}
	c := NewConfigTools(ConfigToolsOptions{Admin: admin})
	set := toolByName(t, c, ConfigWriteSettingToolName)
	result, err := set.Execute(context.Background(), []byte(`{"key":"tts.kokoro.speed","value":1.3}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "tts.kokoro.speed is now 1.3") {
		t.Errorf("setting result %q does not state the saved value", result)
	}

	admin.settings = []ConfigSettingView{{Key: "assistant.name", Label: "Assistant name", Type: "string", Reload: "restart"}}
	admin.settingRes = ConfigSettingReceipt{Value: "Hal", Applied: true, NeedsRestart: true}
	result, err = set.Execute(context.Background(), []byte(`{"key":"assistant.name","value":"Hal"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `assistant.name is now "Hal"`) || !strings.Contains(result, "restarted") {
		t.Errorf("restart-class result %q does not carry the honest status", result)
	}
}

// TestWriteSettingRejectsUnknownKeysWithoutWriting: an unknown key is a
// correctable mistake, never a write and never the exclusion wall.
func TestWriteSettingRejectsUnknownKeysWithoutWriting(t *testing.T) {
	admin := newFakeConfigAdmin()
	c := NewConfigTools(ConfigToolsOptions{Admin: admin})
	set := toolByName(t, c, ConfigWriteSettingToolName)
	result, err := set.Execute(context.Background(), []byte(`{"key":"no.such","value":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "No setting is named") || !strings.Contains(result, "cannot invent") {
		t.Errorf("unknown-key result %q does not steer to config.read_settings", result)
	}
	if len(admin.settingWrites) != 0 {
		t.Errorf("an unknown key reached the admin: %d writes", len(admin.settingWrites))
	}
}

// ---------------------------------------------------------- honest wording

// TestConfigToolDescriptionsAreHonest pins the load-bearing sentences: the
// read-before-edit rule, daemon-side validation, and that the off-limits
// space is stated to the model up front (issue #105's prompt-side NFR).
func TestConfigToolDescriptionsAreHonest(t *testing.T) {
	c := NewConfigTools(ConfigToolsOptions{Admin: newFakeConfigAdmin()})
	get := toolByName(t, c, ConfigGetEntryToolName)
	if d := get.Description(); !strings.Contains(d, "MUST read an entry with this before editing") {
		t.Errorf("get_entry description lost the read-before-edit rule: %q", d)
	}
	write := toolByName(t, c, ConfigWriteEntryToolName)
	if d := write.Description(); !strings.Contains(d, "nothing is written when") ||
		!strings.Contains(d, "confirm") {
		t.Errorf("write_entry description lost validation or confirmation honesty: %q", d)
	}
	settings := toolByName(t, c, ConfigReadSettingsToolName)
	if d := settings.Description(); !strings.Contains(d, "tool permission policy") ||
		!strings.Contains(d, "never appear") {
		t.Errorf("read_settings description lost the off-limits statement: %q", d)
	}
}
