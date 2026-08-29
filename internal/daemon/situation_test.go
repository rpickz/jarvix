package daemon

import (
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/provenance"
	"github.com/rpickz/jarvix/internal/situation"
)

// These tests drive the situation report (#196, ADR 0061) over the real socket
// against a real daemon, because the thing worth proving here is not the
// composition — internal/situation's own tests are hermetic and cover that —
// but the wiring: that six adapters over six live stores produce a report, that
// its ordering survives the wire, and that a line's link resolves through the
// provenance verbs the conversation window already uses.

// situationView is the parsed situation.get reply.
type situationView struct {
	Headline  string `json:"headline"`
	Caveat    string `json:"caveat"`
	Spoken    string `json:"spoken"`
	Quiet     bool   `json:"quiet"`
	Cached    bool   `json:"cached"`
	Truncated bool   `json:"truncated"`
	AgeSpoken string `json:"age_spoken"`
	Sections  []struct {
		Title string `json:"title"`
		Lines []struct {
			Text string `json:"text"`
			Link *int   `json:"link"`
		} `json:"lines"`
	} `json:"sections"`
	Sources []provenance.Reference `json:"sources"`
}

func startSituationDaemon(t *testing.T) *ipc.Client {
	t.Helper()
	client, _, _ := startMemoryDaemon(t, func(cfg *config.Config) {
		// A feed that will never be fetched — its presence is enough for the
		// failing-source adapter to have something to read.
		cfg.Knowledge.Feeds = []config.KnowledgeFeed{{
			Name:        "prices",
			Description: "the price of things",
			Command:     config.Command{"/bin/echo", "7"},
			Mode:        "lazy",
			TTLSec:      600,
			TimeoutSec:  5,
		}}
	})
	return client
}

func getSituation(t *testing.T, client *ipc.Client, fresh bool) situationView {
	t.Helper()
	var view situationView
	if err := client.Call("situation.get", map[string]any{"fresh": fresh}, &view); err != nil {
		t.Fatal(err)
	}
	return view
}

// TestTheSituationReportAnswersFromEveryStoreItHas. Six adapters over six live
// stores, read in parallel, composed into one bounded spoken answer — on a
// daemon where none of them has anything alarming to say, which is the ordinary
// case and the one a manufactured list would be most tempting in.
func TestTheSituationReportAnswersFromEveryStoreItHas(t *testing.T) {
	client := startSituationDaemon(t)

	view := getSituation(t, client, false)
	if view.Spoken == "" {
		t.Fatal("the report said nothing at all")
	}
	if view.Headline == "" {
		t.Error("the report has no headline")
	}
	// Nothing on this daemon needs anybody, so the honest answer is the short
	// one rather than a list assembled to look like news.
	if !view.Quiet {
		t.Errorf("a fresh daemon was not reported quiet: %q", view.Spoken)
	}
	if !strings.HasPrefix(view.Spoken, situation.QuietSentence) {
		t.Errorf("spoken = %q, want it to open with %q", view.Spoken, situation.QuietSentence)
	}
	// Every source was readable, so nothing is named as unavailable.
	for _, section := range view.Sections {
		if section.Title == "I couldn't check" {
			t.Errorf("a source could not be read: %v", section.Lines)
		}
	}
	if view.AgeSpoken == "" {
		t.Error("the report does not say how old it is")
	}
}

// TestTheReportSectionsArriveInThePinnedOrder. The ordering is decided
// daemon-side and it is the feature; this pins that it survives composition,
// the six adapters, and the wire. Sections that earned nothing are absent
// rather than empty, so the assertion is that what IS there is a subsequence of
// the pinned order — never a reordering of it.
func TestTheReportSectionsArriveInThePinnedOrder(t *testing.T) {
	client := startSituationDaemon(t)
	createThread(t, client, "the deploy")

	view := getSituation(t, client, true)
	var want []string
	for _, rank := range situation.Ordered() {
		want = append(want, rank.Title())
	}
	at := 0
	for _, section := range view.Sections {
		found := false
		for ; at < len(want); at++ {
			if want[at] == section.Title {
				found = true
				at++
				break
			}
		}
		if !found {
			t.Fatalf("section %q is out of the pinned order %v (sections: %v)",
				section.Title, want, sectionTitles(view))
		}
	}
	if len(view.Sections) == 0 {
		t.Fatal("no sections at all")
	}
}

func sectionTitles(view situationView) []string {
	var out []string
	for _, s := range view.Sections {
		out = append(out, s.Title)
	}
	return out
}

// TestEverySituationLineLinksThroughTheProvenanceVerbs is the acceptance
// criterion for the window rendering: each line links to the thing it
// describes, through the navigation #168 already built rather than a second
// mechanism.
//
// It is proved end to end, exactly as the tab does it: take the report's
// `sources` array verbatim, hand it to provenance.resolve, and read each line's
// item back at its own `link`. The thread the test creates has to come back
// named, live, and with the action that opens the Focus tab on it.
func TestEverySituationLineLinksThroughTheProvenanceVerbs(t *testing.T) {
	client := startSituationDaemon(t)
	createThread(t, client, "the deploy")

	view := getSituation(t, client, true)
	if len(view.Sources) == 0 {
		t.Fatal("no line pointed at anything")
	}
	items := resolveSources(t, client, view.Sources)
	if len(items) != len(view.Sources) {
		t.Fatalf("resolved %d items for %d sources", len(items), len(view.Sources))
	}

	// Every link is in range and every resolved item is worded — a line that
	// pointed past the end, or at a hole, would be the failure.
	linked := 0
	named := ""
	for _, section := range view.Sections {
		for _, line := range section.Lines {
			if line.Link == nil {
				continue
			}
			if *line.Link < 0 || *line.Link >= len(items) {
				t.Fatalf("line %q links to %d, out of %d sources",
					line.Text, *line.Link, len(items))
			}
			item := items[*line.Link]
			if str(item["name"]) == "" {
				t.Errorf("line %q resolves to an unworded source: %v", line.Text, item)
			}
			linked++
			if strings.Contains(str(item["name"]), "the deploy") {
				named = line.Text
				actions, _ := item["actions"].([]any)
				if len(actions) == 0 {
					t.Fatalf("the thread link offers nothing to do: %v", item)
				}
				action, _ := actions[0].(map[string]any)
				if action["tab"] != "focus" {
					t.Errorf("the thread link does not open the Focus tab: %v", action)
				}
			}
		}
	}
	if linked == 0 {
		t.Fatal("not one line carried a link")
	}
	if named == "" {
		t.Errorf("no line pointed at the thread that was created: %v", sectionTitles(view))
	}
}

// TestAskingTwiceIsCheapAndRefreshIsNot. The caching rule as the wire sees it:
// a second read inside the window is marked as a replay and carries the first
// one's moment, and the Refresh button composes again.
func TestAskingTwiceIsCheapAndRefreshIsNot(t *testing.T) {
	client := startSituationDaemon(t)

	first := getSituation(t, client, false)
	second := getSituation(t, client, false)
	if !second.Cached {
		t.Error("a second read inside the cache window was not marked as a replay")
	}
	if second.Spoken != first.Spoken {
		t.Errorf("a replay said something different:\n%q\n%q", first.Spoken, second.Spoken)
	}

	forced := getSituation(t, client, true)
	if forced.Cached {
		t.Error("Refresh returned the cached report")
	}
}

// TestTheReportNeverCarriesAWindowAddress. Compositor addresses are opaque
// handles that deliberately never travel on the wire (ADR 0022, and the overlay
// feed's own rule). The desktop source describes the shape of the desktop and
// links to nothing, and this is what would notice if somebody later gave it a
// link by reaching for the one identifier that would work.
func TestTheReportNeverCarriesAWindowAddress(t *testing.T) {
	client := startSituationDaemon(t)

	view := getSituation(t, client, true)
	for _, ref := range view.Sources {
		if ref.Kind == "window" || strings.HasPrefix(ref.Ref, "0x") {
			t.Errorf("a window handle reached the wire: %+v", ref)
		}
	}
	if !strings.Contains(view.Spoken, "window") && len(view.Sections) > 0 {
		// The fake compositor starts empty, so this is only a nudge that the
		// desktop source is wired at all rather than an assertion about what
		// it found.
		t.Log("the desktop source produced no line; the fake compositor is empty")
	}
}

// TestTheReportRowSaysNothingAboutWhatItSaid. The activity feed records that a
// report was given and stops there — counts and outcomes, never a word of the
// account. The salt is the thread name, which the report certainly does say.
func TestTheReportRowSaysNothingAboutWhatItSaid(t *testing.T) {
	client := startSituationDaemon(t)
	createThread(t, client, "zarquon")

	view := getSituation(t, client, true)
	if !strings.Contains(view.Spoken, "zarquon") {
		t.Fatalf("the salt is not in the report, so this proves nothing: %q", view.Spoken)
	}
	for _, row := range activityRows(t, client) {
		if row.Label != "Situation report given" {
			continue
		}
		if strings.Contains(row.Label+row.Detail, "zarquon") {
			t.Errorf("the activity row leaked the report: %+v", row)
		}
		return
	}
	t.Error("no situation-report row reached the activity feed")
}

// createThread makes one focus thread through the daemon's own verb, so the
// report's focus source has something real to find.
func createThread(t *testing.T, client *ipc.Client, name string) {
	t.Helper()
	if err := client.Call("focus.save", map[string]any{"name": name}, nil); err != nil {
		t.Fatal(err)
	}
}

// activityRows reads the feed as the window does.
func activityRows(t *testing.T, client *ipc.Client) []desktop.ActivityRow {
	t.Helper()
	var reply struct {
		Rows []struct {
			Kind   string `json:"kind"`
			Label  string `json:"label"`
			Detail string `json:"detail"`
		} `json:"rows"`
	}
	if err := client.Call("activity.get", nil, &reply); err != nil {
		t.Fatal(err)
	}
	out := make([]desktop.ActivityRow, 0, len(reply.Rows))
	for _, r := range reply.Rows {
		out = append(out, desktop.ActivityRow{Kind: r.Kind, Label: r.Label, Detail: r.Detail})
	}
	return out
}
