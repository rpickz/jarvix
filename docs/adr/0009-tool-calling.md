# ADR 0009 — Tool calling: shell.run and the Thinking loop

**Status:** accepted (implements the ADR 0006 boundary)

## Context

Jarvix is meant to *do* things, not just answer — "what's happening in
docker?" should run `docker ps` and summarise it. ADR 0006 sketched the
`Tool` interface and the provider/engine attachment points; this ADR makes
them real for V1.

## Decision

- **Interface (`internal/tools`):** `Tool{Name, Description, Schema, Execute}`
  and a `Registry`. `Execute` returns text for the model; a command that ran
  and failed reports that in the text, so the assistant can recover — only
  infrastructure failures return an error, and even those come back to the
  model as readable text rather than killing the session.
- **First tool: `shell.run`** — runs `bash -c <command>` in the user's home,
  non-interactive (stdin is /dev/null), with a timeout and an output cap.
  Stdout+stderr and exit status are returned so the model sees what really
  happened. It is opt-in (`[tools] shell = true`).
- **Provider (`openaicompat`):** requests carry `tools`; streamed
  `tool_calls` fragments are stitched by index and emitted as
  `ai.EventToolCall` after the text stream ends. Providers without tool
  support ignore the field.
- **Engine loop:** `think` runs up to `maxToolRounds` (6) provider turns.
  Each turn either produces a final answer (→ speak) or tool calls (→ execute
  each under the session context, append results as `role:"tool"` messages,
  loop). Tool execution stays in `Thinking`; if the model streamed text
  before calling a tool, the state drops `Responding → Thinking` so the
  overlay does not read "responding" through a tool pause. New events:
  `tool.started` / `tool.finished`.
- **Cancellation:** tools run under `s.ctx`, so Escape / `session.cancel` /
  interruption kills an in-flight command immediately — the same guarantee
  as every other stage.

## Security

`shell.run` is arbitrary code execution with the user's privileges — the
capability that makes Jarvix useful and the one to be careful with:

- Opt-in, off by default. Enabling it is a deliberate config edit.
- Every command is logged with its text and duration.
- The system prompt instructs the model to prefer read-only commands and to
  confirm before destructive actions, but this is model behaviour, not
  enforcement.
- ~~**Not yet built:** a real permission gate (allow / ask / deny per command
  pattern, with spoken confirmation for the "ask" tier). That is the next
  tools milestone; until then `shell` is a single switch and should be
  enabled only with a model and setup you trust.~~ **Now built** — see
  [ADR 0014](0014-tool-permission-gate.md).

## Consequences

- Any OpenAI-compatible tool-capable model works (tested: qwen2.5:7b via
  Ollama). Non-tool models simply never call tools.
- The loop generalises to future tools (`desktop.*`, `clipboard.*`,
  `hyprland.*`) with no engine change — they just register.
- `maxToolRounds` bounds runaway loops; exceeding it fails the session
  cleanly rather than looping forever.
