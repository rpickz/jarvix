package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The architecture rule of issue #103, held mechanically: the call-sites that
// *derive* the assistant's name — the STT bias construction, the
// wake-transcript matcher and strip, and the prompt self-reference — must not
// carry their own copy of it. The literal is allowed in exactly two places:
// the default-value definition (config.go's defaultAssistantName and
// defaultAssistantAliases) and product branding (window title, bar tooltip,
// docs, binary/socket/service names), none of which are in the files scanned
// here.
//
// The scan reads *string literals* via the Go parser rather than grepping
// raw text, because comments in these files legitimately tell the issue-#83
// story — how "Jarvix" kept arriving as "Jarvis" — and history in prose is
// not a second copy of the name in behaviour.
func TestDerivedCallSitesCarryNoCopyOfTheDefaultName(t *testing.T) {
	files := []string{
		// The bias sentence: composed from Assistant.Name.
		filepath.Join(".", "stt.go"),
		// The identity itself: derivations, validation, prompt composition.
		filepath.Join(".", "assistant.go"),
		// The wake-transcript matcher and strip.
		filepath.Join("..", "session", "wake.go"),
	}
	for _, file := range files {
		if _, err := os.Stat(file); err != nil {
			t.Fatalf("%s: the guarded file moved; update this guard so it keeps guarding: %v", file, err)
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			// Import paths are string literals too, and this module's own
			// path names the product — branding, which is allowed.
			if _, ok := n.(*ast.ImportSpec); ok {
				return false
			}
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if strings.Contains(strings.ToLower(lit.Value), "jarvix") {
				t.Errorf("%s: string literal %s names the assistant; derive it from the [assistant] config instead",
					fset.Position(lit.Pos()), lit.Value)
			}
			return true
		})
	}
}
