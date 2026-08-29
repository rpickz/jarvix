# ADR 0062 — Managed and unmanaged windows

**Status:** accepted
**Date:** 2026-08-29
**Issue:** #197 (part of the operator direction, #195)

## Context

Jarvix can already read the window inventory, focus, move and close windows
(ADR 0022), place them (ADR 0056), and — when the user switches it on — type
into whichever window has focus (ADR 0023). What it has never had is a
boundary: a statement of *which* windows it may act inside.

The user's framing was two kinds of window. The ones Jarvix opened, and the
ones the user hands over ("take control of this terminal"), are Jarvix's to
work in. Everything else is theirs. And the distinction has to be visible on
the window itself, so the answer to "can Jarvix act in here?" is a glance
rather than a memory test.

The reason this needed an ADR rather than a store and a flag is the trap
underneath it. **Typing into a terminal is running commands.** Today
`shell.run` shows the verbatim command on a confirmation card, the classifier
splits compound commands and judges each part, risk words force a question,
and deny rules refuse outright (ADR 0014). If "Jarvix manages this terminal"
meant "Jarvix may type into this live shell", acquisition would be a complete
bypass of that gate: the same power, none of the review, reached by one
sentence the user would reasonably think was about *access*.

## Decision

### What managed means

A **managed** window is one Jarvix opened, or one the user handed over. Being
managed grants exactly three things:

- Jarvix may **read** it — the same bounded reads it already performs.
- Jarvix may **place** it — move, resize, send to a workspace.
- Jarvix may **type** into it, subject to everything below, and a job (#195's
  next slice) may run there.

An **unmanaged** window is the user's. `Desktop.RequireManaged` is the single
seam a job resolves through, and it refuses anything else.

### What managed does NOT mean

**It is not permission to run anything.** This is the load-bearing half of the
decision, and it is enforced structurally rather than promised:

- Text typed into a **terminal** is classified by the very same
  `tools.Policy`, under the **`shell.run` identity**, that a `shell.run` call
  faces — the same compound-command splitter, the same shipped deny rules, the
  same risk words, the same shipped read-only allow list, and the same user
  `[tools.policy] shell_allow` entries. It is literally the same call
  (`Registry.CheckCommand(ShellToolName, …)`), because two classifiers would
  eventually disagree and the disagreement would be a hole.
- A **deny** verdict refuses the typing outright, through the `Refusing`
  interface (#105's structural wall, borrowed). `Refusing` is consulted
  *before* the tiers, so the refusal outranks an explicit
  `[tools.policy.tool]."typing.type_text" = "allow"` and a global allow alike
  — nothing a user can write in configuration softens it, short of removing
  the deny rule itself. Without it, switching typing on would be a way to run
  `rm -rf /` by spelling it into a shell. (A daemon with no gate installed at
  all classifies nothing, because the classifier *is* the gate; that
  construction exists only in tests.)
- An **ask** verdict forces the confirmation however typing is configured
  (`Escalating`), and the card is command-shaped: the command **verbatim**,
  the window named, and the classifier's own reason ("uses the risky command
  `sudo`").
- An **allow** verdict tightens nothing. One narrow case also *loosens*
  something, and it is the case #162 exists for: an allow that a
  **user-granted** `shell_allow` pattern produced (`Verdict.PreApproved`)
  stands down ADR 0023's terminal escalation, because the user has already
  said "yes, for good" to that exact command shape and asking again is the
  feature failing at its one job. The run is not silent — it still earns its
  audit row naming the granted rule, so the evidence survives the question
  going away. A **shipped** read-only allow pattern is deliberately not
  enough: `ls` runs unprompted through `shell.run` because nothing it can do
  is worth a question, while typing into a shell is a different act with a
  different failure mode, and that escalation stands until the user
  personally sets it aside.

The rule in one line: **the effective decision is the stricter of the typing
tier and the `shell.run` verdict for the same text**, with a standing approval
as the single documented exception. Management itself can only ever make the
gate stricter than the user configured it, never looser.

Two consequences follow deliberately:

- **The classification does not depend on management at all.** An unmanaged
  terminal that happens to have focus is classified identically. That is what
  makes "acquire it first" useless as a way around the gate: there is nothing
  on the other side of acquisition that acquisition unlocks.
- **`shell.run = "deny"` denies typing into terminals too.** A user who turned
  command execution off gets it off, including through the keyboard. Typing
  into every non-terminal window is untouched.

"Terminal" has exactly one definition, `tools.isTerminalClass`, shared by the
typing gate and the managed-window surface — two would drift, and the drift
would be a window described as safe and treated as a shell, or the reverse. It
is the configured `[tools.typing] terminal_classes` list *plus* the whole
`dev.jarvix.` namespace, because a window Jarvix opened inside a terminal
wears an identity of its own (#198) and a class-list match would miss it —
`ghostty -e bash` produces a window classed `dev.jarvix.bash`, which is
literally a shell. The price is a command-shaped confirmation for text
dictated into a TUI Jarvix opened, which is the safe direction to be wrong in.

`typing.audit` still records every decision, and #109's `[tools]` exclusion
wall is untouched: nothing typed can reach the gate's own configuration,
because nothing typed goes near the config-writing tools.

### The record in the feed

A typed command gets **a second activity row**, of the gate kind, carrying the
command verbatim and the rule that judged it. This is the one place a typed
payload travels, and it is a deliberate narrowing of ADR 0023's "the text is
never logged":

- text aimed at a terminal is a command line, not private prose;
- a standing grant removes the question, so it must not also remove the
  evidence (the `tool.pre_approved` argument, applied to a keyboard);
- the daemon's **journal** line is unchanged — it still records the length and
  never the characters.

The residual risk is named rather than waved away: a user who dictates a
secret at an interactive prompt inside a terminal will have it in the activity
feed. The alternatives were worse — a command that reached a shell with
nothing in the record but "typed 24 characters" is exactly the audit hole this
feature would otherwise open — and the payload is already spoken aloud and
shown on the confirmation card today.

### Acquisition and release are asymmetric

- **Acquiring asks, every time.** `desktop.manage_window` is in `neverSilent`:
  a global `default = "allow"` does not reach it, only naming the tool does,
  and a global `deny` still wins. The confirmation names the window and says
  in the same breath that commands are still confirmed — a card that said
  "take control" without that would be answered wrongly. Because
  `RememberableApproval` keys on the same map, an approval for one window is
  never spent on the next.
- **Releasing is ungated and immediate.** `desktop.release_window` is
  allow-tier, the `windows.release` IPC verb has no confirmation, and deleting
  a stanza from the store by hand releases a window too. Giving up power needs
  no permission, and a release that could be refused — by a policy default, by
  a mistyped tier, by anything — would be a window the user asked for back and
  did not get.

There is deliberately **no** acquisition verb on the IPC surface and no
"Manage" button in the window. A one-click grant would be the same grant with
none of the naming.

### What management is keyed on

Persistence is required — a window outlives the daemon, so restarting jarvixd
must neither quietly hand a terminal back nor quietly keep one — and Hyprland
addresses are not a key. An address is a pointer value: stable while the
window lives, and **recycled** afterwards, so a record matching on it alone
would eventually hand a stranger's window the grant the user made to one that
has since closed.

A record therefore carries **the compositor address, the compositor's own
stable id, the application class, and the owning process id**, and matches a
live window only when **all four** agree. It is the same identity the window
tools verify with before any state-changing dispatch (`Desktop.verify`), plus
the pid — which is the strongest of the four, because a live process id is
unique on the machine and is what makes a recycled address detectable.

An empty stable id or a zero pid *in the record* is a wildcard, and only
there. Machine-written records always carry whatever the compositor reported,
so the wildcard is reachable only from a hand-edit — where it is the right
answer, because a stanza someone wrote by hand names a window with the facts
they could see.

Every read is judged against the live inventory and any record whose window is
not in it is dropped, so nothing can go on claiming a window that no longer
exists (#180's honest-absence discipline). Reconciling is part of reading
rather than a background chore because there is no window-created or
window-closed event seam anywhere in this repository (ADR 0048 explains why
the overlays poll) — the honest moment to answer "has it gone?" is the moment
someone asks.

### Managed from birth

A window Jarvix launched is managed from the start, and #198's launched-window
identity is how it is recognised: the class the terminal table asks the window
to carry (`dev.jarvix.claude` for ghostty's GTK application id, a bare token
elsewhere).

At the instant of launch there is no window to record — no address, no pid —
so the store keeps a **claim**: "the next window classed X is one I opened".
The first inventory showing such a window turns the claim into a record and
consumes it. A claim nothing matches within a grace period **expires**: a
launch that failed, or a terminal that ignored the class flag, must not leave
a standing offer to adopt whatever turns up wearing that name an hour later.

Two limits are recorded rather than hidden:

- **A graphical launch claims nothing.** Only a terminal-hosted launch is
  given a class of ours, so a graphical program opens a window Jarvix cannot
  recognise. Claiming it would mean adopting whichever window appeared next,
  which is a guess. Honest absence beats a lucky guess.
- **Managed-from-birth is one poll late.** Adoption happens on the first
  inventory read after the window maps — the overlay's two-second tick, or the
  next tool call. Nothing acts on the window in between, so the only visible
  consequence is when the mark appears.
- **Routine-launched windows are not claimed yet.** A routine step's
  `identity` is a user-authored class (#186), not one this path issues; wiring
  it is the same mechanism (`Store.ClaimLaunch`) applied at a second call site
  and is left to the slice that needs it.

### The mark on the window

The indicator rides the **existing** overlay feed (#127, ADR 0048) — a
`managed` flag on the row the daemon already composes — rather than a second
surface. Management is enrolment in its own right: a managed window earns a
chip even with no thread and no nickname, because a mark that only appeared on
windows the user had *also* nicknamed would answer the question for the wrong
half of the desktop. The converse is unchanged: an unmanaged, unanchored,
unnamed window carries nothing at all.

The mark is a **square outline with a solid centre**, drawn leftmost in the
chip. Its shape is the whole of its meaning: the three marks a chip can carry
have to be told apart without colour discrimination, so each is a different
silhouette — a circle for the thread badge (filled = active), a Nerd Font
glyph for the AI-session state, a square for management. It is static, like
everything on that surface.

### Typing switched off

`[tools.typing] enable` is off in the shipped configuration, and acquisition
still works: reading and placement are the majority of what management is for.
The limitation is said at the **earliest honest moment** — in the confirmation
card, before the user answers, and again in the acquisition sentence and on
the Approvals tab — rather than discovered when nothing is typed.

## Consequences

- One more state file, `managed.toml`, on the storage discipline every other
  store here uses (ADR 0011): hand-editable TOML, 0600 in a 0700 directory,
  atomic fsync-and-rename writes, stat-per-operation hand-edit pickup, a
  corrupt file warned about and moved aside rather than overwritten, and a cap.
  `jarvix restore` validates it with the daemon's own loader (ADR 0045).
- The overlay feed's enrolment gate gains a third cheap question
  (`ManagedCount`), so a user who manages one window keeps the poll awake. A
  user who manages none still pays nothing.
- The confirmation card for typing into a terminal changes shape: it names the
  command verbatim and says what the classifier found. Users who had typing on
  will notice their terminal cards became command cards. That is the feature.
- `desktop.manage_window`, `desktop.release_window` and `desktop.list_managed`
  are registered only when a store exists, so a daemon without one behaves
  byte-for-byte as it did before this change.

## Alternatives considered

- **Treating acquisition as consent to type freely.** Rejected: it is the
  bypass the issue was written to prevent, and it would have made every
  control in ADR 0014 optional for anyone who said one sentence.
- **A separate classifier for typed commands.** Rejected: two classifiers
  drift, and the drift is a hole. The typing path asks the same `Policy` the
  same question.
- **Keying persistence on the address alone.** Rejected: addresses are
  recycled, and the failure mode is a stranger's window inheriting a grant.
- **Keying on `(pid, /proc start time)`.** Considered — it is the only
  OS-guaranteed unique process handle — and rejected as unnecessary: the
  four-fact match already requires four simultaneous coincidences, and reading
  `/proc` would add a second source of window identity beside the compositor
  seam ADR 0022 makes the only one.
- **A second overlay surface for the managed mark.** Rejected by the issue's
  own NFR and by ADR 0048: one panel per monitor that never churns is the
  whole reason the overlay surface is cheap.
- **Marking every window, managed and unmanaged.** Rejected: "nothing on
  windows that carry no marks at all" is #127's acceptance criterion, and a
  desktop where every window wears a chip is a desktop where nobody reads
  them.
