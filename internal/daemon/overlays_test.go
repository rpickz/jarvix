package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/session"
)

// The daemon half of the window overlays (#127): the overlays.get verb over
// the real socket, the overlays.changed event when enrolment changes, and
// the one global off switch. The feed's own rules — badges, occlusion,
// fullscreen, pruning — are tested in internal/overlay against fakes; here
// the assertion is that the daemon wires those rules to the wire.

// overlayWindow is a focused, tiled window with real geometry, which is what
// the feed needs before it will say anything at all.
func overlayWindow() desktop.Window {
	return desktop.Window{
		Address: "0xa", Class: "Alacritty", Title: "make test", Focused: true,
		Workspace: 1, X: 100, Y: 50, Width: 800, Height: 600,
	}
}

func TestOverlaysGetServesTheAnchoredWindow(t *testing.T) {
	h := startFocusDaemonWith(t, testConfig(), overlayWindow())
	client := dialDaemon(t, h.socket)

	// Nothing enrolled: the surface is clean by default.
	var before struct {
		Enabled bool             `json:"enabled"`
		Rows    []map[string]any `json:"rows"`
	}
	if err := client.Call("overlays.get", nil, &before); err != nil {
		t.Fatal(err)
	}
	if !before.Enabled || len(before.Rows) != 0 {
		t.Fatalf("before enrolment: %+v, want enabled and empty", before)
	}

	// Anchoring the focused window enrols it (#123's create-with-windows).
	if err := client.Call("focus.create", map[string]any{
		"name": "ci refactor", "windows": 1,
	}, nil); err != nil {
		t.Fatal(err)
	}

	var after struct {
		Enabled bool             `json:"enabled"`
		Rows    []map[string]any `json:"rows"`
	}
	if err := client.Call("overlays.get", nil, &after); err != nil {
		t.Fatal(err)
	}
	if len(after.Rows) != 1 {
		t.Fatalf("after enrolment: rows = %+v, want one", after.Rows)
	}
	row := after.Rows[0]
	if row["x"] != float64(100) || row["y"] != float64(50) ||
		row["width"] != float64(800) || row["height"] != float64(600) {
		t.Errorf("row geometry = %+v, want the inventory's 100,50 800x600", row)
	}
	badge, _ := row["badge"].(map[string]any)
	if badge == nil || badge["thread"] != "ci refactor" || badge["active"] != true {
		t.Errorf("badge = %+v, want the active thread's filled badge", badge)
	}
	if _, present := row["ai_state"]; present {
		t.Errorf("row = %+v carries ai_state; absent means absent until #137 classifies", row)
	}
	// The wire carries no window identity (ADR 0022).
	raw, err := json.Marshal(after.Rows)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"0xa", "address", "Alacritty", "make test"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("overlays.get payload %s carries %q; addresses and window identity never travel", raw, leak)
		}
	}
}

func TestOverlaysChangedIsPublishedWhenEnrolmentChanges(t *testing.T) {
	h := startFocusDaemonWith(t, testConfig(), overlayWindow())
	client := dialDaemon(t, h.socket)
	watcher := dialDaemon(t, h.socket)

	if err := client.Call("focus.create", map[string]any{
		"name": "ci refactor", "windows": 1,
	}, nil); err != nil {
		t.Fatal(err)
	}

	// The create publishes focus.changed, the watcher pokes the feed, and
	// the changed rows go out — no client asked for anything.
	deadline := time.After(5 * time.Second)
	for {
		var ev session.Event
		select {
		case ev = <-watcher.Events():
		case <-deadline:
			t.Fatal("no overlays.changed event arrived after enrolment")
		}
		if ev.Type != "overlays.changed" {
			continue
		}
		rows, _ := ev.Data["rows"].([]any)
		if len(rows) != 1 {
			t.Fatalf("overlays.changed rows = %+v, want one", ev.Data["rows"])
		}
		return
	}
}

func TestOverlaysDisabledSwitchClearsTheSurface(t *testing.T) {
	cfg := testConfig()
	cfg.Overlays.Enabled = false
	h := startFocusDaemonWith(t, cfg, overlayWindow())
	client := dialDaemon(t, h.socket)

	if err := client.Call("focus.create", map[string]any{
		"name": "ci refactor", "windows": 1,
	}, nil); err != nil {
		t.Fatal(err)
	}
	var res struct {
		Enabled bool             `json:"enabled"`
		Rows    []map[string]any `json:"rows"`
	}
	if err := client.Call("overlays.get", nil, &res); err != nil {
		t.Fatal(err)
	}
	if res.Enabled || len(res.Rows) != 0 {
		t.Fatalf("overlays.get with the switch off = %+v, want disabled and empty — "+
			"overlays.enabled is the one global off switch", res)
	}
}
