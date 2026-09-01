//go:build linux

package confine

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"syscall"
	"unsafe"
)

// The Landlock syscalls, by hand.
//
// By hand because the project has exactly one dependency (BurntSushi/toml) and
// the standing rule is to ask before adding another. There is a good Landlock
// library, and this is three syscalls and a bitmask: taking a dependency to
// avoid writing forty lines would be paying a supply-chain cost for a
// convenience. Everything here is pinned by the tests in this package, which
// run the real syscalls against a real kernel and assert on files rather than
// on errors.
//
// The numbers are the same on every architecture Linux has added them to —
// they were allocated after the syscall table was unified for new calls — so
// there is no per-architecture table to keep in step.
const (
	sysLandlockCreateRuleset = 444
	sysLandlockAddRule       = 445
	sysLandlockRestrictSelf  = 446
)

// createRulesetVersion asks landlock_create_ruleset for the ABI version instead
// of a ruleset. It is the only honest way to find out what this kernel can do:
// a release number does not tell you whether Landlock was compiled in, and
// /sys/kernel/security/lsm does not tell you which ABI.
const createRulesetVersion = 1

// ruleTypePathBeneath is the only rule type this package uses.
const ruleTypePathBeneath = 1

// prSetNoNewPrivs is prctl's PR_SET_NO_NEW_PRIVS. landlock_restrict_self
// requires it for an unprivileged caller, which is the point: a process that
// cannot gain privileges cannot execute its way out of the domain through a
// setuid binary.
const prSetNoNewPrivs = 38

// oPath opens a directory for its identity rather than for its contents, which
// is all landlock_add_rule needs. syscall does not export it.
const oPath = 0x200000

// The filesystem access rights, bit for bit as the kernel defines them.
const (
	accessExecute    = 1 << 0
	accessWriteFile  = 1 << 1
	accessReadFile   = 1 << 2
	accessReadDir    = 1 << 3
	accessRemoveDir  = 1 << 4
	accessRemoveFile = 1 << 5
	accessMakeChar   = 1 << 6
	accessMakeDir    = 1 << 7
	accessMakeReg    = 1 << 8
	accessMakeSock   = 1 << 9
	accessMakeFifo   = 1 << 10
	accessMakeBlock  = 1 << 11
	accessMakeSym    = 1 << 12
	accessRefer      = 1 << 13
	accessTruncate   = 1 << 14
)

// handledFS is everything this package asks the kernel to police, and it is
// deliberately constant across every ABI at or above MinABI rather than growing
// with the kernel.
//
// Two rights are worth naming.
//
// REFER is handled and granted on the roots. That reads backwards until you
// know the rule: if REFER is NOT handled, the kernel denies every rename and
// hard link ACROSS directories, including ones entirely inside the scope. So
// leaving it out would break `mv one/x two/y` in the user's own tree while
// adding nothing. Handled and granted on the roots, a rename inside the scope
// works and a rename out of it does not, because the destination is not
// covered.
//
// TRUNCATE is handled because truncate(2) takes a path rather than a
// descriptor: without it a command that could neither open nor write a file
// outside the scope could still empty one. It is why MinABI is 3.
//
// IOCTL_DEV (ABI 5) and the network rights (ABI 4) are deliberately NOT
// handled. Handling a right the caller then has to be granted everywhere is a
// way to break commands for no gain, and handling the network ones would let
// this package imply a network boundary it does not hold — Landlock covers TCP
// bind and connect only, so UDP, DNS and raw sockets would still be open and
// "no network" would be a false sentence. ADR 0068 says so out loud instead.
const handledFS = accessExecute | accessWriteFile | accessReadFile | accessReadDir |
	accessRemoveDir | accessRemoveFile | accessMakeChar | accessMakeDir | accessMakeReg |
	accessMakeSock | accessMakeFifo | accessMakeBlock | accessMakeSym | accessRefer |
	accessTruncate

// The three grants.
const (
	// grantWrite is a root: everything the handled set covers.
	grantWrite = handledFS
	// grantRead is the system base: enough to run a program and read the
	// machine's own files, and nothing that changes them.
	grantRead = accessExecute | accessReadFile | accessReadDir
	// grantDevice is a character device: read and write the stream.
	grantDevice = accessReadFile | accessWriteFile
)

// rulesetAttr is struct landlock_ruleset_attr. Only the first field is passed:
// landlock_create_ruleset takes an explicit size and the kernel zero-fills the
// rest, so a short attribute is how a program says "I am not asking about
// network or scoping" on a kernel that has both.
type rulesetAttr struct {
	HandledAccessFS uint64
}

// pathBeneathAttr is struct landlock_path_beneath_attr, which the kernel
// declares packed: an 8-byte access mask followed by a 4-byte descriptor. Go
// lays this struct out with the same first twelve bytes and pads to sixteen,
// and landlock_add_rule reads exactly twelve, so the padding is never looked
// at. The two fields must stay in this order for that to hold.
type pathBeneathAttr struct {
	AllowedAccess uint64
	ParentFD      int32
}

// abiVersion asks the kernel which Landlock it has, 0 when it has none.
func abiVersion() int {
	v, _, _ := syscall.Syscall(sysLandlockCreateRuleset, 0, 0, createRulesetVersion)
	if int(v) < 0 {
		return 0
	}
	return int(v)
}

// restrict builds the ruleset, attaches it to THIS THREAD, and returns. After
// it returns nil the calling thread — and every process it goes on to exec or
// fork — is inside the boundary, permanently and irreversibly.
//
// runtime.LockOSThread is not optional and not defensive. Landlock attaches to
// the calling task, and a goroutine that is not locked to its thread may be
// running on a different one by the time it reaches execve — which would leave
// the command running on a thread that was never restricted. Locking here and
// exec'ing from the same goroutine (see serve) is what makes the two calls
// happen to the same task. The lock is never released: the only thing this
// thread does afterwards is become the command.
func restrict(p plan) error {
	attr := rulesetAttr{HandledAccessFS: handledFS}
	fd, _, errno := syscall.Syscall(sysLandlockCreateRuleset,
		uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if errno != 0 {
		return fmt.Errorf("this kernel would not give me a boundary to hold: %w", errno)
	}
	ruleset := int(fd)
	defer func() { _ = syscall.Close(ruleset) }()

	grants := []struct {
		paths  []string
		access uint64
	}{
		{p.Write, grantWrite},
		{p.Read, grantRead},
		{p.Device, grantDevice},
	}
	for _, g := range grants {
		for _, path := range g.paths {
			if err := allow(ruleset, path, g.access); err != nil {
				return err
			}
		}
	}
	// The command's own process directory, and only its own.
	//
	// This is what /dev/fd, /dev/stdout and `<(…)` process substitution need,
	// all of which resolve through /proc/self. Granting it here rather than in
	// the parent's plan is the whole trick: "self" is resolved now, in the
	// process that is about to BECOME the command — execve keeps the pid — so
	// the rule names one process directory and cannot name another. Granting
	// /proc instead would have handed the command /proc/<jarvixd>/environ,
	// which is where the user's model credentials live.
	//
	// Best-effort: a kernel or container without procfs mounted is not a reason
	// to refuse a boundary that is otherwise exactly as tight as it should be.
	_ = allow(ruleset, "/proc/"+strconv.Itoa(os.Getpid()), grantDevice|accessReadDir)

	runtime.LockOSThread()
	if _, _, errno := syscall.Syscall(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0); errno != 0 {
		return fmt.Errorf("I could not stop the command gaining privileges, so I did not "+
			"start it: %w", errno)
	}
	if _, _, errno := syscall.Syscall(sysLandlockRestrictSelf, uintptr(ruleset), 0, 0); errno != 0 {
		return fmt.Errorf("this kernel would not hold the boundary: %w", errno)
	}
	// The second wall, on the same thread and under the same no-new-privs, so
	// the two are established or refused together (#222, ADR 0069). Landlock
	// governs no access right over connecting to a unix socket, and Jarvix's own
	// IPC socket is one — so without this the boundary would keep a command out
	// of config.toml while leaving it free to ask the daemon to rewrite
	// config.toml. See seccomp_linux.go.
	//
	// After Landlock rather than before, so that a kernel which can do neither
	// reports the more fundamental failure first.
	return denyUnixSockets()
}

// cloexec marks a descriptor to be closed by a successful execve, which is how
// the status pipe reports that the command actually started. See serve.
func cloexec(fd int) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd),
		syscall.F_SETFD, syscall.FD_CLOEXEC); errno != 0 {
		return fmt.Errorf("I could not arrange to confirm the command had started: %w", errno)
	}
	return nil
}

// allow adds one path rule to the ruleset.
func allow(ruleset int, path string, access uint64) error {
	fd, err := syscall.Open(path, oPath|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("I could not open %s to put it inside the boundary: %w", path, err)
	}
	defer func() { _ = syscall.Close(fd) }()
	attr := pathBeneathAttr{AllowedAccess: access, ParentFD: int32(fd)}
	_, _, errno := syscall.Syscall6(sysLandlockAddRule, uintptr(ruleset), ruleTypePathBeneath,
		uintptr(unsafe.Pointer(&attr)), 0, 0, 0)
	if errno != 0 {
		return fmt.Errorf("this kernel would not admit %s to the boundary: %w", path, errno)
	}
	return nil
}
