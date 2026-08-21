package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeArtifact drops a file in dir with a chosen modification time, so the
// ordering and the age strings are decided by the test rather than by how
// long it took to run.
func writeArtifact(t *testing.T, dir, name string, age time.Duration, now time.Time) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	when := now.Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
	return path
}

// The listing is "recent artifacts": newest first, capped, with the kind and
// the age already worked out. Everything downstream — the CLI print and the
// bar widget's panel — renders this and decides nothing itself.
func TestRecentArtifactsIsNewestFirstAndDescribed(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	writeArtifact(t, dir, "old-diagram.svg", 50*time.Hour, now)
	writeArtifact(t, dir, "notes.md", 90*time.Minute, now)
	writeArtifact(t, dir, "budget.csv", 30*time.Second, now)
	// Hidden files and subdirectories are the artifact pipeline's own
	// workings, not things a user made.
	writeArtifact(t, dir, ".scratch.mmd", time.Second, now)
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}

	listing, err := recentArtifacts(dir, 20, now)
	if err != nil {
		t.Fatal(err)
	}
	if listing.Dir != dir {
		t.Errorf("dir = %q, want %q", listing.Dir, dir)
	}
	var names []string
	for _, a := range listing.Artifacts {
		names = append(names, a.Name)
	}
	want := []string{"budget.csv", "notes.md", "old-diagram.svg"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", names, want)
	}

	newest := listing.Artifacts[0]
	if newest.Kind != "spreadsheet" {
		t.Errorf("kind = %q, want spreadsheet", newest.Kind)
	}
	if newest.Age != "just now" {
		t.Errorf("age = %q, want %q", newest.Age, "just now")
	}
	if newest.Path != filepath.Join(dir, "budget.csv") {
		t.Errorf("path = %q; a caller needs a path it can open", newest.Path)
	}
	if _, err := time.Parse(time.RFC3339, newest.Modified); err != nil {
		t.Errorf("modified = %q, want RFC 3339: %v", newest.Modified, err)
	}
	if listing.Artifacts[1].Age != "1h ago" || listing.Artifacts[2].Age != "2d ago" {
		t.Errorf("ages = %q, %q", listing.Artifacts[1].Age, listing.Artifacts[2].Age)
	}
}

// "Recent" has to mean a glance, not a scroll: the panel and the terminal
// both show a fixed number of the newest files.
func TestRecentArtifactsCapsTheListing(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 6; i++ {
		writeArtifact(t, dir, string(rune('a'+i))+".md", time.Duration(i)*time.Hour, now)
	}
	listing, err := recentArtifacts(dir, 3, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Artifacts) != 3 {
		t.Fatalf("got %d artifacts, want 3", len(listing.Artifacts))
	}
	if listing.Artifacts[0].Name != "a.md" {
		t.Errorf("the cap must keep the newest, got %q", listing.Artifacts[0].Name)
	}
}

// A fresh install has made nothing yet. That is not a failure, and the
// directory still has to be reported — it is how both surfaces say where
// artifacts *will* appear, and how the widget offers to open the folder.
func TestRecentArtifactsOnAMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "never-created")
	listing, err := recentArtifacts(dir, 20, time.Now())
	if err != nil {
		t.Fatalf("a missing artifact directory is the ordinary fresh-install state: %v", err)
	}
	if listing.Dir != dir {
		t.Errorf("dir = %q, want %q", listing.Dir, dir)
	}
	if listing.Artifacts == nil {
		t.Error("artifacts must marshal as [], not null — a client should not have to handle both")
	}
}

// `jarvix artifacts --json` is the bar widget's data source, so its shape is
// a contract: an object with `dir` and `artifacts`, one line, parseable
// without the daemon running.
func TestRunArtifactsJSONIsParseable(t *testing.T) {
	hermeticEnv(t)
	// The default artifact directory is $HOME/Documents/Jarvix, and
	// hermeticEnv points HOME at a temp dir — so this really is empty.
	var code int
	stdout, stderr := capture(t, func() { code = run([]string{"artifacts", "--json"}) })
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	var listing artifactListing
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &listing); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, stdout)
	}
	if listing.Dir == "" {
		t.Error("the listing must name the artifact directory")
	}
	if len(listing.Artifacts) != 0 {
		t.Errorf("a hermetic run should find no artifacts, got %d", len(listing.Artifacts))
	}
}

// An unrecognised flag has to be refused rather than quietly treated as
// --json: a typo that prints machine output where a person wanted a list is
// the sort of thing nobody reports.
func TestRunArtifactsRejectsUnknownFlags(t *testing.T) {
	hermeticEnv(t)
	var code int
	_, stderr := capture(t, func() { code = run([]string{"artifacts", "--jsonn"}) })
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "usage:") {
		t.Errorf("stderr = %q, want usage guidance", stderr)
	}
}
