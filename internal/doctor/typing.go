package doctor

import (
	"context"
	"os"
	"os/exec"
	"time"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/desktop"
)

// checkTyping reports whether Jarvix can type on the user's behalf (ADR 0023).
//
// It probes rather than merely looking for the binary, because three failures
// look identical from the user's chair and have three different fixes: no
// wtype means "install it"; wtype with no Wayland session means "the daemon
// was started outside the graphical session"; and wtype in a session whose
// compositor refuses the virtual-keyboard protocol means "this will never work
// here", which is worth being told once rather than discovered every time.
//
// The probe types the empty string, which is the whole trick: wtype still
// connects to the display and binds the protocol, so success proves both, and
// nothing is pressed. A diagnostic that typed into whatever the user had open
// would be a worse bug than the one it was checking for.
//
// Never a Fail. The typing tools degrade to one spoken sentence about having
// no way to send keystrokes, and everything else Jarvix does is unaffected.
func checkTyping(cfg config.Config, _ config.Paths) Result {
	const name = "typing"
	if !cfg.Tools.Typing.Enable {
		return Result{Status: OK, Name: name, Detail: "disabled ([tools.typing] enable = false)"}
	}
	binary := cfg.Tools.Typing.Binary
	if binary == "" {
		binary = "wtype"
	}
	if _, err := exec.LookPath(binary); err != nil {
		return Result{Status: Warn, Name: name,
			Detail: binary + " not found in PATH; Jarvix cannot type anything",
			Fix: "Install wtype (sudo pacman -S wtype), or switch typing off:\n" +
				"jarvix config set tools.typing.enable=false"}
	}
	if os.Getenv("WAYLAND_DISPLAY") == "" {
		return Result{Status: Warn, Name: name,
			Detail: binary + " found, but there is no Wayland session to type into",
			Fix: "Run this inside your Wayland session. If jarvixd is started by systemd, it needs the\n" +
				"session environment: systemctl --user import-environment WAYLAND_DISPLAY"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	keyboard := &desktop.Wtype{Binary: cfg.Tools.Typing.Binary, Timeout: 2 * time.Second}
	described, err := keyboard.Describe(ctx)
	if err != nil {
		return Result{Status: Warn, Name: name,
			Detail: binary + " found, but the compositor would not grant a virtual keyboard",
			Fix: "This compositor does not implement the virtual-keyboard protocol. Typing cannot work\n" +
				"here; switch it off with: jarvix config set tools.typing.enable=false"}
	}
	return Result{Status: OK, Name: name,
		Detail: described + "; enabled, and every use is confirmed before it happens"}
}
