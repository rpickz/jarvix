package main

// The knowledge-base CLI (ADR 0025). Two jobs, both deliberately outside any
// conversation: `jarvix memory list` shows what Jarvix knows straight from
// the store, and `jarvix memory forget` deletes from it — because hearing
// and correcting a memory must never require talking a model into it.
// printLastMemory is the third surface: the per-turn audit under
// `jarvix status --last`, beside desktop context, answering "which facts was
// the model just given?" with the facts themselves.

import (
	"fmt"
	"strings"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/ipc"
)

// factView is the wire shape of one fact, shared by every memory method.
type factView struct {
	ID       string `json:"id"`
	Content  string `json:"content"`
	Stored   string `json:"stored"`
	Updated  string `json:"updated"`
	Source   string `json:"source"`
	Previous []struct {
		Content    string `json:"content"`
		Stored     string `json:"stored"`
		Superseded string `json:"superseded"`
	} `json:"previous"`
}

// printFact renders one fact with its supersede trail, dates trimmed to the
// day — the precision a person scanning a list actually reads.
func printFact(f factView, indent string) {
	fmt.Printf("%s%-5s %s  (updated %s)\n", indent, f.ID, f.Content, day(f.Updated))
	for _, p := range f.Previous {
		fmt.Printf("%s      previously %q, %s to %s\n", indent, p.Content, day(p.Stored), day(p.Superseded))
	}
}

// day trims an RFC 3339 timestamp to its date.
func day(ts string) string {
	if len(ts) >= 10 {
		return ts[:10]
	}
	return ts
}

// cmdMemoryList prints every remembered fact (or those matching a query).
func cmdMemoryList(paths config.Paths, query string) error {
	client, err := ipc.Dial(paths.Socket)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	var listing struct {
		Enabled bool       `json:"enabled"`
		Path    string     `json:"path"`
		Count   int        `json:"count"`
		Max     int        `json:"max"`
		Facts   []factView `json:"facts"`
	}
	params := map[string]any{"query": query}
	if err := client.Call("memory.list", params, &listing); err != nil {
		return err
	}
	if !listing.Enabled {
		fmt.Println("memory is disabled (memory.enabled = false)")
		return nil
	}
	if len(listing.Facts) == 0 {
		if query != "" {
			fmt.Printf("no remembered fact matches %q\n", query)
		} else {
			fmt.Println("nothing is remembered yet — say \"remember that ...\" to store a fact")
		}
		return nil
	}
	for _, f := range listing.Facts {
		printFact(f, "")
	}
	fmt.Printf("\n%d of %d facts — the file is yours to edit: %s\n",
		listing.Count, listing.Max, listing.Path)
	return nil
}

// cmdMemoryForget deletes one fact, by id ("m3") or by words. An ambiguous
// query deletes nothing: the candidates are listed and the user picks an id
// — the same refuse-to-guess rule the model-facing tool follows.
func cmdMemoryForget(paths config.Paths, target string) error {
	client, err := ipc.Dial(paths.Socket)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	params := map[string]any{}
	// An id is addressed as an id; anything else is words to match. The "m"
	// prefix plus digits is unambiguous because generated ids never collide
	// with English.
	if isFactID(target) {
		params["id"] = target
	} else {
		params["query"] = target
	}
	var result struct {
		Forgotten bool       `json:"forgotten"`
		Fact      factView   `json:"fact"`
		Matches   []factView `json:"matches"`
	}
	if err := client.Call("memory.forget", params, &result); err != nil {
		return err
	}
	if result.Forgotten {
		fmt.Printf("forgotten: %s\n", result.Fact.Content)
		return nil
	}
	if len(result.Matches) == 0 {
		fmt.Printf("no remembered fact matches %q; nothing was forgotten\n", target)
		return nil
	}
	fmt.Printf("several facts match %q; forget one by id:\n", target)
	for _, f := range result.Matches {
		printFact(f, "  ")
	}
	return nil
}

// isFactID reports whether s looks like a generated fact id ("m12").
func isFactID(s string) bool {
	if len(s) < 2 || s[0] != 'm' {
		return false
	}
	for _, r := range s[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// printLastMemory prints the facts injected into the most recent model turn
// — the memory half of `jarvix status --last`, beside desktop context (ADR
// 0019's disclosure precedent). It shows the facts themselves, not counts:
// the point of the audit is that the user can compare what the model was
// given with what they believe is stored.
func printLastMemory(client *ipc.Client) error {
	var last struct {
		Enabled   bool       `json:"enabled"`
		Injected  bool       `json:"injected"`
		SessionID string     `json:"session_id"`
		Facts     []factView `json:"facts"`
		Trimmed   int        `json:"trimmed"`
		Total     int        `json:"total"`
		EstTokens int        `json:"est_tokens"`
	}
	if err := client.Call("memory.last", nil, &last); err != nil {
		return err
	}
	switch {
	case !last.Enabled:
		fmt.Println("memory:   disabled (memory.enabled = false)")
	case !last.Injected:
		fmt.Println("memory:   no turn has consulted the knowledge base yet")
	case len(last.Facts) == 0 && last.Total == 0:
		fmt.Printf("memory:   session %s, nothing remembered yet\n", last.SessionID)
	default:
		note := fmt.Sprintf("%d of %d facts injected (~%d tokens)",
			len(last.Facts), last.Total, last.EstTokens)
		if last.Trimmed > 0 {
			note += fmt.Sprintf(", %d trimmed by the token cap", last.Trimmed)
		}
		fmt.Printf("memory:   session %s, %s\n", last.SessionID, note)
		for _, f := range last.Facts {
			fmt.Println("          " + strings.TrimSpace(fmt.Sprintf("%-5s %s", f.ID, f.Content)))
		}
	}
	return nil
}
