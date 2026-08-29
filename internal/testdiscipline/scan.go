// Package testdiscipline is the machine-checkable half of two test-writing
// rules this repo learned the expensive way, in the same week.
//
// Both rules exist because `go test -race -count=2` — the PR gate — passes
// cleanly on code that carries them. They are ordering faults, and neither
// statement coverage nor a two-run race pass measures ordering. The soak job
// (.github/workflows/soak.yml) is the other half of the answer: it re-runs the
// concurrency-prone packages enough times to make a probabilistic fault
// likely. This package is the deterministic half — it catches the two shapes
// that can be recognised by reading the source, on the PR that introduces
// them, instead of at 04:00 the following morning.
//
// The scans are AST-based rather than the text scan used for the QML guards in
// internal/desktop. That is not a style difference: QML cannot be parsed by
// anything in this module, so a text scan is all a Go test can do to it,
// whereas Go source can be read exactly. Precision matters more here than
// anywhere else in the suite, because a guard that cries wolf gets deleted —
// so every rule below is written to fire on the historical shape and to stay
// silent on every legitimate use of the same call that exists in the tree
// today. Where a rule cannot tell the two apart, it does not fire.
package testdiscipline

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Finding is one violation, located precisely enough to jump to.
type Finding struct {
	File    string // path as handed to the scanner
	Line    int
	Func    string // the enclosing function or method, for the message
	Message string
	// Key identifies a fake-field finding as "package.Type.Field", which is
	// how FakeFieldExemptions is keyed. Empty for the derived-state rule,
	// whose findings are locations rather than named things.
	Key string
}

func (f Finding) String() string {
	return fmt.Sprintf("%s:%d: %s: %s", f.File, f.Line, f.Func, f.Message)
}

// AllowMarker opts a single function out of the derived-state scan. It must be
// followed by a reason on the same comment line — an unexplained opt-out is
// itself reported, because the whole value of the rule is that the exceptions
// are argued rather than accumulated.
const AllowMarker = "testdiscipline:allow"

// ---------------------------------------------------------------------------
// The derived-state rule
// ---------------------------------------------------------------------------

// derivedRule describes one derived read and the ordering it depends on.
//
// The shape it catches, in the abstract: a test does something, observes the
// *cause* of a state change, and then samples the state. The observation
// proves the cause happened. It does not prove the effect has landed, because
// something else — a watcher goroutine, a tail of a flush — is what lands it.
// The test then passes on an idle laptop and fails on a loaded runner, which
// is the worst failure mode there is: it teaches the author that the gate is
// flaky rather than that the test is wrong.
//
// Reads is the sampling call. Causes are the calls that observe only the
// cause; the rule fires only when one of them was reached first, which is what
// keeps it off the many tests that read the same state synchronously, with
// nothing asynchronous in play at all. Barriers are the calls that genuinely
// establish the ordering; any one of them before the read silences the rule.
type derivedRule struct {
	Name     string
	Reads    []string
	Causes   []string
	Barriers []string
	// BarrierBodies, when set, extends Barriers with every function in the
	// same package whose own body calls one of these. That is how a helper
	// that takes the barrier on its caller's behalf counts as one — see
	// session.harness.awaitAppend, which is exactly that.
	BarrierBodies []string
	// Issue is the citation the finding carries. The author reading the report
	// in CI has none of this context, and the fastest route to the reasoning is
	// the issue the shape cost us.
	Issue  string
	Advice string
}

// derivedRules is deliberately short. Every entry is a shape that has actually
// failed in this repo, with the issue number in its advice; nothing is here on
// the theory that it might.
//
// Two reads that look like obvious candidates are deliberately absent:
//
//   - conversation.get. It reads the engine directly and an exchange is
//     committed before session.finished publishes, so a client that has seen
//     the event can always read the record (the reasoning is written out at
//     askAndRead in internal/daemon/provenance_test.go). A rule over it would
//     be pure false positives — a dozen of them in the tree today — and would
//     be deleted within a week, taking the useful rules with it.
//   - status.get. Same argument: it is served from state the caller's own
//     request already ordered.
//
// The distinction that matters is not "is this a read?" but "is this read
// served by a goroutine other than the one whose event I waited for?".
var derivedRules = []derivedRule{
	{
		// #167. Activity rows are derived by the daemon's own subscriber from
		// the events it watches, so an event proves the daemon spoke and never
		// that the row has been appended — docs/ipc.md says exactly this of
		// every activity.row. A `tools: approve and don't ask again` test
		// sampled activity.get on tool.pre_approved's heels: green locally,
		// red on a starved CI runner.
		Name:   "activity feed row read after only its cause",
		Reads:  []string{`Call("activity.get")`, "activityRowsOf"},
		Causes: []string{"waitForEvent", "waitEvent", "awaitBus"},
		Barriers: []string{
			"waitForActivityRow", "waitActivityRow",
			"waitForRunObserved", "waitForRowsAndSessionEnd",
		},
		Issue: "#167",
		Advice: "wait for the row itself (waitForActivityRow / waitActivityRow), " +
			"not for the event the daemon's watcher derives it from",
	},
	{
		// #170. conversations.Fake notifies Ops from *inside* Append, so the
		// op proves the turns are stored — it does not prove the engine has
		// adopted the id that append minted, because persistArchive does that
		// after Append returns. TestResetDetachesAndTheNextThreadIsANew
		// Conversation was exactly that shape and failed 2 times in 100
		// whole-package `-race -count=50` runs.
		//
		// Shutdown is a barrier because it drains the archive before it
		// returns; the three shutdown tests in archive_test.go read the id
		// with no append-observation at all and so never reach this rule
		// anyway, but naming it keeps the rule honest about what orders what.
		Name:          "archived conversation id read after only the append",
		Reads:         []string{"ActiveConversationID"},
		Causes:        []string{"awaitAppend", "awaitArchiveAppend"},
		Barriers:      []string{"SyncArchive", "Shutdown"},
		BarrierBodies: []string{"SyncArchive"},
		Issue:         "#170",
		Advice: "take the engine's read barrier (SyncArchive) before reading the id — " +
			"the append lands the turns, the adoption happens after Append returns",
	},
	{
		// #215. assistant.started is published at engine.go:1628 — before the
		// provider request is opened (streamOnce reaches prov.Chat at
		// engine.go:1891) and before a single word is committed to the voice.
		// It proves think() reached that line and nothing more.
		//
		// Two tests took it as proof that the turn was still in flight and then
		// acted on that belief. TestWakeWordInterruptsSpeech woke the engine
		// while, on a starved runner, the whole turn had already finished, so
		// startSessionLocked had nothing to cancel and the session.cancelled it
		// waited for was never owed. TestInterruptBeforeAnyAnswerCommitsThe
		// Question let a second session claim the fake's "first Chat call"
		// parked branch, so the answer it waited for never came. Both timed out
		// on CI and passed everywhere else, three times over.
		//
		// Reads are actions, not samples — that is what is new here. Cancelling,
		// waking, superseding or reading the in-flight conversation are all
		// things a test does *because* it believes a turn is still running, and
		// assistant.started is the one event that cannot support that belief.
		//
		// The barriers are the two that hold regardless of scheduling: park the
		// synthesizer (tts.Fake.SetHold) so the speaker cannot drain, or stall
		// the provider outright (Delay = time.Hour) so no chunk can ever be
		// produced. tts.started and assistant.delta are the weaker pair — they
		// prove speech is playing and that the provider stream is open, which
		// is what the repo already writes down at session/text_test.go:88 — and
		// they are accepted because they are strictly better evidence than the
		// event they replace. A bounded Delay is deliberately NOT a barrier:
		// `Delay = 50 * time.Millisecond` is a window, and a window is what
		// this whole family is made of.
		Name:  "turn-in-flight action ordered by only assistant.started",
		Reads: []string{"StartWake", "StartSession", "Cancel", "CancelSpeech", "ReplaySpeech", "Conversation"},
		Causes: []string{
			`waitFor("assistant.started")`,
			`collectUntil("assistant.started")`,
		},
		Barriers: []string{
			"SetHold",
			"Delay = time.Hour",
			`waitFor("assistant.delta")`,
			`waitFor("tts.started")`,
			`collectUntil("assistant.delta")`,
			`collectUntil("tts.started")`,
		},
		Issue: "#215",
		Advice: "assistant.started fires before the provider call and before speech, so it cannot " +
			"prove a turn is still running; park the collaborator (tts.Fake.SetHold, Delay = time.Hour, " +
			"a provider that signals when it has parked) or wait for tts.started / assistant.delta",
	},
}

// ScanDerivedState reports reads of asynchronously-derived state that were
// ordered by observing only the cause. files are Go source paths; anything
// that is not a _test.go file is ignored, because the rule is about how tests
// synchronise and production code has no waitForEvent to misuse.
func ScanDerivedState(files []string) ([]Finding, error) {
	fset := token.NewFileSet()
	parsed, err := parseAll(fset, files)
	if err != nil {
		return nil, err
	}

	// Barrier helpers resolve per directory, which for this module is per
	// package: a helper counts as a barrier for the callers that can actually
	// see it. Resolution is by name only — a method and a plain function
	// sharing a name in one package are not told apart, which is the one place
	// this scan is deliberately permissive. Both spellings of awaitAppend in
	// internal/session are such a pair; the alternative is a type checker, and
	// the cost of a missed report here is one soak run, while the cost of a
	// false one is the whole rule.
	barriersByDir := map[string]map[string]bool{}
	for _, f := range parsed {
		dir := filepath.Dir(f.path)
		if barriersByDir[dir] == nil {
			barriersByDir[dir] = map[string]bool{}
		}
		for _, decl := range f.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			for _, rule := range derivedRules {
				if callsAny(fn.Body, rule.BarrierBodies) {
					barriersByDir[dir][fn.Name.Name] = true
				}
			}
		}
	}

	var findings []Finding
	for _, f := range parsed {
		if !strings.HasSuffix(f.path, "_test.go") {
			continue
		}
		for _, decl := range f.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			reason, marked := allowReason(f, fn)
			if marked {
				if reason == "" {
					findings = append(findings, Finding{
						File: f.path, Line: fset.Position(fn.Pos()).Line, Func: fn.Name.Name,
						Message: AllowMarker + " carries no reason; say why this read is ordered",
					})
				}
				continue
			}
			findings = append(findings,
				scanFuncForDerivedReads(fset, f, fn, barriersByDir[filepath.Dir(f.path)])...)
		}
	}
	sortFindings(findings)
	return findings, nil
}

// scanFuncForDerivedReads walks one function body in source order, which is
// the only order that matters: "did a barrier happen before this read?" is a
// question about the text, and a test that reads first and synchronises after
// is wrong however the calls are nested.
func scanFuncForDerivedReads(fset *token.FileSet, f parsedFile, fn *ast.FuncDecl, dirBarriers map[string]bool) []Finding {
	calls := callSequence(fn.Body)

	var findings []Finding
	for _, rule := range derivedRules {
		seenCause := false
		seenBarrier := false
		for _, c := range calls {
			switch {
			case contains(rule.Barriers, c.name):
				seenBarrier = true
			case c.kind == siteAssign:
				// An assignment can establish an ordering (`Delay = time.Hour`
				// stalls the provider for the rest of the test) but it can
				// never *be* the cause or the read, so it stops here rather
				// than falling through to them.
			case dirBarriers[c.name]:
				seenBarrier = true
			case contains(rule.Causes, c.name):
				seenCause = true
			case contains(rule.Reads, c.name) && seenCause && !seenBarrier:
				findings = append(findings, Finding{
					File: f.path, Line: fset.Position(c.pos).Line, Func: fn.Name.Name,
					Message: fmt.Sprintf("%s: %s. %s (see %s; %s with a reason opts out)",
						rule.Name, c.name, rule.Advice, rule.Issue, AllowMarker),
				})
			}
		}
	}
	return findings
}

// ---------------------------------------------------------------------------
// The test-fake rule
// ---------------------------------------------------------------------------

// FakeFieldExemptions names fields that predate this rule. It is a ratchet in
// the same spirit as the coverage floor: the list may shrink and must never
// grow, and each entry says what the fix is rather than pretending the field
// is fine.
//
// These three are the real thing — the #149 shape exactly, and they are here
// because unpicking them is a hundred-odd mechanical call-site edits across
// twenty-five files, which does not belong in the change that introduces the
// guard. The fix pattern is tts.Fake.Last(): unexport the field, add an
// accessor that takes the same mutex the write does, and the unsynchronised
// read becomes a compile error rather than a flake.
var FakeFieldExemptions = map[string]string{
	"ai.Fake.LastRequest": "pre-existing (#171); unexport behind LastRequest() as tts.Fake.Last() did",
	"ai.Fake.Requests":    "pre-existing (#171); unexport behind Requests() as tts.Fake.Last() did",
	"stt.Fake.LastInput":  "pre-existing (#171); unexport behind LastInput() as tts.Fake.Last() did",
}

// fakeTypeMarkers are the name fragments that mark a type as test scaffolding.
// The rule is scoped by NAME on purpose. "Is this type only used by tests?" is
// a whole-program question a source scan cannot answer, but "does its name say
// fake?" is one the author already answered, and it is the population the
// #149 defect lives in.
var fakeTypeMarkers = []string{"Fake", "Stub", "Mock", "Spy"}

// ScanFakeFields reports exported fields on fake/stub types that the type's
// own methods write.
//
// The precision argument is the whole design. Fakes in this repo carry a lot
// of exported fields and nearly all of them are fine: Response, Fail, Chunks,
// BeforeToolCalls and their kind are *scripting* — written once by the test
// goroutine at construction and only read afterwards. Flagging those would
// condemn every fake in the tree and the rule would last a day.
//
// The dangerous ones are the fields the fake writes about itself while it is
// being driven: tts.Fake had LastRequest, assigned inside Speak, and #149 was
// a test goroutine reading it while a production goroutine wrote it — two
// sessions can be inside Speak at once since #111. So the rule is "exported
// AND assigned by one of the type's own methods", which is precisely the
// recording-field population and excludes every scripting field, because a
// scripting field is never assigned by the fake.
//
// Channel and func fields are excluded even when a method writes them: a send
// on a channel is not a data race, and the notifying-fake pattern this repo
// uses everywhere (conversations.Fake.Ops, history.Fake.SaveGate) depends on
// those channels being reachable from the test.
func ScanFakeFields(files []string) ([]Finding, error) {
	return scanFakeFields(files, false)
}

// ScanFakeFieldsIncludingExempt is the same scan with the exemption list
// ignored. It exists so a test can prove every exemption still names something
// real — a ratchet whose entries are never re-checked is just a list.
func ScanFakeFieldsIncludingExempt(files []string) ([]Finding, error) {
	return scanFakeFields(files, true)
}

func scanFakeFields(files []string, includeExempt bool) ([]Finding, error) {
	fset := token.NewFileSet()
	parsed, err := parseAll(fset, files)
	if err != nil {
		return nil, err
	}

	var findings []Finding
	for _, f := range parsed {
		fakes := fakeStructs(f.file)
		if len(fakes) == 0 {
			continue
		}
		for _, decl := range f.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Recv == nil || len(fn.Recv.List) == 0 {
				continue
			}
			recvName, typeName := receiverOf(fn)
			fields, ok := fakes[typeName]
			if !ok || recvName == "" {
				continue
			}
			for _, w := range writesToReceiverFields(fn.Body, recvName) {
				if !fields[w.field] {
					continue
				}
				key := f.file.Name.Name + "." + typeName + "." + w.field
				if _, exempt := FakeFieldExemptions[key]; exempt && !includeExempt {
					continue
				}
				findings = append(findings, Finding{
					File: f.path, Line: fset.Position(w.pos).Line,
					Func: typeName + "." + fn.Name.Name,
					Key:  key,
					Message: fmt.Sprintf(
						"%s.%s is exported and written by the fake itself: a test goroutine can read it "+
							"while a production goroutine writes it (#149, tts.Fake.LastRequest). "+
							"Unexport it and add an accessor that takes the same mutex the write does, "+
							"as tts.Fake.Last() does",
						typeName, w.field),
				})
			}
		}
	}
	sortFindings(findings)
	return findings, nil
}

// fakeStructs returns, per fake-named struct in the file, the set of its
// exported fields that are worth guarding (see ScanFakeFields on why channels
// and funcs are not).
func fakeStructs(file *ast.File) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.TypeSpec)
		if !ok || !isFakeName(spec.Name.Name) {
			return true
		}
		st, ok := spec.Type.(*ast.StructType)
		if !ok {
			return true
		}
		fields := map[string]bool{}
		for _, field := range st.Fields.List {
			switch field.Type.(type) {
			case *ast.ChanType, *ast.FuncType:
				continue
			}
			for _, name := range field.Names {
				if name.IsExported() {
					fields[name.Name] = true
				}
			}
		}
		if len(fields) > 0 {
			out[spec.Name.Name] = fields
		}
		return true
	})
	return out
}

func isFakeName(name string) bool {
	for _, marker := range fakeTypeMarkers {
		if strings.Contains(name, marker) {
			return true
		}
	}
	return false
}

// receiverOf returns the receiver's variable name and its bare type name.
// A receiver declared without a name (`func (*Fake) Name() string`) cannot
// write to a field, so an empty variable name is a legitimate skip.
func receiverOf(fn *ast.FuncDecl) (recv, typeName string) {
	field := fn.Recv.List[0]
	if len(field.Names) == 1 {
		recv = field.Names[0].Name
	}
	expr := field.Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if ident, ok := expr.(*ast.Ident); ok {
		typeName = ident.Name
	}
	return recv, typeName
}

type fieldWrite struct {
	field string
	pos   token.Pos
}

// writesToReceiverFields finds assignments and increments whose target is a
// field of the receiver. `f.x = v`, `f.x++` and `f.x = append(f.x, v)` are all
// AssignStmt/IncDecStmt with the same left-hand shape, so one matcher covers
// the lot.
func writesToReceiverFields(body *ast.BlockStmt, recv string) []fieldWrite {
	var writes []fieldWrite
	record := func(expr ast.Expr) {
		sel, ok := expr.(*ast.SelectorExpr)
		if !ok {
			return
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok || ident.Name != recv {
			return
		}
		writes = append(writes, fieldWrite{field: sel.Sel.Name, pos: sel.Pos()})
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch stmt := n.(type) {
		case *ast.AssignStmt:
			for _, lhs := range stmt.Lhs {
				record(lhs)
			}
		case *ast.IncDecStmt:
			record(stmt.X)
		}
		return true
	})
	return writes
}

// ---------------------------------------------------------------------------
// Shared plumbing
// ---------------------------------------------------------------------------

type parsedFile struct {
	path string
	file *ast.File
}

func parseAll(fset *token.FileSet, files []string) ([]parsedFile, error) {
	out := make([]parsedFile, 0, len(files))
	for _, path := range files {
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", path, err)
		}
		out = append(out, parsedFile{path: path, file: file})
	}
	return out, nil
}

// siteKind separates the two things a rule can match. A call is the ordinary
// case; an assignment exists because some orderings are established by setting
// a field rather than by calling anything — `h.provider.Delay = time.Hour`
// stalls the provider for the rest of the test as firmly as any gate — and a
// rule that could not see one would report the tests that take that route.
type siteKind int

const (
	siteCall siteKind = iota
	siteAssign
)

type callSite struct {
	name string
	kind siteKind
	pos  token.Pos
}

// callSequence returns every call and field assignment in the body, in source
// order, named the way the rules above name them.
//
// ast.Inspect walks in source order, which is what makes "before" answerable
// without a control-flow graph. A control-flow graph would be the rigorous
// answer and the wrong tool: these are straight-line test bodies, and a rule
// nobody can read by eye is a rule nobody trusts.
func callSequence(body *ast.BlockStmt) []callSite {
	var calls []callSite
	ast.Inspect(body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			for _, name := range callNames(node) {
				calls = append(calls, callSite{name: name, kind: siteCall, pos: node.Pos()})
			}
		case *ast.AssignStmt:
			calls = append(calls, assignSites(node)...)
		}
		return true
	})
	return calls
}

// literalNamedCalls are the wrappers whose identity, for a rule's purposes, is
// their first string literal argument rather than their own name.
//
// IPC calls were the original case: every one of them is
// `client.Call(method, …)`, so the selector alone says nothing. Event waits are
// the same problem one level along — `waitFor(t, "assistant.started")` and
// `waitFor(t, "tts.started")` are a defect and its fix, and a scan that folds
// both to `waitFor` cannot tell them apart (#215). The literal is looked for
// among the arguments rather than at a fixed index because `Call` takes its
// method first and the harness helpers take `*testing.T` first.
var literalNamedCalls = []string{"Call", "waitFor", "waitEvent", "waitForEvent", "collectUntil"}

// callNames reduces a call to the identifiers the rules match on: the function
// name, or the final selector segment, so `h.engine.ActiveConversationID()`
// and `engine.ActiveConversationID()` are one thing.
//
// A wrapper from literalNamedCalls yields a second name as well, qualified by
// its first string literal argument — `waitFor("tts.started")`. Both are
// emitted, so a rule may match the wrapper as a whole or one specific use of
// it, and the rules written before this distinction existed keep working
// unchanged. A wrapper called with no literal at all yields only the bare name,
// which the qualified rules do not match: a test that computes the event it
// waits for is outside what this scan can reason about, and guessing would be
// worse than staying quiet.
func callNames(call *ast.CallExpr) []string {
	var base string
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		base = fun.Name
	case *ast.SelectorExpr:
		base = fun.Sel.Name
	default:
		return nil
	}
	names := []string{base}
	if !contains(literalNamedCalls, base) {
		return names
	}
	for _, arg := range call.Args {
		lit, ok := arg.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			continue
		}
		if value, err := strconv.Unquote(lit.Value); err == nil {
			names = append(names, base+`("`+value+`")`)
		}
		break
	}
	return names
}

// assignSites names a field assignment by the field, and — when the value is
// simple enough to write down — by the field and the value together, so a rule
// can be as coarse or as exact as the ordering it describes deserves.
// `Delay = time.Hour` is a barrier; `Delay` alone would also cover
// `Delay = 50 * time.Millisecond`, which is a window and not a barrier at all.
func assignSites(stmt *ast.AssignStmt) []callSite {
	var sites []callSite
	for i, lhs := range stmt.Lhs {
		sel, ok := lhs.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		sites = append(sites, callSite{name: sel.Sel.Name, kind: siteAssign, pos: sel.Pos()})
		if len(stmt.Rhs) != len(stmt.Lhs) {
			continue
		}
		if value := simpleValue(stmt.Rhs[i]); value != "" {
			sites = append(sites, callSite{name: sel.Sel.Name + " = " + value, kind: siteAssign, pos: sel.Pos()})
		}
	}
	return sites
}

// simpleValue renders an identifier or a qualified identifier (`time.Hour`,
// `forever`) and refuses everything else. Anything with structure — an
// arithmetic expression, a call, a composite literal — is deliberately not
// rendered: a rule naming a value has to be readable by eye, and a scan that
// pretty-printed arbitrary expressions would invite rules that match on
// whitespace.
func simpleValue(expr ast.Expr) string {
	switch value := expr.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.SelectorExpr:
		if pkg, ok := value.X.(*ast.Ident); ok {
			return pkg.Name + "." + value.Sel.Name
		}
	}
	return ""
}

func callsAny(body *ast.BlockStmt, names []string) bool {
	if len(names) == 0 {
		return false
	}
	for _, c := range callSequence(body) {
		if c.kind == siteCall && contains(names, c.name) {
			return true
		}
	}
	return false
}

// allowReason reports whether the function carries an opt-out marker, and the
// reason given with it. The marker is looked for in every comment inside the
// function's extent as well as its doc comment, so it can sit on the line that
// needs it rather than only at the top.
func allowReason(f parsedFile, fn *ast.FuncDecl) (reason string, marked bool) {
	for _, group := range f.file.Comments {
		inBody := group.Pos() >= fn.Pos() && group.End() <= fn.End()
		isDoc := fn.Doc != nil && group == fn.Doc
		if !inBody && !isDoc {
			continue
		}
		for _, c := range group.List {
			idx := strings.Index(c.Text, AllowMarker)
			if idx < 0 {
				continue
			}
			return strings.TrimSpace(strings.TrimLeft(
				c.Text[idx+len(AllowMarker):], " :-")), true
		}
	}
	return "", false
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func sortFindings(findings []Finding) {
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		return findings[i].Line < findings[j].Line
	})
}
