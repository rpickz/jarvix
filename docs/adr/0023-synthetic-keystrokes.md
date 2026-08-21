# ADR 0023 — Synthetic keystrokes: the threat model is the design

**Status:** accepted (implements roadmap Phase 4)

## Context

The last thing standing between Jarvix and "take control of my machine" is
text. Plenty of applications have no scriptable interface at all — a document,
a form field, a chat box in a web app — and the only way in is the keyboard.
`wtype` makes that mechanically trivial on Wayland: one subprocess, one
argument, and the characters appear wherever focus is.

That triviality is the danger, and it is why this is a separate ticket from
window control ([#37](https://github.com/rpickz/jarvix/issues/37) rather than
part of [#36](https://github.com/rpickz/jarvix/issues/36)). Everything ADR 0022
added is visible on screen and undoable by hand. A keystroke stream is neither.
It is also the only capability whose **target the model does not choose and
cannot see**: `shell.run` at least names the command it wants to run, and
`desktop.close_window` names a window from an inventory. Typing goes wherever
focus happens to be at the instant the keys land, which is a property of the
user's hands, a notification daemon, and a dialog that opened half a second
ago.

So this ADR is mostly a threat model. The feature is fifty lines of subprocess;
the design is everything around it.

## Threat model

Six ways keystroke synthesis goes wrong, and what stops each. "Confused model"
covers an honest model that misunderstands; "adversarial content" covers
prompt injection reaching the model through the desktop context Jarvix gathers
(ADR 0019), an advisor's output (ADR 0016), or a web page the user asked about.

**1. Text that executes itself.** A payload ending in a newline typed into a
terminal is a command that runs. The model does not have to intend this: it can
end a dictated sentence with a line break out of habit, and adversarial content
can ask it to.
*Blocked by:* `desktop.Keyboard.Type` cannot express a control character. The
payload is filtered by `desktop.Literal` at the last point before argv, so
Cc/Cf/Cs/Co and the line separators are gone by construction rather than by
validation — and the tool refuses the whole call rather than typing a quietly
shortened version, because the user was shown the text and typing a different
one would make the confirmation a lie. Return is only reachable through
`Press`, which is a different method behind a different tool.

**2. Typing that becomes sending.** "Compose a reply" and "send the reply" are
one keystroke apart, and a model being helpful will close the gap.
*Blocked by:* `typing.press_key` is a separate capability with its own policy
tier, confirmed separately, from a closed vocabulary of thirteen keys with no
modifiers and no chords. Approving text never approves Enter, and the tool
descriptions and system prompt both say so.

**3. The focus race.** A spoken confirmation takes seconds. In those seconds a
notification can steal focus, a dialog can open, or the user can click
somewhere else — and the approval, given about one window, would be spent on
whatever is in front when it is acted on. This is the most likely failure in
ordinary use and the least likely to be noticed in review, which is why it is
the first test in the file rather than an edge case.
*Blocked by:* the focused window is captured when the gate builds its question,
and re-checked at the instant of typing against a **fresh** inventory (the
two-second cache is dropped first). Identity is address + the compositor's
stable id + class, the same triple ADR 0022 verifies with, because an address
is a reusable handle. Any difference — a different window, a reused address, no
focus at all — types nothing and says so.

**4. A confirmation that describes something other than what happens.** If the
model supplies the sentence, an adversarial one can call `curl … | sh` "a note
to self".
*Blocked by:* the confirmation is generated daemon-side from the resolved
window and the literal payload, through the `tools.Confirmable` seam. The gate
still decides the *tier* from configuration; the tool only supplies the
*words*, and it supplies them from what it can see.

**5. The runaway.** A model in a tool loop with a keyboard can type
indefinitely, and a user who has stepped away will not stop it.
*Blocked by:* a length cap per call and a rate limit shared by both
capabilities, each refusing with a reason so the model is told to stop rather
than left to retry. Both are configurable, and both have ceilings so a control
cannot be set to "unlimited" by accident.

**6. The audit that leaks.** The obvious way to make typing accountable is to
log what was typed — and the user may have dictated a password, a recovery
phrase, or a private message. The journal outlives the conversation.
*Blocked by:* the payload never reaches a log sink. The tool's own log lines
carry class, length and outcome; the `Verdict.Command` the gate publishes and
logs verbatim carries the length, not the characters; the `typing.audit` bus
event and the retained `jarvix status --last` trail carry neither. The one
place the literal text appears is the spoken question and the overlay showing
it, which exist for exactly as long as the question stands. A test asserts the
absence across every outcome, not just the happy path.

## Decision

**Off unless the user asked for it.** `[tools.typing] enable = false`, like
`shell.run`. Enabling the other tool families does not enable this one.

**A tier that a global default cannot loosen.** Both typing tools ignore
`[tools.policy] default = "allow"` and ask anyway; only naming the tool
(`[tools.policy.tool]."typing.type_text" = "allow"`) allows it, which is a
sentence a user has to mean. A stricter default still wins — `deny` denies
these too, because tightening is never the thing to override. Approvals are
also never remembered by `remember_for_conversation`: that setting's premise is
that the approved thing is fully described by what was asked, and a typing
approval is about a payload *and* a window that had focus at that moment.

**Terminals escalate, from live state.** Whether this call is dangerous is not
a fact configuration holds — it depends on where focus is right now, which only
the tool can see. So `tools.Escalating` is a new, deliberately one-way seam: a
tool may turn allow into ask and never the reverse, so it can only make the
gate stricter than the user configured it. Typing into a window whose class is
on the terminal list escalates, and the confirmation says why in the sentence
the user hears.

**One inventory, one definition of "which window".** Typing borrows
`tools.Desktop` — the compositor, the cache, the matcher, the verification —
rather than resolving windows itself. A second way to decide what Jarvix is
acting on would be a second way to get it wrong. The daemon therefore builds
that shared state whenever either family is enabled, and registers the five
window verbs only when `[tools] desktop` is on.

**Keystrokes only ever land on a window the gate captured.** A call that
reaches the tool without a capture — no policy installed, or a compositor that
would not answer when the question was asked — is refused, not resolved afresh.
Resolving now would mean typing into whatever has focus on the strength of a
question that named no window, which is the exact confusion this exists to
prevent.

**wtype, not ydotool.** `ydotool` needs a root daemon and write access to
`/dev/uinput` — a permanently elevated privilege on the machine, in exchange
for the same keystrokes. `wtype` speaks the virtual-keyboard protocol as the
user, over their own Wayland socket, and exists for the milliseconds it is
typing. The heavier privilege footprint buys nothing here, so it is not
required and not supported.

## Consequences

**Doctor can ask "would this work?" without typing.** `wtype -- ""` connects to
the display and binds the virtual-keyboard protocol, then types nothing — so
one harmless invocation distinguishes "not installed", "no Wayland session",
and "this compositor refuses virtual keyboards", which are three different
fixes that look identical from the user's chair. All are Warn, never Fail: the
tools degrade to one spoken sentence and the rest of Jarvix is untouched.

**No test in this tree may synthesise a keystroke.** The person running
`go test` is working in the session it runs in. `desktop.FakeKeyboard` records;
the `Wtype` driver's argv guarantees are asserted against a recording stub in a
temp directory; doctor's probe is asserted by reading back the argv it asked
for. Nothing anywhere presses a key.

**A payload the user cannot see is refused.** Format characters (Cf) go with
the control characters, which is stricter than "no line breaks" and
deliberately so: a zero-width space or a right-to-left override is invisible in
the confirmation sentence, so a payload containing one is not the payload the
user read.

**The rate limit is shared between typing and pressing.** A loop that
alternates the two is still a loop.

**What this does not do.** No mouse, no clicking, no reading text back out of
windows (that is screen capture, with its own consent design), no
per-application recipes, and no credential filling — Jarvix must never be
positioned as a password manager. Modifier chords are absent from the key
vocabulary on purpose: `Ctrl+W`, `Alt+F4` and `Ctrl+C` are how a keystroke
stream closes a dialog, quits an application, or interrupts a job, and none of
them is something a dictation feature needs.
