package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/tools"
)

// The managed-window wiring at the daemon boundary (#197, ADR 0062): which
// tools a configured daemon offers, and which verbs the window can call.

func TestManagedWindowToolsAreRegisteredWithTheWindowTools(t *testing.T) {
	d := daemonWith(t, testConfig())
	names := strings.Join(d.registry.Names(), ",")
	for _, want := range []string{
		tools.ManageWindowToolName, tools.ReleaseWindowToolName, tools.ListManagedToolName,
	} {
		if !strings.Contains(names, want) {
			t.Errorf("registered tools = %q, missing %q", names, want)
		}
	}
}

func TestManagedWindowToolsAreAbsentWhenTheWindowToolsAre(t *testing.T) {
	cfg := testConfig()
	cfg.Tools.Desktop = false
	d := daemonWith(t, cfg)
	names := strings.Join(d.registry.Names(), ",")
	for _, absent := range []string{
		tools.ManageWindowToolName, tools.ReleaseWindowToolName, tools.ListManagedToolName,
	} {
		if strings.Contains(names, absent) {
			t.Errorf("registered tools = %q, want no %q", names, absent)
		}
	}
}

// managedReply is the windows.managed shape the window decodes.
type managedReply struct {
	Windows []tools.ManagedWindowListing `json:"windows"`
	Path    string                       `json:"path"`
	Typing  bool                         `json:"typing"`
}

// The whole surface the window drives, over a real socket: an empty listing
// that says so, a window handed over by the model's own tool, and the ungated
// release the button calls.
func TestTheManagedWindowVerbsListAndRelease(t *testing.T) {
	d, socket := monitorsDaemon(t)
	serveDaemon(t, d)
	client := dialDaemon(t, socket)

	var listing managedReply
	if err := client.Call("windows.managed", nil, &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Windows) != 0 {
		t.Fatalf("windows = %+v, want none managed on a fresh daemon", listing.Windows)
	}
	if listing.Path == "" {
		t.Error("the reply must name the file, so a user can find and edit it")
	}

	// Hand the one window over through the same seam the model's tool uses.
	if _, err := d.windows.AcquireWindow(context.Background(), "code"); err != nil {
		t.Fatalf("AcquireWindow: %v", err)
	}
	if err := client.Call("windows.managed", nil, &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Windows) != 1 {
		t.Fatalf("windows = %+v, want the one handed over", listing.Windows)
	}
	row := listing.Windows[0]
	if row.App != "code" || row.Workspace != "1" || row.Source != "acquired" {
		t.Errorf("row = %+v, want the window by app, workspace and how it came to be managed", row)
	}
	if row.Reference == "" {
		t.Error("the row carries no reference, so the window could offer no Release button")
	}

	// Release is ungated: one call, no confirmation, nothing to answer.
	var released struct {
		Spoken string `json:"spoken"`
	}
	if err := client.Call("windows.release", map[string]any{"window": row.Reference}, &released); err != nil {
		t.Fatalf("windows.release: %v", err)
	}
	if !strings.Contains(released.Spoken, "let") {
		t.Errorf("spoken = %q, want it to say what was let go", released.Spoken)
	}
	if err := client.Call("windows.managed", nil, &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Windows) != 0 {
		t.Fatalf("windows = %+v, want nothing managed after the release", listing.Windows)
	}
}

// Releasing something the daemon never had is a refusal carrying the seam's
// own sentence, not a bare success.
func TestReleasingAnUnmanagedWindowOverIPCSaysSo(t *testing.T) {
	d, socket := monitorsDaemon(t)
	serveDaemon(t, d)
	client := dialDaemon(t, socket)
	var reply struct {
		Spoken string `json:"spoken"`
	}
	err := client.Call("windows.release", map[string]any{"window": "code"}, &reply)
	if err == nil {
		t.Fatal("releasing an unmanaged window must not report a success")
	}
	if !strings.Contains(err.Error(), "was not managing") {
		t.Errorf("err = %v, want the seam's own sentence", err)
	}
}

// The activity feed renders a typed command as a command, verbatim and with
// the rule that judged it — the evidence that survives a standing approval
// removing the question (#162's discipline, applied to the keyboard).
func TestATypedCommandGetsACommandRowInTheFeed(t *testing.T) {
	rows := desktop.ActivityRowsFor("typing.audit", map[string]any{
		"tool": tools.TypeTextToolName, "window": "Alacritty — zsh", "chars": 12,
		"terminal": true, "outcome": "typed", "approved": true,
		"command": "docker ps -a", "rule": `configured allow pattern "docker ps"`,
	})
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want the command row and the typing row", rows)
	}
	if rows[0].Kind != desktop.ActivityKindGate {
		t.Errorf("kind = %q, want the gate kind — this row IS the gate reporting", rows[0].Kind)
	}
	if !strings.Contains(rows[0].Label, "Command typed into a terminal") {
		t.Errorf("label = %q, want it to say a command was typed", rows[0].Label)
	}
	if !strings.Contains(rows[0].Detail, "docker ps -a") {
		t.Errorf("detail = %q, want the command verbatim", rows[0].Detail)
	}
	if !strings.Contains(rows[0].Detail, "docker ps") {
		t.Errorf("detail = %q, want the rule that allowed it", rows[0].Detail)
	}
}

// Typing that is not a command gets exactly the row it always got: one, with
// a character count and no payload.
func TestOrdinaryTypingStillGetsOneRowAndNoPayload(t *testing.T) {
	rows := desktop.ActivityRowsFor("typing.audit", map[string]any{
		"tool": tools.TypeTextToolName, "window": "Code — engine.go", "chars": 12,
		"terminal": false, "outcome": "typed", "approved": true,
	})
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want exactly one", rows)
	}
	if !strings.Contains(rows[0].Label, "Typed 12 characters") {
		t.Errorf("label = %q, want the character count", rows[0].Label)
	}
}

// A command the gate refused says so, rather than reading as one that ran.
func TestARefusedCommandRowSaysItWasNotTyped(t *testing.T) {
	rows := desktop.ActivityRowsFor("typing.audit", map[string]any{
		"tool": tools.TypeTextToolName, "window": "Alacritty — zsh", "chars": 8,
		"terminal": true, "outcome": "refused", "reason": "it is refused by a deny rule",
		"command": "rm -rf /", "rule": `deny pattern "rm targeting /"`,
	})
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want the command row and the refusal row", rows)
	}
	if !strings.Contains(rows[0].Label, "NOT typed") {
		t.Errorf("label = %q, want it to say the command did not go in", rows[0].Label)
	}
}
