package doctor

import (
	"fmt"
	"strings"
	"time"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/ipc"
)

// knowledgeChecks reports the feed cache (ADR 0031): per feed, is the command
// it names actually here — the same LookPath-or-stat probe the fetcher makes,
// so doctor's answer is the one the daemon will get — and, from the running
// daemon, one summary of the scheduler's health ("2 fresh, 1 failing since
// …"). Detection never runs a feed command: doctor must stay instant, and a
// fetch is a real action against whatever service the command talks to.
//
// A failing feed is a Warn, not a Fail: the last good value still serves,
// with its age disclosed — that degradation is the design, and this is where
// it stops being silent. Values themselves never appear in doctor output;
// terminal output gets pasted into issues, and feed values may be sensitive.
func knowledgeChecks(cfg config.Config, paths config.Paths) []Result {
	feeds := cfg.Knowledge.Feeds
	if len(feeds) == 0 {
		return []Result{{Status: OK, Name: "knowledge feeds",
			Detail: "none configured — [[knowledge.feeds]] tables keep changing facts fetched"}}
	}
	results := make([]Result, 0, len(feeds)+1)
	for _, f := range feeds {
		label := fmt.Sprintf("feed %q command", f.Name)
		binary := ""
		if len(f.Command) > 0 {
			binary = f.Command[0]
		}
		path, err := lookAdvisor(binary)
		if err != nil {
			results = append(results, Result{Status: Warn, Name: label,
				Detail: fmt.Sprintf("%s not found (%v)", binary, err),
				Fix: fmt.Sprintf("Install the command, point the feed's command at it, or remove "+
					"the [[knowledge.feeds]] table for %q", f.Name)})
			continue
		}
		results = append(results, Result{Status: OK, Name: label, Detail: path})
	}
	return append(results, knowledgeStatusCheck(paths))
}

// knowledgeStatusCheck asks the running daemon how the feeds are doing. Only
// the daemon knows: the scheduler is its component, and the failure trail
// lives in its state.
func knowledgeStatusCheck(paths config.Paths) Result {
	const name = "knowledge feeds"
	client, err := ipc.Dial(paths.Socket)
	if err != nil {
		return Result{Status: Warn, Name: name,
			Detail: "configured, but jarvixd is not running so nothing is being refreshed",
			Fix:    "Start it: systemctl --user start jarvixd"}
	}
	defer func() { _ = client.Close() }()
	var status struct {
		Enabled bool `json:"enabled"`
		Feeds   []struct {
			Name         string `json:"name"`
			HasValue     bool   `json:"has_value"`
			Stale        bool   `json:"stale"`
			Failing      bool   `json:"failing"`
			FailingSince string `json:"failing_since"`
			LastError    string `json:"last_error"`
		} `json:"feeds"`
	}
	if err := client.Call("knowledge.status", nil, &status); err != nil {
		return Result{Status: Warn, Name: name, Detail: "jarvixd did not answer: " + err.Error()}
	}
	if !status.Enabled {
		return Result{Status: Warn, Name: name,
			Detail: "configured in the file, but the running daemon has no feeds",
			Fix:    "Restart it to pick them up: systemctl --user restart jarvixd"}
	}

	fresh, stale, cold := 0, 0, 0
	var failing []string
	for _, f := range status.Feeds {
		switch {
		case f.Failing:
			since := f.FailingSince
			if t, err := time.Parse(time.RFC3339, f.FailingSince); err == nil {
				since = t.Local().Format("2006-01-02 15:04")
			}
			detail := fmt.Sprintf("%s failing since %s", f.Name, since)
			if f.LastError != "" {
				detail += " (" + f.LastError + ")"
			}
			failing = append(failing, detail)
		case !f.HasValue:
			cold++
		case f.Stale:
			stale++
		default:
			fresh++
		}
	}
	parts := []string{fmt.Sprintf("%d fresh", fresh)}
	if stale > 0 {
		parts = append(parts, fmt.Sprintf("%d stale", stale))
	}
	if cold > 0 {
		parts = append(parts, fmt.Sprintf("%d not yet fetched", cold))
	}
	detail := strings.Join(parts, ", ")
	if len(failing) > 0 {
		return Result{Status: Warn, Name: name,
			Detail: detail + " — " + strings.Join(failing, "; "),
			Fix: "Run the feed's command by hand to see why it fails; the last good value " +
				"still serves with its age disclosed"}
	}
	return Result{Status: OK, Name: name, Detail: detail}
}
