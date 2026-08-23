# ADR 0032 — Schedules: automations on a clock, allow-tier only, silent by default

**Status:** accepted

## Context

Routines (ADR 0026) and scripts (ADR 0030) fire only by phrase; #61
deliberately deferred time triggers. The user asked for "scheduled
activities" — "run morning setup at 8:30 on weekdays", "run backup my notes
nightly" — and the Automations tab (#93) needs something real to manage. The
nearest precedent is the knowledge feed scheduler (ADR 0031), which settled
the discipline for anything that runs on a clock in this daemon: tracked
goroutines, injected time, generation-swapped reloads, a bounded drain.

The feature's threat model is different in kind from anything before it.
Every prior execution path began with a person present: a phrase was spoken,
a gate could ask, an answer could arrive. **A schedule is pre-authorised
authority** — configuration written at noon executes at 3am with nobody
there. Three consequences follow, and each is a decision below: a gate that
would ask has nobody to ask; an announcement has nobody to hear it and a
household to wake; and a daemon that was off at the scheduled moment must
decide, alone, whether stale intent still holds.

## Decision

### One syntax, the friendly form, validated hard

A `schedule` key on a `[[routines]]` or `[[scripts]]` table: a 24-hour time
plus optional days — `"08:30"`, `"02:00 daily"`, `"08:30 mon-fri"`,
`"22:15 mon,wed,fri"`, with `weekdays` / `weekends` as words and wrapping
ranges (`fri-mon`) allowed. Five-field cron was rejected, not deferred: this
config is edited by the person who will be asleep when a mistake fires, and
cron's five positional fields are five chances to transpose one. One field
that reads aloud, one grammar, no second syntax to keep compatible. Parse
errors carry worked examples, and config validation compiles the real parser
— there is no weaker copy of the grammar to drift. Times are wall-clock in
the daemon's zone; `time.Date` normalisation decides DST edges (a firing
nominally inside a spring-forward gap lands after it, and a repeated
fall-back hour fires once, because the next-fire search is strictly-after
the last).

### Sibling scheduler, shared discipline

`internal/automation` is a sibling of the knowledge scheduler, not an
extraction. Feeds are interval arithmetic fused to fetch state — backoff
streaks, ttl staleness, boot-warm remainders; schedules are calendar moments
with overlap and missed-while-down policies feeds have no analogue for. What
they genuinely share is the *discipline*, restated in full: every goroutine
in one `quiesce.Group` from before it starts, `Now`/`Timer` seams so tests
fire the exact timer armed and never sleep, `Reconfigure` cancelling the old
generation into the same tracked group, `Drain` as one more bounded shutdown
stage (drain test written first). Extracting a shared core would have risked
the knowledge tests' green — the proof of behaviour preservation — to
deduplicate a pattern, not machinery. The knowledge package is untouched.

### A clockfire enters the ordinary session path

An allow-tier firing starts a session and submits the entry's first trigger
phrase — exactly the `routines.run` / `scripts.run` shape, because a second
execution path that skipped the router or the gate would be a hole in both.
Everything downstream is therefore inherited, not rebuilt: the
already-running refusal, the activity rows, the events, the archive, and
cancellation — "stop" aborts a schedule-fired run precisely because it is an
ordinary session. Two differences only, both the clock's:
`StartScheduledSession` **refuses instead of interrupting** when a session is
active (a spoken activation cancels what is in flight because a person is
waiting; nobody waits on a timer — the skip is reported), and the session is
**quiet** unless the entry opts out.

### Ask cannot ask at 3am: allow-only executes

The tier is resolved *before any session exists*, with the same
`DecideRoutine` / `DecideScript` the spoken gate uses, so clock and voice
cannot disagree. Anything short of allow — ask by default for every script,
or an explicit ask/deny — is **refused and reported**: an activity row and a
notification ("backup notes was scheduled but needs your confirmation — run
it now?") whose click opens the window through the standing default action.
The alternatives were both worse: executing an ask-tier entry unattended
silently deletes the one control that answers a misheard-or-malicious config
edit, and parking a confirmation until morning turns a question about *now*
into a landmine answered out of context. Because the refusal is only
discovered at the scheduled moment, configuring it is a load-time WARNING
naming the fix (`[tools.policy.tool]."script.run" = "allow"`) — the user
learns at noon, not at 2am.

### `announce = false` is the default, and the default is the feature

Nothing scheduled is ever spoken unless the entry says `announce = true`. A
3am TTS announcement is an anti-feature; the run's outcome lands in the
activity feed and the finish notification instead. Quiet is a property of
the *session* (every speech exit checks it — the streaming speaker, the
intent acknowledgement, even the confirmation prompt as belt-and-braces), so
the suppression cannot be configured back on by a report mode: `report`
still decides *what* the outcome says, announce decides whether it is said
aloud.

### Overlap skips; missed reports; nothing queues, nothing replays

A firing that arrives while the previous run is still going is **skipped
with a report row** — a schedule is not a queue, and two overlapping "backup
my notes" runs racing over the same files is strictly worse than one late
one. A firing that fell while the daemon was off produces **one boot-time
report row and is never re-fired**: the daemon cannot know whether 3am's
intent still holds at 9am (the backup may be mid-edit, the morning setup
mid-meeting), so it reports and lets the person replay it with the phrase.
The only persistence this needs is a small trail — last-fired and
last-handled-occurrence per entry, 0600 under the state dir, atomic writes —
and schedules otherwise resume from configuration alone; deleting the trail
costs one boot's report, nothing else.

### Surfaces

`automations.schedules` over IPC lists every schedule with its next-fire
time computed daemon-side and a `would_refuse` verdict, so the tab (#93) can
show "needs allow" before the notification teaches it. The scheduler's own
events — `automation.fired` / `skipped` / `refused` / `missed` — render as
activity rows like everything else the daemon does.

## Consequences

- `schedule = "02:00"` on the backup script is the whole feature for the
  nightly-backup case; with `script.run` allowed it runs silently, reports
  through the feed and a notification, and skips rather than piles up.
- An ask-tier schedule is legal but inert-by-refusal: warned at load,
  refused with a click-to-open notification at each moment. Loosening is one
  explicit policy line.
- The daemon gains one always-present supervised component and one state
  file (`automations.toml`); shutdown gains one drained stage; reload
  rebuilds schedules with the same generation discipline as feeds.
- The scheduled path's speech default diverges from the spoken path's — a
  spoken "backup my notes" still answers aloud; the same run at 02:00 does
  not. That asymmetry is deliberate and documented, and `announce = true`
  removes it per entry.
