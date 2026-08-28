package main

import (
	"fmt"
	"strings"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/ipc"
)

// `jarvix approvals` (issue #162, ADR 0053): what runs without asking, and
// how to stop it.
//
// There is no `jarvix approvals add`, and that absence is the design. A
// standing grant is made by answering a confirmation card that shows the
// exact pattern next to the exact command it came from — the display is the
// consent — and a CLI verb would be a second authoring path with none of that
// context, reachable by anything that can spawn a process. Revocation gets a
// verb because taking a permission back is never the dangerous direction.

// approvalView is the wire shape of one row from approvals.list.
type approvalView struct {
	Pattern  string `json:"pattern"`
	Source   string `json:"source"`
	Scope    string `json:"scope"`
	Uses     int    `json:"uses"`
	Added    string `json:"added"`
	LastUsed string `json:"last_used"`
}

// cmdApprovalsList prints every standing grant.
func cmdApprovalsList(paths config.Paths) error {
	client, err := ipc.Dial(paths.Socket)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	var listing struct {
		Path     string         `json:"path"`
		Approved []approvalView `json:"approved"`
	}
	if err := client.Call("approvals.list", nil, &listing); err != nil {
		return err
	}
	if len(listing.Approved) == 0 {
		fmt.Println("nothing is pre-approved — every command still asks first")
		return nil
	}
	for _, a := range listing.Approved {
		printApproval(a)
	}
	fmt.Printf("\n%d pre-approved — the list is yours to edit: %s\n",
		len(listing.Approved), listing.Path)
	fmt.Println("forget one with: jarvix approvals forget <pattern>")
	return nil
}

// printApproval renders one row: the pattern verbatim, then where it came
// from and what it has done. The pattern leads because the pattern is the
// permission — everything else on the line is context for deciding whether to
// keep it.
func printApproval(a approvalView) {
	fmt.Printf("  %s\n", a.Pattern)
	parts := []string{}
	if a.Scope == "conversation" {
		parts = append(parts, "just this conversation")
	}
	switch {
	case a.Added != "":
		parts = append(parts, "added "+day(a.Added))
	case a.Source == "hand" && a.Scope != "conversation":
		// No date means it was not added on a card — it was written into
		// config.toml by hand, or predates the ledger. Said plainly rather
		// than guessed at.
		parts = append(parts, "added by hand")
	}
	if a.Uses > 0 {
		parts = append(parts, fmt.Sprintf("used %s, last %s",
			pluralUses(a.Uses), day(a.LastUsed)))
	} else if a.Scope != "conversation" {
		// The row that most deserves revoking: a standing permission that has
		// never once been needed.
		parts = append(parts, "never used")
	}
	if len(parts) > 0 {
		fmt.Printf("      %s\n", strings.Join(parts, " · "))
	}
}

// pluralUses words a firing count.
func pluralUses(n int) string {
	if n == 1 {
		return "once"
	}
	return fmt.Sprintf("%d times", n)
}

// cmdApprovalsForget revokes one pattern, permanent or conversation-scoped.
// The pattern is matched after whitespace collapsing, so it can be typed the
// way the listing prints it without quoting games.
func cmdApprovalsForget(paths config.Paths, pattern string) error {
	client, err := ipc.Dial(paths.Socket)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	var result struct {
		Forgotten bool           `json:"forgotten"`
		Pattern   string         `json:"pattern"`
		Scope     string         `json:"scope"`
		Approved  []approvalView `json:"approved"`
	}
	if err := client.Call("approvals.forget", map[string]string{"pattern": pattern}, &result); err != nil {
		return err
	}
	if result.Forgotten {
		if result.Scope == "conversation" {
			fmt.Printf("forgotten: %s — it was only for this conversation anyway\n", result.Pattern)
			return nil
		}
		fmt.Printf("forgotten: %s — that command will ask again\n", result.Pattern)
		return nil
	}
	fmt.Printf("nothing is pre-approved as %q; nothing was forgotten\n", pattern)
	if len(result.Approved) > 0 {
		fmt.Println("what is pre-approved:")
		for _, a := range result.Approved {
			fmt.Printf("  %s\n", a.Pattern)
		}
	}
	return nil
}
