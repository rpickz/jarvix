package desktop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The compositor tests never need a running Hyprland: the argv builders and
// the inventory parser are pure, and the process paths run against stub
// binaries, the same pattern the notification and speech-engine tests use.

func TestDispatchArgvIsFixedAndAddressIsOneElement(t *testing.T) {
	const addr = "0x55d8e53e7c60"
	tests := []struct {
		name    string
		build   dispatchArgs
		dialect dispatchDialect
		ws      int
		want    []string
	}{
		{"focus lua", focusArgs, dialectLua, 0,
			[]string{"dispatch", `hl.dsp.focus({ window = "address:0x55d8e53e7c60" })`}},
		{"focus legacy", focusArgs, dialectLegacy, 0,
			[]string{"dispatch", "focuswindow", "address:0x55d8e53e7c60"}},
		{"close lua", closeArgs, dialectLua, 0,
			[]string{"dispatch", `hl.dsp.window.close({ window = "address:0x55d8e53e7c60" })`}},
		{"close legacy", closeArgs, dialectLegacy, 0,
			[]string{"dispatch", "closewindow", "address:0x55d8e53e7c60"}},
		{"move lua", moveArgs, dialectLua, 3,
			[]string{"dispatch", `hl.dsp.window.move({ workspace = 3, window = "address:0x55d8e53e7c60", follow = false })`}},
		{"move legacy", moveArgs, dialectLegacy, 3,
			[]string{"dispatch", "movetoworkspacesilent", "3,address:0x55d8e53e7c60"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.build(tt.dialect, addr, tt.ws)
			if len(got) != len(tt.want) {
				t.Fatalf("argv = %q, want %q", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("argv[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
			// No shell anywhere: whatever the compositor is told, it arrives
			// as argv elements, never as a command line.
			for _, arg := range got {
				if arg == "-c" || strings.HasPrefix(arg, "sh ") {
					t.Errorf("argv %q looks like a shell invocation", got)
				}
			}
		})
	}
}

// TestWorkspaceAndSpawnArgvPerDialect pins the two dispatches the
// deterministic intents make (#47). The legacy row is the form every script
// on the internet uses and a Lua parse error on a Lua-configured compositor;
// the Lua row is what actually reaches a current Omarchy desktop.
func TestWorkspaceAndSpawnArgvPerDialect(t *testing.T) {
	tests := []struct {
		name    string
		got     []string
		want    []string
		mustNot string
	}{
		{"workspace lua", workspaceArgs(dialectLua, 4),
			[]string{"dispatch", `hl.dsp.focus({ workspace = 4 })`}, ""},
		{"workspace legacy", workspaceArgs(dialectLegacy, 4),
			[]string{"dispatch", "workspace", "4"}, ""},
		{"spawn lua", spawnArgs(dialectLua, "alacritty"),
			[]string{"dispatch", `hl.dsp.exec_cmd("alacritty")`}, ""},
		{"spawn legacy", spawnArgs(dialectLegacy, "alacritty"),
			[]string{"dispatch", "exec", "alacritty"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if strings.Join(tt.got, "\x00") != strings.Join(tt.want, "\x00") {
				t.Fatalf("argv = %q, want %q", tt.got, tt.want)
			}
			// Whatever the dialect, the whole dispatch is one argv element
			// after the verb: the compositor parses it, a shell never sees it.
			if len(tt.got) > 3 {
				t.Errorf("argv = %q, want at most a verb and its argument", tt.got)
			}
		})
	}
}

func TestSwitchWorkspaceAndSpawnRefuseNonsenseBeforeDispatching(t *testing.T) {
	bin := writeStub(t, "hyprctl", "#!/bin/sh\nprintf 'ok\\n'\n")
	h := &Hyprland{Binary: bin}
	for _, ws := range []int{0, -1, 100, 1000} {
		if err := h.SwitchWorkspace(context.Background(), ws); err == nil {
			t.Errorf("SwitchWorkspace(%d) was dispatched", ws)
		}
	}
	// A quote or a space would be Lua syntax inside hl.dsp.exec_cmd("…"), and
	// the compositor runs that string through a shell — so neither may ever
	// reach argv.
	for _, program := range []string{
		"", "alacritty --title x", `alacritty") os.execute("id`, "foot; id", "$(id)", "a`b`",
	} {
		if err := h.Spawn(context.Background(), program); err == nil {
			t.Errorf("Spawn(%q) was dispatched", program)
		}
	}
}

func TestSwitchWorkspaceUsesTheProbedDialect(t *testing.T) {
	for _, tt := range []struct {
		name  string
		luaOK bool
		want  string
	}{
		{"lua", true, `dispatch hl.dsp.focus({ workspace = 4 })`},
		{"legacy", false, "dispatch workspace 4"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			bin, dir := hyprStub(t, tt.luaOK, "[]")
			h := &Hyprland{Binary: bin}
			if err := h.SwitchWorkspace(context.Background(), 4); err != nil {
				t.Fatalf("SwitchWorkspace: %v", err)
			}
			got := calls(t, dir)
			if len(got) == 0 || got[len(got)-1] != tt.want {
				t.Fatalf("calls = %q, want a %s dispatch %q", got, tt.name, tt.want)
			}
			// The probe is the seam's, made once: "open a terminal" straight
			// after must not pay for a second one.
			before := len(got)
			if err := h.Spawn(context.Background(), "alacritty"); err != nil {
				t.Fatalf("Spawn: %v", err)
			}
			if got = calls(t, dir); len(got) != before+1 {
				t.Fatalf("calls = %q, want one more dispatch and no re-probe", got)
			}
		})
	}
}

// TestRefusedWorkspaceDispatchIsAFailure is the deeper half of #47: hyprctl
// exits 0 for a dispatch the compositor refused, so a workspace switch that
// did not happen used to be indistinguishable from one that did.
func TestRefusedWorkspaceDispatchIsAFailure(t *testing.T) {
	bin := writeStub(t, "hyprctl", `#!/bin/sh
case "$2" in
  hl.dsp.no_op*) printf 'ok\n' ;;
  *) printf 'warning: =[C]:-1: hl.focus: workspace not found\n' ;;
esac
`)
	h := &Hyprland{Binary: bin}
	err := h.SwitchWorkspace(context.Background(), 4)
	if err == nil || !strings.Contains(err.Error(), "workspace not found") {
		t.Errorf("err = %v, want the compositor's refusal rather than its exit code", err)
	}
	if err := h.Spawn(context.Background(), "alacritty"); err == nil {
		t.Error("a refused spawn reported success")
	}
}

func TestDispatchRefusesMalformedAddress(t *testing.T) {
	bin := writeStub(t, "hyprctl", "#!/bin/sh\nprintf 'ok\\n'\n")
	h := &Hyprland{Binary: bin}
	for _, addr := range []string{
		"", "firefox", "0x", "address:0x1", "0x1; rm -rf /", "0xzz", "$(id)",
	} {
		if err := h.Focus(context.Background(), addr); err == nil {
			t.Errorf("Focus(%q) was dispatched; a malformed address must never reach argv", addr)
		}
	}
}

func TestParseClientsReadsTheFieldsMatchingNeeds(t *testing.T) {
	windows, err := parseClients(`[
		{"address":"0xaaa","class":"md.obsidian.Obsidian","title":"notes",
		 "workspace":{"id":2,"name":"2"},"floating":false,"acceptsInput":true,
		 "mapped":true,"hidden":false,"focusHistoryID":3,"stableId":"180007e8","pid":42},
		{"address":"0xbbb","class":"firefox","title":"GitHub",
		 "workspace":{"id":1,"name":"1"},"floating":true,"acceptsInput":true,
		 "mapped":true,"focusHistoryID":0,"stableId":12345,"pid":7},
		{"address":"0xccc","class":"ghost","title":"not mapped",
		 "workspace":{"id":1,"name":"1"},"mapped":false,"focusHistoryID":1}
	]`)
	if err != nil {
		t.Fatal(err)
	}
	// Unmapped windows are dropped, and the focused one comes first.
	if len(windows) != 2 {
		t.Fatalf("windows = %+v, want the two mapped ones", windows)
	}
	first := windows[0]
	if first.Address != "0xbbb" || !first.Focused || !first.Floating {
		t.Errorf("first window = %+v, want the focused floating firefox", first)
	}
	if first.StableID != "12345" {
		t.Errorf("numeric stableId = %q, want it decoded as text", first.StableID)
	}
	second := windows[1]
	if second.Workspace != 2 || second.WorkspaceName != "2" || second.PID != 42 ||
		!second.AcceptsInput || second.StableID != "180007e8" || second.Focused {
		t.Errorf("second window = %+v", second)
	}
}

func TestParseClientsRejectsGarbage(t *testing.T) {
	if _, err := parseClients("not json"); err == nil {
		t.Error("malformed inventory must be an error, not an empty desktop")
	}
	windows, err := parseClients("[]")
	if err != nil || len(windows) != 0 {
		t.Errorf("empty inventory: %+v, %v", windows, err)
	}
}

func TestAppNameSpeaksTheApplicationNotTheClass(t *testing.T) {
	tests := []struct{ class, want string }{
		{"md.obsidian.Obsidian", "Obsidian"},
		{"org.gnome.Nautilus", "Nautilus"},
		{"com.mitchellh.ghostty", "ghostty"},
		{"firefox", "firefox"},
		{"chrome-chatgpt.com__-Profile_3", "chrome-chatgpt.com__-Profile_3"},
		// Real classes seen on a live desktop: a browser profile is not a
		// reverse-DNS name, and "com__-Default" is not an application.
		{"chrome-web.whatsapp.com__-Default", "chrome-web.whatsapp.com__-Default"},
		{"a.b.", "a.b."},
		{"", ""},
	}
	for _, tt := range tests {
		if got := AppName(tt.class); got != tt.want {
			t.Errorf("AppName(%q) = %q, want %q", tt.class, got, tt.want)
		}
	}
}

func TestWindowDescribe(t *testing.T) {
	tests := []struct {
		win  Window
		want string
	}{
		{Window{Class: "firefox", Title: "GitHub"}, "firefox — GitHub"},
		{Window{Class: "firefox", Title: ""}, "firefox"},
		{Window{Class: "", Title: "GitHub"}, "GitHub"},
		{Window{Class: "Slack", Title: "slack"}, "Slack"},
	}
	for _, tt := range tests {
		if got := tt.win.Describe(); got != tt.want {
			t.Errorf("Describe(%+v) = %q, want %q", tt.win, got, tt.want)
		}
	}
}

// hyprStub writes a hyprctl stub that records its argv and answers the three
// calls the driver makes. luaOK decides which dispatch dialect it accepts,
// which is how the dialect probe is tested without either Hyprland.
func hyprStub(t *testing.T, luaOK bool, clients string) (bin, dir string) {
	t.Helper()
	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "clients.json"), []byte(clients), 0o644); err != nil {
		t.Fatal(err)
	}
	lua := "0"
	if luaOK {
		lua = "1"
	}
	script := `#!/bin/sh
printf '%s\n' "$*" >> "` + dir + `/calls"
case "$1" in
  clients) cat "` + dir + `/clients.json" ;;
  version) printf '{"version":"0.56.2","tag":"v0.56.2"}\n' ;;
  dispatch)
    case "$2" in
      hl.dsp.*)
        if [ "` + lua + `" = "1" ]; then printf 'ok\n'; else printf 'Invalid dispatcher\n'; fi ;;
      *)
        if [ "` + lua + `" = "1" ]; then printf 'error: expected a dispatcher\n'; else printf 'ok\n'; fi ;;
    esac ;;
esac
`
	return writeStub(t, "hyprctl", script), dir
}

func calls(t *testing.T, dir string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func TestHyprlandDiscoversTheLuaDialect(t *testing.T) {
	bin, dir := hyprStub(t, true, "[]")
	h := &Hyprland{Binary: bin}
	if err := h.Focus(context.Background(), "0xabc"); err != nil {
		t.Fatalf("focus: %v", err)
	}
	got := calls(t, dir)
	if len(got) != 2 || !strings.Contains(got[0], "no_op") || !strings.Contains(got[1], "hl.dsp.focus") {
		t.Fatalf("calls = %q, want a probe then a lua focus", got)
	}
	// The dialect is discovered once and reused: a second action re-probes
	// nothing.
	if err := h.Close(context.Background(), "0xabc"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got = calls(t, dir); len(got) != 3 || !strings.Contains(got[2], "hl.dsp.window.close") {
		t.Fatalf("calls = %q, want the dialect reused", got)
	}
}

func TestHyprlandFallsBackToTheLegacyDialect(t *testing.T) {
	bin, dir := hyprStub(t, false, "[]")
	h := &Hyprland{Binary: bin}
	if err := h.MoveToWorkspace(context.Background(), "0xabc", 4); err != nil {
		t.Fatalf("move: %v", err)
	}
	got := calls(t, dir)
	if len(got) == 0 || !strings.Contains(got[len(got)-1], "movetoworkspacesilent 4,address:0xabc") {
		t.Fatalf("calls = %q, want the legacy dispatch", got)
	}
}

func TestHyprlandTreatsARefusalAsAFailure(t *testing.T) {
	// hyprctl exits 0 for a dispatch the compositor refused, so only the
	// output can distinguish "done" from "there is no such window".
	bin := writeStub(t, "hyprctl", `#!/bin/sh
case "$2" in
  hl.dsp.no_op*) printf 'ok\n' ;;
  *) printf 'warning: =[C]:-1: hl.focus: window not found\n' ;;
esac
`)
	h := &Hyprland{Binary: bin}
	err := h.Focus(context.Background(), "0xdeadbeef")
	if err == nil || !strings.Contains(err.Error(), "window not found") {
		t.Errorf("err = %v, want the compositor's refusal", err)
	}
}

func TestHyprlandWindowsAndDescribe(t *testing.T) {
	bin, _ := hyprStub(t, true, `[{"address":"0xaaa","class":"firefox","title":"GitHub",
		"workspace":{"id":1,"name":"1"},"mapped":true,"focusHistoryID":0,"acceptsInput":true}]`)
	h := &Hyprland{Binary: bin}
	windows, err := h.Windows(context.Background())
	if err != nil || len(windows) != 1 || windows[0].Class != "firefox" {
		t.Fatalf("windows = %+v, err = %v", windows, err)
	}
	described, err := h.Describe(context.Background())
	if err != nil || !strings.Contains(described, "0.56.2") || !strings.Contains(described, "lua") {
		t.Errorf("Describe() = %q, %v", described, err)
	}
}

func TestHyprlandUnavailableIsAnOrdinaryError(t *testing.T) {
	h := &Hyprland{Binary: filepath.Join(t.TempDir(), "not-installed")}
	if _, err := h.Windows(context.Background()); err == nil {
		t.Error("a missing hyprctl must be reported, not ignored")
	}
	if _, err := h.Describe(context.Background()); err == nil {
		t.Error("Describe must fail when there is no compositor")
	}
}

func TestFlexString(t *testing.T) {
	tests := []struct{ in, want string }{
		{`"180007e8"`, "180007e8"},
		{`12345`, "12345"},
		{`null`, ""},
	}
	for _, tt := range tests {
		var f flexString
		if err := f.UnmarshalJSON([]byte(tt.in)); err != nil {
			t.Fatalf("UnmarshalJSON(%s): %v", tt.in, err)
		}
		if string(f) != tt.want {
			t.Errorf("UnmarshalJSON(%s) = %q, want %q", tt.in, f, tt.want)
		}
	}
}

// A busy desktop must not read as no desktop: the inventory has its own cap,
// far above what a dispatch's diagnostics get, because a JSON document cut in
// half parses as "no compositor" and that is the worst possible answer.
func TestInventoryIsNotTruncatedByTheDiagnosticCap(t *testing.T) {
	var b strings.Builder
	b.WriteString("[")
	const windows = 60
	for i := 0; i < windows; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"address":"0x` + strings.Repeat("a", 8) + `","class":"firefox",` +
			`"title":"` + strings.Repeat("a long window title ", 20) + `",` +
			`"workspace":{"id":1,"name":"1"},"mapped":true,"focusHistoryID":` +
			strings.Repeat("0", 1) + `}`)
	}
	b.WriteString("]")
	if b.Len() <= maxDispatchOutput {
		t.Fatalf("test inventory is only %d bytes; it must exceed the dispatch cap", b.Len())
	}
	bin, _ := hyprStub(t, true, b.String())
	got, err := (&Hyprland{Binary: bin}).Windows(context.Background())
	if err != nil || len(got) != windows {
		t.Fatalf("windows = %d, err = %v, want %d", len(got), err, windows)
	}
}
