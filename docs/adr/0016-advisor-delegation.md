# ADR 0016 — Advisor delegation: escalating a question to a stronger assistant CLI

**Status:** accepted

## Context

Jarvix runs a small local model (`llama3.2:3b` by default). That is the right
choice for instant conversational turns and the wrong one for "review this
architecture", "plan my week around these constraints", or anything needing
deep reasoning or current knowledge. The machine usually already has a
stronger assistant installed and authenticated as a CLI — Claude Code, Codex,
Gemini — each carrying its own auth and billing.

Jarvix's role is personal assistant, not expert. When a request exceeds the
local model it should delegate, the way an assistant calls a specialist,
instead of producing a confidently shallow answer. `jarvix setup` already
detects installed assistant CLIs and records them as `[advisors.<name>]`
tables (PR #20); this ADR is what happens when one is used.

## Decision

**Delegation is a tool, `advisor.ask`, with two model-supplied values:
*which* advisor and *what to ask*.** Everything else — binary, flags, their
order, environment, timeout — comes from configuration the model cannot
write. There is no `sh -c` anywhere on the path: the binary and every
argument go straight to `execve`, so a question containing `;`, `$(…)`, or
backticks is inert text. The question travels either on the child's stdin
(the default) or as the single argv element written `{question}` in the
template; the placeholder is matched whole-element, so a question can never
grow a flag. Config validation rejects an embedded placeholder
(`--ask={question}`) outright.

**Config shape is the wizard's shape.** A table whose only key is
`binary` — all `jarvix setup` writes — is a complete, working advisor: the
shipped preset for that name supplies the non-interactive argv, a
description for the tool schema, and a 120-second timeout. `args`,
`timeout_sec`, and `description` override it, and an advisor with no preset
(a local script) works by giving it an argv.

**Permission tier: allow for advisors that only answer, ask for everything
else** (the gate from ADR 0014). Consulting a one-shot print/exec-mode CLI
reads and replies — no more authority than the model turn Jarvix already
makes — so it runs silently, keeping the feature usable by voice. Two cases
escalate to ask:

- the advisor's CLI can act on the machine (`aider`, `goose`, `opencode` edit
  files and run commands; their presets are never marked read-only), and
- the advisor's argv came from configuration rather than a shipped preset —
  Jarvix has not audited it, so it cannot claim the CLI only answers.

Classification is per advisor and comes from config, never from the call: the
model names an advisor, and that name's configured tier applies.
`[tools.policy.tool]` still overrides wholesale — `"allow"` trusts every
advisor, `"deny"` disables delegation. The confirmation summary and the
remembered approval are keyed on the **advisor**, not the question, so
"yes, ask Claude" cannot approve asking something else.

**Local-first is a prompt rule, not a pattern.** No classifier can separate
"what time is it?" from "review this architecture" — the model can, so the
tool description and the appended system prompt say it plainly, including the
cost: up to two minutes of silence. The reliable half is the enum: the model
can only name a configured advisor.

**Slow work announces itself.** A voice interface has no spinner. Tools may
implement `tools.Progressive`, returning a label and a waiting sentence; the
engine puts the label on `tool.started` (`detail`) for the overlay to show
for the duration, and after 10 seconds publishes `tool.progress` **once** and
speaks the sentence. Once, not a countdown — the point is reassurance.

**Failure is one spoken sentence.** A missing CLI, a non-zero exit, a
timeout, or an empty answer comes back as a tool result telling the model to
say one short sentence and not retry. The advisor's stderr never leaves the
daemon log: anything returned to the model may be read aloud, and a stack
trace full of paths is the worst possible thing to hear.

**Bounded and killable.** Each consultation runs under the session context
with a hard timeout, in its own process group, killed as a group with
SIGKILL — assistant CLIs spawn helpers, and killing only the parent would
leave a language model running against the user's account after they said
"stop". Output is capped at 64 KB with a truncation note. The environment is
scrubbed: anything named like a credential (`*_API_KEY`, `*_TOKEN`,
`*SECRET*`, `*PASSWORD*`, …) plus every `api_key_env` Jarvix itself reads is
withheld. Advisors carry their own authentication; Jarvix's keys are not
theirs to spend.

## Consequences

- One voice interface fronts every AI capability on the machine, with each
  CLI's auth and billing untouched. This is also the bridge to voice-driven
  productive work: heavyweight generation happens in the delegate.
- The user pays the delegate's bill by talking to Jarvix. The default tier
  keeps that silent for read-only advisors — a deliberate trade of one
  spoken question for a usable feature, reversible with
  `[tools.policy.tool] "advisor.ask" = "ask"`.
- Presets encode CLI flags that will move. A stale preset fails as a spoken
  "I couldn't get an answer" and is fixed by setting `args` — which then
  demotes that advisor to ask, correctly, since Jarvix cannot vouch for it.
- Capture-then-speak, not streaming: the answer arrives whole, up to two
  minutes later. Streaming the delegate's own tokens is future work
  (out of scope for this ticket).
- The progress line is spoken outside the streaming speaker, as confirmation
  questions already are. If the assistant happens to still be mid-sentence
  when it fires, the two streams overlap; the window is small (ten seconds of
  tool execution) and closing it means teaching the speaker about
  out-of-band sentences.
- `jarvix doctor` reports each configured advisor's presence with
  `exec.LookPath` only — no network, no invocation, no spend.
