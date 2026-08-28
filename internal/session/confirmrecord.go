package session

// This file makes resolved tool confirmations part of the conversation record
// (issue #118). The live card (#76) and its events are ephemeral window
// state; before this, closing and reopening the window erased every approval
// exchange from the history — invisible gaps exactly where the user
// authorised Jarvix to act. Now each resolution is recorded under the engine
// lock before its event publishes, rendered by Conversation() at its position
// between the turns of its exchange, and woven into the archive beside them —
// the same staged-before-acknowledged discipline turns follow (#116, #125),
// so a record a client has seen acknowledged is behind the SyncArchive read
// barrier like any turn.

import (
	"time"

	"github.com/rpickz/jarvix/internal/conversations"
)

// confirmationRecord is one resolved permission-gate exchange kept for the
// conversation record. Guarded by Engine.mu, like the history it interleaves
// with.
type confirmationRecord struct {
	conversations.Confirmation
	// summary is the spoken question, shown as the record's text.
	summary string
	// at is when the confirmation resolved — earlier than the timestamps of
	// the exchange it belongs to, which are stamped at commit, and truthfully
	// so: the answer was given while the turn was still running.
	at time.Time
	// afterMsgs anchors the record's position: it renders after the first
	// afterMsgs messages of the thread, counted over the thread's whole life
	// (Engine.msgCount) rather than as an index into the capped history
	// slice, so the retention cap trimming the head never shifts a record
	// onto the wrong exchange. A confirmation resolves mid-exchange, so its
	// anchor is msgCount+1 at resolution time — after the user turn that is
	// about to be committed at index msgCount, before the assistant turn at
	// msgCount+1.
	afterMsgs int
	// staged marks the record as woven into the archive (or adopted from
	// it). Unstaged records belong to the exchange in flight and are staged
	// between its two halves when it commits, or finalized standalone when
	// the turn dies without committing.
	staged bool
}

// archiveTurn renders the record as the archive turn it is stored as.
func (r *confirmationRecord) archiveTurn() conversations.Turn {
	c := r.Confirmation
	return conversations.Turn{
		Role:         conversations.RoleConfirmation,
		Text:         r.summary,
		Time:         r.at,
		Confirmation: &c,
	}
}

// displayTurn renders the record for the conversation.get snapshot.
func (r *confirmationRecord) displayTurn() Turn {
	c := r.Confirmation
	return Turn{Role: conversations.RoleConfirmation, Text: r.summary, Confirmation: &c}
}

// recordConfirmationLocked keeps the resolution of pending confirmation p for
// the record. Callers hold e.mu and call this *before* publishing the
// tool.confirmed/tool.declined event, so a client that has the resolution
// acknowledged — the IPC reply to session.confirm, or the event — finds the
// record on an immediate conversation.get, never a window in which the
// exchange has vanished.
func (e *Engine) recordConfirmationLocked(p *pendingConfirmation, outcome, source string) {
	e.confRecords = append(e.confRecords, &confirmationRecord{
		Confirmation: conversations.Confirmation{
			Tool:       p.tool,
			Command:    p.command,
			Rule:       p.rule,
			Outcome:    outcome,
			Source:     source,
			TimeoutSec: int(p.timeout.Seconds()),
			// The honest half of a "don't ask again" (#162): the record says
			// approved AND says which rule that answer added, so re-reading
			// the conversation shows where a standing grant came from rather
			// than a bare yes that quietly widened the gate.
			Remembered:    p.rememberedPattern(),
			RememberScope: string(p.granted),
		},
		summary:   p.summary,
		at:        e.now(),
		afterMsgs: e.msgCount + 1,
	})
}

// unstagedConfRecordsLocked returns the records of the exchange in flight —
// resolved this turn, not yet woven into the archive — in resolution order.
// Callers hold e.mu.
func (e *Engine) unstagedConfRecordsLocked() []*confirmationRecord {
	var recs []*confirmationRecord
	for _, r := range e.confRecords {
		if !r.staged {
			recs = append(recs, r)
		}
	}
	return recs
}

// finalizeConfRecordsLocked stages any records whose turn is ending without a
// commit — a stage failure after an approval already executed, a cancel with
// nothing committable aboard. The approval happened and the command may well
// have run, so the record survives even though the exchange around it does
// not: anchored after the last committed message, which is exactly where the
// archive appends it. Callers hold e.mu and, when it reports true, must
// arrange persistence off the lock — the dying session's tail will not.
func (e *Engine) finalizeConfRecordsLocked() (staged bool) {
	for _, r := range e.confRecords {
		if r.staged {
			continue
		}
		r.afterMsgs = e.msgCount
		r.staged = true
		if e.opts.Archive != nil {
			e.pendingArchive = append(e.pendingArchive, r.archiveTurn())
			staged = true
		}
	}
	return staged
}

// pruneConfRecordsLocked drops records that have fallen behind the retention
// window: their exchange is no longer displayed, so the record is not either
// — it stays in the archive, which the cap never governs (ADR 0027). Only
// staged records are pruned; an unstaged one still owes the archive its line.
// Callers hold e.mu.
func (e *Engine) pruneConfRecordsLocked() {
	base := e.msgCount - len(e.history)
	kept := e.confRecords[:0]
	for _, r := range e.confRecords {
		if r.staged && r.afterMsgs < base {
			continue
		}
		kept = append(kept, r)
	}
	e.confRecords = kept
}

// AdoptedConfirmation is one confirmation record restored from an archived
// conversation for AdoptConversation — the confirmation half of what
// adoptableMessages extracts (issue #118).
type AdoptedConfirmation struct {
	Record  conversations.Confirmation
	Summary string
	Time    time.Time
	// AfterMessages is how many of the adopted messages precede the record
	// in the archive.
	AfterMessages int
}
