package transcript

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The transcript reader (#137, ADR 0047): discovery from a working
// directory, the bounded tail read, the deterministic classification, and
// the rendering rules — all against fixtures shaped like the real CLIs'
// files, with the clock injected and no real CLI state ever touched.

// fixedNow is the fixture-relative clock: just after the fixtures' own
// timestamps, so the freshness gate passes deterministically whatever wall
// clock the test runs under.
var fixedNow = time.Date(2026, 8, 20, 18, 0, 0, 0, time.UTC)

// claudeFinder builds a Finder over a tempdir Claude layout hosting the
// named fixture as cwd's newest session, its mtime pinned an hour before
// fixedNow.
func claudeFinder(t *testing.T, cwd, fixture string) *Finder {
	t.Helper()
	root := t.TempDir()
	writeClaudeTranscript(t, root, cwd, "session.jsonl", fixture, fixedNow.Add(-time.Hour))
	return &Finder{
		ClaudeDir:   root,
		OpencodeDir: filepath.Join(root, "no-opencode"),
		ProcDir:     filepath.Join(root, "no-proc"),
		Now:         func() time.Time { return fixedNow },
	}
}

// writeClaudeTranscript copies a fixture into <root>/projects/<slug(cwd)>/
// under the given name and pins its mtime.
func writeClaudeTranscript(t *testing.T, root, cwd, name, fixture string, mtime time.Time) string {
	t.Helper()
	dir := filepath.Join(root, "projects", claudeSlug(cwd))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join("testdata", "claude", fixture))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestClaudeSlugPinsTheObservedMapping pins the cwd-to-directory rule
// against mappings observed from a real Claude Code install. If the slugging
// ever changes upstream, this fails before a user gets a silent
// wrong-directory lookup.
func TestClaudeSlugPinsTheObservedMapping(t *testing.T) {
	cases := map[string]string{
		"/home/rpickz/Work/rpickz/jarvix": "-home-rpickz-Work-rpickz-jarvix",
		// Dots and underscores flatten to dashes exactly like slashes.
		"/home/rpickz/Downloads/MRUpdater-linux-x86_64": "-home-rpickz-Downloads-MRUpdater-linux-x86-64",
		"/srv/app.v2": "-srv-app-v2",
	}
	for cwd, want := range cases {
		if got := claudeSlug(cwd); got != want {
			t.Errorf("claudeSlug(%q) = %q, want %q", cwd, got, want)
		}
	}
}

// TestClaudeFixturesClassifyDeterministically drives the four real-shape
// fixtures — mid-task, awaiting an answer, finished, errored — through the
// whole read and pins each classification to the transcript's structure.
func TestClaudeFixturesClassifyDeterministically(t *testing.T) {
	cases := []struct {
		fixture string
		state   State
		// rendered is content the prompt window must carry — the session's
		// actual substance, not a title's implication.
		rendered string
	}{
		{"midtask.jsonl", StateWorking, "Assistant ran Bash."},
		{"awaiting.jsonl", StateNeedsYou, "Should I drop the duplicate column"},
		{"finished.jsonl", StateDone, "all 132 tests pass"},
		{"error.jsonl", StateNeedsYou, "API Error: 529"},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			cwd := "/home/user/work/billing"
			f := claudeFinder(t, cwd, tc.fixture)
			tail, err := f.Read(context.Background(), cwd)
			if err != nil {
				t.Fatal(err)
			}
			if tail.State != tc.state {
				t.Errorf("state = %q, want %q", tail.State, tc.state)
			}
			if tail.Source != "claude" {
				t.Errorf("source = %q", tail.Source)
			}
			if !strings.Contains(tail.Text, tc.rendered) {
				t.Errorf("rendered tail is missing %q:\n%s", tc.rendered, tail.Text)
			}
		})
	}
}

// TestClaudeRenderExcludesWhatMustNeverTravel: chain-of-thought and tool
// output are dropped from the render — they are the most secret-prone
// content in the file and never conversation — and harness-injected XML
// user lines are dropped as words the user never said.
func TestClaudeRenderExcludesWhatMustNeverTravel(t *testing.T) {
	cwd := "/home/user/work/billing"
	f := claudeFinder(t, cwd, "midtask.jsonl")
	tail, err := f.Read(context.Background(), cwd)
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{
		"NEVER-SPOKEN-THINKING",          // a thinking block
		"ok  \tinternal/billing\t0.412s", // a tool result
	} {
		if strings.Contains(tail.Text, banned) {
			t.Errorf("the render carries %q:\n%s", banned, tail.Text)
		}
	}
	if !strings.Contains(tail.Text, "User: Fix the payment webhook") {
		t.Errorf("the user's own words are missing:\n%s", tail.Text)
	}
}

// TestNewestTranscriptWins: two sessions in the same project directory, and
// the read serves the one whose file moved last — the live session, not the
// morning's.
func TestNewestTranscriptWins(t *testing.T) {
	cwd := "/home/user/work/billing"
	root := t.TempDir()
	writeClaudeTranscript(t, root, cwd, "old.jsonl", "finished.jsonl", fixedNow.Add(-6*time.Hour))
	writeClaudeTranscript(t, root, cwd, "new.jsonl", "midtask.jsonl", fixedNow.Add(-time.Minute))
	f := &Finder{ClaudeDir: root, Now: func() time.Time { return fixedNow }}
	tail, err := f.Read(context.Background(), cwd)
	if err != nil {
		t.Fatal(err)
	}
	if tail.State != StateWorking {
		t.Errorf("state = %q; the stale finished session won over the live one", tail.State)
	}
}

// TestStaleTranscriptIsNoSession is the freshness gate: a transcript nobody
// touched in two days is an archive, and resurrecting it would speak a
// confident stale sentence — so it is absence, silently.
func TestStaleTranscriptIsNoSession(t *testing.T) {
	cwd := "/home/user/work/billing"
	root := t.TempDir()
	writeClaudeTranscript(t, root, cwd, "old.jsonl", "awaiting.jsonl", fixedNow.Add(-maxSessionAge-time.Hour))
	f := &Finder{ClaudeDir: root, Now: func() time.Time { return fixedNow }}
	if _, err := f.Read(context.Background(), cwd); !errors.Is(err, ErrNoSession) {
		t.Errorf("a stale transcript read as a session: %v", err)
	}
}

// TestAbsenceIsErrNoSession: a directory hosting no AI session at all — no
// project dir for either CLI — is ErrNoSession, the silent-fallback signal,
// never a hard error.
func TestAbsenceIsErrNoSession(t *testing.T) {
	root := t.TempDir()
	f := &Finder{
		ClaudeDir:   filepath.Join(root, "claude"),
		OpencodeDir: filepath.Join(root, "opencode"),
		Now:         func() time.Time { return fixedNow },
	}
	if _, err := f.Read(context.Background(), "/home/user/somewhere"); !errors.Is(err, ErrNoSession) {
		t.Errorf("absence = %v, want ErrNoSession", err)
	}
}

// TestUnreadableTranscriptIsARealError: a session provably exists but its
// tail yields no conversation — that is found-but-unreadable, which the
// recap must admit, never ErrNoSession's silence.
func TestUnreadableTranscriptIsARealError(t *testing.T) {
	cwd := "/home/user/work/billing"
	root := t.TempDir()
	dir := filepath.Join(root, "projects", claudeSlug(cwd))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "garbage.jsonl")
	if err := os.WriteFile(path, []byte("not json at all\n{{{\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, fixedNow.Add(-time.Hour), fixedNow.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	f := &Finder{ClaudeDir: root, Now: func() time.Time { return fixedNow }}
	_, err := f.Read(context.Background(), cwd)
	if err == nil || errors.Is(err, ErrNoSession) {
		t.Errorf("an unreadable session read as %v; want a real error", err)
	}
}

// TestTailReadIsBoundedAndDropsTheTornLine: with a tail bound smaller than
// the file, the read starts mid-file, drops the torn first line, and still
// classifies from what remains — the bounded-read acceptance criterion.
func TestTailReadIsBoundedAndDropsTheTornLine(t *testing.T) {
	cwd := "/home/user/work/billing"
	root := t.TempDir()
	// Pad the fixture's front with a long prelude of user lines so the tail
	// window provably starts inside one of them.
	var b strings.Builder
	for range 200 {
		line, _ := json.Marshal(map[string]any{
			"type":    "user",
			"message": map[string]any{"role": "user", "content": "PRELUDE " + strings.Repeat("x", 400)},
		})
		b.Write(line)
		b.WriteByte('\n')
	}
	data, err := os.ReadFile(filepath.Join("testdata", "claude", "awaiting.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	b.Write(data)
	dir := filepath.Join(root, "projects", claudeSlug(cwd))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "long.jsonl")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, fixedNow.Add(-time.Hour), fixedNow.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	f := &Finder{ClaudeDir: root, MaxTailBytes: 4096, Now: func() time.Time { return fixedNow }}
	tail, err := f.Read(context.Background(), cwd)
	if err != nil {
		t.Fatal(err)
	}
	if tail.State != StateNeedsYou {
		t.Errorf("state = %q after a bounded read", tail.State)
	}
}

// TestRenderKeepsTheNewestUnderTheRuneBudget: when the exchanges exceed the
// text budget, the oldest lines are dropped — the newest exchange is the one
// the recap exists to speak.
func TestRenderKeepsTheNewestUnderTheRuneBudget(t *testing.T) {
	cwd := "/home/user/work/billing"
	f := claudeFinder(t, cwd, "awaiting.jsonl")
	// Enough for the closing exchange, not for the opening ask.
	f.MaxTextRunes = 200
	tail, err := f.Read(context.Background(), cwd)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tail.Text, "rebase this migration") {
		t.Errorf("the newest exchange was cut:\n%s", tail.Text)
	}
	if strings.Contains(tail.Text, "Write the sessions migration") {
		t.Errorf("the budget kept the oldest line over the newest:\n%s", tail.Text)
	}
}

// TestOpencodeFixtureTreeReads drives the opencode adapter over its real
// file layout: project index by worktree, newest session, messages with
// their parts — and pins the classification and the render exclusions.
func TestOpencodeFixtureTreeReads(t *testing.T) {
	f := &Finder{
		ClaudeDir:   filepath.Join(t.TempDir(), "no-claude"),
		OpencodeDir: filepath.Join("testdata", "opencode"),
		Now:         func() time.Time { return fixedNow },
	}
	tail, err := f.Read(context.Background(), "/home/user/work/billing")
	if err != nil {
		t.Fatal(err)
	}
	if tail.Source != "opencode" {
		t.Errorf("source = %q", tail.Source)
	}
	if tail.State != StateNeedsYou {
		t.Errorf("state = %q, want needs_you (the assistant stopped on a question)", tail.State)
	}
	if !strings.Contains(tail.Text, "Should I drop the duplicate") {
		t.Errorf("the assistant's question is missing:\n%s", tail.Text)
	}
	if !strings.Contains(tail.Text, "Assistant ran bash.") {
		t.Errorf("the tool run note is missing:\n%s", tail.Text)
	}
	if strings.Contains(tail.Text, "NEVER-SPOKEN-REASONING") {
		t.Errorf("a reasoning part reached the render:\n%s", tail.Text)
	}
}

// TestOpencodeClassification is the deterministic table in opencode's
// shapes, unit-level: each rule keyed to structure, never prose.
func TestOpencodeClassification(t *testing.T) {
	completed := func(m opencodeMessage) opencodeMessage {
		m.Time.Completed = 5
		return m
	}
	cases := []struct {
		name string
		last opencodeMessage
		want State
	}{
		{"user message means working",
			opencodeMessage{Role: "user"}, StateWorking},
		{"uncompleted assistant means working",
			opencodeMessage{Role: "assistant"}, StateWorking},
		{"tool-calls finish means working",
			completed(opencodeMessage{Role: "assistant", Finish: "tool-calls"}), StateWorking},
		{"an error needs you",
			completed(opencodeMessage{Role: "assistant", Error: json.RawMessage(`{"name":"x"}`)}), StateNeedsYou},
		{"a closing question needs you",
			completed(opencodeMessage{Role: "assistant", parts: []opencodePart{
				{Type: "text", Text: "Drop the column?"},
			}}), StateNeedsYou},
		{"a closing statement is done",
			completed(opencodeMessage{Role: "assistant", parts: []opencodePart{
				{Type: "text", Text: "All tests pass."},
			}}), StateDone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyOpencode(tc.last); got != tc.want {
				t.Errorf("classifyOpencode = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestEndsWithQuestion pins the question detector's tolerance: trailing
// formatting is looked through, and an interior question never counts.
func TestEndsWithQuestion(t *testing.T) {
	cases := map[string]bool{
		"Should I drop it?":            true,
		"Should I drop it?\"":          true,
		"Should I drop it? \n":         true,
		"Is it done? Yes, completely.": false,
		"All tests pass.":              false,
		"":                             false,
	}
	for text, want := range cases {
		if got := endsWithQuestion(text); got != want {
			t.Errorf("endsWithQuestion(%q) = %v, want %v", text, got, want)
		}
	}
}
