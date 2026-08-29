package tools

import (
	"strings"
	"testing"
)

// Fuzz targets for the permission gate (issue #172).
//
// classifier_property_test.go states the laws and attacks them with a corpus
// this repo wrote. These two targets state the same laws and let the fuzzer
// write the corpus, which is the half that finds the input nobody would think
// to type — the U+2028 that walked through a deny rule was found this way.
//
// Both are cheap enough to replay on every `go test` (the committed corpus
// under testdata/fuzz is a handful of entries) and are fuzzed properly on the
// scheduled job, off the PR gate. Anything a scheduled run finds is minimised
// by the go tool into testdata/fuzz and committed, so it becomes a regression
// test rather than a memory.

// fuzzPolicy is the configuration both targets judge against: a user-granted
// allow rule, a user-written deny rule, and the shipped tables underneath. A
// bare policy would leave the configured lists — the part a user can get
// wrong — untested.
func fuzzPolicy(t *testing.T) *Policy {
	t.Helper()
	return mustPolicy(t, PolicyConfig{
		Default:    PolicyAsk,
		ShellAllow: []string{"zzprobe status"},
		ShellDeny:  []string{"httpie post"},
	})
}

// FuzzShellClassifier throws arbitrary text at the shell classifier and
// asserts the invariants that make it a security control rather than a
// heuristic: a compound line is never judged by one of its parts, silence
// means every part earned it, and deny is not negotiable.
func FuzzShellClassifier(f *testing.F) {
	seeds := []string{
		"ls", "docker ps", "docker ps -a", "df -h", "git log --oneline",
		"ls; rm -rf ~", "ls && sudo reboot", "echo $(rm -rf /)", "echo `id`",
		"docker ps 2>/dev/null", "docker ps > /tmp/x", "journalctl --vacuum-time=1d",
		"FOO=1 rm -rf /tmp/x", "timeout 5 rm -rf ~", "zzprobe status --json",
		"httpie post /x", "ls | httpie post /x", ":(){ :|:& };:",
		"rm -rf /", "rm -rf /*", "dd if=/dev/zero of=/dev/sda",
		// The defect this target's separator law caught: Go's `\s` is ASCII and
		// the classifier's word splitting is not, so one U+2028 walked a
		// `rm -rf /` straight past the deny rule and into an allow.
		"echo hi\u2028rm -rf /", "rm -rf /\u2028rm -rf /", "rm\u00a0-rf /",
		"", "   ", "\n\n", ";;;", "&&&", "|", ")", "$(", "`",
		"ls " + strings.Repeat("x", 5000), strings.Repeat("ls;", 500),
		"ls é🎉", "\x00ls", "ls\x00; rm -rf ~",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, command string) {
		p := fuzzPolicy(t)
		v := p.Decide(shellCall(command))

		// Law: the gate is a pure function of the command.
		if again := p.Decide(shellCall(command)); again != v {
			t.Fatalf("%q judged %+v then %+v", command, v, again)
		}
		// Law: every verdict names the rule that made it, and only an ask
		// carries a question. A silent run with no rule is an unauditable run;
		// a summary on an allow is a question nobody was asked.
		if v.Rule == "" {
			t.Fatalf("%q was judged %s with no rule named", command, v.Decision)
		}
		if (v.Summary != "") != (v.Decision == PolicyAsk) {
			t.Fatalf("%q was judged %s with summary %q", command, v.Decision, v.Summary)
		}
		if v.PreApproved && (v.Decision != PolicyAllow || v.Pattern == "") {
			t.Fatalf("%q was pre-approved as %s by pattern %q", command, v.Decision, v.Pattern)
		}

		segments := splitShellCommand(harmlessRedirect.ReplaceAllString(command, " "))
		// Law: no command containing a separator is ever classified from a
		// single segment. Judged the other way round — the whole is at least as
		// strict as every part — because that is the direction a bypass lives
		// in.
		for _, seg := range segments {
			if strings.ContainsAny(seg, separatorRunes) {
				t.Fatalf("%q produced segment %q, which still holds a separator", command, seg)
			}
			part := p.Decide(shellCall(seg))
			if strictness[v.Decision] < strictness[part.Decision] {
				t.Fatalf("%q was judged %s, but its segment %q alone is %s (%s)",
					command, v.Decision, seg, part.Decision, part.Rule)
			}
		}
		// Law: silence means every segment earned it.
		if v.Decision == PolicyAllow {
			if rule, denied := matchDeny(command, p.extraDeny); denied {
				t.Fatalf("%q ran silently despite %s", command, rule)
			}
			for _, seg := range segments {
				if rule, denied := matchDeny(seg, p.extraDeny); denied {
					t.Fatalf("%q ran silently despite segment %q matching %s", command, seg, rule)
				}
				if w := commandWord(seg); riskWords[w] || strings.HasPrefix(w, "mkfs") {
					t.Fatalf("%q ran silently with segment %q, whose command is the risk word %q",
						command, seg, w)
				}
			}
		}
		// Law: a denied command stays denied whatever it is glued to, and
		// whatever glues it. The whitespace list is the point: Go's `\s` is
		// ASCII and this classifier's word splitting is not, so before the fix
		// in policy.go a single U+2028 between arbitrary text and `rm -rf /`
		// dropped the line from deny to allow. Stated as a law it holds for
		// every fuzzer-generated neighbour, not for the four that were listed.
		for _, core := range []string{"rm -rf /", "dd if=/dev/zero of=/dev/sda"} {
			for _, glue := range []string{" ", "\t", "\n", "\u00a0", "\u2028", "\u3000"} {
				for _, glued := range []string{command + glue + core, core + glue + command} {
					if d := p.Decide(shellCall(glued)); d.Decision != PolicyDeny {
						t.Fatalf("%q is denied, but %q is %s (%s)", core, glued, d.Decision, d.Rule)
					}
				}
			}
		}
		// Law: deny is not negotiable. Prefixing a denied line with the most
		// harmless command in the shipped list must not change its tier.
		if v.Decision == PolicyDeny {
			for _, decorated := range []string{
				"ls; " + command, "ls && " + command, "echo hi | " + command,
				command + " ; ls", "FOO=1 " + command,
			} {
				if d := p.Decide(shellCall(decorated)); d.Decision != PolicyDeny {
					t.Fatalf("%q is denied but %q is %s (%s)",
						command, decorated, d.Decision, d.Rule)
				}
			}
		}
	})
}

// FuzzRememberOffer throws arbitrary text at the remembered-approval matrix
// and asserts the security invariant as a law: no pattern the matrix accepts
// can be a prefix of a command the deny list or the risk words refuse, and
// the derived (card) and typed (form) routes agree on everything they both
// judge.
func FuzzRememberOffer(f *testing.F) {
	seeds := []string{
		"docker ps --format '{{.Image}}'", "ps aux --sort -%cpu", "xdg-open ~/notes.md",
		"timeout 5 make deploy", "find . -name '*.go'", "docker run -v /:/host alpine",
		"./deploy.sh", "httpie post /x", "jarvix approvals list", "git status",
		"npm test", "kubectl get pods -A", "docker compose ps", "podman run alpine",
		"zzprobe status --json", "mkfs.ext4 /dev/sdb1", "rm -rf ~", "sudo apt update",
		"docker ps > /tmp/x", "ls; docker ps", "FOO=1 docker ps",
		"", "   ", "5 rm -rf ~", "-a", "/opt/bin/thing", "docker",
		"docker system prune", "podman volume rm", "a b c d e f g",
		strings.Repeat("z", 200) + " status",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, command string) {
		p := fuzzPolicy(t)
		// The card only exists for the ask tier, but the matrix must answer
		// honestly for any command a caller hands it, so the verdict is
		// synthesised rather than filtered — a refusal is as much an answer as
		// an offer, and both are checked below.
		offer := p.RememberOfferFor(Verdict{
			Decision: PolicyAsk, Tool: shellToolName, Command: command,
		})
		if !offer.Offered {
			// Law: a refusal has a sentence to say and names no rule.
			if strings.TrimSpace(offer.Reason) == "" {
				t.Fatalf("%q was refused without saying why", command)
			}
			if offer.Pattern != "" {
				t.Fatalf("%q was refused and still named the pattern %q", command, offer.Pattern)
			}
			return
		}
		words := strings.Fields(offer.Pattern)
		// Law: an offered pattern is a command prefix and nothing else.
		if len(words) == 0 || len(words) > maxPatternWords {
			t.Fatalf("%q was offered as %q, which is %d words", command, offer.Pattern, len(words))
		}
		if !allFixedWords(words) {
			t.Fatalf("%q was offered as %q, which holds something that is not a command word",
				command, offer.Pattern)
		}
		// Law: the matrix's three groups are never crossed.
		if riskWords[words[0]] || strings.HasPrefix(words[0], "mkfs") {
			t.Fatalf("%q was offered as %q, which heads a risk word", command, offer.Pattern)
		}
		if _, blocked := unrememberableBinaries[words[0]]; blocked {
			t.Fatalf("%q was offered as %q, which heads a refused binary", command, offer.Pattern)
		}
		if reason, blocked := unrememberableShapeFor(words); blocked {
			t.Fatalf("%q was offered as %q, which covers a refused shape: %s",
				command, offer.Pattern, reason)
		}
		for _, d := range p.extraDeny {
			if prefixOf(words, d) || prefixOf(d, words) {
				t.Fatalf("%q was offered as %q beside the deny rule %q",
					command, offer.Pattern, strings.Join(d, " "))
			}
		}
		// Law: the two routes to shell_allow agree. What the card derived, a
		// person typing the same words must be given, verbatim.
		typed := p.VetAllowPattern(offer.Pattern)
		if !typed.Offered || typed.Pattern != offer.Pattern {
			t.Fatalf("the card offered %q for %q, but the form answered offered=%v %q (%s)",
				offer.Pattern, command, typed.Offered, typed.Pattern, typed.Reason)
		}
		// Law: granting it cannot rescue anything the gate refuses.
		granted := mustPolicy(t, PolicyConfig{
			Default:    PolicyAsk,
			ShellAllow: []string{"zzprobe status", offer.Pattern},
			ShellDeny:  []string{"httpie post"},
		})
		for _, probe := range commandsUnder(offer.Pattern) {
			before := p.Decide(shellCall(probe))
			after := granted.Decide(shellCall(probe))
			if before.Decision == PolicyDeny && after.Decision != PolicyDeny {
				t.Fatalf("granting %q (from %q) turned the denied %q into %s",
					offer.Pattern, command, probe, after.Decision)
			}
			if after.Decision != PolicyAllow {
				continue
			}
			for _, seg := range splitShellCommand(harmlessRedirect.ReplaceAllString(probe, " ")) {
				if rule, denied := matchDeny(seg, granted.extraDeny); denied {
					t.Fatalf("granting %q silenced %q, whose segment %q matches %s",
						offer.Pattern, probe, seg, rule)
				}
				if w := commandWord(seg); riskWords[w] || strings.HasPrefix(w, "mkfs") {
					t.Fatalf("granting %q silenced %q, whose segment %q runs the risk word %q",
						offer.Pattern, probe, seg, w)
				}
			}
		}
	})
}
