import QtQuick
import QtTest
import "../plugin/omarchy"
import "Probe.js" as Probe

// The two form leaves, instantiated on their own.
//
// tst_forms.qml drives the same rules through the window, which is the test
// that matters — it proves the daemon's `{field, message}` reaches the right
// control over the wire. This file pins the contract at the component instead,
// where every form in the product inherits it: a problem is rendered verbatim
// under its prefix, it replaces the hint rather than stacking with it, and it
// never touches the text.
//
// Both components are pure QtQuick plus the theme singletons — no socket, no
// window, no daemon — so these run in microseconds and fail with a message
// about one control rather than about a screen.
TestCase {
  id: tc
  name: "FormControls"

  when: windowShown
  visible: true
  width: 400
  height: 400

  JarvixFormField {
    id: field
    width: 380
    label: "The fact, in words"
    placeholder: "the staging server is called atlas"
    hint: "Kept until you forget it."
  }

  JarvixFormToggle {
    id: toggle
    width: 380
    y: 200
    label: "Pinned"
    detail: "A pinned fact rides every prompt."
  }

  function init() {
    field.text = ""
    field.problem = ""
    toggle.checked = false
    toggle.problem = ""
  }

  function test_a_field_with_no_problem_shows_its_hint() {
    waitForRendering(field, 250)

    verify(Probe.says(field, "Kept until you forget it."), "the hint is not shown")
    verify(!Probe.says(field, "Problem:"), "a field with no problem announced one")
  }

  function test_a_problem_is_shown_verbatim_under_its_prefix() {
    field.problem = "A fact that long is not one fact."
    waitForRendering(field, 250)

    verify(Probe.says(field, "Problem: A fact that long is not one fact."),
      "the field does not render the daemon's sentence under the Problem prefix; it said: "
      + JSON.stringify(Probe.texts(field)))
  }

  // The hint explains how the field is meant to be used; the problem says why
  // this attempt was refused. Showing both at once buries the second in the
  // first, which is why the component hides the hint rather than stacking.
  function test_a_problem_replaces_the_hint_rather_than_stacking() {
    field.problem = "That fact is already remembered."
    waitForRendering(field, 250)

    verify(Probe.says(field, "Problem: That fact is already remembered."))
    verify(!Probe.says(field, "Kept until you forget it."),
      "the hint stayed alongside the problem, burying it")
  }

  function test_a_problem_never_touches_what_was_typed() {
    field.text = "the staging server is called atlas"

    field.problem = "That fact is already remembered."
    waitForRendering(field, 250)

    compare(field.text, "the staging server is called atlas",
      "setting a problem cleared the field")
  }

  // The toggle says on or off in words, beside the switch. The switch's own
  // position and colour are the fast signal; the word is the one that works
  // without either.
  function test_a_toggle_says_on_or_off_in_words() {
    waitForRendering(toggle, 250)
    verify(Probe.says(toggle, "off"), "an unchecked toggle does not say it is off; it said: "
      + JSON.stringify(Probe.texts(toggle)))

    toggle.checked = true
    waitForRendering(toggle, 250)
    verify(Probe.says(toggle, "on"), "a checked toggle does not say it is on")
  }

  function test_a_toggle_renders_its_problem_the_same_way() {
    toggle.problem = "Pinning is off while memory is disabled."
    waitForRendering(toggle, 250)

    verify(Probe.says(toggle, "Problem: Pinning is off while memory is disabled."),
      "the toggle does not render a problem the way a field does")
    verify(!Probe.says(toggle, "A pinned fact rides every prompt."),
      "the toggle's detail stayed alongside the problem")
  }

  // Both leaves are reachable and identify themselves by their label, which is
  // what every form in the product relies on for keyboard use.
  function test_both_leaves_are_reachable_and_named() {
    waitForRendering(field, 250)

    var input = Probe.control(field, "The fact, in words")
    verify(input !== null, "the field is not in the focus chain")
    compare(Probe.accessibleName(input), "The fact, in words",
      "the field does not announce its label")

    var switcher = Probe.control(toggle, "Pinned")
    verify(switcher !== null, "the toggle is not in the focus chain")
  }
}
