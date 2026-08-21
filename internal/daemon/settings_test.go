package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/tts"
)

// settingsConfigTOML is the hand-edited config the settings daemon boots
// with: comments and an unknown-to-the-registry endpoint table that every
// rewrite must preserve. The inline api_key is a fixture marker (not a real
// key) used to assert redaction.
const settingsConfigTOML = `# tuned by hand — this comment must survive settings changes
[ai]
provider = "ollama"
model = "llama3.2:3b"

[ai.myserver]
base_url = "http://10.0.0.5:8080/v1"
api_key = "unit-test-inline-marker"

[tts]
provider = "piper" # fast to set up

# Desktop context off: these sessions are real, and gathering would run
# hyprctl and wl-paste against whatever machine the suite is on. Context has
# its own tests in internal/desktop and internal/session.
[context]
window = false
selection = false
clipboard = false
`

// settingsHarness is a daemon over a real socket, booted from a config file
// on disk so config.set/config.reload have something real to rewrite.
type settingsHarness struct {
	client   *ipc.Client
	provider *ai.Fake
	tts      *tts.Fake
	cfgPath  string
}

func startSettingsDaemon(t *testing.T) *settingsHarness {
	t.Helper()
	dir := daemonTempDir(t)
	paths := config.Paths{
		Config:  dir,
		Data:    dir,
		State:   dir,
		Runtime: dir,
		Socket:  filepath.Join(dir, "j.sock"),
	}
	if err := os.WriteFile(paths.ConfigFile(), []byte(settingsConfigTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(paths.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Audio.MinRecordingMs = 0

	h := &settingsHarness{
		provider: &ai.Fake{Response: "Answered."},
		tts:      &tts.Fake{},
		cfgPath:  paths.ConfigFile(),
	}
	d, err := New(cfg, paths, nil, Deps{
		Provider:    h.provider,
		Transcriber: &stt.Fake{Text: "hello"},
		Synthesizer: h.tts,
		Recorder:    &audio.FakeRecorder{Clip: audio.Clip{WAVPath: dir + "/r.wav"}},
		Player:      &audio.FakePlayer{},
		Notifier:    &desktop.FakeNotifier{},
		OpenWindow:  func(context.Context) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = d.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		h.client, err = ipc.Dial(paths.Socket)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon socket never came up: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() { _ = h.client.Close() })
	return h
}

// getResult mirrors the config.get response for assertions.
type getResult struct {
	Path        string `json:"path"`
	Fingerprint string `json:"fingerprint"`
	Fields      []struct {
		Key    string   `json:"key"`
		Label  string   `json:"label"`
		Type   string   `json:"type"`
		Reload string   `json:"reload"`
		Value  any      `json:"value"`
		Enum   []string `json:"enum"`
	} `json:"fields"`
	Secrets []struct {
		Endpoint  string `json:"endpoint"`
		Env       string `json:"env"`
		EnvSet    bool   `json:"env_set"`
		InlineKey bool   `json:"inline_key"`
	} `json:"secrets"`
}

func (h *settingsHarness) get(t *testing.T) getResult {
	t.Helper()
	var res getResult
	if err := h.client.Call("config.get", nil, &res); err != nil {
		t.Fatal(err)
	}
	return res
}

func (h *settingsHarness) field(t *testing.T, res getResult, key string) any {
	t.Helper()
	for _, f := range res.Fields {
		if f.Key == key {
			return f.Value
		}
	}
	t.Fatalf("config.get has no field %q", key)
	return nil
}

// ask runs one text session over the socket and waits for it to finish.
func (h *settingsHarness) ask(t *testing.T) {
	t.Helper()
	if err := h.client.Call("session.start", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Call("session.submit", map[string]string{"text": "hi"}, nil); err != nil {
		t.Fatal(err)
	}
	h.waitEvent(t, "session.finished")
}

func (h *settingsHarness) waitEvent(t *testing.T, eventType string) map[string]any {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-h.client.Events():
			if !ok {
				t.Fatal("connection lost")
			}
			if ev.Type == eventType {
				return ev.Data
			}
			if ev.Type == "error" {
				t.Fatalf("error event: %v", ev.Data)
			}
		case <-deadline:
			t.Fatalf("no %s event", eventType)
		}
	}
}

type setResult struct {
	Fingerprint  string   `json:"fingerprint"`
	Applied      bool     `json:"applied"`
	Reason       string   `json:"reason"`
	NeedsRestart []string `json:"needs_restart"`
}

func TestConfigGetReportsFieldsAndRedactsSecrets(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "unit-test-env-marker")
	h := startSettingsDaemon(t)
	res := h.get(t)

	if !strings.HasPrefix(res.Fingerprint, "sha256:") {
		t.Errorf("fingerprint = %q", res.Fingerprint)
	}
	if got := h.field(t, res, "tts.provider"); got != "piper" {
		t.Errorf("tts.provider = %v", got)
	}
	if got := h.field(t, res, "ai.model"); got != "llama3.2:3b" {
		t.Errorf("ai.model = %v", got)
	}

	// Secrets are presence booleans; neither the env value nor the inline
	// key may appear anywhere in the response.
	foundOpenAI, foundInline := false, false
	for _, s := range res.Secrets {
		if s.Endpoint == "openai" {
			foundOpenAI = true
			if s.Env != "OPENAI_API_KEY" || !s.EnvSet {
				t.Errorf("openai secret entry = %+v", s)
			}
		}
		if s.Endpoint == "myserver" {
			foundInline = true
			if !s.InlineKey {
				t.Error("myserver inline key not reported as present")
			}
		}
	}
	if !foundOpenAI || !foundInline {
		t.Errorf("secrets missing entries: %+v", res.Secrets)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"unit-test-env-marker", "unit-test-inline-marker"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("config.get response leaks %q", secret)
		}
	}
}

// TestConfigSetAppliesWithoutRestart is the issue's headline flow: change a
// value, the file is rewritten preserving hand edits, the daemon reloads,
// and the next session uses the new value — no restart.
func TestConfigSetAppliesWithoutRestart(t *testing.T) {
	h := startSettingsDaemon(t)
	res := h.get(t)

	var set setResult
	err := h.client.Call("config.set", map[string]any{
		"changes":     map[string]any{"ai.model": "qwen-test", "tts.provider": "kokoro"},
		"fingerprint": res.Fingerprint,
	}, &set)
	if err != nil {
		t.Fatal(err)
	}
	if !set.Applied {
		t.Fatalf("set not applied: %s", set.Reason)
	}
	if len(set.NeedsRestart) != 0 {
		t.Errorf("needs_restart = %v", set.NeedsRestart)
	}

	data, err := os.ReadFile(h.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	file := string(data)
	for _, want := range []string{
		"# tuned by hand — this comment must survive settings changes",
		`model = "qwen-test"`,
		`provider = "kokoro" # fast to set up`,
		`api_key = "unit-test-inline-marker"`, // unknown keys untouched
	} {
		if !strings.Contains(file, want) {
			t.Errorf("rewritten file lacks %q:\n%s", want, file)
		}
	}
	if info, err := os.Stat(h.cfgPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("config file mode = %v (err %v), want 0600", info.Mode().Perm(), err)
	}

	// The very next session runs with the new model — the daemon reloaded.
	h.ask(t)
	if got := h.provider.LastRequest.Model; got != "qwen-test" {
		t.Errorf("next session used model %q, want qwen-test", got)
	}
	if got := h.field(t, h.get(t), "tts.provider"); got != "kokoro" {
		t.Errorf("running tts.provider = %v after set", got)
	}
}

func TestConfigSetInvalidIsRejectedAndNothingChanges(t *testing.T) {
	h := startSettingsDaemon(t)
	res := h.get(t)
	before, err := os.ReadFile(h.cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	callErr := h.client.Call("config.set", map[string]any{
		"changes":     map[string]any{"tts.provider": "nova"},
		"fingerprint": res.Fingerprint,
	}, nil)
	var rpcErr *ipc.Error
	if !errors.As(callErr, &rpcErr) || rpcErr.Code != ipc.CodeConfigInvalid {
		t.Fatalf("err = %v, want CodeConfigInvalid", callErr)
	}
	// Field-scoped: the problem carries Config.Validate's message for the key.
	data, _ := rpcErr.Data.(map[string]any)
	problems, _ := data["problems"].([]any)
	if len(problems) == 0 || !strings.Contains(problems[0].(string), `tts.provider "nova" is not supported`) {
		t.Errorf("problems = %v", problems)
	}

	after, err := os.ReadFile(h.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("invalid set modified the file")
	}
	if got := h.field(t, h.get(t), "tts.provider"); got != "piper" {
		t.Errorf("running tts.provider = %v, want piper (unchanged)", got)
	}
}

func TestConfigSetUnknownKeyIsRejected(t *testing.T) {
	h := startSettingsDaemon(t)
	err := h.client.Call("config.set", map[string]any{
		"changes": map[string]any{"ai.myserver.api_key": "sk-nope"},
	}, nil)
	var rpcErr *ipc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != ipc.CodeInvalidParams {
		t.Fatalf("err = %v, want CodeInvalidParams", err)
	}
}

// TestConfigSetDetectsExternalEdit: an edit landing between config.get and
// config.set fails the set with a conflict — never a silent clobber — and
// re-reading then reapplying preserves both edits.
func TestConfigSetDetectsExternalEdit(t *testing.T) {
	h := startSettingsDaemon(t)
	res := h.get(t)

	// The user edits the file in their editor while the screen is open.
	external := strings.Replace(settingsConfigTOML,
		`model = "llama3.2:3b"`, `model = "hand-edited-model"`, 1)
	if err := os.WriteFile(h.cfgPath, []byte(external), 0o600); err != nil {
		t.Fatal(err)
	}

	err := h.client.Call("config.set", map[string]any{
		"changes":     map[string]any{"tts.provider": "kokoro"},
		"fingerprint": res.Fingerprint,
	}, nil)
	var rpcErr *ipc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != ipc.CodeConfigConflict {
		t.Fatalf("err = %v, want CodeConfigConflict", err)
	}
	if data, _ := rpcErr.Data.(map[string]any); data["fingerprint"] == "" {
		t.Error("conflict error lacks the current fingerprint")
	}

	// Reload-then-reapply: fresh get, same set — both edits survive.
	var set setResult
	if err := h.client.Call("config.set", map[string]any{
		"changes":     map[string]any{"tts.provider": "kokoro"},
		"fingerprint": h.get(t).Fingerprint,
	}, &set); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(h.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`model = "hand-edited-model"`, `provider = "kokoro"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("file lost %q after reload-then-reapply:\n%s", want, data)
		}
	}
}

// TestConfigReloadPicksUpHandEdits: `jarvix config reload` (and the screen's
// reload) applies a hand-edited file to the running daemon; an invalid file
// is refused and the running configuration stays.
func TestConfigReloadPicksUpHandEdits(t *testing.T) {
	h := startSettingsDaemon(t)

	edited := strings.Replace(settingsConfigTOML,
		`model = "llama3.2:3b"`, `model = "reloaded-model"`, 1)
	if err := os.WriteFile(h.cfgPath, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Call("config.reload", nil, nil); err != nil {
		t.Fatal(err)
	}
	h.ask(t)
	if got := h.provider.LastRequest.Model; got != "reloaded-model" {
		t.Errorf("session after reload used model %q", got)
	}

	// Now break the file: reload must refuse and keep the running config.
	broken := strings.Replace(edited, `provider = "piper" # fast to set up`,
		`provider = "no-such-tts"`, 1)
	if err := os.WriteFile(h.cfgPath, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	err := h.client.Call("config.reload", nil, nil)
	var rpcErr *ipc.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != ipc.CodeConfigInvalid {
		t.Fatalf("err = %v, want CodeConfigInvalid", err)
	}
	h.ask(t)
	if got := h.provider.LastRequest.Model; got != "reloaded-model" {
		t.Errorf("failed reload changed the running config: model %q", got)
	}
}

// TestConfigSetWhileSessionActive: an idle-class change saved mid-session
// writes the file but does not swap the engine underneath the interaction;
// config.reload applies it once the session is done.
func TestConfigSetWhileSessionActive(t *testing.T) {
	h := startSettingsDaemon(t)

	// Hold the session open deterministically: TTS blocks until released.
	hold := make(chan struct{})
	h.tts.SetHold(hold)
	if err := h.client.Call("session.start", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Call("session.submit", map[string]string{"text": "hi"}, nil); err != nil {
		t.Fatal(err)
	}
	h.waitEvent(t, "assistant.finished") // engine now drains speech, still active

	var set setResult
	if err := h.client.Call("config.set", map[string]any{
		"changes": map[string]any{"ai.model": "later-model"},
	}, &set); err != nil {
		t.Fatal(err)
	}
	if set.Applied {
		t.Error("set applied while a session was active")
	}
	if !strings.Contains(set.Reason, "session is active") {
		t.Errorf("reason = %q", set.Reason)
	}
	if data, _ := os.ReadFile(h.cfgPath); !strings.Contains(string(data), "later-model") {
		t.Error("file was not written for a mid-session set")
	}

	h.tts.SetHold(nil)
	close(hold)
	h.waitEvent(t, "session.finished")

	if err := h.client.Call("config.reload", nil, nil); err != nil {
		t.Fatalf("reload once idle: %v", err)
	}
	h.ask(t)
	if got := h.provider.LastRequest.Model; got != "later-model" {
		t.Errorf("session after reload used model %q, want later-model", got)
	}
}

// TestConfigSetRestartClassIsReported: restart-class settings are written
// but flagged, and the running value stays as booted.
func TestConfigSetRestartClassIsReported(t *testing.T) {
	h := startSettingsDaemon(t)
	var set setResult
	if err := h.client.Call("config.set", map[string]any{
		"changes": map[string]any{"log.level": "debug"},
	}, &set); err != nil {
		t.Fatal(err)
	}
	if len(set.NeedsRestart) != 1 || set.NeedsRestart[0] != "log.level" {
		t.Errorf("needs_restart = %v, want [log.level]", set.NeedsRestart)
	}
	if got := h.field(t, h.get(t), "log.level"); got != "info" {
		t.Errorf("running log.level = %v, want info until restart", got)
	}
}

// TestLexiconAppliesOnReload: a pronunciation added to [tts.lexicon] is heard
// on the very next answer, without restarting the daemon — the point of
// putting the lexicon in the editable-settings registry (ADR 0015, issue
// #30). Asserted on what reached the synthesizer, which is where the spoken
// form is decided.
func TestLexiconAppliesOnReload(t *testing.T) {
	h := startSettingsDaemon(t)
	h.provider.Response = "Jarvix reports 9.2 million rows."

	// Before: the shipped defaults expand the number but know no "Jarvix".
	h.ask(t)
	if got := h.tts.LastRequest.Text; !strings.Contains(got, "nine point two million") {
		t.Errorf("spoken text = %q, want the decimal expanded", got)
	}
	if got := h.tts.LastRequest.Text; strings.Contains(got, "jarviks") {
		t.Fatalf("spoken text = %q before the lexicon entry existed", got)
	}

	var set setResult
	if err := h.client.Call("config.set", map[string]any{
		"changes": map[string]any{"tts.lexicon": map[string]any{"Jarvix": "jarviks"}},
	}, &set); err != nil {
		t.Fatal(err)
	}
	if !set.Applied {
		t.Fatalf("lexicon change not applied: %s", set.Reason)
	}
	if len(set.NeedsRestart) != 0 {
		t.Errorf("needs_restart = %v; a lexicon entry must not need a restart", set.NeedsRestart)
	}

	h.ask(t)
	if got := h.tts.LastRequest.Text; !strings.Contains(got, "jarviks") {
		t.Errorf("spoken text = %q, want the new pronunciation", got)
	}

	// And it survives the round trip through the file: the same table comes
	// back from config.get.
	value, ok := h.field(t, h.get(t), "tts.lexicon").(map[string]any)
	if !ok || value["Jarvix"] != "jarviks" {
		t.Errorf("config.get tts.lexicon = %v", value)
	}
}

// A hand-edited [tts.lexicon] section is picked up by config.reload, which is
// how a user who edits the file directly gets their fix without a restart.
func TestLexiconHandEditAppliesOnReload(t *testing.T) {
	h := startSettingsDaemon(t)
	h.provider.Response = "Kubernetes is healthy."

	edited := settingsConfigTOML + "\n[tts.lexicon]\nkubernetes = \"kates\"\n"
	if err := os.WriteFile(h.cfgPath, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Call("config.reload", nil, nil); err != nil {
		t.Fatal(err)
	}
	h.ask(t)
	if got := h.tts.LastRequest.Text; !strings.Contains(got, "kates") {
		t.Errorf("spoken text = %q, want the hand-edited pronunciation", got)
	}
}

// TestConfigChangedEventIsPublished: every connected client hears about a
// successful set so open settings screens can refresh.
func TestConfigChangedEventIsPublished(t *testing.T) {
	h := startSettingsDaemon(t)
	var set setResult
	if err := h.client.Call("config.set", map[string]any{
		"changes": map[string]any{"ai.model": "event-model"},
	}, &set); err != nil {
		t.Fatal(err)
	}
	data := h.waitEvent(t, "config.changed")
	if data["fingerprint"] != set.Fingerprint {
		t.Errorf("config.changed fingerprint = %v, want %v", data["fingerprint"], set.Fingerprint)
	}
}

func TestDoctorGetOverSocket(t *testing.T) {
	h := startSettingsDaemon(t)
	var res struct {
		Checks []struct {
			Status  string `json:"status"`
			Name    string `json:"name"`
			Related string `json:"related"`
		} `json:"checks"`
	}
	if err := h.client.Call("doctor.get", nil, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Checks) == 0 {
		t.Fatal("doctor.get returned no checks")
	}
	related := map[string]bool{}
	for _, c := range res.Checks {
		if c.Name == "" || c.Status == "" {
			t.Errorf("incomplete check: %+v", c)
		}
		related[c.Related] = true
	}
	for _, want := range []string{"tts.provider", "stt.whisper.model", "activation.ptt_chord"} {
		if !related[want] {
			t.Errorf("no readiness check relates to %s", want)
		}
	}
}
