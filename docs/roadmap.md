# Jarvix roadmap

The premise: **talking to your Linux computer can be a first-class
interaction model rather than a novelty bolted onto a chatbot.** Each phase
keeps that experience coherent while widening what "the computer" can do.

## Phase 1 — Voice conversation ✅ (current milestone)

Push-to-talk one-turn interactions: hold a key, speak, get a streamed and
spoken response, interrupt at will. Local STT (whisper.cpp) and TTS (Piper),
provider-independent AI, Omarchy overlay, `jarvix` CLI, `jarvix doctor`.

## Phase 2 — Desktop context

The conversation gains eyes: active window, selected text, clipboard,
optionally a screen region — gathered by the daemon at session start and
offered to the model. "What does this error mean?" while a terminal is
focused should just work. Requires the permissions architecture to say what
Jarvix may look at.

## Phase 3 — Deterministic local intents

A fast intent router in front of the AI:

```text
STT ──► Intent Router ──► deterministic intent ("volume 30", "mute",
         │                 "workspace 4", "open terminal", "stop talking")
         └───────────────► AI conversation (everything else)
```

Common commands execute in milliseconds without any model. The router starts
as an explicit grammar/pattern table — not a machine-learning system.

## Phase 4 — AI tools

The model can act, not just answer, through the `Tool` interface sketched in
`docs/adr/0006-future-tool-boundary.md`: `desktop.*`, `clipboard.*`,
`apps.*`, `system.*`, `hyprland.*`, `files.*`, `shell.run` — each gated by
the permissions architecture, with spoken confirmation for destructive
actions.

## Phase 5 — Persistent conversational mode

Multi-turn sessions with follow-ups ("What should I change?" … "Can you do
that?"). The conversation engine keeps history; sessions outlive a single
push-to-talk exchange. The session/state model already treats the transcript
as session state rather than a stateless request, so this extends rather than
replaces it.

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
