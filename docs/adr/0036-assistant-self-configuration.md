# ADR 0036 — Assistant self-configuration: thin tools over the admin verbs, a tier floor, and an exclusion wall

**Status:** accepted (implements issue #105 over ADR 0033's entry verbs and ADR 0015's settings registry)

## Context

Every piece of Jarvix's configuration is administrable through validated
machinery: the generic entry verbs (`config.get_entry` / `validate_entry` /
`upsert_entry` / `delete_entry`, ADR 0033) cover routines, scripts, and
knowledge feeds; the settings registry (`config.get` / `config.set`, ADR
0015) covers daemon settings; the window's forms and the CLI prove both. But
the assistant itself could use none of it — "remind yourself to check NVDA
every morning" or "talk a bit faster" dead-ended at "edit the config or open
the window".

Giving the model write access to the file that *configures the model's own
permissions* is the sharpest trust question this project has faced. Three
prior decisions frame the answer: the permission gate classifies daemon-side
from arguments, never from the model's narration (ADR 0014); `script.run`
established that a global `allow` must not reach arbitrary execution — only
naming the tool does (ADR 0030); and the honesty rules (issue #71) demand
that nothing is ever described as done unless it was.

## Decision

**Six thin tools over the existing verbs — zero new write paths.** The
family (`internal/tools/configadmin.go`) is registered unconditionally:

| Tool | What it does | Backed by |
|---|---|---|
| `config.list_entries` | List one family's entries | file read (same parser) |
| `config.get_entry` | Read one whole entry + remember it | file read (same parser) |
| `config.write_entry` | Create/replace one whole entry | `config.upsert_entry` handler, in-process |
| `config.delete_entry` | Remove one entry | `config.delete_entry` handler, in-process |
| `config.read_settings` | List the writable settings + values | settings registry (pruned view) |
| `config.write_setting` | Change one registry setting | `config.set` handler, in-process |

The daemon-side bridge (`internal/daemon/config_admin_tools.go`) invokes the
*same handler functions* the window's IPC registers, with only a `source`
label differing (`assistant` instead of `window`/`user`) — so fingerprint
guarding, key whitelists, whole-document validation, atomic writes, the
standard reload, and the events are one implementation with three clients
(loader, forms, assistant).

**The tier table.**

| Identity | Default | Global `allow` reaches it? | Explicit `[tools.policy.tool]` entry? | Global `deny`? |
|---|---|---|---|---|
| `config.list_entries`, `config.get_entry`, `config.read_settings` | allow (builtin: reads of the user's own config) | n/a | overridable | denies |
| `config.write_entry`, `config.delete_entry` | **ask** | **no** (script.run's floor) | allow/ask/deny all honoured | denies |
| `config.write_setting` — benign keys | ask (the normal unknown-tool default) | yes | honoured | denies |
| `config.write_setting` — **dangerous** keys | **ask, always** | **no** | **allow does not silence it** (escalation) | denies |

The write-entry floor is `script.run`'s rule restated at authoring time:
*everything* these verbs can write is command-bearing — a script **is** a
command, a knowledge feed carries the argv it fetches with, and a routine's
steps launch applications — so writing (or removing) one is arranging for
something to run later, and only naming the tool explicitly may silence the
question. Dangerous settings go one step further: they always ask (via
`Escalating`, which can only ever turn allow into ask), because a single
`"config.write_setting" = "allow"` line covers benign keys like a TTS speed
and must not be able to silently flip what the assistant may do.

**The dangerous set** (`config.Setting.Dangerous`, pinned by test):

- every `tools.*` registry key (prefix rule, fail-closed for future keys):
  `tools.shell`, `tools.shell_timeout_sec`, `tools.shell_max_output_kb`,
  `tools.artifacts`, `tools.desktop`, `tools.desktop_apps`,
  `tools.typing.enable`, `tools.typing.max_chars`, `tools.typing.rate_limit`,
  `tools.typing.rate_window_sec`, `tools.typing.terminal_classes`
- `activation.mode` — the push-to-talk / always-listening switch
- the command- and binary-bearing keys: `activation.wake_command`,
  `artifacts.open_command`, `tts.piper.binary`, `stt.whisper.binary`,
  `intents.terminal`

**The exclusion wall: unreachable, not deny-by-default.** Four spaces are
structurally outside every tool's addressable surface, whatever `[tools]`
says:

- `[ai]` — provider, model, system prompt, tokens/temperature, and every
  `[ai.<endpoint>]` table (credentials included). The assistant steering its
  own brain on request is the one write no confirmation can make safe,
  because the thing being confirmed is the judge of later asks.
- `[tools.policy]` — the gate's own tables. The gate must not loosen itself.
- `[advisors]` — advisor argvs are commands run with the user's credentials,
  and advisor tiers feed the gate (ADR 0016).
- `[[intents.custom]]` — the `intent.run` identity's premise is that the
  user wrote those commands by hand (ADR 0017).

Enforcement is layered, and none of the layers is a policy decision:

1. The entry tools address a closed three-family set (pinned to the daemon's
   `entryAdminFamilies` by a drift test); the settings tools resolve keys
   against `config.AssistantSettings()`, the registry minus the excluded
   prefixes.
2. A new `tools.Refusing` seam lets the tools refuse an excluded family/key
   **before the gate**: `Registry.Check` consults it ahead of the policy —
   ahead even of the no-policy fallback — and returns a deny whose rule is
   the spoken-ready reason ("the assistant may not change the tool
   permission policy that governs it…"). A `default = "allow"` policy plus a
   direct attempt still refuses; a test pins exactly that.
3. `Execute` re-checks the same wall, and the bridge re-checks it once more
   immediately before the shared write path — so even a bypassed gate writes
   nothing.

**The confirmation card is the entry, verbatim.** `Confirmable` builds the
card from the draft the write will hand to the pipeline (never the model's
narration): action + name, phrases, schedule, and **every command-bearing
field verbatim** — a script's path, a feed's argv element by element
(`"curl" "-s" "https://…"`, quoting each element so boundaries cannot hide),
each routine step's launched app. Deletes resolve the card from the file, the
way `memory.forget` resolves from the store. The card is served daemon-side
over the existing `tool.confirmation_required` event and snapshot — no QML
changes.

**Validation and conflicts are model feedback, not failures.** A `-32001`
refusal returns as the daemon's field-keyed `{field, message}` problems plus
exactly two legal continuations (fix and retry, or report the real problem —
never claim success). Fingerprints never travel through the model: the tool
family remembers what the model last read (`config.get_entry` is mandatory
before an edit — a blind edit is refused *with the current entry*, so the
refusal is itself the read), writes under that read's fingerprint, and on a
conflict re-reads once — retrying internally when the entry itself is
untouched (the file moved elsewhere), surfacing the current entry when it is
not (the hand-edit-mid-exchange criterion).

**Mid-session apply.** An assistant write is by construction mid-session,
which is exactly when the engine refuses to reconfigure — so a not-applied
write arms the same post-session reload a layout capture uses (#62), and the
tool result says the truth the model should speak: the change takes effect
the moment this exchange ends. The first knowledge feed's restart boundary
passes through with the daemon's own wording.

**Observability.** Every attempt lands in the activity ring through existing
machinery: `tool.confirmation_required`/`confirmed`/`declined`/`denied` rows
for the gate's outcomes, `config.entry_changed` (now carrying `source:
assistant|window`) for entry saves, and a new `config.setting_changed
{key, source}` event — key and source only, never the value — for settings.
Success wording is drawn from a re-read of the file, never from the request.

## Consequences

- One write discipline, three clients. A rule added to the loader's
  validation immediately governs the assistant, with its message arriving as
  a field-keyed problem the model can act on.
- The permission gate cannot be loosened by anything the model can call, in
  any policy configuration — including no policy at all. Loosening the gate
  remains a hand edit.
- `default = "allow"` keeps meaning what ADR 0030 made it mean: everything
  except arbitrary-execution surfaces, which now include authoring them.
- The prompt grows by one terse paragraph (`config.ConfigSystemPrompt`),
  always appended — the tools are always registered; the doctor's
  context-floor check measures the same prompt.
- `[[intents.custom]]` stays hand-edit-only even though routines/scripts/
  feeds are voice-editable; a future decision may add it as a family, and
  will have to argue with the wall's rationale to do so.
