package main

// The screen CLI (#180): `jarvix monitors` shows which outputs are plugged in
// and what the user calls them, and the three write verbs edit those names
// from the terminal — the same seam the spoken "call this monitor top" uses,
// so the collision rules cannot differ between surfaces.
//
// It needs the daemon for the same reason `jarvix windows` does: which
// screens are attached is live state only jarvixd can see. The names
// themselves are in a file the user can read without any of this, which is
// exactly the point of storing them where they can.

import (
	"encoding/json"
	"fmt"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/tools"
)

// monitorListing is the monitors.list reply.
type monitorListing struct {
	Monitors  []tools.MonitorListing       `json:"monitors"`
	Nicknames []tools.NicknameListingEntry `json:"nicknames"`
	Path      string                       `json:"path"`
	Count     int                          `json:"count"`
	Max       int                          `json:"max"`
	Reserved  []string                     `json:"reserved"`
	Current   string                       `json:"current"`
}

// cmdMonitors prints the screens that are attached with the name each one
// answers to, then any name whose screen is not plugged in — listed rather
// than hidden, because a name the user cannot see is a name they cannot fix.
func cmdMonitors(paths config.Paths, asJSON bool) error {
	client, err := ipc.Dial(paths.Socket)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	var listing monitorListing
	if err := client.Call("monitors.list", nil, &listing); err != nil {
		return err
	}
	if asJSON {
		out, err := json.Marshal(listing)
		if err != nil {
			return err
		}
		fmt.Println(string(out))
		return nil
	}
	for _, m := range listing.Monitors {
		name := "—"
		if m.Nickname != "" {
			name = m.Nickname
		}
		marker := ""
		if m.Focused {
			marker = "  (current)"
		}
		fmt.Printf("%-12s %s%s\n", name, m.Describe, marker)
	}
	for _, n := range listing.Nicknames {
		if n.Present {
			continue
		}
		fmt.Printf("%-12s %s — not plugged in right now\n", n.Name, n.Connector)
	}
	if listing.Path != "" {
		fmt.Printf("\nnames are in %s\n", listing.Path)
	}
	return nil
}

// cmdMonitorsName assigns a screen name; connector "" means the screen
// holding focus. Refusals arrive as the daemon's spoken-ready error and print
// as-is.
func cmdMonitorsName(paths config.Paths, verb, name, connector string) error {
	client, err := ipc.Dial(paths.Socket)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	var result struct {
		Spoken string `json:"spoken"`
	}
	params := map[string]any{"name": name}
	if connector != "" {
		params["connector"] = connector
	}
	if err := client.Call(verb, params, &result); err != nil {
		return err
	}
	fmt.Println(result.Spoken)
	return nil
}
