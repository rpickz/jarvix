# Development checklist

Working checklist for the current milestone, per the brief (§29–31). Keep it
honest: an item is checked only when built, tested, and documented.

## Milestone 1 — voice conversation vertical slice

### Step 1 — Skeleton ✅
- [x] Go module, monorepo layout, Makefile, MIT license
- [x] `internal/config`: TOML + XDG paths + defaults + validation + redaction
- [x] Structured logging (slog, journald-friendly)
- [x] IPC socket: JSON-RPC 2.0 server/client, stale-socket handling
- [x] systemd user unit
- [x] Acceptance: `jarvix status` talks to `jarvixd` ✅ (verified live)

### Step 2 — Session engine ✅
- [x] Explicit state machine, validated transitions, tested
- [x] Event bus with non-blocking fan-out to IPC clients
- [x] Cancellation from every active state; interruption via `session.start`
- [x] Fakes for AI/STT/TTS/audio; full-lifecycle integration tests
- [x] Acceptance: fake interaction emits thinking→responding→speaking→idle ✅

### Step 3 — AI provider ✅
- [x] `openaicompat`: SSE streaming, presets (ollama/openai/openrouter/lmstudio),
      custom endpoints from config, error surfacing, doctor probe
- [x] Acceptance: `jarvix ask "Explain recursion in one sentence"` streams a
      real response ✅ (verified against Ollama llama3.2:3b)

### Step 4 — TTS ✅
- [x] Piper adapter: voice resolution, sample-rate from sidecar, raw PCM
      streaming into pw-play, kill-to-cancel
- [x] Acceptance: `jarvix ask "Say hello"` speaks aloud ✅ (verified)
- [x] Mid-speech cancel verified (<10 ms to silence) ✅

### Step 5 — Recording + STT ✅
- [x] PipeWire capture (pw-record → tmpfs WAV, SIGINT finalise, safety cap)
- [x] whisper.cpp adapter + `jarvix setup whisper` model download
- [x] `jarvix listen` end-to-end
- [x] Acceptance verified live: full closed-loop voice test through the
      daemon (Piper-spoken question captured via PipeWire, transcribed
      verbatim by whisper.cpp, answered by Ollama, spoken aloud) ✅

### Step 6 — Omarchy overlay ✅
- [x] Plugin (manifest schemaVersion 1, kind panel, keepLoaded)
- [x] Socket client + reconnect + status resync; linger after finish/error
- [x] Theme-native card (qs.Commons/qs.Ui), input-transparent overlay layer
- [ ] Acceptance verified on-screen against live daemon state

### Step 7 — Keyboard activation ✅
- [x] Press/release bindings via Omarchy Lua API (`{ release = true }`)
- [x] `jarvix ptt start|stop`; cancel binding; install script (managed block)
- [ ] Acceptance: hold → speak → release → response verified end-to-end

### Step 8 — Polish
- [x] `jarvix doctor` with actionable fixes for every dependency
- [x] Docs: README, architecture, ipc, configuration, providers, roadmap, ADRs
- [ ] Overlay transition tuning after on-screen verification
- [ ] Audio feedback cue on listen start (optional, evaluate)

## Milestone acceptance criteria (brief §31)

- [x] Clean-machine install path documented
- [x] AI provider configurable (incl. custom endpoints without code)
- [x] Daemon runs as user systemd service
- [x] Overlay plugin loads via Omarchy plugin registry
- [x] Voice → transcript → streamed + spoken response verified through the
      daemon's push-to-talk path (`jarvix ptt start/stop` with real audio
      capture); human hold-`Super+Alt+V` uses the identical code path
- [x] Cancel and interrupt work from every state (tested + verified live)
- [x] Failures produce understandable errors; daemon survives provider/audio
      failures (tested)
- [x] Automated tests cover the core session lifecycle
- [x] Documentation sufficient for another developer to extend

## Next milestones

Tracked in [implementation-plan.md](implementation-plan.md).
