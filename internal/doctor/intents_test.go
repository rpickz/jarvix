package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/intent"
)

// intentStubs puts wpctl, the configured terminal, and a scripted hyprctl on
// an otherwise bare PATH, so the check can be run through every outcome
// without a compositor anywhere.
func intentStubs(t *testing.T, hyprctl string) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range intent.BuiltinBinaries("") {
		script := "#!/bin/sh\n"
		if name == "hyprctl" {
			script = hyprctl
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
}

func TestCheckIntentBinariesWhenDisabled(t *testing.T) {
	cfg := config.Default()
	cfg.Intents.Enabled = false
	if got := checkIntentBinaries(cfg, config.Paths{}); got.Status != OK ||
		!strings.Contains(got.Detail, "disabled") {
		t.Errorf("result = %+v", got)
	}
}

func TestCheckIntentBinariesWarnsAboutWhatIsMissing(t *testing.T) {
	stubBinaries(t) // nothing installed
	got := checkIntentBinaries(config.Default(), config.Paths{})
	if got.Status != Warn || !strings.Contains(got.Detail, "wpctl") {
		t.Errorf("result = %+v, want the missing programs named", got)
	}
}

// TestCheckIntentBinariesWarnsWhenNoCompositorAnswers is the doctor half of
// #47: the programs being installed says nothing about whether "workspace
// four" works, because the dispatch has to reach a compositor. Reporting the
// binaries as present here would be reporting the wrong fact.
func TestCheckIntentBinariesWarnsWhenNoCompositorAnswers(t *testing.T) {
	intentStubs(t, "#!/bin/sh\nexit 1\n")
	got := checkIntentBinaries(config.Default(), config.Paths{})
	if got.Status != Warn {
		t.Fatalf("status = %v, want Warn when nothing answers", got.Status)
	}
	if !strings.Contains(got.Detail, "no Hyprland compositor answered") ||
		!strings.Contains(got.Detail, "workspace four") {
		t.Errorf("detail = %q, want it to name what stops working", got.Detail)
	}
	if !strings.Contains(got.Fix, "import-environment") {
		t.Errorf("fix = %q, want the systemd session-environment remedy", got.Fix)
	}
}

// A healthy check reports the dialect, because that is the fact that decides
// how a dispatch is written — and the one that was silently wrong before.
func TestCheckIntentBinariesReportsTheDispatchDialect(t *testing.T) {
	intentStubs(t, `#!/bin/sh
case "$1" in
  version) printf '{"version":"0.56.2"}\n' ;;
  dispatch) printf 'ok\n' ;;
esac
`)
	got := checkIntentBinaries(config.Default(), config.Paths{})
	if got.Status != OK {
		t.Fatalf("result = %+v", got)
	}
	for _, want := range []string{"wpctl", "Hyprland 0.56.2", "lua dispatch"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("detail = %q, want %q", got.Detail, want)
		}
	}
}
