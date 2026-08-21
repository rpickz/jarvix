package intent

import (
	"strconv"
	"testing"
)

// fixedArgs is every word the built-in table is allowed to put on a command
// line. Anything else appearing in a Match's Argv would mean a transcript
// leaked into a command — the one failure this package exists to prevent.
var fixedArgs = map[string]bool{
	"wpctl": true, "set-volume": true, "set-mute": true,
	"-l": true, "1.5": true, defaultSink: true,
	volumeStep + "+": true, volumeStep + "-": true,
	"0": true, "1": true,
	"hyprctl": true, "dispatch": true, "workspace": true, "exec": true,
	DefaultTerminal: true,
}

// FuzzRouterMatch throws arbitrary text at the router — which is exactly what
// a speech-to-text engine does — and asserts the invariants that make the
// fixed-argv design a security property rather than a convention:
//
//   - a matched slot is always inside its declared bounds;
//   - every argv element is either a constant from the table or the decimal
//     rendering of that bounded integer, so no fragment of the transcript can
//     reach a command line;
//   - a user-defined intent only ever carries the command its configuration
//     declared, never one assembled from what was said.
func FuzzRouterMatch(f *testing.F) {
	seeds := []string{
		"volume thirty", "volume 30", "VOLUME 150!", "volume one hundred and forty five",
		"mute", "unmute the sound", "stop", "stop talking",
		"workspace 4", "go to workspace ten", "open a terminal",
		"new conversation", "turn it up a bit", "lock the screen",
		"volume 30; rm -rf ~", "volume $(rm -rf /)", "workspace `id`",
		"volume -0030", "volume 0x1e", "volume ٣٠", "volume 30%",
		"", "   ", "\x00\x01", "volume\nthirty",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	r, err := New(Options{Custom: []Custom{{Match: "lock the screen", Run: "hyprlock"}}})
	if err != nil {
		f.Fatal(err)
	}

	f.Fuzz(func(t *testing.T, transcript string) {
		m, ok := r.Match(transcript)
		if !ok {
			return
		}
		if m.HasSlot && (m.Slot < minWorkspace-1 || m.Slot > maxVolume) {
			t.Fatalf("slot %d escaped every declared bound (intent %q, transcript %q)",
				m.Slot, m.Name, transcript)
		}
		if m.Name == "workspace.switch" && (m.Slot < minWorkspace || m.Slot > maxWorkspace) {
			t.Fatalf("workspace slot %d out of bounds (transcript %q)", m.Slot, transcript)
		}
		for _, arg := range m.Argv {
			if fixedArgs[arg] {
				continue
			}
			if arg == strconv.Itoa(m.Slot) || arg == strconv.Itoa(m.Slot)+"%" {
				continue
			}
			t.Fatalf("argv element %q is neither a table constant nor the slot value "+
				"(intent %q, transcript %q, argv %v)", arg, m.Name, transcript, m.Argv)
		}
		if m.UserDefined && m.Command != "hyprlock" {
			t.Fatalf("user-defined intent ran %q, not its configured command (transcript %q)",
				m.Command, transcript)
		}
		if !m.UserDefined && m.Command != "" {
			t.Fatalf("built-in intent %q carries a shell command %q", m.Name, m.Command)
		}
	})
}
