package daemon

// This file is the Memory tab's write surface (issue #100): adding a fact
// and editing one from the window's form dialog. Memory is deliberately NOT
// a row in the config entry registry (entry_admin.go) — memory.toml is not
// config.toml. It is the memory book's own store (ADR 0025), with its own
// discipline: the stat-per-operation hand-edit pickup, the corrupt-file
// move-aside, the never-reused ids, the supersede trail, and the atomic
// fsync-and-rename write. These verbs go through that one path —
// Book.Add / Book.Update, the same calls the memory.remember tool makes —
// so the window can never write a fact the book would not.
//
// Gating: add and edit are UNGATED, the ADR 0025 reversibility split applied
// as written. The gate exists to guard the irreversible: memory.forget
// destroys (so the window's button routes through memory.forget_gated and
// the confirmation card), while an add is undone with one forget and an
// edit supersedes — the old value stays on the [[fact.previous]] trail with
// both timestamps. And the trust model holds: a fact typed into the form is
// the user's explicit word, exactly like a spoken "remember that ..." — the
// same reason memory.remember is built-in allow. Asking would turn the
// user's own instruction into a question about itself.
//
// Refusals travel in the entry form's wire shape — CodeConfigInvalid with
// field-keyed {field, message} problems — so the window's dialog code pins
// them exactly as it pins a config family's (the shared-shape requirement of
// #100). The rules themselves live in the book; this file only PLACES each
// refusal, matching the book's error sentinels, never re-judging content.

import (
	"encoding/json"
	"errors"

	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/memory"
	"github.com/rpickz/jarvix/internal/session"
)

// registerMemoryAdminMethods adds the Memory tab's two write verbs (#100).
func (d *Daemon) registerMemoryAdminMethods() {
	// memory.add: a new fact, straight into the store. The reply carries the
	// stored fact (id assigned, timestamps set) and the book's near-cap
	// warning when it has one — the refusal at the cap must never be the
	// first anyone hears of the limit, on this surface as on the tool's.
	d.server.Handle("memory.add", func(params json.RawMessage) (any, error) {
		if d.memory == nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"memory is disabled (memory.enabled = false)")
		}
		var p struct {
			Content string `json:"content"`
		}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "memory.add params: %v", err)
			}
		}
		fact, warning, err := d.memory.Add(p.Content, "")
		if err != nil {
			return nil, memoryWriteError(err)
		}
		d.publishMemoryEntryChanged("added", fact)
		result := map[string]any{"fact": factReport(fact)}
		if warning != "" {
			result["warning"] = warning
		}
		return result, nil
	})

	// memory.update: edit one fact's text. The book supersedes rather than
	// overwrites — the old value moves onto the fact's trail — which is what
	// makes this verb safe ungated: nothing is destroyed, and "when did that
	// change" stays answerable.
	d.server.Handle("memory.update", func(params json.RawMessage) (any, error) {
		if d.memory == nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"memory is disabled (memory.enabled = false)")
		}
		var p struct {
			ID      string `json:"id"`
			Content string `json:"content"`
		}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "memory.update params: %v", err)
			}
		}
		if p.ID == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "memory.update needs an id")
		}
		fact, err := d.memory.Update(p.ID, p.Content, "")
		if err != nil {
			return nil, memoryWriteError(err)
		}
		d.publishMemoryEntryChanged("edited", fact)
		return map[string]any{"fact": factReport(fact)}, nil
	})
}

// memoryWriteError places one book refusal for the form: empty content under
// the text field, a full store in the general area (no single field can fix
// it), an unknown id as the crisp parameter error memory.forget gives. The
// sentences are the book's own, verbatim; only the placement is decided
// here. Anything the sentinels do not match is an IO failure, and saying
// "internal" rather than "invalid" is what tells the user to look at disk
// space, not their text.
func memoryWriteError(err error) error {
	switch {
	case errors.Is(err, memory.ErrNoContent):
		return &ipc.Error{
			Code:    ipc.CodeConfigInvalid,
			Message: "the fact was rejected; nothing was written",
			Data: map[string]any{"problems": []entryProblem{
				{Field: "content", Message: err.Error()}}},
		}
	case errors.Is(err, memory.ErrStoreFull):
		return &ipc.Error{
			Code:    ipc.CodeConfigInvalid,
			Message: "the fact was rejected; nothing was written",
			Data: map[string]any{"problems": []entryProblem{
				{Message: err.Error()}}},
		}
	case errors.Is(err, memory.ErrUnknownID):
		return ipc.Errorf(ipc.CodeInvalidParams, "%v; memory.list shows what is stored", err)
	}
	return ipc.Errorf(ipc.CodeInternalError, "%v", err)
}

// publishMemoryEntryChanged announces one form save on the bus: the activity
// feed renders it into a row naming the fact by id, and any open window's
// Memory tab re-requests its listing off it. Id and size only, never the
// content — the feed's memory privacy contract (counts, not facts) holds for
// window saves exactly as for tool calls.
func (d *Daemon) publishMemoryEntryChanged(action string, fact memory.Fact) {
	d.bus.Publish(session.Event{Type: "memory.entry_changed", Data: map[string]any{
		"action": action, "id": fact.ID, "chars": len([]rune(fact.Content)),
	}})
}
