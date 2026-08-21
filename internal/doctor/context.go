package doctor

import (
	"os/exec"
	"sort"
	"strings"

	"github.com/rpickz/jarvix/internal/config"
)

// checkContextSources reports what Jarvix may look at and whether it can.
//
// The check exists for two audiences at once. For the user who wants context
// and does not get it, it names the missing binary instead of leaving them to
// conclude the feature is broken. For the user who did not realise Jarvix
// looks at anything, it is the one place that says so plainly, next to the
// switch that turns it off — which is why it lists the enabled sources even
// when everything is installed and healthy.
//
// A missing binary is a Warn, never a Fail: gathering degrades to no context,
// and the assistant is otherwise untouched.
func checkContextSources(cfg config.Config, _ config.Paths) Result {
	enabled := cfg.Context.EnabledSources()
	if len(enabled) == 0 {
		return Result{Status: OK, Name: "desktop context",
			Detail: "disabled ([context] window/selection/clipboard = false)"}
	}

	// Which binary each enabled source needs. wl-paste serves two sources, so
	// the map is keyed by binary to report it once.
	needs := map[string][]string{}
	for _, source := range enabled {
		binary := "wl-paste"
		if source == "window" {
			binary = "hyprctl"
		}
		needs[binary] = append(needs[binary], source)
	}
	binaries := make([]string, 0, len(needs))
	for binary := range needs {
		binaries = append(binaries, binary)
	}
	sort.Strings(binaries) // deterministic output

	var missing []string
	for _, binary := range binaries {
		if _, err := exec.LookPath(binary); err != nil {
			missing = append(missing, binary+" (for "+strings.Join(needs[binary], ", ")+")")
		}
	}
	detail := "sees " + strings.Join(enabled, ", ")
	if len(missing) > 0 {
		return Result{Status: Warn, Name: "desktop context",
			Detail: detail + "; not found in PATH: " + strings.Join(missing, ", "),
			Fix: "Install them: sudo pacman -S wl-clipboard (wl-paste), hyprland (hyprctl).\n" +
				"Or switch the source off: jarvix config set context.clipboard=false"}
	}
	return Result{Status: OK, Name: "desktop context",
		Detail: detail + " (" + strings.Join(binaries, ", ") + ")"}
}
