package main

import (
	"bufio"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/BurntSushi/toml"
	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/doctor"
	"github.com/rpickz/jarvix/internal/ipc"
)

func cmdStatus(paths config.Paths) error {
	client, err := ipc.Dial(paths.Socket)
	if err != nil {
		return err
	}
	defer client.Close()
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
	return nil
}

func cmdCancel(paths config.Paths) error {
	client, err := ipc.Dial(paths.Socket)
	if err != nil {
		return err
	}
	defer client.Close()
	return client.Call("session.cancel", nil, nil)
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
	defer client.Close()

	beginListening := func() error {
		if err := client.Call("session.start", nil, nil); err != nil {
			return err
		}
		return client.Call("voice.start", nil, nil)
	}
	submit := func() error {
		if err := client.Call("voice.stop", nil, nil); err != nil {
			return err
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
		if status["state"] == "listening" {
			return submit()
		}
		// Idle: start listening. Any other active state (thinking, speaking,
		// ...): interrupt it and start listening — session.start cancels the
		// running session first.
		return beginListening()
	}
}

func cmdAsk(paths config.Paths, question string) error {
	client, err := ipc.Dial(paths.Socket)
	if err != nil {
		return err
	}
	defer client.Close()
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
	defer client.Close()
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
		bufio.NewReader(os.Stdin).ReadString('\n')
		close(enter)
	}()
	select {
	case <-enter:
	case <-interrupt:
		fmt.Println("\ncancelled")
		return client.Call("session.cancel", nil, nil)
	}

	if err := client.Call("voice.stop", nil, nil); err != nil {
		return err
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
	os.Exit(1)
	return nil
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
