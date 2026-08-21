package desktop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Success paths via stub binaries — no notification daemon or Omarchy shell
// is ever required, matching the stub-binary pattern used for PipeWire and
// the speech engines.

func writeStub(t *testing.T, name, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNotifySendReturnsInvokedAction(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NOTIFY_STUB_DIR", dir)
	bin := writeStub(t, "notify-send", `#!/bin/sh
printf '%s\n' "$@" > "$NOTIFY_STUB_DIR/notify.args"
printf 'default\n'
`)
	n := &NotifySend{Binary: bin}
	invoked, err := n.Send(context.Background(), Notification{
		Summary: "Jarvix", Body: "done",
		Actions: []Action{{ID: DefaultActionID, Label: "Open"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The invoked action comes back trimmed from notify-send's stdout.
	if invoked != DefaultActionID {
		t.Errorf("invoked = %q", invoked)
	}
	args, err := os.ReadFile(filepath.Join(dir, "notify.args"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--app-name=Jarvix", "--action=default=Open", "--", "Jarvix", "done"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("argv %q missing %q", args, want)
		}
	}
}

func TestWindowClientOpenAndToggleCallThePlugin(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SHELL_STUB_DIR", dir)
	bin := writeStub(t, "omarchy-shell", `#!/bin/sh
printf '%s\n' "$@" >> "$SHELL_STUB_DIR/shell.args"
`)
	w := &WindowClient{Binary: bin}
	if err := w.Open(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := w.Toggle(context.Background()); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(filepath.Join(dir, "shell.args"))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(string(args))
	if got != "jarvix\nopenWindow\njarvix\ntoggleWindow" {
		t.Errorf("calls = %q", got)
	}
}

func TestWindowClientSurfacesShellFailureOutput(t *testing.T) {
	bin := writeStub(t, "omarchy-shell", `#!/bin/sh
echo "plugin not loaded" >&2
exit 1
`)
	w := &WindowClient{Binary: bin}
	err := w.Open(context.Background())
	if err == nil || !strings.Contains(err.Error(), "plugin not loaded") {
		t.Errorf("err = %v, want the shell's own diagnostics", err)
	}
}
