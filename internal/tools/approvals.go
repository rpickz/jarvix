package tools

import (
	"fmt"
	"regexp"
	"strings"
)

// This file is the *proposal* half of remembered approvals (issue #162, ADR
// 0052). It adds no policy mechanism whatsoever: everything here reads the
// classifier in policy.go and answers one question about a confirmation that
// is already pending —
//
//	"is there a narrow word-prefix rule that would have let this run, and is
//	 it a rule a person should ever be offered?"
//
// The answer becomes the third button on the confirmation card. The rule it
// names is appended verbatim to `[tools.policy] shell_allow`, which the
// classifier has consulted since ADR 0014; nothing about segmentation,
// precedence, or the risk tables moves.
//
// Two properties hold this together, and both are structural rather than
// remembered by convention:
//
//   - The pattern is DERIVED, never supplied. A client answers the card with
//     a scope word ("always", "conversation") and nothing else; the daemon
//     recomputes the pattern from the pending confirmation it published. A
//     model that can put text on the screen — and since #147 Jarvix reads
//     window content and AI-session transcripts, so it can be fed text by
//     anyone — therefore has no channel through which to name a rule. It can
//     provoke the card; it cannot choose what the card offers, and it cannot
//     press it.
//   - The refusal matrix below is checked against the *derived pattern*, in
//     both directions. A proposal is refused when it matches a dangerous
//     shape and equally when a dangerous shape sits underneath it — which is
//     what stops a bare multiplexer binary ever being proposed, without a
//     hand-written "never propose a bare binary" list to keep in step.
//
// The residual risk is stated plainly in ADR 0052 rather than engineered
// away: a standing allow is a standing grant, and the controls that make it
// acceptable are narrowness, this refusal matrix, deny-always-wins, the
// audit row on every pre-approved run, and one-word revocation.

// maxPatternWords caps how deep a proposed prefix goes.
//
// Three is the number that covers the shapes people actually repeat —
// `docker compose ps`, `kubectl get pods` — and stops one word short of
// carrying an argument. A fourth word is almost always the first thing that
// varies between two runs of the same intent, which is precisely the
// nuisance this feature exists to remove: a rule that has to be re-approved
// for every variant is not a memory, it is a longer question.
const maxPatternWords = 3

// maxPatternWordLen bounds a single word of a proposed pattern. A token
// longer than this is a hash, an id, or a base64 blob wearing a letter as its
// first character — never a subcommand — and baking one into a standing rule
// would produce a pattern that matches exactly once and then sits in the
// user's configuration forever, unreadable.
const maxPatternWordLen = 24

// fixedWordPattern is what a word has to look like to be considered part of a
// command's *name* rather than one of its arguments: a letter, then letters,
// digits, underscores, pluses and dashes.
//
// The leading letter is doing real work. It rejects `5` in `timeout 5 rm -rf
// ~` (a bare number is a parameter, never a subcommand) and it rejects `-a`
// and `--format` because those start with a dash. Everything containing `/`,
// `=`, `.`, `:`, `$`, `*`, `~`, a quote or a brace falls out too, which is
// the whole family of paths, URLs, globs, assignments and substitutions —
// exactly the words that differ between two runs of one intent.
var fixedWordPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+-]*$`)

// fixedWord reports whether w may become part of a proposed pattern.
func fixedWord(w string) bool {
	return len(w) <= maxPatternWordLen && fixedWordPattern.MatchString(w)
}

// RememberScope is how long an approval stands. Three values because they are
// three different promises: this once, this conversation, and until revoked —
// and the card has to be able to say which one it is making.
type RememberScope string

// The scopes. RememberNone is the ordinary approve-once, which remains the
// card's primary action: a standing grant must always read as the deliberate
// choice it is, never as the fast path.
const (
	RememberNone         RememberScope = ""
	RememberConversation RememberScope = "conversation"
	RememberAlways       RememberScope = "always"
)

// ValidRememberScope reports whether s names a scope. Used at the IPC edge so
// an unknown word is a rejected request rather than a silently-downgraded
// one — a client asking for something this daemon does not understand must
// not be answered with a weaker grant than it asked for, nor a stronger.
func ValidRememberScope(s string) bool {
	switch RememberScope(s) {
	case RememberNone, RememberConversation, RememberAlways:
		return true
	}
	return false
}

// RememberOffer is what the confirmation card may offer alongside approve and
// reject: either a named pattern, or a refusal with the sentence that
// explains it. There is no third state — a card either shows the remember
// control with its exact rule on it, or shows one line saying why it cannot.
type RememberOffer struct {
	// Offered is whether the remember control appears at all.
	Offered bool
	// Pattern is the rule that would be appended to
	// `[tools.policy] shell_allow`, verbatim, shown on the card before the
	// user commits. Never a generalisation the user did not see: the string
	// here and the string written to config.toml are the same string.
	Pattern string
	// Segment is the part of the command the pattern was derived from. Equal
	// to the whole command for a simple one; for a compound it is the single
	// segment that required the confirmation, which is the only part a rule
	// may ever be built from.
	Segment string
	// Reason is one short sentence saying why remembering is not offered, set
	// only when Offered is false. Written to be shown on the card and read
	// aloud unchanged — a refusal the user cannot understand is a refusal
	// they will route around.
	Reason string
}

// refuse builds an unoffered RememberOffer.
func refuse(reason string) RememberOffer { return RememberOffer{Reason: reason} }

// RememberOfferFor answers whether the pending confirmation described by v
// may be turned into a standing rule, and if so, with exactly which pattern.
//
// It re-derives everything from v.Command — the command the gate parsed and
// the card displayed — rather than trusting any part of the model's tool
// arguments or a client's request. The segmentation, the deny check and the
// per-segment classification are the classifier's own functions, called in
// the classifier's own order, so a shape that this says is rememberable is a
// shape that the running policy really will allow once the rule exists.
func (p *Policy) RememberOfferFor(v Verdict) RememberOffer {
	if v.Decision != PolicyAsk {
		// Nothing is waiting on a rule. Callers do not reach this in practice
		// (the card only exists for the ask tier); answering honestly rather
		// than panicking keeps a future caller's mistake cheap.
		return refuse("that command is not waiting on a rule.")
	}
	if !rememberableIdentity(v.Tool) {
		// The always-ask floor, restated where the offer is made rather than
		// re-derived: `shell_allow` is a list of *shell command* prefixes, so
		// it has nothing to say about typing, scripts, config writes or any
		// other identity. A tool whose tier is a floor (script.run,
		// config.write_entry, the typing tools) therefore cannot be reached
		// by this feature at all — not "denied", but structurally unable to
		// be named by the only vocabulary a remembered rule has.
		return refuse(fmt.Sprintf(
			"only shell commands can be remembered, and this is the %s tool.", v.Tool))
	}
	command := strings.TrimSpace(v.Command)
	if command == "" {
		return refuse("I could not read the command, so there is nothing to remember.")
	}
	// Deny first, against the raw command, exactly as decideShell does it:
	// splitting must never be able to defeat a deny rule, and it must not be
	// able to defeat this refusal either.
	if rule, denied := matchDeny(command, p.extraDeny); denied {
		return refuse(deniedReason(rule))
	}
	segments := splitShellCommand(harmlessRedirect.ReplaceAllString(command, " "))
	asking := make([]string, 0, len(segments))
	for _, seg := range segments {
		if rule, denied := matchDeny(seg, p.extraDeny); denied {
			return refuse(deniedReason(rule))
		}
		if decision, _, _, _, _ := classifySegment(seg, p.extraAllow, nil); decision == PolicyAsk {
			asking = append(asking, seg)
		}
	}
	switch len(asking) {
	case 0:
		return refuse("nothing in that command is waiting on a rule.")
	case 1:
	default:
		// Partial memory of a compound command is a trap: a rule covering one
		// segment would silence that segment forever while the others carry
		// on asking, so a later `A; B` looks half-familiar and the user
		// approves it on the strength of the half they recognise. Two
		// segments needing approval means the line has two decisions in it,
		// and a single button cannot honestly represent two decisions.
		return refuse(fmt.Sprintf(
			"%d parts of that command need approval, and I only remember one command at a time.",
			len(asking)))
	}
	return p.proposeFor(asking[0])
}

// deniedReason words a refusal caused by a deny rule. Deny beats everything,
// including this: a remembered allow can never resurrect a denied command,
// and offering the button would advertise otherwise.
func deniedReason(rule string) string {
	return fmt.Sprintf("that command is refused by %s, and a deny rule always wins.", rule)
}

// rememberableIdentity is the set of gate identities whose confirmations are
// about a shell command string — the only thing `shell_allow` can describe.
//
// intent.run is here beside shell.run because a user-defined intent is a
// shell command facing the very same classifier (ADR 0017), and the nuisance
// the feature exists to remove is identical from either side. The
// consequence is real and stated on the card by showing the pattern: the two
// identities share one `shell_allow` list, so a rule remembered from an
// intent's card also stops the model being asked about that prefix. ADR 0052
// argues why one shared list is nonetheless right — two lists would mean two
// classifiers' worth of precedence to reason about, and the narrowness of the
// pattern, not the identity that produced it, is what bounds the grant.
func rememberableIdentity(tool string) bool {
	return tool == shellToolName || tool == IntentToolName
}

// proposeFor builds the narrowest useful pattern for one simple command
// segment, or refuses.
//
// "Narrowest useful" is: the leading words that name the command, stopping at
// the first word that looks like an argument, capped at maxPatternWords.
// Worked through, on the shapes from the user's own journal —
//
//	docker ps --format '{{.Image}}'  ->  docker ps
//	docker ps                        ->  docker ps      (one rule covers both)
//	ps aux --sort -%cpu              ->  ps aux
//	ps aux --sort=-%cpu              ->  ps aux         (one rule covers both)
//	xdg-open https://example.com     ->  xdg-open
//	xdg-open ~/notes.md              ->  xdg-open       (one rule covers all three)
//	timeout 5 make deploy            ->  refused        (timeout runs anything)
//	find . -name '*.go'              ->  refused        (find -delete, -exec)
//	docker run -v /:/host alpine     ->  refused        (docker run is a shell)
//	./deploy.sh                      ->  refused        (the file can change)
func (p *Policy) proposeFor(seg string) RememberOffer {
	fields := strings.Fields(seg)
	for len(fields) > 0 && envAssignment.MatchString(fields[0]) {
		// `FOO=1 docker ps` is a docker ps. The assignment is dropped from
		// the pattern rather than baked into it because matchWordPrefix skips
		// leading assignments too — a rule containing one would never match
		// anything.
		fields = fields[1:]
	}
	if len(fields) == 0 {
		return refuse("I could not read a command name there, so there is nothing to remember.")
	}
	head := fields[0]
	// The refusal matrix runs BEFORE the shape check, so a destructive
	// command is always refused for being destructive rather than for
	// whatever its spelling happens to trip. `mkfs.ext4` is the case that
	// proves it: the dot would otherwise get it refused as "a path", which is
	// true of the string and irrelevant about the command.
	if reason, blocked := unrememberableWord(head); blocked {
		return refuse(reason)
	}
	if !fixedWord(head) {
		// A path-invoked script (`./deploy.sh`, `/opt/bin/thing`) is the
		// important case, and the reason is not shape but time: a rule names
		// a path, and the *contents* of that path can be rewritten by anything
		// with write access — including, since #147, by the assistant acting
		// on text someone else wrote. A word-prefix rule cannot express "and
		// the file must still be the one I read", so it is not offered.
		return refuse(fmt.Sprintf(
			"%q is a path rather than a command name, and what a file contains can change after I remember it.", head))
	}
	words := []string{head}
	for _, f := range fields[1:] {
		if len(words) >= maxPatternWords || !fixedWord(f) {
			break
		}
		words = append(words, f)
	}
	if reason, blocked := unrememberableShapeFor(words); blocked {
		return refuse(reason)
	}
	pattern := strings.Join(words, " ")
	// Deny, one last time, against the pattern itself. The command in hand
	// passed the deny check above, but the *rule* is broader than the command:
	// a deny of `docker exec` must stop a proposal of `docker` even though the
	// command being confirmed was `docker version`. Both directions, for the
	// reason unrememberableShapeFor checks both.
	for _, deny := range p.extraDeny {
		if prefixOf(words, deny) || prefixOf(deny, words) {
			return refuse(deniedReason(fmt.Sprintf("configured deny pattern %q", strings.Join(deny, " "))))
		}
	}
	for _, allow := range p.extraAllow {
		if prefixOf(allow, words) {
			// Already covered by a standing rule. Reaching here means the
			// segment asked for some other reason (a risk regex, say), which
			// a second copy of the same pattern would not change.
			return refuse(fmt.Sprintf(
				"%q is already on your allow list, and something else about that command is what asks.",
				strings.Join(allow, " ")))
		}
	}
	return RememberOffer{Offered: true, Pattern: pattern, Segment: seg}
}

// prefixOf reports whether a is a prefix of b, word for word (equal counts as
// a prefix). This is matchWordPrefix's comparison lifted to two patterns
// rather than a pattern and a command, which is what lets the refusal matrix
// be checked in both directions.
func prefixOf(a, b []string) bool {
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

// ---------------------------------------------------------- refusal matrix
//
// The refusal matrix is the reason this feature is acceptable at all, so it
// lives in exactly one place and is documented shape by shape.
//
// Its central limitation, stated once here because every entry below is a
// consequence of it: A WORD-PREFIX PATTERN CANNOT EXPRESS A FLAG EXCLUSION.
// The classifier's vocabulary is "these leading words, then anything", and
// there is no way to write "`find` but not `-delete`", "`git push` but not
// `--force`", or "`sh` but not `-c`". That vocabulary was chosen (ADR 0014)
// because a person can read a word-prefix at a glance and know what it
// covers, and it is kept (issue #162 puts regex and glob out of scope) for
// the same reason. The price is that a binary whose destructive behaviour
// lives in its *flags* rather than its *name* can never be safely remembered,
// and the honest response to that is to refuse rather than to pretend.
//
// Three groups, three different arguments:
//
//  1. riskWords (policy.go) — commands that mutate state, escalate privilege,
//     or hand execution to arbitrary code. These are refused for a reason
//     stronger than judgement: classifySegment checks riskWords BEFORE the
//     allow patterns, so a rule naming one would be INERT. Offering it would
//     be offering a button that silently does nothing, which is worse than
//     the question it claims to remove.
//  2. unrememberableBinaries — binaries a rule really would silence, whose
//     flags or arguments reach past what their name suggests. Each carries
//     its own clause, because "why not?" deserves a specific answer.
//  3. unrememberableShapes — word prefixes rather than bare names, for the
//     multiplexers whose subcommands differ wildly in danger. Checked in both
//     directions, so `docker ps` is offered while `docker` and `docker run`
//     are not, with no separate rule about bare binaries to keep in step.

// unrememberableBinaries maps a command name to the clause explaining why a
// standing rule for it is never offered. Ordered here by argument, not
// alphabetically, because the groupings are the reasoning.
var unrememberableBinaries = map[string]string{
	// Destructive behaviour hidden in flags — the matrix's founding cases,
	// named in issue #162's acceptance criteria.
	"find":       "it carries -delete and -exec, and a word-prefix rule cannot exclude a flag",
	"git":        "`push --force` and `reset --hard` sit under the same name as `status`, and a word-prefix rule cannot exclude a flag",
	"systemctl":  "`stop`, `disable` and `mask` change what this machine does at boot",
	"journalctl": "`--vacuum-*` deletes logs that are the audit trail for everything else here",

	// Commands that run *another* command given as an argument. These are the
	// subtlest hole in the whole gate: the classifier judges the leading word,
	// so `timeout 5 rm -rf ~` is classified as a `timeout`, and a remembered
	// `timeout` would wave the `rm` straight through. Refusing the wrappers is
	// what keeps the risk-word list meaningful.
	"nohup":       "it runs whatever command follows it, so a rule for it is a rule for everything",
	"setsid":      "it runs whatever command follows it, so a rule for it is a rule for everything",
	"timeout":     "it runs whatever command follows it, so a rule for it is a rule for everything",
	"nice":        "it runs whatever command follows it, so a rule for it is a rule for everything",
	"ionice":      "it runs whatever command follows it, so a rule for it is a rule for everything",
	"chrt":        "it runs whatever command follows it, so a rule for it is a rule for everything",
	"taskset":     "it runs whatever command follows it, so a rule for it is a rule for everything",
	"stdbuf":      "it runs whatever command follows it, so a rule for it is a rule for everything",
	"flock":       "it runs whatever command follows it, so a rule for it is a rule for everything",
	"watch":       "it runs whatever command follows it, over and over, unattended",
	"runuser":     "it runs another command as another user",
	"systemd-run": "it runs another command as a service, outliving the session that started it",
	"strace":      "it starts and controls another process",
	"ltrace":      "it starts and controls another process",
	"gdb":         "it starts and controls another process",

	// Reaches another machine, or brings code from one.
	"ssh":   "it runs commands on another machine, where nothing here can classify them",
	"scp":   "it copies files to and from other machines",
	"sftp":  "it copies files to and from other machines",
	"rsync": "it can overwrite or delete on either side with `--delete`",
	"curl":  "`-o` and `--upload-file` write files and send them, and a word-prefix rule cannot exclude a flag",
	"wget":  "`-O` writes wherever it is pointed, and a word-prefix rule cannot exclude a flag",

	// Build and package tooling: every one of these runs code written by
	// somebody else, from a manifest in the working directory, under a
	// subcommand that sits next to the harmless ones.
	"make":      "a target runs whatever the Makefile says, and the Makefile is not something I can read into a rule",
	"npm":       "`run` and `install` execute package scripts fetched from the internet",
	"npx":       "it downloads and runs a package",
	"pnpm":      "`run` and `install` execute package scripts fetched from the internet",
	"yarn":      "`run` and `install` execute package scripts fetched from the internet",
	"pip":       "`install` runs setup code from the package it fetches",
	"pip3":      "`install` runs setup code from the package it fetches",
	"pipx":      "it downloads and runs a package",
	"uv":        "`run` and `pip install` fetch and execute code",
	"poetry":    "`run` and `install` execute project scripts",
	"gem":       "`install` runs extension builds from the package it fetches",
	"bundle":    "`exec` runs whatever the project's Gemfile points at",
	"cargo":     "`run`, `build` and `install` compile and execute code from the project",
	"go":        "`run`, `generate` and `test` compile and execute code from the project",
	"dotnet":    "`run` executes the project",
	"mvn":       "a phase runs whatever plugins the project declares",
	"gradle":    "a task runs whatever the build script says",
	"composer":  "`install` and `run-script` execute code from the package it fetches",
	"deno":      "it runs a script fetched from wherever it is pointed",
	"bun":       "it runs a script fetched from wherever it is pointed",
	"nix":       "`run` and `shell` fetch and execute code",
	"nix-shell": "it fetches and executes code, and drops into a shell besides",

	// System package managers. Most need root — which `sudo` already refuses —
	// but not all, and installing software is not something a standing rule
	// should ever cover.
	"apt":          "it installs software on this machine",
	"apt-get":      "it installs software on this machine",
	"dnf":          "it installs software on this machine",
	"yum":          "it installs software on this machine",
	"pacman":       "it installs software on this machine",
	"zypper":       "it installs software on this machine",
	"apk":          "it installs software on this machine",
	"snap":         "it installs software on this machine",
	"flatpak":      "it installs and runs software on this machine",
	"brew":         "it installs software on this machine",
	"pkexec":       "it escalates privilege",
	"loginctl":     "it can end or lock this login session",
	"hostnamectl":  "`set-hostname` renames this machine",
	"timedatectl":  "it changes this machine's clock, which every log and every reminder is measured against",
	"nmcli":        "it reconfigures this machine's networking",
	"iptables":     "it reconfigures this machine's firewall",
	"nft":          "it reconfigures this machine's firewall",
	"ufw":          "it reconfigures this machine's firewall",
	"firewall-cmd": "it reconfigures this machine's firewall",
	"gsettings":    "it rewrites desktop settings that decide what runs at login",
	"dconf":        "it rewrites desktop settings that decide what runs at login",
	"dbus-send":    "it can ask any desktop service to do anything, including start programs",
	"gdbus":        "it can ask any desktop service to do anything, including start programs",
	"busctl":       "it can ask any system service to do anything, including start programs",

	// Synthetic input. typing.type_text and typing.press_key are never-silent
	// by policy (see neverSilent in policy.go) precisely because the keys land
	// wherever focus happens to be and neither the model nor the user can see
	// where that is in advance. A shell rule that reached the same capability
	// through a different binary would be a back door around a floor this
	// project deliberately built, so it is closed here explicitly.
	"xdotool":  "it types and clicks wherever the focus happens to be, which is the one thing I never do silently",
	"ydotool":  "it types and clicks wherever the focus happens to be, which is the one thing I never do silently",
	"wtype":    "it types wherever the focus happens to be, which is the one thing I never do silently",
	"wl-paste": "it replays whatever is on the clipboard into whatever has focus",

	// Jarvix itself. Without this line, one remembered rule would hand the
	// assistant its own command line — `jarvix config set`, `jarvix approvals
	// forget`, `jarvix confirm` — and the gate would be reachable from inside
	// the thing it gates. #109's exclusion wall keeps the assistant's config
	// tools away from [tools]; this keeps the shell away from the same door.
	"jarvix":  "that is my own command line, and I do not get to widen my own permissions",
	"jarvixd": "that is my own daemon, and I do not get to widen my own permissions",
}

// unrememberableShapes are word prefixes rather than bare command names: the
// subcommands of a multiplexer whose blast radius is arbitrary. Checked in
// BOTH directions against a proposed pattern (see unrememberableShapeFor), so
// one table answers two questions —
//
//   - is the proposal itself one of these dangerous shapes? (`docker run`)
//   - would the proposal cover one of them? (`docker`, which covers
//     `docker run`)
//
// The second direction is the whole of "never propose a bare binary whose
// subcommands differ wildly in danger". Deriving it means the rule cannot
// drift out of step with the list it is derived from: adding `podman rm` here
// automatically stops bare `podman` being proposed, with no second edit.
var unrememberableShapes = []struct {
	words  []string
	reason string
}{
	// Docker and Podman: identical surfaces, so the table is written once per
	// verb and mirrored below in init.
	{[]string{"docker", "run"}, "it starts a container that can be given the whole filesystem"},
	{[]string{"docker", "exec"}, "it runs a command inside a container"},
	{[]string{"docker", "rm"}, "it destroys containers"},
	{[]string{"docker", "rmi"}, "it destroys images"},
	{[]string{"docker", "kill"}, "it stops running containers abruptly"},
	{[]string{"docker", "stop"}, "it stops running containers"},
	{[]string{"docker", "restart"}, "it restarts running containers"},
	{[]string{"docker", "start"}, "it starts containers"},
	{[]string{"docker", "build"}, "a Dockerfile runs whatever it likes while building"},
	{[]string{"docker", "push"}, "it publishes an image to a registry"},
	{[]string{"docker", "login"}, "it stores registry credentials"},
	{[]string{"docker", "cp"}, "it copies files into and out of containers"},
	{[]string{"docker", "commit"}, "it turns a container into an image"},
	{[]string{"docker", "load"}, "it imports an image from a file"},
	{[]string{"docker", "import"}, "it imports an image from a file"},
	{[]string{"docker", "update"}, "it changes the resources a running container may use"},
	{[]string{"docker", "system", "prune"}, "it deletes containers, images and volumes in bulk"},
	{[]string{"docker", "image", "prune"}, "it deletes images in bulk"},
	{[]string{"docker", "image", "rm"}, "it destroys images"},
	{[]string{"docker", "container", "prune"}, "it deletes containers in bulk"},
	{[]string{"docker", "container", "rm"}, "it destroys containers"},
	{[]string{"docker", "volume", "prune"}, "it deletes volumes, which is where container data lives"},
	{[]string{"docker", "volume", "rm"}, "it deletes volumes, which is where container data lives"},
	{[]string{"docker", "network", "prune"}, "it deletes networks in bulk"},
	{[]string{"docker", "network", "rm"}, "it deletes networks"},
	{[]string{"docker", "compose", "up"}, "it starts every service the compose file declares"},
	{[]string{"docker", "compose", "down"}, "it stops and removes every service the compose file declares"},
	{[]string{"docker", "compose", "run"}, "it runs a command in a new container"},
	{[]string{"docker", "compose", "exec"}, "it runs a command inside a running service"},
	{[]string{"docker", "compose", "build"}, "a Dockerfile runs whatever it likes while building"},
	{[]string{"docker", "compose", "stop"}, "it stops running services"},
	{[]string{"docker", "compose", "start"}, "it starts services"},
	{[]string{"docker", "compose", "restart"}, "it restarts running services"},
	{[]string{"docker", "compose", "kill"}, "it stops running services abruptly"},
	{[]string{"docker", "compose", "rm"}, "it destroys service containers"},

	// Kubernetes: the same argument, against a cluster instead of a machine.
	{[]string{"kubectl", "delete"}, "it destroys cluster resources"},
	{[]string{"kubectl", "exec"}, "it runs a command inside a pod"},
	{[]string{"kubectl", "run"}, "it starts a pod running whatever image it is given"},
	{[]string{"kubectl", "apply"}, "it changes cluster state from a file"},
	{[]string{"kubectl", "create"}, "it changes cluster state"},
	{[]string{"kubectl", "replace"}, "it changes cluster state"},
	{[]string{"kubectl", "patch"}, "it changes cluster state"},
	{[]string{"kubectl", "edit"}, "it changes cluster state"},
	{[]string{"kubectl", "scale"}, "it changes how many replicas run"},
	{[]string{"kubectl", "drain"}, "it evicts everything from a node"},
	{[]string{"kubectl", "cordon"}, "it changes where the cluster may schedule work"},
	{[]string{"kubectl", "uncordon"}, "it changes where the cluster may schedule work"},
	{[]string{"kubectl", "taint"}, "it changes where the cluster may schedule work"},
	{[]string{"kubectl", "rollout"}, "it restarts or reverts running deployments"},
	{[]string{"kubectl", "port-forward"}, "it opens a tunnel into the cluster"},
	{[]string{"kubectl", "proxy"}, "it opens a tunnel into the cluster"},
	{[]string{"kubectl", "cp"}, "it copies files into and out of pods"},
	{[]string{"kubectl", "attach"}, "it attaches to a running container"},

	// Infrastructure tools, on the same argument again.
	{[]string{"terraform", "apply"}, "it changes real infrastructure"},
	{[]string{"terraform", "destroy"}, "it destroys real infrastructure"},
	{[]string{"helm", "install"}, "it deploys a chart into a cluster"},
	{[]string{"helm", "upgrade"}, "it changes what is deployed in a cluster"},
	{[]string{"helm", "uninstall"}, "it removes what is deployed in a cluster"},
	{[]string{"helm", "rollback"}, "it changes what is deployed in a cluster"},
}

// podmanMirror adds a podman copy of every docker shape. Podman is a
// drop-in replacement for docker's command line, so the two must not be able
// to disagree about what is refusable: mirroring at init keeps one table and
// one set of reasons rather than two that drift.
func init() {
	mirrored := make([]struct {
		words  []string
		reason string
	}, 0, len(unrememberableShapes))
	for _, s := range unrememberableShapes {
		if s.words[0] != "docker" {
			continue
		}
		words := append([]string{"podman"}, s.words[1:]...)
		mirrored = append(mirrored, struct {
			words  []string
			reason string
		}{words, s.reason})
	}
	unrememberableShapes = append(unrememberableShapes, mirrored...)
}

// unrememberableWord reports why a command name may never head a remembered
// rule, or false when it may.
func unrememberableWord(word string) (string, bool) {
	// mkfs.ext4, mkfs.vfat, … share mkfs's tier via the same prefix check
	// classifySegment uses, so the refusal and the classification cannot
	// disagree about what counts as an mkfs.
	if riskWords[word] || strings.HasPrefix(word, "mkfs") {
		return fmt.Sprintf(
			"%q always asks: it can change or destroy things, and an allow rule could never silence it.", word), true
	}
	if clause, ok := unrememberableBinaries[word]; ok {
		return fmt.Sprintf("I can't offer a standing rule for %q — %s.", word, clause), true
	}
	return "", false
}

// unrememberableShapeFor checks a proposed pattern against the shape table in
// both directions and returns the sentence to show, or false.
func unrememberableShapeFor(words []string) (string, bool) {
	for _, shape := range unrememberableShapes {
		switch {
		case prefixOf(shape.words, words):
			// The proposal IS the dangerous shape, or sits inside it.
			return fmt.Sprintf("I can't offer a standing rule for %q — %s.",
				strings.Join(shape.words, " "), shape.reason), true
		case prefixOf(words, shape.words):
			// The proposal is broader than the dangerous shape and would
			// therefore cover it. This is the bare-multiplexer case: a rule
			// for "docker" is a rule for "docker run".
			return fmt.Sprintf("a rule for %q would also cover %q, and %s.",
				strings.Join(words, " "), strings.Join(shape.words, " "), shape.reason), true
		}
	}
	return "", false
}

// CompileGrants turns conversation-scoped pattern strings into the compiled
// word-prefix form Decide takes. Errors are the same ones NewPolicy raises
// for a configured list, so a grant and a configured rule are rejected for
// the same reasons in the same words.
func CompileGrants(patterns []string) ([][]string, error) {
	return compileWordPatterns("conversation approval", patterns)
}
