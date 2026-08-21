# Jarvix IPC protocol

## Transport

- **Socket:** `$XDG_RUNTIME_DIR/jarvix.sock`, mode `0600`. No TCP listener
  exists.
- **Framing:** newline-delimited JSON (one JSON document per line).
- **Protocol:** JSON-RPC 2.0. Clients send requests; the daemon sends
  responses, plus **notifications** (no `id`) for asynchronous events, pushed
  to every connected client.

A stale socket left by a crashed daemon is detected (connect fails) and
replaced on startup; a live socket makes a second daemon refuse to start.

## Versioning

`status.get` returns `protocol` (currently `1`). Incompatible protocol
changes bump it; clients should check it once after connecting. Additive
changes (new methods, new events, new fields) do not bump the version —
clients must ignore unknown events and fields.

## Requests

| Method | Params | Result | Notes |
|---|---|---|---|
| `session.start` | — | `{session_id}` | Cancels any active session first (interruption) |
| `voice.start` | — | `{}` | Idle → Listening; starts capture |
| `voice.stop` | — | `{discarded}` | Listening → Transcribing; transcription runs async. `discarded: true` means the capture was shorter than `audio.min_recording_ms` and the session ended quietly (`session.cancelled`) — skip the follow-up `session.submit` |
| `session.submit` | `{text?}` | `{}` | With `text`: skip audio, go think. Without: proceed when the transcript lands |
| `session.cancel` | — | `{}` | Stops everything; no-op when idle |
| `speech.cancel` | — | `{}` | Stops spoken output only; no-op unless speaking |
| `conversation.reset` | — | `{}` | Forget carried-over context; the next turn starts a fresh thread |
| `conversation.get` | — | `{turns, state, session_id}` | Snapshot of the current conversation for display: `turns` is an array of `{role, text}` (`user`/`assistant`, oldest first, including the in-flight user question once transcribed). Render it on open, then live-append from `assistant.delta` / `transcript.final` / `state.changed` / `error` events |
| `status.get` | — | `{state, session_id, version, protocol, ptt}` | `ptt` is `"daemon"` when jarvixd watches the hold-to-talk chord itself (keybinding toggles must then no-op) or `"external"` when keybindings drive activation |

Errors use JSON-RPC error objects. Application errors (wrong state, no active
session) use code `-32000`; standard codes cover parse/method/params issues.

### Example

```text
→ {"jsonrpc":"2.0","id":1,"method":"session.start"}
← {"jsonrpc":"2.0","id":1,"result":{"session_id":"s7"}}
→ {"jsonrpc":"2.0","id":2,"method":"session.submit","params":{"text":"hello"}}
← {"jsonrpc":"2.0","id":2,"result":{}}
← {"jsonrpc":"2.0","method":"state.changed","params":{"state":"thinking","session_id":"s7"}}
← {"jsonrpc":"2.0","method":"assistant.started","params":{"session_id":"s7","provider":"ollama"}}
← {"jsonrpc":"2.0","method":"state.changed","params":{"state":"responding","session_id":"s7"}}
← {"jsonrpc":"2.0","method":"assistant.delta","params":{"session_id":"s7","content":"Hi"}}
← {"jsonrpc":"2.0","method":"assistant.delta","params":{"session_id":"s7","content":" there."}}
← {"jsonrpc":"2.0","method":"assistant.finished","params":{"session_id":"s7","content":"Hi there."}}
← {"jsonrpc":"2.0","method":"state.changed","params":{"state":"speaking","session_id":"s7"}}
← {"jsonrpc":"2.0","method":"tts.started","params":{"session_id":"s7"}}
← {"jsonrpc":"2.0","method":"tts.finished","params":{"session_id":"s7"}}
← {"jsonrpc":"2.0","method":"state.changed","params":{"state":"idle","session_id":"s7"}}
← {"jsonrpc":"2.0","method":"session.finished","params":{"session_id":"s7"}}
```

Try it by hand:

```bash
printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"status.get"}' | nc -U "$XDG_RUNTIME_DIR/jarvix.sock"
```

## Events

Every event's params include `session_id` where a session is involved.

| Event | Params | Meaning |
|---|---|---|
| `state.changed` | `{state, session_id}` | Authoritative state transition |
| `recording.started` / `recording.stopped` | `{session_id}` | Microphone capture bounds |
| `transcript.partial` | `{text}` | Interim hypothesis (reserved; whisper.cpp V1 emits none) |
| `transcript.final` | `{text}` | Completed transcript |
| `assistant.started` | `{provider}` | Provider request opened |
| `assistant.delta` | `{content}` | One streamed response fragment |
| `assistant.finished` | `{content}` | Full response text |
| `tts.started` / `tts.finished` | `{}`; finished may carry `{interrupted:true}` | Speech bounds |
| `session.finished` | `{}` | Session completed (also after an error) |
| `session.cancelled` | `{reason}` | Session cancelled or interrupted |
| `error` | `{stage, message}` | A stage failed: `audio`, `stt`, `assistant`, `tts`, `session` |

Ordering guarantees: `state.changed` precedes the stage events it enables;
`session.finished`/`session.cancelled` is always the last event of a session.
Slow consumers may lose events (the daemon never blocks on a client), so the
overlay treats `state.changed` as the source of truth and resyncs with
`status.get` on reconnect.
