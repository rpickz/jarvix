package tools

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"unicode"
)

// Properties of the shell classifier and the remembered-approval matrix
// (issue #172).
//
// policy_test.go and approvals_test.go are tables. A table proves the cases
// somebody thought of, and for a classifier over an unbounded input space the
// distance between that and "nothing slips through" is exactly where a
// permission bypass lives. This file states the same components' contracts as
// LAWS — sentences that must hold for every input — and attacks each law with
// generated commands: separators, quoting, substitutions, redirects, unicode,
// absurd lengths. fuzz_test.go points the fuzzer at the same laws.
//
// The generator is seeded from a constant and the seed is printed on every
// failure, so a red run is reproducible from its own output; anything the
// fuzzer finds is committed under testdata/fuzz so it can never come back.
//
// Nothing here reaches the network, the filesystem or a clock. The classifier
// is pure by construction and these laws are the reason it must stay that way.

// propSeed makes every generated corpus in this file identical from run to
// run. A property test that generates a different corpus each time reports a
// failure nobody can reproduce, which in practice means a failure nobody acts
// on.
const propSeed = 20260829

// strictness ranks the three tiers so "at least as strict as" is comparable.
// deny > ask > allow is the gate's own ordering (policy.go).
var strictness = map[PolicyDecision]int{PolicyAllow: 0, PolicyAsk: 1, PolicyDeny: 2}

// --------------------------------------------------------------- generators

// commandHeads are the leading words a generated command may start with: the
// shipped allow list's shapes, the risk words, the wrapper binaries, and words
// that match nothing at all.
var commandHeads = []string{
	"ls", "ls -la /tmp", "pwd", "df -h", "docker ps", "docker ps -a",
	"git status", "git log --oneline", "systemctl status jarvixd",
	"journalctl -u jarvixd", "cat /etc/hostname", "echo hello", "ps aux",
	"rm -rf ./build", "sudo apt update", "dd if=/dev/zero of=./blob",
	"mkfs.ext4 /dev/sdb1", "chmod 777 /etc", "mv a b", "tee /etc/hosts",
	"python3 -c print(1)", "bash -lc id", "eval $CMD", "xargs rm",
	"timeout 5 make deploy", "nohup ./thing", "find . -name x", "curl -o f http://x",
	"kubectl get pods", "helm list", "terraform plan", "npm test",
	"FOO=1 docker ps", "FOO=1 BAR=2 rm -rf /tmp/x", "jarvix approvals list",
	"./deploy.sh", "/opt/bin/thing --go", "zzprobe status", "zzprobe",
	"docker compose ps", "docker run alpine", "podman run alpine",
	"journalctl --vacuum-time=1d", "hostnamectl status",
}

// commandDecorations are the shapes an attacker (or a model, or a transcript)
// might wrap a command in to try to change how it is read.
var commandDecorations = []string{
	"", " > /tmp/out", " >> /tmp/out", " 2>/dev/null", " 2>&1", " > /dev/null",
	" 'quoted string'", " \"double quoted\"", " $(id)", " `id`", " <(cat x)",
	" --flag=value", " -- --", " \\;", " #comment", " \t ", "   ",
	// Unicode whitespace: a no-break space and a line separator.
	// strings.TrimSpace calls these blank and FieldsFunc's predicate does
	// not, so a segment can be trimmed by a rule the splitter never states.
	// They belong in the corpus for the reason the sentencer fuzzes
	// multi-byte runes: the seam is where the two definitions disagree.
	" \u00a0\u2028", "\u00a0", "\u2028rm -rf /",
	" émoji🎉", " " + strings.Repeat("x", 300),
}

// separators are every character sequence splitShellCommand must break on,
// plus the degenerate runs a shell would reject and the classifier must not
// be confused by.
var separators = []string{
	";", "&&", "||", "|", "&", "\n", " ; ", "\t;\t", ";;", "&&&", "|&", "\n\n",
	" && ", " || ", " | ", " & ",
}

// generatedCommands builds the corpus every law in this file is checked
// against: singles, every head against every decoration, and multi-segment
// lines assembled at random from all three vocabularies.
func generatedCommands(rng *rand.Rand) []string {
	var out []string
	out = append(out, commandHeads...)
	for _, head := range commandHeads {
		for _, dec := range commandDecorations {
			out = append(out, head+dec)
		}
	}
	for i := 0; i < 800; i++ {
		parts := 2 + rng.Intn(3)
		var b strings.Builder
		for p := 0; p < parts; p++ {
			if p > 0 {
				b.WriteString(separators[rng.Intn(len(separators))])
			}
			b.WriteString(commandHeads[rng.Intn(len(commandHeads))])
			b.WriteString(commandDecorations[rng.Intn(len(commandDecorations))])
		}
		out = append(out, b.String())
	}
	return dedupe(out)
}

// dedupe keeps the corpus at the size its vocabularies imply. The random half
// of every generator repeats itself heavily, and a property test that spends
// its time re-deciding the same string is a property test people start
// skipping — these run on the fast gate, so they have to stay fast.
func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := in[:0:0]
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// --------------------------------------------------------- splitting is sane

// separatorRunes are the characters splitShellCommand breaks on, and the
// backtick that opens a substitution. A segment holding any of them would mean
// a part of the line was never judged on its own.
const separatorRunes = ";&|\n)`"

// TestNoSegmentStillContainsASeparator is the structural half of the
// segmentation law. A segment that still holds a separator is a segment whose
// tail was classified by whatever its head happened to be — which is the exact
// shape of "ls; rm -rf ~ runs silently".
func TestNoSegmentStillContainsASeparator(t *testing.T) {
	rng := rand.New(rand.NewSource(propSeed)) //nolint:gosec // corpus, not crypto
	for _, command := range generatedCommands(rng) {
		for _, seg := range splitShellCommand(harmlessRedirect.ReplaceAllString(command, " ")) {
			if strings.ContainsAny(seg, separatorRunes) {
				t.Errorf("seed %d: %q produced segment %q, which still holds a separator",
					propSeed, command, seg)
			}
			if seg == "" || seg != strings.TrimSpace(seg) {
				t.Errorf("seed %d: %q produced segment %q, which is blank or untrimmed",
					propSeed, command, seg)
			}
		}
	}
}

// TestSplittingLosesNothingButSeparators is the other half: splitting must be
// a partition, not a filter. If a substring could be dropped, a risky word
// could be dropped with it and the remaining segments would classify clean.
//
// The expected content is computed by an independent character filter rather
// than by calling the splitter again, so a mutation inside splitShellCommand's
// predicate shows up here as a difference rather than as two matching wrongs.
func TestSplittingLosesNothingButSeparators(t *testing.T) {
	rng := rand.New(rand.NewSource(propSeed)) //nolint:gosec // corpus, not crypto
	for _, command := range generatedCommands(rng) {
		flattened := harmlessRedirect.ReplaceAllString(command, " ")
		got := contentOnly(strings.Join(splitShellCommand(flattened), ""))
		want := contentOnly(substitutionOpeners.Replace(flattened))
		if got != want {
			t.Errorf("seed %d: splitting %q changed its content:\n got %q\nwant %q",
				propSeed, command, got, want)
		}
	}
}

// substitutionOpeners mirrors the delimiters splitShellCommand turns into
// separators. It is the one thing the reference filter has to know about the
// implementation, because those delimiters are two characters and the `$` of
// `$(` is consumed with the paren.
var substitutionOpeners = strings.NewReplacer("$(", ";", "`", ";", "<(", ";", ">(", ";")

// contentOnly strips whitespace and every separator, leaving what a shell
// would actually pass to a program.
//
// Whitespace is unicode.IsSpace's definition rather than ASCII's, because the
// splitter's own trim is strings.TrimSpace: a no-break space at a segment's
// edge is removed by the implementation, so a reference that kept it would
// report a content change the splitter never made. (This is the same trap the
// sentencer's stripSpace documents from the opposite side — there the contract
// is byte-exact, so the reference has to be too.)
func contentOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
		case strings.ContainsRune(separatorRunes, r):
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ------------------------------------------------- the classification is sane

// TestNoCompoundIsEverClassifiedFromOneSegment is the security law the ticket
// names first: a line's verdict is at least as strict as the verdict any part
// of it would get on its own. `ls; rm -rf ~` cannot inherit `ls`'s silence.
//
// It is checked in the direction that matters. The reverse — a whole no
// STRICTER than its parts — is deliberately not asserted: the deny rules run
// against the raw line as well as each segment (a fork bomb is nothing but
// separators), so the whole is allowed to be stricter than its pieces.
func TestNoCompoundIsEverClassifiedFromOneSegment(t *testing.T) {
	p := mustPolicy(t, PolicyConfig{
		ShellAllow: []string{"zzprobe status"},
		ShellDeny:  []string{"httpie post"},
	})
	rng := rand.New(rand.NewSource(propSeed)) //nolint:gosec // corpus, not crypto
	for _, command := range generatedCommands(rng) {
		whole := p.Decide(shellCall(command))
		for _, seg := range splitShellCommand(harmlessRedirect.ReplaceAllString(command, " ")) {
			part := p.Decide(shellCall(seg))
			if strictness[whole.Decision] < strictness[part.Decision] {
				t.Errorf("seed %d: %q was judged %s, but its segment %q alone is %s (%s)",
					propSeed, command, whole.Decision, seg, part.Decision, part.Rule)
			}
		}
	}
}

// TestAnAllowVerdictMeansEverySegmentEarnedIt restates the same law from the
// other end, and is the one a mutation inside classifySegment's ordering
// trips: silence is only ever the conclusion of every part being harmless.
func TestAnAllowVerdictMeansEverySegmentEarnedIt(t *testing.T) {
	p := mustPolicy(t, PolicyConfig{
		ShellAllow: []string{"zzprobe status"},
		ShellDeny:  []string{"httpie post"},
	})
	rng := rand.New(rand.NewSource(propSeed)) //nolint:gosec // corpus, not crypto
	for _, command := range generatedCommands(rng) {
		v := p.Decide(shellCall(command))
		if v.Decision != PolicyAllow {
			continue
		}
		if rule, denied := matchDeny(command, p.extraDeny); denied {
			t.Errorf("seed %d: %q ran silently despite %s", propSeed, command, rule)
		}
		for _, seg := range splitShellCommand(harmlessRedirect.ReplaceAllString(command, " ")) {
			if rule, denied := matchDeny(seg, p.extraDeny); denied {
				t.Errorf("seed %d: %q ran silently despite segment %q matching %s",
					propSeed, command, seg, rule)
			}
			if w := commandWord(seg); riskWords[w] || strings.HasPrefix(w, "mkfs") {
				t.Errorf("seed %d: %q ran silently with segment %q, whose command is the risk word %q",
					propSeed, command, seg, w)
			}
			for _, r := range riskRegexes {
				if r.re.MatchString(seg) {
					t.Errorf("seed %d: %q ran silently with segment %q, which matches %s",
						propSeed, command, seg, r.rule)
				}
			}
		}
	}
}

// TestAVerdictSaysWhyItAsked: an ask with no sentence is a confirmation card
// with nothing on it, and a summary on anything else is a question nobody was
// asked. Cheap to state, and it is the invariant that stops a refactor
// dropping the daemon-generated summary back to the model's own words.
func TestAVerdictSaysWhyItAsked(t *testing.T) {
	p := mustPolicy(t, PolicyConfig{ShellDeny: []string{"httpie post"}})
	rng := rand.New(rand.NewSource(propSeed)) //nolint:gosec // corpus, not crypto
	for _, command := range generatedCommands(rng) {
		v := p.Decide(shellCall(command))
		if (v.Summary != "") != (v.Decision == PolicyAsk) {
			t.Errorf("seed %d: %q was %s with summary %q", propSeed, command, v.Decision, v.Summary)
		}
		if v.Rule == "" {
			t.Errorf("seed %d: %q was %s with no rule named", propSeed, command, v.Decision)
		}
		if v.PreApproved && v.Decision != PolicyAllow {
			t.Errorf("seed %d: %q was %s and still marked pre-approved", propSeed, command, v.Decision)
		}
		if v.PreApproved && v.Pattern == "" {
			t.Errorf("seed %d: %q was pre-approved by an unnamed pattern", propSeed, command)
		}
	}
}

// TestClassificationIsAPureFunctionOfTheCommand. The gate is consulted twice
// for one call in places (the verdict, then the card), and two different
// answers to the same question would be a confirmation about a command other
// than the one that runs.
func TestClassificationIsAPureFunctionOfTheCommand(t *testing.T) {
	p := mustPolicy(t, PolicyConfig{ShellAllow: []string{"zzprobe status"}})
	rng := rand.New(rand.NewSource(propSeed)) //nolint:gosec // corpus, not crypto
	for _, command := range generatedCommands(rng) {
		if a, b := p.Decide(shellCall(command)), p.Decide(shellCall(command)); a != b {
			t.Errorf("seed %d: %q was judged %+v then %+v", propSeed, command, a, b)
		}
	}
}

// ----------------------------------------------- the refusal matrix is a law

// TestNoAcceptedPatternCanCoverACommandTheGateRefuses is the security
// invariant of the approvals matrix, stated as a law rather than as the list
// of examples approvals_test.go carries.
//
// Two halves, because "cannot be a prefix of a refusal" means two things:
//
//   - statically, an accepted pattern's own words are never a risk word, an
//     mkfs, a refused binary, a refused shape, or prefix-related to a
//     configured deny rule;
//   - dynamically, granting it never changes a refusal into permission. That
//     is the half worth generating input for, because it is the one that
//     depends on the ORDER of the checks inside classifySegment rather than on
//     the tables.
func TestNoAcceptedPatternCanCoverACommandTheGateRefuses(t *testing.T) {
	deny := []string{"httpie post", "zzdanger"}
	base := mustPolicy(t, PolicyConfig{ShellDeny: deny})
	rng := rand.New(rand.NewSource(propSeed)) //nolint:gosec // corpus, not crypto
	for _, pattern := range generatedPatterns(rng) {
		offer := base.VetAllowPattern(pattern)
		if !offer.Offered {
			continue
		}
		words := strings.Fields(offer.Pattern)
		if riskWords[words[0]] || strings.HasPrefix(words[0], "mkfs") {
			t.Errorf("seed %d: %q was accepted and heads a risk word", propSeed, offer.Pattern)
		}
		if _, blocked := unrememberableBinaries[words[0]]; blocked {
			t.Errorf("seed %d: %q was accepted and heads a refused binary", propSeed, offer.Pattern)
		}
		if reason, blocked := unrememberableShapeFor(words); blocked {
			t.Errorf("seed %d: %q was accepted and covers a refused shape: %s",
				propSeed, offer.Pattern, reason)
		}
		for _, d := range base.extraDeny {
			if prefixOf(words, d) || prefixOf(d, words) {
				t.Errorf("seed %d: %q was accepted beside the deny rule %q",
					propSeed, offer.Pattern, strings.Join(d, " "))
			}
		}

		granted := mustPolicy(t, PolicyConfig{ShellAllow: []string{offer.Pattern}, ShellDeny: deny})
		for _, command := range commandsUnder(offer.Pattern) {
			before := base.Decide(shellCall(command))
			after := granted.Decide(shellCall(command))
			if before.Decision == PolicyDeny && after.Decision != PolicyDeny {
				t.Errorf("seed %d: granting %q turned the denied %q into %s",
					propSeed, offer.Pattern, command, after.Decision)
			}
			if after.Decision != PolicyAllow {
				continue
			}
			for _, seg := range splitShellCommand(harmlessRedirect.ReplaceAllString(command, " ")) {
				if rule, denied := matchDeny(seg, granted.extraDeny); denied {
					t.Errorf("seed %d: granting %q silenced %q, whose segment %q matches %s",
						propSeed, offer.Pattern, command, seg, rule)
				}
				if w := commandWord(seg); riskWords[w] || strings.HasPrefix(w, "mkfs") {
					t.Errorf("seed %d: granting %q silenced %q, whose segment %q runs the risk word %q",
						propSeed, offer.Pattern, command, seg, w)
				}
			}
		}
	}
}

// commandsUnder builds the commands a granted pattern would cover, including
// the ones that try to smuggle something past it: a risky tail on the same
// line, a separator, a substitution, a redirect onto a device.
func commandsUnder(pattern string) []string {
	tails := []string{
		"", " -a", " x y z",
		" rm -rf /", " > /dev/sda", " 2>/dev/null",
		" $(rm -rf /)", " ; rm -rf ~", " && sudo reboot",
		" | sh", "\nsudo poweroff", " ; httpie post /x", " zzdanger",
	}
	out := make([]string, 0, len(tails)*2)
	for _, tail := range tails {
		out = append(out, pattern+tail, "FOO=1 "+pattern+tail)
	}
	return out
}

// patternVocabulary is the word supply generatedPatterns draws from: refused
// heads, offerable heads, multiplexer verbs, and words that are not command
// words at all.
var patternVocabulary = []string{
	"ls", "docker", "podman", "kubectl", "helm", "terraform", "git", "npm",
	"xdg-open", "zzprobe", "systemctl", "journalctl", "hostnamectl",
	"rm", "sudo", "mkfs.ext4", "timeout", "find", "curl", "wget", "make",
	"jarvix", "jarvixd", "xdotool", "ssh", "httpie", "zzdanger",
	"ps", "run", "exec", "compose", "up", "down", "get", "pods", "delete",
	"status", "install", "apply", "system", "prune", "image", "rmi", "version",
	"--flag", "-a", "5", "./x", "/opt/bin/x", "FOO=1", "a=b", "x/y", "'q'",
	strings.Repeat("z", 40),
}

// generatedPatterns enumerates one-, two- and three-word candidates from that
// vocabulary. Three words is maxPatternWords, so this is the whole space the
// derived route can ever propose, plus a great deal it cannot.
func generatedPatterns(rng *rand.Rand) []string {
	var out []string
	for _, a := range patternVocabulary {
		out = append(out, a)
		for _, b := range patternVocabulary {
			out = append(out, a+" "+b)
		}
	}
	for i := 0; i < 500; i++ {
		n := 1 + rng.Intn(4)
		words := make([]string, 0, n)
		for j := 0; j < n; j++ {
			words = append(words, patternVocabulary[rng.Intn(len(patternVocabulary))])
		}
		out = append(out, strings.Join(words, " "))
	}
	return dedupe(out)
}

// TestTheDerivedAndTypedRoutesAgreeOnGeneratedShapes extends what #164 pinned
// over the policy's own tables to generated input: the card's derived pattern
// and a hand-typed one terminate at the same judgePattern, so a shape added to
// the matrix is refused on both routes or on neither, with the same sentence.
//
// The commands are built so the derived route always REACHES the matrix. A
// redirect is appended because it makes any command ask (riskRegexes beat the
// allow patterns) and because `>` is not a command word, so the pattern walk
// stops exactly where the generated words end. Without it, `ls` classifies as
// allow and the card answers "nothing in that command is waiting on a rule" —
// a true statement about a command, which is not a statement about a rule and
// so has no typed counterpart to compare with.
func TestTheDerivedAndTypedRoutesAgreeOnGeneratedShapes(t *testing.T) {
	p := mustPolicy(t, PolicyConfig{ShellDeny: []string{"httpie post", "zzdanger"}})
	rng := rand.New(rand.NewSource(propSeed)) //nolint:gosec // corpus, not crypto
	for _, pattern := range generatedPatterns(rng) {
		words := strings.Fields(pattern)
		if len(words) == 0 || len(words) > maxPatternWords {
			continue // the cap is a documented divergence; see the test below
		}
		if !allFixedWords(words) {
			continue // truncation is a documented divergence; see the test below
		}
		command := pattern + " > /tmp/probe"
		derived := p.RememberOfferFor(Verdict{
			Decision: PolicyAsk, Tool: shellToolName, Command: command,
		})
		typed := p.VetAllowPattern(pattern)
		switch {
		case derived.Offered != typed.Offered:
			t.Errorf("seed %d: %q — the card %s, the form %s",
				propSeed, pattern, offerVerb(derived), offerVerb(typed))
		case derived.Offered && derived.Pattern != typed.Pattern:
			t.Errorf("seed %d: %q — the card offers %q, the form offers %q",
				propSeed, pattern, derived.Pattern, typed.Pattern)
		case !derived.Offered && derived.Reason != typed.Reason:
			t.Errorf("seed %d: %q — the two routes refuse it differently:\n card: %s\n form: %s",
				propSeed, pattern, derived.Reason, typed.Reason)
		}
	}
}

// allFixedWords reports whether every word could be part of a pattern.
func allFixedWords(words []string) bool {
	for _, w := range words {
		if !fixedWord(w) || envAssignment.MatchString(w) {
			return false
		}
	}
	return true
}

func offerVerb(o RememberOffer) string {
	if o.Offered {
		return fmt.Sprintf("offers %q", o.Pattern)
	}
	return fmt.Sprintf("refuses (%s)", o.Reason)
}

// TestTheRoutesDivergeOnlyWhereTheyAreDocumentedTo pins the two differences
// VetAllowPattern's comment claims, so they stay deliberate. Both make the
// typed route STRICTER, which is the direction that is safe: a person typing a
// rule gets refused where the card would have summarised.
//
//  1. a word that is not a command word is refused rather than truncated away;
//  2. there is no maxPatternWords cap on a typed rule, because a longer prefix
//     a person typed is strictly narrower and refusing it would be refusing
//     the careful answer.
func TestTheRoutesDivergeOnlyWhereTheyAreDocumentedTo(t *testing.T) {
	p := mustPolicy(t, PolicyConfig{})
	// (1) The card truncates at the first argument; the form refuses.
	for _, pattern := range []string{
		"docker ps --format", "kubectl get -o", "xdg-open ~/notes.md", "zzprobe status -v",
	} {
		typed := p.VetAllowPattern(pattern)
		if typed.Offered {
			t.Errorf("the form accepted %q, which holds a word that is not a command word", pattern)
		}
		derived := p.RememberOfferFor(Verdict{
			Decision: PolicyAsk, Tool: shellToolName, Command: pattern + " > /tmp/probe",
		})
		if !derived.Offered {
			t.Errorf("the card refused %q (%s); it is supposed to summarise", pattern, derived.Reason)
			continue
		}
		if len(strings.Fields(derived.Pattern)) >= len(strings.Fields(pattern)) {
			t.Errorf("the card kept %q from %q instead of stopping at the argument",
				derived.Pattern, pattern)
		}
	}
	// (2) A four-word typed rule is accepted; the card would have stopped at
	// three. Narrower on the typed side, never wider.
	long := "zzprobe status verbose json"
	typed := p.VetAllowPattern(long)
	if !typed.Offered || typed.Pattern != long {
		t.Errorf("the form refused the four-word %q: %s", long, typed.Reason)
	}
	derived := p.RememberOfferFor(Verdict{
		Decision: PolicyAsk, Tool: shellToolName, Command: long + " > /tmp/probe",
	})
	if !derived.Offered || len(strings.Fields(derived.Pattern)) != maxPatternWords {
		t.Errorf("the card proposed %q from %q; the cap is %d words",
			derived.Pattern, long, maxPatternWords)
	}
	if !prefixOf(strings.Fields(derived.Pattern), strings.Fields(long)) {
		t.Errorf("the card's %q is not a prefix of the typed %q", derived.Pattern, long)
	}
}

// TestEveryRefusalIsAnOfferWithASentence: whichever route refuses, and
// whatever it refuses, the card has a line to show and read aloud. A refusal
// nobody can understand is a refusal people route around, which is how a
// safety control becomes a nuisance and then an exception.
func TestEveryRefusalIsAnOfferWithASentence(t *testing.T) {
	p := mustPolicy(t, PolicyConfig{ShellDeny: []string{"httpie post"}})
	rng := rand.New(rand.NewSource(propSeed)) //nolint:gosec // corpus, not crypto
	for _, pattern := range generatedPatterns(rng) {
		for _, offer := range []RememberOffer{
			p.VetAllowPattern(pattern),
			p.RememberOfferFor(Verdict{
				Decision: PolicyAsk, Tool: shellToolName, Command: pattern + " > /tmp/probe",
			}),
		} {
			switch {
			case offer.Offered && offer.Pattern == "":
				t.Errorf("seed %d: %q was offered with no pattern on the button", propSeed, pattern)
			case offer.Offered && offer.Reason != "":
				t.Errorf("seed %d: %q was offered and still carries a refusal: %s",
					propSeed, pattern, offer.Reason)
			case !offer.Offered && strings.TrimSpace(offer.Reason) == "":
				t.Errorf("seed %d: %q was refused without saying why", propSeed, pattern)
			case !offer.Offered && offer.Pattern != "":
				t.Errorf("seed %d: %q was refused and still named %q",
					propSeed, pattern, offer.Pattern)
			}
		}
	}
}

// TestARiskWordInCommandPositionIsNeverSilent states the risk-word half of the
// separator law without borrowing a single one of the classifier's own
// helpers. The pieces are cut here, by this test, on the separators a shell
// recognises; the leading word of each piece is read here too. So a mutation
// that changed what splitShellCommand breaks on, or what commandWord skips,
// shows up as a command that runs silently rather than as two implementations
// agreeing with each other.
func TestARiskWordInCommandPositionIsNeverSilent(t *testing.T) {
	p := mustPolicy(t, PolicyConfig{ShellAllow: []string{"zzprobe status"}})
	risky := []string{"rm", "sudo", "dd", "chmod", "eval", "bash", "python3", "tee", "mkfs.ext4"}
	prefixes := []string{
		"", "ls", "docker ps", "zzprobe status", "echo hi", "FOO=1 ls",
		"ls -la /tmp", "cat /etc/hostname 2>/dev/null",
	}
	for _, word := range risky {
		for _, prefix := range prefixes {
			for _, sep := range separators {
				for _, tail := range []string{"", " -x", " /tmp/thing", " " + strings.Repeat("a", 200)} {
					command := word + tail
					if prefix != "" {
						command = prefix + sep + command
					}
					if v := p.Decide(shellCall(command)); v.Decision == PolicyAllow {
						t.Errorf("%q ran silently (%s), and %q is a command word in it",
							command, v.Rule, word)
					}
				}
			}
		}
	}
}

// TestADenyRuleSurvivesEveryDecoration: deny beats everything, so no amount of
// wrapping, quoting, redirecting or prefixing with allow-listed commands may
// downgrade it. This is the law behind the "deny patterns run against the raw
// command first" comment in decideShell — stated so a refactor that moved the
// check after the split would fail here rather than in production.
func TestADenyRuleSurvivesEveryDecoration(t *testing.T) {
	p := mustPolicy(t, PolicyConfig{ShellDeny: []string{"httpie post"}})
	denied := []string{
		"rm -rf /", "rm -rf /*", "sudo rm -rf /", "dd if=x of=/dev/sda",
		"echo x > /dev/sda", "cat y > /dev/nvme0n1", ":(){ :|:& };:",
		"httpie post /x", "httpie post",
	}
	for _, bad := range denied {
		if v := p.Decide(shellCall(bad)); v.Decision != PolicyDeny {
			t.Fatalf("%q is not denied on its own (%s); the rest of this law is vacuous",
				bad, v.Decision)
		}
		for _, prefix := range []string{"", "ls", "docker ps", "echo hi", "FOO=1 ls"} {
			for _, sep := range separators {
				for _, dec := range commandDecorations {
					command := bad + dec
					if prefix != "" {
						command = prefix + sep + command
					}
					if v := p.Decide(shellCall(command)); v.Decision != PolicyDeny {
						t.Errorf("%q was judged %s (%s); it contains the denied %q",
							command, v.Decision, v.Rule, bad)
					}
				}
			}
		}
	}
}

// TestUnicodeWhitespaceDoesNotDefeatADenyRule pins the defect the generated
// corpus above found, by name, so it reads as the regression it is.
//
// Every one of these is `rm -rf /` — the shape the shipped deny rule exists
// for — with one non-ASCII space somewhere the rule's boundary class has to
// cope with. Before the fix in policy.go, each dropped to ask, because Go's
// `\s` is ASCII and the classifier's own word splitting is not.
func TestUnicodeWhitespaceDoesNotDefeatADenyRule(t *testing.T) {
	p := mustPolicy(t, PolicyConfig{})
	for _, command := range []string{
		"rm -rf /\u2028rm -rf /", // the generated case, verbatim
		"rm -rf /\u00a0",         // a no-break space where the rule wants an end
		"rm\u00a0-rf /",          // ... and where it wants a separator
		"rm -rf\u2028/",          // ... and between the flags and the target
		"echo hi\u2028rm -rf /",  // ... in front of the command word
		"dd if=/dev/zero\u00a0of=/dev/sda",
		">\u00a0/dev/sda",
		":()\u00a0{ :|:& };:",
	} {
		if v := p.Decide(shellCall(command)); v.Decision != PolicyDeny {
			t.Errorf("%q was judged %s (%s); one unicode space is not an exemption",
				command, v.Decision, v.Rule)
		}
	}
}
