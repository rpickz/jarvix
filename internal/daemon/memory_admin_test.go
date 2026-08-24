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
