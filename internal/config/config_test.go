package config

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AI.Provider != "ollama" {
		t.Errorf("default provider = %q, want ollama", cfg.AI.Provider)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("defaults must validate: %v", err)
	}
}

func TestNotificationDefaultsOn(t *testing.T) {
	cfg := Default()
	if !cfg.UI.Notifications {
		t.Error("ui.notifications should default to true")
	}
	if !cfg.UI.NotificationPreview {
		t.Error("ui.notification_preview should default to true")
	}
}

func TestNotificationKeysOverride(t *testing.T) {
	cfg := writeAndLoad(t, `
[ui]
notifications = false
notification_preview = false
`)
	if cfg.UI.Notifications {
		t.Error("notifications should be off")
	}
	if cfg.UI.NotificationPreview {
		t.Error("notification_preview should be off")
	}
	// Untouched [ui] defaults survive a partial table.
	if !cfg.UI.ShowTranscript {
		t.Error("show_transcript default lost")
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestLoadOverridesDefaults(t *testing.T) {
	cfg := writeAndLoad(t, `
[ai]
provider = "openai"
model = "gpt-4.1-mini"

[stt.whisper]
model = "small.en"

[conversation]
speak_responses = false
`)
	if cfg.AI.Provider != "openai" || cfg.AI.Model != "gpt-4.1-mini" {
		t.Errorf("ai = %+v", cfg.AI)
	}
	if cfg.STT.Whisper.Model != "small.en" {
		t.Errorf("whisper model = %q", cfg.STT.Whisper.Model)
	}
	if cfg.Conversation.SpeakResponses {
		t.Error("speak_responses should be false")
	}
	// Untouched defaults survive.
	if cfg.TTS.Piper.Voice != "en_US-amy-medium" {
		t.Errorf("piper voice default lost: %q", cfg.TTS.Piper.Voice)
	}
}

func TestCustomEndpointWithoutCodeChanges(t *testing.T) {
	cfg := writeAndLoad(t, `
[ai]
provider = "myserver"
model = "local-model"

[ai.myserver]
base_url = "http://10.0.0.5:8080/v1"
api_key_env = "MYSERVER_KEY"
`)
	ep, ok := cfg.Endpoint()
	if !ok {
		t.Fatal("custom endpoint not registered")
	}
	if ep.BaseURL != "http://10.0.0.5:8080/v1" || ep.APIKeyEnv != "MYSERVER_KEY" {
		t.Errorf("endpoint = %+v", ep)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestPresetEndpointOverride(t *testing.T) {
	cfg := writeAndLoad(t, `
[ai.openai]
base_url = "https://proxy.example.com/v1"
`)
	if got := cfg.AI.Endpoints["openai"].BaseURL; got != "https://proxy.example.com/v1" {
		t.Errorf("base_url = %q", got)
	}
	// Preset key env is preserved when only base_url is overridden.
	if got := cfg.AI.Endpoints["openai"].APIKeyEnv; got != "OPENAI_API_KEY" {
		t.Errorf("api_key_env = %q", got)
	}
}

func TestValidateReportsAllProblems(t *testing.T) {
	cfg := writeAndLoad(t, `
[activation]
mode = "telepathy"

[ai]
provider = "nonexistent"
model = ""

[log]
level = "loud"
`)
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	for _, want := range []string{"telepathy", "nonexistent", "ai.model", "log.level"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing mention of %q: %v", want, err)
		}
	}
}

func TestArtifactDefaults(t *testing.T) {
	cfg := Default()
	if !strings.HasSuffix(cfg.Artifacts.Dir, filepath.Join("Documents", "Jarvix")) {
		t.Errorf("artifacts.dir default = %q", cfg.Artifacts.Dir)
	}
	if !slices.Equal(cfg.Artifacts.OpenCommand, []string{"xdg-open"}) || cfg.Artifacts.RenderTimeoutSec != 10 {
		t.Errorf("artifacts defaults = %+v", cfg.Artifacts)
	}
	// PNG, not SVG: the default artifact must show its text in an image
	// viewer, and mermaid's SVG labels only render in a browser (#56).
	if cfg.Artifacts.DiagramFormat != "png" {
		t.Errorf("artifacts.diagram_format default = %q, want png", cfg.Artifacts.DiagramFormat)
	}
	if !cfg.Tools.Artifacts {
		t.Error("tools.artifacts should default on: the tool degrades safely without its renderer")
	}
}

func TestArtifactOverridesAndValidation(t *testing.T) {
	cfg := writeAndLoad(t, `
[tools]
artifacts = false

[artifacts]
dir = "/tmp/my-artifacts"
open_command = "imv"
render_timeout_sec = 30
diagram_format = "svg"

[artifacts.open_commands]
document = "obsidian"
excalidraw = ""
`)
	if cfg.Tools.Artifacts {
		t.Error("tools.artifacts should be off")
	}
	if cfg.Artifacts.Dir != "/tmp/my-artifacts" || !slices.Equal(cfg.Artifacts.OpenCommand, []string{"imv"}) ||
		cfg.Artifacts.RenderTimeoutSec != 30 || cfg.Artifacts.DiagramFormat != "svg" {
		t.Errorf("artifacts = %+v", cfg.Artifacts)
	}
	if !slices.Equal(cfg.Artifacts.OpenCommands["document"], []string{"obsidian"}) {
		t.Errorf("open_commands = %+v", cfg.Artifacts.OpenCommands)
	}
	// An explicitly empty override is meaningful ("no viewer for this
	// format"), so it must survive the parse as a present-but-empty entry.
	if v, ok := cfg.Artifacts.OpenCommands["excalidraw"]; !ok || len(v) != 0 {
		t.Errorf("empty override lost: %+v", cfg.Artifacts.OpenCommands)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestArtifactValidationRejectsBadValues(t *testing.T) {
	cfg := writeAndLoad(t, `
[artifacts]
dir = "~/Documents/Jarvix"
open_command = " "
render_timeout_sec = 0
diagram_format = "jpg"
`)
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	for _, want := range []string{"artifacts.dir", "artifacts.open_command", "artifacts.render_timeout_sec", "artifacts.diagram_format"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing mention of %q: %v", want, err)
		}
	}
}

func TestEndpointKeyPrefersEnvironment(t *testing.T) {
	t.Setenv("JARVIX_TEST_KEY", "from-env")
	ep := Endpoint{APIKeyEnv: "JARVIX_TEST_KEY", APIKey: "inline"}
	if got := ep.Key(); got != "from-env" {
		t.Errorf("Key() = %q, want from-env", got)
	}
	t.Setenv("JARVIX_TEST_KEY", "")
	if got := ep.Key(); got != "inline" {
		t.Errorf("Key() fallback = %q, want inline", got)
	}
}

func TestRedactMasksInlineKeys(t *testing.T) {
	cfg := writeAndLoad(t, `
[ai.openai]
api_key = "sk-secret"
`)
	red := cfg.Redact()
	if got := red.AI.Endpoints["openai"].APIKey; got != "[redacted]" {
		t.Errorf("redacted key = %q", got)
	}
	// Original is untouched.
	if got := cfg.AI.Endpoints["openai"].APIKey; got != "sk-secret" {
		t.Errorf("original mutated: %q", got)
	}
}

func TestParseErrorIsHelpful(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[ai\nbroken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected parse error")
	}
}

func writeAndLoad(t *testing.T, content string) Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func TestToolsPolicyDefaults(t *testing.T) {
	cfg := Default()
	if cfg.Tools.Policy.Default != "ask" {
		t.Errorf("policy default = %q, want ask (unknown tools must never run silently)", cfg.Tools.Policy.Default)
	}
	if cfg.Tools.Policy.ConfirmTimeoutSec != 30 {
		t.Errorf("confirm_timeout_sec = %d, want 30", cfg.Tools.Policy.ConfirmTimeoutSec)
	}
	if cfg.Tools.Policy.RememberForConversation {
		t.Error("remember_for_conversation must default to false")
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("defaults must validate: %v", err)
	}
}

func TestToolsPolicyParsing(t *testing.T) {
	cfg := writeAndLoad(t, `
[tools]
shell = true

[tools.policy]
default = "deny"
confirm_timeout_sec = 10
remember_for_conversation = true
shell_allow = ["docker compose ps"]
shell_deny = ["git push"]

[tools.policy.tool]
"shell.run" = "allow"
"weather.get" = "deny"
`)
	p := cfg.Tools.Policy
	if p.Default != "deny" || p.ConfirmTimeoutSec != 10 || !p.RememberForConversation {
		t.Errorf("policy = %+v", p)
	}
	if p.Tool["shell.run"] != "allow" || p.Tool["weather.get"] != "deny" {
		t.Errorf("per-tool = %v", p.Tool)
	}
	if len(p.ShellAllow) != 1 || p.ShellAllow[0] != "docker compose ps" {
		t.Errorf("shell_allow = %v", p.ShellAllow)
	}
	if len(p.ShellDeny) != 1 || p.ShellDeny[0] != "git push" {
		t.Errorf("shell_deny = %v", p.ShellDeny)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("valid policy must validate: %v", err)
	}
}

func TestToolsPolicyValidation(t *testing.T) {
	cfg := writeAndLoad(t, `
[tools.policy]
default = "yolo"
confirm_timeout_sec = 0
shell_allow = ["  "]

[tools.policy.tool]
"shell.run" = "maybe"
`)
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error")
	}
	for _, want := range []string{
		"tools.policy.default", "confirm_timeout_sec", "shell_allow", "shell.run", "maybe",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing mention of %q: %v", want, err)
		}
	}
}

// A viewer whose path or argument contains a space cannot be written as the
// whitespace-split string form, so open commands also accept an argv array.
// The string form is the original shape and keeps working unchanged
// (raised in review of #19).
func TestOpenCommandAcceptsBothStringAndArrayForms(t *testing.T) {
	cfg := writeAndLoad(t, `
[artifacts]
open_command = ["/opt/my viewer/bin/view", "--new window"]

[artifacts.open_commands]
document = "obsidian --new"
spreadsheet = ["flatpak", "run", "org.libreoffice.LibreOffice"]
excalidraw = []
`)
	if want := []string{"/opt/my viewer/bin/view", "--new window"}; !slices.Equal(cfg.Artifacts.OpenCommand, want) {
		t.Errorf("open_command = %q, want %q", cfg.Artifacts.OpenCommand, want)
	}
	// The legacy string form still splits on whitespace, so configs written
	// before the array existed behave exactly as they did.
	if want := []string{"obsidian", "--new"}; !slices.Equal(cfg.Artifacts.OpenCommands["document"], want) {
		t.Errorf("document = %q, want %q", cfg.Artifacts.OpenCommands["document"], want)
	}
	if want := []string{"flatpak", "run", "org.libreoffice.LibreOffice"}; !slices.Equal(cfg.Artifacts.OpenCommands["spreadsheet"], want) {
		t.Errorf("spreadsheet = %q, want %q", cfg.Artifacts.OpenCommands["spreadsheet"], want)
	}
	// An empty array is the "no viewer" declaration, same as "".
	if v, ok := cfg.Artifacts.OpenCommands["excalidraw"]; !ok || len(v) != 0 {
		t.Errorf("empty array override lost: %+v", cfg.Artifacts.OpenCommands)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestOpenCommandRejectsNonStringElements(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[artifacts]\nopen_command = [\"xdg-open\", 7]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("a non-string element must be a parse error, not a silent drop")
	}
}

// Every TOML block in docs/configuration.md is presented as a config you can
// copy into place, so each one must parse AND validate. The reference block
// once showed artifacts.dir = "~/..." while validation rejected "~", so
// copy-pasting the documentation produced a config the daemon refused to
// start on (raised in review of #17).
func TestDocumentedConfigExamplesAreValid(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "configuration.md"))
	if err != nil {
		t.Fatalf("read the configuration reference: %v", err)
	}
	blocks := regexp.MustCompile("(?s)```toml\\n(.*?)```").FindAllStringSubmatch(string(data), -1)
	if len(blocks) == 0 {
		t.Fatal("no toml examples found; this test guards them, so it must not silently pass")
	}
	for i, block := range blocks {
		cfg, err := parse([]byte(block[1]), Default())
		if err != nil {
			t.Errorf("toml example %d does not parse: %v", i+1, err)
			continue
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("toml example %d is documented but invalid: %v", i+1, err)
		}
	}
}

// The pronunciation lexicon is a hand-editable table of user entries. It
// defaults to empty: the shipped respellings live in the speech layer, so
// what is in the file is only ever what the user asked for (issue #30).
func TestLexiconLoadsFromTOML(t *testing.T) {
	cfg := writeAndLoad(t, `
[tts.lexicon]
Kubernetes = "koo ber net eez"
"k9s" = "kay nine ess"
`)
	if got := cfg.TTS.Lexicon["Kubernetes"]; got != "koo ber net eez" {
		t.Errorf("lexicon[Kubernetes] = %q", got)
	}
	if got := cfg.TTS.Lexicon["k9s"]; got != "kay nine ess" {
		t.Errorf("lexicon[k9s] = %q", got)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("a lexicon must validate: %v", err)
	}
	if len(Default().TTS.Lexicon) != 0 {
		t.Errorf("the default lexicon should be empty, got %v", Default().TTS.Lexicon)
	}
}

func TestIntentsDefaultOn(t *testing.T) {
	cfg := Default()
	if !cfg.Intents.Enabled {
		t.Error("intents.enabled should default to true")
	}
	if cfg.Intents.Terminal == "" {
		t.Error("intents.terminal needs a default")
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("defaults must validate: %v", err)
	}
}

func TestCustomIntentsLoad(t *testing.T) {
	cfg := writeAndLoad(t, `
[intents]
enabled = true
terminal = "ghostty"

[[intents.custom]]
match = "lock the screen"
run = "hyprlock"
say = "Locking."

[[intents.custom]]
match = "good night"
run = "systemctl suspend"
`)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid intents rejected: %v", err)
	}
	if cfg.Intents.Terminal != "ghostty" {
		t.Errorf("terminal = %q", cfg.Intents.Terminal)
	}
	if len(cfg.Intents.Custom) != 2 {
		t.Fatalf("custom intents = %d, want 2", len(cfg.Intents.Custom))
	}
	if cfg.Intents.Custom[0].Run != "hyprlock" || cfg.Intents.Custom[0].Say != "Locking." {
		t.Errorf("first entry = %+v", cfg.Intents.Custom[0])
	}
	opts := cfg.IntentOptions()
	if len(opts.Custom) != 2 || opts.Terminal != "ghostty" {
		t.Errorf("IntentOptions = %+v", opts)
	}
}

// TestMalformedIntentNamesTheEntry is the configuration criterion: a bad
// pattern fails validation with the offending entry named, not a vague
// message the user has to bisect their config file to understand.
func TestMalformedIntentNamesTheEntry(t *testing.T) {
	tests := []struct {
		name    string
		toml    string
		wantSub []string
	}{
		{
			name: "no run command",
			toml: `
[[intents.custom]]
match = "lock the screen"
`,
			wantSub: []string{"intents.custom[0]", "lock the screen", "run command"},
		},
		{
			name: "second entry is the broken one",
			toml: `
[[intents.custom]]
match = "lock the screen"
run = "hyprlock"

[[intents.custom]]
match = ""
run = "systemctl suspend"
`,
			wantSub: []string{"intents.custom[1]", "match is empty"},
		},
		{
			name: "placeholder in a user pattern",
			toml: `
[[intents.custom]]
match = "volume {volume}"
run = "wpctl set-volume"
`,
			wantSub: []string{"intents.custom[0]", "placeholder"},
		},
		{
			name: "shadows a built-in",
			toml: `
[[intents.custom]]
match = "mute"
run = "amixer set Master mute"
`,
			wantSub: []string{"intents.custom[0]", "built-in intent", "volume.mute"},
		},
		{
			name: "terminal is not a single token",
			toml: `
[intents]
terminal = "alacritty --title x"
`,
			wantSub: []string{"intents.terminal", "single executable name"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := writeAndLoad(t, tc.toml).Validate()
			if err == nil {
				t.Fatal("expected a validation error")
			}
			for _, want := range tc.wantSub {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error missing mention of %q: %v", want, err)
				}
			}
		})
	}
}

// A disabled router is not validated: a user turning intents off must not be
// blocked by entries they are no longer using.
func TestDisabledIntentsSkipValidation(t *testing.T) {
	cfg := writeAndLoad(t, `
[intents]
enabled = false

[[intents.custom]]
match = ""
run = ""
`)
	if err := cfg.Validate(); err != nil {
		t.Errorf("disabled intents should not be validated: %v", err)
	}
}

func TestPerformanceDefaultsKeepEnginesWarm(t *testing.T) {
	cfg := Default()
	if !cfg.Performance.WarmEngines {
		t.Error("warm engines must be on by default; presence is the product (ADR 0018)")
	}
	if cfg.Performance.WarmIdleReapSec != 600 || cfg.Performance.WarmMemoryCapMB != 2048 {
		t.Errorf("performance defaults = %+v", cfg.Performance)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the defaults must validate: %v", err)
	}
}

func TestPerformanceSectionParses(t *testing.T) {
	cfg := writeAndLoad(t, `
[performance]
warm_engines = false
warm_memory_cap_mb = 0
warm_idle_reap_sec = 0
`)
	if cfg.Performance.WarmEngines {
		t.Error("warm_engines = false was not read")
	}
	// Zero is the documented "no cap" / "never reap" value, not an error.
	if err := cfg.Validate(); err != nil {
		t.Errorf("zeroes must be accepted: %v", err)
	}
}

func TestPerformanceValidationRejectsUnusableValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		toml string
		want string
	}{
		{"negative cap", "[performance]\nwarm_memory_cap_mb = -1\n", "warm_memory_cap_mb"},
		{"negative reap", "[performance]\nwarm_idle_reap_sec = -5\n", "warm_idle_reap_sec"},
		{
			// A cap below any engine's working set would retire the worker the
			// moment it loaded its model: a restart loop, not a memory budget.
			name: "cap smaller than an engine",
			toml: "[performance]\nwarm_engines = true\nwarm_memory_cap_mb = 16\n",
			want: "at least",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := writeAndLoad(t, tc.toml).Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want a message mentioning %q", err, tc.want)
			}
		})
	}
}

func TestPerformanceSettingsAreEditableAndIdleClass(t *testing.T) {
	// The warm workers live inside the STT/TTS adapters, and adapters are only
	// ever swapped between sessions — so these must not be live-class.
	for _, key := range []string{
		"performance.warm_engines",
		"performance.warm_memory_cap_mb",
		"performance.warm_idle_reap_sec",
	} {
		s, ok := SettingFor(key)
		if !ok {
			t.Fatalf("%s is not in the settings registry", key)
		}
		if s.Reload != ReloadIdle {
			t.Errorf("%s reload class = %q, want idle", key, s.Reload)
		}
	}
}

func TestDesktopToolDefaults(t *testing.T) {
	cfg := Default()
	// On by default, unlike shell.run: each verb is one bounded action on a
	// window the compositor named, and the state-changing ones still ask.
	if !cfg.Tools.Desktop {
		t.Error("tools.desktop should default on")
	}
	if len(cfg.Tools.DesktopApps) != 0 {
		t.Errorf("tools.desktop_apps default = %v, want anything on PATH", cfg.Tools.DesktopApps)
	}
}

func TestDesktopToolOverridesAndValidation(t *testing.T) {
	cfg := writeAndLoad(t, `
[tools]
desktop = false
desktop_apps = ["firefox", "/opt/apps/notes"]
`)
	if cfg.Tools.Desktop {
		t.Error("tools.desktop should be off")
	}
	if !slices.Equal(cfg.Tools.DesktopApps, []string{"firefox", "/opt/apps/notes"}) {
		t.Errorf("desktop_apps = %v", cfg.Tools.DesktopApps)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestDesktopAppsValidationRejectsUnlaunchableEntries(t *testing.T) {
	cfg := writeAndLoad(t, `
[tools]
desktop_apps = ["", "flatpak run org.x.App", "bin/relative"]
`)
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation errors")
	}
	// Each problem names the entry and why it can never launch, because the
	// alternative is a refusal the user only hears when they ask out loud.
	for _, want := range []string{"empty entry", "whitespace", "absolute path"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}
