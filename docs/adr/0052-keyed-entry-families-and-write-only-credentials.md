# ADR 0052 — Keyed entry families, and credentials that only travel one way

**Status:** accepted

## Context

Issue #163 gives the window a Providers section: the `[ai.<name>]` endpoints
Jarvix thinks with and the `[advisors.<name>]` CLIs it consults, administered
through validated forms so a user configuring a new machine or switching
providers is never told to edit a TOML file.

Two things about those families broke the surface ADR 0033 built.

**They are not arrays.** The entry registry (`entryAdminFamilies`) and the
byte-preserving editor beneath it address `[[family]]` array-of-tables entries
by a `name` key *inside* the table, matched case-insensitively. An endpoint is
a `[ai.openai]` table whose key **is** its identity, and that key is what
`ai.provider` resolves through a case-sensitive Go map. `[ai]` additionally
holds the section's own scalars (`provider`, `model`, …) in the same table.

**They hold a credential.** `[ai.<name>].api_key` is the one value in
config.toml that must never come back out. The existing entry pipeline is
built on the opposite premise: `config.get_entry` returns the *whole* parsed
entry so a form can round-trip keys it has no widget for, and validation
problems quote what they refuse.

The obvious shortcuts were both rejected. Migrating endpoints to
`[[ai.providers]]` would break every hand-written config and every published
example for the convenience of one registry. Writing per-family CRUD for the
two new families would make four generic verbs into six family-specific ones —
the exact duplication ADR 0033 exists to prevent, and #164 would inherit it.

## Decision

### A family declares its shape; the pipeline never guesses

`entryFamilySpec` gains `shape`, either `entryShapeArray` (the existing
`[[family]]` behaviour) or `entryShapeKeyed` (`[family.<name>]`). Four
one-line dispatch functions — read, upsert-rewrite, delete-rewrite, label —
are the *entire* cost of the second shape: the handlers, the fingerprint
guard, the whole-document validation, the atomic write, the standard reload
and the events are one piece of code that never learns which shape it serves.

The keyed editor lives beside the array one
(`internal/config/keyed_rewrite.go`) under the same contract: everything
outside the block being written survives byte-for-byte, and the result must
re-parse and read back as exactly the intended edit or nothing is returned.
Goldens pin insert, edit and delete for both families.

**The wire shape does not change.** A draft still carries `name`, so one form
and one registry vocabulary drive both shapes. For a keyed family that key
renders as the table *header* and is never stored as a field — a stored copy
could disagree with the header, and the loader would believe the header.

**Keyed addressing is exact.** Array families match names case-insensitively;
keyed families must not. TOML keys and the maps the loader decodes them into
are case-sensitive, so silently matching `OpenAI` to `openai` would edit a
different endpoint than the one `ai.provider` resolves — precisely the mistake
a byte-preserving editor exists to make impossible.

Three more registry fields carry what these families know beyond their keys,
each a declaration rather than a branch in the flow:

- `reserved` — the scalars sharing a keyed family's own table (`[ai]`'s
  settings), so they are never mistaken for entries. It is
  `config.ReservedAIKeys()`, the same set the loader uses, so the two cannot
  drift about what `[ai.model]` would mean.
- `guardDelete` — why one entry cannot be removed. Whole-document validation
  cannot make this call for endpoints: deleting the table of a *preset*
  endpoint still validates, because the preset's defaults survive the file,
  while leaving the user's provider pointing at something they can no longer
  see or edit.
- `notes` — what a draft *earns*, keyed to the field that decides it. An
  advisor's permission tier is the case that demands it (ADR 0016): a
  hand-written argv drops it from allow to ask, and a form that did not say so
  would let someone loosen a permission gate as a side effect of typing a flag.

### A credential travels in exactly one direction

A family declares its credential keys (`secrets`), and three mechanisms follow,
each the only way its direction of travel is possible:

**Out — never.** Declared secret keys are stripped from every entry that
leaves the daemon and replaced by facts *about* the credential:

```
"secrets": { "api_key": {
    "set": true, "source": "env" | "config" | "none",
    "env": "OPENAI_API_KEY", "env_set": true, "inline_key": false,
    "label": "API key", "env_key": "api_key_env" } }
```

`env`, `env_set` and `inline_key` are deliberately the vocabulary `config.get`
already uses, so the window has one dialect for credentials. `source` states
which one the runtime would actually use, decided the way `Endpoint.Key`
decides it. **No mask is offered** — a masked prefix is a prefix of the key,
and a mask of the right length is the key's length.

**In — as an instruction.** A separate `secrets` parameter carries
`{action: "keep" | "set" | "clear", value}`. The zero value is *keep*, so a
form that never touches the credential — and a caller that has never heard of
credentials — cannot delete one by omission. On keep, the daemon reads the
stored value out of the file and writes it back: it goes file → memory → file
and exists nowhere in between. A draft carrying the secret key itself is
refused by the family's key whitelist, because the entry map is echoed to
forms, carried through validation, and quoted in problems, and a value must
not be in something with that many destinations.

**Escaping — scrubbed.** Every message that could carry a value out — a
validator's problem, a rewriter's error, an engine's refusal reason — passes
through a scrubber holding the values in play for that one call. No validator
quotes a credential today; leak-salted tests exist so nobody has to trust that.

### The Test action performs a real request or says nothing

`config.test_entry` runs the family's declared probe. For an endpoint that is
`GET /models` with the credential resolved daemon-side — the cheapest request
that proves both halves, since it costs no tokens and returns 401 rather than
200 for a wrong key — bounded by a ten-second budget and reported as
**reachable** (a 2xx that actually arrived), **unauthorised** (401/402/403), or
**unreachable** (a refused dial, a timeout, or a status meaning this is not an
API root). Failures carry the service's own words, never a guess. A family
with no probe is refused rather than answered: "nothing to test" is honest and
a fabricated success is the one outcome the ticket forbids.

### The families stay out of the assistant's reach

`assistantReason` on a registry row is the entry half of #109's exclusion
wall. The assistant's config tools resolve families through
`assistantEntryFamily`, which these two are absent from, for the same reason
`[ai]` and `[advisors]` are absent from `AssistantSettings`: a gate must not be
able to loosen itself, and a brain must not be able to choose its own. The
tool layer's closed family list is pinned to the *assistant-reachable* subset
of the registry, so a family added later without a decision about the model
fails a test rather than shipping.

## Consequences

- A third document shape (say a `[x.y.z]` nest) is a `shape` value and four
  dispatch cases, not a new surface.
- `config.list_entries` joins the four write verbs as a registry-driven read,
  so a listing screen for a new family needs no listing code either.
- `Config.Validate` now judges every `[ai.<name>]` table — name, base URL,
  environment-variable name — not only the selected one, so a half-typed
  endpoint is refused at the form that typed it. An existing config with an
  endpoint that has no `base_url` will now be refused at load with directions;
  such an endpoint could never have served a request.
- Endpoint changes join the structured tables `idleClassChanged` compares
  directly, so a corrected base URL is live on the standard reload rather than
  written to a file the daemon ignores. Advisors are pinned like memory: the
  advisor tool and its tiers are built once at construction, so an advisor save
  reports `applied: false` with the restart named.
- A rewrite that adds a credential keeps the config file at mode 0600.
  `WriteFileAtomic` pins that rather than copying the previous mode: a file
  holding a key should not inherit a 0644 somebody set by hand.
- The window can render a credential wrongly only by rendering something it was
  never given. A Go text scan pins that it does not try.
