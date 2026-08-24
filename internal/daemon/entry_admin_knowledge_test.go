package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/ipc"
)

// The Knowledge tab's form surface (issue #100) over a fully wired daemon:
// the generic entry-admin verbs (#99/ADR 0033) driving the knowledge.feeds
// registry row. What is pinned here, on the socket, is everything the window
// renders and nothing it decides (ADR 0013): the loader's own feed rules
// field-keyed to the form, byte preservation around every write, the
// fingerprint conflict, the running service adopting a saved feed on the
// standard reload — and above all that saving a feed NEVER runs its command,
// because a feed's argv is exactly the kind of text a form must not turn
// into an execution path.

// knowledgeFormTOML is the hand-written config the form tests boot from and
// edit: two lazy feeds (lazy so nothing fetches until a test asks), with the
// comments and the trailing [tts] table as the byte-preservation fixture.
const knowledgeFormTOML = `# my config, hand-written
[context]
window = false
selection = false
clipboard = false

# watches the AMD price
[[knowledge.feeds]]
name = "amd"
description = "AMD share price"
command = ["/bin/echo", "187.42"]
mode = "lazy"

# the weather feed
[[knowledge.feeds]]
name = "weather"
description = "Local weather"
command = ["/bin/echo", "sunny"]
mode = "lazy"

[tts]
provider = "piper"
`

// startKnowledgeFormDaemon boots from knowledgeFormTOML, plus a stub command
// whose marker file proves any execution — the never-run assertions read it.
func startKnowledgeFormDaemon(t *testing.T) (*ipc.Client, string, string) {
	t.Helper()
	dir := t.TempDir()
	stub := filepath.Join(dir, "feed-cmd.sh")
	marker := filepath.Join(dir, "fetched.marker")
	if err := os.WriteFile(stub,
		[]byte("#!/bin/sh\ntouch "+marker+"\necho '901.11'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	client, paths := startAdminDaemon(t, knowledgeFormTOML)
	return client, paths.ConfigFile(), stub
}

// knowledgeFingerprint reads the fingerprint the tab's status carries.
func knowledgeFingerprint(t *testing.T, client *ipc.Client) string {
	t.Helper()
	var status map[string]any
	if err := client.Call("knowledge.status", nil, &status); err != nil {
		t.Fatal(err)
	}
	fp, _ := status["fingerprint"].(string)
	if fp == "" {
		t.Fatal("knowledge.status carries no fingerprint")
	}
	return fp
}

// feedNames lists the running service's feeds, in declaration order.
func feedNames(t *testing.T, client *ipc.Client) []string {
	t.Helper()
	var status map[string]any
	if err := client.Call("knowledge.status", nil, &status); err != nil {
		t.Fatal(err)
	}
	feeds, _ := status["feeds"].([]any)
	names := make([]string, 0, len(feeds))
	for _, f := range feeds {
		entry, _ := f.(map[string]any)
		name, _ := entry["name"].(string)
		names = append(names, name)
	}
	return names
}

// TestConfigGetEntryKnowledgeFeedOverSocket: the form's read — the whole
// [[knowledge.feeds]] table as the parser sees it, command included, paired
// with the same fingerprint the tab's status carries, names matched
// case-insensitively like every family.
func TestConfigGetEntryKnowledgeFeedOverSocket(t *testing.T) {
	client, _, _ := startKnowledgeFormDaemon(t)

	fp := knowledgeFingerprint(t, client)
	out := entryCall(t, client, "config.get_entry",
		map[string]any{"family": "knowledge.feeds", "name": "AMD"})
	if out["fingerprint"] != fp {
		t.Errorf("fingerprint = %v, want the status's %q", out["fingerprint"], fp)
	}
	entry, _ := out["entry"].(map[string]any)
	command, _ := entry["command"].([]any)
	if entry["name"] != "amd" || entry["description"] != "AMD share price" ||
		entry["mode"] != "lazy" || len(command) != 2 || command[0] != "/bin/echo" {
		t.Errorf("entry = %v, want the whole [[knowledge.feeds]] table, command included", entry)
	}
}

// TestConfigUpsertEntryCreateFeedOverSocket is the acceptance path for New:
// the entry is appended with every byte above it untouched, the standard
// reload hands it to the running service — the row appears in the status —
// an immediate refresh from the row works through the existing
// knowledge.refresh_now, and the activity feed names the creation.
func TestConfigUpsertEntryCreateFeedOverSocket(t *testing.T) {
	client, configFile, _ := startKnowledgeFormDaemon(t)
	original, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	fp := knowledgeFingerprint(t, client)

	out := entryCall(t, client, "config.upsert_entry", map[string]any{
		"family": "knowledge.feeds", "fingerprint": fp,
		"entry": map[string]any{"name": "nvda", "description": "NVDA share price",
			"command": []string{"/bin/echo", "900.10"}, "mode": "lazy", "ttl_sec": 600},
	})
	if out["created"] != true || out["applied"] != true {
		t.Fatalf("upsert = %v, want created and applied", out)
	}
	waitForActivityRow(t, client, "Feed created: nvda")

	raw, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.TrimRight(string(original), "\n") + "\n\n" +
		"[[knowledge.feeds]]\n" +
		"name = \"nvda\"\n" +
		"description = \"NVDA share price\"\n" +
		"command = [\"/bin/echo\", \"900.10\"]\n" +
		"mode = \"lazy\"\n" +
		"ttl_sec = 600\n"
	if string(raw) != want {
		t.Errorf("config after create:\n%s\n--- want ---\n%s", raw, want)
	}

	names := feedNames(t, client)
	if len(names) != 3 || names[2] != "nvda" {
		t.Fatalf("feeds after create = %v, want the new row listed", names)
	}
	// The row's Refresh now works immediately: the created definition fetches
	// through the exact scheduled path and the value serves from the status.
	if err := client.Call("knowledge.refresh_now", map[string]string{"name": "nvda"}, nil); err != nil {
		t.Fatalf("refreshing the created feed: %v", err)
	}
	updated := waitForEvent(t, client, "knowledge.updated")
	if updated["feed"] != "nvda" {
		t.Errorf("knowledge.updated = %v, want the created feed named", updated)
	}
	if entry := feedEntry(t, client, "nvda"); entry["value"] != "900.10" {
		t.Errorf("created feed after refresh = %v, want the fetched value", entry)
	}
}

// TestConfigUpsertEntryEditFeedOverSocket: only the edited feed's block moves
// on disk — the comments, the sibling feed, and the [tts] table are
// byte-identical — and the running service follows the standard reload.
func TestConfigUpsertEntryEditFeedOverSocket(t *testing.T) {
	client, configFile, _ := startKnowledgeFormDaemon(t)
	original, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	fp := knowledgeFingerprint(t, client)

	out := entryCall(t, client, "config.upsert_entry", map[string]any{
		"family": "knowledge.feeds", "name": "amd", "fingerprint": fp,
		"entry": map[string]any{"name": "amd", "description": "AMD share price in dollars",
			"command": []string{"/bin/echo", "187.42"}, "mode": "eager", "interval_sec": 120},
	})
	if out["created"] != false || out["applied"] != true {
		t.Fatalf("upsert = %v, want an applied edit", out)
	}
	waitForActivityRow(t, client, "Feed edited: amd")

	raw, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	oldBlock := "[[knowledge.feeds]]\nname = \"amd\"\ndescription = \"AMD share price\"\n" +
		"command = [\"/bin/echo\", \"187.42\"]\nmode = \"lazy\"\n"
	newBlock := "[[knowledge.feeds]]\nname = \"amd\"\ndescription = \"AMD share price in dollars\"\n" +
		"command = [\"/bin/echo\", \"187.42\"]\nmode = \"eager\"\ninterval_sec = 120\n"
	want := strings.Replace(string(original), oldBlock, newBlock, 1)
	if want == string(original) {
		t.Fatal("the test's oldBlock no longer matches the fixture")
	}
	if string(raw) != want {
		t.Errorf("config after edit:\n%s\n--- want ---\n%s", raw, want)
	}
	if entry := feedEntry(t, client, "amd"); entry["mode"] != "eager" ||
		entry["interval_sec"] != float64(120) {
		t.Errorf("running feed after edit = %v, want the eager cadence adopted", entry)
	}
}

// TestConfigValidateEntryFeedFieldKeyed: the loader's own feed rules land on
// exactly the form field that carries them — mode, interval_sec, command,
// and the duplicate-name collision on name — and the dry run writes nothing.
func TestConfigValidateEntryFeedFieldKeyed(t *testing.T) {
	client, configFile, _ := startKnowledgeFormDaemon(t)
	original, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}

	out := entryCall(t, client, "config.validate_entry", map[string]any{
		"family": "knowledge.feeds",
		"entry": map[string]any{"name": "nvda", "description": "NVDA",
			"command": []string{}, "mode": "hourly"},
	})
	if out["valid"] != false {
		t.Fatalf("validate = %v, want invalid", out)
	}
	problems := entryProblemList(t, out)
	if msg, ok := problemOn(problems, "mode"); !ok ||
		!strings.Contains(msg, `"hourly"`) || !strings.Contains(msg, `"eager"`) {
		t.Errorf("mode problem = %q (%v), want the loader's accepted-values wording", msg, ok)
	}
	if msg, ok := problemOn(problems, "command"); !ok ||
		!strings.Contains(msg, "command is empty") {
		t.Errorf("command problem = %q (%v), want the loader's empty-command wording", msg, ok)
	}

	// An eager cadence below the floor is keyed to its own field.
	out = entryCall(t, client, "config.validate_entry", map[string]any{
		"family": "knowledge.feeds",
		"entry": map[string]any{"name": "nvda", "description": "NVDA",
			"command": []string{"/bin/echo", "1"}, "mode": "eager", "interval_sec": 5},
	})
	if msg, ok := problemOn(entryProblemList(t, out), "interval_sec"); !ok ||
		!strings.Contains(msg, "must not refresh faster") {
		t.Errorf("interval problem = %q (%v), want the loader's floor wording", msg, ok)
	}

	// The collision case: a new feed stealing an existing name. The draft
	// compiles later, so the validator labels the draft itself and the
	// classifier keys the duplicate to the name field.
	out = entryCall(t, client, "config.validate_entry", map[string]any{
		"family": "knowledge.feeds",
		"entry": map[string]any{"name": "AMD", "description": "a second amd",
			"command": []string{"/bin/echo", "1"}, "mode": "lazy"},
	})
	if msg, ok := problemOn(entryProblemList(t, out), "name"); !ok ||
		!strings.Contains(msg, "duplicate feed name") {
		t.Errorf("duplicate problem = %q (%v), want it on the name field", msg, ok)
	}

	raw, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(original) {
		t.Error("a dry-run validate changed the file")
	}
}

// TestConfigUpsertEntryFeedInvalidWritesNothing is the half-write criterion
// for feeds, with the execution stakes attached: the refused draft's command
// is a real script with a marker, the refusal leaves the file byte-identical,
// and the marker proves the command never ran — refused or not, saving is not
// an execution path.
func TestConfigUpsertEntryFeedInvalidWritesNothing(t *testing.T) {
	client, configFile, stub := startKnowledgeFormDaemon(t)
	marker := filepath.Join(filepath.Dir(stub), "fetched.marker")
	original, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	fp := knowledgeFingerprint(t, client)

	err = client.Call("config.upsert_entry", map[string]any{
		"family": "knowledge.feeds", "fingerprint": fp,
		"entry": map[string]any{"name": "broken", "description": "will be refused",
			"command": []string{stub}, "mode": "hourly"},
	}, nil)
	var rpcErr *ipc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != ipc.CodeConfigInvalid {
		t.Fatalf("err = %v, want CodeConfigInvalid", err)
	}
	data, _ := rpcErr.Data.(map[string]any)
	if msg, ok := problemOn(entryProblemList(t, data), "mode"); !ok ||
		!strings.Contains(msg, `"hourly"`) {
		t.Errorf("problems = %v, want the mode error on its field", data)
	}
	raw, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(original) {
		t.Errorf("a refused save still changed the file:\n%s", raw)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a refused feed draft executed its command")
	}
}

// TestConfigUpsertEntrySavingFeedNeverExecutes pins the other half of the
// security criterion: a VALID feed draft saves, applies, and still runs
// nothing — a lazy feed's command waits for an actual ask, and the save
// pipeline itself contains no exec.
func TestConfigUpsertEntrySavingFeedNeverExecutes(t *testing.T) {
	client, _, stub := startKnowledgeFormDaemon(t)
	marker := filepath.Join(filepath.Dir(stub), "fetched.marker")
	fp := knowledgeFingerprint(t, client)

	out := entryCall(t, client, "config.upsert_entry", map[string]any{
		"family": "knowledge.feeds", "fingerprint": fp,
		"entry": map[string]any{"name": "stub", "description": "the marker stub",
			"command": []string{stub}, "mode": "lazy"},
	})
	if out["applied"] != true {
		t.Fatalf("upsert = %v, want it applied", out)
	}
	// The service knows the feed — the reload adopted it — yet nothing ran.
	names := feedNames(t, client)
	if len(names) != 3 || names[2] != "stub" {
		t.Fatalf("feeds after save = %v, want the stub adopted", names)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("saving a feed executed its command")
	}

	// A draft smuggling a key outside the whitelist (an env for the command)
	// is refused by shape before any validator runs (ADR 0030's stance).
	err := client.Call("config.upsert_entry", map[string]any{
		"family": "knowledge.feeds", "name": "stub",
		"entry": map[string]any{"name": "stub", "description": "the marker stub",
			"command": []string{stub}, "mode": "lazy", "env": []string{"PATH=/tmp"}},
	}, nil)
	var rpcErr *ipc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != ipc.CodeConfigInvalid {
		t.Fatalf("err = %v, want CodeConfigInvalid", err)
	}
	data, _ := rpcErr.Data.(map[string]any)
	if msg, ok := problemOn(entryProblemList(t, data), "env"); !ok ||
		!strings.Contains(msg, `"env"`) {
		t.Errorf("problems = %v, want the refused key named on its own field", data)
	}
}

// TestConfigUpsertEntryFeedRefusesStaleFingerprint: a hand edit made after
// the form opened is never clobbered.
func TestConfigUpsertEntryFeedRefusesStaleFingerprint(t *testing.T) {
	client, configFile, _ := startKnowledgeFormDaemon(t)
	fp := knowledgeFingerprint(t, client)

	original, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	edited := string(original) + "\n# a hand note made while the form sat open\n"
	if err := os.WriteFile(configFile, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}

	err = client.Call("config.upsert_entry", map[string]any{
		"family": "knowledge.feeds", "name": "amd", "fingerprint": fp,
		"entry": map[string]any{"name": "amd", "description": "AMD share price",
			"command": []string{"/bin/echo", "187.42"}, "mode": "lazy"},
	}, nil)
	var rpcErr *ipc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != ipc.CodeConfigConflict ||
		!strings.Contains(rpcErr.Message, "outside the window") {
		t.Fatalf("err = %v, want CodeConfigConflict wording the outside edit", err)
	}
	raw, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != edited {
		t.Errorf("the hand edit was clobbered:\n%s", raw)
	}
}

// TestConfigDeleteEntryFeedOverSocket is the acceptance path for Delete: the
// feed's block and its glued comment go byte-preservingly, and its cached
// value no longer serves — the status drops the row, a refresh refuses by
// name — while the sibling keeps its own cache.
func TestConfigDeleteEntryFeedOverSocket(t *testing.T) {
	client, configFile, _ := startKnowledgeFormDaemon(t)
	original, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}

	// Give the doomed feed a cached value first, so the delete provably
	// stops it serving rather than there never having been one.
	if err := client.Call("knowledge.refresh_now", map[string]string{"name": "amd"}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "knowledge.updated")
	if entry := feedEntry(t, client, "amd"); entry["value"] != "187.42" {
		t.Fatalf("feed before delete = %v, want a cached value", entry)
	}

	fp := knowledgeFingerprint(t, client)
	out := entryCall(t, client, "config.delete_entry",
		map[string]any{"family": "knowledge.feeds", "name": "amd", "fingerprint": fp})
	if out["applied"] != true {
		t.Fatalf("delete = %v, want it applied", out)
	}
	waitForActivityRow(t, client, "Feed deleted: amd")

	raw, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	removed := "# watches the AMD price\n[[knowledge.feeds]]\nname = \"amd\"\n" +
		"description = \"AMD share price\"\ncommand = [\"/bin/echo\", \"187.42\"]\nmode = \"lazy\"\n\n"
	want := strings.Replace(string(original), removed, "", 1)
	if want == string(original) {
		t.Fatal("the test's removed block no longer matches the fixture")
	}
	if string(raw) != want {
		t.Errorf("config after delete:\n%s\n--- want ---\n%s", raw, want)
	}

	names := feedNames(t, client)
	if len(names) != 1 || names[0] != "weather" {
		t.Fatalf("feeds after delete = %v, want only the sibling", names)
	}
	err = client.Call("knowledge.refresh_now", map[string]string{"name": "amd"}, nil)
	if err == nil || !strings.Contains(err.Error(), `"amd"`) {
		t.Errorf("refresh after delete = %v, want the unknown-feed refusal", err)
	}
}

// TestConfigUpsertEntryFirstFeedNeedsRestart pins the restart-class boundary
// honestly (ADR 0031): with zero feeds at boot there is no service to adopt
// the first one, so the save lands in the file but reports applied=false
// with the restart reason — never a row that pretends it is fetching.
func TestConfigUpsertEntryFirstFeedNeedsRestart(t *testing.T) {
	client, paths := startAdminDaemon(t, `# my config, hand-written
[context]
window = false
`)
	out := entryCall(t, client, "config.upsert_entry", map[string]any{
		"family": "knowledge.feeds",
		"entry": map[string]any{"name": "amd", "description": "AMD share price",
			"command": []string{"/bin/echo", "187.42"}, "mode": "lazy"},
	})
	if out["created"] != true || out["applied"] != false {
		t.Fatalf("upsert = %v, want written but not applied", out)
	}
	reason, _ := out["reason"].(string)
	if !strings.Contains(reason, "restart") {
		t.Errorf("reason = %q, want the restart pointer", reason)
	}
	raw, err := os.ReadFile(paths.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "[[knowledge.feeds]]") {
		t.Error("the first feed was not written")
	}
}
