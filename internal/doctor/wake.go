package doctor

import (
	"fmt"
	"strings"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/wake"
)

// wakeInstallHint is the one command that fixes a missing detector. Named
// once, so the doctor line, the daemon's startup warning, and the
// configuration reference cannot drift apart.
const wakeInstallHint = "Install a wake-word detector: scripts/setup-wake.sh\n" +
	"(or set activation.wake_command to your own helper — see docs/configuration.md)"

// checkWakeWord reports background listening (ADR 0024): whether it is on,
// whether its detector is installed, and — when jarvixd is up — what the
// microphone is actually doing right now.
//
// A missing detector is a Warn rather than a Fail on purpose. The feature is
// additive: push-to-talk is untouched, the daemon boots, and everything else
// works. Failing `jarvix doctor` outright would say the installation is
// broken when what has happened is that one optional thing is not installed
// yet — the same call artifact rendering makes about a missing mermaid-cli.
func checkWakeWord(cfg config.Config, paths config.Paths) Result {
	const name = "background listening"
	if !cfg.Activation.WakeWordEnabled() {
		return Result{Status: OK, Name: name,
			Detail: fmt.Sprintf("off (activation.mode = %q); the microphone is only opened while you hold the chord",
				cfg.Activation.Mode),
			Fix: ""}
	}
	if err := wake.DetectorReady(cfg.Activation.WakeCommand); err != nil {
		return Result{Status: Warn, Name: name,
			Detail: fmt.Sprintf("enabled, but the detector is not installed (%s) — Jarvix is running push-to-talk only", err.Error()),
			Fix:    wakeInstallHint}
	}

	settings := fmt.Sprintf("word %q, sensitivity %.2f, submits after %dms of silence, %dms kept before the wake word",
		cfg.WakeDetectorWord(), cfg.Activation.WakeSensitivity,
		cfg.Activation.EndpointSilenceMs, cfg.Activation.WakeRingMs)

	// The daemon is the only thing that knows whether a capture process is
	// actually running — that is not visible on disk, and it is the fact a
	// user checking on their microphone came here for.
	client, err := ipc.Dial(paths.Socket)
	if err != nil {
		return Result{Status: OK, Name: name,
			Detail: settings + "; jarvixd is not running, so nothing is capturing"}
	}
	defer func() { _ = client.Close() }()
	var status map[string]any
	if err := client.Call("wake.status", nil, &status); err != nil {
		return Result{Status: Warn, Name: name,
			Detail: settings + "; jarvixd did not answer: " + err.Error()}
	}

	running, _ := status["running"].(bool)
	muted, _ := status["muted"].(bool)
	capturing, _ := status["capturing"].(bool)
	reason, _ := status["last_reason"].(string)
	detector, _ := status["detector"].(string)
	pid := int(jsonNumber(status["pid"]))
	rss := jsonNumber(status["detector_rss_mb"])
	restarts := jsonNumber(status["restarts"])

	var live []string
	switch {
	case muted:
		live = append(live, "muted — no capture process is running")
	case capturing:
		live = append(live, fmt.Sprintf("listening now (pw-record pid %d)", pid))
	case !running:
		live = append(live, "not running in the daemon — it was started before the detector was installed; restart jarvixd")
	default:
		live = append(live, "enabled, but capture is not up")
	}
	if detector != "" {
		entry := "detector " + detector
		if rss > 0 {
			entry += fmt.Sprintf(", %.0f MB", rss)
		}
		live = append(live, entry)
	}
	if restarts > 0 {
		live = append(live, fmt.Sprintf("%.0f capture restarts", restarts))
	}
	detail := settings + "; " + strings.Join(live, "; ")

	switch {
	case !running:
		return Result{Status: Warn, Name: name, Detail: detail,
			Fix: "Restart the daemon so it picks the detector up: systemctl --user restart jarvixd"}
	case !muted && !capturing && reason != "":
		return Result{Status: Warn, Name: name, Detail: detail + " — last stopped: " + reason,
			Fix: "Check the microphone (wpctl status), then: journalctl --user -u jarvixd -g wake"}
	default:
		return Result{Status: OK, Name: name, Detail: detail}
	}
}

// checkWakeInstalled is the offline half of checkWakeWord, for the settings
// screen: is the detector there. It deliberately does not dial the daemon —
// the daemon is what calls this, and a socket call to itself from inside a
// request handler is a deadlock waiting for someone to write it.
func checkWakeInstalled(cfg config.Config, _ config.Paths) Result {
	const name = "wake-word detector"
	if !cfg.Activation.WakeWordEnabled() {
		return Result{Status: OK, Name: name,
			Detail: "not needed (activation.mode = " + cfg.Activation.Mode + ")"}
	}
	if err := wake.DetectorReady(cfg.Activation.WakeCommand); err != nil {
		return Result{Status: Warn, Name: name, Detail: err.Error(), Fix: wakeInstallHint}
	}
	return Result{Status: OK, Name: name,
		Detail: strings.Join(cfg.Activation.WakeCommand, " ")}
}
