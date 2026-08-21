package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/config"
)

func TestCheckWindowControlWhenDisabled(t *testing.T) {
	cfg := config.Default()
	cfg.Tools.Desktop = false
	got := checkWindowControl(cfg, config.Paths{})
	if got.Status != OK || !strings.Contains(got.Detail, "disabled") {
		t.Errorf("result = %+v", got)
	}
}

func TestCheckWindowControlWarnsWithoutHyprctl(t *testing.T) {
	stubBinaries(t) // nothing installed
	got := checkWindowControl(config.Default(), config.Paths{})
	// A Warn, never a Fail: the window tools say they cannot see the desktop
	// and everything else Jarvix does is untouched.
	if got.Status != Warn {
		t.Fatalf("status = %v, want Warn", got.Status)
	}
	if !strings.Contains(got.Detail, "hyprctl") || !strings.Contains(got.Fix, "tools.desktop=false") {
		t.Errorf("result = %+v, want the missing piece named and a way out", got)
	}
}

func TestCheckWindowControlWarnsWhenNoCompositorAnswers(t *testing.T) {
	// hyprctl is installed but there is no compositor behind it — the
	// systemd-started-outside-the-session case.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hyprctl"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	got := checkWindowControl(config.Default(), config.Paths{})
	if got.Status != Warn || !strings.Contains(got.Detail, "no Hyprland compositor answered") {
		t.Errorf("result = %+v", got)
	}
}

func TestCheckWindowControlReportsAHealthyCompositor(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
case "$1" in
  version) printf '{"version":"0.56.2"}\n' ;;
  clients) printf '[{"address":"0xa","class":"firefox","title":"t","mapped":true,"workspace":{"id":1,"name":"1"}}]\n' ;;
  dispatch) printf 'ok\n' ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "hyprctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	got := checkWindowControl(config.Default(), config.Paths{})
	if got.Status != OK {
		t.Fatalf("result = %+v", got)
	}
	for _, want := range []string{"Hyprland 0.56.2", "1 window"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("detail = %q, want %q", got.Detail, want)
		}
	}
}
