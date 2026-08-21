package doctor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/ipc"
)

// The doctor's job is diagnosing a machine; these tests build a healthy fake
// machine from stub binaries, temp files, and a real IPC server — then break
// it one piece at a time. No PipeWire, whisper, piper, or network required.

const wpctlStubOutput = `Audio
 ├─ Devices:
 │      42. Fake Audio Controller
 │
 ├─ Sources:
 │  *   55. Fake Microphone [vol: 1.00]
 │
 ├─ Sinks:
 │  *   44. Fake Speakers [vol: 1.00]
 │
`

// installDoctorStubs populates PATH with every binary a healthy system has.
func installDoctorStubs(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	stubs := map[string]string{
		"pw-record":   "#!/bin/sh\nexit 0\n",
		"pw-play":     "#!/bin/sh\nexit 0\n",
		"pw-cli":      "#!/bin/sh\nexit 0\n",
		"wpctl":       "#!/bin/sh\ncat <<'EOF'\n" + wpctlStubOutput + "EOF\n",
		"whisper-cli": "#!/bin/sh\nexit 0\n",
		"piper-tts":   "#!/bin/sh\nexit 0\n",
	}
	for name, script := range stubs {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Prepend: the stubs must win over any real installation, but the stub
	// scripts still need the standard shell utilities.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

// healthyWorld builds a config+paths pair where every doctor check can pass:
// stub binaries, an on-disk whisper model and piper voice, a live daemon
// socket, and a reachable fake provider endpoint.
func healthyWorld(t *testing.T) (config.Config, config.Paths) {
	t.Helper()
	stubDir := installDoctorStubs(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfg := config.Default()

	// Whisper model on disk (absolute path skips the model-dir resolution).
	model := filepath.Join(stubDir, "ggml-base.en.bin")
	if err := os.WriteFile(model, []byte("model"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.STT.Whisper.Model = model

	// Piper voice with its sample-rate sidecar.
	voice := filepath.Join(stubDir, "voice.onnx")
	if err := os.WriteFile(voice, []byte("onnx"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(voice+".json", []byte(`{"audio":{"sample_rate":22050}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.TTS.Piper.Voice = voice

	// A reachable "provider" — httptest binds 127.0.0.1, so the endpoint also
	// counts as local and needs no API key.
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(provider.Close)
	ep := cfg.AI.Endpoints["ollama"]
	ep.BaseURL = provider.URL
	cfg.AI.Endpoints["ollama"] = ep

	// The Omarchy plugin manifest under $HOME.
	manifest := filepath.Join(home, ".config", "omarchy", "plugins", "jarvix", "manifest.json")
	if err := os.MkdirAll(filepath.Dir(manifest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// A live daemon on the socket.
	rt := t.TempDir()
	paths := config.Paths{
		Config:  filepath.Join(home, ".config", "jarvix"),
		Data:    filepath.Join(home, ".local", "share", "jarvix"),
		State:   filepath.Join(home, ".local", "state", "jarvix"),
		Runtime: filepath.Join(rt, "jarvix"),
		Socket:  filepath.Join(rt, "jarvix.sock"),
	}
	srv := ipc.NewServer(paths.Socket, nil, nil)
	srv.Handle("status.get", func(json.RawMessage) (any, error) {
		return map[string]any{"state": "idle", "version": "test", "protocol": 1}, nil
	})
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); srv.Close() })

	return cfg, paths
}

// resultByName finds one check in a Run result set.
func resultByName(t *testing.T, results []Result, prefix string) Result {
	t.Helper()
	for _, r := range results {
		if strings.HasPrefix(r.Name, prefix) {
			return r
		}
	}
	t.Fatalf("no check named %q in %+v", prefix, results)
	return Result{}
}

func TestRunOnHealthyMachineHasNoFailures(t *testing.T) {
	cfg, paths := healthyWorld(t)
	results := Run(cfg, paths)

	for _, name := range []string{
		"configuration valid",
		"PipeWire available",
		"microphone detected",
		"audio output available",
		"whisper.cpp installed",
		"Whisper model available",
		"Piper voice available",
		"jarvixd running",
		"AI provider configured",
		"provider authentication succeeded",
		"Omarchy plugin installed",
	} {
		r := resultByName(t, results, name)
		if r.Status != OK {
			t.Errorf("%s: status %v, detail %q, fix %q", name, r.Status, r.Detail, r.Fix)
		}
	}
	if !Healthy(results) {
		t.Errorf("healthy machine reported unhealthy: %+v", results)
	}
	daemon := resultByName(t, results, "jarvixd running")
	if !strings.Contains(daemon.Detail, "version test") {
		t.Errorf("daemon detail = %q", daemon.Detail)
	}
	mic := resultByName(t, results, "microphone detected")
	if !strings.Contains(mic.Detail, "Fake Microphone") {
		t.Errorf("microphone detail = %q", mic.Detail)
	}
}

func TestRunDiagnosesMissingPieces(t *testing.T) {
	cfg, paths := healthyWorld(t)

	t.Run("invalid config", func(t *testing.T) {
		bad := cfg
		bad.AI.Model = ""
		r := resultByName(t, Run(bad, paths), "configuration valid")
		if r.Status != Fail || !strings.Contains(r.Detail, "ai.model") {
			t.Errorf("result = %+v", r)
		}
	})

	t.Run("whisper model missing", func(t *testing.T) {
		bad := cfg
		bad.STT.Whisper.Model = filepath.Join(t.TempDir(), "nope.bin")
		r := resultByName(t, Run(bad, paths), "Whisper model available")
		if r.Status != Fail || !strings.Contains(r.Fix, "jarvix setup whisper") {
			t.Errorf("result = %+v", r)
		}
	})

	t.Run("daemon not running", func(t *testing.T) {
		deadPaths := paths
		deadPaths.Socket = filepath.Join(t.TempDir(), "nope.sock")
		r := resultByName(t, Run(cfg, deadPaths), "jarvixd running")
		if r.Status != Fail || !strings.Contains(r.Fix, "systemctl") {
			t.Errorf("result = %+v", r)
		}
	})

	t.Run("provider without endpoint", func(t *testing.T) {
		bad := cfg
		bad.AI.Provider = "unconfigured"
		r := resultByName(t, Run(bad, paths), "AI provider configured")
		if r.Status != Fail || !strings.Contains(r.Detail, "unconfigured") {
			t.Errorf("result = %+v", r)
		}
		auth := resultByName(t, Run(bad, paths), "provider authentication")
		if auth.Status != Warn {
			t.Errorf("auth without endpoint = %+v", auth)
		}
	})

	t.Run("remote provider without api key", func(t *testing.T) {
		bad := cfg
		eps := map[string]config.Endpoint{}
		for k, v := range cfg.AI.Endpoints {
			eps[k] = v
		}
		eps["openai"] = config.Endpoint{BaseURL: "https://api.openai.com/v1", APIKeyEnv: "DOCTOR_TEST_NO_SUCH_KEY"}
		bad.AI.Endpoints = eps
		bad.AI.Provider = "openai"
		r := resultByName(t, Run(bad, paths), "AI provider configured")
		if r.Status != Fail || !strings.Contains(r.Fix, "DOCTOR_TEST_NO_SUCH_KEY") {
			t.Errorf("result = %+v", r)
		}
	})

	t.Run("kokoro not set up", func(t *testing.T) {
		bad := cfg
		bad.TTS.Provider = "kokoro"
		r := resultByName(t, Run(bad, paths), "Kokoro TTS ready")
		if r.Status != Fail || !strings.Contains(r.Fix, "setup-kokoro") {
			t.Errorf("result = %+v", r)
		}
	})
}

func TestRunWithoutPipeWireToolsFails(t *testing.T) {
	cfg, paths := healthyWorld(t)
	t.Setenv("PATH", t.TempDir()) // everything vanishes from PATH
	results := Run(cfg, paths)
	for _, name := range []string{"PipeWire available", "microphone detected",
		"audio output available", "whisper.cpp installed"} {
		if r := resultByName(t, results, name); r.Status != Fail {
			t.Errorf("%s = %+v, want Fail without the tooling", name, r)
		}
	}
	if Healthy(results) {
		t.Error("a machine without PipeWire must not be healthy")
	}
}

func TestHealthyToleratesWarnings(t *testing.T) {
	if !Healthy([]Result{{Status: OK}, {Status: Warn}}) {
		t.Error("warnings must not fail the doctor")
	}
	if Healthy([]Result{{Status: OK}, {Status: Fail}}) {
		t.Error("failures must fail the doctor")
	}
}
