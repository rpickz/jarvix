package daemon

// A job running a command, end to end (#222, ADR 0068).
//
// internal/confine proves the kernel refuses an escape; internal/tools proves
// the shell tool reaches that boundary and refuses a job's command without one.
// What is left, and what these tests are for, is the whole chain in one piece:
// a job's scope becomes a boundary, the daemon's own registry runs the command
// inside it, the write lands where the scope allowed it and nowhere else, and
// the account gets a row saying what ran.
//
// Every escape assertion here reads the file back. An error is not evidence of
// a refusal — a command can fail for a dozen reasons that have nothing to do
// with the wall — and the whole value of this change is the difference between
// those two things.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/confine"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/jobs"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/tools"
	"github.com/rpickz/jarvix/internal/tts"
)

// TestMain makes this test binary its own confinement helper — jarvixd confines
// a command by re-executing itself, and under `go test` "itself" is this
// binary. See confine.Reexec.
func TestMain(m *testing.M) {
	confine.Reexec()
	// The other role this binary plays: the attacker jobssocket_test.go runs
	// INSIDE a job's confinement, so the thing trying to drive the daemon is a
	// real process under the real walls rather than a test pretending to be one.
	serveTestDrive()
	os.Exit(m.Run())
}

// runPlainCommand runs a program with no confinement at all, for the control
// cases that prove a wall is being tested rather than a broken probe.
func runPlainCommand(t *testing.T, name string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// commandDaemon builds a daemon with shell.run registered, its own state
// directory, and an account to record into. The state directory is a SIBLING of
// the job's root rather than an ancestor, which is the shape a confinable scope
// has to have.
func commandDaemon(t *testing.T) (*Daemon, string, string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(base, "state")
	root := filepath.Join(base, "work")
	outside := filepath.Join(base, "private")
	for _, dir := range []string{state, root, outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := testConfig()
	cfg.Tools.Shell = true
	cfg.Tools.ShellTimeoutSec = 20
	cfg.Tools.ShellMaxOutputKB = 16
	d, err := New(cfg, config.Paths{Config: state, Data: state, State: state, Runtime: state,
		Socket: filepath.Join(state, "j.sock")}, nil, Deps{
		Provider:    &ai.Fake{},
		Transcriber: &stt.Fake{},
		Synthesizer: &tts.Fake{},
		Recorder:    &audio.FakeRecorder{Clip: audio.Clip{WAVPath: state + "/r.wav"}},
		Player:      &audio.FakePlayer{},
		Notifier:    &desktop.FakeNotifier{},
		OpenWindow:  func(context.Context) error { return nil },
		Compositor:  desktop.NewFakeCompositor(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return d, root, outside
}

// commandScope is a job allowed one directory and the shell.
func commandScope(t *testing.T, root string) jobs.Scope {
	t.Helper()
	scope, err := jobs.Scope{Tools: []string{tools.ShellToolName}, Roots: []string{root}}.Validate()
	if err != nil {
		t.Fatal(err)
	}
	return scope
}

// boundaryOrSkip refuses to let a green run mean nothing on a kernel that never
// held the wall.
func boundaryOrSkip(t *testing.T) {
	t.Helper()
	if s := confine.Available(); !s.OK {
		t.Skipf("THE BOUNDARY WAS NOT EXERCISED and this test proved nothing: %s "+
			"(kernel reported Landlock ABI %d, %d required)", s.Because, s.ABI, confine.MinABI)
	}
}

// runStep drives one step through the daemon's actor exactly as the runner
// would, and returns what the actor observed.
func runStep(t *testing.T, d *Daemon, scope jobs.Scope, command string) (jobs.Result, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	step := jobs.Step{Tool: tools.ShellToolName, Intent: "ran a command",
		Args: `{"command":` + quoted(command) + `}`}
	return (&jobActor{d: d}).Do(ctx, "j1", scope, step)
}

// quoted renders a command as a JSON string. The commands below are written by
// hand and contain quotes and backslashes, so this is not optional.
func quoted(s string) string {
	out := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
	return `"` + out + `"`
}

// TestAJobsCommandWritesInsideItsRootAndTheAccountSaysSo is the ticket's first
// acceptance criterion: a job whose scope names a root runs a command that
// writes inside it, and the change is recorded.
func TestAJobsCommandWritesInsideItsRootAndTheAccountSaysSo(t *testing.T) {
	boundaryOrSkip(t)
	d, root, _ := commandDaemon(t)
	scope := commandScope(t, root)
	made := filepath.Join(root, "notes.txt")

	result, err := runStep(t, d, scope, "echo tidy > "+made)
	if err != nil {
		t.Fatalf("a command inside the job's own root was refused: %v (%s)", err, result.Said)
	}
	if result.Failed {
		t.Fatalf("the command reported failure: %q", result.Said)
	}
	got, err := os.ReadFile(made)
	if err != nil || strings.TrimSpace(string(got)) != "tidy" {
		t.Fatalf("file inside the root = %q, %v; want the command's own write", got, err)
	}
	if result.Undo == "" {
		t.Fatal("nothing was recorded in the account, so the report would have no fact to " +
			"stand on and the change could not be reviewed afterwards")
	}
	records := d.account.Job("j1")
	if len(records) != 1 {
		t.Fatalf("the account holds %d rows for this job, want 1", len(records))
	}
	if !strings.Contains(records[0].Summary, "echo tidy") {
		t.Errorf("summary = %q, want the command verbatim: a summary of a command is a "+
			"paraphrase, and this is the one row where a paraphrase would be a lie",
			records[0].Summary)
	}
}

// TestAJobsCommandCannotWriteOutsideEveryRoot — the second criterion, asserted
// on the file rather than on the error.
func TestAJobsCommandCannotWriteOutsideEveryRoot(t *testing.T) {
	boundaryOrSkip(t)
	d, root, outside := commandDaemon(t)
	scope := commandScope(t, root)
	victim := filepath.Join(outside, "diary.txt")
	if err := os.WriteFile(victim, []byte("ORIGINAL"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := runStep(t, d, scope, "echo PWNED > "+victim)
	if err != nil {
		t.Fatalf("the daemon reported the command unrunnable rather than letting the "+
			"kernel refuse it: %v", err)
	}
	if !result.Failed {
		t.Errorf("the step reported success writing outside every root: %q", result.Said)
	}
	after, readErr := os.ReadFile(victim)
	if readErr != nil {
		t.Fatalf("the file outside the boundary is gone: %v", readErr)
	}
	if string(after) != "ORIGINAL" {
		t.Fatalf("the file outside the boundary now reads %q — a job's command reached "+
			"through the wall", after)
	}
}

// TestAJobsCommandCannotReachJarvixsOwnConfiguration is #109's wall against the
// new door a command opens. A job is structurally unable to call
// config.write_entry; it must be equally unable to run `sed -i` over
// config.toml, or the wall has a hole in it shaped like a shell.
func TestAJobsCommandCannotReachJarvixsOwnConfiguration(t *testing.T) {
	boundaryOrSkip(t)
	d, root, _ := commandDaemon(t)
	scope := commandScope(t, root)
	settings := d.paths.ConfigFile()
	if err := os.WriteFile(settings, []byte("[tools]\nshell = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, command := range []string{
		"echo '[tools]' > " + settings,
		"cat " + settings,
		"rm -f " + settings,
	} {
		result, err := runStep(t, d, scope, command)
		if err != nil {
			t.Fatalf("%q: %v", command, err)
		}
		if strings.Contains(result.Said, "shell = true") {
			t.Errorf("%q read Jarvix's own configuration: %q", command, result.Said)
		}
	}
	after, err := os.ReadFile(settings)
	if err != nil {
		t.Fatalf("Jarvix's configuration was deleted by a job's command: %v", err)
	}
	if !strings.Contains(string(after), "shell = true") {
		t.Fatalf("Jarvix's configuration now reads %q — a job rewrote what it is allowed "+
			"to do", after)
	}
}

// TestAJobsCommandCannotReadTheUsersHome. The scope names one directory; the
// rest of the user's tree is not the job's to look at, and "look at" includes
// listing it.
func TestAJobsCommandCannotReadTheUsersHome(t *testing.T) {
	boundaryOrSkip(t)
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory on this machine to be kept out of")
	}
	d, root, _ := commandDaemon(t)
	result, runErr := runStep(t, d, commandScope(t, root), "ls -a "+home+" && echo LISTED")
	if runErr != nil {
		t.Fatal(runErr)
	}
	if strings.Contains(result.Said, "LISTED") {
		t.Errorf("a job's command listed the user's home directory: %q", result.Said)
	}
}

// TestAJobsCommandGetsNoneOfTheDaemonsCredentials. jarvixd reads the user's
// model API keys out of its own environment, so an inherited environment would
// hand them to any command a job runs.
func TestAJobsCommandGetsNoneOfTheDaemonsCredentials(t *testing.T) {
	boundaryOrSkip(t)
	const secret = "sk-fireworks-do-not-leak-this"
	t.Setenv("FIREWORKS_API_KEY", secret)
	d, root, _ := commandDaemon(t)
	result, err := runStep(t, d, commandScope(t, root), "env")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Said, secret) {
		t.Fatalf("a job's command could read the user's API key from its environment: %q",
			result.Said)
	}
}

// TestACommandIsNotRunAtAllWhenTheBoundaryCannotBeHeld, at the daemon boundary
// and asserted on the disk: the refusal happens before anything is dispatched,
// so the file the command would have written is not there.
//
// The scope here is the one shape that cannot be confined on any kernel — a
// root that swallows Jarvix's own state — which is why this test needs no
// kernel injection and runs everywhere.
func TestACommandIsNotRunAtAllWhenTheBoundaryCannotBeHeld(t *testing.T) {
	d, root, _ := commandDaemon(t)
	swallowing := filepath.Dir(d.paths.State)
	scope := jobs.Scope{Tools: []string{tools.ShellToolName}, Roots: []string{swallowing}}
	landed := filepath.Join(root, "it-ran.txt")

	actor := &jobActor{d: d}
	_, err := actor.Subject(context.Background(), scope,
		jobs.Step{Tool: tools.ShellToolName, Args: `{"command":"echo ran > ` + landed + `"}`})
	var unconfinable *jobs.ErrUnconfinable
	if !asJobsUnconfinable(err, &unconfinable) {
		t.Fatalf("error = %v, want the refusal a job parks on", err)
	}
	if _, statErr := os.Stat(landed); statErr == nil {
		t.Fatalf("the command ran anyway — it wrote %s", landed)
	}
}

// TestAJobsCommandThatIsStoppedLeavesTheStepUnverified.
//
// #71's scar, at the one place a command makes it sharpest. A command that was
// dispatched and then cut off did SOMETHING — possibly all of it, possibly none
// — and the honest report of that is "I started this and never saw how it
// ended". Do returns the context's error precisely so the runner writes the
// ledger entry as unverified rather than as failed, and "it did not happen" is
// a claim nobody here has grounds for.
func TestAJobsCommandThatIsStoppedLeavesTheStepUnverified(t *testing.T) {
	boundaryOrSkip(t)
	d, root, _ := commandDaemon(t)
	scope := commandScope(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // stopped before it could start, which is the same fact to the ledger

	step := jobs.Step{Tool: tools.ShellToolName, Args: `{"command":"echo hello"}`}
	_, err := (&jobActor{d: d}).Do(ctx, "j1", scope, step)
	if err == nil {
		t.Fatal("a stopped step came back with no error, so the ledger would have " +
			"recorded it as verified")
	}
	if records := d.account.Job("j1"); len(records) != 0 {
		t.Errorf("the account holds %d rows for a command that never ran, want 0",
			len(records))
	}
}

// TestTheBoundaryRidesEveryStepAndNotOnlyTheCommandShapedOnes. The confinement
// is installed on the context for every step a job takes, so a tool that grows
// a shell out of it later cannot inherit an unconfined path by omission.
func TestTheBoundaryRidesEveryStepAndNotOnlyTheCommandShapedOnes(t *testing.T) {
	d, root, _ := commandDaemon(t)
	scope := commandScope(t, root)
	var seen confine.Spec
	var carried bool
	d.registry.Register(&contextProbe{onCall: func(ctx context.Context) {
		seen, carried = confine.From(ctx)
	}})
	_, err := (&jobActor{d: d}).Do(context.Background(), "j1", scope,
		jobs.Step{Tool: probeToolName, Args: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	if !carried {
		t.Fatal("a step that was not a command carried no boundary, so a tool that " +
			"grew one later would run unheld")
	}
	if len(seen.Roots) != 1 || seen.Roots[0] != root {
		t.Errorf("boundary roots = %v, want the job's own %q", seen.Roots, root)
	}
	if len(seen.Reserved) == 0 {
		t.Error("the boundary named nothing as reserved, so a scope over the home " +
			"directory would have admitted Jarvix's own configuration")
	}
}

// asJobsUnconfinable names the one question the refusal tests ask.
func asJobsUnconfinable(err error, target **jobs.ErrUnconfinable) bool {
	if err == nil {
		return false
	}
	u, ok := err.(*jobs.ErrUnconfinable)
	if ok {
		*target = u
	}
	return ok
}

// probeToolName is the identity of the tool below. It is not a real capability
// and is registered only inside the one test that uses it.
const probeToolName = "test.context_probe"

// contextProbe reports the context its Execute was handed. It exists because
// the property under test is invisible from outside: the boundary is installed
// on a context, and the only way to see whether a tool was given one is to be
// that tool.
type contextProbe struct{ onCall func(context.Context) }

func (p *contextProbe) Name() string            { return probeToolName }
func (p *contextProbe) Description() string     { return "records the context it was called with" }
func (p *contextProbe) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (p *contextProbe) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	p.onCall(ctx)
	return "seen", nil
}

// TestACommandThatExitedNonZeroIsRecordedAsFailed is the ledger's half of
// #71's rule, and it is not about the boundary at all — the command below is
// entirely inside the scope and simply does not work.
//
// shell.run reports a command's failure in its RESULT rather than as an error,
// because a command that exited 3 is information for the model rather than a
// fault in the tool. A job's ledger reads that result to decide whether the
// step happened, and a step recorded as done is one the report will say was
// done under the model's own label for it. So a failing command recorded as
// done would produce "I did tidy the folder" for work that did not happen,
// assembled out of a ledger that was told the wrong fact.
func TestACommandThatExitedNonZeroIsRecordedAsFailed(t *testing.T) {
	boundaryOrSkip(t)
	d, root, _ := commandDaemon(t)
	result, err := runStep(t, d, commandScope(t, root), "echo nope >&2; exit 3")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Failed {
		t.Errorf("result = %+v, want the step marked failed so the report cannot claim it",
			result)
	}
	if !strings.Contains(result.Said, "exit status: 3") {
		t.Errorf("said = %q, want the ledger to record what it exited with", result.Said)
	}
}

// TestASuccessfulCommandIsNotRecordedAsFailed. The predicate above has to be
// able to tell the two apart, or every command a job runs would come back as
// something the report refuses to claim — which is dishonest in the other
// direction and just as useless.
func TestASuccessfulCommandIsNotRecordedAsFailed(t *testing.T) {
	boundaryOrSkip(t)
	d, root, _ := commandDaemon(t)
	result, err := runStep(t, d, commandScope(t, root), "echo all good")
	if err != nil {
		t.Fatal(err)
	}
	if result.Failed {
		t.Errorf("result = %+v, want a command that worked recorded as having worked", result)
	}
	if !strings.Contains(result.Said, "all good") {
		t.Errorf("said = %q, want the command's own output", result.Said)
	}
}
