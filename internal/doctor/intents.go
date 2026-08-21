package doctor

import (
	"os/exec"
	"strings"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/intent"
)

// checkIntentBinaries verifies the programs the built-in intent table drives.
// A missing wpctl is a Warn rather than a Fail: the assistant is unaffected
// and every other intent still works — but the failure mode without this
// check is discovering it by saying "mute" and hearing an apology, which is
// exactly the sort of thing doctor exists to pre-empt.
func checkIntentBinaries(cfg config.Config, _ config.Paths) Result {
	if !cfg.Intents.Enabled {
		return Result{Status: OK, Name: "deterministic intents",
			Detail: "disabled ([intents] enabled = false)"}
	}
	var missing []string
	for _, bin := range intent.BuiltinBinaries(cfg.Intents.Terminal) {
		if _, err := exec.LookPath(bin); err != nil {
			missing = append(missing, bin)
		}
	}
	if len(missing) > 0 {
		return Result{Status: Warn, Name: "intent commands available",
			Detail: "not found in PATH: " + strings.Join(missing, ", "),
			Fix: "Install them: sudo pacman -S wireplumber (wpctl), hyprland (hyprctl).\n" +
				"Set [intents] terminal to the terminal you actually use."}
	}
	return Result{Status: OK, Name: "intent commands available",
		Detail: strings.Join(intent.BuiltinBinaries(cfg.Intents.Terminal), ", ")}
}
