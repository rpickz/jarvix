package intent

import (
	"strings"

	"github.com/rpickz/jarvix/internal/ai"
)

// This file is the router's half of the thinking level (issue #159, ADR
// 0062): the spoken equivalents of the window's Quick / Balanced / Deep
// control.
//
// There are two mechanisms here, and the difference between them is the
// difference between a setting and a request.
//
//   - **Pins** are whole utterances and go through the pattern table like
//     every other built-in: "stay on the deep model" says nothing else, so it
//     is claimed, executed and acknowledged with no model call at all. It
//     moves the same conversation-scoped level the window's control moves —
//     one place the setting lives, three ways to reach it.
//
//   - **Escalations** are prefixes of a question that still has to be
//     answered: "think hard about this, what's the best way to…". The pattern
//     table cannot express those and must not try. ADR 0017 made whole-
//     utterance matching the router's central guarantee, and loosening it to
//     prefixes would put every sentence beginning with a table phrase at risk
//     of being claimed. So an escalation is a *separate, additive* scan that
//     claims nothing: it annotates the turn with a tier and the utterance goes
//     to the model exactly as it always would have, one map lookup later.
//
// The utterance is deliberately **not** stripped of the escalation phrase.
// "Think hard about this" is a legitimate instruction to a model as well as to
// the router, the archive should hold what the user actually said, and a strip
// is one more transformation between a person's words and the record of them.
//
// Deliberately absent: "ask claude". It names an advisor, not a tier — the two
// coincide only when the deep tier happens to be that advisor — and the model
// already has advisor.ask for exactly that request (ADR 0016). A phrase that
// silently meant "the deep tier, whatever that is today" would be the kind of
// near-miss this table exists to avoid.

// ThinkingIntentName identifies the spoken thinking-level pins.
const ThinkingIntentName = "thinking.set"

// thinkingPins is the whole-utterance grammar. Short and literal, like every
// other family: a near-synonym is a code change with a test, and an utterance
// this table does not claim reaches the model, which will handle it perfectly
// well.
//
// Every phrase names the tier by its product word (quick / balanced / deep)
// rather than by a model name, because the level is a trade the user is
// choosing and not a model they are selecting — and because a phrase naming a
// model would rot the moment they changed one.
var thinkingPins = []struct {
	tier     ai.Tier
	patterns []string
}{
	{
		tier: ai.TierDeep,
		patterns: []string{
			"stay on the deep model",
			"use the deep model",
			"switch to the deep model",
			"switch to deep",
			"think hard from now on",
			"deep answers from now on",
		},
	},
	{
		tier: ai.TierMedium,
		patterns: []string{
			"use the balanced model",
			"switch to the balanced model",
			"switch to balanced",
			"back to the balanced model",
			"back to normal answers",
			"balanced answers from now on",
		},
	},
	{
		tier: ai.TierInstant,
		patterns: []string{
			"stay on the quick model",
			"use the quick model",
			"switch to the quick model",
			"switch to quick",
			"quick answers from now on",
		},
	},
}

// thinkingEscalations are the per-turn prefixes. They must be *prefixes of a
// longer utterance*: an utterance that is nothing but the phrase is a sentence
// with no question in it, and claiming a tier for a turn that then asks
// nothing would be routing on a technicality.
//
// The quick list is short on purpose. "Quickly" is an explicit request for
// speed and belongs here; "quick question" is a filler people say before hard
// questions and does not.
var thinkingEscalations = []struct {
	tier   ai.Tier
	phrase string
}{
	{ai.TierDeep, "think hard about this"},
	{ai.TierDeep, "think hard about"},
	{ai.TierDeep, "think carefully about"},
	{ai.TierDeep, "think about this properly"},
	{ai.TierDeep, "take your time"},
	{ai.TierDeep, "give this some thought"},
	{ai.TierInstant, "quick answer"},
	{ai.TierInstant, "short answer"},
	{ai.TierInstant, "just quickly"},
	{ai.TierInstant, "quickly"},
}

// TurnTier reports the tier one utterance asked for, for this turn only.
//
// It is not part of Match and does not go through the pattern table: a hit
// claims nothing, and the caller sends the utterance to the model regardless.
// Matching is whole-word and anchored at the start — the same strictness the
// table uses, minus the requirement to consume the whole utterance, which is
// the one thing an escalation cannot do by definition.
//
// The longest matching phrase wins, so "think hard about this" is not shadowed
// by "think hard about"; without that the shorter entry would decide and the
// two would be indistinguishable in a test.
func TurnTier(utterance string) (ai.Tier, bool) {
	words := normalize(utterance)
	if len(words) == 0 {
		return ai.TierNone, false
	}
	best, bestLen := ai.TierNone, 0
	for _, e := range thinkingEscalations {
		phrase := strings.Fields(e.phrase)
		// Strictly a prefix: there must be a question left after it.
		if len(phrase) >= len(words) || len(phrase) <= bestLen {
			continue
		}
		match := true
		for i, w := range phrase {
			if words[i] != w {
				match = false
				break
			}
		}
		if match {
			best, bestLen = e.tier, len(phrase)
		}
	}
	return best, best != ai.TierNone
}

// ThinkingPhrases lists every whole-utterance pin phrase, for the collision
// map and for documentation. Sorted by nothing in particular — the router
// indexes them by first word.
func ThinkingPhrases() []string {
	var out []string
	for _, group := range thinkingPins {
		out = append(out, group.patterns...)
	}
	return out
}
