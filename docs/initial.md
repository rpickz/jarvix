# Jarvix — Initial Development Agent Prompt

You are building **Jarvix**, an open-source, voice-native computer interaction layer designed primarily for **Omarchy on Arch Linux / Hyprland / Wayland**.

The core concept is:

> **Voice → Computer**

Jarvix is not merely dictation, and it is not merely a voice wrapper around an LLM.

Jarvix should eventually allow a user to communicate intent naturally to their computer, with the system deciding whether that intent should be handled through:

* deterministic local commands
* desktop context
* an AI provider
* a tool invocation
* conversational state
* speech output
* combinations of the above

The experiential inspiration is the **Star Trek computer / JARVIS interaction model**:

* summon the computer instantly
* speak naturally
* receive immediate spoken feedback
* ask follow-up questions without ceremony
* discuss what is currently on screen
* ask the computer to perform actions
* interrupt it naturally
* avoid opening and manipulating a traditional assistant application

The project is called:

# Jarvix

The name is a tongue-in-cheek combination of **JARVIS + Unix/Linux**.

The default wake word may eventually be:

> **Computer**

Do not build wake-word support in the first milestone unless doing so is trivial and does not distract from the core vertical slice.

---

# 1. Your objective

Create the initial Jarvix repository, architecture, documentation and first usable vertical slice.

At the end of this milestone, a user running Omarchy should be able to:

1. invoke Jarvix with a keyboard shortcut
2. see a lightweight native-looking listening overlay
3. speak into their microphone
4. release the shortcut to submit, or cancel explicitly
5. have speech transcribed
6. send the transcript to a configured AI provider
7. receive a streamed AI response
8. see the response appear in the overlay
9. hear the response spoken aloud
10. interrupt/cancel output cleanly

The first milestone should feel polished enough that the interaction demonstrates the product vision.

The target interaction is:

```text
Hold Super+V

    ↓

Jarvix overlay appears

    ↓

User speaks

    ↓

Release Super+V

    ↓

Speech is transcribed

    ↓

AI response streams into overlay

    ↓

Jarvix speaks the response aloud

    ↓

Overlay disappears after completion
```

The UI should not resemble a traditional chat application.

Jarvix should feel like the computer itself responding.

---

# 2. Technology choices

Use the following architecture unless there is a strong technical reason to deviate.

## Core daemon

Use:

> **Go**

Create a long-running daemon:

```text
jarvixd
```

The daemon owns:

* session state
* microphone capture coordination
* speech-to-text
* AI provider interactions
* streaming
* text-to-speech
* audio playback
* cancellation
* configuration
* permissions architecture
* future tools
* future desktop context
* IPC

Do not put application intelligence in QML.

---

## Omarchy interface

Use:

> **QML + Quickshell**

Build Jarvix as an Omarchy-compatible third-party plugin.

It should live conceptually under:

```text
~/.config/omarchy/plugins/jarvix/
```

The plugin should contain the visual overlay and interaction surfaces only.

The QML layer should be deliberately thin.

It should render daemon state such as:

```text
idle
listening
transcribing
thinking
responding
speaking
error
```

and events such as:

```text
transcript.partial
transcript.final

assistant.started
assistant.delta
assistant.finished

tts.started
tts.finished

session.cancelled
```

Do not place AI-provider logic in QML.

Do not spawn a second independent Quickshell environment if current Omarchy provides an appropriate plugin lifecycle.

Respect Omarchy's current plugin conventions and shell IPC mechanisms.

Document any assumptions made about the currently installed Omarchy version.

---

# 3. Repository structure

Create a clean monorepo along these lines:

```text
jarvix/
├── cmd/
│   ├── jarvix/
│   │   └── main.go
│   └── jarvixd/
│       └── main.go
│
├── internal/
│   ├── app/
│   ├── audio/
│   ├── config/
│   ├── conversation/
│   ├── events/
│   ├── ipc/
│   ├── providers/
│   ├── session/
│   ├── speech/
│   └── tts/
│
├── providers/
│   ├── openai/
│   ├── openai-compatible/
│   └── ollama/
│
├── speech/
│   └── whisper/
│
├── tts/
│   ├── kokoro/
│   └── piper/
│
├── plugin/
│   └── omarchy/
│       ├── manifest.json
│       ├── JarvixOverlay.qml
│       └── components/
│
├── docs/
│   ├── architecture.md
│   ├── ipc.md
│   ├── configuration.md
│   ├── providers.md
│   └── roadmap.md
│
├── examples/
│
├── scripts/
│
├── go.mod
├── Makefile
├── README.md
└── LICENSE
```

Adjust this where Go conventions suggest a cleaner layout.

Avoid unnecessary package proliferation.

---

# 4. Core design principle: provider independence

Jarvix must not be hard-coded to OpenAI.

Define clean internal interfaces.

For example:

```go
type AIProvider interface {
    Chat(
        ctx context.Context,
        request ChatRequest,
    ) (<-chan AIEvent, error)
}
```

AI events should support streaming.

For example:

```go
type AIEvent struct {
    Type    AIEventType
    Content string
    Err     error
}
```

Support at least these provider categories architecturally:

```text
OpenAI
OpenAI-compatible API
Ollama
Anthropic
Gemini
OpenRouter
```

The first milestone does not need every implementation.

Implement at minimum:

1. one cloud/provider implementation
2. OpenAI-compatible abstraction or Ollama support

Prefer an architecture where OpenAI-compatible endpoints can be configured without requiring a new provider implementation.

---

# 5. Speech-to-text architecture

Speech recognition must also be provider-independent.

Define an abstraction similar to:

```go
type SpeechToText interface {
    Transcribe(
        ctx context.Context,
        input AudioInput,
    ) (<-chan TranscriptEvent, error)
}
```

Transcript events should support future partial transcription:

```text
partial
final
error
```

For the initial local implementation use:

> **whisper.cpp**

Do not deeply bind whisper.cpp into Go unless necessary.

For V1, prefer invoking or managing whisper.cpp as an external native process/service behind a clean adapter.

The rest of Jarvix must not know or care that whisper.cpp is being used.

Future STT implementations should be possible, including:

```text
OpenAI transcription
faster-whisper
Deepgram
Google
other local engines
```

---

# 6. Text-to-speech architecture

Use a similar provider abstraction:

```go
type TextToSpeech interface {
    Speak(
        ctx context.Context,
        request SpeechRequest,
    ) (<-chan AudioChunk, error)
}
```

Prefer:

> **Kokoro**

for the initial local voice implementation.

Allow:

> **Piper**

as a fallback or alternate local implementation.

Architecturally leave room for:

```text
OpenAI
ElevenLabs
system TTS
other APIs
```

TTS must support cancellation.

If the user invokes Jarvix while Jarvix is already speaking, design the session system so speech can be interrupted immediately.

---

# 7. Audio

Target modern Omarchy directly.

Use:

> **PipeWire**

for audio capture/playback integration.

Do not attempt broad cross-platform audio abstraction in V1.

Jarvix is an Omarchy/Linux-first project.

The audio layer should expose clean internal interfaces, however, so implementation details do not leak throughout the system.

Longer term, PipeWire support should make possible:

* microphone input
* normal output playback
* application audio capture
* system audio capture
* meeting/video transcription

Do not implement all of those now.

V1 only requires reliable microphone input and output playback.

---

# 8. IPC

Communication between the QML plugin and daemon should use a local:

> **Unix domain socket**

Prefer an endpoint under:

```text
$XDG_RUNTIME_DIR/jarvix.sock
```

Use a simple structured protocol.

JSON-RPC 2.0 is acceptable and preferred unless there is a compelling reason to use something even simpler.

The protocol must support both commands and asynchronous events.

Commands should include at least:

```text
session.start
session.submit
session.cancel

voice.start
voice.stop

speech.cancel

status.get
```

Events should include at least:

```text
state.changed

recording.started
recording.stopped

transcript.partial
transcript.final

assistant.started
assistant.delta
assistant.finished

tts.started
tts.finished

session.finished
session.cancelled

error
```

Document the protocol.

Do not expose a TCP listener by default.

---

# 9. Session state machine

Create an explicit state machine.

Suggested states:

```text
Idle
Listening
Transcribing
Thinking
Responding
Speaking
Cancelling
Error
```

Transitions should be explicit and tested.

For example:

```text
Idle
 ↓
Listening
 ↓
Transcribing
 ↓
Thinking
 ↓
Responding
 ↓
Speaking
 ↓
Idle
```

Cancellation should be possible from every active state.

Examples:

```text
Listening → Cancelling → Idle
Thinking → Cancelling → Idle
Speaking → Cancelling → Idle
```

Do not spread state across arbitrary booleans.

There should be one authoritative session state model.

---

# 10. Cancellation and interruption are first-class

Cancellation must not be added later as an afterthought.

Use Go contexts consistently.

Every long-running operation should accept:

```go
context.Context
```

Cancellation must propagate through:

```text
audio recording
STT
HTTP streaming
provider requests
TTS
audio playback
```

If the user presses Escape:

> everything associated with the current interaction stops.

If the user begins a new interaction while Jarvix is speaking:

> current speech should stop immediately and the new interaction should begin.

This is essential to making voice interaction feel natural.

---

# 11. Initial keyboard UX

The default interaction should be push-to-talk.

Target:

```text
Super+V
```

Behaviour:

### Key down

```text
session.start
voice.start
```

Jarvix enters:

```text
Listening
```

and displays the overlay.

### Key up

```text
voice.stop
session.submit
```

Recording finishes and transcription begins.

### Escape

```text
session.cancel
```

Everything stops.

If press/release semantics are awkward due to Hyprland binding behaviour, investigate the correct current Omarchy/Hyprland mechanism rather than faking the experience.

Document the resulting shortcut integration.

---

# 12. Overlay UX

The overlay should be minimal.

Do not build a conventional application window.

Aim for something conceptually like:

```text
╭───────────────────────────────╮
│  ◉  Listening…                │
╰───────────────────────────────╯
```

During processing:

```text
╭───────────────────────────────╮
│  Jarvix is thinking…          │
╰───────────────────────────────╯
```

During response:

```text
╭──────────────────────────────────────────╮
│ The issue appears to be caused by…       │
╰──────────────────────────────────────────╯
```

The visual language should fit naturally with Omarchy.

Prefer:

* subtle animation
* restrained waveform/activity indicator
* strong typography
* low visual noise
* transient behaviour
* no unnecessary buttons
* keyboard-first operation

The overlay should disappear when the interaction is complete.

Keep enough response text visible that spoken output is not the only way to understand the answer.

---

# 13. Streaming

Do not wait for the entire AI response before presenting anything.

Support streaming from provider to UI.

Desired flow:

```text
provider token/chunk
       ↓
conversation engine
       ↓
IPC event
       ↓
overlay updates
```

Design TTS so we can eventually begin speaking before the entire response is available.

However, for the first milestone it is acceptable to:

1. stream text visually
2. wait for a sentence or complete response
3. synthesize speech

provided the TTS interface is designed for future streaming/chunking.

---

# 14. Configuration

Use:

> **TOML**

Configuration location:

```text
~/.config/jarvix/config.toml
```

Example:

```toml
[activation]
mode = "push_to_talk"

[ai]
provider = "openai"
model = "..."

[ai.openai]
base_url = "https://api.openai.com/v1"

[stt]
provider = "whisper"

[stt.whisper]
model = "..."

[tts]
provider = "kokoro"

[tts.kokoro]
voice = "..."

[conversation]
speak_responses = true

[ui]
show_transcript = true
show_response = true
```

Use sensible defaults.

Do not require a huge configuration file.

Configuration must be validated at startup with helpful errors.

---

# 15. Secrets

Do not store secrets directly in the TOML configuration unless explicitly supported as a developer/testing fallback.

Prefer environment variables initially.

For example:

```text
OPENAI_API_KEY
```

Architect for eventual integration with Linux Secret Service/keyrings.

Never log secrets.

Ensure diagnostic output redacts credentials.

---

# 16. CLI

Create:

```text
jarvix
```

as a user-facing/control/debug CLI.

It should eventually become:

```text
jarvix status
jarvix listen
jarvix ask "..."
jarvix cancel
jarvix config
jarvix doctor
```

For the initial milestone implement at least:

```text
jarvix status
jarvix ask "Hello"
jarvix cancel
jarvix doctor
```

`jarvix ask` should exercise the same conversation architecture without requiring microphone input.

This gives us a much easier debugging path.

---

# 17. `jarvix doctor`

Build this early.

Voice/audio/AI integration can fail for many environmental reasons.

`jarvix doctor` should inspect relevant dependencies and produce actionable output.

For example:

```text
Jarvix Doctor

[OK] PipeWire available
[OK] microphone detected
[OK] audio output available
[OK] whisper.cpp installed
[OK] Whisper model available
[OK] Kokoro available
[OK] jarvixd running
[OK] Unix socket reachable
[OK] AI provider configured
[OK] provider authentication succeeded
[OK] Omarchy plugin installed

Jarvix appears ready.
```

Failures should explain what to do.

For example:

```text
[FAIL] Whisper model not found

Expected:
~/.local/share/jarvix/models/whisper/...

Run:
jarvix setup whisper
```

Do not merely expose raw dependency errors where Jarvix can give a clearer message.

---

# 18. Logging

Use structured logging.

Prefer Go's standard:

```go
log/slog
```

Default output should be useful for:

```text
journalctl --user -u jarvixd
```

Include useful fields such as:

```text
session_id
provider
state
duration_ms
component
```

Never log:

* API keys
* raw microphone audio by default
* full private conversation history unless debug behaviour is explicitly enabled

---

# 19. systemd

Jarvix should run as a user-level service.

Provide:

```text
jarvixd.service
```

for:

```text
systemctl --user enable --now jarvixd
```

It should:

* start reliably
* restart after crashes
* use the user's environment appropriately
* use XDG paths
* shut down cleanly

Document installation and troubleshooting.

---

# 20. XDG compliance

Use appropriate XDG paths.

Examples:

```text
Config:
$XDG_CONFIG_HOME/jarvix/
or
~/.config/jarvix/

Data:
$XDG_DATA_HOME/jarvix/
or
~/.local/share/jarvix/

State:
$XDG_STATE_HOME/jarvix/
or
~/.local/state/jarvix/

Runtime:
$XDG_RUNTIME_DIR/jarvix.sock
```

Do not scatter files through the home directory.

---

# 21. Testing

This should not become a project that only works when manually tested with a microphone.

Design interfaces so components are mockable.

Tests should cover at minimum:

## Unit tests

* config parsing
* state transitions
* cancellation
* provider event handling
* transcript handling
* IPC request parsing
* IPC event serialization

## Integration tests

Provide fake implementations:

```text
FakeSTT
FakeAIProvider
FakeTTS
FakeAudio
```

A test should be able to run:

```text
fake speech
    ↓
fake transcript
    ↓
fake AI response stream
    ↓
fake TTS
```

and verify the complete session lifecycle without audio hardware or external APIs.

---

# 22. Do not overbuild V1

Do not implement the following yet unless needed to create appropriate extension points:

* autonomous agent loops
* filesystem modification tools
* shell execution
* email
* calendar
* browser automation
* persistent long-term memory
* vector databases
* embeddings
* wake-word recognition
* screen capture
* OCR
* accessibility tree integration
* system audio capture
* Spotify
* Home Assistant
* MCP support
* WASM extensions
* multi-user support
* remote control
* mobile apps

Document these as future capabilities where relevant.

Do not allow future plans to make the initial architecture overly generic or abstract.

---

# 23. However, preserve the future tool architecture

Although tools are not a V1 feature, define or document the likely boundary now.

The future conceptual interface should resemble:

```go
type Tool interface {
    Name() string
    Description() string
    Schema() ToolSchema

    Execute(
        ctx context.Context,
        input json.RawMessage,
    ) (ToolResult, error)
}
```

Future tools will include things like:

```text
desktop.get_active_window
desktop.get_selected_text
desktop.capture_region

clipboard.read
clipboard.write

apps.launch
apps.focus

shell.run

system.volume
system.brightness

hyprland.workspace

files.search
files.read
```

Do not implement these now except perhaps one harmless proof-of-concept tool if it materially validates the architecture.

---

# 24. Future fast intent router

Document a future distinction between:

```text
voice → deterministic intent → local command
```

and:

```text
voice → AI → reasoning/tools
```

For example:

```text
"volume 30"
"mute"
"workspace 4"
"open terminal"
"stop talking"
```

should eventually avoid an LLM entirely.

The long-term architecture should therefore accommodate:

```text
STT
 ↓
Intent Router
 ├── deterministic intent
 └── AI conversation
```

Do not implement a complex intent engine in this milestone.

---

# 25. Future conversation mode

Jarvix will eventually support both:

## Push-to-talk

Explicit interactions initiated by keyboard.

And:

## Conversational mode

A persistent session where the user can naturally continue:

```text
User:
Why isn't this building?

Jarvix:
The compiler error indicates...

User:
What should I change?

Jarvix:
...

User:
Can you do that?

Jarvix:
...
```

The session/conversation architecture should not assume every message is stateless.

But V1 only needs one-turn interactions.

---

# 26. Future realtime providers

Do not assume all future conversation must use:

```text
STT → text LLM → TTS
```

Some providers may support:

```text
audio ↔ realtime multimodal model ↔ audio
```

The architecture should allow a future realtime conversation backend.

Do not implement it now.

Avoid coupling the entire conversation system to text-only assumptions where practical.

---

# 27. Documentation deliverables

Create useful documentation as part of this milestone.

At minimum:

## README.md

Include:

* what Jarvix is
* product vision
* screenshots/placeholders if appropriate
* project status
* requirements
* installation
* configuration
* running
* keyboard interaction
* troubleshooting
* development setup

## docs/architecture.md

Explain:

* daemon
* Omarchy plugin
* IPC
* audio
* STT
* AI provider
* TTS
* session lifecycle

Include Mermaid diagrams where useful.

## docs/ipc.md

Document:

* socket location
* requests
* responses
* events
* examples
* versioning strategy

## docs/configuration.md

Document all supported configuration.

## docs/providers.md

Explain provider abstractions and implemented providers.

## docs/roadmap.md

Include the larger product direction:

### Phase 1

Voice conversation.

### Phase 2

Desktop context.

### Phase 3

Deterministic local intents.

### Phase 4

AI tools.

### Phase 5

Persistent conversational mode.

### Phase 6

Wake word and realtime interaction.

### Phase 7

Extensible tool ecosystem.

---

# 28. Quality expectations

This is intended to become a serious open-source project.

Prioritise:

* simple architecture
* readable Go
* explicit interfaces
* low dependency count
* fast startup
* low idle resource use
* strong cancellation semantics
* useful errors
* testability
* idiomatic Linux integration
* good documentation

Avoid:

* giant frameworks
* LangChain-style orchestration frameworks
* unnecessary dependency injection frameworks
* premature distributed architecture
* running Redis/Postgres/etc.
* Electron
* embedding a web server simply to render UI
* huge abstractions created only for hypothetical future needs

Jarvix should feel Unix-like internally even though its user experience is deliberately futuristic.

---

# 29. Development workflow

Before writing substantial code:

1. inspect the current Omarchy/Quickshell plugin conventions
2. inspect current Hyprland keyboard binding behaviour
3. verify the best current PipeWire integration approach
4. verify how whisper.cpp should be invoked
5. determine a practical Kokoro integration
6. write the architecture documentation
7. identify any risks or deviations from this brief

Then implement incrementally.

Maintain a development checklist in the repository.

After each meaningful vertical slice:

* build
* run tests
* lint
* verify manually where appropriate
* document the result

Do not leave major architectural decisions only in source code comments.

Use ADRs where a decision has meaningful alternatives or long-term consequences.

---

# 30. First implementation sequence

Use approximately this order.

## Step 1 — Skeleton

Create:

* Go module
* CLI
* daemon
* configuration
* logging
* systemd service
* basic IPC socket

Acceptance:

```text
jarvix status
```

can communicate with:

```text
jarvixd
```

---

## Step 2 — Session engine

Implement the state machine and event bus.

Use fake providers.

Acceptance:

```text
jarvix ask "Hello"
```

can perform a completely fake interaction and emit:

```text
Thinking
Responding
Speaking
Idle
```

---

## Step 3 — AI provider

Implement the first real AI provider.

Acceptance:

```text
jarvix ask "Explain recursion in one sentence"
```

streams a real response.

---

## Step 4 — TTS

Implement local speech synthesis.

Acceptance:

```text
jarvix ask "Say hello"
```

results in audible speech.

---

## Step 5 — Recording + STT

Implement PipeWire microphone capture and whisper.cpp transcription.

Acceptance:

```text
jarvix listen
```

allows the user to speak and receive a response.

---

## Step 6 — Omarchy overlay

Implement the native QML overlay.

Acceptance:

the UI accurately represents daemon state.

---

## Step 7 — Keyboard activation

Wire the desired Omarchy/Hyprland shortcut.

Acceptance:

```text
Hold shortcut
→ speak
→ release
→ response
```

works end-to-end.

---

## Step 8 — Polish

Improve:

* startup latency
* visual transitions
* cancellation
* audio feedback
* failure messages
* `jarvix doctor`
* installation
* docs

---

# 31. Milestone acceptance criteria

The milestone is complete when all of the following are true.

A clean Omarchy machine can install Jarvix according to the documentation.

The user can configure an AI provider.

The daemon runs as a user systemd service.

The Omarchy plugin loads correctly.

The user can hold the configured keyboard shortcut.

An overlay visibly indicates Jarvix is listening.

The user can speak a sentence.

Releasing the key stops capture.

The speech is transcribed successfully.

The transcript is submitted to the configured AI provider.

The response streams into the overlay.

The response is spoken aloud.

The user can cancel the session.

The user can interrupt spoken output.

Failures produce understandable errors.

The daemon does not crash because an individual provider/audio request fails.

Automated tests cover the core session lifecycle.

The project has enough documentation that another developer can understand and extend it.

---

# 32. Product feel

This final point is important.

Do not optimise only for technical completeness.

The interaction should have personality through responsiveness rather than gimmicks.

Jarvix should feel:

* immediate
* calm
* competent
* unobtrusive
* powerful
* keyboard-native
* voice-native

Avoid constant acknowledgement phrases such as:

> “Sure! I'd be happy to help with that!”

Prefer concise responses.

For computer-control interactions, eventual behaviour should resemble:

```text
User:
Computer.

Jarvix:
[brief acknowledgement sound]

User:
Why did this build fail?

Jarvix:
The TypeScript build failed because `UserProfile` no longer satisfies the updated interface...
```

The computer should feel present without becoming annoying.

---

# 33. Definition of success

The first release does not need to control the whole desktop.

It needs to convincingly demonstrate this premise:

> **Talking to your Linux computer can be a first-class interaction model rather than a novelty bolted onto a chatbot.**

If a developer installs the first milestone, presses the Jarvix shortcut, asks a question, gets an immediate visual and spoken response, and thinks:

> “Oh. I want my whole computer to work like this.”

then the milestone has succeeded.

Build that experience first.

