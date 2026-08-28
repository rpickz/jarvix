package session

// This file is the engine half of the durable conversation archive
// (ADR 0027). internal/conversations owns the files; the engine owns what a
// turn means for the record: every completed exchange is staged *before* the
// retention cap trims the in-memory head (commitTurn), and flushed to disk
// after session.finished, off the lock path — exactly where the history write
// lives (ADR 0011), so archiving adds no latency to the spoken exchange and
// rides the same shutdown drain (#29).
//
// The archive and the history store deliberately coexist. history.json stays
// the live head — small, capped, loaded at boot — while the archive is the
// unbounded record behind it; the `active` pointer in the archive directory
// is what lets a restarted daemon keep appending to the same conversation the
// live head came from.

import (
	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/conversations"
	"github.com/rpickz/jarvix/internal/provenance"
)

// stageArchiveTurnLocked records an exchange for the archive. Callers hold
// e.mu. It must run before the retention cap is applied: the cap governs what
// the model is sent, never what is kept, so a 100-turn conversation archives
// 100 turns however small history_turns is.
//
// interrupted marks both halves of an exchange the user cut off (issue #117):
// the flag rides the turn schema (additive, omitempty) so the record says the
// answer was incomplete by act rather than by accident, without disturbing a
// single byte of any completed turn's line.
//
// recs are the confirmations resolved during this exchange (issue #118),
// woven between its halves — after the question that provoked the tool call,
// before the answer that followed it — which is the position the live card
// occupied and the position every rebuilt view puts the record back in.
// prov is what went into the answer (issue #168), nil when the turn consumed
// nothing. It rides the assistant half alone, additively and with omitempty,
// so a turn that used nothing is byte-identical to one written before the
// field existed — and it holds references, never content, so nothing a fact,
// a feed or a captured window said is copied into the archive.
func (e *Engine) stageArchiveTurnLocked(userText, assistantText string, interrupted bool,
	recs []*confirmationRecord, prov *provenance.Record) {
	if e.opts.Archive == nil {
		return // retention off: nothing is ever staged, so nothing is written
	}
	now := e.now()
	e.pendingArchive = append(e.pendingArchive,
		conversations.Turn{Role: string(ai.RoleUser), Text: userText, Time: now, Interrupted: interrupted})
	for _, r := range recs {
		e.pendingArchive = append(e.pendingArchive, r.archiveTurn())
	}
	e.pendingArchive = append(e.pendingArchive,
		conversations.Turn{Role: string(ai.RoleAssistant), Text: assistantText, Time: now,
			Interrupted: interrupted, Provenance: prov})
}

// persistArchive flushes staged turns to the archive. It runs after
// session.finished on the tail of think()/runIntent, inside e.active, so the
// shutdown drain waits for it (#29). Failure degrades exactly like history
// persistence: the engine warns once, latches, and keeps conversing — turn
// contents are never logged.
//
// archiveMu serialises whole flushes. Without it, two session tails could
// interleave: the second would snapshot an empty conversation id before the
// first's append had created one, and a single thread would fork into two
// archived conversations. With it, the first flush adopts the created id
// before the second takes its snapshot.
func (e *Engine) persistArchive() {
	if e.archiveFailed.Load() {
		return
	}
	e.archiveMu.Lock()
	defer e.archiveMu.Unlock()
	e.mu.Lock()
	archive := e.opts.Archive
	turns := e.pendingArchive
	e.pendingArchive = nil
	id, gen := e.archiveID, e.archiveGen
	e.mu.Unlock()
	if archive == nil || len(turns) == 0 {
		return
	}
	landed, err := archive.Append(id, turns)
	if err != nil {
		e.warnArchiveFailed(err)
		return
	}
	if id == "" {
		// The append created a conversation; adopt its id so the thread keeps
		// appending to it — unless the thread this flush belonged to has been
		// reset, reopened, or lapsed in the meantime (the generation check),
		// in which case the new thread must not inherit the old record.
		e.mu.Lock()
		if e.archiveGen == gen && e.archiveID == "" {
			e.archiveID = landed
		}
		e.mu.Unlock()
	}
	e.log.Debug("conversation archived", "component", "session",
		"conversation_id", landed, "turns", len(turns)/2)
}

// SyncArchive is the archive's read-side write barrier: on return, every
// exchange committed before the call — which includes every turn whose
// session.finished a client has already seen, because commitTurn stages the
// exchange before finishLocked publishes the event — has been flushed to the
// archive, and a flush that created the conversation has adopted its id as
// the live thread's.
//
// It exists because the flush itself deliberately runs *after*
// session.finished, off the latency path (ADR 0011). That is right for the
// write side, but it leaves a window in which a client that just watched a
// turn finish can search or list the archive and not find it: the window's
// search box missing the conversation the user is in, or active_id coming
// back "" while the conversation it names is plainly on disk — because the
// append that creates a conversation is also what adopts its id, and both
// happen on the session tail after the event (issue #115; the
// TestConversationSearchOverSocket and TestConversationListOverSocket CI
// flakes were this window, held open by a starved runner). The daemon closes
// the window by calling this barrier before it reads, putting the wait on the
// reader who needs the guarantee instead of the speaker who does not.
//
// It is persistArchive under a public name, and that is the whole mechanism:
// archiveMu makes flushes whole, so either the tail's flush already ran (and
// adopted any new id) and this call finds nothing pending, or this call is
// blocked until an in-flight flush completes, or this call does the flush
// itself and the tail's later one becomes the no-op. A latched archive
// failure keeps the barrier a no-op too — degraded exactly like the write
// path, the reader sees whatever made it to disk before the disk broke.
func (e *Engine) SyncArchive() {
	e.persistArchive()
}

// flushArchiveDetached writes turns that belong to a thread the engine has
// already moved on from — the staged tail a reset or reopen found in flight.
// No id is adopted: the thread is over, the record just has to be complete.
func (e *Engine) flushArchiveDetached(archive conversations.Store, id string, turns []conversations.Turn) {
	if archive == nil || len(turns) == 0 || e.archiveFailed.Load() {
		return
	}
	e.archiveMu.Lock()
	defer e.archiveMu.Unlock()
	if _, err := archive.Append(id, turns); err != nil {
		e.warnArchiveFailed(err)
	}
}

// warnArchiveFailed latches the archive off after its first failed write and
// says so exactly once, mirroring persistHistory's degradation: a broken disk
// costs the record, never the conversation.
func (e *Engine) warnArchiveFailed(err error) {
	if e.archiveFailed.CompareAndSwap(false, true) {
		e.log.Warn("conversation could not be archived; continuing without archiving",
			"component", "session", "error", err.Error())
	}
}

// detachArchiveLocked ends the engine's association with the current archived
// conversation and returns what still has to be written to it. Callers hold
// e.mu and must pass the returned batch to flushArchiveDetached after
// releasing the lock — the conversation already archived stays on disk
// untouched; only the attachment ends.
func (e *Engine) detachArchiveLocked() (archive conversations.Store, id string, turns []conversations.Turn) {
	archive, id, turns = e.opts.Archive, e.archiveID, e.pendingArchive
	e.pendingArchive = nil
	e.archiveID = ""
	e.archiveGen++
	return archive, id, turns
}

// ActiveConversationID reports which archived conversation the live thread
// belongs to, "" when none. The daemon consults it before a delete: removing
// the conversation the user is *in* must also reset the thread, or the next
// turn would quietly rebuild a record they just destroyed.
func (e *Engine) ActiveConversationID() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.archiveID
}

// AdoptConversation installs an archived conversation as the active thread:
// an explicit user action (`jarvix conversations open`, the window's Resume).
// Follow-ups continue it with its context within the model budget — the most
// recent history_turns exchanges reach the model; the archive keeps
// everything — and new turns append to the same archived record.
//
// confs are the conversation's confirmation records (issue #118), restored
// alongside the turns they sat between: reopening a conversation brings back
// the approvals exactly like the questions and answers around them. Records
// anchored before the context window's cut fall away with the turns they
// belonged to — they remain in the archive, which keeps everything.
//
// Like ResetConversation it does not cancel a session in flight; a turn that
// completes after the switch commits into the adopted thread, which is what
// an explicit "continue this conversation" means.
// provs are the turns' provenance (issue #168), restored on the same terms:
// what went into an answer comes back with the answer, rebased to the context
// window's cut, and falls away with the turns the cap dropped — the archive
// keeps it all.
func (e *Engine) AdoptConversation(id string, msgs []ai.Message, confs []AdoptedConfirmation,
	provs []AdoptedProvenance) {
	e.mu.Lock()
	// Records the ending thread had not yet archived go with it, not with
	// the adopted one — the same rule reset applies.
	e.finalizeConfRecordsLocked()
	prevArchive, prevID, pending := e.detachArchiveLocked()
	if max := e.opts.HistoryTurns * 2; e.opts.HistoryTurns <= 0 {
		// Memory is disabled: no context can be carried, but the thread still
		// attaches to the archive so new turns extend the reopened record.
		e.history = nil
	} else if len(msgs) > max {
		e.history = append([]ai.Message(nil), msgs[len(msgs)-max:]...)
	} else {
		e.history = append([]ai.Message(nil), msgs...)
	}
	// The counter restarts at the adopted head's length; anchors arrive as
	// positions in the full archive record and are rebased to it, dropping
	// whatever the cap dropped.
	e.msgCount = len(e.history)
	dropped := len(msgs) - len(e.history)
	e.confRecords = nil
	e.provRecords = nil
	// With memory disabled nothing of the record is displayed, so nothing of
	// it is restored either — the archive still holds it all.
	if e.opts.HistoryTurns > 0 {
		for _, p := range provs {
			at := p.AfterMessages - dropped
			if at < 0 || p.Record == nil {
				continue
			}
			e.provRecords = append(e.provRecords, provRecord{at: at, rec: p.Record})
		}
		for _, c := range confs {
			after := c.AfterMessages - dropped
			if after < 0 {
				continue
			}
			e.confRecords = append(e.confRecords, &confirmationRecord{
				Confirmation: c.Record,
				summary:      c.Summary,
				at:           c.Time,
				afterMsgs:    after,
				staged:       true, // it came from the archive; it owes it nothing
			})
		}
	}
	// The clock restarts now: reopening is an explicit act, so the follow-up
	// window must not immediately expire a conversation last touched days ago.
	e.lastTurn = e.now()
	// Remembered tool approvals are scoped to the thread that earned them,
	// and this is a different thread.
	e.approvals = make(map[string]bool)
	e.archiveID = id
	archive := e.opts.Archive
	e.mu.Unlock()

	e.flushArchiveDetached(prevArchive, prevID, pending)
	if archive != nil {
		// Move the live-head pointer immediately: a daemon restarted before
		// the next turn must resume this conversation, not the previous one.
		if err := archive.SetActive(id); err != nil {
			e.log.Warn("could not record the reopened conversation",
				"component", "session", "error", err.Error())
		}
	}
	// Persist the adopted head at once, for the same restart: the reopen must
	// survive even if no follow-up is ever asked.
	e.persistHistory()
	e.log.Info("conversation reopened", "component", "session", "conversation_id", id)
	e.publish(Event{Type: "conversation.changed", Data: map[string]any{
		"reason": "opened", "conversation_id": id}})
}
