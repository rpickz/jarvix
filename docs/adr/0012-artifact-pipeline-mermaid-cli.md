# ADR 0011 — Artifact pipeline: generic tool, mermaid-cli renderer

**Status:** accepted

## Context

Jarvix answers only in speech and overlay text, but much productive output is
not prose: architecture sketches, flows, comparisons. "Diagram my publish
pipeline" should put a picture on screen, not read syntax aloud. The same
seam — model source → saved file → render → open → notify — is what future
formats (documents, spreadsheets, Excalidraw) will plug into, so it must be
built generically the first time.

For the first format, Mermaid, the rendering options were: **mermaid-cli
(`mmdc`)** as a subprocess, a **pure-Go Mermaid renderer**, a **long-lived
render server**, or a **network service** (kroki.io / mermaid.ink).

## Decision

- **One generic tool, `artifact.create`**, registered in `internal/tools`
  like `shell.run`, taking `{format, title, source}`. Everything
  format-specific lives behind a `Renderer` interface (`Format`, extensions,
  `Available`, `Render`); the tool owns naming, layout, opening, and events.
- **Mermaid renders via `mmdc` (mermaid-cli) as a short-lived subprocess**,
  the ADR 0002 pattern: `mmdc -i <src>.mmd -o <out>.svg --quiet`.
- Artifacts land in one configured directory (`[artifacts] dir`, default
  `~/Documents/Jarvix`, created 0700) as `<date>-<slug>.mmd` + `.svg`, names
  claimed with `O_EXCL` so concurrent requests never overwrite. The model
  supplies only a title; anything path-like (`..`, separators) is rejected.
- The rendered file opens via `[artifacts] open_command` (default
  `xdg-open`); an `artifact.created` event (type, path) goes out on the bus.
  The tool result tells the model to answer in ≤2 spoken sentences and never
  recite source or paths — paths travel on the event and `jarvix artifacts`,
  not through TTS.

## Rationale

- **Subprocess over pure Go** — Mermaid is a moving JavaScript target; the
  only faithful renderer is Mermaid itself. mermaid-cli runs the real thing
  in a headless browser, so every diagram type renders exactly as documented.
  No pure-Go port has that fidelity, and chasing it is not Jarvix's job.
- **Subprocess over network service** — kroki.io/mermaid.ink would ship the
  user's diagrams (often their architecture) to a third party and break
  offline. Local render needs no network.
- **ADR 0002 consistency** — same properties as the speech engines: a crash
  or hang kills one tool call, not the daemon; cancellation is a process kill
  (the whole process group, since mmdc spawns a browser); zero cgo.
- **Optional dependency, graceful degradation** — mmdc missing is not an
  error state: the tool answers "diagram rendering unavailable" so the model
  falls back to prose, and `jarvix doctor` names the install command.

## Consequences

- mermaid-cli drags in Node + headless Chromium (~hundreds of MB) and a
  render costs a browser launch (~1–3 s). Acceptable: rendering is rare,
  bounded by `render_timeout_sec` (default 10 s), and the dependency is
  opt-in by installation rather than required by Jarvix.
- New formats implement `Renderer` and register in the daemon — no tool,
  engine, or protocol changes. The documents/spreadsheets ticket starts from
  this seam.
- If browser-launch latency ever matters, a renderer can switch to a managed
  long-lived process behind the same interface — the ADR 0002 escape hatch.
- Renderer output (SVG) opens in whatever `xdg-open` resolves; rendering
  inside the Jarvix window is a future ticket, not this seam's concern.

## Addendum — documents, spreadsheets, sketches (issue #6)

The second wave of formats (`document`/.md, `spreadsheet`/.csv,
`excalidraw`/.excalidraw) plugged into the seam as predicted, with two small
generalisations of the tool — no per-format branch anywhere in the engine or
daemon beyond appending to the renderer list:

- **Passthrough formats.** For these formats the saved source *is* the
  artifact, so a renderer with `SourceExt() == OutputExt()` makes the tool
  skip the render step entirely; one file is written, not two. A shared
  `passthrough` embed supplies the no-op `Available`/`Render` halves.
- **Pre-write validation.** Renderers may implement `SourceValidator`
  (`ValidateSource(source) error`), checked before anything touches disk.
  Structured formats must fail *before* the write — an invalid CSV or scene
  file must never exist even transiently — and the specific error (line
  numbers, field names) goes back to the model for its retry round, the
  same contract render failures already had. CSV validates via a strict
  `encoding/csv` parse (ragged rows and broken quoting fail with line
  numbers); Excalidraw scenes validate structurally (`type: "excalidraw"`,
  positive numeric `version`, `elements` array of objects with `type` and
  `x`/`y`) without pinning per-element schemas that churn between releases.

Two seam-level guardrails came with them: artifact source is capped at 1 MB
and refused — never truncated, because a truncated structured file is
silently corrupt — and `[artifacts.open_commands]` overrides the viewer per
format (an entry of `""`/`"none"` means "no viewer": the tool saves the file
and names it, by base name only, in the result). `TestArtifactFormatsShareOneSeam`
pins the property that adding a format is registration-only.

## Addendum — diagrams open as PNG, not SVG (issue #56)

The original decision rendered SVG ("crisp at any zoom"). In practice the
artifact that opened was **textless**: mermaid emits labels as HTML inside
`<foreignObject>`, which only a browser engine renders, and `xdg-open` sends
`image/svg+xml` to whatever the desktop associates — on the reporting
machine an image editor. The user's file had 27 foreignObjects and zero
`<text>` elements; every non-browser viewer showed empty boxes.

So the default output is now **PNG at 2× scale** (`mmdc … -s 2`): the
embedded browser that mermaid-cli already runs rasterises its own drawing,
and the file that opens means the same thing in every viewer. The crispness
trade was judged wrong the first time — a raster that shows its words beats
markup that needs a second browser to mean anything.

`[artifacts] diagram_format = "svg"` opts back in for users who want markup
to edit or embed. That path renders under a mermaid config with
`htmlLabels: false`, so labels become real `<text>` — but some shapes still
emit foreignObject regardless, which is why SVG is the opt-in rather than
the default. The `.mmd` source is still saved beside the render either way,
and `jarvix artifacts` folds the pair into one listed diagram.

Verification note for this class of bug: mmdc exits 0 while producing an
artifact no image viewer can show, so the real-renderer test decodes the
output (PNG header, sane dimensions) instead of trusting the exit code.
