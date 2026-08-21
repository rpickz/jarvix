package desktop

import (
	"context"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestNotifySendArgsCarryActionsAndContent(t *testing.T) {
	args := notifySendArgs(Notification{
		Summary: "Jarvix answered",
		Body:    "Recursion is a function calling itself.",
		Actions: []Action{{ID: DefaultActionID, Label: "Open"}},
	})
	want := []string{
		"--app-name=Jarvix",
		"--wait",
		"--action=default=Open",
		"--",
		"Jarvix answered",
		"Recursion is a function calling itself.",
	}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args = %q, want %q", args, want)
	}
}

// A notification carrying an action must wait for it. Without --wait,
// notify-send exits the moment the notification is submitted, so it never
// prints the clicked action, Send reports none, and the click reads as a
// dismissal — the window would never open, which is the whole feature.
func TestNotifySendWaitsWhenAnActionCanBeClicked(t *testing.T) {
	args := notifySendArgs(Notification{
		Summary: "Jarvix answered",
		Actions: []Action{{ID: DefaultActionID, Label: "Open"}},
	})
	if !slices.Contains(args, "--wait") {
		t.Errorf("args = %q, want --wait so the click can be reported", args)
	}
}

// With nothing to click there is nothing to wait for, and waiting would pin
// the delivering goroutine until the notification expires.
func TestNotifySendDoesNotWaitWithoutActions(t *testing.T) {
	args := notifySendArgs(Notification{Summary: "Jarvix answered"})
	if slices.Contains(args, "--wait") {
		t.Errorf("args = %q, want no --wait when there is no action", args)
	}
}

func TestNotifySendArgsOmitEmptyBody(t *testing.T) {
	args := notifySendArgs(Notification{Summary: "Jarvix answered"})
	if got := args[len(args)-1]; got != "Jarvix answered" {
		t.Errorf("last arg = %q, want the summary (no empty body arg)", got)
	}
}

func TestNotifySendArgsNeverParseContentAsFlags(t *testing.T) {
	// An assistant answer could plausibly start with a dash; the "--"
	// sentinel must precede all positional arguments.
	args := notifySendArgs(Notification{Summary: "Jarvix answered", Body: "--help is a flag"})
	sep := -1
	for i, a := range args {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep < 0 {
		t.Fatalf("no -- separator in %q", args)
	}
	if want := []string{"Jarvix answered", "--help is a flag"}; !reflect.DeepEqual(args[sep+1:], want) {
		t.Errorf("positional args = %q, want %q", args[sep+1:], want)
	}
}

func TestNotifySendMissingBinaryReturnsError(t *testing.T) {
	n := &NotifySend{Binary: filepath.Join(t.TempDir(), "no-such-notify-send")}
	if _, err := n.Send(context.Background(), Notification{Summary: "x"}); err == nil {
		t.Fatal("expected an error when notify-send is absent")
	}
}

func TestWindowClientMissingShellIsActionable(t *testing.T) {
	w := &WindowClient{Binary: filepath.Join(t.TempDir(), "no-such-omarchy-shell")}
	err := w.Toggle(context.Background())
	if err == nil {
		t.Fatal("expected an error when omarchy-shell is absent")
	}
	if !strings.Contains(err.Error(), "install-plugin.sh") {
		t.Errorf("error should point at the fix, got: %v", err)
	}
}
