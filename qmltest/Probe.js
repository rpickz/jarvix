// Probe — how a test looks inside a surface it did not build.
//
// Deliberately NOT a `.pragma library`. A library-pragma JS file is evaluated
// with no QML context, and `item.Accessible.name` — the identity these helpers
// match controls by — silently answers undefined there. Sharing the importing
// file's context costs one evaluation per test file and buys the one thing the
// probe is for.
//
// The window's controls are anonymous: delegates inside Repeaters and
// ListViews, with `id`s that are private to the file. #174 forbids editing the
// production QML to make it testable, and adding `objectName` to eighty
// delegates would be exactly that. So the harness finds things the way a
// screen reader does — by walking the object tree and matching on the words
// and roles the controls already publish for accessibility.
//
// That is not a workaround, it is the better test: a control found by its
// accessible name is a control a keyboard user can find, and a refactor that
// renames a button's label breaks the test *because* it broke the label.

// walk returns every QObject reachable from root through the two properties
// QML parents things onto: `children` (visual items) and `data` (the default
// property, which also holds ListModels, Timers, Connections and the like).
// The window puts its transcript model and its socket in `data`, so a walk
// over `children` alone would miss precisely the objects a test needs.
//
// The visited set is not an optimisation. `children` and `data` overlap for
// every visual child, so without it the walk revisits most of the tree once
// per level and an 8000-line window takes minutes instead of milliseconds.
function walk(root) {
  var out = []
  var seen = []
  var queue = [root]
  while (queue.length > 0) {
    var node = queue.shift()
    if (node === null || node === undefined) {
      continue
    }
    if (seen.indexOf(node) >= 0) {
      continue
    }
    seen.push(node)
    out.push(node)
    push(queue, node, "children")
    push(queue, node, "data")
  }
  return out
}

function push(queue, node, property) {
  var list
  try {
    list = node[property]
  } catch (e) {
    return
  }
  if (list === null || list === undefined || list.length === undefined) {
    return
  }
  for (var i = 0; i < list.length; i++) {
    queue.push(list[i])
  }
}

// findAll returns every object under root that the predicate accepts.
function findAll(root, predicate) {
  var all = walk(root)
  var out = []
  for (var i = 0; i < all.length; i++) {
    if (predicate(all[i])) {
      out.push(all[i])
    }
  }
  return out
}

// listModels returns every ListModel under root, identified by its interface
// rather than by its type name — QML gives a test no way to ask "are you a
// ListModel", and duck-typing keeps the probe from caring whether a future
// refactor swaps one for an equivalent model.
function listModels(root) {
  return findAll(root, function (o) {
    return o !== null && typeof o.append === "function"
      && typeof o.setProperty === "function" && typeof o.get === "function"
  })
}

// texts returns the `text` of every currently visible item under root, in
// tree order. Invisible items are excluded on purpose: half the window is
// collapsed forms and unselected tabs, and "the screen says X" is a claim
// about what is on the screen.
function texts(root) {
  var out = []
  var all = walk(root)
  for (var i = 0; i < all.length; i++) {
    var o = all[i]
    if (o === null || o.text === undefined || typeof o.text !== "string") {
      continue
    }
    if (!shown(o)) {
      continue
    }
    out.push(o.text)
  }
  return out
}

// shown reports whether an item is visible all the way up to the root. QML's
// `visible` is already the effective value for a parented item, but an item
// whose parent chain has not been laid out reports true with zero size, and a
// control of no size is not on the screen either.
function shown(item) {
  if (item.visible === undefined) {
    return false
  }
  if (!item.visible) {
    return false
  }
  if (item.width !== undefined && item.height !== undefined) {
    return item.width > 0 && item.height > 0
  }
  return true
}

// saying returns the visible items whose text contains the given fragment.
function saying(root, fragment) {
  return findAll(root, function (o) {
    return o !== null && typeof o.text === "string" && shown(o)
      && o.text.indexOf(fragment) >= 0
  })
}

// says answers whether any visible item's text contains the fragment.
function says(root, fragment) {
  return saying(root, fragment).length > 0
}

// tabStops returns the controls a keyboard user can reach with Tab: visible,
// enabled, and opted into the focus chain. This is the list an accessibility
// assertion is about — `activeFocusOnTab` on an invisible or disabled control
// is not reachability, it is a property nobody can use.
function tabStops(root) {
  return findAll(root, function (o) {
    return o !== null && o.activeFocusOnTab === true && shown(o)
      && (o.enabled === undefined || o.enabled === true)
  })
}

// accessibleName reads the Accessible.name a control publishes, or "" when it
// publishes none. Wrapped because reading an attached property off an object
// that never attached one throws rather than answering undefined.
function accessibleName(item) {
  try {
    var name = item.Accessible.name
    return name === undefined || name === null ? "" : String(name)
  } catch (e) {
    return ""
  }
}

// control finds the single focusable control identified by a fragment of its
// accessible name or of the text it or a visible descendant shows. Buttons in
// this codebase are Rectangles wrapping a Text, so matching only the item that
// owns the string would find the label and not the thing you can press.
//
// Accessible name first, deliberately: it is the identity the control offers
// to a keyboard and screen-reader user, and a test that finds a control the
// same way they do cannot pass on a control they cannot reach.
//
// Returns null when there is no match and throws when there is more than one,
// because a test that silently picked the first of two "Approve" buttons
// would be asserting about whichever one the tree happened to yield first.
function control(root, fragment) {
  var stops = tabStops(root)
  var out = []
  for (var i = 0; i < stops.length; i++) {
    var stop = stops[i]
    if (accessibleName(stop).indexOf(fragment) >= 0
        || (typeof stop.text === "string" && stop.text.indexOf(fragment) >= 0)
        || says(stop, fragment)) {
      out.push(stop)
    }
  }
  if (out.length > 1) {
    throw new Error("more than one focusable control matches " + JSON.stringify(fragment)
      + " — the test cannot tell which one it means")
  }
  return out.length === 0 ? null : out[0]
}

// formField finds the whole form control carrying a given label — the
// JarvixFormField or JarvixFormToggle, not the TextInput inside it.
//
// This matters for "the problem landed on the right field": the daemon's
// sentence is rendered by a sibling of the input, inside the component's own
// column, so asking the input whether it shows the problem would answer no
// however wrong the form was. Matched by duck-typing on `label` + `problem`,
// which is the pair that defines a field in this design system.
function formField(root, label) {
  var found = findAll(root, function (o) {
    return o !== null && o.problem !== undefined && o.label !== undefined
      && String(o.label) === label && shown(o)
  })
  if (found.length > 1) {
    throw new Error("more than one form field is labelled " + JSON.stringify(label))
  }
  return found.length === 0 ? null : found[0]
}

// names returns the accessible name of every reachable control, in tab order,
// for the keyboard-reachability assertions and for failure messages that say
// what *was* there when the expected control was not.
function names(root) {
  var stops = tabStops(root)
  var out = []
  for (var i = 0; i < stops.length; i++) {
    out.push(accessibleName(stops[i]))
  }
  return out
}
