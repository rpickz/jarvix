package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/doctor"
	"github.com/rpickz/jarvix/internal/hotkey"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/setup"
	"github.com/rpickz/jarvix/internal/tts/kokoro"
	"github.com/rpickz/jarvix/internal/tts/piper"
	"github.com/rpickz/jarvix/internal/voice"
)

// cmdSetupWizard runs the first-run wizard: idempotent steps for the voice
// engine, activation, AI provider, advisor CLIs, and a final verification.
// All logic lives in internal/setup behind injected dependencies; this
// function only wires the real environment in.
func cmdSetupWizard(cfg config.Config, paths config.Paths) error {
	file, err := setup.LoadFile(paths.ConfigFile())
	if err != nil {
		return err
	}
	prompt := setup.NewTerminalPrompter(os.Stdin, os.Stdout)
	out := os.Stdout

	fmt.Println("Jarvix setup — walks through the machine-specific choices, verifying")
	fmt.Println("each one. Every step is safe to re-run; finished steps are skipped.")
	fmt.Println("Config file:", paths.ConfigFile())

	piperReady := func() error {
		if _, err := exec.LookPath(cfg.TTS.Piper.Binary); err != nil {
			return fmt.Errorf("%s not found in PATH", cfg.TTS.Piper.Binary)
		}
		s := &piper.Synthesizer{Binary: cfg.TTS.Piper.Binary, Voice: cfg.TTS.Piper.Voice}
		return s.ResolveVoice()
	}
	kokoroReady := func() error {
		k := &kokoro.Synthesizer{Voice: cfg.TTS.Kokoro.Voice, Speed: cfg.TTS.Kokoro.Speed}
		return k.Ready()
	}

	var roundTrip func() error
	if client, err := ipc.Dial(paths.Socket); err == nil {
		_ = client.Close()
		roundTrip = func() error { return cmdListen(paths) }
	}

	w := &setup.Wizard{
		Out:    out,
		Prompt: prompt,
		Save:   file.Save,
		Steps: []setup.Step{
			setup.TTSStep(setup.TTSDeps{
				File: file, Out: out, Prompt: prompt,
				Provider:          cfg.TTS.Provider,
				PiperReady:        piperReady,
				KokoroReady:       kokoroReady,
				KokoroSetupScript: findScript("setup-kokoro.sh"),
				RunScript:         runScript,
			}),
			// Language and voice come straight after the engine step because
			// they are only meaningful once an engine is chosen: the catalog
			// consulted, the config key written, and the install hint for a
			// missing language all depend on which one it is.
			setup.VoiceStep(setup.VoiceDeps{
				File: file, Out: out, Prompt: prompt,
				// Read through the file the previous step just wrote, so
				// installing Kokoro and then choosing a British voice works
				// in one run of the wizard.
				Provider: func() string { return effectiveProvider(file, cfg) },
				Catalog: func(provider string) voice.Catalog {
					return withProvider(cfg, provider).InstalledVoices(paths)
				},
				Current:       func(provider string) string { return activeVoice(withProvider(cfg, provider)) },
				WhisperModel:  cfg.STT.Whisper.Model,
				Preview:       func(id string) error { return previewVoice(withProvider(cfg, effectiveProvider(file, cfg)), id) },
				DownloadModel: func(model string) error { return cmdSetupWhisper(paths, model) },
			}),
			setup.ActivationStep(setup.ActivationDeps{
				Out: out, Prompt: prompt,
				InputAccessible:   hotkey.Accessible,
				BindingsInstalled: hyprlandBindingsInstalled,
				SetupInput:        cmdSetupInput,
				BindingsScript:    findScript("install-hyprland-bindings.sh"),
				RunScript:         runScript,
			}),
			setup.AIStep(setup.AIDeps{
				File: file, Out: out, Prompt: prompt,
				Ollama:       &setup.HTTPOllama{},
				DefaultModel: cfg.AI.Model,
			}),
			setup.AdvisorsStep(setup.AdvisorsDeps{
				File: file, Out: out, Prompt: prompt,
				LookPath: exec.LookPath,
			}),
			setup.VerifyStep(setup.VerifyDeps{
				Out: out, Prompt: prompt,
				Doctor: func() []doctor.Result {
					// Reload so the checks see what the wizard just wrote.
					fresh, err := config.Load(paths.ConfigFile())
					if err != nil {
						fresh = cfg
					}
					return doctor.Run(fresh, paths)
				},
				SetupWhisper: func() error { return cmdSetupWhisper(paths, cfg.STT.Whisper.Model) },
				RoundTrip:    roundTrip,
			}),
		},
	}
	err = w.Run()
	fmt.Println("\nSetup finished. Re-run `jarvix setup` any time; `jarvix doctor` re-checks.")
	return err
}

// effectiveProvider reports the TTS engine the wizard is configuring right
// now: what the run has already written to config.toml if anything, otherwise
// what the loaded configuration says. The wizard's steps are built up front
// but run in sequence, so a step must never read a value an earlier step has
// since changed.
func effectiveProvider(file *setup.File, cfg config.Config) string {
	if provider, ok := file.Get("tts", "provider"); ok && provider != "" {
		return provider
	}
	return cfg.TTS.Provider
}

// withProvider returns cfg as if that engine were selected, so provider-keyed
// helpers (catalog, active voice, preview) answer for the engine being
// configured rather than the one that was configured at start-up.
func withProvider(cfg config.Config, provider string) config.Config {
	cfg.TTS.Provider = provider
	return cfg
}

// findScript locates one of the repo's helper scripts on this installation.
// The wizard delegates its heavy steps (Kokoro setup, Hyprland bindings) to
// them, so failing to find one silently downgrades `jarvix setup` — the
// prompts for those steps simply never appear.
//
// Every place the scripts can plausibly live is searched, in order of how
// specific it is to this installation:
//
//   - /usr/share/jarvix/scripts — distribution package
//   - <exe>/../scripts and <exe>/scripts — release tarball unpacked whole, or
//     a repo checkout running out of ./bin
//   - $XDG_DATA_HOME/jarvix/scripts (~/.local/share/jarvix/scripts) — the
//     tarball's documented manual install, where only the two binaries go to
//     ~/.local/bin and the helpers need a stable home of their own; without
//     this entry that install could never run the delegated steps
//     (raised in review of #20)
//   - ./scripts — running from the checkout root
//
// Returns "" when the script is nowhere to be found.
func findScript(name string) string {
	candidates := []string{
		filepath.Join("/usr/share/jarvix/scripts", name), // installed package
	}
	if exe, err := os.Executable(); err == nil {
		// Release tarball or repo checkout: <root>/bin/jarvix + <root>/scripts.
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "..", "scripts", name))
		// Binaries and scripts installed side by side in one directory.
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "scripts", name))
	}
	// User-local data dir, resolved through the same XDG rules as everything
	// else Jarvix stores.
	candidates = append(candidates, filepath.Join(config.DefaultPaths().Data, "scripts", name))
	candidates = append(candidates, filepath.Join("scripts", name))
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			abs, err := filepath.Abs(c)
			if err != nil {
				return c
			}
			return abs
		}
	}
	return ""
}

// runScript executes a helper script interactively, inheriting the wizard's
// terminal so its prompts and progress are visible.
func runScript(path string) error {
	cmd := exec.Command("/bin/bash", path)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// hyprlandBindingsInstalled reports whether the managed Jarvix block is in
// ~/.config/hypr/bindings.lua (the marker install-hyprland-bindings.sh
// writes).
func hyprlandBindingsInstalled() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	data, err := os.ReadFile(filepath.Join(home, ".config", "hypr", "bindings.lua"))
	return err == nil && strings.Contains(string(data), "JARVIX BINDINGS")
}
