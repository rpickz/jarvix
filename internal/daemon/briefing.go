package daemon

// This file wires the return briefing (#150, ADR 0050) into jarvixd: the
// service construction and its late binds, the five source adapters, the
// model call that words the headline, the briefing.get IPC method, and the
// one activity row that records a briefing was given.
//
// Every source below reads something Jarvix already has for its own reasons —
// the focus snapshot the Focus tab renders, the reminder store the clockwork
// owns, the schedule trail ADR 0032 persists, the feed statuses the knowledge
// service keeps warm, the conversation metadata the archive writes. Nothing
// here opens a new window onto the machine, and nothing may be added that
// does: the boundary is in ADR 0050 and it is the reason this feature exists
// in the form it does.
//
// Two sources — the AI sessions and the focus threads — read the same focus
// snapshot, and they read it exactly once per briefing (see focusOnce). That
// is not only a saving: two snapshots taken a second apart could disagree
// about the same thread, and a briefing that contradicts itself between its
// second and fourth line is worse than one that costs an extra compositor
// call.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/briefing"
	"github.com/rpickz/jarvix/internal/conversations"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/focus"
	"github.com/rpickz/jarvix/internal/reminders"
	"github.com/rpickz/jarvix/internal/session"
	"github.com/rpickz/jarvix/internal/transcript"
)

// The briefing's model call. Pinned rather than configurable for the recap's
// reason (ADR 0043): one short sentence fits far inside the cap, and a
// headline wants faithful, not creative.
const (
	briefingMaxTokens   = 120
	briefingTemperature = 0.2
)

// briefingNamedThreads is how many threads one line will name before it
// stops counting them out loud. Two names and a count reads as a sentence;
// five names reads as a list, and the spoken budget has better uses for the
// words.
const briefingNamedThreads = 2

// newBriefingService builds the return-briefing service. Built before the
// engine because the engine carries it (session.Options.Returning), with the
// sources and the provider seam bound after the daemon exists — the focus
// service's construction rule, for the same reason: half of what a briefing
// reads only exists once the daemon does.
//
// The seed is the conversation archive's newest LastActive: the one durable
// record of when the daemon last dealt with the user, so an absence that
// spans a restart is still an absence. It is deliberately not the engine's
// lastTurn, which the follow-up window zeroes — a lapsed follow-up is a fact
// about working memory, not about whether anyone was here.
func newBriefingService(archive conversations.Store, bus *session.Bus,
	logger *slog.Logger) *briefing.Service {
	return briefing.NewService(briefing.Options{
		Seed: func() (time.Time, bool) { return lastArchived(archive) },
		Publish: func(event string, data map[string]any) {
			bus.Publish(session.Event{Type: event, Data: data})
		},
	}, logger)
}

// lastArchived reads the newest conversation's last-active moment. Metadata
// only — List never opens a transcript (ADR 0027) — and every failure is
// "no idea", which reads as "not away": a briefing is never invented for an
// absence that cannot be demonstrated.
func lastArchived(archive conversations.Store) (time.Time, bool) {
	if archive == nil {
		return time.Time{}, false
	}
	metas, _, err := archive.List()
	if err != nil || len(metas) == 0 {
		return time.Time{}, false
	}
	// List is newest-first by LastActive.
	if metas[0].LastActive.IsZero() {
		return time.Time{}, false
	}
	return metas[0].LastActive, true
}

// bindBriefing completes the service once the daemon exists: the five
// sources and the provider seam, both of which read the running config at
// call time so a reload lands without a restart.
func (d *Daemon) bindBriefing() {
	d.briefing.BindSettings(func() briefing.Settings {
		c := d.runningConfig()
		return briefing.Settings{
			Enabled:       c.Briefing.Enabled,
			AfterHours:    c.Briefing.AfterHours,
			SpeakOnReturn: c.Briefing.SpeakOnReturn,
		}
	})
	// The one thing a briefing has to be able to say about itself: whether this
	// process was running for the whole window. The daemon owns the answer —
	// it is the only thing that knows when it started — and internal/briefing
	// owns the sentence, beside the source names it has to spell out (#190).
	d.briefing.BindStartedAfter(func(since time.Time) bool { return d.started.After(since) })
	shared := &focusOnce{d: d}
	d.briefing.BindSources(
		// Jobs lead, for the situation report's reason with more force here: a
		// job that parked at two in the morning has been stopped all night, and
		// a briefing that mentioned it fourth would be burying the one thing
		// that has been waiting longest.
		briefing.Source{Name: briefing.SourceJobs, Read: d.briefJobs},
		briefing.Source{Name: briefing.SourceSessions, Read: shared.sessions},
		briefing.Source{Name: briefing.SourceReminders, Read: d.briefReminders},
		briefing.Source{Name: briefing.SourceFocus, Read: shared.threads},
		briefing.Source{Name: briefing.SourceActivity, Read: d.briefActivity},
		briefing.Source{Name: briefing.SourceConversations, Read: d.briefConversations},
	)
	d.briefing.BindSummarise(d.briefingSummarise)
}

// briefingSummarise asks the current provider for the headline: one user
// message, no tools, no history — the prompt is the whole exchange, so
// nothing here can leak a conversation into a briefing or a briefing into a
// conversation. The recap's shape (ADR 0043), restated because the briefing
// must be able to run on a daemon whose focus service is idle.
func (d *Daemon) briefingSummarise(ctx context.Context, prompt string) (string, error) {
	d.cfgMu.Lock()
	provider, model := d.provider, d.cfg.AI.Model
	d.cfgMu.Unlock()
	if provider == nil {
		return "", fmt.Errorf("no assistant provider is configured")
	}
	events, err := provider.Chat(ctx, ai.ChatRequest{
		Model:       model,
		Messages:    []ai.Message{{Role: ai.RoleUser, Content: prompt}},
		MaxTokens:   briefingMaxTokens,
		Temperature: briefingTemperature,
	})
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for ev := range events {
		switch ev.Type {
		case ai.EventDelta:
			b.WriteString(ev.Content)
		case ai.EventError:
			return "", ev.Err
		default:
			// EventDone ends the stream; a tool call cannot arrive for a
			// request that advertised no tools.
		}
	}
	return b.String(), nil
}

// focusOnce reads the focus snapshot once per briefing and serves it to both
// sources that need it. The key is the compose's own `now`, which every
// source in one briefing is handed identically — so "the same briefing" is a
// fact rather than a timing assumption, and two briefings a millisecond apart
// still each get their own read.
type focusOnce struct {
	d  *Daemon
	mu sync.Mutex
	at time.Time
	v  focus.View
}

func (f *focusOnce) view(ctx context.Context, now time.Time) (focus.View, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.at.Equal(now) {
		return f.v, nil
	}
	v := f.d.focus.Snapshot(ctx)
	if err := ctx.Err(); err != nil {
		// The snapshot degrades rather than failing — a wedged compositor
		// costs anchors, not threads — so the context is what tells us the
		// read was cut short. A half-read snapshot must be named unavailable,
		// never presented as the state of things.
		return focus.View{}, err
	}
	f.at, f.v = now, v
	return v, nil
}

// sessions is the AI-session source: the deterministic working / needs_you /
// done classification (#137, ADR 0047) of the sessions anchored to focus
// threads, split across the three categories it already names. No transcript
// content travels — the classification is a structural read and the thread's
// own name is what a line says.
func (f *focusOnce) sessions(ctx context.Context, _, now time.Time) ([]briefing.Line, error) {
	v, err := f.view(ctx, now)
	if err != nil {
		return nil, err
	}
	var anchored int
	var needsYou, done, working []string
	for _, tv := range v.Threads {
		if len(tv.Anchors) > 0 {
			anchored++
		}
		switch transcript.State(tv.SessionState) {
		case transcript.StateNeedsYou:
			needsYou = append(needsYou, tv.Name)
		case transcript.StateDone:
			done = append(done, tv.Name)
		case transcript.StateWorking:
			working = append(working, tv.Name)
		}
	}
	if anchored > 0 && len(needsYou)+len(done)+len(working) == 0 && f.d.sessions == nil {
		// Threads are anchored to windows and this daemon has no transcript
		// reader at all, so "no sessions" is not something we know — it is
		// something we could not look at. Named, never quietly omitted.
		return nil, fmt.Errorf("no AI-session transcript reader is available")
	}
	var lines []briefing.Line
	if len(needsYou) > 0 {
		lines = append(lines, briefing.Line{Category: briefing.Awaiting,
			Text: sessionSentence(needsYou, "is waiting on you", "are waiting on you")})
	}
	if len(done) > 0 {
		lines = append(lines, briefing.Line{Category: briefing.Completed,
			Text: sessionSentence(done, "has finished", "have finished")})
	}
	if len(working) > 0 {
		lines = append(lines, briefing.Line{Category: briefing.InProgress,
			Text: sessionSentence(working, "is still working", "are still working")})
	}
	return lines, nil
}

// sessionSentence words one category of AI sessions: named while there are
// few enough to name, counted after that.
func sessionSentence(names []string, singular, plural string) string {
	if len(names) == 1 {
		return "The session on " + names[0] + " " + singular + "."
	}
	if len(names) <= briefingNamedThreads {
		return "The sessions on " + joinNames(names) + " " + plural + "."
	}
	head := joinNames(names[:briefingNamedThreads])
	rest := len(names) - briefingNamedThreads
	return briefing.CountWord(len(names)) + " sessions " + plural + ", including " + head +
		" and " + briefing.CountWord(rest) + " more."
}

// threads is the focus-thread source: the timebox waiting for an answer, the
// timebox still running, and where the active thread stands. Thread names are
// labels the user chose, not content Jarvix read anywhere.
func (f *focusOnce) threads(ctx context.Context, _, now time.Time) ([]briefing.Line, error) {
	v, err := f.view(ctx, now)
	if err != nil {
		return nil, err
	}
	var lines []briefing.Line
	if v.Session != nil {
		switch v.Session.Phase {
		case "closing":
			lines = append(lines, briefing.Line{Category: briefing.Awaiting,
				Text: "Your focus session on " + v.Session.ThreadName +
					" is over and still waiting for an answer."})
		default:
			lines = append(lines, briefing.Line{Category: briefing.InProgress,
				Text: "The focus session on " + v.Session.ThreadName + " is still running."})
		}
	}
	for _, tv := range v.Threads {
		if !tv.Active {
			continue
		}
		text := "Your thread is " + tv.Name + ", last touched " + tv.LastActivitySpoken
		if n := len(tv.Parked); n > 0 {
			text += ", with " + briefing.CountWord(n) + " parked " +
				pluralWord(n, "thought", "thoughts")
		}
		lines = append(lines, briefing.Line{Category: briefing.Housekeeping, Text: text + "."})
		break
	}
	return lines, nil
}

// briefReminders is the reminder source (#141, ADR 0046): what fired while
// the user was away, and what is owed now. Reminder text is the user's own
// sentence, written to be spoken back — that is the whole point of the
// feature — so naming it here breaks no contract.
func (d *Daemon) briefReminders(_ context.Context, since, now time.Time) ([]briefing.Line, error) {
	if d.reminders == nil {
		return nil, nil
	}
	v := d.reminders.Snapshot()
	var fired, due []string
	for _, f := range v.History {
		if f.Outcome != reminders.OutcomeFired {
			continue
		}
		if f.At.After(since) && !f.At.After(now) {
			fired = append(fired, f.Text)
		}
	}
	for _, p := range v.Pending {
		if !p.Due.After(now) {
			due = append(due, p.Text)
		}
	}
	var lines []briefing.Line
	if len(due) > 0 {
		lines = append(lines, briefing.Line{Category: briefing.Awaiting,
			Text: countedSentence(due, "reminder is due now", "reminders are due now")})
	}
	if len(fired) > 0 {
		lines = append(lines, briefing.Line{Category: briefing.Completed,
			Text: countedSentence(fired, "reminder fired while you were away",
				"reminders fired while you were away")})
	}
	return lines, nil
}

// countedSentence words a small set of user sentences: named while there are
// few enough, counted after that.
func countedSentence(texts []string, singular, plural string) string {
	if len(texts) == 1 {
		return "One " + singular + ": " + texts[0] + "."
	}
	if len(texts) <= briefingNamedThreads {
		return briefing.CountWord(len(texts)) + " " + plural + ": " + joinNames(texts) + "."
	}
	return briefing.CountWord(len(texts)) + " " + plural + ", starting with " + texts[0] + "."
}

// briefActivity is the activity source: the schedules that ran, the feeds
// that refreshed, and what failed. The schedule trail is persisted (ADR 0032)
// and the feed statuses are current state, so both are true across a restart;
// the failure count comes from the in-memory ring, which is honest observation
// rather than a transaction log.
//
// This line used to carry the restart admission itself — "My own record only
// goes back to when I restarted." — and it is now made by the briefing, up
// front, for every window that predates start-up (#190). Attached here it fired
// only when this source had produced a line at all, which is the opposite of
// the case that needed it: a window entirely before the restart is the one most
// likely to leave the ring with nothing to say, and so the one where the
// admission was silently dropped and the remaining, durable sources read as a
// confident "nothing happened".
func (d *Daemon) briefActivity(_ context.Context, since, now time.Time) ([]briefing.Line, error) {
	var parts []string
	if d.automations != nil {
		ran := 0
		for _, st := range d.automations.Status() {
			if st.LastFired.After(since) && !st.LastFired.After(now) {
				ran++
			}
		}
		if ran > 0 {
			parts = append(parts, briefing.CountWord(ran)+" of your schedules ran")
		}
	}
	if d.knowledge != nil {
		refreshed, failing := 0, 0
		for _, st := range d.knowledge.Status() {
			if st.FetchedAt.After(since) && !st.FetchedAt.After(now) {
				refreshed++
			}
			if st.Failing {
				failing++
			}
		}
		if refreshed > 0 {
			parts = append(parts, briefing.CountWord(refreshed)+" "+
				pluralWord(refreshed, "feed", "feeds")+" refreshed")
		}
		if failing > 0 {
			parts = append(parts, briefing.CountWord(failing)+" "+
				pluralWord(failing, "feed is", "feeds are")+" failing")
		}
	}
	if failed := d.activityFailures(since, now); failed > 0 {
		parts = append(parts, briefing.CountWord(failed)+" "+
			pluralWord(failed, "thing", "things")+" failed")
	}
	if len(parts) == 0 {
		return nil, nil
	}
	return []briefing.Line{{Category: briefing.Housekeeping,
		Text: capitaliseFirst(joinClauses(parts)) + "."}}, nil
}

// activityFailures counts the failed rows the ring still holds inside the
// window. Bounded and lossy by design (internal/daemon/activity.go); the
// briefing's up-front restart caveat is what keeps that honest.
func (d *Daemon) activityFailures(since, now time.Time) int {
	d.actMu.Lock()
	defer d.actMu.Unlock()
	failed := 0
	for _, e := range d.activity {
		if e.row.Failed && e.ts.After(since) && !e.ts.After(now) {
			failed++
		}
	}
	return failed
}

// briefConversations is the conversation source: how many exchanges there
// were, and nothing else.
//
// The ticket's "and the last topic" is deliberately not answered here. The
// archive has no topic: the only per-conversation string it holds is Preview,
// which is the first line the user actually said, and speaking it back is the
// replaying of content ADR 0027 and ADR 0028 forbid and this feature's own
// scope boundary rules out. The thread the work sat on is a label the user
// chose, and the focus source above already names it — that is the honest
// version of "topic" this daemon can offer.
func (d *Daemon) briefConversations(_ context.Context, since, now time.Time) ([]briefing.Line, error) {
	if d.conversations == nil {
		return nil, nil
	}
	metas, _, err := d.conversations.List()
	if err != nil {
		return nil, err
	}
	touched := 0
	for _, m := range metas {
		if m.LastActive.After(since) && !m.LastActive.After(now) {
			touched++
		}
	}
	if touched == 0 {
		return nil, nil
	}
	return []briefing.Line{{Category: briefing.Housekeeping,
		Text: capitaliseFirst(briefing.CountWord(touched)) + " " +
			pluralWord(touched, "conversation", "conversations") + " here " +
			pluralWord(touched, "was", "were") + " added to while you were away."}}, nil
}

// joinNames renders a short list the way a sentence would.
func joinNames(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	case 2:
		return names[0] + " and " + names[1]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
}

// joinClauses is joinNames for clauses, kept separate so the two can be
// worded differently later without one quietly changing the other.
func joinClauses(parts []string) string { return joinNames(parts) }

func pluralWord(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// registerBriefingMethods adds briefing.get: the Focus tab's button and the
// full, untruncated version of what the voice path shortens. A read with no
// params, like focus.list, and display-only on the same terms (ADR 0013) —
// every sentence in the reply was composed here.
func (d *Daemon) registerBriefingMethods() {
	d.server.Handle("briefing.get", func(json.RawMessage) (any, error) {
		view, err := d.briefing.View(context.Background())
		if err != nil {
			return nil, err
		}
		return briefingViewReport(view), nil
	})
}

// briefingViewReport renders one briefing for the wire. Times are RFC 3339;
// the absence arrives pre-worded (the shared spoken scale), so no client
// invents its own arithmetic.
func briefingViewReport(v briefing.Composed) map[string]any {
	sections := make([]map[string]any, 0, len(v.Sections))
	for _, s := range v.Sections {
		lines := make([]string, len(s.Lines))
		copy(lines, s.Lines)
		sections = append(sections, map[string]any{"title": s.Title, "lines": lines})
	}
	out := map[string]any{
		"disabled":  v.Disabled,
		"no_record": v.NoRecord,
		"empty":     v.Empty,
		"truncated": v.Truncated,
		"headline":  v.Headline,
		"caveat":    v.Caveat,
		"spoken":    v.Spoken,
		"sections":  sections,
	}
	if !v.Since.IsZero() {
		out["since"] = v.Since.Format(time.RFC3339)
		out["away_spoken"] = v.AwaySpoken
	}
	return out
}

// briefingActivityRow words the "a briefing was given" row. Worded beside its
// own verb rather than in the desktop vocabulary, the replay row's precedent
// (replay.go) — and, like it, the row carries counts and outcomes only. Not
// one word of the briefing appears here, which is the leak-salted criterion
// this feature inherits from #147: the composed account exists in the spoken
// sentence and nowhere else.
func briefingActivityRow(data map[string]any) desktop.ActivityRow {
	lines := 0
	switch v := data["lines"].(type) {
	case int:
		lines = v
	case float64:
		lines = int(v)
	}
	detail := fmt.Sprintf("%d %s", lines, pluralWord(lines, "line", "lines"))
	if reason, _ := data["reason"].(string); reason != "" {
		detail = briefingReason(reason) + " · " + detail
	}
	if truncated, _ := data["truncated"].(bool); truncated {
		detail += " · shortened for speech"
	}
	if partial, _ := data["partial"].(bool); partial {
		detail += " · part of the window predates my start-up"
	}
	if unavailable, _ := data["unavailable"].(string); unavailable != "" {
		detail += " · could not read: " + unavailable
	}
	return desktop.ActivityRow{
		Kind:   desktop.ActivityKindAssistant,
		Label:  "Return briefing given",
		Detail: detail,
	}
}

// briefingReason names who asked, in words. The three are the only ways a
// briefing can happen, and saying which one it was is what makes an
// unexpected row explicable.
func briefingReason(reason string) string {
	switch reason {
	case "ask":
		return "you asked"
	case "return":
		return "spoken on return"
	case "window":
		return "opened in the window"
	default:
		return reason
	}
}
