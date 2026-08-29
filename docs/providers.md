# Provider abstractions

Jarvix's three engine seams are Go interfaces with the same shape: a request
goes in with a `context.Context`, a stream of events comes out on a channel,
the channel closes when the stream ends, and cancelling the context stops the
work promptly. The rest of the system depends only on these interfaces —
never on a concrete engine.

## AI (`internal/ai`)

```go
type Provider interface {
    Name() string
    Chat(ctx context.Context, req ChatRequest) (<-chan Event, error)
}
// Event.Type ∈ {delta, done, error}
```

Errors detected before streaming (bad config, connection refused) come back
from `Chat` itself; mid-stream failures arrive as an `error` event.

### Implemented: `openaicompat`

One implementation covers every OpenAI-compatible chat-completions endpoint —
the endpoint is configuration, not code:

| Configured provider | Endpoint |
|---|---|
| `ollama` (default) | `http://127.0.0.1:11434/v1`, no key |
| `openai` | `https://api.openai.com/v1`, key from `OPENAI_API_KEY` |
| `openrouter` | `https://openrouter.ai/api/v1`, key from `OPENROUTER_API_KEY` |
| `lmstudio` | `http://127.0.0.1:1234/v1`, no key |
| any `[ai.<name>]` table | your `base_url` + `api_key_env` |

It speaks SSE (`stream: true`), forwards `choices[].delta.content` fragments
as `delta` events, treats `data: [DONE]` or EOF as completion, and surfaces
API error payloads as readable messages.

`ProbeEndpoint()` makes the cheapest request that proves both halves of an
endpoint — `GET /models`, which costs no tokens and answers 401 rather than 200
for a wrong key — and classifies the result as **reachable** (a 2xx that
actually arrived), **unauthorised** (401/402/403: the URL is right, the key is
not), or **unreachable** (a refused dial, a timeout, or a status meaning this
is not an API root). `Probe()` is the boolean form `jarvix doctor` uses; the
window's Providers section (#163) uses the classified one behind
`config.test_entry`, so a mistyped base URL fails in the form rather than
mid-conversation. Neither ever reports success it did not observe, and neither
puts the credential in an error: the response body is read, the request is not
echoed.

Endpoints are administered in the window's Providers section — base URL and
credential, with `api_key_env` offered as the safer choice. A stored key is
never displayed, returned, or quoted; see ADR 0052.

### Model tiers

`[ai.tiers]` (issue #159, ADR 0063) lets one Jarvix hold three brains at once —
`instant`, `medium`, `deep` — each *pointing at* one of the endpoints above, or
at an advisor CLI through the bridge of ADR 0016. A tier never carries a base
URL or a credential of its own: there is one place an endpoint is described,
and a tier selects it.

Which tier serves a turn is decided by `ai.Decide`, a pure table in this
package with no engine in it: the configured default, overridden by the
conversation's level, overridden by an explicit ask, and then two corrections —
a tier with no binding cannot serve (and the refusal is *said*), and a turn that
may call a tool is never served by the instant tier. There is no classifier and
no pre-flight model call; routing costs a map lookup.

With no `[ai.tiers]` table nothing changes: one provider, one model, the same
request bytes on the wire.

`AdvisorProvider` (internal/tools) is the second implementation of `Provider`
in the tree, and it is not an HTTP one: it presents a configured assistant CLI
as a provider so a tier can be either without any call site knowing.

### Planned

Native Anthropic and Gemini providers (different wire formats) are the
motivating second implementations; OpenRouter/Ollama already work through
`openaicompat`. See docs/implementation-plan.md.

## Speech-to-text (`internal/stt`)

```go
type Transcriber interface {
    Name() string
    Transcribe(ctx context.Context, input AudioInput) (<-chan TranscriptEvent, error)
}
// TranscriptEvent.Type ∈ {partial, final, error}
```

`AudioInput` is a WAV file path in V1 (recordings live on tmpfs). The event
vocabulary already includes `partial` so a future streaming engine
(faster-whisper server, Deepgram, OpenAI transcription) can slot in without
interface changes.

### Implemented: `whispercpp`

Runs `whisper-cli` as a short-lived subprocess per utterance
(`--no-timestamps --no-prints`), emits one `final` event. The model path
resolves from a short name (`base.en`) into
`~/.local/share/jarvix/models/whisper/`; `jarvix setup whisper` downloads
models. Cancellation kills the process.

## Text-to-speech (`internal/tts`)

```go
type Synthesizer interface {
    Name() string
    Speak(ctx context.Context, req Request) (Format, <-chan Chunk, error)
}
```

The synthesizer streams raw s16le PCM chunks and announces the format
up-front, so playback begins while synthesis is still running — and so a
future implementation can speak sentence fragments while the assistant is
still streaming text (the interface already permits it; the engine currently
speaks once the full response has arrived).

### Implemented: `piper`

Runs `piper-tts --output_raw` per utterance, resolving voice names against
`/usr/share/piper-voices` and reading the sample rate from the voice's
`.onnx.json`. Cancellation kills the process — that is what makes
interrupting Jarvix mid-sentence instant.

### Planned

Kokoro (the spec's preferred voice) behind the same interface, likely as a
managed local server process; OpenAI/ElevenLabs TTS as cloud options.

## Fakes

Every interface ships a scripted fake (`ai.Fake`, `stt.Fake`, `tts.Fake`,
`audio.FakeRecorder`, `audio.FakePlayer`). The integration tests run the
complete lifecycle — fake speech → fake transcript → fake AI stream → fake
TTS — through the real engine, real state machine, and real IPC socket, so
`make test` needs no hardware, daemons, or API keys.
