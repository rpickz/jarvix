# ADR 0033 — Entry forms: generic validate/upsert/delete IPC, field-keyed problems

**Status:** accepted

## Context

The Automations tab (#93) manages routines and scripts but could not create,
change, or remove one: it handed over a copyable TOML block and sent the user
to a text editor. Issue #99 replaces that hand-off with a real form dialog in
the window — New, Edit, Delete — with the loader's validation surfaced as
inline field errors before anything is written. The sibling ticket (#100)
needs the same machinery for knowledge feeds and memory entries, so the shape
of this surface is the decision that outlives the tab.

Three prior decisions constrain the design. ADR 0013: the window decides
nothing — every rule must be judged daemon-side, so the form can only
round-trip a draft over IPC. ADR 0015 (extended by #92/#93/#98): config.toml
stays authoritative and hand-editable, so every programmatic write is
byte-preserving outside the part written, validated whole before landing,
atomic, fingerprint-guarded against external edits, and applied by the
standard reload. ADR 0030: a script runs with zero arguments and nothing
spoken or typed may ever reach an argv — a form that saves entries must not
become the code path that breaks this.

## Decision

**Four generic verbs, family-parameterised, zero family logic in the flow.**
`config.get_entry`, `config.validate_entry`, `config.upsert_entry`, and
`config.delete_entry` take the TOML table-array name (`family: "routines" |
"scripts"`) and address entries by `name`, case-insensitively — the
uniqueness rule every family already uses. Automations-scoped wrappers were
considered and rejected: #100 would either duplicate them per family or
unwrap them back to this exact shape. What *is* per-family lives in one
registry (`entryAdminFamilies` in `internal/daemon/entry_admin.go`): the key
whitelist with wire shapes, the rendering order, the sub-table schema, and
the word activity rows use. #100 is new registry rows, not new verbs.

**The form round-trips the parser's own map.** `config.get_entry` returns the
whole entry as decoded from the file, paired with the file's fingerprint from
the same read. The form edits the fields it shows; every other key (`report`,
a captured step's `size`/`position`/`tile`) rides along and is written back
verbatim. This is what lets the form show fewer inputs than the schema has
without a save silently deleting the rest of the entry.

**Validation is the loader's, whole-document, dry-run first.** Both
`validate_entry` and the save build the candidate document in memory through
the byte-preserving editor and run `Config.Validate()` on the result — the
real intent router compiled for phrase grammar and collisions, the real
schedule parser, the real script path checks. There is no second, weaker copy
of any rule, and no rule the form was not shown can first appear at save time
(only the world moving — a fingerprint conflict — can). A valid schedule also
returns `next_fire` from the scheduler's own arithmetic, so the preview the
form shows is the clock's truth, not a QML reimplementation.

**Problems are field-keyed by the validators' own labels.** The response
shape is `problems: [{field, message}]` — `name`, `phrases[1]`, `path`,
`schedule`, `steps[2].app`, `""` for whole-entry — so the window pins each
message under its input. The classifier trusts the label contract the
validators already keep (`family[i] ("name")`, `... steps[j]:`, the quoted
`phrase "…"`), including the asymmetric collision case: when the draft steals
a phrase from an entry that compiles later, the complaint carries the *other*
entry's label, and the quoted phrase is matched back to the draft's own
phrase list. Anything unclassifiable keeps `field: ""` and is shown in the
form's general area — the daemon's words are never dropped, only placed.

**Writes follow the settings discipline exactly.** Fingerprint checked
against the file on disk (the form opened with it; a mismatch is a `-32002`
"changed outside the window", nothing written, hand edits never clobbered);
rewrite through `config.UpsertEntryTOML` / `DeleteEntryTOML` (whole-entry
extensions of the #92 field editor, golden-tested for insert, in-place edit,
and delete with `[[routines.steps]]` sub-tables surviving); whole-document
validation (`-32001` with the field-keyed problems on failure — never a
half-write); atomic write; standard reload (grammar recompiled, schedules
rebuilt); `config.changed` plus a `config.entry_changed` bus event the
activity feed renders as "Routine created: …" / "Script deleted: …".

**Saving is not an execution path.** The registry whitelist is the
zero-argument shape at the wire (ADR 0030): a draft carrying `args` or `env`
is refused by shape with a field problem before any validator runs, and the
pipeline contains no exec — a script's path is stat-ed by the load validator,
never run. Deleting is byte-preserving removal: the entry's block, its
sub-tables, and the comment glued to its header (it documents the entry); a
comment separated by a blank line — a section header — stays.

## Consequences

- The window's QML (JarvixWindow's form pane, JarvixFormField / FormToggle /
  FormButton) is display-and-signal only: it serialises a draft, ships it,
  and pins returned problems. Live per-field errors are one `validate_entry`
  per field commit — cheap, and identical to what the save will judge.
- Replacing an entry re-renders its block, so comments *inside* an edited
  entry are lost (the `UpsertRoutineTOML` precedent); everything outside the
  block is byte-identical, golden-pinned.
- A hand-written key outside the registry whitelist would block form-saving
  that entry until removed — currently impossible, since the whitelists
  mirror the config structs exactly.
- #100 adds knowledge feeds/memory by registering their families and building
  their forms from the same components; if a family ever needs per-family
  behaviour beyond schema, that pressure should produce a registry field, not
  a family-specific verb.
