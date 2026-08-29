import QtQuick
import JarvixTest
import "../plugin/omarchy"
import "Probe.js" as Probe

// The collection row's tab order stops moving underneath the keyboard
// (issue #208, the rule #203 established on the confirmation card).
//
// `activeFocusOnTab` is the one focus property Qt will not let a binding drive
// freely: it refuses to clear it on the item that currently holds focus, logs
// "Cannot set activeFocusOnTab to false once item is the active focus item",
// and leaves the property as it was. A row whose buttons bound it to their own
// `visible` therefore ended up describing its tab chain by focus history — the
// button somebody was standing on kept the property it was refused, the
// identical button in the row below did not — and two rows in the same state
// disagreed about what Tab could reach. These rows are how a routine is
// disabled and a fact is forgotten, so that is not a cosmetic disagreement.
//
// Nothing about reachability changed with the fix: `visible` is a distinction
// Qt's focus chain already honours, so a button with no label was never a tab
// stop and still is not. What changed is that the property now says the same
// thing whatever the keyboard was doing.
//
// Both halves are here. The first drives the row on its own, where the state
// change is the row's own `actionLabel` going away — the routines listing's
// "a disabled entry does not offer Run". The second drives the real window,
// where the same flip arrives from above: putting a tab away hides every
// button in its listing without destroying one of them.
JarvixWindowCase {
  id: tc
  name: "CollectionRow"

  // Two rows in the same state, side by side, which is the whole experiment:
  // the only difference between them will be where the keyboard was.
  JarvixCollectionRow {
    id: keyboardRow
    width: 400
    title: "Morning briefing"
    subtitle: "every weekday at 07:30"
    actionLabel: "Run"
    actionName: "Run the morning briefing"
    action2Label: "Disable"
    action2Name: "Disable the morning briefing"
  }

  JarvixCollectionRow {
    id: quietRow
    y: 200
    width: 400
    title: "Evening wrap-up"
    subtitle: "every weekday at 18:00"
    actionLabel: "Run"
    actionName: "Run the evening wrap-up"
    action2Label: "Disable"
    action2Name: "Disable the evening wrap-up"
  }

  function init() {
    keyboardRow.actionLabel = "Run"
    quietRow.actionLabel = "Run"
    FakeDaemon.reset()
  }

  // The row's primary button, captured while the row still offers it —
  // captured, because the point of every assertion below is what that object
  // looks like *after* the button has gone away, and by then it can no longer
  // be found by searching the focus chain.
  function primaryOf(row, name) {
    var button = Probe.control(row, name)
    verify(button !== null, "the row has no reachable " + JSON.stringify(name)
      + "; reachable controls were: " + JSON.stringify(Probe.names(row)))
    return button
  }

  // A row's own state change: the entry is disabled, so the row stops offering
  // Run. On the unfixed row this is where the two buttons part company — the
  // focused one is refused the write and keeps `activeFocusOnTab`, the other
  // loses it — and this is the assertion the unfixed file fails.
  function test_a_rows_button_keeps_its_place_whatever_the_keyboard_was_doing() {
    waitForRendering(tc, 250)
    var focused = primaryOf(keyboardRow, "Run the morning briefing")
    var untouched = primaryOf(quietRow, "Run the evening wrap-up")

    focused.forceActiveFocus()
    verify(focused.activeFocus, "the row's action button would not take focus")

    keyboardRow.actionLabel = ""
    quietRow.actionLabel = ""
    waitForRendering(tc, 250)

    compare(focused.activeFocusOnTab, untouched.activeFocusOnTab,
      "two rows in the same state disagree about their tab chain, because one "
      + "of them had the keyboard on it when the state changed")
    verify(focused.activeFocusOnTab,
      "activeFocusOnTab is not a constant on the collection row's action "
      + "button — it must describe what the row is, not be edited while "
      + "somebody is standing on it")
  }

  // …and the row is out of the tab chain all the same, because that is what
  // `visible` already does: a listing must not collect a dead tab stop per
  // operation it is no longer offering.
  function test_a_button_the_row_no_longer_offers_is_not_reachable() {
    waitForRendering(tc, 250)
    var focused = primaryOf(keyboardRow, "Run the morning briefing")
    focused.forceActiveFocus()

    keyboardRow.actionLabel = ""
    waitForRendering(tc, 250)

    compare(Probe.control(keyboardRow, "Run the morning briefing"), null,
      "a button the row no longer offers is still reachable by Tab; reachable "
      + "controls were: " + JSON.stringify(Probe.names(keyboardRow)))
    verify(Probe.control(keyboardRow, "Disable the morning briefing") !== null,
      "the row's other button left the tab chain with it")
  }

  // The same claim in the real window, where the flip arrives from above
  // rather than from the row: the Memory tab's fact rows are hidden wholesale
  // when the user goes back to Chat, and every one of their buttons sees its
  // effective visibility go false without being destroyed.
  function test_putting_a_tab_away_does_not_edit_its_rows_tab_chain() {
    var win = openWindow({ turns: [] })
    win.openTab("memory")
    settle()

    var listing = FakeDaemon.lastRequest("memory.list")
    verify(listing !== null, "the Memory tab did not ask the daemon for its facts")
    FakeDaemon.reply(listing.id, { enabled: true, count: 1, max: 100, facts: [
      { id: "f1", content: "the staging server is called atlas", pinned: false }
    ]})
    settle()

    var edit = primaryOf(win, "Edit: the staging server is called atlas")
    var forget = primaryOf(win, "Forget: the staging server is called atlas")

    edit.forceActiveFocus()
    verify(edit.activeFocus, "the fact row's Edit button would not take focus")

    win.openTab("chat")
    settle()

    compare(edit.activeFocusOnTab, forget.activeFocusOnTab,
      "two buttons on the same hidden row disagree about their tab chain, "
      + "because the keyboard was on one of them when the tab was put away")
  }
}
