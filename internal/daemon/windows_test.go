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

// The window tools are wired at boot, so what a daemon offers the model is
// decided by configuration alone — no compositor is required to build one,
// which is also why these tests need no Hyprland.

func daemonWith(t *testing.T, cfg config.Config) *Daemon {
	t.Helper()
	dir := t.TempDir()
	d, err := New(cfg, config.Paths{Config: dir, Data: dir, State: dir, Runtime: dir,
		Socket: filepath.Join(dir, "j.sock")}, nil, Deps{
		Provider:    &ai.Fake{},
		Transcriber: &stt.Fake{},
		Synthesizer: &tts.Fake{},
		Recorder:    &audio.FakeRecorder{Clip: audio.Clip{WAVPath: dir + "/r.wav"}},
		Player:      &audio.FakePlayer{},
		Notifier:    &desktop.FakeNotifier{},
		OpenWindow:  func(context.Context) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestWindowToolsAreRegisteredWhenEnabled(t *testing.T) {
	d := daemonWith(t, testConfig())
	names := strings.Join(d.registry.Names(), ",")
	for _, want := range []string{
		tools.ListWindowsToolName, tools.FocusWindowToolName, tools.MoveWindowToolName,
		tools.CloseWindowToolName, tools.LaunchAppToolName,
	} {
		if !strings.Contains(names, want) {
			t.Errorf("registered tools = %q, missing %q", names, want)
		}
	}
}

func TestWindowToolsAreAbsentWhenDisabled(t *testing.T) {
	cfg := testConfig()
	cfg.Tools.Desktop = false
	d := daemonWith(t, cfg)
	if names := strings.Join(d.registry.Names(), ","); strings.Contains(names, "desktop.") {
		t.Errorf("registered tools = %q, want no window tools", names)
	}
}

// The model is told about the tools it has, and only those: a system prompt
// describing windows it cannot move would be an invitation to hallucinate one.
func TestSystemPromptFollowsTheWindowTools(t *testing.T) {
	cfg := testConfig()
	if prompt := assistantSystemPrompt(cfg); !strings.Contains(prompt, "act on the user's desktop") {
		t.Error("system prompt does not mention the window tools")
	}
	cfg.Tools.Desktop = false
	if prompt := assistantSystemPrompt(cfg); strings.Contains(prompt, "act on the user's desktop") {
		t.Error("system prompt mentions window tools that are switched off")
	}
}
