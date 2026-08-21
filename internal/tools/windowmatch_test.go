package tools

import (
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/desktop"
)

// inventory is the desktop every matcher test runs against: two browser
// windows of the same application, a terminal, a note-taker with a
// reverse-DNS class, and a focused editor.
func inventory() []desktop.Window {
	return []desktop.Window{
		{Address: "0x1", Class: "code", Title: "engine.go — jarvix", Workspace: 1,
			WorkspaceName: "1", Focused: true},
		{Address: "0x2", Class: "firefox", Title: "GitHub — pull requests", Workspace: 1, WorkspaceName: "1"},
		{Address: "0x3", Class: "firefox", Title: "Inbox — Fastmail", Workspace: 2, WorkspaceName: "2"},
		{Address: "0x4", Class: "Alacritty", Title: "go test ./...", Workspace: 2, WorkspaceName: "2"},
		{Address: "0x5", Class: "md.obsidian.Obsidian", Title: "roadmap — notes", Workspace: 3, WorkspaceName: "3"},
	}
}

func TestResolveWindow(t *testing.T) {
	tests := []struct {
		name  string
		query string
		kind  resolveKind
		// want is the address of the single match, or the addresses of the
		// tied candidates for an ambiguous one.
		want []string
	}{
		{"exact class", "alacritty", resolveOne, []string{"0x4"}},
		{"exact class ignores case", "ALACRITTY", resolveOne, []string{"0x4"}},
		{"exact class of a reverse-dns application", "obsidian", resolveOne, []string{"0x5"}},
		{"exact title", "go test ./...", resolveOne, []string{"0x4"}},
		{"substring of a title", "fastmail", resolveOne, []string{"0x3"}},
		{"prefix of a title", "inbox", resolveOne, []string{"0x3"}},
		{"words in any order", "github pull", resolveOne, []string{"0x2"}},
		{"stop words are ignored", "my alacritty window", resolveOne, []string{"0x4"}},
		{"category with one match", "terminal", resolveOne, []string{"0x4"}},
		{"category with several matches asks", "browser", resolveMany, []string{"0x2", "0x3"}},
		{"an application with two windows asks", "firefox", resolveMany, []string{"0x2", "0x3"}},
		{"naming the application beats the category", "obsidian", resolveOne, []string{"0x5"}},
		{"no match", "photoshop", resolveNone, nil},
		{"this means the focused window", "this", resolveOne, []string{"0x1"}},
		{"nothing said means the focused window", "", resolveOne, []string{"0x1"}},
		{"the current window", "the current window", resolveOne, []string{"0x1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := resolveWindow(tt.query, inventory())
			if res.Kind != tt.kind {
				t.Fatalf("kind = %v (window %q, candidates %d), want %v",
					res.Kind, res.Window.Address, len(res.Candidates), tt.kind)
			}
			switch res.Kind {
			case resolveOne:
				if res.Window.Address != tt.want[0] {
					t.Errorf("matched %s (%s), want %s", res.Window.Address, res.Window.Describe(), tt.want[0])
				}
			case resolveMany:
				got := make([]string, 0, len(res.Candidates))
				for _, w := range res.Candidates {
					got = append(got, w.Address)
				}
				if strings.Join(got, ",") != strings.Join(tt.want, ",") {
					t.Errorf("candidates = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestResolveWindowOnAnEmptyDesktop(t *testing.T) {
	for _, query := range []string{"firefox", "this", ""} {
		if res := resolveWindow(query, nil); res.Kind != resolveNone {
			t.Errorf("resolveWindow(%q, nothing open) = %v, want no match", query, res.Kind)
		}
	}
}

// A better tier wins outright: a window whose class the user said is the
// answer even when a weaker tier matches several others.
func TestResolveWindowPrefersTheStrongerSignal(t *testing.T) {
	windows := []desktop.Window{
		{Address: "0x1", Class: "firefox", Title: "a browser tab about terminals"},
		{Address: "0x2", Class: "Alacritty", Title: "terminal"},
	}
	res := resolveWindow("terminal", windows)
	if res.Kind != resolveOne || res.Window.Address != "0x2" {
		t.Errorf("resolve = %v %q, want the window titled terminal", res.Kind, res.Window.Address)
	}
}

// Substring matching must not become "any word starts with any letter": a
// query word matches a haystack word by prefix, not the other way round.
func TestResolveWindowDoesNotMatchOnFragments(t *testing.T) {
	windows := []desktop.Window{{Address: "0x1", Class: "xterm", Title: "shell"}}
	if res := resolveWindow("term", windows); res.Kind != resolveOne {
		// "term" *is* a substring of "xterm", which is the tolerated looseness.
		t.Errorf("resolve = %v, want the substring match", res.Kind)
	}
	if res := resolveWindow("terminology", windows); res.Kind != resolveNone {
		t.Errorf("resolve = %v, want no match for a longer word", res.Kind)
	}
}

func TestDescribeCandidatesNamesWindowsAndBoundsTheList(t *testing.T) {
	res := resolveWindow("firefox", inventory())
	got := describeCandidates(res.Candidates)
	for _, want := range []string{"firefox — GitHub — pull requests", "workspace 1", "Fastmail"} {
		if !strings.Contains(got, want) {
			t.Errorf("describeCandidates() = %q, missing %q", got, want)
		}
	}

	many := make([]desktop.Window, 0, 9)
	for i := 0; i < 9; i++ {
		many = append(many, desktop.Window{Address: "0x", Class: "firefox", Title: "tab"})
	}
	if got := describeCandidates(many); !strings.Contains(got, "4 other windows") {
		t.Errorf("describeCandidates(9) = %q, want the tail counted", got)
	}
}

func TestSummariseWindowsGroupsAndNeverLeaksIdentifiers(t *testing.T) {
	got := summariseWindows(inventory())
	for _, want := range []string{"firefox", "Obsidian", "workspace 3", "the one the user is in"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "0x") {
		t.Errorf("summary %q contains a window address; nothing spoken may", got)
	}
}
