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
