package knowledge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests drive the real fetch path against stub fetcher scripts —
// child processes, process groups, environment, output caps — with nothing
// reaching the network and nothing depending on anything beyond /bin/sh.

// stubFetcher writes an executable /bin/sh script and returns its path. The
// body is the feed command's whole behaviour: printing values, failing,
// sleeping, leaking.
func stubFetcher(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stub-fetcher")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatalf("write stub fetcher: %v", err)
	}
	return path
}

func fetchFeed(t *testing.T, argv []string, timeout time.Duration) FetchResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return runFeed(ctx, Feed{Name: "stub", Argv: argv, Timeout: timeout}, scrubbedFeedEnv(nil))
}

func TestRunFeedCapturesTheValue(t *testing.T) {
	bin := stubFetcher(t, `printf '187.42\n'`)
	res := fetchFeed(t, []string{bin}, 5*time.Second)
	if res.Err != nil || res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != "187.42" {
		t.Fatalf("fetch = %+v, want the printed value", res)
	}
}

func TestRunFeedArgvIsNeverAShell(t *testing.T) {
	// The argument would print a marker if any shell interpreted it. It must
	// arrive as one inert argv element.
	bin := stubFetcher(t, `printf '%s\n' "$1"`)
	res := fetchFeed(t, []string{bin, `$(echo pwned); echo pwned`}, 5*time.Second)
	if !strings.Contains(res.Stdout, "$(echo pwned)") {
		t.Fatalf("argument was not passed verbatim: %q", res.Stdout)
	}
	if strings.Contains(res.Stdout, "\npwned") {
		t.Fatalf("argv reached a shell: %q", res.Stdout)
	}
}

func TestRunFeedEnvironmentIsScrubbed(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-secret")
	t.Setenv("MY_TOKEN", "also-secret")
	t.Setenv("FEED_HOME", "safe-value")
	bin := stubFetcher(t, `printf 'key=%s token=%s home=%s\n' "$OPENAI_API_KEY" "$MY_TOKEN" "$FEED_HOME"`)
	res := fetchFeed(t, []string{bin}, 5*time.Second)
	if strings.Contains(res.Stdout, "sk-secret") || strings.Contains(res.Stdout, "also-secret") {
		t.Fatalf("a credential leaked into the feed command: %q", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "home=safe-value") {
		t.Fatalf("a harmless variable was withheld: %q", res.Stdout)
	}
}

func TestRunFeedTimeoutKillsTheProcessGroup(t *testing.T) {
	// The script spawns a helper and waits forever; the group kill must end
	// both well before the sleep would.
	bin := stubFetcher(t, `sleep 60 & wait`)
	start := time.Now()
	res := fetchFeed(t, []string{bin}, 100*time.Millisecond)
	if !res.TimedOut {
		t.Fatalf("fetch = %+v, want a timeout", res)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("group kill took %v; helpers were not killed with the parent", elapsed)
	}
}

func TestRunFeedOutputIsCapped(t *testing.T) {
	bin := stubFetcher(t, `head -c 100000 /dev/zero | tr '\0' 'x'`)
	res := fetchFeed(t, []string{bin}, 10*time.Second)
	if !res.Truncated {
		t.Fatal("a 100 KB value was not marked truncated")
	}
	if len(res.Stdout) != maxFeedOutput {
		t.Fatalf("captured %d bytes, want exactly the %d cap", len(res.Stdout), maxFeedOutput)
	}
}

func TestRunFeedMissingBinary(t *testing.T) {
	res := fetchFeed(t, []string{filepath.Join(t.TempDir(), "not-there")}, time.Second)
	if res.Err == nil {
		t.Fatal("a missing command fetched successfully")
	}
}

func TestScrubbedFeedEnvDropsExtras(t *testing.T) {
	t.Setenv("CUSTOM_PROVIDER_AUTH", "x")
	env := scrubbedFeedEnv([]string{"CUSTOM_PROVIDER_AUTH"})
	for _, kv := range env {
		if strings.HasPrefix(kv, "CUSTOM_PROVIDER_AUTH=") {
			t.Fatal("an explicitly named variable survived the scrub")
		}
	}
}
