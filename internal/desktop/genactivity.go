//go:build ignore

// Command genactivity writes plugin/omarchy/ActivityState.js from the Go
// table in activity.go. Run it with `go generate ./internal/desktop` after
// changing a row kind or a glyph; activity_test.go fails the build if the
// checked-in file has drifted from the table.
//
// It is a separate `ignore`-tagged program rather than a test flag for the
// same reason genbarstate.go is: regenerating is an explicit act — tests that
// rewrite the tree they are checking cannot fail.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/rpickz/jarvix/internal/desktop"
)

func main() {
	// go:generate runs with the package directory as the working directory.
	out := filepath.Join("..", "..", "plugin", "omarchy", "ActivityState.js")
	if err := os.WriteFile(out, []byte(desktop.RenderActivityJS()), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "genactivity:", err)
		os.Exit(1)
	}
	fmt.Println("wrote", out)
}
