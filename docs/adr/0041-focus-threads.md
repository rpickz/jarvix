# ADR 0041 — Focus threads: one model for monotasking and multi-tasking

**Status:** accepted

## Context

The user works in both modes: deep monotask stretches, and genuine
multi-tasking across two or three fronts — typically a couple of windows and
an AI session. What is expensive (especially with ADHD) is not the work but
the switching: losing track of where each front stands, and paying a full
re-orientation cost on every return. Jarvix already holds the raw materials —
window awareness (ADR 0022), schedules (ADR 0032), a curated store discipline
(ADR 0025), a voice — and #123 asks for them to be composed into **focus
threads**: named pieces of work with cheap switching, instant recaps, parked
thoughts, cross-thread status, timeboxed focus sessions, and per-thread
check-in reminders.

Three prior decisions constrain the design. ADR 0013: every surface is
display-only, so anything spoken or shown must be composed daemon-side. ADR
0017: fixed phrases with one right outcome belong to the deterministic
router, not the model. ADR 0032: anything that runs on a clock in this
daemon uses tracked goroutines, injected time, and an adopt-never-refire
stance for moments that fell while no daemon ran.

## Decision

### One model: a timebox is a thread holding the floor

There is no separate "monotask mode". A **thread** is `{name, optional
anchors (≤2 windows), parked thoughts, last-switched / last-activity times,
optional check-in interval}`; exactly one thread may be **active**; and a
**focus session** is one thread holding the floor for a fixed number of
minutes. "Focus on X for 25 minutes" is therefore a switch plus a countdown —
everything else (parking, recaps, status) behaves identically inside and
outside a timebox, which is what keeps the feature one vocabulary instead of
two.

### The store: the memory book's contract, over threads

`~/.local/state/jarvix/focus.toml` — one hand-editable TOML document under
the XDG state dir, 0600 in a 0700 directory, with the ADR 0025 storage
discipline applied verbatim:

- **Atomic, durable writes**: temp file, fsync, rename, fsync the directory
  (ADR 0011).
- **A stat-based change detector**: every operation begins with one stat(2);
  a hand-edit lands on the very next operation, no watcher, no restart.
- **Normalize-repair**: missing or duplicate ids get fresh ones, missing
  timestamps become now, anchors past two are trimmed, an active pointer at
  a vanished thread clears, a session on an unknown thread ends. Repair
  never fabricates: a nameless thread or an empty parked thought is dropped.
- **A corrupt latch**: an unparseable file is a warning plus an empty store,
  and the first write moves it aside (`focus.toml.corrupt`) rather than
  overwriting a file the user may be mid-way through fixing. An unknown key
  is treated as corruption, because silently dropping a typo'd key's value
  would look exactly like Jarvix forgetting.
- **Ids ratchet**: `t<n>` / `p<n>` high-water marks are persisted and only
  ever move up, so a conversation that named a thread can never come to
  describe different work — the memory book's never-reuse rule.
- **Mutation checks**: an operation commits its state to memory only after
  the write succeeded, so a failed disk write costs exactly nothing.

TOML rather than JSONL for the book's reason: the user owns this file, and it
is small, curated, and rewritten whole — renaming a thread in an editor is a
first-class operation.

The live focus session persists in the same document. A daemon restart
mid-timebox resumes the countdown; one whose timebox blew while the daemon
was off closes it quietly at boot — a journal line, never a voice announcing
a session from before the reboot (the ADR 0032 missed-while-down stance).

### Phrases: the deterministic router grows one bounded text slot

All focus phrases live in the built-in intent table (ADR 0017), in two
halves. Fixed phrases ("what did I park", "take a break", "check in every
{minutes} minutes") compile with the built-ins and enter the collision set —
a routine or custom intent claiming one is a load error naming both owners.
Free-text phrases ("new thread called {text}", "later {text}", "focus on
{text} for {minutes} minutes") compile **last**, like the capture patterns
(#62), so every literal phrase in the system outranks the text slot.

`{text}` is the router's one free-text slot beyond capture's trailing name,
and it is deliberately narrow: bounded (six words for a name, twelve for a
parked thought — beyond that the utterance is a sentence for the model),
anchored by literal words around it with shortest-first backtracking (so
"with this window" or "for {minutes} minutes" can never be swallowed into a
name), and produced only by the focus compiler — custom intents and routine
phrases still refuse placeholders, because their free text would have to
reach a shell. A focus match carries an action name, the text, and a
bounds-checked integer into Jarvix's own store and nowhere else: no argv, no
dispatch, no gate needed — every action is a reversible edit of a state
file, the memory book's reversibility stance.

### Dispatch: through the runner seam, not a new engine option

The session engine dispatches a focus match to a `FocusRunner` — one call,
one spoken sentence — discovered by interface assertion on the engine's
existing `Options.IntentRunner`. The daemon injects
`focus.IntentRunner{Service, ExecRunner-fallback}`, which answers focus
actions and passes argv/shell work through untouched. This keeps the feature
out of `engine.go` entirely (no new Options field, no new engine state)
while sibling work lands in that package, at the cost of one type assertion
— judged worth it, and easy to promote to a first-class option later.

### Recaps: templated from the record, never generated

Every sentence — the switch recap (≤2 sentences: last time here, parked
count and newest, anchor and its liveness), the check-in line, the
cross-thread status (one line per thread, active first, capped at six with
the rest counted, inside ~15s of speech), the timebox start / midpoint /
close — is composed in `internal/focus/sentences.go` from the thread's own
record. A recap can be wrong only if the record is. Counts use the router's
`SpokenNumber`, ages the knowledge feeds' `SpokenAge`, so every surface
counts and dates on one scale. Model-composed summaries of anchored AI
sessions are the sibling ticket's, not this slice's.

### Anchors: captured once, verified per recap, never load-bearing

"With this window" captures the focused window (two: the two most recently
focused) from the shared compositor seam — address, stable id, app, title.
Recaps and the Focus tab check liveness against one fresh inventory read per
operation; a vanished window is spoken and shown as *gone* and nothing else
changes — the thread is the work, the anchor is a convenience. An unreadable
desktop is never reported as a vanished window; the clause simply stays
silent. Deictic references only in this slice: name-based anchor matching
(and the nicknames #126 is adding inside the same seam) composes later
without changes here.

### The clockwork: a third scheduler sibling, and the do-not-nag rule

Check-in reminders ("every 45 minutes") and the timebox's midpoint/close are
interval events created by voice at runtime — they fit neither the
automation scheduler's wall-clock `Spec` nor the feed scheduler's
fetch-state fusion, so `internal/focus` runs its own loop, a **third
sibling** restating the shared discipline (the ADR 0032 stance on reuse):
every goroutine in one tracked `quiesce.Group`, injected clock and timer so
no test sleeps, a bounded `Drain` as its own shutdown stage.

This is the first of the siblings whose schedule can be genuinely empty — no
session, no thread with an interval — and the loop nonetheless stays armed
through it, on a bounded idle sweep rather than sleeping until a mutation
pokes it. **ADR 0049** owns that rule and why it is not optional: a parked
loop is the one reader that never comes back to the store, so a hand-edited
`remind_every_min` arms nothing at all (#152).

State changes latch **before** speech is attempted: the midpoint marks
itself due, the close marks the session `closing`, and only then is a firing
dispatched — so a firing that cannot be spoken is a skipped announcement,
never a lost state and never a re-firing loop. Firings speak through the
ordinary scheduled-session path (`StartScheduledSession` + the phrase the
user could have said: "where am i on <thread>", "focus session update"), so
the router, events, activity feed, and conversation record are identical
however the sentence was asked for.

**Do-not-nag** is two layers. The daemon layer: `StartScheduledSession`
refuses while any session is live or speech is playing, and a refused firing
is dropped with a `focus.skipped` report. The service layer: while a focus
session holds the floor, every check-in is skipped outright — the whole
point of a timebox is that nothing interrupts it. In both layers the next
moment is computed from the schedule, never from a queue: skips cannot bank
and pour out later. Reminder ticks are in-memory only and adopt from boot
("you missed a check-in while I was off" is a false alarm by construction);
the close's continue-or-break question expires quietly after fifteen
unanswered minutes rather than re-asking into the next hour.

### Surfaces

`focus.*` IPC verbs (list / create / switch / park / end / session.start /
session.end / remind) are the whole contract for the window, the CLI, and
the bar/overlay siblings (#124/#127); every mutation returns the same spoken
sentence the voice path earns and publishes `focus.changed` — which carries
the active thread's id/name and session liveness precisely so #127's overlay
can show the current context from the event alone. The Focus tab is a
self-contained QML file with its own socket (the settings screen's pattern)
and request ids in the reserved 500–599 range, display-only per ADR 0013.
The overlay/bar rendering of the active thread name is deliberately deferred
to #127.

One config key: `focus.midpoint_checkin` (default **off** — a timebox is a
promise of quiet), read live at fire time. The threads themselves are state,
not configuration: configuration carries policy, state carries work.

## Consequences

- Switching costs one sentence and re-entry costs two, from data the user
  can read and edit; nothing about a thread is invisible or model-mediated.
- The router grows a bounded free-text capability that other families are
  structurally unable to use; the closed world of phrase collisions holds.
- A fourth store file and a third scheduler loop join the daemon's
  inventory; both restate existing disciplines rather than abstracting them,
  which is more code and less coupling — the trade this repo keeps making.
- The `FocusRunner`-via-`IntentRunner` assertion is the one soft seam; if a
  second feature ever needs the same trick, promote both to explicit engine
  options in one change.
- Reminder cadence is best-effort by design: a busy hour can skip every
  tick, and the record of skips is the journal plus `focus.skipped` events —
  chosen over any queueing, because a backlog of stale check-ins is the nag
  this feature exists to end.
