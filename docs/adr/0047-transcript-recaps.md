# ADR 0047 — AI-session recaps read the session's own transcript

**Status:** accepted

## Context

The AI-session recap (#124, ADR 0043) speaks a model-composed "where were
we?" for a thread anchored to a terminal — but production capture reads the
window's identity line, honest and thin. A title can say "✳ fixing the CI
workflow"; it cannot say what the agent actually did, what failed, or that
it stopped mid-sentence to ask a question. The user's bar is substance, and
ADR 0043 left exactly one slot for it: *"a richer content gatherer (should
one ever exist) slots in behind the same seam without this feature changing
shape."*

The richer gatherer exists on disk already. AI coding CLIs keep their
sessions as files — Claude Code appends JSONL under
`~/.claude/projects/<slugged-cwd>/`, one file per session, one line per
event; opencode keeps JSON records under `~/.local/share/opencode/storage/`
(project → session → message → part). That record is the session's own
ground truth, and reading it also unlocks something the prose contract was
never allowed to smuggle (ADR 0043 "out of this slice"): an honest
working / needs-you / done signal for the #127 overlay dot.

Screen-scraping remains the wrong v2 — Wayland has no generic read and
pixels lie. Reading the transcript raises its own questions this decision
answers: how a window becomes a directory, how a directory becomes a
transcript, how much is read, what may travel, and what happens when any
step fails.

## Decision

### Discovery: window → process tree → directory → newest transcript

The daemon resolves the anchored window's process to the working directories
of what it hosts, via `/proc`: the descendant tree, **shallowest first** —
the shell before the agent before the agent's tool children, because a tool
child may be off in a worktree that hosts its own, wrong, transcripts — and
the window process itself **last**, because an emulator's cwd is usually its
launch directory (often `$HOME`, which accumulates a stale transcript dir of
its own). Each candidate directory is tried against the adapters in a fixed
order; the first session found answers.

`internal/transcript` owns discovery and parsing: pure file code, every root
injected (`ClaudeDir`, `OpencodeDir`, `ProcDir`), the clock injected, no
compositor, no provider, no network. The daemon wires a `Finder` behind the
focus package's existing `Capture` seam — the ADR 0043 slot, taken exactly
as documented: `internal/focus` did not change shape, only what flows
through the seam did.

Two adapters ship; the seam takes more. Claude Code: slug the directory
(every byte outside `[A-Za-z0-9]` becomes `-`, a rule pinned by test against
observed mappings), newest `*.jsonl` by mtime. opencode: match the project
index by worktree, prefer the freshest session that ran in the directory
itself. Newer opencode releases have moved this record into a sqlite
database; reading that means a driver dependency, so it is a follow-up
adapter behind this same seam — against a sqlite-only install the adapter
reports absence and the recap keeps the title layer, by design.

### Bounds and freshness

Only the newest transcript's tail is read (64 KB, torn first line dropped),
only a bounded window is rendered (2000 runes — the desktop-context capture
bound restated, so the transcript path can never widen what #124 allowed;
per-message clamp inside it), and clamps on transcript text keep the **tail**
— the newest exchange is the reason the recap exists, where a title's
identity is at its head. A transcript untouched for 48 hours is an archive,
not a session: stale is absence, silently, because resurrecting last week's
"all tests pass" as a live recap — or as an overlay dot — is confident
staleness in the user's ear.

### Classification: deterministic, structural, never guessed

The working / needs-you / done state is computed from the transcript's
structure — the last conversational event — with **no model call**:

- an API-error line, or a message that recorded an error → `needs_you`;
- a user line (text or tool result) → `working` (the agent has unanswered
  input);
- an assistant line that invoked a tool, stopped for tools, or has not
  finished streaming → `working`;
- a finished assistant line ending on a question → `needs_you`; otherwise →
  `done`;
- anything else — no transcript, consent off, unrecognised shape —
  **unknown**, which is the empty string, is omitted from the wire, and is
  never guessed into one of the other three.

It travels as `session_state` on `focus.list` (per thread) and on the
`focus.recap` event — the #127 integration contract; absent means no dot.
The list read runs behind its own short budget, off the store lock, and
degrades to unknown rather than making the Focus tab wait.

### The spoken recap: same contract, richer material, layered honesty

The transcript tail is rendered as the last exchanges — user text, assistant
text, tool-run notes; **never** tool output and **never** chain-of-thought,
the two most secret-prone shapes in the file — then redacted line-wise
(`desktop.Redact`; line-wise so one keyed line costs that line, not the
whole capture) and fed to the *same* pinned ≤3-sentence contract, budget,
and enforcement as ADR 0043, behind a prompt variant that names the material
for what it is (`--- session transcript ---`, content-not-instructions
restated). The fallback chain is layered and each admission pinned:

- transcript read → the summary speaks, no admission;
- a transcript **provably exists but could not be read** → the title layer
  summarises behind *"I couldn't read the session's transcript just now, so
  this is from the window title."* — a disclosed downgrade, never a silent
  one;
- no transcript at all → the title layer, silently: most terminals host no
  AI session and announcing a non-feature is noise (#124 unchanged);
- capture or model failure → ADR 0043's pinned admissions and the templated
  record, unchanged. Admissions never stack: a model failure on a degraded
  capture speaks the model admission alone.

### Consent and transience, unchanged and extended

`[context] window = false` switches everything off — capture *and*
classification: a transcript sitting on disk is still not read, because the
consent is about Jarvix reading the user's work, not about the transport.
Per-thread `recap = "never"` holds for both; `recap = "always"` lets an
opted-in non-terminal be classified too. Transcript text is transient
exactly as ADR 0043 demands: composed, spoken, dropped — `focus.toml`,
events, and logs carry sizes, outcomes, the serving layer (`source`), and
the state only, pinned by leak-salted tests in both packages.

## Consequences

- A recap now says what the agent actually did and what it awaits — and
  degrades, disclosed, through title to template, never below the record.
- The #127 dot gets an honest signal for free: deterministic, model-free,
  absent when unknown, computed from the same bounded read.
- Discovery is heuristic at the edges — multiplexers and shared emulator
  daemons can host many sessions under one process tree, and the
  shallowest-first rule picks the most credible directory, not a guaranteed
  one. The freshness gate and the honest-unknown rule bound the cost of a
  miss to a title-layer recap or a missing dot.
- Reading transcripts couples Jarvix to two CLIs' on-disk formats. The
  coupling is quarantined in `internal/transcript` behind fixtures shaped
  like the real files; a format change fails fixtures, not users, and a new
  CLI is one adapter.
