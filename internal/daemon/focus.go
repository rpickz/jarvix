package daemon

// This file wires focus threads (#123, ADR 0041) into jarvixd: the service
// construction and its late binds, the firing path for check-in reminders
// and timebox moments, the focus.* IPC methods (the Focus tab's surface and
// the integration contract for the bar/overlay siblings), and the shutdown
// drain adapters.
//
// The firing path is deliberately not a private speech channel. A check-in
// or a timebox moment starts a scheduled session and submits an ordinary
// focus phrase — "where am i on <thread>", "focus session update" — exactly
// as ADR 0032's clockfires replay a routine's trigger, so the intent router,
// the events, the activity feed, and the conversation record all apply
// identically however the sentence was asked for. The do-not-nag rule falls
// out of the same reuse: StartScheduledSession refuses while any session is
// live or speech is playing, and a refused firing is dropped with a report —
// never queued, never retried into a backlog.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/focus"
	"github.com/rpickz/jarvix/internal/intent"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/session"
	"github.com/rpickz/jarvix/internal/statehold"
	"github.com/rpickz/jarvix/internal/tools"
	"github.com/rpickz/jarvix/internal/transcript"
)

// focusWindowsTimeout bounds one inventory read for anchors: a wedged
// compositor must degrade an anchor, never hang a recap.
const focusWindowsTimeout = 3 * time.Second

// focusClassifyTimeout bounds one session-state read for focus.list (#137).
// Tighter than the recap budget because a list is served per thread and the
// read is local disk — a stall must degrade one thread's dot to unknown,
// never make the Focus tab wait on it.
const focusClassifyTimeout = time.Second

// newFocusService builds the thread store over the shared compositor seam.
// Built before the daemon exists because the engine's intent runner carries
// it; the firing path and the midpoint switch bind after (bindFocus), the
// capture service's pattern.
func newFocusService(paths config.Paths, compositor desktop.Compositor, bus *session.Bus,
	gate *statehold.Gate, logger *slog.Logger) *focus.Service {
	return focus.NewService(paths.FocusFile(), focus.Options{
		Gate: gate,
		Windows: func(ctx context.Context) ([]desktop.Window, error) {
			ctx, cancel := context.WithTimeout(ctx, focusWindowsTimeout)
			defer cancel()
			return compositor.Windows(ctx)
		},
		Publish: func(event string, data map[string]any) {
			bus.Publish(session.Event{Type: event, Data: data})
		},
	}, logger)
}

// bindFocus completes the service once the daemon exists: the firing path,
// the midpoint switch, and the AI-session recap's capture, summarise and
// classify halves (#124, #137; ADR 0043, 0047) — all of which read the
// running config at call time so a reload lands without a restart.
func (d *Daemon) bindFocus() {
	d.focus.Bind(d.fireFocus, func() bool {
		return d.runningConfig().Focus.MidpointCheckin
	})
	d.focus.BindRecap(d.recapCapture, d.recapSummarise, d.classifySession)
}

// The AI-session recap's model call (#124). The token cap and temperature
// are pinned rather than configurable: three short sentences fit far inside
// the cap, and a recap wants faithful, not creative.
const (
	recapMaxTokens   = 200
	recapTemperature = 0.2
)

// recapCapture reads one anchored window for the AI-session recap, through
// the same consent, bound, and redaction rules as desktop context (ADR
// 0019): the window source's opt-in gates it entirely, the [context] char
// cap bounds it, and anything that looks like a secret is replaced wholesale
// before it can reach a prompt. The richest layer that can be read answers
// (#137, ADR 0047): the session's own transcript tail when the window's
// process tree hosts a Claude Code or opencode session, the window's
// identity line — app and live title — otherwise. This is exactly the
// richer-gatherer slot ADR 0043 left open: the seam and the focus package
// did not change shape, only what flows through them.
func (d *Daemon) recapCapture(ctx context.Context, a focus.Anchor) (focus.Capture, error) {
	cfg := d.runningConfig()
	if !cfg.Context.Window {
		// The user keeps Jarvix's eyes closed; the recap must not open them.
		return focus.Capture{}, focus.ErrRecapUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, focusWindowsTimeout)
	defer cancel()
	inventory, err := d.compositor.Windows(ctx)
	if err != nil {
		return focus.Capture{}, fmt.Errorf("the desktop could not be read: %w", err)
	}
	maxChars := cfg.Context.MaxChars
	if maxChars <= 0 {
		maxChars = desktop.DefaultMaxChars
	}
	for _, w := range inventory {
		if w.Address != a.Address {
			continue
		}
		capture := focus.Capture{Terminal: terminalClass(cfg, w)}
		if tail, err := d.sessionTail(ctx, w.PID); err == nil {
			// The transcript layer. Redact per line before the clamp — the
			// collector's own ordering, applied line-wise because one keyed
			// line must not silence the whole exchange — and clamp from the
			// TAIL: the newest exchange is the reason this capture exists.
			capture.Text = clampTailCaptureRunes(redactLines(tail.Text), maxChars)
			capture.Transcript = true
			capture.State = string(tail.State)
			return capture, nil
		} else if !errors.Is(err, transcript.ErrNoSession) {
			// A session provably exists and could not be read: fall through
			// to the title layer, flagged so the recap admits the downgrade.
			// Mere absence stays silent — most terminals host no AI session,
			// and #124's behaviour is unchanged for them.
			capture.TranscriptLost = true
		}
		text := desktop.AppName(w.Class)
		if title := strings.TrimSpace(w.Title); title != "" && title != text {
			if text != "" {
				text += " — "
			}
			text += title
		}
		// Redact before clamp, the collector's own ordering: a key cut in
		// half is still a leak.
		if redacted, ok := desktop.Redact(text); ok {
			text = redacted
		}
		capture.Text = clampCaptureRunes(text, maxChars)
		return capture, nil
	}
	return focus.Capture{}, fmt.Errorf("the anchored window is gone")
}

// sessionTail reads the newest AI-session transcript hosted by a window's
// process tree. It only exists when the daemon has a transcript reader; a
// daemon without one (an unresolvable home at start-up) reports absence and
// every recap stays on the title layer.
func (d *Daemon) sessionTail(ctx context.Context, pid int) (transcript.Tail, error) {
	if d.sessions == nil {
		return transcript.Tail{}, transcript.ErrNoSession
	}
	return d.sessions.ReadWindow(ctx, pid)
}

// classifySession is the focus Snapshot's session-state read (#137): the
// deterministic working / needs_you / done classification for one anchored
// window, exposed on focus.list for the overlay dot (#127). The same gates
// as the recap hold — the window-source consent switches it off wholesale,
// and in auto mode only a terminal is looked at — and every failure is the
// honest empty string: unknown, which the wire omits and no dot renders.
// Content never travels here; the transcript is read for its structure and
// dropped.
func (d *Daemon) classifySession(ctx context.Context, _ focus.Anchor, w desktop.Window, trigger string) (string, error) {
	cfg := d.runningConfig()
	if !cfg.Context.Window {
		return "", nil
	}
	if trigger != focus.RecapAlways && !terminalClass(cfg, w) {
		return "", nil
	}
	ctx, cancel := context.WithTimeout(ctx, focusClassifyTimeout)
	defer cancel()
	tail, err := d.sessionTail(ctx, w.PID)
	if err != nil {
		return "", nil
	}
	return string(tail.State), nil
}

// redactLines runs the secret redactor over each line of a transcript
// render. Line-wise on purpose: Redact replaces its input wholesale, and a
// transcript is many exchanges — one line that looks like a credential must
// cost that line, not the whole capture.
func redactLines(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if redacted, ok := desktop.Redact(line); ok {
			lines[i] = redacted
		}
	}
	return strings.Join(lines, "\n")
}

// terminalClass reports whether a window's contents are a command line — the
// recap's auto-trigger — against the same list typing escalates on
// (tools.typing.terminal_classes, shipped default when unset), so "is this a
// terminal?" has one answer across the daemon.
func terminalClass(cfg config.Config, w desktop.Window) bool {
	classes := cfg.Tools.Typing.TerminalClasses
	if len(classes) == 0 {
		classes = tools.DefaultTerminalClasses
	}
	class := strings.ToLower(strings.TrimSpace(w.Class))
	app := strings.ToLower(desktop.AppName(w.Class))
	for _, entry := range classes {
		e := strings.ToLower(strings.TrimSpace(entry))
		if e != "" && (e == class || e == app) {
			return true
		}
	}
	return false
}

// clampCaptureRunes bounds captured text at n runes — runes, not bytes, so
// the clamp can never tear a multi-byte character (the context truncation
// rule, restated for the one capture that does not ride the collector).
func clampCaptureRunes(text string, n int) string {
	if utf8.RuneCountInString(text) <= n {
		return text
	}
	kept := 0
	for i := range text {
		if kept == n {
			return text[:i]
		}
		kept++
	}
	return text
}

// clampTailCaptureRunes bounds captured text at its LAST n runes — the
// transcript variant (#137): a transcript's newest exchange is at its end,
// and a head-biased clamp would keep the stale half.
func clampTailCaptureRunes(text string, n int) string {
	total := utf8.RuneCountInString(text)
	if total <= n {
		return text
	}
	drop := total - n
	for i := range text {
		if drop == 0 {
			return text[i:]
		}
		drop--
	}
	return text
}

// recapSummarise asks the current provider for the pinned-style session
// summary: one user message, no tools, no history — the prompt (composed in
// internal/focus) is the whole exchange, so nothing here can leak a
// conversation into a recap or a recap into a conversation. The provider and
// model are read at call time, so a reload's swap lands on the next recap.
func (d *Daemon) recapSummarise(ctx context.Context, prompt string) (string, error) {
	d.cfgMu.Lock()
	provider, model := d.provider, d.cfg.AI.Model
	d.cfgMu.Unlock()
	if provider == nil {
		return "", fmt.Errorf("no assistant provider is configured")
	}
	events, err := provider.Chat(ctx, ai.ChatRequest{
		Model:       model,
		Messages:    []ai.Message{{Role: ai.RoleUser, Content: prompt}},
		MaxTokens:   recapMaxTokens,
		Temperature: recapTemperature,
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
			// request that advertised no tools, and would be ignored if one
			// did.
		}
	}
	return b.String(), nil
}

// fireFocus speaks one scheduled focus moment through the ordinary session
// path. It blocks until the spoken turn has finished — it runs on a
// goroutine the focus service tracks, so shutdown drains it like everything
// else — and a refusal is a skipped announcement with a report, never a
// backlog: the state the firing announces was already recorded by the
// service before it dispatched.
func (d *Daemon) fireFocus(ctx context.Context, f focus.Firing) {
	phrase, ok := focusPhrase(f)
	if !ok {
		// Only reachable for a hand-edited thread name too long for the
		// grammar: the router could not claim the phrase, and an unattended
		// firing must never fall through to the model.
		d.log.Warn("focus check-in skipped; the thread's name is too long for the phrase table — shorten it",
			"component", "focus", "thread", f.Thread.ID)
		return
	}
	// Subscribe before starting: the session's finish must not be able to
	// outrun the subscription.
	events, unsubscribe := d.bus.Subscribe()
	defer unsubscribe()
	id, err := d.engine.StartScheduledSession(true)
	if err != nil {
		// A conversation is active or speech is playing: the clock yields —
		// this is the do-not-nag rule doing its job — and the yield is
		// reported, never silent.
		d.log.Info("focus firing skipped", "component", "focus",
			"kind", string(f.Kind), "thread", f.Thread.ID, "reason", err.Error())
		d.bus.Publish(session.Event{Type: "focus.skipped", Data: map[string]any{
			"kind": string(f.Kind), "thread": f.Thread.ID, "reason": err.Error(),
		}})
		return
	}
	if err := d.engine.Submit(phrase); err != nil {
		d.log.Warn("focus firing could not submit its phrase",
			"component", "focus", "kind", string(f.Kind), "thread", f.Thread.ID,
			"error", err.Error())
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case ev, open := <-events:
			if !open {
				return
			}
			if ev.Type != "session.finished" && ev.Type != "session.cancelled" {
				continue
			}
			if sid, _ := ev.Data["session_id"].(string); sid == id {
				return
			}
		}
	}
}

// focusPhrase renders one firing as the utterance the session replays — a
// sentence the user could equally have spoken, so the record reads the same
// whether the clock or the voice asked. false means the thread's name cannot
// ride the grammar (hand-edited past the name-word bound) and the firing
// must be skipped rather than reach the model.
func focusPhrase(f focus.Firing) (string, bool) {
	switch f.Kind {
	case focus.FiringReminder:
		name := strings.ToLower(strings.TrimSpace(f.Thread.Name))
		if n := len(strings.Fields(name)); n == 0 || n > intent.MaxFocusNameWords {
			return "", false
		}
		return "where am i on " + name, true
	default:
		// Midpoint and close both land on the tick phrase: the service's
		// session state decides which sentence it earns, so the clock and a
		// spoken "focus session update" can never disagree.
		return "focus session update", true
	}
}

// focusDrain and focusInFlight adapt the service to the shutdown stage table.
func (d *Daemon) focusDrain(ctx context.Context) error {
	return d.focus.Drain(ctx)
}

func (d *Daemon) focusInFlight() int {
	return d.focus.InFlight()
}

// registerFocusMethods adds the focus.* verbs: the Focus tab's whole surface,
// and the contract the bar/overlay siblings (#124, #127) integrate against.
// Reads are focus.list; every mutation returns the same spoken-style sentence
// the voice path earns, so a client that wants to show it can, and publishes
// focus.changed for the ones that don't.
func (d *Daemon) registerFocusMethods() {
	d.server.Handle("focus.list", func(json.RawMessage) (any, error) {
		return focusViewReport(d.focus.Snapshot(context.Background())), nil
	})

	d.server.Handle("focus.create", func(params json.RawMessage) (any, error) {
		p := struct {
			Name    string `json:"name"`
			Windows int    `json:"windows"`
		}{}
		if err := unmarshalFocusParams("focus.create", params, &p); err != nil {
			return nil, err
		}
		th, spoken, err := d.focus.Create(context.Background(), p.Name, p.Windows)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
		}
		return map[string]any{"thread": focusThreadID(th), "spoken": spoken}, nil
	})

	// focus.save is the Focus tab's create/edit FORM (#164): the whole draft in
	// one request, applied in one write. focus.create stays as it is — it is the
	// voice path's verb and the CLI's — and this is not a second write path but
	// the same store's own Save, which is the only place a thread's four
	// settings can land together without a moment where two of them have landed
	// and two have not.
	d.server.Handle("focus.save", func(params json.RawMessage) (any, error) {
		p := struct {
			Thread  string `json:"thread"`
			Name    string `json:"name"`
			Anchors *int   `json:"anchors"`
			Remind  int    `json:"remind_every_min"`
			Recap   string `json:"recap"`
		}{}
		if err := unmarshalFocusParams("focus.save", params, &p); err != nil {
			return nil, err
		}
		th, spoken, err := d.focus.Save(context.Background(), p.Thread,
			focus.ThreadForm{
				Name: p.Name, AnchorWindows: p.Anchors,
				RemindEveryMin: p.Remind, Recap: p.Recap,
			})
		if err != nil {
			return nil, &ipc.Error{
				Code:    ipc.CodeConfigInvalid,
				Message: err.Error(),
				Data:    map[string]any{"problems": focusSaveProblems(err)},
			}
		}
		return map[string]any{
			"thread": focusThreadID(th), "spoken": spoken,
			"created": strings.TrimSpace(p.Thread) == "",
		}, nil
	})

	d.server.Handle("focus.switch", func(params json.RawMessage) (any, error) {
		p := struct {
			Thread string `json:"thread"`
		}{}
		if err := unmarshalFocusParams("focus.switch", params, &p); err != nil {
			return nil, err
		}
		th, recap, err := d.focus.Switch(context.Background(), p.Thread)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
		}
		return map[string]any{"thread": focusThreadID(th), "recap": recap}, nil
	})

	d.server.Handle("focus.park", func(params json.RawMessage) (any, error) {
		p := struct {
			Text string `json:"text"`
		}{}
		if err := unmarshalFocusParams("focus.park", params, &p); err != nil {
			return nil, err
		}
		spoken, err := d.focus.Park(p.Text)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
		}
		return map[string]any{"spoken": spoken}, nil
	})

	d.server.Handle("focus.end", func(params json.RawMessage) (any, error) {
		p := struct {
			Thread string `json:"thread"`
		}{}
		if err := unmarshalFocusParams("focus.end", params, &p); err != nil {
			return nil, err
		}
		spoken, err := d.focus.End(p.Thread)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
		}
		return map[string]any{"spoken": spoken}, nil
	})

	d.server.Handle("focus.session.start", func(params json.RawMessage) (any, error) {
		p := struct {
			Thread  string `json:"thread"`
			Minutes int    `json:"minutes"`
		}{}
		if err := unmarshalFocusParams("focus.session.start", params, &p); err != nil {
			return nil, err
		}
		spoken, err := d.focus.StartSession(context.Background(), p.Thread, p.Minutes)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
		}
		return map[string]any{"spoken": spoken}, nil
	})

	d.server.Handle("focus.session.end", func(json.RawMessage) (any, error) {
		spoken, err := d.focus.EndSession()
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
		}
		return map[string]any{"spoken": spoken}, nil
	})

	d.server.Handle("focus.remind", func(params json.RawMessage) (any, error) {
		p := struct {
			Thread  string `json:"thread"`
			Minutes int    `json:"minutes"`
		}{}
		if err := unmarshalFocusParams("focus.remind", params, &p); err != nil {
			return nil, err
		}
		// The verb reaches past the active thread when one is named — the
		// tab edits any row — by switching resolution, not by a second code
		// path: Remind acts on the active thread, so a named thread is made
		// active first only in the resolve sense, never by side effect.
		if strings.TrimSpace(p.Thread) != "" {
			spoken, err := d.focus.RemindThread(p.Thread, p.Minutes)
			if err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
			}
			return map[string]any{"spoken": spoken}, nil
		}
		spoken, err := d.focus.Remind(p.Minutes)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
		}
		return map[string]any{"spoken": spoken}, nil
	})
}

// focusSaveProblems keys one form refusal to the field that caused it, the
// entry pipeline's {field, message} contract reused so the Focus tab pins a
// problem exactly as every other form does. The matching is on the sentence the
// service already wrote, never on a second copy of the rule.
func focusSaveProblems(err error) []entryProblem {
	msg := err.Error()
	switch {
	case errors.Is(err, focus.ErrNoName), strings.Contains(msg, "already exists"):
		return []entryProblem{{Field: "name", Message: msg}}
	case strings.Contains(msg, "check-in interval"):
		return []entryProblem{{Field: "remind_every_min", Message: msg}}
	case strings.Contains(msg, "is not a mode"):
		return []entryProblem{{Field: "recap", Message: msg}}
	}
	return []entryProblem{{Message: msg}}
}

// unmarshalFocusParams reads one verb's params with the standard refusal.
func unmarshalFocusParams(verb string, params json.RawMessage, into any) error {
	if len(params) == 0 {
		return nil
	}
	if err := json.Unmarshal(params, into); err != nil {
		return ipc.Errorf(ipc.CodeInvalidParams, "%s params: %v", verb, err)
	}
	return nil
}

// focusViewReport renders the snapshot for the wire. Times are RFC 3339;
// ages arrive pre-worded (the shared spoken scale), so no client invents its
// own arithmetic (ADR 0013).
func focusViewReport(v focus.View) map[string]any {
	threads := make([]map[string]any, 0, len(v.Threads))
	for _, tv := range v.Threads {
		entry := map[string]any{
			"id":                   tv.ID,
			"name":                 tv.Name,
			"active":               tv.Active,
			"created":              tv.Created.Format(time.RFC3339),
			"last_activity":        tv.LastActivity.Format(time.RFC3339),
			"last_activity_spoken": tv.LastActivitySpoken,
			"parked_count":         len(tv.Parked),
		}
		if !tv.LastSwitched.IsZero() {
			entry["last_switched"] = tv.LastSwitched.Format(time.RFC3339)
		}
		if tv.RemindEveryMin > 0 {
			entry["remind_every_min"] = tv.RemindEveryMin
		}
		if tv.Recap != "" {
			entry["recap"] = tv.Recap
		}
		if tv.SessionState != "" {
			// The deterministic AI-session classification (#137): "working",
			// "needs_you", or "done". Absent when unknown — the overlay dot
			// (#127) renders absence as no dot, so unknown never guesses.
			entry["session_state"] = tv.SessionState
		}
		if len(tv.Anchors) > 0 {
			anchors := make([]map[string]any, 0, len(tv.Anchors))
			for i, a := range tv.Anchors {
				anchors = append(anchors, map[string]any{
					"app": a.App, "title": a.Title, "gone": tv.AnchorsGone[i],
				})
			}
			entry["anchors"] = anchors
		}
		if len(tv.Parked) > 0 {
			parked := make([]map[string]any, 0, len(tv.Parked))
			for _, pk := range tv.Parked {
				parked = append(parked, map[string]any{
					"id": pk.ID, "text": pk.Text, "at": pk.At.Format(time.RFC3339),
				})
			}
			entry["parked"] = parked
		}
		threads = append(threads, entry)
	}
	out := map[string]any{"threads": threads, "active": v.Active}
	if v.Session != nil {
		out["session"] = map[string]any{
			"thread":        v.Session.ThreadID,
			"thread_name":   v.Session.ThreadName,
			"started":       v.Session.Started.Format(time.RFC3339),
			"minutes":       v.Session.Minutes,
			"phase":         v.Session.Phase,
			"remaining_sec": v.Session.RemainingSec,
		}
	}
	return out
}

// focusThreadID reports one thread for a mutation reply.
func focusThreadID(th focus.Thread) map[string]any {
	return map[string]any{"id": th.ID, "name": th.Name}
}
