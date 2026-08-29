package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/focus"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/provenance"
	"github.com/rpickz/jarvix/internal/tools"
)

// This file serves "what went into this answer" (issue #168). The record is
// deliberately thin — kinds and references, written once when the turn was
// assembled — and everything a person reads is composed here, now, from the
// live stores. That split is what makes the honest cases possible:
//
//   - a fact the user has since forgotten cannot be quoted back from a stale
//     copy, because there is no copy: the resolver looks, does not find it,
//     and the item says the fact has been forgotten with no button beside it;
//   - nothing a fact, a feed value, a captured window or a session transcript
//     said was ever written to the archive in the first place.
//
// Clients render and decide nothing (ADR 0013): the wording, the liveness,
// and which actions exist all come from here.

// urlOpenCommand is the command a feed's page is opened with. Fixed rather
// than configurable, and named as one word, because it is what the permission
// gate is asked about below — a command the user can approve or deny by name
// is worth more here than a command they can change.
const urlOpenCommand = "xdg-open"

// factNameRunes caps how much of a fact is used to name it in the list. A
// fact is a sentence, and the list is a list.
const factNameRunes = 90

// registerProvenanceMethods installs the two verbs the provenance panel and
// the spoken listing use.
func (d *Daemon) registerProvenanceMethods() {
	// provenance.resolve turns a turn's stored references into the list a
	// person reads: the readable name of each source right now, whether it
	// still exists, and what can be done with it.
	//
	// The client sends back exactly what the turn gave it, rather than naming
	// a turn, because the same references reach it from three surfaces with
	// three different addressings — the live conversation, an archived record
	// in the Library, and a reopened thread — and a resolver that had to
	// understand all three would be a fourth place for them to disagree.
	d.server.Handle("provenance.resolve", func(params json.RawMessage) (any, error) {
		var p struct {
			Sources []provenance.Reference `json:"sources"`
		}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, ipc.Errorf(ipc.CodeInvalidParams, "provenance.resolve: %v", err)
			}
		}
		return map[string]any{"items": d.resolveProvenance(p.Sources)}, nil
	})

	// provenance.open performs the actions the window cannot: launching a
	// file in its viewer, opening a feed's page, focusing an anchored window.
	// Tab navigation is not here — a tab is the window's own furniture, and
	// the daemon has no business reaching into it.
	d.server.Handle("provenance.open", func(params json.RawMessage) (any, error) {
		var p struct {
			Kind   string `json:"kind"`
			Ref    string `json:"ref"`
			Action string `json:"action"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "provenance.open: %v", err)
		}
		done, err := d.openProvenanceSource(p.Kind, p.Ref, p.Action)
		if err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "%v", err)
		}
		return map[string]any{"done": done}, nil
	})
}

// resolveProvenance is the whole read side: one item per reference, in the
// order the turn collected them.
func (d *Daemon) resolveProvenance(refs []provenance.Reference) []map[string]any {
	items := make([]map[string]any, 0, len(refs))
	for _, ref := range refs {
		items = append(items, d.resolveOne(ref))
	}
	return items
}

// resolveOne composes one item. Every branch fills the same four things —
// name, gone, note, actions — so a kind this daemon does not recognise still
// produces an honest, actionless row rather than a hole in the list.
func (d *Daemon) resolveOne(ref provenance.Reference) map[string]any {
	item := map[string]any{
		"kind":            ref.Kind,
		"ref":             ref.Ref,
		"strength":        ref.Strength,
		"strength_phrase": provenance.Phrase(ref.Strength),
	}
	name, note, gone, actions := d.describe(ref)
	item["name"] = name
	if gone {
		item["gone"] = true
	}
	if note != "" {
		item["note"] = note
	}
	if len(actions) > 0 && !gone {
		item["actions"] = actions
	}
	return item
}

// describe is the per-kind resolution. A source that no longer exists returns
// gone — which strips its actions in resolveOne, so the item can never offer
// a button that would do nothing.
func (d *Daemon) describe(ref provenance.Reference) (name, note string, gone bool, actions []map[string]any) {
	switch ref.Kind {
	case provenance.KindFact:
		return d.describeFact(ref)
	case provenance.KindFeed:
		return d.describeFeed(ref)
	case provenance.KindVocabulary:
		return d.describeVocabulary(ref)
	case provenance.KindDesktop:
		return d.describeDesktop(ref)
	case provenance.KindThread:
		return d.describeThread(ref)
	case provenance.KindConversation:
		return d.describeConversation(ref)
	case provenance.KindArtifact:
		return d.describeArtifact(ref)
	case provenance.KindReminder:
		return d.describeReminder(ref)
	case provenance.KindSchedule:
		return d.describeSchedule(ref)
	case provenance.KindTool:
		return describeTool(ref), "", false, nil
	}
	return "something Jarvix consulted", "", false, nil
}

// describeFact names a remembered fact by its content, read from the book
// now. A forgotten fact keeps its place in the list — it did go into that
// answer — and says what happened to it.
func (d *Daemon) describeFact(ref provenance.Reference) (string, string, bool, []map[string]any) {
	if d.memory != nil {
		for _, f := range d.memory.List("") {
			if f.ID == ref.Ref {
				return "the remembered fact “" + shorten(f.Content, factNameRunes) + "”", "", false,
					[]map[string]any{tabAction("Show in Memory", "memory", f.ID)}
			}
		}
	}
	return "a remembered fact", "this fact has since been forgotten", true, nil
}

// describeFeed names a knowledge feed, and offers its page when the feed's
// definition carries one.
func (d *Daemon) describeFeed(ref provenance.Reference) (string, string, bool, []map[string]any) {
	if d.knowledge != nil {
		for _, f := range d.knowledge.Feeds() {
			if f.Name != ref.Ref {
				continue
			}
			name := "the “" + f.Name + "” feed"
			actions := []map[string]any{tabAction("Show in Knowledge", "knowledge", f.Name)}
			url := feedURL(f.Argv)
			switch {
			case url == "":
			case d.mayOpenURL(url):
				actions = append(actions, map[string]any{
					"id": "url", "label": "Open the feed's page", "invoke": true,
				})
			default:
				// The gate said ask or deny, and this is not a moment at
				// which Jarvix may ask: the permission card exists in
				// response to something the *model* requested (ADR 0053), and
				// manufacturing one from a button press would be a client
				// deciding what runs. So the action is absent and the reason
				// is words — never a button that argues back.
				return name, "its page opens only with a standing approval for " + urlOpenCommand,
					false, actions
			}
			return name, "", false, actions
		}
	}
	return "a knowledge feed", "this feed has been deleted", true, nil
}

// describeVocabulary names a taught phrase. It lives in the Memory tab beside
// the facts, so that is where its action goes.
func (d *Daemon) describeVocabulary(ref provenance.Reference) (string, string, bool, []map[string]any) {
	if d.vocabulary != nil {
		for _, e := range d.vocabulary.List("") {
			if e.ID == ref.Ref {
				return "the taught phrase “" + e.Phrase + "”", "", false,
					[]map[string]any{tabAction("Show in Memory", "memory", e.ID)}
			}
		}
	}
	return "a taught phrase", "this phrase is no longer taught", true, nil
}

// describeDesktop names a capture source. There is nothing to navigate to and
// nothing that can have been deleted: a capture is a past event, and what it
// saw was never kept (ADR 0043/0047).
func (d *Daemon) describeDesktop(ref provenance.Reference) (string, string, bool, []map[string]any) {
	switch desktop.Source(ref.Ref) {
	case desktop.SourceWindow:
		return "what was on screen in the active window", "", false, nil
	case desktop.SourceSelection:
		return "the text you had selected", "", false, nil
	case desktop.SourceClipboard:
		return "your clipboard", "", false, nil
	}
	return "what was on your desktop", "", false, nil
}

// describeThread names a focus thread, and offers to put the user back in the
// window it is anchored to when that window is still open. The anchor's
// liveness is the focus service's own answer (AnchorsGone), so the panel and
// the Focus tab can never disagree about whether a window is still there.
func (d *Daemon) describeThread(ref provenance.Reference) (string, string, bool, []map[string]any) {
	if d.focus != nil {
		view := d.focus.Snapshot(context.Background())
		for _, tv := range view.Threads {
			if tv.ID != ref.Ref {
				continue
			}
			actions := []map[string]any{tabAction("Show in Focus", "focus", tv.ID)}
			if liveAnchor(tv.Anchors, tv.AnchorsGone) != "" {
				actions = append(actions, map[string]any{
					"id": "focus-window", "label": "Focus that window", "invoke": true,
				})
			}
			return "the focus thread “" + tv.Name + "”", "", false, actions
		}
	}
	return "a focus thread", "this thread has ended", true, nil
}

// describeConversation names an archived conversation by its preview — the
// line the Library recognises it by.
func (d *Daemon) describeConversation(ref provenance.Reference) (string, string, bool, []map[string]any) {
	if d.conversations != nil {
		metas, _, err := d.conversations.List()
		if err == nil {
			for _, m := range metas {
				if m.ID != ref.Ref {
					continue
				}
				name := "an earlier conversation"
				if m.Preview != "" {
					name = "the conversation “" + shorten(m.Preview, factNameRunes) + "”"
				}
				return name, "", false,
					[]map[string]any{tabAction("Open in the Library", "library", m.ID)}
			}
		}
	}
	return "an earlier conversation", "this conversation has been deleted", true, nil
}

// describeArtifact names a file a tool produced, and opens it with the same
// viewer that opened it when it was made. A format the user has declared has
// no viewer keeps its place and says so — the file is there, nothing will
// launch it.
func (d *Daemon) describeArtifact(ref provenance.Reference) (string, string, bool, []map[string]any) {
	name := "the artifact " + filepath.Base(ref.Ref)
	if _, err := os.Stat(ref.Ref); err != nil {
		return name, "this file is no longer on disk", true, nil
	}
	if _, hasViewer := d.artifactViewer(ref.Subject); !hasViewer {
		return name, "no viewer is configured for this kind of artifact", false, nil
	}
	return name, "", false, []map[string]any{{
		"id": "open", "label": "Open the file", "invoke": true,
	}}
}

// describeReminder names a pending reminder by the user's own sentence, which
// is the one string the reminder store holds and the one it was written to
// speak back (ADR 0046) — so naming it here breaks no content boundary.
//
// A reminder that has fired or been cancelled is gone rather than absent: it
// left the pending list, and offering a button to a row that is now history
// would be the dead affordance ADR 0055 refuses.
func (d *Daemon) describeReminder(ref provenance.Reference) (string, string, bool, []map[string]any) {
	if d.reminders != nil {
		for _, p := range d.reminders.Snapshot().Pending {
			if p.ID == ref.Ref {
				return "the reminder “" + shorten(p.Text, factNameRunes) + "”", "", false,
					[]map[string]any{tabAction("Show in Automations", "automations", p.ID)}
			}
		}
	}
	return "a reminder", "this reminder is no longer pending", true, nil
}

// describeSchedule names a scheduled routine or script by its own name and the
// schedule it runs on — both of them values the user wrote in config.toml.
//
// The ref carries the kind because the two namespaces are separate: a routine
// and a script may share a name, and resolving a bare one would be a coin toss
// between two different things the user configured.
func (d *Daemon) describeSchedule(ref provenance.Reference) (string, string, bool, []map[string]any) {
	if d.automations != nil {
		for _, st := range d.automations.Status() {
			if scheduleRef(string(st.Kind), st.Name) != ref.Ref {
				continue
			}
			return "the scheduled " + string(st.Kind) + " “" + st.Name + "”", "", false,
				[]map[string]any{tabAction("Show in Automations", "automations", st.Name)}
		}
	}
	return "a schedule", "this schedule is no longer configured", true, nil
}

// scheduleRef is the identity a KindSchedule reference carries, written once so
// the source that produces one and the resolver that reads it cannot disagree
// about the separator.
func scheduleRef(kind, name string) string { return kind + ":" + name }

// describeTool names a tool call by its tool and, where the subject is itself
// a reference, its subject. The two search tools are named by what they
// searched and never by the query: a query can quote the very fact it is
// looking for, which is why the Activity pane refuses to show one either
// (ADR 0037).
func describeTool(ref provenance.Reference) string {
	switch ref.Tool {
	case tools.ShellToolName:
		if ref.Subject != "" {
			return "the command " + ref.Subject
		}
		return "a shell command"
	case tools.AdvisorToolName:
		if ref.Subject != "" {
			return "what the " + ref.Subject + " advisor answered"
		}
		return "what an advisor answered"
	case tools.MemorySearchToolName:
		return "a search of your remembered facts"
	case tools.ConversationsSearchToolName:
		return "a search of your earlier conversations"
	}
	if ref.Tool == "" {
		return "a tool Jarvix ran"
	}
	return "what the " + ref.Tool + " tool returned"
}

// tabAction is the shape of a navigation the window performs itself: which
// tab, and which item to land on.
func tabAction(label, tab, ref string) map[string]any {
	return map[string]any{"id": "open", "label": label, "tab": tab, "ref": ref}
}

// openProvenanceSource performs a daemon-side action. Each branch re-checks
// what it is about to act on rather than trusting the resolve that offered
// it: the file may have been deleted, the window closed, the feed edited
// since the panel was drawn, and acting on a stale offer is exactly the
// silent no-op this ticket forbids.
func (d *Daemon) openProvenanceSource(kind, ref, action string) (bool, error) {
	switch {
	case kind == provenance.KindArtifact && action == "open":
		return d.openArtifactFile(ref)
	case kind == provenance.KindFeed && action == "url":
		return d.openFeedURL(ref)
	case kind == provenance.KindThread && action == "focus-window":
		return d.focusThreadWindow(ref)
	}
	return false, fmt.Errorf("there is no %q action for a %s source", action, kind)
}

// openArtifactFile launches a produced file in its viewer.
func (d *Daemon) openArtifactFile(path string) (bool, error) {
	if _, err := os.Stat(path); err != nil {
		return false, fmt.Errorf("that file is no longer on disk")
	}
	// The format is not carried on the open call — the reference the client
	// holds has it, but a client-supplied format would choose which command
	// runs. The shared viewer is the honest fallback here.
	argv, hasViewer := d.artifactViewer("")
	if !hasViewer {
		return false, fmt.Errorf("no viewer is configured for artifacts")
	}
	if err := tools.Launch(argv, path); err != nil {
		return false, fmt.Errorf("the viewer could not be started: %v", err)
	}
	return true, nil
}

// openFeedURL opens the page a feed reads from — through the permission gate,
// never around it. The gate is asked about the exact command that would run,
// under the shell tool's identity, and only a standing allow proceeds: an ask
// verdict means the user has not given leave, and this is not a moment at
// which Jarvix may ask for it.
func (d *Daemon) openFeedURL(name string) (bool, error) {
	if d.knowledge == nil {
		return false, fmt.Errorf("that feed has been deleted")
	}
	url := ""
	for _, f := range d.knowledge.Feeds() {
		if f.Name == name {
			url = feedURL(f.Argv)
			break
		}
	}
	if url == "" {
		return false, fmt.Errorf("that feed no longer has a page to open")
	}
	if !d.mayOpenURL(url) {
		return false, fmt.Errorf("opening that page needs a standing approval for %s", urlOpenCommand)
	}
	if err := tools.Launch([]string{urlOpenCommand}, url); err != nil {
		return false, fmt.Errorf("the browser could not be started: %v", err)
	}
	return true, nil
}

// focusThreadWindow puts the user back in the window a thread is anchored to.
// The address is re-read from the store and re-checked against the live
// inventory, because an anchor is a handle to something the user may have
// closed since the panel was drawn.
func (d *Daemon) focusThreadWindow(id string) (bool, error) {
	if d.focus == nil || d.compositor == nil {
		return false, fmt.Errorf("that thread has ended")
	}
	ctx := context.Background()
	for _, tv := range d.focus.Snapshot(ctx).Threads {
		if tv.ID != id {
			continue
		}
		address := liveAnchor(tv.Anchors, tv.AnchorsGone)
		if address == "" {
			return false, fmt.Errorf("that window is no longer open")
		}
		if err := d.compositor.Focus(ctx, address); err != nil {
			return false, fmt.Errorf("the window manager refused to switch")
		}
		return true, nil
	}
	return false, fmt.Errorf("that thread has ended")
}

// mayOpenURL asks the permission gate about the command that would run. The
// gate keys on the shell tool's identity, which is the identity a user's
// standing approval for xdg-open was written under (ADR 0053), so one rule
// governs both the model's route to a browser and this one.
func (d *Daemon) mayOpenURL(url string) bool {
	if d.registry == nil {
		return false
	}
	v := d.registry.CheckCommand(tools.ShellToolName, urlOpenCommand+" "+url)
	return v.Decision == tools.PolicyAllow
}

// artifactViewer resolves the viewer argv for a format, from the running
// configuration and through the artifact tool's own resolution — so the panel
// opens a file with exactly the command that opened it when it was made.
func (d *Daemon) artifactViewer(format string) ([]string, bool) {
	d.cfgMu.Lock()
	shared := d.cfg.Artifacts.OpenCommand
	perFormat := artifactOpenCommands(d.cfg.Artifacts.OpenCommands)
	d.cfgMu.Unlock()
	return tools.ViewerFor(shared, perFormat, format)
}

// feedURL finds the page a feed's command reads. A feed is an argv, not a
// URL, so this is a search rather than a field: the first http(s) argument is
// the page, which is what a curl- or wget-shaped feed always looks like, and
// a feed that reads a file or a device simply has no page.
func feedURL(argv []string) string {
	for _, arg := range argv {
		if strings.HasPrefix(arg, "https://") || strings.HasPrefix(arg, "http://") {
			return arg
		}
	}
	return ""
}

// liveAnchor returns the address of the first anchor whose window is still
// open, or "" when none is.
func liveAnchor(anchors []focus.Anchor, gone []bool) string {
	for i, a := range anchors {
		if i < len(gone) && gone[i] {
			continue
		}
		if a.Address != "" {
			return a.Address
		}
	}
	return ""
}

// shorten trims a sentence to n runes, marking the cut. Rune-wise, so a
// multi-byte character is never split in half.
func shorten(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n])) + "…"
}

// spokenProvenanceCap bounds the spoken listing. Shortest-useful first: a
// person asking "where did that come from?" wants the two or three things
// that answer it, and the window's panel is where a long list belongs. Four
// is the point at which a read-aloud list stops being a sentence.
const spokenProvenanceCap = 4

// provenanceVoice is the daemon's ProvenanceLister: it composes the one
// spoken answer to "where did that come from?". Read-only by construction —
// it holds the same resolver the panel uses and nothing that writes, so the
// voice path can report a source and never touch one.
type provenanceVoice struct{ d *Daemon }

// SpokenProvenance implements session.ProvenanceLister.
//
// The two strengths are spoken as two clauses, never merged, because they are
// two different claims: a tool that ran and returned output is why the answer
// says what it says, while an injected fact was only in front of the model.
// Merging them into one "sources" list would be the overstatement this whole
// feature exists to avoid.
func (v *provenanceVoice) SpokenProvenance(rec *provenance.Record) (string, error) {
	if rec == nil || len(rec.Sources) == 0 {
		return "Nothing I can point you at — that answer did not use anything " +
			"I had looked up or been given.", nil
	}
	items := v.d.resolveProvenance(rec.Sources)
	var returned, available []string
	for _, item := range items {
		name, _ := item["name"].(string)
		if name == "" {
			continue
		}
		if gone, _ := item["gone"].(bool); gone {
			if note, ok := item["note"].(string); ok && note != "" {
				name += " — " + note
			}
		}
		if item["strength"] == provenance.Returned {
			returned = append(returned, name)
			continue
		}
		available = append(available, name)
	}
	// Mechanically causal first: what actually ran is the useful half of the
	// answer, and the shortest useful listing leads with it.
	var parts []string
	if spoken, more := spokenSourceList(returned, spokenProvenanceCap); spoken != "" {
		parts = append(parts, "That answer used "+spoken+", "+provenance.ReturnedPhrase+plural(more)+".")
	}
	if spoken, more := spokenSourceList(available, spokenProvenanceCap-len(returned)); spoken != "" {
		parts = append(parts, "Also "+provenance.AvailablePhrase+": "+spoken+plural(more)+".")
	}
	if len(parts) == 0 {
		// Every source was cut by the spoken cap; the window still has them.
		parts = append(parts, "That answer used several sources — they are listed on the turn "+
			"in the conversation window.")
	}
	if rec.Truncated > 0 {
		parts = append(parts, fmt.Sprintf("%d more went unrecorded — the turn used more than I keep.",
			rec.Truncated))
	}
	return strings.Join(parts, " "), nil
}

// spokenSourceList joins up to n names for speech and reports how many were
// left out, so the sentence can say so rather than quietly stop.
func spokenSourceList(names []string, n int) (spoken string, more int) {
	if len(names) == 0 || n <= 0 {
		return "", 0
	}
	shown := names
	if len(shown) > n {
		more = len(shown) - n
		shown = shown[:n]
	}
	switch len(shown) {
	case 1:
		return shown[0], more
	case 2:
		return shown[0] + " and " + shown[1], more
	}
	return strings.Join(shown[:len(shown)-1], ", ") + ", and " + shown[len(shown)-1], more
}

// plural words the "and N more" tail of a spoken clause.
func plural(more int) string {
	switch more {
	case 0:
		return ""
	case 1:
		return ", and one more"
	}
	return fmt.Sprintf(", and %d more", more)
}
