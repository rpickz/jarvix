package doctor

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/intent"
)

// checkIntentBinaries verifies that the built-in intent table can actually do
// what it promises.
//
// It asks two questions, and the second one is the lesson of #47. Are the
// programs installed? — a missing wpctl is a Warn rather than a Fail, since
// the assistant is unaffected and every other intent still works, but the
// failure mode without the check is discovering it by saying "mute" and
// hearing an apology. And *can a dispatch reach a compositor*? — because
// "workspace four" and "open a terminal" go through the compositor seam (ADR
// 0022), and an installed hyprctl with nothing behind it does precisely
// nothing while looking perfectly healthy from here. Reporting the binaries
// as present would then be reporting the wrong fact: the thing the user wants
// to know is whether talking to their desktop works.
func checkIntentBinaries(cfg config.Config, _ config.Paths) Result {
	const name = "intent commands available"
	if !cfg.Intents.Enabled {
		return Result{Status: OK, Name: "deterministic intents",
			Detail: "disabled ([intents] enabled = false)"}
	}
	binaries := intent.BuiltinBinaries(cfg.Intents.Terminal)
	var missing []string
	for _, bin := range binaries {
		if _, err := exec.LookPath(bin); err != nil {
			missing = append(missing, bin)
		}
	}
	if len(missing) > 0 {
		return Result{Status: Warn, Name: name,
			Detail: "not found in PATH: " + strings.Join(missing, ", "),
			Fix: "Install them: sudo pacman -S wireplumber (wpctl), hyprland (hyprctl).\n" +
				"Set [intents] terminal to the terminal you actually use."}
	}
	// Describe is the seam's own probe: it names the compositor and the
	// dispatch dialect it discovered, which is exactly the pair that decides
	// whether these two intents work. Reusing it also means doctor can never
	// disagree with the daemon about how a dispatch is written.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	described, err := (&desktop.Hyprland{Timeout: 2 * time.Second}).Describe(ctx)
	if err != nil {
		return Result{Status: Warn, Name: name,
			Detail: strings.Join(binaries, ", ") + " are installed, but no Hyprland compositor " +
				`answered: "workspace four" and "open a terminal" will do nothing`,
			Fix: "Run this inside a Hyprland session. If jarvixd is started by systemd, it needs the\n" +
				"session environment: systemctl --user import-environment HYPRLAND_INSTANCE_SIGNATURE WAYLAND_DISPLAY"}
	}
	return Result{Status: OK, Name: name,
		Detail: strings.Join(binaries, ", ") + "; " + described}
}
