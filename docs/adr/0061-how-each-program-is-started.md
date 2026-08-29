# ADR 0061 — Jarvix knows how each program is started: the entry says, PATH implies, the user decides

**Status:** accepted (implements issue #194; supersedes ADR 0058's refusal of `Terminal=true`, extends ADR 0022 and ADR 0056)

## Context

The user asked Jarvix to launch Claude. In their words:

> *"When I ask Jarvix to launch Claude it has no idea how to do that. It doesn't know that
> Claude is a CLI program, and needs to be launched within a terminal… Grok, Codex and
> OpenCode also launch within a terminal — but Claude Desktop could be launched too, and
> that's a separate app. Jarvix needs to be an expert — and it's coming across as inept."*

What happened is worth stating exactly, because the shape of the failure is the reason
this is an ADR and not a patch. `claude` **is** on PATH — a mise shim, on this machine.
So `desktop.launch_app` resolved it, the launcher exec'd it bare as a detached child with
no terminal and no stdin, and it exited immediately. The tool then returned:

```
Started claude. Tell the user in one short sentence that it is opening; its window
will appear on its own.
```

Nothing appeared. The sentence was confident, specific, and false — the one shape the
honesty rules (#71) exist to prevent, produced not by a model hallucinating but by the
daemon *telling* it to. Rewording that sentence would have been the wrong fix: there was
exactly one notion of "launch" in the system — exec a binary and expect a window — and
it is wrong for a whole class of programs. The model of launching is what had to change.

Three signals were already on the machine and unused.

**A graphical application ships an XDG desktop entry; a command-line tool does not.**
`/usr/share/applications` and `~/.local/share/applications` are where a desktop puts the
things that open windows. On this machine `claude`, `opencode`, `codex` and `grok` are
all on PATH with no entry of their own, and that alone identifies them.

**The specification carries `Terminal=true` outright.** `internal/desktopentry` has parsed
it since #186 and `internal/routine` *refused* such entries, with reasoning that was
correct at the time: a routine places graphical windows, and launching a terminal program
with no terminal produces exactly the eight-second silence #175 existed to end. What was
missing was a remedy.

**`[intents] terminal` already names the user's terminal** — `ghostty` here — and "open a
terminal" has used it since ADR 0017.

## Decision

### 1. Classification is a package, and its answer is a kind

`internal/launchkind` answers "what is this program and how does it start", and starts
nothing. Three kinds: **graphical** (started directly; a window appears on its own),
**terminal** (started inside the configured terminal), and **unknown** (nothing on this
machine says which, so ask).

The precedence, highest first, is the whole rule:

| # | Source | Kind | Why it ranks there |
| --- | --- | --- | --- |
| 1 | `[tools.launch]` override | as stated | The person who owns the machine is not offering a hint. |
| 2 | The entry's `Terminal=true` | terminal | The specification's own explicit statement about itself. |
| 3 | A desktop entry exists | graphical | It is on the applications menu; that is what an application is. |
| 4 | On PATH, no entry anywhere | terminal | A command-line tool ships no entry. This is the rule the ticket is built on. |
| 5 | On PATH, and this machine has **no** entries at all | unknown | See §3. |

Rule 3 asks "does **any** entry launch this program?", not "is there an entry with this
name?", and the difference is what makes it survive real packaging. Telegram installs
`org.telegram.desktop.desktop` and a `telegram-desktop` binary; asking only about the id
would answer no and send a graphical application into a terminal window. So the catalogue
indexes entries by their `Exec` program (and `TryExec`) as well as by id and `Name`.

### 2. The user's own answer outranks every inference

```toml
[tools.launch]
terminal_programs  = ["claude"]     # open these inside the terminal
graphical_programs = ["obsidian"]   # start these directly
```

Two lists, not a map, because a program starts one way or the other and there is nothing
else to say about it. Both are registry settings (`tools.launch.terminal_programs`,
`tools.launch.graphical_programs`), so they appear in the settings screen with every
other family and nobody has to find a file (ADR 0054). A name in **both** lists is a
validation error naming it: that is not a preference expressed twice, it is two
incompatible instructions, and resolving it silently would mean choosing one of the
user's own sentences to ignore.

Almost every machine will have both lists empty. That is the point of a default.

### 3. "It has no entry" is only evidence when entries were looked for

Rule 4 is a strong default, not a law, and it rests on a survey. On a machine with **no**
desktop entries at all — no applications directory, an XDG path that points nowhere — the
observation "this program has no entry" is a fact about the search, not about the program.
Answering *terminal* from it would send Firefox into a terminal window with precisely the
confidence this whole change exists to remove.

So that case is `KindUnknown`, and the launcher says what it does not know:

> *"firefox is installed, but I cannot tell whether it opens a window of its own or runs
> inside a terminal: this computer has no application entries at all. Ask the user which
> it is…"*

An override still answers on such a machine, which is the escape hatch that keeps this
honest rather than merely obstructive.

### 4. The per-terminal table, and where every spelling came from

There is no convention to derive here. `-e` came from xterm and most terminals imitate
it, but kitty and foot take the command as trailing positional arguments, gnome-terminal
deprecated `-e` in favour of `--`, and wezterm puts the whole thing behind a `start`
sub-command. A spelling guessed wrong is not a near miss: it is an argument the terminal
rejects at start-up, so the user asks for Claude and gets a window that flashes an
unknown-option error and exits — the same silent nothing, with an extra step.

So the table is curated, and each row's source is recorded here.

| Terminal | Runs a command | Window identity | Source |
| --- | --- | --- | --- |
| `ghostty` | `-e` (last) | `--class=` — a **GTK application id** | `ghostty(1)` on this machine: *"Ghostty supports the common -e flag for executing a command with arguments"*; `--class` *"controls the class field of the WM_CLASS X11 property …, the Wayland application ID"*, and *"the class name must follow the requirements defined in the GTK documentation"* |
| `alacritty` | `-e` (last) | `--class=` | `alacritty(1)`: *"-e, --command &lt;COMMAND&gt;… Command and args to execute (must be last argument)"*; *"--class &lt;GENERAL&gt; … Defines the window class hint on Linux"* |
| `kitty` | positional | `--class=` | `kitty(1)`: *"kitty [options] [program-to-run …]"*; `--class` *"On Wayland set the application id. On X11 set the class part of the WM_CLASS window property"* |
| `foot` | positional | `--app-id=` | `foot(1)` on this machine: *"All trailing (non-option) arguments are treated as a command"*; *"-a,--app-id=ID Value to set the app-id property on the Wayland window to"*. foot accepts `-e` but its manual says it is *"Ignored; for compatibility with xterm -e"*, so the documented positional form is used |
| `wezterm` | `start` … `--` | `--class=` | `wezterm start`: the command follows a `--` separator (*"wezterm start -- bash -l"*); `--class` overrides the windowing-system class / Wayland `app_id` |
| `gnome-terminal` | `--` | none | `gnome-terminal(1)`: *"use -- to terminate the options, and put the program and arguments to execute after it: … prefer to use gnome-terminal -- python3 -q"*. `--class` is documented but is the legacy GTK/X11 toolkit option, inert under Wayland — see below |
| `konsole` | `-e` (last) | none | The Konsole handbook's command-line options: *"-e command — Execute command instead of the normal shell"*. Konsole publishes no window-class option |
| `xterm` | `-e` (last) | none | `xterm(1)`, the original — the flag foot's manual names when explaining why it accepts and ignores it |

Two deliberate absences, both recorded rather than quietly dropped:

- **gnome-terminal has no identity here** even though `--class` exists in its manual.
  Under Wayland, GTK takes the application id from the application itself and ignores the
  toolkit option, so a class set this way would be delivered on some sessions and not on
  others. Promising an identity we can only sometimes keep is worse than saying there is
  none.
- **xterm has no identity here.** Its `-class` is an X toolkit option that takes its value
  as a *separate* argument, a second argument shape the table deliberately does not model
  — one shape applied uniformly is what makes the table readable. An xterm window is found
  by its own class, `XTerm`.

**The terminal on this machine was verified before shipping.** `[intents] terminal =
"ghostty"`; `ghostty(1)` was read from the installed man page, not from memory or a web
page, and both the `-e` and `--class` claims above are quoted from it. `foot(1)` likewise.
The other six come from each project's own published manual.

**An unknown terminal is refused by name**, with the list of the ones that are known:

> *"claude runs in a terminal, and I do not know how to run a command inside st — the
> terminals I know are alacritty, foot, ghostty, gnome-terminal, kitty, konsole, wezterm,
> xterm."*

A terminal that is *known but not installed* is a different failure with a different fix,
so it is a different sentence.

### 5. Identity: the launched terminal window is findable

A terminal-hosted program's window belongs to the terminal, not to the program, so
"focus Claude" would otherwise find a window classed `com.mitchellh.ghostty` among all the
others. #186's mechanism applies unchanged, one level up: the terminal is launched with a
class of our choosing, derived from the program's name.

The *form* of that class is the terminal's decision, not ours. Ghostty validates it as a
GTK application id and would refuse to start on a bare word, so it gets
`dev.jarvix.claude`; alacritty, kitty and foot accept a free-form token and get `claude`.
`desktop.AppName` reduces the three-element form back to `claude`, so the window resolves
under the name the user said either way.

The terminals' flags live in `launchkind` and `internal/routine`'s identity table defers
to them, rather than keeping a second copy: two tables that both claim to know what `foot`
accepts are two tables that will eventually disagree, and the one that would be wrong is
whichever was not edited.

### 6. `Terminal=true` is honoured, not refused

ADR 0058 refused a `Terminal=true` desktop entry in a routine step and told the user to
name their terminal in `app` and the command in `args`. The reasoning was right and the
remedy did not exist yet. It does now, so `routine.Resolver` wraps such an entry in the
configured terminal, classes the window with the entry's id, and reports which terminal it
opened inside. The step's own `identity` is still refused on a `desktop_entry` step, for
the reason it always was: the entry's `Exec` decides what runs.

### 7. The catalogue the model can consult

`desktop.list_apps` — an allow-tier read — lists what can be started here and how each one
starts: `claude — runs in a terminal`, `firefox — opens a window`. With no `match` it lists
this machine's *applications*, which is what a person means by "what can you open?"; with
a `match` the commands on PATH join in, which is how the model discovers that `claude` is
a terminal program rather than assuming from what a Linux machine usually has.

It is a read and it asks nothing, deliberately. The tool exists so the model can check the
machine instead of guessing at it, and a confirmation in front of that check would be an
incentive to skip the check and guess.

### 8. Lazily built, cached, and rebuilt when the machine changes

Reading every desktop entry under the XDG search path is a directory walk, and doing it per
launch is what the NFR forbids. The catalogue is therefore built on the first question, not
at startup — a daemon never asked to launch anything never scans an applications directory
at all — and then held.

The invalidation rule is *the directories it was drawn from*, not a clock. A five-minute
TTL would go on claiming the picture is current for five minutes after an install, and
would redraw it pointlessly for ever on a machine nobody installs anything on. What can
change an answer is an applications directory or a PATH directory changing, so those are
stamped when the catalogue is built and compared when it is next asked — at most once every
two seconds, which bounds the cost of *asking* without pretending to bound staleness. A
configuration reload calls `Invalidate` outright, because the overrides are part of the
picture.

The PATH inventory is built separately and only when a listing needs it: classifying one
named program costs a single `exec.LookPath`, and making a launch pay for a walk of every
PATH directory would be the same mistake in a different place.

### 9. What the user hears, per kind

| kind | what the tool returns | what the user hears |
| --- | --- | --- |
| terminal | `Started claude inside ghostty. … Do not say a window will appear on its own.` | "Claude is running in a terminal." |
| graphical | `Started firefox. … it is opening; its window will appear on its own.` | "Firefox is opening." |
| two candidates | `Several applications match "claude": claude, which runs in a terminal; claude-desktop, which opens a window. … Do not guess.` | "Do you mean the Claude command or the Claude Desktop app?" |
| unknown | `… I cannot tell whether it opens a window of its own or runs inside a terminal … Do not launch it and do not describe anything as opened.` | "Claude's installed, but I'm not sure how it starts — does it open a window?" |
| unknown terminal | `claude runs in a terminal, and I do not know how to run a command inside st …` | "Claude needs a terminal and I don't know how to drive st." |
| failed | `claude would not start. …` | "Claude wouldn't start." |

The kinds are named in the ambiguity question on purpose. Without them it is "claude or
claude-desktop?", which asks the user to know the thing this code was supposed to work
out; with them it is a choice between two outcomes.

The confirmation card carries the kind too — "I want to open claude in a terminal" — for
the ADR 0014 reason: the user approves what will happen, and "open Claude" and "open Claude
in a terminal" are two different things to say yes to.

### 10. What did not change

The model still sends one name and nothing else. ADR 0022's refusal to give it an
`arguments` parameter stands untouched: every extra element in a terminal-hosted argv comes
from the curated table above or from the program's own desktop entry, and the whole thing
goes to `execve` as a list. There is no shell at any point, so a name containing `;` is a
name that does not resolve, never a command.

## Consequences

- `desktop.launch_app` now reads desktop entries, which it never did. "Launch ChatGPT"
  works on a machine where ChatGPT exists only as an entry — a gain, and the reason the
  ambiguity criterion is reachable at all.
- The launcher takes an argv rather than a binary. A terminal-hosted program is its
  terminal plus flags plus the command, and there is no honest way to say that as one
  string: rendering it into a command line would mean quoting for a shell, and "we quote
  correctly" is a far weaker promise than "there is no shell".
- Two things answering to one name is now a question where it used to be a launch. On a
  machine with both `code` and a `code-oss` entry, "launch code" asks. That is the correct
  answer and it is slightly more talkative than before.
- The classification is only as good as the machine's entries. An application installed
  without one will be opened in a terminal until the user says otherwise — which they can,
  in the settings screen, in one line.
- A newly installed application is seen on the next question past the two-second recheck,
  not instantly. Nothing watches inotify; a directory stamp is compared when someone asks.
- `desktopentry.Index` grew `All()`, because `Lookup` trims a `.desktop` suffix and
  therefore cannot fetch `org.telegram.desktop` by its own id — a trap for any caller
  walking the index.
- Pinned by `internal/launchkind`'s tests (each kind against a machine the test wrote:
  PATH-only, `Terminal=true`, `Terminal=false`, an entry whose id is not its binary,
  ambiguous across kinds, unsurveyed, override, unknown terminal, terminal not installed,
  and the rebuild-on-change rule counting the scans) and by
  `internal/tools/desktop_launchkind_test.go` at the tool boundary, where the assertions
  are about the sentence the user ends up hearing.
- Documented in `README.md` and `docs/configuration.md`.
