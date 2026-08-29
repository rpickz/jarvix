//go:build ignore

// Command genipcvocab writes the headless QML harness's IPC vocabulary from
// the Go tables in ipcvocab.go. Run it with `go generate ./internal/desktop`
// after adding a daemon verb or a bus event; ipcvocab_test.go fails the build
// if the checked-in file has drifted from the tables, and also if the tables
// have drifted from the daemon.
//
// Like genbarstate.go it is a separate `ignore`-tagged program rather than a
// test flag, so that regenerating is an explicit act — tests that rewrite the
// tree they are checking cannot fail.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/rpickz/jarvix/internal/desktop"
)

func main() {
	// go:generate runs with the package directory as the working directory.
	out := filepath.Join("..", "..", "qmltest", "stubs", "JarvixTest", "IpcVocabulary.js")
	if err := os.WriteFile(out, []byte(desktop.RenderIpcVocabularyJS()), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "genipcvocab:", err)
		os.Exit(1)
	}
	fmt.Println("wrote", out)
}
