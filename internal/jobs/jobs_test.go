package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The scope's own tests. Everything here is about the one question the feature
// turns on: given a subject the daemon read, is this inside the boundary the
// user set? Nothing in this file consults a model, a clock or a disk.

func TestAScopeWithNoBoundaryIsRefused(t *testing.T) {
	cases := []struct {
		name  string
		scope Scope
		want  string
	}{
		{"no tools", Scope{Roots: []string{"/tmp"}}, "names no tools"},
		{"no boundary", Scope{Tools: []string{"memory.search"}}, "neither a directory nor an application"},
		{"relative root", Scope{Tools: []string{"memory.search"}, Roots: []string{"code"}},
			"not an absolute path"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := c.scope.Validate(); err == nil {
				t.Fatal("a scope that cannot be enforced was accepted; a job must not start without one")
			} else if !strings.Contains(err.Error(), c.want) {
				t.Errorf("refusal = %q, want it to mention %q", err.Error(), c.want)
			}
		})
	}
}

// TestAScopeMayNotReachTheToolsThatGovernJarvix is #109's wall, at a job's
// height. A scope that could name config.write_entry would be a job that could
// rewrite [tools.policy], which is a job that could widen its own boundary.
func TestAScopeMayNotReachTheToolsThatGovernJarvix(t *testing.T) {
	for _, banned := range Forbidden {
		scope := Scope{Tools: []string{"memory.search", banned}, Roots: []string{"/tmp"}}
		if _, err := scope.Validate(); err == nil {
			t.Errorf("a scope naming %s was accepted; a job is not a privilege escalation", banned)
		}
		// And again at the Judge, because a scope can arrive off a hand-edited
		// disk without ever passing through Validate.
		if ruling := (Scope{Tools: []string{banned}}).Judge(Attempt{Tool: banned}); ruling.OK {
			t.Errorf("Judge allowed %s; the wall must hold for a scope read off disk too", banned)
		}
	}
}

func TestAToolTheScopeDidNotNameIsOutsideIt(t *testing.T) {
	scope := must(t, Scope{Tools: []string{"memory.search"}, Roots: []string{t.TempDir()}})
	ruling := scope.Judge(Attempt{Tool: "memory.remember"})
	if ruling.OK {
		t.Fatal("a tool the job was never given was judged inside its scope")
	}
	if !strings.Contains(ruling.Because, "memory.remember") {
		t.Errorf("reason = %q, want it to name the tool the job would have used", ruling.Because)
	}
}

func TestAPathOutsideEveryRootIsOutsideTheScope(t *testing.T) {
	root := t.TempDir()
	scope := must(t, Scope{Tools: []string{"memory.remember"}, Roots: []string{root}})

	inside := filepath.Join(root, "notes", "one.txt")
	if ruling := scope.Judge(Attempt{Tool: "memory.remember", Paths: []string{inside}}); !ruling.OK {
		t.Fatalf("a path inside the root was refused: %s", ruling.Because)
	}
	outside := filepath.Join(filepath.Dir(root), "elsewhere", "one.txt")
	ruling := scope.Judge(Attempt{Tool: "memory.remember", Paths: []string{outside}})
	if ruling.OK {
		t.Fatal("a path outside every root was judged inside the scope")
	}
	if !strings.Contains(ruling.Because, outside) {
		t.Errorf("reason = %q, want it to name the path it would have touched", ruling.Because)
	}
}

// TestASymlinkOutOfTheTreeIsNotAWayThrough is the containment check's whole
// point. `~/code/out -> /etc` reads as inside the scope and writes outside it,
// so the comparison happens after both sides are resolved.
func TestASymlinkOutOfTheTreeIsNotAWayThrough(t *testing.T) {
	root := t.TempDir()
	elsewhere := t.TempDir()
	link := filepath.Join(root, "out")
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}
	scope := must(t, Scope{Tools: []string{"memory.remember"}, Roots: []string{root}})

	// Through the link to a file that does not exist yet, which is the
	// ordinary case for a job about to write one.
	if ruling := scope.Judge(Attempt{Tool: "memory.remember",
		Paths: []string{filepath.Join(link, "passwd")}}); ruling.OK {
		t.Fatal("a symlink out of the tree let a write escape the scope")
	}
	// And to one that does.
	target := filepath.Join(elsewhere, "already-here")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ruling := scope.Judge(Attempt{Tool: "memory.remember",
		Paths: []string{filepath.Join(link, "already-here")}}); ruling.OK {
		t.Fatal("a symlink out of the tree let an existing file be reached")
	}
}

// TestAPathThatIsNotAbsoluteIsNeverHeld pins the refusing direction: a subject
// nobody can place cannot be proved to be inside anything.
func TestAPathThatIsNotAbsoluteIsNeverHeld(t *testing.T) {
	scope := must(t, Scope{Tools: []string{"memory.remember"}, Roots: []string{t.TempDir()}})
	if ruling := scope.Judge(Attempt{Tool: "memory.remember", Paths: []string{"notes/one.txt"}}); ruling.OK {
		t.Fatal("a relative path was judged inside the scope")
	}
}

// TestARootPrefixIsNotEnough guards the classic containment bug: /home/rich
// must not admit /home/richard.
func TestARootPrefixIsNotEnough(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "rich")
	sibling := filepath.Join(base, "richard")
	for _, dir := range []string{root, sibling} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	scope := must(t, Scope{Tools: []string{"memory.remember"}, Roots: []string{root}})
	if ruling := scope.Judge(Attempt{Tool: "memory.remember",
		Paths: []string{filepath.Join(sibling, "notes")}}); ruling.OK {
		t.Fatal("a sibling directory sharing a prefix with the root was judged inside it")
	}
}

func TestAWindowOutsideTheScopesAppsIsRefused(t *testing.T) {
	scope := must(t, Scope{Tools: []string{"desktop.move_window"}, Apps: []string{"Alacritty"}})
	if ruling := scope.Judge(Attempt{Tool: "desktop.move_window", App: "alacritty"}); !ruling.OK {
		t.Fatalf("the scope's own app was refused: %s", ruling.Because)
	}
	ruling := scope.Judge(Attempt{Tool: "desktop.move_window", App: "firefox", Window: "Firefox — bank"})
	if ruling.OK {
		t.Fatal("a window of an app the job was never given was judged inside its scope")
	}
	if !strings.Contains(ruling.Because, "Firefox — bank") {
		t.Errorf("reason = %q, want it to name the window it would have acted on", ruling.Because)
	}
}

// TestTheScopeIsStatedInFull is the confirmation's contract: a listener told
// half a boundary assumes the other half is narrower than it is.
func TestTheScopeIsStatedInFull(t *testing.T) {
	root := t.TempDir()
	scope := must(t, Scope{Tools: []string{"memory.search", "memory.remember"},
		Roots: []string{root}, Apps: []string{"Alacritty"}})
	stated := scope.Stated()
	for _, want := range []string{root, "alacritty", "memory.search", "memory.remember"} {
		if !strings.Contains(stated, want) {
			t.Errorf("Stated() = %q, want it to name %q", stated, want)
		}
	}
}

func TestANameMustBeShortEnoughToSay(t *testing.T) {
	if _, err := CleanName("  "); err == nil {
		t.Error("a job with no name was accepted; there would be no way to ask about it")
	}
	if _, err := CleanName("tidy up the downloads folder please"); err == nil {
		t.Error("a name too long to say was accepted")
	}
	got, err := CleanName("  Tidy   Downloads ")
	if err != nil {
		t.Fatal(err)
	}
	if got != "tidy downloads" {
		t.Errorf("CleanName = %q, want %q", got, "tidy downloads")
	}
}

func TestStatesKnowWhetherTheyAreStillGoing(t *testing.T) {
	for _, st := range []State{Ready, Running, Parked} {
		if !st.Live() {
			t.Errorf("%s should still be going", st)
		}
	}
	for _, st := range []State{Done, Stopped, Failed} {
		if st.Live() {
			t.Errorf("%s should be finished", st)
		}
	}
}

func TestOnlyAQuestionCanBeAnswered(t *testing.T) {
	for _, w := range []Why{WhyApproval, WhyDecision} {
		if !w.Answerable() {
			t.Errorf("%s should be answerable: it is a question", w)
		}
	}
	for _, w := range []Why{WhyOutOfScope, WhyRefused, WhyUnclear, WhyStuck} {
		if w.Answerable() {
			t.Errorf("%s must not be answerable: saying yes to a boundary is not a decision the user gets to make", w)
		}
	}
}

// must validates a scope or fails the test.
func must(t *testing.T, s Scope) Scope {
	t.Helper()
	out, err := s.Validate()
	if err != nil {
		t.Fatalf("scope should have been enforceable: %v", err)
	}
	return out
}
