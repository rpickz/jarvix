# ADR 0065 — Jobs: work that outlives a conversation, inside a scope the daemon enforces

**Status:** accepted

## Context

Every capability Jarvix had before this lived and died inside one exchange.
Tools, routines, scripts, advisors, window control, the situation report — all
of them are answers to a turn, and the turn ends. That is what makes Jarvix an
assistant you direct rather than a machine you manage, and it is why a
*direction* could not be given at all. "Get the CI green", "tidy my downloads",
"set up the new laptop" are not commands; there was no object to hold one.

Issue #195 records the user's framing, and #200 is its second slice, after the
situation report (ADR 0061) and alongside review-and-undo (ADR 0064). The
decisions below are theirs, taken on 2026-08-29 and binding on this design:

- **Act freely within a per-job scope**, stated back when the job starts and
  **enforced daemon-side** — never merely described to the model.
- **Irreversible actions still stop at the gate**, however long the job has run.
  Because nobody may be present, the job **parks on the question** rather than
  blocking a session; approving it later resumes from the checkpoint.
- **A blocked job parks; it never interrupts.** Its place is *state*, not a
  paused goroutine, so it survives a restart, and it surfaces where the user
  already looks.

**The binding constraint is not architecture.** It is model reliability over
long unsupervised work, and this project has the scar: #71 was a small model
narrating actions it never performed. Autonomy widens the blast radius of that
failure from one wrong sentence to a machine acted upon while nobody watched.
The design answer is not a better prompt. It is bounded scope, frequent
checkpoints, daemon-side enforcement, and reports whose facts are **gathered
rather than recalled** — and, where nothing can be gathered, an admission.

## Decision

### The job model

A **job** (`internal/jobs.Job`) is a first-class, durable thing:

| field | what it is |
| --- | --- |
| `ID` | `j3`, minted by the store, never reused |
| `Name` | the user's short handle — up to four words, so it can be said |
| `Goal` | the direction **in the user's own words, verbatim** |
| `Scope` | the boundary, fixed at creation and never widened |
| `State` | `ready` · `running` · `parked` · `done` · `stopped` · `failed` |
| `Question` | what a parked job is waiting for, with the step it stopped on |
| `InFlight` | the step dispatched and not yet finished |
| `Ledger` | every step, oldest first — the checkpoint and the report's only source |
| `Steps` | how many of `MaxSteps` (40) it has spent |

The goal is stored verbatim and never paraphrased. A job that had rewritten its
own instruction could not afterwards be audited against it.

Six states, and the set is closed, because each is a different sentence to the
user and a seventh would be a state nobody could be told about. `Ready` is what
a restart finds; `Running` is written to disk *while* it runs, so a daemon that
died mid-step leaves evidence it did.

### What a scope is, and exactly where it is enforced

A `Scope` has three faces, because three is what the daemon can actually check:

- **`Tools`** — the exact gate identities the job may use. Exact names, no
  globs: a boundary you have to read a pattern language to understand is a
  boundary nobody reads.
- **`Roots`** — absolute directories it may touch. Containment is decided after
  **symlinks are resolved on both sides**; for a path that does not exist yet
  (the ordinary case for a job about to write one) the deepest existing ancestor
  is resolved instead, because a file cannot be a symlink before it exists and
  the only escape available is through one of its directories.
- **`Apps`** — window classes it may act on.

**A job may not start without a scope the daemon can enforce.** `Scope.Validate`
refuses one that names no tools, names no directory *and* no application, or
uses a relative path. That refusal happens before the job exists, so there is
never a job running under a boundary nobody can state.

**`Forbidden` is checked after everything else and wins.** No scope may include
`config.write_entry`, `config.delete_entry`, `config.write_setting`,
`script.run`, `intent.run`, `advisor.ask` or `desktop.manage_window`. #109 built
a wall around the configuration that governs the assistant; a job is a model
with more time, so the wall is the same height. A job that could write
`[tools.policy]` would be a job that could widen its own scope, and a boundary
the bounded thing can move is decoration. It is enforced in three places, on
purpose: at `Validate` (the job is not created), at `Scope.Judge` (so a
hand-edited `jobs.toml` cannot smuggle one in), and through `Refusing` on
`jobs.start` — which the gate consults *before any policy, including the
no-policy case*, so nothing a user writes in configuration softens it.

**The enforcement point is `Runner.once`, in this order, and every line of it is
load-bearing:**

1. **Claim** — `Ready → Running`, inside `Store.Update`, under the store's lock.
2. **Plan** — the planner proposes, bounded by the step timeout.
3. **Read the subject** — `Actor.Subject` says what the call would *actually*
   touch, from the parsed arguments and the live machine. It cannot say → the
   job parks.
4. **Judge the scope** — `Scope.Judge(attempt)`. Outside → the job **stops and
   parks with the reason, and nothing whatever has been dispatched.**
5. **Judge the gate** — `Actor.Judge`, the same registry and the same policy a
   session uses. Deny → park. Ask → park **on the question**, keeping the step.
   A resumption is judged here too, and only its *question* is skipped (#225):
   the user has answered that, so re-asking would park the job on the question
   it was just unparked from — but nobody has answered a **denial**, and a job
   that sat parked for days may have been overtaken by one. The tier in force
   when the step **runs** governs, not the tier in force when it was asked about.
6. **Do it**, and only now.
7. **Checkpoint** — the ledger entry and the state go to disk before the loop
   comes round.

The scope is checked against a struct the *daemon* filled in, never against the
model's account of its own intention. A model that says "I'll tidy ~/Downloads"
while passing a path in `/etc` has told the user one thing and the machine
another, and only the second can be enforced.

**The sharpest consequence, stated rather than discovered: `shell.run` cannot be
used inside a scoped job.** A shell command's filesystem subject cannot be read
out of its text, so it cannot be checked against a directory. `subjectOf` in
`internal/daemon/jobs.go` therefore has no entry for it, an unreadable subject
parks the job, and a directory-scoped job that proposes a command stops. This is
a real limitation and it is the honest one: the alternative is a scope that
*looks* like a boundary and lets any command through. The way forward — a
job-scoped working directory and an exec that cannot leave it — is a separate
decision and is deliberately not taken here.

### Checkpointing and resumability

The checkpoint is the ledger, and it is on disk. Every step writes an `Entry`
saying what was attempted and what is known to have happened, and the job's
state moves through `Store.Update` — the only mutation path besides `Start` —
so "every move a job makes is on disk before anything observes it" is true by
construction rather than by each caller remembering to save.

**The dispatch is written down before it happens.** `InFlight` is persisted
before `Actor.Do` and cleared when the outcome is recorded. That order is the
whole of the daemon's honesty about its own death: a process that goes away
between those two writes leaves a job with a step in flight, which the store
adopts on the next load as a ledger entry marked **unverified**. Writing after
the action instead would make the same crash look like a step that never
started. A `running` job found in a file nobody is running is not a job in
progress — it is a job whose daemon went away — so it comes back `Ready`.

Resumption is not a restart. A parked job keeps the whole `Step` it stopped on,
and `Runner.Answer(approved: true)` runs **that** step, not one the planner
proposes afresh. A planner asked the same question twice may answer differently,
and the user approved the action they were shown.

### The parking contract

| `Why` | what it means | answerable? |
| --- | --- | --- |
| `approval` | the gate is asking about an irreversible action inside the scope | **yes** — resumes at that step |
| `decision` | the planner needs the user to settle something | **yes** — the answer reaches the next plan |
| `out_of_scope` | an attempt outside the boundary; nothing was done | no |
| `refused` | the permission gate denied it outright | no |
| `unclear` | the daemon could not read what the step would touch | no |
| `stuck` | the planner failed, proposed nothing, or spent the step bound | no |

The four unanswerable ones are unanswerable deliberately. A boundary and a
denial are not opinions: the way out of an out-of-scope stop is a *new job with
a scope that admits the work*, which is a decision the user makes deliberately
rather than a yes/no they nod through. Declining an approval **stops** the job;
it does not go looking for another way round, which would be a job inventing a
plan the user has just refused.

Parking is state. Nothing holds a channel open, so a restart costs a parked job
nothing, and one job's parking cannot stall another because there is one
goroutine per running job and no shared lock held across a step.

### The honesty rules for reports

**No model composes a job's report.** Not the finished one, not the "how is it
getting on" one. Every sentence in `internal/jobs/report.go` is read back out of
the ledger the runner wrote as each step ended. The situation report's prose is
model-worded because its facts are fed one line at a time and a wrong headline
is a wrong headline; a job's report is the account of unsupervised work, and #71
says what a model does with an account it is asked to summarise.

Four rules follow:

1. **What could not be confirmed is said first.** "I did nine things and I can't
   tell you whether the tenth happened" is a different report from "I did ten
   things", and the listener must be able to tell them apart without asking.
2. **Both halves, always** — what was done *and* what could not be. A report
   naming only successes reads as if the whole direction had been carried out.
3. **Reading is not changing.** Steps and changes are counted separately, so a
   job that looked at forty files cannot describe itself as having done forty
   things. The change count comes from the undo account, not from the ledger's
   own opinion.
4. **Prose is never an ending.** The planner declares completion by calling a
   synthetic `job_finished` verb. A model that writes "I have finished tidying
   everything up" and asks for nothing parks the job as stuck. Reading an ending
   out of prose is #71 with a longer runway.

The intent line the model wrote *does* survive into a report — but only as the
label on a step the runner independently verified. The claim being made is "this
step ran and reported success"; the intent is how it is named, not the evidence.

### Where a job surfaces

**As a source, not a second mechanism.** `internal/situation` promised in its
own doc comment that "the jobs source of #195's next slice drops in beside the
rest without the composer, the ordering or the speech budget changing", and it
did: one `situation.Source` in `bindSituation`, one function in
`internal/daemon/jobs.go`, and nothing else moved. Jobs are declared **first**,
so within `NeedsYou` a parked job is said before a waiting AI session — a
session is a conversation the user can pick up whenever they like; a job has
stopped dead.

| state | rank | what the line says |
| --- | --- | --- |
| parked, answerable | Needs you | the question, in the gate's own words, plus what it has done |
| parked, not answerable | Needs you | why it stopped, plus what it had done first |
| ready / running | In progress | steps taken, changes made, and anything unconfirmed |
| done / stopped | Finished — **only since the user last looked** | the full report |
| failed | Failing | the full report |

The finished half is interval-shaped, so `Instant.Since`'s zero rule applies: a
fresh daemon with no record of a previous look reports nothing rather than
reading its whole history out as news.

Job lines carry no provenance reference. There is no window surface for a job
yet, so a reference would resolve to a link that opens nothing — the reminders
source's argument for its fired lines, verbatim.

**And as a briefing source**, which is the more consequential of the two. A
blocked job never interrupts, so the morning after is when the user finds out,
and a return briefing (ADR 0050) is a paragraph about exactly the stretch a job
will have parked in. Jobs lead there too: a job that stopped at two in the
morning has been waiting longer than anything else in the briefing. The shape
differs from the report's because the question does — one counted line per
category ("two jobs have stopped and need you") rather than one line per job,
because a briefing is a paragraph about a night and a report is an answer about
now.

### The two questions ADR 0064 left to this issue

**Is a job's reversal one confirmation, or one per step?** **One per job.** The
manager's decision is "put the deploy job back" — a single judgement about a
single piece of work — and asking it twelve times converts one decision into a
fatigue test whose twelfth answer is not a judgement. It widens nothing the gate
protects: `Undoer.Apply` still refuses per action on a denied tool identity, and
the clobber guard still refuses per file.

**May a job that is still running be undone?** **No — it must be stopped first.**
A reversal running underneath a live runner races the thing it is reversing:
restoring a file the next step is about to rewrite, moving a window the job is
about to move back. The account would end up describing a state the machine was
never in, and "I can't tell whether that stuck" is the one answer this feature
refuses to produce. `undo.apply {job}` therefore refuses for `ready` and
`running`, naming the way out. A **parked** job is allowed, because it is not
acting — and undoing it *stops* it, because resuming from a checkpoint whose
effects have just been reversed would be resuming into a world the checkpoint
does not describe.

Populating `Action.Job` was the one thing ADR 0064 said remained, and it is one
context value: `undo.WithJob(ctx, id)` installed by the runner beside the
recorder, read in `undo.Note`. All twelve places that build an `Action` are
grouped without any of them changing.

### The clockwork

`Runner.run` is **always armed** (ADR 0049). There is no branch in which it
stops selecting on its timer; with nothing ready it waits out a bounded idle
sweep, still interruptible by a wake signal and still cancelled by the
generation context. The sweep earns its keep for the sibling loops' reason plus
one of its own: `jobs.toml` is hand-editable and its header says so, and setting
a parked job back to `state = "ready"` is how a person says "carry on" with a
text editor. The supervisor is the only reader of that edit with no caller to
bring it back.

A wake signal carries no claim about what changed, and that asymmetry stays this
way round: a spurious wake costs one re-read of a small file, a suppressed wake
costs a job that never starts.

#136's boundary-reschedule lesson applies to the one transition that could
double-fire. `Ready → Running` happens exactly once per step, inside
`Store.Update` under the store's lock, and *the claim is the store write, not
the in-memory set*. Two racing wakes cannot put two runners on one job: the
second one's `Update` sees a job that is no longer `Ready` and gives it back.

### The store

One TOML document under the XDG state dir, on the discipline every durable store
here keeps (ADR 0011): atomic fsync-and-rename writes, 0600 in a 0700 directory,
stat-based hand-edit pickup, a corrupt document moved aside rather than
overwritten, ids never reused. Registered with `internal/storefault`'s shared
suite (#205) on the first day, which is what that suite was built for — a new
store inherits the promises by construction rather than re-arguing them.

Hand-editable, and more consequentially than its siblings: a scope in this file
is a grant of authority, so a person must be able to read exactly what they
granted. Degradation is towards **no jobs** rather than "the jobs I loaded last
time still stand", because a stale copy would be a runner acting on a scope the
user has just edited. A hand edit that makes a scope unenforceable, or that
gives a job a state nobody recognises, **parks** the job rather than letting it
carry on — the refusing direction, because the alternative is a typo that sets a
job running.

Two bounds. `MaxLive` (4) is a claim about how many pieces of unsupervised work
a person can hold in their head when the situation report reads them out, not
about what the machine can run. `MaxSteps` (40) stops a plan that never says
"finished"; hitting it parks the job as stuck with its whole ledger intact, so
the user can see exactly what it spent them on.

## Consequences

- A direction can be given: `jobs.start` with a goal and a boundary, and the
  boundary is stated back on the confirmation card — which is where it matters,
  because the card is what the user judges.
- Four verbs (`jobs.start`, `jobs.status`, `jobs.stop`, `jobs.answer`) and one
  event (`jobs.changed`). No new IPC methods: everything a job needs to be asked
  about goes through the situation report, which was the acceptance criterion.
- `jobs.start`, `jobs.stop` and `jobs.answer` each rewrite one small TOML
  document whole, so they are **reversible** in the account: undoing a
  `jobs.start` removes the job. It does not un-run what the job did — those
  steps carry their own records under the job's id, which is what makes "undo
  the tidy job" a different and larger request.
- The job verbs take the gate's default tier rather than a built-in one.
  `jobs.start` asking is exactly right: starting a job is a grant of authority,
  and the card states the scope. `jobs.stop` and `jobs.status` asking is a wart,
  and the fix is one line in `builtinToolDefaults` — allow-tier, on
  `desktop.release_window`'s argument that refusing to give up power protects
  nobody. It is not taken here only because `internal/tools/policy.go` was being
  changed under another issue at the same time; it is the first follow-up.
- The situation report gained a rank-leading source and lost nothing. Its
  stub-source test, which proved a new source needs no change to the composer,
  turned out to be true of a real one. The return briefing gained one the same
  way — one `briefing.Source` and one adapter.
- A job cannot run a shell command. See the decision above; this is the price of
  enforcing a scope rather than describing one, and it is paid deliberately.
- No window surface, no CLI, and no deterministic voice grammar. All three are
  deferred for ADR 0064's reason: the wire and the wording exist, and adding
  them is an addition rather than a reshaping. A job is startable, askable,
  stoppable and answerable by voice through the model today.

## Alternatives considered

- **Describing the scope to the model instead of enforcing it.** Rejected on the
  ticket's own terms and on the merits: this is the design that fails silently,
  because a scope kept by a well-behaved model looks identical to a scope kept
  by the daemon right up until it does not.
- **Refusing an out-of-scope proposal and asking the model to try again.**
  Rejected. A planner that has wandered out of its scope once has told you
  something about the rest of its plan, and a retry loop would spend a step
  budget discovering it. Stopping is both safer and more informative.
- **Blocking a session on the gate, as a conversation does.** Rejected by the
  user's own decision and by arithmetic: a job may run for hours with nobody
  present, and a 30-second confirmation timeout would decline every question it
  ever asked.
- **A model-composed report with the facts fed to it**, as the situation report
  does. Rejected here specifically: a report of unsupervised work is the one
  place where an invented sentence is indistinguishable from a real one, and
  deterministic prose costs nothing a listener values.
- **Letting a job run shell commands inside a working directory.** Not rejected
  — deferred. It needs an exec that cannot leave the directory, which is a
  security decision deserving its own ADR rather than one inherited from
  whatever was convenient here.
- **One in-memory registry of running jobs beside the file.** Rejected: two
  records of the same fact disagree the first time one is written and the other
  is not, and the fact here is "what is this job waiting for", which a restart
  must not lose.
