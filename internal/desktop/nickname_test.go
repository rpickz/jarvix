package desktop

import (
	"strings"
	"sync"
	"testing"
)

// The registry tests (#126) pin the assignment rules — single word,
// reserved words, phrase and nickname collisions — and the release
// mechanism: lazy revalidation against whatever inventory the caller is
// judging by, so a closed window's name can never resolve, however the
// close happened.

func nicknameWindows() []Window {
	return []Window{
		{Address: "0xa", Class: "code", Title: "engine.go", StableID: "s1", Focused: true},
		{Address: "0xb", Class: "firefox", Title: "GitHub", StableID: "s2"},
		{Address: "0xc", Class: "Alacritty", Title: "go test", StableID: "s3"},
	}
}

func testRegistry() *Nicknames {
	return NewNicknames(NicknameOptions{
		Reserved: map[string]string{"this": "it is how a reference says \"the window I am in\""},
		PhraseOwner: func(phrase string) (string, bool) {
			if phrase == "mute" {
				return `the built-in intent "volume.mute"`, true
			}
			return "", false
		},
	})
}

func TestAssignNormalisesToOneLowercaseWord(t *testing.T) {
	n := testRegistry()
	windows := nicknameWindows()
	name, previous, warning, err := n.Assign("Builds!", windows[2], windows)
	if err != nil {
		t.Fatal(err)
	}
	if name != "builds" || previous != "" || warning != "" {
		t.Errorf("Assign = %q, %q, %q", name, previous, warning)
	}
	if w, ok := n.Resolve("builds", windows); !ok || w.Address != "0xc" {
		t.Errorf("Resolve(builds) = %+v, %v", w, ok)
	}
}

func TestAssignRefusesMultipleWordsWithGuidance(t *testing.T) {
	n := testRegistry()
	windows := nicknameWindows()
	_, _, _, err := n.Assign("the build terminal", windows[2], windows)
	if err == nil || !strings.Contains(err.Error(), `"the"`) || !strings.Contains(err.Error(), "single word") {
		t.Errorf("err = %v, want a single-word refusal suggesting the first word", err)
	}
}

func TestAssignRefusesReservedWordsNamingTheOwner(t *testing.T) {
	n := testRegistry()
	windows := nicknameWindows()
	_, _, _, err := n.Assign("this", windows[2], windows)
	if err == nil || !strings.Contains(err.Error(), "the window I am in") {
		t.Errorf("err = %v, want the reserved word's owner named", err)
	}
}

func TestAssignRefusesIntentPhrasesNamingTheOwner(t *testing.T) {
	n := testRegistry()
	windows := nicknameWindows()
	_, _, _, err := n.Assign("mute", windows[2], windows)
	if err == nil || !strings.Contains(err.Error(), `the built-in intent "volume.mute"`) {
		t.Errorf("err = %v, want the intent owner named", err)
	}
}

func TestAssignRefusesACollisionNamingTheHolder(t *testing.T) {
	n := testRegistry()
	windows := nicknameWindows()
	if _, _, _, err := n.Assign("builds", windows[2], windows); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := n.Assign("builds", windows[0], windows)
	if err == nil || !strings.Contains(err.Error(), "Alacritty") {
		t.Errorf("err = %v, want the holding window named", err)
	}
	// The same name on the same window is a re-confirmation, never an error.
	if _, _, _, err := n.Assign("builds", windows[2], windows); err != nil {
		t.Errorf("re-assigning the same name errored: %v", err)
	}
}

// TestRenamingReleasesTheOldName: one name per window — the old name is
// given up, answers "released" from then on, and is free for another window.
func TestRenamingReleasesTheOldName(t *testing.T) {
	n := testRegistry()
	windows := nicknameWindows()
	if _, _, _, err := n.Assign("builds", windows[2], windows); err != nil {
		t.Fatal(err)
	}
	_, previous, _, err := n.Assign("tests", windows[2], windows)
	if err != nil || previous != "builds" {
		t.Fatalf("Assign = previous %q, err %v; want the released name back", previous, err)
	}
	if _, ok := n.Resolve("builds", windows); ok {
		t.Error("the old name still resolves")
	}
	if !n.Released("builds", windows) {
		t.Error("the old name is not marked released")
	}
	if _, _, _, err := n.Assign("builds", windows[0], windows); err != nil {
		t.Errorf("a released name was not free to reassign: %v", err)
	}
}

// TestCloseReleasesTheNickname is the release mechanism: the window leaves
// the inventory and its name stops resolving — released, honestly, rather
// than unknown — with no event subscription anywhere.
func TestCloseReleasesTheNickname(t *testing.T) {
	n := testRegistry()
	windows := nicknameWindows()
	if _, _, _, err := n.Assign("builds", windows[2], windows); err != nil {
		t.Fatal(err)
	}
	remaining := windows[:2]
	if _, ok := n.Resolve("builds", remaining); ok {
		t.Fatal("a closed window's nickname resolved")
	}
	if !n.Released("builds", remaining) {
		t.Error("the closed window's name is not marked released")
	}
	if n.Released("nonsense", remaining) {
		t.Error("a name that never existed reads as released")
	}
	// The window coming back under a recycled address is a different window:
	// identity is address AND stable id AND class, so the name stays gone.
	recycled := append(append([]Window(nil), remaining...),
		Window{Address: "0xc", Class: "Alacritty", Title: "new shell", StableID: "s9"})
	if _, ok := n.Resolve("builds", recycled); ok {
		t.Error("a recycled address inherited a released nickname")
	}
}

func TestResolveAnswersWithTheCurrentWindow(t *testing.T) {
	n := testRegistry()
	windows := nicknameWindows()
	if _, _, _, err := n.Assign("mail", windows[1], windows); err != nil {
		t.Fatal(err)
	}
	// The title moved on since assignment; identity held.
	windows[1].Title = "Fastmail — Inbox"
	w, ok := n.Resolve("mail", windows)
	if !ok || w.Title != "Fastmail — Inbox" {
		t.Errorf("Resolve = %+v, %v; want the live window", w, ok)
	}
}

func TestListIsSortedAndLive(t *testing.T) {
	n := testRegistry()
	windows := nicknameWindows()
	for name, i := range map[string]int{"mail": 1, "builds": 2} {
		if _, _, _, err := n.Assign(name, windows[i], windows); err != nil {
			t.Fatal(err)
		}
	}
	named := n.List(windows)
	if len(named) != 2 || named[0].Name != "builds" || named[1].Name != "mail" {
		t.Fatalf("List = %+v, want builds then mail", named)
	}
	// A closed window leaves the listing on the next look.
	named = n.List(windows[:2])
	if len(named) != 1 || named[0].Name != "mail" {
		t.Errorf("List after close = %+v, want mail alone", named)
	}
}

func TestCommonWordsWarnButAssign(t *testing.T) {
	n := testRegistry()
	windows := nicknameWindows()
	name, _, warning, err := n.Assign("work", windows[2], windows)
	if err != nil || name != "work" {
		t.Fatalf("Assign = %q, %v; a common word is a warning, never a refusal", name, err)
	}
	if !strings.Contains(warning, "common word") {
		t.Errorf("warning = %q, want the common-word caution", warning)
	}
	if _, _, warning, _ := n.Assign("zephyr", windows[1], windows); warning != "" {
		t.Errorf("warning = %q for an uncommon word, want none", warning)
	}
}

// TestRegistryIsRaceClean exercises concurrent assignment, resolution and
// listing under -race: the registry is shared by session goroutines, tool
// calls, and IPC handlers, and must hold up without any caller-side locking.
func TestRegistryIsRaceClean(t *testing.T) {
	n := testRegistry()
	windows := nicknameWindows()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			target := windows[i%len(windows)]
			_, _, _, _ = n.Assign("worker", target, windows)
			_, _ = n.Resolve("worker", windows)
			_ = n.List(windows)
			_ = n.Released("worker", windows[:2])
		}(i)
	}
	wg.Wait()
}
