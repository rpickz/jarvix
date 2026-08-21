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
  jarvix new                    Start a fresh conversation (forget context)
  jarvix ptt toggle             Tap-to-talk: start listening / submit (keybinding)
  jarvix ptt start|stop         Hold-to-talk halves for a bare-key binding
  jarvix doctor                 Check every dependency and explain failures
  jarvix setup whisper [model]  Download a Whisper model (default: base.en)
  jarvix setup input            Grant keyboard access for real hold-to-talk
  jarvix config                 Show effective configuration
  jarvix version                Show version

The daemon must be running for session commands:
  systemctl --user enable --now jarvixd`

func main() {
	os.Exit(run(os.Args[1:]))
}

// run dispatches one CLI invocation and returns the process exit code. It is
// the testable seam: main only binds it to os.Args/os.Exit.
func run(args []string) int {
	if len(args) < 1 {
		fmt.Println(usage)
		return 2
	}
	paths := config.DefaultPaths()
	cfg, err := config.Load(paths.ConfigFile())
	if err != nil {
		return fail(err)
	}

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "status":
		err = cmdStatus(paths)
	case "ask":
		if len(rest) < 1 {
			return fail(fmt.Errorf("usage: jarvix ask \"question\""))
		}
		err = cmdAsk(paths, rest[0])
	case "listen":
		err = cmdListen(paths)
	case "cancel":
		err = cmdCancel(paths)
	case "new":
		err = cmdNewConversation(paths)
	case "ptt":
		if len(rest) < 1 || (rest[0] != "start" && rest[0] != "stop" && rest[0] != "toggle") {
			return fail(fmt.Errorf("usage: jarvix ptt start|stop|toggle"))
		}
		err = cmdPTT(paths, rest[0])
	case "doctor":
		err = cmdDoctor(cfg, paths)
	case "setup":
		switch {
		case len(rest) >= 1 && rest[0] == "whisper":
			model := cfg.STT.Whisper.Model
			if len(rest) > 1 {
				model = rest[1]
			}
			err = cmdSetupWhisper(paths, model)
		case len(rest) >= 1 && rest[0] == "input":
			err = cmdSetupInput()
		default:
			return fail(fmt.Errorf("usage: jarvix setup whisper [model] | jarvix setup input"))
		}
	case "config":
		err = cmdConfig(cfg, paths)
	case "version", "--version", "-v":
		fmt.Println("jarvix", build.Version)
	case "help", "--help", "-h":
		fmt.Println(usage)
	default:
		fmt.Fprintf(os.Stderr, "jarvix: unknown command %q\n\n%s\n", cmd, usage)
		return 2
	}
	if err != nil {
		return fail(err)
	}
	return 0
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "jarvix:", err)
	return 1
}
