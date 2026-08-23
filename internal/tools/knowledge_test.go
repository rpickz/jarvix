package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/knowledge"
)

// fakeFeeds scripts the tool's view of the service: readings by name, so
// every honesty rule — ages, staleness, failure — is pinned without a
// scheduler or a subprocess anywhere.
type fakeFeeds struct {
	feeds    []knowledge.Feed
	readings map[string]knowledge.Reading
}

func (f *fakeFeeds) Feeds() []knowledge.Feed { return f.feeds }

func (f *fakeFeeds) Get(_ context.Context, name string) (knowledge.Reading, bool) {
	r, ok := f.readings[name]
	return r, ok
}

var testNow = time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

func knowledgeTool(f *fakeFeeds) *KnowledgeGet {
	return &KnowledgeGet{Source: f, Now: func() time.Time { return testNow }}
}

func askKnowledge(t *testing.T, k *KnowledgeGet, feed string) string {
	t.Helper()
	r := NewRegistry(nil)
	r.Register(k)
	args, _ := json.Marshal(map[string]string{"feed": feed})
	return r.Execute(context.Background(), ai.ToolCall{Name: KnowledgeGetToolName, Arguments: string(args)})
}

func amdFeed() knowledge.Feed {
	return knowledge.Feed{Name: "amd", Description: "AMD share price in dollars",
		Mode: knowledge.ModeEager, TTL: 10 * time.Minute, Enabled: true}
}

func TestKnowledgeGetSpeaksTheValueWithItsAge(t *testing.T) {
	f := &fakeFeeds{
		feeds: []knowledge.Feed{amdFeed()},
		readings: map[string]knowledge.Reading{"amd": {
			Feed: amdFeed(), HasValue: true, Value: "187.42",
			FetchedAt: testNow.Add(-4 * time.Minute), Age: 4 * time.Minute,
		}},
	}
	out := askKnowledge(t, knowledgeTool(f), "amd")
	if !strings.Contains(out, "187.42") {
		t.Errorf("result lacks the value: %q", out)
	}
	if !strings.Contains(out, "as of four minutes ago") {
		t.Errorf("result lacks the spoken age for the model to state: %q", out)
	}
	if strings.Contains(out, "STALE") {
		t.Errorf("a fresh value was reported stale: %q", out)
	}
}

func TestKnowledgeGetDisclosesStalenessAndFailure(t *testing.T) {
	f := &fakeFeeds{
		feeds: []knowledge.Feed{amdFeed()},
		readings: map[string]knowledge.Reading{"amd": {
			Feed: amdFeed(), HasValue: true, Value: "187.42",
			FetchedAt: testNow.Add(-3 * time.Hour), Age: 3 * time.Hour, Stale: true,
			Failing: true, FailingSince: testNow.Add(-2 * time.Hour), Attempts: 5,
		}},
	}
	out := askKnowledge(t, knowledgeTool(f), "amd")
	if !strings.Contains(out, "STALE") || !strings.Contains(out, "three hours ago") {
		t.Errorf("staleness not disclosed with the old age: %q", out)
	}
	if !strings.Contains(out, "failing since two hours ago") {
		t.Errorf("the failure and its duration are not disclosed: %q", out)
	}
	if !strings.Contains(out, "187.42") {
		t.Errorf("the last good value must still be served: %q", out)
	}
}

func TestKnowledgeGetFailingWithNoValue(t *testing.T) {
	f := &fakeFeeds{
		feeds: []knowledge.Feed{amdFeed()},
		readings: map[string]knowledge.Reading{"amd": {
			Feed: amdFeed(), Failing: true,
			FailingSince: testNow.Add(-30 * time.Minute), Attempts: 2,
		}},
	}
	out := askKnowledge(t, knowledgeTool(f), "amd")
	if !strings.Contains(out, "failing since thirty minutes ago") {
		t.Errorf("a cold failing feed is not honestly reported: %q", out)
	}
	if !strings.Contains(out, "do not retry") {
		t.Errorf("the model is not told to stop: %q", out)
	}
}

func TestKnowledgeGetUnknownFeedListsWhatItCanWatch(t *testing.T) {
	weather := knowledge.Feed{Name: "weather", Description: "local weather", Enabled: true}
	f := &fakeFeeds{feeds: []knowledge.Feed{amdFeed(), weather}}
	out := askKnowledge(t, knowledgeTool(f), "tesla")
	if !strings.Contains(out, `No feed is named "tesla"`) {
		t.Errorf("the miss is not stated: %q", out)
	}
	if !strings.Contains(out, "amd: AMD share price in dollars") ||
		!strings.Contains(out, "weather: local weather") {
		t.Errorf("the configured list is not returned: %q", out)
	}
}

func TestKnowledgeGetSchemaOffersOnlyConfiguredFeeds(t *testing.T) {
	f := &fakeFeeds{feeds: []knowledge.Feed{amdFeed()}}
	k := knowledgeTool(f)
	var schema struct {
		Properties struct {
			Feed struct {
				Enum []string `json:"enum"`
			} `json:"feed"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(k.Schema(), &schema); err != nil {
		t.Fatalf("schema does not parse: %v", err)
	}
	if len(schema.Properties.Feed.Enum) != 1 || schema.Properties.Feed.Enum[0] != "amd" {
		t.Errorf("enum = %v, want exactly the configured feed", schema.Properties.Feed.Enum)
	}
	if !strings.Contains(k.Description(), "amd: AMD share price in dollars") {
		t.Errorf("description does not steer by topic: %q", k.Description())
	}
}

func TestKnowledgeGetActivityAndConfirmation(t *testing.T) {
	f := &fakeFeeds{feeds: []knowledge.Feed{amdFeed()}}
	k := knowledgeTool(f)
	args := json.RawMessage(`{"feed":"amd"}`)
	if label, _, ok := k.Activity(args); !ok || !strings.Contains(label, "amd") {
		t.Errorf("activity label = %q ok = %v, want the feed named", label, ok)
	}
	command, summary, ok := k.Confirmation(args)
	if !ok || command != "read feed amd" || !strings.Contains(summary, "amd feed") {
		t.Errorf("confirmation = %q / %q, want the feed named daemon-side", command, summary)
	}
}
