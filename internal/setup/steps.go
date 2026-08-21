package setup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/doctor"
)

// TTSDeps are the injected dependencies of the text-to-speech step.
type TTSDeps struct {
	File   *File
	Out    io.Writer
	Prompt Prompter
	// Provider is the effective tts.provider (defaults applied).
	Provider string
	// PiperReady reports whether the Piper binary and a voice are usable.
	PiperReady func() error
	// KokoroReady reports whether the Kokoro venv, helper, and models exist.
	KokoroReady func() error
	// KokoroSetupScript is the path of scripts/setup-kokoro.sh, or "" when
	// it cannot be found on this installation.
	KokoroSetupScript string
	// RunScript executes a shell script interactively.
	RunScript func(path string) error
}

// TTSStep configures the voice engine: Piper as the zero-setup default,
// Kokoro as the opt-in natural voice (delegating to setup-kokoro.sh).
func TTSStep(d TTSDeps) Step {
	return Step{
		Title: "Voice (text-to-speech)",
		Done: func() (bool, string) {
			if d.Provider == "kokoro" && d.KokoroReady() == nil {
				return true, "Kokoro is the configured voice and is ready"
			}
			if d.Provider == "piper" && d.PiperReady() == nil {
				return true, "Piper is the configured voice and is ready (zero-setup default)"
			}
			return false, ""
		},
		Run: func() error {
			piperErr := d.PiperReady()
			if piperErr == nil {
				fprintln(d.Out, "Piper is installed and has a voice — the zero-setup default works.")
			} else {
				fprintf(d.Out, "Piper is not ready: %v\n", piperErr)
				fprintln(d.Out, "Install it: sudo pacman -S piper-tts-bin piper-voices-en-us (AUR)")
			}

			useKokoro := false
			if d.KokoroReady() == nil {
				useKokoro = d.Prompt.Confirm("Kokoro (natural voice) is installed. Use it as the voice?", d.Provider == "kokoro")
			} else if d.KokoroSetupScript != "" &&
				d.Prompt.Confirm("Set up Kokoro, a much more natural voice (~340 MB download)?", false) {
				if err := d.RunScript(d.KokoroSetupScript); err != nil {
					return fmt.Errorf("Kokoro setup failed (%v) — re-run %s by hand, or stay on Piper", err, d.KokoroSetupScript)
				}
				useKokoro = true
			}
			if useKokoro {
				setValue(d.File, d.Prompt, d.Out, "tts", "provider", "kokoro")
				return nil
			}
			if piperErr != nil {
				return errors.New("no voice engine is ready — install Piper (sudo pacman -S piper-tts-bin piper-voices-en-us) or set up Kokoro, then re-run jarvix setup")
			}
			setValue(d.File, d.Prompt, d.Out, "tts", "provider", "piper")
			return nil
		},
	}
}

// ActivationDeps are the injected dependencies of the activation step.
type ActivationDeps struct {
	Out    io.Writer
	Prompt Prompter
	// InputAccessible reports whether keyboard event devices are readable
	// (real hold-to-talk works).
	InputAccessible func() bool
	// BindingsInstalled reports whether the Hyprland bindings block is in
	// place (tap-to-toggle fallback works).
	BindingsInstalled func() bool
	// SetupInput runs the existing `jarvix setup input` flow, which installs
	// the udev rule as root or prints the exact commands otherwise.
	SetupInput func() error
	// BindingsScript is the path of scripts/install-hyprland-bindings.sh, or
	// "" when it cannot be found.
	BindingsScript string
	// RunScript executes a shell script interactively.
	RunScript func(path string) error
}

// ActivationStep configures push-to-talk: keyboard access for daemon-side
// hold-to-talk, with the Hyprland tap-to-toggle bindings as the fallback.
func ActivationStep(d ActivationDeps) Step {
	return Step{
		Title: "Activation (push-to-talk)",
		Done: func() (bool, string) {
			if d.InputAccessible() {
				return true, "keyboard access granted — hold-to-talk is active"
			}
			if d.BindingsInstalled() {
				return true, "Hyprland bindings installed — tap-to-toggle works (grant keyboard access for real hold-to-talk: jarvix setup input)"
			}
			return false, ""
		},
		Run: func() error {
			instructionsGiven := false
			fprintln(d.Out, "Real hold-to-talk needs one-time read access to keyboard devices;")
			fprintln(d.Out, "without it, the same chord works as tap-to-toggle via Hyprland bindings.")
			if d.Prompt.Confirm("Grant keyboard access now (jarvix setup input)?", true) {
				if err := d.SetupInput(); err != nil {
					return fmt.Errorf("input setup failed: %v — run `jarvix setup input` by hand", err)
				}
				instructionsGiven = true
			}
			if d.BindingsScript != "" && !d.BindingsInstalled() &&
				d.Prompt.Confirm("Install the Hyprland key bindings (push-to-talk, cancel, window)?", true) {
				if err := d.RunScript(d.BindingsScript); err != nil {
					return fmt.Errorf("bindings install failed (%v) — re-run %s by hand", err, d.BindingsScript)
				}
			}
			if d.InputAccessible() || d.BindingsInstalled() {
				return nil
			}
			if instructionsGiven {
				fprintln(d.Out, "Follow the printed commands, then re-run `jarvix setup` to verify.")
				return nil
			}
			return errors.New("activation is not configured — run `jarvix setup input` for hold-to-talk, or scripts/install-hyprland-bindings.sh for the key bindings")
		},
	}
}

// OllamaDetector detects a locally running Ollama instance. It exists as an
// interface so tests never touch the network.
type OllamaDetector interface {
	// Models lists the installed model names; an error means Ollama is not
	// reachable.
	Models(ctx context.Context) ([]string, error)
}

// AIDeps are the injected dependencies of the AI provider step.
type AIDeps struct {
	File   *File
	Out    io.Writer
	Prompt Prompter
	Ollama OllamaDetector
	// DefaultModel is the effective ai.model (defaults applied).
	DefaultModel string
}

// cloudProviders are the preset non-local endpoints the wizard can select.
// Keys are read from the environment, matching config's design — the wizard
// never asks the user to paste one.
var cloudProviders = []struct{ name, keyEnv string }{
	{"openai", "OPENAI_API_KEY"},
	{"openrouter", "OPENROUTER_API_KEY"},
}

// AIStep selects the AI provider: a running Ollama when detected, otherwise
// a preset with environment-variable key instructions.
func AIStep(d AIDeps) Step {
	return Step{
		Title: "AI provider",
		Done: func() (bool, string) {
			if provider, ok := d.File.Get("ai", "provider"); ok {
				detail := "ai.provider = " + provider
				if model, ok := d.File.Get("ai", "model"); ok {
					detail += ", ai.model = " + model
				}
				return true, detail
			}
			return false, ""
		},
		Run: func() error {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			models, err := d.Ollama.Models(ctx)
			if err == nil {
				return chooseOllama(d, models)
			}
			fprintln(d.Out, "No running Ollama detected at 127.0.0.1:11434.")
			options := []string{
				"ollama — local and private; install from https://ollama.com, then: ollama pull " + d.DefaultModel,
			}
			for _, p := range cloudProviders {
				options = append(options, p.name+" — cloud; needs "+p.keyEnv+" in the environment")
			}
			options = append(options, "skip for now")
			choice := d.Prompt.Choose("Which AI provider should Jarvix use?", options, 0)
			if choice == len(options)-1 {
				fprintln(d.Out, "Skipped — Jarvix defaults to Ollama; `jarvix doctor` explains what is missing.")
				return nil
			}
			if choice == 0 {
				setValue(d.File, d.Prompt, d.Out, "ai", "provider", "ollama")
				fprintf(d.Out, "Ollama selected. Start it and pull the model:\n  sudo systemctl enable --now ollama\n  ollama pull %s\n", d.DefaultModel)
				return nil
			}
			p := cloudProviders[choice-1]
			setValue(d.File, d.Prompt, d.Out, "ai", "provider", p.name)
			model := d.Prompt.Input("Model name for "+p.name, d.DefaultModel)
			setValue(d.File, d.Prompt, d.Out, "ai", "model", model)
			fprintf(d.Out, "API keys are read from the environment, never stored in config.toml.\nExport it for your shell and for the daemon:\n  systemctl --user set-environment %s=...\n  systemctl --user restart jarvixd\n", p.keyEnv)
			return nil
		},
	}
}

func chooseOllama(d AIDeps, models []string) error {
	fprintf(d.Out, "Ollama is running with %d model(s) installed.\n", len(models))
	if !d.Prompt.Confirm("Use Ollama (local, private) as the AI provider?", true) {
		fprintln(d.Out, "Skipped — set [ai] provider/model in config.toml yourself, or re-run jarvix setup.")
		return nil
	}
	setValue(d.File, d.Prompt, d.Out, "ai", "provider", "ollama")
	if len(models) == 0 {
		fprintf(d.Out, "No models pulled yet — fetch the default: ollama pull %s\n", d.DefaultModel)
		return nil
	}
	def := 0
	for i, m := range models {
		if m == d.DefaultModel {
			def = i
		}
	}
	choice := d.Prompt.Choose("Which model should Jarvix use?", models, def)
	setValue(d.File, d.Prompt, d.Out, "ai", "model", models[choice])
	return nil
}

// KnownAdvisorCLIs are the assistant CLIs the wizard looks for on PATH.
// Detection is exec.LookPath only — no network, no invocation. The list is
// the shipped preset table itself, so what the wizard records is always
// something the runtime knows how to invoke.
var KnownAdvisorCLIs = config.KnownAdvisors()

// AdvisorsDeps are the injected dependencies of the advisor detection step.
type AdvisorsDeps struct {
	File   *File
	Out    io.Writer
	Prompt Prompter
	// LookPath resolves a binary on PATH (exec.LookPath in production).
	LookPath func(name string) (string, error)
}

// AdvisorsStep detects installed assistant CLIs and records them as
// [advisors.<name>] tables. Recording one is all delegation needs: the
// shipped preset supplies the non-interactive argv and timeout, and the
// assistant can then hand a question too big for the local model to that CLI
// and speak its answer (ADR 0016).
func AdvisorsStep(d AdvisorsDeps) Step {
	return Step{
		Title: "Advisor CLIs (stronger assistants)",
		Done: func() (bool, string) {
			if names := d.File.TablesWithPrefix("advisors."); len(names) > 0 {
				sort.Strings(names)
				return true, fmt.Sprintf("recorded: %v", names)
			}
			return false, ""
		},
		Run: func() error {
			found := make(map[string]string)
			var names []string
			for _, name := range KnownAdvisorCLIs {
				if path, err := d.LookPath(name); err == nil {
					found[name] = path
					names = append(names, name)
				}
			}
			if len(names) == 0 {
				fprintf(d.Out, "No known assistant CLIs found on PATH (looked for: %v).\nNothing to configure — re-run jarvix setup after installing one.\n", KnownAdvisorCLIs)
				return nil
			}
			fprintln(d.Out, "These assistant CLIs are installed; recording one lets Jarvix hand it a")
			fprintln(d.Out, "question too big for the local model and speak the answer (each keeps its")
			fprintln(d.Out, "own auth and billing; Jarvix never passes its own API keys on).")
			for _, name := range names {
				if d.Prompt.Confirm(fmt.Sprintf("Record %s (%s) as an advisor?", name, found[name]), true) {
					setValue(d.File, d.Prompt, d.Out, "advisors."+name, "binary", found[name])
				}
			}
			return nil
		},
	}
}

// whisperModelCheck matches the doctor check name for the Whisper model, so
// the verify step can offer the download inline. Kept in sync with
// internal/doctor.checkWhisperModel.
const whisperModelCheck = "Whisper model available"

// VerifyDeps are the injected dependencies of the final verification step.
type VerifyDeps struct {
	Out    io.Writer
	Prompt Prompter
	// Doctor re-loads the configuration and runs the doctor checks.
	Doctor func() []doctor.Result
	// SetupWhisper downloads the Whisper model (offered when its check
	// fails). nil disables the offer.
	SetupWhisper func() error
	// RoundTrip runs a spoken round-trip test (speak → transcribe → answer →
	// speak). nil means the daemon is not running and the test is skipped.
	RoundTrip func() error
}

// VerifyStep re-runs the doctor checks and offers a spoken round-trip test
// when the daemon is up.
func VerifyStep(d VerifyDeps) Step {
	return Step{
		Title: "Verify the installation",
		Run: func() error {
			results := d.Doctor()
			results = offerWhisperDownload(d, results)
			failures := 0
			for _, r := range results {
				tag := map[doctor.Status]string{
					doctor.OK: "[OK]  ", doctor.Warn: "[WARN]", doctor.Fail: "[FAIL]",
				}[r.Status]
				line := tag + " " + r.Name
				if r.Detail != "" {
					line += " — " + r.Detail
				}
				fprintln(d.Out, line)
				if r.Fix != "" && r.Status != doctor.OK {
					fprintln(d.Out, "        "+r.Fix)
				}
				if r.Status == doctor.Fail {
					failures++
				}
			}

			if d.RoundTrip == nil {
				fprintln(d.Out, "\nDaemon not running — skipping the spoken test.")
				fprintln(d.Out, "Start it (systemctl --user enable --now jarvixd), then try: jarvix listen")
			} else if d.Prompt.Confirm("\nRun a spoken round-trip test now (speak → transcribed → answered → spoken)?", true) {
				if err := d.RoundTrip(); err != nil {
					return fmt.Errorf("round-trip test failed: %v — the doctor output above names what to fix", err)
				}
				fprintln(d.Out, "Round trip succeeded — Jarvix is ready.")
			}

			if failures > 0 {
				return fmt.Errorf("%d check(s) failed — each fix is listed above; `jarvix doctor` re-checks", failures)
			}
			return nil
		},
	}
}

// offerWhisperDownload offers to fetch the Whisper model when its doctor
// check failed, re-running the checks afterwards so the printed report
// reflects the fresh state.
func offerWhisperDownload(d VerifyDeps, results []doctor.Result) []doctor.Result {
	if d.SetupWhisper == nil {
		return results
	}
	for _, r := range results {
		if r.Name != whisperModelCheck || r.Status != doctor.Fail {
			continue
		}
		if !d.Prompt.Confirm("The Whisper speech model is missing. Download it now (~148 MB)?", true) {
			return results
		}
		if err := d.SetupWhisper(); err != nil {
			fprintf(d.Out, "Download failed: %v — retry later with `jarvix setup whisper`.\n", err)
			return results
		}
		return d.Doctor()
	}
	return results
}
