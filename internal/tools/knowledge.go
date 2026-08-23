package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/rpickz/jarvix/internal/knowledge"
)

// This file is the model's hands on the feed cache (ADR 0031): one read
// verb over the knowledge service. The service owns the values — the
// scheduler, the ttl, the persistence — while this file owns what the
// *model* is told, which is where the honesty rules are actually made: every
// result carries the value's age in spoken words, a stale value says it is
// stale, and a failing feed is reported without technical detail, because
// anything returned here may be read aloud.

// Knowledge tool names. KnowledgeGetToolName is the registry name;
// KnowledgeRefreshToolName is the permission-gate identity its calls (and
// the daemon's scheduled refreshes) are judged under — its own identity, not
// shell.run's, because a feed command is user-authored configuration, so a
// user can tighten or disable feeds without touching anything else.
const (
	KnowledgeGetToolName     = "knowledge.get"
	KnowledgeRefreshToolName = "knowledge.refresh"
)

// FeedSource is what the tool needs from the knowledge service; an interface
// so tests supply readings without a scheduler.
type FeedSource interface {
	// Feeds lists the configured feeds in declaration order.
	Feeds() []knowledge.Feed
	// Get returns the current reading for one feed, fetching first when this
	// ask should trigger a fetch. ok is false when no feed has that name.
	Get(ctx context.Context, name string) (knowledge.Reading, bool)
}

// KnowledgeGet is the read tool over the feed cache.
type KnowledgeGet struct {
	// Source is the feed service.
	Source FeedSource
	// Now is the clock for age wording; nil means time.Now. Injectable so
	// tests pin every spoken age.
	Now func() time.Time
	// Log records reads — feed names and outcomes only, never values.
	Log *slog.Logger
}

// Name implements Tool.
func (k *KnowledgeGet) Name() string { return KnowledgeGetToolName }

// Description implements Tool. It is written for a small local model: the
// topics are listed concretely so "what's the AMD price?" reaches for the
// feed instead of the training data, and the freshness rule is stated as
// behaviour, not mechanics.
func (k *KnowledgeGet) Description() string {
	topics := make([]string, 0, 8)
	for _, f := range k.Source.Feeds() {
		topics = append(topics, f.Name+": "+f.Description)
	}
	return "Read the current value of a live feed the user configured. The feeds are: " +
		strings.Join(topics, "; ") + ". Use this whenever the user asks about one of those " +
		"topics — the value is kept fetched, so this is fast, and your own knowledge of such " +
		"things is out of date by definition. Always tell the user how fresh the value is, " +
		"using the spoken age the result gives you."
}

// Schema implements Tool. The feed enum is built from configuration, so the
// model can only name a feed the user configured — and both are read live,
// so a reload that edits the feeds is reflected on the next turn.
func (k *KnowledgeGet) Schema() json.RawMessage {
	feeds := k.Source.Feeds()
	options := make([]string, 0, len(feeds))
	for _, f := range feeds {
		options = append(options, fmt.Sprintf("%q", f.Name))
	}
	return json.RawMessage(fmt.Sprintf(`{
		"type": "object",
		"properties": {
			"feed": {
				"type": "string",
				"enum": [%s],
				"description": "Which feed to read"
			}
		},
		"required": ["feed"]
	}`, strings.Join(options, ", ")))
}

// knowledgeArgs is what the model is allowed to say: a feed name and nothing
// else. No field here can reach the command or its environment.
type knowledgeArgs struct {
	Feed string `json:"feed"`
}

// Activity implements Progressive: a lazy feed's first read runs the fetch
// command inside the call, which can take seconds — long enough for the
// overlay to say what the silence is.
func (k *KnowledgeGet) Activity(input json.RawMessage) (label, waiting string, ok bool) {
	var args knowledgeArgs
	if err := json.Unmarshal(input, &args); err != nil || strings.TrimSpace(args.Feed) == "" {
		return "", "", false
	}
	return "Checking the " + args.Feed + " feed…",
		"I'm still checking the " + args.Feed + " feed. This is taking a moment.", true
}

// Confirmation implements Confirmable, for the ask tier: the question names
// the feed actually about to be read, resolved from the arguments the gate
// judged, not from the model's narration.
func (k *KnowledgeGet) Confirmation(input json.RawMessage) (command, summary string, ok bool) {
	var args knowledgeArgs
	if err := json.Unmarshal(input, &args); err != nil || strings.TrimSpace(args.Feed) == "" {
		return "", "", false
	}
	return "read feed " + args.Feed,
		fmt.Sprintf("I want to check your %s feed. Should I go ahead?", args.Feed), true
}

// Execute implements Tool. Everything a feed can do wrong — unknown, cold,
// stale, failing — comes back as text for the model, because the session
// should end with one honest spoken sentence about it, not an error. Only
// malformed tool arguments are an err.
func (k *KnowledgeGet) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var args knowledgeArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("invalid knowledge.get arguments: %w", err)
	}
	name := strings.TrimSpace(args.Feed)
	if name == "" {
		return "", fmt.Errorf("knowledge.get: empty feed name")
	}
	log := k.Log
	if log == nil {
		log = slog.Default()
	}
	now := time.Now
	if k.Now != nil {
		now = k.Now
	}

	reading, found := k.Source.Get(ctx, name)
	if !found {
		// The configured list is the useful answer: the model can then say
		// what it *can* watch instead of a bare refusal.
		feeds := k.Source.Feeds()
		if len(feeds) == 0 {
			return "No feeds are configured. Tell the user in one short sentence that no live " +
				"feeds are set up on this computer.", nil
		}
		lines := make([]string, 0, len(feeds))
		for _, f := range feeds {
			lines = append(lines, f.Name+": "+f.Description)
		}
		return fmt.Sprintf("No feed is named %q. The configured feeds are:\n%s\n"+
			"If one of these covers what the user asked, call knowledge.get again with its "+
			"name; otherwise tell the user, in one short sentence, which topics you can watch.",
			name, strings.Join(lines, "\n")), nil
	}

	log.Info("feed read", "component", "tools", "tool", k.Name(), "feed", name,
		"has_value", reading.HasValue, "stale", reading.Stale, "failing", reading.Failing)

	if !reading.HasValue {
		if reading.Failing {
			return fmt.Sprintf("The %s feed has no value yet: fetching it has been failing "+
				"since %s. Tell the user in one short sentence that you could not read their "+
				"%s feed, and do not retry or read out any technical detail.",
				name, knowledge.SpokenAge(now(), reading.FailingSince), name), nil
		}
		return fmt.Sprintf("The %s feed has no value yet. Tell the user in one short sentence "+
			"that the feed has not produced a value, and do not retry.", name), nil
	}

	age := knowledge.SpokenAge(now(), reading.FetchedAt)
	var b strings.Builder
	fmt.Fprintf(&b, "The %s feed (%s) reads:\n\n%s\n\nFetched %s.",
		name, reading.Feed.Description, reading.Value, age)
	if reading.Truncated {
		b.WriteString(" (The value was truncated at the output cap.)")
	}
	switch {
	case reading.Stale && reading.Failing:
		fmt.Fprintf(&b, " This value is STALE: refreshing has been failing since %s. Give the "+
			"user the value, say it is from %s, and add in one short sentence that a fresher "+
			"one could not be fetched.", knowledge.SpokenAge(now(), reading.FailingSince), age)
	case reading.Stale:
		fmt.Fprintf(&b, " This value is STALE — older than the feed's freshness window. Give "+
			"the user the value and say clearly that it is from %s and may be out of date.", age)
	default:
		fmt.Fprintf(&b, " Give the user this value and say how fresh it is — e.g. \"as of %s\". "+
			"Plain speech: no markdown, and no URLs or commands read aloud.", age)
	}
	return b.String(), nil
}
