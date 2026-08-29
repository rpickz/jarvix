package ai

// This file is the model-tier vocabulary and the routing table that picks one
// (issue #159, ADR 0063).
//
// It lives in this package, beside the Provider interface, for two reasons.
// The tier is a property of the *request* — which model, at which endpoint,
// answers this turn — so it belongs with ChatRequest rather than with the
// session machinery that happens to be the first caller. And this package
// imports nothing but the standard library, which is what lets the routing
// table be tested exhaustively without an engine, a config file, or a socket
// anywhere near it.
//
// The table decides. It costs no model call, no network round trip and no
// classifier: every input below is either configuration or something the
// daemon already knows by the time the prompt is assembled. That is deliberate
// — a pre-classification round trip to decide which model answers would spend
// the very latency the instant tier exists to save.

// Tier names one of the three model tiers. The vocabulary is fixed at three:
// the user's own framing (an immediate model, today's model, the strongest
// model) maps onto exactly these, and a fourth would have to earn a name a
// person would say out loud.
type Tier string

// The tiers, and the absence of one.
const (
	// TierNone means no tier — either tiering is switched off entirely (no
	// [ai.tiers] table at all) or nothing was asked for. It is the zero value
	// on purpose: a struct nobody filled in must not claim a tier.
	TierNone Tier = ""
	// TierInstant is the small, fast model: chosen for immediacy, accepting
	// lower quality. It is never chosen for a turn that may call a tool —
	// see Decide.
	TierInstant Tier = "instant"
	// TierMedium is today's brain: the tier every existing configuration
	// already has, and the default when nothing says otherwise.
	TierMedium Tier = "medium"
	// TierDeep is the strongest model available — an endpoint, or an advisor
	// CLI through the bridge of ADR 0016. Reached only when the user or the
	// model asks for it, because it is the slow one.
	TierDeep Tier = "deep"
)

// TierOrder lists the tiers weakest-first. It is the order every surface
// prints them in — the settings enum, the doctor's per-tier rows, the window's
// segmented control — so a reader never has to work out which way round a
// particular screen decided to go.
func TierOrder() []Tier { return []Tier{TierInstant, TierMedium, TierDeep} }

// ParseTier resolves a configured or spoken string to a tier. Unknown text is
// not a tier: callers report it rather than guessing, because guessing here
// would silently answer from a model the user did not choose.
func ParseTier(s string) (Tier, bool) {
	switch Tier(s) {
	case TierInstant:
		return TierInstant, true
	case TierMedium:
		return TierMedium, true
	case TierDeep:
		return TierDeep, true
	}
	return TierNone, false
}

// TierLabel is the word a person sees for a tier. It is the *product's* name
// for the trade — Quick, Balanced, Deep — rather than the config file's, which
// names models. One copy, here, read by the window's control, the pending
// turn, the doctor's rows and everything Jarvix says out loud, so no surface
// can invent a fourth word for the same thing.
func TierLabel(tier Tier) string {
	switch tier {
	case TierInstant:
		return "Quick"
	case TierMedium:
		return "Balanced"
	case TierDeep:
		return "Deep"
	}
	return ""
}

// TierDescription is the one-line explanation beside each label, for the
// control and for the settings screen. It states the cost as well as the
// benefit: a speed control whose slow setting does not admit to being slow is
// a control people learn to distrust.
func TierDescription(tier Tier) string {
	switch tier {
	case TierInstant:
		return "the lightest model — answers immediately, at lower quality"
	case TierMedium:
		return "the usual model"
	case TierDeep:
		return "the strongest model available — worth waiting for"
	}
	return ""
}

// RouteInput is everything the router is allowed to look at. Everything here
// is known before the prompt is sent, and none of it is a guess about what the
// user meant: there is no triviality classifier, deliberately (see Decide).
type RouteInput struct {
	// Available is the set of tiers that have a binding of their own. Medium
	// is always available when tiering is on — an absent [ai.tiers.medium]
	// falls back to the [ai] brain, which is exactly what medium means — so a
	// caller that has tiers at all sets Available[TierMedium].
	Available map[Tier]bool
	// Default is the configured default tier ([ai.tiers] default). TierNone
	// or an unavailable tier means medium.
	Default Tier
	// Pinned is the conversation's thinking level: what the window's control
	// or a spoken "stay on the deep model" set. TierNone means unpinned, and
	// the default applies.
	Pinned Tier
	// Asked is a per-turn escalation the utterance carried ("think hard about
	// this…"). It outranks the pin for this one turn and nothing beyond it:
	// asking once is a request about this question, not a new setting.
	Asked Tier
	// ToolsAttached is whether this turn's request carries tool definitions —
	// that is, whether the model *may* call a tool. It is the one input with
	// veto power; see the hard rule in Decide.
	ToolsAttached bool
}

// RouteReason says why a tier was chosen, for the record and for the wording
// of anything Jarvix says about it. It is never inferred from the tier: two
// turns can land on medium for completely different reasons and the record has
// to be able to tell them apart.
type RouteReason string

// Why a turn landed on the tier it did.
const (
	// ReasonDefault: nothing asked for anything; the configured default.
	ReasonDefault RouteReason = "default"
	// ReasonPinned: the conversation is pinned to this tier.
	ReasonPinned RouteReason = "pinned"
	// ReasonAsked: this turn asked for it ("think hard about this…").
	ReasonAsked RouteReason = "asked"
	// ReasonUnavailable: the pinned or asked-for tier has no binding, so the
	// default served it instead. Route.Wanted names what could not be had —
	// this is the case Jarvix must say out loud rather than quietly downgrade.
	ReasonUnavailable RouteReason = "unavailable"
	// ReasonToolsRefuseInstant: instant was in line to serve, and the turn
	// carries tools. Route.Wanted is TierInstant. Not spoken — see Decide.
	ReasonToolsRefuseInstant RouteReason = "tools"
	// ReasonUnreachable: the tier was configured and was *tried*, and did not
	// answer — no key, nothing listening, an advisor that is not installed.
	// Decide never produces it (reachability is not a question a table can
	// answer); the engine writes it after a failover. It is kept distinct from
	// ReasonUnavailable because the two have different fixes and a user
	// reading the record needs to know which one they have.
	ReasonUnreachable RouteReason = "unreachable"
)

// Route is one routing decision.
type Route struct {
	// Tier is the tier that will serve the turn.
	Tier Tier
	// Reason is why.
	Reason RouteReason
	// Wanted is the tier that was asked for and not served, TierNone when
	// nothing was refused. It exists so the refusal can be *stated* — the
	// whole failure this feature must not have is a downgrade nobody is told
	// about.
	Wanted Tier
}

// Decide picks the tier for one turn.
//
// The order is: the configured default, overridden by a conversation pin,
// overridden by this turn's explicit ask — most specific wins, and each step
// is a thing a person did rather than something inferred about their sentence.
// Then two corrections, in this order:
//
//  1. A tier with no binding cannot serve. The default takes the turn and
//     Wanted names what was refused, so the caller can say so.
//  2. **A turn that may call a tool is never served by the instant tier.**
//
// Rule 2 is a hard rule, not a heuristic, and it is enforced last so that no
// path — a default of instant, a pin, an explicit "quick answer" — can get
// round it. It exists because this project has already lived through the
// failure: in issue #71 a model too small for the prompt it was given narrated
// actions it had never performed, and a small model *holding tools* is that
// same failure with the safety catch off. Jarvix would say it had opened the
// file, moved the window, sent the message. Saving a second of latency is not
// worth a sentence like that, so the trade is not offered.
//
// Note what is deliberately absent: there is no classifier deciding that a
// question "looks trivial" and can go to the small model. ADR 0017 settled
// that argument for the intent router — ambiguity belongs to the model, and
// the cost of being liberal is Jarvix doing something the user did not ask
// for — and it applies here with more force, because the cost of being wrong
// is a worse answer the user cannot see the cause of. Instant is reached when
// somebody chooses it, never when something guesses.
//
// Decide is pure and total: any RouteInput, including a zero one, yields a
// tier.
func Decide(in RouteInput) Route {
	available := func(t Tier) bool { return t != TierNone && in.Available[t] }

	// The fallback every correction falls back to. Medium is the tier that
	// always exists when tiering is on, so this terminates.
	fallback := in.Default
	if _, ok := ParseTier(string(fallback)); !ok || !available(fallback) {
		fallback = TierMedium
	}

	route := Route{Tier: fallback, Reason: ReasonDefault}
	if _, ok := ParseTier(string(in.Pinned)); ok {
		route = Route{Tier: in.Pinned, Reason: ReasonPinned}
	}
	if _, ok := ParseTier(string(in.Asked)); ok {
		route = Route{Tier: in.Asked, Reason: ReasonAsked}
	}

	// Correction 1: an unbound tier cannot answer. Say which one.
	if !available(route.Tier) {
		if route.Reason != ReasonDefault {
			route.Wanted = route.Tier
			route.Reason = ReasonUnavailable
		}
		route.Tier = fallback
	}

	// Correction 2: the hard rule. Medium rather than the fallback, because
	// the fallback could itself be instant and this rule admits no exception.
	if route.Tier == TierInstant && in.ToolsAttached {
		route.Wanted = TierInstant
		route.Reason = ReasonToolsRefuseInstant
		route.Tier = TierMedium
	}
	return route
}
