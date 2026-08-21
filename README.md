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
to review my publish pipeline" — and speaks the answer back. The long-term
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
| Cancel / stop speech | `Super+Alt+Escape` (or `jarvix cancel`) |
| Interrupt mid-speech | Hold the chord again — it stops talking and listens |
| Type instead of speaking | **Type in the conversation window and press Enter** — same conversation, same tools, same spoken answer |
| Ask from a terminal | `jarvix ask "explain recursion in one sentence"` |
| Voice from a terminal | `jarvix listen` |
| Review the conversation | **Click the Jarvix icon in the bar**, click the notification when Jarvix answers, or `Super+Alt+C` / `jarvix window` |
| See what Jarvix is doing | The bar icon — hover it for the state in words |
| Actions without speaking | **Right-click the bar icon**: window, new conversation, settings, recent artifacts |
| Fresh conversation | `jarvix new` (forget the current thread) |
| Health check | `jarvix doctor` |
| Daemon state | `jarvix status` |

### The bar widget

Jarvix lives in the top-right of the Omarchy bar, next to the tray and the
network and audio widgets. The icon says what Jarvix is doing at a glance —
ready, listening, thinking, responding, speaking, waiting for a confirmation,
or stopped — with a different **shape** for each state, and the same thing in
words on hover, so it never depends on colour alone. A stopped daemon dims the
icon and offers the start command; it never disappears, because an icon that
is not there cannot be told apart from a plugin that was never installed.

- **Left click** toggles the conversation window (the same route `jarvix
  window` and a clicked notification take — there is only ever one window).
- **Right click** opens the panel: the conversation window, a new
  conversation, settings, and the recent artifacts, each one row you can also
  reach with the arrow keys and Enter.
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

Jarvix remembers the conversation: ask a follow-up ("what should I change?")
and it keeps the prior context, until the thread goes idle (configurable) or
you run `jarvix new`. Answers are spoken **as they stream** — Jarvix starts
talking on the first complete sentence rather than waiting for the whole
reply.

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
broken. The bindings installer performs the same conflict check and refuses
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
