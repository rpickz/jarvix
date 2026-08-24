package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/ipc"
)

// The Memory tab's write surface (issue #100) over a fully wired daemon:
// memory.add and memory.update going through the book's own path — the same
// discipline the memory.remember tool writes with — never through the config
// entry editor, because memory.toml is not config.toml. Pinned on the
// socket: the round trip to disk (the file is the store, and a reopened
// reader must see exactly what the form saved), the supersede trail an edit
// leaves, refusals in the entry form's field-keyed wire shape with the file
// untouched, and the activity rows that name each save by id — content never
// in an event.

// memoryFileBytes reads the store file, or "" while it does not exist.
func memoryFileBytes(t *testing.T, dir string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "memory.toml"))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestMemoryAddOverSocket is the acceptance path for Add: the fact lands in
// the store through the book (id assigned, timestamps set), the tab's
// listing and the file on disk both carry it — the round trip — and the
// activity feed names the save by id with the content's size, never its
// words.
func TestMemoryAddOverSocket(t *testing.T) {
	client, _, dir := startMemoryDaemon(t, nil)

	var out map[string]any
	if err := client.Call("memory.add",
		map[string]any{"content": "  the staging server is called atlas  "}, &out); err != nil {
		t.Fatal(err)
	}
	fact, _ := out["fact"].(map[string]any)
	if fact["id"] != "m1" || fact["content"] != "the staging server is called atlas" {
		t.Fatalf("add = %v, want the trimmed fact with its first id", out)
	}
	if _, warned := out["warning"]; warned {
		t.Errorf("add = %v, want no near-cap warning with a near-empty store", out)
	}
	waitForActivityRow(t, client, "Fact added: m1")

	facts := memoryFacts(t, client)
	if len(facts) != 1 {
		t.Fatalf("listing after add = %v, want the stored fact", facts)
	}
	if listed, _ := facts[0].(map[string]any); listed["content"] != "the staging server is called atlas" {
		t.Fatalf("listing after add = %v, want the stored fact", facts)
	}
	// The disk round trip: the file is the store, and what the form saved
	// must be readable from it alone (the user's hand-edit contract).
	raw := memoryFileBytes(t, dir)
	if !strings.Contains(raw, `content = "the staging server is called atlas"`) ||
		!strings.Contains(raw, `id = "m1"`) {
		t.Errorf("memory.toml after add:\n%s", raw)
	}
}

// TestMemoryUpdateOverSocket is the acceptance path for Edit: the book
// supersedes — the new text serves, the old value moves onto the trail with
// both timestamps, all of it on disk — and the activity row names the edit.
func TestMemoryUpdateOverSocket(t *testing.T) {
	client, _, dir := startMemoryDaemon(t, nil)
	seedMemoryFile(t, dir)

	var out map[string]any
	if err := client.Call("memory.update",
		map[string]any{"id": "m1", "content": "the staging server is called helios"}, &out); err != nil {
		t.Fatal(err)
	}
	fact, _ := out["fact"].(map[string]any)
	previous, _ := fact["previous"].([]any)
	if fact["id"] != "m1" || fact["content"] != "the staging server is called helios" ||
		len(previous) != 1 {
		t.Fatalf("update = %v, want the superseded fact with its trail", out)
	}
	was, _ := previous[0].(map[string]any)
	if was["content"] != "the staging server is called atlas" {
		t.Errorf("trail = %v, want the old value kept", was)
	}
	waitForActivityRow(t, client, "Fact edited: m1")

	raw := memoryFileBytes(t, dir)
	if !strings.Contains(raw, `content = "the staging server is called helios"`) ||
		!strings.Contains(raw, `content = "the staging server is called atlas"`) {
		t.Errorf("memory.toml after update lost the trail:\n%s", raw)
	}
	// The sibling fact is untouched.
	facts := memoryFacts(t, client)
	if len(facts) != 2 {
		t.Fatalf("listing after update = %v, want both facts", facts)
	}
}

// TestMemoryWriteRefusalsAreFieldKeyed: every refusal arrives in the entry
// form's wire shape — empty content on its field, a full store as a
// whole-entry problem — with the file byte-identical, and an unknown id or a
// disabled memory as the crisp parameter errors the other memory verbs give.
func TestMemoryWriteRefusalsAreFieldKeyed(t *testing.T) {
	client, _, dir := startMemoryDaemon(t, func(cfg *config.Config) {
		cfg.Memory.MaxFacts = 2
	})
	seedMemoryFile(t, dir)
	original := memoryFileBytes(t, dir)

	// Empty content, on the content field — the book's own sentence.
	err := client.Call("memory.add", map[string]any{"content": "   "}, nil)
	var rpcErr *ipc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != ipc.CodeConfigInvalid {
		t.Fatalf("empty add err = %v, want CodeConfigInvalid", err)
	}
	data, _ := rpcErr.Data.(map[string]any)
	if msg, ok := problemOn(entryProblemList(t, data), "content"); !ok ||
		!strings.Contains(msg, "a fact needs content") {
		t.Errorf("problems = %v, want the book's wording on the content field", data)
	}
	err = client.Call("memory.update", map[string]any{"id": "m1", "content": ""}, nil)
	if !errors.As(err, &rpcErr) || rpcErr.Code != ipc.CodeConfigInvalid {
		t.Fatalf("empty update err = %v, want CodeConfigInvalid", err)
	}

	// The store cap: no single field can fix it, so it is a whole-entry
	// problem — still the book's actionable sentence, verbatim.
	err = client.Call("memory.add", map[string]any{"content": "one fact too many"}, nil)
	if !errors.As(err, &rpcErr) || rpcErr.Code != ipc.CodeConfigInvalid {
		t.Fatalf("cap err = %v, want CodeConfigInvalid", err)
	}
	data, _ = rpcErr.Data.(map[string]any)
	if msg, ok := problemOn(entryProblemList(t, data), ""); !ok ||
		!strings.Contains(msg, "the memory store is full") ||
		!strings.Contains(msg, "memory.max_facts") {
		t.Errorf("problems = %v, want the cap as a whole-entry problem", data)
	}

	// An unknown id refuses like memory.forget does, naming it.
	err = client.Call("memory.update", map[string]any{"id": "m9", "content": "x"}, nil)
	if !errors.As(err, &rpcErr) || rpcErr.Code != ipc.CodeInvalidParams ||
		!strings.Contains(rpcErr.Message, `"m9"`) {
		t.Errorf("unknown id err = %v, want CodeInvalidParams naming it", err)
	}
	err = client.Call("memory.update", map[string]any{"content": "x"}, nil)
	if !errors.As(err, &rpcErr) || rpcErr.Code != ipc.CodeInvalidParams {
		t.Errorf("missing id err = %v, want CodeInvalidParams", err)
	}

	// Nothing was written by any refusal.
	if memoryFileBytes(t, dir) != original {
		t.Error("a refused write still changed memory.toml")
	}
}

// TestMemoryWriteDisabledRefuses: with memory off the verbs answer like
// memory.forget — a named refusal, not a silent no-op — and no store file
// appears.
func TestMemoryWriteDisabledRefuses(t *testing.T) {
	client, _, dir := startMemoryDaemon(t, func(cfg *config.Config) {
		cfg.Memory.Enabled = false
	})
	for _, call := range []struct {
		method string
		params map[string]any
	}{
		{"memory.add", map[string]any{"content": "x"}},
		{"memory.update", map[string]any{"id": "m1", "content": "x"}},
	} {
		err := client.Call(call.method, call.params, nil)
		var rpcErr *ipc.Error
		if !errors.As(err, &rpcErr) || rpcErr.Code != ipc.CodeInvalidParams ||
			!strings.Contains(rpcErr.Message, "memory is disabled") {
			t.Errorf("%s err = %v, want the disabled refusal", call.method, err)
		}
	}
	if memoryFileBytes(t, dir) != "" {
		t.Error("a disabled memory still wrote a store file")
	}
}

// TestMemorySetPinnedOverSocket is the fact card's pin toggle (#104): the
// pin lands on disk through the book, the listing serves it, the activity
// feed names the toggle by id, and the opposite call undoes it exactly.
func TestMemorySetPinnedOverSocket(t *testing.T) {
	client, _, dir := startMemoryDaemon(t, nil)
	seedMemoryFile(t, dir)

	var out map[string]any
	if err := client.Call("memory.set_pinned",
		map[string]any{"id": "m1", "pinned": true}, &out); err != nil {
		t.Fatal(err)
	}
	fact, _ := out["fact"].(map[string]any)
	if fact["id"] != "m1" || fact["pinned"] != true {
		t.Fatalf("set_pinned = %v, want the fact pinned", out)
	}
	waitForActivityRow(t, client, "Fact pinned: m1")
	if !strings.Contains(memoryFileBody(t, dir), "pinned = true") {
		t.Error("pin did not reach memory.toml")
	}

	if err := client.Call("memory.set_pinned",
		map[string]any{"id": "m1", "pinned": false}, &out); err != nil {
		t.Fatal(err)
	}
	waitForActivityRow(t, client, "Fact unpinned: m1")
	if strings.Contains(memoryFileBody(t, dir), "pinned = true") {
		t.Error("unpin left the pin on disk")
	}

	// The refusals match the sibling verbs: unknown id named, missing id
	// crisp.
	err := client.Call("memory.set_pinned", map[string]any{"id": "m9", "pinned": true}, nil)
	var rpcErr *ipc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != ipc.CodeInvalidParams ||
		!strings.Contains(rpcErr.Message, `"m9"`) {
		t.Errorf("unknown id err = %v, want CodeInvalidParams naming it", err)
	}
	if err := client.Call("memory.set_pinned", map[string]any{"pinned": true}, nil); err == nil {
		t.Error("set_pinned without an id succeeded")
	}
}

// TestMemoryUpdateTogglesPinWithoutManufacturingARevision: the edit form
// always sends content and pin together, so a save that only toggled the pin
// must not push an identical content onto the supersede trail — and a save
// that changed both does both, through the book verb that owns each change.
func TestMemoryUpdateTogglesPinWithoutManufacturingARevision(t *testing.T) {
	client, _, dir := startMemoryDaemon(t, nil)
	seedMemoryFile(t, dir)

	var out map[string]any
	if err := client.Call("memory.update", map[string]any{
		"id": "m1", "content": "the staging server is called atlas", "pinned": true,
	}, &out); err != nil {
		t.Fatal(err)
	}
	fact, _ := out["fact"].(map[string]any)
	if fact["pinned"] != true {
		t.Fatalf("update = %v, want the pin applied", out)
	}
	if previous, _ := fact["previous"].([]any); len(previous) != 0 {
		t.Errorf("unchanged content grew a trail: %v", previous)
	}

	// Content and pin in one save: the trail records the wording change,
	// the pin comes off, one event round.
	if err := client.Call("memory.update", map[string]any{
		"id": "m1", "content": "the staging server is called helios", "pinned": false,
	}, &out); err != nil {
		t.Fatal(err)
	}
	fact, _ = out["fact"].(map[string]any)
	previous, _ := fact["previous"].([]any)
	if fact["content"] != "the staging server is called helios" ||
		fact["pinned"] != false || len(previous) != 1 {
		t.Fatalf("combined update = %v, want new wording, no pin, one revision", out)
	}
	raw := memoryFileBytes(t, dir)
	if !strings.Contains(raw, "the staging server is called atlas") {
		t.Errorf("trail lost on disk:\n%s", raw)
	}
}

// TestMemoryAddPinnedCreatesAnAmbientFact: the form's "add pinned" is one
// save on the wire even though the daemon writes it as add-then-pin.
func TestMemoryAddPinnedCreatesAnAmbientFact(t *testing.T) {
	client, _, dir := startMemoryDaemon(t, nil)
	var out map[string]any
	if err := client.Call("memory.add",
		map[string]any{"content": "the user's editor is neovim", "pinned": true}, &out); err != nil {
		t.Fatal(err)
	}
	fact, _ := out["fact"].(map[string]any)
	if fact["pinned"] != true {
		t.Fatalf("add = %v, want the fact pinned", out)
	}
	if !strings.Contains(memoryFileBody(t, dir), "pinned = true") {
		t.Error("pinned add did not reach memory.toml")
	}
}

// memoryFileBody is the store file below its header — the header's own
// documentation mentions the pinned key, so key assertions scan the document
// body only.
func memoryFileBody(t *testing.T, dir string) string {
	t.Helper()
	raw := memoryFileBytes(t, dir)
	if i := strings.Index(raw, "version ="); i >= 0 {
		return raw[i:]
	}
	return raw
}

// TestMemoryListCarriesStatsAndWarning: the Memory tab's data contract
// (#104). A never-retrieved fact carries no stats keys at all — absence on
// the wire is what stops a client fabricating "retrieved 0 times" — while a
// retrieved fact carries the count, the timestamp, and the shared spoken
// wording; and a pinned set past the budget puts the book's warning sentence
// in the listing, never silently in a log.
func TestMemoryListCarriesStatsAndWarning(t *testing.T) {
	client, _, dir := startMemoryDaemon(t, func(cfg *config.Config) {
		cfg.Memory.MaxInjectedTokens = 100
	})
	seedMemoryFile(t, dir)
	seedRetrievedFact(t, dir)

	facts := memoryFacts(t, client)
	byID := map[string]map[string]any{}
	for _, f := range facts {
		fact, _ := f.(map[string]any)
		byID[fact["id"].(string)] = fact
	}
	for _, id := range []string{"m1", "m2"} {
		if _, has := byID[id]["times_retrieved"]; has {
			t.Errorf("never-retrieved %s carries stats: %v", id, byID[id])
		}
		if byID[id]["pinned"] != false {
			t.Errorf("%s pinned = %v, want false on the wire", id, byID[id]["pinned"])
		}
	}
	retrieved := byID["m3"]
	if retrieved["times_retrieved"] != float64(2) {
		t.Errorf("m3 = %v, want times_retrieved 2", retrieved)
	}
	if spoken, _ := retrieved["last_retrieved_spoken"].(string); !strings.Contains(spoken, "ago") &&
		spoken != "just now" && spoken != "yesterday" {
		t.Errorf("m3 spoken age = %q, want the shared spoken wording", retrieved["last_retrieved_spoken"])
	}

	// No pins, three facts, a 100-token budget: the book is over budget, so
	// the listing must say so and say the fix — the never-silent contract.
	var listing map[string]any
	if err := client.Call("memory.list", nil, &listing); err != nil {
		t.Fatal(err)
	}
	warning, _ := listing["warning"].(string)
	if !strings.Contains(warning, "none are pinned") || !strings.Contains(warning, "memory.search") {
		t.Errorf("listing warning = %q, want the over-budget sentence", warning)
	}
}

// seedRetrievedFact appends a fact with retrieval stats to the seeded store,
// as a previous daemon life's searches would have left it.
func seedRetrievedFact(t *testing.T, dir string) {
	t.Helper()
	path := filepath.Join(dir, "memory.toml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	extra := `
[[fact]]
id = "m3"
content = "the user's terminal is Ghostty and it runs everywhere the user works"
stored = 2026-08-03T10:00:00Z
updated = 2026-08-03T10:00:00Z
times_retrieved = 2
last_retrieved = 2026-08-03T12:00:00Z
`
	if err := os.WriteFile(path, append(raw, []byte(extra)...), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestMemoryAddNearCapWarns: the warning that must precede the refusal — a
// store filling up says so on every successful add, so the cap is never the
// first anyone hears of the limit.
func TestMemoryAddNearCapWarns(t *testing.T) {
	client, _, dir := startMemoryDaemon(t, func(cfg *config.Config) {
		cfg.Memory.MaxFacts = 3
	})
	seedMemoryFile(t, dir)

	var out map[string]any
	if err := client.Call("memory.add", map[string]any{"content": "the third fact"}, &out); err != nil {
		t.Fatal(err)
	}
	warning, _ := out["warning"].(string)
	if !strings.Contains(warning, "nearly full") || !strings.Contains(warning, "3 of 3") {
		t.Errorf("warning = %q, want the book's near-cap sentence", warning)
	}
}
