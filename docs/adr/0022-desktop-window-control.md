# ADR 0022 — Window control: the model picks from an inventory, never writes a command

**Status:** accepted (implements roadmap Phase 4)

## Context

Jarvix can answer questions and, with `shell.run` enabled, run commands — but
it cannot touch the desktop it lives on. "Put me back in the browser", "what
have I got open?", "move this to workspace three", "open a terminal": the
natural things to say to a computer you are talking to all fail, and the user
reaches for the keyboard. Roadmap Phase 4 names `desktop.*` as the next tool
family, and the permission gate it was waiting for shipped in ADR 0014.

The compositor makes it easy: `hyprctl clients -j` reports every window with
its class, title, workspace and focus, and `hyprctl dispatch` acts on one.
That ease is the trap. A tool that took a command string and handed it to
hyprctl would be `shell.run` wearing a hat — the model would be composing
instructions for a program that acts on the machine, and the gate would be
confirming a sentence the model wrote about itself.

Two further problems are specific to windows. References are loose: people say
"my browser", not `firefox`, and a matcher tolerant enough to find it is
tolerant enough to find three things. And windows move while you talk about
them — a spoken confirmation takes seconds, and the desktop does not hold still
for them.

## Decision

**The model chooses an entry from an inventory Jarvix produced; it never
writes what runs.**

Five verbs, one each: `desktop.list_windows`, `desktop.focus_window`,
`desktop.move_window`, `desktop.close_window`, `desktop.launch_app`. What the
model may say is a loose description, a workspace number, or an application
name. What reaches a subprocess is a window address taken from
`hyprctl clients -j`, an integer checked against 1–99, or a binary resolved
through the configured allow list or `exec.LookPath`. No shell is involved at
any point, so a "window" called `firefox; rm -rf ~` is a description that
matches nothing, never a command.

**Loose matching, honest ambiguity.** Matching is case-insensitive and
substring-tolerant across the application class and the window title, with
word-wise matching for phrases and a small hand-written table of categories
("browser", "terminal", "editor") for the words people actually use. Matches
are ranked in tiers — exact application name, exact title, prefix, substring,
all words, category — and a better tier wins outright. A tie *within* the
winning tier is not broken: Jarvix names the candidates and asks. A wrong focus
costs a second and teaches the user that Jarvix is guessing, which costs the
feature; a question costs one sentence.

**A resolution is a fact, not a query.** Resolution produces a window address,
and everything after it — the spoken confirmation, the wait, the dispatch —
carries that address. Before a state-changing dispatch the compositor is asked
once more whether *that* window is still there, identified by address plus the
compositor's own window id and the application class, because an address is a
reusable handle and a window created since the resolution must never inherit an
approval. A window that has gone produces "it has already gone"; it never
produces a second resolution.

**Reads allow, changes ask.** `list` and `focus` are allow-tier by default:
listing sees no more than the desktop context Jarvix may already gather, and
focusing changes nothing but where the user is looking. `move`, `close` and
`launch` take the policy default (ask). The question is built from the
inventory — "I want to close firefox, the window titled GitHub. Should I go
ahead?" — through a new `tools.Confirmable` seam: the gate still decides the
tier from configuration, and the tool supplies the words from what it can see,
so the model's description of what it is doing is never what the user approves.

**The compositor is an interface; Hyprland is the only implementation.**
`desktop.Compositor` has a method per verb and a fake. No test in the tree needs a
running compositor.

## Consequences

**The dispatch dialect is discovered, not assumed.** Hyprland 0.55 moved
configuration to Lua and with it `hyprctl dispatch`, whose argument is now
evaluated as Lua: `hyprctl dispatch focuswindow address:0x…` is a parse error
on a Lua-configured compositor, and `hl.dsp.focus({ window = "address:0x…" })`
is an unknown dispatcher on an hyprlang-configured one. Version is not the
question — a 0.56 user on hyprlang keeps the old syntax — so the driver probes
once with a dispatcher that does nothing (`hl.dsp.no_op()`) and remembers the
answer, re-probing only after a failure. Both dialects fail as syntax errors,
which change nothing, so probing can never move a window.

The same change broke `hyprctl dispatch workspace N` in the deterministic
intent router (ADR 0017) on a Lua-configured Hyprland
([#47](https://github.com/rpickz/jarvix/issues/47)). It was fixed where this
paragraph said it belonged: the router's two compositor intents — "workspace
four" and "open a terminal" — now name an *action* (`SwitchWorkspace`,
`Spawn`) that this seam renders in the probed dialect, instead of carrying a
fixed `hyprctl` argv of their own. There is one dispatch path in the tree and
one place that decides how a dispatch is written.

`Spawn` is the one exception to the paragraph below on launching, and only
because its argument is a different kind of value: `[intents] terminal` is a
configured setting, validated as a single bare token when the router compiles
and again before it is rendered, never a model-chosen or spoken string. In
exchange the terminal is a child of the compositor, so it lands on the active
workspace with the graphical session's environment and outlives a daemon
restart — which is what the intent always did, and what starting it from
jarvixd would have regressed.

**Success is what the compositor says, not its exit code.** `hyprctl` exits 0
for a dispatch the compositor refused — "window not found" arrives on stdout
with a zero status — so a dispatch counts as done only when the reply is `ok`.

**A busy desktop costs one subprocess per turn.** The inventory is cached for
two seconds, so listing and then acting is one `hyprctl clients` call. The
cache is dropped before acting on a confirmed resolution: the user answered a
question about a window seen before they answered, and that is exactly when a
fresh look is worth its milliseconds.

**Launching is a detached child of the daemon.** `hl.dsp.exec_cmd` runs its
argument through a shell, so it is not used; applications are started directly
with `exec.Command`, in their own process group, with the credential-shaped
environment variables scrubbed as for an advisor CLI. They therefore survive a
daemon restart, and Jarvix cannot pass them a single argument.

**Typing is still out of scope.** Everything here is visible on screen and
undoable by hand, and none of it can enter data anywhere — that is what makes
these verbs the safe half of "take control". Sending keystrokes is
categorically different and is [#37](https://github.com/rpickz/jarvix/issues/37);
it can build on this inventory (`Window.AcceptsInput` is reported for exactly
that reason) but not on these tiers.
