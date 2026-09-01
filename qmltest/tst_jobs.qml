import QtQuick
import JarvixTest
import "Probe.js" as Probe

// Work in flight, in the window, executed (issue #221, ADR 0065/0067).
//
// The tab's whole contract is that it words nothing and decides nothing. Where
// a job stands, how long it has been there, what it may touch, what it is
// waiting for, what it has done, and whether it may be answered or stopped are
// all the daemon's answers — composed from the job's ledger and from the same
// offers the verbs refuse with — and rendered here verbatim. jobsqml_test.go
// bans the specific phrases the Go side owns, which stops the known wordings
// being copied in; this file is the general form of the same claim, plus the
// behaviour those sentences are attached to.
//
// The two that matter most are the approval and the refusal. Approving a parked
// gate question must show the verbatim detail a session's confirmation card
// shows and then run THAT step, not a fresh plan (#200); and a refused stop
// comes back as a normal reply carrying its reason, which a window treating
// "not an error" as "done" would narrate as a job it never halted.
JarvixWindowCase {
  id: tc
  name: "Jobs"

  readonly property string disclosure:
    "I run at most 4 jobs at once and keep the last 60, finished ones included."
  readonly property string jobsPath: "/home/u/.local/state/jarvix/jobs.toml"

  // A running job, as jobs.list reports one. `over` replaces fields, so each
  // test states only the difference that is the point of it.
  function job(over) {
    var row = {
      id: "j3", name: "tidy", title: "Tidy",
      state: "Tidy is running — started 4 minutes ago.",
      goal: "You asked for “tidy my downloads”.",
      scope: "It may act inside /home/u/Downloads, using only file.read and file.write.",
      progress: "It has done 3 steps, 1 of which changed something.",
      controls: [{ id: "stop", label: "Stop", name: "Stop the tidy job" }]
    }
    for (var key in over) row[key] = over[key]
    return row
  }

  // A job parked on an approval the gate demanded: the question the session
  // would have asked, the verbatim detail underneath it, and the three controls
  // the daemon offers for it.
  function parkedOnApproval() {
    return tc.job({
      state: "Tidy is waiting on you — parked 4 minutes ago.",
      // The question and the detail are deliberately made of different words.
      // A fixture whose detail was a substring of its question would let the
      // window drop the detail entirely and still satisfy every assertion
      // about it — which is the failure this pair exists to catch.
      question: "I'm about to remove something you can't get back. "
        + "This can't be undone: a deleted file is gone.",
      detail: "rm /home/u/Downloads/old.iso",
      progress: "It has done 3 steps, 1 of which changed something.",
      controls: [
        { id: "approve", label: "Approve",
          name: "Approve what tidy is waiting for and let it carry on" },
        { id: "decline", label: "Say no",
          name: "Say no to what tidy is waiting for, which stops it" },
        { id: "stop", label: "Stop", name: "Stop the tidy job" }
      ]
    })
  }

  // A job parked on a decision only the user can make: the control carries a
  // field, because a question like this is not answered by a boolean.
  function parkedOnDecision() {
    return tc.job({
      state: "Tidy is waiting on you — parked a minute ago.",
      question: "There are two folders called invoices. Which did you mean?",
      controls: [
        { id: "answer", label: "Send your answer",
          name: "Answer what tidy is waiting for and let it carry on",
          words: true, field_label: "Your answer to tidy" },
        { id: "decline", label: "Say no",
          name: "Say no to what tidy is waiting for, which stops it" },
        { id: "stop", label: "Stop", name: "Stop the tidy job" }
      ]
    })
  }

  function listing(rows) {
    return {
      jobs: rows, empty: "You haven't given me any work to do.",
      disclosure: tc.disclosure, path: tc.jobsPath
    }
  }

  function openJobs(view) {
    var win = openWindow({ turns: [] })
    win.openTab("jobs")
    settle()
    var asked = FakeDaemon.lastRequest("jobs.list")
    verify(asked !== null, "the Jobs tab did not ask the daemon what is running")
    FakeDaemon.reply(asked.id, view)
    settle()
    return win
  }

  // The tab exists, is reachable with the keyboard like every other tab, and
  // reading it is one call to one verb.
  function test_the_tab_is_reachable_and_reads_the_work_over_one_verb() {
    var win = openWindow({ turns: [] })
    settle()

    var tab = Probe.control(win, "Jobs")
    verify(tab !== null, "there is no reachable Jobs tab; the tabs were: "
      + JSON.stringify(Probe.names(win)))
    press(tab)
    settle()

    verify(FakeDaemon.lastRequest("jobs.list") !== null,
      "opening the Jobs tab did not read the work in flight")
  }

  // Each row says its name, where it stands, the goal verbatim, the boundary,
  // and what it is waiting for — every one of them a sentence the daemon wrote.
  function test_each_row_says_where_it_stands_what_it_is_for_and_what_bounds_it() {
    var win = openJobs(tc.listing([tc.parkedOnApproval()]))

    verify(Probe.says(win, "Tidy is waiting on you — parked 4 minutes ago."),
      "the row does not say where the job stands in the daemon's own sentence; "
      + "the window shows " + JSON.stringify(Probe.texts(win)))
    verify(Probe.says(win, "“tidy my downloads”"),
      "the row does not carry the goal in the user's own words")
    verify(Probe.says(win, "using only file.read and file.write"),
      "the row does not state the boundary the job is held to")
    verify(Probe.says(win, "It has done 3 steps, 1 of which changed something."),
      "the row does not say how much the job has done")
    verify(Probe.says(win, "There are two folders") === false,
      "the fixture leaked another job's question into this one")
    verify(Probe.says(win, "I'm about to remove something you can't get back."),
      "the row does not say what the job is waiting for")
  }

  // #200's contract, first half: approving from the window shows the same
  // verbatim detail a session's confirmation card shows. The detail is the
  // exact thing being approved, and it is what a person actually judges — a
  // question without it is a question about a paraphrase.
  function test_a_parked_approval_shows_the_cards_own_verbatim_detail() {
    var win = openJobs(tc.listing([tc.parkedOnApproval()]))

    verify(Probe.says(win, "rm /home/u/Downloads/old.iso"),
      "the approval does not show the verbatim detail the confirmation card "
      + "would; the window shows " + JSON.stringify(Probe.texts(win)))
    verify(Probe.says(win, "This can't be undone"),
      "the one-way warning the gate put on the question is not on the screen")
    // The detail gets the monospace treatment this design system reserves for
    // values that must not be reworded — the confirmation card's own block. A
    // detail rendered as prose beside the question would read as a paraphrase.
    var block = Probe.saying(win, "rm /home/u/Downloads/old.iso")
    compare(block.length, 1, "the verbatim detail is on the screen more than once")
    compare(String(block[0].font.family), "monospace",
      "the verbatim detail is not in the block reserved for values that must "
      + "not be reworded")
  }

  // #200's contract, second half: approving is one jobs.answer carrying a
  // literal yes. Nothing here names a step or a tool, because the step the job
  // parked on is kept whole daemon-side — that is what makes this a resumption
  // from a checkpoint rather than a fresh plan.
  function test_approving_answers_the_job_and_re_reads_rather_than_marking_the_row() {
    var win = openJobs(tc.listing([tc.parkedOnApproval()]))

    var approve = Probe.control(win, "Approve what tidy is waiting for")
    verify(approve !== null, "a parked approval offers no way to approve it; "
      + "reachable controls were: " + JSON.stringify(Probe.names(win)))
    press(approve)
    settle()

    var answered = FakeDaemon.lastRequest("jobs.answer")
    verify(answered !== null, "approving asked the daemon nothing")
    compare(answered.params.name, "tidy", "the window answered a different job")
    compare(answered.params.approved, true, "approving did not send a yes")
    verify(answered.params.answer === undefined || answered.params.answer === "",
      "approving invented words the user did not type")

    FakeDaemon.reply(answered.id, { done: true, refused: false, name: "tidy",
      spoken: "Tidy is running: 3 steps, 1 of which changed something." })
    settle()

    verify(Probe.says(win, "Tidy is running: 3 steps"),
      "the daemon's account of what happened next is not on the screen")

    // The re-read is the point: where the job stands after being answered is
    // the store's answer — it resumes from its checkpoint and may park again on
    // the very next step — and a window that marked its own row would be
    // asserting a state nobody looked at.
    var again = FakeDaemon.lastRequest("jobs.list")
    verify(again !== null, "the window did not re-read the work after answering")
    FakeDaemon.reply(again.id, tc.listing([tc.job({})]))
    settle()

    compare(Probe.control(win, "Approve what tidy is waiting for"), null,
      "a job that has been answered is still offering to be approved")
  }

  // A decision needs the user's own words, so that control carries a field —
  // and the words travel as the answer rather than being turned into a yes.
  function test_a_decision_is_answered_in_the_users_own_words() {
    var win = openJobs(tc.listing([tc.parkedOnDecision()]))

    var field = Probe.control(win, "Your answer to tidy")
    verify(field !== null, "a job waiting on a decision offers nowhere to answer it; "
      + "reachable controls were: " + JSON.stringify(Probe.names(win)))
    typeInto(field, "the one in Documents")
    settle()

    press(Probe.control(win, "Answer what tidy is waiting for"))
    settle()

    var answered = FakeDaemon.lastRequest("jobs.answer")
    verify(answered !== null, "answering the decision asked the daemon nothing")
    compare(answered.params.approved, true,
      "an answer to a decision was not carried through as one")
    compare(answered.params.answer, "the one in Documents",
      "the user's own words did not reach the job")
  }

  // An approval is a yes about an action already shown, so there is nothing to
  // type — a field in front of it would invite an answer nothing reads.
  function test_an_approval_offers_no_field_to_type_into() {
    var win = openJobs(tc.listing([tc.parkedOnApproval()]))

    compare(Probe.control(win, "Your answer to tidy"), null,
      "a gate approval offers a text field, though the daemon marked no control "
      + "as needing words; reachable controls were: " + JSON.stringify(Probe.names(win)))
  }

  // Stopping is one jobs.stop, and what the job had done and had not comes back
  // from the account rather than from this window's assumption.
  function test_stopping_a_running_job_asks_by_name_and_shows_what_it_had_done() {
    var win = openJobs(tc.listing([tc.job({})]))

    var stop = Probe.control(win, "Stop the tidy job")
    verify(stop !== null, "a running job offers no way to stop it; "
      + "reachable controls were: " + JSON.stringify(Probe.names(win)))
    press(stop)
    settle()

    var stopped = FakeDaemon.lastRequest("jobs.stop")
    verify(stopped !== null, "pressing Stop asked the daemon nothing")
    compare(stopped.params.name, "tidy", "the window stopped a different job")

    FakeDaemon.reply(stopped.id, { done: true, refused: false, name: "tidy",
      spoken: "Tidy stopped. I did tidy the archives. You stopped it." })
    settle()

    verify(Probe.says(win, "Tidy stopped. I did tidy the archives."),
      "the daemon's account of what the job had done is not on the screen")

    var again = FakeDaemon.lastRequest("jobs.list")
    verify(again !== null, "the window did not re-read the work after stopping")
    FakeDaemon.reply(again.id, tc.listing([tc.job({
      state: "Tidy stopped just now.",
      report: "There is 1 step I started and never saw the end of, so I can't tell "
        + "you whether it happened. I did tidy the archives. You stopped it.",
      controls: []
    })]))
    settle()

    verify(Probe.says(win, "I started and never saw the end of"),
      "the report does not show the unverified step as unverified")
    compare(Probe.control(win, "Stop the tidy job"), null,
      "a job that has already stopped is still offering to be stopped")
  }

  // A job parked on a boundary is visibly non-answerable — no control at all —
  // and says why in the daemon's own clause rather than leaving a dead button.
  function test_a_job_no_answer_can_move_offers_nothing_and_says_why() {
    var win = openJobs(tc.listing([tc.job({
      state: "Tidy has stopped and needs you — parked 2 hours ago.",
      question: "I stopped without doing it: it would have touched /etc/hosts, "
        + "which is outside /home/u/Downloads.",
      why: "Tidy stopped because I stopped without doing it: it would have touched "
        + "/etc/hosts, which is outside /home/u/Downloads, which isn't something "
        + "I can carry on from.",
      controls: []
    })]))

    compare(Probe.control(win, "Approve what tidy is waiting for"), null,
      "a job parked outside its boundary still offers an approval that could only refuse")
    compare(Probe.control(win, "Say no to what tidy is waiting for"), null,
      "a job parked outside its boundary still offers a decline")
    // And no button of any wording, because the daemon offered no control at
    // all: a label written in the window would put one back with no verb behind
    // it, and the accessible name would be empty, so a keyboard user would find
    // a control that announces nothing and does nothing.
    var labels = ["Approve", "Say no", "Stop", "Send your answer"]
    for (var i = 0; i < labels.length; i++) {
      verify(!Probe.says(win, labels[i]),
        "the row shows a " + JSON.stringify(labels[i]) + " control, though the "
        + "daemon offered none; the window shows " + JSON.stringify(Probe.texts(win)))
    }
    verify(Probe.says(win, "which isn't something I can carry on from"),
      "the row does not say why no answer will move the job")
  }

  // A finished job's report is the ledger-derived account, and an unverified
  // step leads it. Nothing about it is composed by a model, here or anywhere.
  function test_a_finished_job_shows_the_ledger_derived_account() {
    var win = openJobs(tc.listing([tc.job({
      state: "Tidy finished 20 minutes ago.",
      progress: "It has done 4 steps, 2 of which changed something, and 1 I can't "
        + "confirm either way.",
      report: "There is 1 step I started and never saw the end of, so I can't tell "
        + "you whether it happened. I did archive the old installers and empty the "
        + "trash. I couldn't reach the network share.",
      controls: []
    })]))

    verify(Probe.says(win, "I started and never saw the end of"),
      "the report does not lead with the step it could not confirm")
    verify(Probe.says(win, "I couldn't reach the network share"),
      "the report does not say what the job could not do")
    compare(Probe.control(win, "Stop the tidy job"), null,
      "a finished job still offers to be stopped")
  }

  // The refusal. A declined stop comes back as a normal reply carrying its own
  // sentence, and the window must show that sentence and claim nothing else.
  function test_a_refused_action_shows_its_own_sentence_and_claims_nothing() {
    var view = tc.listing([tc.job({})])
    var win = openJobs(view)

    press(Probe.control(win, "Stop the tidy job"))
    settle()

    var stopped = FakeDaemon.lastRequest("jobs.stop")
    var refusal = "Tidy has already finished."
    FakeDaemon.reply(stopped.id, { done: false, refused: true, spoken: refusal })
    settle()

    verify(Probe.says(win, refusal),
      "the refusal's own sentence is not on the screen; the window shows "
      + JSON.stringify(Probe.texts(win)))

    var again = FakeDaemon.lastRequest("jobs.list")
    FakeDaemon.reply(again.id, view)
    settle()

    // Nothing anywhere says it worked. These are the shapes a window reaches
    // for when it treats a reply as a success because it was not an error.
    var shown = JSON.stringify(Probe.texts(win))
    var claims = ["Tidy stopped", "I did ", "You stopped it"]
    for (var i = 0; i < claims.length; i++) {
      verify(shown.indexOf(claims[i]) < 0,
        "the window said " + JSON.stringify(claims[i])
        + " after a refusal, without being told anything of the kind")
    }
  }

  // An empty listing still has something to say, and says the daemon's sentence
  // rather than a placeholder of its own.
  function test_no_work_at_all_says_so_in_the_daemons_words() {
    var win = openJobs(tc.listing([]))

    verify(Probe.says(win, "You haven't given me any work to do."),
      "an empty listing says nothing; the window shows " + JSON.stringify(Probe.texts(win)))
    verify(Probe.says(win, tc.disclosure),
      "the listing does not disclose its bounds in the daemon's own sentence")
    verify(Probe.says(win, tc.jobsPath),
      "the listing does not say where the jobs file is")
  }

  // Driven by the event, not by a poll: a job parking or finishing while the
  // tab is open updates it without anybody touching anything.
  function test_the_listing_re_reads_when_the_daemon_says_a_job_changed() {
    var win = openJobs(tc.listing([tc.job({})]))
    var before = FakeDaemon.requests("jobs.list").length

    FakeDaemon.event("jobs.changed", {
      action: "parked", id: "j3", name: "tidy", state: "parked",
      steps: 4, waiting_on: "approval"
    })
    settle()

    compare(FakeDaemon.requests("jobs.list").length, before + 1,
      "the listing did not re-read when the daemon said a job had changed")
  }
}
