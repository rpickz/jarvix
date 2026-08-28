package testdiscipline_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/testdiscipline"
)

// A guard is only worth its maintenance if both halves are pinned: that it
// fires on the shape it was written for, and that it stays silent on every
// legitimate use of the same calls. The second half is the one that decides
// whether the rule survives contact with a real branch — a guard that cries
// wolf gets deleted, and takes its true positives with it. So each fixture
// pair below is a directory the go tool ignores, holding the historical defect
// on one side and the tree's real, correct uses on the other.

func TestDerivedStateScanFiresOnTheHistoricalShapes(t *testing.T) {
	findings := scan(t, testdiscipline.ScanDerivedState, "derived_bad")

	// Every function in the fixture is a violation, named for what it is.
	want := map[string]string{
		"TestFeedRowSampledAfterOnlyItsEvent":  `Call("activity.get")`,
		"TestArchivedIDReadAfterOnlyTheAppend": "ActiveConversationID",
		"TestOptOutWithoutAReason":             "carries no reason",
	}
	got := map[string]string{}
	for _, f := range findings {
		got[f.Func] = f.Message
	}
	for fn, fragment := range want {
		msg, ok := got[fn]
		if !ok {
			t.Errorf("%s was not reported; the rule has stopped watching for it", fn)
			continue
		}
		if !strings.Contains(msg, fragment) {
			t.Errorf("%s reported as %q, want it to mention %q", fn, msg, fragment)
		}
	}
	if len(findings) != len(want) {
		t.Errorf("%d findings, want %d: %v", len(findings), len(want), findings)
	}

	// The message has to carry the evidence, because the author reading it in
	// CI has none of this context and every minute spent working out what the
	// rule means is a minute spent arguing for deleting it.
	for _, f := range findings {
		if f.Func == "TestOptOutWithoutAReason" {
			continue // the marker report is about the marker, not the shape
		}
		for _, cite := range []string{"#167", "#170"} {
			if !strings.Contains(f.Message, cite) {
				t.Errorf("%s does not cite %s: %s", f.Func, cite, f.Message)
			}
		}
	}
}

func TestDerivedStateScanIsQuietOnLegitimateUses(t *testing.T) {
	for _, f := range scan(t, testdiscipline.ScanDerivedState, "derived_good") {
		t.Errorf("false positive: %s", f)
	}
}

func TestFakeFieldScanFiresOnTheHistoricalShape(t *testing.T) {
	findings := scan(t, testdiscipline.ScanFakeFields, "fakes_bad")

	want := map[string]bool{
		"fakesbad.Fake.LastRequest": false,
		"fakesbad.Fake.Speaks":      false,
		"fakesbad.StubStore.Saved":  false,
	}
	for _, f := range findings {
		if _, expected := want[f.Key]; !expected {
			t.Errorf("false positive: %s", f)
			continue
		}
		want[f.Key] = true
		if !strings.Contains(f.Message, "#149") {
			t.Errorf("%s does not cite #149: %s", f.Key, f.Message)
		}
	}
	for key, seen := range want {
		if !seen {
			t.Errorf("%s was not reported; the rule has stopped watching for it", key)
		}
	}
}

func TestFakeFieldScanIsQuietOnScriptingFields(t *testing.T) {
	for _, f := range scan(t, testdiscipline.ScanFakeFields, "fakes_good") {
		t.Errorf("false positive: %s", f)
	}
}

// scan runs one of the scanners over a fixture directory.
func scan(t *testing.T, scanner func([]string) ([]testdiscipline.Finding, error), fixture string) []testdiscipline.Finding {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("testdata", fixture, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("fixture %s is empty; this test is no longer proving anything", fixture)
	}
	findings, err := scanner(files)
	if err != nil {
		t.Fatal(err)
	}
	return findings
}
