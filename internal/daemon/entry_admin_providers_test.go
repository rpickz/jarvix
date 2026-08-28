package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/tools"
	"github.com/rpickz/jarvix/internal/tts"
)

// The Providers section's daemon surface (issue #163): the two map-shaped
// families over the same four verbs the array families use, plus the Test
// action and the credential rules — which are the part of this file that
// matters most, and which are asserted by salting the configuration with a
// value that must never be seen again.

// leakSentinel is the salt. It is a value, not a pattern: every assertion
// below searches whole responses, whole event streams, whole log buffers and
// whole activity rows for it, so a leak through a path nobody thought of
// still fails a test rather than shipping.
const leakSentinel = "sk-LEAK-SENTINEL-must-never-be-echoed-4f2a9c"

// providersTOML is a hand-written configuration with three endpoints — one
// local, one holding a stored credential, one using the environment
// indirection — plus two advisors, one on its shipped preset and one with an
// argv of its own.
//
// Every base URL is loopback on a closed port on purpose: the Test action
// makes a REAL request, and a hostname here — even a reserved .test one —
// would send the suite to a resolver. A refused dial is instant, hermetic,
// and exactly the "unreachable" outcome the leak sweep wants to walk through.
func providersTOML() string {
	return `# hand-written, and it stays that way
[ai]
provider = "local"
model = "llama3.2:3b"

# the local model, no credential at all
[ai.local]
base_url = "http://127.0.0.1:11434/v1"

# a cloud endpoint with the key in the file
[ai.cloud]
base_url = "http://127.0.0.1:1/v1"
api_key = "` + leakSentinel + `"

[ai.viaenv]
base_url = "http://127.0.0.1:4/v1"
api_key_env = "JARVIX_TEST_PROVIDER_KEY"

[advisors.claude]
binary = "/usr/bin/claude"

[advisors.custom]
binary = "/usr/bin/custom"
args = ["--ask", "{question}"]
`
}

// startProvidersDaemon boots a daemon on providersTOML and hands back the
// client, the config path, and the buffer every log line lands in — because
// "never in a log line" is one of the credential rules and a rule nobody can
// read is a rule nobody keeps.
func startProvidersDaemon(t *testing.T) (*ipc.Client, string, *syncBuffer) {
	t.Helper()
	dir := t.TempDir()
	paths := config.Paths{
		Config: dir, Data: dir, State: dir, Runtime: dir,
		Socket: filepath.Join(dir, "j.sock"),
	}
	if err := os.WriteFile(paths.ConfigFile(), []byte(providersTOML()), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(paths.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	d, err := New(cfg, paths, logger, Deps{
		Provider:    &ai.Fake{Response: "ok"},
		Transcriber: &stt.Fake{Text: "hello"},
		Synthesizer: &tts.Fake{},
		Recorder:    &audio.FakeRecorder{Clip: audio.Clip{WAVPath: dir + "/r.wav"}},
		Player:      &audio.FakePlayer{},
		Notifier:    &desktop.FakeNotifier{},
		OpenWindow:  func(context.Context) error { return nil },
		Compositor:  desktop.NewFakeCompositor(),
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDaemon(t, d)
	return dialDaemon(t, paths.Socket), paths.ConfigFile(), logs
}

// entryCallErr calls one entry-admin method expecting a refusal, and returns
// the raw error plus its decoded {field, message} problems.
func entryCallErr(t *testing.T, client *ipc.Client, method string, params map[string]any) (*ipc.Error, []map[string]any) {
	t.Helper()
	err := client.Call(method, params, nil)
	var rpcErr *ipc.Error
	if !errors.As(err, &rpcErr) {
		t.Fatalf("%s: err = %v, want an rpc error", method, err)
	}
	data, _ := rpcErr.Data.(map[string]any)
	if data == nil {
		return rpcErr, nil
	}
	return rpcErr, entryProblemList(t, data)
}

// jsonText renders any wire value as the text a client would receive, so an
// assertion can search the WHOLE reply rather than the fields it remembered
// to look at.
func jsonText(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestProviderCredentialIsNeverReadBack: the read wire carries presence and a
// variable name, never a value and never a mask — a masked prefix is a prefix,
// and a mask of the right length is the length.
func TestProviderCredentialIsNeverReadBack(t *testing.T) {
	client, _, _ := startProvidersDaemon(t)

	out := entryCall(t, client, "config.get_entry", map[string]any{"family": "ai", "name": "cloud"})
	body := jsonText(t, out)
	if strings.Contains(body, leakSentinel) {
		t.Fatalf("config.get_entry returned the stored credential:\n%s", body)
	}
	// Not even a fragment: a leak that only shows the first eight characters
	// is still a leak, and one that shows none but keeps the length is a hint.
	if strings.Contains(body, leakSentinel[:12]) {
		t.Errorf("config.get_entry returned part of the credential:\n%s", body)
	}
	entry, _ := out["entry"].(map[string]any)
	if _, present := entry["api_key"]; present {
		t.Errorf("entry = %v, want no api_key key at all", entry)
	}
	if entry["base_url"] != "http://127.0.0.1:1/v1" || entry["name"] != "cloud" {
		t.Errorf("entry = %v, want the endpoint's own fields", entry)
	}
	state := secretState(t, out, "api_key")
	if state["set"] != true || state["source"] != "config" || state["inline_key"] != true {
		t.Errorf("api_key state = %v, want set from the config file", state)
	}
	if state["env_set"] != false || state["env"] != "" {
		t.Errorf("api_key state = %v, want no environment claim", state)
	}
}

// secretState pulls one credential's reported state out of a reply.
func secretState(t *testing.T, out map[string]any, key string) map[string]any {
	t.Helper()
	secrets, ok := out["secrets"].(map[string]any)
	if !ok {
		t.Fatalf("reply carries no secrets block: %v", out)
	}
	state, ok := secrets[key].(map[string]any)
	if !ok {
		t.Fatalf("secrets carry no %q: %v", key, secrets)
	}
	return state
}

// TestProviderEnvIndirectionIsNamedAndResolved: the form is told WHICH
// variable is expected and whether it currently resolves — the two facts that
// let a user fix a missing key — and never its contents.
func TestProviderEnvIndirectionIsNamedAndResolved(t *testing.T) {
	client, _, _ := startProvidersDaemon(t)

	out := entryCall(t, client, "config.get_entry", map[string]any{"family": "ai", "name": "viaenv"})
	state := secretState(t, out, "api_key")
	if state["env"] != "JARVIX_TEST_PROVIDER_KEY" {
		t.Errorf("env = %v, want the variable's name", state["env"])
	}
	if state["env_set"] != false || state["set"] != false || state["source"] != "none" {
		t.Errorf("state = %v, want an unresolved variable reported as not set", state)
	}

	// With the variable exported, the same endpoint resolves — and the reply
	// still says only that it does.
	t.Setenv("JARVIX_TEST_PROVIDER_KEY", leakSentinel)
	out = entryCall(t, client, "config.get_entry", map[string]any{"family": "ai", "name": "viaenv"})
	state = secretState(t, out, "api_key")
	if state["env_set"] != true || state["set"] != true || state["source"] != "env" {
		t.Errorf("state = %v, want the environment reported as the source", state)
	}
	if body := jsonText(t, out); strings.Contains(body, leakSentinel) {
		t.Errorf("the environment's value reached the wire:\n%s", body)
	}
}

// TestProviderCredentialSurvivesAnEditThatNeverSawIt: the form edits the base
// URL of an endpoint whose key it was never shown, and the key is still there
// afterwards. This is the credential bug this design exists to prevent — a
// round trip that silently drops what it could not render.
func TestProviderCredentialSurvivesAnEditThatNeverSawIt(t *testing.T) {
	client, configFile, _ := startProvidersDaemon(t)

	out := entryCall(t, client, "config.get_entry", map[string]any{"family": "ai", "name": "cloud"})
	entry, _ := out["entry"].(map[string]any)
	entry["base_url"] = "http://127.0.0.1:2/v1"
	entryCall(t, client, "config.upsert_entry", map[string]any{
		"family": "ai", "name": "cloud", "entry": entry, "fingerprint": out["fingerprint"],
	})

	raw, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), leakSentinel) {
		t.Errorf("the save dropped the stored credential:\n%s", raw)
	}
	if !strings.Contains(string(raw), "http://127.0.0.1:2/v1") {
		t.Errorf("the save did not land:\n%s", raw)
	}
	after := entryCall(t, client, "config.get_entry", map[string]any{"family": "ai", "name": "cloud"})
	if state := secretState(t, after, "api_key"); state["set"] != true {
		t.Errorf("after the edit the credential reads as unset: %v", state)
	}
}

// TestProviderCredentialIsReplacedAndCleared: the two write instructions,
// each landing exactly what it says and nothing else.
func TestProviderCredentialIsReplacedAndCleared(t *testing.T) {
	client, configFile, _ := startProvidersDaemon(t)
	const replacement = "sk-replacement-value-9911"

	out := entryCall(t, client, "config.get_entry", map[string]any{"family": "ai", "name": "cloud"})
	entry, _ := out["entry"].(map[string]any)
	entryCall(t, client, "config.upsert_entry", map[string]any{
		"family": "ai", "name": "cloud", "entry": entry, "fingerprint": out["fingerprint"],
		"secrets": map[string]any{"api_key": map[string]any{"action": "set", "value": replacement}},
	})
	raw, _ := os.ReadFile(configFile)
	if strings.Contains(string(raw), leakSentinel) || !strings.Contains(string(raw), replacement) {
		t.Errorf("the replacement did not take:\n%s", raw)
	}

	out = entryCall(t, client, "config.get_entry", map[string]any{"family": "ai", "name": "cloud"})
	entry, _ = out["entry"].(map[string]any)
	entryCall(t, client, "config.upsert_entry", map[string]any{
		"family": "ai", "name": "cloud", "entry": entry, "fingerprint": out["fingerprint"],
		"secrets": map[string]any{"api_key": map[string]any{"action": "clear"}},
	})
	raw, _ = os.ReadFile(configFile)
	if strings.Contains(string(raw), replacement) || strings.Contains(string(raw), "api_key =") {
		t.Errorf("clearing left a credential behind:\n%s", raw)
	}
	after := entryCall(t, client, "config.get_entry", map[string]any{"family": "ai", "name": "cloud"})
	if state := secretState(t, after, "api_key"); state["set"] != false || state["source"] != "none" {
		t.Errorf("after clearing, state = %v, want nothing set", state)
	}
}

// TestProviderCredentialCannotTravelInsideTheEntry: the value has exactly one
// route in, and it is not the entry map — which is echoed back to forms,
// carried through validation, and quoted in problems.
func TestProviderCredentialCannotTravelInsideTheEntry(t *testing.T) {
	client, configFile, _ := startProvidersDaemon(t)
	before, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}

	rpcErr, problems := entryCallErr(t, client, "config.upsert_entry", map[string]any{
		"family": "ai", "name": "local",
		"entry": map[string]any{
			"name": "local", "base_url": "http://127.0.0.1:11434/v1",
			"api_key": "sk-smuggled-in-the-entry-0001",
		},
	})
	if rpcErr.Code != ipc.CodeConfigInvalid {
		t.Errorf("code = %v, want the validation refusal", rpcErr.Code)
	}
	msg, ok := problemOn(problems, "api_key")
	if !ok || !strings.Contains(msg, "write-only") {
		t.Errorf("problems = %v, want api_key refused as write-only", problems)
	}
	if strings.Contains(jsonText(t, rpcErr), "sk-smuggled-in-the-entry-0001") {
		t.Errorf("the refusal echoed the value it refused: %v", rpcErr)
	}
	after, _ := os.ReadFile(configFile)
	if !bytes.Equal(before, after) {
		t.Errorf("a refused write changed the file")
	}
}

// TestProviderCredentialNeverEscapesAnyPath is the leak-salted sweep: every
// verb the surface has, including the failure paths, run against a
// configuration holding the sentinel — and the sentinel must be absent from
// every reply, every event, every activity row, and every log line.
func TestProviderCredentialNeverEscapesAnyPath(t *testing.T) {
	client, _, logs := startProvidersDaemon(t)

	// Every path, in order, each one collected as the text a client sees.
	var seen []string
	record := func(label string, v any) { seen = append(seen, label+": "+jsonText(t, v)) }

	get := entryCall(t, client, "config.get_entry", map[string]any{"family": "ai", "name": "cloud"})
	record("get", get)
	entry, _ := get["entry"].(map[string]any)

	record("validate", entryCall(t, client, "config.validate_entry", map[string]any{
		"family": "ai", "name": "cloud", "entry": entry}))

	// A validation failure, with the credential in the document being
	// validated: the base URL is emptied, so the loader's own rules refuse
	// the whole document while the stored key is folded into the draft.
	broken := map[string]any{"name": "cloud", "base_url": ""}
	record("validate-invalid", entryCall(t, client, "config.validate_entry", map[string]any{
		"family": "ai", "name": "cloud", "entry": broken}))
	refusedErr, _ := entryCallErr(t, client, "config.upsert_entry", map[string]any{
		"family": "ai", "name": "cloud", "entry": broken, "fingerprint": get["fingerprint"]})
	record("upsert-refused", refusedErr)

	// A stale-fingerprint conflict, and a delete guard refusal.
	conflictErr, _ := entryCallErr(t, client, "config.upsert_entry", map[string]any{
		"family": "ai", "name": "cloud", "entry": entry, "fingerprint": "sha256:not-the-file"})
	record("conflict", conflictErr)
	guardErr, _ := entryCallErr(t, client, "config.delete_entry", map[string]any{
		"family": "ai", "name": "local"})
	record("delete-guard", guardErr)

	// The Test action against an endpoint that cannot answer: the failure
	// carries the service's own words, and those words are not the key.
	record("test", entryCall(t, client, "config.test_entry",
		map[string]any{"family": "ai", "name": "cloud"}))

	// A successful save, its event, and the activity row it produces.
	fresh := entryCall(t, client, "config.get_entry", map[string]any{"family": "ai", "name": "cloud"})
	edited, _ := fresh["entry"].(map[string]any)
	edited["base_url"] = "http://127.0.0.1:3/v1"
	record("upsert", entryCall(t, client, "config.upsert_entry", map[string]any{
		"family": "ai", "name": "cloud", "entry": edited, "fingerprint": fresh["fingerprint"]}))
	record("entry_changed", waitForEvent(t, client, "config.entry_changed"))

	// testdiscipline:allow this samples the feed to prove a NEGATIVE — that
	// whatever rows exist do not carry the credential. The usual trap (#167)
	// is asserting a row is present having waited only for the event the
	// watcher derives it from; here a row that has not landed yet weakens the
	// sweep by one row and can never fail it, so waiting for the row would buy
	// nothing and would need a label this test does not otherwise care about.
	var activity map[string]any
	if err := client.Call("activity.get", nil, &activity); err != nil {
		t.Fatal(err)
	}
	record("activity", activity)

	// And the delete of an endpoint that IS removable, which rewrites the
	// document the credential lives in.
	final := entryCall(t, client, "config.get_entry", map[string]any{"family": "ai", "name": "cloud"})
	record("delete", entryCall(t, client, "config.delete_entry", map[string]any{
		"family": "ai", "name": "cloud", "fingerprint": final["fingerprint"]}))

	// Every event still queued for this client, drained without blocking:
	// waitForEvent above consumed the ones it was waiting for and recorded
	// them, and this takes whatever else the daemon pushed on the way —
	// config.changed among them, which accompanies every write.
	record("events", drainEvents(client))
	seen = append(seen, "logs: "+logs.String())
	for _, text := range seen {
		if strings.Contains(text, leakSentinel) {
			t.Errorf("the credential leaked through %s", text)
		}
		if strings.Contains(text, leakSentinel[:12]) {
			t.Errorf("part of the credential leaked through %s", text)
		}
	}
}

// drainEvents takes every event already queued for a client. It never blocks,
// so the ordering it depends on is the one the calls above established — the
// writes have returned, which means their events were published (#156: a test
// that only passes under -race is not passing).
func drainEvents(client *ipc.Client) []map[string]any {
	var out []map[string]any
	for {
		select {
		case ev := <-client.Events():
			out = append(out, map[string]any{"type": ev.Type, "data": ev.Data})
		default:
			return out
		}
	}
}

// TestProvidersListingIsRegistryDriven: one listing verb serves both document
// shapes and every family, hands back whole entries so a screen renders what
// it has widgets for — and carries no credential, exactly like the single-entry
// read it mirrors.
func TestProvidersListingIsRegistryDriven(t *testing.T) {
	client, _, _ := startProvidersDaemon(t)

	out := entryCall(t, client, "config.list_entries", map[string]any{"family": "ai"})
	if out["kind"] != "endpoint" || out["in_use"] != "local" {
		t.Errorf("listing = %v, want the endpoint kind and the in-use provider", out)
	}
	rows, _ := out["entries"].([]any)
	if len(rows) != 3 {
		t.Fatalf("entries = %v, want the three configured endpoints", rows)
	}
	if body := jsonText(t, out); strings.Contains(body, leakSentinel) {
		t.Errorf("the listing carried a credential:\n%s", body)
	}
	names := map[string]bool{}
	for _, r := range rows {
		row, _ := r.(map[string]any)
		entry, _ := row["entry"].(map[string]any)
		name, _ := entry["name"].(string)
		names[name] = true
		if _, present := entry["api_key"]; present {
			t.Errorf("listing row %q carried an api_key key", name)
		}
		if name == "cloud" {
			secrets, _ := row["secrets"].(map[string]any)
			state, _ := secrets["api_key"].(map[string]any)
			if state["set"] != true {
				t.Errorf("cloud row = %v, want its credential reported as set", row)
			}
		}
	}
	for _, want := range []string{"local", "cloud", "viaenv"} {
		if !names[want] {
			t.Errorf("listing is missing %q: %v", want, names)
		}
	}

	// The advisor family through the same verb, carrying the tier note each
	// row earns — which is what lets a listing say "asks first" without the
	// user opening the form.
	out = entryCall(t, client, "config.list_entries", map[string]any{"family": "advisors"})
	rows, _ = out["entries"].([]any)
	if len(rows) != 2 {
		t.Fatalf("advisor entries = %v, want two", rows)
	}
	for _, r := range rows {
		row, _ := r.(map[string]any)
		notes, _ := row["notes"].([]any)
		if len(notes) == 0 {
			t.Errorf("advisor row %v carries no tier note", row)
		}
	}

	// And an array family answers the same verb unchanged, which is the
	// claim: one listing, every shape.
	out = entryCall(t, client, "config.list_entries", map[string]any{"family": "routines"})
	if out["kind"] != "routine" {
		t.Errorf("routines listing = %v, want the array family served too", out)
	}
}

// TestDeletingTheEndpointInUseIsRefusedWithThatReason: the guard the
// whole-document validation cannot make, because a deleted PRESET endpoint
// still validates while leaving the user's provider pointing at something
// they can no longer see.
func TestDeletingTheEndpointInUseIsRefusedWithThatReason(t *testing.T) {
	client, configFile, _ := startProvidersDaemon(t)
	before, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}

	rpcErr, problems := entryCallErr(t, client, "config.delete_entry",
		map[string]any{"family": "ai", "name": "local"})
	if rpcErr.Code != ipc.CodeConfigInvalid {
		t.Errorf("code = %v, want the validation refusal", rpcErr.Code)
	}
	if len(problems) != 1 || !strings.Contains(fmt.Sprint(problems[0]["message"]), "ai.provider") {
		t.Errorf("problems = %v, want the in-use reason", problems)
	}
	after, _ := os.ReadFile(configFile)
	if !bytes.Equal(before, after) {
		t.Errorf("a refused delete changed the file")
	}

	// The endpoint nobody is using deletes fine, which is what proves the
	// guard is a reason rather than a blanket refusal.
	fp := entryCall(t, client, "config.get_entry", map[string]any{"family": "ai", "name": "viaenv"})
	entryCall(t, client, "config.delete_entry", map[string]any{
		"family": "ai", "name": "viaenv", "fingerprint": fp["fingerprint"]})
	raw, _ := os.ReadFile(configFile)
	if strings.Contains(string(raw), "[ai.viaenv]") {
		t.Errorf("the unused endpoint survived its delete:\n%s", raw)
	}
}

// TestEndpointProblemsPinToTheFieldThatIsWrong: the #99/#101 discipline for a
// keyed family — the daemon's field-keyed problem lands on the input, and
// nothing is written.
func TestEndpointProblemsPinToTheFieldThatIsWrong(t *testing.T) {
	client, configFile, _ := startProvidersDaemon(t)
	before, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}

	out := entryCall(t, client, "config.validate_entry", map[string]any{
		"family": "ai", "name": "viaenv",
		"entry": map[string]any{"name": "viaenv", "base_url": "api.example.test/v1"},
	})
	if out["valid"] != false {
		t.Errorf("valid = %v, want a scheme-less URL refused", out["valid"])
	}
	msg, ok := problemOn(entryProblemList(t, out), "base_url")
	if !ok || !strings.Contains(msg, "http://") {
		t.Errorf("problems = %v, want one on base_url", entryProblemList(t, out))
	}

	_, problems := entryCallErr(t, client, "config.upsert_entry", map[string]any{
		"family": "ai", "name": "viaenv",
		"entry": map[string]any{"name": "viaenv", "base_url": "api.example.test/v1"},
	})
	if _, ok := problemOn(problems, "base_url"); !ok {
		t.Errorf("save problems = %v, want one on base_url", problems)
	}
	after, _ := os.ReadFile(configFile)
	if !bytes.Equal(before, after) {
		t.Errorf("a refused save changed the file")
	}
}

// TestKeyedNameCollisionLandsOnTheNameField: a taken name is a fact about the
// name, so it arrives on the name field. Without this it would surface as the
// read-back guard's "unparsable document" — true, and useless to the person
// who typed it.
func TestKeyedNameCollisionLandsOnTheNameField(t *testing.T) {
	client, configFile, _ := startProvidersDaemon(t)
	before, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}

	// Creating a second endpoint called "cloud".
	out := entryCall(t, client, "config.validate_entry", map[string]any{
		"family": "ai", "name": "",
		"entry": map[string]any{"name": "cloud", "base_url": "http://127.0.0.1:9/v1"},
	})
	msg, ok := problemOn(entryProblemList(t, out), "name")
	if !ok || !strings.Contains(msg, "already a [ai.cloud] table") {
		t.Errorf("problems = %v, want the collision on name", entryProblemList(t, out))
	}

	// And renaming one endpoint onto another's name is the same fact.
	_, problems := entryCallErr(t, client, "config.upsert_entry", map[string]any{
		"family": "ai", "name": "viaenv",
		"entry": map[string]any{"name": "cloud", "base_url": "http://127.0.0.1:9/v1"},
	})
	if _, ok := problemOn(problems, "name"); !ok {
		t.Errorf("rename problems = %v, want the collision on name", problems)
	}
	after, _ := os.ReadFile(configFile)
	if !bytes.Equal(before, after) {
		t.Errorf("a refused rename changed the file")
	}

	// A rename to a free name still works, which is what proves the guard is
	// about collisions rather than about renaming.
	fp := entryCall(t, client, "config.get_entry", map[string]any{"family": "ai", "name": "viaenv"})
	entry, _ := fp["entry"].(map[string]any)
	entry["name"] = "elsewhere"
	entryCall(t, client, "config.upsert_entry", map[string]any{
		"family": "ai", "name": "viaenv", "entry": entry, "fingerprint": fp["fingerprint"]})
	raw, _ := os.ReadFile(configFile)
	if !strings.Contains(string(raw), "[ai.elsewhere]") || strings.Contains(string(raw), "[ai.viaenv]") {
		t.Errorf("the rename did not take:\n%s", raw)
	}
}

// TestNewEndpointAppearsInTheProviderPicker: an endpoint written through the
// form is selectable straight away, on the reload class [ai] settings already
// carry — no restart, which is the acceptance criterion.
func TestNewEndpointAppearsInTheProviderPicker(t *testing.T) {
	client, configFile, _ := startProvidersDaemon(t)

	fp := entryCall(t, client, "config.get_entry", map[string]any{"family": "ai", "name": "local"})
	result := entryCall(t, client, "config.upsert_entry", map[string]any{
		"family": "ai", "name": "", "fingerprint": fp["fingerprint"],
		"entry": map[string]any{
			"name": "work", "base_url": "https://work.example.test/v1",
			"api_key_env": "WORK_API_KEY",
		},
		"secrets": map[string]any{"api_key": map[string]any{"action": "set", "value": "sk-work-key-1234"}},
	})
	if result["created"] != true {
		t.Errorf("result = %v, want created", result)
	}
	if result["applied"] != true {
		t.Errorf("result = %v, want the endpoint live without a restart", result)
	}

	raw, _ := os.ReadFile(configFile)
	if !strings.Contains(string(raw), "[ai.work]") || !strings.Contains(string(raw), "sk-work-key-1234") {
		t.Errorf("config.toml does not carry the new endpoint:\n%s", raw)
	}
	// The file's permissions are not widened by a rewrite that adds a
	// credential to it.
	info, err := os.Stat(configFile)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("config.toml is mode %o after a credential write; it must not be group- or world-readable", perm)
	}

	var settings struct {
		Fields []struct {
			Key  string   `json:"key"`
			Enum []string `json:"enum"`
		} `json:"fields"`
	}
	if err := client.Call("config.get", nil, &settings); err != nil {
		t.Fatal(err)
	}
	for _, f := range settings.Fields {
		if f.Key != "ai.provider" {
			continue
		}
		if !containsName(f.Enum, "work") {
			t.Errorf("ai.provider enum = %v, want the new endpoint", f.Enum)
		}
		return
	}
	t.Error("config.get carries no ai.provider field")
}

// containsName reports membership.
func containsName(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestAdvisorTierIsStatedBeforeItIsEarned: the note the form shows beside the
// argv, which is what stops a user loosening or tightening a permission gate
// as a side effect of typing a flag (ADR 0016).
func TestAdvisorTierIsStatedBeforeItIsEarned(t *testing.T) {
	client, configFile, _ := startProvidersDaemon(t)

	// A shipped read-only preset, argv untouched: allow.
	out := entryCall(t, client, "config.validate_entry", map[string]any{
		"family": "advisors", "name": "claude",
		"entry": map[string]any{"name": "claude", "binary": "/usr/bin/claude"},
	})
	note := noteOn(t, out, "args")
	if !strings.Contains(note, "Permission: allow") || !strings.Contains(note, "shipped") {
		t.Errorf("note = %q, want the allow tier explained", note)
	}

	// The same advisor with an argv of its own: ask, and the note says why.
	out = entryCall(t, client, "config.validate_entry", map[string]any{
		"family": "advisors", "name": "claude",
		"entry": map[string]any{"name": "claude", "binary": "/usr/bin/claude",
			"args": []any{"-p", "--dangerously-skip-permissions"}},
	})
	note = noteOn(t, out, "args")
	if !strings.Contains(note, "Permission: ask") || !strings.Contains(note, "not audited") {
		t.Errorf("note = %q, want the ask tier explained", note)
	}

	// A preset that is never read-only stays ask however it is configured —
	// a coding agent's whole purpose is changing things, whatever its flags
	// say. This one is a creation, so it carries no target name.
	out = entryCall(t, client, "config.validate_entry", map[string]any{
		"family": "advisors", "name": "",
		"entry": map[string]any{"name": "aider", "binary": "/usr/bin/aider"},
	})
	if note = noteOn(t, out, "args"); !strings.Contains(note, "Permission: ask") {
		t.Errorf("note = %q, want a file-editing agent held at ask", note)
	}

	// The tier the note promises is the tier the daemon would actually apply:
	// the note is a reading of ADR 0016's rule, not a second copy of it.
	cfg, err := config.Load(configFile)
	if err != nil {
		t.Fatal(err)
	}
	tiers := advisorPolicyTiers(cfg)
	if tiers["claude"] != tools.PolicyAllow {
		t.Errorf("claude tier = %v, want allow to match its note", tiers["claude"])
	}
	if tiers["custom"] != tools.PolicyAsk {
		t.Errorf("custom tier = %v, want ask to match its note", tiers["custom"])
	}
}

// noteOn pulls the note keyed to one field out of a reply.
func noteOn(t *testing.T, out map[string]any, field string) string {
	t.Helper()
	notes, _ := out["notes"].([]any)
	for _, n := range notes {
		m, _ := n.(map[string]any)
		if m["field"] == field {
			msg, _ := m["message"].(string)
			return msg
		}
	}
	t.Fatalf("reply carries no note on %q: %v", field, out)
	return ""
}

// TestAdvisorPlaceholderRuleIsEnforcedOnTheField: the loader's own rule about
// {question} — a whole argv element, never interpolated into a larger one —
// arrives keyed to the argv the user typed.
func TestAdvisorPlaceholderRuleIsEnforcedOnTheField(t *testing.T) {
	client, configFile, _ := startProvidersDaemon(t)
	before, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}

	_, problems := entryCallErr(t, client, "config.upsert_entry", map[string]any{
		"family": "advisors", "name": "custom",
		"entry": map[string]any{"name": "custom", "binary": "/usr/bin/custom",
			"args": []any{"--ask=" + config.AdvisorQuestionPlaceholder}},
	})
	msg, ok := problemOn(problems, "args")
	if !ok || !strings.Contains(msg, "argument of its own") {
		t.Errorf("problems = %v, want the placeholder rule on args", problems)
	}
	after, _ := os.ReadFile(configFile)
	if !bytes.Equal(before, after) {
		t.Errorf("a refused advisor save changed the file")
	}
}

// TestAdvisorSaveSaysItNeedsARestart: the advisor tool and its tiers are
// built once, at construction, so a saved advisor is true of the file and not
// yet of this daemon — and the reply says so rather than showing a saved
// advisor that is never consulted.
func TestAdvisorSaveSaysItNeedsARestart(t *testing.T) {
	client, configFile, _ := startProvidersDaemon(t)

	fp := entryCall(t, client, "config.get_entry", map[string]any{"family": "advisors", "name": "claude"})
	result := entryCall(t, client, "config.upsert_entry", map[string]any{
		"family": "advisors", "name": "", "fingerprint": fp["fingerprint"],
		"entry": map[string]any{"name": "gemini", "binary": "/usr/bin/gemini", "timeout_sec": 90},
	})
	if result["applied"] != false {
		t.Errorf("result = %v, want a restart-class advisor reported as not live", result)
	}
	if reason, _ := result["reason"].(string); !strings.Contains(reason, "restart") {
		t.Errorf("reason = %q, want the restart explained", reason)
	}
	raw, _ := os.ReadFile(configFile)
	if !strings.Contains(string(raw), "[advisors.gemini]") {
		t.Errorf("the advisor was not written:\n%s", raw)
	}
}

// TestEndpointTestActionReportsWhatHappened: the three outcomes, each from a
// real request, none of them invented. The HTTP layer is faked exactly where
// the doctor already fakes it — an httptest server in the base URL, which
// binds loopback and never touches a network.
func TestEndpointTestActionReportsWhatHappened(t *testing.T) {
	var gotAuth, gotPath string
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o-mini"}]}`))
	}))
	t.Cleanup(ok.Close)
	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided"}}`))
	}))
	t.Cleanup(denied.Close)

	toml := "[ai]\nprovider = \"good\"\nmodel = \"m\"\n\n" +
		"[ai.good]\nbase_url = \"" + ok.URL + "\"\napi_key = \"sk-good-key-2211\"\n\n" +
		"[ai.denied]\nbase_url = \"" + denied.URL + "\"\napi_key = \"sk-bad-key-3322\"\n\n" +
		// Port 1 is guaranteed refused — the repo's existing idiom for an
		// unreachable endpoint, and no network is involved either way.
		"[ai.dead]\nbase_url = \"http://127.0.0.1:1/v1\"\n"
	client, _ := startAdminDaemon(t, toml)

	out := entryCall(t, client, "config.test_entry", map[string]any{"family": "ai", "name": "good"})
	if out["outcome"] != "reachable" {
		t.Errorf("outcome = %v, want reachable", out["outcome"])
	}
	if gotPath != "/models" {
		t.Errorf("probe asked for %q, want the models listing — the cheapest call that proves auth", gotPath)
	}
	if gotAuth != "Bearer sk-good-key-2211" {
		t.Errorf("probe sent %q, want the endpoint's own credential resolved daemon-side", gotAuth)
	}
	if body := jsonText(t, out); strings.Contains(body, "sk-good-key-2211") {
		t.Errorf("the test result carried the credential:\n%s", body)
	}

	out = entryCall(t, client, "config.test_entry", map[string]any{"family": "ai", "name": "denied"})
	if out["outcome"] != "unauthorised" {
		t.Errorf("outcome = %v, want unauthorised", out["outcome"])
	}
	if detail, _ := out["detail"].(string); !strings.Contains(detail, "Incorrect API key provided") {
		t.Errorf("detail = %q, want the service's own words", detail)
	}
	if body := jsonText(t, out); strings.Contains(body, "sk-bad-key-3322") {
		t.Errorf("the unauthorised result carried the credential:\n%s", body)
	}

	out = entryCall(t, client, "config.test_entry", map[string]any{"family": "ai", "name": "dead"})
	if out["outcome"] != "unreachable" {
		t.Errorf("outcome = %v, want unreachable", out["outcome"])
	}
	if detail, _ := out["detail"].(string); !strings.Contains(detail, "127.0.0.1:1") {
		t.Errorf("detail = %q, want the transport's own words", detail)
	}
}

// TestTestActionRefusesAFamilyWithNoProbe: honesty over convenience — a family
// with nothing to probe is told so, rather than answered with a success
// nothing performed.
func TestTestActionRefusesAFamilyWithNoProbe(t *testing.T) {
	client, _, _ := startProvidersDaemon(t)
	rpcErr, _ := entryCallErr(t, client, "config.test_entry",
		map[string]any{"family": "advisors", "name": "claude"})
	if rpcErr.Code != ipc.CodeInvalidParams || !strings.Contains(rpcErr.Message, "nothing to test") {
		t.Errorf("err = %v, want a refusal naming the reason", rpcErr)
	}
}

// TestAssistantCannotReachTheProviderFamilies is #109's exclusion wall,
// pinned on the surface the model actually operates: the families are absent
// from the tool's closed set, the bridge refuses them before the shared write
// path, and the reason is spoken-ready.
func TestAssistantCannotReachTheProviderFamilies(t *testing.T) {
	dir := t.TempDir()
	paths := config.Paths{Config: dir, Data: dir, State: dir, Runtime: dir,
		Socket: filepath.Join(dir, "j.sock")}
	if err := os.WriteFile(paths.ConfigFile(), []byte(providersTOML()), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(paths.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	d, err := New(cfg, paths, nil, Deps{
		Provider:    &ai.Fake{Response: "ok"},
		Transcriber: &stt.Fake{Text: "hello"},
		Synthesizer: &tts.Fake{},
		Recorder:    &audio.FakeRecorder{Clip: audio.Clip{WAVPath: dir + "/r.wav"}},
		Player:      &audio.FakePlayer{},
		Notifier:    &desktop.FakeNotifier{},
		OpenWindow:  func(context.Context) error { return nil },
		Compositor:  desktop.NewFakeCompositor(),
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(paths.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}

	admin := &assistantConfigAdmin{d: d}
	for _, family := range []string{"ai", "advisors"} {
		if _, err := admin.ListEntries(family); err == nil {
			t.Errorf("the assistant listed %q", family)
		}
		if _, err := admin.GetEntry(family, "cloud"); err == nil {
			t.Errorf("the assistant read a %q entry", family)
		}
		_, err := admin.UpsertEntry(family, "", map[string]any{
			"name": "mine", "base_url": "https://evil.example.test/v1"}, "")
		var adminErr *tools.ConfigAdminError
		if !errors.As(err, &adminErr) || !strings.Contains(adminErr.Message, "may not") {
			t.Errorf("the assistant wrote to %q: err = %v", family, err)
		}
		if _, err := admin.DeleteEntry(family, "cloud", ""); err == nil {
			t.Errorf("the assistant deleted a %q entry", family)
		}
	}
	// The settings half of the same wall, unchanged and still standing.
	if _, err := admin.WriteSetting("ai.provider", "cloud"); err == nil {
		t.Error("the assistant changed ai.provider")
	}
	after, _ := os.ReadFile(paths.ConfigFile())
	if !bytes.Equal(before, after) {
		t.Errorf("a refused assistant write changed the file")
	}
	// And the tool schema never names them, so the model is not refused — it
	// is never offered.
	for _, family := range tools.ConfigEntryFamilies() {
		if family == "ai" || family == "advisors" {
			t.Errorf("the tool surface offers %q", family)
		}
	}
}
