// jarvix is the user-facing control and debug CLI for the Jarvix daemon.
package main

import (
	"fmt"
	"os"

	"github.com/rpickz/jarvix/internal/build"
	"github.com/rpickz/jarvix/internal/config"
)

const usage = `jarvix — voice-native computer interaction for Omarchy

Usage:
  jarvix status                 Show daemon state
  jarvix ask "question"         Ask through the full conversation pipeline
  jarvix listen                 Record from the microphone, then ask
  jarvix cancel                 Cancel the current interaction
  jarvix ptt start|stop         Push-to-talk half-commands (used by keybindings)
  jarvix doctor                 Check every dependency and explain failures
  jarvix setup whisper [model]  Download a Whisper model (default: base.en)
  jarvix config                 Show effective configuration
  jarvix version                Show version

The daemon must be running for session commands:
  systemctl --user enable --now jarvixd`

func main() {
	if len(os.Args) < 2 {
		fmt.Println(usage)
		os.Exit(2)
	}
	paths := config.DefaultPaths()
	cfg, err := config.Load(paths.ConfigFile())
	if err != nil {
		fatal(err)
	}

	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "status":
		err = cmdStatus(paths)
	case "ask":
		if len(args) < 1 {
			fatal(fmt.Errorf("usage: jarvix ask \"question\""))
		}
		err = cmdAsk(paths, args[0])
	case "listen":
		err = cmdListen(paths)
	case "cancel":
		err = cmdCancel(paths)
	case "ptt":
		if len(args) < 1 || (args[0] != "start" && args[0] != "stop") {
			fatal(fmt.Errorf("usage: jarvix ptt start|stop"))
		}
		err = cmdPTT(paths, args[0])
	case "doctor":
		err = cmdDoctor(cfg, paths)
	case "setup":
		if len(args) < 1 || args[0] != "whisper" {
			fatal(fmt.Errorf("usage: jarvix setup whisper [model]"))
		}
		model := cfg.STT.Whisper.Model
		if len(args) > 1 {
			model = args[1]
		}
		err = cmdSetupWhisper(paths, model)
	case "config":
		err = cmdConfig(cfg, paths)
	case "version", "--version", "-v":
		fmt.Println("jarvix", build.Version)
	case "help", "--help", "-h":
		fmt.Println(usage)
	default:
		fmt.Fprintf(os.Stderr, "jarvix: unknown command %q\n\n%s\n", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "jarvix:", err)
	os.Exit(1)
}
