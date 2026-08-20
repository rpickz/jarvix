package doctor

import (
	"strings"
	"testing"
)

func TestFindBindConflictsCleanInstall(t *testing.T) {
	binds := `[
		{"modmask":64,"key":"V","release":false,"description":"Universal paste","arg":"exec paste"},
		{"modmask":72,"key":"V","release":false,"description":"Talk to Jarvix (hold)","arg":"exec, jarvix ptt start"},
		{"modmask":72,"key":"V","release":true,"description":"Submit to Jarvix","arg":"exec, jarvix ptt stop"},
		{"modmask":72,"key":"Escape","release":false,"description":"Cancel Jarvix","arg":"exec, jarvix cancel"}
	]`
	conflicts, installed, err := findBindConflicts([]byte(binds))
	if err != nil {
		t.Fatal(err)
	}
	if !installed {
		t.Error("jarvix binds should be detected as installed")
	}
	if len(conflicts) != 0 {
		t.Errorf("unexpected conflicts: %v", conflicts)
	}
}

func TestFindBindConflictsSameChordDifferentPhaseIsFine(t *testing.T) {
	// Press and release on the same chord is the push-to-talk pair, not a
	// conflict.
	binds := `[
		{"modmask":72,"key":"V","release":false,"description":"Talk to Jarvix (hold)","arg":""},
		{"modmask":72,"key":"V","release":true,"description":"Submit to Jarvix","arg":""}
	]`
	conflicts, _, err := findBindConflicts([]byte(binds))
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 0 {
		t.Errorf("press/release pair flagged as conflict: %v", conflicts)
	}
}

func TestFindBindConflictsDetectsCollision(t *testing.T) {
	binds := `[
		{"modmask":72,"key":"V","release":false,"description":"Talk to Jarvix (hold)","arg":""},
		{"modmask":72,"key":"v","release":false,"description":"Omarchy new feature","arg":"exec something"}
	]`
	conflicts, installed, err := findBindConflicts([]byte(binds))
	if err != nil {
		t.Fatal(err)
	}
	if !installed {
		t.Error("installed should be true")
	}
	if len(conflicts) != 1 || !strings.Contains(conflicts[0], "Omarchy new feature") {
		t.Errorf("conflicts = %v", conflicts)
	}
}

func TestFindBindConflictsNotInstalled(t *testing.T) {
	binds := `[{"modmask":64,"key":"V","release":false,"description":"Universal paste","arg":""}]`
	conflicts, installed, err := findBindConflicts([]byte(binds))
	if err != nil {
		t.Fatal(err)
	}
	if installed || len(conflicts) != 0 {
		t.Errorf("installed=%v conflicts=%v", installed, conflicts)
	}
}

func TestFindBindConflictsBadJSON(t *testing.T) {
	if _, _, err := findBindConflicts([]byte("nope")); err == nil {
		t.Error("expected parse error")
	}
}
