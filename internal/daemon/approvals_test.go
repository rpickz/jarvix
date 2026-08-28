package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/ai"
	"github.com/rpickz/jarvix/internal/audio"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/stt"
	"github.com/rpickz/jarvix/internal/tools"
	"github.com/rpickz/jarvix/internal/tts"
)

// These tests are the end-to-end contract of issue #162: a confirmation card
// answered with "don't ask again" appends one narrow rule to config.toml
// through the surgical editor, the running gate picks it up immediately, the
// next matching command runs unprompted AND is audited, and the whole thing
// is listable and revocable. The model-cannot-write-a-rule pin is here too:
// it is the property the feature's safety rests on.

// startRememberDaemon runs a wired daemon behind the real permission gate,
// with a config.toml on disk (hand-written, comments and all, so byte
// preservation can be proved) and a scripted model that asks to run commands
// in order.
//
// One command per model ROUND, not per session: the fake provider advances a
// round on every Chat call, so consecutive commands run inside one session's
// tool loop. An empty string is a round with no tool call — the final answer
// that ends a session — which is how a test asks for a second session.
func startRememberDaemon(t *testing.T, commands ...string) (*ipc.Client, config.Paths) {
	t.Helper()
	dir := t.TempDir()
	paths := config.Paths{
		Config: dir, Data: dir, State: dir, Runtime: dir,
		Socket: filepath.Join(dir, "j.sock"),
	}
	if err := os.WriteFile(paths.ConfigFile(), []byte(handWrittenConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.Audio.MinRecordingMs = 0
	cfg.Tools.Shell = true
	// The running gate holds what the file on disk says (#164): the deny list
	// is now editable through the same surface, so a test that compared the two
	// would be comparing a daemon booted from one document against a file
	// holding another.
	cfg.Tools.Policy.ShellDeny = []string{"httpie post"}
	provider := &ai.Fake{Response: "Done."}
	for _, command := range commands {
		if command == "" {
			provider.ToolCallsByRound = append(provider.ToolCallsByRound, nil)
			continue
		}
		args, err := json.Marshal(struct {
			Command string `json:"command"`
		}{command})
		if err != nil {
			t.Fatal(err)
		}
		provider.ToolCallsByRound = append(provider.ToolCallsByRound,
			[]ai.ToolCall{{ID: "c", Name: "shell.run", Arguments: string(args)}})
	}
	d, err := New(cfg, paths, nil, Deps{
		Provider:    provider,
		Transcriber: &stt.Fake{Text: "unused"},
		Synthesizer: &tts.Fake{},
		Recorder:    &audio.FakeRecorder{Clip: audio.Clip{WAVPath: dir + "/r.wav"}},
		Player:      &audio.FakePlayer{},
		Notifier:    &desktop.FakeNotifier{},
		OpenWindow:  func(context.Context) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDaemon(t, d)
	return dialDaemon(t, paths.Socket), paths
}

// handWrittenConfig is a config.toml a person wrote: comments, an ordering
// nobody would generate, and a [tools.policy] table that already exists. The
// rewrite has to leave every byte of it alone except the one line it changes.
// The commands are deliberately a binary that cannot exist on any machine
// ("zzprobe"): the gate's decision is what is under test, and a real command
// would make these tests depend on what happens to be installed — and on
// whether it exits (a `docker stats` without --no-stream never does).
const handWrittenConfig = `# my jarvix config — hands off the comments
[tools]
shell = true

[tools.policy]
# I like being asked about most things.
default = "ask"
shell_deny = ["httpie post"]

[log]
level = "info"   # trailing comment
`

// TestRememberWritesOneNarrowRuleAndTheGatePicksItUp is the headline AC: the
// card's third answer appends the pattern it named, the running policy
// recompiles, and the next matching command runs without asking.
func TestRememberWritesOneNarrowRuleAndTheGatePicksItUp(t *testing.T) {
	client, paths := startRememberDaemon(t,
		"zzprobe status --no-stream", "zzprobe status --format '{{.Name}}'")

	if err := client.Call("session.text", map[string]string{"text": "how busy is it"}, nil); err != nil {
		t.Fatal(err)
	}
	required := waitForEvent(t, client, "tool.confirmation_required")
	// The card names the exact pattern, verbatim, before the user commits.
	if required["remember_pattern"] != "zzprobe status" {
		t.Fatalf("card offered remember_pattern = %v, want %q",
			required["remember_pattern"], "zzprobe status")
	}
	if _, refused := required["remember_reason"]; refused {
		t.Errorf("the card carries both an offer and a refusal: %v", required)
	}

	var reply map[string]any
	if err := client.Call("session.confirm",
		map[string]any{"approved": true, "remember": "always"}, &reply); err != nil {
		t.Fatal(err)
	}
	if reply["remembered"] != "zzprobe status" {
		t.Errorf("reply = %v, want it to name the rule that was added", reply)
	}
	confirmed := waitForEvent(t, client, "tool.confirmed")
	if confirmed["remembered"] != "zzprobe status" {
		t.Errorf("tool.confirmed = %v, want it to record the rule as well as the yes", confirmed)
	}

	// The running gate, in the very next round of the same turn: the second
	// command matches the rule just written and does not ask.
	pre := waitForEvent(t, client, "tool.pre_approved")
	if pre["command"] != "zzprobe status --format '{{.Name}}'" {
		t.Errorf("pre-approved command = %v, want it verbatim", pre["command"])
	}
	if pre["pattern"] != "zzprobe status" {
		t.Errorf("pre-approved pattern = %v, want the rule that let it through", pre["pattern"])
	}
	if rule, _ := pre["rule"].(string); !strings.Contains(rule, `configured allow pattern "zzprobe status"`) {
		t.Errorf("pre-approved rule = %q, want it to name the rule", rule)
	}
	waitForEvent(t, client, "session.finished")

	// The file: one line changed, every comment kept.
	raw, err := os.ReadFile(paths.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	written := string(raw)
	if !strings.Contains(written, `shell_allow = ["zzprobe status"]`) {
		t.Errorf("config.toml has no rule:\n%s", written)
	}
	for _, kept := range []string{
		"# my jarvix config — hands off the comments",
		"# I like being asked about most things.",
		`shell_deny = ["httpie post"]`,
		`level = "info"   # trailing comment`,
	} {
		if !strings.Contains(written, kept) {
			t.Errorf("the rewrite lost %q:\n%s", kept, written)
		}
	}
}

// The audit criterion, stated as the feed sees it: a pre-approved run appears
// in the activity feed naming the rule. Nothing Jarvix does behind a standing
// grant is silent.
func TestAPreApprovedRunAppearsInTheActivityFeed(t *testing.T) {
	client, _ := startRememberDaemon(t, "zzprobe status", "zzprobe status --no-stream")

	if err := client.Call("session.text", map[string]string{"text": "probe"}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "tool.confirmation_required")
	if err := client.Call("session.confirm",
		map[string]any{"approved": true, "remember": "always"}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "tool.pre_approved")
	// The row is not the event. Feed rows are derived by the daemon's own
	// subscriber *from* tool.pre_approved, so the event proves only that the
	// gate spoke — never that the watcher has appended the row yet
	// (docs/ipc.md says exactly this of every activity.row). Sampling
	// activity.get on the event's heels races that watcher, which is how this
	// passed locally and failed in CI. Wait for the row itself, as every
	// other feed assertion in this package does — and watch for the session's
	// end at the same time rather than after it, because that same extra hop
	// lets session.finished overtake the row and be swallowed by the drain
	// waiting for it (see waitForRowsAndSessionEnd).
	waitForRowsAndSessionEnd(t, client, "Ran without asking: shell.run")

	var feed struct {
		Rows []struct {
			Kind   string `json:"kind"`
			Label  string `json:"label"`
			Detail string `json:"detail"`
		} `json:"rows"`
	}
	if err := client.Call("activity.get", nil, &feed); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range feed.Rows {
		if !strings.HasPrefix(row.Label, "Ran without asking") {
			continue
		}
		found = true
		if !strings.Contains(row.Detail, `configured allow pattern "zzprobe status"`) {
			t.Errorf("the audit row does not name the rule: %+v", row)
		}
		if !strings.Contains(row.Detail, "zzprobe status --no-stream") {
			t.Errorf("the audit row does not carry the command: %+v", row)
		}
	}
	if !found {
		t.Errorf("no pre-approved row in the feed: %+v", feed.Rows)
	}
}

// A conversation-scoped grant works exactly like a permanent one and never
// reaches disk — which is the whole of the scope's promise.
func TestAConversationGrantNeverReachesDisk(t *testing.T) {
	client, paths := startRememberDaemon(t,
		"zzprobe status", "zzprobe status --no-stream", "", "zzprobe status --format x")

	before, err := os.ReadFile(paths.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Call("session.text", map[string]string{"text": "probe"}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "tool.confirmation_required")
	var reply map[string]any
	if err := client.Call("session.confirm",
		map[string]any{"approved": true, "remember": "conversation"}, &reply); err != nil {
		t.Fatal(err)
	}
	if reply["remembered"] != "zzprobe status" {
		t.Errorf("reply = %v", reply)
	}
	// …and it still works, and is still audited, and says its scope.
	pre := waitForEvent(t, client, "tool.pre_approved")
	if pre["scope"] != string(tools.RememberConversation) {
		t.Errorf("scope = %v, want %q", pre["scope"], tools.RememberConversation)
	}
	waitForEvent(t, client, "session.finished")

	after, err := os.ReadFile(paths.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("a conversation-scoped grant was written to disk:\n%s", after)
	}
	if _, err := os.Stat(paths.ApprovalsFile()); err == nil {
		t.Errorf("a conversation-scoped grant reached the ledger at %s", paths.ApprovalsFile())
	}

	// Ending the conversation forgets it: "just this conversation" means this
	// one.
	if err := client.Call("conversation.new", nil, nil); err != nil {
		t.Fatal(err)
	}
	var listing struct {
		Approved []map[string]any `json:"approved"`
	}
	if err := client.Call("approvals.list", nil, &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Approved) != 0 {
		t.Errorf("a conversation grant outlived its conversation: %+v", listing.Approved)
	}
	// …and the gate asks again, which is the fact the listing stands for.
	if err := client.Call("session.text", map[string]string{"text": "once more"}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "tool.confirmation_required")
	if err := client.Call("session.confirm", map[string]bool{"approved": false}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "session.finished")
}

// The refusal matrix, as the card sees it: no third option at all, and one
// short sentence saying why.
func TestARefusedShapeGetsNoRememberControl(t *testing.T) {
	client, _ := startRememberDaemon(t, "rm -rf ./build")

	if err := client.Call("session.text", map[string]string{"text": "tidy up"}, nil); err != nil {
		t.Fatal(err)
	}
	required := waitForEvent(t, client, "tool.confirmation_required")
	if _, offered := required["remember_pattern"]; offered {
		t.Fatalf("a destructive command was offered a rule: %v", required)
	}
	reason, _ := required["remember_reason"].(string)
	if !strings.Contains(reason, `"rm" always asks`) {
		t.Errorf("remember_reason = %q", reason)
	}

	// Asking for it anyway — a client that never rendered the card, or one
	// built against an older daemon — is refused, and the confirmation is
	// left standing rather than half-answered.
	err := client.Call("session.confirm", map[string]any{"approved": true, "remember": "always"}, nil)
	if err == nil {
		t.Fatal("a refused shape was remembered on request")
	}
	if !strings.Contains(err.Error(), "cannot be remembered") {
		t.Errorf("error = %v", err)
	}
	if err := client.Call("session.confirm", map[string]bool{"approved": false}, nil); err != nil {
		t.Fatalf("the confirmation did not survive the refused remember: %v", err)
	}
	waitForEvent(t, client, "tool.declined")
	waitForEvent(t, client, "session.finished")
}

// A compound command with two decisions in it remembers nothing, and says so.
func TestACompoundWithTwoDecisionsRemembersNothing(t *testing.T) {
	client, _ := startRememberDaemon(t, "zzprobe status; rm -rf ./x")

	if err := client.Call("session.text", map[string]string{"text": "go"}, nil); err != nil {
		t.Fatal(err)
	}
	required := waitForEvent(t, client, "tool.confirmation_required")
	if _, offered := required["remember_pattern"]; offered {
		t.Fatalf("a compound offered a rule: %v", required)
	}
	if err := client.Call("session.confirm", map[string]bool{"approved": false}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "session.finished")
}

// A decline never writes a rule, whatever scope was asked for: shell_allow
// has no negative form, and a quiet deny rule is a permission change nobody
// asked for.
func TestDecliningNeverWritesARule(t *testing.T) {
	client, paths := startRememberDaemon(t, "zzprobe status")
	before, err := os.ReadFile(paths.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Call("session.text", map[string]string{"text": "probe"}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "tool.confirmation_required")
	if err := client.Call("session.confirm",
		map[string]any{"approved": false, "remember": "always"}, nil); err == nil {
		t.Fatal("a decline was allowed to add a rule")
	}
	if err := client.Call("session.confirm", map[string]bool{"approved": false}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "session.finished")
	after, err := os.ReadFile(paths.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("a decline changed config.toml:\n%s", after)
	}
}

// An unknown scope word is a rejected request, never a silently different
// grant.
func TestAnUnknownScopeIsRejected(t *testing.T) {
	client, _ := startRememberDaemon(t, "zzprobe status")
	if err := client.Call("session.text", map[string]string{"text": "probe"}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "tool.confirmation_required")
	err := client.Call("session.confirm", map[string]any{"approved": true, "remember": "forever"}, nil)
	if err == nil || !strings.Contains(err.Error(), "is invalid") {
		t.Fatalf("remember=\"forever\" was accepted: %v", err)
	}
	if err := client.Call("session.confirm", map[string]bool{"approved": false}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "session.finished")
}

// A card cannot name its own rule: session.confirm takes a scope word and the
// daemon derives the pattern from what it published. This is the pin that
// keeps a model that can reach a client from choosing what gets granted.
func TestAClientCannotNameTheRule(t *testing.T) {
	client, paths := startRememberDaemon(t, "zzprobe status")
	if err := client.Call("session.text", map[string]string{"text": "probe"}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "tool.confirmation_required")
	// Every shape a hopeful client might try. None of them may reach the file.
	if err := client.Call("session.confirm", map[string]any{
		"approved": true, "remember": "always",
		"pattern": "sudo", "remember_pattern": "rm", "rule": "curl",
	}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "tool.confirmed")
	waitForEvent(t, client, "session.finished")

	raw, err := os.ReadFile(paths.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `shell_allow = ["zzprobe status"]`) {
		t.Errorf("the written rule is not the one the daemon derived:\n%s", raw)
	}
	for _, forged := range []string{`"sudo"`, `"rm"`, `"curl"`} {
		if strings.Contains(string(raw), "shell_allow") &&
			strings.Contains(strings.SplitN(string(raw), "shell_allow", 2)[1], forged) {
			t.Errorf("a client-supplied pattern reached shell_allow:\n%s", raw)
		}
	}
}

// #109's exclusion wall stands: the assistant's settings surface still has no
// key that reaches [tools.policy], so the only writer of a rule is a human.
func TestTheAssistantStillCannotReachTheGate(t *testing.T) {
	for _, key := range []string{
		"tools.policy", "tools.policy.shell_allow", "tools.policy.shell_deny",
		"tools.policy.default", "tools.policy.tool",
	} {
		reason, excluded := config.AssistantExcludedSettingReason(key)
		if !excluded {
			t.Errorf("%s is no longer excluded from the assistant's settings surface", key)
		}
		if !strings.Contains(reason, "may not change the tool permission policy") {
			t.Errorf("%s excluded with an unexpected reason: %q", key, reason)
		}
	}
	for _, s := range config.AssistantSettings() {
		if strings.HasPrefix(s.Key, "tools.policy") {
			t.Errorf("the assistant's settings registry now contains %q", s.Key)
		}
	}
	// …and shell_allow is not a registry key at all, so even the human
	// settings screen cannot reach it: the writer in approvals.go drives
	// RewriteTOML directly, which is why this key must stay absent.
	if _, ok := config.SettingFor(shellAllowKey); ok {
		t.Errorf("%s became a settings-registry key; the exclusion wall's shape changed", shellAllowKey)
	}
}

// approvals.list and approvals.forget: visible and revocable, and revoking
// takes effect on the running gate without a restart.
func TestApprovalsAreListableAndRevocable(t *testing.T) {
	client, paths := startRememberDaemon(t,
		"zzprobe status", "zzprobe status --no-stream", "", "zzprobe status --format x")

	if err := client.Call("session.text", map[string]string{"text": "probe"}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "tool.confirmation_required")
	if err := client.Call("session.confirm",
		map[string]any{"approved": true, "remember": "always"}, nil); err != nil {
		t.Fatal(err)
	}
	// The second round fires the rule; the ledger counts it.
	waitForEvent(t, client, "tool.pre_approved")
	waitForEvent(t, client, "session.finished")

	var listing struct {
		Path     string           `json:"path"`
		Approved []map[string]any `json:"approved"`
	}
	if err := client.Call("approvals.list", nil, &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Approved) != 1 {
		t.Fatalf("listing = %+v, want one entry", listing.Approved)
	}
	row := listing.Approved[0]
	if row["pattern"] != "zzprobe status" || row["source"] != "card" {
		t.Errorf("row = %+v", row)
	}
	if row["added"] == nil {
		t.Errorf("row = %+v, want the date it was agreed to", row)
	}
	if listing.Path != paths.ConfigFile() {
		t.Errorf("path = %q, want the config file the rules live in", listing.Path)
	}

	if uses, _ := listing.Approved[0]["uses"].(float64); uses != 1 {
		t.Errorf("uses = %v, want 1", listing.Approved[0]["uses"])
	}

	// Revoke it: the file loses the rule and so does the running gate.
	var forgot map[string]any
	if err := client.Call("approvals.forget",
		map[string]string{"pattern": "zzprobe  status"}, &forgot); err != nil {
		t.Fatal(err)
	}
	if forgot["forgotten"] != true {
		t.Fatalf("forget = %v", forgot)
	}
	raw, err := os.ReadFile(paths.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "zzprobe status") {
		t.Errorf("the revoked rule is still in config.toml:\n%s", raw)
	}
	// And the running gate asks again on the very next command — no restart:
	// a revocation that waits is a revocation that has not happened.
	if err := client.Call("session.text", map[string]string{"text": "once more"}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "tool.confirmation_required")
	if err := client.Call("session.confirm", map[string]bool{"approved": false}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "session.finished")

	if err := client.Call("approvals.list", nil, &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Approved) != 0 {
		t.Errorf("the revoked rule is still listed: %+v", listing.Approved)
	}
}

// A hand edit that removes a rule takes effect on config.reload, without a
// daemon restart. Tightening the gate must never wait.
func TestAHandEditedRevocationAppliesOnReload(t *testing.T) {
	client, paths := startRememberDaemon(t, "zzprobe status", "", "zzprobe status --no-stream")

	if err := client.Call("session.text", map[string]string{"text": "probe"}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "tool.confirmation_required")
	if err := client.Call("session.confirm",
		map[string]any{"approved": true, "remember": "always"}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "tool.confirmed")
	waitForEvent(t, client, "session.finished")

	// The hand edit: shell_allow emptied with an editor.
	raw, err := os.ReadFile(paths.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.ReplaceAll(string(raw), `shell_allow = ["zzprobe status"]`, `shell_allow = []`)
	if edited == string(raw) {
		t.Fatalf("the rule was not where the edit expected it:\n%s", raw)
	}
	if err := os.WriteFile(paths.ConfigFile(), []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := client.Call("config.reload", nil, nil); err != nil {
		t.Fatal(err)
	}

	// The gate asks again — no restart involved.
	if err := client.Call("session.text", map[string]string{"text": "again"}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "tool.confirmation_required")
	if err := client.Call("session.confirm", map[string]bool{"approved": false}, nil); err != nil {
		t.Fatal(err)
	}
	waitForEvent(t, client, "session.finished")

	var listing struct {
		Approved []map[string]any `json:"approved"`
	}
	if err := client.Call("approvals.list", nil, &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Approved) != 0 {
		t.Errorf("the reload did not reach the listing: %+v", listing.Approved)
	}
}

// A rule written on top of a file that changed underneath is refused rather
// than clobbering the hand edit — the settings surface's guard, applied to
// the one file where losing an edit changes what may run.
func TestWritingOverAChangedFileIsRefused(t *testing.T) {
	dir := t.TempDir()
	paths := config.Paths{Config: dir, Data: dir, State: dir, Runtime: dir,
		Socket: filepath.Join(dir, "j.sock")}
	if err := os.WriteFile(paths.ConfigFile(), []byte(handWrittenConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{paths: paths}
	err := d.writeApprovalList(approvalListAllow, []string{"zzprobe status"}, "sha256:not-what-is-on-disk")
	if err == nil {
		t.Fatal("a stale fingerprint was accepted")
	}
	var ipcErr *ipc.Error
	if !asIPCError(err, &ipcErr) || ipcErr.Code != ipc.CodeConfigConflict {
		t.Fatalf("error = %v, want a config-conflict", err)
	}
	raw, err := os.ReadFile(paths.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != handWrittenConfig {
		t.Errorf("the file was written despite the conflict:\n%s", raw)
	}
}

// asIPCError unwraps an ipc.Error, if that is what err is.
func asIPCError(err error, out **ipc.Error) bool {
	e, ok := err.(*ipc.Error)
	if ok {
		*out = e
	}
	return ok
}

// The spoken listing: what "what have I pre-approved?" says, in the three
// states it can be asked in.
func TestSpokenApprovalsListing(t *testing.T) {
	store := newApprovalStore(nil, filepath.Join(t.TempDir(), "approvals.toml"), nil)
	voice := &approvalsVoice{store: store}

	spoken, err := voice.SpokenApprovals(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(spoken, "not pre-approved anything") {
		t.Errorf("empty listing = %q", spoken)
	}

	spoken, err = voice.SpokenApprovals([]string{"zzprobe status"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(spoken, "Nothing permanently") || !strings.Contains(spoken, "zzprobe status") {
		t.Errorf("conversation-only listing = %q", spoken)
	}

	store.setPatterns([]string{"kubectl get pods", "zzprobe status"})
	spoken, err = voice.SpokenApprovals([]string{"jq"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"2 commands", "zzprobe status", "kubectl get pods",
		"Just for this conversation, also jq", "forget",
	} {
		if !strings.Contains(spoken, want) {
			t.Errorf("listing %q is missing %q", spoken, want)
		}
	}

	// Past the cap the sentence says so rather than reading a paragraph.
	many := []string{"a1", "b2", "c3", "d4", "e5", "f6", "g7", "h8"}
	store.setPatterns(many)
	spoken, err = voice.SpokenApprovals(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(spoken, "8 commands") || !strings.Contains(spoken, "Approvals tab") {
		t.Errorf("capped listing = %q", spoken)
	}
	if strings.Contains(spoken, "h8") {
		t.Errorf("capped listing read past the cap: %q", spoken)
	}
}

// ---------------------------------------------------------------- #164

// approvalsSnapshot reads both lists off approvals.list.
func approvalsSnapshot(t *testing.T, client *ipc.Client) (allow, deny []string) {
	t.Helper()
	var out struct {
		Approved []struct {
			Pattern string `json:"pattern"`
			Scope   string `json:"scope"`
		} `json:"approved"`
		Denied []struct {
			Pattern string `json:"pattern"`
		} `json:"denied"`
	}
	if err := client.Call("approvals.list", nil, &out); err != nil {
		t.Fatal(err)
	}
	for _, row := range out.Approved {
		if row.Scope == string(tools.RememberAlways) {
			allow = append(allow, row.Pattern)
		}
	}
	for _, row := range out.Denied {
		deny = append(deny, row.Pattern)
	}
	return allow, deny
}

// TestAddingAnAllowRuleByHandFacesTheCardsRefusalMatrix is the #164 acceptance
// criterion that this whole surface stands on.
//
// The refusal matrix is what makes a standing grant acceptable at all (ADR
// 0053), and a second route to `shell_allow` that judged patterns by its own
// rules would be a second, weaker gate wearing the same name. So the daemon
// imports the matrix rather than restating it, and this pins BOTH halves of
// that: the refusals come back in the matrix's own words, and the shapes it
// refuses are exactly the shapes the card refuses — checked by asking the same
// policy both questions and comparing.
func TestAddingAnAllowRuleByHandFacesTheCardsRefusalMatrix(t *testing.T) {
	client, paths := startRememberDaemon(t)

	for _, tc := range []struct {
		pattern string
		reason  string
	}{
		// Group 1: a risk word. Refused because a rule naming one would be
		// INERT — the classifier checks risk words before the allow list.
		{"rm", "always asks"},
		// Group 2: a binary whose danger lives in its flags, and a command
		// wrapper, each with its own clause.
		{"find", "-delete and -exec"},
		{"timeout 5", "runs whatever command follows it"},
		// Group 3, both directions: the shape itself, and a proposal that would
		// cover one.
		{"docker exec", "runs a command inside a container"},
		{"docker", "would also cover"},
		// A path-invoked command: the reason is time, not shape.
		{"./deploy.sh", "what a file contains can change"},
		// The configured deny list wins over an allow, in both directions.
		{"httpie", "deny rule always wins"},
		// A flag is not a command word, and a rule that stopped before it would
		// be WIDER than the one that was typed — so it is refused rather than
		// silently truncated, which is where the typed path differs from the
		// derived one. (The head is deliberately a binary the matrix knows
		// nothing about, so the refusal is about the flag and not about `git`.)
		{"zzprobe status --json", "not a command word"},
		// An empty rule matches everything.
		{"   ", "type the leading words"},
	} {
		var rpcErr *ipc.Error
		err := client.Call("approvals.add",
			map[string]any{"pattern": tc.pattern, "list": "allow"}, nil)
		if !errors.As(err, &rpcErr) {
			t.Errorf("%q was accepted: %v", tc.pattern, err)
			continue
		}
		if !strings.Contains(rpcErr.Message, tc.reason) {
			t.Errorf("%q refused with %q, want it to say %q", tc.pattern, rpcErr.Message, tc.reason)
		}
		// Field-keyed, so the form pins it under the input that typed it.
		data, _ := rpcErr.Data.(map[string]any)
		if msg, ok := problemOn(entryProblemList(t, data), "pattern"); !ok || msg == "" {
			t.Errorf("%q: problems = %v, want one on the pattern field", tc.pattern, rpcErr.Data)
		}
	}

	// Nothing was written by any of that.
	if allow, _ := approvalsSnapshot(t, client); len(allow) != 0 {
		t.Errorf("allow list = %v after nine refusals", allow)
	}
	raw, err := os.ReadFile(paths.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != handWrittenConfig {
		t.Errorf("a refused add rewrote the file:\n%s", raw)
	}

	// And an offerable rule lands, byte-preservingly, and the running gate
	// holds it immediately.
	added := map[string]any{}
	if err := client.Call("approvals.add",
		map[string]any{"pattern": "zzprobe status", "list": "allow"}, &added); err != nil {
		t.Fatal(err)
	}
	if added["added"] != true || added["pattern"] != "zzprobe status" {
		t.Errorf("receipt = %v", added)
	}
	allow, _ := approvalsSnapshot(t, client)
	if strings.Join(allow, ",") != "zzprobe status" {
		t.Errorf("allow list = %v", allow)
	}
	raw, _ = os.ReadFile(paths.ConfigFile())
	if !strings.Contains(string(raw), "# I like being asked about most things.") ||
		!strings.Contains(string(raw), `level = "info"   # trailing comment`) {
		t.Errorf("the hand-written file did not survive:\n%s", raw)
	}
	// A rule already covered by a standing one is refused rather than silently
	// duplicated — the same clause the card gives when it reaches the same
	// state, and more useful than a no-op that looks like it worked. The
	// whitespace is collapsed first, so "zzprobe  status" is the same rule.
	var dupErr *ipc.Error
	err = client.Call("approvals.add",
		map[string]any{"pattern": "zzprobe  status", "list": "allow"}, nil)
	if !errors.As(err, &dupErr) ||
		!strings.Contains(dupErr.Message, "already on your allow list") {
		t.Errorf("a duplicate add reported %v", err)
	}
	if allow, _ := approvalsSnapshot(t, client); len(allow) != 1 {
		t.Errorf("allow list = %v after a refused duplicate", allow)
	}
}

// TestTheTypedAndDerivedRoutesShareOneMatrix compares the two routes directly,
// over the policy's own tables, so a shape added to one and not the other is a
// test failure rather than a discovery.
func TestTheTypedAndDerivedRoutesShareOneMatrix(t *testing.T) {
	policy, err := tools.NewPolicy(tools.PolicyConfig{
		Default:   tools.PolicyAsk,
		ShellDeny: []string{"httpie post"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Commands whose DERIVED pattern the card refuses; the same pattern typed
	// by hand must be refused too, with the same sentence.
	for _, command := range []string{
		"rm -rf ~/tmp", "find . -name '*.go'", "timeout 5 make deploy",
		"docker run alpine", "docker version", "./deploy.sh", "httpie post /x",
		"git status", "npm test", "jarvix approvals list",
	} {
		derived := policy.RememberOfferFor(tools.Verdict{
			Decision: tools.PolicyAsk, Tool: "shell.run", Command: command,
		})
		if derived.Offered {
			continue // the card would offer it; nothing to compare
		}
		// Type the same leading words the card would have proposed.
		typed := policy.VetAllowPattern(strings.Fields(command)[0])
		if typed.Offered {
			t.Errorf("%q: the card refuses it (%q) but a hand-typed rule was accepted",
				command, derived.Reason)
		}
	}
	// And the offerable ones stay offerable from both directions, or the
	// feature does nothing for the commands it exists for.
	for _, pattern := range []string{"docker ps", "docker compose ps", "kubectl get pods", "xdg-open"} {
		if offer := policy.VetAllowPattern(pattern); !offer.Offered {
			t.Errorf("%q refused by hand: %s", pattern, offer.Reason)
		}
	}
}

// TestAddingADenyRuleIsNeverArgued: tightening the gate has no matrix, because
// a gate that argued with someone making it stricter is a gate people route
// around. What it does carry is a report of the standing grants it now beats.
func TestAddingADenyRuleIsNeverArgued(t *testing.T) {
	client, paths := startRememberDaemon(t)
	if err := client.Call("approvals.add",
		map[string]any{"pattern": "docker ps", "list": "allow"}, nil); err != nil {
		t.Fatal(err)
	}

	added := map[string]any{}
	// `docker` heads the refusal matrix for an ALLOW rule; as a deny it is
	// exactly what someone tightening the gate would type.
	if err := client.Call("approvals.add",
		map[string]any{"pattern": "docker", "list": "deny"}, &added); err != nil {
		t.Fatal(err)
	}
	if added["added"] != true {
		t.Fatalf("receipt = %v", added)
	}
	shadows, _ := added["shadows"].([]any)
	if len(shadows) != 1 || shadows[0] != "docker ps" {
		t.Errorf("shadows = %v, want the allow rule it now beats", added["shadows"])
	}
	_, deny := approvalsSnapshot(t, client)
	if strings.Join(deny, ",") != "httpie post,docker" {
		t.Errorf("deny list = %v", deny)
	}
	raw, _ := os.ReadFile(paths.ConfigFile())
	if !strings.Contains(string(raw), `shell_deny = ["httpie post", "docker"]`) {
		t.Errorf("file:\n%s", raw)
	}

	// The list name is required, so a request that forgot the field cannot
	// widen the gate by omission.
	var rpcErr *ipc.Error
	err := client.Call("approvals.add", map[string]any{"pattern": "ls"}, nil)
	if !errors.As(err, &rpcErr) || !strings.Contains(rpcErr.Message, "list") {
		t.Errorf("a listless add was accepted: %v", err)
	}
}

// TestRemovingADenyRuleRequiresAConfirmationNamingWhatItProtected is the
// asymmetry #164 asks for, and the reason for it: removing an ALLOW rule
// narrows what runs unasked and is answered immediately, while removing a DENY
// rule widens it by deleting a protection whose whole job is that nothing has
// been happening — no ledger row, no activity entry, nothing to remind anyone
// what it has been stopping.
func TestRemovingADenyRuleRequiresAConfirmationNamingWhatItProtected(t *testing.T) {
	client, paths := startRememberDaemon(t)
	if err := client.Call("approvals.add",
		map[string]any{"pattern": "zzprobe status", "list": "allow"}, nil); err != nil {
		t.Fatal(err)
	}

	// The unconfirmed call removes nothing and comes back with the sentence.
	// Every call decodes into a FRESH map: unmarshalling into one that already
	// has keys merges rather than replaces, and a stale key from the previous
	// answer would make this test pass for the wrong reason.
	out := map[string]any{}
	if err := client.Call("approvals.forget",
		map[string]any{"pattern": "httpie post", "list": "deny"}, &out); err != nil {
		t.Fatal(err)
	}
	if out["forgotten"] != false || out["confirm_required"] != true {
		t.Fatalf("an unconfirmed deny removal reported %v", out)
	}
	confirmation, _ := out["confirmation"].(string)
	for _, want := range []string{
		`"httpie post"`,                          // the rule, verbatim
		"refuses every command beginning",        // what it protects
		"deny always wins",                       // and that it beat the allow list
		"they will ask instead of being refused", // what happens without it
		"approve and don't ask again",            // including how it could then go silent
		"Confirm to remove it.",
	} {
		if !strings.Contains(confirmation, want) {
			t.Errorf("confirmation %q is missing %q", confirmation, want)
		}
	}
	if _, deny := approvalsSnapshot(t, client); strings.Join(deny, ",") != "httpie post" {
		t.Errorf("deny list = %v after an unconfirmed removal", deny)
	}
	raw, _ := os.ReadFile(paths.ConfigFile())
	if !strings.Contains(string(raw), `shell_deny = ["httpie post"]`) {
		t.Errorf("an unconfirmed removal touched the file:\n%s", raw)
	}

	// Confirmed, it goes.
	out = map[string]any{}
	if err := client.Call("approvals.forget",
		map[string]any{"pattern": "httpie post", "list": "deny", "confirmed": true}, &out); err != nil {
		t.Fatal(err)
	}
	if out["forgotten"] != true {
		t.Errorf("a confirmed removal reported %v", out)
	}
	if _, deny := approvalsSnapshot(t, client); len(deny) != 0 {
		t.Errorf("deny list = %v after a confirmed removal", deny)
	}

	// And removing an ALLOW rule still needs no confirmation at all: the
	// asymmetry is the whole point.
	out = map[string]any{}
	if err := client.Call("approvals.forget",
		map[string]any{"pattern": "zzprobe status"}, &out); err != nil {
		t.Fatal(err)
	}
	if out["forgotten"] != true || out["confirm_required"] != nil {
		t.Errorf("forgetting an allow rule asked a question: %v", out)
	}
	if allow, _ := approvalsSnapshot(t, client); len(allow) != 0 {
		t.Errorf("allow list = %v", allow)
	}
}
