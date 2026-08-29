import QtQuick
import QtTest
import JarvixTest
import "../plugin/omarchy"
import "Probe.js" as Probe

// The fixture every window test shares: one real JarvixWindow, connected to
// the fake daemon, with its snapshot served.
//
// `visible: true` is load-bearing and easy to lose. QtTest's TestCase is an
// invisible Item, and QML `visible` is the *effective* value — so a window
// parented to a default TestCase never becomes visible however you set it,
// `onVisibleChanged` never runs, the socket never connects, and every test
// passes against a window that received nothing. That failure is silent in
// both directions, which is why it is written down here rather than
// rediscovered.
TestCase {
  id: testCase

  when: windowShown
  visible: true

  // Large enough that the tab strip and the composer are laid out rather than
  // clipped: `Probe.tabStops` only counts controls with a real size, so a
  // cramped fixture would make a reachability assertion pass by finding
  // nothing.
  width: 900
  height: 800

  property var win: null

  Component {
    id: windowComponent

    JarvixWindow {}
  }

  // openWindow builds the window and takes it through the same sequence a
  // real one goes through when the user summons it: shown, which connects the
  // socket, which requests the conversation snapshot, which the daemon
  // answers. Everything is synchronous, so by the time this returns the
  // window is in the state a live one reaches a few milliseconds after being
  // opened — with no wait() anywhere.
  function openWindow(snapshot) {
    FakeDaemon.reset()
    testCase.win = testCase.createTemporaryObject(windowComponent, testCase)
    verify(testCase.win !== null, "the conversation window did not instantiate")
    testCase.win.visible = true
    verify(FakeDaemon.lastRequest("conversation.get") !== null,
      "the window did not ask for its snapshot on connect; it is not connected")
    FakeDaemon.serveSnapshot(snapshot)
    return testCase.win
  }

  // Every test gets a fresh window. init() only clears the bus: the window
  // itself is built by the test, because a few of them want a snapshot with
  // history in it.
  function init() {
    FakeDaemon.reset()
  }

  // cleanup() hides the window before it is destroyed. Destruction is
  // deferred in QML, so a window left visible would still be attached and
  // connected while the next test ran, and its traffic would show up in the
  // next test's assertions. Hiding disconnects the socket, and the fake only
  // delivers to connected ones.
  function cleanup() {
    if (testCase.win !== null) {
      testCase.win.visible = false
      testCase.win = null
    }
    // The fake records a contract breach rather than throwing, because a QML
    // exception raised inside a signal handler is swallowed by the engine and
    // would turn a broken test green. Checking here means every test in every
    // file inherits the vocabulary guard without having to remember it.
    compare(FakeDaemon.failure, "", "the fake daemon recorded a contract breach")
  }

  // settle gives the engine the one polish-and-render pass that turns a model
  // change into laid-out items with a size — which is what `Probe` needs,
  // because a control of no size is not on the screen and must not count as
  // reachable.
  //
  // The bound is not a sleep and not a guess at how long anything takes:
  // waitForRendering returns the instant a frame is swapped (about 10ms here),
  // and the bound only applies when the engine decides nothing needs redrawing
  // at all. Under the offscreen platform that happens occasionally on a
  // subtree's first appearance, and the default five-second timeout turned a
  // whole file into a five-second file for no benefit. Nothing asserts on the
  // return value: "a frame came" is not the claim, "the items have a size" is,
  // and every assertion that follows checks that directly.
  function settle() {
    testCase.waitForRendering(testCase.win, 250)
  }

  // press activates a control the way a keyboard user does. Every actionable
  // control in this plugin handles Return and Space; going through the key
  // handler rather than through the underlying function is what makes these
  // tests evidence of keyboard reachability as well as of behaviour.
  function press(item) {
    verify(item !== null, "no such control")
    item.forceActiveFocus()
    verify(item.activeFocus, "the control took focus but did not keep it")
    testCase.keyClick(Qt.Key_Return)
  }

  // typeInto types a string character by character into a text control, which
  // is what makes onTextEdited fire — assigning `text` does not, and a form
  // seeded that way would never reach the draft property the daemon is sent.
  function typeInto(item, text) {
    verify(item !== null, "no such text control")
    item.forceActiveFocus()
    verify(item.activeFocus, "the text control took focus but did not keep it")
    for (var i = 0; i < text.length; i++) {
      testCase.keyClick(text.charAt(i))
    }
  }

  // The transcript model, found by what it holds rather than by where it sits.
  // The window declares five ListModels and gives them private ids; only the
  // transcript carries a `pending` role, so identifying it that way survives a
  // reordering of the declarations and fails loudly if the role ever goes.
  function transcript(win) {
    var models = Probe.listModels(win)
    var found = []
    for (var i = 0; i < models.length; i++) {
      if (models[i].count > 0 && models[i].get(0).pending !== undefined) {
        found.push(models[i])
      }
    }
    compare(found.length, 1,
      "expected exactly one model holding transcript rows; drive an event first")
    return found[0]
  }
}
