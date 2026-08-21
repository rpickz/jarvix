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
	// run without confirmation.
	ShellAllow []string
	// ShellDeny adds word-prefix patterns that never run, regardless of any
	// confirmation. Deny beats everything.
	ShellDeny []string
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
}

// Policy is the compiled permission gate. All patterns are compiled once at
// construction so a Decide call is a handful of string scans — the gate must
// add no perceptible latency to allow-listed calls.
type Policy struct {
	defaultDecision PolicyDecision
	tools           map[string]PolicyDecision
	extraAllow      [][]string // word-prefix patterns from configuration
	extraDeny       [][]string
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
var builtinToolDefaults = map[string]PolicyDecision{
	"artifact.create": PolicyAllow,
}

// ToolDecision returns the configured tier for a tool: its per-tool entry,
// a built-in default (shell.run classifies with an ask fallback,
// artifact.create is allow), or the policy default. Used by status
// reporting; Decide applies the same resolution.
func (p *Policy) ToolDecision(name string) PolicyDecision {
	if d, ok := p.tools[name]; ok {
		return d
	}
	if name == shellToolName {
		return PolicyAsk
	}
	if d, ok := builtinToolDefaults[name]; ok {
		return d
	}
	return p.defaultDecision
}

const shellToolName = "shell.run"

// Decide classifies one tool call. For shell.run the command is parsed and
// classified daemon-side: a compound command (`;`, `&&`, pipes, command
// substitution) is judged by its riskiest part, and deny beats ask beats
// allow. The model's arguments are the only input — its stated intent is
// never consulted.
func (p *Policy) Decide(call ai.ToolCall) Verdict {
	mode := p.ToolDecision(call.Name)
	if call.Name != shellToolName {
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
			if _, ok := p.tools[call.Name]; ok {
				v.Rule = fmt.Sprintf("tool %q is set to ask", call.Name)
			} else {
				v.Rule = "unknown tool defaults to ask"
			}
			v.Summary = fmt.Sprintf("I want to use the %s tool. Should I go ahead?", call.Name)
		}
		return v
	}
	return p.decideShell(call, mode)
}

func (p *Policy) decideShell(call ai.ToolCall, mode PolicyDecision) Verdict {
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
	for _, seg := range segments {
		if rule, ok := matchDeny(seg, p.extraDeny); ok {
			v.Decision = PolicyDeny
			v.Rule = rule
			return v
		}
		if mode == PolicyAllow {
			continue // trust everything short of a deny pattern
		}
		decision, rule, reason := classifySegment(seg, p.extraAllow)
		if decision == PolicyAsk && worst != PolicyAsk {
			worst, worstRule, worstReason = PolicyAsk, rule, reason
		}
		if worst == PolicyAllow && worstRule == "" {
			worstRule = rule
		}
	}
	if mode == PolicyAllow {
		v.Decision = PolicyAllow
		v.Rule = `tool "shell.run" is set to allow`
		return v
	}
	v.Decision = worst
	v.Rule = worstRule
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
func classifySegment(seg string, extraAllow [][]string) (decision PolicyDecision, rule, reason string) {
	for _, r := range riskRegexes {
		if r.re.MatchString(seg) {
			return PolicyAsk, r.rule, r.reason
		}
	}
	if w := commandWord(seg); w != "" {
		// mkfs.ext4, mkfs.vfat, … share mkfs's tier via the prefix check.
		if riskWords[w] || strings.HasPrefix(w, "mkfs") {
			return PolicyAsk, fmt.Sprintf("risky command %q", w), fmt.Sprintf("uses the risky command %q", w)
		}
	}
	for _, words := range extraAllow {
		if matchWordPrefix(seg, words) {
			return PolicyAllow, fmt.Sprintf("configured allow pattern %q", strings.Join(words, " ")), ""
		}
	}
	for _, words := range allowPatterns {
		if matchWordPrefix(seg, words) {
			return PolicyAllow, fmt.Sprintf("allow pattern %q", strings.Join(words, " ")), ""
		}
	}
	return PolicyAsk, "no matching pattern", "is not on my read-only allow list"
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
