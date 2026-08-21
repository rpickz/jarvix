package desktop

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unicode"
)

// This file is the keystroke seam (ADR 0023). It sits beside the compositor
// seam rather than inside it, because the two answer different questions: the
// compositor knows *which windows exist and which one has focus*, and a
// virtual keyboard knows *how to put characters into whichever one does*.
// Typing therefore reuses the compositor for every decision it makes — the
// inventory, the resolution, the re-verification — and reaches this interface
// only once all of them have been made.
//
// Three properties shape it, and each is a safety requirement rather than a
// convenience:
//
//   - It types text; it does not interpret it. There is no escape syntax, no
//     key-name expansion inside a string, and no way to spell a control key in
//     the text argument. A newline in the payload cannot become Return here
//     because Return is not reachable from Type at all.
//   - Literal characters only, filtered by construction. Text passes through
//     Literal before it can become argv, so even a caller that skipped its own
//     validation cannot deliver a control character to the user's keyboard.
//   - Pressing a key is a different method with a closed vocabulary. Submitting
//     is a separate act from composing (issue #37), and the type system says so:
//     approving "type this" cannot reach Press, and Press cannot type text.

// Keyboard is the synthetic-keystroke seam. Implementations are short-lived
// subprocesses in production and fakes in tests; no test in this tree may
// synthesise a keystroke into the session running it.
//
// Every method must honour ctx: the tool timeouts, and cancelling a session
// mid-word, are enforced through it.
type Keyboard interface {
	// Describe names the injector and proves it can reach the compositor's
	// virtual-keyboard protocol, for `jarvix doctor`. The error is what
	// "unavailable" means here: no binary, no Wayland session, or a compositor
	// that will not hand out a virtual keyboard. It must not type anything.
	Describe(ctx context.Context) (string, error)
	// Type enters text as literal characters. Implementations must never
	// interpret the string: no escapes, no key names, no control characters.
	Type(ctx context.Context, text string) error
	// Press sends one named key (see KeyNames) as a press-and-release. It is
	// the only route to Return, Tab and the rest, and it exists separately so
	// that the permission gate can price it separately.
	Press(ctx context.Context, key string) error
}

// Keystroke bounds. Typing is a local subprocess that talks to the compositor
// over a Wayland protocol; if it has not finished in this long, something is
// wrong and the user is owed a sentence rather than a longer silence.
const (
	// DefaultKeyboardTimeout bounds one injector invocation.
	DefaultKeyboardTimeout = 3 * time.Second
	// maxKeyboardOutput caps a keystroke tool's captured output, which is
	// diagnostics and nothing else. Deliberately tiny: nothing this runs has
	// anything to say beyond one error line, and the output of a *typing* tool
	// is the last thing that should be able to grow without bound.
	maxKeyboardOutput = 4 * 1024
)

// ErrNoKeyboard is what an unavailable injector reports: no binary, no Wayland
// session, or a compositor without the virtual-keyboard protocol. Callers turn
// it into one spoken sentence, never a failed session.
var ErrNoKeyboard = errors.New("no virtual keyboard is available")

// KeyNames is the closed vocabulary of keys that may be pressed, mapped to the
// libxkbcommon keysyms the injector understands.
//
// It is a table rather than a validator for the reason the window tools use an
// inventory rather than a command string: what reaches argv must be a value
// Jarvix chose, never a string the model wrote. A model asking for "Return" or
// "enter" gets the same keysym; a model asking for anything not listed gets a
// refusal, and there is no spelling of "run this shell command" that appears
// here.
//
// The list is short on purpose. It holds the keys a person dictating into a
// form actually needs — submit, move between fields, correct a mistake — and
// nothing that composes with a modifier, because "send a key combination" is
// how a keystroke stream closes a dialog or quits an application.
var KeyNames = map[string]string{
	"enter":     "Return",
	"return":    "Return",
	"tab":       "Tab",
	"escape":    "Escape",
	"esc":       "Escape",
	"backspace": "BackSpace",
	"delete":    "Delete",
	"up":        "Up",
	"down":      "Down",
	"left":      "Left",
	"right":     "Right",
	"home":      "Home",
	"end":       "End",
}

// Keysym resolves a spoken key name to the keysym that may be pressed. ok is
// false for everything else, which is the answer to every attempt to reach a
// modifier, a function key, or a chord.
func Keysym(name string) (string, bool) {
	sym, ok := KeyNames[strings.ToLower(strings.TrimSpace(name))]
	return sym, ok
}

// Literal returns text with everything that is not a printable character
// removed, and reports how many runes that cost.
//
// This is the "by construction" half of the control-character rule. The tool
// refuses a payload that needs filtering rather than typing a silently altered
// version of what the user approved — but the filter still runs here, at the
// last point before argv, so that a bug in a caller cannot deliver a newline to
// a terminal.
//
// Format characters (Cf) go too, and for a reason worth stating: the
// confirmation the user approves shows the text. A zero-width space or a
// right-to-left override is invisible in that sentence, so a payload
// containing one is not the payload the user read.
func Literal(text string) (clean string, removed int) {
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		if isTypable(r) {
			b.WriteRune(r)
			continue
		}
		removed++
	}
	return b.String(), removed
}

// isTypable reports whether a rune may be typed. Everything in Unicode's
// "other" category is refused — control (Cc, which is where newline, tab and
// escape live), format (Cf), surrogate (Cs) and private use (Co) — and so are
// the line and paragraph separators (Zl, Zp), which are line breaks wearing a
// different category. What is left is a character that renders, which is
// exactly the set whose confirmation sentence tells the truth.
func isTypable(r rune) bool {
	if r == utf8Invalid {
		return false
	}
	return !unicode.In(r, unicode.C, unicode.Zl, unicode.Zp)
}

// utf8Invalid is the replacement rune a malformed byte sequence decodes to.
// Refused: it is not what the user was shown, whatever it renders as.
const utf8Invalid = '�'

// Wtype drives wtype, the Wayland virtual-keyboard client, as a short-lived
// subprocess — the same trade the compositor driver makes (ADR 0002): no
// protocol library to track, and a missing binary degrades to a sentence.
//
// wtype is deliberate rather than incidental. The alternative, ydotool, needs
// a root daemon and write access to /dev/uinput, which is a permanently
// elevated privilege on the machine in exchange for the same keystrokes; wtype
// talks the virtual-keyboard protocol as the user, over their own Wayland
// socket, and exists only for the milliseconds it is typing.
type Wtype struct {
	// Binary overrides the wtype executable (tests, unusual installs). Empty
	// means "wtype" from PATH.
	Binary string
	// Timeout bounds one invocation. Zero means DefaultKeyboardTimeout.
	Timeout time.Duration
}

// Describe implements Keyboard. It types the empty string, which is the whole
// probe: wtype still connects to the Wayland display and binds the
// virtual-keyboard protocol, so success proves both, and no key is pressed.
// `jarvix doctor` must be able to answer "would typing work here?" without
// typing anything into whatever the user has open.
func (w *Wtype) Describe(ctx context.Context) (string, error) {
	if _, err := w.run(ctx, "--", ""); err != nil {
		return "", err
	}
	return "wtype (Wayland virtual-keyboard protocol)", nil
}

// Type implements Keyboard.
//
// The argv shape is the security argument. `--` ends option parsing, so the
// payload is one whole argument that wtype can only treat as text: a payload
// beginning `-k` is characters, never a key press, and there is no shell
// anywhere to reinterpret it. The filter runs first, so no control character
// reaches the argument in the first place.
func (w *Wtype) Type(ctx context.Context, text string) error {
	clean, removed := Literal(text)
	if removed > 0 {
		// Callers validate before they get here; this is the construction that
		// makes the rule hold even when one does not.
		return fmt.Errorf("refusing to type %d non-printable character(s)", removed)
	}
	if clean == "" {
		return nil
	}
	_, err := w.run(ctx, "--", clean)
	return err
}

// Press implements Keyboard. The keysym is looked up in KeyNames, so what
// reaches argv is a constant from this package whatever the caller passed.
func (w *Wtype) Press(ctx context.Context, key string) error {
	sym, ok := Keysym(key)
	if !ok {
		return fmt.Errorf("refusing to press unknown key %q", key)
	}
	_, err := w.run(ctx, "-k", sym)
	return err
}

// run executes one wtype invocation. Same process discipline as every other
// subprocess Jarvix starts: no shell, its own process group, killed as a group
// when the context ends, stdin closed, output capped.
//
// stdin closed matters more here than anywhere else: wtype reads text to type
// from stdin when given `-`, and a typing tool that could be fed a stream is a
// typing tool without a length cap.
func (w *Wtype) run(ctx context.Context, args ...string) (string, error) {
	timeout := w.Timeout
	if timeout <= 0 {
		timeout = DefaultKeyboardTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	binary := binaryOr(w.Binary, "wtype")
	cmd := exec.CommandContext(callCtx, binary, args...) //nolint:gosec // binary is configuration; argv is package constants plus a payload filtered by Literal
	out := &capped{max: maxKeyboardOutput}
	cmd.Stdout, cmd.Stderr, cmd.Stdin = out, out, nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid: the whole group, so nothing survives a cancelled
		// session still holding a modifier down.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = time.Second

	if err := cmd.Run(); err != nil {
		if text := firstLine(out.String()); text != "" {
			return "", fmt.Errorf("%w: %s: %s", ErrNoKeyboard, binary, text)
		}
		return "", fmt.Errorf("%w: %s: %v", ErrNoKeyboard, binary, err)
	}
	return out.String(), nil
}
