package main

// The window CLI (#126): `jarvix windows` lists what is open with the
// nicknames the user has given, and `jarvix windows name` assigns one from
// the terminal — the same seam the spoken "call this window builds" uses, so
// the collision and single-word rules cannot differ between surfaces. Both
// need the daemon: windows are live state, and only jarvixd can see them.

import (
	"encoding/json"
	"fmt"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/tools"
)

// cmdWindows prints the live window list, nicknames first on each row they
// exist for — the point of the listing is "what did I call things".
func cmdWindows(paths config.Paths, asJSON bool) error {
	client, err := ipc.Dial(paths.Socket)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	var listing struct {
		Windows []tools.WindowListing `json:"windows"`
	}
	if err := client.Call("windows.list", nil, &listing); err != nil {
		return err
	}
	if asJSON {
		out, err := json.Marshal(map[string]any{"windows": listing.Windows})
		if err != nil {
			return err
		}
		fmt.Println(string(out))
		return nil
	}
	if len(listing.Windows) == 0 {
		fmt.Println("nothing is open")
		return nil
	}
	for _, w := range listing.Windows {
		name := "—"
		if w.Nickname != "" {
			name = w.Nickname
		}
		marker := ""
		if w.Focused {
			marker = "  (focused)"
		}
		title := w.Title
		if title == "" {
			title = "no title"
		}
		fmt.Printf("%-12s %s — %s  [workspace %s]%s\n", name, w.App, title, w.Workspace, marker)
	}
	return nil
}

// cmdWindowsName assigns a nickname; window describes which one ("" means
// the focused window). Refusals arrive as the daemon's spoken-ready error
// and print as-is.
func cmdWindowsName(paths config.Paths, name, window string) error {
	client, err := ipc.Dial(paths.Socket)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	var result struct {
		Spoken string `json:"spoken"`
	}
	if err := client.Call("windows.name", map[string]any{"name": name, "window": window}, &result); err != nil {
		return err
	}
	fmt.Println(result.Spoken)
	return nil
}
