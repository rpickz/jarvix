package desktop

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The argv a gatherer builds is the whole difference between reading the
// user's highlighted text and reading their clipboard, so it is asserted
// directly rather than inferred from behaviour.

func TestGathererArgv(t *testing.T) {
	cases := map[string]struct {
		got  []string
		want string
	}{
		"active window": {activeWindowArgs(), "activewindow -j"},
		// --primary is the one flag that separates the two wl-paste sources.
		"primary selection": {primarySelectionArgs(), "--primary --no-newline --type text"},
		"clipboard":         {clipboardArgs(), "--no-newline --type text"},
	}
	for name, c := range cases {
		if got := strings.Join(c.got, " "); got != c.want {
			t.Errorf("%s argv = %q, want %q", name, got, c.want)
		}
	}
	if strings.Join(primarySelectionArgs(), " ") == strings.Join(clipboardArgs(), " ") {
		t.Error("selection and clipboard would read the same buffer")
	}
}

func TestParseActiveWindow(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"app and title", `{"class":"Alacritty","title":"nvim engine.go"}`, "Alacritty — nvim engine.go", false},
		{"title only", `{"class":"","title":"Inbox — Mail"}`, "Inbox — Mail", false},
		{"class only", `{"class":"Steam","title":""}`, "Steam", false},
		{"identical class and title", `{"class":"Slack","title":"Slack"}`, "Slack", false},
		// Nothing focused: hyprctl prints an empty object. Not an error — an
		// empty desktop is an ordinary state, and it simply contributes nothing.
		{"nothing focused", `{}`, "", false},
		{"extra fields ignored", `{"address":"0x5","class":"foot","title":"~","pid":42}`, "foot — ~", false},
		{"not json", `Invalid window`, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseActiveWindow(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("parseActiveWindow(%q) = %q, want an error", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseActiveWindow(%q): %v", c.in, err)
			}
			if got != c.want {
				t.Errorf("parseActiveWindow(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestGatherersReadTheirBinary(t *testing.T) {
	window := writeStub(t, "hyprctl", `#!/bin/sh
printf '{"class":"kitty","title":"htop"}'
`)
	paste := writeStub(t, "wl-paste", `#!/bin/sh
printf 'selected words\n'
`)
	ctx := context.Background()

	got, err := (&ActiveWindow{Binary: window}).Gather(ctx)
	if err != nil || got != `kitty — htop` {
		t.Errorf("active window = %q, %v", got, err)
	}
	if got, err := (&PrimarySelection{Binary: paste}).Gather(ctx); err != nil || got != "selected words\n" {
		t.Errorf("selection = %q, %v", got, err)
	}
	if got, err := (&Clipboard{Binary: paste}).Gather(ctx); err != nil || got != "selected words\n" {
		t.Errorf("clipboard = %q, %v", got, err)
	}
}

func TestGathererFailureIsReportedNotGuessedAt(t *testing.T) {
	// wl-paste exits non-zero with an empty clipboard, and hyprctl does the
	// same with no compositor. Both are errors here and "no context" upstream.
	empty := writeStub(t, "wl-paste", `#!/bin/sh
echo "Nothing is copied" >&2
exit 1
`)
	if got, err := (&Clipboard{Binary: empty}).Gather(context.Background()); err == nil {
		t.Errorf("clipboard = %q, want an error", got)
	}
	missing := &ActiveWindow{Binary: "/nonexistent/hyprctl"}
	if _, err := missing.Gather(context.Background()); err == nil {
		t.Error("a missing binary must be an error, not silence")
	}
}

func TestGathererIsKilledAtTheDeadline(t *testing.T) {
	// The reliability requirement: a hung tool never wedges a session. The
	// stub blocks forever; the context deadline must kill the whole process
	// group and return promptly.
	hang := writeStub(t, "wl-paste", `#!/bin/sh
sleep 300
`)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if got, err := (&Clipboard{Binary: hang}).Gather(ctx); err == nil {
		t.Errorf("clipboard = %q, want the deadline error", got)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("hung gatherer took %v to give up", elapsed)
	}
}

func TestCapturedOutputIsCapped(t *testing.T) {
	c := &capped{max: 4}
	n, err := c.Write([]byte("abcdefgh"))
	if n != 8 || err != nil {
		t.Fatalf("Write = %d, %v — a capped writer must still absorb everything", n, err)
	}
	if _, _ = c.Write([]byte("ijkl")); c.String() != "abcd" {
		t.Errorf("captured %q, want the first 4 bytes only", c.String())
	}
}

func TestBinaryOr(t *testing.T) {
	if got := binaryOr("", "wl-paste"); got != "wl-paste" {
		t.Errorf("binaryOr = %q", got)
	}
	if got := binaryOr("  ", "wl-paste"); got != "wl-paste" {
		t.Errorf("blank override = %q, want the default", got)
	}
	if got := binaryOr("/opt/wl-paste", "wl-paste"); got != "/opt/wl-paste" {
		t.Errorf("binaryOr = %q", got)
	}
}
