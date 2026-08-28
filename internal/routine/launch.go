package routine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"github.com/rpickz/jarvix/internal/desktopentry"
	"github.com/rpickz/jarvix/internal/tools"
)

// This file is the launching half of a step (issue #175): what a step opens,
// as against internal/placement, which is where the window then goes.
//
// The half existed before this change as one field — `app`, one bare
// executable name — and on the machine this feature was written for that
// could not express a single thing the user wanted. `ChatGPT` is a desktop
// entry whose Exec is a Chromium `--app=` wrapper; "Chrome under my work
// profile" is the same binary as "under my personal profile" plus
// `--profile-directory`; `signal` has no binary on PATH at all. Every app in
// the intended routine needed either an entry or an argument, so a routine
// could describe that desktop only by not being a routine — the user wrote a
// shell script instead.
//
// The security stance is unchanged where it matters and is worth stating
// precisely, because "routines got arguments" sounds like the opposite.
// ADR 0022 refused to give the MODEL-facing desktop.launch_app tool an
// arguments parameter, and that refusal stands untouched: the model still
// sends one name, matched against what this machine has. What gained argv is
// the user's own configuration file — the same place `[[scripts]]` already
// names executables and `[intents] terminal` already names a program. The
// argv here is authored by the person the daemon belongs to, is passed to
// execve as a literal list, and is never rendered into a command line, so
// there is nothing for a shell to interpret because there is no shell.

// Launching-half field names. These are the [[routines.steps]] TOML keys, the
// form's field keys, and the strings a validation message is keyed on — one
// spelling for all three, exactly as placement.Fields does for the other half.
const (
	FieldApp          = "app"
	FieldDesktopEntry = "desktop_entry"
	FieldArgs         = "args"
	FieldIdentity     = "identity"
	FieldMatch        = "match"
	FieldLaunch       = "launch"
)

// LaunchFields returns every key the launching half owns, in the order a form
// presents them. Together with placement.Fields() this is the whole step
// schema, and a contract test pins that the TOML struct carries exactly these.
func LaunchFields() []string {
	return []string{FieldApp, FieldDesktopEntry, FieldArgs, FieldIdentity, FieldMatch, FieldLaunch}
}

// LaunchPolicy is a step's answer to "a matching window is already open —
// then what?".
//
// It is per-step because the user asked for it per-step, and because the two
// answers are both right for different steps of the same routine. A browser
// they leave open all week should be adopted and placed; a scratch terminal
// the routine wants fresh every morning should be launched again. One global
// setting would make one of those two steps wrong every time it ran.
type LaunchPolicy string

// The policies.
const (
	// LaunchIfMissing adopts a matching window when one is open and launches
	// only when none is. The default, and what every routine written before
	// this change did.
	LaunchIfMissing LaunchPolicy = "if_missing"
	// LaunchAlways starts a new window every run, whatever is already open.
	LaunchAlways LaunchPolicy = "always"
)

// LaunchPolicyNames returns the accepted spellings, in the order a form lists
// them — the default first.
func LaunchPolicyNames() []string {
	return []string{string(LaunchIfMissing), string(LaunchAlways)}
}

// ParseLaunchPolicy reads a configured policy. Empty means LaunchIfMissing:
// the key is absent from every routine written before it existed, and their
// behaviour must not change under them.
func ParseLaunchPolicy(s string) (LaunchPolicy, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return LaunchIfMissing, nil
	case string(LaunchIfMissing):
		return LaunchIfMissing, nil
	case string(LaunchAlways):
		return LaunchAlways, nil
	}
	return "", fmt.Errorf("%q is not a launch policy; use %q (adopt a matching window when one is open) "+
		"or %q (start a new one every run)", strings.TrimSpace(s), LaunchIfMissing, LaunchAlways)
}

// Adopts reports whether a step re-uses a matching window that is already
// open. The empty policy adopts, so a step that never mentions the key
// behaves exactly as it did before the key existed.
func (p LaunchPolicy) Adopts() bool { return p != LaunchAlways }

// ErrNotInstalled marks a launch target this machine does not have. It is a
// sentinel rather than a message because the runner reports it differently
// from every other failure — "not installed" is a fact about the machine that
// no amount of waiting will change, and saying so is the whole of this
// ticket's reporting criterion.
var ErrNotInstalled = errors.New("not installed")

// Target is a resolved launch: the exact argv that will be handed to execve,
// and the words to describe it with when something goes wrong.
type Target struct {
	// Argv is program then arguments. Element zero is a path or a name the
	// launcher resolves; nothing here is ever concatenated into a string.
	Argv []string
	// Label is what the step called this — the app name or the entry id — so
	// a spoken failure names what the user wrote rather than what it resolved
	// to.
	Label string
	// FromEntry is the desktop entry this came from, empty for a plain
	// program. It is what lets a message say "the ChatGPT desktop entry"
	// rather than "omarchy-launch-webapp".
	FromEntry string
}

// String renders the argv for a log line. Never for execution — there is no
// code path that turns a Target back into a command line.
func (t Target) String() string { return strings.Join(t.Argv, " ") }

// Resolver turns a step's launching half into a Target, or says why it
// cannot. It reads the machine (PATH, the desktop entries) and runs nothing.
//
// One type, three callers, and that is the point: the config loader asks it
// whether a routine could run, the window's form asks it the same question
// while the user is typing, and the runner asks it again a moment before
// launching. A step refused in one place is refused identically in the
// others, and a step that passed at save time and fails at run time can only
// mean the machine changed — which is exactly what the run then says.
type Resolver struct {
	// Entries is the desktop-entry index. Nil means "no entries were read",
	// which makes every desktop_entry step report the entry as missing —
	// correct for a machine with no applications directory, and the shape a
	// test uses when it wants only the PATH half.
	Entries *desktopentry.Index
	// LookPath resolves a bare program name; nil uses exec.LookPath. The seam
	// routine capture already keeps, so a test can say what is installed
	// without installing anything.
	LookPath func(name string) (string, error)
}

// DefaultResolver reads this machine: PATH through exec.LookPath, and the
// desktop entries under the XDG search path.
func DefaultResolver() Resolver {
	return Resolver{Entries: desktopentry.Default(), LookPath: exec.LookPath}
}

// Resolve produces the argv a step launches, or the reason it cannot.
//
// Order matters and is the security argument, the same one desktop.launch_app
// makes: the program is decided first and from a bounded source (a validated
// bare token, or a desktop entry's own Exec parsed by the specification's
// grammar), the user's arguments are appended after it as literal elements,
// and the identity flag — the only argument this code contributes — is
// appended last from a curated table. There is no point in that sequence
// where a value becomes syntax.
func (r Resolver) Resolve(s Step) (Target, error) {
	return r.resolve(s, true)
}

// Describe is Resolve WITHOUT asking whether the program is installed: the
// entry must exist and be launchable, the arguments and identity must be
// applicable, but PATH is not consulted.
//
// The split is the difference between two questions that look like one. "Is
// this step well formed?" is a fact about the file and is answered at load,
// for every daemon that reads the document — including the ones reading the
// worked examples in the documentation, which name applications no particular
// machine has. "Can this machine run it?" is a fact about the machine, and
// answering it at load would mean a routine naming an application the user
// uninstalled last week stops the daemon from starting at all: the file did
// not change, the machine did, and the punishment would land on everything
// else the daemon does. So the machine question is asked where it can be
// acted on — when a routine is SAVED, by the form and the assistant's config
// tool alike, and again a moment before the run launches, where it becomes
// "chromium is not installed" instead of an eight-second silence.
//
// A missing DESKTOP ENTRY is not on that line and fails the load: the id is a
// name the routine invented, nothing on the machine can make it right, and
// the acceptance criterion for it is literal.
func (r Resolver) Describe(s Step) (Target, error) {
	return r.resolve(s, false)
}

func (r Resolver) resolve(s Step, checkInstalled bool) (Target, error) {
	lookPath := r.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}

	var target Target
	switch {
	case strings.TrimSpace(s.DesktopEntry) != "":
		entry, err := r.Entries.Lookup(s.DesktopEntry)
		if err != nil {
			return Target{}, err
		}
		argv, err := entry.Command()
		if err != nil {
			return Target{}, err
		}
		if entry.Terminal {
			// The entry says it needs a terminal window wrapped round it.
			// Refusing is the honest answer rather than a limitation: a
			// routine places graphical windows, and launching a terminal
			// application with no terminal produces precisely the failure
			// this ticket exists to end — a process that starts, maps
			// nothing, and is waited on for eight seconds.
			return Target{}, fmt.Errorf("the %s desktop entry runs in a terminal; name your terminal "+
				"in app and the command in args instead", entry.ID)
		}
		if checkInstalled {
			if try := strings.TrimSpace(entry.TryExec); try != "" {
				if _, err := lookPath(try); err != nil {
					return Target{}, fmt.Errorf("the %s desktop entry is here but %s is %w",
						entry.ID, try, ErrNotInstalled)
				}
			}
			path, err := lookPath(argv[0])
			if err != nil {
				return Target{}, fmt.Errorf("the %s desktop entry launches %s, which is %w",
					entry.ID, argv[0], ErrNotInstalled)
			}
			// The resolved path, not the name: what runs is decided here and
			// not re-searched on PATH by the exec that follows.
			argv[0] = path
		}
		target = Target{Argv: argv, Label: entry.ID, FromEntry: entry.ID}
	case strings.TrimSpace(s.App) != "":
		app := strings.TrimSpace(s.App)
		program := app
		if checkInstalled {
			path, err := lookPath(app)
			if err != nil {
				return Target{}, fmt.Errorf("%s is %w", app, ErrNotInstalled)
			}
			program = path
		}
		target = Target{Argv: []string{program}, Label: app}
	default:
		return Target{}, errors.New("the step names nothing to launch")
	}

	target.Argv = append(target.Argv, s.Args...)
	if identity := strings.TrimSpace(s.Identity); identity != "" {
		flag, ok := IdentityFlag(target.Argv[0])
		if !ok {
			return Target{}, fmt.Errorf("%s does not take an identity flag, so identity cannot be set for it",
				filepath.Base(target.Argv[0]))
		}
		target.Argv = append(target.Argv, flag+identity)
	}
	return target, nil
}

// identityFlags maps a program to the flag that makes its window carry a
// class of the caller's choosing.
//
// This table is the answer to the hardest problem in the ticket, and the
// reason it is a table and not a rule. Chromium runs every profile in ONE
// process: a window on the personal profile and a window on the work profile
// have the same window class, the same PID, and byte-identical
// /proc/<pid>/cmdline — verified on the machine this was written for. No
// amount of looking at a running desktop can tell them apart, so a routine
// that adopts "a chromium window" cannot know which profile it just adopted,
// and two steps for two profiles fight over the first window they see.
//
// The only thing that CAN distinguish them is a decision made before the
// window exists: launch it with a class nobody else uses. Chromium accepts
// `--class=`, so a step declaring `identity = "work"` opens a window classed
// "work" and matches on it. For a program that offers no such flag there is
// no mechanism and this table honestly has no entry — the step is told so at
// load time and the user distinguishes the two windows some other way (a
// distinct `match`, or two different programs). The ADR records that.
//
// Curated rather than guessed: each entry is a flag this author has confirmed
// the program accepts, and a wrong spelling here would be an argument the
// program rejects at start-up — a launch that fails for a reason the user
// never wrote.
var identityFlags = map[string]string{
	"chromium":                  "--class=",
	"chromium-browser":          "--class=",
	"google-chrome":             "--class=",
	"google-chrome-beta":        "--class=",
	"google-chrome-stable":      "--class=",
	"google-chrome-unstable":    "--class=",
	"brave":                     "--class=",
	"brave-browser":             "--class=",
	"microsoft-edge":            "--class=",
	"microsoft-edge-stable":     "--class=",
	"vivaldi":                   "--class=",
	"vivaldi-stable":            "--class=",
	"thorium-browser":           "--class=",
	"firefox":                   "--class=",
	"firefox-developer-edition": "--class=",
	"librewolf":                 "--class=",
	"alacritty":                 "--class=",
	"kitty":                     "--class=",
	"foot":                      "--app-id=",
}

// IdentityFlag reports the flag a program accepts for setting its window's
// class, and whether it accepts one at all. The argument may be a path; only
// the base name decides.
//
// Exported because the loader, the form's validation and the ADR's claim are
// the same fact, and a second copy of this table is how they would drift.
func IdentityFlag(program string) (string, bool) {
	flag, ok := identityFlags[strings.ToLower(filepath.Base(strings.TrimSpace(program)))]
	return flag, ok
}

// IdentityCapablePrograms lists the programs the identity mechanism works
// for, sorted, for a message that has to name them.
func IdentityCapablePrograms() []string {
	out := make([]string, 0, len(identityFlags))
	for name := range identityFlags {
		out = append(out, name)
	}
	// A stable order so the same machine always words the refusal the same.
	slices.Sort(out)
	return out
}

// Launcher starts a resolved target. An interface so every test in this
// package proves what WOULD have run without anything running: the argv
// guarantees are assertions about a recorded slice, never about a process.
type Launcher interface {
	Launch(ctx context.Context, target Target) error
}

// ExecLauncher starts a target as a detached child of the daemon.
//
// Why not the compositor's spawn dispatcher, which is what a bare `app` step
// has always used: that dispatcher takes a COMMAND LINE. Hyprland's Lua
// dialect spells it `hl.dsp.exec_cmd("…")` and the compositor hands the
// string to a shell, so passing `--profile-directory=Profile 3` through it
// would mean quoting a value for a shell — and "we quote correctly" is a much
// weaker promise than "there is no shell". An argv cannot be expressed
// through that seam at all, so a step carrying arguments is started here
// instead, with the detached-child discipline desktop.launch_app already uses
// (ADR 0022): the context does not kill it, because an application the user
// asked for must outlive the sentence that asked for it, and it gets its own
// process group so a daemon restart does not take the user's browser with it.
type ExecLauncher struct {
	// ScrubEnv names extra environment variables to withhold, on top of the
	// built-in secret-name patterns. A launched application is a third party
	// and has no business seeing Jarvix's keys.
	ScrubEnv []string
}

// Launch implements Launcher.
func (e ExecLauncher) Launch(ctx context.Context, target Target) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(target.Argv) == 0 {
		return errors.New("nothing to launch")
	}
	//nolint:gosec // argv[0] came from exec.LookPath or a desktop entry's own Exec; the arguments are the user's configuration, never the model's
	cmd := exec.Command(target.Argv[0], target.Argv[1:]...)
	cmd.Env = tools.ScrubbedEnv(os.Environ(), e.ScrubEnv)
	cmd.Dir, _ = os.UserHomeDir()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Nobody waits for a GUI application, but something has to reap it or the
	// daemon accumulates zombies for the length of its life.
	go func() { _ = cmd.Wait() }()
	return nil
}

// entryID bounds what a step may name as a desktop entry: an identifier, not
// a path. Load-bearing in the same modest way spawnPattern is — the value
// becomes a file name under an applications directory — and a bound that
// excludes a separator is what keeps `desktop_entry` from reaching outside
// the directories the specification says entries live in.
var entryID = regexp.MustCompile(`^[A-Za-z0-9._+-]+$`)

// identityToken bounds an identity. It becomes a window class and the query
// that matches it, so it is the character set a class can carry and nothing
// wider.
var identityToken = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// maxStepArgs and maxStepArgLen bound a step's argument list. Defensive
// rather than load-bearing — these values are hand-written configuration —
// but a bound means a corrupted file fails as a sentence rather than as a
// process spawned with a megabyte of argv.
const (
	maxStepArgs   = 32
	maxStepArgLen = 4096
)
