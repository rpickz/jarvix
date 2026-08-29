# ADR 0050 — The return briefing: only what Jarvix already participates in, offered rather than ambushed

**Status:** accepted

## Context

Work continues while the user is away. AI sessions anchored to focus threads
run to completion or stall waiting for an answer (#137, ADR 0047), scheduled
routines and scripts fire (ADR 0032), feeds refresh (ADR 0031), reminders come
due (ADR 0046), a timebox closes with nobody there to answer it (ADR 0041).
Coming back means opening windows and scrolling to reconstruct all of it —
precisely the re-entry cost this programme has been attacking with focus
threads, recaps and parked thoughts. The last mile is the return itself.

Issue #150 asks for the account Jarvix can give of that gap. Two things about
it are more consequential than the feature, and both are decisions rather than
implementation details.

The first is **where the facts come from**. A briefing is only as good as what
it can see, and the obvious way to make it better is to let it see more: which
windows were open, what was typed, which sites were visited, what processes
ran. Every one of those is a small, plausible extension, and together they are
a different product. The user drew this line explicitly, and it is the reason
the feature was approved at all.

The second is **how it arrives**. A report the assistant reads at you the
moment you sit down is an interruption dressed as help — worse than the
scrolling, because you did not choose the moment. The ticket's phrase for the
alternative is "offered, not ambushed".

A third constraint follows from the first two. Preparing a briefing on a timer
would mean summarising a machine nobody is using, which costs a model call
every night for an account that may never be asked for and which would be
stale by the time it was.

## Decision

### The sources are a closed list, and it is closed on purpose

A briefing reports **only what Jarvix already participates in**:

- **AI sessions** anchored to focus threads — the deterministic
  working / needs_you / done classification of #137 and ADR 0047, read from
  `focus.Snapshot`. Structure only; no transcript content travels.
- **Focus threads** — the active thread's name and last activity, its parked
  thoughts, and a timebox that is running or sitting closed waiting for an
  answer (ADR 0041).
- **Reminders** — what fired while the user was away and what is owed now,
  from the reminder store (ADR 0046).
- **Its own activity** — the schedules that ran (the persisted trail of
  ADR 0032), the feeds that refreshed or are failing (ADR 0031), and the
  failures the in-memory activity ring still holds (#70).
- **Conversations** — how many were added to, from archive metadata alone
  (ADR 0027).

**No general machine-activity tracking may ever be added to enrich this.** Not
keystrokes, not window history, not browsing, not process inventory — not now
and not as a "small extension" once the shape exists. A record of everything
the user did is a different product with a different consent conversation, and
this feature must not become its Trojan horse. Also out, and separately: email
and calendar integration, and per-project git scanning.

The rule is written into `internal/briefing`'s package comment as well as
here, because a boundary that lives only in a closed ticket is a boundary that
erodes.

One consequence is worth stating plainly rather than quietly working around.
The ticket's source list says "conversations: how many exchanges, with the
last topic". **The topic is not reported**, because the archive has no topic:
the only per-conversation string it holds is `Preview`, the first line the
user actually said, and speaking that back is the replaying of content ADR
0027 and ADR 0028 forbid and this ticket's own boundary rules out. What the
briefing names instead is the **focus thread** the work sat on — a label the
user chose, from a store built to be read back. That is the honest version of
"topic" this daemon can offer, and inventing a better one would mean reading
something it should not.

### Offered, not ambushed

After an absence of at least `briefing.after_hours` (default 8), and **only
when at least one source actually has something**, exactly one sentence is
appended to the answer of the next exchange:

> I've got a briefing on what happened while you were away, whenever you want
> it.

It is appended to the answer, not spoken as a turn of its own, so it arrives in
the same breath as the thing the user came back and asked for. It is a fixed
sentence, never a model call, because an offer that cost a provider round-trip
would be a briefing prepared in advance.

The question "is there anything?" is asked **once per absence**, not once per
exchange: after it has been asked the offer is spent whatever the answer was,
so a machine that did nothing overnight pays for one source read and is never
asked again. The *briefing itself* remains askable until the next absence
supersedes it — the offer is one sentence, the absence is the subject, and the
subject stands.

`briefing.speak_on_return` (default false) speaks the whole account at that
same moment instead. "Unprompted" there means *without you asking for the
briefing*; Jarvix still waits until the user is demonstrably back, because
nothing in this feature runs on a clock.

### Absence is measured from the last user-started exchange

The engine is the only component that knows when a person is here, and it
knows it for free: a user-started exchange reaching `think` or `runIntent` is
the daemon's one unambiguous sighting. Anything else available — a window that
stayed open, a process that kept running — is the surveillance this ADR
forbids.

**But a sighting is only half of it, and the original wording of this section
elided the other half.** It said the absence *is detected on the next arrival*,
which read as a rule about *how* an absence comes to exist rather than about
where the one input comes from — and the implementation followed it literally.
`Arrive` compared the two timestamps; every reader returned only what a
previous arrival had stored. So the daemon could hold a fifteen-hour-old
watermark from the archive and still answer "you haven't been away long enough"
to a user who had been away all night, because nothing had *witnessed* the
night (#188). It was worst on the surface a returning user is most likely to
touch — the window's "What did I miss?" button — because pressing it involves
no exchange at all, and it made the feature's behaviour depend on the order of
the user's first two actions: speak-then-ask worked, ask-then-speak did not.

The corrected rule:

> **An absence is a fact derivable from two timestamps — the last sighting and
> now — not an event that has to be witnessed.** Every reader derives it, at
> the moment it is asked, from the watermark and the clock. Nothing needs to
> have happened for the daemon to be able to say how long it has been.

An arrival keeps three jobs, and they are the three no reader can do for
itself. It **ends** the absence, by moving the watermark — the act that makes a
running absence stop being derivable, which is why nothing else may write that
field. It **preserves** the absence it just ended, so the briefing is still
askable once the user is back. And it **arms the one offer**, because an offer
rides an answer and only an exchange has an answer to ride.

Where the two readings disagree, the *running* absence wins. It is always the
more recent: the stored one only ever holds a watermark that a later arrival
superseded, so it began and ended before the running one started. And "the
absence ended" cannot be true while nothing has arrived to end it — when the
user is asking, the honest reading is that they are standing in it.

Reading is a read. It moves no watermark, arms no offer, and spends none: an
absence is still reported once per absence however it was first observed, and
the "asked once per absence" rule on the offer is untouched. The conservative
stance is untouched too — an unknown watermark (no archive, nobody ever here)
yields no absence rather than an enormous one, and a clock that went backwards
gives a negative gap, which is below any threshold and so reads as no absence,
exactly as it does on the arrival path.

**A clockfire is not a sighting.** A reminder speaking at three in the morning
starts a session, commits a turn, and writes to the archive exactly as a
spoken exchange does; counting it would erase the very night the briefing
exists to describe. `sess.scheduled` (set by `StartScheduledSession`, beside
`quiet` and `wake`) is what tells the two apart.

The watermark is in memory, seeded once at construction from the **newest
archived conversation's `LastActive`** — the one durable record of when the
daemon last dealt with the user — so an absence that spans a restart is still
an absence. It is deliberately not the engine's `lastTurn`: the follow-up
window zeroes that, and a lapsed follow-up is a fact about working memory, not
about whether anyone was here. The seed's own imprecision is accepted and
known: a clockfire that ran after a restart moves it, so a reboot followed by a
3am reminder shortens the measured absence. The error only ever runs one way —
towards *not* claiming an absence — and a briefing invented for a gap that
cannot be demonstrated would be the worse failure.

### The ordering is a claim about the reader

Awaiting-you, then completed, then in-progress, then housekeeping — and the
"I couldn't check…" admissions last. That order is not about the machine; it
is about what a person needs first. A briefing read in any other order makes
the listener wait through the news they did not need for the news they did.

A source contributes **at most one line per category**. Four of the five
sources therefore contribute one line each. The AI sessions are the exception
and the reason the rule is per-category rather than per-source: a session
waiting on you and a session that finished are different news, and merging
them into one line would force a category that is a compromise between the
two — in a briefing whose whole ordering is that category.

### Length is measured in seconds of speech

The spoken form is bounded at ~30 seconds (75 words at a deliberately
conservative 150 wpm), trimmed whole lines at a time from the tail, with a
fixed *"The rest is in the window."* whenever anything was left out. The trim
runs twice: once without the pointer, and again reserving its words only if
the first pass actually dropped something — so a briefing that fits is never
shortened to make room for an announcement that it was shortened.

Two things are exempt from the trim. The headline, because it is the shape of
the whole thing. And the unavailability admissions, because the trim takes the
tail and the admissions live there: a shortened briefing that quietly loses
"I couldn't check the reminders" has become a dishonest one.

The **full version renders in the Focus tab**, behind a "What did I miss?"
button on its own reserved request id (510, inside the tab's 500–599 range).
The Focus tab rather than the Activity tab because the subject matter is the
Focus tab's — the threads and the sessions anchored to them — and because the
Activity tab is a live chronological ring of one row per event, which a
composed multi-line report is not.

### The model words one sentence, and only one

Free prose over a fact list cannot be checked for extrapolation after the
fact. So the split is structural: **every line the user hears is composed by
the source that owns the fact**, and the model is asked for the **headline
alone** — one sentence saying what shape this is in.

That single sentence passes a pinned contract before it is spoken:

1. **Shape.** One sentence, bounded length, list markers and labels stripped
   (tolerantly, the recap contract's own stance).
2. **Claims.** It may not say anything finished, is waiting, or is still
   running unless a line in that category exists. Negations are allowed
   through — "nothing finished" is a true thing to be able to say, and a
   guard that refused it would leave the plain reading speaking on every quiet
   night the model got right.
3. **Counts.** Every number in it must be a number that is actually true of
   the facts: a category's count or the substantive total.

A refusal is not an error. It falls back to the deterministic headline, which
is duller and correct, and the outcome travels in the event as `refused` so a
provider that keeps failing the contract is visible rather than merely
disappointing. **No model is consulted at all** when there is nothing
substantive to word: an empty night and an unreadable one both get the plain
sentence, which is what makes "never a manufactured briefing" a property of
the code rather than a hope about the prompt.

A source that cannot be read becomes a **named** line — "I couldn't check
your reminders just now" — never a silent omission, because the listener has
to be able to tell "nothing happened there" from "I did not look".

### Nothing is prepared until someone is back, and nothing is kept

There is no scheduler and no goroutine in `internal/briefing`. Everything is
read at the moment the user is demonstrably back, which is also the only
moment the answer could be wanted. ADR 0049's "a scheduler loop never parks"
therefore has nothing to say here: there is no loop for it to be about.

The composed briefing is **transient**, like a recap (#124, ADR 0043):

- It is written to no store, and the window's version is recomposed rather
  than replayed from a cache — so a tab opened ten minutes later tells the
  truth about ten minutes later.
- It carries no content into any event. `briefing.given` carries the reason,
  counts, the truncation flag, the model outcome, the spoken age of the
  absence, and the names of any unreadable sources. A leak-salted test fails
  the build if a word of the account reaches it.
- The **deterministic reading is not committed to the conversation.** The
  exchange stays in the record — the user asked, Jarvix answered — but the
  assistant half is the stand-in "I gave the return briefing.", the shape a
  silent script's success already uses. The account does not become
  conversation memory a later turn is sent.

One boundary on that last rule, stated rather than hidden: when the **model**
reaches the briefing through its tool (`briefing.get`, for the natural
phrasings the deterministic grammar does not claim), what is committed is the
model's own answer, exactly as for any tool call. The transience rule binds
Jarvix's own composition and its own record-keeping; it cannot bind what an
assistant says in reply to a tool result without unpicking how every tool
works. The tool's description forbids embellishment and pins that it must read
the result as written.

### Configuration

`briefing.enabled` (true), `briefing.after_hours` (8, bounded 1 to 672),
`briefing.speak_on_return` (false) — all **live class**, on
`focus.midpoint_checkin`'s terms: the service reads them at the moment it
decides, so "stop offering me briefings" spoken mid-conversation is true by
the next answer. None is dangerous; the widest thing any of them can do is
make Jarvix say one more sentence about work the user already owns. Being in
the registry makes them voice-adjustable through the self-configuration tools
(ADR 0036) for free.

On by default is safe because with nothing to report there is no offer at all.
Speaking without being asked is not, so it is opt-in.

## Consequences

- The offer costs one source read, once per absence, running while the answer
  it will be appended to is still draining out of the speaker. Every other
  exchange pays one interface call and a mutex.
- Deriving the absence costs a clock read and the same mutex, on the read path
  only. The alternative — a goroutine that notices the threshold passing — is
  the timer this design refuses, and it would be strictly worse: it would have
  to survive suspend, and the wall-clock difference already does, for free.
- A briefing costs one bounded model call for one sentence. If it fails, the
  facts are read out plainly.
- The activity ring is in-memory, bounded and lossy, and it dies with the
  daemon; the schedule trail and the feed statuses do not. So the activity
  line's counts are true across a restart and its *failure* count is
  best-effort — which is why that line appends "My own record only goes back
  to when I restarted" whenever this process began after the absence did.
- Two sources read the same focus snapshot, and read it exactly once per
  briefing. That is not only a saving: two snapshots a second apart could
  disagree about the same thread, and a briefing that contradicts itself
  between its second and fourth line is worse than one extra compositor call.
- The deterministic phrases ("what did I miss", "give me the briefing",
  "brief me", "catch me up" and their table-mates) never reach the model —
  ADR 0017's rule for a fixed request with one right outcome — and near
  misses like "what did I miss in the standup" fall through to it, where the
  tool is waiting.
- The alternatives were considered and rejected. Preparing on a timer
  summarises a machine nobody is using and goes stale. Speaking on return by
  default is the ambush. Letting the model rewrite every line would make the
  no-extrapolation rule unverifiable, and a rule that cannot be checked is a
  request. Reading `Preview` for the "last topic" would have satisfied the
  ticket's wording by breaking its boundary.
