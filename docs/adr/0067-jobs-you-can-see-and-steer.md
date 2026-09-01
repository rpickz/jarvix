# ADR 0067 — Jobs you can see and steer: surfaces over an engine, and the one tier that moves

**Status:** accepted
**Date:** 2026-09-01
**Issue:** #221 (the surfaces ADR 0065 deferred)

## Context

ADR 0065 built jobs — work that outlives the conversation that asked for it,
inside a scope the daemon enforces — and shipped the engine complete with every
surface deferred. There was no `jobs.list` verb, no window tab and no CLI, so
the only way to learn what a job was doing was to ask the model to call
`jobs.status`. And `jobs.status` inherited the gate's default `ask` tier, so
"what are my jobs doing" produced a confirmation prompt *before* it produced an
answer.

That is the wrong shape for this feature specifically. The premise of #195 is
that the user is the **manager** of this machine, and a manager's most basic act
is to look at what is in flight without asking permission or phrasing a request
well. It is also the one place the user has been most emphatic across this
project: administration belongs in the UI. A job that can only be observed by
successfully talking to a model is a job you cannot manage on the day the model
is the thing behaving oddly.

Nothing about what a job *means* is reconsidered here. The six states, the
scope, the `claim → plan → Subject → Judge → gate → do → checkpoint` order,
parking as disk state and the ledger-derived report are ADR 0065's and are
untouched. This decision is about what the daemon has to *say* before a surface
can show any of it, and about one tier.

## Decision

### `jobs.list` is a new verb rather than a shape over `jobs.status`

`jobs.status` answers for the ear: one spoken paragraph, chosen and joined for a
listener who cannot scroll. A listing is read, by two different clients, one of
which draws controls. Squeezing both through one verb would have made the spoken
answer the constraint on the window, and the first field the window needed that
speech does not — `controls` — would have started composing prose nobody hears.
So there are two, and they cannot drift: every sentence in both is composed in
`internal/jobs` or in `internal/daemon/jobsurface.go` from the same job.

### Every field is a finished sentence, including the goal

ADR 0013 with no exceptions, and ADR 0066's rule about lead-ins carried over.
`state` is where the job stands *and* how long it has been there, in one
sentence: a client reading this over a socket has no clock it can measure the
daemon's with, and a state vocabulary is a vocabulary. `goal` carries the user's
own words verbatim **inside** a sentence the daemon wrote, because a client that
had to introduce it would be supplying a lead-in. `scope` states all three faces
of the boundary. `progress` counts steps and changes separately, from
`Job.Progress`, which is `jobs.status`' own arithmetic exported rather than
re-derived.

There is deliberately no raw state word on the wire. A surface handed `"parked"`
would switch on it, and the branch it wrote would then need wording.

### The ledger never travels

The store's own rule for its event (ADR 0065) applied to a read: a ledger line
holds what a tool *said*, which for a job that read a file is the contents of
the user's work. What travels is the account composed from it. That is also why
`report` is present only once a job has ended — a report composed mid-flight
would be a progress note wearing a conclusion's clothes.

### `controls` **is** the eligibility, and it is worded once

The listing sends the actions that would actually work right now, in order, with
their labels and their accessible names. A job with nothing to press has an
empty list and therefore no buttons — not dimmed ones — which is ADR 0066's
finding about dead affordances plus a reachability consequence: the shared
collection row skips an empty label in the focus chain, so withholding the offer
withholds the tab stop too, and a keyboard user never lands on a control that
could only refuse.

The offers come from `Job.StopOffer` and `Job.AnswerOffer`, which are new
methods on `Job` beside the actions they mirror — `Undoer.Offer`'s arrangement,
restated for work rather than for reversal — and `Runner.Stop` and
`Runner.Answer` refuse with *those* sentences. One function, two callers, so a
control that is withheld and an action that is refused cannot explain the same
policy differently. `internal/jobs/report_test.go` pins the pairing by comparing
the runner's error against the offer's sentence.

`approve` and `answer` are the same verb wearing two labels, and the split is
the parked question's own. An approval the gate demanded is a yes about an
action the user has already been shown, so the press *is* the whole answer. A
decision the planner could not settle needs the user's own words, so that
control carries `words: true` and a `field_label` — because a label is wording,
and because working out from a job's state whether a text box belongs on the row
is exactly the decision QML must not make.

### The verbatim detail, and how it is obtained

#200's contract is that approving a parked gate question from the window shows
the same verbatim detail a session's confirmation card shows. The card's
`command` comes from `tools.Verdict.Command`, which `jobActor.Judge` discarded:
only the generated *question* was kept on the parking.

Storing it would have meant a new field in `jobs.Question` and a change to the
on-disk format for a fact the surface can recover exactly. So the detail is
re-derived from the step the job kept whole, through a new
`Registry.ConfirmationDetail` — the `Confirmable` seam asked **directly, without
a tier**.

Going back through `Check` was the obvious alternative and is wrong.
`CheckWithGrants` consults `Confirmable` only at the ask tier, because only the
ask tier has a question to word; a user who re-tiered the tool after a job
parked would blank the detail under a question the job is still parked on.
Asking the tool is tier-independent and is a pure function of the call's
arguments — exactly as `Verdict.Command` is — so the string a step yields is the
string the card showed when that step was judged.

Only `WhyApproval` carries a detail. The other five parking reasons are not
questions about a pending call: a boundary, a denial, an unreadable subject and
a stuck planner have nothing a card would show, and a planner's own question to
the user is already the whole of what it is asking.

### `jobs.stop` and `jobs.answer` are verbs, and neither has a card

ADR 0066's argument restated: the confirmation card exists for something the
*model* asked to do (ADR 0053). These are the manager's own instruction, given
by hand, on a row that names the job and says what pressing it will do.

Approving here is **not a way round the gate**. It is the gate being answered:
the job parked *because* the gate demanded a confirmation, the question travels
verbatim with the detail underneath it, and `Runner.Answer` then leaves the step
on the question so the runner's resumption branch executes exactly that action
rather than planning a new one. A planner asked twice may answer differently,
and the user approved the action they were shown.

A refusal is a normal reply carrying its sentence — `{refused: true}` — never an
error code, on `undo.apply`'s precedent: a job that had already finished is not
a fault, and a surface that saw a `-32602` would have to invent an explanation.

Neither verb writes to the account. The account is what **Jarvix** changed in
the user's name (ADR 0064); a manager halting their own job by hand is not
Jarvix acting unattended. The `jobs.stop` and `jobs.answer` *tools* still record,
because there it was the model that acted.

### One tier moves: `jobs.status` becomes allow

`desktop.release_window`'s argument, restated for a read: **a question about
work in flight must not cost a confirmation.** It is the reads' argument —
`memory.search`, `conversations.search`, `situation`, `briefing`, the three
`config.*` reads — with one extra clause, which is that the answer is composed
from a ledger the daemon wrote as each step finished. The tool carries no
argument beyond a name and cannot change a job, widen a scope or reach the
machine, so there is nothing here a question could protect.

Its three siblings do not move and are named in the comment as not moving.
`jobs.start` is a grant of authority and states its scope back on a card;
`jobs.stop` ends work the user asked for; `jobs.answer` approves the very thing
a job parked on, which is irreversible often enough that parking exists at all.
All three take the policy default, and a user who disagrees writes
`[tools.policy.tool]."jobs.stop" = "allow"`. `[tools]`, `[advisors]` and `[ai]`
remain structurally unreachable from a job (#109's wall, `jobs.Forbidden`),
which this change does not touch.

### The window is a placement and nothing else

A twelfth tab beside Activity, Situation and Account, on the Account tab's
argument extended: those three answer "every event as it happened", "where
things stand now" and "what actually changed", and this one answers "what is
still going". It is built out of the established furniture —
`JarvixCollectionRow`, `JarvixEmptyState`, `JarvixFormField` — so a job reads
exactly like a routine or a recorded action does in its own tab.

The row uses the collection row's four slots deliberately: `title` is the
standing, `subtitle` is what the job is waiting for, `detail` is the monospace
block this design system reserves for values that must not be reworded, and
`meta` is the progress. That puts all four into the row's accessible name, so a
screen-reader user standing on the Approve button has heard the standing, the
question, the verbatim detail and the progress before pressing it.

State is carried in the daemon's sentence rather than in colour, which matters
here for the same reason it does in the account and one more: "waiting on you"
and "stopped and needs you" are two different things to do, and a person who
cannot tell two greys apart must be able to tell them apart.

Reading comfort (`ui.line_spacing` / `text_size` / `letter_spacing`) does not
scale this tab, following the existing rule ADR 0066 records rather than
extending it.

## Consequences

- Three verbs: `jobs.list`, `jobs.stop`, `jobs.answer`. No new event —
  `jobs.changed` already fires on every state change, is published after the
  store's write commits, and carries identity and shape only, so it is exactly
  the re-read trigger a listing needs.
- `Job` gains `Title`, `Progress` (was `progressClause`), `StopOffer` and
  `AnswerOffer`. `Runner.Stop` and `Runner.Answer` now refuse in the offers'
  words; `endedWord` moved to report.go beside them.
- `Registry.ConfirmationDetail` is a new, tier-independent read of the
  `Confirmable` seam. Nothing else uses it yet; the session still takes its
  detail from the verdict, which is where the tier belongs.
- `jarvix jobs`, `jarvix jobs stop <name>` and
  `jarvix jobs answer <name> yes|no|<words>`. `yes` and `no` are the whole
  vocabulary for an approval; anything else is a decision's answer and travels
  verbatim.
- The window gains a thirteenth tab. The strip is a `Flow` and already wraps.
- `internal/desktop/jobsqml_test.go` bans the tab from wording a standing,
  phrasing an elapsed time, switching on a state word, labelling a control,
  dimming one instead of withholding it, reading the step a job parked on, or
  scaling with the reading-comfort settings.

## Alternatives considered

- **Shaping `jobs.status` into a listing.** Rejected above: one verb serving a
  listener and a listing makes the spoken answer the constraint on the window.
- **Storing the confirmation detail on `jobs.Question` at park time.**
  Considered seriously, and it is the more obviously correct-looking answer —
  the card's detail captured at the moment the gate produced it. Rejected
  because it changes the on-disk shape of a job for a fact that is a pure
  function of the step already stored there, and this issue is explicitly
  surfaces over an unchanged engine. If a future gate ever composes a detail
  from something other than the call's arguments, this becomes the right answer
  and the field should be added then.
- **Re-deriving the detail through `Registry.Check`.** Rejected: `Confirmable`
  is consulted only at the ask tier, so the detail would vanish from a question
  the job is still parked on the moment somebody re-tiered the tool.
- **Booleans for the controls (`can_answer`, `can_stop`) with the labels in
  QML.** Rejected: "Approve" and "Send your answer" are different labels for the
  same verb, chosen by the parked question's kind, and a window choosing between
  them is a window making that decision. Sending the list makes the eligibility
  and its wording one thing.
- **A confirmation card in front of Stop or Approve.** Rejected on ADR 0066's
  reasoning: the card is for what the model asked to do. Approving here *is* the
  answer the gate asked for, and putting a second question in front of it would
  ask the user to confirm their own sentence.
- **Re-tiering `jobs.stop` as well, since stopping is safe.** Rejected. Stopping
  is safe for the machine and not for the work: a job halted mid-plan leaves
  whatever it had done half-finished, and "stop the deploy job" said to a model
  that misheard which job is a sentence the user should get to hear back. The
  window and the CLI already reach it without a card, which is where the
  friction actually mattered.
- **Marking a row's new state in the window after a successful stop or answer.**
  Rejected for ADR 0066's reason, which is sharper here: an approved job resumes
  from its checkpoint and may park again on the very next step, so the only
  honest answer to "where is it now" is a fresh read.
