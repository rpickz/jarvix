package config

import (
	"fmt"
	"strings"
)

// This file owns the `[knowledge]` table and its `[[knowledge.feeds]]`
// entries: user-configured fetchers whose latest value the daemon keeps warm,
// so a question about changing data ("what's the AMD price?") is answered
// from a value already sitting in the daemon rather than by a slow round trip
// per ask (ADR 0031). It is the moving counterpart of the memory book (ADR
// 0025): memory holds facts the user *stated*, feeds hold values a command
// the user *wrote* keeps current.
//
// Like `[[routines]]` and `[[intents.custom]]`, feeds are structured tables:
// hand-edited TOML outside the config.set surface, applied on the next
// idle-class reload or restart, and listed read-only over IPC
// (`knowledge.status`, docs/ipc.md).

// KnowledgeFeed is one [[knowledge.feeds]] table.
//
// The command lives here and only here: the model chooses *which* feed to
// read, never what runs — the argv is fixed at configuration time and
// executed with the advisor path's discipline (no shell, scrubbed
// environment, process-group timeout; ADR 0016 applied verbatim).
type KnowledgeFeed struct {
	// Name is what the model asks for and what every surface calls the feed.
	// Unique across feeds, case-insensitively.
	Name string `toml:"name"`
	// Enabled parks the feed without deleting it: false stops every fetch —
	// scheduled, lazy, or manual — while the entry, its comments, and its
	// last cached value all stay. A pointer because absent means true: the
	// key only appears in config.toml when someone (hand or window) chose to
	// write it, and every existing config keeps working unchanged.
	//
	// This is THE `enabled` convention for [[family]] tables (issue #92):
	// the field is named `enabled`, it defaults to true, and a disabled
	// entry is still fully validated — so re-enabling can never surprise.
	// [[routines]] and [[scripts]] adopt the same key with #93.
	Enabled *bool `toml:"enabled"`
	// Description tells the model what this feed watches, so "what's the AMD
	// price?" reaches for the right one. It appears in the tool schema.
	Description string `toml:"description"`
	// Command is the fixed argv that prints the current value on stdout:
	// element zero is the program (absolute path, or a bare name resolved on
	// PATH at fetch time), the rest are its arguments. Never a shell line.
	Command Command `toml:"command"`
	// Mode is when the value is fetched: "eager" refreshes on a schedule so
	// the value is ready before it is asked for; "lazy" fetches on first use
	// and then serves the cached value until the ttl lapses. Empty means
	// eager — being ready is the point of the feature.
	Mode string `toml:"mode"`
	// IntervalSec is the eager refresh cadence. Zero means
	// DefaultFeedIntervalSec; lazy feeds ignore it.
	IntervalSec int `toml:"interval_sec"`
	// TTLSec is how long a fetched value counts as fresh. Past it, a lazy
	// feed refetches on the next ask, and any served value is disclosed as
	// stale. Zero means twice the interval for an eager feed and
	// DefaultFeedTTLSec for a lazy one.
	TTLSec int `toml:"ttl_sec"`
	// TimeoutSec bounds one fetch; the process group is killed past it. Zero
	// means DefaultFeedTimeoutSec.
	TimeoutSec int `toml:"timeout_sec"`
	// Inject opts this feed's cached value into every model turn, under the
	// [knowledge] injection budget — for small values asked about so often
	// that even a tool call is a detour.
	Inject bool `toml:"inject"`
}

// Knowledge is the [knowledge] table: the feeds plus the one shared knob.
type Knowledge struct {
	// MaxInjectedTokens caps (in estimated tokens, ~4 chars each) what the
	// injected feed values may add to one model turn — the memory block's
	// budget discipline (ADR 0025) applied to feeds. Values that do not fit
	// are dropped from the block only, never from the cache, and the model is
	// told the list is incomplete.
	MaxInjectedTokens int `toml:"max_injected_tokens"`
	// Feeds are the configured fetchers. Empty disables the feature: no
	// scheduler, no tool, nothing injected.
	Feeds []KnowledgeFeed `toml:"feeds"`
}

// Knowledge feed defaults and floors.
const (
	// DefaultFeedIntervalSec refreshes an eager feed every five minutes —
	// frequent enough for a stock price, rare enough to be polite to
	// whatever the command calls.
	DefaultFeedIntervalSec = 300
	// DefaultFeedTTLSec is the freshness horizon of a lazy feed that does
	// not name one: fifteen minutes.
	DefaultFeedTTLSec = 900
	// DefaultFeedTimeoutSec bounds one fetch. A feed command is expected to
	// be one HTTP request and a line of output; thirty seconds is generous.
	DefaultFeedTimeoutSec = 30
	// MinFeedIntervalSec is the fastest allowed eager cadence. Below thirty
	// seconds a "feed" is a stream, and a misconfigured one would hammer
	// whatever service the command talks to all day.
	MinFeedIntervalSec = 30
	// DefaultKnowledgeInjectedTokens caps the injected feed block. Smaller
	// than memory's 500: a feed value is a number or a sentence, and the
	// block must never crowd the conversation.
	DefaultKnowledgeInjectedTokens = 300
	// Feed modes, the accepted values of knowledge.feeds.mode.
	FeedModeEager = "eager"
	FeedModeLazy  = "lazy"
)

// applyKnowledgeDefaults fills each configured feed, so a table holding only
// name, description and command is complete and usable.
func applyKnowledgeDefaults(cfg *Config) {
	for i, f := range cfg.Knowledge.Feeds {
		if f.Enabled == nil {
			enabled := true
			f.Enabled = &enabled
		}
		if f.Mode == "" {
			f.Mode = FeedModeEager
		}
		if f.IntervalSec == 0 {
			f.IntervalSec = DefaultFeedIntervalSec
		}
		if f.TTLSec == 0 {
			// An eager feed refreshed on schedule is fresh until two
			// intervals have passed — one missed refresh is disclosed as
			// stale, not silently served as current.
			if f.Mode == FeedModeEager {
				f.TTLSec = 2 * f.IntervalSec
			} else {
				f.TTLSec = DefaultFeedTTLSec
			}
		}
		if f.TimeoutSec == 0 {
			f.TimeoutSec = DefaultFeedTimeoutSec
		}
		cfg.Knowledge.Feeds[i] = f
	}
}

// knowledgeProblems validates the [knowledge] table. Messages name the
// offending feed and what is accepted.
func (c Config) knowledgeProblems() []string {
	var problems []string
	if len(c.Knowledge.Feeds) > 0 && c.Knowledge.MaxInjectedTokens < MinMemoryInjectedTokens {
		problems = append(problems, fmt.Sprintf(
			"knowledge.max_injected_tokens is %d; it must be at least %d — below that not even "+
				"one feed value fits and injection would be silently useless while looking enabled",
			c.Knowledge.MaxInjectedTokens, MinMemoryInjectedTokens))
	}
	seen := make(map[string]bool, len(c.Knowledge.Feeds))
	for i, f := range c.Knowledge.Feeds {
		label := fmt.Sprintf("knowledge.feeds[%d]", i)
		if f.Name != "" {
			label = fmt.Sprintf("knowledge.feeds[%d] (%q)", i, f.Name)
		}
		switch {
		case strings.TrimSpace(f.Name) == "":
			problems = append(problems, fmt.Sprintf(
				"%s: name is empty; give the feed a short name the model can ask for, e.g. \"amd\"", label))
		case !validAdvisorName(f.Name):
			// The same rule as advisor names, for the same reason: the model
			// must be able to name it back exactly, and a human must be able
			// to hear it in a confirmation.
			problems = append(problems, fmt.Sprintf(
				"%s: name is invalid; use letters, digits, dashes or underscores", label))
		case seen[strings.ToLower(f.Name)]:
			problems = append(problems, fmt.Sprintf(
				"%s: duplicate feed name; each feed needs its own", label))
		}
		seen[strings.ToLower(f.Name)] = true
		if len(f.Command) == 0 || strings.TrimSpace(f.Command[0]) == "" {
			problems = append(problems, fmt.Sprintf(
				"%s: command is empty; set the argv that prints the value, "+
					"e.g. command = [\"/home/you/bin/amd-price\"]", label))
		}
		if f.Mode != FeedModeEager && f.Mode != FeedModeLazy {
			problems = append(problems, fmt.Sprintf(
				"%s: mode %q is not supported; use %q (refreshed on schedule) or %q (fetched on first use)",
				label, f.Mode, FeedModeEager, FeedModeLazy))
		}
		if f.Mode == FeedModeEager && f.IntervalSec < MinFeedIntervalSec {
			problems = append(problems, fmt.Sprintf(
				"%s: interval_sec is %d; an eager feed must not refresh faster than every %d seconds",
				label, f.IntervalSec, MinFeedIntervalSec))
		}
		if f.TTLSec <= 0 {
			problems = append(problems, fmt.Sprintf(
				"%s: ttl_sec must be positive (how long a fetched value counts as fresh)", label))
		}
		if f.Mode == FeedModeEager && f.TTLSec < f.IntervalSec {
			// A ttl shorter than the refresh cadence would disclose every
			// value as stale for most of its life — enabled but useless.
			problems = append(problems, fmt.Sprintf(
				"%s: ttl_sec (%d) is shorter than interval_sec (%d); a scheduled value would be "+
					"stale before its refresh — raise ttl_sec or lower interval_sec",
				label, f.TTLSec, f.IntervalSec))
		}
		if f.TimeoutSec <= 0 {
			problems = append(problems, fmt.Sprintf(
				"%s: timeout_sec must be positive (seconds before the fetch is killed)", label))
		}
	}
	return problems
}

// IsEnabled reads the enabled switch with its default applied, for callers
// that may see a feed before applyKnowledgeDefaults ran (absent means true).
func (f KnowledgeFeed) IsEnabled() bool {
	return f.Enabled == nil || *f.Enabled
}

// KnowledgeFeedNames lists the configured feed names in declaration order —
// the order the tool schema, doctor, and error messages all show.
func (c Config) KnowledgeFeedNames() []string {
	names := make([]string, 0, len(c.Knowledge.Feeds))
	for _, f := range c.Knowledge.Feeds {
		names = append(names, f.Name)
	}
	return names
}

// KnowledgeSystemPrompt is appended to the system prompt when at least one
// feed is configured. Freshness-stating is a prompt-level rule because it is
// behaviour, not mechanics: the tool result carries the age in words, and
// this is what makes the model say it.
const KnowledgeSystemPrompt = " The user has configured live feeds — commands whose current value " +
	"is kept fetched for you. When they ask about a topic one of your feeds covers, read it with " +
	"the knowledge.get tool (or use the injected feed values above) instead of guessing or " +
	"answering from your training data, and always tell them how fresh the value is, in the " +
	"spoken wording the result gives you. If a value is stale or a feed is failing, say so " +
	"honestly in one short sentence."
