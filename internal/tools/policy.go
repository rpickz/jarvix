package tools

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/rpickz/jarvix/internal/ai"
)

// PolicyDecision is one tier of the tool permission gate (ADR 0014): allow
// runs silently, ask requires a spoken/keyed confirmation, deny never runs.
type PolicyDecision string

// The three policy tiers, ordered by severity: deny > ask > allow.
const (
	PolicyAllow PolicyDecision = "allow"
	PolicyAsk   PolicyDecision = "ask"
	PolicyDeny  PolicyDecision = "deny"
)

// ValidDecision reports whether s names a policy tier.
func ValidDecision(s string) bool {
	switch PolicyDecision(s) {
	case PolicyAllow, PolicyAsk, PolicyDeny:
		return true
	}
	return false
}

// PolicyConfig is the user-facing policy configuration ([tools.policy] in
// config.toml), mirrored here so this package does not depend on
// internal/config.
type PolicyConfig struct {
	// Default is the decision for tools with no per-tool entry. An unknown
	// tool must never run silently, so the shipped default is ask.
	Default PolicyDecision
	// Tools maps a tool name to its decision. For shell.run the entry is the
	// fallback for commands no pattern classifies: "ask" (the default) keeps
	// read-only commands silent and confirms everything else; "allow"
	// restores the pre-gate behaviour (everything runs, deny patterns still
	// win); "deny" disables the tool entirely.
	Tools map[string]PolicyDecision
	// ShellAllow adds word-prefix patterns (e.g. "docker compose ps") that
	// run without confirmation. Since issue #162 this is also where a
	// "don't ask again" answer to a confirmation card lands — appended by the
	// surgical config editor, never by the assistant (#109's wall stands) —
	// which is why the list's vocabulary stays exactly what it always was.
	ShellAllow []string
	// ShellDeny adds word-prefix patterns that never run, regardless of any
	// confirmation. Deny beats everything.
	ShellDeny []string
	// Advisors maps a configured advisor name to the tier its configuration
	// earns (ADR 0016): allow for one running a shipped read-only preset
	// unchanged — it only reads and answers — and ask for everything else,
	// which is any advisor given a hand-written argv or one whose CLI can act
	// on the machine. An advisor absent from this map is unknown and asks.
	Advisors map[string]PolicyDecision
}

// Verdict is one gate decision, made daemon-side from the parsed command —
// never from the model's own description of what it is doing.
type Verdict struct {
	Decision PolicyDecision
	Tool     string
	// Command is the exact shell command being judged ("" for non-shell
	// tools). The overlay shows this verbatim; it is the ground truth the
	// user confirms, not whatever the model claimed.
	Command string
	// Rule names the rule that decided, for logs and the audit trail
	// (e.g. `risky command "rm"`, `allow pattern "docker ps"`).
	Rule string
	// Summary is the one-sentence spoken confirmation question, generated
	// from Command so a model cannot describe `rm -rf ~` as "tidying up".
	// Set only when Decision is PolicyAsk.
	Summary string
	// PreApproved marks an allow that a *user-granted* pattern produced —
	// a `[tools.policy] shell_allow` entry or a conversation-scoped grant
	// (issue #162) — as opposed to a shipped read-only allow pattern or a
	// tool tier. It is what makes a pre-approved run auditable: the caller
	// puts a row in the activity feed naming Rule, so a standing grant can
	// never make Jarvix act silently. A separate field rather than a string
	// match on Rule because the audit promise must not rest on prose.
	PreApproved bool
	// Pattern is the granted word-prefix that produced a PreApproved allow,
	// verbatim ("docker ps"). Carried as a field rather than recovered from
	// Rule's prose so the ledger that counts how often a rule has fired keys
	// on the same string the user is shown and revokes by.
	Pattern string
}

// Policy is the compiled permission gate. All patterns are compiled once at
// construction so a Decide call is a handful of string scans — the gate must
// add no perceptible latency to allow-listed calls.
type Policy struct {
	defaultDecision PolicyDecision
	tools           map[string]PolicyDecision
	extraAllow      [][]string // word-prefix patterns from configuration
	extraDeny       [][]string
	advisors        map[string]PolicyDecision // per-advisor tier (ADR 0016)
}

// NewPolicy validates and compiles a policy. Errors are actionable: they name
// the offending key and the accepted values.
func NewPolicy(cfg PolicyConfig) (*Policy, error) {
	p := &Policy{
		defaultDecision: cfg.Default,
		tools:           make(map[string]PolicyDecision, len(cfg.Tools)),
	}
	if p.defaultDecision == "" {
		p.defaultDecision = PolicyAsk
	}
	if !ValidDecision(string(p.defaultDecision)) {
		return nil, fmt.Errorf("tools.policy.default %q is invalid; use \"allow\", \"ask\", or \"deny\"", cfg.Default)
	}
	names := make([]string, 0, len(cfg.Tools))
	for name := range cfg.Tools {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic error order
	for _, name := range names {
		d := cfg.Tools[name]
		if !ValidDecision(string(d)) {
			return nil, fmt.Errorf("tools.policy.tool.%q is %q; use \"allow\", \"ask\", or \"deny\"", name, d)
		}
		p.tools[name] = d
	}
	p.advisors = make(map[string]PolicyDecision, len(cfg.Advisors))
	advisorNames := make([]string, 0, len(cfg.Advisors))
	for name := range cfg.Advisors {
		advisorNames = append(advisorNames, name)
	}
	sort.Strings(advisorNames) // deterministic error order
	for _, name := range advisorNames {
		d := cfg.Advisors[name]
		if !ValidDecision(string(d)) {
			return nil, fmt.Errorf("advisor %q has decision %q; use \"allow\", \"ask\", or \"deny\"", name, d)
		}
		p.advisors[name] = d
	}
	var err error
	if p.extraAllow, err = compileWordPatterns("tools.policy.shell_allow", cfg.ShellAllow); err != nil {
		return nil, err
	}
	if p.extraDeny, err = compileWordPatterns("tools.policy.shell_deny", cfg.ShellDeny); err != nil {
		return nil, err
	}
	return p, nil
}

func compileWordPatterns(key string, patterns []string) ([][]string, error) {
	out := make([][]string, 0, len(patterns))
	for _, pat := range patterns {
		words := strings.Fields(pat)
		if len(words) == 0 {
			return nil, fmt.Errorf("%s contains an empty pattern; each entry must be a command prefix such as \"docker ps\"", key)
		}
		out = append(out, words)
	}
	return out, nil
}

// builtinToolDefaults preserves pre-gate behaviour for tools whose blast
// radius is already bounded by construction: artifact.create only writes
// into the artifact directory and opens a viewer, so it stays silent. A
// [tools.policy.tool] entry overrides these; genuinely unknown tools still
// fall through to the ask default.
//
// The two window verbs here are the reads. Listing windows sees no more than
// the desktop context Jarvix may already gather, and focusing one changes
// nothing but where the user is looking — the user sees it happen, and undoes
// it by looking back. Confirming either would mean a question before every
// "put me back in the browser", which is the interaction this exists to
// replace. Moving, closing and launching are absent on purpose: they are
// state changes, so they take the policy default (ask), and
// `[tools.policy.tool]."desktop.move_window" = "allow"` is how a user who
// disagrees says so.
//
// The two memory verbs here are remember and recall (ADR 0025). Recall is a
// read. Remember mutates state, but its blast radius is bounded by
// construction the way artifact.create's is: it writes only into the user's
// own 0600 memory file, the engine speaks a one-sentence confirmation of
// what was stored, and a wrong fact is undone with "forget that". Asking
// would turn every "remember X" — an instruction the user just gave out
// loud — into a question about itself. memory.forget is absent on purpose:
// deletion is the one memory operation that cannot be undone, so it takes
// the policy default (ask) and confirms the exact fact about to go.
// vocabulary.teach (#129) is allow on memory.remember's exact argument —
// teaching is the user's explicit word, and the write is one line into their
// own 0600 vocabulary file, undone with one forget — and vocabulary.forget
// is absent for memory.forget's exact reason: deleting an entry destroys its
// taught history, so it takes the policy default and confirms the exact
// phrase about to go.
//
// routine.run is allow for yet another reason: authorship. Every step of a
// routine was written by the user in their own configuration — a fixed
// program name and a fixed placement, no shell anywhere (ADR 0026) — and the
// spoken phrase is itself the instruction to run exactly those steps. Asking
// "should I run your morning setup?" after "morning setup" would be asking
// the user to confirm their own sentence. A user who wants the question
// anyway (a shared machine, say) writes
// `[tools.policy.tool]."routine.run" = "ask"`, and deny disables routines
// outright.
//
// conversations.search is allow for the same reason the other reads are: it
// looks at the user's own archive and changes nothing. Asking "may I search
// what you said?" before answering "what did we say about X?" would turn the
// question into a toll booth — and the search's contents never leave the
// daemon except as the spoken answer the user asked for.
// knowledge.refresh is allow for the routine.run reason: authorship. A feed's
// command was written by the user in their own configuration — a fixed argv,
// no shell anywhere (ADR 0031) — and reading its cached output is a read of
// the user's own data. Asking "may I check your AMD feed?" after "what's the
// AMD price?" would be asking the user to confirm their own sentence. A user
// who wants the question anyway writes
// `[tools.policy.tool]."knowledge.refresh" = "ask"` (which also stops the
// background schedules — a scheduled fetch has no way to ask), and deny
// disables feeds outright.
// The three config.* read verbs (issue #105) are allow for the reads' reason:
// they look at the user's own config.toml — entries and registry settings the
// user wrote or the settings screen shows — and change nothing. Asking "may I
// look at your morning setup routine?" before editing it would put a toll
// booth in front of the read the write discipline *requires* (an edit must
// start from what the entry actually contains). Secrets never enter the
// registry (ADR 0015) and the [ai] space is pruned from the settings view
// before the tool sees it, so there is nothing here a read could leak that
// the prompt does not already carry.
// desktop.name_window (#126) sits with the window reads for the focus
// reason: assigning a nickname changes nothing on screen, enters nothing
// anywhere, and the opposite assignment undoes it exactly. The user also
// just said the name out loud — "call this window builds" IS the
// authorisation — so a question would confirm their own sentence.
// The three reminder verbs (#141, ADR 0046) are allow on memory.remember's
// exact argument. reminder.list is a read. reminder.set writes one line
// into the user's own 0600 reminder store, the confirmation speaks exactly
// when it will fire, and a wrong one is undone with "cancel the …
// reminder"; "remind me at three" IS the authorisation, and a card here
// would rebuild the config-write ceremony the feature exists to remove
// (the deterministic phrase creates without one, pinned by test).
// reminder.cancel is allow — unlike memory.forget — because cancelling
// destroys nothing: the entry moves to the retained fired history, and
// re-setting it is one sentence.
var builtinToolDefaults = map[string]PolicyDecision{
	"artifact.create":           PolicyAllow,
	ListWindowsToolName:         PolicyAllow,
	FocusWindowToolName:         PolicyAllow,
	NameWindowToolName:          PolicyAllow,
	MemoryRememberToolName:      PolicyAllow,
	MemorySearchToolName:        PolicyAllow,
	VocabularyTeachToolName:     PolicyAllow,
	RoutineToolName:             PolicyAllow,
	ConversationsSearchToolName: PolicyAllow,
	KnowledgeRefreshToolName:    PolicyAllow,
	ConfigListEntriesToolName:   PolicyAllow,
	ConfigGetEntryToolName:      PolicyAllow,
	ConfigReadSettingsToolName:  PolicyAllow,
	ReminderSetToolName:         PolicyAllow,
	ReminderListToolName:        PolicyAllow,
	ReminderCancelToolName:      PolicyAllow,
	BriefingToolName:            PolicyAllow,
}

// neverSilent are the tools that must not inherit an "allow" policy default.
//
// Everything else in the registry is judged by the gate-wide default, on the
// argument that a user who wrote `default = "allow"` meant it. Synthetic
// keystrokes are the exception, and the reason is that they are the one
// capability whose target the model does not choose and cannot see: the keys
// land wherever focus is at that instant, and a mistake is neither visible
// before it happens nor undoable after. So a global "allow" does not reach
// them — only naming the tool explicitly
// (`[tools.policy.tool]."typing.type_text" = "allow"`) does, which is a
// sentence a user has to mean.
//
// The exception runs one way. A stricter default still wins: `default =
// "deny"` denies these too, because tightening is never the thing to override.
var neverSilent = map[string]bool{
	TypeTextToolName: true,
	PressKeyToolName: true,
}

// ToolDecision returns the configured tier for a tool: its per-tool entry,
// a built-in default (shell.run and advisor.ask classify per call with an ask
// fallback, artifact.create is allow, the typing tools always ask), or the
// policy default. Used by status reporting; Decide applies the same
// resolution.
func (p *Policy) ToolDecision(name string) PolicyDecision {
	if d, ok := p.tools[name]; ok {
		return d
	}
	// knowledge.get is judged under the knowledge.refresh identity (ADR
	// 0030): reading a feed is what triggers fetching it, so one name governs
	// both and a `[tools.policy.tool]` entry cannot half-disable the feature.
	if name == KnowledgeGetToolName {
		return p.ToolDecision(KnowledgeRefreshToolName)
	}
	if name == shellToolName || name == advisorToolName {
		return PolicyAsk
	}
	if name == ScriptToolName {
		// Arbitrary execution behind a spoken phrase: a global "allow" does
		// not reach it (only naming the tool does), a global "deny" does —
		// the same one-way exception the typing tools carry, and for the same
		// reason stated at neverSilent: loosening must be explicit, tightening
		// must always win.
		if p.defaultDecision == PolicyDeny {
			return PolicyDeny
		}
		return PolicyAsk
	}
	if name == ConfigWriteEntryToolName || name == ConfigDeleteEntryToolName {
		// script.run's floor, restated at authoring time (issue #105, ADR
		// 0036). Every entry these tools can write is command-bearing: a
		// script IS a command, a feed carries the argv it fetches with, and a
		// routine's steps launch applications — so writing (or removing) one
		// is arranging for something to run later, and a global "allow" must
		// not make that silent. Only naming the tool explicitly
		// (`[tools.policy.tool]."config.write_entry" = "allow"`) does — a
		// sentence a user has to mean — and `default = "deny"` still wins,
		// because tightening is never the thing to override.
		if p.defaultDecision == PolicyDeny {
			return PolicyDeny
		}
		return PolicyAsk
	}
	if neverSilent[name] {
		if p.defaultDecision == PolicyDeny {
			return PolicyDeny
		}
		return PolicyAsk
	}
	if d, ok := builtinToolDefaults[name]; ok {
		return d
	}
	return p.defaultDecision
}

// RememberableApproval reports whether an approval of this tool may be reused
// for the rest of the conversation when `remember_for_conversation` is on.
//
// It is false for the typing tools, and the reason is that the setting's
// premise does not hold for them. Remembering an approval is safe when the
// thing approved is fully described by what was asked — the same command, the
// same advisor. A typing approval is about a payload *and* a window that had
// focus at that moment, and the second half cannot be carried forward: the
// user is at their keyboard, and by the next call they may be somewhere else
// entirely. Asking again costs a sentence; not asking costs whatever has focus.
func RememberableApproval(tool string) bool { return !neverSilent[tool] }

const shellToolName = "shell.run"

// ShellToolName is the registry name of the shell tool, exported so callers
// outside this package — the turn's provenance among them (issue #168) — can
// name it without repeating the literal.
const ShellToolName = shellToolName

// AdvisorToolName is the registry name of the delegation tool, exported so
// configuration and status reporting can name it without guessing.
const AdvisorToolName = advisorToolName

// IntentToolName is the identity user-defined voice intents ([[intents.custom]])
// execute under. They are not a model tool — the user wrote the command
// themselves — but they are still arbitrary shell execution triggered by
// speech, so they face the same classifier (ADR 0017). Giving them their own
// name rather than borrowing shell.run's means a user can allow their own
// intents (`[tools.policy.tool]."intent.run" = "allow"`) without also
// unleashing the model, and disabling shell.run does not silently break
// phrases they configured by hand. Deny rules still win either way.
const IntentToolName = "intent.run"

// RoutineToolName is the identity a configured routine ([[routines]], ADR
// 0026) runs under. Its own name, not intent.run's, for the same reason
// intent.run is not shell.run's: the risk profiles differ — a custom intent
// is an arbitrary shell command, a routine is a sequence of validated program
// launches and window placements — so each must be tightenable without
// touching the other. Unlike every other identity it defaults to allow (see
// builtinToolDefaults for the argument).
const RoutineToolName = "routine.run"

// ScriptToolName is the identity a configured script ([[scripts]], ADR 0030)
// runs under. Its own name — not routine.run's and not intent.run's — because
// the risk profiles differ and each must be tightenable alone: a routine is
// validated launches and placements, a custom intent is a shell command
// facing the classifier, and a script is an arbitrary executable run whole.
// Unlike routine.run it defaults to ask, and unlike most tools the global
// `default = "allow"` does not reach it (see ToolDecision): a phrase can be
// misheard, and the one control that answers a misheard phrase is the
// question. Only naming the tool explicitly
// (`[tools.policy.tool]."script.run" = "allow"`) silences it — a sentence a
// user has to mean. A stricter global default still wins: `default = "deny"`
// denies scripts too, because tightening is never the thing to override.
const ScriptToolName = "script.run"

// DecideScript classifies running one named script. There is no command to
// parse — the argv is the configured path and nothing else, fixed at load —
// so the verdict is the script.run identity's tier, with the script's name
// AND its absolute path as what the user is asked about. The path is the
// point (ADR 0014: confirmations are generated daemon-side from ground
// truth): a swapped or edited config cannot describe itself as something
// harmless, and a substituted file is visible in the very sentence that asks.
func (p *Policy) DecideScript(name, path string) Verdict {
	v := Verdict{Tool: ScriptToolName, Command: name + " (" + path + ")"}
	_, explicit := p.tools[ScriptToolName]
	switch p.ToolDecision(ScriptToolName) {
	case PolicyDeny:
		v.Decision = PolicyDeny
		if explicit {
			v.Rule = fmt.Sprintf("tool %q is set to deny", ScriptToolName)
		} else {
			v.Rule = fmt.Sprintf("tool %q is denied by the policy default", ScriptToolName)
		}
	case PolicyAllow:
		v.Decision = PolicyAllow
		v.Rule = fmt.Sprintf("tool %q is set to allow", ScriptToolName)
	default:
		v.Decision = PolicyAsk
		if explicit {
			v.Rule = fmt.Sprintf("tool %q is set to ask", ScriptToolName)
		} else {
			v.Rule = fmt.Sprintf("tool %q asks unless the configuration names it", ScriptToolName)
		}
		v.Summary = fmt.Sprintf("I'm about to run your %s script, at %s. Should I go ahead?", name, path)
	}
	return v
}

// DecideRoutine classifies running one named routine. There is no command to
// parse and no shell classifier to consult — a routine's steps were validated
// at config load and contain nothing but program names and placements — so
// the verdict is the tool identity's configured tier, with the routine's name
// as the Command the audit trail and any confirmation are about.
func (p *Policy) DecideRoutine(name string) Verdict {
	v := Verdict{Tool: RoutineToolName, Command: name}
	_, explicit := p.tools[RoutineToolName]
	switch p.ToolDecision(RoutineToolName) {
	case PolicyDeny:
		v.Decision = PolicyDeny
		v.Rule = fmt.Sprintf("tool %q is set to deny", RoutineToolName)
	case PolicyAsk:
		v.Decision = PolicyAsk
		if explicit {
			v.Rule = fmt.Sprintf("tool %q is set to ask", RoutineToolName)
		} else {
			v.Rule = fmt.Sprintf("tool %q asks under this policy", RoutineToolName)
		}
		v.Summary = fmt.Sprintf("I'm about to run your %s routine. Should I go ahead?", name)
	default:
		v.Decision = PolicyAllow
		if explicit {
			v.Rule = fmt.Sprintf("tool %q is set to allow", RoutineToolName)
		} else {
			v.Rule = fmt.Sprintf("tool %q defaults to allow; the user authored every step", RoutineToolName)
		}
	}
	return v
}

// DecideCommand classifies a bare command string that no model asked for —
// a user-defined intent — through the very same shell classifier a model's
// shell.run call faces. The tier comes from tool's own policy entry (or the
// policy default), so the decision is configurable per identity while the
// risk analysis stays identical.
func (p *Policy) DecideCommand(tool, command string) Verdict {
	return p.DecideCommandWithGrants(tool, command, nil)
}

// DecideCommandWithGrants is DecideCommand plus the conversation-scoped
// grants DecideWithGrants documents. A user-defined intent faces the same
// classifier as a model's shell.run call, so it must face the same grants:
// remembering "the docker ps question" on an intent's card and then being
// asked it again by the same intent would be the feature failing at its one
// job.
func (p *Policy) DecideCommandWithGrants(tool, command string, grants [][]string) Verdict {
	args, err := json.Marshal(struct {
		Command string `json:"command"`
	}{Command: command})
	if err != nil { // a string always marshals; belt and braces
		return Verdict{Decision: PolicyAsk, Tool: tool, Command: command,
			Rule:    "arguments could not be encoded",
			Summary: "I could not check that command. Should I go ahead?"}
	}
	v := p.decideShell(ai.ToolCall{Name: tool, Arguments: string(args)}, p.ToolDecision(tool), grants)
	v.Tool = tool
	// The tool-level deny tier short-circuits before the command is recorded;
	// the overlay and the audit trail still need to know what was refused.
	v.Command = command
	// decideShell names shell.run in its tier rules; restate them for this
	// identity so the audit trail says what the user actually configured.
	switch v.Decision {
	case PolicyDeny:
		if v.Rule == `tool "shell.run" is set to deny` {
			v.Rule = fmt.Sprintf("tool %q is set to deny", tool)
		}
	case PolicyAllow:
		if v.Rule == `tool "shell.run" is set to allow` {
			v.Rule = fmt.Sprintf("tool %q is set to allow", tool)
		}
	}
	return v
}

// Decide classifies one tool call. For shell.run the command is parsed and
// classified daemon-side: a compound command (`;`, `&&`, pipes, command
// substitution) is judged by its riskiest part, and deny beats ask beats
// allow. The model's arguments are the only input — its stated intent is
// never consulted.
func (p *Policy) Decide(call ai.ToolCall) Verdict {
	return p.DecideWithGrants(call, nil)
}

// DecideWithGrants is Decide plus conversation-scoped allow patterns (issue
// #162): word prefixes the user granted on a confirmation card for this
// conversation only, which the caller holds in memory and which never reach
// disk.
//
// They are applied at exactly the point the configured allow list is applied
// — after the deny check, after the risk regexes, after the risk words — so a
// grant is weaker than every existing control and cannot be anything else. A
// conversation-scoped grant of "ls" therefore still asks about `ls; rm -rf ~`,
// for the same reason a configured one does: the segments are judged
// separately and the rm is a risk word.
func (p *Policy) DecideWithGrants(call ai.ToolCall, grants [][]string) Verdict {
	mode := p.ToolDecision(call.Name)
	if call.Name == advisorToolName {
		return p.decideAdvisor(call, mode)
	}
	if call.Name == KnowledgeGetToolName {
		return p.decideKnowledge(call, mode)
	}
	if call.Name != shellToolName {
		// Grants are shell-command word prefixes and say nothing about any
		// other identity, so they are simply not consulted here — the
		// always-ask floor a tool carries is untouched by anything the card
		// can grant.
		v := Verdict{Decision: mode, Tool: call.Name}
		switch mode {
		case PolicyDeny:
			v.Rule = fmt.Sprintf("tool %q is set to deny", call.Name)
		case PolicyAllow:
			if _, ok := p.tools[call.Name]; ok {
				v.Rule = fmt.Sprintf("tool %q is set to allow", call.Name)
			} else {
				v.Rule = fmt.Sprintf("tool %q defaults to allow", call.Name)
			}
		default:
			_, explicit := p.tools[call.Name]
			switch {
			case explicit:
				v.Rule = fmt.Sprintf("tool %q is set to ask", call.Name)
			case neverSilent[call.Name]:
				v.Rule = fmt.Sprintf("tool %q always asks unless the configuration names it", call.Name)
			case call.Name == ConfigWriteEntryToolName || call.Name == ConfigDeleteEntryToolName:
				// The authoring floor above: same audit wording as script.run's,
				// because it is the same rule.
				v.Rule = fmt.Sprintf("tool %q asks unless the configuration names it", call.Name)
			default:
				v.Rule = "unknown tool defaults to ask"
			}
			v.Summary = fmt.Sprintf("I want to use the %s tool. Should I go ahead?", call.Name)
		}
		return v
	}
	return p.decideShell(call, mode, grants)
}

// decideAdvisor classifies one advisor.ask call (ADR 0016). Delegation sends
// the user's question to another program on their machine, so *which*
// advisor decides the tier — and the answer comes from configuration, never
// from the call: the model names an advisor, and the tier that name earned in
// config is applied to it.
//
// The shape mirrors shell.run: an explicit [tools.policy.tool] entry of
// "allow" trusts every advisor and "deny" disables delegation outright, while
// the default (ask) means "classify" — advisors on an unmodified read-only
// preset run silently, and anything else asks first.
func (p *Policy) decideAdvisor(call ai.ToolCall, mode PolicyDecision) Verdict {
	v := Verdict{Tool: call.Name}
	if mode == PolicyDeny {
		v.Decision = PolicyDeny
		v.Rule = `tool "advisor.ask" is set to deny`
		return v
	}

	var args struct {
		Advisor string `json:"advisor"`
	}
	if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil || strings.TrimSpace(args.Advisor) == "" {
		// Unparseable arguments name no advisor, so no per-advisor tier
		// applies. Execute will reject the call anyway; asking is the safe
		// failure mode.
		v.Decision = PolicyAsk
		v.Rule = "arguments could not be parsed"
		v.Summary = "I was asked to consult an assistant I could not identify. Should I go ahead?"
		return v
	}
	// Command is what the user is confirming and what a remembered approval
	// is keyed on: the advisor, not the question. Approving "ask Claude" once
	// approves asking Claude, never asking something else.
	advisor := strings.TrimSpace(args.Advisor)
	v.Command = advisor

	if mode == PolicyAllow {
		v.Decision = PolicyAllow
		v.Rule = `tool "advisor.ask" is set to allow`
		return v
	}
	if _, explicit := p.tools[call.Name]; !explicit {
		if p.advisors[advisor] == PolicyAllow {
			v.Decision = PolicyAllow
			v.Rule = fmt.Sprintf("advisor %q answers questions only", advisor)
			return v
		}
	}
	v.Decision = PolicyAsk
	if _, known := p.advisors[advisor]; known {
		v.Rule = fmt.Sprintf("advisor %q can act on this computer or runs a custom command", advisor)
	} else {
		v.Rule = fmt.Sprintf("advisor %q is not configured", advisor)
	}
	v.Summary = fmt.Sprintf("I'd like to ask %s about this. Should I go ahead?", advisor)
	return v
}

// decideKnowledge classifies one knowledge.get call under the
// knowledge.refresh identity (ADR 0031). There is no per-feed classifier to
// consult — every feed's command was written by the user and validated at
// config load — so the verdict is the identity's configured tier, with the
// feed's name as the Command the audit trail and any confirmation are about.
func (p *Policy) decideKnowledge(call ai.ToolCall, mode PolicyDecision) Verdict {
	v := Verdict{Tool: KnowledgeRefreshToolName}
	var args struct {
		Feed string `json:"feed"`
	}
	if err := json.Unmarshal([]byte(call.Arguments), &args); err == nil {
		v.Command = strings.TrimSpace(args.Feed)
	}
	_, explicit := p.tools[KnowledgeRefreshToolName]
	switch mode {
	case PolicyDeny:
		v.Decision = PolicyDeny
		v.Rule = `tool "knowledge.refresh" is set to deny`
	case PolicyAsk:
		v.Decision = PolicyAsk
		if explicit {
			v.Rule = `tool "knowledge.refresh" is set to ask`
		} else {
			v.Rule = `tool "knowledge.refresh" asks under this policy`
		}
		if v.Command != "" {
			v.Summary = fmt.Sprintf("I want to check your %s feed. Should I go ahead?", v.Command)
		} else {
			v.Summary = "I want to read one of your feeds. Should I go ahead?"
		}
	default:
		v.Decision = PolicyAllow
		if explicit {
			v.Rule = `tool "knowledge.refresh" is set to allow`
		} else {
			v.Rule = `tool "knowledge.refresh" defaults to allow; the user authored the feed's command`
		}
	}
	return v
}

func (p *Policy) decideShell(call ai.ToolCall, mode PolicyDecision, grants [][]string) Verdict {
	v := Verdict{Tool: call.Name}
	if mode == PolicyDeny {
		v.Decision = PolicyDeny
		v.Rule = `tool "shell.run" is set to deny`
		return v
	}

	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil || strings.TrimSpace(args.Command) == "" {
		// Unparseable arguments cannot be classified, so they cannot run
		// silently. Shell.Execute will reject them anyway; asking is the
		// safe failure mode.
		v.Decision = PolicyAsk
		v.Rule = "arguments could not be parsed"
		v.Summary = "I was asked to run a shell command I could not parse. Should I go ahead?"
		return v
	}
	command := strings.TrimSpace(args.Command)
	v.Command = command

	// Deny patterns run against the raw command first: splitting must never
	// be able to defeat a deny rule (e.g. a fork bomb is full of separators).
	if rule, ok := matchDeny(command, p.extraDeny); ok {
		v.Decision = PolicyDeny
		v.Rule = rule
		return v
	}

	segments := splitShellCommand(harmlessRedirect.ReplaceAllString(command, " "))
	if len(segments) == 0 {
		// Nothing but separators; bash would reject it, and so do we.
		v.Decision = PolicyAsk
		v.Rule = "no command found"
		v.Summary = "I was asked to run a shell command I could not parse. Should I go ahead?"
		return v
	}
	worst := PolicyAllow
	worstRule := ""
	worstReason := ""
	// preApproved is set by any segment a user-granted pattern allowed, and
	// survives to the verdict only if nothing asks. One remembered segment in
	// a line that runs unprompted is enough to owe the user an audit row: the
	// row names the rule, and the rule is the one the user granted.
	preApproved := false
	preApprovedPattern := ""
	for _, seg := range segments {
		if rule, ok := matchDeny(seg, p.extraDeny); ok {
			v.Decision = PolicyDeny
			v.Rule = rule
			return v
		}
		if mode == PolicyAllow {
			continue // trust everything short of a deny pattern
		}
		decision, rule, reason, remembered, pattern := classifySegment(seg, p.extraAllow, grants)
		if decision == PolicyAsk && worst != PolicyAsk {
			worst, worstRule, worstReason = PolicyAsk, rule, reason
		}
		if worst == PolicyAllow && worstRule == "" {
			worstRule = rule
		}
		if remembered && !preApproved {
			preApproved, preApprovedPattern = true, pattern
			if worst == PolicyAllow {
				// Name the granted rule rather than whichever segment came
				// first: "it ran because of a rule you added" is the fact the
				// row exists to carry.
				worstRule = rule
			}
		}
	}
	if mode == PolicyAllow {
		v.Decision = PolicyAllow
		v.Rule = `tool "shell.run" is set to allow`
		return v
	}
	v.Decision = worst
	v.Rule = worstRule
	if v.PreApproved = preApproved && worst == PolicyAllow; v.PreApproved {
		v.Pattern = preApprovedPattern
	}
	if worst == PolicyAsk {
		v.Summary = fmt.Sprintf("I want to run %q, which %s. Should I go ahead?", spokenCommand(command), worstReason)
	}
	return v
}

// spokenCommand bounds how much of a command is read aloud. The overlay and
// the confirmation event always carry the full text; speech only needs enough
// to identify it.
func spokenCommand(command string) string {
	const maxSpoken = 120
	runes := []rune(command)
	if len(runes) <= maxSpoken {
		return command
	}
	return string(runes[:maxSpoken]) + "…"
}

// splitShellCommand breaks a command into the parts a shell would run:
// segments separated by `;`, `&&`, `||`, `|`, `&`, and newlines, with
// command-substitution bodies (`$(...)`, backticks) surfaced as their own
// segments. Quoting is deliberately not honoured — over-splitting a quoted
// string can only escalate the classification towards ask, never hide a
// risky part inside an allowed one.
func splitShellCommand(command string) []string {
	// Surface substitution bodies by turning their delimiters into
	// separators: `echo $(rm x)` must be judged as `echo` and `rm x`.
	r := strings.NewReplacer("$(", ";", "`", ";", "<(", ";", ">(", ";")
	flattened := r.Replace(command)
	parts := strings.FieldsFunc(flattened, func(c rune) bool {
		return c == ';' || c == '&' || c == '|' || c == '\n' || c == ')'
	})
	segs := make([]string, 0, len(parts))
	for _, part := range parts {
		if s := strings.TrimSpace(part); s != "" {
			segs = append(segs, s)
		}
	}
	return segs
}

// Shipped deny rules: commands whose blast radius is the machine itself and
// which have no plausible voice-assistant use. Everything else destructive
// is ask-tier — the user can approve it out loud. Compiled once.
var denyRules = []struct {
	re   *regexp.Regexp
	rule string
}{
	{regexp.MustCompile(`(^|\s)rm\s+(-\S+\s+)*(/|/\*)(\s|$)`), `deny pattern "rm targeting /"`},
	{regexp.MustCompile(`(^|\s)dd\s[^;|&]*\bof=/dev/`), `deny pattern "dd writing to a device"`},
	{regexp.MustCompile(`>\s*/dev/(sd|hd|vd|nvme|mmcblk|loop|dm-)`), `deny pattern "redirection onto a block device"`},
	{regexp.MustCompile(`:\(\)\s*\{`), `deny pattern "fork bomb"`},
}

func matchDeny(text string, extra [][]string) (string, bool) {
	for _, d := range denyRules {
		if d.re.MatchString(text) {
			return d.rule, true
		}
	}
	for _, words := range extra {
		if matchWordPrefix(text, words) {
			return fmt.Sprintf("configured deny pattern %q", strings.Join(words, " ")), true
		}
	}
	return "", false
}

// riskWords force a confirmation even when a broader allow pattern would
// match — `sudo df -h` must ask no matter what. The list holds command words
// that mutate state, escalate privilege, or hand execution to arbitrary code
// (interpreters, eval, xargs); the words are disjoint from every shipped
// allow pattern's first word so read-only commands stay silent.
var riskWords = map[string]bool{
	"rm": true, "rmdir": true, "unlink": true, "dd": true,
	"sudo": true, "doas": true, "su": true, "pkexec": true,
	"chmod": true, "chown": true, "chgrp": true, "chattr": true,
	"mv": true, "cp": true, "ln": true, "install": true,
	"kill": true, "pkill": true, "killall": true,
	"shutdown": true, "reboot": true, "poweroff": true, "halt": true,
	"truncate": true, "shred": true, "tee": true,
	"mkfs": true, "mkswap": true, "swapoff": true, "wipefs": true, "blkdiscard": true,
	"mount": true, "umount": true, "fdisk": true, "parted": true, "sfdisk": true,
	"useradd": true, "userdel": true, "usermod": true, "passwd": true,
	"crontab": true, "at": true,
	"sed": true, "awk": true, "env": true, "xargs": true,
	"eval": true, "exec": true, "source": true,
	"sh": true, "bash": true, "zsh": true, "fish": true,
	"python": true, "python3": true, "perl": true, "ruby": true, "node": true,
	"nc": true, "ncat": true, "socat": true,
}

// allowPatterns is the shipped read-only allow list: commands (or command
// prefixes) that only observe. Inclusion bar: no flag or subcommand under the
// prefix may write, delete, or execute arbitrary code — which is why `env`,
// `find`, `sed`, and bare `git`/`systemctl` are absent. Anything not matched
// simply asks, so the cost of a conservative list is one spoken question.
var allowPatterns = [][]string{
	{"ls"}, {"pwd"}, {"whoami"}, {"id"}, {"groups"},
	{"uname"}, {"hostname"}, {"hostnamectl", "status"}, {"uptime"}, {"date"}, {"nproc"},
	{"free"}, {"df"}, {"du"}, {"lsblk"}, {"lscpu"}, {"lsusb"}, {"lspci"}, {"lsof"},
	{"ps"}, {"pgrep"}, {"pidof"},
	{"cat"}, {"head"}, {"tail"}, {"wc"}, {"grep"}, {"rg"},
	{"file"}, {"stat"}, {"which"}, {"whereis"}, {"type"},
	{"readlink"}, {"realpath"}, {"basename"}, {"dirname"},
	{"echo"}, {"printf"}, {"printenv"},
	{"tr"}, {"cut"}, {"sort"}, {"uniq"},
	{"md5sum"}, {"sha1sum"}, {"sha256sum"},
	{"git", "status"}, {"git", "log"}, {"git", "diff"}, {"git", "show"},
	{"git", "blame"}, {"git", "describe"}, {"git", "shortlog"}, {"git", "rev-parse"},
	{"docker", "ps"}, {"docker", "images"}, {"docker", "logs"},
	{"docker", "inspect"}, {"docker", "version"}, {"docker", "info"},
	{"systemctl", "status"}, {"systemctl", "show"},
	{"systemctl", "list-units"}, {"systemctl", "list-timers"},
	{"systemctl", "is-active"}, {"systemctl", "is-enabled"},
	{"journalctl"},
	{"ss"}, {"ping"},
}

// harmlessRedirect strips redirections that write nothing anyone can lose —
// discarding a stream to /dev/null or duplicating one onto another — before
// classification, so `docker ps 2>/dev/null` stays silent while every other
// `>` still forces a confirmation.
var harmlessRedirect = regexp.MustCompile(`[0-9]*>>?(\s*/dev/null|&[0-9])`)

// journalctl is allowed above, but `journalctl --vacuum-*` deletes logs;
// force those back to ask. Risk regexes beat allow patterns by design.
var riskRegexes = []struct {
	re     *regexp.Regexp
	rule   string
	reason string
}{
	{regexp.MustCompile(`>`), `risk pattern ">"`, "writes with output redirection"},
	{regexp.MustCompile(`--vacuum`), `risk pattern "--vacuum"`, "deletes journal data"},
}

// classifySegment judges one simple command. Order matters and is the
// security argument: risk checks beat allow patterns (deny was already
// checked by the caller), and anything unmatched asks.
//
// grants are the conversation-scoped patterns of issue #162. They sit beside
// extraAllow — after every risk check, never before one — because a grant is
// the weakest thing in this function by design: the user said "for now", and
// "for now" must not outrank a rule that says "never".
func classifySegment(seg string, extraAllow, grants [][]string) (decision PolicyDecision, rule, reason string, remembered bool, pattern string) {
	for _, r := range riskRegexes {
		if r.re.MatchString(seg) {
			return PolicyAsk, r.rule, r.reason, false, ""
		}
	}
	if w := commandWord(seg); w != "" {
		// mkfs.ext4, mkfs.vfat, … share mkfs's tier via the prefix check.
		if riskWords[w] || strings.HasPrefix(w, "mkfs") {
			return PolicyAsk, fmt.Sprintf("risky command %q", w), fmt.Sprintf("uses the risky command %q", w), false, ""
		}
	}
	for _, words := range extraAllow {
		if matchWordPrefix(seg, words) {
			joined := strings.Join(words, " ")
			return PolicyAllow, fmt.Sprintf("configured allow pattern %q", joined), "", true, joined
		}
	}
	for _, words := range grants {
		if matchWordPrefix(seg, words) {
			// Named distinctly from a configured pattern so the audit row and
			// the log say which kind of permission ran this: one the user can
			// find in config.toml, or one that dies with the conversation.
			joined := strings.Join(words, " ")
			return PolicyAllow, fmt.Sprintf("conversation allow pattern %q", joined), "", true, joined
		}
	}
	for _, words := range allowPatterns {
		if matchWordPrefix(seg, words) {
			return PolicyAllow, fmt.Sprintf("allow pattern %q", strings.Join(words, " ")), "", false, ""
		}
	}
	return PolicyAsk, "no matching pattern", "is not on my read-only allow list", false, ""
}

var envAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// commandWord returns the word a shell would execute: the first field after
// any leading VAR=value assignments. `FOO=1 rm x` is still an rm.
func commandWord(seg string) string {
	for _, f := range strings.Fields(seg) {
		if envAssignment.MatchString(f) {
			continue
		}
		return f
	}
	return ""
}

// matchWordPrefix reports whether the segment's leading words (after env
// assignments) equal the pattern's words exactly. Word equality, not string
// prefix: pattern "docker ps" matches "docker ps -a" but never "docker psx".
func matchWordPrefix(seg string, pattern []string) bool {
	fields := strings.Fields(seg)
	for len(fields) > 0 && envAssignment.MatchString(fields[0]) {
		fields = fields[1:]
	}
	if len(fields) < len(pattern) {
		return false
	}
	for i, w := range pattern {
		if fields[i] != w {
			return false
		}
	}
	return true
}
