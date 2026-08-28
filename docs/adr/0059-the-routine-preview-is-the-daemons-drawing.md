# ADR 0059 — The routine preview is the daemon's drawing, not the window's

**Status:** accepted (implements issue #181; extends ADR 0013, ADR 0026 and
ADR 0056)

## Context

A routine is a description of a desktop: what launches, with which arguments,
adopted or fresh, on which monitor and workspace, in which placement mode, at
what proportion. Issues #183, #185 and #186 gave that description a
vocabulary, screen names and a launching half. What none of them gave it was
a way to find out whether the description you wrote is the description you
meant.

The only feedback loop available was to say the routine's phrase and watch.
That loop is the worst in the product, for reasons specific to this family:

- It runs on a **live desktop**. Six windows move, and they move over whatever
  the user was actually doing.
- It is **slow to be wrong**. A step whose window never appears is waited on
  for eight seconds before it reports; a routine of six steps takes the best
  part of a minute to tell you that one share was a percent too big.
- It is **not reversible**. There is no "undo the arrangement": putting the
  desktop back is a second act of work.
- And the mistake it reports is usually **not the one you made**. Step order
  decides tiling structure (ADR 0056's `place_next` is a one-shot
  preselection), so "the two web apps are side by side instead of stacked" is
  a fact about which step came second, and nothing on screen says so.

The user chose the shape of the answer when asked how ambitious the editor
should be: **structured forms plus a preview diagram** — a form for every
value, and a picture of the arrangement those values produce. Explicitly not
drag-and-drop; a visual layout editor is a later question.

That settles the interaction and opens the only interesting question: **who
computes the picture.**

## Decision

**Every number in the diagram is computed by the daemon, and the window scales
fractions into rectangles.**

Concretely:

1. `placement.Arrange(Monitor, []Arranged) Arrangement` is new in the
   vocabulary package. Given a real monitor and one workspace's windows *in
   the order the routine opens them*, it returns the aspect ratio of the
   glass, the usable area as an inset, and one rectangle per window — all as
   fractions of the monitor — plus the words to label each by (`share`,
   `size`, `note`).
2. `Placement.Sentence(what)` is new beside it: the same placement in one
   sentence. This is the accessibility channel, and it is the channel that
   still says something when the target screen is in a bag.
3. `routine.Describe` assembles those into a per-workspace preview, resolving
   each workspace's screen exactly as the runner does — the first step naming
   one wins, otherwise the screen the workspace is on.
4. The daemon serves it through the **existing** `config.validate_entry`, as a
   `preview` field, behind a new `preview` hook on the entry-admin registry
   (ADR 0033's pressure valve, beside `notes`, `pending`, `probe` and
   `guardDelete`). One family declares it; the pipeline never learns which.
5. `placement.vocabulary` is a new verb serving the closed sets the form's
   pickers offer — modes with their summaries, arrangement directions, focus
   choices, launch policies, and the states the vocabulary declines with their
   reasons.
6. `JarvixLayoutPreview.qml` multiplies fractions by the width of a box and
   renders text it was handed. A text guard in `internal/desktop` fails the
   build if it acquires `Math.`, a percent sign, a pixel unit, a number parse,
   or any reference to monitor geometry.

### Why the picture must not be the window's

ADR 0013 already says the window is a thin client, and this could have been
read as a rendering detail exempt from it. It is the opposite of exempt.

A diagram is a **claim about what will happen**, and it is the most persuasive
claim in the product: a picture reads as a fact in a way a field does not. If
the window worked out its own rectangles it would be a second implementation
of "what does 66% mean" — against `Monitor.Usable`, against the bars'
reservation, against the scale factor — and the two implementations would
diverge. Not immediately: on the day someone changed the rounding in
`Extent.Resolve`, or added a mode, or fixed the scale arithmetic. And on that
day the user would believe the picture, arrange their morning around it, and
run a routine that did something else. That is a worse failure than having no
preview at all, because a wrong preview costs trust in the whole feature.

So the seam is drawn where it can be *tested*: the daemon emits fractions and
words; the QML has no vocabulary and no arithmetic; a Go text scan enforces
it. The same argument monitors.list already makes for the reserved words
(ADR 0057), applied to the rest of the picture.

### Why it rides on `config.validate_entry` rather than its own verb

The diagram and the field errors are two views of the same judgement, and a
form that fetched them separately would show them one edit apart — a picture
of an arrangement whose share has just been refused, or a refusal beside a
picture that has not caught up. Carrying the preview in the validate reply
makes that impossible by construction, and it means the diagram redraws on
exactly the events the problems do: a field committing, and every structural
change (add, remove, **move**) that goes through the draft reassignment.

It also fixes what the preview is computed *from*. The validate pipeline
already rewrites the whole document in memory; the preview is computed from
**that document**, parsed by `config.ParseBytes` and converted by
`Config.RoutineDefinitions` — the same two steps a real load takes. There is
therefore no second reading of the draft's keys anywhere, which is the other
way two surfaces come to disagree about what a value means.

### Why refusals travel back into the preview

Some refusals never reach a `Placement` at all. `config.RoutineStep.placement`
reads a value its parser rejects as *"not said"* — deliberately, so a
half-parsed value can never reach a runner — and the loader refuses the
document instead. A preview computed only from the converted step would
therefore draw a tiled window for `mode = "grouped"`: the exact layout that
will not happen, drawn confidently.

So the validation's own field-keyed problems are handed to `routine.Describe`,
and a workspace any of them names is **not drawn**. The same rule covers the
contradictions `Arrange` finds for itself — two windows asking for more of an
axis than it has — and both come back as `drawable: false` with the daemon's
sentence, keyed to the step and the field.

### What the arithmetic models, and what it does not promise

`Arrange` models the routine's own ask: each window splits the tile of the
window before it, in the direction that one's `place_next` named (or, saying
nothing, along the longer axis — what a dwindle layout does), at whichever of
the two declared a share of that axis. Where the asks are consistent that is
what the compositor delivers.

Where they are not, it **refuses rather than resolves**. Two thirds beside a
half is 116% of a screen; a compositor asked for it will deliver something —
one of the two resizes simply loses — and a preview showing either outcome
would be teaching the user that the routine works. Naming both windows and
both numbers is what someone needs in order to change one of them.

Three things it deliberately declines to draw, each with words instead:

- **A floating window with no size.** Its size is the application's business.
  A rectangle for it would be inventing the one number the routine did not
  give.
- **The master pane's position.** `master` is a property of a layout that has
  one; where that pane is belongs to the workspace's layout, so the panel
  carries a note and the geometry is left alone.
- **A share nothing came along to take the rest of.** A lone tiled window gets
  the whole workspace whatever it asked for. It is drawn full width — because
  that is what happens — and the note says why the 66% was not used.

## Consequences

- The routine editor is complete: every key of `[[routines.steps]]` has a
  control, and the last surface that sent a user to a text editor for a
  placement key is gone. The superseded spellings (`float`, `size`, `tile`)
  are carried through untouched and removed by a button, so nothing is
  silently deleted and nothing is stuck.
- The preview degrades in three named ways rather than one blank panel: a
  screen not plugged in, a compositor that will not answer, and a daemon with
  the window tools switched off each say which they are. The step sentences
  survive all three, which is what makes a routine written on a desktop
  editable from a laptop that has none of it.
- `placement.vocabulary` is now the one place a client learns the closed sets.
  A mode added to `modeSpecs` appears in the editor with its summary and needs
  no window change; a state the vocabulary declines is offered as a *reason*
  rather than as an absence.
- The window gained one shared component (`JarvixLayoutPreview.qml`) and no
  new form widget: the pickers are `JarvixMonitorPicker`, generalised from
  screens to any daemon-supplied closed set, which is what it always was.
- **Rejected: drag-and-drop.** The user chose forms plus preview, and the
  reason holds independently — a drag gesture would have to be turned back
  into `place_next` and a percentage, which means the window deciding what a
  drop *meant*. That is the same arithmetic this ADR just moved out of it.
- **Rejected: applying the arrangement live as you type.** It is the feedback
  loop this whole ticket exists to replace, and it would move the user's
  windows while they were reading about moving them.
