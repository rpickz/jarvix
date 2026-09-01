// Package confine runs one command inside a boundary the KERNEL holds.
//
// It exists because of a refusal (#222, ADR 0068). A job carries a scope — the
// directories it may act within — and internal/jobs enforces that scope against
// a *subject* the daemon read out of each proposed tool call. For every tool
// whose effect is one named file that works. For a shell command it does not,
// and it cannot: a command's filesystem subject is not recoverable from its
// text. Quoting, variable expansion, `$(…)`, relative paths, `cd` and symlinks
// each defeat a reader on their own, and a check that is right most of the time
// is worse than no check at all, because it will be trusted. So the daemon
// refused to run commands inside a job, and that refusal was correct.
//
// The way out is not a better parser. It is to stop predicting.
//
// **The boundary is established before `exec`, by the kernel, and the command
// is then free to say whatever it likes.** Linux's Landlock takes a set of
// directories and a set of access rights and attaches them to the calling
// task; every future descendant inherits them, and nothing the descendant does
// can widen them. A command confined to `~/Downloads` cannot read `~/.ssh/id_ed25519`
// whether it names it absolutely, reaches it through a symlink planted inside
// the scope, walks up with `../../`, builds the path in a variable, or `cd`s
// there first — because none of those is a claim about the path that the kernel
// is asked to believe. The subject question does not get a better answer here;
// it stops being asked.
//
// Three rules shape everything below.
//
// **Refuse rather than degrade.** On a kernel that cannot hold the boundary,
// this package returns an error and no process is started. There is no branch
// in which a command runs unconfined because the confinement was unavailable.
// That is not caution for its own sake: today a job PARKS visibly when it
// proposes a command, and a silently-unconfined command would be strictly worse
// than the refusal it replaced. Every caller therefore gets a sentence it can
// show the user, naming what this kernel can and cannot do.
//
// **The confinement is confirmed, not assumed.** The helper writes one byte
// down a pipe AFTER `landlock_restrict_self` returns and BEFORE it execs the
// command. A caller that did not receive that byte knows the command never ran
// confined — and, because the write is the last thing before the exec, that it
// never ran at all. The ledger is told which of the two happened rather than
// being asked to infer it, which is #71's rule applied to this package's own
// bookkeeping.
//
// **The claim is filesystem-shaped, and only that.** Landlock as used here
// covers reads, writes, creation, deletion, truncation and cross-directory
// renames. It does not cover the network, signals, or connections to unix
// sockets that already exist — all three were measured, not assumed, and all
// three are stated in ADR 0068 as things this boundary does NOT protect
// against. A boundary that is described more widely than it holds is the
// failure this package was built to avoid, so its description stops exactly
// where the enforcement does.
package confine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// MinABI is the lowest Landlock ABI this package will run a command under, and
// the number is a guarantee rather than a convenience.
//
// ABI 1 (Linux 5.13) and ABI 2 (5.19) do not handle LANDLOCK_ACCESS_FS_TRUNCATE,
// which arrived in ABI 3 (6.2). Under those versions `truncate("/outside/file", 0)`
// is not an access Landlock knows how to refuse: the call takes a path rather
// than a descriptor, so a command that could not read or write a file outside
// the boundary could still empty it. That is a modification of a file outside
// the scope, which is precisely what a caller is being told cannot happen. ABI 2
// also lacks nothing else we need, and ABI 1 additionally has no
// LANDLOCK_ACCESS_FS_REFER, which would make every cross-directory `mv` fail
// even INSIDE the scope — a boundary that breaks the ordinary work is a boundary
// people route around.
//
// So the floor is 3 and older kernels are refused with the reason said out
// loud, rather than confined to a boundary with a hole in it that this package
// would then have to describe carefully enough for nobody to notice.
const MinABI = 3

// Support is what this kernel can actually enforce, measured rather than
// assumed. The ABI is read from the kernel itself
// (landlock_create_ruleset(LANDLOCK_CREATE_RULESET_VERSION)), not inferred from
// a release number, because a kernel can be built without Landlock at any
// version and a distribution can disable it in its LSM list.
type Support struct {
	// ABI is the Landlock ABI version the running kernel reports. Zero means
	// Landlock is absent, compiled out, or not enabled in the LSM list.
	ABI int
	// OK is whether this package will run a command on this kernel.
	OK bool
	// Because is the plain sentence to show the user when OK is false. It is
	// written here because this is the only code that knows what was measured.
	Because string
}

// Available reports what this kernel can hold. Cheap — one syscall — and safe
// to call per command rather than cached, so a caller never reports a support
// level from a kernel that has since been replaced under a running daemon.
func Available() Support {
	v := abiVersion()
	switch {
	case v <= 0:
		return Support{ABI: 0, Because: "this kernel has no Landlock, so I have no way " +
			"to hold a command inside the boundary — and I will not run one outside it"}
	case v < MinABI:
		return Support{ABI: v, Because: fmt.Sprintf(
			"this kernel's Landlock is version %d and I need at least %d: before %d it cannot "+
				"stop a command emptying a file outside the boundary, so the boundary I would "+
				"be claiming is not the one I could hold", v, MinABI, MinABI)}
	default:
		return Support{ABI: v, OK: true}
	}
}

// ErrUnconfinable is a boundary this machine cannot hold around a command. It
// carries the sentence rather than a code because the sentence is what reaches
// the user: a job parks on it, and "confinement unavailable" tells nobody
// anything they can act on.
type ErrUnconfinable struct{ Because string }

func (e *ErrUnconfinable) Error() string { return e.Because }

// Spec is the confinement one piece of work asks for.
type Spec struct {
	// Roots are the directories the command may read and write, absolute and
	// already symlink-resolved by the caller (internal/jobs.Scope does this at
	// validation, so the roots a scope holds are the real ones).
	Roots []string
	// Reserved are directories that must stay out of the command's reach
	// whatever the roots say — Jarvix's own configuration, state, data and
	// runtime directories. See Check for why they are a REFUSAL rather than a
	// subtraction.
	Reserved []string
}

// Check reports whether this spec can be held on this kernel, and says why not
// in words the user can act on.
//
// The interesting refusal is the reserved one, and it is a refusal because of a
// measured property of Landlock rather than a preference. **Landlock rights are
// a union up the tree, not a longest-prefix match.** A rule granting read/write
// on `~` and a second rule granting only read on `~/.config/jarvix` does not
// make the second directory read-only: the write right from the ancestor still
// applies, and adding a rule with no rights at all is rejected outright
// (ENOMSG). There is no way to say "everything under here except that".
//
// So a scope whose root contains Jarvix's own configuration cannot be confined
// in a way that keeps #109's wall standing. A command inside such a scope could
// rewrite `[tools]`, `[advisors]` or `[ai]` directly on disk — a new route to
// exactly the place a job is structurally forbidden to reach through tools —
// and a boundary the bounded thing can widen is decoration. The honest answer
// is to decline the command and say which directory made it impossible, which
// also points at the fix: a narrower scope.
//
// The test is ancestor-or-equal in one direction only. A root INSIDE the state
// directory (a job scoped to the artifacts folder, say) is fine: from in there,
// config.toml is outside the root and the kernel refuses it like anything else.
func (s Spec) Check(sup Support) error {
	if !sup.OK {
		return &ErrUnconfinable{Because: sup.Because}
	}
	roots := tidyPaths(s.Roots)
	if len(roots) == 0 {
		return &ErrUnconfinable{Because: "this job's boundary names no directory, and a " +
			"command has to be confined to somewhere before I will run it"}
	}
	for _, root := range roots {
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			return &ErrUnconfinable{Because: root + " is not a directory I can open, so I " +
				"cannot build the boundary the kernel would hold"}
		}
	}
	for _, root := range roots {
		for _, reserved := range tidyPaths(s.Reserved) {
			if !within(reserved, root) {
				continue
			}
			return &ErrUnconfinable{Because: root + " contains my own configuration, and the " +
				"kernel cannot hold part of a directory it has admitted — so a command in " +
				"there could rewrite what I am allowed to do. Give this job a narrower " +
				"boundary and I can run commands in it"}
		}
	}
	return nil
}

// within reports whether path is inside dir, or is dir.
//
// The root directory is special-cased, and it is not a nicety. Written as the
// obvious prefix test, `dir + "/"` becomes `"//"` when dir is `/`, which
// matches nothing — so a scope whose root was `/` would have been found not to
// contain Jarvix's own configuration, and would have been confined to a
// boundary that admitted the entire filesystem. Everything is inside `/`, and
// this says so.
func within(path, dir string) bool {
	if dir == string(filepath.Separator) {
		return true
	}
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

// tidyPaths cleans, drops blanks and non-absolute entries, sorts and
// de-duplicates. Non-absolute entries are dropped rather than resolved against
// a working directory, because a relative root is a root nobody can state.
func tidyPaths(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, p := range in {
		p = strings.TrimSpace(p)
		if p == "" || !filepath.IsAbs(p) {
			continue
		}
		p = filepath.Clean(p)
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// What a confined command may reach besides its own roots
// ---------------------------------------------------------------------------

// SystemBase is the operating system as installed, granted read and execute and
// nothing else. Without it a confined command could not run at all: `bash`
// cannot be executed if `/usr/bin/bash` is unreadable, and it cannot start if
// its shared libraries are not.
//
// The list is deliberately short, fixed, and made of directories that hold the
// *machine's* files rather than the *user's*. Nothing under `$HOME` is here,
// and neither is `/tmp`, `/var`, `/run` or `/proc` — which is what keeps the
// promise the scope actually makes. Two of those absences are load-bearing
// enough to name:
//
//   - **`/proc` is not granted.** `jarvixd` reads its API keys from its own
//     environment (config.Endpoint.Key), and `/proc/<pid>/environ` is readable
//     by the same user. Granting procfs would hand a confined command the very
//     credentials this package is supposed to keep out of its reach. The cost
//     is that `ps`, `/dev/fd` and process substitution need the narrower grant
//     the helper adds for the command's OWN process directory — see reexec.
//   - **`/run` is not granted**, so nothing under `$XDG_RUNTIME_DIR` can be
//     opened. That does NOT stop a command connecting to a unix socket whose
//     path it already knows; see ADR 0068, which states that gap rather than
//     implying it is covered.
//
// `/etc` is here and is the one judgement call. It holds the machine's
// configuration — the dynamic linker cache, the timezone, the CA bundle — and
// none of the user's own files. It is granted read-only, and a command that can
// read it learns nothing it could not have learned from any other program the
// user runs.
var SystemBase = []string{"/usr", "/bin", "/sbin", "/lib", "/lib64", "/opt", "/etc"}

// DeviceBase are the character devices a non-interactive command needs, granted
// read and write. `/dev` as a whole is not granted: it holds the machine's
// block devices, and a boundary that admits `/dev/sda` has not confined
// anything.
var DeviceBase = []string{"/dev/null", "/dev/zero", "/dev/full", "/dev/random", "/dev/urandom"}

// shellCandidates is where the interpreter is looked for, in order.
//
// It is a fixed list rather than exec.LookPath on purpose. The daemon's own
// PATH routinely points at version-manager shims under `$HOME`, and a shell
// resolved to one of those would sit OUTSIDE the system base and outside every
// root — so the command would fail at exec with a permission error that read
// like a confinement bug rather than a lookup one. A fixed absolute path is
// also one less thing the environment can move.
var shellCandidates = []string{"/usr/bin/bash", "/bin/bash", "/usr/bin/sh", "/bin/sh"}

// envAllowed is every variable a confined command inherits from the daemon, and
// the list is short because the alternative is a leak.
//
// `jarvixd` holds the user's model credentials in its own environment —
// config.Endpoint.Key reads them with os.Getenv — so a child that inherited the
// daemon's environment would be handed the Fireworks or LM Studio key in a
// variable any command can print. The child therefore gets an environment built
// from nothing: these names if the daemon has them, plus the three this package
// sets itself (HOME, TMPDIR, PATH), and not one thing else. Anything a command
// needs beyond this belongs in the command.
var envAllowed = []string{"LANG", "LC_ALL", "LC_CTYPE", "LC_COLLATE", "LC_NUMERIC", "LC_TIME", "TZ"}

// safePath is the PATH a confined command gets. It names exactly the
// directories SystemBase grants, so every entry on it is reachable and a
// command that cannot find a program has genuinely not got one rather than
// having been refused a lookup it could not see.
const safePath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// ---------------------------------------------------------------------------
// Running one
// ---------------------------------------------------------------------------

// Request is one confined command.
type Request struct {
	// Command is the shell command, interpreted by `bash -c` exactly as the
	// unconfined shell tool interprets it. Nothing here reads it.
	Command string
	// Spec is the boundary. Check must have passed for it, and Run checks
	// again rather than trusting the caller to have done so.
	Spec Spec
	// Timeout bounds the command. Zero means the context alone bounds it.
	Timeout time.Duration
	// MaxOutput caps the bytes captured. Zero means DefaultMaxOutput.
	MaxOutput int
}

// DefaultMaxOutput caps one command's captured output.
const DefaultMaxOutput = 16 * 1024

// Outcome is what was observed, and every field is an observation rather than
// an inference.
type Outcome struct {
	// Confined is whether the kernel accepted the ruleset before the command
	// ran, read off the helper's own confirmation byte rather than assumed
	// from the absence of an error. False means the command did not run.
	Confined bool
	// Output is the command's combined stdout and stderr, capped.
	Output string
	// Truncated is whether Output was cut at the cap.
	Truncated bool
	// Exit is the command's exit status; -1 when it did not exit normally.
	Exit int
	// Killed is whether the command was stopped rather than finishing — a
	// timeout, or the job being stopped underneath it.
	Killed bool
	// TimedOut narrows Killed to the one cause the caller can describe
	// honestly. "Stopped after 30 seconds" is a true sentence about a command
	// that ran out of time and a false one about a command the user stopped
	// after four, and the two arrive here as the same cancellation — so the
	// distinction is read from WHICH context expired rather than guessed from
	// the fact that one did.
	TimedOut bool
}

// exitNotConfined is what the helper exits with when it could not establish the
// boundary. It is a backstop for the confirmation pipe rather than the primary
// signal: a command could exit with any status it likes, so the byte is what
// Outcome.Confined is read from.
const exitNotConfined = 111

// planEnv carries the helper's instructions. It is read and then replaced: the
// environment the command finally runs with is built by the parent and passed
// to execve, so this variable is not among the ones the command can see.
const planEnv = "JARVIX_CONFINE_PLAN"

// statusFD is where the helper confirms the confinement. Three, because the
// parent passes exactly one extra descriptor.
const statusFD = 3

// plan is what the parent tells the helper. JSON rather than flags because it
// is a private protocol between two copies of the same binary and a flag set
// would invite something else to call it.
type plan struct {
	Write  []string `json:"write"`
	Read   []string `json:"read"`
	Device []string `json:"device"`
	Dir    string   `json:"dir"`
	Argv   []string `json:"argv"`
	Env    []string `json:"env"`
}

// Runner runs confined commands.
//
// The three seams exist for tests and for nothing else, and each defaults to
// the real thing when it is left zero — so production code constructs a
// Runner{} and gets the measured kernel, this executable and the real system
// base, with no way to configure the boundary looser by accident.
type Runner struct {
	// Helper is the binary re-executed to apply the confinement. Empty uses
	// this executable, which is what makes the helper the same program: there
	// is no separate binary to install, to keep in step, or to substitute.
	Helper string
	// Kernel overrides the support probe. Nil measures the running kernel.
	Kernel func() Support
	// Base overrides SystemBase. Nil uses it.
	Base []string
}

// Run establishes the boundary and runs the command inside it, or returns an
// error and starts nothing.
//
// The order is the safety argument and every line of it is load-bearing:
// check the kernel, check the spec, build an environment from nothing, spawn
// the helper, read the helper's confirmation that the kernel accepted the
// ruleset, and only then report anything about the command at all.
func (r Runner) Run(ctx context.Context, req Request) (Outcome, error) {
	support := Available
	if r.Kernel != nil {
		support = r.Kernel
	}
	if err := req.Spec.Check(support()); err != nil {
		return Outcome{}, err
	}
	shell, err := findShell()
	if err != nil {
		return Outcome{}, err
	}
	helper := r.Helper
	if helper == "" {
		if helper, err = os.Executable(); err != nil {
			return Outcome{}, &ErrUnconfinable{Because: "I could not find my own program on " +
				"disk, and I confine a command by re-running myself: " + err.Error()}
		}
	}

	roots := tidyPaths(req.Spec.Roots)
	// A private temporary directory, granted like a root and thrown away after.
	// Without it TMPDIR would point at /tmp, which is not granted, and every
	// command that writes a temporary file would fail for a reason that had
	// nothing to do with the user's boundary. Making it per-command rather than
	// granting /tmp is the narrower answer: two jobs cannot see each other's
	// working files, and nothing survives the command that made it.
	tmp, err := os.MkdirTemp("", "jarvix-confined-")
	if err != nil {
		return Outcome{}, &ErrUnconfinable{
			Because: "I could not make a scratch directory for the command: " + err.Error()}
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	base := r.Base
	if base == nil {
		base = SystemBase
	}
	p := plan{
		Write:  append(append([]string(nil), roots...), tmp),
		Read:   existing(base),
		Device: existing(DeviceBase),
		Dir:    roots[0],
		Argv:   []string{shell, "-c", req.Command},
		Env:    childEnv(roots[0], tmp),
	}
	encoded, err := json.Marshal(p)
	if err != nil {
		return Outcome{}, &ErrUnconfinable{Because: "I could not describe the boundary to " +
			"myself: " + err.Error()}
	}

	runCtx := ctx
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	statusR, statusW, err := os.Pipe()
	if err != nil {
		return Outcome{}, &ErrUnconfinable{
			Because: "I could not open a pipe to check the boundary was applied: " + err.Error()}
	}
	defer func() { _ = statusR.Close() }()

	cap := req.MaxOutput
	if cap <= 0 {
		cap = DefaultMaxOutput
	}
	out := &capped{limit: cap}

	cmd := exec.CommandContext(runCtx, helper)
	// The helper's environment is the plan and nothing else. It is replaced
	// wholesale at execve with plan.Env, so the plan variable does not reach
	// the command either.
	cmd.Env = []string{planEnv + "=" + string(encoded)}
	cmd.Stdout, cmd.Stderr = out, out
	cmd.Stdin = nil // non-interactive: the parent opens /dev/null, not the child
	cmd.ExtraFiles = []*os.File{statusW}
	// Its own process group, so stopping the job reaches everything the command
	// started and not just the shell. A command that backgrounds a child and
	// exits would otherwise leave that child running after the job reported
	// itself stopped, which is a report claiming something that is not true.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killGroup(cmd) }
	// The backstop for a command that exits while a grandchild still holds the
	// output pipe open: without it Wait would block on that grandchild for as
	// long as it cared to live, and the job would report nothing at all.
	cmd.WaitDelay = waitDelay

	if err := cmd.Start(); err != nil {
		_ = statusW.Close()
		return Outcome{}, &ErrUnconfinable{
			Because: "I could not start the confined command: " + err.Error()}
	}
	// The parent's copy goes now, so the read below sees EOF as soon as the
	// helper's copy closes — which it does immediately before execve.
	_ = statusW.Close()
	confirmation, _ := io.ReadAll(statusR)

	waitErr := cmd.Wait()
	outcome := Outcome{
		Confined:  len(confirmation) == 1 && confirmation[0] == confirmed,
		Output:    out.String(),
		Truncated: out.truncated,
		Exit:      exitStatus(waitErr, cmd),
		Killed:    runCtx.Err() != nil,
		TimedOut:  errors.Is(runCtx.Err(), context.DeadlineExceeded),
	}
	if !outcome.Confined {
		// One refusal for both of the sequences that are not "k", because they
		// are the same fact to a caller: the command did not run. Said as an
		// error rather than as an empty outcome, because a caller that treated
		// this as "the command ran and produced nothing" would be making
		// exactly the claim this package exists to prevent.
		return outcome, &ErrUnconfinable{Because: "I could not run that command inside the " +
			"boundary, so I did not run it" + trailing(out.String())}
	}
	return outcome, nil
}

// The two bytes the helper may write down the status pipe. Any values would do;
// named ones make the two ends readable. See serve for what each sequence means.
const (
	confirmed  = 'k'
	notStarted = 'x'
)

// waitDelay is how long Wait will go on waiting for the output pipes after the
// command itself has gone. Short: it is a backstop for an orphaned grandchild,
// not a grace period for the command.
const waitDelay = 2 * time.Second

// trailing appends whatever the helper managed to say about its own failure, so
// a refusal carries the reason rather than only the fact.
func trailing(said string) string {
	said = strings.TrimSpace(said)
	if said == "" {
		return "."
	}
	return ": " + said
}

// exitStatus reads the command's exit code out of Wait's error, -1 when it did
// not exit normally.
func exitStatus(err error, cmd *exec.Cmd) int {
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		return ee.ExitCode()
	case cmd.ProcessState != nil:
		return cmd.ProcessState.ExitCode()
	default:
		return -1
	}
}

// killGroup ends the command's whole process group. The negative pid is the
// group, which is why Setpgid is set above: without it this would signal the
// daemon's own group, and with only cmd.Process.Kill() a backgrounded
// grandchild would survive the job that started it.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}

// childEnv builds the command's environment from nothing.
//
// HOME points at the job's first root rather than at the user's real home,
// which is not cosmetic: a program that looks for `~/.gitconfig` under the real
// home would be refused by the kernel and would report that refusal as a
// warning on every run, teaching the reader to ignore permission errors from
// inside a confinement. Pointing HOME somewhere reachable means a denied path
// is news again.
func childEnv(home, tmp string) []string {
	env := []string{"HOME=" + home, "TMPDIR=" + tmp, "PATH=" + safePath}
	for _, name := range envAllowed {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return env
}

// existing drops the paths this machine does not have. A distribution without
// /lib64, or without /opt, is not a reason to refuse a command — but a rule on
// a path that is not there is an error from landlock_add_rule, so the list is
// filtered here where the filtering can be explained rather than in the helper
// where it would look like leniency.
func existing(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if _, err := os.Lstat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// findShell picks the interpreter, refusing rather than guessing.
func findShell() (string, error) {
	for _, candidate := range shellCandidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", &ErrUnconfinable{Because: "I could not find a shell in " +
		strings.Join(shellCandidates, " or ") + ", and I will not look for one on a PATH " +
		"that the boundary may not admit"}
}

// capped is an io.Writer that stops at a limit and remembers that it did. A
// command that prints a gigabyte is a command whose output nobody is going to
// read, and buffering it would be a job writing that gigabyte to the ledger.
type capped struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (c *capped) Write(p []byte) (int, error) {
	room := c.limit - c.buf.Len()
	if room <= 0 {
		c.truncated = true
		return len(p), nil
	}
	if len(p) > room {
		c.buf.Write(p[:room])
		c.truncated = true
		return len(p), nil
	}
	c.buf.Write(p)
	return len(p), nil
}

func (c *capped) String() string { return c.buf.String() }

// ---------------------------------------------------------------------------
// The other half: this same binary, re-executed
// ---------------------------------------------------------------------------

// Reexec is the helper. Call it as the FIRST thing in main — and in TestMain
// for any package whose tests run a confined command — before flags, before
// configuration, before anything that could fail or print.
//
// With no plan in the environment it returns immediately and the program is
// whatever it was. With one, it never returns: it applies the confinement to
// its own thread and execs the command, or it reports that it could not and
// exits without running anything.
//
// Re-executing this binary is what makes the confinement happen in the right
// place. Landlock attaches to the calling task, so the daemon cannot apply it
// to itself, and Go gives no hook between fork and exec in which a child could
// apply it. A second copy of this program, whose only job is to lock itself
// down and then become the command, is the seam that exists.
func Reexec() {
	raw, ok := os.LookupEnv(planEnv)
	if !ok {
		return
	}
	// serve does not return when it succeeds — it BECOMES the command — so
	// reaching the next line at all is the failure, and there is no success
	// branch to write. Whatever it has to say goes to the child's stderr, which
	// the parent has captured, so the refusal it reports carries the reason
	// rather than only the fact.
	fmt.Fprintln(os.Stderr, serve(raw))
	os.Exit(exitNotConfined)
}

// serve applies the confinement and becomes the command.
//
// It never returns nil: on success the process is replaced by execve and there
// is nothing left to return to. Every path back to the caller is therefore a
// path on which the command did not run.
func serve(raw string) error {
	var p plan
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return fmt.Errorf("the boundary I was given makes no sense: %w", err)
	}
	if len(p.Argv) == 0 {
		return errors.New("I was given a boundary and no command to run inside it")
	}
	if p.Dir != "" {
		if err := os.Chdir(p.Dir); err != nil {
			return fmt.Errorf("I could not start in %s: %w", p.Dir, err)
		}
	}
	if err := restrict(p); err != nil {
		return err
	}
	// The confirmation, written the instant the kernel said yes.
	_, _ = syscall.Write(statusFD, []byte{confirmed})
	// Close-on-exec rather than closed, so the descriptor itself carries the
	// second fact. A successful execve closes it and the parent reads exactly
	// one byte; a failed one leaves this process alive to write the second, and
	// the parent reads two. So:
	//
	//	""   the boundary was never established, and nothing was started
	//	"k"  the boundary held and the command became this process
	//	"kx" the boundary held and the command never started
	//
	// The middle case is the only one a caller may describe as "it ran", and it
	// is read off the pipe rather than inferred from the absence of an error —
	// which is the same rule the ledger lives by, applied to the one place this
	// package could have quietly guessed.
	if err := cloexec(statusFD); err != nil {
		return err
	}
	err := syscall.Exec(p.Argv[0], p.Argv, p.Env)
	_, _ = syscall.Write(statusFD, []byte{notStarted})
	_ = syscall.Close(statusFD)
	return fmt.Errorf("the boundary held and then I could not start %s: %w", p.Argv[0], err)
}
