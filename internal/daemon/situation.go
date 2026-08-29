package daemon

// This file wires the situation report (#196, ADR 0061) into jarvixd: the
// service construction and its late binds, the six source adapters, the model
// call that words the headline, the situation.get IPC method, and the one
// activity row that records a report was given.
//
// Every source below reads something Jarvix already has for its own reasons —
// the focus snapshot the Focus tab renders and the AI-session classifier
// annotates (#137), the reminder store the clockwork owns, the schedule trail
// ADR 0032 persists, the feed statuses the knowledge service keeps warm, the
// activity ring, the window inventory ADR 0022 already reads for anchors.
// Nothing here opens a new window onto the machine, and nothing may be added
// that does: ADR 0050's boundary binds this feature identically and ADR 0061
// says so.
//
// Two properties of the shape are worth naming before the code:
//
//   - The sources are read IN PARALLEL (internal/situation's read). Six
//     sequential reads, two of which touch the compositor and three of which
//     touch disk, add up to a wait a person notices before the first word.
//     Nothing here may therefore assume it runs alone — focusOnce is shared
//     between two of them and is a mutex for exactly that reason.
//   - Each line carries the reference for the thing it is about, and that
//     reference is a provenance one (#168, ADR 0055). The window resolves a
//     situation line with the same verb it resolves "what went into this
//     answer" with, which is why there is no navigation code in this file.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/automation"
	"github.com/rpickz/jarvix/internal/conversations"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/focus"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/provenance"
	"github.com/rpickz/jarvix/internal/reminders"
	"github.com/rpickz/jarvix/internal/session"
	"github.com/rpickz/jarvix/internal/situation"
	"github.com/rpickz/jarvix/internal/transcript"
)

// The situation report's model call. Pinned rather than configurable for the
// recap's reason (ADR 0043): one short sentence fits far inside the cap, and a
// headline wants faithful, not creative.
const (
	situationMaxTokens   = 110
	situationTemperature = 0.2
)

// situationNamed is how many things one rank will name before it stops
// counting them out loud. Three names and a tail reads as a sentence; six names
// reads as a list, and the twenty-second speech budget has better uses for the
// words. Naming at all is the point of the feature — "two sessions are waiting
// on you" is the category the acceptance criteria forbid — so the cap is on the
// tail rather than on the first name.
const situationNamed = 3

// situationWindowsTimeout bounds the one inventory read the desktop source
// makes. Shorter than the whole report's budget, because a wedged compositor
// must cost this report one named unavailable source and not the report.
const situationWindowsTimeout = 2 * time.Second

// newSituationService builds the situation-report service. Built before the
// engine because the engine carries it (session.Options.Operating), with the
// sources and the provider seam bound after the daemon exists — the briefing
// service's construction rule, for the same reason: everything a report reads
// only exists once the daemon does.
//
// The seed is the conversation archive's newest LastActive, the same durable
// record the briefing seeds from and for a closely related reason. "Since you
// last looked" has to survive a restart: without a seed the first report after
// every reboot would have no backward edge at all, so nothing could be reported
// as having finished and — worse — the report could never notice that its own
// activity ring cannot account for the stretch, which is the one admission
// #190 exists to make.
func newSituationService(archive conversations.Store, bus *session.Bus,
	logger *slog.Logger) *situation.Service {
	return situation.NewService(situation.Options{
		Seed: func() (time.Time, bool) { return lastArchived(archive) },
		Publish: func(event string, data map[string]any) {
			bus.Publish(session.Event{Type: event, Data: data})
		},
	}, logger)
}

// bindSituation completes the service once the daemon exists: the six sources,
// the process's own start-up moment, and the provider seam.
//
// The declaration ORDER is load-bearing and is the ordering rule's second half.
// internal/situation orders by rank first and by this list second, so the AI
// sessions lead every rank they share — and `needs_you`, the highest-value fact
// on the machine, is therefore the first thing said without any special case
// for it existing anywhere in the composer.
func (d *Daemon) bindSituation() {
	d.situation.BindStartedAfter(func(since time.Time) bool { return d.started.After(since) })
	// Two sources read the same focus snapshot and read it exactly once per
	// report (focusOnce). That is not only a saving: two snapshots taken a
	// second apart could disagree about the same thread, and a report that
	// contradicts itself between its first line and its fourth is worse than
	// one extra compositor call. Situation's sources run concurrently, which
	// focusOnce's mutex already makes safe — the second caller waits for the
	// first read rather than starting a second one.
	shared := &focusOnce{d: d}
	d.situation.BindSources(
		situation.Source{Name: situation.SourceSessions, Read: shared.situationSessions},
		situation.Source{Name: situation.SourceFocus, Read: shared.situationThreads},
		situation.Source{Name: situation.SourceReminders, Read: d.situationReminders},
		situation.Source{Name: situation.SourceSchedules, Read: d.situationSchedules},
		situation.Source{Name: situation.SourceActivity, Read: d.situationFailures},
		situation.Source{Name: situation.SourceWindows, Read: d.situationWindows},
	)
	d.situation.BindSummarise(d.situationSummarise)
}

// situationSummarise asks the current provider for the headline: one user
// message, no tools, no history — the prompt is the whole exchange, so nothing
// here can leak a conversation into a report or a report into a conversation.
// The briefing's shape (ADR 0050), restated because a report must be able to
// run on a daemon whose focus service is idle.
func (d *Daemon) situationSummarise(ctx context.Context, prompt string) (string, error) {
	d.cfgMu.Lock()
	provider, model := d.provider, d.cfg.AI.Model
	d.cfgMu.Unlock()
	if provider == nil {
		return "", fmt.Errorf("no assistant provider is configured")
	}
	events, err := provider.Chat(ctx, ai.ChatRequest{
		Model:       model,
		Messages:    []ai.Message{{Role: ai.RoleUser, Content: prompt}},
		MaxTokens:   situationMaxTokens,
		Temperature: situationTemperature,
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

// situationSessions is the AI-session source and the reason this feature is
// worth building: the deterministic working / needs_you / done classification
// (#137, ADR 0047) of the sessions anchored to focus threads, split across the
// three ranks it already names.
//
// One line per session, named, because "Claude is waiting on you in the deploy
// thread" is the answer and "two sessions are waiting on you" is the category
// the acceptance criteria rule out. No transcript content travels — the
// classification is a structural read, and the thread's own name, which the
// user chose, is what a line says.
func (f *focusOnce) situationSessions(ctx context.Context, at situation.Instant) ([]situation.Item, error) {
	v, err := f.view(ctx, at.Now)
	if err != nil {
		return nil, err
	}
	var anchored int
	byRank := map[situation.Rank][]focus.ThreadView{}
	for _, tv := range v.Threads {
		if len(tv.Anchors) > 0 {
			anchored++
		}
		switch transcript.State(tv.SessionState) {
		case transcript.StateNeedsYou:
			byRank[situation.NeedsYou] = append(byRank[situation.NeedsYou], tv)
		case transcript.StateWorking:
			byRank[situation.InProgress] = append(byRank[situation.InProgress], tv)
		case transcript.StateDone:
			byRank[situation.Finished] = append(byRank[situation.Finished], tv)
		}
	}
	if anchored > 0 && len(byRank) == 0 && f.d.sessions == nil {
		// Threads are anchored to windows and this daemon has no transcript
		// reader at all, so "no sessions" is not something we know — it is
		// something we could not look at. Named, never quietly omitted.
		return nil, fmt.Errorf("no AI-session transcript reader is available")
	}
	var items []situation.Item
	for _, spec := range []struct {
		rank            situation.Rank
		verb, plural    string
		overflowSubject string
	}{
		{situation.NeedsYou, "is waiting on you", "are waiting on you", "sessions"},
		{situation.InProgress, "is still working", "are still working", "sessions"},
		{situation.Finished, "has finished", "have finished", "sessions"},
	} {
		threads := byRank[spec.rank]
		for i, tv := range threads {
			if i == situationNamed {
				items = append(items, situation.Item{Rank: spec.rank,
					Text: overflowSentence(len(threads)-situationNamed, spec.overflowSubject,
						spec.plural)})
				break
			}
			items = append(items, situation.Item{
				Rank: spec.rank,
				Text: "The AI session on " + tv.Name + " " + spec.verb + ".",
				Where: &provenance.Reference{Kind: provenance.KindThread,
					Strength: provenance.Returned, Ref: tv.ID},
			})
		}
	}
	return items, nil
}

// overflowSentence words the tail of a rank that had more things in it than a
// spoken answer will name. It carries no reference on purpose: it is about a
// group, and a link on it would take the reader to whichever one happened to be
// first, which is the kind of nearly-right navigation that is worse than none.
func overflowSentence(rest int, subject, verb string) string {
	return "And " + situation.CountWord(rest) + " more " + subject + " " + verb + "."
}

// situationThreads is the focus-thread source: the timebox waiting for an
// answer, the timebox still running, and where the user actually is. Thread
// names are labels the user chose, not content Jarvix read anywhere.
func (f *focusOnce) situationThreads(ctx context.Context, at situation.Instant) ([]situation.Item, error) {
	v, err := f.view(ctx, at.Now)
	if err != nil {
		return nil, err
	}
	var items []situation.Item
	if v.Session != nil {
		ref := &provenance.Reference{Kind: provenance.KindThread,
			Strength: provenance.Returned, Ref: v.Session.ThreadID}
		if v.Session.Phase == "closing" {
			items = append(items, situation.Item{Rank: situation.NeedsYou, Where: ref,
				Text: "Your focus session on " + v.Session.ThreadName +
					" is over and still waiting for an answer."})
		} else {
			items = append(items, situation.Item{Rank: situation.InProgress, Where: ref,
				Text: "You're in a focus session on " + v.Session.ThreadName + "."})
		}
	}
	for _, tv := range v.Threads {
		if !tv.Active {
			continue
		}
		text := "You're on " + tv.Name + ", last touched " + tv.LastActivitySpoken
		if n := len(tv.Parked); n > 0 {
			text += ", with " + situation.CountWord(n) + " parked " +
				pluralWord(n, "thought", "thoughts")
		}
		items = append(items, situation.Item{Rank: situation.Housekeeping, Text: text + ".",
			Where: &provenance.Reference{Kind: provenance.KindThread,
				Strength: provenance.Returned, Ref: tv.ID}})
		break
	}
	return items, nil
}

// situationReminders is the reminder source (#141, ADR 0046). Reminder text is
// the user's own sentence, written to be spoken back — that is the whole point
// of the feature — so naming it here breaks no contract.
//
// A reminder that is due now needs the user, and a reminder that fired since
// they last looked is news that it happened. The second half is the report's
// only interval-shaped read besides the failure count, and it therefore obeys
// Instant's zero rule: with no record of a previous look it reports nothing
// rather than reading out the whole fired history as if it had all just
// happened.
func (d *Daemon) situationReminders(_ context.Context, at situation.Instant) ([]situation.Item, error) {
	if d.reminders == nil {
		return nil, nil
	}
	v := d.reminders.Snapshot()
	var items []situation.Item
	due := v.Pending
	for i, p := range due {
		if !p.Due.After(at.Now) {
			if i >= situationNamed {
				items = append(items, situation.Item{Rank: situation.NeedsYou,
					Text: overflowSentence(countDue(due, at.Now)-situationNamed,
						"reminders", "are due")})
				break
			}
			items = append(items, situation.Item{Rank: situation.NeedsYou,
				Text: "A reminder is due: " + p.Text + ".",
				Where: &provenance.Reference{Kind: provenance.KindReminder,
					Strength: provenance.Returned, Ref: p.ID}})
		}
	}
	if at.Since.IsZero() {
		return items, nil
	}
	fired := 0
	for _, f := range v.History {
		if f.Outcome != reminders.OutcomeFired || !f.At.After(at.Since) || f.At.After(at.Now) {
			continue
		}
		fired++
		if fired > situationNamed {
			continue
		}
		// A fired reminder carries no reference: it has left the pending list,
		// so the resolver would honestly report it gone and offer nothing —
		// a row that exists only to say the thing it links to is not there.
		items = append(items, situation.Item{Rank: situation.Finished,
			Text: "A reminder fired: " + f.Text + "."})
	}
	if fired > situationNamed {
		items = append(items, situation.Item{Rank: situation.Finished,
			Text: overflowSentence(fired-situationNamed, "reminders", "fired")})
	}
	return items, nil
}

// countDue counts the pending reminders whose moment has arrived, so the
// overflow tail can say how many it stands for rather than guessing.
func countDue(pending []reminders.PendingView, now time.Time) int {
	n := 0
	for _, p := range pending {
		if !p.Due.After(now) {
			n++
		}
	}
	return n
}

// situationSchedules is the automation source (ADR 0032): a clockfire actually
// in flight, and the shape of what is set to run.
//
// It does not say WHEN the next schedule fires. The daemon has the moment, but
// wording a future moment in speech is a vocabulary this codebase only has for
// reminders (dueSpoken), and a report that invented a second one would be two
// scales for the same thing on the same screen. What it can honestly say is
// which schedule is next, and the line links to it.
func (d *Daemon) situationSchedules(_ context.Context, at situation.Instant) ([]situation.Item, error) {
	if d.automations == nil {
		return nil, nil
	}
	statuses := d.automations.Status()
	var items []situation.Item
	running := 0
	for _, st := range statuses {
		if !st.Running {
			continue
		}
		running++
		if running > situationNamed {
			continue
		}
		items = append(items, situation.Item{Rank: situation.InProgress,
			Text: "Your scheduled " + string(st.Kind) + " " + st.Name + " is running now.",
			Where: &provenance.Reference{Kind: provenance.KindSchedule,
				Strength: provenance.Returned, Ref: scheduleRef(string(st.Kind), st.Name)}})
	}
	if running > situationNamed {
		items = append(items, situation.Item{Rank: situation.InProgress,
			Text: overflowSentence(running-situationNamed, "schedules", "are running")})
	}
	next, ok := nextSchedule(statuses, at.Now)
	if !ok {
		return items, nil
	}
	items = append(items, situation.Item{Rank: situation.Housekeeping,
		Text: situation.CountWord(len(statuses)) + " " +
			pluralWord(len(statuses), "schedule is", "schedules are") +
			" set; " + next.Name + " is next.",
		Where: &provenance.Reference{Kind: provenance.KindSchedule,
			Strength: provenance.Returned, Ref: scheduleRef(string(next.Kind), next.Name)}})
	return items, nil
}

// nextSchedule picks the soonest schedule still ahead of now. A schedule whose
// next fire is unknown or already behind is skipped rather than reported: a
// "next" that is in the past is a fact about the scheduler, not about the day.
func nextSchedule(statuses []automation.Status, now time.Time) (automation.Status, bool) {
	var best automation.Status
	found := false
	for _, st := range statuses {
		if st.NextFire.IsZero() || !st.NextFire.After(now) {
			continue
		}
		if !found || st.NextFire.Before(best.NextFire) {
			best, found = st, true
		}
	}
	return best, found
}

// situationFailures is the failing source: the feeds that are failing now, and
// what has failed since the user last looked.
//
// The two halves have very different provenance and the report treats them
// differently because of it. A feed's failure is durable state the knowledge
// service holds, so it is named and it links to the feed. The activity ring's
// failures are honest observation from an in-memory ring that dies with the
// process (#70): they are counted rather than named, they carry no reference,
// and they are the reason the report has an up-front restart caveat at all.
func (d *Daemon) situationFailures(_ context.Context, at situation.Instant) ([]situation.Item, error) {
	var items []situation.Item
	if d.knowledge != nil {
		failing := 0
		for _, st := range d.knowledge.Status() {
			if !st.Failing {
				continue
			}
			failing++
			if failing > situationNamed {
				continue
			}
			items = append(items, situation.Item{Rank: situation.Failing,
				Text: "The " + st.Name + " feed is failing.",
				Where: &provenance.Reference{Kind: provenance.KindFeed,
					Strength: provenance.Returned, Ref: st.Name}})
		}
		if failing > situationNamed {
			items = append(items, situation.Item{Rank: situation.Failing,
				Text: overflowSentence(failing-situationNamed, "feeds", "are failing")})
		}
	}
	if at.Since.IsZero() {
		// Interval-shaped, so Instant's zero rule applies: with no record of a
		// previous look the ring's whole contents are not news.
		return items, nil
	}
	if failed := d.activityFailures(at.Since, at.Now); failed > 0 {
		items = append(items, situation.Item{Rank: situation.Failing,
			Text: capitaliseFirst(situation.CountWord(failed)) + " " +
				pluralWord(failed, "thing has", "things have") +
				" failed since you last looked."})
	}
	return items, nil
}

// situationWindows is the desktop source: the shape of what is open, and where
// the user actually is.
//
// It carries no reference. A window is not a thing this vocabulary can point
// at: compositor addresses are opaque handles that deliberately never travel on
// the wire (ADR 0022, and the overlay feed's own rule), and a line about the
// shape of a whole desktop is not about one window anyway. The honest answer is
// a line with no link, which the window renders as a line with no button.
func (d *Daemon) situationWindows(ctx context.Context, _ situation.Instant) ([]situation.Item, error) {
	if d.compositor == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, situationWindowsTimeout)
	defer cancel()
	windows, err := d.compositor.Windows(ctx)
	if err != nil {
		return nil, err
	}
	if len(windows) == 0 {
		return nil, nil
	}
	spaces := map[int]bool{}
	here := ""
	for _, w := range windows {
		spaces[w.Workspace] = true
		if w.Focused {
			here = w.Class
		}
	}
	text := capitaliseFirst(situation.CountWord(len(windows))) + " " +
		pluralWord(len(windows), "window is", "windows are") + " open across " +
		situation.CountWord(len(spaces)) + " " + pluralWord(len(spaces), "workspace", "workspaces")
	if here != "" {
		text += "; you're in " + here
	}
	return []situation.Item{{Rank: situation.Housekeeping, Text: text + "."}}, nil
}

// registerSituationMethods adds situation.get: the Situation tab's read and the
// full, untruncated version of what the voice path shortens. Display-only on
// ADR 0013's terms — every sentence in the reply was composed daemon-side, and
// so was every link.
func (d *Daemon) registerSituationMethods() {
	d.server.Handle("situation.get", func(params json.RawMessage) (any, error) {
		var p struct {
			Fresh bool `json:"fresh"`
		}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "situation.get: %v", err)
			}
		}
		view, err := d.situation.View(context.Background(), p.Fresh)
		if err != nil {
			return nil, err
		}
		return situationReport(view), nil
	})
}

// situationReport renders one report for the wire.
//
// `sources` is the flat array of provenance references, in render order, and
// each line's `link` is its index into it — so a client hands `sources`
// straight to provenance.resolve and reads each line's resolved item back at
// its own index, doing no arithmetic and having no chance to pair a line with
// somebody else's subject. A line with nothing to point at simply has no
// `link` key.
func situationReport(r situation.Report) map[string]any {
	sections := make([]map[string]any, 0, len(r.Sections))
	for _, s := range r.Sections {
		lines := make([]map[string]any, 0, len(s.Lines))
		for _, line := range s.Lines {
			out := map[string]any{"text": line.Text}
			if line.Link >= 0 {
				out["link"] = line.Link
			}
			lines = append(lines, out)
		}
		sections = append(sections, map[string]any{"title": s.Title, "lines": lines})
	}
	sources := make([]provenance.Reference, len(r.Sources))
	copy(sources, r.Sources)
	return map[string]any{
		"headline":   r.Headline,
		"caveat":     r.Caveat,
		"spoken":     r.Spoken,
		"truncated":  r.Truncated,
		"quiet":      r.Quiet,
		"cached":     r.Cached,
		"at":         r.At.Format(time.RFC3339),
		"age_spoken": r.AgeSpoken,
		"sections":   sections,
		"sources":    sources,
	}
}

// situationActivityRow words the "a report was given" row. Like the briefing's,
// it carries counts and outcomes only: not one word of the report appears here,
// which is the leak-salted criterion this feature inherits from #147 by way of
// ADR 0050.
func situationActivityRow(data map[string]any) desktop.ActivityRow {
	lines := 0
	switch v := data["lines"].(type) {
	case int:
		lines = v
	case float64:
		lines = int(v)
	}
	detail := fmt.Sprintf("%d %s", lines, pluralWord(lines, "line", "lines"))
	if reason, _ := data["reason"].(string); reason != "" {
		detail = situationReason(reason) + " · " + detail
	}
	if cached, _ := data["cached"].(bool); cached {
		detail += " · read again from the last one"
	}
	if truncated, _ := data["truncated"].(bool); truncated {
		detail += " · shortened for speech"
	}
	if quiet, _ := data["quiet"].(bool); quiet {
		detail += " · nothing needed you"
	}
	if partial, _ := data["partial"].(bool); partial {
		detail += " · I restarted since you last looked"
	}
	if unavailable, _ := data["unavailable"].(string); unavailable != "" {
		detail += " · could not read: " + unavailable
	}
	return desktop.ActivityRow{
		Kind:   desktop.ActivityKindAssistant,
		Label:  "Situation report given",
		Detail: detail,
	}
}

// situationReason names who asked, in words. Saying which one it was is what
// makes an unexpected row explicable.
func situationReason(reason string) string {
	switch reason {
	case "ask":
		return "you asked"
	case "window":
		return "opened in the window"
	case "refresh":
		return "refreshed in the window"
	default:
		return reason
	}
}
