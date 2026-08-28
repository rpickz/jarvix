package placement_test

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/placement"
	"github.com/rpickz/jarvix/internal/tools"
)

// This file is ADR 0056's first acceptance criterion, made mechanical: the
// vocabulary is defined once, and the routine schema and the window-control
// tools derive from it rather than restating it. An option added to
// internal/placement must become available in both, and this file fails until
// it does.
//
// It lives in an external test package (placement_test) so it can import the
// two consumers without the vocabulary itself importing anything.

// TestRoutineSchemaCarriesEveryVocabularyField: every key the vocabulary owns
// is a key of a [[routines.steps]] table, spelled identically. A field added
// to the vocabulary and forgotten in the schema fails here rather than in a
// user's config file six months later.
func TestRoutineSchemaCarriesEveryVocabularyField(t *testing.T) {
	stepKeys := tomlKeys(t, config.RoutineStep{})
	for _, field := range placement.Fields() {
		if !stepKeys[field] {
			t.Errorf("the vocabulary owns %q but [[routines.steps]] has no such key; "+
				"add it to config.RoutineStep with the same spelling", field)
		}
	}
}

// TestRoutineSchemaAcceptsEveryModeAndRefusesTheRest: the routine loader's
// accepted mode set IS placement.ModeNames, proved by running every one of
// them through a real config load — not by comparing two lists, which would
// only prove that two lists match.
func TestRoutineSchemaAcceptsEveryModeAndRefusesTheRest(t *testing.T) {
	for _, mode := range placement.ModeNames() {
		t.Run(mode, func(t *testing.T) {
			if problems := loadStepProblems(t, `mode = "`+mode+`"`); len(problems) != 0 {
				t.Errorf("mode %q is in the vocabulary but the routine loader refuses it: %v",
					mode, problems)
			}
		})
	}
	for _, unsupported := range placement.UnsupportedModes() {
		t.Run("refuses "+unsupported.Name, func(t *testing.T) {
			problems := loadStepProblems(t, `mode = "`+unsupported.Name+`"`)
			if len(problems) == 0 {
				t.Fatalf("the loader accepted %q, which the vocabulary declines", unsupported.Name)
			}
			if !strings.Contains(strings.Join(problems, "\n"), unsupported.Reason) {
				t.Errorf("refusing %q did not carry its recorded reason: %v",
					unsupported.Name, problems)
			}
		})
	}
}

// TestToolSchemaEnumeratesTheVocabularysModes: the enum the model is shown is
// placement.ModeNames, in the vocabulary's own order. A mode added to the
// vocabulary is a mode the assistant can ask for, without anyone editing the
// tool.
func TestToolSchemaEnumeratesTheVocabularysModes(t *testing.T) {
	schema := moveWindowSchema(t)
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema has no properties: %v", schema)
	}
	mode, ok := props["mode"].(map[string]any)
	if !ok {
		t.Fatalf("the move tool offers no mode argument: %v", props)
	}
	raw, ok := mode["enum"].([]any)
	if !ok {
		t.Fatalf("mode has no enum: %v", mode)
	}
	got := make([]string, 0, len(raw))
	for _, v := range raw {
		got = append(got, v.(string))
	}
	if !reflect.DeepEqual(got, placement.ModeNames()) {
		t.Errorf("tool enum = %v, want the vocabulary's %v", got, placement.ModeNames())
	}
}

// TestToolOffersOrExcusesEveryVocabularyField is the drift guard proper: each
// field the vocabulary owns is either an argument of the move tool or written
// into the tool's exclusion list with a reason. Adding a field and doing
// neither fails here — which is what stops the vocabulary and the assistant's
// reach from silently diverging.
func TestToolOffersOrExcusesEveryVocabularyField(t *testing.T) {
	schema := moveWindowSchema(t)
	props, _ := schema["properties"].(map[string]any)
	offered := make(map[string]bool, len(props))
	for key := range props {
		offered[key] = true
	}
	excluded := tools.PlacementFieldsWithheldFromTheModel()
	var missing []string
	for _, field := range placement.Fields() {
		reason, excused := excluded[field]
		switch {
		case offered[field] && excused:
			t.Errorf("%q is both offered to the model and listed as withheld", field)
		case offered[field] || excused:
			if excused && strings.TrimSpace(reason) == "" {
				t.Errorf("%q is withheld from the model with no reason", field)
			}
		default:
			missing = append(missing, field)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("the vocabulary owns %v, which the move tool neither offers nor excuses; "+
			"either add the argument or record why the model may not send it", missing)
	}
	// The exclusion list may not name something the vocabulary does not own —
	// a stale entry makes the debt look larger than it is (the same ratchet
	// discipline testdiscipline.FakeFieldExemptions keeps).
	fields := make(map[string]bool, len(placement.Fields()))
	for _, f := range placement.Fields() {
		fields[f] = true
	}
	for field := range excluded {
		if !fields[field] {
			t.Errorf("%q is excused from the tool but is not a vocabulary field", field)
		}
	}
}

// TestOneValueIsRefusedIdenticallyEverywhere: the same bad share, sent as a
// routine step and as a tool call, is refused by the same rule with the same
// words. Two surfaces validating separately is how "the form let me save it
// and the run would not do it" happens.
func TestOneValueIsRefusedIdenticallyEverywhere(t *testing.T) {
	const badShare = "150%"
	problems := loadStepProblems(t, "mode = \"tiled\"\nwidth = \""+badShare+"\"")
	if len(problems) == 0 {
		t.Fatal("the routine loader accepted a share bigger than the screen")
	}
	routineSaid := strings.Join(problems, "\n")

	toolSaid := runMoveTool(t, map[string]any{"window": "firefox", "width": badShare, "mode": "tiled"})
	const shared = "more than the whole screen"
	if !strings.Contains(routineSaid, shared) || !strings.Contains(toolSaid, shared) {
		t.Errorf("the two surfaces refuse %q differently:\n  routine: %s\n  tool:    %s",
			badShare, routineSaid, toolSaid)
	}
}

// desktopFake is a compositor holding one firefox window on one screen —
// enough for a tool call to resolve a window and be refused by the
// vocabulary, which is all this file asks of it.
func desktopFake() *desktop.FakeCompositor {
	comp := desktop.NewFakeCompositor(desktop.Window{
		Address: "0x1", Class: "firefox", Title: "GitHub", Workspace: 1,
		AcceptsInput: true, Focused: true, Width: 1600, Height: 900,
	})
	comp.Outputs = []placement.Monitor{{
		Name: "HDMI-A-1", Width: 3440, Height: 1440, Scale: 1,
		Reserved: [4]int{0, 26, 0, 0}, Focused: true, ActiveWorkspace: 1,
	}}
	return comp
}

// runMoveTool executes desktop.move_window with the given arguments and
// returns what the assistant would be told.
func runMoveTool(t *testing.T, args map[string]any) string {
	t.Helper()
	for _, tool := range tools.NewDesktop(tools.DesktopOptions{Compositor: desktopFake()}).Tools() {
		if tool.Name() != tools.MoveWindowToolName {
			continue
		}
		input, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		out, err := tool.Execute(t.Context(), input)
		if err != nil {
			t.Fatalf("%s returned an error rather than a sentence: %v", tool.Name(), err)
		}
		return out
	}
	t.Fatalf("no %s tool is registered", tools.MoveWindowToolName)
	return ""
}

// loadStepProblems runs one step's placement keys through a real config load
// and returns what it complained about. A real load rather than a call to the
// validator, so the test proves the path a user's file takes.
func loadStepProblems(t *testing.T, keys string) []string {
	t.Helper()
	doc := `
[[routines]]
name = "contract"
phrases = ["contract routine"]

  [[routines.steps]]
  app = "firefox"
  workspace = 1
` + indent(keys)
	cfg, err := config.ParseBytes([]byte(doc))
	if err != nil {
		return []string{err.Error()}
	}
	if err := cfg.Validate(); err != nil {
		return strings.Split(err.Error(), "\n")
	}
	return nil
}

func indent(keys string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimSpace(keys), "\n") {
		b.WriteString("  " + strings.TrimSpace(line) + "\n")
	}
	return b.String()
}

// moveWindowSchema decodes the move tool's JSON schema, the artefact the
// model is actually shown.
func moveWindowSchema(t *testing.T) map[string]any {
	t.Helper()
	for _, tool := range tools.NewDesktop(tools.DesktopOptions{
		Compositor: desktopFake(),
	}).Tools() {
		if tool.Name() != tools.MoveWindowToolName {
			continue
		}
		var schema map[string]any
		if err := json.Unmarshal(tool.Schema(), &schema); err != nil {
			t.Fatalf("the move tool's schema is not JSON: %v", err)
		}
		return schema
	}
	t.Fatalf("no %s tool is registered", tools.MoveWindowToolName)
	return nil
}

// tomlKeys reads a config struct's toml tag set, the same way the daemon's
// entry-admin drift guard does.
func tomlKeys(t *testing.T, example any) map[string]bool {
	t.Helper()
	typ := reflect.TypeOf(example)
	keys := make(map[string]bool, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("toml")
		if name, _, _ := strings.Cut(tag, ","); name != "" && name != "-" {
			keys[name] = true
		}
	}
	return keys
}

// TestEveryConsumerResolvesThroughTheNicknameTable is the #180 seam claim,
// made mechanical. The whole design of monitor nicknames is that they are ONE
// field of ONE resolver: fill it in, and the routine runner, the window tools
// and every future consumer gain screen names at once. That only holds while
// nobody builds a bare Resolver, which is exactly the mistake a new call site
// makes by copying the old line — and it fails silently, as "no monitor is
// called top right now" on one surface and success on another.
//
// So: no non-test file in the tree may construct placement.Resolver without
// naming Nicknames. A daemon with no store passes nil deliberately (that is
// the pre-#180 behaviour, pinned elsewhere); what is refused here is a
// resolver that never had the chance.
//
// A text scan rather than an AST walk, for the reason the QML guards use one:
// the shape is unambiguous in source, the occurrences are countable on one
// hand, and a regex that reads like the thing it forbids is easier to trust
// than a visitor that does not.
func TestEveryConsumerResolvesThroughTheNicknameTable(t *testing.T) {
	root := repoRoot(t)
	var offenders []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch {
		case d.IsDir() && (d.Name() == ".git" || d.Name() == "testdata"):
			return fs.SkipDir
		case d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go"):
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, literal := range resolverLiterals(string(src)) {
			if !strings.Contains(literal, "Nicknames") {
				rel, _ := filepath.Rel(root, path)
				offenders = append(offenders, rel+": placement.Resolver"+literal)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Errorf("these construct a monitor resolver with no nickname table, so screen names "+
			"would silently not work there:\n  %s", strings.Join(offenders, "\n  "))
	}
}

// resolverLiterals returns the body of every `placement.Resolver{…}` literal
// in src, braces included, one entry per occurrence.
func resolverLiterals(src string) []string {
	const marker = "placement.Resolver{"
	var out []string
	for i := strings.Index(src, marker); i >= 0; {
		start := i + len(marker) - 1
		depth, end := 0, -1
		for j := start; j < len(src); j++ {
			switch src[j] {
			case '{':
				depth++
			case '}':
				if depth--; depth == 0 {
					end = j
				}
			}
			if end >= 0 {
				break
			}
		}
		if end < 0 {
			break // unbalanced; the compiler will have more to say than we do
		}
		out = append(out, src[start:end+1])
		next := strings.Index(src[end:], marker)
		if next < 0 {
			break
		}
		i = end + next
	}
	return out
}

// repoRoot returns the checkout root, found by walking up from the test's
// working directory for the directory holding go.mod — the same walk the
// testdiscipline guards use.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the working directory")
		}
		dir = parent
	}
}
