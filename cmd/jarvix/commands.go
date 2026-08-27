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
	"github.com/rpickz/jarvix/internal/ai/openaicompat"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/desktop"
	"github.com/rpickz/jarvix/internal/doctor"
	"github.com/rpickz/jarvix/internal/history"
	"github.com/rpickz/jarvix/internal/ipc"
	"github.com/rpickz/jarvix/internal/routine"
	"github.com/rpickz/jarvix/internal/session"
)

// cmdStatus prints the daemon's state. With last set, it also prints the
// latency budget of the most recent interaction — the "why did that feel slow"
// answer, without asking anyone to tail the journal.
func cmdStatus(cfg config.Config, paths config.Paths, last bool) error {
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
	printWake(status["wake"])
	printWarmWorkers(status["warm"])
	printConversationSearch(status["conversations"])
	printPromptBudget(cfg, status["prompt_budget"])
	if last {
		printTimings(status["last_timings"])
		// "What did that cost?" and "what did it see?" are the same question
		// asked of the same interaction, so one flag answers both (ADR 0019).
		if err := printLastContext(client); err != nil {
			return err
		}
		// ...and "what did it do with my keyboard?" is the third (ADR 0023).
		printLastTyping(status["last_typing"])
		// ...and "which remembered facts was it given?" is the fourth
		// (ADR 0025).
		if err := printLastMemory(client); err != nil {
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

// printConversationSearch renders the archive-search state (issue #59):
// active with a count, or inactive with the reason — never an error, because
// an empty archive with retention off is a choice working as configured.
func printConversationSearch(v any) {
	report, ok := v.(map[string]any)
	if !ok || len(report) == 0 {
		return // an older daemon that predates the search surface
	}
	if search, _ := report["search"].(string); search == "inactive" {
		fmt.Println("search:   conversation search inactive (retention off, nothing archived)")
		return
	}
	fmt.Printf("search:   conversation search active (%.0f archived)\n", toFloat(report["archived"]))
}

// printPromptBudget renders what one turn sends before the user says a word,
// against the context window the model is actually served with — the check
// that would have named the live incident's silent truncation (issue #71).
// The window is read from ollama best-effort with a short timeout: other
// providers, or an unreachable ollama, just print the budget alone.
func printPromptBudget(cfg config.Config, v any) {
	budget, ok := doctor.BudgetFromReport(v)
	if !ok {
		return // an older daemon that predates the budget surface
	}
	line := fmt.Sprintf("prompt:   ~%d tokens before you speak (system prompt + tools + memory + context + headroom)",
		budget.Floor())
	if window, ok := servedContextWindow(cfg); ok {
		verdict := "fits"
		if window < budget.Floor() {
			verdict = "TOO SMALL — run jarvix doctor"
		}
		line += fmt.Sprintf("\n          model context ~%d tokens: %s", window, verdict)
	}
	fmt.Println(line)
}

// servedContextWindow best-effort reads the served model's window from
// ollama. ok is false for other providers and for any failure — status must
// never block or nag on a provider that cannot answer.
func servedContextWindow(cfg config.Config) (int, bool) {
	if cfg.AI.Provider != "ollama" {
		return 0, false
	}
	ep, ok := cfg.Endpoint()
	if !ok {
		return 0, false
	}
	client := openaicompat.New(cfg.AI.Provider, ep.BaseURL, ep.Key())
	served, err := client.OllamaServedContext(context.Background(), cfg.AI.Model)
	if err != nil {
		return 0, false
	}
	if served.NumCtx > 0 {
		return served.NumCtx, true
	}
	window := doctor.OllamaDefaultContext
	if served.MaxCtx > 0 && served.MaxCtx < window {
		window = served.MaxCtx
	}
	return window, true
}

// timingLabels turn the wire keys of session.timings into the pipeline stages
// a person recognises. Order matters: the pipeline should read as a pipeline.
var timingLabels = []struct{ key, label string }{
	{session.StageCaptureToTranscript, "release → transcript"},
	{session.StageContext, "desktop context gathered"},
	{session.StageTranscriptToDelta, "transcript → first output (model)"},
	{session.StageDeltaToFirstPCM, "first output → first audio sample"},
	{session.StageFirstPCMToAudioOut, "first sample → audio out"},
	{session.StageToolRuns, "tools ran for (excluded)"},
	{session.StageConfirmWait, "waiting on your confirmation (excluded)"},
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
	// The one non-duration key (issue #120): queued sentences dropped unplayed
	// because a newer turn superseded them. Absent when nothing was dropped,
	// like every stage that did not happen.
	if n, ok := report[session.StageSupersededSentences]; ok {
		fmt.Printf("          %-33s %5.0f skipped\n", "stale sentences superseded", toFloat(n))
	}
}

// printLastTyping renders the typing audit trail: the most recent thing Jarvix
// did with the user's keyboard (ADR 0023).
//
// It reports the target, the length, whether a human approved it, and the
// outcome — and it deliberately cannot report the text, because the daemon
// does not keep it. That is the whole design: a user must be able to audit
// what was typed *where*, without the audit itself becoming somewhere their
// dictated password is written down.
func printLastTyping(v any) {
	report, ok := v.(map[string]any)
	if !ok || len(report) == 0 {
		return // typing is off, or nothing has been typed since jarvixd started
	}
	approval := "not confirmed (the policy allowed it)"
	if approved, _ := report["approved"].(bool); approved {
		approval = "confirmed by you"
	}
	where, _ := report["window"].(string)
	if where == "" {
		where = "no window"
	}
	fmt.Printf("typing:   %v — %s\n", report["outcome"], where)
	detail := fmt.Sprintf("%.0f characters, %s", toFloat(report["chars"]), approval)
	if key, _ := report["key"].(string); key != "" {
		detail = fmt.Sprintf("key %s, %s", key, approval)
	}
	if terminal, _ := report["terminal"].(bool); terminal {
		detail += ", into a terminal"
	}
	fmt.Printf("          %s\n", detail)
	if reason, _ := report["reason"].(string); reason != "" {
		fmt.Printf("          %s\n", reason)
	}
	fmt.Println("          (the text itself is never recorded)")
}

// printWake reports background listening: the activation mode, and — the
// part worth printing at all — whether a capture process is running and which
// one. "Is my microphone open?" should be answerable by reading a line, and
// then checkable by grepping the pid out of `ps`.
func printWake(v any) {
	report, ok := v.(map[string]any)
	if !ok || len(report) == 0 {
		return
	}
	mode, _ := report["mode"].(string)
	if enabled, _ := report["enabled"].(bool); !enabled {
		fmt.Printf("wake:     off (activation.mode = %s)\n", mode)
		return
	}
	word, _ := report["word"].(string)
	reason, _ := report["last_reason"].(string)
	running, _ := report["running"].(bool)
	muted, _ := report["muted"].(bool)
	capturing, _ := report["capturing"].(bool)
	switch {
	case !running:
		line := fmt.Sprintf("wake:     enabled for %q but not running — push-to-talk only", word)
		if reason != "" {
			line += " (" + reason + ")"
		}
		fmt.Println(line)
	case muted:
		fmt.Printf("wake:     muted — no capture process is running (jarvix unmute to resume)\n")
	case capturing:
		fmt.Printf("wake:     listening for %q — pw-record pid %.0f, %.0fms kept before the wake word\n",
			word, toFloat(report["pid"]), toFloat(report["ring_ms"]))
		if detector, _ := report["detector"].(string); detector != "" {
			fmt.Printf("          detector %s (pid %.0f, %.0f MB), %.0f activations since start\n",
				detector, toFloat(report["detector_pid"]), toFloat(report["detector_rss_mb"]),
				toFloat(report["activations"]))
		}
	default:
		line := fmt.Sprintf("wake:     enabled for %q, capture not up", word)
		if reason != "" {
			line += " (" + reason + ")"
		}
		fmt.Println(line)
	}
}

// cmdMute is the live privacy control: `jarvix mute` closes the microphone,
// `jarvix unmute` opens it again. The daemon only answers once the capture
// process has actually been killed, so what this prints is a fact rather than
// a request that has been filed.
func cmdMute(paths config.Paths, muted bool) error {
	client, err := ipc.Dial(paths.Socket)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	var report map[string]any
	if err := client.Call("wake.mute", map[string]bool{"muted": muted}, &report); err != nil {
		return err
	}
	if enabled, _ := report["enabled"].(bool); !enabled {
		mode, _ := report["mode"].(string)
		fmt.Printf("background listening is off (activation.mode = %s) — nothing is capturing\n", mode)
		return nil
	}
	if running, _ := report["running"].(bool); !running {
		fmt.Println("background listening is enabled but not running — nothing is capturing")
		if reason, _ := report["last_reason"].(string); reason != "" {
			fmt.Println("  reason:", reason)
		}
		return nil
	}
	if muted {
		fmt.Println("muted — the capture process has been killed; nothing is listening")
		return nil
	}
	// Unmuting is the asymmetric half: killing a process is instant, starting
	// one is not, so this deliberately does not claim a pid it would have to
	// invent. `jarvix status` reports the real one a moment later.
	word, _ := report["word"].(string)
	fmt.Printf("listening for %q again — the microphone reopens in a moment (jarvix status to confirm)\n", word)
	return nil
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
	// conversation.new, not conversation.reset: the explicit-end verb
	// (issue #117) also cancels a session in flight, committing its exchange
	// — marked interrupted — into the thread being ended, so `jarvix new`
	// said over a running answer archives that answer instead of orphaning
	// it. The daemon owns that sequencing; composing session.cancel +
	// conversation.reset here would reopen the gap between the two calls.
	if err := client.Call("conversation.new", nil, nil); err != nil {
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

	// A diagram is two files — the .mmd source and its render — but one
	// artifact: listing both would show every diagram twice and make the
	// count lie. First find which base names have a render, so the pass
	// below can fold the source into it. An orphan .mmd (its render deleted,
	// or kept deliberately) still lists: it is the only file the user has.
	rendered := map[string]bool{}
	for _, entry := range entries {
		switch strings.ToLower(filepath.Ext(entry.Name())) {
		case ".png", ".svg":
			rendered[strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))] = true
		}
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
		if strings.ToLower(filepath.Ext(entry.Name())) == ".mmd" &&
			rendered[strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))] {
			continue // the render is the artifact; the source rides along
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

// cmdRoutines lists the configured routines offline, from config.toml — like
// `jarvix config`, it works with the daemon down, because "what have I set
// up?" should not require a running daemon to answer. Steps are summarised
// per line so the listing reads like the routine will run.
func cmdRoutines(cfg config.Config, asJSON bool) error {
	type routineListing struct {
		Name    string   `json:"name"`
		Phrases []string `json:"phrases"`
		Steps   []string `json:"steps"`
		// Incomplete marks a captured routine (#62) still carrying a launch
		// placeholder; it stays marked until a human edits the app in.
		Incomplete bool `json:"incomplete"`
		// Enabled is the shared switch (#93): a parked routine still lists —
		// disabled means switched off, never hidden.
		Enabled bool `json:"enabled"`
	}
	listing := make([]routineListing, 0, len(cfg.Routines))
	for _, r := range cfg.Routines {
		entry := routineListing{Name: r.Name, Phrases: r.Phrases, Steps: []string{},
			Incomplete: r.Incomplete(), Enabled: r.IsEnabled()}
		for _, s := range r.Steps {
			entry.Steps = append(entry.Steps, describeRoutineStep(s))
		}
		listing = append(listing, entry)
	}
	if asJSON {
		out, err := json.Marshal(map[string]any{"routines": listing})
		if err != nil {
			return err
		}
		fmt.Println(string(out))
		return nil
	}
	if len(listing) == 0 {
		fmt.Println("no routines configured (add [[routines]] tables to config.toml; see docs/configuration.md)")
		return nil
	}
	for i, r := range listing {
		if i > 0 {
			fmt.Println()
		}
		marker := ""
		if r.Incomplete {
			marker = " — incomplete: a step still needs its launch command (edit config.toml)"
		}
		if !r.Enabled {
			marker += " — disabled: the phrases will not trigger it (enabled = false)"
		}
		fmt.Printf("%s — say \"%s\"%s\n", r.Name, strings.Join(r.Phrases, `" or "`), marker)
		for _, step := range r.Steps {
			fmt.Printf("  %s\n", step)
		}
	}
	return nil
}

// describeRoutineStep renders one step the way it will execute.
func describeRoutineStep(s config.RoutineStep) string {
	desc := fmt.Sprintf("%s → workspace %d", s.App, s.Workspace)
	if s.App == routine.PlaceholderApp {
		desc = fmt.Sprintf("%s → workspace %d (set the app that launches this window)", s.App, s.Workspace)
	}
	switch {
	case s.Float:
		desc += " (floating"
		if len(s.Size) == 2 {
			desc += fmt.Sprintf(" %dx%d", s.Size[0], s.Size[1])
		}
		if len(s.Position) == 2 {
			desc += fmt.Sprintf(" at %d,%d", s.Position[0], s.Position[1])
		}
		desc += ")"
	case s.Tile != "":
		desc += " (" + s.Tile + ")"
	}
	return desc
}

// cmdScripts lists the configured scripts offline, from config.toml — like
// `jarvix routines`, "what have I set up?" should not need a running daemon.
// The path is always shown: it is what the permission gate's confirmation
// names, and a listing that hid it would hide exactly the fact the ADR 0030
// threat model wants visible.
func cmdScripts(cfg config.Config, asJSON bool) error {
	type scriptListing struct {
		Name       string   `json:"name"`
		Phrases    []string `json:"phrases"`
		Path       string   `json:"path"`
		Report     string   `json:"report"`
		TimeoutSec int      `json:"timeout_sec"`
		// Enabled is the shared switch (#93): a parked script still lists —
		// disabled means switched off, never hidden.
		Enabled bool `json:"enabled"`
	}
	listing := make([]scriptListing, 0, len(cfg.Scripts))
	for i, d := range cfg.ScriptDefinitions() {
		listing = append(listing, scriptListing{Name: d.Name, Phrases: d.Phrases,
			Path: d.Path, Report: string(d.Report), TimeoutSec: int(d.Timeout.Seconds()),
			Enabled: cfg.Scripts[i].IsEnabled()})
	}
	if asJSON {
		out, err := json.Marshal(map[string]any{"scripts": listing})
		if err != nil {
			return err
		}
		fmt.Println(string(out))
		return nil
	}
	if len(listing) == 0 {
		fmt.Println("no scripts configured (add [[scripts]] tables to config.toml; see docs/configuration.md)")
		return nil
	}
	for i, s := range listing {
		if i > 0 {
			fmt.Println()
		}
		marker := ""
		if !s.Enabled {
			marker = " — disabled: the phrases will not trigger it (enabled = false)"
		}
		fmt.Printf("%s — say \"%s\"%s\n", s.Name, strings.Join(s.Phrases, `" or "`), marker)
		fmt.Printf("  runs %s (no arguments) · report %s · timeout %ds\n", s.Path, s.Report, s.TimeoutSec)
	}
	return nil
}

// cmdScriptRun triggers one script through the daemon and follows the
// session — the same gated path the spoken phrase takes, so the terminal
// carries the confirmation question and the outcome exactly as the ear would.
func cmdScriptRun(paths config.Paths, name string) error {
	client, err := ipc.Dial(paths.Socket)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	if err := client.Call("scripts.run", map[string]string{"name": name}, nil); err != nil {
		return err
	}
	return followSession(client, false)
}

// cmdRoutineRun triggers one routine through the daemon and follows the
// session, so the summary — and anything that failed — lands in the terminal
// the way it lands in the ear.
func cmdRoutineRun(paths config.Paths, name string) error {
	client, err := ipc.Dial(paths.Socket)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	if err := client.Call("routines.run", map[string]string{"name": name}, nil); err != nil {
		return err
	}
	return followSession(client, false)
}
