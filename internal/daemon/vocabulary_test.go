package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/stt/whispercpp"
	"github.com/rpickz/jarvix/internal/vocabulary"
)

// The taught vocabulary (issue #129) over a fully wired daemon: the form
// verbs round-trip through the store's own path, refusals arrive in the
// entry-form wire shape with nothing written, the Delete button routes
// through the gate naming the exact phrase, the voice seam's sentences are
// pinned, and — the STT half — a flagged phrase reaches the bias prompt
// immediately, no reload.

// vocabularyEntries lists the store over the socket.
func vocabularyEntries(t *testing.T, client *ipc.Client) []any {
	t.Helper()
	var listing map[string]any
	if err := client.Call("vocabulary.list", nil, &listing); err != nil {
		t.Fatal(err)
	}
	entries, _ := listing["entries"].([]any)
	return entries
}

// TestVocabularyTeachOverSocket: the acceptance path for the form's Add —
// the entry lands on disk with an id, the listing and the file both carry
// it, a re-teach supersedes in place, and the activity row names the save
// by id with the content's size, never its words.
func TestVocabularyTeachOverSocket(t *testing.T) {
	client, _, dir := startMemoryDaemon(t, nil)

	var out map[string]any
	if err := client.Call("vocabulary.teach", map[string]any{
		"phrase": "  quid ", "meaning": " pounds ", "note": "UK money slang"}, &out); err != nil {
		t.Fatal(err)
	}
	entry, _ := out["entry"].(map[string]any)
	if entry["id"] != "w1" || entry["phrase"] != "quid" || entry["meaning"] != "pounds" {
		t.Fatalf("teach = %v, want the trimmed entry with its first id", out)
	}
	waitForActivityRow(t, client, "Word taught: w1")

	raw, err := os.ReadFile(filepath.Join(dir, "vocabulary.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `phrase = "quid"`) ||
		!strings.Contains(string(raw), `meaning = "pounds"`) {
		t.Errorf("vocabulary.toml after teach:\n%s", raw)
	}

	// A repeated phrase supersedes — never a second entry — and the reply
	// carries the trail.
	if err := client.Call("vocabulary.teach", map[string]any{
		"phrase": "Quid", "meaning": "euros"}, &out); err != nil {
		t.Fatal(err)
	}
	entries := vocabularyEntries(t, client)
	if len(entries) != 1 {
		t.Fatalf("listing after re-teach = %v, want one superseded entry", entries)
	}
	listed, _ := entries[0].(map[string]any)
	if listed["meaning"] != "euros" {
		t.Errorf("entry after re-teach = %v", listed)
	}
	previous, _ := listed["previous"].([]any)
	if len(previous) != 1 {
		t.Errorf("trail after re-teach = %v, want the old meaning kept", listed)
	}
}

// TestVocabularyTeachRefusalsAreFieldKeyed: the entry-form wire shape, file
// untouched.
func TestVocabularyTeachRefusalsAreFieldKeyed(t *testing.T) {
	client, _, dir := startMemoryDaemon(t, nil)

	err := client.Call("vocabulary.teach", map[string]any{"phrase": "quid", "meaning": "  "}, nil)
	var rpcErr *ipc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != ipc.CodeConfigInvalid {
		t.Fatalf("empty meaning error = %v, want -32001 with problems", err)
	}
	data, _ := rpcErr.Data.(map[string]any)
	problems, _ := data["problems"].([]any)
	if len(problems) != 1 {
		t.Fatalf("problems = %v", data)
	}
	if problem, _ := problems[0].(map[string]any); problem["field"] != "meaning" {
		t.Errorf("problem = %v, want it keyed to the meaning field", problem)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "vocabulary.toml")); !os.IsNotExist(statErr) {
		t.Error("a refused teach touched the store file")
	}
}

// TestVocabularyUpdateRenameCollision: renaming onto another taught phrase
// is refused in the same wire shape, nothing written.
func TestVocabularyUpdateRenameCollision(t *testing.T) {
	client, _, _ := startMemoryDaemon(t, nil)
	mustTeachOverSocket(t, client, "quid", "pounds")
	mustTeachOverSocket(t, client, "telly", "television")

	err := client.Call("vocabulary.update", map[string]any{
		"id": "w2", "phrase": "quid", "meaning": "television"}, nil)
	var rpcErr *ipc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != ipc.CodeConfigInvalid ||
		!strings.Contains(rpcErr.Message, "nothing was written") {
		t.Fatalf("rename collision error = %v, want the -32001 refusal", err)
	}
	entries := vocabularyEntries(t, client)
	if len(entries) != 2 {
		t.Fatalf("listing after refused rename = %v", entries)
	}
}

// TestVocabularyFlagCapOverSocket: the bias cap refuses the flag loudly —
// the entry itself is saved, the error says so, and the listing's bias
// counters expose the budget.
func TestVocabularyFlagCapOverSocket(t *testing.T) {
	client, _, _ := startMemoryDaemon(t, nil)
	for i := 0; i < vocabulary.MaxHardToHear; i++ {
		var out map[string]any
		if err := client.Call("vocabulary.teach", map[string]any{
			"phrase": fmt.Sprintf("word%d", i), "meaning": "m", "hard_to_hear": true}, &out); err != nil {
			t.Fatalf("flagged teach %d: %v", i, err)
		}
	}

	err := client.Call("vocabulary.teach", map[string]any{
		"phrase": "quid", "meaning": "pounds", "hard_to_hear": true}, nil)
	var rpcErr *ipc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != ipc.CodeConfigInvalid ||
		!strings.Contains(rpcErr.Message, "saved as w21") ||
		!strings.Contains(rpcErr.Message, "not be listened for") {
		t.Fatalf("flag-at-cap error = %v, want the saved-but-not-flagged refusal", err)
	}

	var listing map[string]any
	if err := client.Call("vocabulary.list", nil, &listing); err != nil {
		t.Fatal(err)
	}
	if listing["bias_count"] != float64(vocabulary.MaxHardToHear) ||
		listing["bias_max"] != float64(vocabulary.MaxHardToHear) {
		t.Errorf("bias counters = %v/%v, want the full budget visible",
			listing["bias_count"], listing["bias_max"])
	}
	if len(vocabularyEntries(t, client)) != vocabulary.MaxHardToHear+1 {
		t.Error("the entry itself was not saved despite the refused flag")
	}
}

// TestVocabularyForgetGatedApproveDeletes: the Delete button's full round
// trip — the card names the exact phrase, resolved from the store, and only
// the approval deletes.
func TestVocabularyForgetGatedApproveDeletes(t *testing.T) {
	client, provider, _ := startMemoryDaemon(t, nil)
	mustTeachOverSocket(t, client, "quid", "pounds")

	var res map[string]any
	if err := client.Call("vocabulary.forget_gated", map[string]string{"id": "w1"}, &res); err != nil {
		t.Fatal(err)
	}
	if res["session_id"] == "" {
		t.Fatalf("reply = %v, want the session id the card belongs to", res)
	}
	required := waitForEvent(t, client, "tool.confirmation_required")
	if required["tool"] != "vocabulary.forget" {
		t.Errorf("tool = %v, want the vocabulary.forget identity", required["tool"])
	}
	command, _ := required["command"].(string)
	if !strings.Contains(command, "w1") || !strings.Contains(command, "quid") {
		t.Errorf("command = %q, want the exact entry named", command)
	}
	if err := client.Call("session.confirm", map[string]bool{"approved": true}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "tool.finished")
	waitForEvent(t, client, "session.finished")

	if entries := vocabularyEntries(t, client); len(entries) != 0 {
		t.Fatalf("entries after approved forget = %v", entries)
	}
	if len(provider.Requests) != 0 {
		t.Errorf("the provider was called %d times; the button is not a model turn", len(provider.Requests))
	}
}

// TestVocabularyForgetGatedDeclineKeeps: a decline deletes nothing.
func TestVocabularyForgetGatedDeclineKeeps(t *testing.T) {
	client, _, _ := startMemoryDaemon(t, nil)
	mustTeachOverSocket(t, client, "quid", "pounds")

	if err := client.Call("vocabulary.forget_gated", map[string]string{"id": "w1"}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "tool.confirmation_required")
	if err := client.Call("session.confirm", map[string]bool{"approved": false}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "tool.declined")
	waitForEvent(t, client, "session.finished")

	if entries := vocabularyEntries(t, client); len(entries) != 1 {
		t.Errorf("entries after decline = %v, want the phrase kept", entries)
	}
}

// TestVocabularyDisabledAnswersHonestly: switched off, the read answers
// enabled=false and the writes refuse naming the switch.
func TestVocabularyDisabledAnswersHonestly(t *testing.T) {
	client, _, _ := startMemoryDaemon(t, func(cfg *config.Config) {
		cfg.Vocabulary.Enabled = false
	})
	var listing map[string]any
	if err := client.Call("vocabulary.list", nil, &listing); err != nil {
		t.Fatal(err)
	}
	if listing["enabled"] != false {
		t.Errorf("listing = %v, want enabled false", listing)
	}
	err := client.Call("vocabulary.teach", map[string]any{"phrase": "quid", "meaning": "pounds"}, nil)
	if err == nil || !strings.Contains(err.Error(), "vocabulary.enabled") {
		t.Errorf("teach on a disabled daemon = %v, want the refusal naming the switch", err)
	}
}

// TestTaughtHardToHearPhraseReachesTheBiasPromptLive is the STT half of
// #129, end to end at the seam: the transcriber's prompt is read per
// request, so a phrase flagged NOW is biased on the NEXT utterance — no
// reload, no restart — through the one composed sentence (#107).
func TestTaughtHardToHearPhraseReachesTheBiasPromptLive(t *testing.T) {
	cfg := config.Default()
	cfg.Assistant.Name = "Hal"
	dir := t.TempDir()
	store := vocabulary.NewStore(filepath.Join(dir, "vocabulary.toml"), vocabulary.StoreOptions{}, nil)

	deps, _, err := fillDeps(cfg, testPaths(t), Deps{}, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	server, ok := deps.Transcriber.(*whispercpp.ServerTranscriber)
	if !ok {
		t.Fatalf("transcriber = %T", deps.Transcriber)
	}
	if got := server.PromptFunc(); got != "The assistant is called Hal." {
		t.Fatalf("prompt before any teach = %q", got)
	}

	entry, _, err := store.Teach("quid", "pounds", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.SetHardToHear(entry.ID, true); err != nil {
		t.Fatal(err)
	}
	want := "The assistant is called Hal. Conversations may mention: quid."
	if got := server.PromptFunc(); got != want {
		t.Errorf("prompt after flagging = %q, want %q — the flag must land without a reload", got, want)
	}
	if got := server.Cold.PromptFunc(); got != want {
		t.Errorf("cold fallback prompt = %q, want %q — both paths must bias identically", got, want)
	}
}

// --- the voice seam's sentences ------------------------------------------

func testVoiceSeam(t *testing.T) (*vocabularyVoice, *vocabulary.Store) {
	t.Helper()
	store := vocabulary.NewStore(filepath.Join(t.TempDir(), "vocabulary.toml"),
		vocabulary.StoreOptions{}, nil)
	return &vocabularyVoice{store: store, publish: func(string, vocabulary.Entry) {}}, store
}

// TestVoiceTeachSpeaksSoftConfirmation pins the spoken sentences: a teach
// confirms both halves, a re-teach names what changed, and the store ends up
// holding exactly one entry.
func TestVoiceTeachSpeaksSoftConfirmation(t *testing.T) {
	seam, store := testVoiceSeam(t)
	spoken, err := seam.TeachEntry("quid", "pounds", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if spoken != "Okay — quid means pounds." {
		t.Errorf("spoken = %q", spoken)
	}
	spoken, err = seam.TeachEntry("quid", "euros", "s2")
	if err != nil {
		t.Fatal(err)
	}
	if spoken != "Okay — quid now means euros; it used to mean pounds." {
		t.Errorf("re-teach spoken = %q", spoken)
	}
	if entries := store.List(""); len(entries) != 1 {
		t.Fatalf("voice re-teach accumulated: %+v", entries)
	}
}

// TestVoiceListenRequiresATaughtPhrase: flagging an untaught word refuses
// with the way forward; flagging a taught one confirms; twice is idempotent.
func TestVoiceListenRequiresATaughtPhrase(t *testing.T) {
	seam, _ := testVoiceSeam(t)
	if _, err := seam.ListenFor("quid"); err == nil ||
		!strings.Contains(err.Error(), "teach it first") {
		t.Errorf("listen for an untaught word err = %v, want the teach-first refusal", err)
	}
	if _, err := seam.TeachEntry("quid", "pounds", ""); err != nil {
		t.Fatal(err)
	}
	spoken, err := seam.ListenFor("Quid")
	if err != nil {
		t.Fatal(err)
	}
	if spoken != "I will listen for quid." {
		t.Errorf("spoken = %q", spoken)
	}
	spoken, err = seam.ListenFor("quid")
	if err != nil {
		t.Fatal(err)
	}
	if spoken != "I am already listening for quid." {
		t.Errorf("repeat spoken = %q", spoken)
	}
}

// TestVoiceListingIsShortAndCapped: empty teaches an example, a few entries
// list in full, and a long vocabulary is summarised with the count — the ear
// never gets two hundred entries.
func TestVoiceListingIsShortAndCapped(t *testing.T) {
	seam, _ := testVoiceSeam(t)
	spoken, err := seam.SpokenListing()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(spoken, "not taught me any words yet") {
		t.Errorf("empty listing = %q", spoken)
	}

	for i := 0; i < spokenListingCap+4; i++ {
		if _, err := seam.TeachEntry(fmt.Sprintf("word%d", i), "meaning", ""); err != nil {
			t.Fatal(err)
		}
	}
	spoken, err = seam.SpokenListing()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(spoken, fmt.Sprintf("%d words", spokenListingCap+4)) ||
		!strings.Contains(spoken, "The full list is in the window") {
		t.Errorf("long listing = %q, want the count and the pointer to the window", spoken)
	}
	if got := strings.Count(spoken, "means"); got != spokenListingCap {
		t.Errorf("long listing reads %d entries aloud, want the cap %d", got, spokenListingCap)
	}
}

func mustTeachOverSocket(t *testing.T, client *ipc.Client, phrase, meaning string) {
	t.Helper()
	if err := client.Call("vocabulary.teach",
		map[string]any{"phrase": phrase, "meaning": meaning}, nil); err != nil {
		t.Fatal(err)
	}
}
