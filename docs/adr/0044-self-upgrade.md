# ADR 0044 — Self-upgrade: release slots, a doctor-run health gate, rollback by symlink flip

**Status:** accepted

## Context

Every Jarvix install so far has been updated by hand from an AI session:
pull, build, restart the daemon, restart the shell, verify by ear. Issue
#139 asks for a self-serve path — and for a daily-driver voice assistant
the dangerous half is not the update, it is the bad update. The 2026-08-25
ggml backend split (ADR-adjacent history in internal/doctor/probe.go)
proved that "it built" and "it can transcribe" are different facts, and a
build that passes the compiler can still brick the assistant with no way
back. Upgrade-with-rollback is therefore day-1 infrastructure: the previous
working version must survive every upgrade, and the decision "did the new
build work?" must be made by the machine, against the real engines, before
the user has to find out by speaking to a dead assistant.

Constraints carried in from earlier decisions: the user's checkout is the
build source and belongs to the user (never stash, reset, or checkout their
work); the Makefile is the single source of build flags, including the
`-X internal/build.Version` stamp; the doctor's probes (#113/#114) are the
one honest definition of "the voice loop works"; and `~/.config` and the
state directories are never migrated at install time — config compatibility
is a load-time concern (ADR 0015, ADR 0038 precedent).

## Decision

### Versioned release slots, live binaries as symlinks

Each build is installed whole into its own slot,
`~/.local/share/jarvix/releases/<version>/{jarvix,jarvixd}`, where
`<version>` is exactly the Makefile's stamp (`git describe --tags --always
--dirty` — one string names the commit, the slot, and what `jarvix status`
reports). `~/.local/bin/jarvix` and `~/.local/bin/jarvixd` become symlinks
into the live slot.

Symlink flip over copy-swap, deliberately. A flip is one `rename(2)` per
binary — the path is never absent and never half-written, where a copy has
a window in which `systemd` could respawn `jarvixd` from a torn file. It
also makes rollback free: restoring the previous version is the same flip
pointed at the previous slot, no copying on exactly the path that runs when
things are already going wrong. And it keeps N previous versions as plain
directories that `ls` can audit. The cost — binaries are one indirection
away — is invisible to systemd (`ExecStart=%h/.local/bin/jarvixd` resolves
through it) and to `$PATH`.

A pre-slot install (plain files from `make install`) is **adopted** on the
first upgrade: the running pair is copied into a slot named after the
installed version before anything is flipped, so a rollback target exists
from the very first use. Retention keeps two slots — live and previous —
and prunes older ones only after a green gate; a failed upgrade prunes
nothing.

### Build from the user's checkout, only when it is exactly "clean and behind"

The upgrade builds from the same checkout the user cloned (found through
the Omarchy plugin symlink, which already points at
`<checkout>/plugin/omarchy`, or by running the command from inside it).
Building from a temp clone was rejected: it would silently diverge from
what the user sees, redownload models of trust the checkout already
carries, and still leave the plugin symlink pointing at the old tree.

The checkout is inspected read-only (`fetch`, `status --porcelain`,
`rev-list --count` both ways, current branch), and the upgrade proceeds
only when it is on `main`, clean, and not ahead of `origin/main`. Anything
else refuses **loudly with the exact git state quoted** and no side
effects: dirty files are listed verbatim, a diverged branch reports its
commit count, an off-main checkout is named. The one mutation ever made is
`git merge --ff-only origin/main` — a pure catch-up, and the invocation of
`jarvix upgrade` is the consent for exactly that. The build itself is
`make build`: the Makefile stays the single source of flags, so the version
stamp cannot drift between hand installs and upgrades.

Up-to-dateness is judged by **version, not commit count**: after a
rollback the checkout is already at `origin/main` while the binaries are
old, and "0 commits behind" must still rebuild.

### The health gate is the doctor, run by the new binary

After flip and `systemctl --user restart jarvixd`, the upgrade waits for
the daemon socket (polling the installed CLI's `jarvix status`, 30s
budget; the answer must carry the expected version, or the restart did not
take), then execs **`jarvix doctor --gate`** — the freshly installed
binary running `doctor.GateChecks`: daemon answering on the socket,
protocol match, whisper really transcribing, the TTS engine really
synthesizing. Zero duplicated probe logic — the gate *is* the doctor's
check functions, and running them in the new binary means a deliberate
protocol bump is judged by the pair that must actually match, not by the
old orchestrating process. Fast environment checks (network, PipeWire,
keybindings) stay out of the gate on purpose: a network blip must not roll
back a good build. One `doctor --gate` run is bounded by a 2-minute
budget (two cold engine probes at 30s each, plus slack).

`protocol match` also joins the full `jarvix doctor` run: a torn install
is worth naming outside upgrades too.

### On any gate failure: flip back, restart, prove recovery

A red gate (daemon won't start, wrong version, probe FAIL, hung gate)
flips the symlinks back to the previous slot, restarts the daemon, and
**re-runs the same gate** to confirm recovery — the report quotes the
failing check verbatim (`[FAIL] whisper.cpp transcribes — …`), because the
probe already carries the engine's own words. The upgrade still exits
non-zero: a rolled-back upgrade did not succeed.

Rollback-of-rollback is refused: when no previous slot exists (nothing
adopted, a dangling symlink, or the rebuild landed in the slot that was
also the rollback target), a failed gate stops loudly, leaves the new
binaries in place, and deletes nothing — a possibly-broken install is
recoverable by hand, a deleted one is not.

### The shell's half: told, never forced

The plugin directory is symlinked from the checkout, so the QML on disk is
new the moment the pull lands — but the running shell keeps executing what
it loaded. When `git diff --name-only old new -- plugin/` is non-empty,
the report says a shell restart is pending and offers
`omarchy-restart-shell` — never the refresh variant, which reloads
configuration without tearing down the QML engine and would leave the old
plugin code running; daemon-only changes say so. The command is offered,
not run: a shell restart flickers every bar and panel, and when to take
that is the user's call.

### One at a time, nothing else touched

A lock file (`~/.local/state/jarvix/upgrade.lock`, `O_EXCL`, holding the
owner's pid) serialises invocations: a second upgrade refuses and names
the holder; a lock whose owner is dead is taken over once. The upgrade
never touches `~/.config` or the state directories — config migrations
remain load-time concerns of the new binary, per ADR 0015/0038.

The state machine, in one line:

```
lock → inspect (read-only) → refuse | check-report | ff-only pull
     → make build (fail: nothing installed) → stage slot → adopt if first
     → flip → restart → gate
     → green: prune to 2 slots, report (shell notice)
     │ red:   previous? flip back → restart → gate again → report verbatim
     │        no previous? loud stop, delete nothing
```

Everything external runs through one exec seam (`internal/upgrade.RunFunc`),
so the entire machine — including gate-fail→rollback→recovery — is tested
hermetically with scripted commands and temp directories; CI never builds,
installs, or restarts anything real.

## Consequences

- A bad build costs one gate run and a restart, not a dead assistant: the
  previous version is always on disk and the daemon ends every failed
  upgrade running on binaries the gate has re-proved.
- `~/.local/bin/jarvix{,d}` are now symlinks. `make install` still writes
  plain files over them, which simply returns the install to the pre-slot
  shape; the next upgrade adopts it again. The two paths coexist.
- The gate's honesty is bounded by the doctor's probes: a regression the
  probes cannot see (a broken tool, a wake-word regression) passes the
  gate. Extending the gate is extending the doctor — one place.
- An upgrade across a protocol bump briefly leaves the old CLI process
  unable to fully drive the new daemon; the gate handles this by delegating
  judgement to the newly installed CLI, and the user's next command runs
  the new binary anyway.
- The upgrade depends on the checkout remaining a real git clone on `main`.
  A tarball install has no upgrade path here — by design, since the release
  artifacts (`release.yml`) already serve that shape.
