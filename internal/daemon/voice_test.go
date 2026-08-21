package daemon

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/voice"
)

// Two guarantees are checked here, both of which only exist end to end.
//
// A voice change lands on the next utterance without a daemon restart — the
// idle-class reload contract of ADR 0015, which the voice keys were already
// registered under and which this feature must not quietly break. And a voice
// the machine cannot speak is refused by config.set, before it is written to
// the file, rather than being accepted and then failing at synthesis time.

// daemonPaths reconstructs the harness's paths. The settings daemon points
// every XDG root at one directory, so the config file's directory is also the
// data directory the voices archive lives under.
func daemonPaths(h *settingsHarness) config.Paths {
	dir := filepath.Dir(h.cfgPath)
	return config.Paths{Config: dir, Data: dir, State: dir, Runtime: dir}
}

// installDaemonVoices writes a stand-in Kokoro voices archive into the
// daemon's data directory. It is a zip of named entries, which is what the
// real voices file is; no model, no engine, no download.
func installDaemonVoices(t *testing.T, paths config.Paths, ids ...string) {
	t.Helper()
	path := paths.KokoroVoicesFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	for _, id := range ids {
		e, err := w.Create(id + ".npy")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.Write([]byte("embedding")); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestVoiceChangeAppliesWithoutARestart(t *testing.T) {
	h := startSettingsDaemon(t)
	installDaemonVoices(t, daemonPaths(h), "af_heart", "bf_emma", "bm_george")

	var set setResult
	if err := h.client.Call("config.set", map[string]any{
		"changes": map[string]any{
			"tts.provider":     "kokoro",
			"tts.kokoro.voice": "bf_emma",
		},
		"fingerprint": h.get(t).Fingerprint,
	}, &set); err != nil {
		t.Fatal(err)
	}
	if !set.Applied {
		t.Fatalf("voice change not applied: %s", set.Reason)
	}
	if len(set.NeedsRestart) != 0 {
		t.Errorf("a voice change asked for a restart: %v", set.NeedsRestart)
	}
	if got := h.field(t, h.get(t), "tts.kokoro.voice"); got != "bf_emma" {
		t.Errorf("running config still says %v", got)
	}
}

func TestUninstalledVoiceIsRefusedBeforeItIsWritten(t *testing.T) {
	h := startSettingsDaemon(t)
	installDaemonVoices(t, daemonPaths(h), "af_heart", "bf_emma", "bm_george")

	before, err := os.ReadFile(h.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	err = h.client.Call("config.set", map[string]any{
		"changes": map[string]any{
			"tts.provider":     "kokoro",
			"tts.kokoro.voice": "bf_emily",
		},
		"fingerprint": h.get(t).Fingerprint,
	}, nil)
	if err == nil {
		t.Fatal("a voice the machine cannot speak was accepted")
	}
	if !strings.Contains(err.Error(), "rejected by validation") {
		t.Errorf("err = %v", err)
	}
	after, err := os.ReadFile(h.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Error("a rejected change was written to config.toml anyway")
	}
}

// Switching engines must consult the *new* engine's voices. Validating a
// Kokoro voice id against the Piper catalog the daemon happened to be running
// with would reject every engine switch there is.
func TestSwitchingEnginesValidatesAgainstTheNewEnginesVoices(t *testing.T) {
	h := startSettingsDaemon(t)
	installDaemonVoices(t, daemonPaths(h), "af_heart", "bf_emma")

	var set setResult
	if err := h.client.Call("config.set", map[string]any{
		"changes":     map[string]any{"tts.provider": "kokoro", "tts.kokoro.voice": "af_heart"},
		"fingerprint": h.get(t).Fingerprint,
	}, &set); err != nil {
		t.Fatalf("switching to Kokoro was rejected: %v", err)
	}
	if !set.Applied {
		t.Fatalf("not applied: %s", set.Reason)
	}
}

// A non-English voice cannot be set on its own while whisper is pinned to the
// English-only default: the refusal names the model to install, and setting
// both together is accepted.
func TestNonEnglishVoiceMustBringSpeechRecognitionWithIt(t *testing.T) {
	h := startSettingsDaemon(t)
	installDaemonVoices(t, daemonPaths(h), "af_heart", "ff_siwis")

	err := h.client.Call("config.set", map[string]any{
		"changes":     map[string]any{"tts.provider": "kokoro", "tts.kokoro.voice": "ff_siwis"},
		"fingerprint": h.get(t).Fingerprint,
	}, nil)
	if err == nil {
		t.Fatal("a French voice was accepted while whisper stayed on base.en")
	}
	if !strings.Contains(err.Error(), "rejected by validation") {
		t.Errorf("err = %v", err)
	}

	var set setResult
	if err := h.client.Call("config.set", map[string]any{
		"changes": map[string]any{
			"tts.provider":         "kokoro",
			"tts.kokoro.voice":     "ff_siwis",
			"stt.whisper.model":    config.MultilingualWhisperModel,
			"stt.whisper.language": "fr",
		},
		"fingerprint": h.get(t).Fingerprint,
	}, &set); err != nil {
		t.Fatalf("setting language and voice together was rejected: %v", err)
	}
	if !set.Applied {
		t.Fatalf("not applied: %s", set.Reason)
	}
}

// The voice keys are in the editable registry as idle-class, which is what
// makes "applies to the next utterance without a restart" true rather than
// hopeful.
func TestVoiceSettingsAreRegisteredAsIdleClass(t *testing.T) {
	want := map[string]bool{
		"tts.kokoro.voice":     false,
		"tts.piper.voice":      false,
		"stt.whisper.model":    false,
		"stt.whisper.language": false,
	}
	for _, s := range config.Settings() {
		if _, ok := want[s.Key]; !ok {
			continue
		}
		if s.Reload != config.ReloadIdle {
			t.Errorf("%s is %q; a voice change must not need a restart", s.Key, s.Reload)
		}
		want[s.Key] = true
	}
	for key, seen := range want {
		if !seen {
			t.Errorf("%s is not in the editable settings registry", key)
		}
	}
}

// The archive read is what the daemon validates against, so it has to be the
// same file the engine speaks from.
func TestDaemonVoicesFileMatchesTheEngines(t *testing.T) {
	paths := config.Paths{Data: "/data/jarvix"}
	if paths.KokoroVoicesFile() != voice.KokoroVoicesFile("/data/jarvix") {
		t.Error("config and the voice package disagree about the archive path")
	}
}
