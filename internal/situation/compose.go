package situation

import (
	"context"
	"strings"
	"time"

	"github.com/rpickz/jarvix/internal/knowledge"
	"github.com/rpickz/jarvix/internal/provenance"
)

// This file turns a pile of source items into the two things a report has to
// be: a spoken answer short enough that someone standing at their desk will
// hear it out, and a full account for the window with a link on every line.
//
// The length rule is the one that needs justifying. The report is *spoken*, so
// its budget is seconds of speech rather than characters — and a spoken answer
// nobody listens to the end of is worse than a shorter one, because the part
// that gets cut is the tail, which is exactly where truncation would have put a
// pointer. So the trim happens here, deterministically, whole lines at a time,
// with the pointer spoken rather than implied. That is ADR 0050's rule, held
// verbatim; only the number changes, because a question about now is answered
// faster than an account of a night away.

const (
	// maxSpokenSeconds is the ticket's ~20 seconds. It is a listening bound
	// rather than a technical one, and it is shorter than the briefing's
	// thirty for a reason about the question rather than about the content: a
	// briefing is settled into, and "what's going on?" is asked on the way
	// past.
	maxSpokenSeconds = 20
	// spokenWordsPerMinute is a deliberately conservative reading rate. Piper
	// and Kokoro at speed 1.0 both sit around 150–170 wpm for prose of this
	// shape; taking the low end means the bound is honoured for the slowest
	// voice rather than only for the fastest.
	spokenWordsPerMinute = 150
	// maxSpokenWords is the budget the trimmer actually enforces.
	maxSpokenWords = maxSpokenSeconds * spokenWordsPerMinute / 60
)

// windowPointer is spoken whenever a line was trimmed. It is a fixed sentence
// and it is never omitted: a truncated report that does not say it was
// truncated is the same lie as a report that drops a source silently.
const windowPointer = "The rest is in the window."

// noLink is the Link value of a line that points at nothing. Explicit rather
// than a zero value, because zero is a perfectly good index into Sources and a
// line that silently linked to somebody else's subject would be worse than a
// line with no link at all.
const noLink = -1

// Line is one rendered line and, when it has one, the index into the report's
// Sources of the thing it is about.
//
// The index is computed here rather than in the client for ADR 0013's reason:
// the window sends Sources to provenance.resolve verbatim and reads the item
// back at Link, so it does no arithmetic and cannot mis-pair a line with
// somebody else's link.
type Line struct {
	Text string `json:"text"`
	Link int    `json:"link"`
}

// Section is one heading and its lines, for the window's full version.
type Section struct {
	Title string `json:"title"`
	Lines []Line `json:"lines"`
}

// Report is one situation report, in both of its renderings.
type Report struct {
	// At is the moment the report was composed, and AgeSpoken is how long ago
	// that was in the shared spoken scale — "just now" for a fresh one, and
	// something older for a replay from the cache, so no surface has to do
	// clock arithmetic and none of them can imply a freshness the report does
	// not have.
	At        time.Time
	AgeSpoken string
	// Cached marks a replay. It never changes a word of what is said; it is
	// how the window and the event can tell a re-read from a re-composition.
	Cached bool
	// Headline is the opening sentence — the only thing a model is ever asked
	// to word, and only when there is something notable to word.
	Headline string
	// Caveat is the up-front admission that this process cannot account for
	// the whole stretch since the user last looked, or "" when it can. It sits
	// between the headline and the lines — spoken second, rendered second —
	// because a shortfall disclosed after the content has been read out is a
	// shortfall the listener has already acted on.
	Caveat string
	// Sections is the full version: every line, in rank order, nothing
	// trimmed. The window renders this.
	Sections []Section
	// Sources are every line's subject, flattened in render order. The window
	// hands this array to provenance.resolve and reads each line's item back
	// at its Link.
	Sources []provenance.Reference
	// Spoken is the headline, the caveat, and as many lines as fit the speech
	// budget, plus the pointer when anything was left out.
	Spoken    string
	Truncated bool
	// Quiet means nothing needs the user and nothing is running, finished or
	// failing — the one case that earns the short honest "nothing needs you"
	// rather than a manufactured list. Housekeeping does not defeat it: the
	// shape of the desktop is real and is still reported, but it is not news,
	// and a headline that treated it as news would be inventing urgency.
	Quiet bool
	// Unavailable names the sources that could not be read, in source order.
	Unavailable []string
	// ModelOutcome is "off", "used", or "refused" — why the headline reads the
	// way it does. It travels in the event, never a word of the report.
	ModelOutcome string
}

// composeNow reads every source and renders both forms. It is the part with no
// caching in it at all: by the time it is called the decision to actually look
// at the machine has already been taken.
func (s *Service) composeNow(ctx context.Context) Report {
	ctx, cancel := context.WithTimeout(ctx, s.budget)
	defer cancel()

	at := s.instant()
	items, unavailable := s.read(ctx, at)

	r := Report{
		At:          at.Now,
		AgeSpoken:   spokenAge(at.Now, at.Now),
		Caveat:      s.coverage(at.Since),
		Unavailable: unavailable,
	}

	sorted := orderItems(items)
	counts := countItems(sorted)
	r.Quiet = counts.notable == 0 && len(unavailable) == 0
	r.Sections, r.Sources = sections(sorted)

	r.Headline, r.ModelOutcome = s.headline(ctx, sorted, counts)
	r.Spoken, r.Truncated = speak(r.Headline, r.Caveat, sorted)
	return r
}

// spokenAge is the shared spoken-style age scale (ADR 0013), reached through
// the one package that owns it so the report, the focus threads and the
// reminders all size a stretch of time the same way.
func spokenAge(now, when time.Time) string { return knowledge.SpokenAge(now, when) }

// headline words the opening sentence. The model gets one bounded call and its
// answer has to survive the contract in prompt.go; anything else — no provider,
// a failed call, a refused sentence, or nothing notable to talk about at all —
// reads the facts plainly, which is a worse sentence and a true one.
func (s *Service) headline(ctx context.Context, items []Item, counts itemCounts) (string, string) {
	plain := plainHeadline(counts)
	s.mu.Lock()
	summarise := s.summarise
	s.mu.Unlock()
	if summarise == nil || counts.notable == 0 {
		// With nothing notable there is nothing to word: a quiet machine and
		// an unreadable one both get the deterministic sentence, and no model
		// is ever asked to make something of them. This is the pinned half of
		// "never a manufactured report" — it is a property of the code rather
		// than a hope about the prompt.
		return plain, "off"
	}
	reply, err := summarise(ctx, Prompt(items))
	if err != nil {
		s.log.Info("situation headline fell back to the plain reading", "component", "situation",
			"error", err.Error())
		return plain, "refused"
	}
	worded, ok := enforceHeadline(reply, counts)
	if !ok {
		s.log.Info("situation headline refused by the contract", "component", "situation")
		return plain, "refused"
	}
	return worded, "used"
}

// itemCounts is how many items each rank earned, plus two totals the headline
// contract checks claims against. They are computed once and passed, never
// recomputed from prose.
type itemCounts struct {
	byRank [len(ordered)]int
	// notable is everything the user might act on: the four ranks above
	// Housekeeping. It decides whether a model is consulted at all and whether
	// the report is Quiet.
	notable int
	// substantive is everything but Unavailable — the total a headline is
	// allowed to state.
	substantive int
}

func countItems(items []Item) itemCounts {
	var c itemCounts
	for _, item := range items {
		if item.Rank < 0 || int(item.Rank) >= len(c.byRank) {
			continue
		}
		c.byRank[item.Rank]++
		if item.Rank == Unavailable {
			continue
		}
		c.substantive++
		if item.Rank != Housekeeping {
			c.notable++
		}
	}
	return c
}

// orderItems puts the items in speaking order: rank first, then the order the
// sources were declared in. It is a stable partition rather than a sort, so a
// source's own ordering inside a rank survives — which is what makes the AI
// sessions lead the needs-you rank by being declared first, rather than by any
// special case for them anywhere in this package.
func orderItems(items []Item) []Item {
	out := make([]Item, 0, len(items))
	for _, rank := range ordered {
		for _, item := range items {
			if item.Rank == rank {
				out = append(out, item)
			}
		}
	}
	return out
}

// sections groups ordered items under their headings and flattens their
// references, so the two come out of one walk and cannot disagree about which
// link belongs to which line.
func sections(items []Item) ([]Section, []provenance.Reference) {
	var out []Section
	refs := make([]provenance.Reference, 0, len(items))
	for _, rank := range ordered {
		var lines []Line
		for _, item := range items {
			if item.Rank != rank {
				continue
			}
			line := Line{Text: item.Text, Link: noLink}
			if item.Where != nil {
				line.Link = len(refs)
				refs = append(refs, *item.Where)
			}
			lines = append(lines, line)
		}
		if len(lines) > 0 {
			out = append(out, Section{Title: rank.Title(), Lines: lines})
		}
	}
	return out, refs
}

// speak renders the spoken form under the word budget. Two passes, and the
// second one is the point: the pointer sentence only costs words when it is
// actually going to be spoken, so a report that fits is never trimmed to make
// room for an announcement that it was trimmed.
func speak(headline, caveat string, items []Item) (string, bool) {
	kept, dropped := fit(headline, caveat, items, 0)
	if dropped == 0 {
		return join(headline, caveat, kept, false), false
	}
	kept, dropped = fit(headline, caveat, items, words(windowPointer))
	return join(headline, caveat, kept, true), dropped > 0
}

// fit selects the items that fit the budget, taking them in speaking order and
// stopping at the first that does not.
//
// Unavailable items are never dropped: "I couldn't check your reminders" is the
// one thing a shortened report must still say, because its absence is
// indistinguishable from having nothing to report. The restart caveat is
// charged for on the same terms and for the same reason — it is an admission,
// and the trim takes the tail, which is where an admission would otherwise be
// lost.
func fit(headline, caveat string, items []Item, reserve int) ([]Item, int) {
	spent := words(headline) + words(caveat) + reserve
	for _, item := range items {
		if item.Rank == Unavailable {
			spent += words(item.Text)
		}
	}
	kept := make([]Item, 0, len(items))
	dropped := 0
	for _, item := range items {
		if item.Rank == Unavailable {
			kept = append(kept, item)
			continue
		}
		cost := words(item.Text)
		if spent+cost > maxSpokenWords {
			dropped++
			continue
		}
		spent += cost
		kept = append(kept, item)
	}
	return kept, dropped
}

// join assembles the spoken report. Lines arrive already punctuated, so this
// only spaces them.
func join(headline, caveat string, items []Item, truncated bool) string {
	parts := make([]string, 0, len(items)+3)
	if headline != "" {
		parts = append(parts, headline)
	}
	if caveat != "" {
		parts = append(parts, caveat)
	}
	for _, item := range items {
		parts = append(parts, item.Text)
	}
	if truncated {
		parts = append(parts, windowPointer)
	}
	return strings.Join(parts, " ")
}

// words counts spoken words. Whitespace-separated is close enough for a budget
// whose own inputs (a reading rate, a listening ceiling) are estimates.
func words(text string) int { return len(strings.Fields(text)) }

// QuietSentence is what the report says when nothing needs the user. It is
// short and it is honest, and it is deliberately not padded out with the
// housekeeping that follows it: a manufactured list read in the voice of news
// is the failure this sentence exists to avoid.
const QuietSentence = "Nothing needs you."

// partialQuietSentence is the same answer with its edges named. "Nothing needs
// you" from a daemon that could not read two of its sources is a claim it has
// not earned, and the difference is exactly the difference between "nothing
// happened there" and "I did not look".
const partialQuietSentence = "Nothing needs you in what I could read — and I couldn't check everything."

// plainHeadline is the deterministic opening sentence: what a report says when
// there is no model, when the model failed, when its sentence was refused, and
// when there is nothing notable to word. Every other path in this package falls
// back to it, so it has to work for every shape — including the quiet machine,
// which is the one the honesty rule cares most about.
func plainHeadline(counts itemCounts) string {
	if counts.notable == 0 {
		if counts.byRank[Unavailable] > 0 {
			return partialQuietSentence
		}
		return QuietSentence
	}
	var parts []string
	if n := counts.byRank[NeedsYou]; n > 0 {
		parts = append(parts, CountWord(n)+" waiting on you")
	}
	if n := counts.byRank[InProgress]; n > 0 {
		parts = append(parts, CountWord(n)+" still going")
	}
	if n := counts.byRank[Finished]; n > 0 {
		parts = append(parts, CountWord(n)+" finished")
	}
	if n := counts.byRank[Failing]; n > 0 {
		parts = append(parts, CountWord(n)+" failing")
	}
	return "Right now: " + list(parts) + "."
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
//
// The default is the identifier itself rather than a vague "one of my sources".
// A source added by a later slice — jobs, another machine — then names itself
// honestly on the day it is added, before anybody remembers to come back here.
func sourceNoun(source string) string {
	switch source {
	case SourceSessions:
		return "the AI sessions"
	case SourceFocus:
		return "your focus threads"
	case SourceReminders:
		return "your reminders"
	case SourceSchedules:
		return "your schedules"
	case SourceActivity:
		return "what I've been running"
	case SourceWindows:
		return "what's open on screen"
	default:
		return source
	}
}

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

// countWords are the number words the plain headline speaks and the contract
// recognises. Small on purpose: a report with more than a dozen lines of
// anything has a different problem than its wording.
//
// The table is this package's own rather than shared with the return
// briefing's. The two agree today, and they should, but they are two spoken
// vocabularies belonging to two features — putting them in a third package
// would move a wording decision somewhere neither feature owns, and the cost
// of the duplication is one array that never changes.
var countWords = [...]string{"zero", "one", "two", "three", "four", "five", "six",
	"seven", "eight", "nine", "ten", "eleven", "twelve"}

// CountWord renders a small count in words, falling back to digits — which the
// speech normaliser reads correctly anyway. Exported because the daemon's
// source adapters compose their own sentences and must count the same way this
// package's headline does: "two sessions" in one line and "2" in the next would
// read as two different voices.
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
// outcomes and source names — never a line, never a headline, never a word the
// model wrote. The activity row built from it says a report was given and stops
// there, which is the leak-salted criterion this feature inherits from #147 by
// way of ADR 0050.
func (s *Service) publishGiven(r Report, reason string) {
	if s.publish == nil {
		return
	}
	data := map[string]any{
		"reason":   reason,
		"lines":    lineTotal(r),
		"sections": len(r.Sections),
		// cached is the caching rule as an outcome rather than as a duration:
		// a row that says a report was replayed is what makes the rule visible
		// to somebody watching the feed instead of reading the ADR.
		"cached":    r.Cached,
		"truncated": r.Truncated,
		"quiet":     r.Quiet,
		// partial is the caveat as an outcome rather than as its sentence: a
		// report that could not cover its own stretch is exactly the kind of
		// thing a row should be able to say, and a bool says it without
		// carrying a word of the account.
		"partial": r.Caveat != "",
		"model":   r.ModelOutcome,
	}
	if len(r.Unavailable) > 0 {
		data["unavailable"] = strings.Join(r.Unavailable, ",")
	}
	s.publish("situation.given", data)
}

func lineTotal(r Report) int {
	total := 0
	for _, section := range r.Sections {
		total += len(section.Lines)
	}
	return total
}
