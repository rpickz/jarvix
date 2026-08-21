package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/doctor"
	"github.com/rpickz/jarvix/internal/history"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/session"
)

// cmdStatus prints the daemon's state. With last set, it also prints the
// latency budget of the most recent interaction — the "why did that feel slow"
// answer, without asking anyone to tail the journal.
func cmdStatus(paths config.Paths, last bool) error {
	client, err := ipc.Dial(paths.Socket)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	var status map[string]any
	if err := client.Call("status.get", nil, &status); err != nil {
		return err
	}
	fmt.Printf("state:    %v\n", status["state"])
	if id, _ := status["session_id"].(string); id != "" {
		fmt.Printf("session:  %v\n", id)
	}
	fmt.Printf("version:  %v (protocol %v)\n", status["version"], status["protocol"])
	fmt.Printf("socket:   %s\n", paths.Socket)
	printWarmWorkers(status["warm"])
	if last {
		printTimings(status["last_timings"])
		// "What did that cost?" and "what did it see?" are the same question
		// asked of the same interaction, so one flag answers both (ADR 0019).
		if err := printLastContext(client); err != nil {
			return err
		}
	}
	if pol, ok := status["policy"].(map[string]any); ok {
		fmt.Printf("policy:   default=%v confirm_timeout=%vs remember_for_conversation=%v\n",
			pol["default"], pol["confirm_timeout_sec"], pol["remember_for_conversation"])
		if tools, ok := pol["tools"].(map[string]any); ok {
			names := make([]string, 0, len(tools))
			for name := range tools {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				fmt.Printf("          %s: %v\n", name, tools[name])
			}
		}
	}
	return nil
}

// timingLabels turn the wire keys of session.timings into the pipeline stages
// a person recognises. Order matters: the pipeline should read as a pipeline.
var timingLabels = []struct{ key, label string }{
	{session.StageCaptureToTranscript, "release → transcript"},
	{session.StageContext, "desktop context gathered"},
	{session.StageTranscriptToDelta, "transcript → first token (model)"},
	{session.StageDeltaToFirstPCM, "first token → first audio sample"},
	{session.StageFirstPCMToAudioOut, "first sample → audio out"},
	{session.StageReleaseToFirstAudio, "release → first audio (total)"},
	{session.StageJarvixOverhead, "  of which Jarvix (excl. model)"},
}

// printTimings renders the last session's latency budget.
func printTimings(v any) {
	report, ok := v.(map[string]any)
	if !ok || len(report) == 0 {
		fmt.Println("last:     no interaction has finished since jarvixd started")
		return
	}
	fmt.Printf("last:     session %v\n", report["session_id"])
	for _, stage := range timingLabels {
		ms, ok := report[stage.key]
		if !ok {
			continue
		}
		fmt.Printf("          %-33s %5.0f ms\n", stage.label, toFloat(ms))
	}
}

// printWarmWorkers summarises the supervised engine processes, one line each,
// so the memory they hold is visible where the daemon's state is.
func printWarmWorkers(v any) {
	workers, ok := v.([]any)
	if !ok || len(workers) == 0 {
		return
	}
	for _, entry := range workers {
		w, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		state := "cold"
		if running, _ := w["running"].(bool); running {
			state = fmt.Sprintf("warm, %.0f MB, up %.0fs", toFloat(w["rss_mb"]), toFloat(w["uptime_sec"]))
		}
		line := fmt.Sprintf("warm:     %-8v %s", w["name"], state)
		if restarts := toFloat(w["restarts"]); restarts > 0 {
			line += fmt.Sprintf(", %.0f restarts", restarts)
		}
		fmt.Println(line)
	}
}

// toFloat reads a JSON number whichever way it decoded.
func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	case int:
		return float64(n)
	}
	return 0
}

// cmdConfirm answers a pending tool confirmation: the keyed counterpart to
// saying "yes" or "no" out loud.
func cmdConfirm(paths config.Paths, approved bool) error {
	client, err := ipc.Dial(paths.Socket)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	if err := client.Call("session.confirm", map[string]bool{"approved": approved}, nil); err != nil {
		return err
	}
	if approved {
		fmt.Println("confirmed — running the command")
	} else {
		fmt.Println("declined — the command will not run")
	}
	return nil
}

func cmdCancel(paths config.Paths) error {
	client, err := ipc.Dial(paths.Socket)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	return client.Call("session.cancel", nil, nil)
}

// daemonIsDown reports whether a dial error means "nothing is listening on
// the socket" as opposed to "something went wrong reaching it".
//
// The distinction decides whether `jarvix new` may delete the user's saved
// conversation, so it is drawn narrowly: only a missing socket file (ENOENT)
// and a socket nobody is accepting on (ECONNREFUSED, which is also what
// connecting to a stale non-socket file gives) count as down. A permission
// error, a misconfigured path, a dial timeout — anything else — leaves the
// history alone, because the daemon may well be running and holding a
// conversation this command has no business destroying.
//
// ipc.Dial wraps the net error with %w, and net/os wrap the syscall errno the
// same way, so errors.Is reaches the errno through all three layers.
func daemonIsDown(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED)
}

func cmdNewConversation(paths config.Paths) error {
	client, err := ipc.Dial(paths.Socket)
	if err != nil {
		if !daemonIsDown(err) {
			// Destroying the saved conversation on an error we do not
			// understand is unrecoverable data loss; report and change
			// nothing (raised in review of #16).
			return fmt.Errorf("could not reach jarvixd, so the saved conversation was left untouched: %w", err)
		}
		// No daemon means no in-memory thread to reset — but a persisted
		// conversation may still sit on disk, and it would resurrect when the
		// daemon next starts. Clear it directly so "new" always means new.
		if clearErr := (&history.File{Path: paths.HistoryFile()}).Clear(); clearErr != nil {
			return clearErr
		}
		fmt.Println("started a fresh conversation (daemon not running; cleared saved history)")
		return nil
	}
	defer func() { _ = client.Close() }()
	if err := client.Call("conversation.reset", nil, nil); err != nil {
		return err
	}
	fmt.Println("started a fresh conversation")
	return nil
}

// cmdPTT implements the push-to-talk commands bound to keys. They must
// return fast — a human is at the keyboard.
//
// "toggle" is the primary binding (tap to listen, tap again to submit):
// Hyprland release-binds are only reliable for bare keys, not modifier
// chords, so the chord gets tap semantics and hold-to-talk lives on a bare
// key using start/stop (see ADR 0004).
func cmdPTT(paths config.Paths, phase string) error {
	client, err := ipc.Dial(paths.Socket)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	beginListening := func() error {
		// While a tool confirmation is pending, this press answers it: the
		// pending session must keep waiting, so no session.start (which
		// would interrupt it) — the capture flows into the confirmation.
		var status map[string]any
		if err := client.Call("status.get", nil, &status); err == nil &&
			status["state"] == "awaiting_confirmation" {
			return client.Call("voice.start", nil, nil)
		}
		if err := client.Call("session.start", nil, nil); err != nil {
			return err
		}
		return client.Call("voice.start", nil, nil)
	}
	submit := func() error {
		var stopped struct {
			Discarded bool `json:"discarded"`
		}
		if err := client.Call("voice.stop", nil, &stopped); err != nil {
			return err
		}
		if stopped.Discarded {
			return nil // too short to mean anything; the session already ended
		}
		return client.Call("session.submit", nil, nil)
	}

	switch phase {
	case "start":
		return beginListening()
	case "stop":
		return submit()
	default: // toggle
		var status map[string]any
		if err := client.Call("status.get", nil, &status); err != nil {
			return err
		}
		if status["ptt"] == "daemon" {
			// The daemon watches the chord itself (real hold-to-talk); the
			// Hyprland tap binding must not drive the session as well.
			return nil
		}
		if status["state"] == "listening" {
			return submit()
		}
		// Idle: start listening. Any other active state (thinking, speaking,
		// ...): interrupt it and start listening — session.start cancels the
		// running session first.
		return beginListening()
	}
}

// cmdWindow toggles the conversation window. The window is rendered by the
// Omarchy shell plugin and shows its own "daemon is not running" state, so
// this deliberately never touches the daemon socket — the window must open
// even when jarvixd is down (see ADR 0013).
func cmdWindow() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	windows := &desktop.WindowClient{}
	return windows.Toggle(ctx)
}

func cmdAsk(paths config.Paths, question string) error {
	client, err := ipc.Dial(paths.Socket)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	if err := client.Call("session.start", nil, nil); err != nil {
		return err
	}
	if err := client.Call("session.submit", map[string]string{"text": question}, nil); err != nil {
		return err
	}
	return followSession(client, false)
}

func cmdListen(paths config.Paths) error {
	client, err := ipc.Dial(paths.Socket)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	if err := client.Call("session.start", nil, nil); err != nil {
		return err
	}
	if err := client.Call("voice.start", nil, nil); err != nil {
		return err
	}
	fmt.Println("● Listening — press Enter to finish, Ctrl+C to cancel")

	// Ctrl+C cancels the whole interaction, matching Escape in the overlay.
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(interrupt)
	enter := make(chan struct{})
	go func() {
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		close(enter)
	}()
	select {
	case <-enter:
	case <-interrupt:
		fmt.Println("\ncancelled")
		return client.Call("session.cancel", nil, nil)
	}

	var stopped struct {
		Discarded bool `json:"discarded"`
	}
	if err := client.Call("voice.stop", nil, &stopped); err != nil {
		return err
	}
	if stopped.Discarded {
		fmt.Println("recording too short — discarded")
		return nil
	}
	if err := client.Call("session.submit", nil, nil); err != nil {
		return err
	}
	return followSession(client, true)
}

// followSession streams events for the active session to the terminal until
// it ends. Ctrl+C during the session cancels it.
func followSession(client *ipc.Client, showTranscript bool) error {
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(interrupt)

	responding := false
	for {
		select {
		case <-interrupt:
			fmt.Println("\ncancelled")
			return client.Call("session.cancel", nil, nil)
		case ev, ok := <-client.Events():
			if !ok {
				return fmt.Errorf("connection to jarvixd lost")
			}
			switch ev.Type {
			case "state.changed":
				if ev.Data["state"] == "transcribing" {
					fmt.Println("… transcribing")
				}
			case "transcript.final":
				if showTranscript {
					fmt.Printf("you: %v\n", ev.Data["text"])
				}
			case "assistant.delta":
				if !responding {
					responding = true
				}
				fmt.Print(ev.Data["content"])
			case "assistant.finished":
				fmt.Println()
			case "intent.executed":
				// A deterministic intent produces no assistant.delta stream,
				// so without this the CLI would say nothing at all about the
				// thing it just did.
				if ack, _ := ev.Data["acknowledgement"].(string); ack != "" {
					fmt.Printf("⚡ %s\n", ack)
				}
			case "tts.started":
				fmt.Println("🔊 speaking (jarvix cancel to stop)")
			case "tool.started":
				// Only slow tools carry a label; the rest finish before
				// anyone would read a line about them.
				if detail, ok := ev.Data["detail"].(string); ok && detail != "" {
					fmt.Println("… " + detail)
				}
			case "tool.progress":
				fmt.Printf("… %v\n", ev.Data["message"])
			case "tool.confirmation_required":
				fmt.Printf("? %v\n  command: %v\n  answer with: jarvix confirm | jarvix deny (auto-declines in %vs)\n",
					ev.Data["summary"], ev.Data["command"], ev.Data["timeout_sec"])
			case "tool.confirmed":
				fmt.Println("✓ confirmed")
			case "tool.declined":
				fmt.Printf("✗ declined (%v) — nothing was run\n", ev.Data["source"])
			case "tool.denied":
				fmt.Printf("✗ denied by policy (%v): %v\n", ev.Data["rule"], ev.Data["command"])
			case "session.finished":
				return nil
			case "session.cancelled":
				return nil
			case "error":
				return fmt.Errorf("%v (stage: %v)", ev.Data["message"], ev.Data["stage"])
			}
		}
	}
}

func cmdDoctor(cfg config.Config, paths config.Paths) error {
	fmt.Println("Jarvix Doctor")
	fmt.Println()
	results := doctor.Run(cfg, paths)
	for _, r := range results {
		tag := map[doctor.Status]string{
			doctor.OK: "[OK]  ", doctor.Warn: "[WARN]", doctor.Fail: "[FAIL]",
		}[r.Status]
		line := tag + " " + r.Name
		if r.Detail != "" {
			line += " — " + r.Detail
		}
		fmt.Println(line)
		if r.Fix != "" && r.Status != doctor.OK {
			for _, fixLine := range splitLines(r.Fix) {
				fmt.Println("        " + fixLine)
			}
		}
	}
	fmt.Println()
	if doctor.Healthy(results) {
		fmt.Println("Jarvix appears ready.")
		return nil
	}
	fmt.Println("Fix the failures above, then run jarvix doctor again.")
	return errChecksFailed
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// artifactsListLimit keeps the listing to what "recent" means at a glance.
const artifactsListLimit = 20

// artifactEntry is one file in the artifact directory, already described the
// way a reader wants it: no timestamps to do arithmetic on, no extensions to
// interpret. The JSON tags are the contract the Omarchy bar widget's panel
// reads (`jarvix artifacts --json`) — QML gets a list to draw, and none of
// the deciding.
type artifactEntry struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	Path string `json:"path"`
	Age  string `json:"age"`
	// Modified is RFC 3339 so a client that wants to re-sort or re-phrase can,
	// without this command having to guess which it wanted.
	Modified string `json:"modified"`
}

// artifactListing is the whole answer, directory included: the directory is
// configurable (artifacts.dir), so a caller that only got the files would
// have to parse config.toml to say where they live, or to offer "open the
// folder". Reporting it here keeps that knowledge in Go.
type artifactListing struct {
	Dir       string          `json:"dir"`
	Artifacts []artifactEntry `json:"artifacts"`
}

// recentArtifacts reads the artifact directory and returns the newest
// `limit` files. `now` is a parameter rather than a call to time.Now so the
// age strings are testable without sleeping.
//
// A missing directory is not an error: it is the ordinary state of a fresh
// install that has not made anything yet, and both renderings say so.
func recentArtifacts(dir string, limit int, now time.Time) (artifactListing, error) {
	listing := artifactListing{Dir: dir, Artifacts: []artifactEntry{}}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return listing, nil
	}
	if err != nil {
		return listing, err
	}

	type found struct {
		entry   artifactEntry
		modTime time.Time
	}
	var list []found
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue // deleted between ReadDir and Info; not worth failing over
		}
		list = append(list, found{
			entry: artifactEntry{
				Name:     entry.Name(),
				Kind:     artifactKind(entry.Name()),
				Path:     filepath.Join(dir, entry.Name()),
				Age:      humanAge(now.Sub(info.ModTime())),
				Modified: info.ModTime().Format(time.RFC3339),
			},
			modTime: info.ModTime(),
		})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].modTime.After(list[j].modTime) })
	if len(list) > limit {
		list = list[:limit]
	}
	for _, f := range list {
		listing.Artifacts = append(listing.Artifacts, f.entry)
	}
	return listing, nil
}

// cmdArtifacts lists recent artifacts straight off the filesystem: the
// artifact directory is the source of truth, so this works with the daemon
// stopped — same spirit as `jarvix config` and `jarvix doctor`.
//
// `--json` prints the same listing as one JSON object. That is what the
// Omarchy bar widget's panel runs to fill its "recent artifacts" section:
// the shell plugin gets the names, kinds, ages, and paths already decided
// here rather than reimplementing any of it in QML (ADR 0013).
func cmdArtifacts(cfg config.Config, asJSON bool) error {
	listing, err := recentArtifacts(cfg.Artifacts.Dir, artifactsListLimit, time.Now())
	if err != nil {
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

	if len(listing.Artifacts) == 0 {
		fmt.Printf("no artifacts yet (they will land in %s)\n", listing.Dir)
		return nil
	}
	fmt.Printf("recent artifacts in %s:\n", listing.Dir)
	for _, a := range listing.Artifacts {
		fmt.Printf("  %-10s %-9s %s\n", a.Age, a.Kind, a.Path)
	}
	return nil
}

// artifactKind labels a file for the listing by its extension.
func artifactKind(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mmd":
		return "source"
	case ".svg", ".png":
		return "diagram"
	case ".md":
		return "document"
	case ".csv":
		return "spreadsheet"
	case ".excalidraw":
		return "sketch"
	default:
		return strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")
	}
}

// humanAge renders a duration the way a person scanning a list thinks about
// it — coarse buckets, most recent first is already the sort order.
func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func cmdConfig(cfg config.Config, paths config.Paths) error {
	fmt.Println("# Effective configuration (secrets redacted)")
	fmt.Println("# File:", paths.ConfigFile())
	if _, err := os.Stat(paths.ConfigFile()); err != nil {
		fmt.Println("# (file does not exist; these are the built-in defaults)")
	}
	fmt.Println()
	enc := toml.NewEncoder(os.Stdout)
	if err := enc.Encode(cfg.Redact()); err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "\nwarning:", err)
	}
	return nil
}
