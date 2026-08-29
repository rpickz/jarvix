package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rpickz/jarvix/internal/desktop"
)

// This file implements the typing tools (ADR 0023): the assistant can enter
// literal text into the window the user is in, and — as a separate capability
// with its own tier — press one key from a closed list.
//
// It is the dangerous half of "take control", and every decision here exists
// because of that. Window control (ADR 0022) acts on things the user can see
// and undo by hand; keystrokes are neither. A stream of synthetic keys can
// enter a command into a shell and run it, fill a password field that echoes
// nowhere, answer a confirmation dialog, or send a half-composed message —
// and the target is not something the model chose, it is whatever has focus
// at the instant the keys land.
//
// So the shape is:
//
//   - Off unless the user turned it on ([tools.typing] enable), the same way
//     shell.run is.
//   - The target is never named by the model. It is the focused window, taken
//     from the compositor's inventory — the same inventory, the same matcher
//     and the same verification the window tools use, because a second way to
//     decide what Jarvix is acting on would be a second way to get it wrong.
//   - Focus is captured when the question is asked and re-checked when the
//     answer is acted on. If a different window took focus in between, nothing
//     is typed. This is the whole point of the file, not an edge case: a
//     spoken confirmation takes seconds, and a notification, a dialog, or the
//     user's own hand can move focus inside one.
//   - Text is literal characters, and nothing else. Return, Tab and the rest
//     are not reachable from the typing path at any layer — not by escape, not
//     by validation, but because desktop.Keyboard.Type has no way to express
//     them. Submitting is desktop.Keyboard.Press, a different method behind a
//     different tool with a different tier.
//   - The typed text is never logged. The user may have dictated a password,
//     and the journal outlives the conversation. The window, the length, and
//     the outcome are audited; the characters are not.
//
// Since #197 (ADR 0062) there is one more, and it is the load-bearing one:
//
//   - Text typed into a TERMINAL is a command, and is classified and
//     confirmed exactly as `shell.run` is. The same compound-command
//     splitter, the same deny rules, the same risk words, the same verbatim
//     card — reached through the same Policy, under the shell.run identity.
//     The two gates compose the strict way round: a shell verdict of deny
//     refuses whatever the typing tier says, a verdict of ask forces the
//     question whatever the typing tier says, and a verdict of allow tightens
//     nothing. The terminal escalation ADR 0023 already had stands over all
//     of it, and the single thing that sets it aside is a standing approval
//     the user personally granted (#162) for that exact command shape.
//
//     This exists because of what managed windows would otherwise be. Handing
//     a terminal over gives Jarvix a live shell; if typing into it were
//     merely "typing", acquisition would be a complete bypass of the
//     permission gate — the same power with none of the review. So
//     acquisition grants access to the WINDOW and no permission to execute,
//     and the check below is what makes that true rather than merely stated.
//     Note that it does not depend on management at all: an unmanaged
//     terminal that happens to have focus is classified identically, which is
//     what makes "acquire it first" impossible to use as a way around the
//     gate.

// Tool names. Two tools rather than one with a mode, because the permission
// gate keys on the tool name: "typing may be allowed and submitting still
// asks" is then a fact about the registry rather than a special case inside a
// tool (ADR 0014).
const (
	TypeTextToolName = "typing.type_text"
	PressKeyToolName = "typing.press_key"
)

// Typing bounds.
const (
	// DefaultTypingMaxChars caps one payload. Long enough for a dictated
	// paragraph, short enough that a model in a loop cannot fill a document
	// before anyone reaches the keyboard.
	DefaultTypingMaxChars = 500
	// DefaultTypingRateLimit is how many typing actions may happen inside
	// DefaultTypingRateWindow.
	DefaultTypingRateLimit = 6
	// DefaultTypingRateWindow is the rate limiter's window.
	DefaultTypingRateWindow = time.Minute
	// DefaultTypingTimeout bounds one injection. Typing is a local Wayland
	// round trip; past this something is wrong.
	DefaultTypingTimeout = 3 * time.Second
	// captureTTL is how long a focus capture made for a confirmation stays
	// usable. Comfortably longer than the confirmation timeout, so a user who
	// takes their time still gets an answer rather than a silent expiry — the
	// focus re-check, not this, is what keeps the approval honest.
	captureTTL = 2 * time.Minute
	// maxSpokenPayload bounds how much of the payload is read aloud in the
	// confirmation. The overlay event carries the whole thing; speech only
	// needs enough to recognise it, and a two-minute recitation is a
	// confirmation nobody listens to.
	maxSpokenPayload = 160
)

// DefaultTerminalClasses is the shipped terminal-class list: window classes
// whose contents are a command line. Typing into one is the highest-
// consequence case there is — the characters may be executed the moment
// something presses Return, and the user may not be the one who presses it —
// so a match escalates the tier to ask however typing is otherwise configured.
//
// It reuses the matcher's terminal category (windowmatch.go) rather than
// keeping a second list, because two lists drift and this one drifting means a
// terminal Jarvix does not recognise as one.
var DefaultTerminalClasses = appCategories["terminal"]

// TypingAudit is one typing decision, for the event bus and `jarvix status
// --last`. It is deliberately everything *except* the payload: which window,
// how many characters, whether a human approved it, and what happened.
type TypingAudit struct {
	// Tool is which capability acted.
	Tool string
	// Window describes the target for a human ("Firefox — GitHub"). Empty when
	// there was no target to name.
	Window string
	// Class is the target's application class, the identifier safe to log.
	Class string
	// Chars is how many characters would have been typed — never which.
	Chars int
	// Key is the key name for a press, from the closed vocabulary.
	Key string
	// Terminal marks a target whose contents are a command line.
	Terminal bool
	// Command is the text verbatim, set ONLY when the target was a terminal
	// and the text was therefore classified as a command (#197). It is the
	// one place a typed payload travels, and it is deliberate: a command that
	// ran must appear in the activity feed as a command, because a standing
	// approval removes the question and must not also remove the evidence
	// (the tool.pre_approved argument, applied to a keyboard). It reaches the
	// feed and nothing else — the journal line beside it still records the
	// length and never the characters.
	Command string
	// Rule names the gate rule that decided about Command, in the shell
	// classifier's own words (`risky command "rm"`, `allow pattern "ls"`).
	// Empty when nothing was classified.
	Rule string
	// Approved reports whether a human answered a confirmation for this exact
	// action. False means the gate's configured tier let it through silently.
	Approved bool
	// Outcome is what happened: "typed", "pressed", "refused",
	// "focus-changed", or "unavailable".
	Outcome string
	// Reason explains a refusal in one clause. Generated from the rule that
	// refused; it never contains the payload.
	Reason string
}

// Typing is the shared state behind the two typing tools: the keystroke seam,
// the window tools it borrows every window decision from, the caps, and the
// focus captures held between a confirmation and the action it authorised.
type Typing struct {
	// windows is the window tools' shared state — the compositor, the
	// inventory cache, the matcher and the verification. Borrowed rather than
	// duplicated: there is exactly one thing in Jarvix that decides which
	// window is being acted on.
	windows *Desktop
	kb      desktop.Keyboard

	maxChars  int
	rateLimit int
	rateOver  time.Duration
	terminals []string
	timeout   time.Duration
	log       *slog.Logger
	onAudit   func(TypingAudit)
	// classify judges a command line through the permission gate under the
	// shell.run identity (#197). The daemon wires it to the live policy;
	// tests hand back literals. Nil is the pre-#197 behaviour — a terminal
	// still escalates to ask, but the text is not classified — and it is the
	// STRICT direction only by luck of the tier, so the daemon always
	// installs one.
	classify func(command string) Verdict

	// now is the clock, injectable so the rate limiter is tested without
	// sleeping.
	now func() time.Time

	mu sync.Mutex
	// captures holds the focused window seen while the gate was deciding, so
	// the window the user was asked about is the window the answer is checked
	// against.
	captures map[string]focusCapture
	// recent holds the times of recent typing actions, for the rate limiter.
	recent []time.Time
}

// focusCapture is the focused window as it was when the gate asked about it.
type focusCapture struct {
	window desktop.Window
	at     time.Time
	// asked is true once a confirmation question was actually built from this
	// capture, which is how the audit trail knows a human approved rather than
	// the tier being allow.
	asked bool
}

// TypingOptions configure the typing tools.
type TypingOptions struct {
	// Windows is the window tools' shared state, reused for resolution and
	// re-verification. Required.
	Windows *Desktop
	// Keyboard is the keystroke seam. Required.
	Keyboard desktop.Keyboard
	// MaxChars caps one payload. Zero means DefaultTypingMaxChars.
	MaxChars int
	// RateLimit is how many typing actions may happen inside RateWindow. Zero
	// means DefaultTypingRateLimit.
	RateLimit int
	// RateWindow is the rate limiter's window. Zero means
	// DefaultTypingRateWindow.
	RateWindow time.Duration
	// TerminalClasses overrides DefaultTerminalClasses.
	TerminalClasses []string
	// Timeout bounds one injection. Zero means DefaultTypingTimeout.
	Timeout time.Duration
	// OnAudit is called for every typing decision, for the bus event and the
	// retained audit trail. Never called with the payload — except for the
	// one case TypingAudit.Command documents: a command typed into a
	// terminal, which is a command and belongs in the feed as one.
	OnAudit func(TypingAudit)
	// Classify judges one command line through the permission gate under the
	// shell.run identity, so text typed into a terminal faces exactly the
	// classification a shell command faces (#197, ADR 0062). Required in the
	// daemon; nil in a test that is not exercising the gate.
	Classify func(command string) Verdict
	// Log records each decision. Nil uses slog.Default().
	Log *slog.Logger

	// now overrides the clock in tests.
	now func() time.Time
}

// NewTyping builds the typing tools' shared state.
func NewTyping(opts TypingOptions) *Typing {
	t := &Typing{
		windows:   opts.Windows,
		kb:        opts.Keyboard,
		maxChars:  opts.MaxChars,
		rateLimit: opts.RateLimit,
		rateOver:  opts.RateWindow,
		terminals: append([]string(nil), opts.TerminalClasses...),
		timeout:   opts.Timeout,
		log:       opts.Log,
		onAudit:   opts.OnAudit,
		classify:  opts.Classify,
		now:       opts.now,
		captures:  make(map[string]focusCapture),
	}
	if t.maxChars <= 0 {
		t.maxChars = DefaultTypingMaxChars
	}
	if t.rateLimit <= 0 {
		t.rateLimit = DefaultTypingRateLimit
	}
	if t.rateOver <= 0 {
		t.rateOver = DefaultTypingRateWindow
	}
	if len(t.terminals) == 0 {
		t.terminals = append([]string(nil), DefaultTerminalClasses...)
	}
	if t.timeout <= 0 {
		t.timeout = DefaultTypingTimeout
	}
	if t.log == nil {
		t.log = slog.Default()
	}
	if t.now == nil {
		t.now = time.Now
	}
	return t
}

// Tools returns the two typing tools, in registration order.
func (t *Typing) Tools() []Tool {
	return []Tool{&typingTool{t: t}, &typingTool{t: t, press: true}}
}

// Names returns the tool names, for the daemon's startup log.
func (t *Typing) Names() []string { return []string{TypeTextToolName, PressKeyToolName} }

// typingTool is one capability. It holds no state: both point at the shared
// Typing, so one focus capture and one rate limiter cover both.
type typingTool struct {
	t *Typing
	// press distinguishes the submit capability from the typing one.
	press bool
}

// Name implements Tool.
func (t *typingTool) Name() string {
	if t.press {
		return PressKeyToolName
	}
	return TypeTextToolName
}

// Description implements Tool. Written for a small local model, so it states
// the two things that would otherwise be discovered by trying: that the text
// goes wherever the user is, and that finishing an entry is a different call
// the user has to approve separately.
func (t *typingTool) Description() string {
	if t.press {
		return "Press one key in the window the user is working in: enter, tab, escape, backspace, " +
			"delete, or an arrow key. Use it only when the user asked you to submit, send, or move " +
			"between fields — pressing enter can send a message or run a command, so it is always " +
			"confirmed separately from typing, and approving some text never approves sending it."
	}
	return "Type text into the window the user is currently working in, as if they had typed it " +
		"themselves. Use it when they ask you to write, enter, or dictate something into what is on " +
		"their screen. Send exactly the characters they want entered and nothing else: no line " +
		"breaks, no tabs, no quotes you added, no explanation. It types the text and stops — it does " +
		"not press enter, send, or submit anything. If the user also wants it sent, that is a " +
		"separate call they will be asked to approve on its own."
}

// Schema implements Tool.
func (t *typingTool) Schema() json.RawMessage {
	if t.press {
		names := make([]string, 0, len(desktop.KeyNames))
		for name := range desktop.KeyNames {
			names = append(names, strconv.Quote(name))
		}
		sort.Strings(names)
		return json.RawMessage(`{
			"type": "object",
			"properties": {
				"key": {
					"type": "string",
					"enum": [` + strings.Join(names, ", ") + `],
					"description": "Which single key to press. Nothing else can be pressed, and keys cannot be combined."
				}
			},
			"required": ["key"]
		}`)
	}
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"text": {
				"type": "string",
				"maxLength": ` + strconv.Itoa(t.t.maxChars) + `,
				"description": "The exact characters to type. Printable characters only — line breaks, tabs and other control keys are refused, and the whole request is refused if the text contains one."
			}
		},
		"required": ["text"]
	}`)
}

// typingArgs is everything the model may say. Neither field reaches a command
// line: text is filtered to printable characters and passed as one argv
// element to a program with no shell, and key is looked up in a closed table.
type typingArgs struct {
	Text string `json:"text"`
	Key  string `json:"key"`
}

// payload returns what this call is about, as one string: the text to type, or
// the key to press. It is what the confirmation is keyed on, so an approval
// can never be spent on a different payload.
func (t *typingTool) payload(args typingArgs) string {
	if t.press {
		return strings.ToLower(strings.TrimSpace(args.Key))
	}
	return args.Text
}

// Execute implements Tool.
//
// The order is the safety argument, and it does not vary: validate the payload
// before anything is resolved, apply the caps, take the capture the gate made,
// look at the desktop once more, and only then type. Every refusal is a tool
// result rather than an error — a desktop that moved is something to say in a
// sentence, not a failed session.
func (t *typingTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var args typingArgs
	if err := json.Unmarshal(coalesceArgs(input), &args); err != nil {
		return "", fmt.Errorf("invalid %s arguments: %w", t.Name(), err)
	}

	payload, key, refusal := t.checkPayload(args)
	// chars is what the audit trail records: how much would have been typed,
	// never what. Zero for a key press, which types nothing — the key name is
	// recorded instead, and it comes from a closed vocabulary.
	chars := len([]rune(payload))
	if t.press {
		chars = 0
	}
	if refusal != "" {
		return t.t.refuse(t.Name(), desktop.Window{}, chars, key, false, refusal), nil
	}
	if reason, limited := t.t.rateLimited(); limited {
		return t.t.refuse(t.Name(), desktop.Window{}, chars, key, false, reason), nil
	}

	// The capture the gate made — at the moment it asked the user, or at the
	// moment it decided it did not need to. Both paths capture, so a call that
	// reaches here without one is a call the gate never managed to attach a
	// window to, and there is nothing to re-check against.
	//
	// That is refused rather than resolved afresh, and the refusal is the
	// design. Resolving now would mean typing into whatever has focus at this
	// instant on the strength of a question that never named a window — which
	// is precisely the confusion this capability is built to make impossible.
	captured, approved, held := t.t.takeCapture(t.Name(), payload)
	if !held {
		return t.t.refuse(t.Name(), desktop.Window{}, chars, key, false,
			"I could not tell which window this would go to, so there was nothing to confirm"), nil
	}

	// The critical check. The capture was made before the user answered; the
	// desktop was not asked to hold still while they did. Drop the cache and
	// look again: if the window that has focus now is not the window the
	// decision was made about, type nothing at all.
	t.t.windows.invalidate()
	current, err := t.t.focused(ctx)
	if err != nil {
		return t.t.unavailable(t.Name(), chars, key, err), nil
	}
	if !sameWindow(captured, current) {
		return t.t.focusChanged(t.Name(), captured, current, chars, key, approved), nil
	}
	if !current.AcceptsInput {
		return t.t.refuse(t.Name(), current, chars, key, approved,
			"the focused window does not accept typed input"), nil
	}

	// The command classification, re-run against the window that has focus
	// NOW rather than the one the gate saw. Two things ride on it, and the
	// second is the reason it is here at all:
	//
	//   - a denied command is refused again, because Refusing's promise is
	//     that nothing is written even if a refused call is somehow executed
	//     — and "somehow" here includes a daemon built with no gate at all;
	//   - the audit carries the command verbatim, so a typed command appears
	//     in the activity feed as a command.
	var mark commandMark
	if verdict, classified := t.commandVerdict(args, current); classified {
		mark = commandMark{Command: strings.TrimSpace(payload), Rule: verdict.Rule}
		if verdict.Decision == PolicyDeny {
			return t.t.refuseCommand(t.Name(), current, chars, key, approved, mark,
				fmt.Sprintf("typing that into %s would run it, and it is refused by %s",
					desktop.AppName(current.Class), verdict.Rule)), nil
		}
	}

	return t.t.inject(ctx, t.Name(), current, chars, payload, key, approved, mark), nil
}

// commandMark is what the gate found in a payload headed for a terminal: the
// text verbatim — because at that point it is a command line, not private
// prose — and the rule that judged it. Zero for everything else, which is
// what keeps the payload out of every other audit row.
type commandMark struct {
	Command string
	Rule    string
}

// checkPayload validates what the model asked for, before anything is resolved
// and before the desktop is touched. refusal is the clause explaining why not,
// empty when the payload is usable.
func (t *typingTool) checkPayload(args typingArgs) (payload, key, refusal string) {
	payload = t.payload(args)
	if t.press {
		if payload == "" {
			return "", "", "no key was named"
		}
		if _, ok := desktop.Keysym(payload); !ok {
			// The name is echoed back so the model can correct itself, and
			// bounded first: it is the one string here the model chose freely,
			// and it lands in a log line and an audit event.
			named := boundedKeyName(payload)
			return payload, named, fmt.Sprintf("%q is not a key that can be pressed", named)
		}
		return payload, payload, ""
	}
	if strings.TrimSpace(payload) == "" {
		return payload, "", "there was no text to type"
	}
	// Control characters are refused, not stripped. The user is shown the text
	// in the confirmation, so typing a quietly altered version of it would mean
	// they approved something other than what happened — and a stripped
	// newline is exactly the case where the difference matters.
	if _, removed := desktop.Literal(payload); removed > 0 {
		return payload, "", "the text contains line breaks or other control characters, which are " +
			"never typed — send the text as a single line, and use the separate key-press tool if " +
			"something has to be submitted"
	}
	if n := len([]rune(payload)); n > t.t.maxChars {
		return payload, "", fmt.Sprintf("the text is %d characters and the limit is %d", n, t.t.maxChars)
	}
	return payload, "", ""
}

// commandVerdict classifies this call's payload as a shell command when the
// target's contents are a command line.
//
// ok is false whenever there is nothing to classify: a key press (which types
// no characters — pressing enter is confirmed on its own terms), a target
// that is not a terminal, a payload the ordinary checks already refuse, or a
// daemon built without a gate.
//
// It never captures and never writes: the capture is made by the gate hooks
// that need one, and this is a pure question asked of the same Policy
// `shell.run` is asked. That matters for the audit story — the answer here
// must be the answer there, and the only way to guarantee that is for it to
// be literally the same call.
func (t *typingTool) commandVerdict(args typingArgs, target desktop.Window) (Verdict, bool) {
	if t.press || t.t.classify == nil {
		return Verdict{}, false
	}
	if !t.t.isTerminal(target) {
		return Verdict{}, false
	}
	payload, _, refusal := t.checkPayload(args)
	if refusal != "" {
		return Verdict{}, false
	}
	command := strings.TrimSpace(payload)
	if command == "" {
		return Verdict{}, false
	}
	return t.t.classify(command), true
}

// Refuse implements Refusing: a command the permission gate DENIES is not
// typeable into a terminal, whatever the typing tier says.
//
// This is the one direction the tiering could not express. Escalate can turn
// allow into ask; nothing else could turn an explicit
// `[tools.policy.tool]."typing.type_text" = "allow"` into a refusal, and
// without that, switching typing on would be a way to run `rm -rf /` by
// spelling it into a shell. Refusing is consulted before any policy — before
// the tiers, before a global allow, before the no-policy case — which is
// exactly the strength this needs (issue #105's wall, borrowed for a
// different reason).
//
// The reason is spoken-ready and names the rule, because "I can't do that"
// without saying what is off limits is the failure mode the interface exists
// to avoid.
func (t *typingTool) Refuse(input json.RawMessage) (reason string, ok bool) {
	// Nothing to classify, so nothing to look at: a key press types no
	// characters, and a daemon with no gate has no verdict to honour. Checked
	// before the compositor is consulted, because this hook runs on every
	// call and a wasted round trip here is a pause before every question.
	if t.press || t.t.classify == nil {
		return "", false
	}
	var args typingArgs
	if err := json.Unmarshal(coalesceArgs(input), &args); err != nil {
		return "", false
	}
	if _, _, refusal := t.checkPayload(args); refusal != "" {
		return "", false // Execute refuses it outright; refusing twice says nothing new
	}
	// A LOOK, not a capture. Refusing runs before any policy — including on a
	// daemon with no gate installed at all — and a capture made here would be
	// a capture the gate never made, which is precisely the thing the whole
	// capture mechanism exists to make impossible: keystrokes only ever land
	// in a window the gate itself resolved and the user was asked about.
	target, found := t.t.peek()
	if !found {
		return "", false
	}
	verdict, classified := t.commandVerdict(args, target)
	if !classified || verdict.Decision != PolicyDeny {
		return "", false
	}
	return fmt.Sprintf("typing that into %s would run it, and it is refused by %s",
		desktop.AppName(target.Class), verdict.Rule), true
}

// Confirmation implements Confirmable: the ask tier's question, built from the
// live inventory and the literal payload.
//
// Both halves matter and neither comes from the model. The window is the one
// the compositor says has focus, described the way the user would recognise
// it; the text is the characters that will be typed, verbatim. A model cannot
// describe "sudo rm -rf /" as "a note to self", because the sentence the user
// hears is not written by the model at all.
//
// The resolution behind that sentence is kept, and Execute checks it again —
// so approving *this* window is not an approval that survives the window
// changing underneath it.
func (t *typingTool) Confirmation(input json.RawMessage) (command, summary string, ok bool) {
	var args typingArgs
	if err := json.Unmarshal(coalesceArgs(input), &args); err != nil {
		return "", "", false
	}
	payload, key, refusal := t.checkPayload(args)
	if refusal != "" {
		return "", "", false // Execute will refuse and explain; there is nothing to approve
	}
	target, ok := t.t.capture(t.Name(), payload, true)
	if !ok {
		return "", "", false
	}

	where := target.Describe()
	terminal := ""
	if t.t.isTerminal(target) {
		terminal = ", which is a terminal, so anything typed there could be run as a command"
	}
	if t.press {
		// Named, not spelled: "press enter" is what the user is approving, and
		// the sentence says what pressing it there would mean.
		return fmt.Sprintf("press %s in %s", key, where),
			fmt.Sprintf("I want to press %s in %s%s. Should I go ahead?", key, where, terminal), true
	}
	// A terminal target makes this a command, so it is confirmed as one: the
	// card names the command verbatim and says what the shell classifier
	// found in it, exactly as `shell.run` would (ADR 0014). The verbatim
	// display is the whole point — a model cannot describe `sudo rm -rf ~` as
	// "a note to self" when the sentence the user hears is not written by the
	// model at all.
	if verdict, classified := t.commandVerdict(args, target); classified {
		command := strings.TrimSpace(payload)
		if verdict.Reason != "" {
			return fmt.Sprintf("type %q into %s, where it would run", command, where),
				fmt.Sprintf("I want to type %q into %s, which is a terminal, so it would run as a "+
					"command — and it %s. Should I go ahead?", spokenCommand(command), where, verdict.Reason), true
		}
		return fmt.Sprintf("type %q into %s, where it would run", command, where),
			fmt.Sprintf("I want to type %q into %s, which is a terminal, so it would run as a "+
				"command. Should I go ahead?", spokenCommand(command), where), true
	}
	// Everywhere else, command is published on the bus and written to the
	// daemon's journal, so it carries the length and never the characters.
	// The summary — spoken, and shown by the overlay for exactly as long as
	// the question stands — is where the literal text belongs.
	return fmt.Sprintf("type %s into %s", plural(len([]rune(payload)), "character", "characters"), where),
		fmt.Sprintf("I want to type %q into %s%s. Should I go ahead?",
			spokenPayload(payload), where, terminal), true
}

// Escalate implements Escalating: a call the configured tier would have run
// silently is pushed back to ask when the focused window is a terminal.
//
// This is the one place a tool may make the gate stricter, and the reason is
// that the tier cannot be decided from configuration alone. "May Jarvix type?"
// has one answer; "may Jarvix type *into a shell*?" has another, and which
// question is being asked depends on where focus happens to be at this
// instant — which only the tool can see.
//
// It runs on every allow-tier call, which is also where the focus capture for
// that tier is made: the allow path must re-check focus at execution too, and
// it needs something to re-check against.
func (t *typingTool) Escalate(input json.RawMessage) (rule string, ok bool) {
	var args typingArgs
	if err := json.Unmarshal(coalesceArgs(input), &args); err != nil {
		return "", false
	}
	payload, _, refusal := t.checkPayload(args)
	if refusal != "" {
		return "", false // Execute refuses it outright; asking about it would be worse
	}
	target, captured := t.t.capture(t.Name(), payload, false)
	if !captured {
		return "", false
	}
	if !t.t.isTerminal(target) {
		return "", false
	}
	// The command classification decides the wording, and — in one case —
	// whether the question is asked at all.
	//
	//   - Ask names the classifier's own rule, so an escalated call reads in
	//     the audit trail exactly as the same command through `shell.run`
	//     would: `risky command "rm"`, not "it is a terminal". Deny is
	//     Refuse's job, above.
	//   - A *user-granted* allow — a `[tools.policy] shell_allow` pattern or
	//     a conversation grant (#162) — is the ONE thing that stands down the
	//     terminal escalation. The user has already said "yes, for good" to
	//     this exact command shape, and asking again is the feature failing at
	//     its one job. The run is not silent: it still earns its audit row,
	//     naming the granted rule, so the evidence survives the question
	//     going away.
	//
	// A shipped read-only allow pattern is deliberately NOT enough. `ls` runs
	// unprompted through `shell.run` because nothing it can do is worth a
	// question; typing into a shell is a different act with a different
	// failure mode (ADR 0023 — the keys land wherever focus is, and a
	// mistake is neither visible before nor undoable after), and that
	// escalation stands until the user personally sets it aside.
	verdict, classified := t.commandVerdict(args, target)
	switch {
	case classified && verdict.Decision == PolicyAsk:
		return fmt.Sprintf("%s, and typing it into %s would run it",
			verdict.Rule, desktop.AppName(target.Class)), true
	case classified && verdict.Decision == PolicyAllow && verdict.PreApproved:
		return "", false
	}
	return fmt.Sprintf("the focused window (%s) is a terminal", desktop.AppName(target.Class)), true
}

// capture resolves the focused window and remembers it for this call. asked
// records whether the capture was made to build a question, which is how the
// audit trail distinguishes "a human approved this" from "the configured tier
// allowed it".
func (t *Typing) capture(tool, payload string, asked bool) (desktop.Window, bool) {
	// The gate has no context of its own — it is a synchronous decision on the
	// session's think goroutine. Bound it tightly: a compositor that will not
	// answer must cost a generic question, not a pause before one.
	ctx, cancel := context.WithTimeout(context.Background(), t.windows.timeout)
	defer cancel()
	found, err := t.focused(ctx)
	if err != nil || found.Address == "" {
		return desktop.Window{}, false
	}
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	for key, held := range t.captures {
		if now.Sub(held.at) > captureTTL {
			delete(t.captures, key)
		}
	}
	t.captures[captureKey(tool, payload)] = focusCapture{window: found, at: now, asked: asked}
	return found, true
}

// peek resolves the focused window without remembering it — the read-only
// half of capture, for the gate hooks that must decide something about the
// target without manufacturing an authorisation to act on it (Refuse).
func (t *Typing) peek() (desktop.Window, bool) {
	// The gate has no context of its own; bound it tightly, as capture does.
	ctx, cancel := context.WithTimeout(context.Background(), t.windows.timeout)
	defer cancel()
	found, err := t.focused(ctx)
	if err != nil || found.Address == "" {
		return desktop.Window{}, false
	}
	return found, true
}

// takeCapture consumes the capture held for this call. Consumed rather than
// read: an approval authorises one action, and a second identical call is a
// second decision.
func (t *Typing) takeCapture(tool, payload string) (w desktop.Window, approved, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := captureKey(tool, payload)
	held, found := t.captures[key]
	if !found {
		return desktop.Window{}, false, false
	}
	delete(t.captures, key)
	if t.now().Sub(held.at) > captureTTL {
		return desktop.Window{}, false, false
	}
	return held.window, held.asked, true
}

// captureKey identifies one call: the tool plus the exact payload, so an
// approval for one piece of text is never spent on another.
func captureKey(tool, payload string) string { return tool + "\x00" + payload }

// focused returns the window that has focus right now, or a zero Window when
// nothing does. It goes through the window tools' matcher with an empty
// reference, which is how every other "the window the user is in" is resolved
// — one inventory, one cache, one definition of focus.
func (t *Typing) focused(ctx context.Context) (desktop.Window, error) {
	res, err := t.windows.resolve(ctx, "")
	if err != nil {
		return desktop.Window{}, err
	}
	if res.Kind != resolveOne {
		return desktop.Window{}, nil
	}
	return res.Window, nil
}

// sameWindow reports whether two captures are the same window.
//
// Identity is the address plus the compositor's own window id and the
// application class — the same three things the window tools verify with, and
// for the same reason: an address is a reusable handle, so matching on it
// alone would let a window created since the capture inherit an approval given
// about a different one.
func sameWindow(a, b desktop.Window) bool {
	return a.Address != "" && a.Address == b.Address &&
		a.StableID == b.StableID && strings.EqualFold(a.Class, b.Class)
}

// isTerminal reports whether a window's contents are a command line.
func (t *Typing) isTerminal(w desktop.Window) bool {
	return isTerminalClass(w, t.terminals)
}

// jarvixIdentityPrefix is launchkind's reverse-DNS namespace for the windows
// Jarvix opens inside a terminal (#198). Restated rather than imported so this
// file keeps its short dependency list; a guard test pins the two spellings
// together.
const jarvixIdentityPrefix = "dev.jarvix."

// isTerminalClass is the ONE definition of "this window's contents are a
// command line", shared by the typing gate and the managed-window surface.
// Two definitions would drift, and this one drifting means a shell Jarvix
// does not recognise as one — which is the exact hole #197 exists to close.
//
// configured is the [tools.typing] terminal_classes list; empty uses the
// shipped one.
//
// The reverse-DNS clause is the part worth arguing. A window Jarvix opened
// inside a terminal wears an identity of its own (#198), so its class is no
// longer the terminal's — `ghostty -e claude` produces a window classed
// `dev.jarvix.claude`, and a class-list match would miss it. Every such window
// IS a terminal emulator window, and `ghostty -e bash` is one that is
// literally a shell, so the whole namespace counts. The price is a
// command-shaped confirmation for text dictated into a TUI Jarvix opened,
// which is the safe direction to be wrong in.
func isTerminalClass(w desktop.Window, configured []string) bool {
	class := strings.ToLower(strings.TrimSpace(w.Class))
	app := strings.ToLower(desktop.AppName(w.Class))
	if strings.HasPrefix(class, jarvixIdentityPrefix) {
		return true
	}
	if len(configured) == 0 {
		configured = DefaultTerminalClasses
	}
	for _, entry := range configured {
		e := strings.ToLower(strings.TrimSpace(entry))
		if e == "" {
			continue
		}
		if e == class || e == app {
			return true
		}
	}
	return false
}

// rateLimited reports whether this action falls outside the rate limit, and
// records it when it does not. A runaway loop must not be able to type
// indefinitely, and the limit refuses with a reason rather than silently
// dropping the call, so the model is told to stop rather than left to retry.
func (t *Typing) rateLimited() (reason string, limited bool) {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	kept := t.recent[:0]
	for _, at := range t.recent {
		if now.Sub(at) < t.rateOver {
			kept = append(kept, at)
		}
	}
	t.recent = kept
	if len(t.recent) >= t.rateLimit {
		return fmt.Sprintf("typing has already happened %d times in the last %s, which is the limit",
			t.rateLimit, humanWindow(t.rateOver)), true
	}
	t.recent = append(t.recent, now)
	return "", false
}

// humanWindow renders the rate window for a sentence someone hears.
func humanWindow(d time.Duration) string {
	switch {
	case d >= time.Hour:
		return plural(int(d/time.Hour), "hour", "hours")
	case d >= time.Minute:
		return plural(int(d/time.Minute), "minute", "minutes")
	default:
		return plural(int(d/time.Second), "second", "seconds")
	}
}

// inject performs the action. Everything before this point decided *whether*;
// this is the only place a keystroke is produced.
func (t *Typing) inject(ctx context.Context, tool string, w desktop.Window, chars int, payload, key string, approved bool, mark commandMark) string {
	callCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()

	var err error
	if key != "" {
		err = t.kb.Press(callCtx, key)
	} else {
		err = t.kb.Type(callCtx, payload)
	}
	if err != nil {
		// The injector's own diagnostics stay daemon-side: they are the
		// operator's material, and anything returned here may be read aloud.
		// They also cannot be allowed near the payload.
		t.log.Warn("typing failed", "component", "tools", "tool", tool,
			"class", w.Class, "chars", chars, "key", key, "error", err.Error())
		t.audit(TypingAudit{Tool: tool, Window: w.Describe(), Class: w.Class, Chars: chars,
			Key: key, Terminal: t.isTerminal(w), Approved: approved,
			Command: mark.Command, Rule: mark.Rule,
			Outcome: "unavailable", Reason: "the keyboard could not be reached"})
		return "Nothing could be typed: this computer has no way to send keystrokes. Tell the user " +
			"in one short sentence, and do not retry."
	}

	// The audit line. Class, length and outcome — never the characters, and
	// never the window title, because both are content and the journal outlives
	// the conversation.
	t.log.Info("typed", "component", "tools", "tool", tool, "class", w.Class,
		"chars", chars, "key", key, "terminal", t.isTerminal(w), "approved", approved)
	outcome, done := "typed", fmt.Sprintf("Typed the text into %s.", w.Describe())
	if key != "" {
		outcome, done = "pressed", fmt.Sprintf("Pressed %s in %s.", key, w.Describe())
	}
	t.audit(TypingAudit{Tool: tool, Window: w.Describe(), Class: w.Class, Chars: chars,
		Key: key, Terminal: t.isTerminal(w), Approved: approved,
		Command: mark.Command, Rule: mark.Rule, Outcome: outcome})
	return done + " Confirm it to the user in one short sentence. Never repeat the text back — it " +
		"is already on their screen, and it may be private."
}

// focusChanged is the answer to the race this whole file exists to survive.
// Nothing was typed, and the model is told plainly enough that it does not
// treat the refusal as a transient failure to retry into whatever is in front
// now.
func (t *Typing) focusChanged(tool string, was, now desktop.Window, chars int, key string, approved bool) string {
	t.log.Info("typing abandoned: focus changed", "component", "tools", "tool", tool,
		"was", was.Class, "now", now.Class, "chars", chars, "approved", approved)
	t.audit(TypingAudit{Tool: tool, Window: was.Describe(), Class: was.Class, Chars: chars,
		Key: key, Terminal: t.isTerminal(was), Approved: approved, Outcome: "focus-changed",
		Reason: "focus moved to " + describeOrNothing(now) + " before it could be typed"})
	return fmt.Sprintf("Nothing was typed: the user moved to %s, and this was meant for %s. Tell "+
		"them in one short sentence that the window changed so you did not type anything, and ask "+
		"whether they still want it. Do not retry on your own.",
		describeOrNothing(now), was.Describe())
}

// describeOrNothing names a window, or says there is none.
func describeOrNothing(w desktop.Window) string {
	if w.Address == "" {
		return "another window"
	}
	return w.Describe()
}

// refuse records and explains a refusal by one of the caps or checks.
func (t *Typing) refuse(tool string, w desktop.Window, chars int, key string, approved bool, reason string) string {
	return t.refuseCommand(tool, w, chars, key, approved, commandMark{}, reason)
}

// refuseCommand is refuse for a payload the gate classified as a command: the
// same row, with the command and the rule that refused it. A denied command
// must be as findable afterwards as an allowed one — more so, because it is
// the row that says the gate did its job.
func (t *Typing) refuseCommand(tool string, w desktop.Window, chars int, key string, approved bool,
	mark commandMark, reason string) string {
	t.log.Info("typing refused", "component", "tools", "tool", tool,
		"class", w.Class, "chars", chars, "key", key, "rule", mark.Rule, "reason", reason)
	t.audit(TypingAudit{Tool: tool, Window: w.Describe(), Class: w.Class, Chars: chars,
		Key: key, Terminal: t.isTerminal(w), Approved: approved,
		Command: mark.Command, Rule: mark.Rule, Outcome: "refused", Reason: reason})
	return fmt.Sprintf("Nothing was typed: %s. Tell the user in one short sentence why, and do not "+
		"retry the same way.", reason)
}

// unavailable is what both tools say when the desktop cannot be seen. A tool
// result, never an error: a compositor Jarvix cannot reach is a thing to
// mention in one sentence, not a failed session.
func (t *Typing) unavailable(tool string, chars int, key string, err error) string {
	t.log.Warn("compositor unavailable", "component", "tools", "tool", tool, "error", err.Error())
	t.audit(TypingAudit{Tool: tool, Chars: chars, Key: key, Outcome: "unavailable",
		Reason: "the window manager could not be reached"})
	return "Nothing was typed: the window manager is not available, so there is no way to tell " +
		"where the text would go. Tell the user in one short sentence, and do not retry."
}

func (t *Typing) audit(a TypingAudit) {
	if t.onAudit != nil {
		t.onAudit(a)
	}
}

// spokenPayload bounds how much of the payload is read aloud. The bus event
// carries the whole summary; speech only needs enough to recognise it.
func spokenPayload(payload string) string {
	runes := []rune(payload)
	if len(runes) <= maxSpokenPayload {
		return payload
	}
	return string(runes[:maxSpokenPayload]) + "…"
}

// coalesceArgs treats an absent argument object as an empty one, so a
// malformed-but-empty call is refused by the payload checks (with a sentence
// the user hears) rather than as a session error.
func coalesceArgs(input json.RawMessage) []byte {
	trimmed := strings.TrimSpace(string(input))
	if trimmed == "" || trimmed == "null" {
		return []byte("{}")
	}
	return []byte(trimmed)
}

// boundedKeyName trims a rejected key name to something a log line and a
// spoken sentence can carry. It is the only string in this file the model
// chooses freely and that is repeated back, so it is bounded where it is
// produced rather than wherever it happens to be printed.
func boundedKeyName(name string) string {
	const maxKeyName = 32
	runes := []rune(name)
	if len(runes) <= maxKeyName {
		return name
	}
	return string(runes[:maxKeyName]) + "…"
}
