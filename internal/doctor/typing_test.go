package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/config"
)

// No test here presses a key. The healthy path runs a stub that records
// nothing and exits 0, so what is asserted is that doctor asked the right
// question — never that a keystroke arrived somewhere.

func enabledTyping(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Tools.Typing.Enable = true
	return cfg
}

// TestCheckTypingWhenDisabled: the default. Doctor says so plainly rather than
// staying silent, because "can it type?" is a question a user should be able
// to answer by running one command.
func TestCheckTypingWhenDisabled(t *testing.T) {
	got := checkTyping(config.Default(), config.Paths{})
	if got.Status != OK || !strings.Contains(got.Detail, "disabled") {
		t.Errorf("result = %+v", got)
	}
}

func TestCheckTypingWarnsWithoutWtype(t *testing.T) {
	stubBinaries(t) // nothing installed
	got := checkTyping(enabledTyping(t), config.Paths{})
	// A Warn, never a Fail: the typing tools say they have no way to send
	// keystrokes and everything else Jarvix does is untouched.
	if got.Status != Warn {
		t.Fatalf("status = %v, want Warn", got.Status)
	}
	if !strings.Contains(got.Detail, "wtype") || !strings.Contains(got.Fix, "tools.typing.enable=false") {
		t.Errorf("result = %+v, want the missing piece named and a way out", got)
	}
}

func TestCheckTypingWarnsOutsideAWaylandSession(t *testing.T) {
	stubWtype(t, "#!/bin/sh\nexit 0\n")
	t.Setenv("WAYLAND_DISPLAY", "")
	got := checkTyping(enabledTyping(t), config.Paths{})
	if got.Status != Warn || !strings.Contains(got.Detail, "no Wayland session") {
		t.Errorf("result = %+v", got)
	}
	if !strings.Contains(got.Fix, "import-environment WAYLAND_DISPLAY") {
		t.Errorf("fix = %q, want the systemd environment hint", got.Fix)
	}
}

// TestCheckTypingWarnsWhenTheCompositorRefuses: wtype is installed and there
// is a session, but the compositor does not implement the virtual-keyboard
// protocol. Worth saying once, because it will never work here and the user
// would otherwise discover it by watching nothing happen.
func TestCheckTypingWarnsWhenTheCompositorRefuses(t *testing.T) {
	stubWtype(t, "#!/bin/sh\necho 'compositor does not support the virtual keyboard protocol' >&2\nexit 1\n")
	t.Setenv("WAYLAND_DISPLAY", "wayland-0")
	got := checkTyping(enabledTyping(t), config.Paths{})
	if got.Status != Warn || !strings.Contains(got.Detail, "virtual keyboard") {
		t.Errorf("result = %+v", got)
	}
}

// TestCheckTypingProbesWithoutTyping is the one that matters: the diagnostic
// must be able to answer "would typing work?" without typing into whatever the
// user has open. The stub records its argv, and the argv must be the empty
// payload.
func TestCheckTypingProbesWithoutTyping(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "argv.log")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >> " + log + "; done\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "wtype"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("WAYLAND_DISPLAY", "wayland-0")

	got := checkTyping(enabledTyping(t), config.Paths{})
	if got.Status != OK {
		t.Fatalf("result = %+v", got)
	}
	if !strings.Contains(got.Detail, "confirmed before it happens") {
		t.Errorf("detail = %q, want it to say the capability is gated", got.Detail)
	}
	recorded, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("read argv: %v", err)
	}
	if string(recorded) != "--\n\n" {
		t.Fatalf("probe argv = %q, want an empty payload after `--` — the probe must type nothing",
			string(recorded))
	}
}

func stubWtype(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "wtype"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}
