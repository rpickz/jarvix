package config

import (
	"fmt"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/rpickz/jarvix/internal/ai"
)

// This file owns `[ai.tiers]`: the instant / medium / deep model tiers of
// issue #159 (ADR 0063).
//
// The shape is deliberately the shape the [ai.<name>] endpoints already have,
// one level down. A tier names an endpoint the loader already knows about and
// a model to ask it for — or it names an advisor, in which case the bridge of
// ADR 0016 answers the turn instead. It never carries a base URL or a
// credential of its own: there is exactly one place an endpoint is described,
// and a tier points at it. That is what keeps "add a tier" from meaning "add a
// second copy of the provider configuration".
//
//	[ai.tiers]
//	default = "medium"
//
//	[ai.tiers.instant]
//	provider      = "lmstudio"
//	model         = "qwen3-1.7b"
//	history_turns = 4
//
//	[ai.tiers.medium]
//	provider = "fireworks"
//	model    = "accounts/fireworks/models/qwen3p8-max"
//
//	[ai.tiers.deep]
//	advisor = "claude"
//
// **An absent tier is not a configured tier.** A missing [ai.tiers.medium]
// resolves to the [ai] brain, because that is precisely what medium means: the
// model this configuration has always used. Instant and deep have no such
// stand-in — an absent one does not exist, and asking for it is answered by
// saying so, never by quietly serving the same model under a stronger name.
// The whole document being absent switches tiering off: one brain, one code
// path, byte-identical behaviour, which is what
// TestNoTiersConfiguredIsTodaysEngineExactly pins.

// AITier is one [ai.tiers.<name>] table.
type AITier struct {
	// Provider keys into the [ai.<name>] endpoints, exactly as ai.provider
	// does. Empty when the tier is served by an advisor.
	Provider string `toml:"provider"`
	// Model is the model name that provider should be asked for.
	Model string `toml:"model"`
	// Advisor names an [advisors.<name>] CLI that answers this tier's turns
	// through the bridge of ADR 0016. Mutually exclusive with provider/model:
	// a tier is one endpoint or one advisor, and a table claiming both is a
	// configuration error rather than a precedence puzzle.
	Advisor string `toml:"advisor"`
	// HistoryTurns caps how many prior exchanges this tier is sent, tighter
	// than conversation.history_turns. It is the instant tier's whole point —
	// first-token latency scales with the prompt — and 0 means "the
	// conversation's own budget", so a tier that says nothing about context
	// gets exactly what every tier used to get.
	//
	// Trimming is disclosed, never silent: the turn's record carries how many
	// exchanges were left out and the model is told, on ADR 0037's terms.
	HistoryTurns int `toml:"history_turns"`
}

// Configured reports whether this table says anything at all. It is how an
// absent tier is told from a present but empty one, and it is what the loader
// records rather than a separate presence flag: a table with nothing in it
// fails validation anyway, so there is no state in which the two disagree.
func (t AITier) Configured() bool {
	return t.Provider != "" || t.Model != "" || t.Advisor != "" || t.HistoryTurns != 0
}

// AITiers is the whole [ai.tiers] section.
type AITiers struct {
	// Default is the tier a new conversation starts on. Empty means medium.
	// It is the *default*, not the current setting: the window's Quick /
	// Balanced / Deep control and its spoken equivalents move a
	// conversation-scoped pin, and a new conversation comes back here.
	Default string `toml:"default"`
	// Tiers holds every [ai.tiers.<name>] table found, including names that
	// are not tiers — validation names those rather than the loader dropping
	// them, so a typo is reported instead of silently doing nothing.
	Tiers map[string]AITier `toml:"-"`
}

// Enabled reports whether tiering is on at all: at least one tier table
// exists. `default = "medium"` on its own is not tiering — it configures
// nothing, so it changes nothing.
func (t AITiers) Enabled() bool {
	for _, tier := range t.Tiers {
		if tier.Configured() {
			return true
		}
	}
	return false
}

// Names lists the configured tier names, sorted — the order every message and
// every doctor row uses.
func (t AITiers) Names() []string {
	names := make([]string, 0, len(t.Tiers))
	for name := range t.Tiers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// tiersTableKey is the key under [ai] that holds the tier tables, and the one
// key of that table that is a scalar rather than a tier.
const (
	tiersTableKey   = "tiers"
	tiersDefaultKey = "default"
)

// parseAITiers harvests [ai.tiers] out of the loose decode. It runs from
// parse(), beside the endpoint harvest and for the same reason: a table of
// arbitrary sub-tables is not something TOML struct decoding can express.
//
// Unknown sub-table names are kept rather than skipped. The loader's job is to
// report the document faithfully; deciding that `[ai.tiers.fast]` is not a
// tier is validation's job, and it can say so with the name in the message.
func parseAITiers(md toml.MetaData, prim toml.Primitive, cfg *Config) error {
	var raw map[string]toml.Primitive
	if err := md.PrimitiveDecode(prim, &raw); err != nil {
		return fmt.Errorf("[ai.tiers]: %w", err)
	}
	tiers := AITiers{Default: cfg.AI.Tiers.Default, Tiers: map[string]AITier{}}
	for name, child := range raw {
		if name == tiersDefaultKey {
			var value string
			if err := md.PrimitiveDecode(child, &value); err != nil {
				return fmt.Errorf("[ai.tiers]: default: %w", err)
			}
			tiers.Default = value
			continue
		}
		var tier AITier
		if err := md.PrimitiveDecode(child, &tier); err != nil {
			return fmt.Errorf("[ai.tiers.%s]: %w", name, err)
		}
		tiers.Tiers[name] = tier
	}
	cfg.AI.Tiers = tiers
	return nil
}

// validateTiers reports tier configuration a user must fix. Messages are
// labelled `ai.tiers.<name>.<key>` so a whole-document validation can be keyed
// back to the field that owns it, exactly as validateEndpoints and
// validateAdvisors already are.
//
// Nothing here reads a credential, and nothing here dials anything: whether a
// tier can actually answer is `jarvix doctor`'s question, asked with a real
// probe (#114), and a validator that guessed at reachability would either
// block a save while a laptop is offline or claim a reachability it never
// observed.
func (c Config) validateTiers() []string {
	set := c.AI.Tiers
	var problems []string

	if d := strings.TrimSpace(set.Default); d != "" {
		if _, ok := ai.ParseTier(d); !ok {
			problems = append(problems, fmt.Sprintf(
				"ai.tiers.default %q is not a model tier; use %s", d, tierNameList()))
		} else if set.Enabled() && !set.Tiers[d].Configured() && d != string(ai.TierMedium) {
			// Medium is exempt: an absent [ai.tiers.medium] *is* the [ai]
			// brain, so defaulting to it is always serviceable. Instant and
			// deep have no stand-in, and a default nobody can serve would be
			// discovered as a wrong-feeling answer rather than as a message.
			problems = append(problems, fmt.Sprintf(
				"ai.tiers.default is %q but there is no [ai.tiers.%s] table; "+
					"add one naming a provider and model (or an advisor), or choose another default", d, d))
		}
	}

	for _, name := range set.Names() {
		tier := set.Tiers[name]
		if _, ok := ai.ParseTier(name); !ok {
			problems = append(problems, fmt.Sprintf(
				"tier name %q is not a model tier; the tiers are %s (the table is [ai.tiers.<name>])",
				name, tierNameList()))
			continue
		}
		problems = append(problems, c.tierProblems(name, tier)...)
	}
	return problems
}

// tierProblems checks one tier table. The two shapes are exclusive and one of
// them is required: a tier that names neither an endpoint nor an advisor
// configures nothing, and silently ignoring it would leave a user staring at a
// tier control that does not move.
func (c Config) tierProblems(name string, tier AITier) []string {
	var problems []string
	key := func(k string) string { return "ai.tiers." + name + "." + k }

	switch {
	case tier.Advisor != "" && (tier.Provider != "" || tier.Model != ""):
		problems = append(problems, fmt.Sprintf(
			"%s and %s are both set; a tier names an endpoint and a model, or an advisor, never both",
			key("advisor"), key("provider")))
	case tier.Advisor != "":
		if _, ok := c.Advisors[tier.Advisor]; !ok {
			problems = append(problems, fmt.Sprintf(
				"%s %q is not configured; %s", key("advisor"), tier.Advisor, advisorChoices(c)))
		}
	case tier.Provider != "" || tier.Model != "":
		if tier.Provider == "" {
			problems = append(problems, fmt.Sprintf(
				"%s is empty; name one of: %s (or set %s instead)",
				key("provider"), strings.Join(c.endpointNames(), ", "), key("advisor")))
		} else if _, ok := c.AI.Endpoints[tier.Provider]; !ok {
			problems = append(problems, fmt.Sprintf(
				"%s %q has no endpoint; add an [ai.%s] table with a base_url, or use one of: %s",
				key("provider"), tier.Provider, tier.Provider, strings.Join(c.endpointNames(), ", ")))
		}
		if tier.Model == "" {
			problems = append(problems, fmt.Sprintf(
				"%s is empty; set the model name that tier's provider should use", key("model")))
		}
	case tier.HistoryTurns != 0:
		// A table carrying only a context budget names no model at all.
		problems = append(problems, fmt.Sprintf(
			"%s is empty; a tier names a provider and model, or an advisor", key("provider")))
	}

	if tier.HistoryTurns < 0 {
		problems = append(problems, fmt.Sprintf(
			"%s must not be negative (0 uses conversation.history_turns)", key("history_turns")))
	}
	return problems
}

// tierNameList is the tier vocabulary as an English list, for messages.
func tierNameList() string {
	names := make([]string, 0, 3)
	for _, t := range ai.TierOrder() {
		names = append(names, "\""+string(t)+"\"")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// advisorChoices names the advisors a tier could point at, or says plainly
// that there are none — "use one of: " followed by nothing is the kind of
// message that makes a user check whether the tool is broken.
func advisorChoices(c Config) string {
	names := c.AdvisorNames()
	if len(names) == 0 {
		return "no advisors are configured; add an [advisors.<name>] table first"
	}
	return "add an [advisors." + names[0] + "]-style table, or use one of: " + strings.Join(names, ", ")
}
