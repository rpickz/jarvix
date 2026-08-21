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
  jarvix confirm                Approve the pending tool confirmation
  jarvix deny                   Decline the pending tool confirmation
  jarvix new                    Start a fresh conversation (forget context)
  jarvix ptt toggle             Tap-to-talk: start listening / submit (keybinding)
  jarvix ptt start|stop         Hold-to-talk halves for a bare-key binding
  jarvix window                 Open/close the conversation window
  jarvix artifacts              List recent artifacts (diagrams, documents, sheets, sketches)
  jarvix doctor                 Check every dependency and explain failures
  jarvix setup                  First-run wizard: voice, activation, AI, advisors
  jarvix setup whisper [model]  Download a Whisper model (default: base.en)
  jarvix setup input            Grant keyboard access for real hold-to-talk
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
	case "confirm":
		err = cmdConfirm(paths, true)
	case "deny":
		err = cmdConfirm(paths, false)
	case "new":
		err = cmdNewConversation(paths)
	case "ptt":
		if len(args) < 1 || (args[0] != "start" && args[0] != "stop" && args[0] != "toggle") {
			fatal(fmt.Errorf("usage: jarvix ptt start|stop|toggle"))
		}
		err = cmdPTT(paths, args[0])
	case "window":
		err = cmdWindow()
	case "artifacts":
		err = cmdArtifacts(cfg)
	case "doctor":
		err = cmdDoctor(cfg, paths)
	case "setup":
		switch {
		case len(args) >= 1 && args[0] == "whisper":
			model := cfg.STT.Whisper.Model
			if len(args) > 1 {
				model = args[1]
			}
			err = cmdSetupWhisper(paths, model)
		case len(args) >= 1 && args[0] == "input":
			err = cmdSetupInput()
		case len(args) == 0:
			err = cmdSetupWizard(cfg, paths)
		default:
			fatal(fmt.Errorf("usage: jarvix setup | jarvix setup whisper [model] | jarvix setup input"))
		}
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
