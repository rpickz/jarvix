package daemon

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/ipc"
)

// The two families #164 moved into the window, over a fully wired daemon and a
// real socket: `[[intents.custom]]`, the only array family whose identity is
// not a `name`, and `[tts.lexicon]`, the first of the third document shape.
//
// The claim being tested is the architectural one — neither family added a
// verb, a handler, or a write path — so these tests call the SAME six verbs the
// routines form calls, and the things they check are the things a registry row
// is supposed to buy: field-keyed problems, byte preservation, the fingerprint
// guard, and nothing written on a refusal.

// holdoutsTOML is the hand-written config these tests boot from and edit. The
// comments, the inline comment beside a lexicon line, and the sibling entries
// are the byte-preservation fixture.
const holdoutsTOML = `# my config, hand-written
[context]
window = false
selection = false
clipboard = false

[intents]
terminal = "alacritty"

# lock it when I walk away
[[intents.custom]]
match = "lock the screen"
run = "hyprlock"
say = "Locking."

[tts]
provider = "kokoro"

# how it should say the words it gets wrong
[tts.lexicon]
Kubernetes = "koo ber net eez"   # kokoro says koo-ber-NEET-es
k9s = "kay nine ess"

# the evening wind-down
[[routines]]
name = "evening"
phrases = ["evening mode"]

  [[routines.steps]]
  app = "mpv"
  workspace = 5
`

// listEntryNames reads one family's listing and returns each entry's identity
// value under the given key — the listing verb is registry-driven, so this is
// the same call for both families.
func listEntryNames(t *testing.T, client *ipc.Client, family, idKey string) (string, []string) {
	t.Helper()
	out := entryCall(t, client, "config.list_entries", map[string]any{"family": family})
	fingerprint, _ := out["fingerprint"].(string)
	rows, _ := out["entries"].([]any)
	names := make([]string, 0, len(rows))
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		entry, _ := row["entry"].(map[string]any)
		name, _ := entry[idKey].(string)
		names = append(names, name)
	}
	return fingerprint, names
}

// TestCustomIntentFormOverSocket is the acceptance path for a spoken command:
// create, edit and delete through the generic verbs, with the phrase field
// carrying the router's own verdict at every step.
func TestCustomIntentFormOverSocket(t *testing.T) {
	client, paths := startAdminDaemon(t, holdoutsTOML)
	before, err := os.ReadFile(paths.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}

	fingerprint, names := listEntryNames(t, client, "intents.custom", "match")
	if len(names) != 1 || names[0] != "lock the screen" {
		t.Fatalf("listing = %v, want the hand-written entry", names)
	}

	// A phrase the built-in table owns is refused ON THE PHRASE FIELD, naming
	// the owner — the router's own message, not a second copy of the rule.
	var rpcErr *ipc.Error
	err = client.Call("config.upsert_entry", map[string]any{
		"family": "intents.custom", "fingerprint": fingerprint,
		"entry": map[string]any{"match": "mute", "run": "playerctl pause"},
	}, nil)
	if !errors.As(err, &rpcErr) || rpcErr.Code != ipc.CodeConfigInvalid {
		t.Fatalf("a colliding phrase was accepted: %v", err)
	}
	problems := entryProblemList(t, rpcErr.Data.(map[string]any))
	msg, ok := problemOn(problems, "match")
	if !ok || !strings.Contains(msg, "the built-in intent") {
		t.Errorf("problems = %v, want the owner named on the match field", problems)
	}
	if after, _ := os.ReadFile(paths.ConfigFile()); string(after) != string(before) {
		t.Error("a refused save touched the file")
	}

	// A phrase another custom intent owns is refused the same way — the hole
	// #164 closed in the router, reaching the form that would have opened it.
	err = client.Call("config.upsert_entry", map[string]any{
		"family": "intents.custom", "fingerprint": fingerprint,
		"entry": map[string]any{"match": "Lock The Screen", "run": "true"},
	}, nil)
	if !errors.As(err, &rpcErr) || rpcErr.Code != ipc.CodeConfigInvalid {
		t.Fatalf("a duplicate phrase was accepted: %v", err)
	}
	problems = entryProblemList(t, rpcErr.Data.(map[string]any))
	if msg, ok := problemOn(problems, "match"); !ok ||
		!strings.Contains(msg, `intents.custom[0] ("lock the screen")`) {
		t.Errorf("problems = %v, want the owning entry named", problems)
	}

	// A good one lands, byte-preservingly.
	out := entryCall(t, client, "config.upsert_entry", map[string]any{
		"family": "intents.custom", "fingerprint": fingerprint,
		"entry": map[string]any{"match": "mute the music", "run": "playerctl pause"},
	})
	if out["created"] != true {
		t.Errorf("receipt = %v, want created", out)
	}
	raw, err := os.ReadFile(paths.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "# lock it when I walk away") ||
		!strings.Contains(string(raw), `run = "playerctl pause"`) {
		t.Errorf("file after create:\n%s", raw)
	}

	// Read one back whole, edit it, and read the edit back.
	got := entryCall(t, client, "config.get_entry",
		map[string]any{"family": "intents.custom", "name": "lock the screen"})
	entry, _ := got["entry"].(map[string]any)
	if entry["say"] != "Locking." || entry["run"] != "hyprlock" {
		t.Errorf("entry = %v, want the whole table", entry)
	}
	fingerprint, _ = got["fingerprint"].(string)
	entryCall(t, client, "config.upsert_entry", map[string]any{
		"family": "intents.custom", "name": "lock the screen", "fingerprint": fingerprint,
		"entry": map[string]any{"match": "lock the screen", "run": "hyprlock --immediate",
			"say": "Locking."},
	})
	got = entryCall(t, client, "config.get_entry",
		map[string]any{"family": "intents.custom", "name": "lock the screen"})
	entry, _ = got["entry"].(map[string]any)
	if entry["run"] != "hyprlock --immediate" {
		t.Errorf("entry after edit = %v", entry)
	}

	// And delete, leaving the sibling and the routine alone.
	fingerprint, _ = got["fingerprint"].(string)
	entryCall(t, client, "config.delete_entry", map[string]any{
		"family": "intents.custom", "name": "lock the screen", "fingerprint": fingerprint,
	})
	_, names = listEntryNames(t, client, "intents.custom", "match")
	if len(names) != 1 || names[0] != "mute the music" {
		t.Errorf("listing after delete = %v", names)
	}
	raw, _ = os.ReadFile(paths.ConfigFile())
	if !strings.Contains(string(raw), `name = "evening"`) {
		t.Errorf("the delete took a sibling with it:\n%s", raw)
	}
}

// TestCustomIntentGrammarRecompilesOnTheStandardReload: the acceptance
// criterion that the phrase actually WORKS afterwards.
//
// Saving an entry is not the claim — the claim is that the running router
// learns it without a restart. So this saves one whose command touches a marker
// file, then submits the phrase as typed text, and waits for the marker: the
// intent router, the permission gate, and the session are all the ordinary
// ones, which is the whole point.
func TestCustomIntentGrammarRecompilesOnTheStandardReload(t *testing.T) {
	dir := t.TempDir()
	marker := dir + "/spoken.marker"
	client, _ := startAdminDaemon(t, holdoutsTOML+`
[tools.policy]
default = "allow"
`)
	fingerprint, _ := listEntryNames(t, client, "intents.custom", "match")
	entryCall(t, client, "config.upsert_entry", map[string]any{
		"family": "intents.custom", "fingerprint": fingerprint,
		"entry": map[string]any{
			"match": "make the marker", "run": "touch " + marker, "say": "Marked."},
	})

	if err := client.Call("session.start", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := client.Call("session.submit", map[string]any{"text": "make the marker"}, nil); err != nil {
		t.Fatal(err)
	}
	// The session's end is the synchronisation: the intent runs inside it, and
	// the marker is written before the turn finishes.
	waitForEvent(t, client, "session.finished")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the new phrase did not reach the router without a restart: %v", err)
	}
}

// TestLexiconFormOverSocket is the acceptance path for a pronunciation: the
// third document shape through the same six verbs, byte-preserving down to the
// inline comment beside the line it edits.
func TestLexiconFormOverSocket(t *testing.T) {
	client, paths := startAdminDaemon(t, holdoutsTOML)

	fingerprint, names := listEntryNames(t, client, "tts.lexicon", "name")
	if strings.Join(names, ",") != "Kubernetes,k9s" {
		t.Fatalf("listing = %v, want the two hand-written entries", names)
	}

	// Create.
	entryCall(t, client, "config.upsert_entry", map[string]any{
		"family": "tts.lexicon", "fingerprint": fingerprint,
		"entry": map[string]any{"name": "Hyprland", "spoken": "hyper land"},
	})
	raw, err := os.ReadFile(paths.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `Hyprland = "hyper land"`) ||
		!strings.Contains(string(raw), "# kokoro says koo-ber-NEET-es") {
		t.Errorf("file after create:\n%s", raw)
	}

	// Read one back — the value arrives under the wire key the family declares,
	// never as a bare map the form would have to guess the shape of.
	got := entryCall(t, client, "config.get_entry",
		map[string]any{"family": "tts.lexicon", "name": "Kubernetes"})
	entry, _ := got["entry"].(map[string]any)
	if entry["name"] != "Kubernetes" || entry["spoken"] != "koo ber net eez" {
		t.Errorf("entry = %v", entry)
	}

	// Edit in place: the inline comment survives, because for a one-line entry
	// it is the only place the entry can be documented at all.
	fingerprint, _ = got["fingerprint"].(string)
	entryCall(t, client, "config.upsert_entry", map[string]any{
		"family": "tts.lexicon", "name": "Kubernetes", "fingerprint": fingerprint,
		"entry": map[string]any{"name": "Kubernetes", "spoken": "koober net ees"},
	})
	raw, _ = os.ReadFile(paths.ConfigFile())
	if !strings.Contains(string(raw), `Kubernetes = "koober net ees"   # kokoro says koo-ber-NEET-es`) {
		t.Errorf("the inline comment did not survive the edit:\n%s", raw)
	}

	// An empty spoken form is refused on the field that holds it, and nothing
	// is written.
	before := string(raw)
	fingerprint, _ = listEntryNames(t, client, "tts.lexicon", "name")
	var rpcErr *ipc.Error
	err = client.Call("config.upsert_entry", map[string]any{
		"family": "tts.lexicon", "name": "k9s", "fingerprint": fingerprint,
		"entry": map[string]any{"name": "k9s", "spoken": "  "},
	}, nil)
	if !errors.As(err, &rpcErr) || rpcErr.Code != ipc.CodeConfigInvalid {
		t.Fatalf("an empty spoken form was accepted: %v", err)
	}
	if msg, ok := problemOn(entryProblemList(t, rpcErr.Data.(map[string]any)), "spoken"); !ok ||
		!strings.Contains(msg, "spoken form is empty") {
		t.Errorf("problems = %v, want one on the spoken field", rpcErr.Data)
	}
	if after, _ := os.ReadFile(paths.ConfigFile()); string(after) != before {
		t.Error("a refused save touched the file")
	}

	// A written form that is already taken is refused on the name field rather
	// than producing a document TOML cannot parse.
	err = client.Call("config.upsert_entry", map[string]any{
		"family": "tts.lexicon", "fingerprint": fingerprint,
		"entry": map[string]any{"name": "k9s", "spoken": "kay nines"},
	}, nil)
	if !errors.As(err, &rpcErr) || rpcErr.Code != ipc.CodeConfigInvalid {
		t.Fatalf("a duplicate written form was accepted: %v", err)
	}
	if msg, ok := problemOn(entryProblemList(t, rpcErr.Data.(map[string]any)), "name"); !ok ||
		!strings.Contains(msg, "already has an entry") {
		t.Errorf("problems = %v, want one on the name field", rpcErr.Data)
	}

	// Delete.
	entryCall(t, client, "config.delete_entry", map[string]any{
		"family": "tts.lexicon", "name": "k9s", "fingerprint": fingerprint,
	})
	_, names = listEntryNames(t, client, "tts.lexicon", "name")
	if strings.Join(names, ",") != "Hyprland,Kubernetes" {
		t.Errorf("listing after delete = %v", names)
	}
}

// TestLexiconWarnsAboutAnOrdinaryWord: the note the ticket asks for.
//
// It is a NOTE, not a problem: the entry is legal and the user may well mean
// it, so the form says what will happen and saves it anyway. Which is also why
// it is checked here on config.validate_entry — the dry run the form calls
// while typing, before anything is written.
func TestLexiconWarnsAboutAnOrdinaryWord(t *testing.T) {
	client, _ := startAdminDaemon(t, holdoutsTOML)

	out := entryCall(t, client, "config.validate_entry", map[string]any{
		"family": "tts.lexicon",
		"entry":  map[string]any{"name": "read", "spoken": "reed"},
	})
	if out["valid"] != true {
		t.Errorf("an ordinary word was refused rather than flagged: %v", out)
	}
	notes, _ := out["notes"].([]any)
	if len(notes) != 1 {
		t.Fatalf("notes = %v, want one warning", out["notes"])
	}
	note, _ := notes[0].(map[string]any)
	msg, _ := note["message"].(string)
	if note["field"] != "name" || !strings.Contains(msg, "ordinary English word") {
		t.Errorf("note = %v, want it keyed to the written form", note)
	}

	// A technical term — the vocabulary the feature exists for — is not warned
	// about. A warning that fires on the normal case is a warning people learn
	// to click past.
	out = entryCall(t, client, "config.validate_entry", map[string]any{
		"family": "tts.lexicon",
		"entry":  map[string]any{"name": "Hyprland", "spoken": "hyper land"},
	})
	if notes, _ := out["notes"].([]any); len(notes) != 0 {
		t.Errorf("notes for a technical term = %v, want none", notes)
	}
}

// TestHoldoutFamiliesRefuseAStaleFingerprint: the external-edit guard, on both
// new families, because it is the pipeline's and not the family's.
func TestHoldoutFamiliesRefuseAStaleFingerprint(t *testing.T) {
	client, paths := startAdminDaemon(t, holdoutsTOML)
	before, err := os.ReadFile(paths.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		family string
		entry  map[string]any
	}{
		{"intents.custom", map[string]any{"match": "mute the music", "run": "playerctl pause"}},
		{"tts.lexicon", map[string]any{"name": "Hyprland", "spoken": "hyper land"}},
	} {
		var rpcErr *ipc.Error
		err := client.Call("config.upsert_entry", map[string]any{
			"family": tc.family, "fingerprint": "sha256:not-what-is-on-disk",
			"entry": tc.entry,
		}, nil)
		if !errors.As(err, &rpcErr) || rpcErr.Code != ipc.CodeConfigConflict {
			t.Errorf("%s: stale fingerprint accepted: %v", tc.family, err)
		}
	}
	if after, _ := os.ReadFile(paths.ConfigFile()); string(after) != string(before) {
		t.Error("a refused save touched the file")
	}
}

// TestTheAssistantCannotReachTheHoldoutFamiliesOrTools is the exclusion wall,
// pinned where #164 could have breached it.
//
// Three separate claims, all of them structural rather than policy:
//
//   - The assistant's entry surface still holds exactly the three families it
//     held before, so a spoken command — which runs a shell command — cannot be
//     written by the model, and the lexicon's per-entry route stays the
//     window's.
//   - `[tools]` is not an entry family at all, for anyone. The permission gate
//     is administered on its own screen with its own refusal matrix, and adding
//     two families to the registry did not make a third one addressable.
//   - `[tools.policy]` is still excluded from the assistant's SETTINGS, which
//     is the half of the wall #109 built and ADR 0053 leaned on.
func TestTheAssistantCannotReachTheHoldoutFamiliesOrTools(t *testing.T) {
	for _, family := range []string{"intents.custom", "tts.lexicon"} {
		spec, ipcErr := assistantEntryFamily(family)
		if ipcErr == nil {
			t.Errorf("the assistant can reach %q: %+v", family, spec)
			continue
		}
		if !strings.Contains(ipcErr.Message, "window") {
			t.Errorf("%s refusal %q should say where the family IS edited", family, ipcErr.Message)
		}
	}
	for _, family := range []string{"tools", "tools.policy"} {
		if _, ipcErr := entryFamily(family); ipcErr == nil {
			t.Errorf("%q became an entry family — the permission gate is not a form", family)
		}
	}
	if _, excluded := config.AssistantExcludedSettingReason("tools.policy.shell_allow"); !excluded {
		t.Error("tools.policy.shell_allow is reachable from the assistant's settings")
	}
	if _, excluded := config.AssistantExcludedSettingReason("tools.policy.shell_deny"); !excluded {
		t.Error("tools.policy.shell_deny is reachable from the assistant's settings")
	}
}
