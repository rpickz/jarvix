package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
)

// Every test here drives the real tool path — registry, policy-free Execute,
// a real child process — against a fake advisor script. Nothing reaches the
// network, and nothing depends on an assistant CLI being installed.

// fakeAdvisor writes an executable /bin/sh script and returns its path. The
// body is the advisor's whole behaviour: canned answers, sleeping, exiting
// non-zero, printing megabytes.
func fakeAdvisor(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-advisor")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatalf("write fake advisor: %v", err)
	}
	return path
}

// askAdvisor runs one consultation through the registry, exactly as the
// engine's tool loop does.
func askAdvisor(t *testing.T, a *Advisor, advisor, question string) string {
	t.Helper()
	return askAdvisorCtx(t, context.Background(), a, advisor, question)
}

func askAdvisorCtx(t *testing.T, ctx context.Context, a *Advisor, advisor, question string) string {
	t.Helper()
	r := NewRegistry(nil)
	r.Register(a)
	args, _ := json.Marshal(map[string]string{"advisor": advisor, "question": question})
	return r.Execute(ctx, ai.ToolCall{Name: "advisor.ask", Arguments: string(args)})
}

// oneAdvisor builds a tool with a single advisor named "oracle".
func oneAdvisor(binary string, args []string, timeout time.Duration) *Advisor {
	return &Advisor{Advisors: []AdvisorSpec{{
		Name: "oracle", Binary: binary, Args: args, Timeout: timeout,
		Description: "test advisor",
	}}}
}

func TestAdvisorAnswerReachesTheModel(t *testing.T) {
	// Echoes back whatever it was asked, proving the question travelled on
	// stdin and the answer travelled back.
	bin := fakeAdvisor(t, `printf 'The build is slow because tests run twice.\n'; cat > /dev/null`)
	out := askAdvisor(t, oneAdvisor(bin, nil, 0), "oracle", "why is my build slow")

	if !strings.Contains(out, "tests run twice") {
		t.Errorf("advisor answer missing from result: %q", out)
	}
	if !strings.Contains(out, "oracle answered") {
		t.Errorf("result should attribute the answer: %q", out)
	}
	if !strings.Contains(out, "no file paths, URLs, or code read out verbatim") {
		t.Errorf("result should steer the spoken answer: %q", out)
	}
}

func TestAdvisorSendsSpeakablePromptOnStdin(t *testing.T) {
	bin := fakeAdvisor(t, `cat`) // the advisor's "answer" is its own prompt
	out := askAdvisor(t, oneAdvisor(bin, nil, 0), "oracle", "explain the CAP theorem")

	if !strings.Contains(out, "explain the CAP theorem") {
		t.Errorf("question did not reach the advisor: %q", out)
	}
	if !strings.Contains(out, "read aloud") || !strings.Contains(out, "no markdown") {
		t.Errorf("prompt must ask for a speakable answer: %q", out)
	}
}

func TestAdvisorQuestionAsSingleArgumentIsNeverInterpreted(t *testing.T) {
	// The placeholder form: the question must arrive as exactly one argv
	// element, with shell syntax inside it inert.
	bin := fakeAdvisor(t, `printf 'argc=%s\n' "$#"; printf 'arg:%s\n' "$@"`)
	a := oneAdvisor(bin, []string{"--ask", AdvisorQuestionPlaceholder}, 0)
	out := askAdvisor(t, a, "oracle", "what does $(whoami) mean; rm -rf /tmp/nope && echo pwned")

	if !strings.Contains(out, "argc=2") {
		t.Errorf("question must be exactly one argument: %q", out)
	}
	if !strings.Contains(out, "$(whoami)") || !strings.Contains(out, "rm -rf /tmp/nope") {
		t.Errorf("question must arrive verbatim: %q", out)
	}
	// Interpretation would put "pwned" on a line of its own; quoted inside
	// the argument it is only ever preceded by "echo ".
	if strings.Contains(out, "\npwned") {
		t.Fatalf("shell syntax in the question was interpreted: %q", out)
	}
}

func TestAdvisorArgvComesOnlyFromConfig(t *testing.T) {
	// Whatever else the model puts in the arguments object — a binary, extra
	// flags, an environment — is not part of the tool's input contract and
	// cannot reach the child.
	bin := fakeAdvisor(t, `printf 'args:[%s]\n' "$*"; printf 'me:%s\n' "$0"`)
	a := oneAdvisor(bin, []string{"--configured-flag"}, 0)

	r := NewRegistry(nil)
	r.Register(a)
	out := r.Execute(context.Background(), ai.ToolCall{Name: "advisor.ask", Arguments: `{
		"advisor": "oracle",
		"question": "hello",
		"binary": "/bin/sh",
		"args": ["-c", "echo pwned"],
		"env": {"ANTHROPIC_API_KEY": "sk-injected"},
		"timeout_sec": 1
	}`})

	if !strings.Contains(out, "args:[--configured-flag]") {
		t.Errorf("argv must come from config alone: %q", out)
	}
	if strings.Contains(out, "pwned") || strings.Contains(out, "/bin/sh") {
		t.Fatalf("model-supplied binary or args were honoured: %q", out)
	}
	if !strings.Contains(out, "me:"+bin) {
		t.Errorf("configured binary was not the one executed: %q", out)
	}
}

func TestAdvisorEnvironmentIsScrubbed(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-anthropic-secret")
	t.Setenv("OPENAI_API_KEY", "sk-openai-secret")
	t.Setenv("GITHUB_TOKEN", "ghp-secret")
	t.Setenv("DB_PASSWORD", "hunter2")
	t.Setenv("SIGNING_KEY", "priv-secret")
	t.Setenv("MY_PROVIDER_CREDENTIALS", "cred-secret")
	t.Setenv("JARVIX_TEST_KEEPME", "kept")

	bin := fakeAdvisor(t, `env`)
	a := oneAdvisor(bin, nil, 0)
	// The daemon adds the api_key_env of every configured endpoint; a name
	// that looks innocent must still be withheld.
	a.ScrubEnv = []string{"MYSERVER_SECRET_VALUE"}
	t.Setenv("MYSERVER_SECRET_VALUE", "also-secret")

	out := askAdvisor(t, a, "oracle", "what is in your environment")

	for _, leaked := range []string{
		"sk-anthropic-secret", "sk-openai-secret", "ghp-secret",
		"hunter2", "priv-secret", "cred-secret", "also-secret",
	} {
		if strings.Contains(out, leaked) {
			t.Errorf("secret leaked into the advisor's environment: %q", leaked)
		}
	}
	// Scrubbing must not amount to an empty environment: the CLI still needs
	// to find its own configuration and credentials.
	for _, kept := range []string{"PATH=", "HOME=", "JARVIX_TEST_KEEPME=kept"} {
		if !strings.Contains(out, kept) {
			t.Errorf("advisor environment lost %q: %q", kept, out)
		}
	}
}

func TestAdvisorTimeoutKillsTheProcessGroup(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "grandchild-ran")
	t.Setenv("JARVIX_TEST_MARKER", marker)
	// A helper process outliving its parent is exactly what assistant CLIs
	// spawn; killing only the parent would leave it running.
	bin := fakeAdvisor(t, `( sleep 1; echo ran > "$JARVIX_TEST_MARKER" ) &
sleep 30`)

	start := time.Now()
	out := askAdvisor(t, oneAdvisor(bin, nil, 150*time.Millisecond), "oracle", "think forever")
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("timeout not enforced (took %s)", elapsed)
	}
	if !strings.Contains(out, "did not answer") {
		t.Errorf("result should report the timeout: %q", out)
	}
	time.Sleep(1500 * time.Millisecond) // past the grandchild's own sleep
	if _, err := os.Stat(marker); err == nil {
		t.Error("grandchild survived the timeout: the process group was not killed")
	}
}

func TestAdvisorCancellationKillsTheChild(t *testing.T) {
	bin := fakeAdvisor(t, `sleep 30`)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()

	start := time.Now()
	out := askAdvisorCtx(t, ctx, oneAdvisor(bin, nil, 60*time.Second), "oracle", "think forever")

	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("session cancellation did not kill the advisor (took %s)", elapsed)
	}
	if !strings.Contains(out, "interrupted") {
		t.Errorf("result = %q", out)
	}
}

func TestAdvisorFailureNeverExposesStderr(t *testing.T) {
	bin := fakeAdvisor(t, `echo "Traceback: /home/user/.config/secret-path/oauth.json missing" >&2; exit 7`)
	out := askAdvisor(t, oneAdvisor(bin, nil, 0), "oracle", "review this")

	if strings.Contains(out, "Traceback") || strings.Contains(out, "oauth.json") {
		t.Fatalf("raw stderr must never reach the model (it gets spoken): %q", out)
	}
	if !strings.Contains(out, "failed to answer") || !strings.Contains(out, "one short sentence") {
		t.Errorf("result should ask for a one-sentence spoken failure: %q", out)
	}
	if !strings.Contains(out, "do not retry") {
		t.Errorf("result should stop the model retrying: %q", out)
	}
}

func TestAdvisorNotInstalled(t *testing.T) {
	a := oneAdvisor("jarvix-definitely-not-installed", nil, 0)
	out := askAdvisor(t, a, "oracle", "review this")
	if !strings.Contains(out, "not installed") {
		t.Errorf("result = %q", out)
	}

	// An absolute path that is not executable fails the same way, rather
	// than as an exec error nobody can act on.
	plain := filepath.Join(t.TempDir(), "not-executable")
	if err := os.WriteFile(plain, []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := askAdvisor(t, oneAdvisor(plain, nil, 0), "oracle", "review this"); !strings.Contains(out, "not installed") {
		t.Errorf("result = %q", out)
	}
}

func TestAdvisorOutputIsCapped(t *testing.T) {
	bin := fakeAdvisor(t, `yes "a very long advisory answer" | head -5000`)
	a := oneAdvisor(bin, nil, 0)
	a.MaxOutput = 512
	out := askAdvisor(t, a, "oracle", "say a lot")

	if len(out) > 4096 {
		t.Errorf("output cap not applied: %d bytes", len(out))
	}
	if !strings.Contains(out, "truncated") {
		t.Errorf("truncation must be marked: %q", out[max(0, len(out)-120):])
	}
}

func TestAdvisorEmptyAnswer(t *testing.T) {
	bin := fakeAdvisor(t, `exit 0`)
	if out := askAdvisor(t, oneAdvisor(bin, nil, 0), "oracle", "hello"); !strings.Contains(out, "no answer") {
		t.Errorf("result = %q", out)
	}
}

func TestAdvisorRejectsBadInput(t *testing.T) {
	a := oneAdvisor("/bin/true", nil, 0)
	for name, input := range map[string]string{
		"malformed":       `not json`,
		"no question":     `{"advisor":"oracle"}`,
		"blank question":  `{"advisor":"oracle","question":"   "}`,
		"unknown advisor": `{"advisor":"gpt5","question":"hi"}`,
	} {
		if _, err := a.Execute(context.Background(), json.RawMessage(input)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	// Through the registry, the same failures come back as readable text so
	// the session survives them.
	out := askAdvisor(t, a, "gpt5", "hi")
	if !strings.Contains(out, "unknown advisor") || !strings.Contains(out, "oracle") {
		t.Errorf("unknown advisor should name what is configured: %q", out)
	}
}

func TestAdvisorSchemaOffersOnlyConfiguredAdvisors(t *testing.T) {
	a := &Advisor{Advisors: []AdvisorSpec{
		{Name: "oracle", Description: "deep reasoning"},
		{Name: "scribe", Description: "writes things"},
	}}
	var schema struct {
		Properties struct {
			Advisor struct {
				Enum        []string `json:"enum"`
				Description string   `json:"description"`
			} `json:"advisor"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(a.Schema(), &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if len(schema.Properties.Advisor.Enum) != 2 || schema.Properties.Advisor.Enum[0] != "oracle" {
		t.Errorf("enum = %v", schema.Properties.Advisor.Enum)
	}
	if !strings.Contains(schema.Properties.Advisor.Description, "deep reasoning") {
		t.Errorf("per-advisor descriptions should guide the choice: %q", schema.Properties.Advisor.Description)
	}
	if len(schema.Required) != 2 {
		t.Errorf("required = %v", schema.Required)
	}
	// The description is the local-first steer; it must say so plainly.
	desc := a.Description()
	if !strings.Contains(desc, "ONLY") || !strings.Contains(desc, "Answer everything else yourself") {
		t.Errorf("description must steer the model local-first: %q", desc)
	}
}

func TestAdvisorActivityDescribesTheWait(t *testing.T) {
	a := oneAdvisor("/bin/true", nil, 0)
	args, _ := json.Marshal(map[string]string{"advisor": "oracle", "question": "hi"})
	label, waiting, ok := a.Activity(args)
	if !ok || !strings.Contains(label, "Consulting oracle") {
		t.Errorf("label = %q, ok = %v", label, ok)
	}
	if !strings.Contains(waiting, "oracle") {
		t.Errorf("waiting = %q", waiting)
	}
	// Unknown or unparseable calls have nothing to announce.
	if _, _, ok := a.Activity(json.RawMessage(`{"advisor":"nobody"}`)); ok {
		t.Error("unknown advisor must not produce an activity label")
	}
	if _, _, ok := a.Activity(json.RawMessage(`nonsense`)); ok {
		t.Error("unparseable arguments must not produce an activity label")
	}
	// And the registry only surfaces it for tools that opt in.
	r := NewRegistry(nil)
	r.Register(a)
	r.Register(&Shell{})
	if _, _, ok := r.Activity(ai.ToolCall{Name: "shell.run", Arguments: `{"command":"ls"}`}); ok {
		t.Error("shell.run is not progressive")
	}
	if _, _, ok := r.Activity(ai.ToolCall{Name: "advisor.ask", Arguments: string(args)}); !ok {
		t.Error("advisor.ask should report activity through the registry")
	}
}

func TestScrubbedEnvKeepsOnlyNonSecrets(t *testing.T) {
	in := []string{
		"PATH=/usr/bin", "HOME=/home/u", "LANG=en_GB.UTF-8",
		"ANTHROPIC_API_KEY=x", "anthropic_api_key=x", "GH_TOKEN=x",
		"AWS_SECRET_ACCESS_KEY=x", "SIGNING_KEY=x", "PGPASSWORD=x",
		"NOT_AN_ENV_ENTRY", "CUSTOM=keepme",
	}
	got := strings.Join(scrubbedEnv(in, []string{"custom"}), " ")
	for _, banned := range []string{"ANTHROPIC", "anthropic", "TOKEN", "SECRET", "SIGNING_KEY", "PGPASSWORD", "CUSTOM"} {
		if strings.Contains(got, banned) {
			t.Errorf("scrubbedEnv kept %q: %q", banned, got)
		}
	}
	for _, kept := range []string{"PATH=/usr/bin", "HOME=/home/u", "LANG="} {
		if !strings.Contains(got, kept) {
			t.Errorf("scrubbedEnv dropped %q: %q", kept, got)
		}
	}
	if strings.Contains(got, "NOT_AN_ENV_ENTRY") {
		t.Errorf("malformed entries should be dropped: %q", got)
	}
}
