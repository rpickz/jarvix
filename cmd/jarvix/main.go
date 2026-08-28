// jarvix is the user-facing control and debug CLI for the Jarvix daemon.
package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/rpickz/jarvix/internal/build"
	"github.com/rpickz/jarvix/internal/config"
)

const usage = `jarvix — voice-native computer interaction for Omarchy

Usage:
  jarvix status                 Show daemon state and warm-engine workers
  jarvix status --last          ...plus the last interaction's stage latencies,
                                the desktop context and remembered facts it was given
  jarvix ask "question"         Ask through the full conversation pipeline
  jarvix listen                 Record from the microphone, then ask
  jarvix cancel                 Cancel the current interaction
  jarvix say-again [n]          Speak a message again (default: the last answer)
  jarvix confirm                Approve the pending tool confirmation
  jarvix deny                   Decline the pending tool confirmation
  jarvix new                    Start a fresh conversation (the old one is archived)
  jarvix conversations          List archived conversations, newest first
  jarvix conversations search <query>  Find what was said, ranked best first
  jarvix conversations show <id>   Print one conversation (read-only)
  jarvix conversations open <id>   Continue one as the active conversation
  jarvix conversations delete <id> Delete one from disk (--all deletes every one)
  jarvix memory list [query]    List remembered facts (the knowledge base)
  jarvix memory forget <what>   Delete a remembered fact, by id or by words
  jarvix ptt toggle             Tap-to-talk: start listening / submit (keybinding)
  jarvix ptt start|stop         Hold-to-talk halves for a bare-key binding
  jarvix mute                   Close the microphone: kill background capture
  jarvix unmute                 Listen for the wake word again
  jarvix window                 Open/close the conversation window
  jarvix windows [--json]       List open windows with their nicknames
  jarvix windows name <nickname> [window]  Nickname a window (default: the focused one)
  jarvix routines [--json]      List the configured routines and their phrases
  jarvix routines run "name"    Run one routine (same as speaking its phrase)
  jarvix scripts [--json]       List the configured scripts and their phrases
  jarvix scripts run "name"     Run one script (same as speaking its phrase)
  jarvix artifacts [--json]     List recent artifacts (diagrams, documents, sheets, sketches)
  jarvix voices [--json]        List installed voices by language, accent, and gender
  jarvix doctor                 Check every dependency and explain failures
  jarvix doctor --gate          Run only the upgrade health gate's critical subset
  jarvix upgrade                Update to origin/main: build, install, restart,
                                health-gate — rolled back automatically on failure
  jarvix upgrade --check        Report available vs installed, changing nothing
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
	// Attach the machine's installed voices so validation can say "that voice
	// is not installed, try these" instead of leaving it to fail at synthesis
	// time. The catalog reads nothing until something asks it to, so commands
	// that never validate pay nothing for it.
	cfg.Voices = cfg.InstalledVoices(paths)

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "status":
		// An unrecognised flag is refused rather than silently treated as
		// "no flag": `jarvix status --lastt` must not look like it worked.
		switch {
		case len(rest) == 0:
			err = cmdStatus(cfg, paths, false)
		case rest[0] == "--last":
			err = cmdStatus(cfg, paths, true)
		default:
			return fail(fmt.Errorf("usage: jarvix status [--last]"))
		}
	case "ask":
		if len(rest) < 1 {
			return fail(fmt.Errorf("usage: jarvix ask \"question\""))
		}
		err = cmdAsk(paths, rest[0])
	case "listen":
		err = cmdListen(paths)
	case "cancel":
		err = cmdCancel(paths)
	case "say-again":
		turn := 0
		if len(rest) > 1 {
			return fail(fmt.Errorf("usage: jarvix say-again [turn]"))
		}
		if len(rest) == 1 {
			turn, err = strconv.Atoi(rest[0])
			if err != nil || turn < 1 {
				return fail(fmt.Errorf("usage: jarvix say-again [turn] — turn is a position in the current conversation, counting from 1"))
			}
		}
		err = cmdSayAgain(paths, turn)
	case "confirm":
		err = cmdConfirm(paths, true)
	case "deny":
		err = cmdConfirm(paths, false)
	case "new":
		err = cmdNewConversation(paths)
	case "conversations":
		err = cmdConversations(paths, rest)
	case "memory":
		switch {
		case len(rest) >= 1 && rest[0] == "list":
			query := ""
			if len(rest) > 1 {
				// Join so `jarvix memory list staging server` needs no quotes
				// — a query is words, and words arrive as arguments.
				query = strings.Join(rest[1:], " ")
			}
			err = cmdMemoryList(paths, query)
		case len(rest) >= 2 && rest[0] == "forget":
			err = cmdMemoryForget(paths, strings.Join(rest[1:], " "))
		default:
			return fail(fmt.Errorf("usage: jarvix memory list [query] | jarvix memory forget <id-or-words>"))
		}
	case "ptt":
		if len(rest) < 1 || (rest[0] != "start" && rest[0] != "stop" && rest[0] != "toggle") {
			return fail(fmt.Errorf("usage: jarvix ptt start|stop|toggle"))
		}
		err = cmdPTT(paths, rest[0])
	case "mute":
		err = cmdMute(paths, true)
	case "unmute":
		err = cmdMute(paths, false)
	case "window":
		err = cmdWindow()
	case "windows":
		switch {
		case len(rest) == 0:
			err = cmdWindows(paths, false)
		case rest[0] == "--json" && len(rest) == 1:
			err = cmdWindows(paths, true)
		case rest[0] == "name" && len(rest) >= 2:
			// Words after the nickname describe which window; none means
			// the focused one, exactly as the spoken assignment works.
			err = cmdWindowsName(paths, rest[1], strings.Join(rest[2:], " "))
		default:
			return fail(fmt.Errorf("usage: jarvix windows [--json] | jarvix windows name <nickname> [window]"))
		}
	case "routines":
		switch {
		case len(rest) == 0:
			err = cmdRoutines(cfg, false)
		case rest[0] == "--json" && len(rest) == 1:
			err = cmdRoutines(cfg, true)
		case rest[0] == "run" && len(rest) == 2:
			err = cmdRoutineRun(paths, rest[1])
		default:
			return fail(fmt.Errorf("usage: jarvix routines [--json] | jarvix routines run \"name\""))
		}
	case "scripts":
		switch {
		case len(rest) == 0:
			err = cmdScripts(cfg, false)
		case rest[0] == "--json" && len(rest) == 1:
			err = cmdScripts(cfg, true)
		case rest[0] == "run" && len(rest) == 2:
			err = cmdScriptRun(paths, rest[1])
		default:
			return fail(fmt.Errorf("usage: jarvix scripts [--json] | jarvix scripts run \"name\""))
		}
	case "artifacts":
		if len(rest) > 0 && rest[0] != "--json" {
			return fail(fmt.Errorf("usage: jarvix artifacts [--json]"))
		}
		err = cmdArtifacts(cfg, len(rest) > 0)
	case "voices":
		if len(rest) > 0 && rest[0] != "--json" {
			return fail(fmt.Errorf("usage: jarvix voices [--json]"))
		}
		err = cmdVoices(cfg, paths, len(rest) > 0)
	case "doctor":
		switch {
		case len(rest) == 0:
			err = cmdDoctor(cfg, paths, false)
		case len(rest) == 1 && rest[0] == "--gate":
			err = cmdDoctor(cfg, paths, true)
		default:
			return fail(fmt.Errorf("usage: jarvix doctor [--gate]"))
		}
	case "upgrade":
		switch {
		case len(rest) == 0:
			err = cmdUpgrade(paths, false)
		case len(rest) == 1 && rest[0] == "--check":
			err = cmdUpgrade(paths, true)
		default:
			return fail(fmt.Errorf("usage: jarvix upgrade [--check]"))
		}
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
