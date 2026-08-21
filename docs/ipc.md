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
| `session.text` | `{text}` | `{session_id, confirmation, approved?}` | One typed turn, for a client with a text field (the conversation window's composer). Composes `session.start` + `session.submit {text}` — the `jarvix ask` path — but decides between them and a pending confirmation **in the daemon**, under one lock: with `awaiting_confirmation` in force the text answers the confirmation (`confirmation: true`, `approved` being the same reading a spoken yes/no gets) and the waiting session is left alone; otherwise a new session starts, cancelling any session in flight. Text that is empty or whitespace is rejected with `-32602` and starts nothing. Do not sequence `session.start` yourself from a text field: the state you read can go stale in the round trip, and starting a session then abandons the confirmation the user was answering |
| `session.cancel` | — | `{}` | Stops everything; no-op when idle |
| `session.confirm` | `{approved?}` | `{approved}` | Answers a pending tool confirmation (`approved` defaults to `true`); errors when nothing is pending or a voice reply is already being captured |
| `speech.cancel` | — | `{stopped}` | Stops spoken output whenever audio is actually playing, whatever the session state — mid-answer tool rounds included (issue #54). Stopping speech ends the turn (`tts.finished {interrupted: true}` when the answer had started speaking, then `session.finished`); a pending tool confirmation is abandoned as declined. `stopped: false` reports that nothing was playing — a no-op, not an error |
| `conversation.reset` | — | `{}` | Forget carried-over context (and any remembered tool approvals); the next turn starts a fresh thread. With retention on (ADR 0027) the old thread is already archived — turns are appended as they complete — so the reset only detaches it; it stays listed and reopenable. Publishes `conversation.changed {reason: "reset"}` |
| `conversation.get` | — | `{turns, state, session_id}` | Snapshot of the current conversation for display: `turns` is an array of `{role, text}` (`user`/`assistant`, oldest first, including the in-flight user question once transcribed). Render it on open, then live-append from `assistant.delta` / `transcript.final` / `state.changed` / `error` events |
| `conversation.list` | — | `{retention, active_id, conversations, unreadable}` | The archive (ADR 0027), newest first: each `{id, started, last_active, turns, preview}`. Metadata only — no transcript is opened, so a large library lists fast. `unreadable` (`{id, error}` each) names conversations that could not be parsed: one bad file never hides the library, or itself. `retention: false` means `conversation.retention = "off"` — nothing new is being archived, but the listing (and deletion) still work. `active_id` is the conversation the live thread belongs to, `""` when none. `jarvix conversations` and the window's History view are thin clients |
| `conversation.read` | `{id}` | `{id, started, last_active, preview, turns}` | One whole archived conversation, read-only: `turns` is an array of `{role, text, ts}` (RFC 3339), oldest first. Unknown or unreadable ids are `-32602` |
| `conversation.search` | `{query, limit?}` | `{retention, active_id, results, searched, skipped}` | Full-text search over the archive, live head included (ADR 0028). `results` is ranked best first — exact phrase beats scattered words, recency breaks ties — each `{id, turn, role, ts, passage, current}`: `turn` is the 1-based position in that transcript, `passage` the matched text clipped to one line, `current` true when the hit is in the active conversation ("earlier in this conversation"). `limit` caps results (default 10, ceiling 20). `searched` counts conversations scanned; `skipped` (`{id, error}` each) names ones that could not be — the listing's unreadable contract. A blank query is `-32602`. The window's search box and `jarvix conversations search` are thin clients; the model's `conversations.search` tool shares the same searcher. Queries and passages are never logged |
| `conversation.open` | `{id}` | `{id, turns}` | Reopens an archived conversation as the active thread — the explicit action. The engine keeps the most recent `history_turns` exchanges as model context (the archive keeps everything the prompt cannot), restarts the follow-up window, and appends follow-up turns to the same record. Publishes `conversation.changed {reason: "opened", conversation_id}` |
| `conversation.delete` | `{id}` or `{all: true}` | `{deleted}` | Removes conversations from disk — files gone, no tombstones. Deleting the active conversation (or `all`) also resets the live thread, so the next turn cannot rebuild the record from working memory. Unknown ids are `-32602` |
| `context.last` | — | `{captured, session_id?, at?, age_sec?, duration_ms?, sources?}` | The desktop context gathered for the most recent question that reached the model (ADR 0019) — the audit surface behind `jarvix status --last`. `captured: false` means nothing has been gathered yet this daemon lifetime. Each source is `{source, text, chars, truncated, redacted}`: `text` is **exactly what the model was sent**, already capped at `context.max_chars` and already replaced by `[looks like a secret — not shared]` if it matched the secret heuristics; `chars` is the pre-truncation length. Retained in memory only — never persisted, never surviving a restart |
| `memory.last` | — | `{enabled, injected, session_id?, facts?, trimmed?, total?, est_tokens?}` | The remembered facts injected into the most recent question that reached the model (ADR 0025) — the memory half of `jarvix status --last`, beside `context.last`. `enabled: false` means `memory.enabled = false`; `injected: false` means no turn has consulted the knowledge base yet this daemon lifetime. `facts` is what the model was actually given — each `{id, content, stored, updated, source?, previous?}` — `trimmed` how many stored facts the token cap left out of the block (never out of storage), `total` the store size at injection time, `est_tokens` the block's estimated cost. Retained in memory only |
| `memory.list` | `{query?}` | `{enabled, path?, count?, max?, facts?}` | The store itself, straight from disk (hand-edits included): every remembered fact, or those matching `query` (case-insensitive substring or shared significant word), most recently confirmed first. Each fact carries its supersede trail (`previous`: `{content, stored, superseded}` each), so "when did that change" is answerable. `path` names the file — it is the user's to edit; `count`/`max` report the store cap. `jarvix memory list` is a thin client |
| `memory.forget` | `{id?}` or `{query?}` | `{forgotten, fact?, matches?}` | Deletes one fact from disk — trail and all; deletion is deletion. By `id`, or by `query` when it matches exactly one fact; several matches delete **nothing** and come back as `matches` for the caller to pick an id from (the same refuse-to-guess rule the model-facing tool follows). An unknown id is `-32602`; so is calling it with memory disabled |
| `status.get` | — | `{state, session_id, version, protocol, ptt, policy, warm, wake, wake_state, last_timings, last_typing, conversations, prompt_budget}` | `ptt` is `"daemon"` when jarvixd watches the hold-to-talk chord itself (keybinding toggles must then no-op) or `"external"` when keybindings drive activation. `policy` is the effective tool permission policy: `{default, confirm_timeout_sec, remember_for_conversation, tools: {name: decision}}`. `warm` lists the supervised engine workers (ADR 0018), one entry per engine: `{name, running, pid, rss_mb, uptime_sec, restarts, last_error}` — empty when `performance.warm_engines` is off. `wake` is the background-listening report described under `wake.status` below, and `wake_state` is the one word the microphone indicator draws (`off`, `armed`, `muted`) — both included here so a client that connects mid-life gets the indicator right from one call rather than waiting for the next `wake.changed`. `last_timings` is the most recent `session.timings` payload (plus its `session_id`), or `null` before any session has finished; `jarvix status --last` prints it, alongside the desktop context from `context.last`. `last_typing` is the most recent `typing.audit` payload, or `null` when typing is off or nothing has been typed since the daemon started — it never carries the typed text. `conversations` is the archive-search state (ADR 0028): `{retention, archived, search}` — `search` is `"active"` or `"inactive"` (retention off with nothing archived), a state rather than a failure. `prompt_budget` is what one turn sends before the user has said a word (issue #71), estimated at bytes/4: `{system_prompt_tokens, tool_schema_tokens, memory_tokens, context_tokens, headroom_tokens, floor_tokens}` — measured from the daemon's composed system prompt and its actually-registered tool schemas; `jarvix status` prints the floor against the context window ollama reports serving, and `jarvix doctor` warns when the window is below it |
| `wake.mute` | `{muted?}` | the `wake.status` report | Closes or reopens the background microphone (ADR 0024). `muted` defaults to `true` — `jarvix mute` is the point, and unmuting is the explicit case. **Muting is synchronous**: the call returns only once the capture process has been killed and reaped and every audio buffer has been wiped, so the report it answers with is a statement of fact. Unmuting is not symmetric — killing a process is instant, starting one is not, so the reply will not yet name a pid. Answers on a daemon with background listening switched off (`enabled: false`), because "nothing is listening" is the answer the caller came for |
| `wake.status` | — | `{mode, enabled, running, state, muted, capturing, pid, word, sensitivity, threshold, endpoint_silence_ms, ring_ms, detector, detector_pid, detector_rss_mb, activations, restarts, last_reason}` | Background listening as it actually is. `enabled` is the configured mode; `running` is whether the listener was started (a configured wake word whose detector is not installed is `enabled: true, running: false`, with `last_reason` saying why); `capturing` and `pid` are the live capture process, which is what makes "is my microphone open?" checkable against `ps` rather than merely believable. `activations` counts wake words since the daemon started — divide by uptime to measure your own false-activation rate. With the feature off, only `mode`, `enabled`, `running`, `state`, `muted`, `capturing`, `pid`, `word` and `last_reason` are present |
| `config.get` | — | `{path, fingerprint, fields, secrets}` | The editable settings with their **running** values: each field carries `key` (dotted TOML path), `label`, `type` (`string`/`int`/`float`/`bool`/`string_list`/`string_map`), `reload` class (`live`/`idle`/`restart` — see docs/configuration.md), `value`, and `enum` for closed sets. `fingerprint` identifies the file content on disk (`sha256:…`, or `missing`) for external-edit detection. `secrets` reports API-key **presence** only (`{endpoint, env, env_set, inline_key}`) — key values never travel over IPC in either direction |
| `config.set` | `{changes, fingerprint?}` | `{fingerprint, applied, reason?, needs_restart}` | Validates and writes field changes into config.toml (surgical rewrite preserving comments/unknown keys; atomic temp+rename, mode 0600), then applies them to the running daemon per reload class. `changes` maps dotted keys to values — native JSON types or strings (`"true"`, `"leftmeta,space"`, `"Golang=go lang,nginx=engine ex"` for a `string_map`), both accepted. Pass the `fingerprint` from your `config.get`: if the file changed on disk meanwhile the set fails with `-32002` instead of clobbering the hand edit. `applied: false` + `reason` means the file was written but a session was in flight — retry with `config.reload` once idle. `needs_restart` lists restart-class keys whose file value now differs from the running one |
| `config.reload` | — | `{fingerprint, needs_restart}` | Re-reads config.toml into the running daemon (the recovery path after a hand edit, and the retry after a busy `config.set`). A file that fails parsing or validation is refused with `-32001` and the running configuration is untouched; a session in flight refuses with `-32003` |
| `activity.get` | — | `{rows, limit}` | The activity feed (issue #70): recent daemon activity as rendered rows, oldest first — each `{seq, ts, session_id, kind, label, detail, failed}`, assembled daemon-side from bus events (the window is display-only, ADR 0013). `seq` increases monotonically for the daemon's lifetime and never repeats, so a client can merge this snapshot with `activity.row` pushes without duplicates; `limit` is the live `ui.activity_rows` bound. In-memory only — the ring dies with the daemon; conversations are the durable record. This is also the reconciliation path: pushes may be dropped for a slow client, the snapshot is what the daemon actually holds. Privacy contracts hold in every row: counts for memory and desktop context (ADR 0019/0025), lengths for typed text and search queries (ADR 0023/0028), per-tool argument summaries that default to nothing for unknown tools |
| `routines.list` | — | `{routines}` | The configured routines (ADR 0026), read-only: each entry is `{name, phrases, steps, incomplete}` where `steps` is a count and `incomplete` marks a captured routine (#62) still carrying a launch-command placeholder a human must resolve. The steps themselves are hand-edited TOML (`[[routines]]` in config.toml) and are not exposed or editable over IPC in v1 |
| `routines.run` | `{name}` | `{session_id}` | Runs one routine by name (case-insensitive) by starting a session and submitting the routine's first trigger phrase — exactly as if it had been spoken, so the intent router, the `routine.run` permission gate, the already-running refusal, and the spoken summary behave identically however a routine is triggered. An unknown name is `-32602` with a pointer to `routines.list` |
| `doctor.get` | — | `{checks}` | The settings-relevant readiness subset of `jarvix doctor` — offline and fast (no provider probe, no audio round trips). Each check: `status` (`ok`/`warn`/`fail`), `name`, `detail`, `fix`, and `related` — the config key the check informs, so a settings screen can show readiness inline |

Errors use JSON-RPC error objects. Application errors (wrong state, no active
session) use code `-32000`; standard codes cover parse/method/params issues.
The config surface adds three codes, each with structured `data`:

| Code | Meaning | `data` |
|---|---|---|
| `-32001` | config.set/reload rejected by validation; nothing written/applied | `{problems: [...]}` — `config.Validate` messages, each prefixed with the offending key |
| `-32002` | config.toml changed on disk since the client's `config.get`; nothing written | `{fingerprint}` — the file as it is now; re-read and reapply |
| `-32003` | config.reload could not apply because a session is active; running config unchanged | — |

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
| `context.captured` | `{sources, duration_ms}` | Desktop context was gathered for this turn (ADR 0019), immediately before the provider request. `sources` is an array of `{source, chars, truncated, redacted}` — **sizes and flags only, never content**, because events fan out to every connected client. Published only on turns that reach the model: a matched deterministic intent gathers nothing. Fetch the captured text itself with `context.last` |
| `memory.injected` | `{facts, trimmed, total, est_tokens}` | Remembered facts were offered to the model for this turn (ADR 0025), immediately before the provider request. **Counts and estimates only, never content** — fact text is fetched with `memory.last`, over the socket, on request. Published only on turns that reach the model, and only with memory enabled |
| `assistant.started` | `{provider}` | Provider request opened |
| `assistant.delta` | `{content}` | One streamed response fragment |
| `assistant.finished` | `{content, tool_calls}` | Full response text. `tool_calls` counts the tool calls the model requested this turn — attempts, including denied and declined ones. Zero on an answer that claims action is the model narrating work it never asked to do, which is exactly what the activity feed's text-only marker makes visible |
| `tool.started` / `tool.finished` | `{tool, arguments, detail?}` / `{tool, duration_ms, outcome}` | Bounds of one real tool execution — never published for denied or declined calls. `detail` is present only for tools that can run long (an advisor consultation) and is a short label to show for the duration, e.g. `Consulting claude…`. `outcome` is `ok`, or `error` for the registry's own failures (unknown tool, infrastructure err) — the one failure shape this layer can attest to; a tool that ran and refused reports its reason in its own audit events (`typing.audit`, `desktop.refusal`) |
| `tool.progress` | `{tool, message, elapsed_sec}` | A tool has been running for `elapsed_sec` seconds and is still going. Published **at most once** per call, and only for tools that carry a `detail` label; `message` is also spoken aloud. Reassurance, not a countdown — clients should show it and expect nothing further until `tool.finished` |
| `tool.confirmation_required` | `{tool, command, summary, rule, timeout_sec}` | The permission gate paused a tool call (state `awaiting_confirmation`). `command` is the exact command, verbatim; `summary` is the spoken question, generated daemon-side from the command; the overlay should display `command`. For `advisor.ask`, `command` is the advisor's name — what the user is actually approving, and what a remembered approval is keyed on |
| `tool.confirmed` | `{tool, command, source}` | The user approved; the call executes. `source`: `cli`, `text`, or `voice` |
| `tool.declined` | `{tool, command, source}` | The call will not run. `source`: `cli`, `text`, `voice`, `timeout`, `interrupted`, or `error` |
| `tool.denied` | `{tool, command, rule}` | A deny rule blocked the call outright; no confirmation is possible |
| `typing.audit` | `{tool, window, chars, approved, terminal, outcome, key?, reason?}` | Jarvix made a typing decision (ADR 0023). `outcome` is `typed`, `pressed`, `refused`, `focus-changed`, or `unavailable`; `approved` says whether a human confirmed this exact action rather than the configured tier allowing it; `terminal` marks a target whose contents are a command line. **The typed text is never published**: the payload may be a password the user dictated, and a bus event reaches every subscriber. `chars` is how many characters, never which. The daemon retains the most recent one for `jarvix status --last` |
| `desktop.action` | `{verb, target}` | Jarvix acted on the desktop (ADR 0022). `verb` is `focus`, `move`, `close`, `launch`, or `list`; `target` names the window as a person would (`firefox — GitHub`) or the application started. Published after the action succeeded, so it is a record of what happened, not what was attempted. Window addresses are never published: they are compositor internals and nothing spoken or shown needs them |
| `desktop.refusal` | `{verb, target, reason}` | Jarvix tried to act on the desktop and could not (issue #70): a launch the resolver refused (`launch` / `firefox` / `it is not installed`), a launch that failed to start, a dispatch the compositor rejected. `reason` is composed to be safe on the bus — the resolver's own spoken-form sentence, or a generic clause where the underlying error is operator material (launcher errors, compositor diagnostics, which stay in the journal). Window addresses never travel, same as `desktop.action` |
| `activity.row` | `{seq, ts, session_id, kind, label, detail, failed}` | One rendered activity-feed row, pushed as it is appended to the ring — the live half of `activity.get`, same shape, merged by `seq`. Derived by the daemon's own feed subscriber from the event that caused it, so it may trail that event on the wire (see ordering below). Clients render it verbatim; the wording is decided and tested daemon-side |
| `intent.executed` | `{intent, source, status, acknowledgement, duration_ms, slot?, routine?, error?}` | The deterministic intent router handled the utterance locally, with no provider request (ADR 0017). `intent` is the id (`volume.set`, `speech.stop`, `custom.lock the screen`); `source` is `builtin`, `user`, or `routine` (with `routine` naming which — ADR 0026; a layout capture, #62, reports `intent: routine.capture` with `routine` naming the saved entry); `status` is `ok` or `failed` (with `error` when the command failed, was declined, or was denied); `slot` is the validated integer for intents that take one; `duration_ms` is measured from the final transcript. `acknowledgement` is what Jarvix says — flash it in the overlay; it is empty for `speech.stop`, where silence is the confirmation |
| `routine.started` | `{routine, steps}` | A routine began executing (ADR 0026): its name and how many steps it will run |
| `routine.step` | `{routine, step, app, workspace, launched, status, detail?}` | One step finished: `step` counts from 1, `launched` distinguishes a fresh launch from a deduped existing window, `status` is `placed` or `failed` (with `detail` naming why). Progress for the bar and the window — the user only ever *hears* the final summary |
| `routine.finished` | `{routine, placed, failed, failures, summary}` | The routine completed: how many steps landed, the failure clauses, and the summary sentence that was spoken. Not published for a cancelled run — `session.cancelled` owns that ending |
| `wake.detected` | `{confidence}` | The wake word was recognised and a session is starting (ADR 0024). Published **before** transcription, so the overlay can acknowledge the wake word while whisper is still working. It carries a confidence and nothing else — never audio, never text: events fan out to every connected client, and pre-wake audio does not leave the daemon |
| `wake.changed` | `{state}` | What the microphone indicator should show: `off` (nothing is capturing), `armed` (a capture process is running), or `muted` (`jarvix mute` is in force). A second dimension alongside `state.changed` rather than another session state — a session state describes a turn, this describes the microphone between turns |
| `tts.started` / `tts.finished` | `{}`; finished may carry `{interrupted:true}` | Speech bounds |
| `session.timings` | `{capture_to_transcript_ms?, context_ms?, transcript_to_first_delta_ms?, first_delta_to_first_pcm_ms?, first_pcm_to_audio_out_ms?, tool_ms?, confirm_wait_ms?, release_to_first_audio_ms?, jarvix_ms?}` | The session's latency budget, published immediately before `session.finished` (ADR 0018). Each key is a completed stage in milliseconds; **a stage that did not happen is absent, not zero** — a typed question has no capture stage, a silent answer has no audio stages, and a turn with desktop context disabled has no `context_ms`. `context_ms` is desktop-context gathering (ADR 0019), which sits between the transcript and the request: the model's clock starts where it stops, so gathering is counted in `jarvix_ms` rather than excused as thinking time. `transcript_to_first_delta_ms` is the model's time to its first output — a text token or a tool call — which is the user's choice of model, not Jarvix's latency. `tool_ms` (time inside tool executions) and `confirm_wait_ms` (time waiting on the user's confirmation answer) are the turn's **excluded spans** (issue #72): reported so the turn's real length is on the record, attributed to neither Jarvix nor the model, and subtracted from any pipeline stage they fell inside. `jarvix_ms` is `release_to_first_audio_ms` minus thinking minus the excluded time before the first sound — the number Jarvix is accountable for, **non-negative by construction** (every stage is; a span whose accounting cannot be reconciled is dropped rather than published negative). Also emitted as one structured log line with identical keys |
| `session.finished` | `{}` | Session completed (also after an error) |
| `session.cancelled` | `{reason}` | Session cancelled or interrupted |
| `error` | `{stage, message}` | A stage failed: `audio`, `stt`, `assistant`, `tts`, `session` |
| `config.changed` | `{fingerprint}` | Configuration was saved (`config.set`) or reloaded; open settings views should refresh via `config.get` |
| `conversation.changed` | `{reason, conversation_id?}` | The live thread changed underneath open views: `reset` (`jarvix new`, a delete of the active conversation) or `opened` (an archived conversation was reopened, ADR 0027). Re-request `conversation.get` rather than guessing what the change meant |

States seen in `state.changed`: `idle`, `listening`, `transcribing`,
`thinking`, `responding`, `awaiting_confirmation`, `acting`, `speaking`,
`cancelling`, `error`. `acting` is a matched deterministic intent — it never
passes through `thinking`/`responding`, because no provider request is open.

Ordering guarantees: `state.changed` precedes the stage events it enables;
`session.timings` immediately precedes `session.finished`; and
`session.finished`/`session.cancelled` is always the last *engine* event of a
session. `activity.row` is exempt from that last clause: it is derived — the
daemon's feed subscriber rendering engine events it has just received — so a
row for a session's final events can arrive shortly after `session.finished`.
Rows do preserve their own order (`seq`), and a client that treats
`session.finished` as terminal loses nothing: `activity.get` reconciles.
Slow consumers may lose events (the daemon never blocks on a client), so the
overlay treats `state.changed` as the source of truth and resyncs with
`status.get` on reconnect.

## The shell plugin's own IPC surface

Separate from the daemon socket above: the Omarchy shell exposes each
plugin's `IpcHandler` through `omarchy-shell <target> <function>`. Jarvix
registers two targets, and this is how the CLI, notifications, and the bar
widget reach the plugin's windows without the daemon being involved — which
matters, because the window and the widget must work with jarvixd stopped.

| Target | Function | Effect |
|---|---|---|
| `jarvix` | `openWindow` / `closeWindow` / `toggleWindow` | Show, hide, or toggle the conversation window. `jarvix window`, a clicked notification, and the bar widget all go through here — there is only ever one window |
| `jarvix` | `openSettings` | Open the window already showing the settings screen (the bar widget's Settings action) |
| `jarvix` | `state` / `ping` | The overlay's view of the session state; liveness |
| `jarvix.bar` | `open` / `close` / `toggle` / `show` / `hide` | The bar widget's panel |
| `jarvix.bar` | `state` | The state key the bar icon is showing — one of the daemon states above, or `not-running`, `error`, or `working` (an unrecognised state), or `wake-armed` / `wake-muted` when the daemon is idle and background listening is on. `scripts/verify-bar-widget.sh` reads this to check the icon against the daemon |

The widget's own vocabulary — the glyph, words, and urgency for each state —
is defined in `internal/desktop/barstatus.go` and generated into
`plugin/omarchy/BarState.js`; see [ADR 0020](adr/0020-bar-widget-not-tray-icon.md).
The same arrangement covers the activity feed: row wording lives in
`internal/desktop/activity.go` (rows arrive from the daemon already
rendered), and the window's glyph-per-kind table is generated into
`plugin/omarchy/ActivityState.js`.
