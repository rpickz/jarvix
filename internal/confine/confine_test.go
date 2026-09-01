package confine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain makes this test binary its own confinement helper.
//
// Run applies the boundary by re-executing the running program, so under `go
// test` the program is the test binary — and the first thing it must do, before
// the flag parsing and the test registry and anything that could print, is
// notice that it was started to be a command rather than to run tests. Any
// package whose tests run a confined command needs this same three lines.
func TestMain(m *testing.M) {
	Reexec()
	// The other role this binary plays: the probe that socket_test.go runs
	// INSIDE a confinement, so the thing attempting a forbidden connection is a
	// real process under a real filter rather than a test pretending to be one.
	serveTestDial()
	os.Exit(m.Run())
}

// makeFifo is the rendezvous the stop test needs: a pipe with a name, so the
// test can wait for the command to reach a known point without waiting on a
// clock.
func makeFifo(path string) error { return syscall.Mkfifo(path, 0o600) }

// The boundary's own tests (#222, ADR 0068).
//
// Two rules shape every one of them, and both come from what this package is
// for rather than from style.
//
// **An escape test asserts on the FILE, never on the error.** A command that
// tried to write outside the boundary and failed for some unrelated reason —
// a typo in the path, a shell that never started, a permission that was
// already denied — produces exactly the error a refusal produces. So every
// case below reads the outside file back afterwards and asserts its contents
// are the ones the test wrote. That is the only observation that distinguishes
// "the kernel refused it" from "it did not happen to work today", and it is
// the same discipline internal/testdiscipline's third rule states in general:
// do not assert on derived state after observing only its cause.
//
// **A skipped test says so loudly.** On a kernel that cannot hold the boundary
// these tests cannot prove anything, and the worst possible outcome for a
// change like this one is a green suite that never exercised the wall. So the
// skip names the kernel, the ABI it reported, the one required, and says in
// plain words that the test proved nothing — rather than passing quietly.

// confinedOrSkip refuses to pretend. See the note above.
func confinedOrSkip(t *testing.T) Support {
	t.Helper()
	s := Available()
	if !s.OK {
		t.Skipf("THE BOUNDARY WAS NOT EXERCISED ON THIS MACHINE and this test proved "+
			"nothing: %s (kernel reported Landlock ABI %d, this package needs %d)",
			s.Because, s.ABI, MinABI)
	}
	return s
}

// original is what an out-of-bounds file holds before a command tries to reach
// it, and what it must still hold afterwards.
const original = "ORIGINAL"

// tree is one job's boundary plus a file outside it.
type tree struct {
	root    string // in bounds, read and write
	outside string // a directory outside every root
	victim  string // a file in outside, holding original
}

// newTree builds the two directories as siblings, so neither contains the
// other. The paths are symlink-resolved because the roots a real Scope carries
// are resolved too (internal/jobs.Scope.Validate), and a boundary built from an
// unresolved path would be testing something the daemon never does.
func newTree(t *testing.T) tree {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tr := tree{
		root:    filepath.Join(base, "in"),
		outside: filepath.Join(base, "out"),
	}
	tr.victim = filepath.Join(tr.outside, "victim.txt")
	for _, dir := range []string{tr.root, tr.outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(tr.victim, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	return tr
}

// spec is the boundary for this tree.
func (tr tree) spec() Spec { return Spec{Roots: []string{tr.root}} }

// run executes one command inside the tree's boundary.
func (tr tree) run(t *testing.T, command string) (Outcome, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return Runner{}.Run(ctx, Request{Command: command, Spec: tr.spec()})
}

// stillOriginal is the assertion every escape test ends with: the file outside
// the boundary was not touched. Read off the disk, not inferred from an error.
func (tr tree) stillOriginal(t *testing.T) {
	t.Helper()
	got, err := os.ReadFile(tr.victim)
	if err != nil {
		t.Fatalf("the file outside the boundary is gone: %v", err)
	}
	if string(got) != original {
		t.Errorf("the file outside the boundary now reads %q, want %q — a command "+
			"reached through the wall", got, original)
	}
}

// ---------------------------------------------------------------------------
// What the kernel can actually do, measured
// ---------------------------------------------------------------------------

// TestTheKernelIsMeasuredRatherThanAssumed. Available asks the kernel; it does
// not read a release number, and it does not cache an answer from start-up.
func TestTheKernelIsMeasuredRatherThanAssumed(t *testing.T) {
	got := Available()
	switch {
	case got.ABI < 0:
		t.Fatalf("support = %+v, an ABI cannot be negative", got)
	case got.OK && got.ABI < MinABI:
		t.Errorf("support = %+v, says yes on an ABI below the floor", got)
	case got.OK && strings.TrimSpace(got.Because) != "":
		t.Errorf("support = %+v, a kernel that can hold the boundary needs no excuse", got)
	case !got.OK && strings.TrimSpace(got.Because) == "":
		t.Errorf("support = %+v, a refusal with nothing to tell the user", got)
	}
	t.Logf("this machine reports Landlock ABI %d (floor %d, usable %v)", got.ABI, MinABI, got.OK)
}

// TestAKernelThatCannotHoldTheBoundaryRefusesToRun is the ticket's
// non-negotiable rule, and the one this whole package would be worthless
// without: there is no branch in which a command runs unconfined. The kernel is
// injected here rather than found, because the interesting kernel is one this
// machine does not have.
func TestAKernelThatCannotHoldTheBoundaryRefusesToRun(t *testing.T) {
	tr := newTree(t)
	landed := filepath.Join(tr.root, "it-ran.txt")
	for _, kernel := range []Support{
		{ABI: 0, Because: "this kernel has no Landlock"},
		{ABI: MinABI - 1, Because: "this kernel's Landlock is too old"},
	} {
		r := Runner{Kernel: func() Support { return kernel }}
		_, err := r.Run(context.Background(), Request{
			Command: "echo ran > " + landed, Spec: tr.spec()})
		var unconfinable *ErrUnconfinable
		if !asUnconfinable(err, &unconfinable) {
			t.Fatalf("ABI %d: error = %v, want a refusal that says the boundary "+
				"cannot be held", kernel.ABI, err)
		}
		if !strings.Contains(unconfinable.Because, kernel.Because) {
			t.Errorf("ABI %d: refusal = %q, want it to carry what was measured",
				kernel.ABI, unconfinable.Because)
		}
		if _, err := os.Stat(landed); err == nil {
			t.Fatalf("ABI %d: the command ran anyway — it wrote %s", kernel.ABI, landed)
		}
	}
}

// asUnconfinable names the one question every refusal test asks, so they all
// read the same way: is this the kind of error a caller can show the user and
// know that nothing was run?
func asUnconfinable(err error, target **ErrUnconfinable) bool {
	return err != nil && errors.As(err, target)
}

// ---------------------------------------------------------------------------
// Inside the boundary
// ---------------------------------------------------------------------------

// TestAWriteInsideARootLands. The boundary has to permit the work, or it is
// not a boundary, it is an off switch.
func TestAWriteInsideARootLands(t *testing.T) {
	confinedOrSkip(t)
	tr := newTree(t)
	made := filepath.Join(tr.root, "made.txt")
	got, err := tr.run(t, "echo hello > "+made)
	if err != nil {
		t.Fatalf("a write inside the boundary was refused: %v (%s)", err, got.Output)
	}
	if !got.Confined {
		t.Fatal("the command ran without the kernel confirming the boundary")
	}
	if got.Exit != 0 {
		t.Fatalf("exit = %d, output = %q", got.Exit, got.Output)
	}
	content, err := os.ReadFile(made)
	if err != nil || strings.TrimSpace(string(content)) != "hello" {
		t.Errorf("file inside the root = %q, %v; want the command's own write", content, err)
	}
}

// TestAFailingCommandsExitStatusIsReportedAsItWas. The ledger says what the
// command exited with, so a report can claim the run and nothing more.
func TestAFailingCommandsExitStatusIsReportedAsItWas(t *testing.T) {
	confinedOrSkip(t)
	tr := newTree(t)
	got, err := tr.run(t, "echo trouble >&2; exit 7")
	if err != nil {
		t.Fatalf("a command that ran and failed was reported as unrunnable: %v", err)
	}
	if got.Exit != 7 {
		t.Errorf("exit = %d, want 7", got.Exit)
	}
	if !strings.Contains(got.Output, "trouble") {
		t.Errorf("output = %q, want the command's own stderr", got.Output)
	}
}

// ---------------------------------------------------------------------------
// The escapes. One test each, and each one reads the file back.
// ---------------------------------------------------------------------------

// TestASymlinkOutOfTheTreeIsRefused. A link planted INSIDE the scope pointing
// out of it is the escape a path check has to resolve its way out of; the
// kernel resolves the path itself and never sees the link at all.
func TestASymlinkOutOfTheTreeIsRefused(t *testing.T) {
	confinedOrSkip(t)
	tr := newTree(t)
	if err := os.Symlink(tr.outside, filepath.Join(tr.root, "escape")); err != nil {
		t.Fatal(err)
	}
	got, _ := tr.run(t, "echo PWNED > "+filepath.Join(tr.root, "escape", "victim.txt"))
	tr.stillOriginal(t)
	if got.Exit == 0 {
		t.Errorf("the command reported success writing through a symlink out of the tree")
	}
}

// TestReadingThroughASymlinkOutOfTheTreeIsRefused. The write case above is not
// the whole of it: a scope over a directory is also a promise that what is
// outside stays unread.
func TestReadingThroughASymlinkOutOfTheTreeIsRefused(t *testing.T) {
	confinedOrSkip(t)
	tr := newTree(t)
	if err := os.Symlink(tr.outside, filepath.Join(tr.root, "escape")); err != nil {
		t.Fatal(err)
	}
	got, _ := tr.run(t, "cat "+filepath.Join(tr.root, "escape", "victim.txt"))
	if strings.Contains(got.Output, original) {
		t.Errorf("output = %q, the command read a file outside its boundary", got.Output)
	}
	if got.Exit == 0 {
		t.Errorf("the command reported success reading through a symlink out of the tree")
	}
}

// TestAnAbsolutePathOutsideIsRefused — the plainest case, and the one a subject
// parser would actually have caught. It is here so that the plain case is
// pinned by the same mechanism as the ones a parser could not catch.
func TestAnAbsolutePathOutsideIsRefused(t *testing.T) {
	confinedOrSkip(t)
	tr := newTree(t)
	got, _ := tr.run(t, "echo PWNED > "+tr.victim)
	tr.stillOriginal(t)
	if got.Exit == 0 {
		t.Errorf("the command reported success writing to an absolute path outside the boundary")
	}
}

// TestARelativeTraversalIsRefused. `../..` from inside the scope is a path that
// reads as inside it right up until the kernel resolves it.
func TestARelativeTraversalIsRefused(t *testing.T) {
	confinedOrSkip(t)
	tr := newTree(t)
	got, _ := tr.run(t, "cd "+tr.root+" && echo PWNED > ../out/victim.txt")
	tr.stillOriginal(t)
	if got.Exit == 0 {
		t.Errorf("the command reported success writing through a relative traversal")
	}
}

// TestAPathArrivingInAVariableIsRefused. This is the case that makes the whole
// design necessary: the path never appears in the command's text as a path, so
// there is nothing for a reader of that text to find. The kernel is not reading
// the text.
func TestAPathArrivingInAVariableIsRefused(t *testing.T) {
	confinedOrSkip(t)
	tr := newTree(t)
	got, _ := tr.run(t, `d=$(dirname `+tr.victim+`); f=$(basename `+tr.victim+
		`); echo PWNED > "$d/$f"`)
	tr.stillOriginal(t)
	if got.Exit == 0 {
		t.Errorf("the command reported success writing to a path it assembled at run time")
	}
}

// TestACommandThatChangesDirectoryFirstIsRefused. Landlock does not restrict
// where a process may STAND — chdir out of the scope succeeds — it restricts
// what it may touch from there. The distinction is worth a test of its own,
// because it is the one that surprises people reading the design.
func TestACommandThatChangesDirectoryFirstIsRefused(t *testing.T) {
	confinedOrSkip(t)
	tr := newTree(t)
	got, _ := tr.run(t, "cd "+tr.outside+" && echo PWNED > victim.txt")
	tr.stillOriginal(t)
	if got.Exit == 0 {
		t.Errorf("the command reported success writing after changing directory out of the scope")
	}
}

// TestAFileOutsideCannotBeEmptied is why MinABI is 3 rather than 1. truncate(2)
// takes a path and not a descriptor, so a kernel that does not handle
// LANDLOCK_ACCESS_FS_TRUNCATE would let a command empty a file it could neither
// open nor write.
func TestAFileOutsideCannotBeEmptied(t *testing.T) {
	confinedOrSkip(t)
	tr := newTree(t)
	got, _ := tr.run(t, "truncate -s 0 "+tr.victim)
	tr.stillOriginal(t)
	if got.Exit == 0 {
		t.Errorf("the command reported success emptying a file outside the boundary")
	}
}

// TestAFileOutsideCannotBeDeleted. Removing is a change to the directory the
// file is in, and that directory is outside every root.
func TestAFileOutsideCannotBeDeleted(t *testing.T) {
	confinedOrSkip(t)
	tr := newTree(t)
	got, _ := tr.run(t, "rm -f "+tr.victim)
	tr.stillOriginal(t)
	if got.Exit == 0 {
		t.Errorf("the command reported success deleting a file outside the boundary")
	}
}

// TestADirectoryOutsideCannotBeListed. A scope is also about what a job gets to
// SEE: a command that could read the names in ~/Documents has learned something
// about the user it was not given.
func TestADirectoryOutsideCannotBeListed(t *testing.T) {
	confinedOrSkip(t)
	tr := newTree(t)
	got, _ := tr.run(t, "ls -a "+tr.outside)
	if strings.Contains(got.Output, "victim.txt") {
		t.Errorf("output = %q, the command listed a directory outside its boundary", got.Output)
	}
}

// TestTheUsersHomeIsOutOfReachUnlessTheScopeNamesIt is the case the feature is
// actually for. Nothing about the home directory is special to this package —
// it is out of bounds for the same reason everything else is — and a test that
// says so out loud is the one a reader looks for.
func TestTheUsersHomeIsOutOfReachUnlessTheScopeNamesIt(t *testing.T) {
	confinedOrSkip(t)
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory on this machine to be kept out of")
	}
	tr := newTree(t)
	got, _ := tr.run(t, "ls -a "+home+" && echo LISTED")
	if strings.Contains(got.Output, "LISTED") {
		t.Errorf("output = %q, a job's command listed the user's home directory", got.Output)
	}
}

// ---------------------------------------------------------------------------
// The environment
// ---------------------------------------------------------------------------

// TestTheDaemonsCredentialsDoNotReachTheCommand.
//
// jarvixd reads the user's model API keys out of its OWN environment
// (config.Endpoint.Key calls os.Getenv), so a child that inherited that
// environment would be handed them in a variable any command can print. The
// child's environment is therefore built from nothing rather than filtered, and
// this test sets a plausible key on the parent and asserts the command cannot
// see it — by dumping the whole environment, so the assertion does not depend
// on guessing the variable's name.
func TestTheDaemonsCredentialsDoNotReachTheCommand(t *testing.T) {
	confinedOrSkip(t)
	const secret = "sk-fireworks-do-not-leak-this"
	t.Setenv("FIREWORKS_API_KEY", secret)
	t.Setenv("LM_STUDIO_API_KEY", secret)
	tr := newTree(t)
	got, err := tr.run(t, "env")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.Output, secret) {
		t.Fatalf("the command could read a credential from its environment; output was %q",
			got.Output)
	}
	if strings.Contains(got.Output, "FIREWORKS_API_KEY") {
		t.Errorf("output = %q, the variable's NAME reached the command as well", got.Output)
	}
	for _, want := range []string{"HOME=" + tr.root, "PATH=" + safePath} {
		if !strings.Contains(got.Output, want) {
			t.Errorf("output = %q, want it to carry %q", got.Output, want)
		}
	}
}

// TestAPathArrivingFromTheParentsEnvironmentIsRefusedTwice. The escape case the
// ticket names, in its other flavour: the path is not in the command at all,
// it is in a variable the daemon holds. It is stopped twice over — the variable
// never reaches the child, and the kernel would refuse the path if it had.
func TestAPathArrivingFromTheParentsEnvironmentIsRefusedTwice(t *testing.T) {
	confinedOrSkip(t)
	tr := newTree(t)
	t.Setenv("VICTIM_PATH", tr.victim)
	got, _ := tr.run(t, `echo PWNED > "${VICTIM_PATH:-/dev/null}"; echo "saw:[${VICTIM_PATH:-}]"`)
	tr.stillOriginal(t)
	if !strings.Contains(got.Output, "saw:[]") {
		t.Errorf("output = %q, the daemon's variable reached the command", got.Output)
	}
}

// ---------------------------------------------------------------------------
// The confirmation, and what a caller is entitled to claim
// ---------------------------------------------------------------------------

// TestACommandThatWasNeverConfinedIsReportedAsNeverRun.
//
// The helper writes its confirmation byte after the kernel accepts the ruleset
// and before it execs, so the byte's absence means the command was not started.
// Here the helper is replaced by a program that does neither, which is the only
// way to reach that branch on a working kernel — and the point is that the
// caller is told "I did not run it" rather than being handed an empty output it
// might read as "it ran and said nothing". #71's rule, applied to this
// package's own bookkeeping.
func TestACommandThatWasNeverConfinedIsReportedAsNeverRun(t *testing.T) {
	confinedOrSkip(t)
	tr := newTree(t)
	stub := filepath.Join(t.TempDir(), "stub")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := Runner{Helper: stub}.Run(context.Background(), Request{
		Command: "echo ran > " + filepath.Join(tr.root, "ran.txt"), Spec: tr.spec()})
	if got.Confined {
		t.Fatal("a boundary nobody confirmed was reported as confirmed")
	}
	var unconfinable *ErrUnconfinable
	if !asUnconfinable(err, &unconfinable) {
		t.Fatalf("error = %v, want a refusal saying the command was not run", err)
	}
	if !strings.Contains(unconfinable.Because, "did not run it") {
		t.Errorf("refusal = %q, want it to say the command was not run", unconfinable.Because)
	}
}

// TestABoundaryThatHeldAndACommandThatNeverStartedIsNotReportedAsRun.
//
// The confirmation is two facts on one pipe, not one. A helper that applied the
// boundary and then could not exec has written "k" — so a caller reading only
// "did a byte arrive?" would report a command that never started as one that ran
// and produced nothing, which is the #71 sentence with a shell behind it. The
// close-on-exec descriptor is what separates them: the "x" only exists because
// the helper was still alive to write it.
//
// The system base is emptied to reach this branch, which is also the test that
// the base is load-bearing rather than decorative: with nothing readable outside
// the roots, there is no `bash` to become.
func TestABoundaryThatHeldAndACommandThatNeverStartedIsNotReportedAsRun(t *testing.T) {
	confinedOrSkip(t)
	tr := newTree(t)
	landed := filepath.Join(tr.root, "it-ran.txt")
	got, err := Runner{Base: []string{}}.Run(context.Background(), Request{
		Command: "echo ran > " + landed, Spec: tr.spec()})
	if got.Confined {
		t.Fatal("a command that never started was reported as having run inside the boundary")
	}
	var unconfinable *ErrUnconfinable
	if !asUnconfinable(err, &unconfinable) {
		t.Fatalf("error = %v, want a refusal saying the command was not run", err)
	}
	if !strings.Contains(unconfinable.Because, "the boundary held and then I could not start") {
		// Not decoration: this string is the proof the test reached the
		// "confined, never started" branch rather than failing earlier, which
		// is the only branch it is about.
		t.Errorf("refusal = %q, want the helper's own account of where it stopped",
			unconfinable.Because)
	}
	if _, statErr := os.Stat(landed); statErr == nil {
		t.Fatalf("the command ran anyway — it wrote %s", landed)
	}
}

// ---------------------------------------------------------------------------
// Stopping
// ---------------------------------------------------------------------------

// TestAHangingCommandIsKilledWithItsWholeProcessGroup.
//
// A job that is stopped must terminate the command promptly, and "promptly"
// has to include the things the command started. The rendezvous is a fifo
// rather than a sleep: opening it for reading blocks until the command opens it
// for writing, so the test knows the command is genuinely running before it
// cancels, without guessing how long that takes. Nothing here waits on a clock.
//
// The assertion is a file, not a duration. The command would write `finished`
// after its long sleep, so the absence of that file after Run returns is the
// direct observation that the sleep never completed — rather than an inference
// from the fact that Run came back quickly.
func TestAHangingCommandIsKilledWithItsWholeProcessGroup(t *testing.T) {
	confinedOrSkip(t)
	tr := newTree(t)
	ready := filepath.Join(tr.root, "ready.fifo")
	finished := filepath.Join(tr.root, "finished.txt")
	mustFifo(t, ready)

	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		out Outcome
		err error
	}
	done := make(chan result, 1)
	go func() {
		// The trailing `&` and `wait` put the sleep in a child of the shell, so
		// killing only the shell would leave it behind. Killing the group is
		// what this test is really about.
		out, err := Runner{}.Run(ctx, Request{
			Spec: tr.spec(),
			Command: "(sleep 600; echo done > " + finished + ") & echo up > " + ready +
				"; wait",
		})
		done <- result{out, err}
	}()

	// Blocks until the command opens the fifo for writing. This is the
	// synchronisation point: past it, the command is running.
	f, err := os.OpenFile(ready, os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	cancel()
	got := <-done
	if !got.out.Killed {
		t.Errorf("outcome = %+v, want it to report the command was stopped", got.out)
	}
	if got.out.TimedOut {
		// The distinction the ledger's sentence rests on: this command was
		// stopped by the user, not by running out of time, and "stopped after
		// 30 seconds" would be a small false claim about why it ended.
		t.Errorf("outcome = %+v, want a stop rather than a timeout", got.out)
	}
	if _, err := os.Stat(finished); err == nil {
		t.Error("the backgrounded child outlived the command and finished its work")
	}
	if got.err != nil && got.out.Confined {
		// A stopped command is not an unconfined one: the boundary held, the
		// work simply did not finish. Reporting it as a confinement failure
		// would put the wrong sentence in the ledger.
		t.Errorf("error = %v on a command that WAS confined and then stopped", got.err)
	}
}

// mustFifo makes a named pipe, or says why it could not.
func mustFifo(t *testing.T, path string) {
	t.Helper()
	if err := makeFifo(path); err != nil {
		t.Skipf("THE STOP PATH WAS NOT EXERCISED: this machine would not make a fifo "+
			"at %s (%v), so there is no way to know the command had started before "+
			"stopping it", path, err)
	}
}

// TestATimeoutStopsTheCommandAndSaysSo.
func TestATimeoutStopsTheCommandAndSaysSo(t *testing.T) {
	confinedOrSkip(t)
	tr := newTree(t)
	finished := filepath.Join(tr.root, "finished.txt")
	got, _ := Runner{}.Run(context.Background(), Request{
		Command: "sleep 600; echo done > " + finished,
		Spec:    tr.spec(),
		Timeout: 50 * time.Millisecond,
	})
	if !got.Killed || !got.TimedOut {
		t.Errorf("outcome = %+v, want it to report the command ran out of time", got)
	}
	if _, err := os.Stat(finished); err == nil {
		t.Error("the command finished its work after its timeout had passed")
	}
}

// ---------------------------------------------------------------------------
// The spec's own refusals, which need no kernel
// ---------------------------------------------------------------------------

// TestARootThatContainsJarvixsOwnConfigurationIsRefused.
//
// This is the answer to "does the state directory need excluding from a job's
// roots", and it is a refusal rather than an exclusion because Landlock cannot
// exclude. Rights are a union up the tree: a rule granting read/write on the
// home directory is not narrowed by a second rule granting less on a directory
// beneath it, and a rule with no rights at all is rejected outright. So there
// is no ruleset that says "all of ~ except ~/.config/jarvix", and a scope that
// asked for one would be a scope in which a command could rewrite `[tools]` —
// #109's wall, reached by a different door.
func TestARootThatContainsJarvixsOwnConfigurationIsRefused(t *testing.T) {
	base := t.TempDir()
	spec := Spec{
		Roots:    []string{base},
		Reserved: []string{filepath.Join(base, ".config", "jarvix")},
	}
	err := spec.Check(Support{ABI: MinABI, OK: true})
	var unconfinable *ErrUnconfinable
	if !asUnconfinable(err, &unconfinable) {
		t.Fatalf("error = %v, want a refusal", err)
	}
	for _, want := range []string{base, "my own configuration", "narrower boundary"} {
		if !strings.Contains(unconfinable.Because, want) {
			t.Errorf("refusal = %q, want it to mention %q", unconfinable.Because, want)
		}
	}
}

// TestARootInsideJarvixsOwnStateIsAllowed. The test above is one-directional on
// purpose: a job scoped to the artifacts folder is a perfectly ordinary job, and
// from inside it config.toml is outside the root like anything else.
func TestARootInsideJarvixsOwnStateIsAllowed(t *testing.T) {
	state := t.TempDir()
	root := filepath.Join(state, "artifacts")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	spec := Spec{Roots: []string{root}, Reserved: []string{state}}
	if err := spec.Check(Support{ABI: MinABI, OK: true}); err != nil {
		t.Errorf("a job scoped inside my own state directory was refused: %v", err)
	}
}

// TestASpecWithNothingInItIsRefused: a command confined to nowhere is not a
// safe command, it is a confusing one.
func TestASpecWithNothingInItIsRefused(t *testing.T) {
	err := Spec{}.Check(Support{ABI: MinABI, OK: true})
	var unconfinable *ErrUnconfinable
	if !asUnconfinable(err, &unconfinable) {
		t.Fatalf("error = %v, want a refusal", err)
	}
	if !strings.Contains(unconfinable.Because, "names no directory") {
		t.Errorf("refusal = %q, want it to say the boundary is empty", unconfinable.Because)
	}
}

// TestARootThatIsNotThereIsRefused. The kernel needs a descriptor for every
// root, so a boundary naming a directory that has since been deleted cannot be
// built — and saying so beats a command failing later for a reason nobody can
// connect to the scope.
func TestARootThatIsNotThereIsRefused(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "never-existed")
	err := Spec{Roots: []string{gone}}.Check(Support{ABI: MinABI, OK: true})
	var unconfinable *ErrUnconfinable
	if !asUnconfinable(err, &unconfinable) {
		t.Fatalf("error = %v, want a refusal", err)
	}
	if !strings.Contains(unconfinable.Because, gone) {
		t.Errorf("refusal = %q, want it to name the directory", unconfinable.Because)
	}
}

// TestARelativeRootIsNotAdmitted. A relative root is a root nobody can state —
// it means something different depending on where the daemon happens to be —
// so it is dropped rather than resolved, and dropping the only root leaves a
// spec that is refused.
func TestARelativeRootIsNotAdmitted(t *testing.T) {
	if err := (Spec{Roots: []string{"downloads"}}).Check(Support{ABI: MinABI, OK: true}); err == nil {
		t.Error("a relative root was accepted as a boundary")
	}
}

// ---------------------------------------------------------------------------
// The base
// ---------------------------------------------------------------------------

// TestTheSystemBaseHoldsNothingOfTheUsers.
//
// The base is what a confined command may read outside its roots, and its whole
// justification is that it holds the MACHINE's files rather than the user's. A
// test rather than a comment, because the list is the sort of thing that grows
// by one entry at a time until it is /.
func TestTheSystemBaseHoldsNothingOfTheUsers(t *testing.T) {
	home, _ := os.UserHomeDir()
	for _, dir := range SystemBase {
		if home != "" && within(dir, home) {
			t.Errorf("the system base admits %s, which is inside the user's home", dir)
		}
		for _, forbidden := range []string{"/proc", "/run", "/var", "/tmp", "/sys", "/home", "/root"} {
			if dir == forbidden || within(dir, forbidden) {
				t.Errorf("the system base admits %s: %s must stay out of reach", dir, forbidden)
			}
		}
	}
}

// TestTheCommandCannotReadAnotherProcessesEnvironment. /proc is not in the
// base, and this is the reason: jarvixd's own credentials are in its
// environment, and /proc/<pid>/environ is readable by the same user.
func TestTheCommandCannotReadAnotherProcessesEnvironment(t *testing.T) {
	confinedOrSkip(t)
	const secret = "sk-parent-process-secret"
	t.Setenv("JARVIX_TEST_PARENT_SECRET", secret)
	tr := newTree(t)
	got, _ := tr.run(t, "cat /proc/1/environ /proc/*/environ 2>/dev/null; echo END")
	if strings.Contains(got.Output, secret) {
		t.Fatalf("the command read another process's environment: %q", got.Output)
	}
}
