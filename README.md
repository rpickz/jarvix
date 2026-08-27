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
| Ask from a terminal | `jarvix ask "explain recursion in one sentence"` |
| Voice from a terminal | `jarvix listen` |
| Review the conversation | **Click the Jarvix icon in the bar**, click the notification when Jarvix answers, or `Super+Alt+C` / `jarvix window` |
| See what Jarvix is doing | The bar icon — hover it for the state in words |
| Actions without speaking | **Right-click the bar icon**: window, new conversation, settings, recent artifacts |
| Set up your desktop in one sentence | **Say a routine's phrase** ("Jarvix, morning setup") — your apps launch onto your workspaces, arranged; `jarvix routines` lists them, `jarvix routines run "morning setup"` triggers one, and the conversation window has a Run button. Defined as `[[routines]]` in config.toml — see [docs/configuration.md](docs/configuration.md#routines-routines) |
| Save the desktop you already arranged | **"Jarvix, save this as my morning setup"** — the live window layout is read once and written as a `[[routines]]` entry (comment-preserving, provenance-stamped), immediately runnable and hand-editable; an existing name asks before it is replaced. See [docs/configuration.md](docs/configuration.md#capturing-a-routine-from-the-live-desktop-save-this-as-) |
| Run your own script by voice | **Say a script's phrase** ("Jarvix, backup my notes") — a `[[scripts]]` entry in config.toml names an executable and its trigger phrases; it runs behind the permission gate (asks first by default, naming the script and its path), with zero arguments, and speaks the outcome. `jarvix scripts` lists them, `jarvix scripts run "backup notes"` triggers one, the window has a Run button — see [docs/configuration.md](docs/configuration.md#scripts-scripts) |
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
words on hover, so it never depends on colour alone. With background listening
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

Conversations are durable ([ADR 0027](docs/adr/0027-durable-conversation-archive.md)):
`jarvix new` archives the thread instead of destroying it, whole — the
`history_turns` cap only limits what the model is sent, never what is kept.
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
make build   # binaries into ./bin
make test    # unit + integration tests (no hardware or network needed)
make lint    # go vet (+ staticcheck when installed)
```

The entire session lifecycle is testable with fakes — `internal/session`'s
tests run fake speech → fake transcript → fake AI stream → fake TTS through
the real engine and real IPC. Architecture, protocol, and design decisions:

- [docs/architecture.md](docs/architecture.md) — components and session lifecycle
- [docs/ipc.md](docs/ipc.md) — the JSON-RPC protocol
- [docs/providers.md](docs/providers.md) — provider abstractions
- [docs/adr/](docs/adr/) — architecture decision records
- [docs/CHECKLIST.md](docs/CHECKLIST.md) — development checklist

## License

MIT — see [LICENSE](LICENSE).
