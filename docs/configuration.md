# Jarvix configuration

File: `~/.config/jarvix/config.toml` (`$XDG_CONFIG_HOME/jarvix/config.toml`).
The file is optional — every setting has a default, chosen so a machine with
Ollama and Piper installed works with no configuration at all. Configuration
is validated at daemon startup; validation errors name the offending key and
say what to do.

Inspect the effective configuration (defaults + your file, secrets redacted):

```bash
jarvix config
```

The first-run wizard (`jarvix setup`) writes this file for you: it detects
the voice engine, activation access, AI provider, and installed advisor
CLIs, and records the choices. It edits only the keys it owns, preserves
your comments and layout, and asks before changing any value you set by
hand — safe to re-run at any time.

## Full reference

```toml
[activation]
mode = "push_to_talk"            # the only supported mode in V1
# Hold-to-talk chord, watched by the daemon via evdev (needs keyboard read
# access: jarvix setup input). Key names are evdev names: letters, digits,
# f1-f12, leftmeta/rightmeta, leftalt/rightalt, leftctrl/rightctrl,
# leftshift/rightshift, space, esc (aliases: super, alt, ctrl, shift).
# Empty list disables the watcher; keybindings then drive activation.
ptt_chord = ["leftmeta", "leftalt", "v"]

[ai]
provider = "ollama"              # endpoint name: a preset or your own [ai.<name>]
model = "llama3.2:3b"            # model name understood by that endpoint
system_prompt = "..."            # default: concise, speech-friendly answers
max_tokens = 1024
temperature = 0.7

# Presets exist for openai, openrouter, ollama, lmstudio. Override any field,
# or define an entirely new endpoint — no code changes needed:
[ai.openai]
base_url = "https://api.openai.com/v1"
api_key_env = "OPENAI_API_KEY"   # environment variable holding the key
# api_key = "sk-..."             # dev/testing fallback only; prefer the env var

[ai.myserver]                    # example: your own OpenAI-compatible server
base_url = "http://10.0.0.5:8080/v1"
api_key_env = "MYSERVER_KEY"

[stt]
provider = "whisper"             # the only supported STT in V1

[stt.whisper]
model = "base.en"                # known name, or absolute path to a ggml file
binary = "whisper-cli"           # found on PATH, or absolute path
language = "en"                  # "auto" to detect

[tts]
provider = "piper"               # "piper" (zero-setup default) or "kokoro" (more natural)

[tts.piper]
voice = "en_US-amy-medium"       # voice name searched under /usr/share/piper-voices,
                                 # or absolute path to a .onnx model
binary = "piper-tts"

[tts.kokoro]                     # requires scripts/setup-kokoro.sh first
voice = "af_heart"               # Kokoro voice id
speed = 1.0                      # speech rate multiplier

[tools]                          # the assistant can act on your computer
shell = false                    # enable shell.run — see the Tools section below
shell_timeout_sec = 30           # per-command timeout
shell_max_output_kb = 16         # captured output cap fed back to the model
artifacts = true                 # enable artifact.create (diagrams, documents,
                                 # spreadsheets, sketches on screen)

[artifacts]                      # where the assistant's files land and open
dir = "~/Documents/Jarvix"       # default is your real home path; must be
                                 # absolute in the file ("~" is not expanded)
open_command = "xdg-open"        # how an artifact is shown (all formats)
render_timeout_sec = 10          # renderer killed past this

[artifacts.open_commands]        # optional per-format viewer overrides;
                                 # formats without an entry use open_command
# document = "obsidian"          # .md drafts in your editor of choice
# spreadsheet = "libreoffice"    # .csv tables in a spreadsheet app
# excalidraw = "none"            # "" or "none" = no viewer: the file is
                                 # saved and announced by name, not opened

[tools.policy]                   # the permission gate (see the Tools section)
default = "ask"                  # decision for tools with no [tools.policy.tool] entry
confirm_timeout_sec = 30         # unanswered confirmations decline after this
remember_for_conversation = false # re-run an approved command without asking again
                                 # for the rest of this conversation only
shell_allow = []                 # extra command prefixes that run silently,
                                 # e.g. ["docker compose ps", "kubectl get"]
shell_deny = []                  # extra command prefixes that never run,
                                 # e.g. ["git push"] — deny beats everything

[tools.policy.tool]              # per-tool decisions: allow | ask | deny
# "shell.run" = "ask"            # ask (default): classify commands; allow:
                                 # trust everything except deny patterns;
                                 # deny: disable the tool entirely
# "artifact.create" = "allow"    # built-in default: it only writes into the
                                 # artifact directory. Unknown tools default
                                 # to "ask".

[conversation]
speak_responses = true           # false = text-only sessions
history_turns = 16               # remember this many prior exchanges as context
                                 # (0 = every question is standalone)
follow_up_window_sec = 900       # forget the thread after this idle gap
                                 # (0 = keep until restart or `jarvix new`)

[audio]
input_device = ""                # PipeWire target name/serial; empty = default
output_device = ""
max_recording_sec = 60           # safety cap on capture length
min_recording_ms = 300           # discard shorter captures as accidental taps
                                 # (no transcription, no error; session ends quietly)

[ui]                             # desktop surfaces: overlay hints + notifications
show_transcript = true
show_response = true
notifications = true             # desktop notification when a session finishes;
                                 # clicking it opens the conversation window.
                                 # false = no notifications (the window stays
                                 # reachable via `jarvix window`)
notification_preview = true      # show the start of the answer in the
                                 # notification body; false = a generic
                                 # "Jarvix answered" with no content

[log]
level = "info"                   # debug | info | warn | error

# Assistant CLIs Jarvix can delegate heavyweight questions to. `jarvix setup`
# detects installed CLIs (claude, codex, gemini, aider, goose, opencode) and
# records them here; delegation itself ships separately (issue #3) and will
# consume these tables — recording them now costs nothing.
[advisors.claude]
binary = "/usr/bin/claude"       # absolute path found on PATH at setup time
```

## Secrets

API keys are **never** stored in `config.toml` in normal use — each endpoint
names an environment variable (`api_key_env`) and the daemon reads it at
request time. The inline `api_key` field exists as an explicit
developer/testing fallback, and `jarvix config` redacts it.

The daemon runs under systemd, which does not inherit your shell exports:

```bash
systemctl --user set-environment OPENAI_API_KEY=sk-...
systemctl --user restart jarvixd
```

Keyring/Secret Service integration is planned (see the implementation plan).
Keys are never logged; diagnostics redact credentials.

## Whisper models

`jarvix setup whisper [name]` downloads into
`~/.local/share/jarvix/models/whisper/`. Known names: `tiny`, `tiny.en`,
`base`, `base.en` (default), `small`, `small.en`, `medium`, `large-v3`,
`large-v3-turbo`. Bigger models are more accurate and slower; `base.en` is a
good push-to-talk default on any recent CPU.

## Piper voices

Voice names resolve by searching `/usr/share/piper-voices` (the Arch
`piper-voices-*` packages install there). Any voice from the
[Piper voices collection](https://huggingface.co/rhasspy/piper-voices) works:
download the `.onnx` + `.onnx.json` pair and point `tts.piper.voice` at the
`.onnx` path.

## Tools (assistant actions)

With `[tools] shell = true`, the assistant can run shell commands itself to
answer questions about your system and carry out tasks — "what's happening in
docker?" makes it run `docker ps` and summarise the result, rather than
telling you the command. This is **opt-in** because it gives the assistant
the same authority as your own shell:

- Every command is logged (`journalctl --user -u jarvixd`), and every
  ask/deny decision is logged and published as IPC events.
- Each command has a timeout (`shell_timeout_sec`) and an output cap.
- Commands run under the session context: cancelling a session (Escape /
  `jarvix cancel`) kills any command in flight.

Use a tool-capable model — `qwen2.5:7b` (local) or any OpenAI/Anthropic model
with tool support. Small models without tool training will not call tools.

### The permission gate (`[tools.policy]`)

Every tool call is classified **allow / ask / deny** before it executes
([ADR 0014](adr/0014-tool-permission-gate.md)):

- **allow** — a shipped read-only allow list (`docker ps`, `df -h`,
  `git status`, `journalctl`, pipelines of those, …) runs silently, exactly
  as before the gate existed. `shell_allow` extends it with your own
  command prefixes.
- **ask** — everything else: Jarvix speaks a one-sentence summary generated
  from the command itself ("I want to run 'rm -rf ./build'…"), the overlay
  shows the exact command, and nothing runs until you say **yes / go ahead**
  (push-to-talk while it waits), type an answer, or run `jarvix confirm`
  (`jarvix deny` declines). No answer within `confirm_timeout_sec` declines.
  Risky command words (`rm`, `dd`, `mkfs`, `sudo`, output redirection `>`)
  always ask, even inside an otherwise-allowed pipeline — a compound command
  is judged by its riskiest part.
- **deny** — catastrophic patterns (`rm -rf /`, `dd` onto a device, fork
  bombs) and anything in `shell_deny` never run, with or without
  confirmation. Deny beats allow, always.

Classification happens in the daemon on the parsed command; the model's own
description of what it is doing is never trusted, so a model cannot describe
`rm -rf ~` as "tidying up". Unknown tools default to `ask`.

`remember_for_conversation = true` re-runs a command you already approved
without asking again — scoped strictly to the current conversation:
`jarvix new`, the follow-up window, and daemon restarts all forget the
approvals.

`jarvix status` prints the effective policy.

## Artifacts (work you keep)

Ask for something better seen than heard and the assistant calls
`artifact.create` with the right format instead of reading structure aloud:

| You say | Format | File | Opens via |
|---|---|---|---|
| "diagram my publish pipeline" | `mermaid` | `.mmd` + rendered `.svg` | `open_command` |
| "draft a one-page brief" | `document` | `.md`, saved verbatim | `open_commands.document` |
| "put those numbers in a spreadsheet" | `spreadsheet` | `.csv`, validated before write | `open_commands.spreadsheet` |
| "sketch this out on a canvas" | `excalidraw` | `.excalidraw` scene JSON, validated | `open_commands.excalidraw` |

Every format rides the same seam: files land under `[artifacts] dir`
(default `~/Documents/Jarvix`, created private, 0700) as `<date>-<slug>.<ext>`,
the daemon publishes an `artifact.created` IPC event (type, path) for the
overlay/notifications, and `jarvix artifacts` lists the most recent ones
with type and age. The spoken answer stays a short summary; file paths are
never read aloud. Structured formats (CSV, scene JSON) are validated before
anything is written — broken quoting or malformed scene JSON goes back to
the model with the specific error for one retry, and an invalid file is
never saved. Artifact source is capped at 1 MB; oversized content is
refused, never truncated.

Per-format viewers come from `[artifacts.open_commands]` (falling back to
`open_command`, default `xdg-open`). Setting a format's entry to `""` or
`"none"` means "no viewer": the assistant saves the file and tells you its
name instead of opening anything — useful for `.excalidraw`, which usually
wants dragging into [excalidraw.com](https://excalidraw.com) rather than a
local handler.

Only diagrams need an external renderer,
[mermaid-cli](https://github.com/mermaid-js/mermaid-cli):

```bash
npm install -g @mermaid-js/mermaid-cli   # or from the AUR: mermaid-cli
```

Without it the assistant simply answers in prose, and `jarvix doctor` names
the missing piece. Renders run as a local subprocess (no network), bounded by
`render_timeout_sec`. Documents, spreadsheets, and sketches have no external
dependency at all. See ADR 0012 for the design.

## Notifications and the conversation window

When a session finishes, the daemon sends a desktop notification
(`org.freedesktop.Notifications`, via `notify-send`): the first ~80
characters of the answer on success, or the failing stage and message on
error. Clicking the notification opens the **conversation window** — the
full current exchange, streaming live — which is also reachable any time
with `jarvix window` (bound to `Super+Alt+C` by the Hyprland bindings).

- `ui.notifications = false` turns notifications off entirely.
- `ui.notification_preview = false` keeps answer content out of
  notifications: successes say just "Jarvix answered", failures name only
  the failing stage. Use this when the notification daemon logs or mirrors
  notification bodies somewhere you don't want answers to land.
- No notification daemon running? Delivery degrades to a debug log line;
  sessions are unaffected. Answer content is never written to the journal.

The window is rendered by the Omarchy shell plugin and works without the
daemon: if jarvixd is down it says so and points at
`systemctl --user start jarvixd`.

## Natural voice (Kokoro)

Piper (the default) needs no setup but sounds robotic. Kokoro is markedly
more natural:

```bash
scripts/setup-kokoro.sh          # Python venv + kokoro-onnx + models (~340 MB)
# then set tts.provider = "kokoro" and: systemctl --user restart jarvixd
```

Assistant answers are normalised before they are spoken — markdown, code
fences, and list bullets are stripped so nothing reads "asterisk" or
"backtick" aloud — while the overlay still shows the formatted text.

## XDG paths

| Purpose | Path |
|---|---|
| Config | `~/.config/jarvix/config.toml` |
| Models | `~/.local/share/jarvix/models/` |
| State | `~/.local/state/jarvix/` |
| Socket | `$XDG_RUNTIME_DIR/jarvix.sock` |
| Recordings (transient) | `$XDG_RUNTIME_DIR/jarvix/` (tmpfs, deleted after use) |
| Artifacts (diagrams, documents, spreadsheets, sketches) | `~/Documents/Jarvix/` (configurable: `[artifacts] dir`) |
