# ADR 0026 — Named routines: one phrase places your apps on your workspaces

**Status:** accepted (implements issue #61)

## Context

Every mechanical piece of "start my usual apps on my workspaces" already
exists: spawning through the compositor seam with the probed dispatch dialect
(ADR 0022, #47/#48), workspace dispatch, the window inventory and matcher
(#36), deterministic phrase triggering (ADR 0017), and the permission gate
(ADR 0014). What does not exist is composition — a way to say "these apps, on
these workspaces, arranged like this" once, and then say one sentence every
morning instead of clicking for two minutes.

The composition raises questions the pieces did not: what the configuration
schema is, how placement is expressed, what identity the run has at the gate,
what happens when an app is already running or refuses to start, and whether
a routine may run arbitrary commands.

## Decision

**A routine is configuration: `[[routines]]` tables of name, phrases, and
ordered steps — and a step is a program name plus a placement, never a
command.** Each step carries `app` (one bare executable name or absolute
path, the same token rule the terminal intent enforces), a target
`workspace`, and optionally `float` with `size`/`position`, or `tile =
"master" | "split"`, plus a `match` override for apps whose window class is
not their binary name. The schema is deliberately flat — scalars and
two-element integer arrays — because the capture feature (#62) will write
these tables programmatically.

**No arbitrary-command steps, and that is a threat-model decision, not a
missing feature.** A "run this command" step would put a shell behind a
single spoken phrase and behind the capture tooling that writes these tables.
`[[intents.custom]]` already exists for phrase-triggered commands and pays
for it by facing the shell classifier per run. Routines instead launch
through `Compositor.Spawn` — the validated path ADR 0022 carved out for the
terminal intent — so the launched app is the compositor's child (session
environment, survives daemon restarts) and the name is bounded to one bare
token at config load and again at the seam. Revisit only with its own threat
model.

**Triggering is the intent router (ADR 0017), not the model.** Routine
phrases compile into the same grammar table as the built-ins, match whole
utterances exactly, and cost zero provider calls on a hit. Collisions — with
built-ins, custom intents, or other routines — fail configuration validation
naming both owners. A match carries only the routine's *name* into the
engine; nothing about steps travels through the router.

**Placement is four new compositor verbs, probed-dialect and set-shaped.**
`SetFloating`, `ResizeWindow`, `PositionWindow`, `PromoteMaster` join the
seam, rendered in whichever dispatch dialect the machine was probed for and
judged by the compositor's reply, like every other dispatch. Every verb is a
*set* rather than a toggle (`setfloating`/`settiled`, `exact` geometry), so a
re-run converges on the same layout instead of oscillating. `tile = "split"`
means "tiled into the workspace's split"; `"master"` additionally promotes
the window — which focuses it first, stated openly, because the legacy
dialect's layout message has no window selector. A run ends by switching to
the first step's workspace, so it finishes somewhere predictable.

**Already-running dedupe before every launch.** One inventory read per run;
each step first claims an existing window via the `desktop.focus_window`
matcher's identity logic, with two routine-specific adaptations: ties break
to the most recently focused window (nobody is there to answer "which
Firefox?"), and the category-alias tier does not apply (a step names a
program; letting the "editor" alias claim GoLand would place the wrong app
and then never launch the right one). Claimed addresses are excluded from
later steps, so two terminal steps get two terminals.

**Failure continues; the run speaks once.** A binary that will not start or
a window that never appears within a bounded, injected-clock wait is recorded
and stepped past; the remaining steps run, and the single summary names every
casualty ("Morning setup: three apps placed; slack did not start"). Progress
is `routine.started/step/finished` bus events for the bar and window — never
speech. The whole run honours the session context, so "stop" aborts it
mid-placement; a phrase spoken while a run is still placing is refused with
one line rather than interleaving two sequences.

**Gate identity `routine.run`, default allow.** Every step is authored by the
user in their own configuration and the phrase is itself the instruction —
asking "should I run your morning setup?" after "morning setup" would be
asking the user to confirm their own sentence. It is its own identity (not
`intent.run`'s) because the risk profiles differ and each must be tightenable
alone: `[tools.policy.tool]."routine.run" = "ask"` demands a confirmation
through the one shared mechanism, `"deny"` disables routines outright.

**One trigger path.** `jarvix routines run` and the window's Run button call
`routines.run`, which starts a session and submits the routine's first
phrase — so the router, the gate, the refusal, and the summary behave
identically however a routine is invoked. Routines are listed read-only
(`routines.list`); the tables stay hand-edited TOML like `[[intents.custom]]`
(ADR 0015's rewrite preserves them byte-for-byte).

## Consequences

- One sentence replaces the morning clicking, deterministically and offline;
  re-running is safe and converges.
- The compositor interface grows four verbs; both implementations (Hyprland,
  fake) carry them, and a future compositor must too.
- The master arrangement briefly moves focus on both dialects — visible, but
  identical across machines, which matters more.
- Waits are per-step bounded (default 8s), so a routine of dead apps ends in
  seconds-per-casualty, never a wedged session; the injected clock keeps
  every wait deterministic under test.
- #62 (capture by demonstration) needs only to emit `[[routines]]` tables:
  the schema is flat, `config.RoutineDefinitions` + `routine.Problems` are
  the reusable parser/validator, and validation messages name tables by
  index.

## Addendum: capture by demonstration (issue #62)

Capture closes the authoring loop this ADR left open: arrange the desktop,
say "save this as my morning setup", and the `[[routines]]` entry above is
written rather than typed. The decisions that shaped it, each an extension
of a decision already on this page rather than a new direction:

**The trigger is the router's one free-text slot, and literal phrases always
beat it.** "save this as {name}" compiles into the grammar table with a
trailing name slot (one to six words) — the only free text the router will
ever carry, trailing-only so where the name stops is never ambiguous. The
capture rules compile last, and rules are tried in insertion order, so a
built-in, custom-intent, or routine phrase that happens to begin with the
same words keeps its meaning; the slot claims only utterances no literal
phrase owns. `{name}` stays unusable in `[[intents.custom]]` and routine
phrases, where free text would reach a shell or parameterise fixed steps.

**Derivation shares the dedupe matcher's identity logic; it never guesses.**
The launch command is the class collapsed the way spoken summaries collapse
it (`desktop.AppName`, plus the one widespread `-desktop` packaging
convention), and a candidate is only written if it actually resolves on
PATH. When class ≠ command, the step records `match` so this ADR's dedupe
finds the running window on every re-run. An underivable command is a saved
partial capture, never a drop: placeholder `app = "CHANGE-ME"` with `match`
on the class and a `# TODO:` naming it, one spoken line saying which app
needs a hand, and an `incomplete` mark on every listing surface until a
human resolves it.

**Exclusion is a documented rule set, counted honestly.** The Jarvix/shell
surfaces, windows that accept no input (transients), classless windows, and
special workspaces are excluded; the spoken confirmation counts only what
was kept. Tiled windows are captured as `tile = "split"` — the inventory
cannot say which window is the master, and promoting the wrong one every
morning is worse than a one-word hand edit.

**Writes go through ADR 0015's contract, extended to array-of-tables.** The
settings editor addresses dotted keys, which every `[[routines]]` block
shares, so capture gets its own surgical writer: blocks addressed by
position (the parser decides which block is which), a provenance comment
("captured 2026-08-21") above the entry that a replace refreshes rather than
stacks, per-step TODO comments, and the same guard — the result must parse
and the whole routines list must read back as exactly the intended edit, or
nothing is written. Writes are atomic; a failure leaves the file
byte-identical. Hand-written comments elsewhere survive both this write and
every later `config.set`.

**Replacing an existing name goes through the ADR 0014 exchange, and the
approval is never remembered.** A misheard phrase must not clobber a curated
routine; each replace is its own question. Commit re-reads the file after
the question — an entry that appeared during the thirty-second window is
refused, because nobody was asked about it. Replacing keeps the old entry's
curated phrases; the steps are replaced wholesale (diffing is out of scope).

**The captured routine is immediately runnable via a deferred reload.** The
engine cannot swap its router under the session that spoke the capture, so
the daemon's config catches up at commit (routines.list, routines.run name
lookup) and the session watcher rebuilds the engine on that session's
`session.finished` — announced as `config.changed`. This also fixed a
latent gap: a reload after a *hand-edit* to `[[routines]]` or
`[[intents.custom]]` now rebuilds the router too (the structured tables are
compared directly, since they have no settings-registry entry).

**Capture is read-only against the desktop by construction.** The snapshot
is a pure function of one inventory read; the service calls no compositor
verb but `Windows`. The inventory grew geometry (`at`/`size`) for it — the
one addition to the seam.
