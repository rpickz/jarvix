# ADR 0030 — Voice-triggered scripts: your executable behind a phrase, behind the gate

**Status:** accepted (implements issue #85; the deliberate revisit of ADR
0026's arbitrary-command exclusion)

## Threat model — first, because it is the design's spine

ADR 0026 refused arbitrary-command steps in routines with one sentence: "a
shell behind a single spoken phrase" needed its own threat model, and would
be excluded until it had one. The user now asks for exactly that capability
— *"scripts should be able to be configured which Jarvix can launch for
frequently performed operations"* — so this ADR starts where that sentence
stopped. Three paths could make a script run something the user did not
mean. Each gets a named control; everything else in the design follows from
this table.

| Attack path | What could happen | The control that answers it |
| --- | --- | --- |
| **A misheard phrase** (or an ambient voice, a TV, a guest). STT produces "backup my notes" when nobody meant it. | The script runs on a phantom instruction. | **The gate's ask default.** `script.run` asks before every run unless the user has explicitly written `"script.run" = "allow"` — and a global `default = "allow"` deliberately does *not* reach it, so silence has to be chosen per machine, in a sentence the user has to mean. The confirmation is generated daemon-side (ADR 0014) and must itself be answered; a second phantom "yes" is a far taller order than one phantom phrase. A global `default = "deny"` denies scripts too: the exception runs one way, tightening always wins. |
| **A malicious or careless config edit.** Something that can write config.toml adds a `[[scripts]]` entry (or repoints an existing one) at a hostile executable. | A phrase now runs the attacker's file. | **Stated honestly: writing config.toml is already the authority to make Jarvix run things** — `[[intents.custom]]` has run arbitrary shell commands behind phrases since ADR 0017, so this path adds no authority that did not exist. What this ADR adds is *visibility*: the ask confirmation and every listing surface (`jarvix scripts`, `scripts.list`, the window panel, the activity feed's started row) name the absolute path, a new entry is a startup journal line, and a repointed path invalidates any remembered approval, because the approval key carries name *and* path. IPC cannot write the tables at all — `scripts.list` is read-only by design, so no connected client can repoint a phrase. |
| **A compromised or substituted script file.** The config is honest but the file at the path changed — replaced, or a relative name resolving to whatever shadows it on PATH. | The phrase runs different code than the user wrote. | **Absolute paths only, validated and then re-checked.** Config validation (and doctor) refuse a relative, missing, or non-executable path before any phrase is ever spoken — a bare name resolved on PATH would let the shadowing file own the phrase, so the syntax is rejected outright. The runner re-stats at run time, and the ask confirmation names the exact path, so a substitution at the file level is at least visible as *which file* is about to run. What the file's own bytes do is, unavoidably, the user's trust in their own script — the same trust running it by hand expresses. The activity feed's per-run row (path, exit status, duration) is the after-the-fact audit. |
| **The transcript itself as input.** Anything spoken flowing into the child's argv or environment — the injection class ADR 0017 exists to prevent. | Speech becomes syntax. | **Zero arguments, by construction, stated in docs.** The exec call names the path and nothing else; there is no argv slice, no template, no env addition anywhere in the runner, and the config schema has no field arguments could be written into. Phrases with placeholders are refused at load. The property cannot be weakened by a filter change because there is no filter — there is no code path. Spoken slots are future work with their own validation design. |

Residual risk, stated plainly: a user who writes `"script.run" = "allow"` on
a machine where others speak has chosen to accept the misheard-phrase path;
the docs say so next to the setting. And Jarvix cannot audit what a script
does once running — it can only bound it (timeout, process-group kill,
output caps, scrubbed environment) and record it (activity feed, journal).

## Context

Every mechanical piece already exists: deterministic phrase triggering (ADR
0017), the permission gate with per-identity tiers (ADR 0014), subprocess
discipline proven on the advisor path (ADR 0016 — fixed argv, no shell,
scrubbed env, process-group kill, capped output), tracked post-session
goroutines that drain on shutdown (#74), and the activity feed (ADR 0029).
What does not exist is the composition: configuration that names an
executable and its phrases, an identity for the gate, and rules for what a
run says.

## Decision

**A script is configuration: `[[scripts]]` tables of name, trigger phrases,
absolute path, timeout, and report mode — and nothing else.** No args field,
no env field, no shell string. v1 scripts take zero arguments (see the
threat model's fourth row); a script needing variants gets one entry per
variant. New package `internal/script` owns definitions, validation, and the
runner.

**Triggering is the intent router, and scripts are the fourth phrase
family.** Script phrases compile into the same grammar table as built-ins,
custom intents, and routines; matching is whole-utterance and literal, costs
zero provider calls, and a collision with any family — including another
script — fails configuration validation naming both owners. A match carries
only the script's *name* into the engine: not the path, not a payload —
nothing an argument could ride in.

**Gate identity `script.run`, default ask, confirmation names name + path.**
Its own identity — not `intent.run`'s, not `routine.run`'s — because each
risk profile must be tightenable alone. The tier resolution is deliberately
asymmetric: an explicit `[tools.policy.tool]` entry wins; otherwise a global
`default = "deny"` denies, and *everything else asks* — a global allow does
not reach scripts. Approvals are rememberable within a conversation, keyed
on name and path together.

**Execution reuses the advisor path's discipline wholesale.** The child is
`exec`'d directly (no shell) with an empty stdin, the daemon's environment
minus credential-shaped variables (the one shared scrub list,
`tools.ScrubbedEnv`), its own process group killed as a group on timeout or
cancellation, `stdout`/`stderr` capped, and a bounded `Wait`. The run holds
the session context, so "stop" aborts it, and it executes inside the
engine's tracked intent goroutine, so shutdown drains it (#74). One script
at a time across all scripts: the runner cannot know which pairs share
state, so none interleave; a phrase during a run is refused with one line.

**Report modes govern success; failure is always spoken.** `summary` speaks
"Backup notes finished."; `stdout` speaks the capped first line the script
printed (speech-normalised by the speaker like everything else); `silent`
speaks nothing. Failure — non-zero exit (with the code and stderr's first
line), timeout, a file that would not start — travels as an *error*, which
the engine speaks in every mode: the report mode shapes only the success
string, so no configuration can make breakage inaudible.

**Every run is on the record.** `script.started` (name, path) and
`script.finished` (status, exit code, duration — success included) go out on
the bus and become activity-feed rows; the journal carries the same. Output
appears in no event, no log, and no row — it can contain anything the script
read.

**Validation is load-time and filesystem-honest.** Name unique
(case-insensitive), phrases present and literal, path absolute + present +
executable + a regular file, report mode known, timeout within 1s–1h. The
file checks run at config load *and* in doctor (one result per script) *and*
again at run time. A deleted script file is therefore a startup error — the
same stance as a misconfigured routine: config describes this machine, and
an entry promising a phrase the machine cannot honour is wrong, not latent.

**One trigger path.** `jarvix scripts run` and the window's Run button call
`scripts.run`, which starts a session and submits the script's first phrase
— router, gate, refusal, and outcome identical to speech. Listing is
read-only everywhere (`jarvix scripts`, `scripts.list`, the panel), always
showing the path.

**Scripts and routines stay distinct.** A routine step launches and places
applications and can never be a command (ADR 0026's exclusion stands
unchanged *inside* routines); a script runs a command and never places
windows. No script steps in routines, no placement fields in scripts. The
two compose at the phrase level only: say one, then the other.

## Consequences

- "Can Jarvix do X?" now has a standing answer — write a script and give it
  a phrase — which relieves pressure to grow bespoke features for every
  operation.
- The shipped experience interrupts: every script run asks until the user
  promotes `script.run`. That is the correct default for an arbitrary
  executable behind a microphone, and the promotion is one documented line.
- A missing script file blocks daemon startup (validation is load-time).
  The trade was made knowingly — "config validation says so before any
  phrase is ever spoken" is the acceptance criterion — and doctor names the
  entry and the fix; scripts on removable media will surface it.
- Zero arguments means some scripts need wrapper variants ("backup notes
  weekly" as its own entry). Spoken slots, if they ever come, get their own
  ADR with their own injection analysis.
- The engine grows a fourth intent family and a second runner interface;
  both mirror the routine shapes exactly, so the next family (if any) has
  two precedents that agree.
- `tools.ScrubbedEnv` is now shared surface between advisors and scripts —
  one list of secret-name rules to maintain, which is the point.
