import QtQuick
import JarvixTest
import "Probe.js" as Probe

// The account of work in the window, executed (issue #210, ADR 0064/0066).
//
// The tab's whole contract is that it words nothing and decides nothing. What
// changed, when it changed, whether it can be put back, why it cannot, what a
// job is called and whether the whole job may go back are all the daemon's
// answers, composed against the account and the permission gate and rendered
// here verbatim. accountqml_test.go bans the specific phrases the Go side owns,
// which stops the known wordings being copied in; this file is the general form
// of the same claim, plus the behaviour those sentences are attached to.
//
// The one that matters most is the refusal. `undo.apply` answers a declined
// reversal with a normal reply carrying a sentence — Jarvix declining is not a
// fault — and a window that treated "not done" as "done" would be narrating a
// restoration that never happened, which is #71's scar with the user's files
// underneath it.
JarvixWindowCase {
  id: tc
  name: "Account"

  readonly property string disclosure: "I keep the last 100 actions; 3 older ones have dropped off."
  readonly property string undoPath: "/home/u/.local/state/jarvix/undo.toml"

  // One group holding a single action that belonged to no job — the shape the
  // daemon sends for anything asked for in a conversation.
  function alone(action) {
    return { job: "", heading: "", can_undo: false, actions: [action] }
  }

  // The envelope, with `actions` derived from the groups so the flat list and
  // the arrangement cannot disagree in a fixture the way they cannot on the
  // wire.
  function account(groups) {
    var actions = []
    for (var i = 0; i < groups.length; i++) {
      var held = groups[i].actions || []
      for (var j = 0; j < held.length; j++) {
        actions.push(held[j])
      }
    }
    return {
      actions: actions, groups: groups, bound: 100, forgotten: 3,
      disclosure: tc.disclosure,
      empty: "I haven't changed anything on this machine.",
      path: tc.undoPath
    }
  }

  // A reversible file change, as undo.list reports one.
  function reversibleRow() {
    return {
      id: "a2", at: "2026-08-29T09:56:00Z", when: "4 minutes ago",
      tool: "config.write_entry", summary: "saved the routine “morning”",
      target: "/home/u/.config/jarvix/config.toml",
      reversible: true, can_undo: true, state: "I can put this back."
    }
  }

  // A shell command: recorded, described, never falsely promised as undoable.
  function oneWayRow() {
    return {
      id: "a1", at: "2026-08-29T09:40:00Z", when: "20 minutes ago",
      tool: "shell.run", summary: "ran rm -rf ./build",
      reversible: false, can_undo: false,
      why: "a command that has run has run",
      state: "I can't put this back — a command that has run has run."
    }
  }

  function openAccount(view) {
    var win = openWindow({ turns: [] })
    win.openTab("account")
    settle()
    var asked = FakeDaemon.lastRequest("undo.list")
    verify(asked !== null, "the Account tab did not ask the daemon for the account")
    FakeDaemon.reply(asked.id, view)
    settle()
    return win
  }

  // The tab exists, is reachable with the keyboard like every other tab, and
  // reading it is one call to the verb #201 already shipped.
  function test_the_tab_is_reachable_and_reads_the_account_over_the_existing_verb() {
    var win = openWindow({ turns: [] })
    settle()

    var tab = Probe.control(win, "Account")
    verify(tab !== null, "there is no reachable Account tab; the tabs were: "
      + JSON.stringify(Probe.names(win)))
    press(tab)
    settle()

    var asked = FakeDaemon.lastRequest("undo.list")
    verify(asked !== null, "opening the Account tab did not read the account")
  }

  // Newest first, each row saying what changed and what its standing is — and
  // the standing is a sentence the daemon wrote, not a state this file worded
  // from a boolean.
  function test_each_row_says_what_changed_and_where_it_stands() {
    var win = openAccount(account([tc.alone(tc.reversibleRow()), tc.alone(tc.oneWayRow())]))

    verify(Probe.says(win, "saved the routine “morning”"),
      "the account does not say what the config write changed")
    verify(Probe.says(win, "ran rm -rf ./build"),
      "the account does not say what the shell command was")
    verify(Probe.says(win, "I can put this back."),
      "a reversible row does not say that it can be put back")
    verify(Probe.says(win, "4 minutes ago"),
      "the row does not say when it happened in the daemon's own phrase")
    verify(Probe.says(win, "/home/u/.config/jarvix/config.toml"),
      "the row does not name what it touched")
  }

  // A row that cannot go back is visibly non-offerable — no control at all —
  // and says why in the daemon's clause rather than leaving a dead button that
  // would refuse when pressed.
  function test_a_row_that_cannot_go_back_offers_nothing_and_says_why() {
    var win = openAccount(account([tc.alone(tc.oneWayRow())]))

    compare(Probe.control(win, "Put back: ran rm -rf ./build"), null,
      "an irreversible action still offers a control that could only refuse; "
      + "reachable controls were: " + JSON.stringify(Probe.names(win)))
    verify(Probe.says(win, "a command that has run has run"),
      "the account does not say why the command cannot be taken back")
  }

  // The gate is the other half of the same rule: a record that is perfectly
  // reversible, whose tool identity the policy denies, is withheld here rather
  // than refused when pressed. The daemon computed can_undo from the very
  // Undoer the reversal would run.
  function test_a_reversal_the_gate_would_refuse_is_not_offered_either() {
    var denied = tc.reversibleRow()
    denied.can_undo = false
    denied.why = "putting it back means another config.write_entry, and you have that turned off"
    denied.state = "I can't put this back — " + denied.why + "."
    var win = openAccount(account([tc.alone(denied)]))

    compare(Probe.control(win, "Put back: saved the routine"), null,
      "a reversal the gate would refuse is still offered as a control")
    verify(Probe.says(win, "you have that turned off"),
      "the account does not say that the policy is what is in the way")
    // `reversible` is still true on that row. The window must not be reading
    // it as the offer, or the button would be back.
    compare(denied.reversible, true, "the fixture no longer tests what it claims to")
  }

  // Pressing the control puts one action back by id, and the row's new
  // standing comes from a fresh read rather than from this file marking it.
  function test_putting_a_row_back_asks_by_id_and_re_reads_the_account() {
    var win = openAccount(account([tc.alone(tc.reversibleRow())]))

    var control = Probe.control(win, "Put back: saved the routine")
    verify(control !== null, "a reversible row offers no way to put it back; "
      + "reachable controls were: " + JSON.stringify(Probe.names(win)))
    press(control)
    settle()

    var applied = FakeDaemon.lastRequest("undo.apply")
    verify(applied !== null, "pressing the control did not ask the daemon to undo anything")
    compare(applied.params.id, "a2", "the window asked to undo something other than that row")

    FakeDaemon.reply(applied.id, {
      done: true, refused: false, id: "a2", reversal_id: "a4",
      spoken: "I've put your config back the way it was."
    })
    settle()

    verify(Probe.says(win, "I've put your config back the way it was."),
      "the daemon's account of what it did is not on the screen")

    // The re-read is the point: the row's new standing is the account's
    // answer, not this window's assumption.
    var again = FakeDaemon.lastRequest("undo.list")
    verify(again !== null && again.id !== undefined,
      "the window did not re-read the account after the reversal")
    var undone = tc.reversibleRow()
    undone.reversible = false
    undone.can_undo = false
    undone.undone_by = "a4"
    undone.undone_at = "2026-08-29T10:00:00Z"
    undone.why = "I already put that back"
    undone.state = "I put this back just now — that reversal is a4."
    FakeDaemon.reply(again.id, account([tc.alone(undone)]))
    settle()

    verify(Probe.says(win, "I put this back just now — that reversal is a4."),
      "the row does not show that it was reversed and by what")
    compare(Probe.control(win, "Put back: saved the routine"), null,
      "a row that has already been put back is still offering to be put back")
  }

  // The refusal. A declined reversal comes back as a normal reply carrying its
  // own sentence, and the window must show that sentence and claim nothing
  // else — the account is unchanged, so the offer still stands once the person
  // has looked at the file.
  function test_a_refused_reversal_shows_its_own_sentence_and_claims_nothing() {
    var view = account([tc.alone(tc.reversibleRow())])
    var win = openAccount(view)

    press(Probe.control(win, "Put back: saved the routine"))
    settle()

    var applied = FakeDaemon.lastRequest("undo.apply")
    var refusal = "I won't undo saved the routine “morning”: "
      + "/home/u/.config/jarvix/config.toml has changed since I wrote it, so putting "
      + "it back would overwrite newer work."
    FakeDaemon.reply(applied.id, { done: false, refused: true, id: "a2", spoken: refusal })
    settle()

    verify(Probe.says(win, refusal),
      "the refusal's own sentence is not on the screen; the window shows "
      + JSON.stringify(Probe.texts(win)))

    // Refusing is not consuming: the account comes back unchanged, and the
    // offer still stands.
    var again = FakeDaemon.lastRequest("undo.list")
    FakeDaemon.reply(again.id, view)
    settle()

    verify(Probe.control(win, "Put back: saved the routine") !== null,
      "a refused reversal took the offer away, though nothing was consumed")

    // And nothing anywhere says it worked. These are the shapes a window
    // reaches for when it treats a reply as a success because it was not an
    // error.
    var shown = JSON.stringify(Probe.texts(win))
    for (var i = 0; i < ["I put this back", "put back by", "I can't put this back"].length; i++) {
      var claim = ["I put this back", "put back by", "I can't put this back"][i]
      verify(shown.indexOf(claim) < 0,
        "the window said " + JSON.stringify(claim)
        + " after a refusal, without being told anything of the kind")
    }
  }

  // The bound discloses itself, in the sentence the daemon composed from the
  // two numbers rather than in one derived here from them.
  function test_the_bounds_disclosure_is_shown_as_the_daemon_wrote_it() {
    var win = openAccount(account([tc.alone(tc.reversibleRow())]))

    verify(Probe.says(win, tc.disclosure),
      "the account does not disclose its bound in the daemon's own sentence")
    verify(Probe.says(win, tc.undoPath),
      "the account does not say where the file is")
  }

  // An empty account still has something to say, and says the daemon's
  // sentence rather than a placeholder of its own.
  function test_an_empty_account_says_so_in_the_daemons_words() {
    var win = openAccount(account([]))

    verify(Probe.says(win, "I haven't changed anything on this machine."),
      "an empty account says nothing; the window shows " + JSON.stringify(Probe.texts(win)))
  }

  // Provenance clicks through, on the same verb the answer panel uses. The
  // daemon has already split the record's stored references into the shape
  // provenance.resolve takes, so the window hands them straight over.
  function test_an_actions_sources_are_reached_the_way_an_answers_are() {
    var row = tc.reversibleRow()
    row.provenance = ["fact:f1"]
    row.sources = [{ kind: "fact", ref: "f1" }]
    var win = openAccount(account([tc.alone(row)]))

    var toggle = Probe.control(win, "Show what this action touched")
    verify(toggle !== null, "an action with sources offers no way to see them; "
      + "reachable controls were: " + JSON.stringify(Probe.names(win)))
    press(toggle)
    settle()

    var asked = FakeDaemon.lastRequest("provenance.resolve")
    verify(asked !== null, "unfolding the sources did not ask the daemon to resolve them")
    compare(asked.params.sources.length, 1)
    compare(asked.params.sources[0].kind, "fact")
    compare(asked.params.sources[0].ref, "f1",
      "the window sent something other than the references the account holds")

    FakeDaemon.reply(asked.id, {
      items: [{
        kind: "fact", ref: "f1",
        name: "the remembered fact “atlas is the staging server”",
        strength_phrase: "weighed heavily on the answer",
        note: "", gone: false,
        actions: [{ id: "open", label: "Show in Memory", tab: "memory", ref: "f1" }]
      }]
    })
    settle()

    verify(Probe.says(win, "the remembered fact “atlas is the staging server”"),
      "the source's name is not on the screen")
    verify(Probe.control(win, "Show in Memory") !== null,
      "the daemon's action for that source is not reachable")
  }

  // A row with no sources offers nothing: absence is information, and an
  // affordance that is always there says nothing.
  function test_an_action_with_no_sources_offers_no_way_to_see_them() {
    var win = openAccount(account([tc.alone(tc.reversibleRow())]))

    compare(Probe.control(win, "Show what this action touched"), null,
      "an action the daemon recorded no sources for still offers a sources control")
  }

  // Grouped by job where a job exists, under the heading the daemon composed —
  // this file has no idea what a job is or when grouping applies.
  function test_a_jobs_actions_are_grouped_under_the_daemons_heading() {
    var first = tc.reversibleRow()
    first.id = "a7"
    first.job = "j3"
    first.summary = "wrote the release notes"
    var second = tc.reversibleRow()
    second.id = "a6"
    second.job = "j3"
    second.summary = "tagged the build"
    var win = openAccount(account([
      { job: "j3", heading: "The job \"tidy\" — 2 actions", can_undo: true,
        actions: [first, second] },
      tc.alone(tc.oneWayRow())
    ]))

    verify(Probe.says(win, "The job \"tidy\" — 2 actions"),
      "the job's actions are not gathered under the daemon's heading")

    var whole = Probe.control(win, "Put back everything in The job \"tidy\"")
    verify(whole !== null, "a reversible job offers no way to put the whole of it back; "
      + "reachable controls were: " + JSON.stringify(Probe.names(win)))
    press(whole)
    settle()

    var applied = FakeDaemon.lastRequest("undo.apply")
    verify(applied !== null, "pressing the job control asked the daemon nothing")
    compare(applied.params.job, "j3",
      "the window asked to undo something other than that job")

    FakeDaemon.reply(applied.id, {
      job: "j3",
      spoken: "I undid tagged the build; wrote the release notes.",
      actions: [{ done: true, spoken: "I've put that back." },
                { done: true, spoken: "I've put that back." }]
    })
    settle()

    verify(Probe.says(win, "I undid tagged the build; wrote the release notes."),
      "the job report the daemon composed is not on the screen")
  }

  // A job that is still working is refused daemon-side, so the control is not
  // offered — the reason is on the screen instead of behind a press.
  function test_a_job_that_is_still_working_says_so_instead_of_offering_a_control() {
    var step = tc.reversibleRow()
    step.id = "a7"
    step.job = "j4"
    var win = openAccount(account([
      { job: "j4", heading: "The job \"deploy\" — 1 action", can_undo: false,
        why: "deploy is still working; stop it first and then I can put back what it did",
        actions: [step] }
    ]))

    compare(Probe.control(win, "Put back everything in The job \"deploy\""), null,
      "a job that is still working still offers a control that could only refuse")
    verify(Probe.says(win, "deploy is still working; stop it first"),
      "the account does not say why the job cannot be put back")
  }

  // What putting a parked job back will ALSO do is said before the press, not
  // discovered after it.
  function test_a_parked_jobs_consequence_is_stated_before_the_press() {
    var step = tc.reversibleRow()
    step.id = "a8"
    step.job = "j5"
    var win = openAccount(account([
      { job: "j5", heading: "The job \"tidy\" — 1 action", can_undo: true,
        note: "\"tidy\" is waiting on you; putting its work back stops it.",
        actions: [step] }
    ]))

    verify(Probe.says(win, "putting its work back stops it"),
      "a parked job's reversal does not say that it will also stop the job")
    verify(Probe.control(win, "Put back everything in The job \"tidy\"") !== null,
      "a parked job cannot be put back at all, though the daemon said it could")
  }

  // Driven by the event, not by a poll: something recorded anywhere — the CLI,
  // a job running unattended, this conversation — re-reads the account.
  function test_the_account_re_reads_when_the_daemon_says_something_changed() {
    var win = openAccount(account([tc.alone(tc.reversibleRow())]))
    var before = FakeDaemon.requests("undo.list").length

    FakeDaemon.event("undo.changed", {
      action: "recorded", id: "a9", tool: "memory.remember",
      summary: "remembered that atlas is the staging server", reversible: true
    })
    settle()

    compare(FakeDaemon.requests("undo.list").length, before + 1,
      "the account did not re-read when the daemon said it had changed")
  }
}
