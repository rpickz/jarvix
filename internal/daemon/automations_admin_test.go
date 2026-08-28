package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ipc"
)

// The Automations tab's daemon surface (issue #93) over a fully wired daemon:
// the unified listing with its joins (schedule arithmetic, tier verdicts,
// path rechecks, last-run memory), and set_enabled writing through the shared
// surgical editor — grammar recompiled on the standard reload, the schedule
// off the clock, the re-enable collision refused with the load error. All
// decisions are pinned here, on the socket, because the window renders and
// calls (ADR 0013).

// automationsAdminTOML builds the hand-written config the tests boot from
// and edit: one scheduled routine (allow tier by default), one script, and
// one parked script whose schedule must stay off the clock. The comments and
// sibling entries are the byte-preservation fixture.
func automationsAdminTOML(scriptPath string, allowScripts bool) string {
	policy := ""
	if allowScripts {
		policy = "[tools.policy.tool]\n\"script.run\" = \"allow\"\n\n"
	}
	return `# my config, hand-written
[context]
window = false
selection = false
clipboard = false

` + policy + `# the evening wind-down
[[routines]]
name = "evening"
phrases = ["evening mode"]
schedule = "18:00"

  [[routines.steps]]
  app = "mpv"
  workspace = 5

# the nightly backup
[[scripts]]
name = "backup notes"
phrases = ["backup my notes"]
path = "` + scriptPath + `"
report = "stdout"
schedule = "02:00"

# parked while I rework it
[[scripts]]
name = "rotate wallpaper"
phrases = ["rotate the wallpaper"]
path = "` + scriptPath + `"
schedule = "03:00"
enabled = false
`
}

// startAutomationsDaemon boots a daemon from automationsAdminTOML, with a
// real stub script whose marker file proves any run, and hands back the
// pieces the tests read.
func startAutomationsDaemon(t *testing.T, allowScripts bool) (*ipc.Client, string, string) {
	t.Helper()
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "backup-notes.sh")
	marker := filepath.Join(dir, "ran.marker")
	if err := os.WriteFile(scriptPath,
		[]byte("#!/bin/sh\ntouch "+marker+"\necho 'Notes backed up.'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	client, paths := startAdminDaemon(t, automationsAdminTOML(scriptPath, allowScripts))
	return client, paths.ConfigFile(), marker
}

// automationsEntry is the wire shape of one row, for the assertions.
type automationsEntry struct {
	Kind        string   `json:"kind"`
	Name        string   `json:"name"`
	Phrases     []string `json:"phrases"`
	Enabled     *bool    `json:"enabled"`
	Steps       *int     `json:"steps"`
	Incomplete  *bool    `json:"incomplete"`
	Path        string   `json:"path"`
	PathProblem string   `json:"path_problem"`
	Schedule    string   `json:"schedule"`
	Announce    *bool    `json:"announce"`
	NextFire    string   `json:"next_fire"`
	Running     *bool    `json:"running"`
	LastFired   string   `json:"last_fired"`
	WouldRefuse *bool    `json:"would_refuse"`
	Rule        string   `json:"rule"`
	LastRun     *struct {
		At         string `json:"at"`
		Outcome    string `json:"outcome"`
		Failed     bool   `json:"failed"`
		Duration   string `json:"duration"`
		DurationMS *int64 `json:"duration_ms"`
	} `json:"last_run"`
}

// automationsList calls automations.list and returns the fingerprint plus
// the rows.
func automationsList(t *testing.T, client *ipc.Client) (string, []automationsEntry) {
	t.Helper()
	var out struct {
		Fingerprint string             `json:"fingerprint"`
		Automations []automationsEntry `json:"automations"`
	}
	if err := client.Call("automations.list", nil, &out); err != nil {
		t.Fatal(err)
	}
	return out.Fingerprint, out.Automations
}

// TestAutomationsListOverSocket: the tab's one read — routines and scripts as
// one collection in declaration order, each row carrying exactly the facts
// its kind has: phrases and the enabled switch for all; steps and the
// incomplete marker for routines; the path for scripts; the schedule with
// daemon-computed next-fire and the tier verdict for entries the scheduler
// holds; the schedule alone — no next fire, it will not fire — for a parked
// one; and no last_run anywhere before anything has run (never fabricated).
func TestAutomationsListOverSocket(t *testing.T) {
	client, _, _ := startAutomationsDaemon(t, false)

	fp, rows := automationsList(t, client)
	if fp == "" {
		t.Fatal("automations.list carries no fingerprint; set_enabled could not detect external edits")
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %+v", rows)
	}

	routine := rows[0]
	if routine.Kind != "routine" || routine.Name != "evening" ||
		len(routine.Phrases) != 1 || routine.Phrases[0] != "evening mode" {
		t.Errorf("routine row = %+v", routine)
	}
	if routine.Enabled == nil || !*routine.Enabled || routine.Steps == nil || *routine.Steps != 1 ||
		routine.Incomplete == nil || *routine.Incomplete {
		t.Errorf("routine row markers = %+v", routine)
	}
	if routine.Schedule != "18:00" || routine.NextFire == "" {
		t.Errorf("routine schedule = %+v, want the schedule with its next fire", routine)
	}
	if _, err := time.Parse(time.RFC3339, routine.NextFire); err != nil {
		t.Errorf("next_fire %q: %v", routine.NextFire, err)
	}
	// routine.run defaults to allow, so the clock would run it.
	if routine.WouldRefuse == nil || *routine.WouldRefuse {
		t.Errorf("routine verdict = %+v, want would_refuse false", routine)
	}

	scheduled := rows[1]
	if scheduled.Kind != "script" || scheduled.Name != "backup notes" || scheduled.Path == "" {
		t.Errorf("script row = %+v", scheduled)
	}
	if scheduled.PathProblem != "" {
		t.Errorf("path_problem = %q for a healthy stub", scheduled.PathProblem)
	}
	// script.run defaults to ask and a schedule cannot answer: the tab must
	// say so before a 2am notification does.
	if scheduled.NextFire == "" || scheduled.WouldRefuse == nil || !*scheduled.WouldRefuse ||
		scheduled.Rule == "" {
		t.Errorf("script verdict = %+v, want would_refuse with its rule", scheduled)
	}

	parked := rows[2]
	if parked.Name != "rotate wallpaper" || parked.Enabled == nil || *parked.Enabled {
		t.Errorf("parked row = %+v, want it listed and disabled", parked)
	}
	if parked.Schedule != "03:00" {
		t.Errorf("parked schedule = %q; disabled means switched off, never hidden", parked.Schedule)
	}
	if parked.NextFire != "" {
		t.Errorf("parked next_fire = %q; a parked schedule has no next fire", parked.NextFire)
	}

	for _, row := range rows {
		if row.LastRun != nil {
			t.Errorf("row %q fabricated a last run: %+v", row.Name, row.LastRun)
		}
	}
}

// TestAutomationsListPathProblemAfterTheFileRots: load validation refused a
// bad path, so the marker exists for the file that changed underneath the
// running daemon — deleting the stub makes the recheck speak on the next
// listing.
func TestAutomationsListPathProblemAfterTheFileRots(t *testing.T) {
	client, _, _ := startAutomationsDaemon(t, false)
	_, rows := automationsList(t, client)
	if err := os.Remove(rows[1].Path); err != nil {
		t.Fatal(err)
	}
	_, rows = automationsList(t, client)
	if !strings.Contains(rows[1].PathProblem, "does not exist") {
		t.Errorf("path_problem = %q, want the missing file named", rows[1].PathProblem)
	}
}

// TestAutomationsListCarriesLastRunAfterARun: a run through the existing
// gated path lands in the listing — outcome, duration, failed flag — from
// the same observation that feeds the activity ring. The activity.row push
// for the run's ending is the synchronisation: the record is written before
// the row is published, so once the row has arrived the listing must carry
// the run.
func TestAutomationsListCarriesLastRunAfterARun(t *testing.T) {
	client, _, marker := startAutomationsDaemon(t, true)

	var out map[string]string
	if err := client.Call("scripts.run", map[string]string{"name": "backup notes"}, &out); err != nil {
		t.Fatal(err)
	}
	// Both, in whichever order they arrive. They come off one bus through one
	// channel and nothing orders them against each other: the row is written by
	// the daemon's own activity subscriber, so it may trail the session's end or
	// beat it. Two sequential drains would work only for one of those orders,
	// and would eat the other event on the way past — which is exactly how this
	// read as a five-second hang rather than as the race it is.
	waitForRunObserved(t, client, "Script finished: backup notes")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the stub script never ran: %v", err)
	}

	_, rows := automationsList(t, client)
	run := rows[1].LastRun
	if run == nil {
		t.Fatalf("no last_run on %+v", rows[1])
	}
	if run.Outcome != "exit 0" || run.Failed {
		t.Errorf("last_run = %+v, want the exit status", run)
	}
	if run.Duration == "" || run.DurationMS == nil {
		t.Errorf("last_run = %+v, want the duration the event carried", run)
	}
	if at, err := time.Parse(time.RFC3339, run.At); err != nil || time.Since(at) > time.Minute {
		t.Errorf("last_run at = %q (%v), want a fresh timestamp", run.At, err)
	}
	// The routine has not run; its row still says nothing.
	if rows[0].LastRun != nil {
		t.Errorf("the routine fabricated a last run: %+v", rows[0].LastRun)
	}
}

// waitForRunObserved drains events until BOTH the named activity row and the
// session's end have been seen, in either order. See the call site for why the
// order cannot be assumed; the practical rule is that a test which needs two
// events off one channel has to watch for both at once, because the first
// single-event drain consumes whatever else arrives first.
func waitForRunObserved(t *testing.T, client *ipc.Client, label string) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	row, finished := false, false
	for !row || !finished {
		select {
		case ev := <-client.Events():
			switch {
			case ev.Type == "error":
				t.Fatalf("waiting for row %q and session.finished, got error event: %v", label, ev.Data)
			case ev.Type == "activity.row" && ev.Data["label"] == label:
				row = true
			case ev.Type == "session.finished":
				finished = true
			}
		case <-timeout:
			t.Fatalf("row %q seen: %v; session.finished seen: %v", label, row, finished)
		}
	}
}

// waitForActivityRow drains events until the activity feed's row with the
// wanted label arrives — the proof the daemon's own watcher has processed
// the underlying event, because the row is published after the record.
func waitForActivityRow(t *testing.T, client *ipc.Client, label string) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev := <-client.Events():
			if ev.Type == "error" {
				t.Fatalf("waiting for row %q, got error event: %v", label, ev.Data)
			}
			if ev.Type == "activity.row" && ev.Data["label"] == label {
				return
			}
		case <-timeout:
			t.Fatalf("no activity row %q", label)
		}
	}
}

// TestAutomationsSetEnabledOverSocket is the acceptance path for the switch:
// disabling the scheduled script writes `enabled = false` into exactly one
// entry with every other byte preserved, the standard reload takes its
// phrases out of the grammar (the run surface refuses by name) and its
// schedule off the clock (no next fire, gone from automations.schedules),
// and re-enabling restores all of it.
func TestAutomationsSetEnabledOverSocket(t *testing.T) {
	client, configFile, marker := startAutomationsDaemon(t, false)

	fp, _ := automationsList(t, client)
	original, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}

	var res map[string]any
	if err := client.Call("automations.set_enabled",
		map[string]any{"kind": "script", "name": "backup notes", "enabled": false,
			"fingerprint": fp}, &res); err != nil {
		t.Fatal(err)
	}
	if res["applied"] != true {
		t.Fatalf("set_enabled = %v, want it applied on an idle daemon", res)
	}

	raw, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(string(original),
		"schedule = \"02:00\"\n\n# parked",
		"schedule = \"02:00\"\nenabled = false\n\n# parked", 1)
	if string(raw) != want {
		t.Errorf("config after set_enabled:\n%s\n--- want ---\n%s", raw, want)
	}

	// The running daemon — not just the file — knows: the row is disabled
	// with its schedule parked, the scheduler no longer holds it, and the
	// run surface says "disabled", not "unknown".
	_, rows := automationsList(t, client)
	if rows[1].Enabled == nil || *rows[1].Enabled || rows[1].NextFire != "" {
		t.Errorf("row after disable = %+v, want disabled with no next fire", rows[1])
	}
	var schedules struct {
		Schedules []struct {
			Name string `json:"name"`
		} `json:"schedules"`
	}
	if err := client.Call("automations.schedules", nil, &schedules); err != nil {
		t.Fatal(err)
	}
	for _, s := range schedules.Schedules {
		if s.Name == "backup notes" {
			t.Error("a disabled entry is still on the clock")
		}
	}
	err = client.Call("scripts.run", map[string]string{"name": "backup notes"}, nil)
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Errorf("run of a disabled script = %v, want the disabled refusal", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the refused run executed the script anyway")
	}

	// Back on: the same line flips in place and the schedule returns.
	var again map[string]any
	if err := client.Call("automations.set_enabled",
		map[string]any{"kind": "script", "name": "backup notes", "enabled": true,
			"fingerprint": res["fingerprint"]}, &again); err != nil {
		t.Fatal(err)
	}
	_, rows = automationsList(t, client)
	if rows[1].Enabled == nil || !*rows[1].Enabled || rows[1].NextFire == "" {
		t.Errorf("row after re-enable = %+v, want enabled with its next fire back", rows[1])
	}
	raw, err = os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "enabled = true") ||
		!strings.Contains(string(raw), "# parked while I rework it") {
		t.Errorf("config after re-enable lost content:\n%s", raw)
	}
}

// TestAutomationsSetEnabledRoutinePreservesSteps: the routine family's write
// lands in the entry's own body, never inside a [[routines.steps]] table —
// the sub-table shape the golden tests pin, proved here over the socket
// against the running file.
func TestAutomationsSetEnabledRoutinePreservesSteps(t *testing.T) {
	client, configFile, _ := startAutomationsDaemon(t, false)

	fp, _ := automationsList(t, client)
	if err := client.Call("automations.set_enabled",
		map[string]any{"kind": "routine", "name": "evening", "enabled": false,
			"fingerprint": fp}, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "schedule = \"18:00\"\nenabled = false\n") {
		t.Errorf("the switch missed the routine's body:\n%s", raw)
	}
	if !strings.Contains(string(raw), "  [[routines.steps]]\n  app = \"mpv\"\n  workspace = 5") {
		t.Errorf("the steps were disturbed:\n%s", raw)
	}
	err = client.Call("routines.run", map[string]string{"name": "evening"}, nil)
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Errorf("run of a disabled routine = %v, want the disabled refusal", err)
	}
}

// TestAutomationsSetEnabledReenableCollision is #93's collision criterion on
// the socket: while "old thing" slept, "new thing" took its phrase — a state
// config load accepts — and re-enabling fails with the same actionable error
// a load gives, nothing written, never a half-enable.
func TestAutomationsSetEnabledReenableCollision(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "thing.sh")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	doc := fmt.Sprintf(`[context]
window = false
selection = false
clipboard = false

[[routines]]
name = "old thing"
phrases = ["do the thing"]
enabled = false

  [[routines.steps]]
  app = "firefox"
  workspace = 1

[[scripts]]
name = "new thing"
phrases = ["do the thing"]
path = "%s"
`, stub)
	client, paths := startAdminDaemon(t, doc)

	err := client.Call("automations.set_enabled",
		map[string]any{"kind": "routine", "name": "old thing", "enabled": true}, nil)
	var rpcErr *ipc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != ipc.CodeConfigInvalid {
		t.Fatalf("err = %v, want CodeConfigInvalid", err)
	}
	data, _ := rpcErr.Data.(map[string]any)
	problems, _ := data["problems"].([]any)
	found := false
	for _, p := range problems {
		if s, _ := p.(string); strings.Contains(s, `already the trigger for routine "old thing"`) {
			found = true
		}
	}
	if !found {
		t.Errorf("problems = %v, want the collision naming both owners", problems)
	}
	raw, readErr := os.ReadFile(paths.ConfigFile())
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(raw) != doc {
		t.Errorf("a refused enable still changed the file:\n%s", raw)
	}
	// Nothing half-enabled: the phrase still routes to the enabled owner and
	// the sleeping routine still refuses to run.
	err = client.Call("routines.run", map[string]string{"name": "old thing"}, nil)
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Errorf("run after the refused enable = %v, want still disabled", err)
	}
}

// TestAutomationsSetEnabledRefusals: an unknown kind, an unknown name, and a
// stale fingerprint each refuse with the structured error the tab surfaces —
// and never touch the file.
func TestAutomationsSetEnabledRefusals(t *testing.T) {
	client, configFile, _ := startAutomationsDaemon(t, false)
	original, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}

	callErr := client.Call("automations.set_enabled",
		map[string]any{"kind": "advisor", "name": "x", "enabled": false}, nil)
	var rpcErr *ipc.Error
	if !errors.As(callErr, &rpcErr) || rpcErr.Code != ipc.CodeInvalidParams ||
		!strings.Contains(rpcErr.Message, `"advisor"`) {
		t.Errorf("unknown kind err = %v, want CodeInvalidParams naming it", callErr)
	}

	callErr = client.Call("automations.set_enabled",
		map[string]any{"kind": "script", "name": "no such", "enabled": false}, nil)
	if !errors.As(callErr, &rpcErr) || rpcErr.Code != ipc.CodeInvalidParams ||
		!strings.Contains(rpcErr.Message, `"no such"`) {
		t.Errorf("unknown name err = %v, want CodeInvalidParams naming it", callErr)
	}

	// A hand edit while the tab sat open: the switch is a conflict carrying
	// the fresh fingerprint, and the hand edit survives untouched.
	fp, _ := automationsList(t, client)
	edited := string(original) + "\n# hand note added while the tab was open\n"
	if err := os.WriteFile(configFile, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	callErr = client.Call("automations.set_enabled",
		map[string]any{"kind": "script", "name": "backup notes", "enabled": false,
			"fingerprint": fp}, nil)
	if !errors.As(callErr, &rpcErr) || rpcErr.Code != ipc.CodeConfigConflict {
		t.Fatalf("stale fingerprint err = %v, want CodeConfigConflict", callErr)
	}
	data, _ := rpcErr.Data.(map[string]any)
	if fresh, _ := data["fingerprint"].(string); fresh == "" || fresh == fp {
		t.Errorf("conflict data = %v, want the fresh fingerprint to retry with", rpcErr.Data)
	}
	raw, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != edited {
		t.Errorf("the hand edit was clobbered:\n%s", raw)
	}
}
