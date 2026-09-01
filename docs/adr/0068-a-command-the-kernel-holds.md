# ADR 0068 — A command the kernel holds: confining a job's shell instead of reading it

**Status:** accepted

## Context

ADR 0065 gave a job a scope and enforced it daemon-side: before anything is
dispatched, `jobs.Scope.Judge` runs against an `Attempt` the daemon read out of
the proposed tool call. For every tool whose effect is one named file that
works. For `shell.run` it does not, and #200 said so plainly — the daemon has no
way to name what a command would touch, so a job proposing one parks.

That refusal was correct, and it is worth restating why, because everything
below is an attempt to keep it correct while making a job useful.

**A command's filesystem subject cannot be recovered from its text.** Quoting,
variable expansion, `$(…)`, relative paths, `cd`, and symlinks each defeat a
reader on their own; together they defeat any reader anybody will write. A check
that is right most of the time is *worse* than no check, because it will be
trusted — the boundary would be believed exactly as much as if it were real, and
would fail exactly where an adversarial or merely careless plan pushed on it.

But the refusal left jobs unable to do the things #200 itself gave as examples.
"Get the CI green", "tidy my downloads", "set up the new laptop" are all
commands. A job could remember facts, set reminders, write artifacts and move
windows; everything else parked. #222 is the ticket that says this gap is a
design question rather than an oversight, and names the answer to avoid: a
free-text command with a parsed subject is not acceptable under any
circumstances.

## Decision

**Stop predicting what a command will touch. Confine it, before `exec`, in the
kernel — and refuse to run it at all on a machine where that confinement cannot
be established.**

Concretely, in `internal/confine`:

- A job's command is run by **re-executing `jarvixd` itself**. The second copy
  reads a plan from its environment, applies a **Landlock** ruleset to its own
  (OS-locked) thread, confirms it, and `execve`s into `bash -c <command>`. The
  Landlock domain is inherited by everything the command goes on to start, and
  nothing the command does can widen it.
- The ruleset grants **read/write on the scope's roots**, **read+execute on a
  short, fixed system base**, **read/write on five character devices**, and
  **read/write on the command's own `/proc/<pid>` directory** — and nothing
  else. Everything outside that, including the whole of the user's home
  directory, is refused by the kernel.
- If the boundary cannot be built, **the step is refused and nothing runs**. The
  job parks on a new reason, `jobs.WhyUnconfined`, with the sentence that says
  what was measured.

`internal/daemon`'s `jobActor.Subject` therefore reads *nothing* out of a
command. It establishes the precondition and returns an `Attempt` with **no
paths at all**; `Scope.Judge` is left with the question it can still answer,
which is whether the job was given `shell.run` in the first place. The
filesystem half of the check has moved from a parser to the kernel.

**There is deliberately no function anywhere in this daemon that takes a command
string and returns paths, and there must not be one.**

### Why Landlock, and not `bwrap`

A mount namespace would be stronger: a path that does not exist in the child's
namespace cannot be named at all, which would close the IPC gap described below.
It was measured on the development machine — unprivileged user namespaces are
enabled and `bwrap` is installed — and rejected for this slice:

- **Availability is the point of the exercise.** Ubuntu 24.04 restricts
  unprivileged user namespaces through AppArmor by default; GitHub's runner
  images relax it, but that is a property of an image rather than of the
  platform. A mechanism whose tests skip in CI is a mechanism nobody is holding
  to account, and #222 is explicit that a green suite which never exercised the
  wall is the worst possible outcome here. Landlock needs no privilege, no
  namespace, and no daemon; it is present on every kernel from 5.13 with the LSM
  enabled, and was confirmed present on both the development machine and the CI
  runner class.
- **Landlock's failure mode is legible.** `landlock_create_ruleset` reports an
  ABI version. There is one syscall that answers "can this machine hold the
  boundary", which is exactly what a design built around refusing rather than
  degrading needs.

A namespace remains the right way to close the remaining gap, and this ADR does
not pretend otherwise. It is named as follow-up, not as something already done.

### The ABI floor is 3, and that is a guarantee rather than a convenience

Measured, not assumed. The probe asks the kernel:

| ABI | Kernel | What it adds | Why it matters here |
| --- | --- | --- | --- |
| 1 | 5.13 | filesystem rights | no `REFER`: every cross-directory `mv` fails, *including inside the scope* |
| 2 | 5.19 | `LANDLOCK_ACCESS_FS_REFER` | rename inside the scope works; rename out of it does not |
| 3 | 6.2 | `LANDLOCK_ACCESS_FS_TRUNCATE` | **the floor** — see below |
| 4 | 6.7 | TCP bind/connect | not handled; see "what this does not protect against" |
| 5 | 6.10 | `IOCTL_DEV` | not handled |
| 6 | 6.12 | scoping (abstract unix sockets, signals) | not used; see below |
| 9 | 7.x | — | what the development machine reports |

The floor is 3 because of `truncate(2)`. It takes a **path**, not a descriptor,
so on ABI 1 and 2 a command that could neither open nor write a file outside the
scope could still **empty** one. That is a modification of a file outside the
boundary, which is precisely what the caller is being told cannot happen. Rather
than confine to a boundary with a hole in it and then describe the hole
carefully enough that nobody notices, ABI 1 and 2 are refused with the reason
said out loud.

`REFER` is *handled and granted on the roots*, which reads backwards until you
know the rule: if `REFER` is not handled, the kernel denies every rename and
hard link across directories, including ones wholly inside the scope. Leaving it
out would break the ordinary work while adding nothing, and a boundary that
breaks the ordinary work is a boundary people route around.

**Measured on the development machine:** kernel 7.1.8, `/sys/kernel/security/lsm`
= `capability,landlock,lockdown,yama,bpf`, `landlock_create_ruleset(NULL, 0,
LANDLOCK_CREATE_RULESET_VERSION)` returns **9**.

### Landlock cannot subtract, so Jarvix's own directories are a refusal

This was measured rather than reasoned about, and the measurement changed the
design.

A rule granting read/write on a directory and a second rule granting *less* on a
directory beneath it does **not** narrow the second. Landlock's rights are a
**union up the tree**, not a longest-prefix match: a write granted on `~` still
applies under `~/.config/jarvix`. Adding a rule with `allowed_access = 0` — the
obvious way to express a hole — is rejected outright with `ENOMSG`.

So there is no ruleset meaning "all of `~` except `~/.config/jarvix`". A job
scoped to a directory that contains Jarvix's own configuration therefore
**cannot** be confined in a way that keeps #109's wall standing: a command in
there could rewrite `[tools]`, `[advisors]` or `[ai]` directly on disk, which is
the place a job is structurally forbidden to reach through tools, arrived at
through a different door.

`Spec.Check` refuses such a scope and says which directory made it impossible,
which also points at the fix — a narrower boundary. The test is one-directional
on purpose: a root *inside* the state directory (the artifacts folder, say) is
an ordinary job, because from in there `config.toml` is outside the root like
anything else.

An alternative was considered and rejected: enumerate the root's children and
grant each except the excluded one. It is defeated by `rmdir` + `mkdir` — the
exclusion is pinned to an inode, and the parent grants permission to replace it.
A boundary that a `rm -rf` can step around is not one worth having.

### The system base, and what it deliberately leaves out

A confined command needs to be able to run at all: `bash` cannot be executed if
`/usr/bin/bash` is unreadable, and cannot start if its shared libraries are not.
The base is `/usr`, `/bin`, `/sbin`, `/lib`, `/lib64`, `/opt`, `/etc`, granted
**read and execute only**, and a test asserts that nothing on it lies under the
user's home or under `/proc`, `/run`, `/var`, `/tmp`, `/sys`, `/home` or
`/root`.

`/etc` is the one judgement call. It holds the machine's configuration — the
linker cache, the timezone, the CA bundle — and none of the user's files; a
command that reads it learns nothing it could not learn from any other program
the user runs. It is granted read-only.

**`/proc` is not granted, and that is load-bearing.** `jarvixd` reads the user's
model API keys out of its *own environment* (`config.Endpoint.Key` calls
`os.Getenv`), and `/proc/<pid>/environ` is readable by the same user. Granting
procfs would hand a confined command the exact credentials this design is meant
to keep away from it. The cost is that `/dev/fd`, `/dev/stdout` and `<(…)`
process substitution stop working — which is why the *helper*, not the parent,
adds a rule for `/proc/<its own pid>` immediately before `execve`. `execve`
keeps the pid, so that rule names the command's own process directory and cannot
name another one.

`/tmp` is not granted either. Instead each command gets a fresh private
directory, granted like a root, pointed at by `TMPDIR`, and removed afterwards.
Two jobs cannot see each other's working files and nothing survives the command
that made it.

### Environment hygiene

The child's environment is built **from nothing**, not filtered. It gets `HOME`
(pointed at the job's first root, so a program looking for `~/.gitconfig` finds
a reachable absence rather than generating a permission warning on every run),
`TMPDIR`, a fixed `PATH` naming exactly the directories the base grants, and the
locale and timezone variables if the daemon has them. Nothing else — so the
API-key variables are not merely unreadable, they are not present, and neither
are their names. `PR_SET_NO_NEW_PRIVS` is set before the ruleset, so the command
cannot execute its way out through a setuid binary either.

### Confirmed, not assumed

The helper writes one byte down a pipe the instant `landlock_restrict_self`
returns, then marks that descriptor **close-on-exec** rather than closing it. So
the pipe carries two facts, not one:

| what the parent reads | what it means |
| --- | --- |
| nothing | the boundary was never established, and nothing was started |
| `k` | the boundary held and the command became that process |
| `kx` | the boundary held and the command never started — the helper was still alive to say so |

Only the middle case may be described as "it ran". Reading only "did a byte
arrive?" would report a command that could not be exec'd as one that ran and
produced nothing, which is #71's sentence with a shell behind it. `Outcome.Confined`
is therefore an observation of the pipe rather than the absence of an error, and
both other cases become a refusal the caller can show the user.

### What did not change

- **The permission gate.** Confinement is an *additional* wall. An irreversible
  command still asks; the job still parks on the question and resumes at exactly
  that step; the gate is still consulted on resumption (#225). A step whose
  boundary can be held perfectly well still parks on the gate, and a test pins
  it — "it cannot reach outside the scope" and "the user agreed to this" are
  different facts, and only the second is an approval.
- **`Forbidden`.** `script.run`, `intent.run`, the config writers and
  `advisor.ask` remain refused to every scope.
- **The ledger's order.** The dispatch is written before the action, so a killed
  or crashed child leaves the step *unverified* rather than done.
- **A session's own `shell.run`.** Outside a job it is the user's own authority,
  exercised while they are present. Nothing here narrows it.

One thing did change in the ledger, and it is a correction rather than an
addition: a command that ran and exited non-zero is now recorded as a **failed**
step. `shell.run` reports a command's failure in its result rather than as an
error — correctly, because a command that exited 3 is information for the model
and not a fault in the tool — but a job's ledger reads that result to decide
whether the step happened, and a step recorded as done is one the report will
claim, under the model's own label for it. Without the correction, a command the
kernel refused at the boundary would come back as "I did tidy the folder".

### Fail-closed by construction

The boundary rides the context, next to the job id the account already uses. On
its own that would be fail-*open*: forget to install it and the command runs the
old way. So the shell tool asks a second question. A call that carries a **job
id** and **no boundary** is refused outright, with nothing executed. The failure
mode of forgetting is therefore a command that does not run, never one that runs
unheld, and `TestAJobsCommandWithNoBoundaryIsRefused` is that rule's test.

The boundary is installed on *every* step a job takes, not only the
command-shaped ones, so a tool that grows a shell out of it later cannot inherit
an unconfined path by omission.

## What this does not protect against

Stated here rather than discovered later. Each was measured on the development
machine unless marked otherwise.

- **~~Unix sockets, including Jarvix's own.~~** *Measured, and then closed by
  [ADR 0069](0069-the-socket-a-confined-command-cannot-reach.md).* Landlock
  defines no right covering `connect(2)` to an `AF_UNIX` path, and a confined
  command connected to a listener outside its roots with the socket's directory
  not granted. `$XDG_RUNTIME_DIR/jarvix.sock` has no authentication beyond the
  file mode, so a command that deliberately spoke the IPC protocol could drive
  the daemon — reaching `config.set` and rewriting the very policy that bounds
  it. **This was the largest known hole in this boundary, and it was a hole
  through the guarantee rather than a narrowing of it**, so it did not stay
  documented. A confined command now has no unix sockets at all: a seccomp
  filter denying `socket(AF_UNIX, …)` is installed beside the Landlock ruleset,
  on the same thread, refused on the same terms. The remaining entries below
  stand.
- **The network.** *Measured: open.* Deliberately not handled. Landlock's
  network rights (ABI 4) cover TCP `bind` and `connect` only, so UDP, DNS and
  raw sockets would remain open and "this command cannot reach the network"
  would be a false sentence. #222 puts network confinement out of scope; a
  boundary described more widely than it holds is the failure this whole design
  exists to avoid, so it is not described at all.
- **Signals and other processes.** A confined command can signal the user's
  other processes, `jarvixd` included. ABI 6's `LANDLOCK_SCOPE_SIGNAL` would
  narrow this, but only on kernels at 6.12 and above — and a guarantee that is
  sometimes wider is a guarantee nobody can state. One floor, one sentence.
- **Metadata outside the roots.** *Measured.* `stat` and `test -e` still
  succeed on paths outside the boundary: Landlock governs access, not
  existence. A command can learn that `~/.ssh/id_ed25519` exists and how big it
  is. It cannot read a byte of it, and it cannot list the directory.
- **Where the process stands.** `chdir` out of the scope succeeds — Landlock
  restricts what a process may touch, not where it may stand. This surprises
  people reading the design, so it has a test of its own; the write from that
  directory is what fails.
- **Anything the scope actually admits.** A job scoped to `~/code` can delete
  `~/code`. Confinement narrows *reach*; it says nothing about *judgement*, and
  judgement is the gate's job.
- **The daemon's socket, from anything that is not a confined command.** The
  socket still has no authentication: any process of the same user outside a
  job's confinement drives it exactly as it always could. That is the CLI and
  the window working, and it is the limit of ADR 0069's claim.
- **Kernels below ABI 3.** They do not get a weaker boundary. They get a
  refusal.
- **What a command did.** The account records that a command ran, verbatim, and
  offers no undo. Jarvix has no idea what a command did and an offer it could
  not keep would be worse than no offer at all.

## Consequences

- A job can run commands inside its scope, which is what makes "tidy my
  downloads" a direction a job can take rather than one it parks on
  immediately.
- `jobs.Actor` gained the scope on `Subject` and `Do`. Explicitly, rather than
  on the context: the compiler is then what notices a future `Actor` that
  forgot, and a security parameter that can be forgotten silently is one that
  will be.
- A new parking reason, `WhyUnconfined`. It is not answerable — a "yes" cannot
  supply a kernel — and its sentence points at a narrower scope or a different
  machine rather than at a different step.
- `jarvixd` re-executes itself. `confine.Reexec` is the first thing `main` does,
  before flags, configuration or logging, and any test package that runs a
  confined command needs the same three lines in its `TestMain`.
- **No new dependency.** The three Landlock syscalls are made by hand through
  `syscall.Syscall`; a library for forty lines would be a supply-chain cost
  bought with a convenience. The same is true of the seccomp filter ADR 0069
  adds beside them.
- The escape tests assert on the **file outside**, read back after the command,
  rather than on the error. A command can fail for a dozen reasons unrelated to
  the wall, and only the file distinguishes "the kernel refused it" from "it did
  not happen to work today". Where the kernel cannot hold the boundary the tests
  skip **loudly**, naming the ABI and saying in plain words that they proved
  nothing.
