# ADR 0042 — Personal vocabulary: explicitly taught, budgeted beside memory, biasing STT within a cap

**Status:** accepted (implements issue #129)

## Context

Natural speech is full of colloquialisms and personal shorthand — "quid" for
pounds, a project codename, family words. The model knows common slang; what
it cannot know is *this user's* vocabulary, and whisper mishears rare terms
besides. Issue #129 adds a taught vocabulary: phrase → meaning entries the
user teaches by voice, chat, or the window, injected into the model's
context, with hard-to-hear phrases also joining the STT bias prompt (the
name-alias precedent of #83/#107).

Several decisions had defensible alternatives; this record keeps them from
being relitigated by accident.

## Decision

**Explicit teaching only — never inference.** Nothing enters the store
unless the user taught it in so many words. The teach tool's description is
the enforcement point (with zero entries no vocabulary block rides the
prompt, so the block cannot carry the rule), and there is deliberately no
"learn from usage" path anywhere: an assistant silently deciding what its
user "really meant" would be rewriting them. Auto-learning stays out of
scope permanently, not provisionally.

**Its own store, on the memory book's discipline verbatim.** One TOML file
under the XDG state dir (`vocabulary.toml`, 0600 in 0700), atomic
fsync-and-rename writes, stat-per-operation hand-edit pickup, corrupt files
warned about and moved aside — never overwritten — ids ratcheted and never
reused, an injected clock for tests, and a supersede trail. A separate store
rather than fact entries in `memory.toml`, because the two lists are
curated, listed, and injected on different terms, and each stays small
enough to ride a prompt precisely because it does not absorb the other.

**The phrase is the entry's identity; re-teaching supersedes.** "When I say
quid I mean euros" updates the existing entry (spoken identity: case and
punctuation folded), keeping the old meaning on `[[entry.previous]]` with
both timestamps. Never a silent second entry, on any surface — the window's
rename path refuses a collision with another taught phrase for the same
reason. Identical re-teaches write nothing.

**Injection is budgeted beside memory, disclosed, and byte-identical when
empty.** The block sits directly after the remembered-facts block — how the
user talks is standing knowledge, true for the whole thread — with its own
token budget (`vocabulary.max_injected_tokens`, floor 150 because the
preamble alone costs ~100). Entries past the budget leave the *block*, least
recently taught first, never the store; the trim is stated inside the block
and as a warning in the window's section (the ADR 0037 stance: caps are
never silent). With zero entries no message is appended at all, and a test
pins that the provider request is byte-identical to a pre-feature daemon's.
There is deliberately no `vocabulary.search` tool in v1: vocabularies are
small, and the trim disclosure tells the model to ask rather than guess —
revisit if real stores outgrow the budget.

**`vocabulary.speak_back` defaults to false, and the reasoning is the
point.** Understanding the user's words and performing them back are
different acts: mirrored slang from a machine reads as mockery more often
than rapport, an in-joke is the user's to make, and a wrong register
("Jarvix, when I say nana I mean my grandmother" → "your nana called")
lands badly precisely when it matters. So the block's default stance
sentence tells the model to answer in plain words, and the setting —
idle-class, registry-editable — is the user's explicit invitation.

**Hard-to-hear phrases join the one bias composition, capped at 20, live.**
Flagged phrases enter whisper's conditioning prompt through
`config.STTBiasPromptWith` — the single copy of the #107 sentence rule
(full capitalised sentences; bare terms get absorbed) shared with
`stt.vocabulary`. The cap exists because the conditioning window is ~224
tokens and the assistant's name and configured vocabulary already spend from
it; twenty phrases fits comfortably, and past it terms crowd each other out
rather than adding. The cap refuses loudly at the limit, warns from
nine-tenths, holds against hand-edits (normalize clears excess flags with a
logged warning), and is doctor-visible. The transcribers read the prompt
through a function (`PromptFunc`) so a flag lands on the very next utterance
— a static prompt would have made "listen for the word X" a mechanism that
looks enabled while doing nothing until a reload. Flagging requires a
taught entry: an entry is a phrase AND a meaning, and inventing a meaning to
hold a flag would put words in the store the user never taught (bare
recognition terms belong in `stt.vocabulary`).

**Teaching is allow-tier; forgetting is gated.** `vocabulary.teach` is
built-in allow on `memory.remember`'s exact argument: the user just said the
teaching out loud, the write is one line into their own file, and one forget
undoes it. `vocabulary.forget` takes the policy default (ask) with a
confirmation naming the exact phrase and meaning, and the window's Delete
routes through the same gated identity (`vocabulary.forget_gated` →
`engine.ForgetVocabularyEntry`). We considered ungated deletion —
"reversible-ish", since the user can re-teach the phrase — and rejected it:
deletion destroys the supersede trail and its dates, which no re-teach
reconstructs, and a second forget verb with a softer stance than
`memory.forget`'s would make the gate's rule unguessable. ADR 0025's
reversibility split, applied as written.

**Router phrases are deterministic; taught phrases never touch the router.**
"When I say X I mean Y" (two free-text slots hinged on a separator that must
occur exactly once — ambiguity falls through to the model), "listen for the
word X", and the listing phrases are compiled into the intent grammar; the
listing phrases are owned literals with the standard collision refusals.
Nothing taught ever enters the grammar: intent-router synonym substitution
is explicitly out of scope, so teaching cannot rewrite deterministic command
matching or weaken the collision guarantees. If synonym substitution is ever
wanted, it is its own ticket against this paragraph.

**Admin surfaces mirror memory's.** `vocabulary.list/last/teach/update/
forget_gated` over IPC; a Vocabulary section inside the window's Memory tab
(the second collection — both are "what Jarvix keeps about you", and a
seventh tab for a usually-short list costs more navigation than it buys);
events carry ids and sizes, never words; `vocabulary.last` keeps the
injection auditable.

## Consequences

- Understanding improves without the user translating themselves, and the
  zero-entry cost is provably nothing.
- The store file is the user's: editable, inspectable, deletable — and a
  hand-edit is live on the next question.
- The bias budget is finite and honest: full is a refusal with a named fix,
  never a silently inert flag.
- `whispercpp.Transcriber`/`ServerTranscriber` carry an optional
  `PromptFunc`; injected test transcribers are unaffected.
- The engine's message assembly gained one seam (a system message beside the
  memory block); the tool loop, speaker, and confirmation machinery are
  untouched.
