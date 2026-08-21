# Jarvix roadmap

The premise: **talking to your Linux computer can be a first-class
interaction model rather than a novelty bolted onto a chatbot.** Each phase
keeps that experience coherent while widening what "the computer" can do.

## Phase 1 — Voice conversation ✅

Hold-to-talk interactions: hold a key, speak, get a response that streams
into the overlay and is **spoken as it streams**, interrupt at will, with
conversation memory across turns. Local STT (whisper.cpp), local TTS (Piper
or the more natural Kokoro), provider-independent AI, tool calling
(`shell.run`), Omarchy overlay, `jarvix` CLI, `jarvix doctor`.

## Phase 2 — Desktop context (window/selection/clipboard done)

The conversation gains eyes: active window, selected text, and clipboard are
gathered by the daemon and offered to the model, so "what does this error
mean?" with a stack trace selected answers *that* error
([ADR 0018](adr/0018-desktop-context.md)). Every source is opt-in
(`[context]`, clipboard off by default), a disabled source is never read at
all, gathering is parallel inside a 300ms hard budget (measured: ~2ms), and
anything that looks like a secret is redacted before it leaves the machine.
What was captured is always inspectable afterwards with
`jarvix status --last`.

Gathering happens on the model path only — after the Phase 3 intent router —
so a deterministic intent never pays for eyes it does not use.

Remaining: the optional **screen region** (capture + OCR), which needs its own
consent UX, and the `desktop.*` action tools in Phase 4.

## Phase 3 — Deterministic local intents (done)

A fast intent router in front of the AI:

```text
STT ──► Intent Router ──► deterministic intent ("volume 30", "mute",
         │                 "workspace 4", "open terminal", "stop talking")
         └───────────────► AI conversation (everything else)
```

Common commands execute in milliseconds without any model
([ADR 0017](adr/0017-deterministic-intent-router.md)): the router is an
explicit grammar/pattern table — not a machine-learning system — matched
strictly against the whole utterance, so anything it does not recognise
verbatim reaches the AI unchanged. Built-ins (volume set/up/down,
mute/unmute, "stop", "new conversation", workspace *n*, open terminal) map to
a fixed argv; user-defined intents (`[[intents.custom]]`) run through the
tool permission gate. Measured: ~230ns on the miss path, zero provider calls
on a hit.

## Phase 4 — AI tools (started)

The model can act, not just answer. `shell.run` ships now
([ADR 0009](adr/0009-tool-calling.md)): the assistant runs commands itself
and summarises the result. The permission gate ships too
([ADR 0014](adr/0014-tool-permission-gate.md)): allow / ask / deny per tool,
with spoken confirmation for the ask tier. Remaining: structured tools —
`desktop.*`, `clipboard.*`, `apps.*`, `system.*`, `hyprland.*`, `files.*` —
each behind that gate (unknown tools default to ask). The engine's tool loop
already generalises to all of them.

## Phase 5 — Conversational mode (persistence: done; threads: next)

Multi-turn follow-ups work now ([ADR 0010](adr/0010-streaming-speech-and-memory.md)):
the engine keeps a rolling history, so "why is my build failing?" → "what
should I change?" retains context, bounded by a turn cap and an idle window,
clearable with `jarvix new`. History also persists across daemon restarts
([ADR 0011](adr/0011-persistent-conversation-history.md)), honouring the
same idle window. Remaining: per-conversation threads.

## Phase 6 — Wake word and realtime interaction

"Computer." — always-on summoning without a keyboard, plus support for
realtime multimodal providers (`audio ↔ model ↔ audio`) as an alternative
backend to the STT → LLM → TTS pipeline. The provider seam is per-session, so
a realtime backend replaces the pipeline inside a session without changing
the IPC surface.

## Phase 7 — Extensible tool ecosystem

Third-party tools (MCP or similar), per-tool permissions, and integration
targets like Home Assistant and Spotify. Explicitly out of scope until the
core interaction is excellent.

---

Deliberately **not** planned into V1 (see brief §22): agent loops, shell
execution, email/calendar, browser automation, vector databases, embeddings,
OCR, accessibility-tree integration, system audio capture, multi-user,
remote control, mobile apps. These stay out until a phase actually needs
them.
