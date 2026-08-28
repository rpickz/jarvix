# ADR 0049 — A scheduler loop never parks: an empty schedule is a long wait, not the absence of one

**Status:** accepted

## Context

Jarvix now runs several sibling scheduler loops, each restating the same
discipline over a different kind of event (ADR 0032's stance on reuse):
`internal/automation` over wall-clock occurrences, `internal/knowledge` over
feed refresh delays, `internal/focus` over interval check-ins and one
timebox's midpoint and close (ADR 0041), with more expected.

Two of them can always name a next moment. A cron `Spec` always has a next
occurrence; a feed always has a next refresh delay. Their loops therefore
arm unconditionally and have no other shape.

The focus loop was the first whose schedule can be genuinely *empty* — no
session, no thread with a check-in interval — and it grew a branch for it:

```go
wait, any := s.nextWait()
if !any {
    select {           // nothing scheduled: sleep until a mutation says otherwise
    case <-ctx.Done():
        return
    case <-s.rearm:
    }
    continue
}
```

Issue #152 was filed against a focus test that failed only under CPU
contention, with the loop apparently "parked with nothing armed" while a
session sat closing. It was not that. `nextWait` always considers
`ClosingAt + closingAnswerWindow` for a closing session, and that moment is
never zero, so a closing session cannot reach the park branch — the state the
issue set out to find is unreachable, and the honest answer is that the park
branch itself is the defect.

What the instrumented loop actually showed is that the loop had reached the
park branch *correctly*: the session had already expired. The loop dispatches
on entry and on every wake, always against the current clock, so a pass
nobody asked for — the entry pass, or one bought by a `Rearm` token the loop
consumed after its last `nextWait` had already read the change — can
legitimately consume the last due moment. The schedule then empties one tick
earlier than the caller expected, and the park makes that ordinary outcome
indistinguishable from a hang.

Two things are wrong with the park, and only one of them is about tests.

**It stops reading the store.** Every other entry point refreshes from disk on
its way in, which is what makes `focus.toml` hand-editable without a restart —
the file's own header promises exactly that, and the memory book's storage
contract (ADR 0025) is built on it. The scheduler loop is the one reader with
no caller to bring it back. Adding `remind_every_min = 30` by hand while
nothing else is scheduled therefore arms nothing at all, until some unrelated
verb happens to call `Rearm`. The interval is in the file, the file is
correct, and the user hears silence.

**It makes "armed" unobservable.** The park drops `<-fire` from the select
entirely, so a tick delivered to a parked loop is not late — it is never
received. Anything holding the timer seam's channel (every test in these
packages) waits out its full bound and cannot tell an empty schedule from a
wedged goroutine.

## Decision

**A scheduler loop is always armed.** There is no branch in which it stops
selecting on its timer. When nothing is scheduled, the wait is a bounded
idle sweep (`idleSweep`, one minute in `internal/focus`) rather than an
indefinite sleep, still interruptible by a mutation signal and still
cancelled by the generation context.

`nextWait` keeps reporting the truth — `(0, false)` for an empty schedule —
and the loop translates that into a wait. Whether an empty schedule is worth
re-reading, and how often, is a policy about the *store*; it is not a fact
about the clockwork, and inventing a fake moment inside `nextWait` would hide
that distinction from the next reader.

The sweep is deliberately **not** a poll for due moments. Everything with a
due moment has an arm of its own, computed exactly; the sweep exists so the
one reader that has no caller comes back to the file, and so that "armed" is
a state an observer can rely on.

Two supporting rules, learned in the same investigation and binding on every
sibling:

- **A wake signal carries no claim about what changed.** Callers signal after
  dropping the lock, so a loop routinely consumes a token for a change its
  last recompute already read. That asymmetry must stay this way round: the
  loop re-reads the whole schedule after every wake, so a spurious wake costs
  one recompute, while a suppressed wake would cost a missed firing. Do not
  "optimise" a wake channel into something that can lie.
- **A rendezvous on the timer channel is the only honest readiness
  handshake.** A side-channel that announces arming cannot distinguish an arm
  that later unwinds through the wake channel from a live one, so a waiter
  proceeds on stale readiness. #152 tried that and it does not work.

## Consequences

- An otherwise idle focus daemon wakes once a minute to stat one small file.
  `refreshLocked`'s mtime-and-size check makes an unchanged file cost one
  `stat` and no I/O, and `dispatchDue` with nothing due does no work at all.
  That is the whole price.
- A hand-edited check-in interval now arms within the sweep, without a
  restart and without an unrelated verb — the store's documented promise,
  finally true for its last reader. It is adopted from *now*, the ADR 0032
  missed-while-down stance: an interval that appeared by hand has no missed
  ticks to back-fire.
- Tests of these loops can treat the timer seam's blocking send as a
  handshake that always completes, which is what makes assertions after it
  statements about a dispatch that has happened.
- `internal/automation`, `internal/knowledge` and `internal/routine` already
  satisfy this — they never had a park branch. The rule is written down so the
  next sibling does not invent one, which is easy to do the first time a
  schedule can be empty.
- The alternatives were both worse. Widening the waiter's bound treats an
  indefinite park as slowness, which it is not — a 30-second bound failed
  identically. Making the loop re-arm only after a "real" mutation needs a
  wake signal that can be trusted to be complete, and the second supporting
  rule above says it cannot.
