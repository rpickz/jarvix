package daemon

// This file is the daemon half of remembered command approvals (issue #162,
// ADR 0053): the writer that appends one word-prefix rule to
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

// The two configuration keys this file writes. Named once so the writer, the
// reload and the tests cannot disagree about them.
//
// shell_deny joined shell_allow in #164, when the Approvals view learned to add
// a deny rule and remove an allow one. Both are reload-class (ADR 0053) and
// both are structurally absent from the settings registry, which is what keeps
// #109's exclusion wall standing while a human edits either.
const (
	shellAllowKey = "tools.policy.shell_allow"
	shellDenyKey  = "tools.policy.shell_deny"
)

// The two lists, named as the wire names them. A verb that writes the gate
// takes one of these and nothing else: "which list" is never inferred from the
// shape of a request, because inferring it wrong in the widening direction is
// the one mistake this surface must not be able to make.
const (
	approvalListAllow = "allow"
	approvalListDeny  = "deny"
)

// Approval-change sources, mirroring the setting-change sources in
// settings.go: a card answered in the window or the overlay, and the CLI.
// Both are people; the label says which surface, so the activity feed can
// report where a rule came from.
const (
	approvalSourceCard   = "card"
	approvalSourceCLI    = "cli"
	approvalSourceWindow = "window"
)

func (d *Daemon) registerApprovalMethods() {
	d.server.Handle("approvals.list", d.handleApprovalsList)
	d.server.Handle("approvals.forget", d.handleApprovalsForget)
	d.server.Handle("approvals.add", d.handleApprovalsAdd)
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
	// The deny list travels with the allow list because the view edits both and
	// because a person reading "what runs without asking" is owed the other
	// half: a deny rule is the reason something they granted still asks. It
	// carries no ledger — the ledger counts standing GRANTS being used, and a
	// deny rule's whole job is that nothing happens.
	denied := make([]map[string]any, 0, len(d.shellDenyPatterns()))
	for _, pattern := range d.shellDenyPatterns() {
		denied = append(denied, map[string]any{"pattern": pattern})
	}
	return map[string]any{
		"path":     d.paths.ConfigFile(),
		"approved": rows,
		"denied":   denied,
	}, nil
}

// handleApprovalsAdd is the Approvals view's authoring verb (#164, ADR 0054):
// one pattern a PERSON typed, onto one named list.
//
// It is the first IPC method in this project that accepts a pattern on the
// granting path, and ADR 0053 rejected exactly that for the confirmation card.
// The distinction it rests on is not politeness, it is provenance: the card's
// pattern must be derived because the card exists in response to something the
// MODEL asked for, and a model that could name a rule would only need to
// persuade some client to forward it. Nothing here happens in response to the
// model. There is no tool that reaches this method, `jarvix`/`jarvixd` are in
// the refusal matrix so no remembered shell rule can reach the CLI either, and
// the reply says in words what was written.
//
// The allow list carries the confirmation card's own refusal matrix, imported
// rather than restated (tools.Policy.VetAllowPattern) — the two routes to
// shell_allow judge patterns with one function, so they cannot come to
// different answers about `docker` or `timeout`. The deny list carries none:
// every deny is a tightening, and a gate that argued with someone making it
// stricter would be a gate people work around.
func (d *Daemon) handleApprovalsAdd(params json.RawMessage) (any, error) {
	var p struct {
		Pattern     string `json:"pattern"`
		List        string `json:"list"`
		Fingerprint string `json:"fingerprint"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "approvals.add params: %v", err)
		}
	}
	// `list` is required, not defaulted. A default would mean a request that
	// forgot the field widened the gate, and "the caller omitted a field" must
	// never be a way to reach the loosening direction.
	if p.List != approvalListAllow && p.List != approvalListDeny {
		return nil, ipc.Errorf(ipc.CodeInvalidParams,
			"approvals.add list %q is invalid; use %q or %q",
			p.List, approvalListAllow, approvalListDeny)
	}
	pattern := normalisePattern(p.Pattern)
	if pattern == "" {
		// Field-keyed rather than a protocol error: the form is asking about
		// text somebody typed, and an empty rule is the most likely first thing
		// they will send. It belongs under the input, not in a banner.
		return nil, approvalPatternRefused("type the leading words of a command, like \"docker ps\".")
	}
	if p.List == approvalListAllow {
		if offer := d.registry.Policy().VetAllowPattern(pattern); !offer.Offered {
			// The matrix's own sentence, verbatim: it was written to be shown
			// on a card and read aloud unchanged, and the form shows the same
			// words for the same refusal.
			return nil, approvalPatternRefused(offer.Reason)
		}
		return d.addApprovalPattern(approvalListAllow, pattern, p.Fingerprint)
	}
	return d.addApprovalPattern(approvalListDeny, pattern, p.Fingerprint)
}

// approvalPatternRefused wraps one refusal in the entry pipeline's field-keyed
// shape, so the add form pins it under the input that typed it exactly as every
// other form in the window does.
func approvalPatternRefused(reason string) *ipc.Error {
	return &ipc.Error{
		Code:    ipc.CodeConfigInvalid,
		Message: "that rule cannot be added: " + reason,
		Data:    map[string]any{"problems": []entryProblem{{Field: "pattern", Message: reason}}},
	}
}

// addApprovalPattern appends one pattern to one list and reports what it did.
func (d *Daemon) addApprovalPattern(list, pattern, fingerprint string) (any, error) {
	current := d.approvalPatterns(list)
	for _, existing := range current {
		if normalisePattern(existing) == pattern {
			// Idempotent rather than an error, like rememberPattern: the user
			// asked for a state the file is already in.
			return map[string]any{"added": false, "pattern": pattern, "list": list,
				"reason": "that rule is already on the list"}, nil
		}
	}
	next := append(append([]string(nil), current...), pattern)
	if err := d.writeApprovalList(list, next, fingerprint); err != nil {
		return nil, err
	}
	result := map[string]any{"added": true, "pattern": pattern, "list": list}
	if list == approvalListAllow {
		d.approvals.ledger.Added(pattern, time.Now())
	} else {
		// What this deny now overrides. Deny wins over allow in both directions
		// of prefix (ADR 0014), so a new deny can silence a standing grant the
		// user made months ago — and being told which is the difference between
		// tightening the gate and discovering later that something stopped
		// working.
		if shadowed := shadowedAllows(pattern, d.shellAllowPatterns()); len(shadowed) > 0 {
			result["shadows"] = shadowed
		}
	}
	d.log.Info("approval rule added", "component", "tools",
		"pattern", pattern, "list", list, "source", approvalSourceWindow)
	d.bus.Publish(session.Event{Type: "approvals.changed", Data: map[string]any{
		"action": "added", "pattern": pattern, "list": list,
		"scope": string(tools.RememberAlways), "source": approvalSourceWindow,
	}})
	return result, nil
}

// shadowedAllows lists the allow patterns a new deny rule now beats, in either
// prefix direction — the same comparison the classifier makes.
func shadowedAllows(deny string, allows []string) []string {
	denyWords := strings.Fields(deny)
	var out []string
	for _, allow := range allows {
		allowWords := strings.Fields(allow)
		if wordPrefix(denyWords, allowWords) || wordPrefix(allowWords, denyWords) {
			out = append(out, allow)
		}
	}
	sort.Strings(out)
	return out
}

// wordPrefix reports whether a is a word-for-word prefix of b (equal counts).
// The daemon's own copy of the comparison, because the tools package's is
// unexported and this is a report about the file rather than a decision about
// a command — a decision would have to be the classifier's or it would be a
// second gate.
func wordPrefix(a, b []string) bool {
	if len(a) > len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
		List    string `json:"list"`
		// Confirmed answers the question a deny removal asks first. Absent is
		// "not yet", which is why it is a plain bool: there is no third state,
		// and a client that has never heard of it cannot remove a deny rule.
		Confirmed bool `json:"confirmed"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, ipc.Errorf(ipc.CodeInvalidParams, "approvals.forget params: %v", err)
		}
	}
	// The allow list is the default because it is what forgetting meant before
	// there was a second list, and `jarvix approvals forget` still says nothing
	// about lists. Defaulting is safe HERE and unsafe in approvals.add for the
	// same one reason: this direction only ever tightens the gate.
	if p.List == "" {
		p.List = approvalListAllow
	}
	if p.List != approvalListAllow && p.List != approvalListDeny {
		return nil, ipc.Errorf(ipc.CodeInvalidParams,
			"approvals.forget list %q is invalid; use %q or %q",
			p.List, approvalListAllow, approvalListDeny)
	}
	pattern := normalisePattern(p.Pattern)
	if pattern == "" {
		return nil, ipc.Errorf(ipc.CodeInvalidParams,
			"approvals.forget needs a pattern, e.g. {\"pattern\": \"docker ps\"}")
	}
	if p.List == approvalListDeny {
		return d.forgetDenyPattern(pattern, p.Confirmed)
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
	kept, found := withoutPattern(current, pattern)
	if !found {
		return map[string]any{"forgotten": false, "pattern": pattern,
			"list": approvalListAllow, "approved": current}, nil
	}
	if err := d.writeApprovalList(approvalListAllow, kept, ""); err != nil {
		return nil, err
	}
	d.approvals.ledger.Forget(pattern)
	d.bus.Publish(session.Event{Type: "approvals.changed", Data: map[string]any{
		"action": "forgotten", "pattern": pattern, "list": approvalListAllow,
		"scope": string(tools.RememberAlways), "source": approvalSourceCLI,
	}})
	return map[string]any{"forgotten": true, "pattern": pattern,
		"list": approvalListAllow, "scope": string(tools.RememberAlways)}, nil
}

// forgetDenyPattern removes one deny rule, and asks first.
//
// This is the one revocation in the project that is deliberately harder than
// the thing it revokes, and the asymmetry is the point. Forgetting an ALLOW
// rule narrows what runs unasked, so it is answered immediately and never
// questioned. Removing a DENY rule is the opposite act wearing the same verb:
// it widens what may run, and it does so by deleting a protection whose whole
// job is that nothing has been happening — there is no ledger row, no activity
// feed entry, nothing to remind anyone what it has been stopping. So the daemon
// says what the rule protected, in its own words, and does nothing until that
// sentence is answered.
func (d *Daemon) forgetDenyPattern(pattern string, confirmed bool) (any, error) {
	current := d.shellDenyPatterns()
	kept, found := withoutPattern(current, pattern)
	if !found {
		return map[string]any{"forgotten": false, "pattern": pattern,
			"list": approvalListDeny, "denied": current}, nil
	}
	if !confirmed {
		return map[string]any{
			"forgotten":        false,
			"pattern":          pattern,
			"list":             approvalListDeny,
			"confirm_required": true,
			"confirmation":     denyRemovalConfirmation(pattern),
		}, nil
	}
	if err := d.writeApprovalList(approvalListDeny, kept, ""); err != nil {
		return nil, err
	}
	d.log.Info("deny rule removed", "component", "tools",
		"pattern", pattern, "list", approvalListDeny, "source", approvalSourceWindow)
	d.bus.Publish(session.Event{Type: "approvals.changed", Data: map[string]any{
		"action": "forgotten", "pattern": pattern, "list": approvalListDeny,
		"source": approvalSourceWindow,
	}})
	return map[string]any{"forgotten": true, "pattern": pattern,
		"list": approvalListDeny}, nil
}

// denyRemovalConfirmation is the sentence a deny removal has to be answered
// with, naming what the rule protected rather than asking "are you sure?" —
// which is a question nobody reads. It states the shape of command the rule has
// been refusing, that the refusal beat every allow rule including a remembered
// one, and what those commands will do instead.
func denyRemovalConfirmation(pattern string) string {
	return fmt.Sprintf(
		"The deny rule %q refuses every command beginning with those words, whoever asks — "+
			"the assistant, one of your own spoken intents, or an allow rule that would "+
			"otherwise cover it, because deny always wins. Removing it means those commands "+
			"are classified like any other again: they will ask instead of being refused, and "+
			"an answer of \"approve and don't ask again\" could then make them silent. Nothing "+
			"else in [tools.policy] changes. Confirm to remove it.", pattern)
}

// withoutPattern returns list minus pattern, and whether it was there.
func withoutPattern(list []string, pattern string) ([]string, bool) {
	kept := make([]string, 0, len(list))
	found := false
	for _, existing := range list {
		if normalisePattern(existing) == pattern {
			found = true
			continue
		}
		kept = append(kept, existing)
	}
	return kept, found
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
	if err := d.writeApprovalList(approvalListAllow,
		append(append([]string(nil), current...), pattern), ""); err != nil {
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

// writeApprovalList replaces one whole `[tools.policy]` pattern list in
// config.toml and recompiles the running policy from the result.
//
// One writer for both lists, because everything that makes the write safe is
// the same for both: the fingerprint guard, the rebase onto the file rather
// than the running config, whole-document validation, a compile BEFORE the
// write so a rule the classifier will not accept never lands, the atomic write,
// and only then the swap of the running gate. Two writers would be two places
// for one of those to go missing.
//
// fingerprint, when non-empty, is the caller's view of the file and a
// mismatch fails the write — the settings surface's external-edit guard,
// applied here for the same reason: a rule appended on top of a file someone
// has since hand-edited would silently discard their edit, and the one file
// where that must never happen is the one that says what may run.
func (d *Daemon) writeApprovalList(list string, patterns []string, fingerprint string) error {
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
	key := shellAllowKey
	read := func(c config.Config) any { return c.Tools.Policy.ShellAllow }
	if list == approvalListDeny {
		key, read = shellDenyKey, func(c config.Config) any { return c.Tools.Policy.ShellDeny }
		fileCfg.Tools.Policy.ShellDeny = patterns
	} else {
		fileCfg.Tools.Policy.ShellAllow = patterns
	}
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
	newRaw, err := config.RewriteOffRegistryKey(raw, key, patterns, read)
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
	d.cfgMu.Lock()
	if list == approvalListDeny {
		d.cfg.Tools.Policy.ShellDeny = patterns
	} else {
		d.cfg.Tools.Policy.ShellAllow = patterns
	}
	d.cfgMu.Unlock()
	if list == approvalListAllow {
		// The approval store holds the ALLOW list only: it exists to answer
		// "what runs without asking", which a deny rule never does.
		d.approvals.setPatterns(patterns)
	}
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

// shellDenyPatterns is the running gate's configured deny list. It comes from
// the running configuration rather than a holder of its own, for the reason the
// approval store exists and this does not: the store is there because the
// SESSION ENGINE needs a read-only view of the allow list to answer a spoken
// question, and nothing speaks the deny list.
func (d *Daemon) shellDenyPatterns() []string {
	return append([]string(nil), d.runningConfig().Tools.Policy.ShellDeny...)
}

// approvalPatterns is one named list, as the running gate holds it.
func (d *Daemon) approvalPatterns(list string) []string {
	if list == approvalListDeny {
		return d.shellDenyPatterns()
	}
	return d.shellAllowPatterns()
}

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
