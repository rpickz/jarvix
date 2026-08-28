package session

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/conversations"
	"github.com/rpickz/jarvix/internal/tools"
)

// scriptShellRounds scripts one shell.run call per model round, "" meaning a
// round with no call (the answer that ends a session). The fake provider's
// round counter runs for the life of the engine, not the session, so a test
// that needs two sessions scripts both up front rather than re-scripting
// between them.
func scriptShellRounds(h *harness, answer string, commands ...string) {
	h.provider.ToolCallsByRound = nil
	for _, command := range commands {
		if command == "" {
			h.provider.ToolCallsByRound = append(h.provider.ToolCallsByRound, nil)
			continue
		}
		args, _ := json.Marshal(map[string]string{"command": command})
		h.provider.ToolCallsByRound = append(h.provider.ToolCallsByRound,
			[]ai.ToolCall{{ID: "c1", Name: "shell.run", Arguments: string(args)}})
	}
	h.provider.Response = answer
}

// The engine half of issue #162: the offer the card is given, the
// conversation-scoped grant, and the record that must stay honest about what
// the user actually answered.

// TestTheCardIsOfferedTheRuleTheGateDerived: the offer rides the question, and
// it is derived from the parsed command by the running policy — not from
// anything the model said.
func TestTheCardIsOfferedTheRuleTheGateDerived(t *testing.T) {
	rec := &namedTool{name: "shell.run", result: "ok"}
	h := newGateHarness(t, Options{}, rec, tools.PolicyConfig{})
	scriptShellCall(h, "zzprobe status --json", "Checked.")

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("check it")
	ev := h.waitFor(t, "tool.confirmation_required")
	if ev.Data["remember_pattern"] != "zzprobe status" {
		t.Fatalf("remember_pattern = %v, want the derived rule", ev.Data["remember_pattern"])
	}
	if ev.Data["remember_segment"] != "zzprobe status --json" {
		t.Errorf("remember_segment = %v", ev.Data["remember_segment"])
	}
	pending, ok := h.engine.PendingConfirmation()
	if !ok || pending.Remember.Pattern != "zzprobe status" {
		t.Errorf("snapshot offer = %+v, want the same rule the event carried", pending.Remember)
	}
	_ = h.engine.Confirm(false)
	h.countUntil(t, "session.finished")
	h.waitIdle(t)
}

// A refused shape carries its sentence instead, on both surfaces.
func TestARefusedShapeCarriesItsSentence(t *testing.T) {
	rec := &namedTool{name: "shell.run", result: "ok"}
	h := newGateHarness(t, Options{}, rec, tools.PolicyConfig{})
	scriptShellCall(h, "rm -rf ./build", "Not done.")

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("tidy")
	ev := h.waitFor(t, "tool.confirmation_required")
	if _, offered := ev.Data["remember_pattern"]; offered {
		t.Fatalf("a destructive command was offered a rule: %v", ev.Data)
	}
	reason, _ := ev.Data["remember_reason"].(string)
	if !strings.Contains(reason, `"rm" always asks`) {
		t.Errorf("remember_reason = %q", reason)
	}
	_ = h.engine.Confirm(false)
	h.countUntil(t, "session.finished")
	h.waitIdle(t)
}

// A conversation-scoped grant silences the next matching command, audits it,
// and dies with the conversation.
func TestAConversationGrantLastsExactlyOneConversation(t *testing.T) {
	rec := &namedTool{name: "shell.run", result: "ok"}
	h := newGateHarness(t, Options{}, rec, tools.PolicyConfig{})
	scriptShellRounds(h, "Checked.", "zzprobe status --json", "", "zzprobe status --quiet")

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("check it")
	h.waitFor(t, "tool.confirmation_required")
	if err := h.engine.ConfirmRemembering(true, tools.RememberConversation); err != nil {
		t.Fatal(err)
	}
	h.countUntil(t, "session.finished")
	h.waitIdle(t)

	if got := h.engine.ConversationGrants(); len(got) != 1 || got[0] != "zzprobe status" {
		t.Fatalf("grants = %v, want the derived rule", got)
	}

	// The next matching command runs unprompted, and says so on the bus.
	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("again")
	pre := h.waitFor(t, "tool.pre_approved")
	if pre.Data["scope"] != string(tools.RememberConversation) {
		t.Errorf("scope = %v, want %q", pre.Data["scope"], tools.RememberConversation)
	}
	if pre.Data["pattern"] != "zzprobe status" {
		t.Errorf("pattern = %v", pre.Data["pattern"])
	}
	h.countUntil(t, "session.finished")
	h.waitIdle(t)

	// And the conversation ending takes it: "just this conversation" means
	// this one.
	h.engine.NewConversation()
	if got := h.engine.ConversationGrants(); len(got) != 0 {
		t.Errorf("grants survived the conversation: %v", got)
	}
}

// A conversation grant is revocable mid-conversation: a user who changes
// their mind must not have to end the conversation to act on it.
func TestAConversationGrantIsRevocable(t *testing.T) {
	rec := &namedTool{name: "shell.run", result: "ok"}
	h := newGateHarness(t, Options{}, rec, tools.PolicyConfig{})
	scriptShellRounds(h, "Checked.", "zzprobe status --json", "", "zzprobe status --quiet")

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("check it")
	h.waitFor(t, "tool.confirmation_required")
	if err := h.engine.ConfirmRemembering(true, tools.RememberConversation); err != nil {
		t.Fatal(err)
	}
	h.countUntil(t, "session.finished")
	h.waitIdle(t)

	if !h.engine.RevokeConversationGrant("zzprobe status") {
		t.Fatal("the grant could not be revoked")
	}
	if h.engine.RevokeConversationGrant("zzprobe status") {
		t.Error("revoking twice reported a second removal")
	}

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("again")
	h.waitFor(t, "tool.confirmation_required")
	_ = h.engine.Confirm(false)
	h.countUntil(t, "session.finished")
	h.waitIdle(t)
}

// The record is honest: approved AND the rule that answer added (#128's
// role:"confirmation" turn, told the whole truth).
func TestTheRecordSaysARuleWasAdded(t *testing.T) {
	rec := &namedTool{name: "shell.run", result: "ok"}
	h := newGateHarness(t, Options{HistoryTurns: 8}, rec, tools.PolicyConfig{})
	scriptShellCall(h, "zzprobe status --json", "Checked.")

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("check it")
	h.waitFor(t, "tool.confirmation_required")
	if err := h.engine.ConfirmRemembering(true, tools.RememberConversation); err != nil {
		t.Fatal(err)
	}
	h.countUntil(t, "session.finished")
	h.waitIdle(t)

	var found *conversations.Confirmation
	for _, turn := range h.engine.Conversation() {
		if turn.Role == conversations.RoleConfirmation && turn.Confirmation != nil {
			found = turn.Confirmation
		}
	}
	if found == nil {
		t.Fatal("no confirmation record in the conversation")
	}
	if found.Outcome != conversations.ConfirmationApproved {
		t.Errorf("outcome = %q, want approved", found.Outcome)
	}
	if found.Remembered != "zzprobe status" {
		t.Errorf("remembered = %q, want the rule the answer added", found.Remembered)
	}
	if found.RememberScope != string(tools.RememberConversation) {
		t.Errorf("scope = %q", found.RememberScope)
	}
}

// …and an ordinary approve-once records nothing extra, so every existing
// record's shape is unchanged.
func TestAnApproveOnceRecordsNoRule(t *testing.T) {
	rec := &namedTool{name: "shell.run", result: "ok"}
	h := newGateHarness(t, Options{HistoryTurns: 8}, rec, tools.PolicyConfig{})
	scriptShellCall(h, "zzprobe status --json", "Checked.")

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("check it")
	h.waitFor(t, "tool.confirmation_required")
	_ = h.engine.Confirm(true)
	h.countUntil(t, "session.finished")
	h.waitIdle(t)

	for _, turn := range h.engine.Conversation() {
		if turn.Confirmation == nil {
			continue
		}
		if turn.Confirmation.Remembered != "" || turn.Confirmation.RememberScope != "" {
			t.Errorf("an approve-once recorded a rule: %+v", turn.Confirmation)
		}
	}
	if len(h.engine.ConversationGrants()) != 0 {
		t.Error("an approve-once left a standing grant")
	}
}

// Declining with a scope is refused: shell_allow has no negative form, and a
// decline that quietly wrote a rule would be a permission change nobody asked
// for.
func TestDecliningWithAScopeIsRefused(t *testing.T) {
	rec := &namedTool{name: "shell.run", result: "ok"}
	h := newGateHarness(t, Options{}, rec, tools.PolicyConfig{})
	scriptShellCall(h, "zzprobe status --json", "Not checked.")

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("check it")
	h.waitFor(t, "tool.confirmation_required")
	if err := h.engine.ConfirmRemembering(false, tools.RememberAlways); err == nil {
		t.Fatal("a decline was allowed to add a rule")
	}
	// The question is still standing, which is the honest failure.
	if err := h.engine.Confirm(false); err != nil {
		t.Fatalf("the confirmation did not survive: %v", err)
	}
	h.countUntil(t, "session.finished")
	h.waitIdle(t)
}

// Remembering a shape the gate refused is rejected at the resolution too, not
// only at the offer: a client that never rendered the card must not be able
// to add a rule this daemon declined to propose.
func TestRememberingARefusedShapeIsRejectedAtResolution(t *testing.T) {
	rec := &namedTool{name: "shell.run", result: "ok"}
	h := newGateHarness(t, Options{}, rec, tools.PolicyConfig{})
	scriptShellCall(h, "rm -rf ./build", "Not done.")

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("tidy")
	h.waitFor(t, "tool.confirmation_required")
	err := h.engine.ConfirmRemembering(true, tools.RememberConversation)
	if err == nil || !strings.Contains(err.Error(), "cannot be remembered") {
		t.Fatalf("error = %v, want a refusal", err)
	}
	if len(h.engine.ConversationGrants()) != 0 {
		t.Error("a refused shape became a grant anyway")
	}
	_ = h.engine.Confirm(false)
	h.countUntil(t, "session.finished")
	h.waitIdle(t)
}

// remember_for_conversation's own fast path is audited too. It re-ran an
// approved command with nothing on the bus to say so, which is the same
// promise #162 makes about rules — broken before a single rule existed.
func TestARememberedApprovalIsAudited(t *testing.T) {
	rec := &namedTool{name: "shell.run", result: "ok"}
	h := newGateHarness(t, Options{RememberApprovals: true}, rec, tools.PolicyConfig{})
	scriptShellRounds(h, "Checked.", "zzprobe status --json", "", "zzprobe status --json")

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("check it")
	h.waitFor(t, "tool.confirmation_required")
	_ = h.engine.Confirm(true)
	h.countUntil(t, "session.finished")
	h.waitIdle(t)

	_, _ = h.engine.StartSession()
	_ = h.engine.Submit("again")
	pre := h.waitFor(t, "tool.pre_approved")
	if rule, _ := pre.Data["rule"].(string); !strings.Contains(rule, "you approved") {
		t.Errorf("rule = %q, want it to say why no question was asked", rule)
	}
	h.countUntil(t, "session.finished")
	h.waitIdle(t)
}

// The spoken listing refuses honestly when the seam is absent. A listing that
// says "nothing" because it cannot see is the worst possible answer to a
// question about permissions.
func TestTheSpokenListingRefusesHonestlyWithoutASeam(t *testing.T) {
	h := newHarness(t, Options{})
	_, err := h.engine.runApprovalsList()
	if err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("error = %v, want an honest refusal", err)
	}
}
