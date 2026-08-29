import QtQuick
import JarvixTest
import "Probe.js" as Probe

// The confirmation card (ADR 0014, ADR 0053), executed.
//
// The card is where a permission decision is actually made, and its payload is
// the security-relevant part: the window may say *how far* an approval reaches
// — once, this conversation, always — and must never say *what* it covers. The
// rule to write is the one the daemon derived and published on the card; a
// client that named its own pattern would be a client that could widen a grant
// the daemon never offered.
//
// approvalsqml_test.go already forbids the words `pattern`, `rule` and
// `command` inside answerConfirmation's body. That is a claim about the source.
// This is the claim about the wire: press the button, read the frame.
JarvixWindowCase {
  id: tc
  name: "ConfirmationCard"

  // The pattern the daemon derived for this command. Every "never a pattern"
  // assertion below is against this exact string, so a client that started
  // echoing it back would be caught wherever it put it in the payload.
  readonly property string rule: "ls *"

  function ask() {
    var win = openWindow({ turns: [] })
    FakeDaemon.event("tool.confirmation_required", {
      summary: "Run a shell command?",
      command: "ls -la /tmp",
      timeout_sec: 30,
      remember_pattern: tc.rule,
      remember_reason: ""
    })
    settle()
    return win
  }

  // The one frame the card sent, decoded. Exactly one, because a card that
  // answered twice would have raced the daemon's own resolution.
  function answer() {
    var sent = FakeDaemon.requests("session.confirm")
    compare(sent.length, 1, "the card must send exactly one session.confirm")
    return sent[0].params
  }

  function test_the_card_offers_three_ways_to_approve_and_one_to_decline() {
    var win = ask()

    // Found by accessible name, which is how a keyboard or screen-reader user
    // finds them — so this doubles as the reachability assertion for the most
    // consequential controls in the product.
    verify(Probe.control(win, "Approve — run the command") !== null,
      "no approve-once control; reachable controls were: " + JSON.stringify(Probe.names(win)))
    verify(Probe.control(win, "Approve for this conversation only") !== null,
      "no conversation-scoped approve control")
    verify(Probe.control(win, "Approve and do not ask again") !== null,
      "no permanent approve control")
    verify(Probe.control(win, "Decline — do not run the command") !== null,
      "no decline control")

    // The card says what it is asking about, in the daemon's own words, and
    // says what each scope costs. A permission dialog that only signalled by
    // colour would be unusable to the people most at risk from it.
    verify(Probe.says(win, "Run a shell command?"), "the card does not show the question")
    verify(Probe.says(win, "ls -la /tmp"), "the card does not show the command")
    verify(Probe.says(win, tc.rule), "the card does not show the rule it would add")
    verify(Probe.says(win, "never written to disk"),
      "the card does not say that the conversation-only scope is not saved")
  }

  function test_approving_once_sends_no_scope_at_all() {
    var win = ask()

    press(Probe.control(win, "Approve — run the command"))

    var params = answer()
    compare(params.approved, true)
    compare(params.remember, undefined,
      "approve-once must not carry a scope; an empty one would still be a grant")
  }

  function test_approving_permanently_sends_a_scope_word_and_never_the_rule() {
    var win = ask()

    press(Probe.control(win, "Approve and do not ask again"))

    var params = answer()
    compare(params.approved, true)
    compare(params.remember, "always", "the scope must be the word, not the reach")
    verify(JSON.stringify(params).indexOf(tc.rule) < 0,
      "the card sent the pattern back: " + JSON.stringify(params)
      + " — the rule to write is the daemon's, and a client that names one can widen it")
  }

  function test_approving_for_this_conversation_sends_a_scope_word_too() {
    var win = ask()

    press(Probe.control(win, "Approve for this conversation only"))

    var params = answer()
    compare(params.approved, true)
    compare(params.remember, "conversation")
    verify(JSON.stringify(params).indexOf(tc.rule) < 0, "the card sent the pattern back")
  }

  function test_declining_sends_a_refusal_and_no_scope() {
    var win = ask()

    press(Probe.control(win, "Decline — do not run the command"))

    var params = answer()
    compare(params.approved, false)
    compare(params.remember, undefined, "a refusal cannot carry a scope to remember")
  }

  // The card is answerable without ever reaching for the mouse, and each key
  // sends the same payload its button does. Y and N are the whole vocabulary
  // when the daemon offered no rule; A and C appear only alongside one.
  function test_the_keyboard_answers_the_card() {
    var keys = [
      { key: Qt.Key_Y, approved: true,  remember: undefined },
      { key: Qt.Key_N, approved: false, remember: undefined },
      { key: Qt.Key_A, approved: true,  remember: "always" },
      { key: Qt.Key_C, approved: true,  remember: "conversation" }
    ]
    for (var i = 0; i < keys.length; i++) {
      var win = ask()
      var card = Probe.control(win, "Permission question:")
      verify(card !== null, "the card itself is not in the focus chain")
      card.forceActiveFocus()
      verify(card.activeFocus, "the card would not take focus")

      keyClick(keys[i].key)

      var params = answer()
      compare(params.approved, keys[i].approved, "wrong answer for key " + i)
      compare(params.remember, keys[i].remember, "wrong scope for key " + i)
      verify(JSON.stringify(params).indexOf(tc.rule) < 0, "a key sent the pattern back")

      // Each pass needs its own window and its own bus: the fixture's cleanup
      // only runs between test *functions*, not between loop iterations.
      win.visible = false
      tc.win = null
      FakeDaemon.reset()
    }
  }

  // The countdown is the card's only moving part, and it is words. A card that
  // conveyed "you are running out of time" by turning red would say nothing at
  // all to a reader who cannot see the colour.
  function test_the_time_left_is_said_in_words() {
    var win = ask()
    verify(Probe.says(win, "Up to 30s to answer"),
      "the card does not say how long there is before the daemon's clock starts")

    FakeDaemon.event("tool.confirmation_deadline", { deadline_ms: Date.now() + 12000 })
    settle()

    verify(Probe.says(win, "s left to answer — no answer declines"),
      "the running countdown does not say what happens when it runs out")
  }

  // The resolution comes from the daemon, not from the click. A card that
  // resolved itself would show "Approved" for a decision the daemon may have
  // already taken the other way — the race the reply handler deliberately
  // swallows.
  function test_the_outcome_is_the_daemons_word_not_the_clicks() {
    var win = ask()
    var turns = transcript(win)
    compare(turns.get(0).outcome, "", "the card starts unresolved")

    press(Probe.control(win, "Approve — run the command"))
    compare(turns.get(0).outcome, "",
      "the card resolved on the click; it must wait for the daemon to say so")

    FakeDaemon.event("tool.confirmed", { tool: "shell" })
    compare(turns.get(0).outcome, "Approved")

    settle()
    verify(Probe.says(win, "Approved"), "the resolved card does not say what happened")
  }

  function test_a_declined_question_says_why_in_words() {
    var win = ask()

    FakeDaemon.event("tool.declined", { tool: "shell", source: "timeout" })
    settle()

    var turns = transcript(win)
    compare(turns.get(0).outcome, "Declined — timed out after 30s")
    verify(Probe.says(win, "Declined — timed out after 30s"),
      "a timed-out question does not say on screen that it timed out")
  }
}
