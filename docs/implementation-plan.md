# Implementation plan — beyond the foundation

The foundation (Milestone 1) proves the interaction: push-to-talk → local
STT → streamed AI → spoken response, cancellable at any instant. This plan
sequences the remaining work. Each milestone is a shippable vertical slice
with tests and docs; none requires reworking the foundation's seams — that
was the point of choosing them.

Milestones are ordered by product value per unit of risk. Within each, the
listed order is the intended build order.

---

## M2 — Finish and harden the first-run experience

*Goal: a stranger installs Jarvix in ten minutes and it feels finished.*

1. **Live-verify the voice path end-to-end** (whisper-cpp install, real mic,
   overlay on screen, hold-to-talk). Tune overlay transitions and timings
   from real use.
2. **Latency pass.** Measure and log per-stage latency (`duration_ms` fields
   already exist). Targets: key-release → transcript < 1.5 s (base.en);
   transcript → first token < 500 ms local; response-done → first audio
   < 300 ms. Options if missed: whisper.cpp server mode behind the same
   adapter (ADR 0002 anticipates this), keeping Piper warm.
3. **Audio feedback cues.** A subtle earcon on listen-start and cancel
   (PipeWire play of a bundled sample; config to disable). This is the
   "brief acknowledgement sound" of the product vision.
4. **Sentence-chunked TTS.** Speak sentence 1 while sentences 2+ are still
   streaming from the model: split the assistant stream on sentence
   boundaries and feed Piper per sentence through the existing chunk
   channel. The `tts.Synthesizer` interface already supports it; the change
   is engine-side (a `Speaking` sub-loop) plus tests. This is the single
   biggest perceived-latency win.
5. **Packaging.** PKGBUILD (AUR: `jarvix-git`), `omarchy plugin add`
   compatibility (the repo already carries `manifest.json` at the plugin
   root), release workflow with versioned builds.
6. **CI.** GitHub Actions: build, vet, staticcheck, race tests. Add
   `golangci-lint` config.

**Acceptance:** `paru -S jarvix-git && jarvix doctor` reaches all-OK on a
stock Omarchy machine in one sitting; speech begins before the model finishes
responding.

## M3 — Conversational state (Phase 5 of the roadmap, pulled early)

*Goal: follow-ups without ceremony — the single biggest step from "demo" to
"daily tool". Pulled ahead of desktop context because it needs no new
permissions surface.*

1. **Conversation history in the engine.** A `conversation` type owning
   `[]ai.Message` with a turn limit and token budget; sessions append to it.
   `session.start` gains `{continue: bool}` — the PTT chord continues the
   conversation within a configurable follow-up window (e.g. 60 s since last
   response), a long-press or `jarvix new` starts fresh.
2. **IPC additions** (additive, no version bump): `conversation.reset`,
   `conversation.get`; events `conversation.turn`. Overlay shows a subtle
   "continuing" indicator.
3. **Config:** `[conversation] history_turns`, `follow_up_window_sec`.
4. **Tests:** multi-turn flows, window expiry, reset, interruption
   mid-conversation.

**Acceptance:** "Why isn't this building?" → answer → "What should I
change?" works by just holding the key again.

## M4 — Desktop context (roadmap Phase 2)

*Goal: "what's on my screen?" answers correctly.*

1. **Permissions architecture first** (it gates everything from here on):
   `[permissions]` config table, capability tags, allow/ask/deny, an
   `permission.request` overlay/IPC flow for "ask".
2. **Context collectors** in the daemon, gathered at session start under the
   session context: active window (`hyprctl activewindow -j`), selected text
   (`wl-paste -p`), clipboard (`wl-paste`). Injected as a structured context
   block in the user message. Config + per-collector permission tags.
3. **Screen region capture (stretch):** `grim`/portal screenshot piped to a
   vision-capable model when the provider supports images — requires
   extending `ai.Message` with image content parts (additive).

**Acceptance:** with a compiler error focused in a terminal, "what does this
error mean?" answers about *that* error.

## M5 — Native cloud providers

*Goal: first-class Anthropic (and Gemini) support, proving the provider seam
with a second wire format.*

1. `internal/ai/anthropic`: Messages API streaming (`content_block_delta`),
   `ANTHROPIC_API_KEY`, model presets; registered as provider `anthropic`.
2. `internal/ai/gemini` (optional, same pattern).
3. Provider registry: replace the single-constructor switch in
   `internal/daemon` with a small `name → factory` map; config unchanged for
   users.
4. Doctor: per-provider probe messages.

**Acceptance:** `provider = "anthropic"` + exported key streams and speaks.

## M6 — Kokoro TTS (ADR 0007 follow-through)

1. `internal/tts/kokoro` as a managed local server adapter (kokoro-onnx or
   kokoro-fastapi; decide by benchmarking install friction vs quality).
2. `jarvix setup kokoro` for model/voice download.
3. Config `tts.provider = "kokoro"`, voice selection, doctor checks.
4. Keep Piper as the zero-setup default until Kokoro's install is one
   command.

**Acceptance:** switching `tts.provider` swaps the voice; cancellation
latency unchanged (<50 ms to silence).

## M7 — Deterministic intent router (roadmap Phase 3)

*Goal: "volume 30", "mute", "workspace 4", "open terminal", "stop talking"
execute in milliseconds, no model involved.*

1. `internal/intent`: an explicit pattern table (verb + slot grammar, no ML)
   run on the final transcript before the AI stage. Match → execute local
   action + brief spoken/overlay confirmation; no match → AI as today.
2. Actions reuse the tool executor from M8's boundary (or precede it with a
   thin internal action layer if M7 lands first — both orders work).
3. New state is *not* needed: router runs inside `Thinking` entry; on match
   the session jumps to a short confirmation `Speaking`.
4. Config: enable/disable per intent group; custom phrases.
5. Tests: table-driven matching, near-miss phrases falling through to AI.

**Acceptance:** "mute" mutes in under 500 ms from key release, offline.

## M8 — AI tools (roadmap Phase 4)

*Goal: the assistant can act, gated by permissions.*

1. Implement the `Tool` interface + registry per ADR 0006; wire tool-call
   streaming into `openaicompat` (standard `tool_calls` format) and the
   engine's Thinking loop.
2. First tools (read-only first): `desktop.get_active_window`,
   `clipboard.read`, `system.volume`, `hyprland.workspace`, `apps.launch`,
   `clipboard.write`. `shell.run` last, default-deny, always-ask.
3. Overlay: `tool.started/finished` events rendered as a quiet activity line.
4. Every tool execution under the session context (cancel kills it), through
   the M4 permission gate, with structured audit logging.

**Acceptance:** "switch to workspace 2 and open a terminal" works, asks
before anything destructive, and Escape aborts mid-plan.

## M9 — Wake word & realtime (roadmap Phase 6, exploratory)

- Wake word ("Computer") via openWakeWord/Porcupine as an *activation
  source* beside push-to-talk — a new producer of `session.start` +
  `voice.start`, nothing else changes. Requires an always-on capture path
  (native PipeWire stream — the measured revisit ADR 0003 anticipated).
- Realtime providers (`audio ↔ model ↔ audio`): a session *mode* where one
  provider implements STT+AI+TTS combined. The engine gains a second
  pipeline path; states and IPC events are already voice-shaped, so the
  overlay works unchanged.
- Ship behind config flags; both are UX experiments until latency and
  false-trigger rates are measured.

---

## Cross-cutting tracks (ongoing, any milestone)

- **Secrets:** Linux Secret Service/keyring lookup as an `api_key_source`
  option beside env vars (config schema already isolates key resolution in
  `Endpoint.Key()`).
- **Streaming STT:** `transcript.partial` events exist in the protocol;
  a whisper.cpp-server or faster-whisper adapter can start emitting them
  with zero IPC/overlay changes (overlay already renders partials).
- **Observability:** `jarvix status --watch` (follow events in the
  terminal); per-stage latency summary in `session.finished`.
- **Robustness:** property/fuzz tests for the IPC parser; soak test script
  cycling 1000 fake sessions checking for goroutine/file leaks.

## Explicitly deferred

Everything in brief §22 stays out until its phase: agent loops, email,
calendar, browser automation, memory/vector stores, OCR, accessibility tree,
system-audio capture, Spotify/Home Assistant, MCP, WASM, multi-user, remote,
mobile.
