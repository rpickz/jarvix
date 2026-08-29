package tools

import (
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/desktop"
)

// The gate-equivalence tests (#197, ADR 0062): typing into a terminal IS
// running a command, so it faces exactly what `shell.run` faces.
//
// This file exists because of one specific way the managed-window feature
// could have gone wrong. Handing a terminal over gives Jarvix a live shell;
// if typing into it were merely "typing", acquisition would be a complete
// bypass of ADR 0014 — the same power with none of the review. Every test
// below is an assertion that there is nothing on the other side of
// acquisition to unlock.

// aTerminalDesktop is one focused terminal and nothing else, so "the focused
// window" and "the terminal" are the same window in every test here.
func aTerminalDesktop() []desktop.Window {
	return []desktop.Window{{Address: "0x1", Class: "Alacritty", Title: "zsh",
		Workspace: 1, WorkspaceName: "1", Focused: true, StableID: "s1", AcceptsInput: true}}
}

// typingAllowed is the strictest test of every refusal below: the user has
// explicitly named the tool and allowed it, which is the loudest "yes" the
// configuration can express.
var typingAllowed = map[string]PolicyDecision{TypeTextToolName: PolicyAllow}

// A command the gate denies cannot be typed into a shell, even when typing
// itself is explicitly allowed. Without this, switching typing on would be a
// way to run `rm -rf /` by spelling it.
func TestADeniedCommandCannotBeTypedIntoATerminal(t *testing.T) {
	h := newTypingHarness(t, typingSetup{windows: aTerminalDesktop(), tiers: typingAllowed})
	_, verdict := h.gate(TypeTextToolName, typeArgs("rm -rf /"))
	if verdict.Decision != PolicyDeny {
		t.Fatalf("decision = %q, want deny — an explicit typing allow must not outrank a deny rule", verdict.Decision)
	}
	if !strings.Contains(verdict.Rule, "rm targeting /") {
		t.Errorf("rule = %q, want the deny rule that refused it", verdict.Rule)
	}
	if !strings.Contains(verdict.Rule, "would run it") {
		t.Errorf("rule = %q, want it to say why typing it is running it", verdict.Rule)
	}
}

// A configured deny pattern reaches the keyboard too: `[tools.policy]
// shell_deny` is a statement about commands, not about which tool produced
// them.
func TestAConfiguredDenyPatternReachesTypedCommands(t *testing.T) {
	h := newTypingHarness(t, typingSetup{windows: aTerminalDesktop(), tiers: typingAllowed,
		shellDeny: []string{"terraform apply"}})
	_, verdict := h.gate(TypeTextToolName, typeArgs("terraform apply -auto-approve"))
	if verdict.Decision != PolicyDeny {
		t.Fatalf("decision = %q, want deny", verdict.Decision)
	}
}

// A risky command asks, with the classifier's own rule and reason, however
// typing is configured. The card is command-shaped and shows the command
// verbatim — a model cannot describe `sudo rm -rf ~` as tidying up when the
// sentence the user hears is not written by the model at all.
func TestARiskyTypedCommandAsksWithTheShellClassifiersReason(t *testing.T) {
	h := newTypingHarness(t, typingSetup{windows: aTerminalDesktop(), tiers: typingAllowed})
	_, verdict := h.gate(TypeTextToolName, typeArgs("sudo systemctl restart nginx"))
	if verdict.Decision != PolicyAsk {
		t.Fatalf("decision = %q, want ask", verdict.Decision)
	}
	if !strings.Contains(verdict.Rule, `risky command "sudo"`) {
		t.Errorf("rule = %q, want the shell classifier's own rule", verdict.Rule)
	}
	if !strings.Contains(verdict.Summary, "sudo systemctl restart nginx") {
		t.Errorf("summary = %q, want the command verbatim", verdict.Summary)
	}
	if !strings.Contains(verdict.Summary, `uses the risky command "sudo"`) {
		t.Errorf("summary = %q, want the classifier's reason in the question", verdict.Summary)
	}
	if !strings.Contains(verdict.Summary, "would run as a command") {
		t.Errorf("summary = %q, want it to say typing it runs it", verdict.Summary)
	}
	if !strings.Contains(verdict.Command, "sudo systemctl restart nginx") {
		t.Errorf("command = %q, want the published action to carry the command verbatim", verdict.Command)
	}
}

// The compound-command splitter applies: a line is judged by its riskiest
// part, exactly as `shell.run` judges it.
func TestACompoundTypedCommandIsJudgedByItsRiskiestPart(t *testing.T) {
	h := newTypingHarness(t, typingSetup{windows: aTerminalDesktop(), tiers: typingAllowed})
	_, verdict := h.gate(TypeTextToolName, typeArgs("ls -la && rm -rf ~/work"))
	if verdict.Decision != PolicyAsk {
		t.Fatalf("decision = %q, want ask — the rm must not hide behind the ls", verdict.Decision)
	}
	if !strings.Contains(verdict.Rule, `risky command "rm"`) {
		t.Errorf("rule = %q, want the riskiest segment to decide", verdict.Rule)
	}
}

// The equivalence runs both ways: a standing approval (#162) that covers a
// command covers it when it is typed, or the feature would be asking about
// something the user has already said yes to for good.
func TestAStandingApprovalCoversATypedCommand(t *testing.T) {
	h := newTypingHarness(t, typingSetup{windows: aTerminalDesktop(), tiers: typingAllowed,
		shellAllow: []string{"docker ps"}})
	_, verdict := h.gate(TypeTextToolName, typeArgs("docker ps -a"))
	if verdict.Decision != PolicyAllow {
		t.Fatalf("decision = %q, want allow — a standing approval covers this command (rule %q)",
			verdict.Decision, verdict.Rule)
	}
}

// …but only as far as the typing tier allows. The shell verdict is a floor,
// never a ceiling: with typing left at its shipped tier, every payload still
// asks, and a `shell_allow` cannot talk the keyboard's own gate down.
func TestTheTypingTierIsAFloorOverTheShellVerdict(t *testing.T) {
	h := newTypingHarness(t, typingSetup{windows: aTerminalDesktop(),
		shellAllow: []string{"docker ps"}})
	_, verdict := h.gate(TypeTextToolName, typeArgs("docker ps -a"))
	if verdict.Decision != PolicyAsk {
		t.Fatalf("decision = %q, want ask — typing asks by default whatever the shell list says",
			verdict.Decision)
	}
}

// THE no-bypass proof. The same command, into the same terminal, judged once
// while the window is the user's and once after they have handed it over: the
// gate must reach the identical verdict. Acquisition grants access to the
// window; it grants no permission to run anything, so there is nothing about
// it for the classification to notice.
func TestManagementChangesNothingAboutTheGate(t *testing.T) {
	commands := []string{"rm -rf /", "sudo systemctl restart nginx", "ls -la && rm -rf ~/work", "git status"}

	unmanaged := newTypingHarness(t, typingSetup{windows: aTerminalDesktop(), tiers: typingAllowed})
	handedOver := newTypingHarness(t, typingSetup{windows: aTerminalDesktop(), tiers: typingAllowed,
		managed: true})
	term := aTerminalDesktop()[0]
	if _, fresh, err := handedOver.store.Acquire(term, aTerminalDesktop()); err != nil || !fresh {
		t.Fatalf("Acquire: fresh=%v err=%v", fresh, err)
	}
	// Observed, not assumed: the second harness really is managing the window
	// the commands are aimed at.
	if _, ok := handedOver.store.Managed(term, aTerminalDesktop()); !ok {
		t.Fatal("the handed-over harness should be managing the terminal")
	}

	for _, command := range commands {
		_, before := unmanaged.gate(TypeTextToolName, typeArgs(command))
		_, after := handedOver.gate(TypeTextToolName, typeArgs(command))
		if before.Decision != after.Decision {
			t.Errorf("%q: unmanaged decision %q, managed decision %q — acquiring a window must not "+
				"change what the gate says about a command", command, before.Decision, after.Decision)
		}
		if before.Rule != after.Rule {
			t.Errorf("%q: unmanaged rule %q, managed rule %q", command, before.Rule, after.Rule)
		}
		if before.Summary != after.Summary {
			t.Errorf("%q: the confirmation differs once the window is managed:\n  %q\n  %q",
				command, before.Summary, after.Summary)
		}
	}
}

// The Execute re-check, exercised where it is actually reachable: the gate
// asked about a command, and a deny rule arrived — a config reload, a `jarvix
// approvals deny` — while the user was answering. Nothing is typed.
func TestADenyRuleArrivingWhileTheUserAnsweredStillRefuses(t *testing.T) {
	h := newTypingHarness(t, typingSetup{windows: aTerminalDesktop(), tiers: typingAllowed})
	call, verdict := h.gate(TypeTextToolName, typeArgs("terraform apply"))
	if verdict.Decision != PolicyAsk {
		t.Fatalf("decision = %q, want ask before the rule arrives", verdict.Decision)
	}

	tightened, err := NewPolicy(PolicyConfig{Default: PolicyAsk, Tools: typingAllowed,
		ShellDeny: []string{"terraform apply"}})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	h.registry.SetPolicy(tightened)

	result := h.approve(call)
	if got := h.kb.Typed(); len(got) != 0 {
		t.Fatalf("typed = %q, want nothing", got)
	}
	if !strings.Contains(result, "Nothing was typed") {
		t.Errorf("result = %q, want a refusal", result)
	}
	if audit := h.lastAudit(t); audit.Outcome != "refused" || audit.Command != "terraform apply" {
		t.Errorf("audit = %+v, want a refused row naming the command", audit)
	}
}

// A typed command appears in the audit as a command: verbatim, with the rule
// that judged it. A standing grant removes the question, so it must not also
// remove the evidence.
func TestATypedCommandIsAuditedAsACommand(t *testing.T) {
	h := newTypingHarness(t, typingSetup{windows: aTerminalDesktop(), tiers: typingAllowed,
		shellAllow: []string{"docker ps"}})
	h.run(TypeTextToolName, typeArgs("docker ps -a"))
	audit := h.lastAudit(t)
	if audit.Outcome != "typed" {
		t.Fatalf("outcome = %q, want typed", audit.Outcome)
	}
	if audit.Command != "docker ps -a" {
		t.Errorf("audit command = %q, want the command verbatim", audit.Command)
	}
	if !strings.Contains(audit.Rule, "docker ps") {
		t.Errorf("audit rule = %q, want the rule that allowed it named", audit.Rule)
	}
}

// The journal is untouched by the above: the log line still records how much
// was typed and never what. The one new place the text travels is the audit
// event, which the activity feed renders as a command row.
func TestATypedCommandStaysOutOfTheJournal(t *testing.T) {
	const command = "docker ps --filter name=hunter2"
	h := newTypingHarness(t, typingSetup{windows: aTerminalDesktop(), tiers: typingAllowed,
		shellAllow: []string{"docker ps"}})
	h.run(TypeTextToolName, typeArgs(command))
	if logs := h.logs.String(); strings.Contains(logs, "hunter2") {
		t.Errorf("the command reached a log sink:\n%s", logs)
	}
	if audit := h.lastAudit(t); audit.Command != command {
		t.Fatalf("audit command = %q, want the command — the feed is where it belongs", audit.Command)
	}
}

// Typing into anything that is not a terminal is untouched: no
// classification, no command row, and the same character-count wording the
// audit has always carried.
func TestTypingIntoANonTerminalIsUnchanged(t *testing.T) {
	h := newTypingHarness(t, typingSetup{
		windows: []desktop.Window{{Address: "0x1", Class: "code", Title: "engine.go",
			Focused: true, StableID: "s1", AcceptsInput: true}},
		tiers: typingAllowed,
	})
	_, verdict := h.gate(TypeTextToolName, typeArgs("rm -rf /"))
	if verdict.Decision != PolicyAllow {
		t.Fatalf("decision = %q, want allow — an editor is not a shell", verdict.Decision)
	}
	h.run(TypeTextToolName, typeArgs("rm -rf /"))
	if audit := h.lastAudit(t); audit.Command != "" || audit.Rule != "" {
		t.Errorf("audit = %+v, want no command classification for a non-terminal", audit)
	}
}

// Pressing a key types no characters, so there is no command to classify —
// pressing enter is confirmed on its own terms, as it always was.
func TestPressingAKeyIsNotClassifiedAsACommand(t *testing.T) {
	h := newTypingHarness(t, typingSetup{windows: aTerminalDesktop(), tiers: typingAllowed})
	_, verdict := h.gate(PressKeyToolName, keyArgs("enter"))
	if verdict.Decision != PolicyAsk {
		t.Fatalf("decision = %q, want ask — submitting has its own tier", verdict.Decision)
	}
	h.run(PressKeyToolName, keyArgs("enter"))
	if audit := h.lastAudit(t); audit.Command != "" {
		t.Errorf("audit = %+v, want no command on a key press", audit)
	}
}

// A payload the ordinary checks already refuse is not classified twice: the
// refusal that arrives first is the one the user hears.
func TestAnUnusablePayloadIsNotClassified(t *testing.T) {
	h := newTypingHarness(t, typingSetup{windows: aTerminalDesktop(), tiers: typingAllowed})
	_, verdict := h.gate(TypeTextToolName, typeArgs("rm -rf /\nyes"))
	if verdict.Decision == PolicyDeny {
		t.Fatalf("decision = %q, want the payload check to refuse it rather than the gate", verdict.Decision)
	}
	_, result := h.run(TypeTextToolName, typeArgs("rm -rf /\nyes"))
	if !strings.Contains(result, "line breaks") {
		t.Errorf("result = %q, want the control-character refusal", result)
	}
	if got := h.kb.Typed(); len(got) != 0 {
		t.Fatalf("typed = %q, want nothing", got)
	}
}

// A window Jarvix opened inside a terminal wears an identity of its own
// (#198), so a class-list match would miss it — and `ghostty -e bash` is a
// window that is literally a shell. The whole `dev.jarvix.` namespace counts,
// through the one definition both the gate and the managed-window surface
// ask.
func TestAWindowJarvixOpenedInATerminalIsATerminal(t *testing.T) {
	opened := []desktop.Window{{Address: "0x1", Class: "dev.jarvix.bash", Title: "bash",
		Workspace: 1, WorkspaceName: "1", Focused: true, StableID: "s1", AcceptsInput: true}}
	h := newTypingHarness(t, typingSetup{windows: opened, tiers: typingAllowed})
	_, verdict := h.gate(TypeTextToolName, typeArgs("rm -rf /"))
	if verdict.Decision != PolicyDeny {
		t.Fatalf("decision = %q, want deny — the window Jarvix opened is a shell", verdict.Decision)
	}
	if !h.windows.isManagedTerminal(opened[0]) {
		t.Error("the managed-window surface does not agree that this is a terminal; " +
			"the two must share one definition")
	}
}
