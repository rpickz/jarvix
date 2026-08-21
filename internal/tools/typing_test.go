package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/desktop"
)

// Every test here runs against a fake compositor and a fake keyboard. Nothing
// in this file synthesises a keystroke: the person running `go test` is working
// in the session it runs in, and a test that typed would type into whatever
// they had open. desktop.FakeKeyboard records; it never presses.

// typingHarness wires the two typing tools the way the daemon does — through a
// real registry and a real permission gate — because most of what is being
// asserted is the interaction between them. Half the safety of this feature
// lives in the gate (which tier, which question), and testing the tool in
// isolation would test the half that is easy.
type typingHarness struct {
	typing   *Typing
	windows  *Desktop
	comp     *desktop.FakeCompositor
	kb       *desktop.FakeKeyboard
	registry *Registry
	logs     *bytes.Buffer

	mu     sync.Mutex
	audits []TypingAudit
	clock  time.Time
}

// typingSetup are the knobs the tests vary.
type typingSetup struct {
	windows   []desktop.Window
	tiers     map[string]PolicyDecision
	def       PolicyDecision
	maxChars  int
	rateLimit int
	rateOver  time.Duration
	terminals []string
}

func newTypingHarness(t *testing.T, setup typingSetup) *typingHarness {
	t.Helper()
	if setup.windows == nil {
		setup.windows = testWindows()
	}
	if setup.def == "" {
		setup.def = PolicyAsk
	}
	h := &typingHarness{
		comp:  desktop.NewFakeCompositor(setup.windows...),
		kb:    &desktop.FakeKeyboard{},
		logs:  &bytes.Buffer{},
		clock: time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC),
	}
	// Debug level, and every logger in the wiring points here: the assertion
	// that the payload never reaches a log is only worth anything if the test
	// is holding every log it could reach.
	logger := slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h.windows = NewDesktop(DesktopOptions{Compositor: h.comp, launcher: &fakeLauncher{}, Log: logger})
	h.typing = NewTyping(TypingOptions{
		Windows:         h.windows,
		Keyboard:        h.kb,
		MaxChars:        setup.maxChars,
		RateLimit:       setup.rateLimit,
		RateWindow:      setup.rateOver,
		TerminalClasses: setup.terminals,
		Log:             logger,
		OnAudit: func(a TypingAudit) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.audits = append(h.audits, a)
		},
		now: h.nowFn,
	})
	policy, err := NewPolicy(PolicyConfig{Default: setup.def, Tools: setup.tiers})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	h.registry = NewRegistry(logger)
	for _, tool := range h.typing.Tools() {
		h.registry.Register(tool)
	}
	h.registry.SetPolicy(policy)
	return h
}

func (h *typingHarness) nowFn() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.clock
}

func (h *typingHarness) advance(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clock = h.clock.Add(d)
}

func (h *typingHarness) lastAudit(t *testing.T) TypingAudit {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.audits) == 0 {
		t.Fatal("no typing audit was recorded")
	}
	return h.audits[len(h.audits)-1]
}

// gate runs the permission gate for one call, exactly as the session engine
// does, and returns the verdict without executing anything. This is the
// "Jarvix asked and the user has not answered yet" moment.
func (h *typingHarness) gate(name string, args any) (ai.ToolCall, Verdict) {
	encoded, err := json.Marshal(args)
	if err != nil {
		panic(err)
	}
	call := ai.ToolCall{Name: name, Arguments: string(encoded)}
	return call, h.registry.Check(call)
}

// approve executes a call the gate asked about, which is what the engine does
// once the user says yes.
func (h *typingHarness) approve(call ai.ToolCall) string {
	return h.registry.Execute(context.Background(), call)
}

// run is gate-then-approve: the whole path for a call the user consents to.
func (h *typingHarness) run(name string, args any) (Verdict, string) {
	call, verdict := h.gate(name, args)
	if verdict.Decision == PolicyDeny {
		return verdict, ""
	}
	return verdict, h.approve(call)
}

func typeArgs(text string) map[string]string { return map[string]string{"text": text} }
func keyArgs(key string) map[string]string   { return map[string]string{"key": key} }

// ---------------------------------------------------------------- the gate

// TestTypingAlwaysAsks: the tier does not come from the gate-wide default.
// Somebody who wrote `default = "allow"` was thinking about reading their
// system state, not about handing the model their keyboard, so allowing typing
// takes naming the tool. A stricter default still wins.
func TestTypingAlwaysAsks(t *testing.T) {
	cases := []struct {
		name  string
		def   PolicyDecision
		tiers map[string]PolicyDecision
		want  PolicyDecision
	}{
		{"the shipped default asks", PolicyAsk, nil, PolicyAsk},
		{"a global allow does not reach typing", PolicyAllow, nil, PolicyAsk},
		{"a global deny does", PolicyDeny, nil, PolicyDeny},
		{"naming the tool allows it", PolicyAsk,
			map[string]PolicyDecision{TypeTextToolName: PolicyAllow}, PolicyAllow},
		{"naming the tool denies it", PolicyAllow,
			map[string]PolicyDecision{TypeTextToolName: PolicyDeny}, PolicyDeny},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTypingHarness(t, typingSetup{def: tc.def, tiers: tc.tiers})
			_, verdict := h.gate(TypeTextToolName, typeArgs("hello"))
			if verdict.Decision != tc.want {
				t.Fatalf("decision = %q, want %q (rule %q)", verdict.Decision, tc.want, verdict.Rule)
			}
		})
	}
}

// TestSubmittingIsItsOwnCapability: approving typing must never imply
// approving Enter. Allowing one tool by name leaves the other asking.
func TestSubmittingIsItsOwnCapability(t *testing.T) {
	h := newTypingHarness(t, typingSetup{
		tiers: map[string]PolicyDecision{TypeTextToolName: PolicyAllow},
	})
	if _, v := h.gate(TypeTextToolName, typeArgs("hello")); v.Decision != PolicyAllow {
		t.Fatalf("typing decision = %q, want allow", v.Decision)
	}
	_, v := h.gate(PressKeyToolName, keyArgs("enter"))
	if v.Decision != PolicyAsk {
		t.Fatalf("press decision = %q, want ask — submitting has its own tier", v.Decision)
	}
	if !strings.Contains(v.Summary, "press enter") {
		t.Errorf("summary = %q, want it to name the key being pressed", v.Summary)
	}
}

// TestTypingApprovalIsNeverRemembered: remember_for_conversation may not carry
// a typing approval forward. The approval was about a payload *and* a window
// that had focus at that moment, and the second half does not survive the
// user moving.
func TestTypingApprovalIsNeverRemembered(t *testing.T) {
	for _, tool := range []string{TypeTextToolName, PressKeyToolName} {
		if RememberableApproval(tool) {
			t.Errorf("%s approvals must never be remembered for the conversation", tool)
		}
	}
	for _, tool := range []string{"shell.run", AdvisorToolName, IntentToolName, CloseWindowToolName} {
		if !RememberableApproval(tool) {
			t.Errorf("%s approvals should still be rememberable", tool)
		}
	}
}

// ------------------------------------------------------- the confirmation

// TestConfirmationNamesTheWindowAndTheText: the sentence the user approves is
// built from the live inventory and the literal payload, so a model cannot
// describe what it is about to type.
func TestConfirmationNamesTheWindowAndTheText(t *testing.T) {
	h := newTypingHarness(t, typingSetup{})
	_, verdict := h.gate(TypeTextToolName, typeArgs("the quick brown fox"))
	if verdict.Decision != PolicyAsk {
		t.Fatalf("decision = %q, want ask", verdict.Decision)
	}
	if !strings.Contains(verdict.Summary, "the quick brown fox") {
		t.Errorf("summary = %q, want the literal text", verdict.Summary)
	}
	// testWindows() has the editor focused.
	if !strings.Contains(verdict.Summary, "code — engine.go") {
		t.Errorf("summary = %q, want the target window named", verdict.Summary)
	}
	// The command is the audited, logged, bus-published form. It says how much,
	// never what: the summary is where the text belongs, and it is shown for
	// exactly as long as the question stands.
	if strings.Contains(verdict.Command, "quick brown fox") {
		t.Errorf("command = %q must not carry the payload — it is logged", verdict.Command)
	}
	if !strings.Contains(verdict.Command, "19 characters") {
		t.Errorf("command = %q, want the length", verdict.Command)
	}
}

// TestConfirmationIsNotAnExecution: asking is not doing. Nothing is typed
// until the call is executed, so a declined or timed-out question types
// nothing at all.
func TestConfirmationIsNotAnExecution(t *testing.T) {
	h := newTypingHarness(t, typingSetup{})
	if _, v := h.gate(TypeTextToolName, typeArgs("hello")); v.Decision != PolicyAsk {
		t.Fatalf("decision = %q, want ask", v.Decision)
	}
	if got := h.kb.Typed(); len(got) != 0 {
		t.Fatalf("nothing may be typed while the question stands, got %q", got)
	}
}

// -------------------------------------------------------- the focus race

// TestFocusChangedBetweenApprovalAndExecution is the critical test of this
// feature, not an edge case.
//
// A spoken confirmation takes seconds. In those seconds a notification can
// steal focus, a dialog can open, or the user can simply click somewhere else
// — and an approval given about one window must never be spent on another. So
// the focused window is captured when the question is asked and checked again
// at the instant of typing, and any difference means nothing is typed.
func TestFocusChangedBetweenApprovalAndExecution(t *testing.T) {
	editor := desktop.Window{Address: "0x1", Class: "code", Title: "engine.go", Workspace: 1,
		Focused: true, StableID: "s1", AcceptsInput: true}
	browser := desktop.Window{Address: "0x2", Class: "firefox", Title: "GitHub", Workspace: 1,
		StableID: "s2", AcceptsInput: true}

	cases := []struct {
		name string
		// after is the inventory as it is when the approval is acted on.
		after     []desktop.Window
		wantTyped bool
		wantSays  string
	}{
		{
			name:      "focus did not move: the text is typed",
			after:     []desktop.Window{editor, browser},
			wantTyped: true,
		},
		{
			name: "another window took focus",
			after: []desktop.Window{
				withFocus(browser, true), withFocus(editor, false),
			},
			wantSays: "firefox",
		},
		{
			name: "the window that had focus has gone",
			after: []desktop.Window{
				withFocus(browser, true),
			},
			wantSays: "firefox",
		},
		{
			name: "nothing has focus at all",
			after: []desktop.Window{
				withFocus(editor, false), withFocus(browser, false),
			},
			wantSays: "another window",
		},
		{
			name: "the address was reused by a different window",
			after: []desktop.Window{
				{Address: "0x1", Class: "kitty", Title: "zsh", Workspace: 1,
					Focused: true, StableID: "s9", AcceptsInput: true},
			},
			wantSays: "kitty",
		},
		{
			name: "the same address, but the compositor gave it a new id",
			after: []desktop.Window{
				{Address: "0x1", Class: "code", Title: "engine.go", Workspace: 1,
					Focused: true, StableID: "s99", AcceptsInput: true},
			},
			wantSays: "code",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTypingHarness(t, typingSetup{windows: []desktop.Window{editor, browser}})

			// Jarvix asks. The window it names is captured now.
			call, verdict := h.gate(TypeTextToolName, typeArgs("dear team"))
			if verdict.Decision != PolicyAsk {
				t.Fatalf("decision = %q, want ask", verdict.Decision)
			}
			if !strings.Contains(verdict.Summary, "code — engine.go") {
				t.Fatalf("summary = %q, want the editor named", verdict.Summary)
			}

			// The desktop moves while the user is answering.
			h.comp.SetWindows(tc.after...)

			// The user says yes.
			result := h.approve(call)

			typed := h.kb.Typed()
			if tc.wantTyped {
				if len(typed) != 1 || typed[0] != "dear team" {
					t.Fatalf("typed = %q, want [dear team]", typed)
				}
				if got := h.lastAudit(t).Outcome; got != "typed" {
					t.Errorf("audit outcome = %q, want typed", got)
				}
				return
			}
			if len(typed) != 0 {
				t.Fatalf("nothing may be typed once focus has changed, got %q", typed)
			}
			if !strings.Contains(result, "Nothing was typed") {
				t.Errorf("result = %q, want it to report that nothing was typed", result)
			}
			if !strings.Contains(result, tc.wantSays) {
				t.Errorf("result = %q, want it to name %q", result, tc.wantSays)
			}
			audit := h.lastAudit(t)
			if audit.Outcome != "focus-changed" {
				t.Errorf("audit outcome = %q, want focus-changed", audit.Outcome)
			}
			if !audit.Approved {
				t.Error("the audit must record that a human had approved this")
			}
		})
	}
}

func withFocus(w desktop.Window, focused bool) desktop.Window {
	w.Focused = focused
	return w
}

// TestFocusIsRecheckedAgainstAFreshInventory: the re-check is worthless if it
// reads the capture back out of a cache. The cache is dropped first, so the
// compositor is asked again after the user answers.
func TestFocusIsRecheckedAgainstAFreshInventory(t *testing.T) {
	h := newTypingHarness(t, typingSetup{})
	call, _ := h.gate(TypeTextToolName, typeArgs("hello"))
	before := h.comp.Reads()
	h.approve(call)
	if h.comp.Reads() <= before {
		t.Fatalf("the compositor was read %d times before and %d after; the approval must cost a fresh look",
			before, h.comp.Reads())
	}
}

// TestPressKeyRechecksFocusToo: submitting is the more dangerous half, so it
// gets the same protection and not less.
func TestPressKeyRechecksFocusToo(t *testing.T) {
	h := newTypingHarness(t, typingSetup{})
	call, verdict := h.gate(PressKeyToolName, keyArgs("enter"))
	if verdict.Decision != PolicyAsk {
		t.Fatalf("decision = %q, want ask", verdict.Decision)
	}
	h.comp.SetWindows(desktop.Window{Address: "0x9", Class: "firefox", Title: "GitHub",
		Focused: true, StableID: "s9", AcceptsInput: true})
	result := h.approve(call)
	if got := h.kb.Pressed(); len(got) != 0 {
		t.Fatalf("no key may be pressed once focus has changed, got %q", got)
	}
	if !strings.Contains(result, "Nothing was typed") {
		t.Errorf("result = %q, want it to report that nothing happened", result)
	}
}

// --------------------------------------------------------------- the caps

// TestControlCharactersAreRefused: literal characters only. A newline in the
// payload is how "type this note" becomes "run this command", so the whole
// call is refused rather than quietly shortened — the user was shown the text,
// and typing a different one would make the confirmation a lie.
func TestControlCharactersAreRefused(t *testing.T) {
	cases := []struct {
		name string
		text string
		ok   bool
	}{
		{"ordinary text", "remember to call the bank", true},
		{"punctuation and symbols", `it's #1 — "quoted" (50%)`, true},
		{"accents and emoji", "café 🎉 日本語", true},
		{"leading and trailing spaces", "  indented  ", true},
		{"a trailing newline", "sudo rm -rf /\n", false},
		{"an embedded newline", "line one\nline two", false},
		{"a carriage return", "sudo reboot\r", false},
		{"a tab", "name\tvalue", false},
		{"an escape sequence", "\x1b[2J", false},
		{"a nul byte", "quiet\x00", false},
		{"a zero-width space", "pay\u200bme", false},
		{"a right-to-left override", "safe\u202egnp.exe", false},
		{"empty", "", false},
		{"whitespace only", "   ", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTypingHarness(t, typingSetup{})
			_, result := h.run(TypeTextToolName, typeArgs(tc.text))
			typed := h.kb.Typed()
			if tc.ok {
				if len(typed) != 1 || typed[0] != tc.text {
					t.Fatalf("typed = %q, want [%q]", typed, tc.text)
				}
				return
			}
			if len(typed) != 0 {
				t.Fatalf("typed = %q, want nothing", typed)
			}
			if !strings.Contains(result, "Nothing was typed") {
				t.Errorf("result = %q, want a refusal with a reason", result)
			}
		})
	}
}

// TestControlCharactersAreNeverAskedAbout: a payload that will be refused must
// not become a question. Asking the user to approve something that cannot
// happen trains them to say yes.
func TestControlCharactersAreNeverAskedAbout(t *testing.T) {
	h := newTypingHarness(t, typingSetup{})
	_, verdict := h.gate(TypeTextToolName, typeArgs("echo hi\n"))
	if strings.Contains(verdict.Summary, "echo hi") {
		t.Errorf("summary = %q, want no question built from a payload that will be refused", verdict.Summary)
	}
}

// TestLengthCap: a runaway loop must not be able to fill a document.
func TestLengthCap(t *testing.T) {
	cases := []struct {
		name  string
		chars int
		ok    bool
	}{
		{"under the cap", 9, true},
		{"exactly at the cap", 10, true},
		{"one over the cap", 11, false},
		{"far over the cap", 5000, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTypingHarness(t, typingSetup{maxChars: 10})
			text := strings.Repeat("a", tc.chars)
			_, result := h.run(TypeTextToolName, typeArgs(text))
			if tc.ok {
				if got := h.kb.Typed(); len(got) != 1 {
					t.Fatalf("typed = %q, want the text", got)
				}
				return
			}
			if got := h.kb.Typed(); len(got) != 0 {
				t.Fatalf("typed = %q, want nothing", got)
			}
			if !strings.Contains(result, "the limit is 10") {
				t.Errorf("result = %q, want the cap named", result)
			}
			if got := h.lastAudit(t).Outcome; got != "refused" {
				t.Errorf("audit outcome = %q, want refused", got)
			}
		})
	}
}

// TestLengthCapCountsRunesNotBytes: a cap that counted bytes would refuse a
// short sentence in most of the world's languages.
func TestLengthCapCountsRunesNotBytes(t *testing.T) {
	h := newTypingHarness(t, typingSetup{maxChars: 5})
	_, result := h.run(TypeTextToolName, typeArgs("日本語です"))
	if got := h.kb.Typed(); len(got) != 1 {
		t.Fatalf("typed = %q, want five characters to fit a five-character cap (%s)", got, result)
	}
}

// TestRateLimit: however many times the model calls, the keyboard is not
// available more often than the configured limit — and the refusal says so,
// rather than silently dropping the call and leaving the model to retry.
func TestRateLimit(t *testing.T) {
	h := newTypingHarness(t, typingSetup{rateLimit: 2, rateOver: time.Minute})
	for i := range 2 {
		if _, result := h.run(TypeTextToolName, typeArgs(fmt.Sprintf("call %d", i))); strings.Contains(result, "Nothing was typed") {
			t.Fatalf("call %d was refused: %s", i, result)
		}
	}
	_, result := h.run(TypeTextToolName, typeArgs("call 3"))
	if got := h.kb.Typed(); len(got) != 2 {
		t.Fatalf("typed %d times, want 2 — the third must be refused", len(got))
	}
	if !strings.Contains(result, "which is the limit") {
		t.Errorf("result = %q, want the limit explained", result)
	}
	if got := h.lastAudit(t).Outcome; got != "refused" {
		t.Errorf("audit outcome = %q, want refused", got)
	}

	// The window slides: once it has passed, typing works again.
	h.advance(61 * time.Second)
	if _, result := h.run(TypeTextToolName, typeArgs("later")); strings.Contains(result, "Nothing was typed") {
		t.Fatalf("the limit should have expired: %s", result)
	}
}

// TestRateLimitIsSharedBetweenBothCapabilities: a loop that alternates typing
// and pressing enter is still a loop.
func TestRateLimitIsSharedBetweenBothCapabilities(t *testing.T) {
	h := newTypingHarness(t, typingSetup{rateLimit: 2, rateOver: time.Minute})
	h.run(TypeTextToolName, typeArgs("one"))
	h.run(PressKeyToolName, keyArgs("enter"))
	_, result := h.run(TypeTextToolName, typeArgs("two"))
	if !strings.Contains(result, "which is the limit") {
		t.Errorf("result = %q, want the shared limit to refuse the third action", result)
	}
}

// ---------------------------------------------------------- the terminal

// TestTerminalEscalation: typing into a shell is the highest-consequence case
// there is, so it asks even when typing is otherwise allowed — and the
// question says why.
func TestTerminalEscalation(t *testing.T) {
	cases := []struct {
		name       string
		class      string
		configured []string
		wantAsk    bool
	}{
		{"an editor is not a terminal", "code", nil, false},
		{"a browser is not a terminal", "firefox", nil, false},
		{"alacritty is", "Alacritty", nil, true},
		{"kitty is", "kitty", nil, true},
		{"ghostty is", "ghostty", nil, true},
		{"a reverse-DNS terminal class is", "org.wezfurlong.wezterm", nil, true},
		{"a configured class is", "myterm", []string{"myterm"}, true},
		{"a configured list replaces the shipped one", "alacritty", []string{"myterm"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTypingHarness(t, typingSetup{
				windows: []desktop.Window{{Address: "0x1", Class: tc.class, Title: "session",
					Focused: true, StableID: "s1", AcceptsInput: true}},
				// Typing explicitly allowed: the escalation must be able to
				// override the user's own "allow", which is the whole point.
				tiers:     map[string]PolicyDecision{TypeTextToolName: PolicyAllow},
				terminals: tc.configured,
			})
			_, verdict := h.gate(TypeTextToolName, typeArgs("ls -la"))
			if !tc.wantAsk {
				if verdict.Decision != PolicyAllow {
					t.Fatalf("decision = %q, want allow (rule %q)", verdict.Decision, verdict.Rule)
				}
				return
			}
			if verdict.Decision != PolicyAsk {
				t.Fatalf("decision = %q, want ask — a terminal escalates", verdict.Decision)
			}
			if !strings.Contains(verdict.Rule, "terminal") {
				t.Errorf("rule = %q, want it to name the reason", verdict.Rule)
			}
			if !strings.Contains(verdict.Summary, "which is a terminal") {
				t.Errorf("summary = %q, want the confirmation to say so explicitly", verdict.Summary)
			}
			if !strings.Contains(verdict.Summary, "ls -la") {
				t.Errorf("summary = %q, want the literal text", verdict.Summary)
			}
		})
	}
}

// TestEscalationOnlyTightens: a tool may turn allow into ask, never the other
// way. Whatever a tool believes about its own call, it cannot talk the gate
// down.
func TestEscalationOnlyTightens(t *testing.T) {
	h := newTypingHarness(t, typingSetup{
		windows: []desktop.Window{{Address: "0x1", Class: "code", Title: "engine.go",
			Focused: true, StableID: "s1", AcceptsInput: true}},
		tiers: map[string]PolicyDecision{TypeTextToolName: PolicyDeny},
	})
	_, verdict := h.gate(TypeTextToolName, typeArgs("hello"))
	if verdict.Decision != PolicyDeny {
		t.Fatalf("decision = %q, want deny to survive", verdict.Decision)
	}
}

// TestTerminalIsAuditedAsSuch: the audit trail records that the target was a
// terminal, because "it typed into a shell" is the line an operator is looking
// for afterwards.
func TestTerminalIsAuditedAsSuch(t *testing.T) {
	h := newTypingHarness(t, typingSetup{
		windows: []desktop.Window{{Address: "0x1", Class: "Alacritty", Title: "zsh",
			Focused: true, StableID: "s1", AcceptsInput: true}},
	})
	h.run(TypeTextToolName, typeArgs("git status"))
	if audit := h.lastAudit(t); !audit.Terminal {
		t.Errorf("audit = %+v, want Terminal true", audit)
	}
}

// ------------------------------------------------------------- the audit

// TestTypedTextNeverReachesALogSink is the privacy guarantee, asserted rather
// than asserted-about.
//
// The user may have dictated a password. The journal outlives the
// conversation, so the characters must not be in it — not in the tool's own
// log line, not in the registry's, not in the audit event the daemon retains
// and `jarvix status --last` prints, and not in the command the gate publishes
// and logs verbatim. The one place the text legitimately appears is the spoken
// question, which exists for exactly as long as the question does.
func TestTypedTextNeverReachesALogSink(t *testing.T) {
	const secret = "hunter2-correct-horse-battery-staple"
	h := newTypingHarness(t, typingSetup{})

	call, verdict := h.gate(TypeTextToolName, typeArgs(secret))
	if !strings.Contains(verdict.Summary, secret) {
		t.Fatalf("the spoken question must show what will be typed; summary = %q", verdict.Summary)
	}
	if strings.Contains(verdict.Command, secret) {
		t.Errorf("verdict.Command is logged and published verbatim; it must not carry the payload: %q",
			verdict.Command)
	}
	if strings.Contains(verdict.Rule, secret) {
		t.Errorf("verdict.Rule is logged; it must not carry the payload: %q", verdict.Rule)
	}
	result := h.approve(call)
	if got := h.kb.Typed(); len(got) != 1 || got[0] != secret {
		t.Fatalf("typed = %q, want the text to have been typed", got)
	}
	if strings.Contains(result, secret) {
		t.Errorf("the tool result goes back to the model and may be spoken; it must not repeat the payload: %q", result)
	}
	if logged := h.logs.String(); strings.Contains(logged, secret) {
		t.Errorf("the payload reached a log sink:\n%s", logged)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, a := range h.audits {
		if strings.Contains(fmt.Sprintf("%+v", a), secret) {
			t.Errorf("the payload reached the audit trail: %+v", a)
		}
	}
}

// TestTextIsNotLoggedOnAnyPath walks every outcome, because a privacy
// guarantee that holds only on the happy path is not one.
func TestTextIsNotLoggedOnAnyPath(t *testing.T) {
	const secret = "s3cret-passphrase"
	paths := map[string]func(*typingHarness) (ai.ToolCall, bool){
		"typed": func(h *typingHarness) (ai.ToolCall, bool) {
			call, _ := h.gate(TypeTextToolName, typeArgs(secret))
			return call, true
		},
		"focus changed": func(h *typingHarness) (ai.ToolCall, bool) {
			call, _ := h.gate(TypeTextToolName, typeArgs(secret))
			h.comp.SetWindows(desktop.Window{Address: "0x8", Class: "firefox", Title: "GitHub",
				Focused: true, StableID: "s8", AcceptsInput: true})
			return call, true
		},
		"refused for length": func(h *typingHarness) (ai.ToolCall, bool) {
			call, _ := h.gate(TypeTextToolName, typeArgs(secret+strings.Repeat("!", 1000)))
			return call, true
		},
		"refused for a control character": func(h *typingHarness) (ai.ToolCall, bool) {
			call, _ := h.gate(TypeTextToolName, typeArgs(secret+"\n"))
			return call, true
		},
		"the keyboard was unavailable": func(h *typingHarness) (ai.ToolCall, bool) {
			call, _ := h.gate(TypeTextToolName, typeArgs(secret))
			h.kb.Err = desktop.ErrNoKeyboard
			return call, true
		},
		"the compositor was unavailable": func(h *typingHarness) (ai.ToolCall, bool) {
			call, _ := h.gate(TypeTextToolName, typeArgs(secret))
			h.comp.Err = desktop.ErrNoCompositor
			return call, true
		},
	}
	for name, arrange := range paths {
		t.Run(name, func(t *testing.T) {
			h := newTypingHarness(t, typingSetup{maxChars: 200})
			call, execute := arrange(h)
			if execute {
				if result := h.approve(call); strings.Contains(result, secret) {
					t.Errorf("tool result carries the payload: %q", result)
				}
			}
			if logged := h.logs.String(); strings.Contains(logged, secret) {
				t.Errorf("the payload reached a log sink:\n%s", logged)
			}
		})
	}
}

// TestAuditRecordsWindowLengthAndApproval: what the audit trail is *for*.
func TestAuditRecordsWindowLengthAndApproval(t *testing.T) {
	t.Run("after a confirmation", func(t *testing.T) {
		h := newTypingHarness(t, typingSetup{})
		h.run(TypeTextToolName, typeArgs("hello there"))
		audit := h.lastAudit(t)
		if audit.Tool != TypeTextToolName || audit.Outcome != "typed" {
			t.Fatalf("audit = %+v", audit)
		}
		if audit.Window != "code — engine.go" || audit.Class != "code" {
			t.Errorf("audit window = %q/%q, want the target named", audit.Window, audit.Class)
		}
		if audit.Chars != len("hello there") {
			t.Errorf("audit chars = %d, want %d", audit.Chars, len("hello there"))
		}
		if !audit.Approved {
			t.Error("a confirmed action must be audited as approved")
		}
	})
	t.Run("when the policy allowed it silently", func(t *testing.T) {
		h := newTypingHarness(t, typingSetup{
			tiers: map[string]PolicyDecision{TypeTextToolName: PolicyAllow},
		})
		h.run(TypeTextToolName, typeArgs("hello there"))
		if audit := h.lastAudit(t); audit.Approved {
			t.Errorf("audit = %+v, want Approved false — nobody was asked", audit)
		}
	})
}

// ------------------------------------------------------ unhappy desktops

// TestWindowThatCannotTakeInput: the inventory reports AcceptsInput for
// exactly this — a window that cannot be typed into is refused rather than
// typed at, which would send the keys somewhere nobody chose.
func TestWindowThatCannotTakeInput(t *testing.T) {
	h := newTypingHarness(t, typingSetup{
		windows: []desktop.Window{{Address: "0x1", Class: "wofi", Title: "overlay",
			Focused: true, StableID: "s1", AcceptsInput: false}},
	})
	_, result := h.run(TypeTextToolName, typeArgs("hello"))
	if got := h.kb.Typed(); len(got) != 0 {
		t.Fatalf("typed = %q, want nothing", got)
	}
	if !strings.Contains(result, "does not accept typed input") {
		t.Errorf("result = %q, want the reason", result)
	}
}

// TestNothingIsFocused: with no focused window there is nowhere for the text to
// go, so there is nothing to ask about either.
func TestNothingIsFocused(t *testing.T) {
	h := newTypingHarness(t, typingSetup{
		windows: []desktop.Window{{Address: "0x1", Class: "code", Title: "engine.go",
			StableID: "s1", AcceptsInput: true}},
	})
	_, result := h.run(TypeTextToolName, typeArgs("hello"))
	if got := h.kb.Typed(); len(got) != 0 {
		t.Fatalf("typed = %q, want nothing", got)
	}
	if !strings.Contains(result, "Nothing was typed") {
		t.Errorf("result = %q, want a refusal", result)
	}
}

// TestKeyboardUnavailable: no wtype, or a compositor that will not grant a
// virtual keyboard, is a sentence — never a failed session.
func TestKeyboardUnavailable(t *testing.T) {
	h := newTypingHarness(t, typingSetup{})
	h.kb.Err = desktop.ErrNoKeyboard
	_, result := h.run(TypeTextToolName, typeArgs("hello"))
	if !strings.Contains(result, "no way to send keystrokes") {
		t.Errorf("result = %q, want the unavailability explained", result)
	}
	if got := h.lastAudit(t).Outcome; got != "unavailable" {
		t.Errorf("audit outcome = %q, want unavailable", got)
	}
}

// TestCompositorUnavailable: with no window manager Jarvix cannot tell where
// the text would land, so it does not send it.
func TestCompositorUnavailable(t *testing.T) {
	h := newTypingHarness(t, typingSetup{})
	h.comp.Err = desktop.ErrNoCompositor
	_, result := h.run(TypeTextToolName, typeArgs("hello"))
	if got := h.kb.Typed(); len(got) != 0 {
		t.Fatalf("typed = %q, want nothing", got)
	}
	if !strings.Contains(result, "Nothing was typed") {
		t.Errorf("result = %q, want a refusal", result)
	}
}

// TestUngatedCallIsRefused: keystrokes only ever happen against a window the
// gate captured. A call that reaches the tool without one — no policy
// installed, or a compositor that would not answer when the question was
// asked — is refused rather than resolved afresh, because resolving now would
// mean typing into whatever has focus on the strength of a question that named
// no window.
func TestUngatedCallIsRefused(t *testing.T) {
	h := newTypingHarness(t, typingSetup{})
	h.registry.SetPolicy(nil)
	call := ai.ToolCall{Name: TypeTextToolName, Arguments: `{"text":"hello"}`}
	if v := h.registry.Check(call); v.Decision != PolicyAllow {
		t.Fatalf("decision = %q, want the no-policy allow", v.Decision)
	}
	result := h.approve(call)
	if got := h.kb.Typed(); len(got) != 0 {
		t.Fatalf("typed = %q, want nothing", got)
	}
	if !strings.Contains(result, "could not tell which window") {
		t.Errorf("result = %q, want the reason", result)
	}
}

// TestApprovalIsSpentOnce: an approval authorises one action. A second
// identical call is a second decision, so the capture is consumed.
func TestApprovalIsSpentOnce(t *testing.T) {
	h := newTypingHarness(t, typingSetup{})
	call, _ := h.gate(TypeTextToolName, typeArgs("hello"))
	if result := h.approve(call); strings.Contains(result, "Nothing was typed") {
		t.Fatalf("the first call should type: %s", result)
	}
	result := h.approve(call)
	if got := h.kb.Typed(); len(got) != 1 {
		t.Fatalf("typed %d times, want 1 — one approval is one action", len(got))
	}
	if !strings.Contains(result, "could not tell which window") {
		t.Errorf("result = %q, want the replay refused", result)
	}
}

// TestApprovalIsKeyedOnThePayload: approving one piece of text may not be
// spent on another.
func TestApprovalIsKeyedOnThePayload(t *testing.T) {
	h := newTypingHarness(t, typingSetup{})
	h.gate(TypeTextToolName, typeArgs("send the report"))
	other := ai.ToolCall{Name: TypeTextToolName, Arguments: `{"text":"transfer the money"}`}
	result := h.approve(other)
	if got := h.kb.Typed(); len(got) != 0 {
		t.Fatalf("typed = %q, want nothing — that text was never approved", got)
	}
	if !strings.Contains(result, "could not tell which window") {
		t.Errorf("result = %q, want the substitution refused", result)
	}
}

// -------------------------------------------------------------- pressing

// TestPressKeyVocabularyIsClosed: there is no spelling of a chord, a modifier,
// or a shell command that this tool accepts.
func TestPressKeyVocabularyIsClosed(t *testing.T) {
	cases := []struct {
		key string
		ok  bool
	}{
		{"enter", true},
		{"tab", true},
		{"escape", true},
		{"backspace", true},
		{"up", true},
		{"ctrl+c", false},
		{"ctrl", false},
		{"f4", false},
		{"super", false},
		{"Return; reboot", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			h := newTypingHarness(t, typingSetup{})
			_, result := h.run(PressKeyToolName, keyArgs(tc.key))
			pressed := h.kb.Pressed()
			if tc.ok {
				if len(pressed) != 1 {
					t.Fatalf("pressed = %q, want one key (%s)", pressed, result)
				}
				return
			}
			if len(pressed) != 0 {
				t.Fatalf("pressed = %q, want nothing", pressed)
			}
			if !strings.Contains(result, "Nothing was typed") {
				t.Errorf("result = %q, want a refusal", result)
			}
		})
	}
}

// TestPressKeySchemaOffersOnlyTheVocabulary: the model is told what exists, so
// a refusal is not something it has to discover by trying.
func TestPressKeySchemaOffersOnlyTheVocabulary(t *testing.T) {
	h := newTypingHarness(t, typingSetup{})
	var schema struct {
		Properties struct {
			Key struct {
				Enum []string `json:"enum"`
			} `json:"key"`
		} `json:"properties"`
	}
	for _, tool := range h.typing.Tools() {
		if tool.Name() != PressKeyToolName {
			continue
		}
		if err := json.Unmarshal(tool.Schema(), &schema); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	if len(schema.Properties.Key.Enum) != len(desktop.KeyNames) {
		t.Fatalf("enum = %v, want every key name and nothing else", schema.Properties.Key.Enum)
	}
	for _, name := range schema.Properties.Key.Enum {
		if _, ok := desktop.Keysym(name); !ok {
			t.Errorf("schema offers %q, which does not resolve", name)
		}
	}
}

// TestPressKeyRefusalIsBounded: the key name is the one string here the model
// chooses freely and that is echoed back into a log line, an audit event and a
// spoken sentence. It is trimmed where it is produced, so no caller has to
// remember to trim it.
func TestPressKeyRefusalIsBounded(t *testing.T) {
	h := newTypingHarness(t, typingSetup{})
	huge := strings.Repeat("k", 5000)
	_, result := h.run(PressKeyToolName, keyArgs(huge))
	if got := h.kb.Pressed(); len(got) != 0 {
		t.Fatalf("pressed = %q, want nothing", got)
	}
	if len(result) > 500 {
		t.Errorf("the refusal is %d characters; a model-supplied key name must not size it", len(result))
	}
	if audit := h.lastAudit(t); len([]rune(audit.Key)) > 40 {
		t.Errorf("the audited key name is %d characters, want it bounded", len([]rune(audit.Key)))
	}
	if logged := h.logs.String(); len(logged) > 4000 {
		t.Errorf("the log line is %d bytes; it must not carry the whole argument", len(logged))
	}
}

// TestPressAuditRecordsTheKeyNotALength: a key press types no characters, so
// the audit says which key rather than reporting a misleading count.
func TestPressAuditRecordsTheKeyNotALength(t *testing.T) {
	h := newTypingHarness(t, typingSetup{})
	h.run(PressKeyToolName, keyArgs("enter"))
	audit := h.lastAudit(t)
	if audit.Outcome != "pressed" || audit.Key != "enter" || audit.Chars != 0 {
		t.Fatalf("audit = %+v, want a pressed enter with no character count", audit)
	}
}
