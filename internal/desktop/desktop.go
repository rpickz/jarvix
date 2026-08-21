// Package desktop integrates Jarvix with the user's desktop session: it
// delivers freedesktop notifications and opens the conversation window that
// the Omarchy shell plugin renders.
//
// Both integrations shell out to existing binaries — notify-send for
// org.freedesktop.Notifications, omarchy-shell for the plugin's IPC surface
// — rather than taking a D-Bus library dependency. That is the same trade
// ADR 0002 made for the speech engines: zero new dependencies, crash
// isolation, and clean degradation (a missing binary or absent notification
// daemon becomes a log line, never a daemon failure).
package desktop

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Notification is one desktop notification to deliver.
type Notification struct {
	Summary string
	Body    string
	// Actions are the clickable choices offered on the notification.
	Actions []Action
}

// Action is one clickable notification action.
type Action struct {
	ID    string
	Label string
}

// DefaultActionID is the freedesktop-reserved action ID invoked by clicking
// the notification body itself rather than a named button. Daemons that
// support it make the whole notification the click target — exactly what
// "click the notification to open the window" wants.
const DefaultActionID = "default"

// Notifier delivers desktop notifications. Send blocks until the
// notification is acted on, dismissed, or expires, and returns the ID of the
// invoked action ("" when the user did nothing) — callers that must not
// block run it from a goroutine. The fake in fake.go stands in for tests.
type Notifier interface {
	Send(ctx context.Context, n Notification) (invoked string, err error)
}

// NotifySend delivers notifications by running notify-send, which speaks
// org.freedesktop.Notifications on our behalf. `--action` makes notify-send
// wait for the outcome and print the invoked action's ID to stdout, giving
// us click handling without a bus connection. When no notification daemon is
// listening, notify-send fails and the caller degrades to log-only.
type NotifySend struct {
	// Binary overrides the notify-send executable (tests, unusual installs).
	// Empty means "notify-send" from PATH.
	Binary string
}

// Send implements Notifier.
func (n *NotifySend) Send(ctx context.Context, note Notification) (string, error) {
	bin := n.Binary
	if bin == "" {
		bin = "notify-send"
	}
	out, err := exec.CommandContext(ctx, bin, notifySendArgs(note)...).Output()
	if err != nil {
		// The body may hold assistant content; report only the failure, never
		// the arguments.
		return "", fmt.Errorf("notify-send failed: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// notifySendArgs builds the notify-send invocation. Split out so tests can
// assert on exactly what would run without needing a notification daemon.
func notifySendArgs(n Notification) []string {
	args := []string{"--app-name=Jarvix"}
	if len(n.Actions) > 0 {
		// Without --wait, notify-send exits as soon as the notification is
		// submitted and is no longer around to receive the click — it prints
		// nothing, Send reports no action, and the click is indistinguishable
		// from a dismissal, so the window never opens. With it, notify-send
		// lives until the notification is actioned, dismissed, or expires,
		// which is the contract watchSessions already dispatches for.
		// Only meaningful when an action exists: with none there is nothing
		// to report back, and waiting would pin a goroutine for nothing.
		args = append(args, "--wait")
	}
	for _, a := range n.Actions {
		args = append(args, "--action="+a.ID+"="+a.Label)
	}
	// "--" stops option parsing so a summary or body can never be mistaken
	// for a flag, whatever the assistant said.
	args = append(args, "--", n.Summary)
	if n.Body != "" {
		args = append(args, n.Body)
	}
	return args
}

// WindowClient opens and toggles the conversation window by asking the
// Omarchy shell's Jarvix plugin over its IPC surface (`omarchy-shell
// <target> <function>`, the same CLI scripts/install-plugin.sh drives). The
// daemon is deliberately not involved: the plugin renders its own "daemon is
// not running" state, so the window must open even when jarvixd is down.
type WindowClient struct {
	// Binary overrides the omarchy-shell executable (tests). Empty means
	// "omarchy-shell" from PATH.
	Binary string
}

// Open shows the conversation window (idempotent when already open).
func (w *WindowClient) Open(ctx context.Context) error { return w.call(ctx, "openWindow") }

// Toggle shows the window when hidden and hides it when shown — the
// behaviour a keybinding wants.
func (w *WindowClient) Toggle(ctx context.Context) error { return w.call(ctx, "toggleWindow") }

func (w *WindowClient) call(ctx context.Context, fn string) error {
	bin := w.Binary
	if bin == "" {
		bin = "omarchy-shell"
	}
	out, err := exec.CommandContext(ctx, bin, "jarvix", fn).CombinedOutput()
	if err != nil {
		return fmt.Errorf("could not reach the Jarvix window — is the Omarchy shell running "+
			"and the plugin installed? (scripts/install-plugin.sh): %v: %s",
			err, strings.TrimSpace(string(out)))
	}
	return nil
}
