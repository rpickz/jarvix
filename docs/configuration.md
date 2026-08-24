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

## Changing settings without a restart

The common options are editable from the **settings screen** in the Jarvix
window (`jarvix window` → Settings, or `Super+Alt+C`) and from the CLI —
both are thin clients of the same daemon IPC methods (`config.get` /
`config.set` / `config.reload`, docs/ipc.md):

```bash
jarvix config get                     # every editable setting, with its reload class
jarvix config get tts.provider       # one value
jarvix config set tts.provider=kokoro ai.model=qwen2.5:7b
jarvix config set activation.ptt_chord=leftmeta,space
jarvix config reload                  # re-read a hand-edited config.toml, no restart
```

A change is validated first — invalid values are rejected with the same
messages startup validation produces, and **nothing is written**. Valid
changes are written into `config.toml` with a surgical rewrite: only the
changed key's value is touched, so your comments, custom `[ai.<name>]`
tables, and formatting survive every save. Writes are atomic
(temp file + rename, mode 0600). If the file was edited externally between
reading and saving, the save is refused and the screen/CLI tells you to
re-read and reapply — a hand edit is never silently clobbered.

### When changes take effect (reload classes)

Every setting has a **reload class**, shown by `jarvix config get` and next
to each field in the settings screen:

| Class | Options | When it takes effect |
|---|---|---|
| **live** | `ui.*` (notifications, notification_preview, show_transcript, show_response, activity_rows, activity_clear_on_new) | Immediately on save, even mid-session |
| **idle** | `assistant.aliases`, `ai.*` (provider, model, system_prompt, max_tokens, temperature), `tts.*` (including the `[tts.lexicon]` pronunciation table), `stt.whisper.*`, `conversation.*`, `context.*`, `audio.*`, `performance.*`, `intents.enabled`, `intents.terminal` | On save, when no session is in flight — the daemon swaps its adapters between sessions, never underneath one. Saved mid-session, the file is written and the change applies on the next `jarvix config reload` (or restart) |
| **restart** | `assistant.name`, `activation.*` (mode, ptt_chord, and the wake-word settings), `tools.*`, `artifacts.*`, `log.level` | Written to the file, but the chord watcher, the wake listener, the tool registry, the artifact tool, and the logger are wired at daemon boot: `systemctl --user restart jarvixd` finishes the job (the screen/CLI says so explicitly). The live control for background listening is `jarvix mute`, not a setting |

A reload that fails validation keeps the running configuration and reports
why — the daemon never hot-swaps into a broken state. `[[intents.custom]]`
entries are structured tables rather than single values, so like
`[ai.<name>]` they stay hand-edited; they are picked up by the next
idle-class reload or a restart. Secrets never pass
through the settings surface: API keys are shown as presence only
("OPENAI_API_KEY: set") and cannot be entered there; manage them via the
environment as described below. Endpoint tables (`[ai.<name>]`) also stay
hand-edited — the file remains authoritative for everything.

The settings screen shows each option's external readiness inline (Kokoro
not set up, Whisper model missing, input access not granted), reusing
`jarvix doctor`'s checks with the fix command attached.

## Full reference

```toml
[assistant]                      # the assistant's identity (see "Naming your
                                 # assistant" below)
name = "Jarvix"                  # what it answers to and calls itself. One
                                 # source of truth: speech recognition biases
                                 # toward it, wake matching and stripping
                                 # accept it, and the default prompt
                                 # self-refers by it. May be multi-word
                                 # ("Mister Smith").
aliases = ["jarvis", "javax", "jarvic", "jarvicks", "jarvex"]
                                 # spellings transcripts mishear the name as;
                                 # the wake-transcript strip accepts any of
                                 # them as the summons, leading whole words
                                 # only. Unset, the shipped list above covers
                                 # the default name — a custom name starts
                                 # with none (jarvix doctor will nudge).
                                 # Transcript-side only: the acoustic wake
                                 # gate never sees them.

[activation]
mode = "push_to_talk"            # "push_to_talk" (default) or "wake_word".
                                 # "wake_word" *adds* background listening;
                                 # the chord keeps working either way.
# Hold-to-talk chord, watched by the daemon via evdev (needs keyboard read
# access: jarvix setup input). Key names are evdev names: letters, digits,
# f1-f12, leftmeta/rightmeta, leftalt/rightalt, leftctrl/rightctrl,
# leftshift/rightshift, space, esc (aliases: super, alt, ctrl, shift).
# Empty list disables the watcher; keybindings then drive activation.
ptt_chord = ["leftmeta", "leftalt", "v"]

# Background listening (only used when mode = "wake_word").
# See "Background listening" below before turning it on.
# wake_word = "hey_jarvis"       # detector override: a bundled model word or
                                 # a path to a model you trained yourself.
                                 # Unset (the default), the detector is
                                 # handed the assistant's name, lowercased.
                                 # Detector-only: the bias prompt, the strip,
                                 # and the prompt follow [assistant].
wake_command = ["jarvix-wake"]   # the detector helper, run directly (not
                                 # through a shell): one argument per entry.
                                 # Installed by scripts/setup-wake.sh.
wake_sensitivity = 0.5           # 0..1, higher is more eager. Maps onto the
                                 # model's score threshold (0.5 → 0.5).
endpoint_silence_ms = 800        # silence that submits a hands-free request
wake_ring_ms = 1200              # audio kept from *before* the wake word, so
                                 # the first syllables are not lost. Maximum
                                 # 3000; this is the only ambient audio that
                                 # can ever reach a transcript, so keep it
                                 # short. 0 keeps none.
max_utterance_sec = 15           # longest hands-free request
# wake_aliases moved to [assistant] aliases; a config still setting it here
# is refused at load with directions, so a tuned list can never be silently
# dropped.

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
vocabulary = []                  # extra terms speech recognition is biased
                                 # toward — project names, jargon, anything
                                 # whisper keeps rounding to a nearby real
                                 # word. They join the wake word in the bias
                                 # prompt both transcription paths carry.
                                 # Input-side only; tts.lexicon fixes what
                                 # Jarvix *says*, this fixes what it *hears*.

[stt.whisper]
model = "base.en"                # known name, or absolute path to a ggml file
                                 # ".en" models transcribe English only — a
                                 # non-English voice needs a multilingual one
binary = "whisper-cli"           # found on PATH, or absolute path
language = "en"                  # "auto" to detect; must match the voice's
                                 # language (see "Language and accent")

[tts]
provider = "piper"               # "piper" (zero-setup default) or "kokoro" (more natural)

[tts.piper]
voice = "en_US-amy-medium"       # voice name searched under /usr/share/piper-voices,
                                 # or absolute path to a .onnx model; the
                                 # locale prefix (en_GB-…) sets the language
binary = "piper-tts"

[tts.kokoro]                     # requires scripts/setup-kokoro.sh first
voice = "af_heart"               # Kokoro voice id; its first letter is the
                                 # language (a=en-us, b=en-gb, …) — see
                                 # `jarvix voices`
speed = 1.0                      # speech rate multiplier

[tts.lexicon]                    # how words are pronounced: term = spoken form
# Kubernetes = "koo ber net eez" # merged over the shipped defaults, so an
# k9s = "kay nine ess"           # entry adds a word or overrides a default.
                                 # Matched case-insensitively on word
                                 # boundaries; spoken output only.

[tools]                          # the assistant can act on your computer
shell = false                    # enable shell.run — see the Tools section below
shell_timeout_sec = 30           # per-command timeout
shell_max_output_kb = 16         # captured output cap fed back to the model
artifacts = true                 # enable artifact.create (diagrams, documents,
                                 # spreadsheets, sketches on screen)
desktop = true                   # enable the desktop.* window tools: list,
                                 # focus, move, close, launch
desktop_apps = []                # what desktop.launch_app may start; empty
                                 # means anything on PATH, e.g.
                                 # ["firefox", "alacritty", "/opt/apps/notes"]

[tools.typing]                   # Jarvix typing on YOUR keyboard — read the
                                 # Typing section below before enabling this
enable = false                   # off by default, like shell.run
max_chars = 500                  # longest text one call may type (max 50000)
rate_limit = 6                   # typing actions allowed per window below
rate_window_sec = 60             # the rate-limit window (max 3600)
terminal_classes = []            # window classes whose contents are a command
                                 # line; typing into one always asks first.
                                 # Empty uses the shipped list (alacritty,
                                 # kitty, foot, ghostty, wezterm, konsole, …);
                                 # set it to name your own instead
# binary = "wtype"               # the keystroke injector; one program, no
                                 # arguments — it is executed directly

[artifacts]                      # where the assistant's files land and open
dir = "/home/you/Documents/Jarvix"
                                 # must be an absolute path — "~" is NOT
                                 # expanded; write your real home directory.
                                 # The default is already <your home>/Documents/Jarvix,
                                 # so leave this out unless you want it elsewhere.
open_command = "xdg-open"        # how an artifact is shown (all formats).
                                 # A string is split on whitespace; use an
                                 # array when a path or argument contains a
                                 # space: ["/opt/my viewer/bin/view", "--new"]
render_timeout_sec = 10          # renderer killed past this
diagram_format = "png"           # what a diagram renders to: "png" (default,
                                 # 2x scale — opens with its text in any image
                                 # viewer) or "svg" (markup for editing or
                                 # embedding; see the Artifacts section for
                                 # the caveat before choosing it)

[artifacts.open_commands]        # optional per-format viewer overrides;
                                 # formats without an entry use open_command
# document = "obsidian"          # .md drafts in your editor of choice
# spreadsheet = ["flatpak", "run", "org.libreoffice.LibreOffice"]
                                 # array form: argv, no shell, spaces safe
# excalidraw = "none"            # "", [] or "none" = no viewer: the file is
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
# "desktop.list_windows" = "allow"   # built-in defaults: the two window reads
# "desktop.focus_window" = "allow"   # run silently; move, close and launch
                                     # take the default above ("ask")
# "conversations.search" = "allow"   # built-in default: a read of your own
                                     # archive (ADR 0028), like memory.recall
# "typing.type_text" = "allow"   # the typing tools ALWAYS ask unless you name
# "typing.press_key" = "allow"   # them here — a global default of "allow"
                                 # deliberately does not reach them. A global
                                 # "deny" still does.

[intents]                        # the deterministic intent router (Phase 3)
enabled = true                   # false = every utterance goes to the AI
terminal = "alacritty"           # what "open terminal" launches; a single
                                 # executable name or absolute path (it is run
                                 # directly, never through a shell)

[[intents.custom]]               # your own phrases; repeat the block per intent
match = "lock the screen"        # literal phrase, matched whole and exactly
run = "hyprlock"                 # shell command, subject to [tools.policy]
say = "Locking."                 # spoken acknowledgement (default: "Done.")

[[routines]]                     # named app-placement sequences; see "Routines"
name = "morning setup"           # what summaries open with; unique
phrases = ["morning setup"]      # literal trigger phrases (intent grammar)
schedule = "08:30 mon-fri"       # optional clock trigger; see "Schedules"
announce = false                 # speak a scheduled run's summary (default false)
[[routines.steps]]               # repeat per step, run in order
app = "firefox"                  # program to launch if not already running
workspace = 2                    # where its window goes (1-99)
tile = "master"                  # optional: "master" or "split"; or float =
                                 # true with size = [w, h], position = [x, y]

[[scripts]]                      # your executables behind phrases; see "Scripts"
name = "backup notes"            # what the confirmation names; unique
phrases = ["backup my notes"]    # literal trigger phrases (intent grammar)
path = "/home/you/bin/backup-notes.sh"  # absolute, executable; run with NO arguments
timeout_sec = 60                 # optional; the run's whole process group is
                                 # killed at expiry (default 60)
report = "summary"               # optional: "summary" | "stdout" | "silent";
                                 # failures are spoken in every mode
schedule = "02:00"               # optional clock trigger; see "Schedules"
announce = false                 # speak a scheduled run's report (default false)

[context]                        # what Jarvix may look at before it answers
window = true                    # the focused window's app and title
selection = true                 # text you have highlighted (primary selection)
clipboard = false                # what you last copied — opt-in, see below
max_chars = 2000                 # per source; longer content is truncated
                                 # with a marker
timeout_ms = 300                 # gathering budget, per source and in total
                                 # (sources run in parallel). May be lowered,
                                 # never raised above 300

[memory]                         # facts you ask Jarvix to remember (see below)
enabled = true                   # off: tools not registered, nothing injected —
                                 # the store file is left alone (restart-class)
max_facts = 200                  # store cap; warns from nine-tenths full
max_injected_tokens = 500        # per-turn budget for the remembered-facts
                                 # block (~4 chars/token); facts that do not
                                 # fit are left out of the turn, never deleted.
                                 # Minimum 100

[conversation]
speak_responses = true           # false = text-only sessions
history_turns = 16               # remember this many prior exchanges as context
                                 # (0 = every question is standalone)
follow_up_window_sec = 900       # forget the thread after this idle gap
                                 # (0 = keep until restart or `jarvix new`)
retention = "on"                 # archive every conversation until you delete it
                                 # (ADR 0027; `jarvix conversations`). The cap
                                 # above limits what the model is sent, never
                                 # what is archived. "off" stops all archive
                                 # writing; it removes nothing already kept

[audio]
input_device = ""                # PipeWire target name/serial; empty = default
output_device = ""
max_recording_sec = 60           # safety cap on capture length
min_recording_ms = 300           # discard shorter captures as accidental taps
                                 # (no transcription, no error; session ends quietly)

[performance]                    # how much of the engine stack stays loaded
warm_engines = true              # keep supervised STT/TTS workers alive between
                                 # sessions; false restores the pre-ADR-0017
                                 # behaviour exactly (a cold start per session)
warm_memory_cap_mb = 2048        # retire a worker whose resident set passes this
                                 # (a leak detector, not a working budget; 0 = off)
warm_idle_reap_sec = 600         # free the workers after this long without an
                                 # interaction; 0 = keep them until jarvixd exits

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
activity_rows = 400              # rows of recent activity the daemon keeps in
                                 # memory for the window's Activity pane and
                                 # `activity.get` (issue #70). In-memory only —
                                 # never written to disk; max 10000
activity_clear_on_new = false    # true = `jarvix new` also empties the
                                 # activity feed. Off by default: "what did it
                                 # just do?" is usually asked after starting
                                 # fresh

[log]
level = "info"                   # debug | info | warn | error

# Assistant CLIs Jarvix delegates heavyweight questions to. `jarvix setup`
# detects installed CLIs (aider, claude, codex, gemini, goose, opencode) and
# records them here. A table naming nothing but a binary is complete: the
# shipped preset supplies the rest.
[advisors.claude]
binary = "/usr/bin/claude"       # absolute path found on PATH at setup time
# args = ["-p"]                  # argv template; preset per known CLI.
                                 # One element may be "{question}" (passed as
                                 # a single argument); with none, the question
                                 # goes to the CLI's stdin. Never shell-parsed.
# timeout_sec = 120              # the process group is killed past this
# description = "..."            # what the model is told this advisor is for
```

## Latency and warm engines (`[performance]`)

Jarvix's premise is that the computer feels present, so the time between
letting go of push-to-talk and hearing the first word is a budget, not an
accident. It is measured on **every** session and published as the
`session.timings` event; `jarvix status --last` prints the breakdown:

```
$ jarvix status --last
state:    idle
warm:     whisper  warm, 168 MB, up 412s
warm:     kokoro   warm, 640 MB, up 388s
last:     session s7
          release → transcript                 31 ms
          transcript → first token (model)    240 ms
          first token → first audio sample    256 ms
          first sample → audio out              2 ms
          release → first audio (total)       529 ms
            of which Jarvix (excl. model)     289 ms
```

The model's thinking time is reported separately and excluded from the
Jarvix line: which model you point at is your choice, and mixing it in would
hide what Jarvix itself costs.

**What warm mode does.** With `warm_engines = true` (the default) the daemon
keeps one supervised child per engine — `whisper-server` holding the ggml
model, and the TTS helper holding its voice — instead of starting a fresh
process and reloading the model for every question. On the development
machine that is the difference between ~900 ms and ~290 ms to first audio
with Kokoro, and ~374 ms versus ~81 ms with Piper (ADR 0018 has the full
table and the method).

**What it costs.** Memory, while the machine is idle: whisper base.en is
~165 MB resident and Kokoro's helper several hundred more. Three things
bound it:

- `warm_idle_reap_sec` frees the workers after ten minutes of quiet. The
  next question pays one cold start — the numbers above, cold column.
- `warm_memory_cap_mb` retires a worker that grows past the cap, so a leaking
  engine costs one cold start rather than your session. It is deliberately
  well above what a healthy engine uses; setting it below 256 MB is rejected,
  because it would retire the worker the moment it loaded its model.
- `warm_engines = false` turns the whole thing off and restores the previous
  behaviour exactly. That is the setting for a low-RAM machine.

**Failure is never yours to handle.** If an engine is not installed, is still
restarting after a crash, or the helper script predates the protocol, the
session silently falls back to the per-operation path and answers normally.
You get one warning in the journal and one slower answer, not an error.

`jarvix doctor` reports each worker, its memory, and whether it has been
restarting:

```
[OK]   warm engines — whisper warm (168 MB), kokoro warm (640 MB); 808 MB resident, cap 2048 MB, reaped after 600s idle
```

Warm workers are children of `jarvixd` in the literal sense: they are killed
when it exits and when a config reload rebuilds the adapters, so stopping the
daemon always leaves nothing behind.

> **Kokoro users:** the warm path needs the current helper script. Re-run
> `scripts/setup-kokoro.sh` after upgrading; an older `kokoro_stream.py` is
> detected at start-up and degrades to the per-utterance path.

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

Advisor CLIs (below) run with a **scrubbed environment**: every variable
named like a credential (`*_API_KEY`, `*_TOKEN`, `*SECRET*`, `*PASSWORD*`, …)
and every `api_key_env` Jarvix itself reads are withheld from the child. An
advisor authenticates itself — that is the premise of delegating to it — and
Jarvix's keys are not its to spend.

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

Piper voice names begin with their locale — `en_GB-alba-medium`,
`fr_FR-siwis-medium` — and that prefix is what Jarvix reads to know which
language the voice speaks. A British Piper voice is one package away:

```bash
sudo pacman -S piper-voices-en-gb            # AUR
jarvix voices                                # confirm it is there
jarvix config set tts.piper.voice=en_GB-alba-medium
```

## Language and accent

Jarvix ships speaking American English and can speak eight other languages,
plus British English, out of files that are already on disk once
`scripts/setup-kokoro.sh` has run. `jarvix voices` lists them grouped by
language with each voice's gender, marks the one in use, and prints the
command to change it. `jarvix setup` walks the same choice interactively —
language first, then accent, with a spoken preview before committing.

```bash
jarvix voices                                # what this machine can say
jarvix config set tts.kokoro.voice=bf_emma   # British female, applies to the
                                             # next answer — no restart
```

**The language is derived from the voice, never configured beside it.** A
Kokoro voice id begins with its language family and its gender:

| Prefix | Language | Phonemiser | Whisper | Example voices |
|---|---|---|---|---|
| `a` | English (American) | `en-us` | `en` | `af_heart`, `am_adam` |
| `b` | English (British) | `en-gb` | `en` | `bf_emma`, `bm_george` |
| `e` | Spanish | `es` | `es` | `ef_dora`, `em_alex` |
| `f` | French | `fr-fr` | `fr` | `ff_siwis` |
| `h` | Hindi | `hi` | `hi` | `hf_alpha`, `hm_omega` |
| `i` | Italian | `it` | `it` | `if_sara`, `im_nicola` |
| `j` | Japanese | `ja` | `ja` | `jf_alpha`, `jm_kumo` |
| `p` | Portuguese (Brazilian) | `pt-br` | `pt` | `pf_dora`, `pm_alex` |
| `z` | Chinese (Mandarin) | `zh` | `zh` | `zf_xiaoni`, `zm_yunxi` |

The second letter is the gender (`f`/`m`). The **phonemiser** column is what
drives letter-to-sound conversion; it used to be hardcoded to `en-us`, which
is why a British voice previously sounded British-*ish* while pronouncing
words American-style. `jarvix doctor` now reports the phonemiser in use, so
the accent can be confirmed without listening for it.

A voice id that is not in the installed voices file is rejected when the
configuration is validated, with installed alternatives named — rather than
being accepted and failing in the middle of an answer.

### Non-English languages need a multilingual speech model

whisper.cpp's `.en` models (`base.en`, `tiny.en`, `small.en`) transcribe
English and nothing else, so selecting a French voice while leaving
`stt.whisper.model = "base.en"` would leave Jarvix speaking French and
listening in English — which looks like a broken assistant, not a
misconfiguration. Jarvix refuses that combination. Change both together:

```bash
jarvix setup whisper base                    # multilingual, ~148 MB
jarvix config set tts.kokoro.voice=ff_siwis \
                 stt.whisper.model=base \
                 stt.whisper.language=fr     # or "auto" to detect
```

Both English accents share the `en` whisper code — whisper.cpp has no notion
of accent — so switching between American and British voices needs no speech
model change at all.

> **Upgrading:** `--lang` is passed to the Kokoro helper on the command line,
> so an installed `~/.local/share/jarvix/kokoro_stream.py` from before this
> change will reject it. Re-run `scripts/setup-kokoro.sh` after upgrading;
> `jarvix doctor` checks the installed helper and says so if it is stale.

## Naming your assistant (`[assistant]`)

Naming is the most personal knob an assistant has, so it is one setting —
editable in the window's Settings tab and via `jarvix config set
assistant.name=…` — and the whole pipeline follows it:

- **Speech recognition biases toward it.** Both transcription paths carry the
  sentence `The assistant is called <Name>.` (the sentence form matters — see
  "Hearing its own name" below).
- **Wake matching and stripping accept it**, and its `aliases`, as the
  summons at the front of a hands-free transcript.
- **It refers to itself by it.** The default system prompt opens
  "You are <Name>, …". A hand-written `ai.system_prompt` is sent verbatim —
  your words are yours.

```toml
[assistant]
name = "Hal"
aliases = ["howl", "hull"]   # what transcripts actually wrote when you said "Hal"
```

Two things deliberately do **not** follow the name:

- **Product branding.** The window title, the bar tooltip, the docs, and the
  binary/socket/service names stay Jarvix — they name the software on disk,
  not the persona you talk to.
- **The acoustic wake model.** openWakeWord only detects words it has a model
  for, so background listening keeps using the nearest model (`hey_jarvis` by
  default). Set `activation.wake_word` to a bundled word or your own `.onnx`
  to change what the *detector* listens for; everything else follows
  `assistant.name` regardless.

Give a custom name aliases. Whisper only writes words it knows, so a novel
name arrives as a nearby real word — the default name shipped with five
aliases because that is what transcripts actually produced — and the strip
only accepts spellings it is told about. Say the name, read what the
transcript wrote (`jarvix doctor` warns and shows the shape while a custom
name has none), and accept those spellings. Validation refuses a blank or
duplicate alias, and an alias equal to the name itself: the name is always
accepted, in any case, so listing it again adds nothing.

## Background listening (`activation.mode = "wake_word"`)

Say "Jarvix, what's my disk usage?" and it answers — no keyboard, no chord.
The rest of the sentence after the wake word is the request, and the silence
after it submits ([ADR 0024](adr/0024-background-wake-word-listening.md)).
Push-to-talk keeps working; the two coexist.

It is **off by default**, and turning it on means leaving a microphone open.
What that does and does not mean is set out below, because it should be a
decision rather than a default.

### Installing it

```bash
scripts/setup-wake.sh                        # openWakeWord in its own venv
jarvix config set activation.mode=wake_word
systemctl --user restart jarvixd             # restart class: see the table above
jarvix doctor                                # confirms the detector and the capture pid
```

The detector is a separate process (`jarvix-wake`) that scores 80 ms frames
of audio and answers with a number. Nothing else about it is special: point
`activation.wake_command` at anything that speaks the same protocol
(`wake/wake_detect.py` is the reference implementation).

> **"hey Jarvis", not "Jarvix".** openWakeWord ships no model for Jarvix's own
> name, so the installer uses the nearest pretrained one, `hey_jarvis`.
> `jarvix status` reports the model that is actually loaded. It responds to
> "hey Jarvis" far more reliably than to "Jarvix"; train your own model if you
> want the real word, and point `activation.wake_word` at the `.onnx` file.

### What happens to your audio

| | |
|---|---|
| Where detection runs | A process on this machine. There is no network path in the wake code |
| Audio from *before* the wake word | A fixed-size RAM ring — `wake_ring_ms`, 1200 ms by default, **hard-capped at 3000 ms**. Never written to disk, never logged |
| Audio from *after* the wake word | Written to `$XDG_RUNTIME_DIR/jarvix` (tmpfs, mode 0600) so whisper can read it, and **deleted as soon as it is transcribed** — exactly what a push-to-talk capture does |
| A false activation with nothing said | Abandoned after 2.5 s. No file, no transcription, no provider call |
| What is logged | A timestamp and a confidence per wake word. Never audio, never text |
| While it is running | The Omarchy bar widget shows a hollow microphone; muted, a struck-through one |

The pre-roll exists because people do not pause between "Jarvix" and the rest
of the sentence — without it the first syllables of every request would be
lost. It is also the only ambient audio that can ever reach a transcript,
which is why the default is well under the ceiling. `wake_ring_ms = 0` keeps
none of it, at the cost of a clipped first word.

### Muting

```bash
jarvix mute      # kills the capture process
jarvix unmute    # opens it again
jarvix status    # prints the pid, so `ps -p <pid>` either finds it or does not
```

`jarvix mute` does not set a flag that makes Jarvix ignore what it hears: it
kills `pw-record` and returns only once the process has been reaped, wiping
every audio buffer on the way. "Nothing is listening" is checkable in the
process table rather than promised in a document.

Muting is runtime state and does not survive a restart —
`activation.mode = "push_to_talk"` is the durable off switch. The bar widget's
panel offers mute and unmute as one click whenever background listening is on.

### Tuning

| Setting | What it changes |
|---|---|
| `wake_sensitivity` | Higher activates more readily and misfires more. The default 0.5 is the score threshold openWakeWord's own examples use |
| `endpoint_silence_ms` | How long a lull ends a request. Below ~500 ms a pause mid-sentence submits half of it; above ~1200 ms every request ends in a wait |
| `wake_ring_ms` | See above. Shorter is more private and clips more |
| `max_utterance_sec` | Bounds one request, and with it how much audio exists at once |

To measure false activations for yourself, leave the daemon running for a
working day without saying the wake word and divide `activations` (from
`jarvix status`) by the uptime. Jarvix does not publish a figure for this,
because it depends on your model, your microphone, and your room — the ADR
explains what *is* measured.

Two known limits, both stated in the ADR: there is no echo cancellation, so
Jarvix saying "Jarvix" in an answer can retrigger it (PipeWire's
`module-echo-cancel` is the fix, and it belongs to your audio setup); and a
wake word heard while the push-to-talk chord is held is ignored, because the
deliberate gesture wins.

### Hearing its own name

"Jarvix" is not an English word, so whisper — which can only write words it
knows — rounds it to the nearest real one: "Jarvis", "JavaX". The detector
fires correctly (it listens to sound, not spelling), but the transcript then
opens with a word the strip does not recognise, the summons stays in the
request, and an intent like "Jarvix, volume thirty" never matches. Two
mechanisms fix this, from both ends:

- **The recogniser is biased toward the name.** Every transcription — warm
  `whisper-server` request and cold `whisper-cli` run alike — carries an
  initial prompt naming the assistant (`assistant.name`), plus anything you
  add to `stt.vocabulary`. With the shipped `base.en` model this alone turns
  "Jarvis, what time is it?" back into "Jarvix, what time is it?".
- **The known mishearings are accepted anyway.** `assistant.aliases` lists
  the spellings the wake-transcript strip treats as the name, under exactly
  the same leading-whole-word rule. Ask about Jarvis Cocker mid-sentence and
  every word survives.

Both are visible in `jarvix doctor` ("name recognition"). The aliases widen
only what is *stripped from a wake transcript* — they are never given to the
acoustic detector, so false-activation behaviour is unchanged, and a
push-to-talk transcript is never edited at all.

## Deterministic intents (`[intents]`)

Some things are not questions. "Volume thirty" has exactly one right outcome,
and waiting seconds for a language model to reach it is absurd — so Jarvix
matches a table of fixed phrases *before* the AI and, on a hit, runs a local
command in microseconds with no model call at all
([ADR 0017](adr/0017-deterministic-intent-router.md)).

The shipped table:

| Intent | Say | Runs |
|---|---|---|
| `volume.set` | "volume thirty", "volume 30", "set the volume to 55", "volume 20 percent" | `wpctl set-volume -l 1.5 @DEFAULT_AUDIO_SINK@ <n>%` |
| `volume.up` | "volume up", "louder", "turn it up", "turn the volume up" | `wpctl set-volume … 5%+` |
| `volume.down` | "volume down", "quieter", "turn it down", "turn the volume down" | `wpctl set-volume … 5%-` |
| `volume.mute` | "mute", "mute the volume/audio/sound" | `wpctl set-mute … 1` |
| `volume.unmute` | "unmute", "unmute the volume/audio/sound" | `wpctl set-mute … 0` |
| `speech.stop` | "stop", "stop talking", "be quiet", "shut up", "enough" | stops speech — **acknowledged with silence** |
| `conversation.new` | "new conversation", "start over", "forget that", "clear the conversation" | the same reset as `jarvix new` |
| `workspace.switch` | "workspace 4", "go to workspace four", "switch to workspace 4" | switches you to workspace `<n>` |
| `terminal.open` | "open terminal", "open a terminal", "new terminal" | starts `<[intents] terminal>` |

Numbers work spoken or written — "volume thirty" and "volume 30" are the same
request — anywhere from 0 to 150 (workspaces 1–10).

**Matching is strict, and that is the feature.** A pattern must match the
*whole* utterance, word for word. "Turn it up" is an intent; "turn it up a
bit" is not, and goes to the AI, which is exactly where an ambiguous request
belongs. Nothing is fuzzy-matched, and a value outside the allowed range
(`volume 500`) is treated as a miss rather than quietly clamped. A matched
intent is still recorded in the conversation, so a follow-up like "a bit
louder" reaches the model with the context it needs.

The last two act on your compositor, and they go through the same seam the
`desktop.*` window tools use ([ADR 0022](adr/0022-desktop-window-control.md)).
That matters because `hyprctl dispatch` changed syntax when Hyprland moved its
configuration to Lua, and which syntax your machine speaks follows the config
format rather than the version — so it is discovered once, at run time, and
both forms work.

`jarvix doctor` checks that `wpctl`, `hyprctl`, and your terminal are
installed, **and that a dispatch actually reaches a compositor** — an
installed `hyprctl` with no Hyprland behind it (a daemon started outside the
graphical session, say) would otherwise look perfectly healthy while
"workspace four" did nothing. A command that fails, a dispatch the compositor
refuses, or a missing binary gets one spoken line — the session never hangs,
and it never claims to have done something it did not do.

### Your own intents (`[[intents.custom]]`)

```toml
[[intents.custom]]
match = "good night"
run = "systemctl suspend"
say = "Good night."
```

`match` is a literal phrase (no placeholders — a slot would mean splicing
speech into a shell command). `run` is a real shell command, so it goes
through the **tool permission gate** exactly like the assistant's own calls,
under the tool name `intent.run`: by default it asks for confirmation before
running. To make your own intents instant, either allow that identity —

```toml
[tools.policy.tool]
"intent.run" = "allow"           # deny rules (rm -rf /, …) still apply
```

— or allow-list just the commands you use (`shell_allow = ["hyprlock"]`).

A malformed entry fails validation at startup, naming the entry:
`intents.custom[1]: match "…" has no run command`.

## Routines (`[[routines]]`)

One sentence sets up your desktop: a routine is a named sequence of "launch
this app on that workspace" steps with optional placement, triggered by a
phrase the intent router recognises — so it runs deterministically, in
milliseconds, with **no model call**. "Jarvix, morning setup" and the day's
windows land where you always put them.

The worked example, matching that scenario — terminal on one, browser and
editor split on two, chat floated on nine:

```toml
[[routines]]
name = "morning setup"
phrases = ["morning setup", "start my usual apps"]

  [[routines.steps]]
  app = "alacritty"
  workspace = 1

  [[routines.steps]]
  app = "firefox"
  workspace = 2
  tile = "master"                # the big pane of the master/stack layout

  [[routines.steps]]
  app = "code"
  workspace = 2
  tile = "split"                 # tiled into the workspace's split

  [[routines.steps]]
  app = "signal-desktop"
  match = "signal"               # how the *window* is recognised, when its
                                 # class differs from the binary's name
  workspace = 9
  float = true
  size = [1200, 800]             # pixels; floating only
  position = [100, 100]          # pixels; floating only
```

How a run behaves:

- **Already running beats launching.** Each step first looks for an existing
  window (matched the way `desktop.focus_window` matches, on `match` or the
  app name) and places that window instead of starting a second copy. Re-run
  the routine and it converges on the same layout rather than opening a
  second browser.
- **Launches go through the compositor**, exactly like "open a terminal":
  `app` must be one bare executable name or absolute path — never a command
  line, never a shell — and the window inherits your graphical session's
  environment.
- **One failure doesn't strand the rest.** An app that will not start, or
  whose window never appears within the bounded wait, is skipped and named in
  the single spoken summary at the end: "Morning setup: three apps placed;
  slack did not start." There is no per-step monologue; the bar and the
  conversation window show progress via `routine.*` events instead.
- **"Jarvix, stop" aborts a run mid-placement**, and speaking the phrase
  again while a run is still placing windows is refused with one line rather
  than interleaving two sequences.
- The run ends with your view on the **first step's workspace**.

Placement notes: `float`, `size`, and `position` go together (a tiled
window's geometry belongs to the layout); `tile` and `float` are mutually
exclusive; `tile = "master"` briefly focuses the window to promote it, which
is why a routine's master steps pull your view along until the final switch.

Routines run under their own permission identity, **`routine.run`, default
"allow"** — every step is something you wrote in your own configuration, and
the phrase is itself the instruction. On a shared machine you can still
demand a confirmation, or disable routines outright:

```toml
[tools.policy.tool]
"routine.run" = "ask"            # or "deny"
```

Phrases are validated at load against the built-in intents, your
`[[intents.custom]]` phrases, and each other — a collision is a startup
message naming both owners, never a coin toss at match time. Because the
router is the trigger, `intents.enabled = false` with routines configured is
a validation error.

Every routine also takes the shared **`enabled`** switch (the
`[[knowledge.feeds]]` convention, adopted here with #93): `enabled = false`
parks the routine without deleting it. The entry, its steps, and your
comments stay — still listed everywhere, still validated, so a parked entry
that rots is reported — but its phrases leave the intent grammar on the next
reload (the utterance falls through to the assistant), any `schedule` comes
off the clock, and the run surfaces refuse it by name with "disabled", never
"unknown". A parked phrase is free for another entry to take; see the
knowledge section for how re-enabling behaves then.

`jarvix routines` lists what is configured (offline, from the file);
`jarvix routines run "morning setup"` triggers one from a script or
keybinding through the same gate the spoken phrase uses, and the conversation
window's Automations tab shows each routine with Run and Enable/Disable.
Enable/Disable persists through `automations.set_enabled` (the surgical
config write — only that entry's `enabled` key changes); everything else in
the tables is hand-edited TOML — outside `jarvix config set` — and lands on
the next `config.reload` or restart.

### Capturing a routine from the live desktop ("save this as …")

You never have to write these tables yourself. Arrange the desktop the way
you want it, then say:

> "Jarvix, save this as my morning setup"

(also "save this layout as …", "save this setup as …", "save this desktop
as …", "remember this layout as …" — the words after "as", up to six, become
the routine's name and its trigger phrase). Jarvix reads the window
inventory once, derives one step per window — app, workspace, floating
geometry or tiled split — and writes the `[[routines]]` entry through the
same comment-preserving rewrite `jarvix config set` uses, with a provenance
comment (`# captured 2026-08-21`) above it. Your hand-written comments and
formatting elsewhere in the file are untouched, and a failed write leaves
the file exactly as it was. The confirmation counts what was kept: "Seven
windows across three workspaces, saved as morning setup."

What capture **excludes**, by rule:

- the Jarvix/Omarchy-shell surfaces themselves;
- windows that accept no input (splash screens and other transient
  surfaces), and windows with no class at all;
- windows on special (scratchpad) workspaces — routine steps place numbered
  workspaces 1–99 only.

Unmapped windows never appear in the inventory to begin with. Capture is
read-only against the desktop: it can never move a window.

The launch command is derived from the window class (collapsing reverse-DNS
classes like `md.obsidian.Obsidian` to `obsidian`, and trying the
`-desktop` packaging convention), and only a command that actually resolves
on your PATH is written; when class and command differ, `match` is recorded
so re-runs dedupe onto the running window. A window whose command cannot be
derived — a browser web-app profile, say — is still saved: its step gets the
placeholder `app = "CHANGE-ME"` with a `# TODO:` comment naming the class,
the spoken confirmation says which app needs a hand, and `jarvix routines`
(and `routines.list`) mark the routine **incomplete** until you edit the
real command in. Tiled windows are captured as `tile = "split"`; promoting
one to `"master"` is a one-word hand edit, because the inventory cannot say
which window owns the layout.

If the name already exists, Jarvix asks before replacing ("A routine called
morning setup already exists. Should I replace it with this layout?") —
declining leaves the file untouched, and replacing keeps the entry's curated
trigger phrases while the steps are replaced wholesale. A captured routine
is immediately runnable: the daemon reloads its intent table as soon as the
capture's own session finishes.

## Scripts (`[[scripts]]`)

Give a script a phrase: any executable you have written can run by voice.
"Jarvix, backup my notes" matches in the deterministic intent router — **no
model is involved in deciding what runs** — and executes your file behind the
permission gate. This is the escape hatch for "can Jarvix do X?": write a
script that does X, give it a phrase.

The worked example:

```toml
[[scripts]]
name = "backup notes"
phrases = ["backup my notes", "back up my notes"]
path = "/home/you/bin/backup-notes.sh"
timeout_sec = 120                # killed (whole process group) at expiry
report = "summary"               # what success says; see below
```

How a run behaves:

- **It asks first, by default.** Running a script is gated under its own
  permission identity, **`script.run`, default "ask"** — the opposite of
  `routine.run`'s allow, because a script is an arbitrary executable behind a
  phrase that can be misheard. The spoken confirmation names the script *and
  its full path* ("I'm about to run your backup notes script, at
  /home/you/bin/backup-notes.sh. Should I go ahead?"), so what is about to
  run is in the question itself. Promote a specific setup you trust, or
  disable scripts outright:

  ```toml
  [tools.policy.tool]
  "script.run" = "allow"         # or "deny"; deny always wins
  ```

  A global `[tools.policy] default = "allow"` deliberately does **not**
  reach scripts — only naming `script.run` does. A global `default = "deny"`
  denies them, like everything else.
- **Zero arguments, always (v1).** The script is executed as its path and
  nothing else: no arguments, no shell, no extra environment. Nothing you say
  — and nothing the assistant generates — can reach the script's argv or
  environment, because the configuration has no field to put it in and the
  runner passes none. Phrases with `{placeholders}` are refused at load.
  Spoken parameters are future work with their own validation design; for
  now, a script that needs variants gets one entry per variant.
- **What success says is yours to choose** with `report`:
  - `summary` (the default): "Backup notes finished."
  - `stdout`: the first line your script printed, so it can compose its own
    acknowledgement ("Notes backed up: 12 files."). Capped and normalised
    for speech.
  - `silent`: nothing.

  **Failures are spoken in every mode** — "Backup notes failed — exit 2:
  disk full" (stderr's first line, when there is one), or "backup notes did
  not finish within 120 seconds and was stopped". Silent means quiet
  success, never invisible breakage.
- **Bounded and stoppable.** The run is killed — its whole process group, so
  children die too — at `timeout_sec`, and "Jarvix, stop" (or any
  interruption) aborts it mid-run. One run at a time: a script phrase spoken
  while any script is still running is refused with one line. Output is
  captured with hard caps; the daemon's environment is passed with
  credential-shaped variables (API keys, tokens, passwords) scrubbed.
- **Every run is on the record.** The activity feed gets a row when a script
  starts (with its path) and when it finishes (exit status and duration,
  success included), and the journal logs the same. Output is never in
  events or logs.

The path must be **absolute, present, and executable** — checked at config
load (a relative path, a missing file, or a `chmod -x` is a startup message
naming the entry, and `jarvix doctor` reports the same per script), and
re-checked at run time, so a file that changed under a running daemon is a
spoken sentence rather than a surprise. Absolute-only is deliberate: a bare
name resolved on PATH would let whatever shadows it first own your phrase.

Phrases are validated at load against the built-in intents, your
`[[intents.custom]]` phrases, your routines, and each other — a collision is
a startup message naming both owners, never a coin toss at match time.
Because the router is the trigger, `intents.enabled = false` with scripts
configured is a validation error.

Every script also takes the shared **`enabled`** switch (the
`[[knowledge.feeds]]` convention, adopted here with #93): `enabled = false`
parks the script without deleting it. The entry stays — still listed, still
validated, path checks included, so a parked script whose file rotted is
still a startup message — but its phrases leave the intent grammar on the
next reload, any `schedule` comes off the clock, and the run surfaces refuse
it by name with "disabled", never "unknown". A parked phrase is free for
another entry to take; see the knowledge section for how re-enabling behaves
then.

`jarvix scripts` lists what is configured (offline, from the file, path
included); `jarvix scripts run "backup notes"` triggers one through the same
gate the spoken phrase uses, and the conversation window's Automations tab
shows each script with its path, Run, and Enable/Disable. Enable/Disable
persists through `automations.set_enabled` (the surgical config write — only
that entry's `enabled` key changes); everything else in the tables is
hand-edited TOML — outside `jarvix config set`, and the definitions
(phrases, paths) stay read-only over IPC on purpose: no IPC client can
repoint a phrase at a different file. Definition edits land on the next
`config.reload` or restart.

Scripts and routines stay distinct on purpose: a routine step launches and
places apps and can never be a command (ADR 0026); a script runs a command
and never places windows. The full threat model — what a misheard phrase, a
malicious config edit, or a swapped script file could do, and which control
answers each — is ADR 0030.

## Schedules (`schedule` on routines and scripts)

Any routine or script can also fire on a clock: add a `schedule` key to its
table and the daemon runs it through the **exact same gated path** the
spoken phrase takes — router, permission gate, events, activity rows, and
"Jarvix, stop" all behave identically ([ADR 0032](adr/0032-scheduled-automations.md)).

The syntax is one friendly form (there is no cron): a 24-hour time,
optionally followed by days.

```
"08:30"                # every day at 08:30
"02:00 daily"          # the same, spelled out
"08:30 mon-fri"        # weekdays (also: "08:30 weekdays")
"10:00 weekends"       # sat and sun
"22:15 mon,wed,fri"    # a list; ranges may wrap ("fri-mon")
```

A bad schedule is refused at load with the accepted forms in the message.
Times are your local wall clock; a firing lands within the minute. A
disabled entry (`enabled = false`, #93) keeps its `schedule` key but is off
the clock entirely — nothing fires, and the Automations tab shows the
schedule as paused rather than with a next-fire time.

The worked nightly-backup example — the backup script from above, run every
night at two:

```toml
[[scripts]]
name = "backup notes"
phrases = ["backup my notes"]
path = "/home/you/bin/backup-notes.sh"
timeout_sec = 120
report = "stdout"
schedule = "02:00"               # nightly; nobody is awake, so —
# announce = false               # — the default: nothing is spoken

[tools.policy.tool]
"script.run" = "allow"           # required for the schedule to execute:
                                 # a schedule cannot answer "should I go
                                 # ahead?", so only allow runs unattended
```

What a scheduled firing does, and does not do:

- **Only allow-tier entries execute unattended.** `script.run` defaults to
  ask, and a schedule has nobody to ask at 2am — so without the policy line
  above, the scheduled moment produces a **refusal**: an activity row plus a
  notification ("backup notes was scheduled but needs your confirmation —
  run it now?") whose click opens the conversation window. Configuring a
  schedule on an ask-tier entry is warned about at load, so you learn at
  noon, not at 2am. Routines default to allow and run unattended as
  configured.
- **Nothing is spoken, by default.** A scheduled run's outcome lands in the
  activity feed and the finish notification; `report` still decides what
  that outcome *says*. Set `announce = true` on the entry if you really want
  the summary spoken aloud at the scheduled hour.
- **Overlap skips.** A firing that arrives while the previous run (or any
  conversation) is still going is skipped and reported — never queued, and
  never an interruption of a session in flight.
- **Missed firings report, never replay.** A firing that fell while the
  daemon was off produces one activity row at the next boot ("missed while
  off") and is not re-run — say the phrase if you still want it. The only
  state behind this is a small machine-written trail
  (`$XDG_STATE_HOME/jarvix/automations.toml`); deleting it costs one boot's
  report, nothing else.

`automations.schedules` over IPC lists every schedule with its next-fire
time, last run, and whether it would currently be refused — the Automations
tab's read surface. Schedule keys are part of the hand-edited tables and
land on the next `config.reload` or restart, like the tables themselves.

## Desktop context (`[context]`)

Ask "what does this error mean?" with a stack trace selected and Jarvix
answers *that* error. Before each question reaches the model, the daemon
gathers what you are looking at — the focused window, your selection, your
clipboard — and offers it as a clearly-delimited system message
([ADR 0019](adr/0019-desktop-context.md)):

```text
Desktop context: what the user is looking at on this computer right now…

--- active window ---
Alacritty — nvim internal/session/engine.go
--- end active window ---

--- selected text ---
panic: runtime error: index out of range [5] with length 3
--- end selected text ---
```

| Source | Default | Read with |
|---|---|---|
| `window` | on | `hyprctl activewindow -j` |
| `selection` | on | `wl-paste --primary --no-newline --type text` |
| `clipboard` | **off** | `wl-paste --no-newline --type text` |

The clipboard is the one source that is off by default: a window title is
already on screen and a selection is what you are pointing at, but the
clipboard holds whatever you last copied, for any purpose. Turn it on when you
want "reply to this" to work:

```bash
jarvix config set context.clipboard=true     # applies between sessions
jarvix config set context.selection=false    # or take an eye away
```

What the design guarantees:

- **A source that is off is never read.** No `wl-paste`, no `hyprctl`, no
  subprocess at all — the gatherer does not exist. With every source off,
  context costs a session literally nothing.
- **Never more than 300ms.** Sources are gathered in parallel, each under
  `timeout_ms`, and anything slow, hung, or missing degrades to *no context*
  rather than to a slower answer. `timeout_ms` may be lowered but not raised:
  the budget is the feature's premise. (Measured on a live Hyprland session:
  about 2ms.)
- **Secrets are redacted before the model sees them.** Private-key headers,
  vendor token prefixes (`sk-…`, `ghp_…`, `AKIA…`), labelled assignments
  (`api_key = "…"`), and high-entropy random tokens replace the *whole*
  source with `[looks like a secret — not shared]`. It is a heuristic, and
  the first line of defence is still that the clipboard is opt-in.
- **You can always see what it saw.** `jarvix status --last` prints the exact
  text that reached the model, already truncated and redacted, beside the
  latency budget of the same interaction — what it cost and what it saw are
  the same question:

  ```text
  last:     session s7
            release → transcript                 412 ms
            desktop context gathered               2 ms
            transcript → first token (model)     286 ms
            release → first audio (total)        901 ms
              of which Jarvix (excl. model)      615 ms
  context:  session s7, captured just now, gathered in 2ms
            window     (26 chars)
              Alacritty — nvim internal/session/engine.go
            selection  (4212 chars, truncated)
              panic: runtime error: index out of range [5] with length 3
              …
  ```

  Gathering is reported as its own stage (`context_ms`) and counted in
  Jarvix's own number rather than inside the model's thinking time — a cost
  hidden in the figure it inflates would be worse than not measuring it.

- **Captured content is never logged and never persisted.** The debug log
  records which sources contributed and how many characters each held, never
  what they said. A capture lives for one turn — it is not written into the
  conversation history, and it does not survive a daemon restart.

`jarvix doctor` lists the enabled sources and whether `hyprctl` and `wl-paste`
are installed (`sudo pacman -S wl-clipboard`). A missing binary is a warning,
never a failure: the assistant simply answers without eyes.

## Memory (`[memory]`)

Say "remember that the staging server is called atlas", and from then on any
question that needs that fact gets it — across conversations and daemon
restarts — without you repeating yourself
([ADR 0025](adr/0025-curated-knowledge-base.md)). This is not conversation
history: it is a small, curated set of facts Jarvix stores **only when you
explicitly ask**, offered to the model on every turn as a clearly-delimited
block ("things the user asked you to remember").

```text
you:    remember that the staging server is called atlas
jarvix: I'll remember that the staging server is called atlas.
        …weeks later…
you:    how do I ssh to staging?
jarvix: Connect to atlas — ssh atlas — and …
```

Hearing, correcting, and forgetting all work by voice and from the CLI:

```bash
jarvix memory list                  # everything Jarvix knows
jarvix memory list staging          # ...matching a query
jarvix memory forget m3             # by id, or by words:
jarvix memory forget "partner's birthday"
```

What the design guarantees:

- **You own the store.** It is one hand-editable TOML file,
  `~/.local/state/jarvix/memory.toml` (0600), with its format documented in
  its own header. Edit it and the change is live on the next question — no
  restart. A file Jarvix cannot parse degrades to a warning and an empty
  memory, and is moved aside (`memory.toml.corrupt`), never overwritten, so
  a typo cannot cost you your facts.
- **Corrections update; they do not accumulate.** "Actually the staging
  server is helios" supersedes the stored fact rather than sitting beside
  it, and the old value stays on the fact's trail with both timestamps —
  `jarvix memory list` answers "when did that change".
- **Memory cannot crowd out the conversation.** The injected block has a
  token budget (`max_injected_tokens`); facts that do not fit are left out
  of the turn — least recently confirmed first, never deleted — and the
  model is told the list is incomplete so it can search with its recall
  tool instead of concluding a fact does not exist.
- **Forgetting is deletion, and deletion is confirmed.** The forget tool is
  the one memory verb behind an "ask" confirmation by default, and the
  question names the exact fact about to go. Remember and recall run
  silently: storing is disclosed by the spoken confirmation and undoable by
  forgetting.
- **Every turn is auditable.** `jarvix status --last` prints which facts the
  model was just given, beside the desktop context and the latency budget of
  the same interaction. Fact content never appears in logs or bus events.
- **Off means off — but never deletes.** `enabled = false` unregisters the
  tools and injects nothing, and leaves the store file untouched: only an
  explicit forget (or deleting the file) removes facts.

## Knowledge feeds (`[[knowledge.feeds]]`)

Memory holds facts you *stated*; feeds hold values that **change** — a stock
price, the weather, a build status
([ADR 0031](adr/0031-knowledge-feeds.md)). You write a command that prints
the current value; Jarvix runs it on a schedule (or on first use), keeps the
latest output warm in the daemon, and answers from it instantly — with the
value's age spoken honestly:

```text
you:    what's the AMD price?
jarvix: AMD is at 187 dollars 42, as of four minutes ago.
```

A worked example — a stock price via a one-line script. First the script
(yours to write; Jarvix ships no network code for this):

```bash
#!/bin/sh
# ~/bin/amd-price — print the current AMD share price and nothing else
curl -sf 'https://query1.finance.yahoo.com/v8/finance/chart/AMD?range=1d&interval=1d' \
  | jq -r '.chart.result[0].meta.regularMarketPrice'
```

`chmod +x ~/bin/amd-price`, check it prints one value, then configure the
feed:

```toml
[[knowledge.feeds]]
name = "amd"                              # what the model asks for
description = "AMD share price in US dollars"  # steers the model to it
command = ["/home/you/bin/amd-price"]     # fixed argv — never a shell line
mode = "eager"                            # refreshed on schedule (default)
interval_sec = 300                        # every five minutes (default)
ttl_sec = 600                             # fresh for ten minutes (default: 2× interval)
inject = true                             # optional: carry the value into every turn
```

Two modes:

- **`eager`** (default) refreshes on `interval_sec`, so the value is already
  sitting in the daemon before you ask. Best for things you ask about often.
- **`lazy`** fetches on the first ask, then serves the cached value until
  `ttl_sec` lapses, after which the next ask refetches. Best for occasional
  questions where a few seconds' wait on first use is fine.

What the design guarantees:

- **The model never chooses what runs.** The command is a fixed argv from
  your configuration; the model can only name a feed
  (the `knowledge.get` tool). Fetches run with the advisor path's
  discipline: no shell, a scrubbed environment (nothing named like a
  credential, none of Jarvix's own API keys), a timeout that kills the whole
  process group, and a capped output.
- **Freshness is spoken, staleness is disclosed.** Every answer from a feed
  carries the value's age in words ("as of four minutes ago"). A value past
  its `ttl_sec` still serves — but is disclosed as stale, out loud.
- **A failing feed never breaks a session.** The last good value serves with
  its (older) age disclosed, the failure is logged and visible in
  `jarvix doctor` (`feed "amd": … failing since …`), and retries back off —
  doubling from the feed's own cadence, capped at an hour — so a broken
  command is not hammered.
- **Values survive restarts.** They persist in
  `~/.local/state/jarvix/feeds.toml` (0600 — feed values may be sensitive;
  they never appear in logs or bus events), so the daemon boots warm:
  eager schedules resume where their timestamps left off, and a value past
  its ttl at boot is served as stale until refreshed. The file is a
  machine-written cache — deleting it just makes every feed fetch fresh.
- **Injection shares the memory block's budget.** Feeds with `inject = true`
  ride into every model turn under `knowledge.max_injected_tokens`
  (default 300, `[knowledge]` table); values that do not fit are left out
  of the turn — last-declared first — and the model is told the list is
  incomplete so it reaches for `knowledge.get` instead.
- **The gate applies, under its own identity.** Feed reads and refreshes are
  judged as `knowledge.refresh`, which defaults to **allow** — you authored
  the command, and a read of your own data should not interrupt itself with
  a question. `[tools.policy.tool]."knowledge.refresh" = "deny"` disables
  feeds outright; `"ask"` confirms each tool read and also stops the
  background schedules (a scheduled fetch has no way to ask).

Every feed also takes an **`enabled`** switch:

```toml
[[knowledge.feeds]]
name = "amd"
command = ["/home/you/bin/amd-price"]
enabled = false   # parked: nothing fetches, the entry and its last value stay
```

This is the one convention for switching any `[[…]]` entry off (issue #92),
and every table-array family follows it: the field is named `enabled`,
**absent means true** (every existing config keeps working unchanged), and a
disabled entry is still validated — a broken command or a rotted script path
is reported even while parked, so re-enabling can never surprise with a
per-entry problem. Feeds implement it; a disabled feed is never fetched
(scheduled, lazy, or manual), never injected, answers "switched off" through
the tool, and keeps its cached value visible — with its honest age — in
`knowledge.status` and the window's Knowledge tab. `[[routines]]` and
`[[scripts]]` carry the same key (#93): a disabled entry's phrases leave the
intent grammar on the standard reload and its schedule leaves the clock,
while the entry stays listed in every surface. One carve-out follows from
parking a phrase family: a disabled entry's phrases leave the **collision
check** too — that is exactly what lets a new entry take a sleeping one's
phrase — so re-enabling recompiles the grammar and can fail with the same
collision error a config load gives, naming both owners. The switch surfaces
that error and writes nothing: never a silent half-enable.

Feed tables stay plain TOML you can hand-edit like `[[routines]]` — outside
`config.set` and listed over IPC (`knowledge.status`). The conversation
window's Knowledge tab operates them (#92, extended by #100): **Refresh
now** (`knowledge.refresh_now`, an immediate fetch through the scheduled
path), **Enable/Disable** (`knowledge.set_enabled`), and **New/Edit/Delete**
in a form dialog (the generic entry verbs of ADR 0033 with `family:
"knowledge.feeds"` — the loader's own validation shown inline per field
before anything is written). Every write shares one discipline: only the
entry being written moves — your comments and every other byte of the file
are preserved — and a file edited by hand underneath an open window is a
refused conflict, never a clobber. Hand edits apply on the next
`jarvix config reload` (idle-class); adding the *first* feed registers the
tool, which takes a daemon restart — the form's save says so honestly
(`applied: false` with the restart reason) when it writes that first feed.

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

## Window control (`[tools] desktop`)

Jarvix can move you around your own desktop: say what is open, switch to a
window, send one to another workspace, close one, or start an application
([ADR 0022](adr/0022-desktop-window-control.md)). "Put me back in my browser",
"what have I got open?", "move this to workspace three", "open Spotify".

It is **on by default**, unlike `shell.run`, because each verb is one bounded
action on a window the compositor itself named — visible on screen, undoable
by hand, and unable to enter data anywhere. Sending keystrokes into a window is
a different thing entirely and is not part of this.

Name a window loosely and it is found on class *and* title, case-insensitively:
"firefox", "the editor", "my browser", "the one about pull requests". Say
"this" (or nothing) and it means the window you are in.

- **Several windows match** → Jarvix names them and asks which you meant. It
  does not pick one. A wrong focus is cheap but it teaches you that it is
  guessing.
- **Nothing matches** → it says so in one sentence.
- **Hyprland is not running, or `hyprctl` is missing** → the tools say they
  cannot see your windows, and everything else Jarvix does is unaffected.
  `jarvix doctor` names the missing piece.

How much it asks first:

| Verb | Default | Why |
| --- | --- | --- |
| `desktop.list_windows` | allow | Sees no more than the desktop context Jarvix may already gather |
| `desktop.focus_window` | allow | Changes only where you are looking, and you can see it happen |
| `desktop.move_window` | ask | Changes your workspace layout |
| `desktop.close_window` | ask | Closes something you might be in the middle of |
| `desktop.launch_app` | ask | Starts a program |

The confirmation names the actual window — "I want to close firefox, the window
titled GitHub. Should I go ahead?" — generated from the live window list, never
from the assistant's description of what it is doing. Override any of them
under `[tools.policy.tool]`, e.g. `"desktop.move_window" = "allow"` or
`"desktop.close_window" = "deny"`.

`desktop_apps` restricts what may be launched:

```toml
[tools]
desktop_apps = ["firefox", "alacritty", "/opt/apps/notes"]
```

Each entry is one program — a name found on PATH or an absolute path — because
applications are executed directly, never through a shell. Empty (the default)
allows anything installed. A category also works ("open a browser") when
exactly one such application is installed; when several are, Jarvix asks which.

## Typing (`[tools.typing]`)

Jarvix can type into the window you are working in — dictating into a
document, filling a form field, composing a message in a web app
([ADR 0023](adr/0023-synthetic-keystrokes.md)). It is **off by default**, and
it is the one capability worth reading about before you turn it on.

```bash
sudo pacman -S wtype
jarvix config set tools.typing.enable=true    # takes effect on daemon restart
```

Two tools, and they are separate on purpose:

| Tool | Default | What it does |
| --- | --- | --- |
| `typing.type_text` | ask | Types literal characters into the focused window |
| `typing.press_key` | ask | Presses one key: enter, tab, escape, backspace, delete, or an arrow |

**Both always ask.** Unlike every other tool, they do not inherit
`[tools.policy] default = "allow"` — allowing them takes naming them under
`[tools.policy.tool]`, which is a sentence you have to mean. A global `deny`
still denies them. An approval is never carried forward by
`remember_for_conversation` either: it was about a payload *and* the window
that had focus at that moment, and the second half does not survive you moving.

What the safeguards actually do:

- **The confirmation shows the text.** "I want to type "call the bank at
  three" into Obsidian — Daily note. Should I go ahead?" — built from the live
  window list and the literal characters, never from the assistant's
  description of what it is doing.
- **Focus is re-checked at the last moment.** The window is captured when you
  are asked and checked again the instant before the keys go out, against a
  fresh look at the desktop. If a notification, a dialog or your own hand moved
  focus while you were answering, **nothing is typed** and Jarvix says the
  window changed.
- **Literal characters only.** Line breaks, Tab, Escape and every other control
  key are refused — the whole request, not just the offending character, so
  what you approved is what happens. Text can therefore never submit itself.
- **Submitting is separate.** Approving text is never approving Enter. Pressing
  a key is its own tool, its own confirmation, and a closed list of thirteen
  keys with no modifiers and no chords.
- **Terminals always ask**, even with `"typing.type_text" = "allow"`, and the
  confirmation says so: anything typed into a shell may be run as a command.
  `terminal_classes` names the windows this applies to.
- **Caps.** `max_chars` per call and `rate_limit` per `rate_window_sec`, shared
  between both tools, each refusing with a reason rather than silently dropping
  the call.
- **The text is never logged.** Which window, how many characters, whether you
  approved it and what happened are recorded — `jarvix status --last` prints
  the last one, and a `typing.audit` event goes out on the bus. *What* was
  typed is not recorded anywhere, because you may have dictated a password.

If `wtype` is missing, there is no Wayland session, or the compositor refuses
the virtual-keyboard protocol, the tools say they have no way to send
keystrokes and everything else Jarvix does is unaffected. `jarvix doctor` names
which of the three it is and how to fix it — the check probes with an empty
payload, so it never types anything either.

`ydotool` is deliberately not supported: it needs a root daemon and write
access to `/dev/uinput`, which is a permanently elevated privilege on your
machine in exchange for the same keystrokes.

Turning it off again:

```bash
jarvix config set tools.typing.enable=false
```

## Advisors (asking a stronger assistant)

Jarvix runs a small local model — right for instant answers, wrong for
"review this architecture". When a request exceeds it (or you say "ask Claude
about…"), it hands the question to an assistant CLI you already have
installed and speaks the answer back
([ADR 0016](adr/0016-advisor-delegation.md)). Each CLI keeps its own auth and
billing; Jarvix never passes its own API keys on.

`jarvix setup` detects the known CLIs on PATH and writes a table per advisor
you accept. That table is all it takes — the shipped preset supplies the
non-interactive command line, a description for the model, and a 120-second
timeout:

```toml
[advisors.claude]
binary = "/usr/bin/claude"
```

Override anything you like. `args` is an argv template, passed straight to
the program — **no shell is involved at any point**, so nothing in a question
can be interpreted as syntax. The question reaches the CLI on stdin unless
exactly one argument is the literal `{question}`, which is replaced with it
as a single argument:

```toml
[advisors.house]
binary = "/home/me/bin/house-llm"
args = ["--ask", "{question}"]
timeout_sec = 300
description = "the research box in the basement, good at maths"
```

How much it asks first:

- An advisor running an **unmodified read-only preset** (`claude`, `codex`,
  `gemini` — one-shot answering modes) is consulted **silently**: it reads and
  replies, which is no more authority than the local model already has.
- Everything else **asks first**: the coding agents that edit files and run
  commands (`aider`, `goose`, `opencode`), and any advisor whose `args` you
  wrote yourself — Jarvix has not audited that command line, so it will not
  claim it only answers. You hear "I'd like to ask aider about this. Should I
  go ahead?" and answer as with any other confirmation.
- `[tools.policy.tool]` overrides both: `"advisor.ask" = "ask"` confirms every
  consultation, `"allow"` confirms none, `"deny"` turns delegation off.

What a consultation is bounded by: the configured timeout (the whole process
group is killed, so helper processes die too), a 64 KB cap on the answer, and
the session context — Escape or `jarvix cancel` kills the CLI immediately. If
it is missing, fails, or takes too long, Jarvix says so in one sentence and
moves on; the CLI's error output is logged, never read aloud. After about ten
seconds it says once that it is still working, and the overlay shows
"Consulting claude…" for the duration.

`jarvix doctor` lists each configured advisor and whether its binary is still
there (a `LookPath`, nothing more — no network, no invocation, nothing
billed).

## Artifacts (work you keep)

Ask for something better seen than heard and the assistant calls
`artifact.create` with the right format instead of reading structure aloud:

| You say | Format | File | Opens via |
|---|---|---|---|
| "diagram my publish pipeline" | `mermaid` | `.mmd` + rendered `.png` (2×) | `open_command` |
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
`open_command`, default `xdg-open`). Each entry is either a string, split on
whitespace, or an argv array — viewers are exec'd directly, never through a
shell, so a viewer under `/opt/my viewer/bin` needs the array form
(`["/opt/my viewer/bin/view", "--new"]`) to survive. Setting a format's entry
to `""`, `[]`, or `"none"` means "no viewer": the assistant saves the file and tells you its
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

Diagrams render to **PNG at 2× scale by default**. Mermaid's SVG output
carries its labels as HTML inside `<foreignObject>`, which only a browser
engine renders — opened in an image viewer, the diagram is boxes with no
text. The PNG is the pixels the renderer's embedded browser already drew, so
it shows its text in whatever viewer the desktop picks. If you want markup to
edit or embed, set `diagram_format = "svg"`: that path renders with
`htmlLabels` disabled so labels become real `<text>` elements, **but some
shapes still emit `foreignObject`** and will lose their text outside a
browser — it is an opt-in for people who know they want SVG, not a better
default. Either way the `.mmd` source is saved beside the render, and
`jarvix artifacts` lists the pair as one diagram (an orphan `.mmd` with no
render still lists on its own).

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

The download includes **54 voices across nine languages** — four British
female and four British male among them. See "Language and accent" above, or
run `jarvix voices`.

Assistant answers are normalised before they are spoken — markdown, code
fences, and list bullets are stripped so nothing reads "asterisk" or
"backtick" aloud — while the overlay still shows the formatted text.

### Numbers

The engine's own handling of figures is not good enough for an assistant that
reports them: Kokoro reads "9.2 million" as "nine two million", dropping the
decimal point and with it the meaning. Numbers are therefore expanded to their
spoken form before synthesis:

| Written | Spoken |
|---|---|
| `9.2 million`, `3.14` | nine point two million, three point one four |
| `82.4%` | eighty two point four percent |
| `£3.50`, `$1`, `$9.2m` | three pounds fifty, one dollar, nine point two million dollars |
| `v1.5.2`, `1.5.2` | version one point five point two, one point five point two |
| `4.7s`, `250ms`, `2 mins` | four point seven seconds, two hundred and fifty milliseconds, two minutes |
| `1.5GB`, `512MB` | one point five gigabytes, five hundred and twelve megabytes |
| `3-5` | three to five |
| `21st` | twenty first |
| `-3.5` | minus three point five |

Numbers that belong to something else are left exactly as they are: bare
integers (`port 8080`, `1995` — every engine reads those correctly), and
anything wedged against letters, a slash, a colon or an underscore
(`sail-8.5/app`, `127.0.0.1:8080`, `2026-08-21`, `COVID-19`). A figure the
expander cannot parse is passed through unchanged rather than failing the
answer.

### Pronunciation lexicon (`[tts.lexicon]`)

Technical vocabulary is the vocabulary Jarvix says most, and it is exactly
what a voice model guesses at: Kokoro reads "Golang" with the vowel of *posh*
rather than *going*. The lexicon respells a term before synthesis:

```toml
[tts.lexicon]
Kubernetes = "koo ber net eez"   # override a shipped default
k9s = "kay nine ess"             # or add your own word
```

Jarvix ships defaults for `Golang`, `Kubernetes`, `nginx`, `PostgreSQL`,
`Hyprland`, `Wayland`, `PipeWire` and `sudo`; your entries are merged over
them, so writing a term here either adds a word or replaces a default —
including replacing one with the original spelling to turn it off.

Terms match case-insensitively and only on whole words, so an entry never
corrupts a longer word that contains it ("sudo" leaves "sudoku" alone). Only
the *spoken* form changes: the overlay and the conversation window always show
what the assistant actually wrote. The lexicon is an idle-class setting, so a
fix applies on the next answer after `jarvix config reload` — no restart.

```bash
jarvix config get tts.lexicon
jarvix config set 'tts.lexicon=Kubernetes=koo ber net eez,k9s=kay nine ess'
```

## XDG paths

| Purpose | Path |
|---|---|
| Config | `~/.config/jarvix/config.toml` |
| Models | `~/.local/share/jarvix/models/` |
| State | `~/.local/state/jarvix/` |
| Memory (remembered facts, hand-editable) | `~/.local/state/jarvix/memory.toml` |
| Socket | `$XDG_RUNTIME_DIR/jarvix.sock` |
| Recordings (transient) | `$XDG_RUNTIME_DIR/jarvix/` (tmpfs, deleted after use) |
| Artifacts (diagrams, documents, spreadsheets, sketches) | `~/Documents/Jarvix/` (configurable: `[artifacts] dir`) |
