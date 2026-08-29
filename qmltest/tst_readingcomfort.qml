import QtQuick
import JarvixTest
import "Probe.js" as Probe

// Reading comfort, executed (issue #121; the binding loop fixed in #203).
//
// `ui.text_size` and `ui.letter_spacing` are readability settings — someone
// turned them up on purpose, for their own eyes — so the failure that matters
// is not "the transcript looks wrong", it is "the setting quietly stopped
// taking effect". That is exactly what a binding loop does: the message
// text's letter spacing used to be computed from `font.pixelSize` on the same
// element, and because `font` is one grouped value, reading a member
// subscribes to the whole group that writing a member notifies. Qt breaks
// such a cycle by dropping a binding, and which binding it drops is an
// evaluation-order detail — so the setting can stop applying, or apply to
// some messages and not others, without anything on screen looking broken.
//
// The runner is what catches the loop itself: scripts/qml-test.sh fails the
// suite on any "Binding loop detected" line from any file, which is general
// and is what goes red on the unfixed window. These tests are the other half
// — that the two settings actually arrive on the rendered message, and that
// the untouched defaults still render what the transcript rendered before the
// settings existed.
//
// Everything goes through the daemon rather than through the window's own
// properties: the path under test is config.get on connect, config.changed
// re-reading it, and loadTypography landing on the delegate. Assigning
// win.chatTextScale by hand would prove nothing about that path.
JarvixWindowCase {
  id: tc
  name: "ReadingComfort"

  readonly property string message: "The staging server is called atlas."

  // QFont keeps letter spacing as a fixed-point number with 1/64 of a pixel
  // of resolution, so reading it back gives the requested value rounded to
  // that grid: 24 x 0.12 comes out of the font as 2.875 rather than 2.88. The
  // tolerance below is that grid and nothing more — it is a property of
  // QFont, not slack in the assertion.
  readonly property real fontGrid: 1 / 64

  // A transcript with one assistant answer in it, which is the thing whose
  // typography these settings govern.
  function openWithAMessage() {
    var win = openWindow({ turns: [{ role: "assistant", text: tc.message, pos: 1 }] })
    settle()
    return win
  }

  // The one item under root whose text is exactly `text`. Exact rather than
  // by fragment because both things this file looks for — the answer and the
  // speaker label beside it — are substrings of other strings in the window.
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

  // The rendered answer — the only text in the window the settings govern.
  function messageText(win) {
    return exactly(win, tc.message, "message")
  }

  // The speaker label above the answer, which carries the design system's own
  // unscaled size. Reading the baseline off the window rather than importing
  // the theme keeps these assertions about the *relationship* #121 promises —
  // message text scales, the chrome beside it does not — instead of about
  // whatever number a particular Omarchy theme happens to configure.
  //
  // Searched from the answer's own delegate rather than from the window: the
  // header says "Jarvix" too, and the label that matters is the one in this
  // row.
  function designSizeLabel(body) {
    return exactly(body.parent, "Jarvix", "speaker label")
  }

  // Answers the config.get the window sent — on connect, or in response to
  // config.changed — with a typography snapshot. Matched on the id the window
  // minted for its own typography request rather than on the most recent
  // config.get, so this stays honest if another surface ever starts reading
  // the registry too.
  function serveTypography(win, textSize, letterSpacing, lineSpacing) {
    var sent = FakeDaemon.requests("config.get")
    verify(sent.length > 0, "the window never asked for the settings snapshot")
    var pending = null
    var ids = []
    for (var i = 0; i < sent.length; i++) {
      ids.push(sent[i].id)
      if (sent[i].id === win.typographyRequestId) {
        pending = sent[i]
      }
    }
    verify(pending !== null, "no outstanding typography request; config.get ids were "
      + JSON.stringify(ids))
    FakeDaemon.reply(pending.id, { fields: [
      { key: "ui.text_size", value: String(textSize) },
      { key: "ui.letter_spacing", value: String(letterSpacing) },
      { key: "ui.line_spacing", value: String(lineSpacing) }
    ]})
    settle()
  }

  // All three knobs, checked on the item that actually renders the answer —
  // and, alongside, that the chrome beside it did not move.
  function expectTypography(win, textSize, letterSpacing, lineSpacing, when) {
    var body = messageText(win)
    var design = designSizeLabel(body).font.pixelSize
    var size = Math.max(1, Math.round(design * textSize))

    compare(body.font.pixelSize, size,
      "ui.text_size did not reach the message text " + when)
    fuzzyCompare(body.font.letterSpacing, size * letterSpacing, tc.fontGrid,
      "ui.letter_spacing did not reach the message text " + when)
    fuzzyCompare(body.lineHeight, lineSpacing, tc.fontGrid,
      "ui.line_spacing did not reach the message text " + when)
  }

  // The defaults are load-bearing in a way the other values are not: they are
  // what every install that never opened the setting renders, so they have to
  // be the pre-#121 hard-coded typography exactly — the design token itself,
  // no extra letter spacing, proportional line height of 1. The Go side pins
  // those three numbers in the registry
  // (TestReadingComfortDefaultsPinTheHardCodedRendering); this pins that the
  // window turns them back into the original rendering rather than merely
  // storing them.
  function test_the_untouched_defaults_render_what_the_transcript_always_did() {
    var win = openWithAMessage()

    compare(win.chatTextScale, 1.0)
    compare(win.chatLetterSpacing, 0.0)
    compare(win.chatLineSpacing, 1.0)

    var body = messageText(win)
    compare(body.font.pixelSize, designSizeLabel(body).font.pixelSize,
      "an untouched install must render the answer at the design size, "
      + "the same size as the speaker label beside it")
    compare(body.font.letterSpacing, 0.0,
      "an untouched install must carry no extra letter spacing at all")
    compare(body.lineHeight, 1.0,
      "an untouched install must keep the proportional line height at 1")
  }

  // The settings apply — both of them, together, on the same element. The
  // "together" is the whole point: size and letter spacing are each computed
  // from their own expression, and the defect this test outlives had one
  // computed from the other.
  function test_both_reading_comfort_settings_reach_the_message() {
    var win = openWithAMessage()

    serveTypography(win, 1.5, 0.12, 1.4)
    expectTypography(win, 1.5, 0.12, 1.4, "after the first change")
  }

  // …and keep applying. A setting that lands once and then stops is the shape
  // of the failure a dropped binding produces, so the claim is walked over
  // several changes — up, down, and back to the defaults, which is what
  // turning the setting off again looks like.
  function test_the_settings_keep_applying_change_after_change() {
    var win = openWithAMessage()

    var rounds = [
      { size: 1.5,  spacing: 0.12, line: 1.4 },
      { size: 2.0,  spacing: 0.25, line: 1.8 },
      { size: 0.85, spacing: 0.05, line: 1.1 },
      { size: 1.0,  spacing: 0.0,  line: 1.0 }
    ]
    for (var i = 0; i < rounds.length; i++) {
      // config.changed is what a save from the Settings form, `jarvix config
      // set`, or the assistant's own settings tool looks like from here: the
      // window re-reads the snapshot rather than being told the new values.
      FakeDaemon.event("config.changed", {})
      serveTypography(win, rounds[i].size, rounds[i].spacing, rounds[i].line)
      expectTypography(win, rounds[i].size, rounds[i].spacing, rounds[i].line,
        "on change " + (i + 1) + " of " + rounds.length)
    }
  }
}
