# ADR 0045 — Backup and restore: one archive, a held write barrier, a staged swap

**Status:** accepted (implements issue #140)

## Context

Jarvix now holds a relationship's worth of personal state — remembered facts
(ADR 0025), taught vocabulary (ADR 0042), focus threads (ADR 0041),
conversations (ADR 0027), rolling history (ADR 0011), routines, scripts,
feeds, settings — across `~/.config/jarvix` and `~/.local/state/jarvix`, with
no unified export. A disk failure or a botched hand-edit loses the
assistant's memory of its user. Issue #140 asks for `jarvix backup` (one
dated archive with a manifest) and `jarvix restore` (validated, atomic,
reversible), cron-able, proven by a fresh-machine socket smoke test.

Three decisions had defensible alternatives; this record keeps them from
being relitigated.

## Decision

**Discovery is wholesale: the two roots, never a store list.** The archive
is whatever `filepath.WalkDir` finds under the config root and the state
root — the two directories `config.Paths` promises every Jarvix file lives
under. There is deliberately no list of store filenames anywhere in
`internal/backup`: the wave before this one added three stores, and an
enumerated list would have silently dropped each of them until someone
noticed. A store this build has never heard of is archived and restored
verbatim, and a test pins exactly that. The only exclusions are wholesale
rules, not names: dot-prefixed files (every store's atomic-write scratch
files, `.memory-*.tmp` and kin — Jarvix writes no real dotfiles under its
roots) and non-regular files (a symlink is recorded in the report and left
behind, because following one could reach `~/.ssh`, and the archive must
contain nothing outside Jarvix's own dirs — manifest-pinned, with a test
hunting planted key material in the archive bytes). The data root (Whisper
models — gigabytes, re-downloadable with `jarvix setup whisper`) and the
runtime dir (a socket) are not personal state and are not archived.

**Consistency is a held write barrier, not a flush and not a daemon-side
copy.** Every store already writes with the ADR 0011 discipline — temp
file, fsync, rename — so no single file is ever torn on disk and a stopped
daemon can be backed up by plain reads. What renames cannot promise is
coherence *across* files: `history.json` naming a conversation whose
transcript is mid-append, a metadata file one turn ahead of its `.jsonl`
(the transcript is the one append-in-place file in the state root, and a
conversation commit is a three-file mutation: turns, metadata, active
pointer). So the daemon carries one `statehold.Gate`, threaded through
every store it writes — memory, vocabulary, focus, feeds, the automation
trail, history, conversations — each entering it for the duration of one
disk mutation. `jarvix backup` asks the daemon to hold the gate (the
`state.hold` verb): in-flight writes drain first (bounded — a wedged disk
fails the backup with a reason, never hangs the daemon), new ones block
until `state.release`, and a TTL (default 10s, max 60s) reopens the gate on
its own if the backup process dies mid-copy — writes are *delayed, never
lost*, and the daemon can never be wedged by a dead client. The CLI reads
every file under the hold and releases before compressing, so the hold
lasts milliseconds. Alternatives rejected: a daemon-side snapshot verb
(moves file I/O into the daemon and duplicates the CLI's direct-copy path
for the stopped case — two implementations of one archive); quiescing only
the session tail (misses the automation scheduler and feed fetches); and
holding nothing on the grounds that renames are atomic (true per file,
false per archive). Config writes are deliberately not gated: `config.toml`
is a single atomically-renamed file the user may also save from an editor
at any moment — no gate can make it more coherent than one complete
version, which a plain read already gets. A hold is a coherent *cut*, not a
flush: tail work that has not started writing simply lands after release.

**Restore is validate-everything-then-swap; the old state steps aside,
never dies.** The archive is extracted into staging siblings of the real
roots (`<root>.restore-stage-<ts>` — same filesystem, so the final move is
a rename). Refusal comes before contact, with the specific reason: not
gzip, truncated tar, no manifest, unreadable manifest, archive format newer
than this build, manifest and contents disagreeing in either direction, a
hash mismatch, an unsafe path (absolute, `..`, outside `config/`/`state/`),
a symlink entry, a running daemon (whose in-memory stores would rewrite the
restored files on their next save), or a staged store that fails to load.
That last check is the schema gate: the store's own loader — the exact code
the daemon will run — speaks the refusal ("memory store version 99 is not
supported"), so newer-schema archives are caught without `internal/backup`
maintaining a second copy of anyone's version knowledge; files no loader
claims are held to well-formedness wholesale (.toml parses, .json parses)
and otherwise restored verbatim. Only after everything passes do the roots
swap: each existing root is renamed whole to `<root>.pre-restore-<ts>` (the
safety copy the report names — recovery is one `mv`), the staged root is
renamed into place, and a failure mid-swap rolls back. There is no
rm-then-copy anywhere. The result is proven twice: the same load validation
re-runs against the real roots, and CI's smoke test boots a daemon against
restored temp roots and asks it — over the socket, as the CLI would — for
the memory, vocabulary, threads, conversations, and config it should hold.
A committed fixture archive pins the format: if a change to Create or
Restore strands archives users already have, that test fails.

**Secrets ship by default; `--no-secrets` redacts by line.** Api keys live
in `config.toml` (`[ai.<name>] api_key`), and a backup that silently
dropped them would not restore a working machine — so they are included,
and the archive is written 0600 like the stores it carries. `--no-secrets`
replaces each `api_key` value with a fixed placeholder by line rewrite (the
rest of the file byte-identical), records the table-qualified key names in
the manifest, and restore warns exactly which keys need re-entering.
`api_key_env` values are env-var *names*, not secrets, and are untouched.

## Consequences

- `jarvix backup [path] [--no-secrets] [--quiet]` and
  `jarvix restore <archive> [--quiet]`, with stable exit codes for cron
  (0 success, 1 any failure, 2 unknown command) and a recommended crontab
  line in the README.
- Two new IPC verbs (`state.hold`, `state.release`) documented in
  docs/ipc.md; one hold at a time, releasable from any connection.
- Every daemon-owned store's options carry an optional
  `*statehold.Gate`; a nil gate never blocks, so CLI and test
  constructions are unchanged. A future store should thread the daemon's
  gate through the same way — and even if its author forgets, the archive
  still carries the store (wholesale discovery); only cross-file coherence
  with other stores would be weaker.
- The manifest records jarvix version, capture mode (`daemon-held` /
  `direct`), redaction, per-file SHA-256 + size + mode, and per-store
  schema markers discovered wholesale (any `.toml`/`.json` with a
  top-level integer `version`).
- Restore requires the daemon stopped; the README documents the
  stop → restore → doctor → start sequence.
