package desktop

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The checked-in generated harness vocabulary must match the tables, for the
// same reason BarState.js must: the plugin — and its tests — run from a git
// clone with no Go toolchain, so drift is possible and silent. Regenerate with
// `go generate ./internal/desktop`.
func TestIpcVocabularyJSIsUpToDate(t *testing.T) {
	path := harnessFilePath(t, filepath.Join("stubs", "JarvixTest", "IpcVocabulary.js"))
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != RenderIpcVocabularyJS() {
		t.Errorf("%s is stale — run: go generate ./internal/desktop", path)
	}
}

// The request half of the fake daemon's vocabulary must be exactly the set of
// methods the daemon registers.
//
// This is the check that makes the fake worth running (issue #174's NFR: "the
// fake surface must be generated from or checked against the real IPC
// vocabulary so it cannot drift into testing a fiction"). Set equality, not
// containment, and in both directions on purpose:
//
//   - a name here the daemon does not register would let a QML test drive a
//     verb that answers -32601 in production, and pass;
//   - a verb the daemon registers and this list omits would let the same test
//     suite go on being green after a rename, because the fake would simply
//     never be asked about the new name.
//
// The registrations are read out of the source rather than out of a running
// daemon: constructing one needs a config, a socket and every engine, which is
// the opposite of what a vocabulary check should cost.
func TestDaemonMethodNamesMatchTheServerRegistrations(t *testing.T) {
	registered := map[string]bool{}
	walkNonTestGo(t, func(file *ast.File, path string) {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Handle" {
				return true
			}
			if name, ok := stringLit(call.Args[0]); ok {
				registered[name] = true
			}
			return true
		})
	})
	if len(registered) == 0 {
		t.Fatal("found no server.Handle registrations at all; this guard is no longer watching anything")
	}
	assertSameSet(t, "daemonMethods", DaemonMethodNames(), registered,
		"run go generate ./internal/desktop after fixing the list in ipcvocab.go")
}

// The notification half, checked the same way and for the same reasons.
//
// Events are published through four shapes rather than one — `Event{Type: …}`
// composite literals, `publish("…", …)` / `emit("…", …)` helpers, and the one
// `eventType := "…"` in internal/session/confirm.go that picks between
// tool.confirmed and tool.pre_approved. All four are read, because a rule that
// only understood the composite literal would quietly stop watching two thirds
// of the vocabulary.
//
// The publish/emit helpers also carry names that are not events — focus.go
// passes its own row verbs ("anchored", "parked", "switched") through the same
// call — so a dotted name is required. "error" is the single undotted event
// the daemon has published since the first session engine, and it is named
// here rather than inferred: a rule that accepted any undotted literal would
// sweep the row verbs straight back in.
func TestDaemonEventNamesMatchThePublishedEvents(t *testing.T) {
	const undottedEvent = "error"

	published := map[string]bool{}
	note := func(name string) {
		if strings.Contains(name, ".") || name == undottedEvent {
			published[name] = true
		}
	}
	walkNonTestGo(t, func(file *ast.File, path string) {
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CompositeLit:
				// Event{Type: "…"} — the session engine's own publications.
				if !isEventType(node.Type) {
					return true
				}
				for _, elt := range node.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if key, ok := kv.Key.(*ast.Ident); !ok || key.Name != "Type" {
						continue
					}
					if name, ok := stringLit(kv.Value); ok {
						note(name)
					}
				}
			case *ast.CallExpr:
				// publish("…", …) / emit("…", …) — the services that reach the
				// bus through a function value rather than the Event type.
				if len(node.Args) == 0 || !isPublishFunc(node.Fun) {
					return true
				}
				if name, ok := stringLit(node.Args[0]); ok {
					note(name)
				}
			case *ast.AssignStmt:
				// eventType := "…" — the confirmation path, which chooses its
				// event name before publishing it.
				for i, lhs := range node.Lhs {
					ident, ok := lhs.(*ast.Ident)
					if !ok || ident.Name != "eventType" || i >= len(node.Rhs) {
						continue
					}
					if name, ok := stringLit(node.Rhs[i]); ok {
						note(name)
					}
				}
			}
			return true
		})
	})
	if len(published) == 0 {
		t.Fatal("found no published events at all; this guard is no longer watching anything")
	}
	assertSameSet(t, "daemonEvents", DaemonEventNames(), published,
		"run go generate ./internal/desktop after fixing the list in ipcvocab.go")
}

// Every event the conversation window's dispatch switch handles must be one
// the daemon actually publishes.
//
// The window's switch is the other end of the same contract, and it is the end
// that fails silently: an unknown case is simply never taken, so a renamed
// event turns a live surface into a dead one with no error anywhere. The QML
// tests drive events through the fake, and the fake only speaks the generated
// vocabulary — but only for the events a test happens to exercise. This closes
// the rest.
func TestTheWindowOnlyHandlesEventsTheDaemonSends(t *testing.T) {
	known := map[string]bool{}
	for _, name := range DaemonEventNames() {
		known[name] = true
	}
	for _, file := range []string{"JarvixWindow.qml", "JarvixOverlay.qml", "JarvixBar.qml"} {
		qml := stripQMLComments(readPlugin(t, file))
		cases := 0
		for _, line := range strings.Split(qml, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, `case "`) || !strings.HasSuffix(line, `":`) {
				continue
			}
			name := strings.TrimSuffix(strings.TrimPrefix(line, `case "`), `":`)
			// The same switch idiom serves several vocabularies in these
			// files (row kinds, tab ids, decline sources). Only dotted names
			// are candidates for being an event, and an undotted one that
			// really is an event would fail the daemonEvents check above
			// rather than slipping past here.
			if !strings.Contains(name, ".") {
				continue
			}
			cases++
			if !known[name] {
				t.Errorf("%s handles event %q, which no daemon publication uses; "+
					"an unknown case is never taken and the surface would go quietly dead",
					file, name)
			}
		}
		if cases == 0 {
			t.Errorf("%s has no dotted case labels; this guard is no longer watching anything", file)
		}
	}
}

// The QML test harness must never end up inside the shipped plugin.
//
// scripts/install-plugin.sh copies plugin/omarchy into the user's shell and
// scripts/package-release.sh copies it into the release tarball, both
// wholesale. The harness carries a qmldir declaring a module called
// `Quickshell` whose FloatingWindow is an ordinary Item — put that on a live
// shell's import path and the real window stops being a window, silently, on
// somebody's desktop. Hence qmltest/ at the repository root, and hence this.
func TestThePluginShipsNoTestHarness(t *testing.T) {
	pluginDir := filepath.Dir(pluginFilePath(t, "placeholder"))
	entries, err := os.ReadDir(pluginDir)
	if err != nil {
		t.Fatalf("reading %s: %v", pluginDir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			t.Errorf("%s/%s would be copied into the user's shell by install-plugin.sh; "+
				"the plugin directory ships verbatim and holds QML files only",
				pluginDir, entry.Name())
			continue
		}
		if entry.Name() == "qmldir" {
			t.Errorf("%s/qmldir turns the shipped plugin into a QML module; "+
				"the plugin is loaded as a directory of files, not as a module", pluginDir)
		}
	}

	// And the harness really is where the runner expects it, so this guard
	// cannot pass by the tests having quietly vanished.
	if _, err := os.Stat(harnessFilePath(t, "JarvixWindowCase.qml")); err != nil {
		t.Errorf("the QML harness is not at qmltest/: %v", err)
	}
}

// --- helpers ---------------------------------------------------------------

// harnessFilePath resolves a file under qmltest/ the same way pluginFilePath
// resolves one under plugin/omarchy, by walking up to the module root.
func harnessFilePath(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(moduleRoot(t), "qmltest", name)
}

// moduleRoot is plugin/omarchy's grandparent — the directory holding go.mod.
// Derived from pluginFilePath rather than walking again, so there is one
// module-root walk in the package and not two that can disagree.
func moduleRoot(t *testing.T) string {
	t.Helper()
	return filepath.Dir(filepath.Dir(filepath.Dir(pluginFilePath(t, "placeholder"))))
}

// walkNonTestGo visits every non-test Go file in the module. Test files are
// skipped because they are full of invented verbs and events — a fake daemon
// in a Go test is exactly the kind of fiction this check exists to keep out of
// the QML one.
func walkNonTestGo(t *testing.T, visit func(file *ast.File, path string)) {
	t.Helper()
	root := moduleRoot(t)
	fset := token.NewFileSet()
	seen := 0
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			parsed, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				// A file that does not parse is a compile failure elsewhere;
				// reporting it here as well only adds noise.
				return nil
			}
			seen++
			visit(parsed, path)
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", dir, err)
		}
	}
	if seen == 0 {
		t.Fatal("no Go sources found; the module root walk is wrong")
	}
}

// isEventType reports whether a composite literal's type is a session Event —
// either `Event{…}` inside internal/session or `session.Event{…}` outside it.
func isEventType(expr ast.Expr) bool {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name == "Event"
	case *ast.SelectorExpr:
		return t.Sel.Name == "Event"
	}
	return false
}

// isPublishFunc reports whether a call is one of the bus-publishing helpers.
// Matched by name rather than by type because the helpers are function-valued
// struct fields as often as they are methods, and a type-checked walk would
// cost a full package load for a question this shallow.
func isPublishFunc(fun ast.Expr) bool {
	var name string
	switch f := fun.(type) {
	case *ast.Ident:
		name = f.Name
	case *ast.SelectorExpr:
		name = f.Sel.Name
	default:
		return false
	}
	return name == "publish" || name == "Publish" || name == "emit" || name == "Emit"
}

func stringLit(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return value, true
}

func assertSameSet(t *testing.T, listName string, declared []string, found map[string]bool, remedy string) {
	t.Helper()
	have := map[string]bool{}
	for _, name := range declared {
		have[name] = true
	}
	for _, name := range declared {
		if !found[name] {
			t.Errorf("%s lists %q, which the daemon's source no longer uses; %s", listName, name, remedy)
		}
	}
	for name := range found {
		if !have[name] {
			t.Errorf("the daemon uses %q, which %s does not list; %s", name, listName, remedy)
		}
	}
}
