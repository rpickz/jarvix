# Soaking, the coverage floor, and the two test guards

The PR gate runs `go test -race -count=2 ./...`. In one week, four real defects
walked straight through it:

| | what it was | what found it |
| --- | --- | --- |
| #149 | a data race on a test fake's exported field (`tts.Fake.LastRequest`), written by a production goroutine while a test read it | a human, reading the fake |
| #155 / ADR 0049 | the focus scheduler's park branch — a loop that parked forever and stopped reading its own store. The same defect again in `internal/reminders` (#166) | `GOMAXPROCS=2 -race -count=25`, run by hand under CPU load |
| #156 | supersession / replay / confirm tests that passed **only** under `-race`, because the detector changed their timing | running the suite without `-race` |
| #170 | an archive adoption window: `awaitAppend` observed an append and assumed the engine had adopted the id | `-race -count=50` whole-package, 2 failures in 100 |

Coverage was 83.3% throughout and did not help with any of them. These are
ordering faults, and statement coverage does not measure ordering.

So there are three things now, and this page is what each is for.

---

## 1. The soak

`.github/workflows/soak.yml` runs nightly at 04:00 UTC, and on demand from the
Actions tab. It is **not** on the PR path: the gate's promise is a verdict in
minutes, and these runs take tens of them.

Per package — `internal/session`, `internal/daemon`, `internal/focus`,
`internal/reminders`, `internal/conversations`, `internal/automation` — it runs
three modes:

| mode | command | what only it catches |
| --- | --- | --- |
| `repeat` | `go test -race -count=50 ./internal/PKG` | #170. **Whole-package** is load-bearing: the fault needed the rest of the package running alongside to perturb the schedule. A `-run=TheOneTest` soak found nothing. |
| `constrained` | `GOMAXPROCS=2 go test -race -count=25 ./internal/PKG` | #155 and #166. Fewer processors means goroutines queue behind each other instead of running side by side, which is what surfaces a loop that has stopped reading its own store. |
| `unraced` | `go test -count=50 ./internal/PKG` | #156. The detector changes timing, and those tests were green under `-race` and red without it — which a race-only soak cannot see, by construction. |

### Running one locally

Everything above is `scripts/soak.sh`, and CI runs that script, so the local
command and the nightly command are the same command:

```bash
make soak                                     # every mode, every package (slow)
make soak-repeat      SOAKPKG=./internal/session
make soak-constrained SOAKPKG=./internal/focus
make soak-unraced     SOAKPKG=./internal/daemon

scripts/soak.sh repeat ./internal/session     # the same thing, without make
scripts/soak.sh all    ./internal/reminders
```

Or, if you would rather type it out — this is the whole of it:

```bash
go test -race -count=50 -timeout=40m ./internal/session
GOMAXPROCS=2 go test -race -count=25 -timeout=40m ./internal/focus
go test -count=50 -timeout=40m ./internal/daemon
```

Turn the counts down while iterating, and back up before you believe a
negative result:

```bash
SOAK_COUNT=5 scripts/soak.sh repeat ./internal/session
```

Soaking is more effective on a busy machine. #155 was found under load; an idle
laptop is the environment these faults hide in. `stress-ng --cpu $(nproc)` in
another terminal is a fair imitation of a shared CI runner.

### Reading a failure

Output goes to `soak-logs/MODE-PACKAGE.log` and is **never** truncated. That is
not a stylistic preference: the first sighting of #170 was piped through `tail`,
the evidence was lost, and it took another day to see the failure again. Nothing
in the script or the workflow pipes through `head` or `tail`, and CI keeps every
log as an artefact for 30 days whether the run passed or failed.

`-v` is deliberately **not** passed, which is what makes "print the whole log"
affordable: `go test` is silent on a passing run, so a clean log is a header and
four lines, and a failing one is exactly the failure and its context.

Each log opens with the reproduce context — the exact command, `GOMAXPROCS`, the
Go version, and the commit — because `go test` has no seed to record. The
variable is the schedule, so what you need to reproduce is the command and the
machine, not a number.

A hang is bounded by `go test -timeout` rather than by the runner's clock,
on purpose: on expiry Go panics and dumps every goroutine's stack, and for a
park-forever fault that dump **is** the diagnosis. A wall-clock kill would take
it away and leave you with "the job was cancelled".

### When a soak goes red

1. Read the `--- FAIL` block in the artefact. It is complete.
2. Re-run the same command locally from the log's `reproduce:` line.
3. If it will not reproduce, raise the count and add load. Two in a hundred is
   a normal frequency for this class of fault.
4. Fix the test's synchronisation, or the production ordering, and say which in
   the commit message. #170's fix was in a test helper because production was
   already correct; #155's was in production because it was not.

---

## 2. The coverage floor

`coverage.floor` holds one number: the total statement coverage this repo will
not go below. `scripts/coverage-ratchet.sh` measures the total and fails if it
has dropped more than **0.5 percentage points** below that line. It runs as its
own job in `.github/workflows/ci.yml`, beside the race pass rather than after
it, so it costs the gate no wall-clock it did not already spend.

```bash
make coverage-ratchet          # measure and compare
make coverage-ratchet-raise    # print the line to paste, if it is time
```

**Raising it is a deliberate committed act.** Nothing writes the file for you;
`--raise` prints the number and you paste it in, in the change that earned the
increase, so a reviewer sees the floor move. A floor that follows the
measurement is not a floor — it makes today's number the rule and leaves the
next drop with nothing to hit.

The 0.5pp tolerance is there because the total moves by a tenth or two on
unrelated changes — a refactor that deletes covered code, a new error path in a
rarely-hit branch — and a gate that reddens on noise is a gate people learn to
re-run rather than read.

If the ratchet fails, the fix is to cover what the change added. If the drop is
genuinely correct, lower the floor **in the same commit** and say why in the
message.

And keep it in proportion: coverage caught none of the four defects at the top
of this page. What the floor buys is narrow and real — the number cannot slide
quietly while the suite grows.

---

## 3. The two guards

`internal/testdiscipline` is the deterministic half of the answer: two shapes
that can be recognised by reading the source, caught on the PR that introduces
them instead of at 04:00 the next morning. Both are ordinary Go tests, so they
run in the gate.

### Derived state read after only its cause

> **`TestNoDerivedStateReadAfterOnlyItsCause`**

The shape: a test does something, observes the **cause** of a state change, and
then samples the state. The observation proves the cause happened. It does not
prove the effect has landed, because something else — a watcher goroutine, the
tail of a flush — is what lands it. The test passes on an idle laptop and fails
on a loaded runner, which is the worst failure mode there is: it teaches the
author that the gate is flaky rather than that the test is wrong.

Two rules, both from real failures:

- **activity feed rows** (#167). Rows are derived by the daemon's own subscriber
  from the events it watches, so an event proves the daemon spoke, never that
  the row has been appended — `docs/ipc.md` says exactly this of every
  `activity.row`. Waiting for `waitForEvent` and then calling `activity.get`
  races the watcher. Wait for `waitForActivityRow` / `waitActivityRow` instead.
- **the archived conversation id** (#170). `conversations.Fake` notifies from
  *inside* `Append`, so the op proves the turns are stored — not that the engine
  has adopted the id, which happens after `Append` returns. Take the engine's
  read barrier, `SyncArchive`, before reading `ActiveConversationID`. (A helper
  that calls `SyncArchive` on your behalf counts, which is why `awaitAppend`
  satisfies the rule today.)

**`conversation.get` is deliberately not a rule.** It reads the engine directly,
and an exchange is committed before `session.finished` publishes, so a client
that has seen the event can always read the record. A rule over it would be
pure false positives — a dozen in the tree today — and would be deleted within a
week, taking the useful rules with it. The distinction that matters is not "is
this a read?" but "is this read served by a goroutine other than the one whose
event I waited for?".

If a report is genuinely wrong, say so where the code is:

```go
// testdiscipline:allow this samples the feed to prove a NEGATIVE — a row that
// has not landed weakens the sweep by one row and can never fail it.
```

The reason is required. An unexplained marker is itself reported, because the
exceptions have to be argued rather than accumulated. There is exactly one in
the tree.

### Exported mutable state on a test fake

> **`TestNoExportedMutableStateOnTestFakes`**

#149's shape: `tts.Fake.LastRequest`, an exported field the fake assigned inside
`Speak` and a test read from another goroutine. The fix is
`tts.Fake.Last()` — unexport the field, add an accessor that takes the same
mutex the write does, and an unsynchronised read becomes a compile error rather
than a flake.

The rule fires on **an exported field that one of the type's own methods
assigns**, on a type whose name says `Fake`, `Stub`, `Mock` or `Spy`. That is
precisely the recording-field population. It leaves *scripting* fields alone —
`Response`, `Chunks`, `Fail`, `BeforeToolCalls` and their kind, written once by
the test at construction and only read by the fake — because a scripting field
is never assigned by the fake. Channel and func fields are excluded even when a
method writes them: a send is not a data race, and the notifying-fake pattern
(`conversations.Fake.Ops`, `history.Fake.SaveGate`) depends on those channels
being reachable from the test.

Three fields predate the rule and are listed in `FakeFieldExemptions` with the
fix beside each: `ai.Fake.LastRequest`, `ai.Fake.Requests`, `stt.Fake.LastInput`.
They are the real thing, not false positives; unpicking them is roughly a
hundred mechanical call-site edits across twenty-five files, which did not
belong in the change that introduced the guard. The list is a ratchet: a test
fails if an entry stops matching anything, so a paid-off debt has to be deleted
rather than left to make the list look longer than it is. It may shrink. It must
not grow.

### Would any of this have caught the four?

Asked honestly, and checked by running the scanners over the pre-fix source
taken out of git history rather than by reasoning about them:

| defect | caught by | how |
| --- | --- | --- |
| #149 | the fake guard | reports `tts.Fake.LastRequest` at `internal/tts/fake.go` as it stood before `ec1a10e` |
| #155 / #166 | the soak, `constrained` mode | **no guard catches these.** A parked loop is not a shape in the source; it is a shape in the schedule. `GOMAXPROCS=2 -race -count=25` is what found them, and it is now a job |
| #156 | the soak, `unraced` mode | **no guard catches this either.** "Passes only under `-race`" is only visible by running without it |
| #167 | the derived-state guard | reports `TestAPreApprovedRunAppearsInTheActivityFeed` in `internal/daemon/approvals_test.go` as it stood before `285a042` — the exact test that commit repaired |
| #170 | the derived-state guard, and the soak | reports `TestResetDetachesAndTheNextThreadIsANewConversation` in `internal/session/archive_test.go` at `798d8d2` — the exact test that flaked — plus four sibling reads carrying the same latent window |

Two of the five, then, are caught only by the soak, and that is the honest
division of labour: the guards catch what is legible in the source, and the
soak catches what is only legible in a schedule.

### Changing a guard

Both scanners have fixture pairs under
`internal/testdiscipline/testdata/`: a `*_bad` directory holding the historical
defects, which the guard's own test asserts are all reported, and a `*_good`
directory holding every legitimate use of the same calls, which it asserts are
all silent. Change a rule and both halves must still hold. The `_good` fixture
is the important one — a guard that cries wolf gets deleted, and takes its true
positives with it.
