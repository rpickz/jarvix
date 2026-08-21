package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/config"
)

// stubBinaries puts empty executables on an otherwise bare PATH.
func stubBinaries(t *testing.T, names ...string) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
}

func TestCheckContextSourcesReportsWhatIsEnabled(t *testing.T) {
	stubBinaries(t, "hyprctl", "wl-paste")
	cfg := config.Default()
	cfg.Context = config.Context{Window: true, Selection: true, MaxChars: 100, TimeoutMs: 100}

	got := checkContextSources(cfg, config.Paths{})
	if got.Status != OK {
		t.Fatalf("status = %v, detail %q", got.Status, got.Detail)
	}
	// Listing the sources is the point: this is where a user finds out what
	// Jarvix is allowed to look at.
	for _, want := range []string{"window", "selection", "hyprctl", "wl-paste"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("detail = %q, want it to mention %q", got.Detail, want)
		}
	}
	if strings.Contains(got.Detail, "clipboard") {
		t.Errorf("detail = %q, want no mention of the disabled clipboard", got.Detail)
	}
}

func TestCheckContextSourcesWarnsAboutMissingBinaries(t *testing.T) {
	stubBinaries(t) // nothing installed
	cfg := config.Default()
	cfg.Context = config.Context{Window: true, Clipboard: true, MaxChars: 100, TimeoutMs: 100}

	got := checkContextSources(cfg, config.Paths{})
	// A Warn, never a Fail: gathering degrades to no context and the
	// assistant is untouched.
	if got.Status != Warn {
		t.Fatalf("status = %v, want Warn", got.Status)
	}
	for _, want := range []string{"hyprctl", "wl-paste", "clipboard"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("detail = %q, want it to name %q", got.Detail, want)
		}
	}
	if !strings.Contains(got.Fix, "wl-clipboard") {
		t.Errorf("fix = %q, want an install command", got.Fix)
	}
}

func TestCheckContextSourcesWhenDisabled(t *testing.T) {
	stubBinaries(t) // nothing installed, and nothing needed
	cfg := config.Default()
	cfg.Context = config.Context{MaxChars: 100, TimeoutMs: 100}

	got := checkContextSources(cfg, config.Paths{})
	if got.Status != OK || !strings.Contains(got.Detail, "disabled") {
		t.Errorf("result = %+v, want a healthy 'disabled'", got)
	}
}
