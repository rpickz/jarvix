# Properties, mutation testing, and the first report that was read

Several of the pieces that decide whether Jarvix may run a command, or when a
reminder fires, are **classifiers over an unbounded input space**:

| component | a wrong answer is |
| --- | --- |
| `internal/tools/policy.go` — the shell classifier, its segmentation, the risk words and the allow patterns | a command running that nobody approved |
| `internal/tools/approvals.go` — the remembered-approval refusal matrix, and `judgePattern`, where the card's derived route and the Approvals view's typed route both terminate | a standing grant that covers more than the user was shown |
| `internal/intent/when.go` — the spoken-time parser | a reminder at the wrong hour, confirmed in words that sounded right |
| the sentencer in `internal/session/speaker.go` | half a rune sent to a speech engine, or a sentence cut where nobody said one ended |

Every one of them was tested by **example**. A table of examples proves the
cases somebody thought of, and the gap between that and "no input slips
through" is exactly where a permission bypass or a mis-scheduled reminder
lives. Issue #172 closed that gap from two directions at once: state the
contracts as **properties** and attack them with generated input, and make the
**mutation job** — which existed, ran manually, and whose output nothing acted
on — into a report somebody reads.

Both find things. The properties found three defects on their first runs, and
the first mutation report that was read cost eleven decisions.

---

## The properties

A property is a sentence that must hold for *every* input, written so that
generated input can attack it. They live beside the tables rather than
replacing them: a table says where a bound is drawn, a property says what is
true either side of it, and the mutation report below is what happens when only
one of the two exists.

### The shell classifier — `internal/tools/classifier_property_test.go`, `fuzz_test.go`

| law | how it is expressed |
| --- | --- |
| No segment still contains a separator | every segment of every generated command is checked for `;`, `&`, `\|`, newline, `)`, backtick |
| Splitting loses nothing but separators | the segments are re-joined and compared against an **independent character filter** over the input — not against the splitter again, so a mutation inside `FieldsFunc`'s predicate shows as a difference rather than as two matching wrongs |
| No command containing a separator is ever classified from a single segment | the whole line's verdict must be **at least as strict** as every one of its segments judged alone. Checked in that direction only: deny runs against the raw line too (a fork bomb is nothing but separators), so the whole is allowed to be *stricter* than its parts |
| A risk word in command position is never silent | the pieces are cut **by the test**, on the separators a shell recognises, and the leading word of each is read by the test — no classifier helper is borrowed, so a mutation in either would show |
| Deny survives every decoration | the shipped deny shapes, wrapped in quoting, redirects, substitutions, and prefixed with allow-listed commands |
| Silence means every segment earned it | an allow verdict implies no segment matches a deny rule, a risk regex, or a risk word |
| A verdict says why it asked | `Summary` is set exactly when the decision is ask; `PreApproved` implies an allow and a named pattern |
| Classification is a pure function of the command | the gate is consulted twice for one call — two answers would mean confirming a different command from the one that runs |

### The approvals matrix — same two files

| law | how it is expressed |
| --- | --- |
| **No pattern the matrix accepts can be a prefix of a command the deny list or the risk words refuse** — the security invariant | statically: an accepted pattern's head is never a risk word, an `mkfs*`, a refused binary, or prefix-related to a configured deny rule, and the pattern never covers a refused shape. Dynamically: the pattern is added to `shell_allow` and every generated command under it is re-judged — a grant may never turn a deny into anything else, and any allow it produces implies no segment was refusable |
| The derived (card) and typed (form) routes agree | #164 pinned this over the policy's own tables; the property extends it to generated shapes. Both routes terminate at `judgePattern`, so an offered pattern must be offered identically by the other route, and a refusal must carry the identical sentence |
| ...and diverge only where they are documented to | a typed rule refuses a non-command word rather than truncating it, and has no `maxPatternWords` cap. Both make the typed route **stricter**, which is the safe direction, and both are pinned so they stay deliberate |
| Every refusal is a sentence | a refusal that names a pattern, or an offer that carries a reason, is a card that contradicts itself |

### The spoken-time parser — `internal/intent/when_property_test.go`

| law | how it is expressed |
| --- | --- |
| A parsed time always resolves to a moment in the future | every hour × minute × anchor × reading the grammar can produce, against seven clocks including both edges of a day, a year boundary and a leap day |
| A twelve-hour reading picks the **next** occurrence | the resolved moment reads as the spoken hour, is never more than twelve hours away, and the previous occurrence — exactly twelve hours before it — has already gone by. The weaker phrasing, "in the future and the hour matches", passes on an implementation that skips a day |
| Round-tripping a resolved moment through its spoken form re-parses to the same moment | every resolved clock reading is said back with `SpokenClock` plus its meridiem and re-resolved; every reachable relative delay is said back with `SpokenDuration` and re-parsed. The relative domain is enumerated from the **number table and the unit words**, never from `SpokenDuration`, so the two sides of the round trip have independent origins |
| Unparseable input never yields a time | a refusal must carry the zero `When`, not merely `ok == false` — a caller that forgot to check would otherwise schedule midnight |

### The sentencer — `internal/session/fuzz_test.go`

`FuzzSentencer` already existed and already asserted that no non-whitespace
content is lost, duplicated or reordered. It was **extended**, not duplicated:

- **the split does not depend on the chunking** — the same text arriving byte by
  byte, in the fuzzer's chunk size, or all at once produces the same sentences.
  Scoped to buffers under `maxSentenceRunon`, and that scope is a real
  exemption rather than a convenience: the run-on rule exists so a wall of
  unpunctuated text cannot hold speech hostage, and it necessarily cuts
  wherever the buffer crossed the cap, which is a chunk boundary. Past the cap
  the weaker law still holds — the *content* never depends on chunking.
- **a rune that arrived whole leaves whole** — for valid input, every emitted
  sentence is valid UTF-8.

---

## The three defects the properties found

### 1. Jarvix could not read back what it had just said (`parseCount`)

`SpokenDuration` renders 65 minutes as **"an hour and five minutes"** — that is
the sentence the confirmation speaks — and `parseRelative` could not read it.
`parseNumber` has no entry for "an", so only the synonymous "one hour and five
minutes" matched. A user repeating the assistant's own words missed the
deterministic grammar for the whole hour-and-something family and fell through
to the model: slower, and a different answer.

Fixed on the parsing side, because the parser faces speech and a person *will*
say "an hour and ten minutes". Widening what is understood cannot make any
previously-matching utterance match differently.

### 2. "at three sixty" scheduled a reminder for four o'clock

`parseClockMinutes` read a single tens-word as the minutes, and `tensWords`
runs to "ninety". So `at three sixty` parsed as 3:60 — and `time.Date`
**normalises** a minute of 60 into the next hour rather than rejecting it. The
reminder was accepted, confirmed as "at four this afternoon", and fired an hour
from where the words pointed.

A minute that does not exist is now a miss, where the caller falls through to
the model, rather than a silent carry in the calendar.

### 3. One non-ASCII space walked `rm -rf /` past the deny list

**The one that matters.** Go's `\s` is ASCII — `[\t\n\f\r ]`, not even `\v` —
while everything else in the classifier reads words with `strings.Fields` and
trims with `strings.TrimSpace`, both of which use `unicode.IsSpace`. Two
different ideas of where a word ends, inside one classifier, is a hole:

```
echo hi<U+2028>rm -rf /      →  allow    (before)
echo hi<U+2028>rm -rf /      →  deny     (after)
rm -rf /<U+2028>rm -rf /     →  ask      (before)
rm<U+00A0>-rf /              →  ask      (before)
```

The first line is the serious one: a line containing `rm -rf /` classified as
**allow**, which means it runs with no question asked. The shipped deny rule
did not fire because `(\s|$)` had nothing to match after the slash, and the
allow pattern `echo` matched because `strings.Fields` split on U+2028 and the
classifier's comment — "over-splitting can only escalate the classification
towards ask, never hide a risky part inside an allowed one" — turned out not to
be true.

The deny rules' boundary classes are now Unicode-aware (`sp` and `nonSp` in
`policy.go`). Widening those classes can only make the rules match **more**, so
it tightens the gate and cannot loosen it. `harmlessRedirect` keeps its ASCII
`\s` for the mirror-image reason: widening what counts as blank *there* would
silence more commands, not fewer.

`internal/tools/testdata/fuzz/FuzzShellClassifier/` holds all three shapes, and
`TestUnicodeWhitespaceDoesNotDefeatADenyRule` pins them by name.

---

## The mutation job

```bash
make mutate                             # the defined package set
make mutate MUTATE_PKGS=./internal/tools
```

`.github/workflows/mutation.yml` runs it weekly (Monday 03:00 UTC) and on
`workflow_dispatch`, keeps `mutation-out/` as a 90-day artefact, and prints the
report on the run's summary page. The same file runs `make fuzz-properties`,
which fuzzes the property targets for five minutes each — the PR gate replays
only their committed corpora, which takes milliseconds, so the fast gate stays
fast.

Four things changed, and all four were needed for the job to be a signal rather
than a job:

- **a defined package set** — `./internal/tools ./internal/intent
  ./internal/session`, named in `scripts/mutation-report.sh` with the argument
  for each. Deliberately not `./...`: a run over the whole module takes hours
  and produces a report too long to act on, which is the failure this replaces.
- **a schedule of its own**, rather than borrowed from `test-depth.yml`'s
  triggers.
- **a surviving-mutant report**, written to a file and retained, instead of a
  wall of scrolling output.
- **somebody reading it.** That is this section, and it is the part with no
  automation behind it.

### The rule

Every surviving mutant in the components at the top of this page is either
**killed by a new test** or **recorded here as equivalent or accepted, with its
reason**. There is no third option, because "we looked at it and moved on" is
how the previous job stopped being read.

---

## The first report, and what it cost

`gremlins v0.6.0`, `--timeout-coefficient 3`, on the commit that added the
properties above. Line numbers below are that commit's; the packages have moved
since, which is what the retained artefact is for.

| package | killed | survived | not covered | efficacy | run time |
| --- | --- | --- | --- | --- | --- |
| `internal/tools` | 859 | 107 | 316 | 88.92% | 2m10s |
| `internal/intent` | 60 | 1 | 52 | 98.36% | 13s |
| `internal/session` | 619 | 104 | 72 | 85.62% | 1m57s |

And after acting on it:

| package | killed | survived | efficacy | survivors in the components above |
| --- | --- | --- | --- | --- |
| `internal/tools` | 866 | 100 | **89.65%** | 7 → 0 |
| `internal/intent` | 58 | 0 | **100.00%** | 1 → 0 |
| `internal/session` | 613 | 101 | **85.85%** | 4 → 2, both recorded as equivalent below |

Two notes on reading those numbers honestly. The package totals wobble by a handful
between runs because gremlins derives each mutant's timeout from the baseline
test duration, and a mutant that times out is counted apart from one that
lives — `internal/intent` reports 343 timeouts against 58 kills, because
its suite runs in a tenth of a second and the derived timeout is correspondingly
tiny. The **efficacy** column and the last one are the signal; the survivor
totals are a trend, not a score.

Of the 212 survivors, **eleven** were in the components this page is about.
Every one of them was a boundary or a branch that no example happened to sit
on — which is exactly what mutation testing is for, and exactly what a property
cannot tell you: a property is silent about *where* a bound is drawn, because a
parser that rejected the digit '9' would parse fewer things and every law would
still hold.

### Killed — eight

| mutant | what it broke | killed by |
| --- | --- | --- |
| `approvals.go:75` `len(w) <= maxPatternWordLen` → `<` | a 24-character subcommand refused, or a 25-character hash baked into a standing rule | `TestAPatternWordOfExactlyTheMaximumLengthIsStillACommandWord` |
| `approvals.go:238` `len(fields) > 0` → `>=` | `proposeFor` walks off the end of the slice on a command made of nothing but `VAR=value` assignments — a panic on input a user can send | `TestACommandThatIsNothingButAssignmentsNamesNoCommand` |
| `policy.go:1023` `len(fields) > 0` → `>=` | the same walk-off in `matchWordPrefix`, reached whenever an allow list is configured | the same test |
| `approvals.go:316` `w == words[0]` → `!=` | the two refusal sentences swap places: a path-invoked head is refused as "not a command word", a flag is refused as "a path". Both sentences are read aloud, and no test looked at what it said — only that it refused | `TestATypedRuleSaysWhichWordItRefused` |
| `policy.go:781` `worst == PolicyAllow` → `!=` | the audit row for a pre-approved run names whichever shipped pattern matched first instead of the rule the user granted. Every existing test put the granted segment first, where the distinction cannot show | `TestAPreApprovedRunNamesTheRuleTheUserGranted` |
| `policy.go:812` `len(runes) <= maxSpoken` → `<` | a command exactly at the spoken bound gets an ellipsis — a user approving a command they were not told the end of, which is the one thing ADR 0014's daemon-generated summary exists to prevent | `TestTheSpokenCommandIsShortenedOnlyWhenItIsTooLong` |
| `speaker.go:68` `s[len(s)-1] < utf8.RuneSelf` → `<=` | the run-on flush cuts a multi-byte rune in half and sends it to the speech engine — issue #28, back again | `TestARunOnFlushNeverCutsARune` (and the new UTF-8 law in `FuzzSentencer`) |
| `speaker.go:71` `n <= 3` → `n < 3` | the same, for four-byte runes only | the same test |
| `when.go:496` `r > '9'` → `>=` | the parser stops believing in nines: "at 9pm" and "at 15:09" become misses and fall through to the model, silently, for one ninth of the clock | `TestNineIsADigit` |

One more was killed on the way, outside the classifiers but in the same
package: `shell.go:101` `len(result) > maxOutput` → `>=` appends "[output
truncated]" to output that was complete, so the model reasons about a reply it
has been told is partial when it is not
(`TestOutputExactlyAtTheCapIsNotTruncated`).

### Accepted as equivalent — two

Both are provably equivalent: the mutated program computes the same thing, so
no test can distinguish them and writing one would be writing a test that
asserts an implementation detail.

- **`incompleteTail`, `n <= len(s)` → `n < len(s)`.** This changes
  `incompleteTail` only when the buffer is three bytes or shorter, because
  that is the only length at which `n` can reach it. A buffer that short cannot
  trip the run-on rule (`maxSentenceRunon` is 240) and cannot contain a
  sentence terminator in its held bytes — every terminator is ASCII and every
  held byte is not. So the emitted sentences and the retained remainder are
  byte-identical either way.
- **The speaker's turn floor, `sp.floor < u.turn` → `<=`.** The body is
  `sp.floor = u.turn`. When the two are equal the assignment writes the value
  that is already there.

### The other 201

They are in `desktop.go` (19), `windowmatch.go` (17), `configadmin.go` (11),
`typing.go` (10), `advisor.go` (9) and the rest of `internal/tools`; and in
`engine.go` (24), `speech_lexicon.go` (17), `speech_numbers.go` (13),
`timings.go` (11), `tier.go` (11) and the rest of `internal/session`. They are
real, and they are not triaged here, because #172's scope is the components at
the top of this page and a triage of 201 mutants written in one sitting would
be a list nobody reads — the thing this whole change is against.

What holds the line instead is the rule above plus the schedule: the report is
produced weekly and retained for ninety days, so the next change to any of
those files arrives with a report that can be diffed against this one. The
number to watch is not 201 but the efficacy column, and the question to ask of
a drop is which file it came from.
