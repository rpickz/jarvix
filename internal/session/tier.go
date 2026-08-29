package session

import (
	"fmt"
	"strings"

	"github.com/rpickz/jarvix/internal/ai"
)

// This file is the engine's half of model tiers (issue #159, ADR 0063): the
// resolved bindings, the per-conversation thinking level, and the two moments
// Jarvix has to say something about which model answered.
//
// The routing decision itself is not here — it is ai.Decide, a pure table with
// no engine in it. What is here is everything that decision needs to become a
// provider call: which client and model each tier resolves to, how much
// conversation a tier is sent, and what is said when the tier somebody asked
// for cannot answer.
//
// The whole file is inert when tiering is off. An engine built without a
// TierSet takes the same path through think() it always did, with the same
// provider, the same model and the same messages — which is what
// TestNoTiersConfiguredIsTodaysTurnExactly pins, byte for byte on the wire.

// TierBinding is one resolved tier: what actually answers a turn routed to it.
//
// It is resolved once, when the daemon builds the engine's options, and never
// re-derived per turn. A tier is a client and a model name, not a lookup.
type TierBinding struct {
	// Provider is the client this tier calls. Never nil in a usable binding.
	Provider ai.Provider
	// Model is the model name to ask that provider for.
	Model string
	// Advisor names the CLI behind an advisor-backed tier (ADR 0016), empty
	// for an endpoint-backed one. It is carried for two reasons: the record
	// has to say what actually answered, and an advisor cannot call tools —
	// see Tools below.
	Advisor string
	// HistoryTurns is this tier's own conversation budget, tighter than
	// conversation.history_turns. 0 means the conversation's own budget.
	HistoryTurns int
}

// Tools reports whether a turn served by this tier may carry tool
// definitions.
//
// An advisor-backed tier cannot: the bridge runs a one-shot CLI and hands its
// prose back, with no mechanism by which it could call anything of Jarvix's.
// Offering it tools it cannot use is how a model comes to describe work it
// never did — the #71 shape again — so the tools are withheld and the prompt
// says plainly that this tier cannot act on the machine.
func (b TierBinding) Tools() bool { return b.Advisor == "" }

// TierSet is the engine's whole tier configuration: the bindings, and which
// tier a conversation starts on.
type TierSet struct {
	// Default is the tier a new conversation begins at ([ai.tiers] default).
	Default ai.Tier
	// Bindings holds one entry per *configured* tier. Medium is always
	// present when tiering is on — an absent [ai.tiers.medium] binds to the
	// [ai] brain, which is what medium means — while instant and deep are
	// present only when their table is. An absent instant or deep does not
	// exist, and asking for one is answered by saying so.
	Bindings map[ai.Tier]TierBinding
}

// Enabled reports whether this engine routes at all.
func (s TierSet) Enabled() bool { return len(s.Bindings) > 0 }

// available is the routing table's view of which tiers can serve.
func (s TierSet) available() map[ai.Tier]bool {
	out := make(map[ai.Tier]bool, len(s.Bindings))
	for tier := range s.Bindings {
		out[tier] = true
	}
	return out
}

// serving is one turn's answer to "which model is answering this". It is
// built before the first provider call and carried through the tool loop, so
// every round of a turn is served by the same tier and the record can say so
// without reconstructing anything.
type serving struct {
	// on is whether tiering is configured at all. Everything below is
	// meaningless when it is false, and every caller checks it first.
	on bool
	// route is what ai.Decide said, including why and what it refused.
	route ai.Route
	// binding is the resolved tier. Zero when on is false.
	binding TierBinding
	// unreachable is the tier this turn tried and could not reach, TierNone
	// until one actually fails. It is what turns a downgrade into a sentence.
	unreachable ai.Tier
	// fallback is the tier a failover lands on — the configured default, or
	// medium. Never instant: a failover happens on a turn already in flight,
	// and the tool rule admits no exception at any point in one.
	fallback ai.Tier
	// failedOver latches once a turn has already changed tier, so an
	// unreachable fallback cannot start a chain.
	failedOver bool
	// contextDropped is how many prior exchanges this tier's tighter budget
	// left out of the prompt. Disclosed, never silent (ADR 0037).
	contextDropped int
}

// model is the model name to send, falling back to the engine's own when
// tiering is off or an advisor-backed tier has no model name of its own.
func (s serving) model(fallback string) string {
	if s.on && s.binding.Model != "" {
		return s.binding.Model
	}
	return fallback
}

// provider is the client to call, falling back to the engine's own brain.
func (s serving) provider(fallback ai.Provider) ai.Provider {
	if s.on && s.binding.Provider != nil {
		return s.binding.Provider
	}
	return fallback
}

// servedModel is what the record says answered: the model for an endpoint
// tier, the advisor's name for an advisor tier. Never the model that was
// asked for when a different one answered — the ledger elsewhere in this
// project makes the same distinction, and for the same reason.
func (s serving) servedModel(fallback string) string {
	if s.on && s.binding.Advisor != "" {
		return "advisor " + s.binding.Advisor
	}
	return s.model(fallback)
}

// beginTier decides which tier serves this turn and records it as the phase's
// tier, so the pending indicator can say which model is being waited on from
// the moment the wait starts (#158's surface).
//
// toolsAttached is the whole reason this takes an argument at all: it is
// computed in think() from the tool definitions the request will carry, and
// handing it to ai.Decide is what makes the never-instant-with-tools rule a
// property of the routing table rather than a comment somebody could forget.
func (e *Engine) beginTier(s *sess, toolsAttached bool) serving {
	e.mu.Lock()
	defer e.mu.Unlock()
	set := e.opts.Tiers
	if !set.Enabled() {
		return serving{}
	}
	route := ai.Decide(ai.RouteInput{
		Available: set.available(),
		Default:   set.Default,
		// The raw pin, not the effective level: an unpinned conversation must
		// reach the table as unpinned, or every turn would record itself as
		// pinned and the reason would stop meaning anything.
		Pinned:        e.thinking,
		Asked:         s.askedTier,
		ToolsAttached: toolsAttached,
	})
	// Where a failover lands. Never instant: a failover happens on a turn
	// already in flight, whose tools were decided before it started, and the
	// rule that instant never holds tools has no exception at any point in a
	// turn.
	fallback := set.Default
	if _, ok := set.Bindings[fallback]; !ok || fallback == ai.TierInstant {
		fallback = ai.TierMedium
	}
	out := serving{
		on:       true,
		route:    route,
		binding:  set.Bindings[route.Tier],
		fallback: fallback,
	}
	e.servingTier, e.servingModel = out.route.Tier, out.servedModel(e.opts.Model)
	return out
}

// failOverTier moves a turn that could not reach its tier onto the one Jarvix
// can reach, and reports whether it did.
//
// It refuses in the two cases where moving would be worse than stopping: when
// the turn was already on the fallback (there is nowhere honest left to go),
// and when this turn has failed over once already (a chain of downgrades is
// how a user ends up several models away from what they asked for, told once).
func (e *Engine) failOverTier(s *sess, out *serving, cause error, speaker *streamingSpeaker) bool {
	if !out.on || out.failedOver || out.route.Tier == out.fallback {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	binding, ok := e.opts.Tiers.Bindings[out.fallback]
	if !ok {
		return false
	}
	e.log.Warn("model tier unreachable, failing over", "component", "assistant",
		"session_id", s.id, "tier", string(out.route.Tier), "to", string(out.fallback),
		"error", cause.Error())
	out.unreachable = out.route.Tier
	out.failedOver = true
	out.route.Tier = out.fallback
	out.route.Wanted = out.unreachable
	out.route.Reason = ai.ReasonUnreachable
	out.binding = binding
	e.servingTier, e.servingModel = out.route.Tier, out.servedModel(e.opts.Model)
	return true
}

// tierRequest builds the provider request for the serving tier. With tiering
// off every field resolves to what it resolved to before tiers existed, which
// is the byte-identity guarantee stated as code.
func (e *Engine) tierRequest(out serving, messages []ai.Message, defs []ai.ToolDef) ai.ChatRequest {
	return ai.ChatRequest{
		Model:       out.model(e.opts.Model),
		MaxTokens:   e.opts.MaxTokens,
		Temperature: e.opts.Temperature,
		Messages:    messages,
		Tools:       defs,
	}
}

// tierPrompt shapes a turn's prompt for the tier that will serve it: the
// tier's own context budget, and whether it is offered tools at all.
//
// Returned rather than mutated so the caller can rebuild both after a
// failover — a context budget and a tool surface are part of what a tier *is*,
// and a turn that changed tier mid-flight must not keep the old one's prompt.
func (e *Engine) tierPrompt(out *serving, msgs []ai.Message, defs []ai.ToolDef) ([]ai.Message, []ai.ToolDef) {
	if !out.on {
		return msgs, defs
	}
	trimmed, dropped := trimForTier(msgs, out.binding.HistoryTurns)
	out.contextDropped = dropped
	var notes []string
	if dropped > 0 {
		notes = append(notes, tierContextNote(dropped))
	}
	if !out.binding.Tools() {
		defs = nil
		notes = append(notes, advisorTierNote)
	}
	return insertSystemNotes(trimmed, notes), defs
}

// insertSystemNotes puts each note in as a system message immediately before
// the user's question — the position the desktop capture and the feed values
// already occupy, so a note about this turn sits with the rest of this turn's
// standing material rather than at the top with the identity.
func insertSystemNotes(msgs []ai.Message, notes []string) []ai.Message {
	if len(notes) == 0 || len(msgs) == 0 {
		return msgs
	}
	out := make([]ai.Message, 0, len(msgs)+len(notes))
	out = append(out, msgs[:len(msgs)-1]...)
	for _, note := range notes {
		out = append(out, ai.Message{Role: ai.RoleSystem, Content: note})
	}
	return append(out, msgs[len(msgs)-1])
}

// tierNotice is the one sentence, if any, this turn owes the user about which
// model is answering it. Empty is the common case and the right one: a turn
// served by the tier the user is set to needs no commentary.
//
// Three things earn a sentence, and nothing else does:
//
//   - a tier that was tried and could not be reached,
//   - a tier that was asked for and is not configured at all,
//   - a deliberate trip to the deep tier, which is worth warning about once.
//
// Deliberately *not* on the list: the tool rule turning instant into medium.
// That fires on a large share of ordinary turns, and a speed control that
// apologised every time the user asked a question needing a tool would be
// noise within an afternoon. It is recorded instead, and the pending turn
// names the tier that actually answered, which is the disclosure that matters.
func (e *Engine) tierNotice(out serving) string {
	switch {
	case !out.on:
		return ""
	case out.unreachable != ai.TierNone:
		return tierUnreachableLine(out.unreachable, out.route.Tier)
	case out.route.Reason == ai.ReasonUnavailable:
		return tierUnavailableLine(out.route.Wanted, out.route.Tier)
	case out.route.Tier == ai.TierDeep && out.route.Reason == ai.ReasonAsked:
		return DeepThinkingCue
	}
	return ""
}

// assistantStartedData is the assistant.started payload. The tier keys are
// additive and absent when tiering is off, so a client that predates them sees
// exactly the payload it always saw.
func (e *Engine) assistantStartedData(s *sess, out serving) map[string]any {
	data := map[string]any{
		"session_id": s.id,
		"provider":   out.provider(e.provider).Name(),
	}
	if out.on {
		data["tier"] = string(out.route.Tier)
		data["tier_label"] = ai.TierLabel(out.route.Tier)
		data["model"] = out.servedModel(e.opts.Model)
	}
	return data
}

// noteServedTier writes which tier answered into the turn's timings, where
// `jarvix status --last` and the activity feed both read it. Nothing is
// written when tiering is off: a record key that appeared on every turn saying
// "medium" would claim a routing decision nobody made.
func (e *Engine) noteServedTier(s *sess, out serving) {
	if !out.on {
		return
	}
	s.timings.noteTier(string(out.route.Tier), out.servedModel(e.opts.Model),
		string(out.route.Reason), string(out.route.Wanted), out.contextDropped)
}

// Thinking levels: the spoken and written vocabulary
// ---------------------------------------------------------------------------

// tierPhrase is the tier named inside a sentence, lower case.
func tierPhrase(tier ai.Tier) string { return strings.ToLower(ai.TierLabel(tier)) }

// DeepThinkingCue is said once, before the answer, when a turn has been sent
// to the deep tier because the user asked for it. Once — not a countdown and
// not a progress narration: the point is that the wait was chosen, and being
// reminded of it every ten seconds would make a deliberate wait feel like a
// hang. ADR 0016 settled the same question for advisor consultations.
const DeepThinkingCue = "Thinking about that properly. This will take a moment."

// tierUnavailableLine is what Jarvix says when the tier somebody asked for is
// not configured at all. It names the tier that was wanted and the one that
// answered, because the failure this feature must never have is an answer that
// quietly came from somewhere else.
func tierUnavailableLine(wanted, served ai.Tier) string {
	return fmt.Sprintf("I have no %s model configured, so this is the %s one's answer.",
		tierPhrase(wanted), tierPhrase(served))
}

// tierUnreachableLine is what Jarvix says when the tier it tried could not be
// reached — no key, nothing listening, an advisor that is not installed. Same
// shape as the sentence above and deliberately so: from where the user sits,
// "not configured" and "not answering" are one disappointment with two causes,
// and both have to be *said*.
func tierUnreachableLine(wanted, served ai.Tier) string {
	return fmt.Sprintf("I couldn't reach the %s model, so this is the %s one's answer.",
		tierPhrase(wanted), tierPhrase(served))
}

// tierContextNote is the system line a tier with a tighter budget carries when
// that budget actually left something out. The model is told what it is
// missing rather than silently handed a shorter conversation — ADR 0037's
// stance, applied to a new budget: a prompt that has been trimmed must say so
// inside itself, or the answer's confidence outruns its material.
func tierContextNote(dropped int) string {
	exchange := "exchange"
	if dropped != 1 {
		exchange = "exchanges"
	}
	return fmt.Sprintf("Note: %d earlier %s of this conversation %s left out of this prompt "+
		"to keep the answer fast. If the question depends on something you cannot see, say so "+
		"rather than guessing.", dropped, exchange, map[bool]string{true: "was", false: "were"}[dropped == 1])
}

// advisorTierNote is the system line an advisor-backed tier carries. It exists
// because that tier holds no tools: without being told, a model that knows
// Jarvix can act on the desktop will happily say it has done so.
const advisorTierNote = "You are answering as a consulted specialist. You cannot run tools or " +
	"act on this computer in this reply, so never say that you have done anything to it."

// trimForTier cuts a turn's prompt down to a tier's own context budget and
// says how much it removed.
//
// It trims *conversation history* and nothing else. The system prompt, the
// remembered facts, the taught vocabulary, the desktop capture and the feed
// values all stay: they are the standing knowledge that makes an answer
// Jarvix's rather than a stranger's, they are already individually budgeted,
// and dropping them to save tokens is how a fast tier becomes a tier that does
// not know the user's name. History is the part that grows without bound, so
// history is the part a latency budget takes from.
//
// budget is in exchanges, matching conversation.history_turns; 0 or less means
// no tier budget and the messages are returned untouched.
func trimForTier(msgs []ai.Message, budget int) (out []ai.Message, dropped int) {
	if budget <= 0 {
		return msgs, 0
	}
	// The history is the run of non-system messages between the leading
	// system block and the trailing user turn — exactly what
	// conversationMessages appends from e.history. Counting it here rather
	// than taking it as a parameter keeps this function total over any
	// message list, which is what lets it be tested on its own.
	first, last := -1, -1
	for i, m := range msgs {
		if m.Role == ai.RoleSystem {
			continue
		}
		if first < 0 {
			first = i
		}
		last = i
	}
	if first < 0 || last <= first {
		return msgs, 0
	}
	// The final user turn is not history; everything before it is.
	history := msgs[first:last]
	keep := budget * 2
	if len(history) <= keep {
		return msgs, 0
	}
	drop := len(history) - keep
	out = make([]ai.Message, 0, len(msgs)-drop)
	out = append(out, msgs[:first]...)
	out = append(out, history[drop:]...)
	out = append(out, msgs[last:]...)
	// Reported in exchanges, the unit the budget is written in. An odd
	// dangling message rounds up: half an exchange is still something the
	// model cannot see.
	return out, (drop + 1) / 2
}

// runThinking performs a spoken thinking-level pin (#159). It is the engine
// half of intent.ThinkingIntentName: the router decided the utterance was a
// pin and which tier it named, and everything about whether that tier exists
// on this machine is decided here, by the one call the window's control also
// makes.
//
// A refusal is an acknowledgement, not an error: the session completes
// normally and Jarvix says in one line that the level the user asked for is
// not configured. That is the same asymmetry the approvals listing draws — a
// question about what Jarvix can do deserves an answer, never a failure.
func (e *Engine) runThinking(tier ai.Tier) (string, error) {
	level, err := e.SetThinking(tier)
	if err != nil {
		return capitalise(err.Error()) + ".", nil
	}
	return ai.TierLabel(level) + " answers.", nil
}

// capitalise upper-cases the first letter of a spoken sentence. The errors
// this file hands to the voice are written lower case because they are also
// Go errors (and the linter's ST1005 exception exists for the ones that are
// not); a sentence Jarvix says aloud reads as a sentence on screen too.
func capitalise(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// AvailableTiers lists the tiers this engine can actually serve, weakest
// first, empty when tiering is off. It is what the window's control is drawn
// from: the levels the *running* engine holds bindings for, rather than the
// levels a file mentions, so a control can never offer something a reload has
// not applied yet.
func (e *Engine) AvailableTiers() []ai.Tier {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []ai.Tier
	for _, tier := range ai.TierOrder() {
		if _, ok := e.opts.Tiers.Bindings[tier]; ok {
			out = append(out, tier)
		}
	}
	return out
}
