package main

// The conversation-archive CLI (ADR 0027): list past conversations, show one
// read-only, reopen one as the active thread, and delete one or all. The
// daemon is the normal path — its store is the one the engine is appending to
// — but a transcript of everything said in the user's home must stay under
// their control even with jarvixd stopped, so list, show and delete fall back
// to the files directly when the daemon is down (the same down-detection
// `jarvix new` uses for history.json). Reopening is the exception: adopting a
// thread is an engine action, so it honestly requires the daemon.

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/conversations"
	"github.com/rpickz/jarvix/internal/ipc"
)

// conversationView is the wire shape of one conversation's metadata.
type conversationView struct {
	ID         string `json:"id"`
	Started    string `json:"started"`
	LastActive string `json:"last_active"`
	Turns      int    `json:"turns"`
	Preview    string `json:"preview"`
}

// turnView is the wire shape of one archived turn.
type turnView struct {
	Role string `json:"role"`
	Text string `json:"text"`
	TS   string `json:"ts"`
}

// fileStore opens the archive directly, for the daemon-down fallback.
func fileStore(paths config.Paths) *conversations.FileStore {
	return &conversations.FileStore{Dir: paths.ConversationsDir()}
}

// cmdConversationsList prints the archive, newest first.
func cmdConversationsList(paths config.Paths) error {
	client, err := ipc.Dial(paths.Socket)
	if err != nil {
		if !daemonIsDown(err) {
			return err
		}
		metas, unreadable, err := fileStore(paths).List()
		if err != nil {
			return err
		}
		views := make([]conversationView, 0, len(metas))
		for _, m := range metas {
			views = append(views, conversationView{
				ID: m.ID, Started: m.Started.Format(time.RFC3339),
				LastActive: m.LastActive.Format(time.RFC3339),
				Turns:      m.TurnCount, Preview: m.Preview,
			})
		}
		bad := make([]string, 0, len(unreadable))
		for _, u := range unreadable {
			bad = append(bad, fmt.Sprintf("%s (%s)", u.ID, u.Err))
		}
		printConversations(views, bad, "", true)
		return nil
	}
	defer func() { _ = client.Close() }()
	var listing struct {
		Retention     bool               `json:"retention"`
		ActiveID      string             `json:"active_id"`
		Conversations []conversationView `json:"conversations"`
		Unreadable    []struct {
			ID    string `json:"id"`
			Error string `json:"error"`
		} `json:"unreadable"`
	}
	if err := client.Call("conversation.list", nil, &listing); err != nil {
		return err
	}
	bad := make([]string, 0, len(listing.Unreadable))
	for _, u := range listing.Unreadable {
		bad = append(bad, fmt.Sprintf("%s (%s)", u.ID, u.Error))
	}
	printConversations(listing.Conversations, bad, listing.ActiveID, listing.Retention)
	return nil
}

// printConversations renders the listing: id, when, how much, and the first
// line — enough to pick a conversation out of a library.
func printConversations(views []conversationView, unreadable []string, activeID string, retention bool) {
	if !retention {
		fmt.Println("retention is off (conversation.retention = \"off\") — nothing new is being archived")
	}
	if len(views) == 0 && len(unreadable) == 0 {
		fmt.Println("no archived conversations yet")
		return
	}
	for _, v := range views {
		marker := " "
		if activeID != "" && v.ID == activeID {
			marker = "*" // the thread follow-ups currently continue
		}
		preview := v.Preview
		if preview == "" {
			preview = "(no preview)"
		}
		fmt.Printf("%s %s  %3d turns  %s  %s\n", marker, v.ID, v.Turns, day(v.LastActive), preview)
	}
	// Unreadable records are stated, never skipped: one bad file must not
	// hide itself, let alone the library.
	for _, u := range unreadable {
		fmt.Printf("! %s — unreadable\n", u)
	}
	if activeID != "" {
		fmt.Println("* = the active conversation")
	}
}

// searchResultView is the wire shape of one conversation.search result.
type searchResultView struct {
	ID      string `json:"id"`
	Turn    int    `json:"turn"`
	Role    string `json:"role"`
	TS      string `json:"ts"`
	Passage string `json:"passage"`
	Current bool   `json:"current"`
}

// cmdConversationsSearch searches the archive, ranked best first. The daemon
// answers when it is up (its searcher is the same implementation the window
// and the model's tool use); with jarvixd stopped the files are searched
// directly, because finding what you said must not require the daemon any
// more than reading it does.
func cmdConversationsSearch(paths config.Paths, query string) error {
	client, err := ipc.Dial(paths.Socket)
	if err != nil {
		if !daemonIsDown(err) {
			return err
		}
		store := fileStore(paths)
		matches, stats, err := store.Search(conversations.Query{Text: query})
		if err != nil {
			return err
		}
		views := make([]searchResultView, 0, len(matches))
		activeID := store.Active()
		for _, m := range matches {
			views = append(views, searchResultView{
				ID: m.ConversationID, Turn: m.Turn, Role: m.Role,
				TS: m.Time.Format(time.RFC3339), Passage: m.Passage,
				Current: activeID != "" && m.ConversationID == activeID,
			})
		}
		skipped := make([]string, 0, len(stats.Skipped))
		for _, u := range stats.Skipped {
			skipped = append(skipped, fmt.Sprintf("%s (%s)", u.ID, u.Err))
		}
		printSearchResults(query, views, skipped, stats.Conversations, true)
		return nil
	}
	defer func() { _ = client.Close() }()
	var result struct {
		Retention bool               `json:"retention"`
		Results   []searchResultView `json:"results"`
		Searched  int                `json:"searched"`
		Skipped   []struct {
			ID    string `json:"id"`
			Error string `json:"error"`
		} `json:"skipped"`
	}
	if err := client.Call("conversation.search", map[string]string{"query": query}, &result); err != nil {
		return err
	}
	skipped := make([]string, 0, len(result.Skipped))
	for _, u := range result.Skipped {
		skipped = append(skipped, fmt.Sprintf("%s (%s)", u.ID, u.Error))
	}
	printSearchResults(query, result.Results, skipped, result.Searched, result.Retention)
	return nil
}

// printSearchResults renders ranked passages: where, when, who, and the
// clipped text — with the hint that opening a result is one command away.
func printSearchResults(query string, views []searchResultView, skipped []string, searched int, retention bool) {
	if !retention {
		fmt.Println("retention is off (conversation.retention = \"off\") — recent conversations were not archived")
	}
	if searched == 0 && len(skipped) == 0 {
		fmt.Println("no archived conversations to search yet")
		return
	}
	if len(views) == 0 {
		fmt.Printf("no conversation mentions %q\n", query)
	}
	current := false
	for _, v := range views {
		marker := " "
		if v.Current {
			marker = "*"
			current = true
		}
		speaker := "jarvix"
		if v.Role == "user" {
			speaker = "you"
		}
		fmt.Printf("%s %s  turn %3d  %s  %s: %s\n", marker, v.ID, v.Turn, day(v.TS), speaker, v.Passage)
	}
	// Skipped records are stated, never hidden: a search that could not read
	// part of the library must say so (the listing's unreadable rule).
	for _, s := range skipped {
		fmt.Printf("! %s — could not be searched\n", s)
	}
	if current {
		fmt.Println("* = earlier in the active conversation")
	}
	if len(views) > 0 {
		fmt.Println("open one with: jarvix conversations show <id>")
	}
}

// cmdConversationsShow prints one conversation read-only.
func cmdConversationsShow(paths config.Paths, id string) error {
	client, err := ipc.Dial(paths.Socket)
	if err != nil {
		if !daemonIsDown(err) {
			return err
		}
		conv, err := fileStore(paths).Read(id)
		if err != nil {
			return err
		}
		turns := make([]turnView, 0, len(conv.Turns))
		for _, t := range conv.Turns {
			turns = append(turns, turnView{Role: t.Role, Text: t.Text, TS: t.Time.Format(time.RFC3339)})
		}
		printConversation(conv.Meta.ID, conv.Meta.Started.Format(time.RFC3339), turns)
		return nil
	}
	defer func() { _ = client.Close() }()
	// The wire "turns" key is the turn list here (conversation.read replaces
	// the listing's count with the transcript itself).
	var conv struct {
		ID      string     `json:"id"`
		Started string     `json:"started"`
		Turns   []turnView `json:"turns"`
	}
	if err := client.Call("conversation.read", map[string]string{"id": id}, &conv); err != nil {
		return err
	}
	printConversation(conv.ID, conv.Started, conv.Turns)
	return nil
}

// printConversation renders a transcript the way the window does: who, then
// what, oldest first.
func printConversation(id string, started string, turns []turnView) {
	fmt.Printf("conversation %s (started %s, %d turns)\n\n", id, day(started), len(turns))
	for _, t := range turns {
		speaker := "jarvix"
		if t.Role == "user" {
			speaker = "you"
		}
		fmt.Printf("%s: %s\n", speaker, t.Text)
	}
	fmt.Printf("\ncontinue it with: jarvix conversations open %s\n", id)
}

// cmdConversationsOpen reopens an archived conversation as the active thread.
func cmdConversationsOpen(paths config.Paths, id string) error {
	client, err := ipc.Dial(paths.Socket)
	if err != nil {
		if daemonIsDown(err) {
			return errors.New("reopening a conversation needs jarvixd running: systemctl --user start jarvixd")
		}
		return err
	}
	defer func() { _ = client.Close() }()
	var result struct {
		ID    string `json:"id"`
		Turns int    `json:"turns"`
	}
	if err := client.Call("conversation.open", map[string]string{"id": id}, &result); err != nil {
		return err
	}
	fmt.Printf("reopened %s (%d turns) — follow-ups continue it\n", result.ID, result.Turns)
	return nil
}

// cmdConversationsDelete removes one conversation, or all of them.
func cmdConversationsDelete(paths config.Paths, id string, all bool) error {
	client, err := ipc.Dial(paths.Socket)
	if err != nil {
		if !daemonIsDown(err) {
			// Deleting less than the user asked for is worse than asking them
			// to retry: report and change nothing (the `jarvix new` rule).
			return fmt.Errorf("could not reach jarvixd, so nothing was deleted: %w", err)
		}
		// No daemon means no live thread to reset — delete straight from disk.
		store := fileStore(paths)
		if all {
			n, err := store.DeleteAll()
			if err != nil {
				return err
			}
			fmt.Printf("deleted %d conversation(s)\n", n)
			return nil
		}
		if err := store.Delete(id); err != nil {
			return err
		}
		fmt.Printf("deleted %s\n", id)
		return nil
	}
	defer func() { _ = client.Close() }()
	params := map[string]any{}
	if all {
		params["all"] = true
	} else {
		params["id"] = id
	}
	var result struct {
		Deleted int `json:"deleted"`
	}
	if err := client.Call("conversation.delete", params, &result); err != nil {
		return err
	}
	if all {
		fmt.Printf("deleted %d conversation(s)\n", result.Deleted)
	} else {
		fmt.Printf("deleted %s\n", id)
	}
	return nil
}

// cmdConversations dispatches the `jarvix conversations` subcommands.
func cmdConversations(paths config.Paths, args []string) error {
	if len(args) == 0 {
		return cmdConversationsList(paths)
	}
	switch args[0] {
	case "list":
		return cmdConversationsList(paths)
	case "search":
		if len(args) < 2 {
			return errors.New("usage: jarvix conversations search <query>")
		}
		return cmdConversationsSearch(paths, strings.Join(args[1:], " "))
	case "show":
		if len(args) != 2 {
			return errors.New("usage: jarvix conversations show <id>")
		}
		return cmdConversationsShow(paths, args[1])
	case "open":
		if len(args) != 2 {
			return errors.New("usage: jarvix conversations open <id>")
		}
		return cmdConversationsOpen(paths, args[1])
	case "delete":
		switch {
		case len(args) == 2 && args[1] == "--all":
			return cmdConversationsDelete(paths, "", true)
		case len(args) == 2 && !strings.HasPrefix(args[1], "-"):
			return cmdConversationsDelete(paths, args[1], false)
		default:
			return errors.New("usage: jarvix conversations delete <id> | jarvix conversations delete --all")
		}
	default:
		return fmt.Errorf("usage: jarvix conversations [list | search <query> | show <id> | open <id> | delete <id>|--all]")
	}
}
