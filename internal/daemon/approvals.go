package daemon

// This file is the daemon half of remembered command approvals (issue #162,
// ADR 0052): the writer that appends one word-prefix rule to
// `[tools.policy] shell_allow`, the ledger that remembers when it was added
// and how often it has fired, the two IPC verbs that list and revoke, and the
// spoken listing behind "what have I pre-approved?".
//
// Three properties are load-bearing and none of them are conventions:
//
//  1. THE ASSISTANT CANNOT REACH THIS. Every write here goes through
//     config.RewriteTOML directly with a key the settings registry does not
//     contain — `tools.policy.shell_allow` is structurally absent from
//     config.Settings() and explicitly excluded by
//     AssistantExcludedSettingReason (#109, ADR 0036) — and the only callers
//     are a human answering a card and a human at the CLI. The assistant's
//     config.write_setting tool resolves keys against AssistantSettings(),
//     which never held this key and still does not. Adding a writer here did
//     not put a door in that wall; it built a second door, on the human side
//     of it, with its own lock.
//  2. THE PATTERN IS NEVER SUPPLIED BY A CLIENT ON THE CARD PATH. session.confirm
//     takes a scope word, and the daemon asks the engine what it published on
//     the card. A pattern arriving over IPC would be a channel the model could
//     reach through any client it could persuade — and since #147 the model
//     reads window content and AI-session transcripts, so "persuade" is not
//     hypothetical.
//  3. THE FILE IS THE TRUTH. The write goes through the same surgical,
//     fingerprint-guarded, whole-document-validated editor the settings screen
//     uses: hand edits are never clobbered, comments survive, and a change
//     that would not validate is not written at all. The running policy is
//     then recompiled from the same configuration, so what the classifier
//     believes and what the file says can never drift.

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rpickz/jarvix/internal/approvals"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/session"
	"github.com/rpickz/jarvix/internal/tools"
)

// approvalStore owns the running allow list and the ledger beside it.
//
// It exists as its own object, rather than as two fields on the Daemon, for
// one structural reason: the session engine is constructed before the Daemon
// is, and the engine needs a read-only view of this to answer "what have I
// pre-approved?" out loud. Building the holder first and handing it to both
// means the voice path and the write path share one list by construction —
// there is no second copy to fall out of step, and no closure repointed after
// the fact for a data race to live in.
//
// It is the running gate's list, not the file's. They are the same list
// except for the instant between a hand edit and the reload that picks it up,
// and reporting the running one is the honest choice: the question is "what
// runs without asking", which the running gate answers and the file only
// predicts.
type approvalStore struct {
	mu       sync.Mutex
	patterns []string
	ledger   *approvals.Store
}

// newApprovalStore builds the holder from the booted configuration.
func newApprovalStore(patterns []string, path string, logger *slog.Logger) *approvalStore {
	return &approvalStore{
		patterns: append([]string(nil), patterns...),
		ledger:   approvals.NewStore(path, logger),
	}
}

// Patterns is the running allow list.
func (s *approvalStore) Patterns() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.patterns...)
}

// setPatterns replaces the running allow list. Called only after the file has
// been written (or re-read) and the gate recompiled, so this and the
// classifier always describe the same permissions.
func (s *approvalStore) setPatterns(patterns []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.patterns = append([]string(nil), patterns...)
}

// List folds the ledger onto the running list.
func (s *approvalStore) List() []approvals.Entry { return s.ledger.List(s.Patterns()) }

// shellAllowKey is the one configuration key this file writes. Named once so
// the writer, the reload and the tests cannot disagree about it.
const shellAllowKey = "tools.policy.shell_allow"

// Approval-change sources, mirroring the setting-change sources in
// settings.go: a card answered in the window or the overlay, and the CLI.
// Both are people; the label says which surface, so the activity feed can
// report where a rule came from.
const (
	approvalSourceCard = "card"
	approvalSourceCLI  = "cli"
)

func (d *Daemon) registerApprovalMethods() {
	d.server.Handle("approvals.list", d.handleApprovalsList)
	d.server.Handle("approvals.forget", d.handleApprovalsForget)
}

// handleApprovalsList reports every standing grant: the permanent patterns
// from `[tools.policy] shell_allow` with their ledger history, and the
// conversation-scoped grants that live only in the engine.
//
// Both in one answer because they are one question. A user asking what runs
// without being asked does not care which of two mechanisms is responsible,
// and splitting the answer would let a conversation grant hide behind the
// fact that it is not in the file.
func (d *Daemon) handleApprovalsList(json.RawMessage) (any, error) {
	entries := d.approvals.List()
	rows := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		row := map[string]any{
			"pattern": e.Pattern,
			"source":  e.Source,
			"scope":   string(tools.RememberAlways),
			"uses":    e.Uses,
		}
		// Absent rather than zero: a hand-added pattern has no date, and
		// 0001-01-01 on a card would be a lie the window then has to detect.
		if !e.Added.IsZero() {
			row["added"] = e.Added.UTC().Format(time.RFC3339)
		}
		if !e.LastUsed.IsZero() {
			row["last_used"] = e.LastUsed.UTC().Format(time.RFC3339)
		}
		rows = append(rows, row)
	}
	for _, pattern := range d.engine.ConversationGrants() {
		rows = append(rows, map[string]any{
			"pattern": pattern,
			"source":  approvals.SourceCard,
			"scope":   string(tools.RememberConversation),
			"uses":    0,
		})
	}
	return map[string]any{
		"path":     d.paths.ConfigFile(),
		"approved": rows,
	}, nil
}

// handleApprovalsForget revokes one permanent pattern.
//
// Revocation is deliberately easier than granting: no fingerprint is required
// from the caller, no confirmation is asked, and an unknown pattern is a
// plain "nothing matched" rather than an error. Tightening the gate is never
// the thing to make hard — the asymmetry is the same one the policy tiers
// carry, where a stricter default always wins.
func (d *Daemon) handleApprovalsForget(params json.RawMessage) (any, error) {
	var p struct {
		Pattern string `json:"pattern"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "approvals.forget params: %v", err)
		}
	}
	pattern := normalisePattern(p.Pattern)
	if pattern == "" {
		return nil, ipc.Errorf(ipc.CodeInvalidParams,
			"approvals.forget needs a pattern, e.g. {\"pattern\": \"docker ps\"}")
	}
	// The conversation-scoped grants first: they are not in the file, so a
	// config write would never find them, and "forget docker ps" must forget
	// the one the user can see whichever kind it is.
	if d.engine.RevokeConversationGrant(pattern) {
		d.bus.Publish(session.Event{Type: "approvals.changed", Data: map[string]any{
			"action": "forgotten", "pattern": pattern,
			"scope": string(tools.RememberConversation), "source": approvalSourceCLI,
		}})
		return map[string]any{"forgotten": true, "pattern": pattern,
			"scope": string(tools.RememberConversation)}, nil
	}
	current := d.shellAllowPatterns()
	kept := make([]string, 0, len(current))
	found := false
	for _, existing := range current {
		if normalisePattern(existing) == pattern {
			found = true
			continue
		}
		kept = append(kept, existing)
	}
	if !found {
		return map[string]any{"forgotten": false, "pattern": pattern,
			"approved": current}, nil
	}
	if err := d.writeShellAllow(kept, ""); err != nil {
		return nil, err
	}
	d.approvals.ledger.Forget(pattern)
	d.bus.Publish(session.Event{Type: "approvals.changed", Data: map[string]any{
		"action": "forgotten", "pattern": pattern,
		"scope": string(tools.RememberAlways), "source": approvalSourceCLI,
	}})
	return map[string]any{"forgotten": true, "pattern": pattern,
		"scope": string(tools.RememberAlways)}, nil
}

// rememberPattern appends one pattern to `[tools.policy] shell_allow` and
// recompiles the running gate. It is the only path that widens the gate on a
// user's behalf, and it is called from exactly one place: session.confirm
// with scope "always".
//
// It runs BEFORE the confirmation resolves. A write that fails therefore
// leaves the question standing and the command unrun, which is the honest
// failure: the user asked for "approve and don't ask again", and half of that
// is not what they asked for.
func (d *Daemon) rememberPattern(pattern, source string) error {
	pattern = normalisePattern(pattern)
	if pattern == "" {
		return ipc.Errorf(ipc.CodeInvalidParams, "there is no pattern to remember")
	}
	current := d.shellAllowPatterns()
	for _, existing := range current {
		if normalisePattern(existing) == pattern {
			// Already granted — the classifier would have allowed the command
			// and no card would have been shown, so this is a duplicate
			// answer to a question that resolved some other way. Idempotent
			// rather than an error: nothing to write, nothing wrong.
			return nil
		}
	}
	if err := d.writeShellAllow(append(append([]string(nil), current...), pattern), ""); err != nil {
		return err
	}
	d.approvals.ledger.Added(pattern, time.Now())
	d.log.Info("approval remembered", "component", "tools",
		"pattern", pattern, "scope", string(tools.RememberAlways), "source", source)
	d.bus.Publish(session.Event{Type: "approvals.changed", Data: map[string]any{
		"action": "added", "pattern": pattern,
		"scope": string(tools.RememberAlways), "source": source,
	}})
	return nil
}

// writeShellAllow replaces the whole `shell_allow` list in config.toml and
// recompiles the running policy from the result.
//
// fingerprint, when non-empty, is the caller's view of the file and a
// mismatch fails the write — the settings surface's external-edit guard,
// applied here for the same reason: a rule appended on top of a file someone
// has since hand-edited would silently discard their edit, and the one file
// where that must never happen is the one that says what may run.
func (d *Daemon) writeShellAllow(patterns []string, fingerprint string) error {
	path := d.paths.ConfigFile()
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return ipc.Errorf(ipc.CodeInternalError, "read config: %v", err)
	}
	fp := config.FingerprintMissing
	if raw != nil {
		fp = config.Fingerprint(raw)
	}
	if fingerprint != "" && fingerprint != fp {
		return &ipc.Error{
			Code: ipc.CodeConfigConflict,
			Message: "config.toml changed on disk since it was read; " +
				"reload and answer the question again",
			Data: map[string]any{"fingerprint": fp},
		}
	}
	// Rebase onto the file, not the running config: whatever else the user
	// hand-edited stays, exactly as config.set does it.
	fileCfg, err := config.ParseBytes(raw)
	if err != nil {
		return &ipc.Error{
			Code:    ipc.CodeConfigInvalid,
			Message: fmt.Sprintf("config.toml does not parse; fix it by hand first: %v", err),
		}
	}
	fileCfg.Tools.Policy.ShellAllow = patterns
	fileCfg.Voices = fileCfg.InstalledVoices(d.paths)
	if err := fileCfg.Validate(); err != nil {
		return &ipc.Error{
			Code:    ipc.CodeConfigInvalid,
			Message: "the rule was rejected by validation; nothing was written",
			Data:    map[string]any{"problems": validationProblems(err)},
		}
	}
	// The compile happens before the write, not after: a pattern the
	// classifier will not accept must not reach the file, or the next daemon
	// start would fail on a rule the user cannot remember agreeing to.
	policy, err := d.compilePolicy(fileCfg)
	if err != nil {
		return &ipc.Error{
			Code:    ipc.CodeConfigInvalid,
			Message: fmt.Sprintf("the rule was rejected by the permission gate; nothing was written: %v", err),
		}
	}
	// RewriteOffRegistryKey, not RewriteTOML: `shell_allow` is not a settings
	// registry key and must never become one, because the registry is the
	// surface the assistant's config tools resolve names against (#109). Same
	// surgical edit, same parse-and-read-back guard, no widening of the
	// registry to get it.
	newRaw, err := config.RewriteOffRegistryKey(raw, shellAllowKey, patterns,
		func(c config.Config) any { return c.Tools.Policy.ShellAllow })
	if err != nil {
		return ipc.Errorf(ipc.CodeInternalError, "rewrite config: %v", err)
	}
	if err := config.WriteFileAtomic(path, newRaw); err != nil {
		return ipc.Errorf(ipc.CodeInternalError, "write config: %v", err)
	}
	// Only now does the running gate move. The order matters: a policy
	// installed before a failed write would be a permission the file does not
	// grant, which is the one direction of drift nobody could audit.
	d.registry.SetPolicy(policy)
	d.approvals.setPatterns(patterns)
	d.cfgMu.Lock()
	d.cfg.Tools.Policy.ShellAllow = patterns
	d.cfgMu.Unlock()
	d.publishConfigChanged(config.Fingerprint(newRaw))
	return nil
}

// compilePolicy builds the permission gate for a configuration — the same
// construction daemon start performs, factored out so the boot path, a
// reload, and a remembered rule all produce a gate the identical way.
func (d *Daemon) compilePolicy(cfg config.Config) (*tools.Policy, error) {
	perTool := make(map[string]tools.PolicyDecision, len(cfg.Tools.Policy.Tool))
	for name, decision := range cfg.Tools.Policy.Tool {
		perTool[name] = tools.PolicyDecision(decision)
	}
	return tools.NewPolicy(tools.PolicyConfig{
		Default:    tools.PolicyDecision(cfg.Tools.Policy.Default),
		Tools:      perTool,
		ShellAllow: cfg.Tools.Policy.ShellAllow,
		ShellDeny:  cfg.Tools.Policy.ShellDeny,
		Advisors:   advisorPolicyTiers(cfg),
	})
}

// shellAllowPatterns is the running gate's configured allow list.
func (d *Daemon) shellAllowPatterns() []string { return d.approvals.Patterns() }

// normalisePattern collapses whitespace so `docker  ps` and `docker ps` are
// one rule — the same collapsing the classifier performs when it compiles a
// pattern, so a revocation always names what the gate is actually holding.
func normalisePattern(pattern string) string {
	return strings.Join(strings.Fields(pattern), " ")
}

// recordApprovalUse bumps the ledger when a remembered rule lets a command
// run. Driven from the same bus subscription that feeds the activity ring, so
// the count and the audit row come from one event and cannot disagree about
// whether something happened.
//
// Conversation-scoped grants are skipped: they are not in the file, they
// vanish with the conversation, and a ledger row for one would outlive the
// grant it counts — a history entry for a permission that no longer exists,
// which is exactly the confusion the ledger is meant to remove.
func (d *Daemon) recordApprovalUse(ev session.Event) {
	if ev.Type != "tool.pre_approved" {
		return
	}
	if scope, _ := ev.Data["scope"].(string); scope != string(tools.RememberAlways) {
		return
	}
	pattern, _ := ev.Data["pattern"].(string)
	if pattern == "" {
		return
	}
	d.approvals.ledger.Used(pattern, time.Now())
}

// ---------------------------------------------------------- spoken listing

// spokenApprovalsCap bounds how many patterns the voice listing reads in
// full. Past a handful the ear stops following; the window's Approvals tab
// always shows everything, and the sentence says so — the vocabulary
// listing's rule, applied to a subject where being told "and there are nine
// more" matters considerably more.
const spokenApprovalsCap = 6

// approvalsVoice is the daemon's ApprovalsLister: it composes the one spoken
// answer to "what have I pre-approved?". Read-only by construction — it holds
// no writer, so the voice path cannot become an authoring path by accident.
type approvalsVoice struct{ store *approvalStore }

// SpokenApprovals implements session.ApprovalsLister.
func (v *approvalsVoice) SpokenApprovals(conversation []string) (string, error) {
	permanent := v.store.Patterns()
	sorted := append([]string(nil), permanent...)
	sort.Strings(sorted) // spoken order is alphabetical; the file keeps its own
	switch {
	case len(sorted) == 0 && len(conversation) == 0:
		return "You have not pre-approved anything. Every command still asks first.", nil
	case len(sorted) == 0:
		return fmt.Sprintf(
			"Nothing permanently. Just for this conversation, %s. That goes when the conversation does.",
			spokenPatternList(conversation, len(conversation))), nil
	}
	shown := sorted
	if len(shown) > spokenApprovalsCap {
		shown = shown[:spokenApprovalsCap]
	}
	spoken := fmt.Sprintf("You have pre-approved %s: %s.",
		approvalCountPhrase(len(sorted)), spokenPatternList(shown, len(shown)))
	if len(sorted) > len(shown) {
		spoken = fmt.Sprintf("You have pre-approved %s, including %s. "+
			"The full list is in the window's Approvals tab.",
			approvalCountPhrase(len(sorted)), spokenPatternList(shown, len(shown)))
	}
	if len(conversation) > 0 {
		spoken += fmt.Sprintf(" Just for this conversation, also %s.",
			spokenPatternList(conversation, len(conversation)))
	}
	// Said every time, and deliberately not trimmed once the list gets long:
	// a listing of standing permissions that does not end with how to take
	// one back is a listing that makes the user feel stuck with them.
	return spoken + " Say the word, or run jarvix approvals forget, to take one back.", nil
}

// spokenPatternList joins patterns for speech, quoting nothing — the ear
// cannot hear a quotation mark, and a pattern is short enough to say plainly.
func spokenPatternList(patterns []string, n int) string {
	if n > len(patterns) {
		n = len(patterns)
	}
	switch n {
	case 0:
		return "nothing"
	case 1:
		return patterns[0]
	}
	return strings.Join(patterns[:n-1], ", ") + ", and " + patterns[n-1]
}

// approvalCountPhrase words a pattern count for speech.
func approvalCountPhrase(n int) string {
	if n == 1 {
		return "one command"
	}
	return fmt.Sprintf("%d commands", n)
}

// ---------------------------------------------------------- session.confirm

// handleSessionConfirm answers the pending tool confirmation. It is the one
// verb the window card, the overlay controls, and `jarvix confirm` all
// resolve through (ADR 0013), and since #162 it carries the third answer as
// well as the first two.
//
// The `remember` parameter is a SCOPE WORD and nothing else — "always",
// "conversation", or absent. It is never a pattern, and that is the single
// most important line in this file: the rule about to be written is the one
// the engine derived when it asked the question and published on the card, so
// a client cannot name a rule and neither can anything that can reach a
// client. The model provokes cards; it does not write them, press them, or
// choose what they say.
func (d *Daemon) handleSessionConfirm(params json.RawMessage) (any, error) {
	// Approved defaults to true: `jarvix confirm` is the affirmative;
	// declining is the explicit case.
	p := struct {
		Approved *bool  `json:"approved"`
		Remember string `json:"remember"`
	}{}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "session.confirm params: %v", err)
		}
	}
	approved := p.Approved == nil || *p.Approved
	if !tools.ValidRememberScope(p.Remember) {
		return nil, ipc.Errorf(ipc.CodeInvalidParams,
			"session.confirm remember %q is invalid; use %q, %q, or omit it",
			p.Remember, string(tools.RememberAlways), string(tools.RememberConversation))
	}
	scope := tools.RememberScope(p.Remember)

	result := map[string]any{"approved": approved}
	if scope == tools.RememberAlways {
		// The write comes first, and the confirmation is not resolved if it
		// fails. "Approve and don't ask again" is one answer, not two: landing
		// half of it — running the command while the rule silently did not
		// save — would leave the user believing they had granted something
		// they had not, which is the worst possible state for a permission
		// surface to reach.
		pending, ok := d.engine.PendingConfirmation()
		if !ok {
			return nil, ipc.Errorf(ipc.CodeSessionError, "no tool confirmation is pending")
		}
		if !pending.Remember.Offered {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"that command cannot be remembered: %s", pending.Remember.Reason)
		}
		if !approved {
			return nil, ipc.Errorf(ipc.CodeInvalidParams,
				"a rule is only added when you approve; decline simply declines")
		}
		if err := d.rememberPattern(pending.Remember.Pattern, approvalSourceCard); err != nil {
			return nil, err
		}
		result["remembered"] = pending.Remember.Pattern
	}
	if err := d.engine.ConfirmRemembering(approved, scope); err != nil {
		return nil, ipc.Errorf(ipc.CodeSessionError, "%v", err)
	}
	if scope == tools.RememberConversation {
		if pattern := d.conversationGrantJustAdded(); pattern != "" {
			result["remembered"] = pattern
		}
	}
	if scope != tools.RememberNone {
		result["remember_scope"] = string(scope)
	}
	return result, nil
}

// conversationGrantJustAdded reports the most recent conversation-scoped
// grant, for the reply's "here is what I remembered" field. Read after the
// resolution rather than before, because before the resolution the grant does
// not exist yet and reporting it would be a promise rather than a fact.
func (d *Daemon) conversationGrantJustAdded() string {
	grants := d.engine.ConversationGrants()
	if len(grants) == 0 {
		return ""
	}
	return grants[len(grants)-1]
}
