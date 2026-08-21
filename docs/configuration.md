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

[ui]                             # hints for the overlay
show_transcript = true
show_response = true

[log]
level = "info"                   # debug | info | warn | error
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

- Every command is logged (`journalctl --user -u jarvixd`).
- Each command has a timeout (`shell_timeout_sec`) and an output cap.
- The default system prompt tells the model to prefer read-only commands and
  to ask before anything destructive — but a capable model with shell access
  can still do real things. Enable it deliberately.
- Commands run under the session context: cancelling a session (Escape /
  `jarvix cancel`) kills any command in flight.

Use a tool-capable model — `qwen2.5:7b` (local) or any OpenAI/Anthropic model
with tool support. Small models without tool training will not call tools.

A per-tool permission gate (allow / ask / deny, with spoken confirmation) is
the next step on the roadmap; today `shell` is a single on/off switch.

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
