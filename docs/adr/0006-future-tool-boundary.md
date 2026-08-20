# ADR 0006 — The future tool boundary (documented now, built later)

**Status:** accepted (boundary only; no implementation in V1)

## Context

Tools (Phase 4) will let the assistant act on the desktop. Building them now
would violate the brief's do-not-overbuild rule, but the boundary must be
chosen while the session engine is young so tools can attach without a
rewrite.

## Decision

Tools will be a daemon-side registry behind this interface:

```go
type Tool interface {
    Name() string        // e.g. "system.volume"
    Description() string // model-facing description
    Schema() ToolSchema  // JSON schema for input

    Execute(ctx context.Context, input json.RawMessage) (ToolResult, error)
}
```

Attachment points already in place:

- **Provider seam** — `ai.ChatRequest`/`ai.Event` will grow tool-call
  events (`EventToolCall`) alongside `delta`; `openaicompat` maps them from
  the standard `tool_calls` streaming format. Providers without tool support
  simply never emit them.
- **Session engine** — `Thinking` gains a loop: tool-call event → execute
  under the session context → append result → continue the stream. States and
  cancellation semantics are unchanged; a cancelled session kills in-flight
  tool executions through the same context.
- **Permissions** — each tool declares a capability tag; the config gains a
  `[permissions]` table mapping tags to allow/ask/deny. "Ask" is a spoken +
  overlay confirmation. No tool executes outside this gate.

Planned first tools: `desktop.get_active_window`, `desktop.get_selected_text`,
`clipboard.read/write`, `apps.launch`, `system.volume`, `hyprland.workspace`.
`shell.run` comes last, default-deny.

## Consequences

- Nothing in V1 code references tools; the cost today is only that the
  provider event type is extensible (it is: `ai.EventType` is a string enum).
- When tools land, the IPC surface adds `tool.started`/`tool.finished` events
  for the overlay; the protocol's additive-change rule (ipc.md) covers this
  without a version bump.
