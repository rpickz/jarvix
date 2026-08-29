package tools

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/managed"
)

// The managed-window surface's tests (#197, ADR 0062), at the tool boundary:
// what the user is asked, what they are told, and — the half the ticket is
// really about — what management does NOT let anything do.
//
// Everything is hermetic: a fake compositor, a store in a temp directory, and
// no gate calls that touch a real desktop.

// managedHarness is the window tools with a managed-window store behind them.
type managedHarness struct {
	*harness
	store *managed.Store
	path  string
}

func newManagedHarness(t *testing.T, opts DesktopOptions) *managedHarness {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "managed.toml")
	store := managed.NewStore(path, managed.StoreOptions{}, nil)
	opts.Managed = store
	return &managedHarness{harness: newHarnessWith(t, opts), store: store, path: path}
}

// theTerminal is testWindows()'s Alacritty window — an unmanaged terminal on
// workspace 2, the one the user points at when they say "take control of this
// terminal".
func theTerminal() desktop.Window {
	for _, w := range testWindows() {
		if w.Class == "Alacritty" {
			return w
		}
	}
	panic("testWindows has no terminal")
}

// confirm runs one tool's Confirmable half, which is what the user hears
// before they answer.
func confirm(t *testing.T, tool Tool, args map[string]any) (command, summary string, ok bool) {
	t.Helper()
	c, confirmable := tool.(Confirmable)
	if !confirmable {
		t.Fatalf("%s is not Confirmable", tool.Name())
	}
	input, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return c.Confirmation(input)
}

// ------------------------------------------------------------ acquisition

// Handing a window over is a grant, so it is asked about — and the question
// names the window, because "may I use the desktop.manage_window tool" asks
// the user to approve a tool rather than an act.
func TestAcquiringAWindowAsksAndNamesIt(t *testing.T) {
	h := newManagedHarness(t, DesktopOptions{})
	tool := h.tool(t, ManageWindowToolName)
	command, summary, ok := confirm(t, tool, map[string]any{"window": "alacritty"})
	if !ok {
		t.Fatal("acquiring a window must produce a confirmation")
	}
	if !strings.Contains(summary, "Alacritty") {
		t.Errorf("summary = %q, want the window named", summary)
	}
	if !strings.Contains(summary, "confirmed command by command") {
		t.Errorf("summary = %q, want it to say management is not permission to run things", summary)
	}
	if !strings.Contains(command, "Alacritty") {
		t.Errorf("command = %q, want the published action to name the window", command)
	}
}

// The tier does not come from the gate-wide default. Somebody who wrote
// `default = "allow"` was thinking about reading their system state, not
// about handing over a live shell; a stricter default still wins.
func TestAcquisitionAlwaysAsks(t *testing.T) {
	cases := []struct {
		name  string
		def   PolicyDecision
		tiers map[string]PolicyDecision
		want  PolicyDecision
	}{
		{"the shipped default asks", PolicyAsk, nil, PolicyAsk},
		{"a global allow does not reach it", PolicyAllow, nil, PolicyAsk},
		{"a global deny does", PolicyDeny, nil, PolicyDeny},
		{"naming the tool allows it", PolicyAsk,
			map[string]PolicyDecision{ManageWindowToolName: PolicyAllow}, PolicyAllow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			policy, err := NewPolicy(PolicyConfig{Default: tc.def, Tools: tc.tiers})
			if err != nil {
				t.Fatalf("NewPolicy: %v", err)
			}
			if got := policy.ToolDecision(ManageWindowToolName); got != tc.want {
				t.Fatalf("decision = %q, want %q", got, tc.want)
			}
		})
	}
}

// An approval is about ONE window. Carrying it forward would hand over the
// next one unasked, which is a grant nobody made.
func TestAcquisitionApprovalIsNeverRemembered(t *testing.T) {
	if RememberableApproval(ManageWindowToolName) {
		t.Error("a manage-window approval must never be remembered for the conversation")
	}
}

// Releasing is never gated: giving up power needs no permission, so it is
// allow-tier however the rest of the policy is written.
func TestReleaseIsUngated(t *testing.T) {
	for _, def := range []PolicyDecision{PolicyAsk, PolicyAllow} {
		policy, err := NewPolicy(PolicyConfig{Default: def})
		if err != nil {
			t.Fatalf("NewPolicy: %v", err)
		}
		if got := policy.ToolDecision(ReleaseWindowToolName); got != PolicyAllow {
			t.Errorf("with default %q, release decision = %q, want allow", def, got)
		}
		if got := policy.ToolDecision(ListManagedToolName); got != PolicyAllow {
			t.Errorf("with default %q, listing decision = %q, want allow", def, got)
		}
	}
}

// The whole round trip by voice: hand it over, see it listed, let it go.
func TestAcquireListRelease(t *testing.T) {
	h := newManagedHarness(t, DesktopOptions{})
	term := theTerminal()

	out := h.run(t, ManageWindowToolName, map[string]any{"window": "alacritty"})
	if !strings.Contains(out, "I have Alacritty") {
		t.Fatalf("acquire = %q, want it to say what it now has", out)
	}
	if _, ok := h.store.Managed(term, testWindows()); !ok {
		t.Fatal("the window should be in the store after acquisition")
	}

	listed := h.run(t, ListManagedToolName, nil)
	if !strings.Contains(listed, "Alacritty") || !strings.Contains(listed, "workspace 2") {
		t.Errorf("listing = %q, want the window and its workspace", listed)
	}

	out = h.run(t, ReleaseWindowToolName, map[string]any{"window": "alacritty"})
	if !strings.Contains(out, "let Alacritty") {
		t.Fatalf("release = %q, want it to name what it let go", out)
	}
	if _, ok := h.store.Managed(term, testWindows()); ok {
		t.Fatal("the window must not still be managed after a release")
	}
}

// Releasing something Jarvix never had is said, not answered with a bare
// success: "done" when nothing changed is the shrug this feature replaces.
func TestReleasingAnUnmanagedWindowSaysSo(t *testing.T) {
	h := newManagedHarness(t, DesktopOptions{})
	out := h.run(t, ReleaseWindowToolName, map[string]any{"window": "alacritty"})
	if !strings.Contains(out, "was not managing") {
		t.Errorf("release = %q, want it to say there was nothing to let go", out)
	}
}

// Handing over the same window twice is one wish stated twice.
func TestAcquiringTwiceSaysItAlreadyHasIt(t *testing.T) {
	h := newManagedHarness(t, DesktopOptions{})
	h.run(t, ManageWindowToolName, map[string]any{"window": "alacritty"})
	out := h.run(t, ManageWindowToolName, map[string]any{"window": "alacritty"})
	if !strings.Contains(out, "already have") {
		t.Errorf("second acquire = %q, want it to say it already has that window", out)
	}
}

// The listing names windows the way the user does: nickname first, then app
// and workspace.
func TestManagedListingUsesTheNickname(t *testing.T) {
	h := newManagedHarness(t, DesktopOptions{})
	if _, err := h.d.AssignNickname(context.Background(), "alacritty", "builds"); err != nil {
		t.Fatalf("AssignNickname: %v", err)
	}
	h.run(t, ManageWindowToolName, map[string]any{"window": "builds"})
	listed := h.run(t, ListManagedToolName, nil)
	if !strings.Contains(listed, "builds") {
		t.Errorf("listing = %q, want the nickname the user chose", listed)
	}

	rows, err := h.d.ManagedWindowListings(context.Background())
	if err != nil {
		t.Fatalf("ManagedWindowListings: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("listings = %+v, want one row", rows)
	}
	if rows[0].Nickname != "builds" || rows[0].App != "Alacritty" || rows[0].Workspace != "2" {
		t.Errorf("row = %+v, want nickname, app and workspace", rows[0])
	}
	if rows[0].Reference != "builds" {
		t.Errorf("reference = %q, want the nickname — it is how the window names it back", rows[0].Reference)
	}
	if !rows[0].Terminal {
		t.Error("an Alacritty window should be marked as a terminal in the listing")
	}
	if rows[0].Source != string(managed.SourceAcquired) {
		t.Errorf("source = %q, want %q", rows[0].Source, managed.SourceAcquired)
	}
}

// A row with no unambiguous way to name its window carries no reference, and
// the surfaces offer no button rather than releasing a sibling.
func TestAmbiguousWindowsCarryNoReference(t *testing.T) {
	twins := []desktop.Window{
		{Address: "0xa", Class: "Alacritty", Title: "shell", Workspace: 1, WorkspaceName: "1",
			Focused: true, StableID: "sa", AcceptsInput: true},
		{Address: "0xb", Class: "Alacritty", Title: "shell", Workspace: 1, WorkspaceName: "1",
			StableID: "sb", AcceptsInput: true},
	}
	dir := t.TempDir()
	store := managed.NewStore(filepath.Join(dir, "managed.toml"), managed.StoreOptions{}, nil)
	d := NewDesktop(DesktopOptions{Compositor: desktop.NewFakeCompositor(twins...), Managed: store})
	if _, _, err := store.Acquire(twins[0], twins); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	rows, err := d.ManagedWindowListings(context.Background())
	if err != nil {
		t.Fatalf("ManagedWindowListings: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("listings = %+v, want one row", rows)
	}
	if rows[0].Reference != "" {
		t.Errorf("reference = %q, want none — nothing distinguishes this window in words", rows[0].Reference)
	}
}

// ------------------------------------------------------- managed from birth

// A window Jarvix launched is managed from the moment it appears, recognised
// by the identity the launch gave it (#198).
func TestALaunchedTerminalWindowIsManagedFromBirth(t *testing.T) {
	h := newManagedHarness(t, DesktopOptions{})
	terminalMachine(t, h.harness, "claude")
	out := h.run(t, LaunchAppToolName, map[string]any{"app": "claude"})
	if !strings.Contains(out, "Started claude inside ghostty") {
		t.Fatalf("launch = %q, want the terminal launch", out)
	}

	// The window appears wearing the class the launch asked for.
	opened := desktop.Window{Address: "0x9", Class: "dev.jarvix.claude", Title: "claude",
		Workspace: 1, WorkspaceName: "1", StableID: "s9", AcceptsInput: true}
	rec, ok := h.store.Managed(opened, []desktop.Window{opened})
	if !ok {
		t.Fatal("a window Jarvix opened should be managed the moment it appears")
	}
	if rec.Source != managed.SourceLaunched {
		t.Errorf("source = %q, want %q", rec.Source, managed.SourceLaunched)
	}
	if rec.Program != "claude" {
		t.Errorf("program = %q, want %q", rec.Program, "claude")
	}
}

// A graphical launch carries no identity of ours, so nothing is claimed. The
// alternative — adopting whichever window appeared next — is a guess, and
// honest absence beats a lucky guess.
func TestAGraphicalLaunchClaimsNothing(t *testing.T) {
	h := newManagedHarness(t, DesktopOptions{})
	terminalMachine(t, h.harness, "firefox")
	h.run(t, LaunchAppToolName, map[string]any{"app": "firefox"})
	if got := h.store.Count(); got != 0 {
		t.Errorf("count = %d, want 0 — a graphical launch has no identity to claim", got)
	}
}

// ------------------------------------------------------------ the job seam

// A job runs in a managed window and cannot act in an unmanaged one. This is
// the refusal #195's next slice depends on, pinned here so the rule cannot
// quietly become "whichever window it found".
func TestAJobRefusesAnUnmanagedWindow(t *testing.T) {
	h := newManagedHarness(t, DesktopOptions{})
	_, err := h.d.RequireManaged(context.Background(), "alacritty")
	if err == nil {
		t.Fatal("a job must not be allowed to act in an unmanaged window")
	}
	if !errors.Is(err, ErrNotManaged) {
		t.Fatalf("err = %v, want it to be ErrNotManaged so callers can key on it", err)
	}
	if !strings.Contains(err.Error(), "take control") {
		t.Errorf("err = %v, want the refusal to say the way out", err)
	}

	h.run(t, ManageWindowToolName, map[string]any{"window": "alacritty"})
	got, err := h.d.RequireManaged(context.Background(), "alacritty")
	if err != nil {
		t.Fatalf("RequireManaged after acquisition: %v", err)
	}
	if got.Class != "Alacritty" {
		t.Errorf("window = %+v, want the terminal", got)
	}
}

// Management ends with the window. Nothing may be left claiming one that no
// longer exists.
func TestManagementEndsWhenTheWindowCloses(t *testing.T) {
	h := newManagedHarness(t, DesktopOptions{})
	h.run(t, ManageWindowToolName, map[string]any{"window": "alacritty"})

	// The terminal closes: the compositor's inventory no longer has it.
	remaining := testWindows()[:3]
	h.comp.SetWindows(remaining...)
	h.d.invalidate()

	listed := h.run(t, ListManagedToolName, nil)
	if !strings.Contains(listed, "not managing any windows") {
		t.Errorf("listing = %q, want nothing managed once the window has gone", listed)
	}
	if _, err := h.d.RequireManaged(context.Background(), "alacritty"); err == nil {
		t.Error("a closed window must not still satisfy a job's managed check")
	}
}

// Management survives a daemon restart, because the window does.
func TestManagementSurvivesARestart(t *testing.T) {
	h := newManagedHarness(t, DesktopOptions{})
	h.run(t, ManageWindowToolName, map[string]any{"window": "alacritty"})

	// A restart is a new store over the same file, and new window tools over
	// the same compositor.
	restarted := NewDesktop(DesktopOptions{
		Compositor: desktop.NewFakeCompositor(testWindows()...),
		Managed:    managed.NewStore(h.path, managed.StoreOptions{}, nil),
	})
	if _, err := restarted.RequireManaged(context.Background(), "alacritty"); err != nil {
		t.Fatalf("management should survive a restart: %v", err)
	}
}

// ------------------------------------------------------------ typing is off

// Acquisition still works with typing switched off — reading and placement
// are most of what management is for — and the refusal to type is said at the
// earliest honest moment rather than discovered when nothing happens.
func TestTypingSwitchedOffIsSaidWhenTheWindowIsHandedOver(t *testing.T) {
	h := newManagedHarness(t, DesktopOptions{TypingEnabled: func() bool { return false }})

	_, summary, ok := confirm(t, h.tool(t, ManageWindowToolName), map[string]any{"window": "alacritty"})
	if !ok {
		t.Fatal("acquisition should still be offered when typing is off")
	}
	if !strings.Contains(summary, "typing is switched off in your configuration") {
		t.Errorf("summary = %q, want it to say typing is off before the user answers", summary)
	}
	if !strings.Contains(summary, "tools.typing") {
		t.Errorf("summary = %q, want it to name the setting", summary)
	}

	out := h.run(t, ManageWindowToolName, map[string]any{"window": "alacritty"})
	if !strings.Contains(out, "not type in it") {
		t.Errorf("acquire = %q, want the limitation named in the result too", out)
	}
	if _, err := h.d.RequireManaged(context.Background(), "alacritty"); err != nil {
		t.Fatalf("acquisition must still work for reading and placement: %v", err)
	}
}

// ------------------------------------------------------- absent by default

// A daemon with no store behaves exactly as one did before #197: no tools,
// nothing managed, and the job seam refusing everything.
func TestWithoutAStoreNothingIsManagedAndNoToolsAppear(t *testing.T) {
	h := newHarness(t)
	for _, tool := range h.d.Tools() {
		switch tool.Name() {
		case ManageWindowToolName, ReleaseWindowToolName, ListManagedToolName:
			t.Errorf("%s is offered on a daemon with no managed-window store", tool.Name())
		}
	}
	if got := h.d.ManagedCount(); got != 0 {
		t.Errorf("count = %d, want 0", got)
	}
	if got := h.d.ManagedByAddress(testWindows()); got != nil {
		t.Errorf("managed set = %v, want nil", got)
	}
	if _, err := h.d.RequireManaged(context.Background(), "alacritty"); err == nil {
		t.Error("a daemon with no store must refuse a job's managed check")
	}
}

// The overlay feed's seams: which windows carry the mark, and the cheap gate
// that keeps the poll asleep when nothing does.
func TestTheOverlayFeedSeesTheManagedWindow(t *testing.T) {
	h := newManagedHarness(t, DesktopOptions{})
	if got := h.d.ManagedCount(); got != 0 {
		t.Fatalf("count = %d before anything is managed, want 0", got)
	}
	h.run(t, ManageWindowToolName, map[string]any{"window": "alacritty"})
	if got := h.d.ManagedCount(); got != 1 {
		t.Errorf("count = %d, want 1 — the feed must wake for a managed window", got)
	}
	marks := h.d.ManagedByAddress(testWindows())
	if !marks[theTerminal().Address] {
		t.Errorf("managed set = %v, want the terminal marked", marks)
	}
	if len(marks) != 1 {
		t.Errorf("managed set = %v, want exactly the one window handed over", marks)
	}
}
