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
	"strings"

	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/memory"
	"github.com/rpickz/jarvix/internal/session"
)

// registerMemoryAdminMethods adds the Memory tab's write verbs (#100, #104).
func (d *Daemon) registerMemoryAdminMethods() {
	// memory.add: a new fact, straight into the store. The reply carries the
	// stored fact (id assigned, timestamps set) and the book's near-cap
	// warning when it has one — the refusal at the cap must never be the
	// first anyone hears of the limit, on this surface as on the tool's.
	// `pinned` (#104) lets the form create an ambient fact in one save; it
	// is a second book call rather than an Add parameter because pinning is
	// its own reversible operation, not part of a fact's content.
	d.server.Handle("memory.add", func(params json.RawMessage) (any, error) {
		if d.memory == nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"memory is disabled (memory.enabled = false)")
		}
		var p struct {
			Content string `json:"content"`
			Pinned  bool   `json:"pinned"`
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
		if p.Pinned {
			// The fact exists whatever happens next, so a failed pin is a
			// failed *pin*, not a failed add: the error names what did not
			// happen and the fact report shows the true state.
			if pinnedFact, pinErr := d.memory.SetPinned(fact.ID, true); pinErr == nil {
				fact = pinnedFact
			} else {
				return nil, ipc.Errorf(ipc.CodeInternalError,
					"the fact was stored as %s but could not be pinned: %v", fact.ID, pinErr)
			}
		}
		d.publishMemoryEntryChanged("added", fact)
		result := map[string]any{"fact": factReport(fact)}
		if warning != "" {
			result["warning"] = warning
		}
		return result, nil
	})

	// memory.update: edit one fact from the form. The book supersedes rather
	// than overwrites — the old value moves onto the fact's trail — which is
	// what makes this verb safe ungated: nothing is destroyed, and "when did
	// that change" stays answerable. Since #104 the form also carries the
	// pin state, so the handler compares before it writes: unchanged content
	// must not manufacture a revision of itself just because the user opened
	// the form to toggle the pin, and an untouched pin must not cost a
	// write. Each real change goes through the book verb that owns it.
	d.server.Handle("memory.update", func(params json.RawMessage) (any, error) {
		if d.memory == nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"memory is disabled (memory.enabled = false)")
		}
		var p struct {
			ID      string `json:"id"`
			Content string `json:"content"`
			Pinned  *bool  `json:"pinned"`
		}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "memory.update params: %v", err)
			}
		}
		if p.ID == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "memory.update needs an id")
		}
		current, ok := d.memoryFact(p.ID)
		if !ok {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"no remembered fact has id %q; memory.list shows what is stored", p.ID)
		}
		fact := current
		if strings.TrimSpace(p.Content) != current.Content {
			updated, err := d.memory.Update(p.ID, p.Content, "")
			if err != nil {
				return nil, memoryWriteError(err)
			}
			fact = updated
		}
		if p.Pinned != nil && *p.Pinned != fact.Pinned {
			pinned, err := d.memory.SetPinned(p.ID, *p.Pinned)
			if err != nil {
				return nil, memoryWriteError(err)
			}
			fact = pinned
		}
		d.publishMemoryEntryChanged("edited", fact)
		return map[string]any{"fact": factReport(fact)}, nil
	})

	// memory.set_pinned: the fact card's one-click pin toggle (#104).
	// Ungated for the reversibility reason add and edit are: the opposite
	// click undoes it exactly, nothing is destroyed, and the click is the
	// user's own explicit act. The reply carries the book's over-budget
	// warning when the new pin state creates one, so pinning past the
	// budget answers with the consequence in the same breath.
	d.server.Handle("memory.set_pinned", func(params json.RawMessage) (any, error) {
		if d.memory == nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"memory is disabled (memory.enabled = false)")
		}
		var p struct {
			ID     string `json:"id"`
			Pinned bool   `json:"pinned"`
		}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "memory.set_pinned params: %v", err)
			}
		}
		if p.ID == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "memory.set_pinned needs an id")
		}
		fact, err := d.memory.SetPinned(p.ID, p.Pinned)
		if err != nil {
			return nil, memoryWriteError(err)
		}
		action := "unpinned"
		if p.Pinned {
			action = "pinned"
		}
		d.publishMemoryEntryChanged(action, fact)
		result := map[string]any{"fact": factReport(fact)}
		if warning := d.memory.AmbientWarning(); warning != "" {
			result["warning"] = warning
		}
		return result, nil
	})
}

// memoryFact resolves one fact by id from the store, for handlers that need
// the current state before deciding what to write.
func (d *Daemon) memoryFact(id string) (memory.Fact, bool) {
	for _, f := range d.memory.List("") {
		if f.ID == id {
			return f, true
		}
	}
	return memory.Fact{}, false
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
