import QtQuick
import JarvixTest
import "Probe.js" as Probe

// Forms, executed: a refusal from the daemon lands under the field it names,
// and never costs the user what they typed.
//
// The Go guards next door check that each field *asks* for its own problem
// (`problem: win.memoryProblemFor("content")`). They cannot check the round
// trip — that the daemon's `{field, message}` reaches that call, renders as
// the daemon's own sentence, and leaves the draft alone. A form that cleared
// the input on refusal would pass every text scan in the repo and still make
// the user retype their sentence to find out what was wrong with it.
JarvixWindowCase {
  id: tc
  name: "Forms"

  readonly property string fact: "the staging server is called atlas"

  // Opens the Memory tab's add-a-fact form the way a user does — by finding
  // the control and pressing it — and types a draft into it.
  function openFormWithADraft() {
    var win = openWindow({ turns: [] })
    win.openTab("memory")
    settle()

    press(Probe.control(win, "Add a new remembered fact"))
    settle()

    var input = Probe.control(win, "The fact, in words")
    verify(input !== null, "the form has no reachable text field; controls were: "
      + JSON.stringify(Probe.names(win)))
    typeInto(input, tc.fact)
    compare(win.memoryFormContent, tc.fact, "typing did not reach the draft")
    return win
  }

  // Saves and has the daemon refuse, with one problem pinned to a named field.
  function refuse(win, field, message) {
    press(Probe.control(win, "Save the fact"))
    var sent = FakeDaemon.lastRequest("memory.add")
    verify(sent !== null, "the form did not send the fact to the daemon")
    compare(sent.params.content, tc.fact, "the form sent something other than the draft")

    FakeDaemon.replyError(sent.id, -32001, "the fact could not be saved",
      { problems: [{ field: field, message: message }] })
    settle()
  }

  function test_a_field_problem_lands_under_the_field_it_names() {
    var win = openFormWithADraft()
    var message = "A fact that long is not one fact."

    refuse(win, "content", message)

    var field = Probe.formField(win, "The fact, in words")
    verify(field !== null, "the labelled field is gone from the form")
    verify(Probe.says(field, "Problem: " + message),
      "the daemon's refusal is not under the field it named")

    // And nowhere else. The pin toggle is the other control on this form; a
    // problem that painted itself across every field would tell the user to
    // fix things that are not wrong.
    var toggle = Probe.formField(win, "Pinned")
    verify(toggle !== null, "the form has no pin toggle")
    verify(!Probe.says(toggle, "Problem"),
      "a problem the daemon pinned to `content` also appeared on the pin toggle")
  }

  function test_a_refusal_never_clears_what_was_typed() {
    var win = openFormWithADraft()

    refuse(win, "content", "That fact is already remembered.")

    // Both halves, because they can break independently: the draft the daemon
    // would be sent, and the characters actually on the screen.
    compare(win.memoryFormContent, tc.fact,
      "the refusal cleared the draft; the user would have to retype it")
    var input = Probe.control(win, "The fact, in words")
    compare(input.text, tc.fact, "the refusal emptied the field on screen")
  }

  function test_the_form_says_the_problem_in_the_daemons_own_words() {
    var win = openFormWithADraft()
    var message = "Memory is full — forget something first."

    refuse(win, "content", message)

    // Verbatim. The window is a mirror (ADR 0013): a form that reworded a
    // refusal would put two vocabularies in front of the user, and only one of
    // them would match what `jarvix` says on the command line.
    verify(Probe.says(win, "Problem: " + message),
      "the refusal was not rendered verbatim; the screen said: "
      + JSON.stringify(Probe.texts(win)))
  }

  // A refusal that belongs to the whole form — a full store, a transport
  // failure — has no field to sit under. It must still be seen: a message
  // dropped because it named no field is a save that silently did nothing.
  function test_a_whole_form_refusal_is_still_shown() {
    var win = openFormWithADraft()
    var message = "The book is full."

    refuse(win, "", message)

    verify(Probe.says(win, message),
      "a refusal with no field vanished; the screen said: " + JSON.stringify(Probe.texts(win)))
    var field = Probe.formField(win, "The fact, in words")
    verify(!Probe.says(field, "Problem"),
      "a whole-form refusal was pinned to a field it does not name")
  }
}
