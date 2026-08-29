import QtQuick
import JarvixTest
import "Probe.js" as Probe

// The provenance panel (ADR 0055, issue #168), executed.
//
// The panel's whole contract is that it words nothing. Whether a source is
// still there, how strongly it bore on the answer, what can be done with it —
// all of that is the daemon's judgement, arrives already in English, and is
// rendered verbatim. A panel that composed a sentence of its own would be
// telling the user something no part of the system actually checked.
//
// provenanceqml_test.go bans the specific phrases the Go package owns. That
// stops the known wordings being copied in. This test is the general form of
// the same claim: give the panel one source, and nothing appears on the screen
// that the daemon did not put in the payload.
JarvixWindowCase {
  id: tc
  name: "Provenance"

  // A snapshot with one answer that has sources behind it. `pos` is the
  // window's own 1-based index into the snapshot, so the single turn is pos 1.
  function snapshotWithSources(count, truncated) {
    var sources = []
    for (var i = 0; i < count; i++) {
      sources.push({ kind: "memory", ref: "fact-" + i })
    }
    return {
      turns: [{
        role: "assistant",
        text: "Atlas is the staging server.",
        provenance: { sources: sources, truncated: truncated || 0 }
      }]
    }
  }

  function openPanel(win) {
    var toggle = Probe.control(win, "What went into this")
    verify(toggle !== null, "the answer offers no way to see what went into it; controls were: "
      + JSON.stringify(Probe.names(win)))
    press(toggle)
    settle()
    return toggle
  }

  function test_an_answer_with_sources_offers_to_show_them() {
    var win = openWindow(snapshotWithSources(3))
    settle()

    var toggle = Probe.control(win, "What went into this")
    verify(toggle !== null, "no provenance control on an answer that has sources")
    // The count is the daemon's, not a guess: three sources, three announced.
    verify(Probe.says(win, "What went into this · 3"),
      "the panel does not say how many sources there were")
    verify(Probe.says(win, "press to unfold"),
      "the control does not say what pressing it does")
  }

  function test_an_answer_with_no_sources_offers_nothing() {
    var win = openWindow({ turns: [{ role: "assistant", text: "Hello." }] })
    settle()

    compare(Probe.control(win, "What went into this"), null,
      "a turn the daemon recorded no sources for still offered a provenance panel")
  }

  function test_unfolding_asks_the_daemon_and_names_the_sources_it_was_given() {
    var win = openWindow(snapshotWithSources(1))
    settle()
    openPanel(win)

    // The window asks rather than answering from the snapshot: liveness is a
    // question only the daemon can answer, and it may have changed since.
    var asked = FakeDaemon.lastRequest("provenance.resolve")
    verify(asked !== null, "unfolding the panel did not ask the daemon to resolve the sources")
    compare(asked.params.sources.length, 1)
    compare(asked.params.sources[0].ref, "fact-0",
      "the window sent something other than the sources the daemon recorded")
  }

  function test_the_panel_renders_only_strings_the_daemon_supplied() {
    var win = openWindow(snapshotWithSources(1))
    settle()
    openPanel(win)

    var asked = FakeDaemon.lastRequest("provenance.resolve")
    FakeDaemon.reply(asked.id, {
      items: [{
        kind: "memory",
        ref: "fact-0",
        name: "the staging server is called atlas",
        strength_phrase: "weighed heavily on the answer",
        note: "",
        gone: false,
        actions: [{ id: "reveal", label: "Show in Memory", tab: "memory", ref: "fact-0" }]
      }]
    })
    settle()

    verify(Probe.says(win, "the staging server is called atlas"),
      "the source's name is not on the screen")
    verify(Probe.says(win, "weighed heavily on the answer"),
      "the daemon's strength phrase is not on the screen")
    verify(Probe.says(win, "Show in Memory"), "the daemon's action label is not on the screen")

    // Nothing else. These are the sentences internal/provenance owns and the
    // panel has no business knowing; a copy here would keep saying them after
    // the daemon stopped, which is the failure mode that matters — a stale
    // reassurance reads exactly like a checked one.
    var invented = [
      "has since been forgotten", "has been deleted", "no longer on disk",
      "this thread has ended", "no longer taught", "still available"
    ]
    var shown = JSON.stringify(Probe.texts(win))
    for (var i = 0; i < invented.length; i++) {
      verify(shown.indexOf(invented[i]) < 0,
        "the panel said " + JSON.stringify(invented[i])
        + " without being told to; liveness wording belongs to internal/provenance")
    }
  }

  function test_a_source_that_is_gone_says_so_in_the_daemons_words() {
    var win = openWindow(snapshotWithSources(1))
    settle()
    openPanel(win)

    var asked = FakeDaemon.lastRequest("provenance.resolve")
    FakeDaemon.reply(asked.id, {
      items: [{
        kind: "memory",
        ref: "fact-0",
        name: "the staging server is called atlas",
        strength_phrase: "weighed heavily on the answer",
        // The daemon words the liveness. The row also flags it, but the flag
        // is a colour and the sentence is the meaning.
        note: "that fact has since been forgotten",
        gone: true,
        actions: []
      }]
    })
    settle()

    verify(Probe.says(win, "that fact has since been forgotten"),
      "a source that is gone does not say so in words — the flag colour is the only signal")
  }

  // A failure to resolve is about one row, not about the daemon, so it is
  // rendered inside the panel and in the daemon's own sentence.
  function test_a_source_that_cannot_be_reached_says_why_inside_the_panel() {
    var win = openWindow(snapshotWithSources(1))
    settle()
    openPanel(win)

    var asked = FakeDaemon.lastRequest("provenance.resolve")
    FakeDaemon.replyError(asked.id, -32000, "that conversation has been archived")
    settle()

    verify(Probe.says(win, "that conversation has been archived"),
      "the panel swallowed the daemon's explanation")
  }

  // What the daemon could not record is said too. A turn that used more
  // sources than Jarvix keeps must not quietly present the kept ones as the
  // whole story.
  function test_sources_that_went_unrecorded_are_counted_in_words() {
    var win = openWindow(snapshotWithSources(2, 3))
    settle()
    openPanel(win)

    verify(Probe.says(win, "3 more sources went unrecorded"),
      "the panel does not admit that this turn used more sources than were kept")
  }
}
