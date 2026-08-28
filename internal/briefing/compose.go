package briefing

import (
	"context"
	"strings"
	"time"

	"github.com/rpickz/jarvix/internal/knowledge"
)

// This file turns a pile of source lines into the two things a briefing has
// to be: a spoken account short enough that someone standing at their desk
// will hear it out, and a full account for the window.
//
// The length rule is the one that needs justifying. A briefing is *spoken*,
// so its budget is seconds of speech, not characters — and a spoken sentence
// nobody listens to the end of is worse than a shorter one, because the part
// that gets cut is the part at the end, which is exactly where truncation
// would have put a pointer. So the trim happens here, deterministically,
// whole lines at a time, with the pointer spoken rather than implied.

const (
	// maxSpokenSeconds is the ticket's ~30 seconds, and it is a listening
	// bound rather than a technical one: past half a minute of unbroken
	// report, a person stops holding the earlier items in their head.
	maxSpokenSeconds = 30
	// spokenWordsPerMinute is a deliberately conservative reading rate.
	// Piper and Kokoro at speed 1.0 both sit around 150–170 wpm for prose of
	// this shape; taking the low end means the bound is honoured for the
	// slowest voice rather than only for the fastest.
	spokenWordsPerMinute = 150
	// maxSpokenWords is the budget the trimmer actually enforces.
	maxSpokenWords = maxSpokenSeconds * spokenWordsPerMinute / 60
)

// windowPointer is spoken whenever a line was trimmed. It is a fixed sentence
// and it is never omitted: a truncated briefing that does not say it was
// truncated is the same lie as a briefing that drops a source silently.
const windowPointer = "The rest is in the window."

// Section is one heading and its lines, for the window's full version.
type Section struct {
	Title string   `json:"title"`
	Lines []string `json:"lines"`
}

// Composed is one briefing, in both of its renderings.
type Composed struct {
	// Since is the moment the absence began; AwaySpoken is that moment in the
	// shared spoken scale ("nine hours ago"), so no client does its own
	// arithmetic (ADR 0013).
	Since      time.Time
	AwaySpoken string
	Headline   string
	// Sections is the full version: every line, in category order, nothing
	// trimmed. The window renders this.
	Sections []Section
	// Spoken is the headline plus as many lines as fit the speech budget,
	// plus the pointer when anything was left out.
	Spoken    string
	Truncated bool
	// Empty means nothing happened and every source could be read — the one
	// case that earns "nothing while you were away".
	Empty bool
	// Unavailable names the sources that could not be read, in source order.
	Unavailable []string
	// Disabled and NoAbsence are the two non-briefings the window asks about:
	// the feature is off, or there is no absence to report yet.
	Disabled  bool
	NoAbsence bool
	// ModelOutcome is "off", "used", or "refused" — why the headline reads
	// the way it does. It travels in the event, never a word of the briefing.
	ModelOutcome string
}

// compose reads every source and renders both forms. reason is carried into
// the event so the activity feed can say a briefing was given without saying
// anything about what was in it.
func (s *Service) compose(ctx context.Context, since time.Time, budget time.Duration, reason string) (Composed, error) {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	now := s.now()
	lines, unavailable := s.read(ctx, since, now)
	c := Composed{
		Since:       since,
		AwaySpoken:  knowledge.SpokenAge(now, since),
		Unavailable: unavailable,
	}

	sorted := orderLines(lines)
	counts := countLines(sorted)
	c.Empty = counts.substantive == 0 && len(unavailable) == 0
	c.Sections = sections(sorted)

	c.Headline, c.ModelOutcome = s.headline(ctx, c.AwaySpoken, sorted, counts)
	c.Spoken, c.Truncated = speak(c.Headline, sorted)

	s.publishGiven(c, reason)
	return c, nil
}

// headline words the opening sentence. The model gets one bounded call and
// its answer has to survive the contract in prompt.go; anything else — no
// provider, a failed call, a refused sentence, or nothing to talk about at
// all — reads the facts plainly, which is a worse sentence and a true one.
func (s *Service) headline(ctx context.Context, away string, lines []Line, counts lineCounts) (string, string) {
	plain := plainHeadline(away, counts)
	s.mu.Lock()
	summarise := s.summarise
	s.mu.Unlock()
	if summarise == nil || counts.substantive == 0 {
		// With nothing substantive there is nothing to word: an empty night
		// and an unreadable one both get the deterministic sentence, and no
		// model is ever asked to make something of them. This is the pinned
		// half of "never a manufactured briefing".
		return plain, "off"
	}
	reply, err := summarise(ctx, Prompt(away, lines))
	if err != nil {
		s.log.Info("briefing headline fell back to the plain reading", "component", "briefing",
			"error", err.Error())
		return plain, "refused"
	}
	worded, ok := enforceHeadline(reply, counts)
	if !ok {
		s.log.Info("briefing headline refused by the contract", "component", "briefing")
		return plain, "refused"
	}
	return worded, "used"
}

// lineCounts is how many lines each category earned, plus the substantive
// total (everything but Unavailable). The headline contract checks claims
// against these, so they are computed once and passed, never recomputed from
// prose.
type lineCounts struct {
	byCategory  [len(ordered)]int
	substantive int
}

func countLines(lines []Line) lineCounts {
	var c lineCounts
	for _, line := range lines {
		if line.Category >= 0 && int(line.Category) < len(c.byCategory) {
			c.byCategory[line.Category]++
		}
		if line.Category != Unavailable {
			c.substantive++
		}
	}
	return c
}

// orderLines puts the lines in speaking order: category first, then the order
// the sources were declared in. It is a stable partition rather than a sort
// so a source's own ordering inside a category survives.
func orderLines(lines []Line) []Line {
	out := make([]Line, 0, len(lines))
	for _, cat := range ordered {
		for _, line := range lines {
			if line.Category == cat {
				out = append(out, line)
			}
		}
	}
	return out
}

// sections groups ordered lines under their headings for the window.
func sections(lines []Line) []Section {
	var out []Section
	for _, cat := range ordered {
		var texts []string
		for _, line := range lines {
			if line.Category == cat {
				texts = append(texts, line.Text)
			}
		}
		if len(texts) > 0 {
			out = append(out, Section{Title: cat.Title(), Lines: texts})
		}
	}
	return out
}

// speak renders the spoken form under the word budget. Two passes, and the
// second one is the point: the pointer sentence only costs words when it is
// actually going to be spoken, so a briefing that fits is never trimmed to
// make room for an announcement that it was trimmed.
func speak(headline string, lines []Line) (string, bool) {
	kept, dropped := fit(headline, lines, 0)
	if dropped == 0 {
		return join(headline, kept, false), false
	}
	kept, dropped = fit(headline, lines, words(windowPointer))
	return join(headline, kept, true), dropped > 0
}

// fit selects the lines that fit the budget, taking them in speaking order
// and stopping at the first that does not. Unavailable lines are never
// dropped: "I couldn't check the reminders" is the one thing a shortened
// briefing must still say, because its absence is indistinguishable from
// having nothing to report.
func fit(headline string, lines []Line, reserve int) ([]Line, int) {
	spent := words(headline) + reserve
	for _, line := range lines {
		if line.Category == Unavailable {
			spent += words(line.Text)
		}
	}
	kept := make([]Line, 0, len(lines))
	dropped := 0
	for _, line := range lines {
		if line.Category == Unavailable {
			kept = append(kept, line)
			continue
		}
		cost := words(line.Text)
		if spent+cost > maxSpokenWords {
			dropped++
			continue
		}
		spent += cost
		kept = append(kept, line)
	}
	return kept, dropped
}

// join assembles the spoken briefing. Lines arrive already punctuated, so
// this only spaces them.
func join(headline string, lines []Line, truncated bool) string {
	parts := make([]string, 0, len(lines)+2)
	if headline != "" {
		parts = append(parts, headline)
	}
	for _, line := range lines {
		parts = append(parts, line.Text)
	}
	if truncated {
		parts = append(parts, windowPointer)
	}
	return strings.Join(parts, " ")
}

// words counts spoken words. Whitespace-separated is close enough for a
// budget whose own inputs (a reading rate, a listening ceiling) are estimates.
func words(text string) int {
	return len(strings.Fields(text))
}

// plainHeadline is the deterministic opening sentence: what a briefing says
// when there is no model, when the model failed, when its sentence was
// refused, and when there is nothing to word. Every other path in this
// package can fall back to it, so it has to work for every shape — including
// the empty night, which is the one the honesty rule cares most about.
func plainHeadline(away string, counts lineCounts) string {
	if counts.substantive == 0 {
		if counts.byCategory[Unavailable] > 0 {
			return "Nothing I could find while you were away, and I couldn't check everything — " +
				"you were last here " + away + "."
		}
		return "Nothing while you were away — you were last here " + away + "."
	}
	var parts []string
	if n := counts.byCategory[Awaiting]; n > 0 {
		parts = append(parts, CountWord(n)+" waiting on you")
	}
	if n := counts.byCategory[Completed]; n > 0 {
		parts = append(parts, CountWord(n)+" finished")
	}
	if n := counts.byCategory[InProgress]; n > 0 {
		parts = append(parts, CountWord(n)+" still going")
	}
	if n := counts.byCategory[Housekeeping]; n > 0 {
		parts = append(parts, CountWord(n)+" "+plural(n, "bit", "bits")+" of housekeeping")
	}
	return "Since you were last here " + away + ": " + list(parts) + "."
}

// unavailableSentence words one source Jarvix could not read. Named, always,
// and in the source's own terms — a listener has to be able to tell "nothing
// happened there" from "I did not look".
func unavailableSentence(source string) string {
	return "I couldn't check " + sourceNoun(source) + " just now."
}

// sourceNoun is the spoken name of a source. Sources are stable identifiers;
// this is the one place they become English, so the wording of a failure is
// tested here rather than restated at each adapter.
func sourceNoun(source string) string {
	switch source {
	case SourceSessions:
		return "the AI sessions"
	case SourceFocus:
		return "your focus threads"
	case SourceReminders:
		return "your reminders"
	case SourceActivity:
		return "what I've been running"
	case SourceConversations:
		return "our conversations"
	default:
		return source
	}
}

// The source identifiers. They are the ordering key, the event's vocabulary,
// and the argument to sourceNoun — declared here so the daemon's adapters and
// this package's wording cannot drift.
const (
	SourceSessions      = "sessions"
	SourceReminders     = "reminders"
	SourceFocus         = "focus"
	SourceActivity      = "activity"
	SourceConversations = "conversations"
)

// list joins clauses the way a sentence would: "a", "a and b", "a, b and c".
func list(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	case 2:
		return parts[0] + " and " + parts[1]
	default:
		return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// countWords are the number words the plain headline speaks and the contract
// recognises. Small on purpose: a briefing with more than a dozen lines of
// anything has a different problem than its wording.
var countWords = [...]string{"zero", "one", "two", "three", "four", "five", "six",
	"seven", "eight", "nine", "ten", "eleven", "twelve"}

// CountWord renders a small count in words, falling back to digits — which
// the speech normaliser reads correctly anyway. Exported because the daemon's
// source adapters compose their own sentences and must count the same way
// this package's headline does: "two sessions" in one line and "2" in the
// next would read as two different voices.
func CountWord(n int) string {
	if n >= 0 && n < len(countWords) {
		return countWords[n]
	}
	return itoa(n)
}

// itoa avoids pulling strconv in for one call site.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// publishGiven emits the one event this feature has. It carries counts,
// outcomes and source names — never a line, never a headline, never a word
// the model wrote. The activity row built from it says a briefing was given
// and stops there, which is the leak-salted criterion (#147's shape).
func (s *Service) publishGiven(c Composed, reason string) {
	if s.publish == nil {
		return
	}
	data := map[string]any{
		"reason":    reason,
		"lines":     lineTotal(c),
		"sections":  len(c.Sections),
		"truncated": c.Truncated,
		"empty":     c.Empty,
		"model":     c.ModelOutcome,
		"away":      c.AwaySpoken,
	}
	if len(c.Unavailable) > 0 {
		data["unavailable"] = strings.Join(c.Unavailable, ",")
	}
	s.publish("briefing.given", data)
}

func lineTotal(c Composed) int {
	total := 0
	for _, section := range c.Sections {
		total += len(section.Lines)
	}
	return total
}
