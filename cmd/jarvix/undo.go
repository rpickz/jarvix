package main

// `jarvix actions` and `jarvix undo` — the account of what Jarvix did in the
// user's name, and putting it back (#201, ADR 0064).
//
// The two commands are deliberately a pair and deliberately in that order:
// review, then reversal. `jarvix undo` with no argument is the common case
// ("undo that"); `jarvix undo <id>` reaches back to a row `jarvix actions`
// showed; `jarvix undo --job <id>` reverses a piece of work (#200 — nothing
// sets a job id yet, so today it reports having nothing for that job, which
// is the honest answer).

import (
	"errors"
	"fmt"
	"strings"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/ipc"
)

// actionRow is the wire shape of one row from undo.list.
type actionRow struct {
	ID         string `json:"id"`
	At         string `json:"at"`
	Tool       string `json:"tool"`
	Summary    string `json:"summary"`
	Target     string `json:"target"`
	Job        string `json:"job"`
	Reversible bool   `json:"reversible"`
	Why        string `json:"why"`
	UndoneBy   string `json:"undone_by"`
}

// accountView is the wire shape of undo.list.
type accountView struct {
	Actions []actionRow `json:"actions"`
	// Disclosure is the daemon's own sentence about the bound. Printed
	// verbatim rather than reworded here: "I only keep the last N" is a
	// promise, and a promise the CLI phrased its own way would be a second
	// promise (ADR 0013).
	Disclosure string `json:"disclosure"`
	Path       string `json:"path"`
}

// cmdActions prints the account, newest first.
func cmdActions(paths config.Paths) error {
	client, err := ipc.Dial(paths.Socket)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	var view accountView
	if err := client.Call("undo.list", nil, &view); err != nil {
		return err
	}
	if len(view.Actions) == 0 {
		fmt.Println("Jarvix has not changed anything on this machine.")
		fmt.Println(view.Disclosure)
		return nil
	}
	for _, a := range view.Actions {
		mark := "  "
		switch {
		case a.UndoneBy != "":
			mark = "↩ "
		case a.Reversible:
			mark = "↺ "
		}
		fmt.Printf("%s%-5s %s  %s\n", mark, a.ID, shortTime(a.At), a.Summary)
		switch {
		case a.UndoneBy != "":
			fmt.Printf("        put back by %s\n", a.UndoneBy)
		case !a.Reversible:
			fmt.Printf("        can't be undone: %s\n", a.Why)
		}
	}
	fmt.Println()
	fmt.Println(view.Disclosure)
	fmt.Println("undo one with: jarvix undo <id>   —   the file is", view.Path)
	return nil
}

// cmdUndo reverses one action, the most recent reversible one, or a job's
// worth.
//
// A refusal exits non-zero. "I won't do that because it would overwrite
// newer work" is not a success, and a script that ran `jarvix undo` and
// carried on as though the file had been restored would be exactly the kind
// of quiet wrongness this feature exists to remove.
func cmdUndo(paths config.Paths, id, job string) error {
	client, err := ipc.Dial(paths.Socket)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	params := map[string]string{}
	if strings.TrimSpace(id) != "" {
		params["id"] = id
	}
	if strings.TrimSpace(job) != "" {
		params["job"] = job
	}
	var res struct {
		Done    bool   `json:"done"`
		Refused bool   `json:"refused"`
		Spoken  string `json:"spoken"`
		Actions []struct {
			Done   bool   `json:"done"`
			Spoken string `json:"spoken"`
		} `json:"actions"`
	}
	if err := client.Call("undo.apply", params, &res); err != nil {
		return err
	}
	fmt.Println(res.Spoken)
	for _, a := range res.Actions {
		fmt.Println(" ", a.Spoken)
	}
	if res.Refused {
		// The reason is already printed; errChecksFailed is the CLI's
		// "already said, just exit 1".
		return errChecksFailed
	}
	if !res.Done && len(res.Actions) == 0 {
		return errChecksFailed
	}
	return nil
}

// shortTime renders an RFC 3339 timestamp as the date and minute a person
// scanning a list needs. A parse failure prints the raw value rather than an
// empty column: the daemon's format changing should look like a formatting
// bug, not like a row with no time.
func shortTime(ts string) string {
	if len(ts) < 16 {
		return ts
	}
	return ts[:10] + " " + ts[11:16]
}

// errUndoUsage is the shared refusal for a malformed invocation.
var errUndoUsage = errors.New("usage: jarvix undo [<action-id>] [--job <job-id>]")
