// jarvix is the user-facing control and debug CLI for the Jarvix daemon.
package main

import (
	"errors"
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
  jarvix config                 Show effective configuration (offline)
  jarvix config get [key]       Show the daemon's settings (or one value)
  jarvix config set k=v [...]   Change settings: validated, written to
                                config.toml, applied without a restart
  jarvix config reload          Re-read config.toml into the running daemon
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
	case "confirm":
		err = cmdConfirm(paths, true)
	case "deny":
		err = cmdConfirm(paths, false)
	case "new":
		err = cmdNewConversation(paths)
	case "ptt":
		if len(rest) < 1 || (rest[0] != "start" && rest[0] != "stop" && rest[0] != "toggle") {
			return fail(fmt.Errorf("usage: jarvix ptt start|stop|toggle"))
		}
		err = cmdPTT(paths, rest[0])
	case "window":
		err = cmdWindow()
	case "artifacts":
		err = cmdArtifacts(cfg)
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
		case len(rest) == 0:
			err = cmdSetupWizard(cfg, paths)
		default:
			return fail(fmt.Errorf("usage: jarvix setup | jarvix setup whisper [model] | jarvix setup input"))
		}
	case "config":
		switch {
		case len(rest) == 0:
			err = cmdConfig(cfg, paths)
		case rest[0] == "get":
			key := ""
			if len(rest) > 1 {
				key = rest[1]
			}
			err = cmdConfigGet(paths, key)
		case rest[0] == "set":
			if len(rest) < 2 {
				return fail(fmt.Errorf("usage: jarvix config set key=value [key=value ...]"))
			}
			err = cmdConfigSet(paths, rest[1:])
		case rest[0] == "reload":
			err = cmdConfigReload(paths)
		default:
			return fail(fmt.Errorf("usage: jarvix config [get [key] | set key=value ... | reload]"))
		}
	case "version", "--version", "-v":
		fmt.Println("jarvix", build.Version)
	case "help", "--help", "-h":
		fmt.Println(usage)
	default:
		fmt.Fprintf(os.Stderr, "jarvix: unknown command %q\n\n%s\n", cmd, usage)
		return 2
	}
	if err != nil {
		if errors.Is(err, errChecksFailed) {
			// The command already printed its own report (doctor's check
			// list); only the exit code is left to deliver.
			return 1
		}
		return fail(err)
	}
	return 0
}

// errChecksFailed signals a non-zero exit whose explanation was already
// printed. It exists so commands never call os.Exit themselves — run() is
// the only place an exit code is decided, which is what makes it testable.
var errChecksFailed = errors.New("checks failed")

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "jarvix:", err)
	return 1
}
