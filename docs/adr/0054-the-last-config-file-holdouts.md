# ADR 0054 — The last config-file holdouts: a third shape, a declared identity, and a hand-authored rule

**Status:** accepted
**Extends:** ADR 0033 (the entry surface), ADR 0052 (keyed families), ADR 0053 (remembered approvals)

## Context

After #163 the window administered routines, scripts, knowledge feeds, AI
endpoints and advisors. What was left needing a text editor was the small
stuff people actually reach for — the phrases they invented
(`[[intents.custom]]`), the words the voice says wrongly (`[tts.lexicon]`),
and the shell allow/deny lists (`[tools.policy]`) — plus two surfaces that
were creatable only by *speaking*: one-shot reminders and focus threads.

Issue #164's outcome is that every persistent thing Jarvix holds can be made,
changed and removed in the window, and `config.toml` becomes an implementation
detail a user may inspect but never has to touch.

Four of those five raised a decision worth recording. The fifth — reminders —
did not: `reminders.create` calls the spoken path's own `Service.Create` with
typed words instead of said ones, and the only new thing is `reminders.preview`,
because a spoken reminder hears which reading of "at three" won and a typed one
would otherwise find out in the morning.

## Decision

### 1. A third document shape, as ADR 0052 said it would be

ADR 0052 ended with "a third document shape is a `shape` value and four
dispatch cases, not a new surface". `[tts.lexicon]` is that third shape and
this is that claim being paid:

```
entryShapeArray      [[routines]]        an entry is an element of an array
entryShapeKeyed      [ai.<name>]         an entry is a table
entryShapeScalarMap  [tts.lexicon]       an entry is ONE LINE
```

`internal/config/scalarmap_rewrite.go` is the editor, beside the other two and
under the same contract: everything outside the line being written survives
byte-for-byte, and the result must re-parse and read back as exactly the
intended edit or nothing is returned. The registry row declares
`valueKey: "spoken"` — a line has no keys of its own to name its value with —
and the four dispatch functions gained one `case` each. No handler, no verb, no
write path, no validator moved.

**One rule differs from the other shapes, deliberately: an in-place edit keeps
the line's inline comment.** For a family whose entry is a block, a replaced
block renders fresh and its inner comments go with it — that is right when the
block has room for comments of its own. Here the block is a single line, so
`# kokoro says koo-ber-NEET-es` is the only place an entry can be documented at
all, and swallowing it on every save would delete the user's notes one edit at a
time.

Addressing is **exact**, like the keyed shape: the written form is a TOML key,
and the lexicon stores what the user typed even though it matches
case-insensitively at speech time, so folding `GIF` onto `gif` would edit an
entry the user can see is a different one.

### 2. A family declares which key carries its identity

`[[intents.custom]]` is an array family with no `name`. Its identity is the
phrase it matches, and inventing a `name` key for it would change a file format
that published examples and years of hand-written configs already use.

So `entryFamilySpec` gains `idKey` (empty meaning `"name"`), and the array
editor takes it as a parameter. Every other family passes `"name"` and reads
exactly as it did. Two consequences are worth naming:

- **The wire shape does not change.** A draft still carries its identity under
  whichever key the family declares, and `name` in the verbs' params is still
  "which entry am I editing" — so one form and one registry vocabulary drive
  all three shapes.
- **`phraseKeys` is declared too.** A phrase collision is reported by whichever
  entry compiles *later*, quoting the phrase and carrying its own label, so the
  problem has to be matched back to the field the user must change. Routines
  and scripts hold their phrases in a `phrases` list; a custom intent holds one
  in `match`. A registry field says which, rather than a branch saying "unless
  it is intents.custom".

**The router now refuses two custom intents claiming one phrase.** It only
checked the built-ins before. The failure that let through was silent — rules
are tried in insertion order, so the second entry never fired and answered with
the first one's acknowledgement — and it was unreachable in practice until a
form offered to create one. The check is the same one the routine and script
loops have always run, against the same collision map, so a taken phrase names
its owner in the same words. An existing config with two identical custom
phrases will now be refused at load; it had one working intent and one dead
one, and the message says which.

### 3. Adding an allow rule by hand imports the card's refusal matrix

ADR 0053 said no IPC method accepts a pattern **on the granting path**. `#164`
adds `approvals.add`, which does. That is a real change and rests on a
distinction of *provenance*, not of politeness.

The card's pattern must be derived because the card exists **in response to
something the model asked for**: a model that could name a rule would only need
to persuade some client to forward it, and since #147 the model's input includes
text written by other people. Nothing about `approvals.add` happens in response
to the model. No tool reaches it, `jarvix`/`jarvixd` are in the refusal matrix
so no remembered shell rule can reach the CLI either, and the confirmation card
still takes a scope word and nothing else — that method is untouched, and the
test that pins it is unchanged.

**What is shared is the matrix itself, in one function.** `proposeFor` (derive
from a pending confirmation) and `VetAllowPattern` (judge what a person typed)
both end in `Policy.judgePattern`, which is the single place the three groups of
ADR 0053's matrix are consulted plus the deny check in both directions and the
already-covered check. Adding `podman rm` to the shape table refuses it on both
routes with one edit. A test compares the two routes over the tables directly,
so a shape refused by one and accepted by the other is a failure rather than a
discovery.

Two rules differ for a typed pattern, both because a person typed it:

- **Every word must be a command word, and one that is not is refused rather
  than silently dropped.** The card truncates at the first argument because it
  is summarising a real command; a typed `git log --oneline` that quietly became
  `git log` would be a rule the user did not write — and a *wider* one.
- **There is no three-word cap.** Three is where *deriving* stops guessing. A
  longer prefix a person typed is strictly narrower, so it is strictly safer,
  and refusing it would be refusing the careful answer.

`jarvix approvals` still has **no `add`**. ADR 0053's argument holds at the
shell: the window shows the refusal in the matrix's own words and the deny
removal as a paragraph to read, and a shell flag would make a standing grant
scriptable.

### 4. Removing a deny rule is confirmed; removing an allow rule is not

The deny list is a protection, not a preference, and the asymmetry is the point:

- Forgetting an **allow** rule narrows what runs unasked. It is answered
  immediately, needs no fingerprint and asks nothing — tightening a gate is
  never the thing to make hard.
- Removing a **deny** rule widens what may run, and does it by deleting a
  protection whose whole job is that nothing has been happening. There is no
  ledger row, no activity entry, nothing to remind anyone what it has been
  stopping.

So `approvals.forget` with `list: "deny"` does nothing on the first call. It
returns the sentence instead, which names what the rule protected rather than
asking "are you sure?" — a question nobody reads:

> The deny rule "httpie post" refuses every command beginning with those words,
> whoever asks — the assistant, one of your own spoken intents, or an allow rule
> that would otherwise cover it, because deny always wins. Removing it means
> those commands are classified like any other again: they will ask instead of
> being refused, and an answer of "approve and don't ask again" could then make
> them silent. Nothing else in `[tools.policy]` changes. Confirm to remove it.

The two-step lives **on the wire**, not in the window, so a client cannot skip
it by not implementing it.

Adding a deny rule carries no matrix at all — a gate that argued with someone
making it stricter is a gate people route around — but the receipt reports which
standing grants it now beats, because being told is the difference between
tightening the gate and finding out later that something quietly stopped
working.

### 5. A focus thread is saved whole, or not at all

A thread's four settings were reachable only in pieces and only by speaking:
"new thread", a name, "with this window", "check in every twenty minutes" — and
the recap mode was not reachable by voice at all, only by hand-editing
`focus.json`.

`focus.save` applies the whole draft in **one** store write. Four verbs in
sequence would be four writes, and a failure between two of them would leave a
half-configured thread — the one thing #164 forbids. Every rule it enforces is
the voice path's own, reached through the same fields, and the acknowledgement
is composed from the voice path's own sentences. An edit leaves the anchors
alone unless the form says otherwise: a rename must not silently re-point a
thread at whatever is in front of the window now.

### 6. The exclusion wall is untouched, and two families sit outside it

`[tools]` is not an entry family and never becomes one: the permission gate is
administered on its own screen with its own refusal matrix, and adding two
families to the registry did not make a third addressable. `[tools.policy]`
stays absent from the settings registry, `shell_deny` joins `shell_allow` as a
key written through `RewriteOffRegistryKey`, and `AssistantExcludedSettingReason`
still excludes both.

Both new families carry `assistantReason`, for two different reasons that the
field is now documented to hold:

- **`[[intents.custom]]` is a wall.** A custom intent runs a shell command the
  user never sees again once the phrase is spoken. #109 has always named it in
  prose; this makes it structural for the entry surface as ADR 0052 did for
  `[ai]` and `[advisors]`.
- **`[tts.lexicon]` is not.** The model can already respell a word through the
  `tts.lexicon` *setting*, a whole-table write it has had since #105. What it
  does not get is a second, per-entry route to the same table, because two write
  paths to one table is the duplication the registry exists to prevent. The
  reason says exactly that rather than claiming a prohibition that is not there.

## Consequences

- **The entry registry now serves three document shapes and seven families**,
  and the four write verbs, the listing verb, the fingerprint guard, the
  whole-document validation, the atomic write, the reload and the events are
  still one piece of code that never learns which shape it is serving.
- **`Config.Validate` judges `[tts.lexicon]`**: an empty written form (which
  would match nowhere) and an empty spoken form (which would delete the word
  from every sentence it appears in) are refused, labelled so the form pins each
  to its own input. Both were silently discarded by the compiler before.
- **A lexicon entry earns a note, not a problem.** The form warns when the
  written form is an ordinary English word — the lexicon respells every whole
  word it matches, in every sentence, so "read" respelled for a product name
  changes every "I read your note" — and saves it anyway, because the user may
  well mean it. The word list is deliberately short: a dictionary would warn
  about "kubernetes" the day it was added to one, and a warning that fires on
  the case the feature exists for is a warning people learn to click past.
- **`[tools.policy] shell_deny` is now written by the daemon**, through the same
  surgical, fingerprint-guarded, compile-before-write, atomic path
  `shell_allow` uses. One writer for both lists, because everything that makes
  the write safe is the same for both and two writers would be two places for
  one of those to go missing.
- **The Approvals view shows both lists.** A person asking "what runs without
  asking" is owed the other half: a deny rule is the reason something they
  granted still asks.
- **The Automations tab is one scroll of three collections** rather than a fixed
  40/60 split. Three fixed shares of a 600px pane would leave every one of them
  too short to read.
- **A window test that banned `approvals.add` outright was replaced.** The ban
  was right while the card was the only way to make a standing grant. What
  survives it is the property that mattered underneath: the window never judges
  a pattern and never words a refusal — it types a rule, sends it, and shows
  what the daemon says.
