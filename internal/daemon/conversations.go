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
//
// Every read here starts at the engine's archive barrier (SyncArchive,
// issue #115). The archive flush runs on the session tail *after*
// session.finished (ADR 0011), so without the barrier a client that just
// watched a turn finish could list or search the archive and miss it — the
// live conversation absent from its own search, or active_id "" for a
// conversation already on disk. The guarantee these methods give is
// read-your-acknowledged-writes: a turn whose session.finished has been
// published is visible to every conversation.* read that starts afterwards.

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/conversations"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/session"
)

func (d *Daemon) registerConversationMethods() {
	// conversation.list answers from metadata alone — the store never opens a
	// transcript for it — so a large library lists as fast as a small one.
	// Unreadable conversations are reported beside the readable rest: one bad
	// file never hides the library, and never hides itself either.
	d.server.Handle("conversation.list", func(json.RawMessage) (any, error) {
		// The barrier before either read: the listing and active_id must agree
		// with each other and with any turn the client has seen acknowledged.
		// Without it, a listing racing a just-finished turn showed the new
		// conversation with active_id still "" — the long-running
		// TestConversationListOverSocket flake (issue #115).
		d.engine.SyncArchive()
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
		// The barrier, so the transcript view opened right after a turn
		// finishes includes that turn (issue #115).
		d.engine.SyncArchive()
		conv, err := d.conversations.Read(id)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
		}
		turns := make([]map[string]any, 0, len(conv.Turns))
		for _, t := range conv.Turns {
			turn := map[string]any{
				"role": t.Role, "text": t.Text, "ts": t.Time.Format(time.RFC3339),
			}
			// Present only when true, mirroring the file's omitempty: an
			// exchange the user cut off says so on the wire too (issue #117),
			// and a completed turn's shape is unchanged.
			if t.Interrupted {
				turn["interrupted"] = true
			}
			// A confirmation record travels whole (issue #118): the same
			// structured facts conversation.get's turns carry, so every
			// transcript view renders the approval — question, verbatim
			// command, outcome — from the daemon's record, never a guess.
			if t.Confirmation != nil {
				turn["confirmation"] = t.Confirmation
			}
			turns = append(turns, turn)
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
		// The barrier, so reopening a conversation right after its last turn
		// finished adopts the whole record, that turn included (issue #115).
		d.engine.SyncArchive()
		conv, err := d.conversations.Read(id)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
		}
		msgs, confs, err := adoptableMessages(conv.Turns)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
		}
		d.engine.AdoptConversation(id, msgs, confs)
		d.log.Info("conversation reopened", "component", "daemon", "conversation_id", id,
			"turns", len(conv.Turns))
		return map[string]any{"id": id, "turns": len(conv.Turns)}, nil
	})

	// conversation.search is full-text search over the archive (issue #59):
	// the window's search box and `jarvix conversations search` are thin
	// clients of it, and the model's conversations.search tool shares the
	// same Searcher — one implementation, three surfaces. Ranked passages
	// come back with conversation id and turn references; `current` marks
	// hits in the live thread so results can distinguish "earlier in this
	// conversation" from a past one. The query and the passages stay off the
	// journal: the log records that a search happened and what it counted.
	d.server.Handle("conversation.search", func(params json.RawMessage) (any, error) {
		p := struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
		}{}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "conversation.search params: %v", err)
			}
		}
		if strings.TrimSpace(p.Query) == "" {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "conversation.search needs a query")
		}
		// No explicit barrier here: d.searcher is the syncedSearcher, which
		// runs it inside Search — so the model's conversations.search tool,
		// which holds the same Searcher, gets the identical guarantee. The
		// active_id read below is ordered after it for the same reason
		// `current` must be truthful: both happen behind the flush.
		matches, stats, err := d.searcher.Search(conversations.Query{Text: p.Query, Limit: p.Limit})
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInternalError, "%v", err)
		}
		activeID := d.engine.ActiveConversationID()
		results := make([]map[string]any, 0, len(matches))
		for _, m := range matches {
			results = append(results, map[string]any{
				"id":      m.ConversationID,
				"turn":    m.Turn,
				"role":    m.Role,
				"ts":      m.Time.Format(time.RFC3339),
				"passage": m.Passage,
				"current": activeID != "" && m.ConversationID == activeID,
			})
		}
		skipped := make([]map[string]any, 0, len(stats.Skipped))
		for _, u := range stats.Skipped {
			skipped = append(skipped, map[string]any{"id": u.ID, "error": u.Err})
		}
		d.log.Info("conversation search", "component", "daemon",
			"results", len(matches), "searched", stats.Conversations, "skipped", len(stats.Skipped))
		return map[string]any{
			"retention": d.retentionOn(),
			"active_id": activeID,
			"results":   results,
			"searched":  stats.Conversations,
			"skipped":   skipped,
		}, nil
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
		// The barrier matters most here: the active-id checks below decide
		// whether deleting also resets the live thread. Without it, deleting
		// the conversation the user is in right after a turn — before the tail
		// flush had adopted its id — would skip the reset, and the record they
		// just destroyed would quietly rebuild from working memory (issue #115).
		d.engine.SyncArchive()
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

// conversationsReport summarises the archive for status.get: the retention
// switch, how many conversations are stored, and whether search has anything
// to work with. "inactive" is a state, not a failure — retention off with an
// empty archive means search *correctly* has nothing to do, and status must
// say that rather than look broken (issue #59).
func (d *Daemon) conversationsReport() map[string]any {
	retention := d.retentionOn()
	archived := 0
	if metas, _, err := d.conversations.List(); err == nil {
		archived = len(metas)
	}
	search := "active"
	if !retention && archived == 0 {
		search = "inactive"
	}
	return map[string]any{"retention": retention, "archived": archived, "search": search}
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

// syncedSearcher is the daemon's Searcher: the engine's archive barrier
// (SyncArchive, issue #115) in front of the real search, so a query never
// scans the archive while an acknowledged turn's flush is still in flight.
// It wraps once, in New, rather than as a call at each surface — the IPC
// method, and the model's conversations.search tool registered beside it,
// share this one value, so a future surface cannot take the searcher and
// forget the barrier.
type syncedSearcher struct {
	engine *session.Engine
	inner  conversations.Searcher
}

// Search implements conversations.Searcher.
func (s *syncedSearcher) Search(q conversations.Query) ([]conversations.Match, conversations.SearchStats, error) {
	s.engine.SyncArchive()
	return s.inner.Search(q)
}

// adoptableMessages converts archived turns into the engine's message shape,
// splitting out confirmation records (issue #118) with their positions so
// AdoptConversation restores them beside the turns they sat between. Only the
// roles the engine commits — user and assistant — may enter the model
// context; a confirmation record never does (the model heard about it through
// the tool round when it happened); anything else in the file is a hand edit
// or corruption the parser happened to accept, and refusing it here keeps the
// corruption from becoming a malformed provider request mid-conversation
// (the history precedent, ADR 0011).
func adoptableMessages(turns []conversations.Turn) ([]ai.Message, []session.AdoptedConfirmation, error) {
	msgs := make([]ai.Message, 0, len(turns))
	var confs []session.AdoptedConfirmation
	for i, t := range turns {
		if t.Role == conversations.RoleConfirmation {
			// A record without its payload is not a shape this daemon ever
			// writes; refuse it like an unknown role rather than adopting a
			// blank approval.
			if t.Confirmation == nil {
				return nil, nil, fmt.Errorf("conversation turn %d is a confirmation record without its payload", i+1)
			}
			confs = append(confs, session.AdoptedConfirmation{
				Record:        *t.Confirmation,
				Summary:       t.Text,
				Time:          t.Time,
				AfterMessages: len(msgs),
			})
			continue
		}
		role := ai.Role(t.Role)
		if role != ai.RoleUser && role != ai.RoleAssistant {
			return nil, nil, fmt.Errorf("conversation turn %d has unknown role %q", i+1, t.Role)
		}
		msgs = append(msgs, ai.Message{Role: role, Content: t.Text})
	}
	return msgs, confs, nil
}
