package desktop

//go:generate go run genipcvocab.go

import (
	"sort"
	"strings"
)

// The daemon's IPC vocabulary, as the QML test harness is allowed to speak it
// (issue #174).
//
// The headless QML runner drives the real window against a fake daemon. A fake
// is only worth running while it says what the daemon says: a stub that
// answers a message the daemon does not send, or sends one under a name the
// daemon has since renamed, turns a green suite into a proof about a fiction.
// So the vocabulary is not written down in the harness. It is written down
// here, compiled into the harness by genipcvocab.go, and checked against the
// daemon's own source by ipcvocab_test.go — which parses internal/**/*.go and
// fails if either list has drifted from the registrations and publications it
// finds. The harness then refuses, at runtime, to send or receive any name
// that is not in the compiled list.
//
// Both lists are deliberately *complete* rather than "the ones the tests
// happen to use". A partial list would make the set-equality check impossible,
// and it is the set-equality check that stops the drift.

// daemonMethods is every JSON-RPC method jarvixd answers — one entry per
// server.Handle registration outside _test.go files. This is the vocabulary a
// surface is allowed to *send*.
//
// Kept sorted so the generated JavaScript is byte-stable; ipcvocab_test.go
// fails if the set differs from the registrations, in either direction, so a
// new verb is a compile-and-test failure rather than a silent omission.
var daemonMethods = []string{
	"activity.get",
	"approvals.add",
	"approvals.forget",
	"approvals.list",
	"automations.list",
	"automations.schedules",
	"automations.set_enabled",
	"briefing.get",
	"config.delete_entry",
	"config.get",
	"config.get_entry",
	"config.list_entries",
	"config.reload",
	"config.set",
	"config.test_entry",
	"config.upsert_entry",
	"config.validate_entry",
	"context.last",
	"conversation.delete",
	"conversation.get",
	"conversation.list",
	"conversation.new",
	"conversation.open",
	"conversation.read",
	"conversation.reset",
	"conversation.search",
	"doctor.get",
	"focus.create",
	"focus.end",
	"focus.list",
	"focus.park",
	"focus.remind",
	"focus.save",
	"focus.session.end",
	"focus.session.start",
	"focus.switch",
	"knowledge.refresh_now",
	"knowledge.set_enabled",
	"knowledge.status",
	"memory.add",
	"memory.forget",
	"memory.forget_gated",
	"memory.last",
	"memory.list",
	"memory.set_pinned",
	"memory.update",
	"monitors.forget",
	"monitors.list",
	"monitors.name",
	"monitors.repoint",
	"overlays.get",
	"placement.vocabulary",
	"provenance.open",
	"provenance.resolve",
	"reminders.cancel",
	"reminders.create",
	"reminders.list",
	"reminders.preview",
	"routines.list",
	"routines.run",
	"scripts.list",
	"scripts.run",
	"session.cancel",
	"session.confirm",
	"session.start",
	"session.submit",
	"session.text",
	"situation.get",
	"speech.cancel",
	"speech.replay",
	"thinking.get",
	"thinking.set",
	"undo.apply",
	"undo.list",
	"state.hold",
	"state.release",
	"status.get",
	"vocabulary.forget_gated",
	"vocabulary.last",
	"vocabulary.list",
	"vocabulary.teach",
	"vocabulary.update",
	"voice.start",
	"voice.stop",
	"wake.mute",
	"wake.status",
	"windows.list",
	"windows.managed",
	"windows.name",
	"windows.release",
}

// daemonEvents is every bus event type the daemon pushes to its clients as a
// JSON-RPC notification. This is the vocabulary a surface is allowed to
// *receive*.
//
// Every name here is dotted except "error", which the daemon has published
// under that bare name since the first session engine; ipcvocab_test.go knows
// about that single exception rather than guessing, because a rule that
// accepted any undotted literal would sweep in every unrelated word passed to
// a publish helper.
var daemonEvents = []string{
	"activity.row",
	"approvals.changed",
	"artifact.created",
	"assistant.delta",
	"assistant.finished",
	"assistant.host",
	"assistant.started",
	"automation.fired",
	"automation.missed",
	"automation.refused",
	"automation.skipped",
	"briefing.given",
	"config.changed",
	"config.entry_changed",
	"config.setting_changed",
	"context.captured",
	"conversation.changed",
	"desktop.action",
	"desktop.refusal",
	"error",
	"focus.changed",
	"focus.recap",
	"focus.skipped",
	"intent.executed",
	"jobs.changed",
	"knowledge.injected",
	"knowledge.updated",
	"memory.entry_changed",
	"memory.injected",
	"overlays.changed",
	"recording.started",
	"recording.stopped",
	"reminders.changed",
	"routine.finished",
	"routine.started",
	"routine.step",
	"script.finished",
	"script.started",
	"session.cancelled",
	"session.finished",
	"session.nothing_heard",
	"session.timings",
	"situation.given",
	"speech.replayed",
	"state.changed",
	"thinking.changed",
	"tool.confirmation_deadline",
	"tool.confirmation_required",
	"tool.confirmed",
	"tool.declined",
	"tool.denied",
	"tool.finished",
	"tool.pre_approved",
	"tool.progress",
	"tool.started",
	"transcript.final",
	"transcript.partial",
	"tts.finished",
	"tts.started",
	"tts.superseded",
	"typing.audit",
	"undo.changed",
	"vocabulary.entry_changed",
	"vocabulary.injected",
	"wake.changed",
	"wake.detected",
}

// DaemonMethodNames returns the request vocabulary, sorted. The copy is
// deliberate: a caller that sorted or appended to the package's own slice
// would change what every later caller — and the generator — sees.
func DaemonMethodNames() []string {
	out := append([]string(nil), daemonMethods...)
	sort.Strings(out)
	return out
}

// DaemonEventNames returns the notification vocabulary, sorted, for the same
// reasons as DaemonMethodNames.
func DaemonEventNames() []string {
	out := append([]string(nil), daemonEvents...)
	sort.Strings(out)
	return out
}

// RenderIpcVocabularyJS compiles both lists into the library the headless QML
// harness imports, the same arrangement as RenderBarStateJS and
// RenderActivityJS: one Go table, one generated file, one drift test.
//
// It carries names only — no payload shapes. Payloads belong to the tests that
// drive them, because a fake that also owned the shapes would let a test pass
// against a params object the daemon never sends, and the whole point of
// generating this file is that the harness cannot invent vocabulary.
func RenderIpcVocabularyJS() string {
	var b strings.Builder
	b.WriteString(`.pragma library

// Code generated by internal/desktop/genipcvocab.go. DO NOT EDIT.
//
// The daemon's IPC vocabulary, compiled from the Go tables in
// internal/desktop/ipcvocab.go, for the headless QML test harness (issue
// #174). The fake daemon refuses to send an event or accept a request whose
// name is not in here, so a test cannot drive a message the real daemon has
// no word for. ipcvocab_test.go checks the Go tables against the daemon's own
// Handle() registrations and Event publications, so the fake cannot drift into
// testing a fiction even if nobody rereads it. Regenerate with:
//
//     go generate ./internal/desktop

var methods = {
`)
	methods := DaemonMethodNames()
	for i, m := range methods {
		b.WriteString("  " + jsString(m) + ": true" + jsSeparator(i, len(methods)) + "\n")
	}
	b.WriteString(`}

var events = {
`)
	events := DaemonEventNames()
	for i, e := range events {
		b.WriteString("  " + jsString(e) + ": true" + jsSeparator(i, len(events)) + "\n")
	}
	b.WriteString(`}

// knowsMethod answers whether the daemon registers a handler for this name.
// Unknown means the test is wrong, not that the daemon is lenient: the real
// server answers an unregistered method with -32601 and the surface would see
// an error, never a result.
function knowsMethod(name) {
  return methods[String(name || "")] === true
}

// knowsEvent answers whether the daemon ever publishes an event of this type.
function knowsEvent(name) {
  return events[String(name || "")] === true
}
`)
	return b.String()
}
