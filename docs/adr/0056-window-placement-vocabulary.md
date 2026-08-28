# ADR 0056 — One window-placement vocabulary, and the compositor verbs it really speaks

**Status:** accepted (implements issues #176 and #177; supersedes the placement
half of ADR 0026's routine schema; the seam it corrects is ADR 0022's)

## Context

A user set out to place their morning windows — a personal browser at two
thirds of the top screen, X and ChatGPT stacked in the remaining third, a work
browser on the bottom screen — and could not. A routine step could say almost
nothing about *where* a window goes: a workspace number, a floating flag, a
pixel size and position that only applied while floating, and
`tile = "split" | "master"`. The window-control tools could say a workspace
number and nothing else. The gap was worked around with a shell script, which
had to reimplement launching, matching, monitor geometry and placement, and
still could not express a tiled proportion — the script is the evidence that
the model was too thin, not the fix.

Underneath it, worse: **the placement verbs the daemon sent did not exist.**
Hyprland 0.55 moved `hyprctl dispatch` to Lua, ADR 0022's seam learned to
discover which dialect to speak, and the Lua *spellings* were then written from
documentation and pinned by tests asserting the argv the daemon builds against
a fixture of the same spelling — a self-consistent pair with no external truth
in it. `hyprctl` exits 0 for a dispatch the compositor refused, so every wrong
verb failed silently and the step was reported as placed.

The user's requirement, stated directly: *"It's crucial that we're able to
control, with a great UI, what a routine does — and if it launches UI windows,
we should be able to choose how they're composited on the screen… Basically any
option for compositing the window which the OS provides… therefore this needs to
be implemented in a standard way, with even standard form elements and such,
standard validations."*

## Decision

### One vocabulary, in `internal/placement`

A new package defines *where a window goes*, once. It is pure — values,
parsing, validation and arithmetic, no compositor — and it is imported by the
routine schema (`internal/config`), the runner (`internal/routine`), the
window-control tools (`internal/tools`) and the dialect seam
(`internal/desktop`). A contract test
(`internal/placement/contract_test.go`) fails if any of them drifts from it.

| Field | Values | Meaning |
| --- | --- | --- |
| `mode` | `tiled`, `floating`, `pinned`, `fullscreen`, `maximised` | how the window sits |
| `width`, `height` | `"66%"`, `"1200px"`, `"1200"` | a share of the monitor's **usable** area, or pixels |
| `position` | `[x, y]` pixels | floating modes only |
| `place_next` | `right`, `left`, `below`, `above` | where the **next** tiled window on this workspace goes |
| `master` | `true` | promote into the layout's big pane |
| `workspace` | 1–99 | which workspace |
| `monitor` | a connector name, or `current` | which screen the workspace lives on |
| `focus` | `silent` (default), `follow` | whether the view follows |

Three properties are the design:

- **Percentages are of the usable area**, not the output: the part left after
  the bars reserved theirs. "Two thirds of the screen" means two thirds of the
  part you can put a window in; sized against the output it overhangs the bar
  by exactly the bar's height, on every monitor the user owns.
- **Arrangement is `place_next`, and therefore step order is meaning.** It is
  this shape rather than a grid because it is what tiling compositors actually
  offer: a dwindle-family layout decides where a new window lands *when the
  window maps*, from the focused window and a one-shot preselection, and never
  revisits it. The user's case is "this one takes two thirds; the next goes to
  its right; the one after that goes below" — three steps, in that order.
  Reordering steps reorders the desktop, which is why the editor (#181) has to
  draw it.
- **Everything is a set, never a toggle.** ADR 0026's convergence rule applied
  to the whole vocabulary: a floating placement also *un*pins, a tiled one also
  *leaves* fullscreen. A routine's second run lands where its first did even
  when the user moved things by hand in between.

Validation is `placement.Problems`, run by the config loader, the form's
validate call and the tools alike, and keyed to the field it belongs to —
`width`, `monitor`, `place_next` — so one message lands on the right control in
the window and reads correctly in a config-load error. A percentage over a
hundred, a pixel size larger than the target screen, a size on a mode that has
none: all load-time and form-time, never an eight-second silence at run time.

### The probed verb table

Every verb was probed against **Hyprland 0.56.2** before it was written, with a
deliberately bogus window address (`address:0xdeadbeef`). Argument parsing
happens *before* the window lookup, so the reply distinguishes a wrong argument
shape from a missing window, and nothing real is touched. The `hl.dsp` tree was
also enumerated directly, read-only, through `hyprctl eval` — `error()` is the
one channel that returns a value, so a `pairs()` walk surfaced as an error
message — and each signature was then confirmed against Hyprland's own
`src/config/lua/bindings/LuaBindingsDispatchers.cpp` at tag `v0.56.2`.

| What is wanted | The Lua verb, as probed | Notes |
| --- | --- | --- |
| focus a window | `hl.dsp.focus({ window = "address:…" })` | reports `window not found` — one of the few that does |
| focus a workspace | `hl.dsp.focus({ workspace = N })` | |
| send to a workspace | `hl.dsp.window.move({ workspace = N, window = …, follow = false })` | `follow = false` is what makes it silent |
| **resize** | `hl.dsp.window.resize({ window = …, x = W, y = H, relative = false })` | **`x`/`y` are the SIZE.** `width`/`height` → *"unrecognized arguments. Expected positions (x & y) or keep_aspect_ratio"*. There is no `exact` key; exactness is `relative = false` (its default) |
| **position** | `hl.dsp.window.move({ window = …, x, y, relative = false })` | **there is no `hl.dsp.window.position`** — *"attempt to call a nil value (field 'position')"* |
| **float / tile** | `hl.dsp.window.float({ window = …, action = "enable"\|"disable" })` | **there is no `hl.dsp.window.set_floating`** — *"attempt to call a nil value"*. And it validates nothing: an unrecognised `action` silently falls back to **toggle** (`Internal::parseToggleStr`), so the two words are constants in the argv and not something a caller can influence |
| pin | `hl.dsp.window.pin({ window = …, action = "enable"\|"disable" })` | same silent toggle fallback |
| fullscreen / maximise | `hl.dsp.window.fullscreen({ window = …, mode = "fullscreen"\|"maximized", action = "set"\|"unset" })` | the well-behaved one: validates both enumerations *and* reports `no target` for a missing window |
| arrange the next window | `hl.dsp.layout("preselect r"\|"l"\|"d"\|"u")` | **`hl.dsp.layout` is a FUNCTION taking a plain string**; a table is *"expected string, got table"*. A dwindle-family message |
| promote to master | `hl.dsp.layout("swapwithmaster")` | **there is no `hl.dsp.layout.swap_with_master`** — *"attempt to index a function value"*. No window selector on either dialect, so the seam focuses first |
| send a window to a screen | `hl.dsp.window.move({ monitor = "DP-2", window = …, follow = false })` | an absent output is a hard error, not `ok` |
| send a workspace to a screen | `hl.dsp.workspace.move({ workspace = N, monitor = "DP-2" })` | `monitor` is required; an absent one answers `Monitor not found` |
| read the outputs | `hyprctl monitors -j` | `reserved` is `[left, top, right, bottom]` in logical pixels |
| read the layout | `hyprctl getoption general:layout -j` | tells `preselect` and `swapwithmaster` apart |

Four of those — resize, position, float and master — were **wrong in shipped
code**, which is the whole of issue #177. The remaining trap is that
`hyprctl` exits 0 while printing `warning: … window not found`, so the seam
judges the *reply* (`runDispatch`: success is "ok" and nothing else) rather
than the exit status. `hl.dsp.window.move`, `resize`, `float` and `pin` answer
plain `ok` even for a window that does not exist, which is why every dispatch
still names an address the inventory itself reported.

**The legacy (hyprlang) spellings could not be re-probed here**, because the
machine this was written on runs the Lua dialect. They are Hyprland's
documented dispatchers and are pinned by the same argv test; the live
verification script probes whichever dialect the machine it runs on speaks, so
a legacy user's first run of it is the check.

### What the compositor offers and this vocabulary declines

Each is recorded in `placement.UnsupportedModes()` with its reason, so the
refusal message, the docs and this ADR say the same sentence, and a test
asserts that refusing one of them quotes its reason.

| Not a mode | Why |
| --- | --- |
| grouped | `hl.dsp.group.toggle` only toggles — there is no "be in a group" verb — so a routine run twice would group and then ungroup. Placement must converge, not oscillate |
| tabbed | tabs are how a group is *drawn*, not a separate state: the grouped case with the same problem |
| pseudotiled | `hl.dsp.window.pseudo` takes enable/disable, but pseudotiling has no size of its own (it keeps the window's last floating size inside its tile), so it cannot express a proportion — which is what this vocabulary is for |
| scratchpad / special workspace | summoned rather than placed; it is a *target*, and this vocabulary's targets are the numbered workspaces 1–99 |
| per-client fullscreen (`fullscreen_state`) | tells an application it is fullscreen without making it so — a compatibility shim for misbehaving clients, not a way to place a window |

Two more are supported but **layout-dependent**, and say so rather than
failing silently: `place_next` needs a dwindle-family layout, `master` a
master-family one. Both refusals arrive as `Unknown <layout> layoutmsg`, which
the seam rewrites into the vocabulary's own words before the runner reports
them.

### Monitors: three forms, one seam

`placement.MonitorRef` classifies a connector name, the reserved word
`current`, and — reserved for the monitor-identity issue (#180) — a nickname.
`placement.Resolver` resolves one against a live inventory, with `Nicknames`
nil today. Precedence is deliberate: **a present output wins over everything**,
then `current`, then a nickname — so a nickname can never quietly redirect a
routine that named a real connector, which is the failure a nickname feature
must not introduce. A ref that names nothing present is an error naming it
("no monitor is called `top` right now; the screens plugged in are …") rather
than a fallback to the focused screen, because falling back would put the
morning windows on the wrong monitor and report success.

`monitor` on a step moves the **workspace**, not the window: the windows of one
workspace belong together, and moving them individually would scatter a layout
across two screens. `desktop.move_window` naming a monitor and no workspace
moves that one window, because "put this on the other screen" is about one
window.

### Consequences the implementation has to live with

- **A routine that arranges its windows launches them one at a time.** The
  layout decides where a window lands at the moment it maps, so the previous
  design — dispatch every launch up front so the applications cold-start in
  parallel — cannot control arrangement. A routine with no `place_next` keeps
  the parallel path unchanged; one with any keeps the slow, correct one.
- **It switches the view.** A new window maps onto whatever workspace is in
  view, so an arranging routine focuses each step's workspace before launching
  it. This is visible and unavoidable on every compositor this seam models.
- **Tiled proportions are a separate pass, last.** A tiled resize moves the
  split the window sits in, which means nothing until the windows sharing that
  split exist.

### The superseded spellings stay

`float`, `size` and `tile` — the entire placement vocabulary before this ADR —
are still accepted and translate into the new mode on load. Refusing them would
have broken every routine in the field to make a schema tidier. The renderer,
the capture writer and the form emit **only** the current vocabulary, so an
entry migrates the first time anyone saves it; a step spelling one directive
both ways is a validation error naming both, because picking a winner quietly
is how a routine ends up doing something nobody wrote.

## Alternatives considered

- **Fix #177's four verbs and stop there.** It would have made `size = [w, h]`
  work and left the user unable to say "two thirds", which is what they asked
  for. The vocabulary cannot be built on a broken seam, but the seam alone is
  not the vocabulary.
- **A grid or a percentage rectangle per window** (`region = [x, y, w, h]`).
  Expressive on paper and undeliverable: a tiling compositor has no verb that
  places a tiled window at coordinates, so honouring it would mean floating
  every window — the mode the user explicitly did not want — or reimplementing
  the layout. `place_next` is what the compositor genuinely offers.
- **Exposing the whole vocabulary to the model.** `position`, `place_next`,
  `master` and `focus` are withheld, each with a recorded reason
  (`tools.PlacementFieldsWithheldFromTheModel`): pixel coordinates are a thing
  a person points at, `place_next` only means something inside a written
  sequence, `master` changes every other window on the workspace, and the tools
  already say "follow" by which verb was called. The contract test fails if a
  new field is neither offered nor excused.
- **Extending `jarvix doctor` with the verb-shape probe** (#177 suggested
  considering it). Declined for now: the probe's value is catching a *spelling*
  that regressed, which happens when this repo changes rather than when a
  user's machine does, and `scripts/verify-window-placement.sh` runs it in the
  place where someone is already looking. Worth revisiting if a Hyprland
  release breaks a verb in the field.

## Consequences

Placement stops being something we hope works. A refused dispatch is a reported
failure naming what it could not do ("could not be sized", "no monitor is
called DP-2 right now"), not a step counted as placed. Layout capture no longer
records a size it cannot replay. The tools and routines refuse the same value
with the same words, and the form gets its controls and its validation from the
same place. #175 (launching), #180 (monitor nicknames) and #181 (the editor)
all sit on this: their integration contract is `placement.Placement`,
`placement.Resolver.Nicknames`, and `placement.Fields()`.
