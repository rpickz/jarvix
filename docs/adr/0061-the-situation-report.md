# ADR 0061 — The situation report: one question about now, composed from what Jarvix already holds

**Status:** accepted

## Context

The user's framing for the operator programme (#195) puts one question above
every other: *"Jarvix needs to be able to quickly tell me what's going on on the
machine, what it's doing, where it's at with things."* It is the question a
manager asks most often, and it is the one Jarvix could not answer.

Not for want of facts. Every one of them is already held, and all of them are
scattered:

- the deterministic working / needs_you / done classification of the AI sessions
  anchored to focus threads (#137, ADR 0047) — **the highest-value fact on the
  machine, and already computed**;
- the focus threads with their anchors, parked thoughts and last activity, and a
  timebox running or sitting closed waiting for an answer (ADR 0041);
- the reminders pending and fired (ADR 0046);
- the routines and scripts on a schedule, and whether one is firing (ADR 0032);
- the knowledge feeds and whether any is failing (ADR 0031);
- Jarvix's own activity ring;
- the window inventory (ADR 0022).

Asking today means asking five different things, and four of them have no spoken
form at all. This is the first slice of #195 deliberately: it is buildable from
what exists, it is immediately useful, and it **sets the summarisation quality
bar** everything after it has to meet.

The return briefing (#150, ADR 0050, as amended by #188/#189/#190/#192) is the
model for how to be honest here, and most of its rules transfer verbatim. What
does not transfer is the thing the briefing is *about*.

## Decision

### The report is about NOW, and that is the whole distinction

A briefing is an account of a stretch of time the user was not here. A situation
report is a description of the machine as it stands. Everything below follows
from that one difference, and the ticket's instruction — *do not force it
through the briefing's since/now source interface if that distorts it; decide,
and record the decision* — is answered here.

**Forcing it would have distorted it in exactly one place, and it is a bad
place.** The briefing cannot compose without a window: with no record of when
the user was last here it answers `no_record` — *"I've no record of when you
were last here, so I can't say what you missed."* That is right for a briefing
and absurd for a report. A daemon that answered *"I don't know when you last
asked, so I can't tell you what's going on"* would be refusing to describe a
machine it is looking straight at.

So:

> **No reading of the clock can stop a situation report being given.** There is
> no threshold, no floor, and no `no_record` state. A first-ever ask on a
> freshly installed daemon composes in full.

A source is handed an `Instant`, not a `since, now` pair:

```go
type Instant struct {
    Now   time.Time // the moment the report is about
    Since time.Time // when the user last looked; ZERO when they never have
}

type Source struct {
    Name string
    Read func(ctx context.Context, at Instant) ([]Item, error)
}
```

`Since` survives because one rank genuinely has a backward edge —
*finished-since-you-last-looked* — and because the coverage caveat needs
something to be short of. Four of the six shipped sources ignore it entirely,
which is the point: `needs_you`, `working`, a failing feed, a running clockfire
and an open window are all current state.

**The zero rule.** `Since` is zero when nobody has ever looked and there is no
durable record to seed from. A source whose news is interval-shaped must then
report *nothing* rather than reporting all of its history: a fresh daemon
reading out every reminder that ever fired would be answering a question about
now with an archive. Two sources obey it — the fired reminders and the ring's
failure count — and a test pins it.

`Since` is seeded, once, at construction, from the newest archived
conversation's `LastActive` — the same durable record the briefing seeds from.
Without a seed the first report after every reboot would have no backward edge,
and, worse, could never notice that its own activity ring cannot account for the
stretch, which is the admission #190 exists to make.

A struct parameter rather than two arguments is also the extensibility answer: a
later source that needs to be told something more about the moment gets a field
here, and every source already written keeps compiling.

### The ordering, and why it is not the briefing's

> **needs-you → in-progress → finished-since-you-last-looked → failing →
> housekeeping**, with the *"I couldn't check…"* admissions last.

The briefing's order is awaiting → completed → in-progress → housekeeping.
Inverting the middle two is deliberate and is the ordering's whole argument: a
person coming back from a night away wants to know what landed; a person asking
where things stand right now wants to know what is *running*. Same facts,
different question, different order.

`needs_you` leads. It is the state the machine has already computed, it is the
only one that is blocking a person, and it earns its place **by rank rather than
by a special case** — there is no branch anywhere in `internal/situation` that
names the AI sessions.

The ordering is pinned three times, because it is the feature:
`TestTheOrderingIsPinned` builds one source that emits its ranks upside down and
asserts both the section order and the spoken order;
`TestTheOrderingIsNotTheBriefings` asserts the rank constants directly, so
somebody "correcting" this package to match its neighbour fails immediately; and
`TestTheReportSectionsArriveInThePinnedOrder` asserts it over the wire against a
real daemon.

### Specifics, never categories

*"Two sessions are waiting on you"* is a category. *"The AI session on the
deploy thread is waiting on you"* is the report. Each `Item` is one thing, and
each is worded by the source that owns the fact — the composer never turns data
into prose.

A source names up to three things in a rank and then words a counted tail
(*"And two more sessions are waiting on you."*). The cap is on the tail, not on
the first name: naming is the point, and the spoken budget is enforced
separately, downstream, where it can be measured.

### Every line links to the thing it describes, through #168

The window has to get from a line to the thing. It does so through the
**existing provenance navigation** (#168, ADR 0055) rather than a second
mechanism, so there is one resolver, one liveness answer and one set of buttons
on this machine.

Each `Item` carries an optional `*provenance.Reference`. The wire shape puts the
references in one flat `sources` array in render order and gives each line a
`link` index into it. The tab hands `sources` to `provenance.resolve` verbatim
and reads each line's item back at its own index — **the index is computed
daemon-side**, so the client does no arithmetic and cannot pair a line with
somebody else's subject (ADR 0013). Following a link uses the same split the
conversation window's panel makes: an action carrying a `tab` is the window's
own navigation, anything else goes to `provenance.open`.

Three consequences are worth stating.

**Two kinds were added to the vocabulary**, `KindReminder` and `KindSchedule`,
with resolver branches beside the existing seven. They are additions to the same
vocabulary, not a parallel one, and they resolve on identical terms: named from
the live store, `gone` with a plain-English note when the thing has left, and no
action offered on a row that would do nothing.

**These references are never recorded on a turn.** They are composed at read
time from the same stores the lines were composed from, and they never enter a
`provenance.Record`. That does not weaken ADR 0055's central rule — attribution
is mechanically derived, never model-reported — it satisfies it trivially: the
reference is derived from the fact the source read, by the code that read it,
with no model anywhere near it.

**A line about no single thing carries no link.** The desktop line describes the
shape of a whole desktop; the ring's failure count describes rows whose detail
died with the previous process; a counted tail describes a group. A link on any
of those would take the reader somewhere nearly right, which is worse than
nowhere. Compositor addresses are additionally forbidden from travelling at all
(ADR 0022, and the overlay feed's own rule), and a test asserts none does.

### The facts are fed to the model, and the model words one sentence

The split is structural and is the same one ADR 0050 made, for the same reason:
free prose over a fact list cannot be checked for extrapolation after the fact,
and a single sentence with a pinned contract can be.

**Every line the user hears is composed by the source that owns the fact.** The
model is asked for the **headline alone**, and its sentence passes a contract
before it is spoken:

1. **Shape.** One sentence, bounded length, list markers and labels stripped
   tolerantly.
2. **Claims.** It may not say anything needs you, is running, has finished, or is
   failing unless an item in that rank exists. Denials are allowed through —
   *"nothing has finished"* is a true thing to be able to say.
3. **Counts.** Every number must be a number actually true of the facts: a rank's
   count, the notable total, or the substantive total.

A refusal is not an error. It falls back to the deterministic headline, which is
duller and correct, and the outcome travels in the event as `refused`.
`TestAModelThatInventsProgressIsRefused` is the pin: over facts in which nothing
has finished, four fixtures that announce progress are each refused and the
deterministic reading is spoken instead. The scar tissue is #71 — a small model
narrating actions it never performed — and a situation report is precisely the
shape of answer in which that failure is invisible.

**No model is consulted at all** when there is nothing notable to word. That is
what makes "never a manufactured report" a property of the code rather than a
hope about the prompt.

The shape half of this contract now lives in `internal/sentence`, shared with the
briefing's. The claims half stays with each feature, because only it is a
question about that feature's categories. Two copies of "did the model answer
with one sentence, and what number did it say?" would drift, and drift in the
direction the honesty rules cannot afford.

### Nothing needs you

> **Given nothing of note, the answer is a short honest "Nothing needs you."**

*Notable* means the four ranks above housekeeping. Housekeeping does **not**
defeat it, and that is a decision rather than an oversight: the desktop always
has something on it, so a quiet answer that any housekeeping line defeated would
be a quiet answer that never happened. The housekeeping is still reported — it
was really read — but the headline does not treat it as news, because inventing
urgency is the failure this sentence exists to avoid.

A source that could not be read *does* defeat it. *"Nothing needs you"* from a
daemon that could not read two of its sources is a claim it has not earned, so
that case says so: *"Nothing needs you in what I could read — and I couldn't
check everything."*

### An unreadable source is named, never omitted

ADR 0050's discipline, held verbatim. A source that errors becomes a named line
in an *"I couldn't check"* section — *"I couldn't check your reminders just
now."* — because the listener has to be able to tell *"nothing is failing"* from
*"I did not look"*. The sentence is `internal/situation`'s, not the adapter's: a
source that files an `Unavailable` item of its own is dropped, so the one wording
a listener has learned to trust cannot have as many variants as there are
adapters.

The default noun for an unknown source is its own identifier, so a source added
by a later slice names itself honestly on the day it is added rather than on the
day somebody remembers to come back and word it.

### Length is measured in seconds of speech

Bounded at ~20 seconds (50 words at a conservative 150 wpm), trimmed whole lines
at a time from the tail, with a fixed *"The rest is in the window."* whenever
anything was left out. The trim runs twice: once without the pointer, and again
reserving its words only if the first pass actually dropped something.

Twenty rather than the briefing's thirty, for a reason about the question rather
than the content: a briefing is settled into, and *"what's going on?"* is asked
on the way past.

Two things are exempt: the caveat and the unavailability admissions. The trim
takes the tail, admissions live there, and a shortened report that quietly lost
*"I couldn't check the reminders"* has become a dishonest one. Needs-you needs no
exemption — it is at the head, which is what the ordering is for.

### The caching rule

> **One report is composed at most once per 30 seconds. Every ask inside that
> window — voice, tool, or window — replays the same composition, at no source
> read and no model call. The replay carries the moment it was composed and is
> flagged as a replay; the window's Refresh button forces a new reading.**

Thirty seconds is chosen against **what the report can honestly say**, not
against a cost target, and that is what makes it defensible rather than merely
convenient. The shared spoken age scale (`knowledge.SpokenAge`, ADR 0013) bottoms
out at *"just now"* for anything under a minute. So a report inside the cache
window is not merely close enough to fresh — it is a report whose age Jarvix has
**no word to distinguish** from a fresh one. Handing back the held composition
and handing back a new one produce the same sentence about when it was read.
Past the window that stops being true, and it expires.
`TestTheCacheNeverOutlivesWhatTheAgeScaleCanSay` fails if the window is ever
raised past the floor.

The other half is ordinary: *"what's going on"* asked twice in a row — because
the speaker was interrupted, or the answer was missed — is a real and frequent
thing, and it must not cost two compositor reads and two model calls.

The cache is **time-based only**. Nothing invalidates it early, deliberately: an
invalidation hook would be a second, quieter definition of "the machine changed",
and the honest bound on staleness is the one the reader can see. Compositions are
serialised, so two asks arriving together cost one reading rather than racing
each other to the compositor.

**The watermark moves only on a real composition.** A replay is not a new look;
if it moved `Since`, a later ask would report nothing as having finished since a
report the user was only ever shown a copy of.

### A window that predates this process says so up front

The reminders, the focus threads, the schedules and the AI-session transcripts
are read live or from disk, so they answer for the whole stretch since the user
last looked. Jarvix's own activity record is an in-memory ring that died with the
previous process (#70). A report whose *"since you last looked"* opens before
this process started is therefore composed from five sources answering
confidently and one that could not have seen the start of it — and it reads as a
confident *"nothing is failing"* with the one thing that could not be checked
left unsaid.

So the report says it about **itself**, in its own field, spoken second and
rendered directly under the headline, exempt from the trim. It names both halves,
because a listener told only that "some of this is missing" has been handed a
doubt rather than a fact:

> I restarted since you last looked, so my own record of what has failed only
> goes back to then; your sessions, threads, reminders and schedules are read
> live, so those are complete.

It is #190's correction applied from the start rather than learned again. With
`Since` zero there is no caveat: nothing is being claimed about a past stretch,
and a doubt with no claim attached to it is worse than no doubt at all.

### The source interface is shaped so the next slices are additions

This is the design constraint that matters more than any single feature, and it
is stated here because #195's next slice depends on it.

**A source is a name and one function.** Nothing in `internal/situation` knows
which sources exist. The ordering is over *ranks*, not over sources. The
unavailability wording falls back to the source's own name. The speech budget,
the trim, the headline contract and the link flattening are all defined over
items rather than over origins.

So the **jobs** source of #195's next slice, and the **remote machine** source of
its last, are added by writing one adapter and one line in `bindSituation` — no
change to the composer, the ordering, the prompt, or the wire shape. A job that
needs a decision is a `NeedsYou` item; a job on step two of four is `InProgress`;
a machine that cannot be reached is an error, and the report names it as
unavailable in its own name.

`TestASourceAddedLaterNeedsNoChangeToTheComposer` demonstrates it rather than
asserting it: a source this package has never heard of, with a name appearing
nowhere in it, produces items in four ranks and gets them ordered, ranked,
linked, spoken and — when it fails — named. If a later slice ever has to touch
the composer, that test is what stops compiling first.

Two smaller shapes serve the same end. `Instant` is a struct, so a new field
costs no signature change. And sources are read **in parallel**, so the cost of
adding one is its own latency rather than the sum of everything before it — which
matters most for the remote-machine source, whose latency is a network's.

### Where it is reached from

- **Voice**, through a deterministic intent (`situation.speak`) on ADR 0017's
  rule: a fixed phrase with one right outcome must not spend a provider
  round-trip deciding it did. The phrases are literal and take no free text.
  *"What's going on with the deploy"* is a question for the model about one
  thing, and ambiguity belongs to the model — which reaches the same answer
  through the tool. The bare word **"status" is deliberately not claimed**: it is
  one word, it is the most likely thing to be left when STT clips a longer
  sentence, and claiming it would make a whole class of half-heard questions
  answer with a report nobody asked for.
- **The model**, through `situation.get` — allow-tier by built-in default, on the
  briefing tool's terms: a read with no arguments, of the user's own machine,
  answered to the user, at it. Its description forbids embellishment, because the
  report's own contract cannot reach what an assistant says in reply to a tool
  result.
- **The window**, through the `situation.get` IPC method and its own tab.

The tab is **its own tab, beside Activity**, rather than a corner of the Focus
tab where the briefing lives. The subject matter is the whole machine, not the
threads; and the Activity tab is a live chronological ring of one row per event,
which this is the summary *of* rather than a replacement *for*. It is
self-contained in `JarvixSituationTab.qml` with its own socket and its own
request-id range (600–699), the Focus tab's shape.

### The report is transient

Nothing is persisted. The composed report is written to no store; the window
recomposes (or replays, inside the cache window) rather than replaying a
remembered one; and the **deterministic reading is not committed to the
conversation** — the assistant half of the exchange is the stand-in *"I gave the
situation report."*

The transience rule is ADR 0050's, and here it has a reason of its own on top of
that one: a report is a description of a moment, and a moment that has passed is
the single most misleading thing a later turn could be handed. *"The session on
the deploy thread is waiting on you"* committed at nine o'clock is false by half
past, and a model reading it back as context would state it with the confidence
of something it was told.

The event `situation.given` carries the reason, counts, the truncation flag, the
cached flag, the quiet flag, the partial-coverage flag, the model outcome and the
names of any unreadable sources — never a word of the account. A leak-salted test
fails the build if one gets through.

### The boundary is inherited whole

ADR 0050's closed source list binds this feature identically. **No general
machine-activity tracking may ever be added to enrich a situation report.** Not
keystrokes, not window history, not browsing, not process inventory — not now and
not as a small extension once the shape exists. The window *inventory* is in
scope because ADR 0022 already reads it for anchors and overlays and it reports
what is on screen now; a *history* of what has been on screen is a different
product with a different consent conversation. The rule is written into
`internal/situation`'s package comment as well as here, because a boundary that
lives only in a closed ticket is a boundary that erodes.

## Consequences

- A report costs six source reads in parallel, bounded at five seconds together,
  plus one bounded model call for one sentence — and only when there is something
  notable to word. A second ask inside thirty seconds costs a mutex.
- The Failing rank is honest but lossy across a restart, which is what the
  up-front caveat is for. Persisting the activity ring stays out of scope and a
  separate decision, exactly as ADR 0050 left it.
- Two sources share one focus snapshot per report. That is not only a saving:
  two snapshots a second apart could disagree about the same thread, and a report
  that contradicts itself between its first line and its fourth is worse than one
  extra compositor call. They now read it concurrently, which the existing mutex
  already makes safe.
- `internal/briefing`'s prompt contract was refactored onto `internal/sentence`
  with no behaviour change; its own tests are the proof.
- The Automations tab has no per-row reveal, so a reminder or schedule link opens
  the tab and stops there. The daemon labels the action accordingly ("Show in
  Automations"), which makes it a weaker promise rather than a broken one.
  Row-level reveal there is a later, separate improvement.
- The alternatives were considered and rejected. Reusing the briefing's
  `since, now` source interface would have imported its refusal to compose
  without a window, which is the one thing a report about now must never do.
  Letting the model write the whole report would make the no-extrapolation rule
  unverifiable, and a rule that cannot be checked is a request. Putting the
  report in the Focus tab would have filed a question about the whole machine
  under the threads. Inventing a second navigation for the lines would have given
  this machine two answers to "is that thing still there?".
