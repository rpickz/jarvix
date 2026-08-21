package desktop

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// The three shipped gatherers. Each is one short-lived subprocess around a
// binary the desktop already has — hyprctl for the compositor, wl-paste for
// the Wayland data device — rather than a Wayland protocol client or a
// hyprland-ipc library. That is ADR 0002's trade again: no new dependencies,
// crash isolation, and a missing binary degrades to "no context" instead of
// to a broken daemon.
//
// The argv builders are separate functions so tests can assert on exactly
// what would run without a compositor, a Wayland session, or anything on
// screen — the whole package must stay testable on a headless CI runner.

// ActiveWindow reports the focused window as "<app> — <title>", read from
// `hyprctl activewindow -j`.
type ActiveWindow struct {
	// Binary overrides the hyprctl executable (tests). Empty means "hyprctl"
	// from PATH.
	Binary string
}

// Source implements Gatherer.
func (a *ActiveWindow) Source() Source { return SourceWindow }

// Gather implements Gatherer.
func (a *ActiveWindow) Gather(ctx context.Context) (string, error) {
	out, err := runCapture(ctx, binaryOr(a.Binary, "hyprctl"), activeWindowArgs()...)
	if err != nil {
		return "", err
	}
	return parseActiveWindow(out)
}

// activeWindowArgs builds the hyprctl invocation. JSON rather than the human
// format: the human format is a display surface that may be reworded, while
// -j is the documented machine interface.
func activeWindowArgs() []string { return []string{"activewindow", "-j"} }

// parseActiveWindow turns hyprctl's JSON into one line for the model. With
// nothing focused hyprctl prints an empty object, which is "no context" — not
// an error, because an empty desktop is a perfectly ordinary state.
func parseActiveWindow(out string) (string, error) {
	var win struct {
		Class string `json:"class"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal([]byte(out), &win); err != nil {
		return "", fmt.Errorf("hyprctl activewindow: %w", err)
	}
	class, title := strings.TrimSpace(win.Class), strings.TrimSpace(win.Title)
	switch {
	case class != "" && title != "" && class != title:
		return class + " — " + title, nil
	case title != "":
		return title, nil
	default:
		return class, nil
	}
}

// PrimarySelection reads the Wayland primary selection — text the user has
// highlighted, which they have not "copied" in any deliberate sense but are
// visibly looking at. "Summarise what I've selected" is this source.
type PrimarySelection struct {
	// Binary overrides the wl-paste executable (tests). Empty means
	// "wl-paste" from PATH.
	Binary string
}

// Source implements Gatherer.
func (p *PrimarySelection) Source() Source { return SourceSelection }

// Gather implements Gatherer.
func (p *PrimarySelection) Gather(ctx context.Context) (string, error) {
	return runCapture(ctx, binaryOr(p.Binary, "wl-paste"), primarySelectionArgs()...)
}

// Clipboard reads the regular clipboard. Off by default: it is the one source
// whose contents the user may have put there for somewhere else entirely.
type Clipboard struct {
	// Binary overrides the wl-paste executable (tests). Empty means
	// "wl-paste" from PATH.
	Binary string
}

// Source implements Gatherer.
func (c *Clipboard) Source() Source { return SourceClipboard }

// Gather implements Gatherer.
func (c *Clipboard) Gather(ctx context.Context) (string, error) {
	return runCapture(ctx, binaryOr(c.Binary, "wl-paste"), clipboardArgs()...)
}

// primarySelectionArgs and clipboardArgs differ by one flag, which is the
// whole distinction between the two sources — and the reason the argv is
// asserted in tests: getting it wrong would silently read the wrong one.
//
// `--type text` asks for the first text/* offer, so a copied image or file
// list produces nothing rather than binary noise, and `--no-newline` stops
// wl-paste appending a newline the user never selected.
func primarySelectionArgs() []string {
	return []string{"--primary", "--no-newline", "--type", "text"}
}

func clipboardArgs() []string { return []string{"--no-newline", "--type", "text"} }

// binaryOr resolves a configured override against the default name.
func binaryOr(configured, fallback string) string {
	if strings.TrimSpace(configured) != "" {
		return configured
	}
	return fallback
}

// runCapture executes one gatherer subprocess and returns its stdout.
//
// The shape mirrors the advisor runner (internal/tools/advisor.go) for the
// same reasons, minus the environment scrubbing: these are the user's own
// compositor tools and they need the Wayland environment (WAYLAND_DISPLAY,
// HYPRLAND_INSTANCE_SIGNATURE) to work at all, unlike a third-party assistant
// CLI that must never see a credential.
//
//   - No shell: argv goes straight to execve, and none of it is user- or
//     model-controlled anyway.
//   - Its own process group, killed as a group at the timeout, so a wl-paste
//     waiting on a data offer cannot outlive the session that asked.
//   - Output capped, because a clipboard can hold anything.
//   - stderr discarded: wl-paste writes "Nothing is copied" there routinely,
//     and a gatherer's diagnostics are not worth a log line.
func runCapture(ctx context.Context, binary string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, binary, args...) //nolint:gosec // binary and argv are package constants or config; nothing here is model- or speech-derived
	out := &capped{max: maxCaptureBytes}
	cmd.Stdout = out
	cmd.Stderr = nil
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid: the whole group, so a helper the tool spawned dies too.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// A grandchild holding the pipe must not keep Wait blocked past the kill.
	cmd.WaitDelay = time.Second

	if err := cmd.Run(); err != nil {
		return "", err
	}
	return out.String(), nil
}

// capped collects at most max bytes and discards the rest, so a gigabyte on
// the clipboard costs a gigabyte of nothing. Written by one goroutine (the
// exec copier) per command.
type capped struct {
	max int
	buf bytes.Buffer
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		if len(p) <= room {
			c.buf.Write(p)
		} else {
			c.buf.Write(p[:room])
		}
	}
	return len(p), nil
}

func (c *capped) String() string { return c.buf.String() }
