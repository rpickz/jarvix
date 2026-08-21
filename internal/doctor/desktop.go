package doctor

import (
	"context"
	"os/exec"
	"strconv"
	"time"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/desktop"
)

// checkWindowControl reports whether Jarvix can act on the user's windows
// (ADR 0022).
//
// It probes rather than merely looking for the binary, because the two
// failures look identical from the user's chair and have different fixes: no
// hyprctl means "install it", while hyprctl that cannot reach a compositor
// means "these tools do nothing in this session" — a daemon started outside
// the graphical session, or a machine that is not running Hyprland at all.
//
// Never a Fail. The window tools degrade to one spoken sentence about being
// unable to see the desktop, and everything else Jarvix does is unaffected.
func checkWindowControl(cfg config.Config, _ config.Paths) Result {
	const name = "window control"
	if !cfg.Tools.Desktop {
		return Result{Status: OK, Name: name, Detail: "disabled ([tools] desktop = false)"}
	}
	if _, err := exec.LookPath("hyprctl"); err != nil {
		return Result{Status: Warn, Name: name,
			Detail: "hyprctl not found in PATH; Jarvix cannot focus, move, close or list windows",
			Fix: "Install Hyprland (sudo pacman -S hyprland), or switch the tools off:\n" +
				"jarvix config set tools.desktop=false"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	compositor := &desktop.Hyprland{Timeout: 2 * time.Second}
	described, err := compositor.Describe(ctx)
	if err != nil {
		return Result{Status: Warn, Name: name,
			Detail: "hyprctl found, but no Hyprland compositor answered",
			Fix: "Run this inside a Hyprland session. If jarvixd is started by systemd, it needs the\n" +
				"session environment: systemctl --user import-environment HYPRLAND_INSTANCE_SIGNATURE WAYLAND_DISPLAY"}
	}
	windows, err := compositor.Windows(ctx)
	if err != nil {
		return Result{Status: Warn, Name: name, Detail: described + ", but the window list could not be read",
			Fix: "Check hyprctl clients -j runs as your user."}
	}
	return Result{Status: OK, Name: name,
		Detail: described + "; " + pluralWindows(len(windows)) + " open"}
}

func pluralWindows(n int) string {
	if n == 1 {
		return "1 window"
	}
	return strconv.Itoa(n) + " windows"
}
