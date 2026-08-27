package doctor

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/config"
)

// The probes run engines; the tests run stub scripts on a controlled PATH —
// the repo's exec seam — so no real whisper, piper, or kokoro is ever
// invoked, and pacman's presence is a fixture rather than a property of the
// machine running the suite.

// incidentStderr is the real breakage of 2026-08-25: an Arch update split
// ggml 0.20's compute backends into separate packages, none were installed,
// and every whisper-cli invocation aborted after this stderr — while doctor
// said "[OK] whisper.cpp installed" for two days.
const incidentStderr = `whisper_init_from_file_with_params_no_state: loading model from '/home/rich/.local/share/jarvix/models/whisper/ggml-base.en.bin'
ggml_backend_load_best: search path /usr/lib/ggml does not exist
ggml_backend_load_best: search path /usr/lib/ggml does not exist
/usr/include/ggml-backend-impl.h:213: GGML_ASSERT(device) failed`

// sttWorld builds a config/paths pair whose whisper-cli is the given stub
// script, with a model file on disk and a runtime dir for scratch. PATH is
// the stub dir alone — hermetic, so tests decide whether pacman "exists".
func sttWorld(t *testing.T, stub string) (config.Config, config.Paths, string) {
	t.Helper()
	dir := t.TempDir()
	writeStub(t, dir, "whisper-cli", stub)
	t.Setenv("PATH", dir)

	model := filepath.Join(dir, "ggml-base.en.bin")
	if err := os.WriteFile(model, []byte("model"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.STT.Whisper.Model = model
	paths := config.Paths{Runtime: filepath.Join(t.TempDir(), "jarvix")}
	return cfg, paths, dir
}

func writeStub(t *testing.T, dir, name, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// stderrScript renders a stub that prints text to stderr line by line using
// only shell builtins (the PATH it runs under holds nothing else) and exits
// with the given code.
func stderrScript(text string, exit string) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	for _, line := range strings.Split(text, "\n") {
		b.WriteString(`printf '%s\n' "` + line + `" >&2` + "\n")
	}
	b.WriteString("exit " + exit + "\n")
	return b.String()
}

// The probe wav generator is tested on its own: the probes' stubs never look
// inside the file, so this is where its shape is pinned — a valid RIFF/WAVE,
// 16 kHz mono s16le (whisper's native input), and audibly non-silent.
func TestProbeWAVIsWhisperNativeFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "probe.wav")
	if err := writeProbeWAV(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		t.Fatalf("not a RIFF/WAVE file: % x", data[:12])
	}
	if ch := binary.LittleEndian.Uint16(data[22:24]); ch != 1 {
		t.Errorf("channels = %d, want mono", ch)
	}
	if rate := binary.LittleEndian.Uint32(data[24:28]); rate != probeRate {
		t.Errorf("sample rate = %d, want %d", rate, probeRate)
	}
	if bits := binary.LittleEndian.Uint16(data[34:36]); bits != 16 {
		t.Errorf("bits per sample = %d, want 16", bits)
	}
	pcm := data[44:]
	if want := int(probeToneSecs*probeRate) * 2; len(pcm) != want {
		t.Errorf("pcm length = %d, want %d", len(pcm), want)
	}
	silent := true
	for _, b := range pcm {
		if b != 0 {
			silent = false
			break
		}
	}
	if silent {
		t.Error("probe wav is pure silence; the tone generator produced nothing")
	}
}

func TestSTTProbeTranscribesReportsBackendAndCleansUp(t *testing.T) {
	stub := "#!/bin/sh\n" +
		`printf '%s\n' "load_backend: loaded CPU backend from /usr/lib/libggml-cpu-haswell.so" >&2` + "\n" +
		`printf '%s\n' "probe transcript"` + "\n" +
		"exit 0\n"
	cfg, paths, _ := sttWorld(t, stub)
	r := probeSTT(cfg, paths, probeTimeout)
	if r.Status != OK {
		t.Fatalf("status = %v: %s", r.Status, r.Detail)
	}
	for _, want := range []string{"transcribed", "backend /usr/lib/libggml-cpu-haswell.so", "budget"} {
		if !strings.Contains(r.Detail, want) {
			t.Errorf("detail %q missing %q", r.Detail, want)
		}
	}
	// The probe's scratch is gone: nothing it wrote survives the check.
	entries, err := os.ReadDir(paths.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("probe left artifacts behind: %v", entries)
	}
}

func TestSTTProbeSkipsWhenBinaryAbsent(t *testing.T) {
	cfg, paths, dir := sttWorld(t, "#!/bin/sh\nexit 0\n")
	if err := os.Remove(filepath.Join(dir, "whisper-cli")); err != nil {
		t.Fatal(err)
	}
	r := probeSTT(cfg, paths, probeTimeout)
	if r.Status != OK || !strings.Contains(r.Detail, "skipped") {
		t.Errorf("result = %+v, want a skip note (the install check owns this failure)", r)
	}
}

func TestSTTProbeFailsWhenModelMissing(t *testing.T) {
	cfg, paths, _ := sttWorld(t, "#!/bin/sh\nexit 0\n")
	cfg.STT.Whisper.Model = filepath.Join(t.TempDir(), "nope.bin")
	r := probeSTT(cfg, paths, probeTimeout)
	if r.Status != Fail {
		t.Fatalf("status = %v, want Fail", r.Status)
	}
	if !strings.Contains(r.Detail, "nope.bin") {
		t.Errorf("detail %q does not name the model path", r.Detail)
	}
	if !strings.Contains(r.Fix, "jarvix setup whisper") {
		t.Errorf("fix %q does not carry the model-download remedy", r.Fix)
	}
}

// The incident, end to end: whisper-cli aborts with the real 2026-08-25
// stderr, and the check that used to say OK now names the breakage, quotes
// the assert, and — on a pacman system — the split-package possibility.
func TestSTTProbeNamesTheIncidentBreakage(t *testing.T) {
	for name, withPacman := range map[string]bool{"pacman system": true, "no pacman": false} {
		t.Run(name, func(t *testing.T) {
			cfg, paths, dir := sttWorld(t, stderrScript(incidentStderr, "134"))
			if withPacman {
				writeStub(t, dir, "pacman", "#!/bin/sh\nexit 0\n")
			}
			r := probeSTT(cfg, paths, probeTimeout)
			if r.Status != Fail {
				t.Fatalf("status = %v, want Fail — this exact breakage was OK for two days", r.Status)
			}
			for _, want := range []string{"compute backend", "GGML_ASSERT(device) failed"} {
				if !strings.Contains(r.Detail, want) {
					t.Errorf("detail %q missing %q", r.Detail, want)
				}
			}
			if !strings.Contains(r.Fix, "Reinstall whisper.cpp") {
				t.Errorf("fix %q missing the reinstall remedy", r.Fix)
			}
			if got := strings.Contains(r.Fix, "pacman"); got != withPacman {
				t.Errorf("fix mentions pacman = %v, want %v: %q", got, withPacman, r.Fix)
			}
			// Failure or not, the scratch is cleaned.
			entries, err := os.ReadDir(paths.Runtime)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Errorf("failing probe left artifacts behind: %v", entries)
			}
		})
	}
}

// The FAIL classes, table-driven on the engines' own stderr.
func TestClassifySTTFailure(t *testing.T) {
	cases := map[string]struct {
		stderr  string
		pacman  bool
		summary string // substring of the returned summary
		fix     string // substring of the returned fix
		notFix  string // substring the fix must NOT contain ("" = no constraint)
	}{
		"incident backend abort, pacman": {
			stderr: incidentStderr, pacman: true,
			summary: "compute backend", fix: "pacman -Qs ggml",
		},
		"incident backend abort, no pacman": {
			stderr: incidentStderr, pacman: false,
			summary: "compute backend", fix: "Reinstall whisper.cpp", notFix: "pacman",
		},
		"backend search path gone": {
			stderr:  "ggml_backend_load_best: search path /usr/lib/ggml does not exist",
			summary: "compute backend", fix: "ggml",
		},
		"model unreadable": {
			stderr:  "whisper_init_from_file_with_params_no_state: failed to load model '/x/y.bin'",
			summary: "could not load the model", fix: "jarvix setup whisper",
		},
		"unrecognised failure": {
			stderr:  "Segmentation fault (core dumped)",
			summary: "failed on the probe wav", fix: "--model",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			summary, fix := classifySTTFailure(c.stderr, "/models/ggml-base.en.bin", c.pacman)
			if !strings.Contains(summary, c.summary) {
				t.Errorf("summary %q missing %q", summary, c.summary)
			}
			if !strings.Contains(fix, c.fix) {
				t.Errorf("fix %q missing %q", fix, c.fix)
			}
			if c.notFix != "" && strings.Contains(fix, c.notFix) {
				t.Errorf("fix %q must not mention %q", fix, c.notFix)
			}
		})
	}
}

func TestSTTProbeBoundsAHungEngine(t *testing.T) {
	// The stub hangs (a killed sleep, not a test sleep: the probe's deadline
	// ends it almost immediately). PATH keeps the real utilities so the stub
	// can find sleep.
	dir := t.TempDir()
	writeStub(t, dir, "whisper-cli", "#!/bin/sh\nexec sleep 30\n")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	model := filepath.Join(dir, "ggml-base.en.bin")
	if err := os.WriteFile(model, []byte("model"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.STT.Whisper.Model = model
	paths := config.Paths{Runtime: filepath.Join(t.TempDir(), "jarvix")}

	r := probeSTT(cfg, paths, 50*time.Millisecond)
	if r.Status != Fail || !strings.Contains(r.Detail, "gave up after") {
		t.Errorf("result = %+v, want a bounded-time Fail", r)
	}
}

// piperWorld builds a piper config whose binary is the given stub.
func piperWorld(t *testing.T, stub string) config.Config {
	t.Helper()
	dir := t.TempDir()
	writeStub(t, dir, "piper-tts", stub)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	voice := filepath.Join(dir, "voice.onnx")
	if err := os.WriteFile(voice, []byte("onnx"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(voice+".json", []byte(`{"audio":{"sample_rate":22050}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.TTS.Piper.Voice = voice
	return cfg
}

func TestTTSProbeSpeaksThroughPiper(t *testing.T) {
	cfg := piperWorld(t, "#!/bin/sh\ncat > /dev/null\nprintf 'PCM-BYTES-PCM-BYTES'\nexit 0\n")
	r := probeTTS(cfg, probeTimeout)
	if r.Status != OK {
		t.Fatalf("status = %v: %s", r.Status, r.Detail)
	}
	for _, want := range []string{probePhrase, "discarded", "budget"} {
		if !strings.Contains(r.Detail, want) {
			t.Errorf("detail %q missing %q", r.Detail, want)
		}
	}
}

func TestTTSProbeQuotesPipersOwnStderr(t *testing.T) {
	cfg := piperWorld(t, "#!/bin/sh\ncat > /dev/null\n"+
		`printf '%s\n' "phonemize: espeak-ng data not found at /usr/share/espeak-ng-data" >&2`+"\nexit 1\n")
	r := probeTTS(cfg, probeTimeout)
	if r.Status != Fail {
		t.Fatalf("status = %v, want Fail", r.Status)
	}
	if !strings.Contains(r.Detail, "espeak-ng data not found") {
		t.Errorf("detail %q does not quote the engine's stderr", r.Detail)
	}
}

func TestTTSProbeFailsOnSilentSuccess(t *testing.T) {
	cfg := piperWorld(t, "#!/bin/sh\ncat > /dev/null\nexit 0\n")
	r := probeTTS(cfg, probeTimeout)
	if r.Status != Fail || !strings.Contains(r.Detail, "no audio") {
		t.Errorf("result = %+v, want Fail on an engine that renders nothing", r)
	}
}

func TestTTSProbeSkipsWhatOtherChecksAlreadyFail(t *testing.T) {
	t.Run("piper binary absent", func(t *testing.T) {
		cfg := piperWorld(t, "#!/bin/sh\nexit 0\n")
		cfg.TTS.Piper.Binary = "piper-definitely-not-installed"
		r := probeTTS(cfg, probeTimeout)
		if r.Status != OK || !strings.Contains(r.Detail, "skipped") {
			t.Errorf("result = %+v, want a skip note", r)
		}
	})
	t.Run("piper voice absent", func(t *testing.T) {
		cfg := piperWorld(t, "#!/bin/sh\nexit 0\n")
		cfg.TTS.Piper.Voice = filepath.Join(t.TempDir(), "nope.onnx")
		r := probeTTS(cfg, probeTimeout)
		if r.Status != OK || !strings.Contains(r.Detail, "skipped") {
			t.Errorf("result = %+v, want a skip note", r)
		}
	})
	t.Run("kokoro not set up", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", t.TempDir())
		cfg := config.Default()
		cfg.TTS.Provider = "kokoro"
		r := probeTTS(cfg, probeTimeout)
		if r.Status != OK || !strings.Contains(r.Detail, "skipped") {
			t.Errorf("result = %+v, want a skip note (checkTTS owns this failure)", r)
		}
	})
}

// kokoroWorld installs a fake Kokoro venv under XDG_DATA_HOME — the same
// layout setup-kokoro.sh creates — whose "python" is the given stub.
func kokoroProbeWorld(t *testing.T, stub string) config.Config {
	t.Helper()
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	base := filepath.Join(data, "jarvix")
	writeFile := func(rel, content string) {
		path := filepath.Join(base, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(filepath.Join("kokoro-venv", "bin", "python"), stub)
	writeFile("kokoro_stream.py", `parser.add_argument("--lang")`)
	writeFile(filepath.Join("models", "kokoro", "kokoro-v1.0.onnx"), "onnx")
	writeFile(filepath.Join("models", "kokoro", "voices-v1.0.bin"), "voices")
	cfg := config.Default()
	cfg.TTS.Provider = "kokoro"
	return cfg
}

func TestTTSProbeSpeaksThroughKokoro(t *testing.T) {
	cfg := kokoroProbeWorld(t, "#!/bin/sh\ncat > /dev/null\n"+
		"echo 'SAMPLE_RATE=24000' >&2\nprintf 'KOKORO-PCM'\nexit 0\n")
	r := probeTTS(cfg, probeTimeout)
	if r.Status != OK {
		t.Fatalf("status = %v: %s", r.Status, r.Detail)
	}
	if !strings.Contains(r.Detail, "discarded") {
		t.Errorf("detail %q missing the discarded-sink note", r.Detail)
	}
}

func TestTTSProbeQuotesKokorosTraceback(t *testing.T) {
	cfg := kokoroProbeWorld(t, "#!/bin/sh\ncat > /dev/null\n"+
		"echo 'Traceback (most recent call last):' >&2\n"+
		"echo 'ImportError: no module named kokoro_onnx' >&2\nexit 1\n")
	r := probeTTS(cfg, probeTimeout)
	if r.Status != Fail {
		t.Fatalf("status = %v, want Fail", r.Status)
	}
	if !strings.Contains(r.Detail, "ImportError: no module named kokoro_onnx") {
		t.Errorf("detail %q does not quote the helper's traceback", r.Detail)
	}
	if !strings.Contains(r.Fix, "setup-kokoro") {
		t.Errorf("fix %q missing the setup remedy", r.Fix)
	}
}

// The probes join the report next to the existence checks they correct for,
// and leave the rest of the ordering exactly as it was.
func TestProbesSitBesideTheirExistenceChecks(t *testing.T) {
	cfg, paths := healthyWorld(t)
	results := Run(cfg, paths)
	index := func(prefix string) int {
		for i, r := range results {
			if strings.HasPrefix(r.Name, prefix) {
				return i
			}
		}
		t.Fatalf("no check named %q", prefix)
		return -1
	}
	if got, want := index("whisper.cpp transcribes"), index("Whisper model available")+1; got != want {
		t.Errorf("STT probe at %d, want directly after the model check (%d)", got, want)
	}
	if got, want := index("piper synthesizes"), index("Piper voice available")+1; got != want {
		t.Errorf("TTS probe at %d, want directly after the TTS check (%d)", got, want)
	}
}
