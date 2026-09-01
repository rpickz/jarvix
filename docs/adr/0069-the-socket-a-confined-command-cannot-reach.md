# ADR 0069 — The socket a confined command cannot reach

**Status:** accepted

## Context

[ADR 0068](0068-a-command-the-kernel-holds.md) confines a job's shell command to
its scope's roots with Landlock, and states honestly what that does not cover.
One item on that list was different in kind from the others, and it should not
have shipped as a documented limitation.

**Landlock defines no access right covering `connect(2)` to a unix socket.**
This was measured, not inferred: a confined command connected to a listener
outside its roots, with the socket's directory not granted and `/run` absent
from the system base.

Jarvix's IPC socket is a unix socket. Its vocabulary includes
`config.upsert_entry`, `config.set` and `config.delete_entry`, and its only
guard is mode 0600 — which keeps other *people* out and says nothing about the
user's own processes, every one of which has the right uid. A confined child is
one of those.

So the wall that stopped a command from writing `config.toml` did not stop it
from asking the daemon to write `config.toml`. That is not a narrowing of the
guarantee, it is a hole through it: #109's wall is meant to make a job
*structurally* unable to reconfigure Jarvix, and a confinement feature that
hands back the ability to reconfigure the confiner is a net loss however good
its filesystem half is.

The hole was real. With the fix reverted, the test in
`internal/daemon/jobssocket_test.go` shows a confined command connecting to the
daemon's own socket, issuing `config.set`, and the daemon answering
`{"result":{"ok":true}}`.

## Decision

**A confined command cannot create a unix socket. A seccomp-BPF filter denying
`socket(AF_UNIX, …)` is installed on the same thread, at the same moment, under
the same no-new-privs as the Landlock ruleset — and, like it, a machine where it
cannot be installed does not run the command.**

The filter is nine classic-BPF instructions: verify the architecture, match
`socket(2)`, match domain `AF_UNIX`, return `EACCES`. Everything else is
allowed; a wrong architecture kills the process rather than falling through.

### Why the syscall and not the socket

The first design was a per-run secret, and it is the one that fits this
codebase's grain: Landlock already puts the runtime directory out of a confined
command's reach, and `confine.Spec.Check` already refuses any scope enclosing
it, so a key stored there would be readable by every legitimate client and
unreadable by any confined child — **enforced by the wall that had just been
built** rather than by a check the daemon performs and could get wrong. It was
written before it was withdrawn.

It was withdrawn on the client this project cannot afford to break. The
Quickshell window is not one client, it is **seven independent
`Quickshell.Io.Socket` declarations** (one per surface, ADR 0013), each with its
own connect handler and retry timer, and the ~87 outbound requests are
hand-rolled `daemon.write(JSON.stringify(…))` calls with **no `send()` choke
point** to thread a handshake through. Worse, the plugin has never read a file:
there is no `FileView` anywhere in it, and the test harness stubs
`Quickshell.Io` with exactly two types (`Socket`, `SplitParser`), so the reading
of the key could not have been exercised by `qml-test.sh` and could not have
been verified against a real Quickshell from here at all. A broken window is a
broken assistant, and "probably the right API" is not a standard this change is
allowed to work to.

Blocking the syscall has the property the key cannot match: **no client changes
at all.** Nothing about how the daemon is reached has changed — only what a
confined command may do. The CLI, `jarvix backup` and all seven of the window's
sockets keep working byte for byte, and
`TestEveryExistingClientStillReachesTheDaemon` pins both transports.

The two alternatives raised alongside the key were weighed and rejected:

- **`SO_PEERCRED` plus a process-group check.** Less invasive, but a child can
  `setsid()` out of its process group and a double fork reparents it. It
  distinguishes the well-behaved from the permitted, which is the wrong
  distinction for a security boundary.
- **A cgroup-membership check on the peer.** Robust — a process cannot leave its
  cgroup unprivileged — but a confined child *inherits jarvixd's own cgroup*, so
  the check only works if each command is first placed in a cgroup of its own.
  That needs delegation, pid-reuse care, and a second mechanism in a second
  place. Its failure mode is also the wrong way round: if the cgroup cannot be
  created or read, a daemon-side check has to decide what to do with a peer it
  cannot classify.

The filter's failure mode is the right way round by construction. It is applied
by the same helper that applies Landlock, immediately before `execve`; if it
cannot be installed the helper returns an error, the confirmation byte is never
written, and `Run` reports that the command did not start. There is no branch in
which a command runs with one wall and not the other.

### Why `socket(2)` and not `connect(2)`

Forced rather than chosen. Seccomp can inspect scalar syscall arguments but
cannot dereference pointers, and `connect`'s address is a pointer — a filter
cannot see which socket is being reached. `socket`'s domain is a plain `int`, so
the capability can be removed where it is created instead.

A process that cannot make a unix socket cannot connect to one, and every other
route to a unix descriptor is already shut:

| route | why it is closed |
| --- | --- |
| inheriting one | the child gets stdin, stdout, stderr and a status pipe closed at `exec`; Go marks everything else `CLOEXEC` |
| `SCM_RIGHTS` | needs a socket to arrive on |
| `open(2)` on the socket file | returns `ENXIO`; you cannot connect through it |
| `/proc/<pid>/fd` | `/proc` is not in the boundary, and only the command's own directory is granted |

`socketpair(2)` is deliberately left alone: a different syscall, an anonymous
connected pair with no name in any filesystem, unable to reach a listener.
Blocking it would break ordinary programs for nothing.

`EACCES` rather than `SECCOMP_RET_KILL`: a command that tried and was refused
reports something a person can read in the ledger, where a command killed by
`SIGSYS` loses whatever else it was doing and reports nothing. The boundary
refuses an action; it does not destroy the work around it.

### The second lock on the same door

While writing the tests for this, a bug was found in ADR 0068's own refusal.
`confine.within(path, dir)` was the obvious prefix test, and for `dir = "/"` it
formed `"//"`, which matches nothing — so **a job scoped to `/` was found not to
contain Jarvix's configuration** and would have been confined to a boundary
admitting the entire filesystem. Fixed, and pinned by
`TestNoConfinableScopeCanContainTheDaemonsSocket`, which checks the runtime
directory and `/` together.

## What this does and does not change

- **Nothing about the daemon.** No handshake, no key, no new method, no change
  to `docs/ipc.md`. The socket's existing guard is untouched, not weakened.
- **Nothing about any client.** The Go transport and a hand-written frame both
  still reach the daemon, and both are tested.
- **Everything about a job's command.** It has no unix sockets at all.

That last point is broader than the hole it closes, and the breadth is stated
rather than discovered. Inside a job, syslog via `/dev/log`, D-Bus, an SSH agent
and `docker.sock` all stop working. Two of those are worth naming as *gains* —
a job's command reaching D-Bus or the Docker socket would have been two more
routes to the same escalation — and none of them is something a job needs to
tidy a folder or get the CI green. `TestOrdinaryWorkStillRunsUnderTheFilter`
pins that pipes, subshells, redirection, loops and the coreutils are unaffected.

## Consequences

- The claim ADR 0068 could make becomes true: a job's command cannot reconfigure
  Jarvix, by any route this project knows of.
- ADR 0068's "does not protect against" list loses its largest item and keeps
  the rest — the network, signals, metadata via `stat`, and `chdir` are still
  open and still documented.
- Every escape test now runs against **both** walls, and each has a control that
  reaches the socket unconfined. A wall tested only from one side is a wall
  nobody has seen: without the control, the confined test would pass on a
  machine where the probe never started.
- The tests assert on the **daemon's own state** — the request was never
  handled, the setting still reads what the user left — and were verified by
  reverting the filter and watching them go red with
  `DRIVE-CONNECTED: {"result":{"ok":true}}`.
- **No new dependency.** Nine BPF instructions and one `prctl`.

## What remains open

- **The socket still has no authentication.** Any process of the same user that
  is *not* inside a job's confinement can drive the daemon exactly as it always
  could. That is the CLI and the window working, and it is also the limit of the
  claim — `TestTheSameCommandDrivesTheDaemonWhenItIsNotConfined` states it out
  loud rather than leaving it to be discovered. If a second confined surface
  ever appears that is not a job's command, this wall does not automatically
  cover it, and the per-run key is the design to reach for — with the QML work
  budgeted honestly as its own change.
- **Architectures other than amd64 and arm64 refuse to run commands**, because
  the filter needs an `AUDIT_ARCH` and a `socket(2)` number this ADR has not
  verified for them. Refusing is the failure direction this project takes
  everywhere else.
