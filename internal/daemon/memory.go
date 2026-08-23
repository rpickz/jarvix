package daemon

// This file is the IPC surface of the knowledge base (ADR 0025), in two
// halves that mirror desktop context's disclosure precedent (ADR 0019):
//
//   - memory.last answers "what facts was the model just given?" with the
//     exact injected content — `jarvix status --last` is a thin client.
//   - memory.list / memory.forget are the CLI's direct line to the store
//     (`jarvix memory list|forget`), bypassing the model entirely: hearing
//     and correcting what Jarvix knows must never require a conversation.
//
// Contents travel here and nowhere else: it is the user's own memory, asked
// for over their own 0600 socket. Events and logs carry counts only.

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/memory"
)

func (d *Daemon) registerMemoryMethods() {
	// The methods are registered even with memory disabled, answering
	// enabled=false, so a client can tell "switched off" from "old daemon"
	// without version sniffing.
	d.server.Handle("memory.last", func(json.RawMessage) (any, error) {
		if d.memory == nil {
			return map[string]any{"enabled": false, "injected": false}, nil
		}
		inj, sessionID, ok := d.engine.LastMemory()
		if !ok {
			return map[string]any{"enabled": true, "injected": false}, nil
		}
		return map[string]any{
			"enabled":    true,
			"injected":   true,
			"session_id": sessionID,
			"facts":      factReports(inj.Facts),
			"trimmed":    inj.Trimmed,
			"total":      inj.Total,
			"est_tokens": inj.EstTokens,
		}, nil
	})

	d.server.Handle("memory.list", func(params json.RawMessage) (any, error) {
		if d.memory == nil {
			return map[string]any{"enabled": false}, nil
		}
		p := struct {
			Query string `json:"query"`
		}{}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "memory.list params: %v", err)
			}
		}
		facts := d.memory.List(p.Query)
		count, max := d.memory.Count()
		return map[string]any{
			"enabled": true,
			"path":    d.memory.Path(),
			"count":   count,
			"max":     max,
			"facts":   factReports(facts),
		}, nil
	})

	// Deletion by id or by words, with the same refuse-to-guess rule as the
	// model-facing tool: an ambiguous query deletes nothing and reports the
	// candidates instead.
	d.server.Handle("memory.forget", func(params json.RawMessage) (any, error) {
		if d.memory == nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"memory is disabled (memory.enabled = false)")
		}
		p := struct {
			ID    string `json:"id"`
			Query string `json:"query"`
		}{}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "memory.forget params: %v", err)
			}
		}
		id := p.ID
		if id == "" {
			if strings.TrimSpace(p.Query) == "" {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "memory.forget needs an id or a query")
			}
			matches := d.memory.List(p.Query)
			switch len(matches) {
			case 0:
				return map[string]any{"forgotten": false, "matches": []any{}}, nil
			case 1:
				id = matches[0].ID
			default:
				return map[string]any{"forgotten": false, "matches": factReports(matches)}, nil
			}
		}
		forgotten, err := d.memory.Forget(id)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
		}
		return map[string]any{"forgotten": true, "fact": factReport(forgotten)}, nil
	})

	// The window's per-fact Forget button (issue #92). Where memory.forget
	// above is the CLI's direct line — the user typed the id, deliberately —
	// a button in a listing deserves the same second look the model gets, so
	// this routes through the gated tool path instead: the engine runs the
	// memory.forget identity through the permission gate, the standard
	// confirmation card appears (naming the exact fact, resolved from the
	// store), and only an approval deletes. The reply carries the session id;
	// resolution arrives as the ordinary tool.* events.
	d.server.Handle("memory.forget_gated", func(params json.RawMessage) (any, error) {
		if d.memory == nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"memory is disabled (memory.enabled = false)")
		}
		p := struct {
			ID string `json:"id"`
		}{}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "memory.forget_gated params: %v", err)
			}
		}
		if p.ID == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "memory.forget_gated needs an id")
		}
		// Resolved here so an unknown id is a crisp error to the caller, not
		// a session that starts only to apologise — and so the conversation
		// record can name the fact being forgotten.
		var content string
		for _, f := range d.memory.List("") {
			if f.ID == p.ID {
				content = f.Content
				break
			}
		}
		if content == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"no remembered fact has id %q; memory.list shows what is stored", p.ID)
		}
		id, err := d.engine.ForgetFact(p.ID, content)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeSessionError, "%v", err)
		}
		return map[string]string{"session_id": id}, nil
	})
}

// factReport renders one fact for the wire, trail included, timestamps in
// RFC 3339 like every other IPC surface.
func factReport(f memory.Fact) map[string]any {
	report := map[string]any{
		"id":      f.ID,
		"content": f.Content,
		"stored":  f.Stored.Format(time.RFC3339),
		"updated": f.Updated.Format(time.RFC3339),
	}
	if f.Source != "" {
		report["source"] = f.Source
	}
	if len(f.Previous) > 0 {
		previous := make([]map[string]any, 0, len(f.Previous))
		for _, p := range f.Previous {
			previous = append(previous, map[string]any{
				"content":    p.Content,
				"stored":     p.Stored.Format(time.RFC3339),
				"superseded": p.Superseded.Format(time.RFC3339),
			})
		}
		report["previous"] = previous
	}
	return report
}

// factReports renders a fact list, never nil, so clients always see an array.
func factReports(facts []memory.Fact) []map[string]any {
	out := make([]map[string]any, 0, len(facts))
	for _, f := range facts {
		out = append(out, factReport(f))
	}
	return out
}
