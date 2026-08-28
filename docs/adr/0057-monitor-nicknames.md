# ADR 0057 — Monitor nicknames: persisted, resolved at run time, refused into the vocabulary's own words

**Status:** accepted (implements issue #180)

## Context

A routine had no safe way to say which screen it meant. The user's setup is
two monitors — `HDMI-A-1`, 3440×1440, physically above; `DP-2`, 5120×1440,
below — and they think about them as *"my top monitor"* and *"the bottom
screen"*. Connector names are exact and brittle in precisely the way a cable
is brittle: a dock move, a different GPU port, and every routine naming the
old connector breaks. Before #177 it broke *silently*; after ADR 0056 it
breaks loudly, which is better and still not what the user wants.

Their own answer, when asked, was the same instinct that made window
nicknames work (#130, ADR 0040): identify a monitor **by connector name, or
as the current one, or by a name I choose**.

ADR 0056 built the seam for this deliberately and left it empty:
`placement.Resolver{Nicknames func(string) (string, bool)}`, with the
precedence rule already decided and tested. This record covers what filling it
in decided.

## Decision

**Nicknames are persisted, in their own store, on the memory book's
discipline.** One hand-editable TOML file, `monitors.toml`, in the XDG state
dir (0600 in 0700): atomic fsync-and-rename writes, stat-per-operation
hand-edit pickup, a corrupt file warned about and **moved aside** rather than
overwritten, and a `ValidateFile` row in `jarvix restore`'s deep check.

This is the deliberate difference from window nicknames, which are in-memory
and die with the daemon. ADR 0040 chose that because *windows are ephemeral* —
a name outliving the daemon would sooner or later point at nothing, or at a
different window wearing a recycled address. **Monitors are furniture.** The
screen above the desk is the same screen tomorrow, and a user who has to
re-teach their assistant the layout of their own desk every morning has been
given a worse tool than the connector name they started with.

State, not `config.toml`, and the reason is the reminder store's verbatim
(`config.Paths.RemindersFile`): the primary way to create one is *by saying
it*, and a write that needs config-write ceremony is a write the user cannot
make by talking. The config-family editor was the other defensible answer —
it is one registry row, and the scalar-map shape (`[monitors] top = "HDMI-A-1"`)
fits exactly — and it was rejected on that one point. The visibility argument
it wins on is answered anyway: the file is documented in its own header,
its path is printed by `jarvix monitors` and shown in the window, and a
hand-edit is live on the next resolution without a restart.

**Resolution happens at run time. Nothing is ever baked into a routine.** The
routine keeps the word `top`; the store is consulted on every run, through the
live `Lookup` rather than a snapshot taken when the runner was built. That is
not an optimisation detail — it is the entire feature. A design that resolved
`top` when the routine was *written* would have reproduced the brittleness one
indirection further down, and a snapshot taken at daemon start would mean a
name assigned by voice needed a restart to work.

**Precedence is ADR 0056's, unchanged and not re-litigated.** A **present
output** whose connector matches wins over everything; then `current`; then a
nickname. A nickname can therefore never quietly redirect a routine that named
a real connector — which is the one failure a nickname feature must not
introduce — and a nickname naming nothing present is an error naming the ref,
never a silent fallback to the focused screen.

The resolver's error was **reworded** to lead with `no monitor is called …`.
That is not cosmetic: `desktop.PlacementSentence` extracts the speakable half
of a placement error by finding a known prefix, so the old wording
("`top` means DP-2, and no monitor is called that…") arrived at the user with
its most useful clause — what the name means — trimmed off.

**The collision matrix is #130's discipline in the vocabulary's own words.**
Assignment refuses, most specific owner first, each refusal a field-keyed
problem naming what owns the word:

| The name is… | Refused because | The refusal names |
| --- | --- | --- |
| a connector that is plugged in | a present output always outranks a nickname, so the name could never mean what its owner meant | that screen and its size |
| a connector nothing is plugged into (`DP-9`) | it would work until that cable arrived, then quietly mean something else | that it is a connector name |
| more than one word | a nickname is a single word so it stays easy to say | the first word, as a suggestion |
| unspellable as a monitor ref | the vocabulary would refuse to resolve it, so it could be stored, listed, and never work | the spelling rule |
| `current` or `primary` | reserved by the vocabulary | what owns the word |
| a name another screen answers to | one name, one screen | that screen |

The connector checks run **before** the single-word rule on purpose: `DP-2`
normalises to two tokens, so a single-word rule applied first would refuse the
most likely collision of all with *"try just dp"* instead of explaining that a
connector name cannot be a nickname.

Two collisions are deliberately **not** refused. A monitor nickname may be
the same word as a *window* nickname, and may be verbatim an intent phrase.
Window nicknames are checked against the intent grammar (ADR 0040) because a
bare window reference *is* an utterance the router could claim; a monitor
nickname never is — it is only ever read as the value of a `monitor` key or
as the tail of "call this monitor …" — so the two namespaces cannot collide in
any sentence, and refusing there would take words the user is entitled to.

**Assigning and re-pointing are different verbs.** `monitors.name` refuses a
name another screen already answers to; `monitors.repoint` moves it. One
verb that silently re-pointed would mean "call this monitor top", misheard at
the wrong desk, changes what every routine mentioning `top` does — with
nothing said. Re-pointing is the cable-moved case and it is still cheap: one
Edit in the window, or one CLI call. Renaming is deliberately not offered:
forget the name and give it again.

**Assignment is stricter than resolution about absent screens, and the
asymmetry is the point.** A nickname *resolving* to an unplugged output is a
normal Tuesday — the dock is in a bag — and answers "no monitor is called top
right now: it means HDMI-A-1, which is not plugged in", with the run carrying
on. A nickname being *assigned* to an output nobody can see is a typo: the
user is looking at their screens as they say it.

**One seam, and a guard that keeps it one.** Filling in
`Resolver.Nicknames` lit nicknames up in the routine runner, the window
tools, `desktop.move_window` and `desktop.Placer` at once, because all of them
already resolved through it. A drift guard in `internal/placement` fails the
build if any non-test file constructs a `placement.Resolver` without naming
`Nicknames` — the mistake a new call site makes by copying the old line, which
would show as "no monitor is called top" on one surface and success on
another.

**Voice mirrors #130's grammar with one addition.** "call this monitor top"
(and the screen/name/nickname variants), "forget the monitor called top",
"what are my screens called" — all deterministic, no provider call. Every
phrase says *monitor* or *screen*: "call this top" is indistinguishable from
the window phrase, so neither table claims it and the model decides.
The forget phrase exists here and not for windows because a window releases
its name by closing and a monitor never does.

**The window's Screens section lives in the Automations tab.** A screen name
is placement vocabulary and the thing that uses it — a routine step's
`monitor` key — is on that tab. It is deliberately not in Memory beside the
taught words: `top` is a fact about furniture, not about the user. The picker
(`JarvixMonitorPicker`) is a cycler rather than a dropdown, this window's
house style for a closed set, and it offers "the current monitor" first
followed by every present output with its size and any name it answers to —
every word of it from `monitors.list`, none composed in QML.

## Consequences

- `top` and `bottom` work in a routine step, a `desktop.move_window` call, a
  spoken request and the window's form, from one store.
- A cable move is one edit to one name, not an edit to every routine.
- A daemon with no nicknames is byte-for-byte the pre-#180 daemon: no file is
  written, `Resolver.Nicknames` is nil, and every reference resolves exactly as
  it did — pinned by tests in the store, the runner and the window tools.
- The nickname file is the user's: editable, inspectable, deletable, and a
  hand-edit is live on the next resolution. A corrupt one costs them their
  nicknames, never their morning routine.
- `jarvix monitors` prints what is attached and what it is called; the three
  write verbs are ungated, on `windows.name`'s reading — naming changes nothing
  on screen and the opposite act undoes it.
- The store is capped at 32 names, refused loudly at the limit. Nobody has 32
  screens; the cap exists so a stuck loop cannot fill a state dir.
