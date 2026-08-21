package daemon

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/tools"
	"github.com/rpickz/jarvix/internal/tts"
)

// The typing tools are wired at boot from configuration alone — no wtype and
// no Wayland session are needed to build a daemon, which is why these tests
// need neither, and why nothing here can type.

// TestTypingToolsAreAbsentByDefault: the shipped configuration builds a daemon
// that cannot type. This is the acceptance criterion the whole feature hangs
// off, asserted where a regression would actually happen — the registry.
func TestTypingToolsAreAbsentByDefault(t *testing.T) {
	d := daemonWith(t, testConfig())
	if names := strings.Join(d.registry.Names(), ","); strings.Contains(names, "typing.") {
		t.Errorf("registered tools = %q, want no typing tools without opting in", names)
	}
}

func TestTypingToolsAreRegisteredWhenEnabled(t *testing.T) {
	cfg := testConfig()
	cfg.Tools.Typing.Enable = true
	d := daemonWith(t, cfg)
	names := strings.Join(d.registry.Names(), ",")
	for _, want := range []string{tools.TypeTextToolName, tools.PressKeyToolName} {
		if !strings.Contains(names, want) {
			t.Errorf("registered tools = %q, missing %q", names, want)
		}
	}
	// Both ask, whatever else is configured, and `jarvix status` says so.
	policy := d.effectivePolicy()
	tiers, _ := policy["tools"].(map[string]string)
	for _, name := range []string{tools.TypeTextToolName, tools.PressKeyToolName} {
		if tiers[name] != "ask" {
			t.Errorf("%s tier = %q, want ask", name, tiers[name])
		}
	}
}

// TestTypingWorksWithoutTheWindowTools: typing borrows the window inventory,
// so it has to build one even when the desktop verbs are switched off — and it
// must not register them as a side effect.
func TestTypingWorksWithoutTheWindowTools(t *testing.T) {
	cfg := testConfig()
	cfg.Tools.Desktop = false
	cfg.Tools.Typing.Enable = true
	d := daemonWith(t, cfg)
	names := strings.Join(d.registry.Names(), ",")
	if strings.Contains(names, "desktop.") {
		t.Errorf("registered tools = %q, want no window tools", names)
	}
	if !strings.Contains(names, tools.TypeTextToolName) {
		t.Errorf("registered tools = %q, want the typing tools", names)
	}
}

// TestSystemPromptFollowsTheTypingTools: the model is told it can type only
// when it can, and the sentence that keeps typing and submitting apart is part
// of what it is told.
func TestSystemPromptFollowsTheTypingTools(t *testing.T) {
	cfg := testConfig()
	if prompt := assistantSystemPrompt(cfg); strings.Contains(prompt, "type into the window") {
		t.Error("system prompt mentions typing when it is switched off")
	}
	cfg.Tools.Typing.Enable = true
	prompt := assistantSystemPrompt(cfg)
	if !strings.Contains(prompt, "type into the window") {
		t.Error("system prompt does not mention the typing tools")
	}
	if !strings.Contains(prompt, "Typing never submits") {
		t.Error("system prompt must state that typing does not submit")
	}
}

// TestTypingAuditIsRetainedForStatus: the audit trail `jarvix status --last`
// prints comes off the bus, and what it retains is the window, the length and
// the outcome — the event does not carry the text, so the trail cannot.
func TestTypingAuditIsRetainedForStatus(t *testing.T) {
	d := daemonWith(t, testConfig())
	if d.lastTypingReport() != nil {
		t.Fatal("nothing should be retained before anything is typed")
	}
	d.setLastTyping(map[string]any{
		"tool": tools.TypeTextToolName, "window": "code — engine.go",
		"chars": 11, "approved": true, "outcome": "typed",
	})
	got := d.lastTypingReport()
	if got["window"] != "code — engine.go" || got["outcome"] != "typed" {
		t.Fatalf("retained = %v", got)
	}
	for key := range got {
		if key == "text" || key == "payload" {
			t.Errorf("the retained audit must never hold the typed text: %v", got)
		}
	}
}

// TestTypingRunsThroughTheDaemonsOwnWiring is the end-to-end one: a daemon
// built from configuration, with the two seams injected, asked to type through
// its own registry and its own permission gate.
//
// The injection is not a convenience here, it is the safety property. A daemon
// built with the real seams would drive hyprctl and wtype, and the wtype half
// would type into the session running the test. Deps.Keyboard exists so that
// cannot happen.
func TestTypingRunsThroughTheDaemonsOwnWiring(t *testing.T) {
	cfg := testConfig()
	cfg.Tools.Typing.Enable = true

	comp := desktop.NewFakeCompositor(desktop.Window{
		Address: "0x1", Class: "obsidian", Title: "Daily note", Workspace: 1,
		Focused: true, StableID: "s1", AcceptsInput: true,
	})
	kb := &desktop.FakeKeyboard{}
	dir := t.TempDir()
	d, err := New(cfg, config.Paths{Config: dir, Data: dir, State: dir, Runtime: dir,
		Socket: filepath.Join(dir, "j.sock")}, nil, Deps{
		Provider:    &ai.Fake{},
		Transcriber: &stt.Fake{},
		Synthesizer: &tts.Fake{},
		Recorder:    &audio.FakeRecorder{Clip: audio.Clip{WAVPath: dir + "/r.wav"}},
		Player:      &audio.FakePlayer{},
		Notifier:    &desktop.FakeNotifier{},
		Compositor:  comp,
		Keyboard:    kb,
		OpenWindow:  func(context.Context) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}

	call := ai.ToolCall{Name: tools.TypeTextToolName, Arguments: `{"text":"call the bank at three"}`}
	verdict := d.registry.Check(call)
	if verdict.Decision != tools.PolicyAsk {
		t.Fatalf("decision = %q, want ask", verdict.Decision)
	}
	for _, want := range []string{"call the bank at three", "obsidian — Daily note"} {
		if !strings.Contains(verdict.Summary, want) {
			t.Errorf("summary = %q, want %q", verdict.Summary, want)
		}
	}
	if strings.Contains(verdict.Command, "call the bank") {
		t.Errorf("command = %q is logged verbatim; it must not carry the text", verdict.Command)
	}

	if result := d.registry.Execute(context.Background(), call); !strings.Contains(result, "Typed the text") {
		t.Fatalf("result = %q", result)
	}
	if got := kb.Typed(); len(got) != 1 || got[0] != "call the bank at three" {
		t.Fatalf("typed = %q", got)
	}
}
