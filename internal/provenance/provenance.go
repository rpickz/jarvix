// Package provenance is the vocabulary of "what went into this answer"
// (issue #168): the references a turn collected while it was being assembled,
// and the two strengths that keep the claim honest.
//
// The whole feature rests on one rule, and this package exists to make it
// structural rather than a matter of prompt discipline: **attribution is
// mechanically derived, never model-reported.** Which retrieved fact a model
// actually leaned on is not knowable, and asking it to attribute invites
// invented citations — the exact failure the honesty rules exist for (#71).
// So nothing here is ever filled in from a model's words. Every reference is
// noted at the one point the daemon already knows it: the injection that put
// something in front of the model, or the tool call that ran and returned
// output during the turn.
//
// Two further rules shape the types:
//
//   - **References, never content.** A reference is an id, a name, or a path
//     — the handle that finds the thing again. It is deliberately not enough
//     to reconstruct what the thing said, so the archive never becomes a
//     second copy of the memory book, the feed cache, or a captured window.
//     The readable name is resolved from the live store at the moment
//     somebody looks, which is also what lets a source that no longer exists
//     say so instead of lying.
//   - **Bounded per turn.** A long tool round must not bloat the record, so
//     the list is capped and the overflow is disclosed rather than silently
//     dropped (the ADR 0037 stance applied to a different budget).
package provenance

import (
	"context"
	"sync"
)

// The source kinds. The string values travel on disk and on the wire, so they
// are one vocabulary everywhere — the record, the IPC payload, and the spoken
// answer all say the same word for the same thing.
const (
	// KindFact is a remembered fact (ADR 0025/0037); Ref is its short id.
	KindFact = "fact"
	// KindFeed is a knowledge feed (ADR 0031); Ref is its name, which is the
	// feed's identity.
	KindFeed = "feed"
	// KindVocabulary is a taught word or phrase (ADR 0042); Ref is its id.
	KindVocabulary = "vocabulary"
	// KindDesktop is a desktop capture source (ADR 0019); Ref is the source
	// word — "window", "selection", "clipboard". There is exactly one of
	// each, so the source *is* the specific item; naming the window's own
	// text instead would be storing the capture, which the transient rule of
	// ADR 0043/0047 forbids.
	KindDesktop = "desktop"
	// KindThread is a focus thread or the AI session anchored to it (ADR
	// 0041/0043/0047); Ref is the thread id.
	KindThread = "thread"
	// KindConversation is an archived conversation (ADR 0027); Ref is its id.
	KindConversation = "conversation"
	// KindArtifact is a file a tool produced; Ref is its path.
	KindArtifact = "artifact"
	// KindReminder is a one-shot reminder (ADR 0046); Ref is its id.
	//
	// It arrived with the situation report (#196, ADR 0061) rather than with
	// a turn: nothing injects a reminder into a model's context, so no answer
	// is ever attributed to one. What it exists for is the other half of this
	// vocabulary — a line in a report saying which thing it is about, so the
	// window can take the reader there through the resolver every other
	// source already goes through.
	KindReminder = "reminder"
	// KindSchedule is a scheduled routine or script (ADR 0032); Ref is
	// "routine:<name>" or "script:<name>", the two together because the two
	// namespaces are separate and a bare name would be ambiguous. Added with
	// KindReminder, for the same reason and on the same terms.
	KindSchedule = "schedule"
	// KindTool is any other tool call that returned output. Tool names the
	// tool and Subject its subject — never a query, because a query can quote
	// the very facts it searches for (the Activity pane's "query not shown"
	// rule, ADR 0037).
	KindTool = "tool"
)

// The two strengths. They are two different claims about the same answer and
// must never be blurred into one word: the whole point of this feature is
// that Jarvix says what it actually knows.
const (
	// Available means the reference was injected into the turn's context. We
	// know it was in front of the model. We do NOT know the model used it,
	// and nothing here will ever claim otherwise.
	Available = "available"
	// Returned means something ran during this turn and produced output that
	// went into the answer — a tool call, a capture. That is mechanically
	// causal, so it may be stated more strongly.
	Returned = "returned"
)

// The wording of each strength, in the words a person reads and hears. They
// live here, beside the constants they describe, so the two can never drift
// apart, and they are pinned by TestStrengthWordingIsPinned — the honesty
// line of #168 is a test, not a convention.
const (
	AvailablePhrase = "available to the answer"
	ReturnedPhrase  = "returned during this turn"
)

// Phrase is the human wording for a strength. An unrecognised strength gets
// the weaker phrase: overstating what Jarvix knows is the one failure this
// feature cannot afford, so the fallback is the cautious claim.
func Phrase(strength string) string {
	if strength == Returned {
		return ReturnedPhrase
	}
	return AvailablePhrase
}

// MaxSources bounds the references one turn may record. Twelve is chosen
// against the shape of a real turn rather than a round number: a pinned
// memory set and a couple of feeds are the ambient floor, and six tool rounds
// with a call or two each is the ceiling the engine already enforces
// (maxToolRounds). Past that the list stops being something a person reads
// and starts being something the archive carries — so it is cut, and the cut
// is disclosed.
const MaxSources = 12

// Reference is one thing that went into a turn. It is a handle, not a copy:
// what it points at is looked up when somebody asks, so nothing here holds
// the content of a fact, a feed value, a transcript, or a captured window.
type Reference struct {
	// Kind is one of the Kind* constants.
	Kind string `json:"kind"`
	// Strength is Available or Returned.
	Strength string `json:"strength"`
	// Ref is the handle the source is found by — a fact id, a feed name, a
	// thread id, a conversation id, a file path, a capture source word.
	Ref string `json:"ref,omitempty"`
	// Tool names the tool that returned this, on a KindTool reference and on
	// anything a tool produced. Empty on an injection.
	Tool string `json:"tool,omitempty"`
	// Subject is what a tool call was about, when the subject is itself a
	// reference and not content: the verbatim shell command (already the
	// record's currency, ADR 0014), an advisor's name. Never a search query.
	Subject string `json:"subject,omitempty"`
}

// key is the identity a reference is deduplicated by: the thing pointed at,
// not the route taken to it. A fact that was injected *and* returned by
// memory.search is one source, listed once — which is why the tool is
// deliberately not part of the identity of anything that has a Ref. A tool
// call has no Ref, so there its own name and subject are the identity.
func (r Reference) key() [3]string {
	if r.Ref != "" {
		return [3]string{r.Kind, r.Ref, ""}
	}
	return [3]string{r.Kind, r.Tool, r.Subject}
}

// Record is a turn's provenance as the archive and the wire carry it.
type Record struct {
	// Sources are the references, in the order the turn collected them:
	// injections first (they happen before the model is called), then tool
	// results in call order.
	Sources []Reference `json:"sources"`
	// Truncated counts references the cap left out. Disclosed rather than
	// silent, so a list that stops short says it stopped short.
	Truncated int `json:"truncated,omitempty"`
}

// Bound turns a turn's collected references into the record it stores:
// deduplicated, capped, and honest about what the cap removed. It returns nil
// when nothing was collected — a turn that consumed nothing retrievable
// carries no provenance at all, because absence is information and an empty
// affordance would be noise.
//
// The cap drops the weaker claim first. An Available reference says only that
// something was in front of the model; a Returned one says a tool ran and its
// output reached the answer. When only some can be kept, the mechanically
// causal ones are the ones worth keeping, so Available references leave from
// the end of the list before any Returned one does.
func Bound(refs []Reference) *Record {
	kept := make([]Reference, 0, len(refs))
	at := make(map[[3]string]int, len(refs))
	for _, r := range refs {
		if r.Kind == "" {
			continue
		}
		if i, seen := at[r.key()]; seen {
			// Same source twice: keep its first position, but let the
			// stronger claim win — a fact that was injected and then searched
			// for was genuinely returned during this turn.
			if r.Strength == Returned {
				kept[i].Strength = Returned
				if kept[i].Tool == "" {
					kept[i].Tool = r.Tool
				}
			}
			continue
		}
		at[r.key()] = len(kept)
		kept = append(kept, r)
	}
	if len(kept) == 0 {
		return nil
	}
	dropped := 0
	for len(kept) > MaxSources {
		i := lastIndexOfStrength(kept, Available)
		if i < 0 {
			i = len(kept) - 1 // nothing weak left; the tail goes
		}
		kept = append(kept[:i], kept[i+1:]...)
		dropped++
	}
	return &Record{Sources: kept, Truncated: dropped}
}

// lastIndexOfStrength finds the last reference with the given strength, or -1.
func lastIndexOfStrength(refs []Reference, strength string) int {
	for i := len(refs) - 1; i >= 0; i-- {
		if refs[i].Strength == strength {
			return i
		}
	}
	return -1
}

// Sink collects references during one unit of work. It carries its own mutex
// because a tool may report from whatever goroutine it runs on, while the
// turn that owns the sink drains it from its own.
type Sink struct {
	mu   sync.Mutex
	refs []Reference
}

// Note records a reference.
func (s *Sink) Note(r Reference) {
	if s == nil || r.Kind == "" {
		return
	}
	s.mu.Lock()
	s.refs = append(s.refs, r)
	s.mu.Unlock()
}

// Drain takes everything collected so far and empties the sink.
func (s *Sink) Drain() []Reference {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	refs := s.refs
	s.refs = nil
	return refs
}

// sinkKey is the context key the sink travels under.
type sinkKey struct{}

// WithSink returns a context that collects references reported through Note.
//
// A context is the transport because the alternative is worse: the things
// that know a reference the caller cannot derive — the artifact tool knows
// which file it actually wrote after de-duplicating the name, the
// conversation search knows which conversations answered — are reached
// through interfaces whose signatures belong to what they do, not to this
// feature. Threading a collector through every one of them would make every
// tool's contract mention provenance; a context lets exactly the two that
// have something to say say it, and costs the rest nothing.
func WithSink(ctx context.Context, s *Sink) context.Context {
	if s == nil {
		return ctx
	}
	return context.WithValue(ctx, sinkKey{}, s)
}

// Note reports a reference to the sink on ctx, if there is one. Safe to call
// from anywhere: with no sink installed it does nothing, so a tool run
// outside a turn — a CLI invocation, a test — is unaffected.
func Note(ctx context.Context, r Reference) {
	if ctx == nil {
		return
	}
	if s, ok := ctx.Value(sinkKey{}).(*Sink); ok {
		s.Note(r)
	}
}
