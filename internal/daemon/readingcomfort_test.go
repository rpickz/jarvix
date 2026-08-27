package daemon

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/ipc"
)

// Reading comfort (issue #121) at the IPC surface: the three typography
// settings ride config.get — the same snapshot the conversation window
// re-reads on config.changed — apply live on config.set, persist in the
// file, and refuse out-of-range values with the standard field problem.

// TestReadingComfortAppliesLiveAndPersists is the issue's headline flow: a
// change from the Settings form, `jarvix config set`, or the assistant's
// settings tool (all clients of config.set) lands in the running config
// immediately — no restart, no idle wait — so the window's next config.get,
// triggered by the config.changed event this set publishes, re-renders the
// open transcript with the new values.
func TestReadingComfortAppliesLiveAndPersists(t *testing.T) {
	h := startSettingsDaemon(t)
	res := h.get(t)

	// The defaults pin the pre-#121 rendering all the way out to the wire.
	for key, want := range map[string]float64{
		"ui.line_spacing":   1.0,
		"ui.text_size":      1.0,
		"ui.letter_spacing": 0.0,
	} {
		if got := h.field(t, res, key); got != want {
			t.Errorf("default %s = %v, want %v", key, got, want)
		}
	}

	var set setResult
	err := h.client.Call("config.set", map[string]any{
		"changes": map[string]any{
			"ui.line_spacing":   1.5,
			"ui.text_size":      1.15,
			"ui.letter_spacing": 0.12,
		},
		"fingerprint": res.Fingerprint,
	}, &set)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Applied {
		t.Fatalf("typography set not applied live: %s", set.Reason)
	}
	if len(set.NeedsRestart) != 0 {
		t.Errorf("needs_restart = %v, want none — typography is live-class", set.NeedsRestart)
	}

	// The running values the window's refresh reads are the new ones.
	after := h.get(t)
	for key, want := range map[string]float64{
		"ui.line_spacing":   1.5,
		"ui.text_size":      1.15,
		"ui.letter_spacing": 0.12,
	} {
		if got := h.field(t, after, key); got != want {
			t.Errorf("running %s = %v after set, want %v", key, got, want)
		}
	}

	// And the change persists: the values are in config.toml, not only in
	// the running daemon.
	data, err := os.ReadFile(h.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"line_spacing = 1.5", "text_size = 1.15", "letter_spacing = 0.12"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("rewritten file lacks %q:\n%s", want, data)
		}
	}
}

// TestReadingComfortOutOfRangeIsRefusedWithFieldProblem: registry validation
// refuses a value outside the bounded range with the standard field problem —
// the message names the key, so the settings screen pins it to the field and
// the CLI/voice paths read it back verbatim. Nothing is written.
func TestReadingComfortOutOfRangeIsRefusedWithFieldProblem(t *testing.T) {
	h := startSettingsDaemon(t)
	res := h.get(t)
	before, err := os.ReadFile(h.cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	callErr := h.client.Call("config.set", map[string]any{
		"changes":     map[string]any{"ui.line_spacing": 5.0},
		"fingerprint": res.Fingerprint,
	}, nil)
	var rpcErr *ipc.Error
	if !errors.As(callErr, &rpcErr) || rpcErr.Code != ipc.CodeConfigInvalid {
		t.Fatalf("err = %v, want CodeConfigInvalid", callErr)
	}
	data, _ := rpcErr.Data.(map[string]any)
	problems, _ := data["problems"].([]any)
	if len(problems) == 0 || !strings.Contains(problems[0].(string), "ui.line_spacing") {
		t.Errorf("problems = %v, want one naming ui.line_spacing", problems)
	}

	after, err := os.ReadFile(h.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("refused set modified the file")
	}
	if got := h.field(t, h.get(t), "ui.line_spacing"); got != 1.0 {
		t.Errorf("running ui.line_spacing = %v, want 1.0 (unchanged)", got)
	}
}
