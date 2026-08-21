package setup

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/doctor"
)

var errNotReady = errors.New("not installed")

func ready() error    { return nil }
func notReady() error { return errNotReady }

func TestTTSStepDoneWhenConfiguredEngineReady(t *testing.T) {
	step := TTSStep(TTSDeps{Provider: "piper", PiperReady: ready, KokoroReady: notReady})
	if done, _ := step.Done(); !done {
		t.Fatal("piper configured and ready must be done")
	}
	step = TTSStep(TTSDeps{Provider: "kokoro", PiperReady: ready, KokoroReady: notReady})
	if done, _ := step.Done(); done {
		t.Fatal("kokoro configured but not ready must not be done")
	}
}

func TestTTSStepDefaultsToPiper(t *testing.T) {
	f := loadString(t, "")
	var out strings.Builder
	step := TTSStep(TTSDeps{
		File: f, Out: &out, Prompt: &fakePrompter{},
		Provider: "piper", PiperReady: ready, KokoroReady: notReady,
		KokoroSetupScript: "/opt/setup-kokoro.sh",
		RunScript:         func(string) error { t.Fatal("must not run the script unasked"); return nil },
	})
	if err := step.Run(); err != nil {
		t.Fatal(err)
	}
	if v, _ := f.Get("tts", "provider"); v != "piper" {
		t.Fatalf("got %q", v)
	}
}

func TestTTSStepInstallsKokoroOnRequest(t *testing.T) {
	f := loadString(t, "")
	var out strings.Builder
	installed := false
	step := TTSStep(TTSDeps{
		File: f, Out: &out, Prompt: &fakePrompter{confirms: []bool{true}},
		Provider: "piper", PiperReady: ready, KokoroReady: notReady,
		KokoroSetupScript: "/opt/setup-kokoro.sh",
		RunScript: func(path string) error {
			if path != "/opt/setup-kokoro.sh" {
				t.Fatalf("wrong script: %s", path)
			}
			installed = true
			return nil
		},
	})
	if err := step.Run(); err != nil {
		t.Fatal(err)
	}
	if !installed {
		t.Fatal("setup-kokoro.sh must be delegated to")
	}
	if v, _ := f.Get("tts", "provider"); v != "kokoro" {
		t.Fatalf("got %q", v)
	}
}

func TestTTSStepFailureNamesTheFix(t *testing.T) {
	f := loadString(t, "")
	var out strings.Builder
	step := TTSStep(TTSDeps{
		File: f, Out: &out, Prompt: &fakePrompter{},
		Provider: "piper", PiperReady: notReady, KokoroReady: notReady,
	})
	err := step.Run()
	if err == nil || !strings.Contains(err.Error(), "piper-tts-bin") {
		t.Fatalf("failure must name the install fix, got %v", err)
	}
	if _, ok := f.Get("tts", "provider"); ok {
		t.Fatal("a failed step must not write config")
	}
}

func TestActivationStepDoneStates(t *testing.T) {
	yes := func() bool { return true }
	no := func() bool { return false }
	step := ActivationStep(ActivationDeps{InputAccessible: yes, BindingsInstalled: no})
	if done, _ := step.Done(); !done {
		t.Fatal("input access must mean done")
	}
	step = ActivationStep(ActivationDeps{InputAccessible: no, BindingsInstalled: yes})
	if done, detail := step.Done(); !done || !strings.Contains(detail, "tap-to-toggle") {
		t.Fatalf("bindings must mean done with the fallback named, got %v %q", done, detail)
	}
	step = ActivationStep(ActivationDeps{InputAccessible: no, BindingsInstalled: no})
	if done, _ := step.Done(); done {
		t.Fatal("neither path configured must not be done")
	}
}

func TestActivationStepRunsBothPaths(t *testing.T) {
	var out strings.Builder
	inputRan, bindingsRan := false, false
	step := ActivationStep(ActivationDeps{
		Out: &out, Prompt: &fakePrompter{}, // accept both defaults (yes)
		InputAccessible:   func() bool { return false },
		BindingsInstalled: func() bool { return bindingsRan },
		SetupInput:        func() error { inputRan = true; return nil },
		BindingsScript:    "/opt/install-hyprland-bindings.sh",
		RunScript:         func(string) error { bindingsRan = true; return nil },
	})
	if err := step.Run(); err != nil {
		t.Fatal(err)
	}
	if !inputRan || !bindingsRan {
		t.Fatalf("input %v bindings %v: both flows must run when accepted", inputRan, bindingsRan)
	}
}

func TestActivationStepDeclinedEverythingNamesTheFix(t *testing.T) {
	var out strings.Builder
	step := ActivationStep(ActivationDeps{
		Out: &out, Prompt: &fakePrompter{confirms: []bool{false, false}},
		InputAccessible:   func() bool { return false },
		BindingsInstalled: func() bool { return false },
		SetupInput:        func() error { t.Fatal("declined"); return nil },
		BindingsScript:    "/opt/install-hyprland-bindings.sh",
		RunScript:         func(string) error { t.Fatal("declined"); return nil },
	})
	err := step.Run()
	if err == nil || !strings.Contains(err.Error(), "jarvix setup input") {
		t.Fatalf("must name the fix, got %v", err)
	}
}

type fakeOllama struct {
	models []string
	err    error
}

func (f *fakeOllama) Models(context.Context) ([]string, error) { return f.models, f.err }

func TestAIStepDoneWhenProviderInFile(t *testing.T) {
	f := loadString(t, "[ai]\nprovider = \"openai\"\nmodel = \"gpt-4o\"\n")
	step := AIStep(AIDeps{File: f})
	done, detail := step.Done()
	if !done || !strings.Contains(detail, "openai") || !strings.Contains(detail, "gpt-4o") {
		t.Fatalf("got %v %q", done, detail)
	}
}

func TestAIStepUsesRunningOllamaAndItsModels(t *testing.T) {
	f := loadString(t, "")
	var out strings.Builder
	step := AIStep(AIDeps{
		File: f, Out: &out, Prompt: &fakePrompter{choices: []int{1}},
		Ollama:       &fakeOllama{models: []string{"llama3.2:3b", "qwen2.5:7b"}},
		DefaultModel: "llama3.2:3b",
	})
	if err := step.Run(); err != nil {
		t.Fatal(err)
	}
	if v, _ := f.Get("ai", "provider"); v != "ollama" {
		t.Fatalf("provider: got %q", v)
	}
	if v, _ := f.Get("ai", "model"); v != "qwen2.5:7b" {
		t.Fatalf("model: got %q", v)
	}
}

func TestAIStepCloudProviderPointsAtEnvVarNeverAsksForKey(t *testing.T) {
	f := loadString(t, "")
	var out strings.Builder
	p := &fakePrompter{choices: []int{1}} // first cloud option: openai
	step := AIStep(AIDeps{
		File: f, Out: &out, Prompt: p,
		Ollama:       &fakeOllama{err: errors.New("connection refused")},
		DefaultModel: "llama3.2:3b",
	})
	if err := step.Run(); err != nil {
		t.Fatal(err)
	}
	if v, _ := f.Get("ai", "provider"); v != "openai" {
		t.Fatalf("provider: got %q", v)
	}
	if !strings.Contains(out.String(), "OPENAI_API_KEY") {
		t.Fatalf("must point at the env var:\n%s", out.String())
	}
	for _, q := range p.asked {
		if strings.Contains(strings.ToLower(q), "key") {
			t.Fatalf("must never prompt for an API key, asked %q", q)
		}
	}
	if strings.Contains(out.String(), "sk-") {
		t.Fatal("no key material in output")
	}
}

func TestAIStepSkipWritesNothing(t *testing.T) {
	f := loadString(t, "")
	var out strings.Builder
	step := AIStep(AIDeps{
		File: f, Out: &out, Prompt: &fakePrompter{choices: []int{3}}, // "skip for now"
		Ollama:       &fakeOllama{err: errors.New("connection refused")},
		DefaultModel: "llama3.2:3b",
	})
	if err := step.Run(); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Get("ai", "provider"); ok {
		t.Fatal("skip must not write config")
	}
}

func TestAdvisorsStepRecordsDetectedCLIs(t *testing.T) {
	f := loadString(t, "")
	var out strings.Builder
	paths := map[string]string{"claude": "/usr/bin/claude", "aider": "/home/u/.local/bin/aider"}
	step := AdvisorsStep(AdvisorsDeps{
		File: f, Out: &out, Prompt: &fakePrompter{confirms: []bool{true, false}},
		LookPath: func(name string) (string, error) {
			if p, ok := paths[name]; ok {
				return p, nil
			}
			return "", errors.New("not found")
		},
	})
	if done, _ := step.Done(); done {
		t.Fatal("no advisors recorded yet")
	}
	if err := step.Run(); err != nil {
		t.Fatal(err)
	}
	if v, _ := f.Get("advisors.claude", "binary"); v != "/usr/bin/claude" {
		t.Fatalf("claude binary: got %q", v)
	}
	if _, ok := f.Get("advisors.aider", "binary"); ok {
		t.Fatal("declined advisor must not be recorded")
	}
	if done, detail := step.Done(); !done || !strings.Contains(detail, "claude") {
		t.Fatalf("re-run must show recorded advisors, got %v %q", done, detail)
	}
}

func TestAdvisorsStepNothingFound(t *testing.T) {
	f := loadString(t, "")
	var out strings.Builder
	step := AdvisorsStep(AdvisorsDeps{
		File: f, Out: &out, Prompt: &fakePrompter{},
		LookPath: func(string) (string, error) { return "", errors.New("not found") },
	})
	if err := step.Run(); err != nil {
		t.Fatal(err)
	}
	if f.HasTablePrefix("advisors.") {
		t.Fatal("nothing found must write nothing")
	}
	if !strings.Contains(out.String(), "No known assistant CLIs") {
		t.Fatalf("must say nothing was found:\n%s", out.String())
	}
}

func TestVerifyStepReportsFailuresAndSkipsRoundTripWithoutDaemon(t *testing.T) {
	var out strings.Builder
	step := VerifyStep(VerifyDeps{
		Out: &out, Prompt: &fakePrompter{},
		Doctor: func() []doctor.Result {
			return []doctor.Result{
				{Status: doctor.OK, Name: "PipeWire available"},
				{Status: doctor.Fail, Name: "jarvixd running", Detail: "socket not reachable",
					Fix: "Start it: systemctl --user start jarvixd"},
			}
		},
	})
	err := step.Run()
	if err == nil || !strings.Contains(err.Error(), "1 check(s) failed") {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(out.String(), "systemctl --user start jarvixd") {
		t.Fatalf("fix must be printed:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "skipping the spoken test") {
		t.Fatalf("round trip must be skipped gracefully:\n%s", out.String())
	}
}

func TestVerifyStepRunsRoundTripWhenDaemonUp(t *testing.T) {
	var out strings.Builder
	ran := false
	step := VerifyStep(VerifyDeps{
		Out: &out, Prompt: &fakePrompter{}, // accept the default (yes)
		Doctor:    func() []doctor.Result { return []doctor.Result{{Status: doctor.OK, Name: "all good"}} },
		RoundTrip: func() error { ran = true; return nil },
	})
	if err := step.Run(); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("round trip must run when the daemon is available")
	}
	if !strings.Contains(out.String(), "Round trip succeeded") {
		t.Fatalf("success must be reported:\n%s", out.String())
	}
}

func TestVerifyStepOffersWhisperDownload(t *testing.T) {
	var out strings.Builder
	downloaded := false
	calls := 0
	step := VerifyStep(VerifyDeps{
		Out: &out, Prompt: &fakePrompter{confirms: []bool{true, false}}, // download yes, round trip no
		Doctor: func() []doctor.Result {
			calls++
			if downloaded {
				return []doctor.Result{{Status: doctor.OK, Name: whisperModelCheck}}
			}
			return []doctor.Result{{Status: doctor.Fail, Name: whisperModelCheck,
				Fix: "Download it: jarvix setup whisper"}}
		},
		SetupWhisper: func() error { downloaded = true; return nil },
		RoundTrip:    func() error { t.Fatal("declined"); return nil },
	})
	if err := step.Run(); err != nil {
		t.Fatalf("after the download the checks pass, got %v", err)
	}
	if !downloaded {
		t.Fatal("accepted offer must download the model")
	}
	if calls != 2 {
		t.Fatalf("checks must be re-run after the download, got %d runs", calls)
	}
}
