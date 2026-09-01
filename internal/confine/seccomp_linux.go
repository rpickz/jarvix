//go:build linux && (amd64 || arm64)

package confine

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

// The second wall: a confined command cannot make a unix socket (#222, ADR
// 0069).
//
// Landlock closes the filesystem and nothing else, and one of the things it
// does not close turned out to matter more than the rest of that list put
// together. **Landlock defines no access right covering `connect(2)` to a unix
// socket** — measured, not assumed: a confined command connected to a listener
// outside its roots with the socket's directory ungranted. Jarvix's own IPC
// socket is a unix socket, its vocabulary includes `config.upsert_entry`,
// `config.set` and `config.delete_entry`, and its only guard is mode 0600 —
// same uid, which a confined child has.
//
// So the wall that stops a command rewriting `config.toml` did not stop it
// asking the daemon to rewrite `config.toml`. A confinement that hands back the
// ability to reconfigure the confiner is a net loss however good its
// filesystem half is, and #109's wall is meant to be structurally unreachable
// rather than merely inconvenient. That is a hole in the guarantee, not a
// narrowing of it, and it had to close before the feature was worth having.
//
// **The mechanism is seccomp, and the choice is argued in ADR 0069.** The
// obvious alternative — a per-run secret in the runtime directory, which
// Landlock already puts out of a confined command's reach — is a good design
// and was written before it was withdrawn. It failed on the client the project
// cannot afford to break: the Quickshell window opens SEVEN independent sockets
// (`Quickshell.Io.Socket`, one per surface, ADR 0013), writes ~87 hand-rolled
// JSON-RPC frames with no send() choke point to thread a handshake through, and
// has never read a file in its life — there is no `FileView` anywhere in the
// plugin and the test harness stubs `Quickshell.Io` with exactly two types. The
// key could not have been verified against a real Quickshell from here, and a
// broken window is a broken assistant.
//
// Blocking the syscall instead has a property the token cannot match: **no
// client changes at all**. The CLI, `jarvix backup` and all seven of the
// window's sockets keep working byte for byte, because nothing about how the
// daemon is reached has changed — only what the confined command may do.
//
// **What is blocked is `socket(AF_UNIX, …)`, not `connect`.** That is forced
// rather than chosen: seccomp can inspect scalar syscall arguments but cannot
// dereference pointers, and `connect`'s address is a pointer — a filter cannot
// see which socket is being reached. `socket`'s domain is a plain int, so the
// capability can be removed at the point it is created. A process that cannot
// make a unix socket cannot connect to one, and the ways of obtaining one
// without `socket(2)` are all already shut: no unix descriptor is inherited
// (the child gets stdin, stdout, stderr and a status pipe closed at exec),
// `SCM_RIGHTS` needs a socket to arrive on, `open(2)` on a socket file gives
// ENXIO, and `/proc` is not in the boundary.
//
// `socketpair(2)` is deliberately left alone. It is a different syscall, it
// creates an anonymous connected pair with no name in any filesystem, and it
// cannot reach a listener — so blocking it would break ordinary programs to no
// end.

// The seccomp constants, by hand, for the same reason the Landlock ones are:
// this is one small filter and a dependency would be a supply-chain cost bought
// with a convenience.
const (
	prSetSeccomp       = 22
	seccompModeFilter  = 2
	seccompRetAllow    = 0x7fff0000
	seccompRetErrno    = 0x00050000
	seccompRetKillProc = 0x80000000

	// afUnix is AF_UNIX, which is also AF_LOCAL. One is the other.
	afUnix = 1
)

// The classic-BPF opcodes this filter needs.
const (
	bpfLdWAbs = 0x20 // BPF_LD | BPF_W | BPF_ABS
	bpfJeqK   = 0x15 // BPF_JMP | BPF_JEQ | BPF_K
	bpfRetK   = 0x06 // BPF_RET | BPF_K
)

// Offsets into struct seccomp_data: {int nr; __u32 arch; __u64 ip; __u64 args[6];}.
const (
	offsetNR   = 0
	offsetArch = 4
	offsetArg0 = 16
)

// sockFilter is struct sock_filter.
type sockFilter struct {
	Code uint16
	JT   uint8
	JF   uint8
	K    uint32
}

// sockFprog is struct sock_fprog. The six bytes of padding are what the C
// compiler inserts to align the pointer on a 64-bit machine, and Go lays the
// struct out the same way — but they are written down rather than left implicit
// because a silently different layout here would hand the kernel a filter
// pointer read out of the wrong bytes.
type sockFprog struct {
	Len    uint16
	_      [6]byte
	Filter *sockFilter
}

// auditArch is this machine's AUDIT_ARCH_*, which the filter checks first.
//
// The check is not ceremony. On x86-64 the x32 ABI shares the syscall entry and
// sets bit 30 of the syscall number, so a filter that matched on the number
// alone could be walked around by issuing the same call under the other ABI.
// The architecture is therefore verified before the number is looked at, and an
// unexpected one kills the process rather than falling through to allow.
func auditArch() (uint32, error) {
	switch runtime.GOARCH {
	case "amd64":
		return 0xC000003E, nil // AUDIT_ARCH_X86_64
	case "arm64":
		return 0xC00000B7, nil // AUDIT_ARCH_AARCH64
	default:
		return 0, fmt.Errorf("I do not know how to block unix sockets on %s, and I will "+
			"not run a command I cannot keep away from my own socket", runtime.GOARCH)
	}
}

// denyUnixSockets installs the filter on the calling thread. Once installed it
// cannot be removed, and it survives both fork and exec — which, with
// PR_SET_NO_NEW_PRIVS already set by restrict, is what makes it a property of
// the command rather than a request to it.
//
// The refusal is EACCES rather than a kill. A command that tried to reach a
// unix socket and got "permission denied" reports something a person can read
// in the ledger; a command killed by SIGSYS loses whatever else it was doing
// and reports nothing. The boundary is meant to refuse an action, not to
// destroy the work around it.
func denyUnixSockets() error {
	arch, err := auditArch()
	if err != nil {
		return err
	}
	// The jump offsets are left zero here and written by fixupJumps from named
	// targets. See its comment for why they are not spelled out inline.
	filter := []sockFilter{
		/* 0 */ {Code: bpfLdWAbs, K: offsetArch}, // is this the architecture the numbers below belong to?
		/* 1 */ {Code: bpfJeqK, K: arch}, // no → kill
		/* 2 */ {Code: bpfLdWAbs, K: offsetNR}, // is it socket(2)?
		/* 3 */ {Code: bpfJeqK, K: uint32(syscall.SYS_SOCKET)}, // no → allow
		// Is its domain AF_UNIX? The low word is what the kernel truncates the
		// register to when it reads `int domain`, so this sees exactly what the
		// call will act on — including a caller that set high bits to hide it.
		/* 4 */ {Code: bpfLdWAbs, K: offsetArg0},
		/* 5 */ {Code: bpfJeqK, K: afUnix}, // yes → deny
		/* 6 */ {Code: bpfRetK, K: seccompRetKillProc}, // wrong architecture
		/* 7 */ {Code: bpfRetK, K: seccompRetErrno | uint32(syscall.EACCES)},
		/* 8 */ {Code: bpfRetK, K: seccompRetAllow},
	}
	fixupJumps(filter)

	prog := sockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	_, _, errno := syscall.Syscall6(syscall.SYS_PRCTL, prSetSeccomp, seccompModeFilter,
		uintptr(unsafe.Pointer(&prog)), 0, 0, 0)
	runtime.KeepAlive(filter)
	if errno != 0 {
		return fmt.Errorf("this kernel would not stop the command reaching my own socket, "+
			"so I did not start it: %w", errno)
	}
	return nil
}

// The three return instructions, by index in the filter above.
const (
	retKill  = 6
	retDeny  = 7
	retAllow = 8
)

// fixupJumps writes the jump offsets from named targets rather than by hand.
//
// Classic BPF counts a jump from the instruction AFTER the jump, so every
// offset is a subtraction that is easy to get right once and wrong for ever
// after — and a filter with an off-by-one here does not fail loudly. It falls
// through to whichever return it lands on, and if that is the allow, the wall
// is silently not there. That is the one failure this whole package exists to
// prevent, so the arithmetic is done in one place, from labels, where a reader
// can check it: `target - (index of the jump) - 1`.
func fixupJumps(filter []sockFilter) {
	filter[1].JF = uint8(retKill - 1 - 1)  // architecture is not ours → kill
	filter[3].JF = uint8(retAllow - 3 - 1) // not socket(2) → nothing to say
	filter[5].JT = uint8(retDeny - 5 - 1)  // AF_UNIX → refuse
	filter[5].JF = uint8(retAllow - 5 - 1) // any other domain → allow
}
