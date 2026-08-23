package script

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The runner tests execute real child processes — stub scripts written into
// test-owned temp dirs, never a user's files — because the properties under
// test are process facts: what argv the child saw, what environment it got,
// what happened to its output, and whether its whole group died on time.
// Synchronisation is by events and process exit, never by sleeping in the
// test; where a timeout must fire, the budget is milliseconds and the stub
// blocks forever, so the outcome is determined, not raced.

// recorder collects published events. Publish is called from whatever
// goroutine runs Run, so it locks.
type recorder struct {
	mu     sync.Mutex
	events []string
	data   []map[string]any
	// started is closed on the first script.started, giving concurrency
	// tests a deterministic "the runner has claimed the run" signal.
	started chan struct{}
	once    sync.Once
}

func newRecorder() *recorder { return &recorder{started: make(chan struct{})} }

func (r *recorder) publish(event string, data map[string]any) {
	r.mu.Lock()
	r.events = append(r.events, event)
	r.data = append(r.data, data)
	r.mu.Unlock()
	if event == "script.started" {
		r.once.Do(func() { close(r.started) })
	}
}

func (r *recorder) find(event string) (map[string]any, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, e := range r.events {
		if e == event {
			return r.data[i], true
		}
	}
	return nil, false
}

func newRunner(t *testing.T, defs ...Definition) (*Runner, *recorder) {
	t.Helper()
	rec := newRecorder()
	return New(Options{Definitions: defs, Publish: rec.publish}), rec
}

func def(t *testing.T, dir, body string, report Report) Definition {
	t.Helper()
	return Definition{
		Name:    "backup notes",
		Phrases: []string{"backup my notes"},
		Path:    stubScript(t, dir, "backup.sh", body),
		Timeout: 10 * time.Second,
		Report:  report,
	}
}

func TestRunSummarySuccess(t *testing.T) {
	r, rec := newRunner(t, def(t, t.TempDir(), "echo ignored for summary; exit 0", ReportSummary))
	line, err := r.Run(context.Background(), "backup notes")
	if err != nil {
		t.Fatal(err)
	}
	if line != "Backup notes finished." {
		t.Errorf("spoken line = %q", line)
	}
	data, ok := rec.find("script.finished")
	if !ok {
		t.Fatal("no script.finished event")
	}
	if data["status"] != "ok" || data["exit_code"] != 0 {
		t.Errorf("event = %v", data)
	}
	if _, ok := data["duration_ms"]; !ok {
		t.Errorf("event carries no duration: %v", data)
	}
}

func TestRunStdoutModeSpeaksFirstLine(t *testing.T) {
	r, _ := newRunner(t, def(t, t.TempDir(),
		"echo 'Notes backed up: 12 files.'; echo 'second line stays unspoken'", ReportStdout))
	line, err := r.Run(context.Background(), "backup notes")
	if err != nil {
		t.Fatal(err)
	}
	if line != "Notes backed up: 12 files." {
		t.Errorf("spoken line = %q", line)
	}
}

func TestRunStdoutModeWithNoOutputStillSaysSomething(t *testing.T) {
	r, _ := newRunner(t, def(t, t.TempDir(), "exit 0", ReportStdout))
	line, err := r.Run(context.Background(), "backup notes")
	if err != nil {
		t.Fatal(err)
	}
	if line != "Backup notes finished." {
		t.Errorf("spoken line = %q", line)
	}
}

func TestRunStdoutModeCapsTheSpokenLine(t *testing.T) {
	// One enormous line: the cap must cut it for speech without failing the run.
	r, _ := newRunner(t, def(t, t.TempDir(), `printf 'x%.0s' $(seq 1 5000)`, ReportStdout))
	line, err := r.Run(context.Background(), "backup notes")
	if err != nil {
		t.Fatal(err)
	}
	if got := len([]rune(line)); got > maxSpokenLine+1 { // +1 for the ellipsis
		t.Errorf("spoken line is %d runes", got)
	}
	if !strings.HasSuffix(line, "…") {
		t.Errorf("capped line %q carries no truncation mark", line[:20])
	}
}

func TestRunSilentSuccessSaysNothing(t *testing.T) {
	r, rec := newRunner(t, def(t, t.TempDir(), "echo chatty; exit 0", ReportSilent))
	line, err := r.Run(context.Background(), "backup notes")
	if err != nil {
		t.Fatal(err)
	}
	if line != "" {
		t.Errorf("silent success spoke %q", line)
	}
	// Silence for the ear, never for the record: the feed still gets the run.
	if data, ok := rec.find("script.finished"); !ok || data["status"] != "ok" {
		t.Errorf("silent run left no activity trail: %v, %v", data, ok)
	}
}

// TestRunFailureIsSpokenInEveryMode is the acceptance criterion "failures
// ALWAYS spoken": each report mode's failing run comes back as an error the
// engine speaks unconditionally, naming the exit code and stderr's first line.
func TestRunFailureIsSpokenInEveryMode(t *testing.T) {
	for _, mode := range []Report{ReportSummary, ReportStdout, ReportSilent} {
		t.Run(string(mode), func(t *testing.T) {
			r, rec := newRunner(t, def(t, t.TempDir(),
				"echo 'disk full' >&2; echo 'partial' ; exit 2", mode))
			_, err := r.Run(context.Background(), "backup notes")
			if err == nil {
				t.Fatalf("mode %s swallowed the failure", mode)
			}
			if want := "backup notes failed — exit 2: disk full"; err.Error() != want {
				t.Errorf("err = %q, want %q", err, want)
			}
			data, ok := rec.find("script.finished")
			if !ok || data["status"] != "failed" || data["exit_code"] != 2 {
				t.Errorf("event = %v, %v", data, ok)
			}
		})
	}
}

func TestRunFailureWithoutStderrNamesTheExitCode(t *testing.T) {
	r, _ := newRunner(t, def(t, t.TempDir(), "exit 7", ReportSummary))
	_, err := r.Run(context.Background(), "backup notes")
	if err == nil || err.Error() != "backup notes failed — exit 7" {
		t.Errorf("err = %v", err)
	}
}

// TestRunPassesZeroArgumentsAndAScrubbedEnv is the no-args-from-anywhere
// property, asserted from inside the child: the script reports its own
// argument count, its arguments, and two environment variables — one
// credential-shaped, one plain. Speech cannot reach argv because *nothing*
// reaches argv; the secret must be gone while ordinary variables survive.
func TestRunPassesZeroArgumentsAndAScrubbedEnv(t *testing.T) {
	t.Setenv("JARVIX_TEST_API_KEY", "secret-value")
	t.Setenv("JARVIX_TEST_PLAIN", "hello")
	r, _ := newRunner(t, def(t, t.TempDir(),
		`printf '%s|%s|%s|%s' "$#" "$*" "${JARVIX_TEST_API_KEY:-scrubbed}" "${JARVIX_TEST_PLAIN:-missing}"`,
		ReportStdout))
	line, err := r.Run(context.Background(), "backup notes")
	if err != nil {
		t.Fatal(err)
	}
	if line != "0||scrubbed|hello" {
		t.Errorf("child saw %q; want zero args, no secret, plain env intact", line)
	}
}

func TestRunUnknownScriptIsASentence(t *testing.T) {
	r, _ := newRunner(t)
	_, err := r.Run(context.Background(), "backup notes")
	if err == nil || !strings.Contains(err.Error(), `no script is called "backup notes"`) {
		t.Errorf("err = %v", err)
	}
}

// TestRunRechecksTheFileAtRunTime: config validation passed when the file
// existed; deleting it afterwards must produce a spoken sentence, not an
// exec error — the gate named this path, so its absence is user-facing news.
func TestRunRechecksTheFileAtRunTime(t *testing.T) {
	d := def(t, t.TempDir(), "exit 0", ReportSummary)
	r, rec := newRunner(t, d)
	if err := os.Remove(d.Path); err != nil {
		t.Fatal(err)
	}
	_, err := r.Run(context.Background(), "backup notes")
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("err = %v", err)
	}
	if _, ok := rec.find("script.started"); ok {
		t.Error("a run refused at the re-check still published script.started")
	}
}

// TestRunTimeoutKillsTheProcessGroup: a script that blocks forever (and has
// spawned its own child) is stopped at the timeout, group and all, and the
// failure names the stop. The 100ms budget bounds the test; the stub would
// otherwise run for minutes, so a pass is only reachable through the kill.
func TestRunTimeoutKillsTheProcessGroup(t *testing.T) {
	d := def(t, t.TempDir(), "sleep 120 &\nexec sleep 120", ReportSummary)
	d.Timeout = 100 * time.Millisecond
	r, rec := newRunner(t, d)
	_, err := r.Run(context.Background(), "backup notes")
	if err == nil || !strings.Contains(err.Error(), "was stopped") {
		t.Errorf("err = %v", err)
	}
	data, ok := rec.find("script.finished")
	if !ok || data["status"] != "failed" || data["timed_out"] != true {
		t.Errorf("event = %v, %v", data, ok)
	}
}

// TestRunHonoursCancellationAndRefusesOverlap covers the two halves of "one
// at a time, stoppable": a phrase spoken while a run is in flight is refused
// with ErrAlreadyRunning, and cancelling the context (the engine's "stop")
// ends the run with ctx.Err() and no script.finished — the cancel path owns
// that ending.
func TestRunHonoursCancellationAndRefusesOverlap(t *testing.T) {
	d := def(t, t.TempDir(), "exec sleep 120", ReportSummary)
	r, rec := newRunner(t, d)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := r.Run(ctx, "backup notes")
		done <- err
	}()
	<-rec.started // the runner has claimed the run and started the child

	if _, err := r.Run(context.Background(), "backup notes"); !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("overlapping run: err = %v", err)
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled run: err = %v", err)
	}
	if _, ok := rec.find("script.finished"); ok {
		t.Error("a cancelled run still published script.finished; session.cancelled owns that ending")
	}

	// And the claim was released: a cancelled run must not leave the runner
	// reporting already-running forever.
	if !r.begin("probe") {
		t.Error("the cancelled run never released its claim")
	}
	r.end()
}

func TestPathResolvesCaseInsensitively(t *testing.T) {
	d := def(t, t.TempDir(), "exit 0", ReportSummary)
	r, _ := newRunner(t, d)
	if p, ok := r.Path("Backup Notes"); !ok || p != d.Path {
		t.Errorf("Path = %q, %v", p, ok)
	}
	if _, ok := r.Path("unknown"); ok {
		t.Error("an unknown name resolved a path")
	}
}

func TestRunStartedEventNamesTheExactPath(t *testing.T) {
	d := def(t, t.TempDir(), "exit 0", ReportSummary)
	r, rec := newRunner(t, d)
	if _, err := r.Run(context.Background(), "backup notes"); err != nil {
		t.Fatal(err)
	}
	data, ok := rec.find("script.started")
	if !ok || data["path"] != d.Path || data["script"] != "backup notes" {
		t.Errorf("script.started = %v, %v", data, ok)
	}
	if !filepath.IsAbs(data["path"].(string)) {
		t.Errorf("audited path %v is not absolute", data["path"])
	}
}

// TestRunEventsCarryNoOutput: stdout and stderr can hold anything the script
// read, so no event field may carry a byte of either — the caps bound cost,
// this bounds exposure.
func TestRunEventsCarryNoOutput(t *testing.T) {
	r, rec := newRunner(t, def(t, t.TempDir(),
		"echo 'SECRET-STDOUT'; echo 'SECRET-STDERR' >&2; exit 3", ReportStdout))
	_, _ = r.Run(context.Background(), "backup notes")
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for i, data := range rec.data {
		for k, v := range data {
			if s, ok := v.(string); ok && strings.Contains(s, "SECRET") {
				t.Errorf("event %s field %s leaked output: %q", rec.events[i], k, s)
			}
		}
	}
}
