import QtQuick
import JarvixTest
import qs.Commons
import "Probe.js" as Probe

// The thinking control, executed (issue #159; the note's size fixed in #208).
//
// The control offers a trade — a quicker answer or a better one — and the note
// beneath it is what the window says when the trade could not be taken. Both
// halves are the daemon's, verbatim: the levels come from the snapshot and the
// refusal is the sentence `thinking.set` answered with, so the click and the
// spoken phrase refuse in the same words.
//
// The note also carries the one thing in this file that is not about wording.
// It asks for a smaller size than the text around it, and for a while it asked
// for `Style.font.small`, which the theme has never defined. A missing member
// on a grouped theme object is not an error in QML: the binding evaluates to
// undefined, the engine logs "Unable to assign [undefined] to int" and leaves
// the property at Qt's own default. So the note rendered at whatever size the
// platform's default font happened to be — not the design's — and nothing was
// red anywhere. scripts/qml-test.sh now fails the suite on that warning from
// any file; this file pins the other half, that the note really does render at
// the token the theme defines for it.
JarvixWindowCase {
  id: tc
  name: "Thinking"

  // What `thinking.get` and the conversation snapshot serve: the tiers this
  // machine can offer, one of them not configured. The shape is the daemon's
  // (#159) — tier, label, description, available — and the window renders it
  // without deciding anything about it.
  readonly property var levels: [
    { tier: "instant", label: "Quick", description: "answers immediately",
      available: true },
    { tier: "deep", label: "Deep", description: "thinks it through",
      available: false }
  ]

  readonly property string refusal:
    "Deep needs a model this computer has not been given."

  function openWithTiers() {
    var win = openWindow({ turns: [], thinking: "instant",
      thinking_label: "Quick", thinking_levels: tc.levels })
    settle()
    return win
  }

  // Presses the unconfigured level and has the daemon refuse it. An
  // unconfigured level is still pressable on purpose: pressing it is how the
  // user finds out why it is not there, in one sentence, rather than by asking
  // a question and getting a worse answer than they expected.
  function refuseDeep(win) {
    press(Probe.control(win, "Deep — thinks it through"))
    var sent = FakeDaemon.lastRequest("thinking.set")
    verify(sent !== null, "pressing a level did not reach the daemon; the window sent "
      + JSON.stringify(Probe.names(win)))
    compare(sent.params.thinking, "deep", "the window asked for the wrong level")

    FakeDaemon.replyError(sent.id, -32001, tc.refusal)
    settle()
  }

  // The one item under root whose text is exactly `text`. Exact rather than by
  // fragment because the note and the level's own description are both prose
  // and one can contain the other.
  function exactly(root, text, what) {
    var candidates = Probe.saying(root, text)
    var found = []
    for (var i = 0; i < candidates.length; i++) {
      if (candidates[i].text === text && candidates[i].font !== undefined) {
        found.push(candidates[i])
      }
    }
    compare(found.length, 1, "expected exactly one " + what + " showing "
      + JSON.stringify(text) + "; the window shows " + JSON.stringify(Probe.texts(root)))
    return found[0]
  }

  function test_a_refused_level_is_explained_in_the_daemons_own_words() {
    var win = openWithTiers()

    refuseDeep(win)

    verify(Probe.says(win, tc.refusal),
      "the refusal was not shown where the control stands; the screen said: "
      + JSON.stringify(Probe.texts(win)))
  }

  // The size the theme defines for it, read from the theme rather than
  // hard-coded: the number is Omarchy's to choose and a test that asserted 12
  // would be a test of somebody else's file. What is being pinned is that the
  // note asks for a token that *exists* — `Style.font.small` never did, and the
  // window silently rendered this note at Qt's default instead.
  //
  // This is the assertion the unfixed window fails.
  function test_the_note_renders_at_the_size_the_theme_defines_for_it() {
    var win = openWithTiers()
    refuseDeep(win)

    var note = exactly(win, tc.refusal, "thinking note")
    compare(note.font.pixelSize, Style.font.bodySmall,
      "the thinking note is not rendered at the theme's bodySmall token — a "
      + "binding that names a member the theme does not have leaves the "
      + "property at Qt's default and says so only in a warning")
  }

  // …and the design claim the token was chosen for: the note reads as a note.
  // Kept separate from the assertion above because they fail for different
  // reasons — this one goes red if somebody re-points the note at `subtitle`,
  // which is a real token and would produce no warning at all.
  function test_the_note_reads_smaller_than_the_control_it_explains() {
    var win = openWithTiers()
    refuseDeep(win)

    var note = exactly(win, tc.refusal, "thinking note")
    var label = exactly(win, "Thinking: Quick", "thinking label")
    verify(note.font.pixelSize < label.font.pixelSize,
      "the note beneath the thinking control is not smaller than the control's "
      + "own label: note " + note.font.pixelSize + "px, label "
      + label.font.pixelSize + "px")
  }
}
