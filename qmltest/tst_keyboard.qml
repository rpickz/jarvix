import QtQuick
import JarvixTest
import "Probe.js" as Probe

// Keyboard reachability, executed.
//
// A text scan can see `activeFocusOnTab: true` in the source. It cannot see
// whether the control is visible, enabled, laid out, or reachable at all — and
// `activeFocusOnTab` on a control of no size is a property nobody can use. The
// only way to know is to run the window and ask it what a Tab press would find.
//
// The tab strip is the load-bearing case: it is a Flow of eleven surfaces, and
// the window is usable from the keyboard alone only if Left/Right walk it,
// Enter selects, Ctrl+Tab cycles from anywhere, and Escape comes home.
JarvixWindowCase {
  id: tc
  name: "Keyboard"

  function test_every_tab_is_reachable_and_says_which_one_it_is() {
    var win = openWindow({ turns: [] })
    settle()

    var reachable = Probe.names(win)
    for (var i = 0; i < win.tabs.length; i++) {
      var label = win.tabs[i].label
      verify(Probe.control(win, label) !== null,
        "the " + label + " tab is not reachable from the keyboard; reachable controls were: "
        + JSON.stringify(reachable))
    }

    // The current tab announces itself in words rather than by the underline
    // colour alone, so a reader who cannot see the accent still knows where
    // they are.
    verify(reachable.indexOf("Chat, current tab") >= 0,
      "the selected tab does not say it is the current one: " + JSON.stringify(reachable))
  }

  function test_the_primary_controls_are_reachable() {
    var win = openWindow({ turns: [] })
    settle()

    // The three things the window exists for, beyond the tabs: type a
    // question, start a fresh conversation, and read the answer.
    verify(Probe.control(win, "Ask Jarvix") !== null, "the composer is not reachable")
    verify(Probe.control(win, "New chat") !== null, "the new-chat control is not reachable")
    verify(Probe.tabStops(win).length >= win.tabs.length + 2,
      "fewer reachable controls than there are tabs plus the composer and new chat")
  }

  function test_the_arrow_keys_walk_the_tab_strip() {
    var win = openWindow({ turns: [] })
    settle()

    var chat = Probe.control(win, "Chat, current tab")
    chat.forceActiveFocus()
    verify(chat.activeFocus, "the first tab would not take focus")

    keyClick(Qt.Key_Right)
    compare(win.currentTab, win.tabs[1].id, "Right did not move to the next tab")

    keyClick(Qt.Key_Left)
    compare(win.currentTab, win.tabs[0].id, "Left did not move back")

    // And it wraps rather than stopping, so a keyboard user never has to know
    // which end of the strip they are at.
    keyClick(Qt.Key_Left)
    compare(win.currentTab, win.tabs[win.tabs.length - 1].id,
      "Left from the first tab did not wrap to the last")
  }

  function test_enter_selects_the_focused_tab() {
    var win = openWindow({ turns: [] })
    settle()

    press(Probe.control(win, "Memory"))

    compare(win.currentTab, "memory", "Enter on a focused tab did not open it")
  }

  // Ctrl+Tab is a window shortcut rather than a key handler on the strip, so
  // it works from wherever focus happens to be — including the composer, where
  // a plain Tab would be typing.
  function test_ctrl_tab_cycles_the_tabs_from_anywhere() {
    var win = openWindow({ turns: [] })
    settle()

    var composer = Probe.control(win, "Ask Jarvix")
    composer.forceActiveFocus()
    verify(composer.activeFocus, "the composer would not take focus")

    keyClick(Qt.Key_Tab, Qt.ControlModifier)
    compare(win.currentTab, win.tabs[1].id, "Ctrl+Tab did not move forward from the composer")

    keyClick(Qt.Key_Tab, Qt.ControlModifier | Qt.ShiftModifier)
    compare(win.currentTab, win.tabs[0].id, "Ctrl+Shift+Tab did not move back")
  }

  function test_escape_comes_back_to_the_conversation() {
    var win = openWindow({ turns: [] })
    win.openTab("approvals")
    settle()
    compare(win.currentTab, "approvals")

    keyClick(Qt.Key_Escape)

    compare(win.currentTab, "chat",
      "Escape from another surface did not come back to the conversation")
  }

  // Enter in the composer sends; Shift+Enter deliberately does not, so a
  // half-typed multi-line question is never sent by a stray newline.
  function test_enter_sends_the_typed_question_and_shift_enter_does_not() {
    var win = openWindow({ turns: [] })
    settle()

    var composer = Probe.control(win, "Ask Jarvix")
    typeInto(composer, "what is recursion")

    keyClick(Qt.Key_Return, Qt.ShiftModifier)
    compare(FakeDaemon.lastRequest("session.text"), null,
      "Shift+Enter sent the question; it must not")

    keyClick(Qt.Key_Return)
    var sent = FakeDaemon.lastRequest("session.text")
    verify(sent !== null, "Enter did not send the typed question")
    compare(sent.params.text, "what is recursion")
  }

  // A question sent while the daemon was going away must come back to the
  // composer rather than vanishing: a typed sentence is the one thing in this
  // window the user cannot recover from anywhere else.
  function test_a_question_lost_to_a_dropped_connection_comes_back() {
    var win = openWindow({ turns: [] })
    settle()

    var composer = Probe.control(win, "Ask Jarvix")
    typeInto(composer, "what is recursion")
    keyClick(Qt.Key_Return)
    compare(composer.text, "", "the composer did not clear on send")

    FakeDaemon.close()

    compare(composer.text, "what is recursion",
      "the question died with the connection instead of coming back to be re-sent")
  }
}
