package tools

import (
	"errors"
	"strings"
	"testing"
)

// The refusal events behind the activity feed (issue #70). The day the
// feature was asked for, "launch refused: firefox is not installed" existed
// in exactly two places — the journal and the model's tool result — and the
// user's surfaces heard nothing. These tests pin the third place: the bus.

func TestLaunchRefusalPublishesTheReason(t *testing.T) {
	stubApp(t) // an empty PATH: nothing is installed
	h := newHarness(t)
	out := h.run(t, LaunchAppToolName, map[string]any{"app": "firefox"})
	if !strings.Contains(out, "cannot be started") {
		t.Fatalf("launch = %q, want a refusal", out)
	}
	refusals := h.firedRefusals()
	if len(refusals) != 1 {
		t.Fatalf("refusals = %v, want exactly one", refusals)
	}
	if refusals[0] != "launch:firefox:it is not installed" {
		t.Errorf("refusal = %q, want the resolver's own reason", refusals[0])
	}
	if got := h.firedEvents(); len(got) != 0 {
		t.Errorf("a refused launch must not also publish an action: %v", got)
	}
}

// A near-match refusal (#71) is still a refusal — nothing launched — so the
// feed's row carries both facts in one clause: not installed, but this is.
func TestLaunchNearMatchRefusalPublishesTheSuggestion(t *testing.T) {
	stubApp(t, "chromium")
	h := newHarness(t)
	out := h.run(t, LaunchAppToolName, map[string]any{"app": "chrome"})
	if !strings.Contains(out, "chromium is") {
		t.Fatalf("launch = %q, want the near-match suggestion", out)
	}
	refusals := h.firedRefusals()
	if len(refusals) != 1 || refusals[0] != "launch:chrome:it is not installed, but chromium is" {
		t.Errorf("refusals = %v, want the reason with its suggestion", refusals)
	}
	if got := h.firedEvents(); len(got) != 0 {
		t.Errorf("a refused launch must not also publish an action: %v", got)
	}
}

func TestLaunchFailurePublishesAGenericReason(t *testing.T) {
	stubApp(t, "firefox")
	h := newHarness(t)
	h.install(t, "firefox", "Exec=firefox")
	// The launcher's error may carry paths and exec detail — operator
	// material for the journal, not the bus.
	h.launcher.err = errors.New("fork/exec /usr/bin/firefox: permission denied")
	out := h.run(t, LaunchAppToolName, map[string]any{"app": "firefox"})
	if !strings.Contains(out, "would not start") {
		t.Fatalf("launch = %q, want a failure sentence", out)
	}
	refusals := h.firedRefusals()
	if len(refusals) != 1 || refusals[0] != "launch:firefox:it would not start" {
		t.Fatalf("refusals = %v, want the generic reason", refusals)
	}
	if strings.Contains(refusals[0], "/usr/bin") {
		t.Errorf("the launcher's diagnostics leaked onto the bus: %q", refusals[0])
	}
}

func TestDispatchRefusalPublishesWithoutAddresses(t *testing.T) {
	h := newHarness(t)
	h.comp.FailAction = errors.New("hyprctl: dispatch failed for address 0x2")
	out := h.run(t, FocusWindowToolName, map[string]any{"window": "github"})
	if !strings.Contains(out, "would not focus") {
		t.Fatalf("focus = %q, want the dispatch refusal", out)
	}
	refusals := h.firedRefusals()
	if len(refusals) != 1 {
		t.Fatalf("refusals = %v, want exactly one", refusals)
	}
	if !strings.HasPrefix(refusals[0], "focus:") ||
		!strings.HasSuffix(refusals[0], ":the window manager refused") {
		t.Errorf("refusal = %q, want verb, human window name, and the generic reason", refusals[0])
	}
	// Window addresses are compositor internals and never travel (ADR 0022);
	// neither do the compositor's diagnostics.
	if strings.Contains(refusals[0], "0x") || strings.Contains(refusals[0], "hyprctl") {
		t.Errorf("refusal leaks operator material: %q", refusals[0])
	}
}

// A successful action publishes no refusal — the two feeds cannot both fire
// for one call.
func TestSuccessfulActionPublishesNoRefusal(t *testing.T) {
	h := newHarness(t)
	out := h.run(t, FocusWindowToolName, map[string]any{"window": "github"})
	if !strings.Contains(out, "Switched to") {
		t.Fatalf("focus = %q, want success", out)
	}
	if refusals := h.firedRefusals(); len(refusals) != 0 {
		t.Errorf("refusals = %v, want none", refusals)
	}
}
