# Jarvix

[![ci](https://github.com/rpickz/jarvix/actions/workflows/ci.yml/badge.svg)](https://github.com/rpickz/jarvix/actions/workflows/ci.yml)
**Voice → Computer.** A voice-native interaction layer for
[Omarchy](https://omarchy.org) (Arch Linux / Hyprland / Wayland).

Hold a key, speak, release. Jarvix transcribes locally, streams an AI response
into a minimal native overlay, and speaks it aloud — with instant,
first-class interruption. The inspiration is the Star Trek computer / JARVIS
interaction model: the computer feels present, immediate, and unobtrusive,
not like a chat app you have to operate.

```text
Hold Super+Alt+V   →  overlay appears: ◉ Listening
Speak              →  "why is my build failing?"
Release            →  transcribed (whisper.cpp, on device)
                   →  response streams into the overlay
                   →  Jarvix speaks it aloud (Piper, on device)
                   →  overlay fades
```

Jarvix is not dictation, and it is not a voice wrapper around a chatbot. Ask
"what's happening in docker?" and it runs `docker ps` itself and summarises
the result aloud. Say "volume thirty" and it just happens — in microseconds,
with no model call at all, because an explicit intent table sits in front of
the AI ([ADR 0017](docs/adr/0017-deterministic-intent-router.md)). Ask
something its small local model has no business answering and it hands the
question to a stronger assistant CLI you already have installed — "ask Claude
to review my publish pipeline" — and speaks the answer back. Say "put me back
in my browser" and it finds the window and switches to it; ask what you have
open and it tells you
([ADR 0022](docs/adr/0022-desktop-window-control.md)). The long-term
goal is a system that routes natural intent to the right mechanism:
deterministic local commands, desktop context, AI reasoning, tools, or
speech — see [docs/roadmap.md](docs/roadmap.md).

## Status

**Milestone 1 (voice conversation vertical slice) — working.**
One-turn interactions: push-to-talk → local STT → streaming AI → local TTS,
with an Omarchy overlay and full cancellation. See
[docs/implementation-plan.md](docs/implementation-plan.md) for what's next.

## Requirements

- Omarchy 4.x (Quickshell shell with the plugin registry), Hyprland, Wayland
- PipeWire (`pw-record` / `pw-play`)
- Go ≥ 1.25 (to build)
- [whisper.cpp](https://github.com/ggerganov/whisper.cpp): `sudo pacman -S whisper-cpp`
- [Piper](https://github.com/rhasspy/piper) + a voice (zero-setup default):
  `paru -S piper-tts-bin piper-voices-en-us` (AUR); or Kokoro for a much more
  natural voice via `scripts/setup-kokoro.sh`
- An AI backend: [Ollama](https://ollama.com) locally (default), or any
  OpenAI-compatible endpoint (OpenAI, OpenRouter, LM Studio, …)
- Optional, and only if you turn it on: [wtype](https://github.com/atx/wtype)
  (`sudo pacman -S wtype`) for **Jarvix typing on your behalf** — see
  [Letting Jarvix type for you](#letting-jarvix-type-for-you) before you do

## Installation

### From a release (recommended)

Grab the tarball for your architecture from the
[latest release](https://github.com/rpickz/jarvix/releases) (checksums in
`SHA256SUMS`), follow its `INSTALL.md`, then let the first-run wizard walk
you through the machine-specific choices — voice engine, push-to-talk
access, AI provider, advisor CLIs — verifying each step as it goes:

```bash
jarvix setup            # idempotent: re-run any time, finished steps are skipped
```

An AUR package is seeded at `packaging/arch/PKGBUILD` (build with
`makepkg -si` until it is published).

### From source

```bash
git clone https://github.com/rpickz/jarvix.git
cd jarvix

make install            # builds and installs jarvix + jarvixd to ~/.local/bin
make install-systemd    # installs the user service
systemctl --user enable --now jarvixd

jarvix setup            # first-run wizard: voice, activation, AI, advisors + verify
```

The wizard covers the pieces below; they remain available individually:

```bash
jarvix setup whisper    # downloads the Whisper model (~148 MB, one-time)
make install-plugin     # links the Omarchy plugin and puts Jarvix in the bar
make install-hyprland   # adds the push-to-talk keybindings
jarvix doctor           # verifies every dependency, explains anything missing
```

For the default local setup, make sure Ollama is running with the model:

```bash
sudo systemctl enable --now ollama
ollama pull llama3.2:3b
```

`jarvix --version` + `jarvix doctor` together describe an installation —
include both in bug reports.

### Upgrading

A source install updates itself, safely:

```bash
jarvix upgrade --check  # what's available vs what's installed — changes nothing
jarvix upgrade          # fetch, build, install, restart, health-gate
```

`jarvix upgrade` fast-forwards your checkout to `origin/main`, builds
through the Makefile, installs the pair into a versioned slot under
`~/.local/share/jarvix/releases/`, restarts the daemon, and then holds the
result to the doctor's health gate: socket answering, protocol match, and
the real engine probes — whisper actually transcribes, the TTS engine
actually synthesizes. **If any of that fails, it automatically rolls back**
to the previous release, restarts onto it, re-runs the gate to confirm
recovery, and names the failing check verbatim. The previous version is
always kept on disk, so a bad build costs a restart, not your assistant.

Some ground rules it enforces (ADR 0044):

- your checkout is yours: uncommitted changes, a diverged branch, or being
  off `main` make it refuse with the exact git state — it never stashes,
  resets, or touches your work;
- a build failure installs nothing and leaves the running daemon untouched;
- when the update changed the shell plugin's QML it tells you a shell
  restart is pending and offers `omarchy-restart-shell` (daemon-only
  changes say so) — it never restarts your shell for you;
- one upgrade at a time — a second invocation refuses on the lock.

## Configuration

Jarvix works with **no configuration file** on a machine with Ollama and
Piper installed. To customise, create `~/.config/jarvix/config.toml`:

```toml
[ai]
provider = "ollama"        # or "openai", "openrouter", "lmstudio", or your own
model = "llama3.2:3b"

[tts.piper]
voice = "en_US-amy-medium"
```

Cloud providers read API keys from the environment (`OPENAI_API_KEY`,
`OPENROUTER_API_KEY`) — never from the config file. For the daemon:

```bash
systemctl --user set-environment OPENAI_API_KEY=sk-...
systemctl --user restart jarvixd
```

One model has to be both quick and good, and none is. Configure two or three
under `[ai.tiers]` and pick between them per conversation — **Quick /
Balanced / Deep**, beside the composer or by saying "switch to deep" — and
Jarvix tells you which one answered. With an instant tier configured, the small
model also **hosts** while a heavier one answers: if the answer has not started
within 700ms it says one short line over the wait, or asks which of two things
you meant. It never answers anything, holds no tools, and a line that states a
fact or claims an action is thrown away rather than spoken. Nothing changes
until you configure a tier: see
[Thinking levels](docs/configuration.md#thinking-levels-aitiers).

All options: [docs/configuration.md](docs/configuration.md).

## Using Jarvix

| Interaction | How |
|---|---|
| Talk to Jarvix | **Hold `Super+Alt+V`, speak, release** |
| Talk to Jarvix hands-free | **Say "Jarvix, …"** — off by default; see [Hands-free](#hands-free-say-jarvix) |
| Close the microphone | `jarvix mute` (and `jarvix unmute`) |
| Cancel / stop speech | `Super+Alt+Escape` (or `jarvix cancel`) |
| Interrupt mid-speech | Hold the chord again — it stops talking and listens |
| Type instead of speaking | **Type in the conversation window and press Enter** — same conversation, same tools, same spoken answer |
| Trade speed for quality | **Quick / Balanced / Deep beside the composer**, or say "switch to deep" / "think hard about this, …" — needs `[ai.tiers]` configured |
| Ask from a terminal | `jarvix ask "explain recursion in one sentence"` |
| Voice from a terminal | `jarvix listen` |
| Review the conversation | **Click the Jarvix icon in the bar**, click the notification when Jarvix answers, or `Super+Alt+C` / `jarvix window` |
| See what Jarvix is doing | The bar icon — hover it for the state in words |
| Actions without speaking | **Right-click the bar icon**: window, new conversation, settings, recent artifacts |
| Set up your desktop in one sentence | **Say a routine's phrase** ("Jarvix, morning setup") — your apps launch onto your workspaces, arranged: a browser at two thirds of the top screen, two more stacked in the remaining third, floating, pinned, fullscreen or maximised, on the monitor you name. One placement vocabulary, used by routines, the window tools and voice alike ([ADR 0056](docs/adr/0056-window-placement-vocabulary.md)); `jarvix routines` lists them, `jarvix routines run "morning setup"` triggers one, and the conversation window has a Run button. Defined as `[[routines]]` in config.toml — see [docs/configuration.md](docs/configuration.md#routines-routines) |
| Launch anything your desktop can launch | A routine step opens a **program with arguments** (`args = ["--profile-directory=Profile 3"]` — an argv, never a shell line) or a **desktop entry by name** (`desktop_entry = "ChatGPT"`), so web apps, browser profiles and applications with no binary on `PATH` are all expressible. Each step says whether it re-uses a window that is already open or starts a fresh one, and two windows of one process — the classic two-Chrome-profiles problem — are told apart by launching each with a class of its own. A step that produced nothing says *which* nothing: not installed, opened no window, or opened one that did not match ([ADR 0058](docs/adr/0058-step-launching.md)) |
| Launch a program the way *that* program starts | **"Jarvix, launch Claude"** — a command-line tool opens **inside your terminal** (`[intents] terminal`, the same one "open a terminal" uses) and the answer says so; a graphical application opens directly. Jarvix works out which from the machine itself: an application ships a desktop entry, a command does not, and an entry that says `Terminal=true` is believed. Two things answering to one name — the `claude` command and a Claude Desktop app — is a question, never a guess, and a program it cannot classify is asked about rather than launched hopefully. "What can you open?" reads the catalogue rather than guessing at it. Correct it once in **Settings → Programs to open inside a terminal** ([ADR 0061](docs/adr/0061-how-each-program-is-started.md)) |
| Build a routine in a form, and see it before you run it | **Automations → Routines** in the conversation window: a control for every value a step has — what to launch, adopt or launch fresh, screen, workspace, mode, share in percent or pixels, where the next window goes — with the daemon's own validation on the field it belongs to. Beside it, a **preview diagram** per workspace, drawn to your screen's real proportions, with a labelled rectangle per window and its share; it redraws as you type and when you move a step, because step order decides the tiling. An arrangement that cannot happen is not drawn — the reason appears in its place — and every step also reads as a sentence, so the picture is never the only way to check it ([ADR 0059](docs/adr/0059-the-routine-preview-is-the-daemons-drawing.md)) |
| Save the desktop you already arranged | **"Jarvix, save this as my morning setup"** — the live window layout is read once and written as a `[[routines]]` entry (comment-preserving, provenance-stamped), immediately runnable and hand-editable; an existing name asks before it is replaced. See [docs/configuration.md](docs/configuration.md#capturing-a-routine-from-the-live-desktop-save-this-as-) |
| Run your own script by voice | **Say a script's phrase** ("Jarvix, backup my notes") — a `[[scripts]]` entry in config.toml names an executable and its trigger phrases; it runs behind the permission gate (asks first by default, naming the script and its path), with zero arguments, and speaks the outcome. `jarvix scripts` lists them, `jarvix scripts run "backup notes"` triggers one, the window has a Run button — see [docs/configuration.md](docs/configuration.md#scripts-scripts) |
| Call a window by a name you chose | **"Jarvix, call this window builds"** — the focused window gains a one-word nickname that works anywhere you'd describe a window ("focus builds", "move builds to workspace 3"), resolved before any app/title matching. "What are my windows called?" or `jarvix windows` lists them; names are per-session and released when the window closes ([ADR 0040](docs/adr/0040-window-nicknames.md)) |
| Call a screen by a name you chose | **"Jarvix, call this monitor top"** — a one-word name for a monitor, usable anywhere a screen is named (`monitor = "top"` in a routine step, "put this on the bottom screen"). Names are resolved when the routine RUNS, so moving a cable is one correction rather than an edit to every routine; a screen that is unplugged says so — *"no monitor is called top right now: it means DP-2, which is not plugged in"* — and the rest of the routine still lands. "What are my screens called?", `jarvix monitors`, or Automations → Screens in the window ([ADR 0057](docs/adr/0057-monitor-nicknames.md)) |
| Ask where everything is | **"Jarvix, what's going on?"** (or "where are we", "status report") — one short spoken answer about the whole machine, ordered by what deserves attention: what is waiting on you first (an AI session classified needs-you leads, by name), then what is still running, then what has finished since you last looked, then anything failing, then quiet housekeeping. Specifics, never categories, and a short honest *"Nothing needs you."* when there is nothing of note — never a manufactured list. A source that could not be read is named rather than dropped, and the report says so up front when a restart means its own record cannot cover the whole stretch. The **Situation** tab in the conversation window has the full version, every line linking to the thing it describes ([ADR 0061](docs/adr/0061-the-situation-report.md)) |
| Give a direction, not a command | **"Jarvix, work on tidying my downloads"** — a **job**: work that outlives the conversation that asked for it. It has a goal in your own words, a **scope** you set (which directories, which applications, which tools), checkpoints it resumes from, and an honest report. The scope is stated back before it starts and then **enforced by the daemon before every single action** — never merely described to the model — so an attempt outside it stops the job and parks it with the reason, having done nothing. Anything that cannot be undone still stops and asks, however long the job has run; because nobody may be there, it **parks on the question** instead of interrupting, and answering later resumes it exactly where it stopped. A parked job turns up in "what's going on?" and in your return briefing. **"How's the tidy job?"** and **"stop the tidy job"** are sentences. It survives a restart, and a step it started and never saw the end of is reported as unknown rather than as done ([ADR 0065](docs/adr/0065-jobs-that-outlive-a-conversation.md)). A job can run shell commands too, held inside its scope by the kernel rather than by a reading of the command — and refused outright on a machine that cannot hold them there ([ADR 0068](docs/adr/0068-a-command-the-kernel-holds.md)). You never have to ask a model to look: the window's **Jobs** tab and `jarvix jobs` show every job with its state, its goal, its scope and what it is waiting for, and answer, stop or resume it from there ([ADR 0067](docs/adr/0067-jobs-you-can-see-and-steer.md)) |
| Hand one window over, and keep the rest to yourself | **"Jarvix, take control of this terminal"** — Jarvix asks once, naming the window, and from then on it may read, move and type in that one. A window Jarvix opened is managed from birth. **Being managed is not permission to run anything:** text typed into a terminal is classified and confirmed exactly as a shell command is — the verbatim command on the card, compound commands split and judged part by part, risky words always asked about, deny rules refusing outright — so acquiring a terminal buys access, never execution. "Let this go" releases it immediately and is never gated. "What do you manage?", or Approvals → Windows Jarvix manages in the window ([ADR 0062](docs/adr/0062-managed-and-unmanaged-windows.md)) |
| See what each window is for, at a glance | **Anchor, name, or hand over a window** and a tiny static chip appears in its top-right corner: a square mark for a window Jarvix manages, a thread badge (filled = active thread), the nickname tag, and — for an anchored AI session — a working / needs-you / done mark. Each mark is its own shape, so none of them depends on colour. Nothing animated, nothing clickable, unenrolled windows stay clean; `overlays.enabled = false` turns it all off. The bar shows the active thread's name beside the icon ([ADR 0044](docs/adr/0044-window-overlays.md)) |
| Fresh conversation | `jarvix new` — the current thread is archived, not destroyed |
| Past conversations | `jarvix conversations` lists them; `show`, `open`, `delete <id>`/`--all` — or the window's **Library** tab |
| Change the accent | `jarvix voices` lists what is installed, by language and gender |
| Health check | `jarvix doctor` |
| Daemon state | `jarvix status` |

### The bar widget

Jarvix lives in the top-right of the Omarchy bar, next to the tray and the
network and audio widgets. The icon says what Jarvix is doing at a glance —
ready, listening, thinking, responding, speaking, waiting for a confirmation,
or stopped — with a different **shape** for each state, and the same thing in
words on hover, so it never depends on colour alone. While a turn is running
the words are on the bar itself, next to the icon — "Thinking 4s", "Listening",
"Confirm? 6s" — so a wait is legible without hovering and without reading a
glyph; at rest the chip is gone and the widget is the bare icon it always was.
With background listening
on it is also the microphone indicator: a hollow microphone whenever a capture
process is open, struck through when muted. A stopped daemon dims the
icon and offers the start command; it never disappears, because an icon that
is not there cannot be told apart from a plugin that was never installed.

- **Left click** toggles the conversation window (the same route `jarvix
  window` and a clicked notification take — there is only ever one window).
- **Right click** opens the panel: mute or unmute the microphone (when
  background listening is on), the conversation window, a new conversation,
  settings, and the recent artifacts, each one row you can also reach with the
  arrow keys and Enter.
- **Middle click** starts a fresh conversation.

`make install-plugin` puts it in the bar's `right` section. To place it
yourself, or to move it:

```bash
omarchy plugin enable jarvix right          # or left / center
omarchy plugin enable jarvix --before omarchy.tray
```

Everything the widget shows is decided in Go (`internal/desktop/barstatus.go`)
and compiled into `plugin/omarchy/BarState.js` by `go generate
./internal/desktop`; the QML only draws it. Change a label or a glyph there,
regenerate, and the tests keep the two in step.

### Hands-free: say "Jarvix, …"

Optional, and off by default. With background listening on, saying "Jarvix,
what's my disk usage?" activates the assistant: the rest of the sentence is
the request, the silence after it submits, and a second "Jarvix, …" interrupts
an answer in progress. Push-to-talk keeps working exactly as before.

```bash
scripts/setup-wake.sh                        # a local detector, in its own venv
jarvix config set activation.mode=wake_word
systemctl --user restart jarvixd
```

Leaving a microphone open is a big ask, so here is exactly what it means
([ADR 0024](docs/adr/0024-background-wake-word-listening.md)):

- Detection runs **on this machine**, in a local process. There is no network
  path in the wake code.
- Audio from *before* the wake word lives only in a **fixed-size RAM ring** —
  1.2 seconds by default, hard-capped at 3 — and is **never written to disk or
  logged**. Wake events record a timestamp and a confidence, nothing else.
- Only the request *after* the wake word becomes a file, on tmpfs, deleted the
  moment it is transcribed — exactly what a push-to-talk capture does.
- **`jarvix mute` kills the capture process.** Not a flag that makes Jarvix
  ignore what it hears: `jarvix status` prints the pid, and after muting `ps`
  will not find it.
- The **bar icon shows a hollow microphone** whenever a capture process is
  running, and a struck-through one when you have muted. Right-click to mute
  or unmute.

Two things worth knowing before you leave it on. openWakeWord ships no model
for "Jarvix", so the installer uses `hey_jarvis` — `jarvix status` reports
what is really loaded, and it answers to "hey Jarvis" far more reliably than
to "Jarvix". And there is no echo cancellation, so Jarvix saying its own name
in an answer can retrigger it; PipeWire's `module-echo-cancel` is the fix.

### Typing to Jarvix

The conversation window has a text field at the bottom. Type a question,
press Enter, and it joins the same conversation — same history, same tools,
the answer spoken the same way — as if you had said it. Use it for the things
speech is bad at (`summarise https://example.com/some/long/path`), for the
times speaking is not an option, and to correct a bad transcription by
retyping instead of repeating yourself louder.

Between pressing Enter and the first word of the answer, the conversation
shows a pending turn from Jarvix saying what it is doing — "Thinking",
"Running a shell command", "Consulting claude" — and, once the wait passes a
couple of seconds, how long it has been doing it. It turns into the answer in
place when the first token arrives, so there is never a placeholder to watch
disappear, and it says what happened instead of freezing if the turn fails or
is cancelled. A window opened halfway through a long think shows the same
thing, counting from the same instant, as one that was already open.

Typing while Jarvix is answering interrupts it and starts the new turn, just
like speaking over it. If Jarvix is waiting on a tool confirmation, typing
"yes" or "no" answers *that* rather than asking something new. Shift+Enter is
reserved for multi-line composition and deliberately does not send.
`scripts/verify-typed-input.sh` walks the whole thing on a live session.

### Letting Jarvix type for you

The section above is you typing *to* Jarvix. This one is the opposite, and it
is **off by default**: Jarvix can type *for* you — into the document, form
field or chat box you are working in — but only if you switch it on.

```bash
sudo pacman -S wtype                          # the Wayland virtual keyboard
jarvix config set tools.typing.enable=true    # then restart jarvixd
```

**What enabling it grants, in plain English.** Jarvix gains the ability to
enter characters into whatever window has focus, as if you had typed them
yourself. That is genuinely the most powerful thing you can give it — more
than `shell.run`, because a shell command at least names what it is going to
do, and typing goes wherever your attention happens to be. So:

- **Nothing is typed without you saying yes.** Jarvix speaks a confirmation
  naming the window *and* reading back the exact text, every time. The
  sentence is built from the live window list and the literal characters, not
  from the assistant's description of what it is doing.
- **It cannot press Enter as part of typing.** Line breaks, Tab, Escape and
  the rest are refused outright — the text goes in and stops. Submitting is a
  separate tool with its own confirmation, so approving "type this" is never
  approving "and send it".
- **It re-checks where you are.** The window is captured when you are asked
  and checked again the instant before the keys go out. If a notification, a
  dialog or your own hand moved focus while you were answering, nothing is
  typed at all and Jarvix tells you why.
- **Typing into a terminal always asks**, even if you configured typing to
  run silently, and the confirmation says that is what it is.
- **There are caps.** A maximum length per request and a rate limit, both
  refusing with a reason, so a confused assistant in a loop cannot type
  indefinitely.
- **The text is never written down.** Which window, how many characters and
  whether you approved it are recorded (`jarvix status --last`, and an event
  on the bus); *what* was typed is not, anywhere, because you may have
  dictated a password.

Turning it off is one command and takes effect on the next daemon start:

```bash
jarvix config set tools.typing.enable=false
```

The threat model — what a confused or adversarial assistant could do with a
keyboard and which control blocks each path — is written down in
[ADR 0023](docs/adr/0023-synthetic-keystrokes.md). `jarvix doctor` reports
whether typing would work here and why not if it would not.

Jarvix remembers the conversation: ask a follow-up ("what should I change?")
and it keeps the prior context. A conversation only ends when you say so —
"start a new conversation", the window's **New chat** button, the bar menu, or
`jarvix new` ([ADR 0038](docs/adr/0038-conversation-lifecycle.md)); sessions,
interruptions, idle time, and daemon restarts never end it (an optional idle
window is configurable for those who want auto-forget back). Interrupting an
answer keeps the exchange too, marked interrupted, so answering a clarifying
question never loses the question. Answers are spoken **as they stream** —
Jarvix starts talking on the first complete sentence rather than waiting for
the whole reply.

**Approve and don't ask again.** The confirmation card offers a third answer
beside Approve once and Reject, with the exact rule it would add printed on
the button — `docker ps`, `xdg-open`, `kubectl get pods`. Choosing it appends
that word prefix to `[tools.policy] shell_allow`, so the next matching command
runs without a question; `…just this conversation` grants the same rule in
memory only and never writes it down. The rule offered is always the narrowest
useful one — the command's leading words up to the first argument that varies
— so one rule covers `docker ps` and `docker ps --format '{{.Image}}'`, which
is the nuisance this removes: not one command repeating, but the same intent
in different spellings.

Some commands are never offered the button, and the card says why in a
sentence: a word-prefix rule cannot say "but not those flags", so `rm`, `sudo`
and their family, `find` (`-delete`), `git` (`push --force`), wrappers like
`timeout` that run whatever follows them, `xdotool`, `jarvix` itself, anything
invoked by path, and `docker run`-shaped subcommands get Approve-once and
Reject only. Deny rules and the always-risky words still beat every rule, so a
remembered `ls` can never authorise `ls; rm -rf ~`. A pre-approved run appears
in the activity feed as "Ran without asking", naming the rule — nothing behind
a standing grant is silent. See them with `jarvix approvals list` or the
window's **Approvals** tab, take one back with `jarvix approvals forget docker
ps` (immediate, no restart), or ask "what have I pre-approved?" out loud. The
assistant cannot add, change or remove one — `[tools.policy]` is structurally
unreachable from its configuration tools
([ADR 0053](docs/adr/0053-remembered-approvals.md)).

The Approvals view shows the **deny** list beside the allow list and edits
both: an allow rule typed by hand faces the identical refusal matrix the card
uses, so the two routes cannot disagree; a deny rule faces none, because a gate
that argued with someone making it stricter is a gate people route around; and
**removing** a deny rule asks first, with a sentence naming what that rule
protected ([ADR 0054](docs/adr/0054-the-last-config-file-holdouts.md)).

**Where did that come from.** Every answer that used something you can get
back to carries a **What went into this** control in the window: unfold it and
each source is listed, and each one takes you there — the Knowledge tab at that
feed, the Memory tab at that fact or taught phrase, that conversation opened in
the Library, the window a focus thread is anchored to, an artifact opened in
its viewer. An answer that used nothing shows nothing. Ask "where did that come
from?" out loud for the same list.

The label is deliberately *what went into this*, never *what I cited*: which
remembered fact an assistant actually leaned on is not knowable, and asking a
model to attribute its own answer invites it to invent a citation that reads
exactly like a real one. So the list is derived from what Jarvix put in front
of the model and what a tool returned, and it says which of the two each source
is — **available to the answer** for something that was in context, **returned
during this turn** for a tool that ran and produced output. It stores
references only, never copies: a fact you have since forgotten says it was
forgotten rather than quoting a stale copy back at you, a deleted feed or a
removed file says so too, and neither offers a button that would do nothing
([ADR 0055](docs/adr/0055-answer-provenance.md)).

Conversations are durable ([ADR 0027](docs/adr/0027-durable-conversation-archive.md)):
`jarvix new` archives the thread instead of destroying it, whole — the
`history_turns` cap only limits what the model is sent, never what is kept.
Tool approvals are part of the record too
([ADR 0039](docs/adr/0039-approvals-in-the-record.md)): every confirmation
exchange — what was asked, the exact command, and whether you approved,
declined, or let it time out — survives closing the window, is shown in place
when the history is rebuilt, and rides `show`/`open` like any turn.
`jarvix conversations` lists the archive newest-first (the window's
**Library** tab shows the same library), `show` prints one read-only,
`open` continues one as the active thread with its context, and
`delete <id>` / `delete --all` actually remove the files — the archive lives
under your XDG state dir, private (0600), and never leaves the machine. Set
`conversation.retention = "off"` to stop archiving entirely; it removes
nothing already kept.

The archive is searchable ([ADR 0028](docs/adr/0028-conversation-search.md)):
ask Jarvix "what did we decide about the deployment approach?" and it
searches past conversations with the `conversations.search` tool, quotes
what it finds, and says when it was said in words ("last Tuesday") — and if
nothing matches, it says it found nothing rather than inventing a
recollection. The same search is yours directly: type in the Library tab's
search box, or run `jarvix conversations search <query>` (which works with
the daemon stopped, straight off the files). Results are ranked — exact
phrase first, then recency — and distinguish "earlier in this conversation"
from past ones.

Hold-to-talk is watched by the daemon itself (evdev — the same mechanism
Mumble and Discord use), because compositor release-binds are unreliable for
modifier chords. It needs one-time read access to keyboard devices:

```bash
jarvix setup input     # prints the udev rule + commands, states the trade-off
```

Without that access Jarvix automatically falls back to tap-to-toggle on the
same chord (tap to listen, tap to submit), plus `F10` as a bare-key hold.
The chord is `activation.ptt_chord` in the config; plain `Super+V` stays
Omarchy's universal paste, and the installer + `jarvix doctor` verify Jarvix
never clashes with another binding. See
[ADR 0004](docs/adr/0004-keyboard-activation.md) and
[ADR 0008](docs/adr/0008-daemon-side-push-to-talk.md) (including the privacy
model: non-chord key events are discarded immediately and never logged).

**What did you do, and put it back.** Jarvix keeps an account of every change
it makes to this machine in your name — a config entry saved, a fact
remembered, a phrase taught, a reminder set, a window moved, an artifact
written, a command run — and, where it can, what would restore it.

```bash
jarvix actions        # newest first, with which ones can be put back
jarvix undo           # put the last change back
jarvix undo a17       # put one particular change back
```

For anything Jarvix wrote to a file, "what would restore it" is the exact
bytes that were there before — comments, key order and spacing included, so an
undo is a reversal and not a second edit. For a window it is the workspace,
layer and geometry it was in. **For a shell command it is nothing, and Jarvix
says so rather than pretending**: a command that has run has run, and an offer
it could not keep would be worse than no offer. The same goes for a script, a
routine, a keystroke, a closed window and a refreshed feed — each is recorded,
described, and never promised as undoable.

Which decisions are one-way is said **on the confirmation card, before you
approve** — "This can't be undone: a command that has run has run" — because
learning it afterwards would be learning nothing useful.

An undo is a change like any other, so it faces the same permission tier as
the thing it reverses, and it **refuses rather than clobbering** when it
cannot tell: if you edited the file yourself after Jarvix wrote it, `jarvix
undo` says what it found and leaves your edit alone rather than overwriting
it. Putting something back is itself recorded, so the account is always the
whole story.

The account is bounded and says so — "I keep the last 100 actions; 37 older
ones have dropped off" — because an account that silently forgets is worse
than one that tells you what it no longer has. It lives at
`~/.local/state/jarvix/undo.toml`, private (0600) and hand-editable; deleting
a stanza removes that record, which is the right thing to do with a command
you would rather not keep a copy of. See
[ADR 0064](docs/adr/0064-review-and-undo.md).

The same account is the conversation window's **Account** tab, so reviewing
what was done in your name and reversing it are the same place rather than a
terminal. Each row says what changed, when, and where it stands — "I can put
this back", "I can't put this back — a command that has run has run", "I put
this back 4 minutes ago — that reversal is a19" — and a row that cannot go
back has no button to press rather than one that refuses. A job's steps sit
under one heading with a single control that puts the whole piece of work
back, newest step first; a job that is still working says so instead of
offering one, and a parked job's control says out loud that using it will also
stop the job. Every source an action touched clicks through to the thing
itself, the same way an answer's do. See
[ADR 0066](docs/adr/0066-the-account-in-the-window.md).

**What are you doing right now.** Jobs run while you are not watching, so
there is a place to watch them from that does not go through a model at all —
the conversation window's **Jobs** tab, and the same account at the terminal:

```bash
jarvix jobs                        # every job: state, goal, scope, what it waits for
jarvix jobs stop tidy              # stop one; it says what it had done and had not
jarvix jobs answer tidy yes        # approve what it parked on and let it carry on
jarvix jobs answer tidy the one in Documents   # ...or answer a question it asked
```

Each row says where the job stands and how long it has been there ("Tidy is
waiting on you — parked 4 minutes ago"), the goal in your own words, the
boundary it is held to, how much it has done — counting steps and changes
separately, and naming any step it started and never saw the end of. A job
that has finished shows the report read back from its ledger, unverified
steps first; nothing on this surface is composed by a model.

When a job parked because something could not be undone, approving it here
shows **the same verbatim detail the confirmation card would have shown**, and
approving runs that exact step rather than asking the planner again — you
approve the action you were shown. A job nothing can answer, because it walked
into its own boundary, has no Approve button at all and says why in its place.

Asking out loud costs nothing either: **"what are my jobs doing?"** answers
straight away rather than asking permission first. Stopping and answering
still ask, because those change work you asked for. See
[ADR 0067](docs/adr/0067-jobs-you-can-see-and-steer.md).

**A job can run commands, and the boundary is the kernel's rather than a
guess.** "Tidy my downloads" and "get the CI green" are commands, and until
recently a job parked the moment it proposed one — honestly, because a
command's files cannot be read out of its text and a boundary that cannot be
checked is not a boundary. So Jarvix stopped trying to read them. A job's
command is confined by **Landlock** to the scope's directories *before it
starts*: it physically cannot read or write anything outside them, whether it
names the path outright, reaches it through a symlink planted inside the
scope, walks up with `../..`, builds it in a variable, or changes directory
first. It inherits none of the daemon's environment, so none of your API keys
are reachable, and it cannot touch Jarvix's own configuration — a job still
cannot change what Jarvix is allowed to do.

On a machine whose kernel cannot hold that boundary, **a job refuses to run
the command and says so**, rather than running it unconfined. The permission
gate is unchanged: anything irreversible still stops and asks, and confinement
is an extra wall rather than a substitute for one. What the boundary does
*not* cover — the network, signals, and the fact that a command can still see
that a file outside exists without being able to read it — is written down
rather than implied. See
[ADR 0068](docs/adr/0068-a-command-the-kernel-holds.md).

### Backing up the assistant's memory of you

Jarvix's whole knowledge of you — remembered facts, taught vocabulary, focus
threads, conversations, routines, settings — lives under `~/.config/jarvix`
and `~/.local/state/jarvix`. One command archives both roots as a single
dated tar.gz with a manifest (Jarvix version, file list, SHA-256 hashes,
per-store schema versions):

```bash
jarvix backup                        # jarvix-backup-YYYYMMDD-HHMMSS.tar.gz here
jarvix backup ~/backups/jarvix/      # dated archive inside that directory
jarvix backup --no-secrets           # api keys in config.toml replaced with
                                     # placeholders; restore lists what to re-enter
```

Secrets are **included by default** — an api key written into
`config.toml` (the per-endpoint fallback to the environment variables) is
part of a working machine, and a backup that silently dropped it would not
restore one. Treat the archive like the private files it contains (it is
written `0600`).

With the daemon running, backup asks it to briefly hold store writes
(`state.hold`) so the copy is a coherent point in time; with the daemon
stopped it copies directly. Either way every file's hash is pinned in the
manifest, and the archive contains nothing outside Jarvix's own two
directories — no shell configs, no ssh keys, ever.

Restore validates before it touches anything, and refuses — specifically and
loudly — on a corrupt or truncated archive, a hash mismatch, an archive or
store format newer than the installed Jarvix, or a running daemon:

```bash
systemctl --user stop jarvixd
jarvix restore jarvix-backup-20260828-030000.tar.gz
jarvix doctor
systemctl --user start jarvixd
```

The archive is staged and swapped into place with renames (never
delete-then-copy), and any existing state first moves aside whole to a
timestamped safety copy (`~/.local/state/jarvix.pre-restore-<timestamp>`,
same for config) which the report names — your old state is never
destroyed. An archive made with `--no-secrets` restores fine; the report
lists each api key that needs re-entering.

For unattended backups, `--quiet` prints nothing on success and the exit
codes are stable: `0` success, `1` any failure, `2` unknown command. A
sensible cron line:

```cron
0 3 * * * jarvix backup --quiet ~/backups/jarvix/
```

See [ADR 0045](docs/adr/0045-backup-restore.md) for the consistency model.

## Troubleshooting

Start with `jarvix doctor` — it checks PipeWire, microphone, speakers,
whisper.cpp, the model, Piper, the voice, the daemon, the AI provider, the
Omarchy plugin, and that no other Hyprland binding (Omarchy default or
personal) shares a Jarvix key chord — and tells you how to fix whatever is
broken. It does not stop at "installed": the voice loop is probed for real —
whisper-cli transcribes a generated wav against your configured model, and
the configured TTS engine speaks a short phrase into a discarded sink (no
mic, no speakers, 30s budget per probe) — so an engine whose libraries broke
under an update fails the report with its own stderr instead of passing as
"installed" while every session dies. The bindings installer performs the same conflict check and refuses
to leave a clashing chord in place.

Daemon logs:

```bash
journalctl --user -u jarvixd -f
```

The overlay, the window, and the bar widget are all display-only. If any of
them doesn't appear: `omarchy plugin list` should show `jarvix` enabled;
`omarchy-shell shell rescanPlugins` re-discovers it; saving any file in
`~/.config/omarchy/plugins/jarvix/` hot-reloads it.

No icon in the bar, but `omarchy plugin list` says jarvix is enabled? An
installation that predates the widget is recorded as a plain plugin rather
than as a bar widget, and enabling it again will not move it. Re-run
`scripts/install-plugin.sh`, which migrates it, or do it by hand:

```bash
omarchy plugin disable jarvix && omarchy plugin enable jarvix right
```

`scripts/verify-bar-widget.sh` checks the whole thing on a live session —
manifest, placement, IPC, and whether the icon actually follows a session.

## Development

```bash
make build             # binaries into ./bin
make test              # unit + integration tests (no hardware or network needed)
make lint              # go vet (+ staticcheck when installed)
make ci                # exactly what the CI gate runs
make coverage-ratchet  # total coverage vs the floor in coverage.floor
make soak              # the high-count, constrained runs that catch ordering faults
make voice-corpus      # real recordings through the real whisper (needs the tag and a corpus)
make fuzz-properties   # the classifier and parser laws, attacked by the fuzzer
make mutate            # mutation testing, with a report of the survivors
```

The entire session lifecycle is testable with fakes — `internal/session`'s
tests run fake speech → fake transcript → fake AI stream → fake TTS through
the real engine and real IPC.

`make test` and the CI gate are fast on purpose, and there is a class of defect
they cannot see: ordering faults that need dozens of repetitions, or constrained
parallelism, to show up at all. Those are soaked nightly instead —
[docs/soak.md](docs/soak.md) has the exact commands to run one by hand, what the
coverage floor is (and is not), and the two guards that catch the ordering traps
this repo has already fallen into.

There is a second such class, and it is the reason `make fuzz-properties` and
`make mutate` are on the list above. Several of the pieces that decide whether
Jarvix may run a command, or when a reminder fires, are classifiers over an
unbounded input space, and a table of examples only ever proves the cases
somebody thought of. Those components are stated as **properties** — laws that
hold for every input — and attacked with generated input on a weekly schedule
rather than on the PR gate, so the fast gate stays fast.
[docs/mutation.md](docs/mutation.md) is the properties, the mutation report,
and what the first report cost to act on.

Every test above uses fakes, and there is one thing fakes cannot prove: that
whisper, biased the way this daemon biases it, turns *real speech* into the
transcripts those tests assume. That is what the voice corpus is for — the
user's own recordings run through the real engine and asserted on the intent
that matched and the word that survived, never on the transcript itself. It is
behind a build tag (recordings are personal data; whisper is heavy) and skips
gracefully while the corpus is empty. `jarvix doctor` prints where it stands.
See [docs/voice-corpus.md](docs/voice-corpus.md).

Architecture, protocol, and design decisions:

- [docs/architecture.md](docs/architecture.md) — components and session lifecycle
- [docs/ipc.md](docs/ipc.md) — the JSON-RPC protocol
- [docs/providers.md](docs/providers.md) — provider abstractions
- [docs/soak.md](docs/soak.md) — soaking, the coverage floor, and the test guards
- [docs/voice-corpus.md](docs/voice-corpus.md) — the real-voice test corpus and its harness
- [docs/mutation.md](docs/mutation.md) — the classifier properties and the mutation report
- [docs/adr/](docs/adr/) — architecture decision records
- [docs/CHECKLIST.md](docs/CHECKLIST.md) — development checklist

## License

MIT — see [LICENSE](LICENSE).
