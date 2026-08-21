# ADR 0015 — Settings apply through daemon-side config IPC, with reload classes and a surgical TOML rewrite

**Status:** accepted

## Context

Issue #9 wants settings viewable and editable from the Jarvix window,
applied without a restart. Three problems have to be owned somewhere: how a
change is validated and written into `~/.config/jarvix/config.toml` without
destroying hand edits, how the running daemon picks the change up safely,
and how a QML screen stays testable when QML cannot be integration-tested
here (ADR 0013).

## Decision

**1. All settings intelligence lives in the daemon**, behind four IPC
methods: `config.get` (running values + reload class per field, file
fingerprint, secret *presence*), `config.set` (validate → rewrite file →
apply), `config.reload` (re-read the file), and `doctor.get` (the fast
offline readiness subset). The settings screen and the
`jarvix config get/set/reload` CLI are thin clients of the same surface, so
every behaviour is covered by hermetic Go tests over a real socket.

**2. The file is rewritten surgically, not re-serialised.** A single-key
editor scans the TOML document (tables, multi-line strings, multi-line
arrays), replaces only the changed key's value, and preserves everything
else byte-for-byte — comments, unknown keys, custom endpoint tables,
ordering. Alternatives rejected: a *managed section* cannot legally repeat
tables the user already defined (TOML forbids duplicate table headers), and
re-encoding the whole config destroys comments. The editor is backstopped by
a guard: the result must re-parse and every change must read back, otherwise
nothing is written — a scanner bug can cost a save, never the user's file.
Writes are atomic (same-directory temp + rename, 0600). External edits are
detected by fingerprint (sha256 taken at `config.get`, checked at
`config.set`): a mismatch is a structured error telling the client to
re-read and reapply, never a silent clobber.

**3. Every setting has a declared reload class**, kept in the settings
registry (`internal/config/settings.go`) and enforced by the daemon:

- **live** (`ui.*`): flag reads guarded by a mutex; effective immediately.
- **idle** (`ai.*`, `tts.*`, `stt.whisper.*`, `conversation.*`, `audio.*`):
  the daemon rebuilds the affected adapters and swaps them into the engine
  via `Engine.Reconfigure`, which refuses while a session is active —
  adapters are swapped between sessions, never underneath one. Because
  finished sessions' goroutines can still be draining after the state goes
  idle, `Reconfigure` briefly blocks new sessions and waits for the tracked
  goroutines to exit before swapping — race-clean by construction.
- **restart** (`activation.ptt_chord`, `tools.*`, `artifacts.*`,
  `log.level`): these are wired at daemon boot (chord watcher, tool
  registry, artifact tool, logger); the file is written and the response
  names the keys still waiting on `systemctl --user restart jarvixd`.

A reload that fails validation keeps the running configuration; the daemon
never boots into or hot-swaps to a broken state. `config.get` reports the
*running* values, so a restart-pending key honestly shows the old value.

## Consequences

- The window stays display-only: `JarvixSettings.qml` renders fields and
  readiness, submits changes, and maps the structured errors (`-32001`
  validation, `-32002` conflict, `-32003` busy) to inline text.
- Injected test collaborators survive reloads (`fillDeps` only rebuilds what
  came from config), so daemon tests observe reloads through fakes.
- The rewrite normalises the changed value's formatting (a multi-line array
  collapses to one line); everything else is untouched. Accepted trade for
  never losing a comment.
- Restart-class settings changed via the screen take two steps (save, then
  restart). The response and the screen say so explicitly rather than
  pretending otherwise.
