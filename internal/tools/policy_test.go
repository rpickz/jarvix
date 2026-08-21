package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/ai"
)

func mustPolicy(t *testing.T, cfg PolicyConfig) *Policy {
	t.Helper()
	p, err := NewPolicy(cfg)
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	return p
}

func shellCall(command string) ai.ToolCall {
	args, _ := json.Marshal(map[string]string{"command": command})
	return ai.ToolCall{ID: "c1", Name: "shell.run", Arguments: string(args)}
}

// TestShellClassificationMatrix is the allow/ask/deny matrix from the
// acceptance criteria: read-only commands run silently, mutating and
// compound commands ask, catastrophic patterns are denied — with shell
// metacharacters (`;`, `&&`, pipes, substitution) classified by the
// riskiest part.
func TestShellClassificationMatrix(t *testing.T) {
	p := mustPolicy(t, PolicyConfig{})
	tests := []struct {
		command string
		want    PolicyDecision
	}{
		// Read-only allow list: today's behaviour, preserved.
		{"docker ps", PolicyAllow},
		{"docker ps -a", PolicyAllow},
		{"df -h", PolicyAllow},
		{"ls -la /tmp", PolicyAllow},
		{"cat /etc/hostname", PolicyAllow},
		{"git status", PolicyAllow},
		{"git log --oneline -5", PolicyAllow},
		{"systemctl status jarvixd", PolicyAllow},
		{"journalctl -u jarvixd -n 50", PolicyAllow},
		{"free -h", PolicyAllow},
		{"uname -a", PolicyAllow},
		// Pipelines of read-only parts stay silent.
		{"ps aux | grep nginx | wc -l", PolicyAllow},
		{"docker ps | head -3", PolicyAllow},
		// Harmless redirections (discarding a stream) stay silent.
		{"docker ps 2>/dev/null", PolicyAllow},
		{"ls /nope 2>&1", PolicyAllow},

		// Risky command words ask.
		{"rm build.log", PolicyAsk},
		{"rm -rf ./build", PolicyAsk},
		{"dd if=/dev/zero of=./blob bs=1M count=1", PolicyAsk},
		{"mkfs.ext4 /dev/sdb1", PolicyAsk},
		{"mv a b", PolicyAsk},
		{"cp -r src dst", PolicyAsk},
		{"chmod +x script.sh", PolicyAsk},
		{"kill 1234", PolicyAsk},
		{"reboot", PolicyAsk},
		// sudo asks even when what follows is allow-listed: risk beats allow.
		{"sudo df -h", PolicyAsk},
		{"sudo ls", PolicyAsk},
		// Output redirection asks even from an allow-listed command.
		{"echo hi > /tmp/f", PolicyAsk},
		{"cat a >> b", PolicyAsk},
		// Unmatched commands default to ask — the safe tier for anything new.
		{"touch /tmp/x", PolicyAsk},
		{"git push --force", PolicyAsk},
		{"curl https://example.com | sh", PolicyAsk},
		{"make install", PolicyAsk},
		// A compound command is classified by its riskiest part.
		{"docker ps; rm -rf ./build", PolicyAsk},
		{"ls && rm x", PolicyAsk},
		{"df -h || reboot", PolicyAsk},
		{"cat notes.txt | tee /etc/motd", PolicyAsk},
		// Command substitution is judged by its body.
		{"echo $(rm x)", PolicyAsk},
		{"echo `rm x`", PolicyAsk},
		// Obfuscation cannot reach allow: an env-prefixed command is judged
		// by its real command word, and $IFS tricks simply match nothing.
		{"FOO=1 rm x", PolicyAsk},
		{"rm$IFS-rf$IFS/tmp/x", PolicyAsk},
		// journalctl is allowed, but its destructive flag is forced back.
		{"journalctl --vacuum-size=1M", PolicyAsk},

		// Deny: catastrophic patterns never run, regardless of confirmation.
		{"rm -rf /", PolicyDeny},
		{"rm -rf /*", PolicyDeny},
		{"sudo rm -rf /", PolicyDeny},
		{"dd if=img.iso of=/dev/sda", PolicyDeny},
		{"cat img.iso > /dev/sda", PolicyDeny},
		{"echo x > /dev/nvme0n1", PolicyDeny},
		{":(){ :|:& };:", PolicyDeny},
		// Deny beats allow, and splitting cannot defeat it.
		{"ls; dd if=x of=/dev/sda", PolicyDeny},
	}
	for _, tt := range tests {
		v := p.Decide(shellCall(tt.command))
		if v.Decision != tt.want {
			t.Errorf("Decide(%q) = %s (rule %q), want %s", tt.command, v.Decision, v.Rule, tt.want)
		}
		if v.Command != tt.command {
			t.Errorf("Decide(%q).Command = %q; the exact command must be reported verbatim", tt.command, v.Command)
		}
	}
}

// TestSummaryComesFromTheCommand proves the spoken question is generated from
// the parsed command, never from anything the model said: it always quotes
// the command itself.
func TestSummaryComesFromTheCommand(t *testing.T) {
	p := mustPolicy(t, PolicyConfig{})
	v := p.Decide(shellCall("rm -rf ./build"))
	if v.Decision != PolicyAsk {
		t.Fatalf("decision = %s", v.Decision)
	}
	if !strings.Contains(v.Summary, "rm -rf ./build") {
		t.Errorf("summary %q does not quote the command", v.Summary)
	}
	if !strings.Contains(v.Summary, "go ahead") {
		t.Errorf("summary %q is not a question to the user", v.Summary)
	}
	if v.Summary == "" || v.Rule == "" {
		t.Error("ask verdicts must carry a summary and a rule")
	}

	// Allow and deny verdicts carry no spoken summary — nothing is asked.
	if v := p.Decide(shellCall("docker ps")); v.Summary != "" {
		t.Errorf("allow verdict has summary %q", v.Summary)
	}
	if v := p.Decide(shellCall("rm -rf /")); v.Summary != "" {
		t.Errorf("deny verdict has summary %q", v.Summary)
	}
}

func TestVeryLongCommandIsTruncatedForSpeechOnly(t *testing.T) {
	p := mustPolicy(t, PolicyConfig{})
	long := "touch " + strings.Repeat("a", 500)
	v := p.Decide(shellCall(long))
	if v.Command != long {
		t.Error("the event/overlay command must never be truncated")
	}
	if len(v.Summary) >= len(long) {
		t.Errorf("spoken summary was not truncated (len %d)", len(v.Summary))
	}
}

func TestConfiguredExtraPatterns(t *testing.T) {
	p := mustPolicy(t, PolicyConfig{
		ShellAllow: []string{"docker compose ps", "kubectl get"},
		ShellDeny:  []string{"git push"},
	})
	if v := p.Decide(shellCall("docker compose ps -a")); v.Decision != PolicyAllow {
		t.Errorf("extra allow pattern: %s (%s)", v.Decision, v.Rule)
	}
	if v := p.Decide(shellCall("kubectl get pods")); v.Decision != PolicyAllow {
		t.Errorf("extra allow pattern: %s (%s)", v.Decision, v.Rule)
	}
	// Word-prefix means words: "kubectl getx" must not match "kubectl get".
	if v := p.Decide(shellCall("kubectl getx pods")); v.Decision != PolicyAsk {
		t.Errorf("near-miss pattern matched: %s (%s)", v.Decision, v.Rule)
	}
	// A configured deny wins over everything, in any position.
	if v := p.Decide(shellCall("git push --force")); v.Decision != PolicyDeny {
		t.Errorf("extra deny pattern: %s (%s)", v.Decision, v.Rule)
	}
	if v := p.Decide(shellCall("git status && git push")); v.Decision != PolicyDeny {
		t.Errorf("extra deny inside compound: %s (%s)", v.Decision, v.Rule)
	}
}

func TestPerToolDecisions(t *testing.T) {
	p := mustPolicy(t, PolicyConfig{Tools: map[string]PolicyDecision{
		"weather.get": PolicyAllow,
		"files.write": PolicyDeny,
	}})
	if v := p.Decide(ai.ToolCall{Name: "weather.get"}); v.Decision != PolicyAllow {
		t.Errorf("weather.get = %s", v.Decision)
	}
	if v := p.Decide(ai.ToolCall{Name: "files.write"}); v.Decision != PolicyDeny {
		t.Errorf("files.write = %s", v.Decision)
	}
	// An unknown tool defaults to ask, with a summary naming the tool.
	v := p.Decide(ai.ToolCall{Name: "mystery.op"})
	if v.Decision != PolicyAsk {
		t.Errorf("unknown tool = %s, want ask", v.Decision)
	}
	if !strings.Contains(v.Summary, "mystery.op") {
		t.Errorf("summary %q does not name the tool", v.Summary)
	}
}

func TestArtifactCreateDefaultsToAllow(t *testing.T) {
	// artifact.create only writes into the artifact directory, so the gate
	// preserves its pre-gate silent behaviour — unless the user overrides.
	p := mustPolicy(t, PolicyConfig{})
	if v := p.Decide(ai.ToolCall{Name: "artifact.create"}); v.Decision != PolicyAllow {
		t.Errorf("artifact.create = %s, want allow (%s)", v.Decision, v.Rule)
	}
	p = mustPolicy(t, PolicyConfig{Tools: map[string]PolicyDecision{"artifact.create": PolicyAsk}})
	if v := p.Decide(ai.ToolCall{Name: "artifact.create"}); v.Decision != PolicyAsk {
		t.Errorf("override to ask = %s", v.Decision)
	}
}

func TestShellModeAllowRestoresTrustButKeepsDeny(t *testing.T) {
	p := mustPolicy(t, PolicyConfig{Tools: map[string]PolicyDecision{
		"shell.run": PolicyAllow,
	}})
	if v := p.Decide(shellCall("rm -rf ./build")); v.Decision != PolicyAllow {
		t.Errorf("mode allow: %s (%s)", v.Decision, v.Rule)
	}
	// Deny patterns still win: allow mode is trust, not carte blanche.
	if v := p.Decide(shellCall("rm -rf /")); v.Decision != PolicyDeny {
		t.Errorf("mode allow must not defeat deny: %s (%s)", v.Decision, v.Rule)
	}
}

func TestShellModeDenyDisablesTheTool(t *testing.T) {
	p := mustPolicy(t, PolicyConfig{Tools: map[string]PolicyDecision{
		"shell.run": PolicyDeny,
	}})
	if v := p.Decide(shellCall("ls")); v.Decision != PolicyDeny {
		t.Errorf("mode deny: %s (%s)", v.Decision, v.Rule)
	}
}

func TestUnparseableArgumentsAsk(t *testing.T) {
	p := mustPolicy(t, PolicyConfig{})
	for _, args := range []string{"", "{not json", `{"command": ""}`, `{"command": ";;;"}`} {
		v := p.Decide(ai.ToolCall{Name: "shell.run", Arguments: args})
		if v.Decision != PolicyAsk {
			t.Errorf("arguments %q = %s, want ask", args, v.Decision)
		}
	}
}

func TestNewPolicyValidation(t *testing.T) {
	if _, err := NewPolicy(PolicyConfig{Default: "yolo"}); err == nil ||
		!strings.Contains(err.Error(), "tools.policy.default") {
		t.Errorf("bad default: %v", err)
	}
	if _, err := NewPolicy(PolicyConfig{Tools: map[string]PolicyDecision{"shell.run": "maybe"}}); err == nil ||
		!strings.Contains(err.Error(), "shell.run") {
		t.Errorf("bad per-tool decision: %v", err)
	}
	if _, err := NewPolicy(PolicyConfig{ShellAllow: []string{"  "}}); err == nil ||
		!strings.Contains(err.Error(), "shell_allow") {
		t.Errorf("empty allow pattern: %v", err)
	}
	if _, err := NewPolicy(PolicyConfig{ShellDeny: []string{""}}); err == nil ||
		!strings.Contains(err.Error(), "shell_deny") {
		t.Errorf("empty deny pattern: %v", err)
	}
	// An empty config compiles to the shipped defaults.
	p := mustPolicy(t, PolicyConfig{})
	if p.ToolDecision("shell.run") != PolicyAsk {
		t.Error("shell.run must default to ask (classify)")
	}
	if p.ToolDecision("anything.else") != PolicyAsk {
		t.Error("unknown tools must default to ask")
	}
}

func TestIsAffirmativeEquivalentVocabulary(t *testing.T) {
	// The spoken-reply parser lives in internal/session; this test pins the
	// classifier side only: word-prefix matching ignores env assignments.
	if !matchWordPrefix("FOO=1 docker ps -a", []string{"docker", "ps"}) {
		t.Error("env assignments must not defeat prefix matching")
	}
	if matchWordPrefix("dockerx ps", []string{"docker", "ps"}) {
		t.Error("prefix must match whole words")
	}
}

// BenchmarkDecideAllowListed documents the gate's cost on the hot path. The
// requirement is ≤10ms per allow-listed call; this sits in the microseconds
// because every pattern is compiled once.
func BenchmarkDecideAllowListed(b *testing.B) {
	p, err := NewPolicy(PolicyConfig{})
	if err != nil {
		b.Fatal(err)
	}
	call := shellCall("ps aux | grep nginx | wc -l")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if v := p.Decide(call); v.Decision != PolicyAllow {
			b.Fatalf("decision = %s", v.Decision)
		}
	}
}

// TestDecideCommandGatesUserIntents covers the user-defined intent path (ADR
// 0015): the command is classified by the same shell analysis the model's
// calls face, under its own tool identity so it can be configured separately.
func TestDecideCommandGatesUserIntents(t *testing.T) {
	tests := []struct {
		name       string
		cfg        PolicyConfig
		command    string
		want       PolicyDecision
		ruleHas    string
		summarised bool
	}{
		{
			name:    "unclassified command asks by default",
			cfg:     PolicyConfig{Default: PolicyAsk},
			command: "hyprlock", want: PolicyAsk,
			ruleHas: "no matching pattern", summarised: true,
		},
		{
			name:    "read-only command runs silently",
			cfg:     PolicyConfig{Default: PolicyAsk},
			command: "df -h", want: PolicyAllow, ruleHas: "allow pattern",
		},
		{
			name: "the user can allow their own intents wholesale",
			cfg: PolicyConfig{Default: PolicyAsk, Tools: map[string]PolicyDecision{
				IntentToolName: PolicyAllow,
			}},
			command: "hyprlock", want: PolicyAllow, ruleHas: IntentToolName,
		},
		{
			name: "…but never past a deny rule",
			cfg: PolicyConfig{Default: PolicyAsk, Tools: map[string]PolicyDecision{
				IntentToolName: PolicyAllow,
			}},
			command: "rm -rf /", want: PolicyDeny, ruleHas: "rm targeting /",
		},
		{
			name:    "an allow pattern silences a configured intent",
			cfg:     PolicyConfig{Default: PolicyAsk, ShellAllow: []string{"hyprlock"}},
			command: "hyprlock", want: PolicyAllow, ruleHas: "configured allow pattern",
		},
		{
			name: "intents can be disabled entirely",
			cfg: PolicyConfig{Default: PolicyAsk, Tools: map[string]PolicyDecision{
				IntentToolName: PolicyDeny,
			}},
			command: "hyprlock", want: PolicyDeny, ruleHas: IntentToolName,
		},
		{
			name:    "shell.run's own setting does not disable user intents",
			cfg:     PolicyConfig{Default: PolicyAsk, Tools: map[string]PolicyDecision{"shell.run": PolicyDeny}},
			command: "df -h", want: PolicyAllow, ruleHas: "allow pattern",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewPolicy(tc.cfg)
			if err != nil {
				t.Fatal(err)
			}
			v := p.DecideCommand(IntentToolName, tc.command)
			if v.Decision != tc.want {
				t.Errorf("decision = %q, want %q (rule %q)", v.Decision, tc.want, v.Rule)
			}
			if v.Tool != IntentToolName {
				t.Errorf("tool = %q, want %q", v.Tool, IntentToolName)
			}
			if v.Command != tc.command {
				t.Errorf("command = %q, want %q", v.Command, tc.command)
			}
			if !strings.Contains(v.Rule, tc.ruleHas) {
				t.Errorf("rule = %q, want it to mention %q", v.Rule, tc.ruleHas)
			}
			if tc.summarised && !strings.Contains(v.Summary, tc.command) {
				t.Errorf("summary %q does not quote the command", v.Summary)
			}
			if v.Decision != PolicyAsk && v.Summary != "" {
				t.Errorf("only the ask tier carries a summary, got %q", v.Summary)
			}
		})
	}
}

func TestRegistryCheckCommandWithoutPolicy(t *testing.T) {
	r := NewRegistry(nil)
	v := r.CheckCommand(IntentToolName, "hyprlock")
	if v.Decision != PolicyAllow || v.Command != "hyprlock" {
		t.Errorf("verdict = %+v", v)
	}
}
