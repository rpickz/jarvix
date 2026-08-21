package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/ipc"
)

// cmdStatusLast prints the desktop context Jarvix captured for its most
// recent question (ADR 0018) — the answer to "what did it just see?".
//
// It prints the captured text itself, not a summary of it. The whole point of
// the audit is that the user can compare what was sent with what they thought
// was on screen, and character counts cannot do that. What is shown is
// exactly what reached the model: already truncated at the configured cap,
// already redacted if it looked like a secret.
func cmdStatusLast(paths config.Paths) error {
	client, err := ipc.Dial(paths.Socket)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	var last struct {
		Captured   bool   `json:"captured"`
		SessionID  string `json:"session_id"`
		AgeSec     int    `json:"age_sec"`
		DurationMs int64  `json:"duration_ms"`
		Sources    []struct {
			Source    string `json:"source"`
			Text      string `json:"text"`
			Chars     int    `json:"chars"`
			Truncated bool   `json:"truncated"`
			Redacted  bool   `json:"redacted"`
		} `json:"sources"`
	}
	if err := client.Call("context.last", nil, &last); err != nil {
		return err
	}
	if !last.Captured {
		fmt.Println("no desktop context has been captured yet.")
		fmt.Println("(sources are configured under [context]; jarvix doctor shows which are enabled)")
		return nil
	}

	fmt.Printf("last context: session %s, captured %s, gathered in %dms\n",
		last.SessionID, humanAge(time.Duration(last.AgeSec)*time.Second), last.DurationMs)
	if len(last.Sources) == 0 {
		fmt.Println("  (nothing was captured — no window, selection, or clipboard content was available)")
		return nil
	}
	for _, s := range last.Sources {
		note := fmt.Sprintf("%d chars", s.Chars)
		if s.Truncated {
			note += ", truncated"
		}
		if s.Redacted {
			note += ", redacted"
		}
		fmt.Printf("  %-10s (%s)\n", s.Source, note)
		for _, line := range strings.Split(s.Text, "\n") {
			fmt.Println("    " + line)
		}
	}
	return nil
}
