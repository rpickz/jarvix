# ADR 0040 — Window nicknames are session-scoped, resolved first, released by revalidation

**Status:** accepted (implements issue #126)

## Context

Referring to windows by app or title is the weakest link in voice window
control: titles are long, app names mishear ("chromium" → "premium"), and two
windows of one app are indistinguishable by speech. Issue #126 adds
user-chosen nicknames — "call this window builds" — that every surface
accepting a window reference resolves.

Three policy questions had answers that were not obvious, and this record
keeps them from being relitigated by accident.

## Decision

**One resolution seam.** The nickname registry (`desktop.Nicknames`) lives
behind the window tools' shared state, and every consumer of a spoken window
reference — the five window verbs, typing's target resolution, the daemon's
`windows.*` verbs, and (next) focus-thread anchors (#123) and session recaps
(#124) via `tools.Desktop.ResolveReference` — goes through the one
`resolveWindow` path. There is no per-tool nickname code to drift.

**Nicknames resolve before fuzzy matching.** A nickname outranks every
matching tier, including an exact application name. The user picked the name
precisely to stop depending on what apps and titles happen to say, so a title
containing the same word must never outbid it. Deictic words ("this",
"current") sit above nicknames only because no nickname can be one: the
matcher's vocabulary — deictic words, stop words, category words — is
reserved at assignment, and a name that is verbatim an intent phrase is
refused naming the owner (`intent.Router.Owner`, the same collision map
config loads are judged by). Common English words are a spoken warning, not a
refusal: precedence is deterministic either way, and the name is the user's
to choose.

**Session-scoped, in-memory, no persistence.** Windows are ephemeral: a name
that outlived the daemon would sooner or later point at nothing — or at a
different window wearing a recycled address, which is worse. So nicknames die
with the daemon, deliberately, and there is no schema, file, or migration.
*Recorded for later:* if demand appears, the sticky version of this feature
is **rules, not bindings** — persist "windows whose class is X get called Y"
and re-bind at session start — never persisted address bindings. That is a
separate decision for a separate day (tracked in issue #126's thread).

**Release is lazy revalidation, not an event subscription.** Every registry
operation is handed the live inventory and drops names whose window
(address + stable id + class, the same identity the verbs verify before
acting) is no longer in it. A closed window's name therefore cannot resolve
no matter how the close happened — compositor kill, crash, `close_window` —
and there is no event stream to fall behind on. A released name is remembered
by name alone, so "focus builds" after the close gets the honest "nothing is
called builds right now" rather than "no window matches", and reassignment
frees the record.

## Consequences

- "Focus builds" is deterministic the day after "call this window builds",
  pinned by `TestNicknameOutranksEveryMatchingTier`.
- A daemon restart forgets every nickname; the listing surfaces ("what are my
  windows called", `jarvix windows`) make the current state cheap to see.
- Assignment is allow-tier everywhere (spoken intent, `desktop.name_window`,
  `windows.name`): it changes nothing on screen and the opposite assignment
  undoes it, and the spoken name is itself the authorisation.
- The registry consults the *current* intent router through a holder the
  daemon updates on config reload, so a nickname refused as "already a
  routine trigger" and the routine that owns the phrase always come from the
  same configuration read.
