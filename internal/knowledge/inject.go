package knowledge

import (
	"fmt"
	"strings"

	"github.com/rpickz/jarvix/internal/memory"
)

// This file builds the block of feed values a model turn receives, for feeds
// that opted in with inject = true. It is the memory block's budget
// discipline (ADR 0025) applied to feeds: the block is measured with the same
// token estimate, values that do not fit are dropped from the block (never
// from the cache), and the model is told the list is incomplete so it reaches
// for knowledge.get instead of concluding a feed has no value.

// injectionPreamble introduces the block with its provenance and the one
// behaviour that matters: the ages are part of the answer.
const injectionPreamble = "Live feed values: current readings from feeds the user configured, " +
	"fetched at the ages shown. Use one only when it helps answer what the user actually asked, " +
	"never recite this list unprompted — and when you answer from one, tell the user its age in " +
	"the words shown."

// Injection is the assembled block plus the accounting every disclosure
// surface reports — counts and estimates, never values.
type Injection struct {
	// Message is the system message to inject; empty when nothing qualifies.
	Message string
	// Feeds is how many values the block carries.
	Feeds int
	// Names are the feeds the block carries, in block order — names only,
	// never values. The counts above were enough while the only consumers
	// were disclosure events, but "what went into this answer" (issue #168)
	// has to name the specific feed, and a count cannot. A name is the feed's
	// identity and already travels on every knowledge surface; the value
	// stays where it always was, in the block and the tab.
	Names []string
	// Trimmed is how many qualifying values were dropped to fit the budget.
	Trimmed int
	// EstTokens is the estimated token cost of Message.
	EstTokens int
}

// Inject builds the feed block for one model turn from whatever is cached
// right now — it never fetches, because a turn must not wait on a feed
// command. Declaration order decides who survives the budget: the user
// ordered the feeds, so the user decided their priority.
func (s *Service) Inject() Injection {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	now := s.now()
	lines := make([]string, 0, len(s.feeds))
	names := make([]string, 0, len(s.feeds))
	for _, f := range s.feeds {
		// A disabled feed keeps its value but leaves the model's context: the
		// user parked it, and injecting an ageing value would un-park it in
		// the one place they cannot see.
		if !f.Inject || !f.Enabled {
			continue
		}
		r := s.readingLocked(f)
		if !r.HasValue {
			continue
		}
		line := fmt.Sprintf("- %s (%s): %s (as of %s", f.Name, f.Description, r.Value,
			SpokenAge(now, r.FetchedAt))
		if r.Stale {
			line += "; stale — a fresher value could not be fetched"
		}
		line += ")"
		lines = append(lines, line)
		names = append(names, f.Name)
	}
	return buildInjection(lines, names, s.maxInjected)
}

// buildInjection assembles the block, keeping lines while the whole message —
// preamble, entries, and the trim disclosure it may need — fits capTokens,
// then dropping from the end of the list. names runs parallel to lines, so
// the kept feeds are named by exactly the same cut.
func buildInjection(lines, names []string, capTokens int) Injection {
	if len(lines) == 0 {
		return Injection{}
	}
	kept := len(lines)
	for kept > 0 {
		msg := assemble(lines[:kept], len(lines)-kept)
		if memory.EstimateTokens(msg) <= capTokens {
			return Injection{
				Message:   msg,
				Feeds:     kept,
				Names:     append([]string(nil), names[:kept]...),
				Trimmed:   len(lines) - kept,
				EstTokens: memory.EstimateTokens(msg),
			}
		}
		kept--
	}
	// Not even one value fits — a pathological cap against a pathological
	// value. Disclose that feeds exist but could not be carried, so the model
	// knows to use the tool rather than deny.
	msg := assemble(nil, len(lines))
	return Injection{Message: msg, Trimmed: len(lines), EstTokens: memory.EstimateTokens(msg)}
}

// assemble renders the final message: preamble, entries, and — only when the
// cap trimmed something — the disclosure that the list is incomplete.
func assemble(lines []string, trimmed int) string {
	var b strings.Builder
	b.WriteString(injectionPreamble)
	if len(lines) > 0 {
		b.WriteString("\n\n")
		b.WriteString(strings.Join(lines, "\n"))
	}
	if trimmed > 0 {
		fmt.Fprintf(&b, "\n\n(%d more feed %s left out to save space; read them with the "+
			"knowledge.get tool.)", trimmed, plural(trimmed, "value was", "values were"))
	}
	return b.String()
}

// plural picks the grammatical form for n.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
