# ADR 0001 — JSON-RPC 2.0 over a Unix domain socket

**Status:** accepted

## Context

The daemon needs a protocol serving two client kinds: the CLI (synchronous
commands) and the QML overlay (asynchronous event stream). The overlay side
is constrained: Quickshell offers `Socket` + line-based parsing in QML, so
anything requiring an HTTP client, binary framing, or code generation raises
the bar sharply.

## Decision

JSON-RPC 2.0, newline-delimited, over `$XDG_RUNTIME_DIR/jarvix.sock`
(mode 0600). Server-initiated events are JSON-RPC notifications pushed on the
same connection. No TCP listener.

## Alternatives

- **D-Bus** — native to the desktop, but Quickshell's QML D-Bus support is
  limited, the API surface is heavier, and testing is harder. Nothing here
  needs bus semantics.
- **gRPC** — code generation and HTTP/2 for two local clients is weight
  without benefit; unusable from QML.
- **Custom ad-hoc JSON** — what JSON-RPC gives over it (ids, error objects, a
  spec to point at) costs nothing.

## Consequences

- One `JSON.parse` per line is the entire client requirement — QML, `nc -U`,
  and Go all speak it trivially.
- Notifications share the command connection, so clients get events without a
  subscription handshake.
- The socket's 0600 mode is the security boundary (same-user only), which is
  correct for a per-user daemon.
