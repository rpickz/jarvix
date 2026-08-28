package placement

import (
	"strings"
	"testing"
)

// TestRefKindTellsTheThreeFormsApart: the classification #180 extends. A
// connector is recognised by its shape rather than by a list of families,
// because the families are the kernel's and a new one must not need a code
// change here.
func TestRefKindTellsTheThreeFormsApart(t *testing.T) {
	for ref, want := range map[MonitorRef]RefKind{
		"":          RefNone,
		"current":   RefCurrent,
		"CURRENT":   RefCurrent,
		"DP-2":      RefConnector,
		"HDMI-A-1":  RefConnector,
		"eDP-1":     RefConnector,
		"DP-99":     RefConnector,
		"top":       RefNickname,
		"the-big-1": RefConnector, // shaped like a connector; resolution decides, not spelling
		"bottom":    RefNickname,
	} {
		if got := ref.Kind(); got != want {
			t.Errorf("Kind(%q) = %v, want %v", ref, got, want)
		}
	}
}

// TestResolvePrefersAPresentOutputOverANickname is the precedence rule that
// makes nicknames safe to add: a name on the end of a cable right now is
// never ambiguous, so a nickname can never quietly redirect a routine that
// named a real connector.
func TestResolvePrefersAPresentOutputOverANickname(t *testing.T) {
	inventory := []Monitor{topMonitor(), bottomMonitor()}
	r := Resolver{Nicknames: func(name string) (string, bool) {
		if name == "DP-2" {
			return "HDMI-A-1", true // a nickname that shadows a real connector
		}
		return "", false
	}}
	got, err := r.Resolve("DP-2", inventory)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "DP-2" {
		t.Errorf("resolved to %q; a present output must win over a nickname of the same name", got.Name)
	}
}

// TestResolveHonoursTheNicknameSeam: the hook #180 fills in. Nothing here
// implements nicknames — this proves the shape they slot into resolves, so
// that issue is one field and not a rewrite.
func TestResolveHonoursTheNicknameSeam(t *testing.T) {
	inventory := []Monitor{topMonitor(), bottomMonitor()}
	r := Resolver{Nicknames: func(name string) (string, bool) {
		return map[string]string{"top": "HDMI-A-1", "bottom": "DP-2"}[name],
			name == "top" || name == "bottom"
	}}
	for ref, want := range map[MonitorRef]string{"top": "HDMI-A-1", "bottom": "DP-2"} {
		got, err := r.Resolve(ref, inventory)
		if err != nil || got.Name != want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", ref, got.Name, err, want)
		}
	}
	// With no nickname table — today's state — the same ref is an honest
	// "nothing is called that", never a silent fallback to the focused screen.
	_, err := Resolver{}.Resolve("top", inventory)
	if err == nil || !strings.Contains(err.Error(), `no monitor is called "top" right now`) {
		t.Errorf("without nicknames, Resolve(top) = %v", err)
	}
}

// TestResolveNamesWhatIsPluggedIn: a monitor that disappeared fails with THAT
// reason and the screens that are there — the #180 contract, and the
// difference between a routine a user can fix and one that just went wrong.
func TestResolveNamesWhatIsPluggedIn(t *testing.T) {
	_, err := Resolver{}.Resolve("DP-9", []Monitor{topMonitor(), bottomMonitor()})
	if err == nil {
		t.Fatal("a monitor that is not there resolved")
	}
	if !strings.Contains(err.Error(), "DP-2, HDMI-A-1") {
		t.Errorf("error %q does not list the screens that are plugged in", err)
	}
	if _, err := (Resolver{}).Resolve("DP-2", nil); err == nil ||
		!strings.Contains(err.Error(), "reports no monitors") {
		t.Errorf("empty inventory = %v", err)
	}
}

// TestCurrentAndEmptyResolveToTheFocusedScreen: "current" is a reserved word,
// and saying nothing about a monitor still has to yield one, because a
// percentage needs something real to be a share of.
func TestCurrentAndEmptyResolveToTheFocusedScreen(t *testing.T) {
	inventory := []Monitor{bottomMonitor(), topMonitor()} // focused one second
	for _, ref := range []MonitorRef{"", MonitorCurrent} {
		got, err := Resolver{}.Resolve(ref, inventory)
		if err != nil || got.Name != "HDMI-A-1" {
			t.Errorf("Resolve(%q) = %q, %v", ref, got.Name, err)
		}
	}
	// An inventory where nothing claims focus still answers: the first output
	// is the only defensible guess, and an error here would be worse.
	unfocused := []Monitor{bottomMonitor()}
	if got, err := (Resolver{}).Resolve(MonitorCurrent, unfocused); err != nil || got.Name != "DP-2" {
		t.Errorf("unfocused inventory = %q, %v", got.Name, err)
	}
}

// TestForWorkspaceFindsTheScreenAWorkspaceIsOn: what a step that named no
// monitor resolves its percentages against.
func TestForWorkspaceFindsTheScreenAWorkspaceIsOn(t *testing.T) {
	inventory := []Monitor{topMonitor(), bottomMonitor()} // ws 1 above, ws 2 below
	if got := ForWorkspace(2, inventory); got.Name != "DP-2" {
		t.Errorf("workspace 2 is on %q, want DP-2", got.Name)
	}
	// A workspace nobody is showing has not been opened yet; it will open on
	// the focused screen, so that is what a percentage is a share of.
	if got := ForWorkspace(7, inventory); got.Name != "HDMI-A-1" {
		t.Errorf("unopened workspace resolved to %q, want the focused screen", got.Name)
	}
}

// TestReservedMonitorWordsAreDeclaredHere: #180 needs the list to refuse a
// colliding nickname, and it must come from the vocabulary rather than being
// typed a second time in the nickname store.
func TestReservedMonitorWordsAreDeclaredHere(t *testing.T) {
	words := ReservedMonitorWords()
	found := false
	for _, w := range words {
		if w == string(MonitorCurrent) {
			found = true
		}
	}
	if !found {
		t.Errorf("reserved words %v do not include %q", words, MonitorCurrent)
	}
}
