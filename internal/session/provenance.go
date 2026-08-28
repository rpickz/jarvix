package session

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/knowledge"
	"github.com/rpickz/jarvix/internal/memory"
	"github.com/rpickz/jarvix/internal/provenance"
	"github.com/rpickz/jarvix/internal/tools"
	"github.com/rpickz/jarvix/internal/vocabulary"
)

// This file is the engine half of answer provenance (issue #168). It is
// deliberately the *only* place a turn's sources are decided, and it decides
// them from what the daemon did — never from what the model said.
//
// The collection points are the points that already know:
//
//   - the four gather* functions, each of which has just been handed the
//     exact list of things it put in front of the model (memory.go,
//     vocabulary.go, knowledge.go, context.go). Those are Available: we know
//     they were in context, not that they were used.
//   - the tool loop in think(), where a call has just returned. Those are
//     Returned: something ran and its output reached the answer.
//
// Nothing else may add a source, and nothing anywhere asks the model to
// attribute anything. That is the honesty line the whole ticket is built on:
// which retrieved fact a model actually leaned on is not knowable, and asking
// it invites invented citations (#71).

// noteSources records references collected for this turn. Appends happen on
// the turn's own goroutine; the lock is for the commit paths that read.
func (s *sess) noteSources(refs ...provenance.Reference) {
	if len(refs) == 0 {
		return
	}
	s.provMu.Lock()
	s.prov = append(s.prov, refs...)
	s.provMu.Unlock()
}

// provenanceRecord is what this turn will carry into the record: bounded,
// deduplicated, and nil when the turn consumed nothing retrievable — absence
// is information, and a turn that used nothing must show nothing at all.
func (s *sess) provenanceRecord() *provenance.Record {
	s.provMu.Lock()
	defer s.provMu.Unlock()
	return provenance.Bound(s.prov)
}

// ProvenanceLister is the engine's view of the spoken source listing for the
// deterministic "where did that come from?" phrase (issue #168). It returns
// the sentence to speak, already composed, on the ApprovalsLister contract —
// the engine speaks the string and words nothing itself, so the window's
// panel and the spoken answer read the same list through one composer and
// cannot end up describing a source two ways.
//
// The record is handed in rather than looked up: what went into the last
// answer is engine state, and the seam stays a pure composer with no reach
// into the thread.
type ProvenanceLister interface {
	SpokenProvenance(rec *provenance.Record) (spoken string, err error)
}

// runProvenanceList carries out a matched "where did that come from".
//
// The empty case is an answer, not a failure: a thread whose last answer used
// nothing retrievable has nothing to list, and saying so is the whole point —
// the alternative is a machine that always finds something to cite.
func (e *Engine) runProvenanceList() (ack string, runErr error) {
	if e.opts.Provenance == nil {
		return "", fmt.Errorf("I cannot look up where my answers came from on this daemon")
	}
	rec, _ := e.LastProvenance()
	return e.opts.Provenance.SpokenProvenance(rec)
}

// provRecord anchors one assistant turn's provenance to its position in the
// thread. It is the confirmation record's arrangement (issue #118) applied to
// a different payload, and for the same reason: the live view is rebuilt from
// e.history, which holds messages and nothing else, so anything that rides a
// turn without being part of its text has to be anchored to a monotonic
// message counter — otherwise the retention cap trimming the head would slide
// every record onto the wrong turn.
type provRecord struct {
	// at is the global index of the assistant message this describes.
	at  int
	rec *provenance.Record
}

// noteProvenanceLocked anchors a committed turn's provenance to its assistant
// message. Callers hold e.mu, and call it from the one place an exchange
// enters the record. A nil record anchors nothing: a turn that consumed
// nothing carries no provenance, on the wire or on disk.
func (e *Engine) noteProvenanceLocked(rec *provenance.Record) {
	if rec == nil || e.opts.HistoryTurns <= 0 {
		return
	}
	// msgCount still points at the user half of the exchange being committed,
	// so the assistant half is the next index.
	e.provRecords = append(e.provRecords, provRecord{at: e.msgCount + 1, rec: rec})
}

// pruneProvenanceLocked drops provenance whose turn has fallen out of the
// retention window — the confirmation records' rule exactly: the exchange is
// no longer displayed, so neither is what went into it. The archive keeps it
// all; the cap governs what the model is sent and what the live view shows,
// never what is kept (ADR 0027).
func (e *Engine) pruneProvenanceLocked() {
	base := e.msgCount - len(e.history)
	kept := e.provRecords[:0]
	for _, r := range e.provRecords {
		if r.at < base {
			continue
		}
		kept = append(kept, r)
	}
	e.provRecords = kept
}

// provenanceAtLocked reports the provenance anchored to a message index.
func (e *Engine) provenanceAtLocked(at int) *provenance.Record {
	for _, r := range e.provRecords {
		if r.at == at {
			return r.rec
		}
	}
	return nil
}

// LastProvenance reports the most recently committed turn's provenance — what
// "where did that come from?" answers about (issue #168). ok is false when the
// thread holds no answer with provenance, which is the honest empty: a turn
// that consumed nothing retrievable has nothing to list.
func (e *Engine) LastProvenance() (rec *provenance.Record, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.provRecords) == 0 {
		return nil, false
	}
	return e.provRecords[len(e.provRecords)-1].rec, true
}

// AdoptedProvenance is one turn's provenance restored from an archived
// conversation for AdoptConversation — the provenance half of what
// adoptableMessages extracts (issue #168), shaped like AdoptedConfirmation.
type AdoptedProvenance struct {
	Record *provenance.Record
	// AfterMessages is how many adopted messages precede the assistant
	// message this belongs to.
	AfterMessages int
}

// memorySources names the remembered facts a turn was given. The fact's id is
// the whole reference: its content stays in the memory book, which is where
// the Memory tab reads it from when the user follows the link, and where it
// gets deleted from when the user forgets it — so a forgotten fact can be
// reported as forgotten instead of quoted back from a stale copy.
func memorySources(inj memory.Injection) []provenance.Reference {
	refs := make([]provenance.Reference, 0, len(inj.Facts))
	for _, f := range inj.Facts {
		refs = append(refs, provenance.Reference{
			Kind: provenance.KindFact, Strength: provenance.Available, Ref: f.ID,
		})
	}
	return refs
}

// vocabularySources names the taught words a turn was given, by id, on the
// same terms as the facts above.
func vocabularySources(inj vocabulary.Injection) []provenance.Reference {
	refs := make([]provenance.Reference, 0, len(inj.Entries))
	for _, e := range inj.Entries {
		refs = append(refs, provenance.Reference{
			Kind: provenance.KindVocabulary, Strength: provenance.Available, Ref: e.ID,
		})
	}
	return refs
}

// knowledgeSources names the feeds whose values a turn was given. A feed's
// name is its identity, so here the name *is* the reference; the value is not
// recorded, and never was going to be.
func knowledgeSources(inj knowledge.Injection) []provenance.Reference {
	refs := make([]provenance.Reference, 0, len(inj.Names))
	for _, name := range inj.Names {
		refs = append(refs, provenance.Reference{
			Kind: provenance.KindFeed, Strength: provenance.Available, Ref: name,
		})
	}
	return refs
}

// contextSources names the desktop capture sources a turn was given.
//
// The reference is the source word, not the window. That is the honest limit
// of what may be recorded: for the active-window source the capture's whole
// text *is* the window's identity line (class — title), so writing a name
// here would be writing the capture into the archive, which the transient
// rule of ADR 0043/0047 forbids. There is exactly one active window, one
// selection and one clipboard, so the source is the specific item — the
// category would have been "desktop context", and this names the three
// separately instead.
func contextSources(snap desktop.Snapshot) []provenance.Reference {
	refs := make([]provenance.Reference, 0, len(snap.Items))
	for _, item := range snap.Items {
		refs = append(refs, provenance.Reference{
			Kind: provenance.KindDesktop, Strength: provenance.Available, Ref: string(item.Source),
		})
	}
	return refs
}

// toolSource derives the reference for one completed tool call from the call
// itself — the tool's name and, where the subject is a reference rather than
// content, its subject.
//
// reported are the references the tool noted about itself through the
// provenance sink. When a tool has something to say that the arguments cannot
// — which file artifact.create actually wrote after de-duplicating the name,
// which conversations a search matched — that is the answer, and it replaces
// the generic line rather than adding to it: "the artifact q3-chart.png" is
// what the user wants to press, and "artifact.create" beside it is noise.
//
// A failed call contributes nothing. `result` carries the engine's own
// error convention, and a call that returned an error returned no output —
// saying it went into the answer would be the overstatement this feature
// exists to avoid.
func toolSource(call ai.ToolCall, result string, reported []provenance.Reference) []provenance.Reference {
	if strings.HasPrefix(result, "error: ") {
		return nil
	}
	if len(reported) > 0 {
		out := make([]provenance.Reference, 0, len(reported))
		for _, r := range reported {
			r.Strength = provenance.Returned
			if r.Tool == "" {
				r.Tool = call.Name
			}
			out = append(out, r)
		}
		return out
	}
	// A feed the model read or refreshed is a feed, not an anonymous tool
	// call: it navigates to the Knowledge tab exactly like an injected one,
	// and only its strength differs.
	if call.Name == tools.KnowledgeGetToolName || call.Name == tools.KnowledgeRefreshToolName {
		if feed := stringArg(call.Arguments, "feed"); feed != "" {
			return []provenance.Reference{{
				Kind: provenance.KindFeed, Strength: provenance.Returned,
				Ref: feed, Tool: call.Name,
			}}
		}
	}
	return []provenance.Reference{{
		Kind: provenance.KindTool, Strength: provenance.Returned,
		Tool: call.Name, Subject: toolSubject(call),
	}}
}

// toolSubject is what a call was about, when that is a reference and not
// content. The list is short on purpose, and everything absent from it is
// absent for a reason:
//
//   - shell.run's command is the record's existing currency — the permission
//     card shows it verbatim and ADR 0039 stores it verbatim — so naming it
//     here reveals nothing the archive does not already hold.
//   - an advisor's name is a name.
//   - a *query* is never a subject. memory.search and conversations.search
//     take queries, and a query can quote the very fact it is looking for —
//     the reason the Activity pane already refuses to show them (ADR 0037).
//     Those calls are named by their tool and nothing else, and the daemon
//     words them as "your remembered facts" and "your earlier conversations".
func toolSubject(call ai.ToolCall) string {
	switch call.Name {
	case tools.ShellToolName:
		return stringArg(call.Arguments, "command")
	case tools.AdvisorToolName:
		return stringArg(call.Arguments, "advisor")
	}
	return ""
}

// stringArg pulls one string field out of a model's raw tool arguments.
// Arguments come from a model, so anything unparseable is simply absent —
// a malformed call still ran and still gets its tool-named source.
func stringArg(arguments, field string) string {
	if arguments == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(arguments), &m); err != nil {
		return ""
	}
	s, _ := m[field].(string)
	return strings.TrimSpace(s)
}
