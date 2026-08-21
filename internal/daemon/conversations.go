package daemon

// This file is the IPC surface of the durable conversation archive
// (ADR 0027): list, read, reopen, delete. The CLI (`jarvix conversations`)
// and the window's history view are thin clients of these methods — every
// decision, including what deleting the *active* conversation means, is made
// here in the daemon (ADR 0013, ADR 0015).
//
// Transcript contents travel here and nowhere else: it is the user's own
// record, asked for over their own 0600 socket. Events and logs carry ids and
// counts only.

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/conversations"
	"github.com/rpickz/jarvix/internal/ipc"
)

func (d *Daemon) registerConversationMethods() {
	// conversation.list answers from metadata alone — the store never opens a
	// transcript for it — so a large library lists as fast as a small one.
	// Unreadable conversations are reported beside the readable rest: one bad
	// file never hides the library, and never hides itself either.
	d.server.Handle("conversation.list", func(json.RawMessage) (any, error) {
		metas, unreadable, err := d.conversations.List()
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, "%v", err)
		}
		bad := make([]map[string]any, 0, len(unreadable))
		for _, u := range unreadable {
			bad = append(bad, map[string]any{"id": u.ID, "error": u.Err})
		}
		list := make([]map[string]any, 0, len(metas))
		for _, m := range metas {
			list = append(list, metaReport(m))
		}
		return map[string]any{
			"retention":     d.retentionOn(),
			"active_id":     d.engine.ActiveConversationID(),
			"conversations": list,
			"unreadable":    bad,
		}, nil
	})

	// conversation.read returns one whole conversation, read-only: the
	// window's history view and `jarvix conversations show` render it without
	// touching the live thread.
	d.server.Handle("conversation.read", func(params json.RawMessage) (any, error) {
		id, _, err := conversationParams(params, "conversation.read")
		if err != nil {
			return nil, err
		}
		conv, err := d.conversations.Read(id)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
		}
		turns := make([]map[string]any, 0, len(conv.Turns))
		for _, t := range conv.Turns {
			turns = append(turns, map[string]any{
				"role": t.Role, "text": t.Text, "ts": t.Time.Format(time.RFC3339),
			})
		}
		report := metaReport(conv.Meta)
		report["turns"] = turns
		return report, nil
	})

	// conversation.open reopens an archived conversation as the active
	// thread — the explicit action the ticket requires. The whole record is
	// read here; the engine keeps the most recent history_turns exchanges for
	// the model (the context budget) while the archive keeps everything.
	d.server.Handle("conversation.open", func(params json.RawMessage) (any, error) {
		id, _, err := conversationParams(params, "conversation.open")
		if err != nil {
			return nil, err
		}
		conv, err := d.conversations.Read(id)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
		}
		msgs, err := adoptableMessages(conv.Turns)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
		}
		d.engine.AdoptConversation(id, msgs)
		d.log.Info("conversation reopened", "component", "daemon", "conversation_id", id,
			"turns", len(conv.Turns))
		return map[string]any{"id": id, "turns": len(conv.Turns)}, nil
	})

	// conversation.delete removes one conversation (id) or every one (all).
	// Deleting the conversation the live thread belongs to also resets the
	// thread — head, approvals, and history.json included — because a record
	// the user destroyed must not survive in working memory and quietly
	// rebuild itself on the next turn.
	d.server.Handle("conversation.delete", func(params json.RawMessage) (any, error) {
		id, all, err := conversationParams(params, "conversation.delete")
		if err != nil {
			return nil, err
		}
		if all {
			if d.engine.ActiveConversationID() != "" {
				d.engine.ResetConversation()
			}
			n, err := d.conversations.DeleteAll()
			if err != nil {
				return nil, ipc.Errorf(ipc.CodeInternalError, "%v", err)
			}
			d.log.Info("conversations deleted", "component", "daemon", "count", n)
			return map[string]any{"deleted": n}, nil
		}
		if d.engine.ActiveConversationID() == id {
			d.engine.ResetConversation()
		}
		if err := d.conversations.Delete(id); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
		}
		d.log.Info("conversation deleted", "component", "daemon", "conversation_id", id)
		return map[string]any{"deleted": 1}, nil
	})
}

// retentionOn reads the live retention switch.
func (d *Daemon) retentionOn() bool {
	d.cfgMu.Lock()
	defer d.cfgMu.Unlock()
	return d.cfg.Conversation.Retention != config.RetentionOff
}

// conversationParams decodes the {id} / {all} parameter shape the
// conversation methods share.
func conversationParams(params json.RawMessage, method string) (id string, all bool, err error) {
	p := struct {
		ID  string `json:"id"`
		All bool   `json:"all"`
	}{}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return "", false, ipc.Errorf(ipc.CodeInvalidParams, "%s params: %v", method, err)
		}
	}
	if p.ID == "" && !p.All {
		return "", false, ipc.Errorf(ipc.CodeInvalidParams, "%s needs a conversation id", method)
	}
	return p.ID, p.All, nil
}

// metaReport renders one conversation's metadata for the wire, timestamps in
// RFC 3339 like every other IPC surface.
func metaReport(m conversations.Meta) map[string]any {
	return map[string]any{
		"id":          m.ID,
		"started":     m.Started.Format(time.RFC3339),
		"last_active": m.LastActive.Format(time.RFC3339),
		"turns":       m.TurnCount,
		"preview":     m.Preview,
	}
}

// adoptableMessages converts archived turns into the engine's message shape.
// Only the roles the engine commits — user and assistant — may enter the
// model context; anything else in the file is a hand edit or corruption the
// parser happened to accept, and refusing it here keeps the corruption from
// becoming a malformed provider request mid-conversation (the history
// precedent, ADR 0011).
func adoptableMessages(turns []conversations.Turn) ([]ai.Message, error) {
	msgs := make([]ai.Message, 0, len(turns))
	for i, t := range turns {
		role := ai.Role(t.Role)
		if role != ai.RoleUser && role != ai.RoleAssistant {
			return nil, fmt.Errorf("conversation turn %d has unknown role %q", i+1, t.Role)
		}
		msgs = append(msgs, ai.Message{Role: role, Content: t.Text})
	}
	return msgs, nil
}
