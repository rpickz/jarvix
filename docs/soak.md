# Soaking, the coverage floor, and the two test guards

The PR gate runs `go test -race -count=2 ./...`. In one week, four real defects
walked straight through it — and the soak's first scheduled nights added two
more that had been walking through it for months:

| | what it was | what found it |
| --- | --- | --- |
| #149 | a data race on a test fake's exported field (`tts.Fake.LastRequest`), written by a production goroutine while a test read it | a human, reading the fake |
| #155 / ADR 0049 | the focus scheduler's park branch — a loop that parked forever and stopped reading its own store. The same defect again in `internal/reminders` (#166) | `GOMAXPROCS=2 -race -count=25`, run by hand under CPU load |
| #156 | supersession / replay / confirm tests that passed **only** under `-race`, because the detector changed their timing | running the suite without `-race` |
| #170 | an archive adoption window: `awaitAppend` observed an append and assumed the engine had adopted the id | `-race -count=50` whole-package, 2 failures in 100 |
| #179 | a daemon harness that called a daemon ready as soon as its socket existed, so tests reaching past the socket raced the rest of `Run`'s boot; and a leak assertion whose salt was not exclusive to the feature it guarded, so another feature's honest record was reported as a privacy leak | `-race -count=60` whole-package, 3 failures in 60 — the soak's own first find |

Coverage was 83.3% throughout and did not help with any of them. These are
ordering faults, and statement coverage does not measure ordering.

So there are three things now, and this page is what each is for. A fourth —
the store fault-injection suite — is documented below, because it is the same
argument applied to a different kind of failure: not what the schedule does,
but what the disk does.

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
5. **A leak assertion that trips is not automatically a leak.** Before believing
   the report, check that the salt is exclusive to the feature under test. In
   #179 the briefing's salt was also the text of a live reminder, so the row
   that "leaked the briefing" was the reminder feature recording its own spoken
   delivery — legitimate, and nothing ADR 0050 forbids. The guarantee held; the
   scan was unsound. Establish which before changing anything, because the two
   have opposite fixes.
6. **"The socket is up" is not "the daemon has booted."** `Run` binds the socket
   before it starts the scheduled services, so a dial can be ahead of them, and
   a test that then reaches past the socket into the daemon's own services is
   racing a boot it never waited for (#179 again: a reminder planted in that gap
   was read as a missed-while-down catch-up). One served round trip is the
   barrier — `internal/daemon`'s `awaitDaemon` takes it for every test in the
   package.

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

**The number is the CI runner's, not yours.** The gate runs on a clean runner
with none of the external engines installed (no PipeWire, whisper, piper,
kokoro), so the tests that probe for them skip and their code goes uncovered. A
development box has some of them and reports about a point higher — measured on
the same commit, 83.4% locally against 82.2% on `ubuntu-latest`. The floor has
to be the number the gate measures. If your local reading sits a point above
the line, that is why, and it is not a licence to raise it.

The 0.5pp tolerance is there because the total moves by a tenth or two on
unrelated changes — a refactor that deletes covered code, a new error path in a
rarely-hit branch — and a gate that reddens on noise is a gate people learn to
re-run rather than read.

If the ratchet fails, the fix is to cover what the change added. If the drop is
genuinely correct, lower the floor **in the same commit** and say why in the
message.

And keep it in proportion: coverage caught none of the defects at the top of
this page. What the floor buys is narrow and real — the number cannot slide
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

Three rules, all from real failures:

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
- **a turn believed to be in flight** (#215). `assistant.started` is published
  before the provider request is opened and before a word reaches the voice, so
  it proves `think()` began and nothing else. Waking, superseding, cancelling or
  reading the mid-turn conversation on the strength of it is a race with the
  whole turn: three CI timeouts came from tests that did exactly that, one where
  the turn had already finished and one where a fake's "first provider call" was
  claimed by the wrong session. Park the collaborator — `tts.Fake.SetHold`,
  `Delay = time.Hour`, a provider that closes a channel when it has parked — or
  wait for `tts.started` / `assistant.delta`. A *bounded* delay is not a
  barrier: `Delay = 50 * time.Millisecond` is a window, and the window is what
  this family is made of.

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

### Would any of this have caught the known defects?

Asked honestly, and checked by running the scanners over the pre-fix source
taken out of git history rather than by reasoning about them:

| defect | caught by | how |
| --- | --- | --- |
| #149 | the fake guard | reports `tts.Fake.LastRequest` at `internal/tts/fake.go` as it stood before `ec1a10e` |
| #155 / #166 | the soak, `constrained` mode | **no guard catches these.** A parked loop is not a shape in the source; it is a shape in the schedule. `GOMAXPROCS=2 -race -count=25` is what found them, and it is now a job |
| #156 | the soak, `unraced` mode | **no guard catches this either.** "Passes only under `-race`" is only visible by running without it |
| #167 | the derived-state guard | reports `TestAPreApprovedRunAppearsInTheActivityFeed` in `internal/daemon/approvals_test.go` as it stood before `285a042` — the exact test that commit repaired |
| #170 | the derived-state guard, and the soak | reports `TestResetDetachesAndTheNextThreadIsANewConversation` in `internal/session/archive_test.go` at `798d8d2` — the exact test that flaked — plus four sibling reads carrying the same latent window |
| #179, the reminder | the soak, `repeat` mode | **no guard catches this.** The harness called a daemon ready as soon as its socket existed, which is a fact about `Run`'s boot order, not a shape in a test's source. `TestTheHarnessWaitsForTheDaemonToAnswer` pins the repaired rule instead |
| #179, the leak assertion | the soak, `repeat` mode | **no guard catches this either.** The salt was planted on a record another feature legitimately owns, so the scan was unsound rather than mis-synchronised — no scanner can know which feature a string belongs to. The repaired test delivers that reminder on purpose, so the case is in every run rather than one in two hundred |

Four of the seven, then, are caught only by the soak, and that is the honest
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

---

## 4. The store fault-injection suite

`internal/storefault` is the shared suite every durable store runs
(issue #173): the conversation archive, the memory book, the taught
vocabulary, the focus threads, the reminders, the approval ledger and the
monitor nicknames. It is not about ordering — that is what the soak and the
guards above are for — it is about the conditions that actually break durable
files.

Six promises, each its own named subtest so a red build reads as the promise
that broke and the store that broke it:

| subtest | the promise |
| --- | --- |
| `AFailedWriteLeavesThePreviousFileAndTheMemoryIntact` | a write that fails mid-flight leaves the previous file byte-identical and the in-memory state unchanged |
| `AFullDiskIsSurfacedAndNoSuccessIsRecorded` | ENOSPC at the write seam is surfaced, no id is handed back, and the store still works once there is room |
| `ACorruptFileIsNotOverwrittenAndTheStoreSaysSo` | an unparseable document is set aside (or left where it is) but never overwritten, the store starts clean, and it discloses what it found |
| `AHandEditBetweenOperationsIsPickedUp` | an edit made between two operations lands on the next one — no restart, no watcher |
| `IDsAreNeverReusedAcrossAReload` | an id is retired with the record that held it |
| `AReadConcurrentWithAWriteNeverSeesAPartialRecord` | a reader running alongside a writer never observes half a record |

### Adding a store

Fill in a `storefault.Subject` and implement `storefault.Store` over the real
type, in the store's own package (the write seam and the on-disk shape are
both unexported, and the adapter is the one part that has to know what a
focus thread actually is). That is the whole of it — see
`internal/monitors/faults_test.go`, which joined last, was not on the
ticket's list, and needed no new assertions: the two ways it differs (it
mints no ids, and its records are single words) are declared on the Subject
rather than special-cased in the suite.

### Hermetic by construction

Nothing fills a disk, drops a privilege, or chmods a directory to provoke a
failure. Faults arrive through the store's own `write` field — the seam each
store already carries so a write can be made to fail on command — because a
real filesystem cannot be made to fail hermetically: every trick a test could
play on a temp directory is either privileged or repaired by the store's own
chmod on the next write, and a fault that needs a privileged runner is a
fault nobody runs.

### What it found

Five real defects on its first runs, all fixed at the mechanism:

- **The approval ledger overwrote an unreadable file** instead of setting it
  aside, destroying the user's approval history on the first write after any
  damage. Every sibling store moves it aside.
- **The approval ledger never picked up a hand edit** — it loaded once per
  process, and the daemon runs for days, so an edit was silently reverted by
  the next write from the cached copy.
- **The approval ledger committed to memory before the disk took the write**,
  so a refused write left it reporting a card grant, with a date, that no
  file held.
- **The archive could bury a torn line.** A failed append leaves half a line
  at the end of a transcript; both readers tolerate a torn *last* line and
  treat a bad line anywhere earlier as corruption — so the next successful
  append turned one lost turn into a lost conversation. The writer now cuts
  back to the last complete line first.
- **The archive reported a conversation being created as damaged.** Creating
  a transcript is an `open(O_CREAT)` and then a write, and the search scan
  runs without the store's lock on purpose, so a search landing between the
  two saw a zero-length file and told the user their live conversation could
  not be searched.
