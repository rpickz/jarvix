# ADR 0055 — What went into the answer: provenance is derived, never reported

**Status:** accepted (implements issue #168; extends ADR 0027's archive schema
on the #118/#125 terms, and applies ADR 0037's disclosure stance to a new
budget)

## Context

Jarvix answers from real things — a remembered fact, a knowledge-feed value, a
taught phrase, a conversation it searched, a window it captured, an advisor's
reply, a file it wrote. Every one of those is known daemon-side at the moment
the answer is assembled, in exactly one place each: the four `gather*`
functions that build the prompt, and the tool loop that runs a call and takes
its result. All of it was then thrown away. To check where a number came from,
or to get back to the window a recap described, the user had to reconstruct it
by hand.

The obvious implementation is the wrong one. Asking the model which sources it
used produces a citation list that reads exactly like a real one and is not
one: which retrieved fact a model actually leaned on is not knowable to the
model either, and the request itself invites invention — the failure mode the
honesty rules of #71 exist to prevent. A feature whose entire value is
trustworthiness cannot be built on a guess dressed as a record.

## Decision

**Provenance is mechanically derived from what the daemon injected or a tool
returned during the turn, it is stored as references and never as content, and
its wording distinguishes two strengths that are never merged.**

### The two strengths, and why there are two

- **available to the answer** — the reference was injected into the turn's
  context. We know it was in front of the model. We do *not* know it was used,
  and nothing says otherwise.
- **returned during this turn** — something ran and produced output that went
  into the answer: a tool call, a capture, a recap. That is mechanically
  causal, so it may be stated more strongly.

They are two different claims and the product says both. The wording lives
once, in `internal/provenance`, beside the constants it describes, and
`TestStrengthWordingIsPinned` fails if either drifts — including into the other
— because "source", "cited" and "used" all claim knowledge nobody has. An
unrecognised strength resolves to the *weaker* phrase: overstating what Jarvix
knows is the one failure this feature cannot afford, so the fallback is the
cautious claim. A QML guard (`TestTheProvenancePanelWordsNothingItself`) keeps
the window from wording a strength itself, so there is exactly one copy.

### Collected where it is already known — and nowhere else

One collection point per kind, each at the line the fact is already in hand:

| Source | Collected in | Strength |
| --- | --- | --- |
| memory facts (ADR 0025/0037) | `gatherMemory` | available |
| knowledge feed values (ADR 0031) | `gatherKnowledge` | available |
| taught vocabulary (ADR 0042) | `gatherVocabulary` | available |
| desktop capture (ADR 0019) | `gatherContext` | available |
| any tool call that ran and returned output | the tool loop, in `executeTool` | returned |
| the focus thread a switch or recap read (ADR 0041/0043/0047) | `runFocus` | returned |

Nothing else may add a source **to a turn**, and no code path anywhere asks the
model to attribute anything. A call that returned an error contributes nothing:
it returned no output, so saying it went into the answer would be the
overstatement this feature exists to avoid.

**One thing has since reused the vocabulary without being a turn at all.** The
situation report (#196, ADR 0061) gives each of its lines a `Reference` so the
window can link a line to the thing it describes, and it composes those
references at read time from the same live stores the lines were composed from.
They enter no `Record`, no archive and no event — the report is transient — and
they added two kinds beside the seven above, `KindReminder` and `KindSchedule`,
resolving on identical terms. That is a use of the resolver, not a second
collection point, and it satisfies the rule above trivially rather than bending
it: the reference is derived by the code that read the fact, with no model
anywhere near it. Anything else wanting to point a person at something should
come this way too, and the test to extend is
`TestEverySourceKindResolvesToWordsAndAnHonestAction`.

Two tools know something their arguments cannot say — `artifact.create` knows
which file it actually wrote after de-duplicating the name, and
`conversations.search` knows which conversations answered. They report it on a
context-carried sink (`provenance.WithSink` / `provenance.Note`), which costs
every other tool nothing and keeps provenance out of the `Tool` interface. What
a tool reports *replaces* the generic line for that call: "the artifact
q3-chart.png" is the thing to press, and "artifact.create" beside it is noise.

### References, never content

A reference is an id, a name or a path — the handle that finds the thing
again — and deliberately not enough to reconstruct what it said. The readable
name is composed at the moment somebody looks, from the live store, by
`provenance.resolve`. Three things follow, and they are the whole argument for
the split:

1. The archive never becomes a second copy of the memory book, the feed cache,
   or a captured window. The transient rule of ADR 0043/0047 stands unchanged;
   a leak-salted test proves a captured session's text reaches neither the
   archive nor any event, and asserts the provenance is nonetheless there, so
   it cannot pass by the feature simply not working.
2. A source that has since gone **says so**, because the resolver looked and
   did not find it — and its actions are absent rather than dead. A forgotten
   fact cannot be quoted back from a stale copy, because there is no copy.
3. A *query* is never a subject. `memory.search` and `conversations.search` are
   named by what they searched — "your remembered facts", "your earlier
   conversations" — for the reason the Activity pane already refuses to show a
   query (ADR 0037): a query can quote the very fact it is looking for.

The one deliberate limit is desktop capture. For the active-window source the
capture's whole text *is* the window's identity line (`class — title`), so
naming the window would be recording the capture. The reference is the source
word instead, and there is exactly one active window, one selection and one
clipboard — so the source *is* the specific item; the category would have been
"desktop context".

### Additive in the record, on the #118/#125 terms

`conversations.Turn` grows a last field, `provenance,omitempty`, carrying
`{sources, truncated?}`. Every line already on disk stays byte-identical (the
golden files are untouched), an old archive loads with nil, and
`SchemaVersion` stays 1 — a reader that ignores the key still reads every
utterance correctly. It rides the **assistant** half alone, and is absent on
every turn that consumed nothing: absence is information, and an affordance
that is always there says nothing.

In the live view it is anchored to its assistant message's position on the
same monotonic counter the confirmation records use (ADR 0039), so the
retention cap trimming the head can never slide it onto the wrong answer.
Reopening a conversation restores it beside the turns, rebased to the context
window's cut, exactly as the approvals are. Like them it is display state and
never model context: the model is not told a second time what it was given.

### Bounded, and the cut disclosed

Twelve references per turn (`provenance.MaxSources`), chosen against the shape
of a real turn — an ambient pinned set plus a feed or two, and at most six tool
rounds — rather than as a round number. **The cap drops the weaker claim
first**: when only some fit, the mechanically causal ones are the ones worth
keeping, so `available` references leave from the end before any `returned` one
does. What left is counted in `truncated` and said out loud, in the panel and
in the spoken answer, the way ADR 0037 discloses its trim. A source that
appears twice — injected and then found again by `memory.search` — is one
source, listed once, carrying the stronger of the two claims.

### Navigation, and the gate

`provenance.resolve` returns each item's actions. A `tab` action is the
window's own navigation (Knowledge at that feed, Memory at that fact or taught
phrase, the Library showing that conversation, Focus at that thread); an
`invoke` action is `provenance.open`, for the three things that leave the
process: opening an artifact with the configured viewer (`artifacts.open_command`,
resolved through the artifact tool's own `ViewerFor` so a file opens with the
command that opened it when it was made), focusing the window a thread is
anchored to (the anchor's liveness is the focus service's own answer, so the
panel and the Focus tab can never disagree), and opening a feed's page.

`provenance.open` re-checks what it is about to act on rather than trusting the
resolve that offered it — the file may have been deleted, the window closed,
the feed edited since the panel was drawn — and refuses in words instead of
silently doing nothing.

A feed's page goes **through the permission gate, never around it**: the gate
is asked about `xdg-open <url>` under the shell tool's identity — the identity
a standing approval for `xdg-open` is written under (ADR 0053) — and only an
allow verdict proceeds. An `ask` verdict does *not* raise a card. The
confirmation card exists in response to something the **model** asked for, and
manufacturing one from a button press would be a client deciding what runs,
which is precisely the door ADR 0053 closed. So the action is absent, and the
reason is words: "its page opens only with a standing approval for xdg-open".
That is narrower than "the button always works", and it is the only version of
this that cannot be argued into opening something the gate would have
questioned.

### The spoken path

"Where did that come from?" is a deterministic intent (`provenance.list`),
owned like the vocabulary and approvals listings, read-only, and routed before
the model — so the question can never be answered by the thing whose account
of itself is the one account we do not trust. It reads the same record the
panel does, through the same resolver, so the two cannot describe a source two
ways. Shortest-useful first: the causal clause leads, the available clause
follows, four names per clause with "and N more", and the honest empty is an
answer — *"Nothing I can point you at — that answer did not use anything I had
looked up or been given."*

## Consequences

- Every answer that used something retrievable can be checked and followed,
  and every answer that used nothing plainly says so by showing nothing.
- Two claims now exist where the industry usually ships one. A user who reads
  "available to the answer" and expects "the model used this" has been told the
  truth and may still be disappointed by it; that is the correct trade, and the
  wording is test-pinned so it cannot quietly become the comfortable lie.
- The archive grows by a small object on answering turns only. A pinned
  20-fact book adds twenty short references per turn before the cap bites;
  that is the cost of the feature and the cap is where it is bounded.
- `provenance.resolve` reads the live stores on every expand — the memory book,
  the feed list, the focus snapshot (which reads the compositor), a `stat` per
  artifact. It runs on demand, never on `conversation.get`, so a long
  transcript costs nothing until somebody asks about one turn of it.
- A feed's page will usually not offer a button, because most users have no
  standing approval for `xdg-open`. The honest note is the feature until
  something in the product can ask for leave outside a model turn.
- `knowledge.Injection` gained a `Names` field. Counts were enough while the
  only consumers were disclosure events; naming the specific feed is an
  acceptance criterion here, and a count cannot do it.
