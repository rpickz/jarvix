# ADR 0058 — A routine step launches what the desktop launches: entries, arguments, identities

**Status:** accepted (implements issue #175; extends ADR 0022 and ADR 0026)

## Context

A user built a routine to place four windows across two monitors, spoke its
phrase, and nothing opened. The daemon's own record of the run said
`placed=3 failed=3`, with three `routine step window never appeared … no
window appeared within 8s` warnings.

The routine could never have worked, and the reason was a capability gap
rather than a fault in the running. `[[routines.steps]]` could say `app`, and
`app` was documented as *"one bare executable name or absolute path, launched
through the compositor — never a command line, never a shell"*. On this
desktop, nothing they wanted is a bare executable:

- `X.desktop`, `ChatGPT.desktop`, `WhatsApp.desktop` and `Discord.desktop` all
  have `Exec=omarchy-launch-webapp <url>`, a Chromium `--app=` wrapper. Their
  windows take a class like `chrome-chatgpt.com__-Profile_3`.
- `signal` exists only as a desktop entry; `discord` and `whatsapp` have no
  binary on PATH at all.
- "Chrome under my personal profile" and "under my work profile" are the same
  binary, distinguished only by `--profile-directory`.

So every application in the intended routine needed either a desktop entry or
an argument, and the one step that *was* a bare binary — `app = "chromium"`,
`match = "facebook"` — launched a plain browser and then waited eight seconds
for a window nothing had told it to open. The user wrote a shell script
instead (`~/.local/bin/jarvix-workspace-setup`) and said the thing this record
exists to answer: *"We shouldn't be writing scripts to get around the
inflexibility of Routines — we should make the Routines better."*

Asked what a step should do when a matching window is already open, they chose
**per step**. That interacts with the hardest problem here, which is
identification, dealt with below.

## Decision

### 1. A step names a program or a desktop entry, and may carry arguments

The launching half of a step is now six keys — `app`, `desktop_entry`, `args`,
`identity`, `match`, `launch` — beside the placement half ADR 0056 defined.
Exactly one of `app` and `desktop_entry` says what to launch.

```toml
  [[routines.steps]]
  app = "chromium"
  args = ["--profile-directory=Profile 3", "--restore-last-session"]
  identity = "personal-browser"
  workspace = 1
  monitor = "top"
  mode = "tiled"
  width = "66%"
  place_next = "right"

  [[routines.steps]]
  desktop_entry = "ChatGPT"
  match = "chrome-chatgpt.com"
  workspace = 1
  mode = "tiled"
  height = "50%"
```

A desktop entry is read from the XDG search path
(`$XDG_DATA_HOME/applications`, then each `$XDG_DATA_DIRS/applications`), by
`internal/desktopentry`, and launched through its own `Exec` with the
specification's field codes applied — `%f`/`%F`/`%u`/`%U` removed because a
routine hands it no files, `%i` expanded to `--icon <Icon>`, `%c` to the
entry's name, `%k` to its path, the deprecated codes removed, `%%` a literal
percent. `Hidden=true` means the entry is absent, per the specification;
`NoDisplay=true` does not, because it only hides an entry from a *menu* and
several web-app wrappers are written that way.

The `Exec` value is parsed by the **specification's** quoting grammar, not by
a shell: arguments split on unquoted whitespace, double quotes group, and a
backslash escapes one of `" ` $ \` and nothing else. A desktop file is
writable by anything the user installs, so the promise is "we launch what it
names", not "we hand its line to `sh`".

An entry with `Terminal=true` is **refused** rather than launched. It needs a
terminal wrapped round it, and starting it without one produces exactly the
failure this ticket exists to end: a process that starts, maps nothing, and is
waited on for eight seconds. The refusal names the entry and says to write the
terminal in `app` and the command in `args`.

### 2. Arguments are an argv, and that is the whole of the security argument

`args` reaches `execve` as a list. It is never joined into a string, so there
is no quoting to get right and nothing for a shell to interpret, because there
is no shell. A value containing `;`, `&&`, a backtick or `$(` is one argument
containing those characters, and a test pins each of them.

**ADR 0022's refusal to give the model-facing `desktop.launch_app` tool an
arguments parameter is unchanged.** That is worth stating plainly, because
"routines got arguments" sounds like the opposite of it. The distinction is
authorship, not mechanism:

| | who writes it | what it may say |
| --- | --- | --- |
| `desktop.launch_app` | the model | one program name, matched against the machine's own list |
| `[[routines.steps]] args` | the user, in their config file | a literal argv |

The model can propose a routine through `config.write_entry`, and the
confirmation card shows every argument verbatim (ADR 0014's discipline), so an
argv the assistant suggests is an argv the user reads before it is written.
The assistant still cannot reach `[tools]`, and there is still no
command-line step kind — a shell behind a spoken phrase is what `[[scripts]]`
is for (ADR 0030), deliberately gated and deliberately separate.

### 3. Two launch paths, and the asymmetry is on purpose

- A step naming a **bare program with no arguments** is still started through
  the compositor's spawn dispatcher, exactly as every routine in the field
  already is. That path makes the application a child of the compositor, with
  the graphical session's environment, which matters for a daemon started
  outside that session — and it works today. Changing it for steps that asked
  for nothing new would be spending a working feature on symmetry.
- Anything carrying **arguments, a desktop entry, or an identity** cannot go
  that way at all: `hl.dsp.exec_cmd("…")` takes a command *line* and the
  compositor hands it to a shell. Those steps are started directly by the
  daemon, on `desktop.launch_app`'s detached-child discipline (ADR 0022): own
  process group, context does not kill it, credential-shaped environment
  variables withheld.

### 4. Identity: how two windows of one process are told apart

This is the hard problem, and the honest answer is narrow.

**Verified on the machine:** Chromium runs every profile in **one process**. A
window on the personal profile and a window on the work profile have the same
window class, the same PID, and byte-identical `/proc/<pid>/cmdline`. No
amount of looking at a running desktop distinguishes them. Adoption — "is a
matching window already open?" — therefore *cannot* identify a profile, and
two steps for two profiles fight over whichever window the compositor lists
first.

The only thing that can distinguish them is a decision made **before the
window exists**: launch it with a class nobody else uses. Chromium accepts
`--class=`, so `identity = "work-browser"` appends that flag and makes `match`
default to the identity. The routine then recognises its own window on every
subsequent run.

`identity` is sugar over `args` + `match` for programs whose flag spelling
this repo has confirmed — the Chromium family, the Firefox family, Alacritty
and Kitty (`--class=`), Foot (`--app-id=`). The table is curated rather than
guessed: a wrong spelling would be an argument the program rejects at
start-up, a launch that fails for a reason the user never wrote.

**For a program that offers no such flag there is no mechanism, and the
schema says so instead of pretending.** `identity` on such a program is
refused when the routine is saved, with the message naming the programs that
do take one and pointing at `match`. A user in that position has three
options, all of which the refusal implies: give the two steps distinct
`match` queries if the windows differ in title or class at all; write the
flag themselves in `args` if their program has one under a different name;
or accept that the two windows are interchangeable, which for two terminals
or two editors is usually true. What is *not* offered is a heuristic — PID
order, mapping order, "the newest window" — because each of those is right
most of the time and wrong silently, and a layout that comes out backwards
one morning in three is worse than one that refuses to be written.

`identity` is refused on a `desktop_entry` step for the same honesty: the
entry's own `Exec` decides what runs, and a class flag appended to a wrapper
script is an argument the wrapper never passes on.

Relatedly, two steps that **could not be told apart** are refused at load:
same effective `match`, different launch arguments. Same arguments is *not*
a problem — two terminals are interchangeable and the runner already claims
windows one at a time.

### 5. Adopt or launch, per step

`launch = "if_missing"` (the default, and what every routine written before
this key did) adopts a matching window when one is open. `launch = "always"`
starts a new one every run, and the run excludes every window that was already
open from what that step may place — otherwise it would open a new window and
then place the old one, which is the instruction inverted.

Per step because both answers are right for different steps of one routine: a
browser left open all week should be adopted; a scratch terminal should be
fresh. One global setting would make one of those two steps wrong every time.

### 6. Where each question is asked

Two questions look like one and are not:

- **"Is this step well formed?"** — a fact about the file. Asked at load, for
  every daemon that reads the document: the schema, the argument shapes, the
  identity's applicability, the two-steps-alike rule, and **whether the
  desktop entry exists**. A missing entry is a name the routine invented and
  nothing on the machine can make it right, so it fails the load, naming the
  entry and the closest installed spellings.
- **"Can this machine run it *right now*?"** — a fact about the machine at
  this moment. Reported when a routine is **saved** (the window's form and the
  assistant's config tool alike) as a **note**, not a refusal, and enforced at
  the run, where the step is skipped by name.

Program existence is deliberately *not* a load-time rule, and — after a first
attempt got this wrong — deliberately not a save-time refusal either.

A load-time rule would stop the daemon starting because the user uninstalled
something a routine mentions: the file did not change, the machine did, and
the punishment would land on reminders, briefings and everything else. It
would also make this repo's own documented examples invalid on any machine
without `firefox`.

A save-time *refusal* is wrong for a different and more important reason. It
makes `config.toml` unwritable exactly where it most needs editing — a new
laptop, a machine being set up, an application the user is about to install.
Authoring the routine first and installing the program second is ordinary, and
a routine written on a desktop must stay editable from a machine that has none
of it. (It also failed CI, which is the cheap version of the same lesson: the
runner has no `alacritty`, and a routine naming one could no longer be
written.)

So it travels the **notes** channel the entry registry already has (#163) —
"something true about a saved draft that is not a problem, stated in the
user's words and keyed to the field that causes it". The form shows it as a
caution worded about *this computer right now*, and the user saves through it.
Reusing that channel rather than inventing a second one matters: the two are
rendered differently on purpose, and a caution shown in the problem channel
reads as a refusal on a draft the daemon will happily accept.

The enforcement point is the **run**, which resolves the same way from the
same code and says *"discord is not installed"* by name — skipping the step
rather than starting nothing and waiting eight seconds for its window, which
is the behaviour this ticket was actually reported for.

A missing **desktop entry** stays a refusal, and stays in whole-document
validation. An entry id is resolved out of the machine's own applications
index; nothing will ever install one under a name the user invented, so there
is no "not yet" reading of it — it is a typo, and telling the user at once
with the closest installed spellings is the whole value of checking.

### 7. Failure is classified, not narrated

A step that produced no window now says which of four things happened, in the
spoken summary, the `routine.step` event (`failure`), and the log:

| kind | what the user hears | what to do about it |
| --- | --- | --- |
| `not_installed` | *"discord is not installed"* | install it, or fix the name. Decided before anything starts, so the feed calls the step **skipped** rather than failed |
| `did_not_start` | *"chromium did not start"* | look at the journal |
| `no_window` | *"chromium opened no window within 8 seconds"* | it is slow, or it crashed |
| `no_match` | *"chromium opened a window, but nothing matched \"facebook\""* | fix `match` |

`not_installed` is decided *before* anything is started, so a routine naming
an application this machine does not have costs no launch and no wait.
`no_window` and `no_match` are separated by comparing the window list against
what was on screen when that step's wait began — waits run one at a time, so
each window is attributed to a single step, which is the best attribution an
observer of a window list can make and the same one the user's own script
made.

## Consequences

- The user's whole morning layout is expressible as one routine. It is pinned
  as a fixture (`internal/routine/fixture_test.go`), argv and dispatch
  sequence both, so the shell script it replaces can be retired.
- A step is now two halves with one schema: `routine.LaunchFields()` +
  `placement.Fields()` is exactly the set of `[[routines.steps]]` keys, and a
  contract test fails if any of the three drifts. That is the interface the
  routine editor (#181) builds its controls from.
- Reading desktop entries is a directory walk. It happens once per runner, and
  only when some step actually names one — a configuration written before this
  change touches no applications directory at all.
- An application installed while the daemon is running is not visible to a
  `desktop_entry` step until the next config reload. That is the same
  staleness every other configured fact has, and a newly installed application
  needs a reload anyway.
- Localised desktop-entry keys (`Name[de]`) are ignored; the unlocalised value
  is used. Desktop *actions* (`[Desktop Action …]`) are not modelled — a step
  naming one is naming something this package deliberately does not have yet.
