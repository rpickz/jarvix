package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/ai"
)

// These tests are the contract of issue #162's riskiest half: which commands
// may become a standing rule, which may not, and exactly what rule each one
// produces. The wording of a refusal is pinned too — it is shown on the card
// and read aloud, and a refusal a user cannot understand is one they route
// around.

// offerFor builds a policy, classifies command, and returns the remember
// offer for the resulting verdict — the whole path a card takes.
func offerFor(t *testing.T, cfg PolicyConfig, command string) (Verdict, RememberOffer) {
	t.Helper()
	p, err := NewPolicy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	args, err := json.Marshal(struct {
		Command string `json:"command"`
	}{command})
	if err != nil {
		t.Fatal(err)
	}
	v := p.Decide(ai.ToolCall{Name: shellToolName, Arguments: string(args)})
	return v, p.RememberOfferFor(v)
}

// The pattern proposal, on the shapes from the user's own journal in #162 and
// the ones the algorithm must stop one word short of.
func TestProposedPatternIsTheNarrowestUsefulPrefix(t *testing.T) {
	cases := []struct {
		command string
		want    string
	}{
		// The journal's own variants: one rule covers both spellings.
		{"docker stats --no-stream", "docker stats"},
		{"docker stats", "docker stats"},
		{"xdg-open https://example.com/a?b=c", "xdg-open"},
		{"xdg-open ~/notes.md", "xdg-open"},
		{"xdg-open .", "xdg-open"},
		// Env assignments are dropped: matchWordPrefix skips them too, so a
		// pattern containing one would never match anything.
		{"FOO=1 BAR=2 notify-send 'build done'", "notify-send"},
		// A number is an argument, never a subcommand — the leading-letter
		// rule in fixedWordPattern.
		{"tree -L 2", "tree"},
		// Three words deep, and no further.
		{"kubectl get pods -o json", "kubectl get pods"},
		// A flag stops the walk immediately.
		{"btm --basic", "btm"},
	}
	for _, c := range cases {
		v, offer := offerFor(t, PolicyConfig{}, c.command)
		if v.Decision != PolicyAsk {
			t.Fatalf("%q was decided %q, want ask (the card would never appear)", c.command, v.Decision)
		}
		if !offer.Offered {
			t.Errorf("%q: remembering refused (%s), want the pattern %q",
				c.command, offer.Reason, c.want)
			continue
		}
		if offer.Pattern != c.want {
			t.Errorf("%q proposed %q, want %q", c.command, offer.Pattern, c.want)
		}
	}
}

// A proposed pattern must actually silence the command it came from. This is
// the property that makes the button honest: the user is told a rule, and the
// rule does what the label says.
func TestTheProposedPatternReallySilencesTheCommand(t *testing.T) {
	for _, command := range []string{
		"docker stats --no-stream", "xdg-open https://example.com",
		"kubectl get pods -o json", "jq .items x.json",
	} {
		_, offer := offerFor(t, PolicyConfig{}, command)
		if !offer.Offered {
			t.Fatalf("%q: remembering refused (%s)", command, offer.Reason)
		}
		v, _ := offerFor(t, PolicyConfig{ShellAllow: []string{offer.Pattern}}, command)
		if v.Decision != PolicyAllow {
			t.Errorf("%q with rule %q still %q (%s)", command, offer.Pattern, v.Decision, v.Rule)
		}
		if !v.PreApproved || v.Pattern != offer.Pattern {
			t.Errorf("%q with rule %q: PreApproved=%v pattern=%q, want true and %q",
				command, offer.Pattern, v.PreApproved, v.Pattern, offer.Pattern)
		}
	}
}

// The refusal matrix. Every entry the acceptance criteria name, plus the
// families the criteria's "…" invites — each with the reason it must give.
func TestRefusalMatrix(t *testing.T) {
	cases := []struct {
		command  string
		contains string
	}{
		// The AC's destructive leading words. Every one of these is a
		// riskWord, so an allow rule would be inert as well as unwise — and
		// the sentence says so.
		{"rm -rf ./build", `"rm" always asks`},
		{"dd if=/dev/zero of=./x", `"dd" always asks`},
		{"mkfs.ext4 /dev/loop0", `"mkfs.ext4" always asks`},
		{"sudo systemctl restart nginx", `"sudo" always asks`},
		{"chmod 777 ./x", `"chmod" always asks`},
		{"chown me ./x", `"chown" always asks`},
		{"kill -9 123", `"kill" always asks`},
		{"shutdown -h now", `"shutdown" always asks`},
		{"mv a b", `"mv" always asks`},
		{"crontab -l", `"crontab" always asks`},
		{"xargs rm", `"xargs" always asks`},
		{"sh -c 'echo hi'", `"sh" always asks`},
		{"bash -c 'echo hi'", `"bash" always asks`},
		{"eval something", `"eval" always asks`},
		// The AC's destructive-flag binaries: a rule WOULD silence these, and
		// a word prefix cannot exclude the flag that makes them dangerous.
		{"find . -name '*.go'", "-delete and -exec"},
		{"git remote -v", "push --force"},
		{"systemctl list-dependencies nginx", "at boot"},
		// Command wrappers: the classifier judges the leading word, so a rule
		// here would wave anything through.
		{"timeout 5 make deploy", "rule for it is a rule for everything"},
		{"nohup somejob", "rule for it is a rule for everything"},
		{"watch date", "unattended"},
		// Reaches another machine, or brings code from one.
		{"ssh box uptime", "another machine"},
		{"curl -sS https://example.com", "-o` and `--upload-file"},
		// Runs code from a manifest.
		{"npm ls", "package scripts"},
		{"go version", "compile and execute"},
		// Synthetic input, the never-silent floor's back door.
		{"xdotool key ctrl+a", "the one thing I never do silently"},
		// Jarvix's own command line.
		{"jarvix approvals forget docker ps", "widen my own permissions"},
		// A path-invoked script: the file can change after the rule is made.
		{"./deploy.sh", "what a file contains can change"},
		{"/opt/bin/thing", "what a file contains can change"},
	}
	for _, c := range cases {
		_, offer := offerFor(t, PolicyConfig{}, c.command)
		if offer.Offered {
			t.Errorf("%q offered the rule %q; it must never be rememberable",
				c.command, offer.Pattern)
			continue
		}
		if !strings.Contains(offer.Reason, c.contains) {
			t.Errorf("%q refused with %q, want it to contain %q",
				c.command, offer.Reason, c.contains)
		}
	}
}

// The derived half of the matrix: a proposal is refused when a dangerous
// shape sits UNDER it, which is the whole of "never propose a bare binary
// whose subcommands differ wildly in danger" — with no separate list to keep
// in step.
func TestABareMultiplexerIsNeverProposed(t *testing.T) {
	for _, c := range []struct{ command, covers string }{
		{"docker --version", "docker run"},
		{"podman --version", "podman run"},
		{"docker compose --profile dev", "docker compose up"},
	} {
		_, offer := offerFor(t, PolicyConfig{}, c.command)
		if offer.Offered {
			t.Errorf("%q offered %q, which would also cover %q",
				c.command, offer.Pattern, c.covers)
			continue
		}
		if !strings.Contains(offer.Reason, c.covers) {
			t.Errorf("%q refused with %q, want it to name %q", c.command, offer.Reason, c.covers)
		}
	}
}

// …and the shapes themselves are refused outright, not merely as coverage.
func TestDangerousSubcommandsAreRefusedOutright(t *testing.T) {
	for _, command := range []string{
		"docker run -v /:/host alpine sh",
		"docker exec -it web sh",
		"docker compose up -d",
		"podman run alpine",
		"kubectl delete pod web",
		"helm upgrade app ./chart",
	} {
		_, offer := offerFor(t, PolicyConfig{}, command)
		if offer.Offered {
			t.Errorf("%q offered the rule %q", command, offer.Pattern)
		}
	}
	// The read-only siblings stay offerable — the matrix must not be a
	// blanket ban on the binary, or the feature does nothing for the exact
	// commands it exists for.
	for _, c := range []struct{ command, want string }{
		{"docker stats", "docker stats"},
		{"docker compose ps", "docker compose ps"},
		{"kubectl get pods", "kubectl get pods"},
	} {
		_, offer := offerFor(t, PolicyConfig{}, c.command)
		if !offer.Offered || offer.Pattern != c.want {
			t.Errorf("%q: offered=%v pattern=%q reason=%q, want %q",
				c.command, offer.Offered, offer.Pattern, offer.Reason, c.want)
		}
	}
}

// Compound commands: remember only the segment that asked, and refuse
// outright when more than one did. Partial memory of a compound is a trap.
func TestCompoundCommands(t *testing.T) {
	// The issue's own example: the docker half is fine, the rm half is a risk
	// word, so exactly one segment asks and it is one the matrix refuses —
	// nothing is remembered.
	_, offer := offerFor(t, PolicyConfig{}, "docker stats; rm -rf ./x")
	if offer.Offered {
		t.Errorf("`docker stats; rm -rf ./x` offered %q", offer.Pattern)
	}

	// One asking segment inside a pipeline of allowed ones: the rule is built
	// from that segment alone, never from the whole line.
	_, offer = offerFor(t, PolicyConfig{}, "cat log | jq .items | head")
	if !offer.Offered {
		t.Fatalf("`cat log | jq .items | head` refused: %s", offer.Reason)
	}
	if offer.Pattern != "jq" {
		t.Errorf("pattern = %q, want %q (the segment that asked, not the line)", offer.Pattern, "jq")
	}
	if offer.Segment != "jq .items" {
		t.Errorf("segment = %q, want %q", offer.Segment, "jq .items")
	}

	// Two asking segments: refused, and the reason says how many.
	_, offer = offerFor(t, PolicyConfig{}, "jq .a x.json && yq .b y.yaml")
	if offer.Offered {
		t.Errorf("a two-decision compound offered %q", offer.Pattern)
	}
	if !strings.Contains(offer.Reason, "2 parts") {
		t.Errorf("reason = %q, want it to count the parts", offer.Reason)
	}
}

// Deny always wins, in both directions: a denied command offers nothing, and
// neither does a proposal broad enough to cover a denied shape.
func TestDenyBeatsRemembering(t *testing.T) {
	// A denied command never reaches a card at all, so there is nothing to
	// remember and nothing to offer: deny short-circuits above the ask tier.
	v, offer := offerFor(t, PolicyConfig{ShellDeny: []string{"jq"}}, "jq .items x.json")
	if v.Decision != PolicyDeny {
		t.Fatalf("a denied command was decided %q", v.Decision)
	}
	if offer.Offered {
		t.Errorf("a denied command offered %q", offer.Pattern)
	}

	// The interesting case is the rule that is BROADER than the deny: the
	// command in hand passes the deny check, but the pattern it would produce
	// covers a denied shape. Refused, and the sentence says deny wins.
	cfg := PolicyConfig{ShellDeny: []string{"httpie post"}}
	v, offer = offerFor(t, cfg, "httpie --version")
	if v.Decision != PolicyAsk {
		t.Fatalf("`httpie --version` was decided %q, want ask", v.Decision)
	}
	if offer.Offered {
		t.Errorf("a proposal covering a denied shape offered %q", offer.Pattern)
	}
	if !strings.Contains(offer.Reason, "deny rule always wins") {
		t.Errorf("reason = %q", offer.Reason)
	}

	// And the mirror: the proposal IS the denied shape.
	_, offer = offerFor(t, PolicyConfig{ShellDeny: []string{"httpie"}}, "httpie post /x")
	if offer.Offered {
		t.Errorf("a denied shape offered %q", offer.Pattern)
	}
}

// The word walk itself, below the tier: `ps aux` is a shipped read-only
// allow pattern so it never reaches a card, but the derivation is the one the
// issue's journal names and is worth pinning where the tier cannot hide it.
func TestPatternWalkStopsAtTheFirstVariableWord(t *testing.T) {
	p, err := NewPolicy(PolicyConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ segment, want string }{
		{"ps aux --sort -%cpu", "ps aux"},
		{"ps aux --sort=-%cpu", "ps aux"},
		{"jq .items x.json", "jq"},
		{"tree -L 2 /tmp", "tree"},
		{"btop --preset 1", "btop"},
	} {
		offer := p.proposeFor(c.segment)
		if !offer.Offered || offer.Pattern != c.want {
			t.Errorf("%q proposed %q (offered=%v, %s), want %q",
				c.segment, offer.Pattern, offer.Offered, offer.Reason, c.want)
		}
	}
}

// A remembered rule never overrides a risk word, a risk regex, or a deny —
// which is what stops `ls` authorising `ls; rm -rf ~`.
func TestARememberedRuleCannotAuthoriseWhatTheGateRefuses(t *testing.T) {
	p, err := NewPolicy(PolicyConfig{ShellAllow: []string{"ls"}, ShellDeny: []string{"jq"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		command string
		want    PolicyDecision
	}{
		{"ls -la", PolicyAllow},
		{"ls; rm -rf ~", PolicyAsk},          // the rm segment is a risk word
		{"ls > /etc/passwd", PolicyAsk},      // the redirection risk regex
		{"ls $(rm -rf ~)", PolicyAsk},        // substitution bodies are segments
		{"ls && jq .x", PolicyDeny},          // deny beats the allow
		{"ls; sudo reboot", PolicyAsk},       // privilege escalation
		{"ls; docker run alpine", PolicyAsk}, // unmatched segment
	} {
		args, err := json.Marshal(struct {
			Command string `json:"command"`
		}{c.command})
		if err != nil {
			t.Fatal(err)
		}
		v := p.Decide(ai.ToolCall{Name: shellToolName, Arguments: string(args)})
		if v.Decision != c.want {
			t.Errorf("%q = %q (%s), want %q", c.command, v.Decision, v.Rule, c.want)
		}
	}
}

// The always-ask floor: shell_allow's vocabulary describes shell commands, so
// no other identity can be reached by it at all. Structural, not denied.
func TestOnlyShellIdentitiesCanBeRemembered(t *testing.T) {
	p, err := NewPolicy(PolicyConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{
		ScriptToolName, ConfigWriteEntryToolName, ConfigDeleteEntryToolName,
		TypeTextToolName, PressKeyToolName, AdvisorToolName, "memory.forget",
	} {
		offer := p.RememberOfferFor(Verdict{Decision: PolicyAsk, Tool: tool, Command: "whatever"})
		if offer.Offered {
			t.Errorf("%s offered a rule (%q); shell_allow cannot describe it", tool, offer.Pattern)
		}
		if !strings.Contains(offer.Reason, "only shell commands can be remembered") {
			t.Errorf("%s refused with %q", tool, offer.Reason)
		}
	}
	// …and the two identities that ARE shell commands can be.
	for _, tool := range []string{shellToolName, IntentToolName} {
		v := p.DecideCommand(tool, "docker stats")
		if offer := p.RememberOfferFor(v); !offer.Offered {
			t.Errorf("%s refused a plain shell command: %s", tool, offer.Reason)
		}
	}
}

// Conversation-scoped grants use exactly the classifier the configured list
// does, and are named distinctly so the audit row can say which kind ran a
// command.
func TestConversationGrantsAreAppliedLikeConfiguredPatterns(t *testing.T) {
	p, err := NewPolicy(PolicyConfig{})
	if err != nil {
		t.Fatal(err)
	}
	grants, err := CompileGrants([]string{"docker stats"})
	if err != nil {
		t.Fatal(err)
	}
	args, err := json.Marshal(struct {
		Command string `json:"command"`
	}{"docker stats --no-stream"})
	if err != nil {
		t.Fatal(err)
	}
	call := ai.ToolCall{Name: shellToolName, Arguments: string(args)}

	if v := p.Decide(call); v.Decision != PolicyAsk {
		t.Fatalf("without the grant: %q, want ask", v.Decision)
	}
	v := p.DecideWithGrants(call, grants)
	if v.Decision != PolicyAllow {
		t.Fatalf("with the grant: %q (%s), want allow", v.Decision, v.Rule)
	}
	if v.Rule != `conversation allow pattern "docker stats"` {
		t.Errorf("rule = %q, want it named as a conversation grant", v.Rule)
	}
	if !v.PreApproved || v.Pattern != "docker stats" {
		t.Errorf("PreApproved=%v pattern=%q", v.PreApproved, v.Pattern)
	}

	// And a grant is still the weakest thing in the classifier.
	deny, err := NewPolicy(PolicyConfig{ShellDeny: []string{"docker"}})
	if err != nil {
		t.Fatal(err)
	}
	if v := deny.DecideWithGrants(call, grants); v.Decision != PolicyDeny {
		t.Errorf("a grant beat a deny rule: %q", v.Decision)
	}
}

// A shipped read-only allow pattern is NOT a grant anybody made, so it must
// not produce an audit row — the rows that matter would drown in the ones
// that never did.
func TestShippedAllowPatternsAreNotPreApprovals(t *testing.T) {
	p, err := NewPolicy(PolicyConfig{})
	if err != nil {
		t.Fatal(err)
	}
	args, err := json.Marshal(struct {
		Command string `json:"command"`
	}{"ls -la"})
	if err != nil {
		t.Fatal(err)
	}
	v := p.Decide(ai.ToolCall{Name: shellToolName, Arguments: string(args)})
	if v.Decision != PolicyAllow {
		t.Fatalf("`ls -la` = %q", v.Decision)
	}
	if v.PreApproved {
		t.Errorf("a shipped allow pattern reported itself as a pre-approval (%s)", v.Rule)
	}
}

// An already-covered command is not offered a duplicate rule.
func TestAnAlreadyCoveredPatternIsNotOfferedAgain(t *testing.T) {
	// `docker stats` is allowed, but the redirection forces the question —
	// and a second copy of the same rule would not change that.
	_, offer := offerFor(t, PolicyConfig{ShellAllow: []string{"docker stats"}},
		"docker stats > /tmp/out")
	if offer.Offered {
		t.Fatalf("offered %q for a command already covered by that rule", offer.Pattern)
	}
	if !strings.Contains(offer.Reason, "already on your allow list") {
		t.Errorf("reason = %q", offer.Reason)
	}
}

// Every refusal says something, and says it as one sentence a person can act
// on: the card shows this text and the voice reads it.
func TestEveryRefusalCarriesASentence(t *testing.T) {
	for _, command := range []string{
		"rm -rf /tmp/x", "find . -delete", "git push --force", "./x.sh",
		"docker run alpine", "jq .a a && yq .b b", "timeout 5 date",
	} {
		_, offer := offerFor(t, PolicyConfig{}, command)
		if offer.Offered {
			continue
		}
		switch {
		case strings.TrimSpace(offer.Reason) == "":
			t.Errorf("%q refused with no reason at all", command)
		case !strings.HasSuffix(offer.Reason, "."):
			t.Errorf("%q refused with %q, which is not a sentence", command, offer.Reason)
		case len(offer.Reason) > 200:
			t.Errorf("%q refused with %d characters; one short sentence was asked for",
				command, len(offer.Reason))
		}
	}
}

// The scope vocabulary is closed at the IPC edge: an unknown word must be a
// rejected request, never a silently weaker (or stronger) grant.
func TestRememberScopeVocabularyIsClosed(t *testing.T) {
	for _, ok := range []string{"", "always", "conversation"} {
		if !ValidRememberScope(ok) {
			t.Errorf("%q should be a valid scope", ok)
		}
	}
	for _, bad := range []string{"forever", "session", "yes", "ALWAYS", "permanent"} {
		if ValidRememberScope(bad) {
			t.Errorf("%q should not be a valid scope", bad)
		}
	}
}
