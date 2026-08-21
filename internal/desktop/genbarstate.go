//go:build ignore

// Command genbarstate writes plugin/omarchy/BarState.js from the Go table in
// barstatus.go. Run it with `go generate ./internal/desktop` after changing a
// glyph, a label, or a rule; barstatus_test.go fails the build if the
// checked-in file has drifted from the table.
//
// It is a separate `ignore`-tagged program rather than a test flag so that
// regenerating is an explicit act — tests that rewrite the tree they are
// checking cannot fail.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/rpickz/jarvix/internal/desktop"
)

func main() {
	// go:generate runs with the package directory as the working directory.
	out := filepath.Join("..", "..", "plugin", "omarchy", "BarState.js")
	if err := os.WriteFile(out, []byte(desktop.RenderBarStateJS()), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "genbarstate:", err)
		os.Exit(1)
	}
	fmt.Println("wrote", out)
}
