# ADR 0046 — One-shot reminders: owed, never nagged, never doubled

**Status:** accepted

## Context

"Remind me at three to call the pharmacy" is the most universal assistant ask
there is, and Jarvix could not do it. Everything on a clock in this daemon was
a **recurring configuration entry** — `[[routines]]` and `[[scripts]]` with a
`schedule` (ADR 0032), per-thread check-in intervals (ADR 0041) — and creating
one by voice routes through the config-write confirmation card (ADR 0036).
That is exactly wrong-shaped for a throwaway reminder: heavyweight to make,
permanent once made, and gated behind a question about the user's own
sentence.

For the ADHD support arc this is thought-parking with a clock: the single
highest-leverage missing primitive (#141). It needs three things the existing
clocks do not provide — a **one-shot** moment, **spoken time parsing** with an
honest answer to "which three?", and a delivery contract where a reminder that
could not be spoken at its moment is **owed**, not dropped.

That last point is where this ADR earns its keep. Both existing schedulers are
deliberately **drop-on-refusal**: a check-in skipped behind a live session is
gone, and the next one is computed from the schedule so skips can never bank
(ADR 0041), while a schedule missed while the daemon was down is *reported,
never re-fired* (ADR 0032). Those stances are right for a cadence — a stale
check-in is the nag the feature exists to end — and wrong for a one-shot: the
user asked once, for one thing, and dropping it is losing it.

Three prior decisions constrain the design. ADR 0013: every surface is
display-only, so anything spoken or shown is composed daemon-side. ADR 0017:
fixed phrases with one right outcome belong to the deterministic router, not
the model. ADR 0025: a reversible write into the user's own state file is the
user's own word — allow-tier, no card.

## Decision

### The store: state, not configuration — and that is the whole point

`~/.local/state/jarvix/reminders.toml`, one hand-editable TOML document under
the XDG state dir, 0600 in a 0700 directory, with the ADR 0025 storage
discipline applied verbatim: atomic fsync-and-rename writes, a stat-based
change detector (a hand edit lands on the very next operation, no watcher, no
restart), normalize-repair that never fabricates, a corrupt latch that serves
an empty store and moves the unparseable file aside rather than overwriting a
file the user may be mid-way through fixing, and ids that ratchet so a
reminder id is never reissued.

Deliberately **not** `config.toml`. Configuration carries policy; state
carries work — and a reminder is work, created a dozen times a day by voice
and gone by evening. Putting it in configuration would drag every "remind me
at three" through the entry-write confirmation card, which is the ceremony
this feature exists to remove. A pinned test asserts that creating a reminder
by voice raises no confirmation event of any kind, at both the engine and the
daemon layer, so the card cannot regrow by accident.

The document also holds a **capped fired history** (twenty entries): a
reminder that fired or was cancelled leaves the pending list and lands there,
which is what makes "what fired today" answerable without keeping reminders
alive in listings after their moment.

### Time parsing: pure code, in the router's package, with no clock

`internal/intent/when.go` is the `{when}` slot's grammar, sitting beside the
number table it reuses — `parseNumber` and `SpokenNumber`, one copy of
words-to-numbers for the whole daemon. Parsing is **syntax only and needs no
clock**, which is what lets the pattern matcher validate a `{when}` slot the
way it validates `{volume}`: an expression the table does not recognise simply
does not fill the slot, the phrase misses, and the utterance falls through to
the model. The accepted shapes are a short, exhaustive, table-tested list —
24-hour (`at 15:00`), 12-hour with words or digits (`at three`, `at three
thirty`, `at nine oh five`), meridiems (`at three pm`, `in the afternoon`, `at
night`), the named hours (`noon`, `midday`, `midnight`), `tomorrow at nine`
either way round, and relative delays (`in twenty minutes`, `in an hour and a
half`, `in two hours and thirty five minutes`).

**Resolution is the separate half.** `When.Resolve(now)` applies the
next-occurrence rule against a caller-supplied clock — so the reminders
service, which owns the injected clock, decides *when* "three" is, and the
whole table stays testable to the minute without a single sleep.

### The ambiguity policy: next occurrence, and the confirmation says which

A bare 12-hour hour is not guessed at and not asked about — it resolves to the
**next** of its two readings, and the spoken confirmation names that reading:
"Reminding you at three this afternoon: call the pharmacy." Said at 16:00 the
same words earn "at three tonight", because the next three is tomorrow's small
hours. An exact 24-hour time already past today rolls to tomorrow on the same
rule. "Tomorrow at nine" takes the daytime reading, because "tomorrow at nine"
almost never means tomorrow night — and the confirmation says so either way.

The honesty is load-bearing: the user hears which reading won at the moment
they can still correct it, in one sentence, with no question asked. An
expression the table cannot read is refused with a spoken hint naming two
shapes that do work, rather than a silently invented time.

### Phrases: the deterministic router grows a validated `{when}` slot

Fixed phrases ("what reminders do I have", "what fired today", the delivery
path's own "reminder check") compile with the built-ins and enter the
collision set — a routine claiming one is a load error naming both owners.
Free-text phrases ("remind me {when} to {text}", "remind me to {text} {when}",
"cancel the {text} reminder") compile **last**, after every literal phrase in
the system — including the focus grammar's own "remind me where i am every
{minutes} minutes", which therefore keeps its exact words.

`{when}` is a new slot kind and it is validated *at match time*, which is what
makes a mid-utterance split deterministic: the matcher tries the shortest
reading and backtracks, and only a split whose when-words actually parse can
win. So "remind me to pick up the kids at school at three" puts "at school" in
the errand and "at three" on the clock, because "at school at three" is not a
time. Both slots reach Jarvix's own store and nowhere else: no argv, no shell,
no dispatch, and — as with focus actions — no gate, because every action is a
reversible edit of a state file.

### The model tool: for the phrasings the grammar cannot claim

`reminder.set` / `reminder.list` / `reminder.cancel` are the model's path for
"could you give me a nudge about the oven around six?" — allow-tier by
built-in default, on **memory.remember's exact argument**: the write is one
line into the user's own 0600 file, the confirmation speaks exactly when it
will fire, and a wrong reminder is undone with one cancel. Asking would turn
an instruction the user just gave out loud into a question about itself.
`reminder.cancel` is allow too — unlike `memory.forget`, which asks — because
cancelling destroys nothing: the entry moves to the retained history, and
re-setting it is one sentence. The tool relays the parser's refusal hint
verbatim rather than guessing a time the user did not say.

### The clockwork: a third scheduler sibling with an owed contract

One-shot moments fit neither the automation scheduler's recurring wall-clock
`Spec` nor the focus clockwork's interval cadences, so `internal/reminders`
runs its own loop — a **third sibling** restating the shared discipline (the
ADR 0032 stance on reuse over abstraction): one loop armed to the single next
due moment, every goroutine in one tracked `quiesce.Group`, injected clock and
timer so no test sleeps, a bounded `Drain` as its own shutdown stage.
Deliveries speak through the ordinary scheduled-session path — a session plus
the phrase the user could have said, "reminder check" — so the router, events,
activity feed, and conversation record are identical however the sentence was
asked for.

**The owed contract is the deviation, and it is deliberate.** A moment gets
exactly one delivery attempt. If the engine refuses the floor — a live session
or playing speech, the do-not-nag rule — the reminder is *parked as deferred*
rather than retried into a pile-up, and the daemon's session-boundary watcher
releases it at the one moment it is most likely speakable: that session's end.
Delivered more than two minutes after its moment, the spoken line says so
("Reminder, six minutes late: …"). Deferral is in memory only; on disk,
"pending with a past due" **is** the owed state, which is what makes recovery
inevitable rather than best-effort.

**Boot is the same contract over downtime.** Where ADR 0032 reports a missed
schedule and never re-fires, a reminder missed while the daemon was down fires
**once** at boot, marked late: "While I was off: you asked me to remind you to
call the pharmacy at three." However many were missed, they arrive as one
announcement — a catch-up, never a backlog storm.

### Never doubled: one claim, under the lock that observes it

This is the #136 lesson (a check-in due inside a timebox must not pour out at
its end) applied at the mechanism rather than patched at the symptom. The
owed→delivered transition happens in exactly one place, `ClaimDue`, which
moves every arrived reminder into the fired history **under the store's lock**
and returns the one spoken announcement for them all. Whichever path won the
floor — the scheduler's own attempt, a boundary release, or the user saying
"reminder check" themselves — exactly one claim finds the owed reminders. Two
racing wakes cannot speak a reminder twice; and until a claim commits to disk
they stay pending, so a crash mid-delivery loses nothing. A claim whose write
fails speaks nothing at all, because a delivery the disk does not record is a
reminder the next boot must still owe.

### Surfaces

`reminders.list` and `reminders.cancel` are the contract for the window; the
Automations tab grows a **one-shot section** on the shared collection rows
(the Memory tab's Vocabulary-section pattern: a fixed share of the pane, its
own scroll, neither collection able to push the other off screen), with the
daemon's own wording for each due moment and Cancel as the only operation — a
reminder is not edited, it is cancelled and said again. There is no New button
on purpose: a reminder is made by saying one, and the empty state says so. The
section refreshes on `reminders.changed`, so a firing or a spoken cancel
updates the tab without a click.

No configuration keys at all. There is no policy to carry.

## Consequences

- The daemon's most-asked-for primitive costs one spoken sentence to create
  and one to cancel, with no card, no config write, and no restart.
- A fifth store file and a fourth scheduler loop join the inventory; both
  restate existing disciplines rather than abstracting them — more code, less
  coupling, the trade this repo keeps making.
- The router gains a second bounded slot kind (`{when}`) that only the
  reminder compiler can produce, so custom intents and routine phrases remain
  structurally unable to carry free text or times into a command.
- The runner chain is now two deep (`reminders.IntentRunner` wrapping
  `focus.IntentRunner` wrapping `ExecRunner`), each family answering its own
  dispatch and delegating the rest. This is the second use of the
  interface-assertion seam ADR 0041 flagged as soft; a third should promote
  all of them to explicit engine options in one change.
- Reminders are **owed**, which is the opposite of every other clock in the
  daemon. The cost is that a reminder can arrive late — after a long session,
  or at the next boot — and the mitigation is honesty: the spoken line says it
  was held, and past two minutes it says how long.
- A reminder deferred behind a session that never ends waits for the next
  boundary, whenever that is. Chosen over a timeout that would speak over the
  user, because interrupting is the one thing the do-not-nag rule forbids.
