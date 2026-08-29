package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/provenance"
)

// "What went into this answer" through the daemon's own surfaces (issue
// #168): a turn records what it was given, the record survives the window
// reopening and the conversation being reopened, each source resolves to a
// name and a way to get there, a source that has since gone says so and
// offers nothing, and the spoken question reads the same list.

// startProvenanceDaemon is a daemon with a remembered fact and a knowledge
// feed, so an ordinary turn has something to record. The feed's command
// carries a URL, which is the definition-carries-a-page case.
func startProvenanceDaemon(t *testing.T) (*ipc.Client, string) {
	t.Helper()
	client, _, dir := startMemoryDaemon(t, func(cfg *config.Config) {
		// The command carries a URL, which is the "definition carries a
		// page" case; lazy, so nothing fetches until a test asks.
		// The bounds are spelled out because this config is built in code
		// and never went through config.Load's normalisation, where a zero
		// timeout would have become the default rather than "no time at all".
		cfg.Knowledge.Feeds = []config.KnowledgeFeed{{
			Name:        "prices",
			Description: "the price of things",
			Command:     config.Command{"/bin/echo", "https://example.com/prices"},
			Mode:        "lazy",
			TTLSec:      600,
			TimeoutSec:  5,
			Inject:      true,
		}}
	})
	return client, dir
}

// askAndRead drives one turn and returns the conversation as the window sees
// it. conversation.get reads the engine directly, and an exchange is
// committed before session.finished publishes, so the record a client has
// seen acknowledged is always there to read.
func askAndRead(t *testing.T, client *ipc.Client, text string) []map[string]any {
	t.Helper()
	if err := client.Call("session.text", map[string]string{"text": text}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "session.finished")
	return conversationTurns(t, client)
}

// answerSources pulls the provenance references off the newest answer.
func answerSources(t *testing.T, turns []map[string]any) []provenance.Reference {
	t.Helper()
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i]["role"] != "assistant" {
			continue
		}
		prov, ok := turns[i]["provenance"].(map[string]any)
		if !ok {
			t.Fatalf("the answer carries no provenance: %v", turns[i])
		}
		raw, _ := prov["sources"].([]any)
		refs := make([]provenance.Reference, 0, len(raw))
		for _, item := range raw {
			m, _ := item.(map[string]any)
			refs = append(refs, provenance.Reference{
				Kind:     str(m["kind"]),
				Strength: str(m["strength"]),
				Ref:      str(m["ref"]),
				Tool:     str(m["tool"]),
				Subject:  str(m["subject"]),
			})
		}
		return refs
	}
	t.Fatal("no assistant turn in the conversation")
	return nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// resolveSources is the panel's round trip: hand the turn's references back
// and read what a person would see.
func resolveSources(t *testing.T, client *ipc.Client, refs []provenance.Reference) []map[string]any {
	t.Helper()
	var reply struct {
		Items []map[string]any `json:"items"`
	}
	if err := client.Call("provenance.resolve",
		map[string]any{"sources": refs}, &reply); err != nil {
		t.Fatal(err)
	}
	return reply.Items
}

// itemFor finds the resolved item for one kind/ref pair.
func itemFor(t *testing.T, items []map[string]any, kind, ref string) map[string]any {
	t.Helper()
	for _, item := range items {
		if item["kind"] == kind && item["ref"] == ref {
			return item
		}
	}
	t.Fatalf("no %s/%s among %v", kind, ref, items)
	return nil
}

// TestTurnCarriesWhatWentIntoItOverTheSocket is the acceptance path: a fact
// and a feed value were put in front of the model, and the turn says so — by
// naming each specific item, and with the weaker of the two claims, because
// injection is not use.
func TestTurnCarriesWhatWentIntoItOverTheSocket(t *testing.T) {
	client, _ := startProvenanceDaemon(t)
	var added struct {
		Fact struct {
			ID string `json:"id"`
		} `json:"fact"`
	}
	if err := client.Call("memory.add",
		map[string]any{"content": "the staging server is called atlas"}, &added); err != nil {
		t.Fatal(err)
	}

	refs := answerSources(t, askAndRead(t, client, "where do I deploy?"))
	var sawFact bool
	for _, ref := range refs {
		if ref.Kind == provenance.KindFact && ref.Ref == added.Fact.ID {
			sawFact = true
			if ref.Strength != provenance.Available {
				t.Errorf("an injected fact claimed %q", ref.Strength)
			}
		}
	}
	if !sawFact {
		t.Fatalf("the injected fact is not among %+v", refs)
	}
	// References, never content: the archive must not have become a second
	// copy of the memory book.
	for _, ref := range refs {
		if strings.Contains(ref.Ref+ref.Subject, "atlas") {
			t.Errorf("a fact's content reached the record: %+v", ref)
		}
	}

	// Resolved, the fact is named and takes you to it.
	item := itemFor(t, resolveSources(t, client, refs), provenance.KindFact, added.Fact.ID)
	if !strings.Contains(str(item["name"]), "atlas") {
		t.Errorf("the fact was not named: %v", item["name"])
	}
	if item["strength_phrase"] != provenance.AvailablePhrase {
		t.Errorf("strength phrase = %v", item["strength_phrase"])
	}
	if item["gone"] != nil {
		t.Errorf("a live fact was reported gone: %v", item)
	}
	actions, _ := item["actions"].([]any)
	if len(actions) != 1 {
		t.Fatalf("actions = %v", item["actions"])
	}
	action, _ := actions[0].(map[string]any)
	if action["tab"] != "memory" || action["ref"] != added.Fact.ID {
		t.Errorf("the fact's action does not open the Memory tab at it: %v", action)
	}
}

// TestAForgottenFactSaysSoAndOffersNothing is the missing-source criterion:
// never a dead button, never a silent no-op. It also proves the record holds
// references rather than copies — a forgotten fact cannot be named, because
// there is nothing left to name it from.
func TestAForgottenFactSaysSoAndOffersNothing(t *testing.T) {
	client, _ := startProvenanceDaemon(t)
	var added struct {
		Fact struct {
			ID string `json:"id"`
		} `json:"fact"`
	}
	if err := client.Call("memory.add",
		map[string]any{"content": "the staging server is called atlas"}, &added); err != nil {
		t.Fatal(err)
	}
	refs := answerSources(t, askAndRead(t, client, "where do I deploy?"))

	if err := client.Call("memory.forget", map[string]any{"id": added.Fact.ID}, nil); err != nil {
		t.Fatal(err)
	}

	item := itemFor(t, resolveSources(t, client, refs), provenance.KindFact, added.Fact.ID)
	if item["gone"] != true {
		t.Errorf("a forgotten fact was not reported gone: %v", item)
	}
	if note := str(item["note"]); !strings.Contains(note, "forgotten") {
		t.Errorf("the item does not say what happened: %q", note)
	}
	if item["actions"] != nil {
		t.Errorf("a forgotten fact still offers actions: %v", item["actions"])
	}
	if strings.Contains(str(item["name"]), "atlas") {
		t.Errorf("a forgotten fact was quoted from a stale copy: %v", item["name"])
	}
}

// TestFeedSourceNavigatesToTheKnowledgeTab, and its page is offered only
// through the gate: with no standing approval for xdg-open there is no
// button, and the reason is words rather than a button that argues back.
func TestFeedSourceNavigatesToTheKnowledgeTab(t *testing.T) {
	client, _ := startProvenanceDaemon(t)
	// A lazy feed has no value until it is fetched, and only a feed with a
	// value is injected — so the fetch is driven explicitly and waited for,
	// rather than the test hoping a scheduler got there first.
	if err := client.Call("knowledge.refresh_now", map[string]string{"name": "prices"}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "knowledge.updated")

	refs := answerSources(t, askAndRead(t, client, "what is the price?"))

	item := itemFor(t, resolveSources(t, client, refs), provenance.KindFeed, "prices")
	if !strings.Contains(str(item["name"]), "prices") {
		t.Errorf("the feed was not named: %v", item["name"])
	}
	actions, _ := item["actions"].([]any)
	if len(actions) == 0 {
		t.Fatalf("the feed offers nothing: %v", item)
	}
	first, _ := actions[0].(map[string]any)
	if first["tab"] != "knowledge" || first["ref"] != "prices" {
		t.Errorf("the feed's action does not open the Knowledge tab at it: %v", first)
	}
	// The page is behind the gate, and the gate has not been asked to let
	// anything through, so the action is absent and the note explains.
	for _, raw := range actions {
		a, _ := raw.(map[string]any)
		if a["id"] == "url" {
			t.Errorf("a page opened without leave from the gate: %v", a)
		}
	}
	if note := str(item["note"]); !strings.Contains(note, "standing approval") {
		t.Errorf("the item does not say why the page is not offered: %q", note)
	}
	// And asking the daemon to open it anyway is refused, rather than
	// quietly going around the gate.
	err := client.Call("provenance.open",
		map[string]any{"kind": provenance.KindFeed, "ref": "prices", "action": "url"}, nil)
	if err == nil {
		t.Fatal("provenance.open bypassed the gate")
	}
	if !strings.Contains(err.Error(), "standing approval") {
		t.Errorf("refusal did not name the gate: %v", err)
	}
}

// TestADeletedFeedSaysSo mirrors the forgotten fact for the other injected
// kind, through the reload that removes the feed from the configuration.
func TestADeletedFeedSaysSo(t *testing.T) {
	client, _ := startProvenanceDaemon(t)
	refs := []provenance.Reference{{
		Kind: provenance.KindFeed, Strength: provenance.Available, Ref: "vanished",
	}}
	item := itemFor(t, resolveSources(t, client, refs), provenance.KindFeed, "vanished")
	if item["gone"] != true {
		t.Errorf("a feed that does not exist was not reported gone: %v", item)
	}
	if note := str(item["note"]); !strings.Contains(note, "deleted") {
		t.Errorf("the item does not say what happened: %q", note)
	}
	if item["actions"] != nil {
		t.Errorf("a deleted feed still offers actions: %v", item["actions"])
	}
}

// TestArtifactSourceOpensWithTheConfiguredOpener, and a file that has since
// been removed says so instead of launching nothing.
func TestArtifactSourceOpensWithTheConfiguredOpener(t *testing.T) {
	client, _ := startProvenanceDaemon(t)
	path := filepath.Join(t.TempDir(), "2026-08-28-chart.png")
	if err := os.WriteFile(path, []byte("not really a png"), 0o600); err != nil {
		t.Fatal(err)
	}
	refs := []provenance.Reference{{
		Kind: provenance.KindArtifact, Strength: provenance.Returned,
		Ref: path, Tool: "artifact.create", Subject: "diagram",
	}}

	item := itemFor(t, resolveSources(t, client, refs), provenance.KindArtifact, path)
	if !strings.Contains(str(item["name"]), "2026-08-28-chart.png") {
		t.Errorf("the artifact was not named by its file: %v", item["name"])
	}
	if item["strength_phrase"] != provenance.ReturnedPhrase {
		t.Errorf("a tool's output claimed %v", item["strength_phrase"])
	}
	actions, _ := item["actions"].([]any)
	if len(actions) != 1 {
		t.Fatalf("actions = %v", item["actions"])
	}
	action, _ := actions[0].(map[string]any)
	if action["invoke"] != true || action["id"] != "open" {
		t.Errorf("the artifact's action is not the daemon's to run: %v", action)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	gone := itemFor(t, resolveSources(t, client, refs), provenance.KindArtifact, path)
	if gone["gone"] != true || gone["actions"] != nil {
		t.Errorf("a removed file still offers to open: %v", gone)
	}
	if note := str(gone["note"]); !strings.Contains(note, "no longer on disk") {
		t.Errorf("the item does not say what happened: %q", note)
	}
	if err := client.Call("provenance.open",
		map[string]any{"kind": provenance.KindArtifact, "ref": path, "action": "open"},
		nil); err == nil {
		t.Fatal("opening a removed file reported success")
	}
}

// TestUnknownActionIsRefused: the daemon never performs an action a source
// does not have, whatever a client asks for.
func TestUnknownActionIsRefused(t *testing.T) {
	client, _ := startProvenanceDaemon(t)
	if err := client.Call("provenance.open",
		map[string]any{"kind": provenance.KindFact, "ref": "m1", "action": "open"},
		nil); err == nil {
		t.Fatal("a fact accepted a daemon-side open")
	}
}

// TestProvenanceSurvivesReopeningTheConversation: what went into an answer is
// part of that answer's record, so it comes back with the turns — both when a
// window re-reads the archive and when the thread itself is reopened.
func TestProvenanceSurvivesReopeningTheConversation(t *testing.T) {
	client, _ := startProvenanceDaemon(t)
	var added struct {
		Fact struct {
			ID string `json:"id"`
		} `json:"fact"`
	}
	if err := client.Call("memory.add",
		map[string]any{"content": "the staging server is called atlas"}, &added); err != nil {
		t.Fatal(err)
	}
	before := answerSources(t, askAndRead(t, client, "where do I deploy?"))

	// A new thread, so reopening is a real reopen rather than a no-op.
	if err := client.Call("conversation.new", nil, nil); err != nil {
		t.Fatal(err)
	}
	var list struct {
		Conversations []struct {
			ID string `json:"id"`
		} `json:"conversations"`
	}
	if err := client.Call("conversation.list", nil, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Conversations) == 0 {
		t.Fatal("nothing was archived")
	}
	id := list.Conversations[0].ID

	// The Library's read carries it...
	var read struct {
		Turns []map[string]any `json:"turns"`
	}
	if err := client.Call("conversation.read", map[string]any{"id": id}, &read); err != nil {
		t.Fatal(err)
	}
	if got := answerSources(t, read.Turns); len(got) != len(before) {
		t.Errorf("the archived record lost sources: %+v vs %+v", got, before)
	}

	// ...and so does the thread once it is live again.
	if err := client.Call("conversation.open", map[string]any{"id": id}, nil); err != nil {
		t.Fatal(err)
	}
	if got := answerSources(t, conversationTurns(t, client)); len(got) != len(before) {
		t.Errorf("the reopened thread lost sources: %+v vs %+v", got, before)
	}
}

// TestSpokenProvenanceReadsTheSameList is the voice half: the deterministic
// phrase answers from the record, names the specific items, and keeps the two
// strengths apart in words.
func TestSpokenProvenanceReadsTheSameList(t *testing.T) {
	client, _ := startProvenanceDaemon(t)
	if err := client.Call("memory.add",
		map[string]any{"content": "the staging server is called atlas"}, nil); err != nil {
		t.Fatal(err)
	}
	askAndRead(t, client, "where do I deploy?")

	if err := client.Call("session.text",
		map[string]string{"text": "where did that come from"}, nil); err != nil {
		t.Fatal(err)
	}
	ev := waitForEvent(t, client, "intent.executed")
	if ev["intent"] != "provenance.list" {
		t.Fatalf("intent.executed = %v", ev)
	}
	spoken, _ := ev["acknowledgement"].(string)
	if !strings.Contains(spoken, "atlas") {
		t.Errorf("the spoken list does not name the fact: %q", spoken)
	}
	if !strings.Contains(spoken, provenance.AvailablePhrase) {
		t.Errorf("the spoken list does not say how strong the claim is: %q", spoken)
	}
	waitForEvent(t, client, "session.finished")
}

// TestSpokenProvenanceIsHonestWhenThereIsNothing: a machine that always finds
// something to cite is the failure this feature exists to prevent.
func TestSpokenProvenanceIsHonestWhenThereIsNothing(t *testing.T) {
	client, _ := startDaemon(t)

	if err := client.Call("session.text",
		map[string]string{"text": "where did that come from"}, nil); err != nil {
		t.Fatal(err)
	}
	ev := waitForEvent(t, client, "intent.executed")
	if ev["intent"] != "provenance.list" || ev["status"] != "ok" {
		t.Fatalf("intent.executed = %v", ev)
	}
	spoken, _ := ev["acknowledgement"].(string)
	if !strings.Contains(spoken, "Nothing I can point you at") {
		t.Errorf("the empty answer was not honest: %q", spoken)
	}
	waitForEvent(t, client, "session.finished")
}

// TestAnUnusedTurnCarriesNoProvenanceOnTheWire: absence is information, and
// the key is absent rather than empty — the same presence rule the disk keeps.
func TestAnUnusedTurnCarriesNoProvenanceOnTheWire(t *testing.T) {
	client, _ := startDaemon(t)
	turns := askAndRead(t, client, "say something")
	for _, turn := range turns {
		if _, ok := turn["provenance"]; ok {
			t.Errorf("a turn that used nothing carries the key: %v", turn)
		}
	}
}

// TestEverySourceKindResolvesToWordsAndAnHonestAction walks the kinds a
// single daemon can answer for without staging each one's world: the two that
// are past events and can never be "gone" (a capture, a tool call) resolve to
// words and no action, and the three that point at something the user can
// delete resolve to a plain statement that it is not there any more.
func TestEverySourceKindResolvesToWordsAndAnHonestAction(t *testing.T) {
	client, _ := startProvenanceDaemon(t)

	var taught struct {
		Entry struct {
			ID string `json:"id"`
		} `json:"entry"`
	}
	if err := client.Call("vocabulary.teach",
		map[string]any{"phrase": "the box", "meaning": "the home server"}, &taught); err != nil {
		t.Fatal(err)
	}

	refs := []provenance.Reference{
		{Kind: provenance.KindVocabulary, Strength: provenance.Available, Ref: taught.Entry.ID},
		{Kind: provenance.KindVocabulary, Strength: provenance.Available, Ref: "w404"},
		{Kind: provenance.KindDesktop, Strength: provenance.Available, Ref: "window"},
		{Kind: provenance.KindDesktop, Strength: provenance.Available, Ref: "clipboard"},
		{Kind: provenance.KindThread, Strength: provenance.Returned, Ref: "t404"},
		{Kind: provenance.KindConversation, Strength: provenance.Returned, Ref: "c404"},
		// The two kinds the situation report added (#196, ADR 0061). They are
		// in this sweep for the same reason as everything else in it: a kind
		// that resolves to a hole rather than to words is the failure the
		// whole resolver exists to prevent, whichever feature minted it.
		{Kind: provenance.KindReminder, Strength: provenance.Returned, Ref: "r404"},
		{Kind: provenance.KindSchedule, Strength: provenance.Returned, Ref: "routine:gone"},
		{Kind: provenance.KindTool, Strength: provenance.Returned,
			Tool: "shell.run", Subject: "git status"},
		{Kind: provenance.KindTool, Strength: provenance.Returned, Tool: "memory.search"},
		{Kind: provenance.KindTool, Strength: provenance.Returned,
			Tool: "advisor.ask", Subject: "claude"},
	}
	items := resolveSources(t, client, refs)
	if len(items) != len(refs) {
		t.Fatalf("resolved %d items for %d references", len(items), len(refs))
	}

	// Every item is worded, whatever else is true of it.
	for i, item := range items {
		if str(item["name"]) == "" {
			t.Errorf("item %d has no name: %v", i, item)
		}
		if item["strength_phrase"] != provenance.Phrase(refs[i].Strength) {
			t.Errorf("item %d strength phrase = %v", i, item["strength_phrase"])
		}
	}

	// A taught phrase is named and lands in the Memory tab beside the facts.
	live := itemFor(t, items, provenance.KindVocabulary, taught.Entry.ID)
	if !strings.Contains(str(live["name"]), "the box") {
		t.Errorf("the taught phrase was not named: %v", live["name"])
	}
	actions, _ := live["actions"].([]any)
	if len(actions) != 1 {
		t.Fatalf("actions = %v", live["actions"])
	}
	if action, _ := actions[0].(map[string]any); action["tab"] != "memory" {
		t.Errorf("a taught phrase does not open the Memory tab: %v", actions[0])
	}

	// Everything that can be deleted and has been says so, with no action.
	for _, missing := range []struct{ kind, ref, says string }{
		{provenance.KindVocabulary, "w404", "no longer taught"},
		{provenance.KindThread, "t404", "ended"},
		{provenance.KindConversation, "c404", "deleted"},
		{provenance.KindReminder, "r404", "no longer pending"},
		{provenance.KindSchedule, "routine:gone", "no longer configured"},
	} {
		item := itemFor(t, items, missing.kind, missing.ref)
		if item["gone"] != true || item["actions"] != nil {
			t.Errorf("%s/%s was not reported gone: %v", missing.kind, missing.ref, item)
		}
		if note := str(item["note"]); !strings.Contains(note, missing.says) {
			t.Errorf("%s/%s note = %q", missing.kind, missing.ref, note)
		}
	}

	// A capture and a tool call are past events: nothing to navigate to, and
	// nothing that can have vanished — so neither is ever "gone".
	for _, past := range []struct{ kind, ref string }{
		{provenance.KindDesktop, "window"},
		{provenance.KindDesktop, "clipboard"},
	} {
		item := itemFor(t, items, past.kind, past.ref)
		if item["gone"] != nil || item["actions"] != nil {
			t.Errorf("%s/%s = %v, want words and nothing else", past.kind, past.ref, item)
		}
	}

	// A tool is named by what it did, and a search by what it searched —
	// never by its query.
	names := map[string]string{}
	for _, item := range items {
		if item["kind"] == provenance.KindTool {
			names[str(item["ref"])+str(item["name"])] = str(item["name"])
		}
	}
	joined := strings.Join(mapValues(names), " | ")
	for _, want := range []string{"git status", "claude", "your remembered facts"} {
		if !strings.Contains(joined, want) {
			t.Errorf("tool sources did not name %q: %s", want, joined)
		}
	}
}

func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}
