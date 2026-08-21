package main

import (
	"bufio"
	"context"
	"fmt"
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
)

func cmdStatus(paths config.Paths) error {
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

func cmdNewConversation(paths config.Paths) error {
	client, err := ipc.Dial(paths.Socket)
	if err != nil {
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
			case "tts.started":
				fmt.Println("🔊 speaking (jarvix cancel to stop)")
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

// cmdArtifacts lists recent artifacts straight off the filesystem: the
// artifact directory is the source of truth, so this works with the daemon
// stopped — same spirit as `jarvix config` and `jarvix doctor`.
func cmdArtifacts(cfg config.Config) error {
	dir := cfg.Artifacts.Dir
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		fmt.Printf("no artifacts yet (they will land in %s)\n", dir)
		return nil
	}
	if err != nil {
		return err
	}

	type artifact struct {
		name    string
		kind    string
		modTime time.Time
	}
	var list []artifact
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue // deleted between ReadDir and Info; not worth failing over
		}
		list = append(list, artifact{
			name:    entry.Name(),
			kind:    artifactKind(entry.Name()),
			modTime: info.ModTime(),
		})
	}
	if len(list) == 0 {
		fmt.Printf("no artifacts yet (they will land in %s)\n", dir)
		return nil
	}
	sort.Slice(list, func(i, j int) bool { return list[i].modTime.After(list[j].modTime) })
	if len(list) > artifactsListLimit {
		list = list[:artifactsListLimit]
	}

	fmt.Printf("recent artifacts in %s:\n", dir)
	for _, a := range list {
		fmt.Printf("  %-10s %-9s %s\n", humanAge(time.Since(a.modTime)), a.kind, filepath.Join(dir, a.name))
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
