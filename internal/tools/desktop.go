package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rpickz/jarvix/internal/desktop"
)

// This file implements the desktop window tools (ADR 0022): the assistant can
// say what is open, focus a window, send one to another workspace, close one,
// and start an application. It is the safe half of "take control" — every
// action here is visible on screen and undoable by hand, and none of it can
// enter data anywhere.
//
// The security shape is the same one advisor.ask has, and for the same reason:
// the model chooses *which entry of a list Jarvix produced*, never what runs.
// Concretely —
//
//   - Window addresses come from the compositor's own inventory. The model's
//     words are matched against that inventory here, in Go; nothing it says
//     reaches argv.
//   - Workspace numbers are range-checked integers.
//   - An application launch resolves to a binary through the configured
//     allow list or exec.LookPath, and is executed directly. No shell is
//     involved at any point, so a name containing `;` is a name that does not
//     resolve, never a command.
//
// The other property worth stating is that a resolved window stays resolved.
// Everything after resolution — the spoken confirmation, the wait for the
// user's answer, the dispatch — carries the address captured at that moment.
// A window opening, closing, or being renamed in between can therefore make
// the action fail, which is fine and speakable, but it can never redirect it
// onto a different window.

// Tool names. One tool per verb rather than one tool with a verb argument,
// because the permission gate keys on the tool name: "reads run silently,
// closing asks" is then a fact about the registry, not a special case inside
// a tool (ADR 0014).
const (
	ListWindowsToolName = "desktop.list_windows"
	FocusWindowToolName = "desktop.focus_window"
	MoveWindowToolName  = "desktop.move_window"
	CloseWindowToolName = "desktop.close_window"
	LaunchAppToolName   = "desktop.launch_app"
	NameWindowToolName  = "desktop.name_window"
)

// Window tool bounds.
const (
	// DefaultInventoryTTL is how long one `hyprctl clients` result is reused.
	// Long enough that listing and then acting in the same turn costs one
	// call; short enough that the inventory a match is made against is what
	// is on screen now.
	DefaultInventoryTTL = 2 * time.Second
	// DefaultDesktopTimeout bounds one compositor round trip. A focus must
	// feel immediate; if the compositor has not answered in this long, the
	// user is owed a sentence instead of a longer silence.
	DefaultDesktopTimeout = 300 * time.Millisecond
	// resolutionTTL is how long a resolution made for a confirmation stays
	// usable. Comfortably longer than the confirmation timeout, so a user who
	// takes their time still closes the window they were asked about.
	resolutionTTL = 2 * time.Minute
	// maxWorkspace bounds the workspace numbers this tool will dispatch.
	// Hyprland numbers workspaces from 1; named and special workspaces are
	// out of scope, so anything outside the range is a mistake rather than a
	// destination.
	maxWorkspace = 99
	// maxSpokenTitle bounds a window title inside a confirmation question.
	maxSpokenTitle = 60
	// maxListedWindows bounds how much of a busy desktop reaches the model.
	maxListedWindows = 20
)

// Desktop is the shared state behind the five window tools: the compositor
// seam, the short-lived inventory cache, and the resolutions made for pending
// confirmations. The tools themselves are thin — Tools() hands out one per
// verb, all pointing here.
type Desktop struct {
	comp     desktop.Compositor
	launcher appLauncher
	// lookPath resolves a bare name on PATH — exec.LookPath in production, a
	// canned map in tests (the same seam routine capture keeps).
	lookPath func(name string) (string, error)
	// apps is the configured launch allow list. Empty means "anything on
	// PATH", which is the sensible default for a machine whose owner is
	// talking to it: launching an installed application is not an escalation,
	// and the launch verb asks first anyway.
	apps    []string
	ttl     time.Duration
	timeout time.Duration
	log     *slog.Logger
	// onAction is told about each completed action so the overlay can show
	// what Jarvix did. Nil in tests.
	onAction func(verb, target string)
	// onRefusal is told about each action that did NOT happen and why — a
	// launch the resolver refused, a dispatch the compositor rejected — so
	// the activity feed can show the reason (issue #70). Until it existed,
	// "launch refused: firefox is not installed" lived only in the journal
	// and the model's tool result; the user's surfaces heard nothing. Nil in
	// tests.
	onRefusal func(verb, target, reason string)
	// names is the window-nickname registry (#126): one instance behind
	// every consumer of a window reference, so a nickname means the same
	// window in a tool call, a typed target, and a deterministic intent.
	names *desktop.Nicknames

	mu sync.Mutex
	// inventory is the last capture and when it was taken.
	inventory []desktop.Window
	fetched   time.Time
	// pending holds resolutions made while answering the permission gate, so
	// the window the user was asked about is the window that gets acted on.
	pending map[string]pendingTarget
}

// pendingTarget is one resolution held between a confirmation question and
// the dispatch it authorised.
type pendingTarget struct {
	window desktop.Window
	at     time.Time
}

// DesktopOptions configure the window tools.
type DesktopOptions struct {
	// Compositor is the window-manager seam. Required.
	Compositor desktop.Compositor
	// Apps is the launch allow list: bare binary names or absolute paths.
	// Empty allows anything resolvable on PATH.
	Apps []string
	// ScrubEnv names extra environment variables to withhold from launched
	// applications, on top of the built-in secret-name patterns.
	ScrubEnv []string
	// InventoryTTL overrides DefaultInventoryTTL.
	InventoryTTL time.Duration
	// Timeout overrides DefaultDesktopTimeout for one compositor call.
	Timeout time.Duration
	// OnAction is called after each completed action with the verb and the
	// window (or application) it acted on, for the overlay and the event bus.
	OnAction func(verb, target string)
	// OnRefusal is called when an action was refused or failed, with the verb,
	// the target as a person would name it, and the reason — never a window
	// address, never compositor diagnostics.
	OnRefusal func(verb, target, reason string)
	// PhraseOwner reports whether a whole utterance already belongs to the
	// intent grammar, naming the owner (#126) — intent.Router.Owner behind a
	// func so this package never imports the router. Nil skips the check.
	PhraseOwner func(phrase string) (owner string, taken bool)
	// Log records each action. Nil uses slog.Default().
	Log *slog.Logger

	// launcher overrides process execution in tests; the real path is
	// execLauncher.
	launcher appLauncher
	// lookPath overrides PATH resolution in tests; the real path is
	// exec.LookPath.
	lookPath func(name string) (string, error)
}

// NewDesktop builds the window tools' shared state.
func NewDesktop(opts DesktopOptions) *Desktop {
	d := &Desktop{
		comp:      opts.Compositor,
		launcher:  opts.launcher,
		lookPath:  opts.lookPath,
		apps:      append([]string(nil), opts.Apps...),
		ttl:       opts.InventoryTTL,
		timeout:   opts.Timeout,
		log:       opts.Log,
		onAction:  opts.OnAction,
		onRefusal: opts.OnRefusal,
		names: desktop.NewNicknames(desktop.NicknameOptions{
			Reserved:    ReservedWindowWords(),
			PhraseOwner: opts.PhraseOwner,
		}),
		pending: make(map[string]pendingTarget),
	}
	if d.launcher == nil {
		d.launcher = &execLauncher{scrubEnv: opts.ScrubEnv}
	}
	if d.lookPath == nil {
		d.lookPath = exec.LookPath
	}
	if d.ttl <= 0 {
		d.ttl = DefaultInventoryTTL
	}
	if d.timeout <= 0 {
		d.timeout = DefaultDesktopTimeout
	}
	if d.log == nil {
		d.log = slog.Default()
	}
	return d
}

// desktopVerb is which of the five things a window tool does.
type desktopVerb int

const (
	verbList desktopVerb = iota
	verbFocus
	verbMove
	verbClose
	verbLaunch
	verbName
)

// Tools returns the six window tools, in the order they are registered and
// therefore offered to the model: read first, then act.
func (d *Desktop) Tools() []Tool {
	return []Tool{
		&windowTool{d: d, verb: verbList},
		&windowTool{d: d, verb: verbFocus},
		&windowTool{d: d, verb: verbMove},
		&windowTool{d: d, verb: verbClose},
		&windowTool{d: d, verb: verbLaunch},
		&windowTool{d: d, verb: verbName},
	}
}

// Names returns the tool names, for the daemon's startup log.
func (d *Desktop) Names() []string {
	tools := d.Tools()
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name())
	}
	return names
}

// windowTool is one verb. It holds no state of its own: everything lives on
// the shared Desktop, so all five verbs see one inventory and one cache.
type windowTool struct {
	d    *Desktop
	verb desktopVerb
}

// Name implements Tool.
func (t *windowTool) Name() string {
	switch t.verb {
	case verbList:
		return ListWindowsToolName
	case verbFocus:
		return FocusWindowToolName
	case verbMove:
		return MoveWindowToolName
	case verbClose:
		return CloseWindowToolName
	case verbName:
		return NameWindowToolName
	default:
		return LaunchAppToolName
	}
}

// Description implements Tool. Written for a small local model deciding
// between five similar tools, so each says what it is for in the user's words
// and states the one thing that would otherwise go wrong.
func (t *windowTool) Description() string {
	switch t.verb {
	case verbList:
		return "List the windows open on the user's desktop right now, with their application, " +
			"title and workspace. Use it for \"what have I got open?\" and whenever you need to know " +
			"what is running. Summarise the result in one or two spoken sentences — never read " +
			"window identifiers or raw data aloud."
	case verbFocus:
		return "Switch the user to one of their open windows. Describe the window the way they did " +
			"— an application name, a category like \"browser\" or \"terminal\", or part of the " +
			"title — and this tool finds it in the live window list. You do not need to list windows " +
			"first. If several windows match, it says so and names them: ask the user which one they " +
			"meant and call again, rather than picking one."
	case verbMove:
		return "Send one of the user's open windows to another workspace, by number. The user stays " +
			"where they are — the window moves away. Describe the window as the user did, or leave " +
			"it empty for the window they are currently in."
	case verbClose:
		return "Close one of the user's open windows, exactly as clicking its close button would: " +
			"the application may still ask them to save. Describe the window as the user did. Use " +
			"this only when they asked to close something, and never to tidy up on your own."
	case verbName:
		return "Give one of the user's open windows a short spoken nickname, when they ask to call " +
			"or name a window something (\"call this window builds\"). The name must be a single " +
			"word; afterwards the user can say it anywhere they would describe a window. Use it " +
			"only when the user chose the name — never invent nicknames on your own."
	default:
		return "Start an application on the user's computer by name (\"firefox\", \"spotify\"). Use " +
			"it when they ask you to open or launch something. It starts the program and returns " +
			"immediately; the window appears on its own."
	}
}

// windowArgSchema is the shared description of "which window". It is the one
// piece of model-facing text that decides whether loose reference works, so it
// names the three shapes that resolve and the shortcut for "this one".
const windowArgSchema = `{
			"type": "string",
			"description": "Which window, described as the user described it: an application name (\"firefox\", \"obsidian\"), a category (\"browser\", \"terminal\", \"editor\"), or part of the window title. Leave it out, or say \"this\", for the window the user is currently in."
		}`

// Schema implements Tool.
func (t *windowTool) Schema() json.RawMessage {
	switch t.verb {
	case verbList:
		return json.RawMessage(`{"type": "object", "properties": {}}`)
	case verbFocus, verbClose:
		return json.RawMessage(`{
		"type": "object",
		"properties": {"window": ` + windowArgSchema + `}
	}`)
	case verbMove:
		return json.RawMessage(`{
		"type": "object",
		"properties": {
			"window": ` + windowArgSchema + `,
			"workspace": {
				"type": "integer",
				"minimum": 1,
				"maximum": ` + strconv.Itoa(maxWorkspace) + `,
				"description": "The workspace number to send the window to."
			}
		},
		"required": ["workspace"]
	}`)
	case verbName:
		return json.RawMessage(`{
		"type": "object",
		"properties": {
			"window": ` + windowArgSchema + `,
			"name": {
				"type": "string",
				"description": "The nickname the user chose: one short word, exactly as they said it."
			}
		},
		"required": ["name"]
	}`)
	default:
		desc, _ := json.Marshal("Which application to start. " + t.d.launchHint())
		return json.RawMessage(`{
		"type": "object",
		"properties": {"app": {"type": "string", "description": ` + string(desc) + `}},
		"required": ["app"]
	}`)
	}
}

// launchHint tells the model what it may name. With an allow list configured
// it is the list, because anything else is a refusal the model should not have
// to discover by trying.
func (d *Desktop) launchHint() string {
	if len(d.apps) == 0 {
		return "Give the program's name as it is installed, e.g. \"firefox\", not a command line."
	}
	return "Only these are allowed: " + strings.Join(d.apps, ", ") + "."
}

// windowArgs is everything the model is allowed to say. Nothing here reaches
// argv: window is matched against the inventory, workspace is range-checked,
// and app is resolved against the allow list or PATH.
type windowArgs struct {
	Window    string `json:"window"`
	Workspace int    `json:"workspace"`
	App       string `json:"app"`
	Name      string `json:"name"`
}

// Execute implements Tool. Every way the desktop can disappoint — no
// compositor, no match, several matches, a refused dispatch — comes back as
// text the assistant can say in one sentence. Only malformed tool arguments
// are an err, because those are a programming failure, not a desktop state.
func (t *windowTool) Execute(ctx context.Context, input json.RawMessage) (string, error) {
	var args windowArgs
	if err := unmarshalWindowArgs(input, &args); err != nil {
		return "", fmt.Errorf("invalid %s arguments: %w", t.Name(), err)
	}
	if t.verb == verbLaunch {
		return t.d.launch(ctx, args.App)
	}
	if t.verb == verbList {
		return t.d.list(ctx)
	}
	if t.verb == verbName {
		return t.d.nameWindow(ctx, args.Window, args.Name)
	}
	if t.verb == verbMove && (args.Workspace < 1 || args.Workspace > maxWorkspace) {
		return fmt.Sprintf("Workspace %d does not exist; workspaces are numbered 1 to %d. Tell the "+
			"user which workspaces they can use, in one short sentence, and do not retry.",
			args.Workspace, maxWorkspace), nil
	}

	// The resolution made when the user was asked to confirm wins, so the
	// window they approved is the window acted on. Without a confirmation
	// (the allow tier), resolve now against a fresh inventory.
	res, found := t.d.takePending(t.Name(), args)
	if found {
		// The user answered a question about a window seen before they
		// answered it. However short the wait was, look again rather than
		// trusting a capture from before the decision.
		t.d.invalidate()
	} else {
		resolved, err := t.d.resolve(ctx, args.Window)
		if err != nil {
			return t.d.unavailable(err), nil
		}
		if msg, done := t.d.explainResolution(resolved); done {
			return msg, nil
		}
		res = resolved.Window
	}

	// One last look before acting: the address is dispatched exactly as
	// captured, but only if the compositor still reports *that* window under
	// it. Addresses are reusable handles, so this is what stops a window
	// created since the resolution from inheriting the user's "yes".
	current, ok, err := t.d.verify(ctx, res)
	if err != nil {
		return t.d.unavailable(err), nil
	}
	if !ok {
		return fmt.Sprintf("The %s window is no longer there, so nothing was done. Tell the user "+
			"in one short sentence that it has already gone, and do not retry.",
			desktop.AppName(res.Class)), nil
	}
	return t.d.act(ctx, t.verb, current, args.Workspace), nil
}

// unmarshalWindowArgs decodes the tool arguments. An absent object is the same
// as an empty one: a model calling desktop.focus_window with no arguments at
// all means "the window I'm in", which is a legitimate request rather than a
// malformed call.
func unmarshalWindowArgs(input json.RawMessage, args *windowArgs) error {
	trimmed := strings.TrimSpace(string(input))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	return json.Unmarshal([]byte(trimmed), args)
}

// act performs one resolved action and returns what the assistant should say.
func (d *Desktop) act(ctx context.Context, verb desktopVerb, w desktop.Window, workspace int) string {
	callCtx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	var err error
	var verbName, done string
	switch verb {
	case verbFocus:
		verbName, err = "focus", d.comp.Focus(callCtx, w.Address)
		done = fmt.Sprintf("Switched to %s.", w.Describe())
	case verbMove:
		verbName, err = "move", d.comp.MoveToWorkspace(callCtx, w.Address, workspace)
		done = fmt.Sprintf("Moved %s to workspace %d. The user stayed where they were.", w.Describe(), workspace)
	case verbClose:
		verbName, err = "close", d.comp.Close(callCtx, w.Address)
		done = fmt.Sprintf("Closed %s.", w.Describe())
	}

	if err != nil {
		// The compositor's own diagnostics stay daemon-side: they are the
		// operator's material, and anything returned here may be read aloud.
		d.log.Warn("window action failed", "component", "tools", "verb", verbName,
			"class", w.Class, "address", w.Address, "error", err.Error())
		// Same rule on the bus: the window as a person would name it and the
		// fact of the refusal — never the address, never the diagnostics.
		d.publishRefusal(verbName, w.Describe(), "the window manager refused")
		return fmt.Sprintf("The window manager would not %s that window. Tell the user in one "+
			"short sentence that it did not work, and do not retry.", verbName)
	}
	// Class and address, never the title: a window title is content, and the
	// journal outlives the conversation (the same rule desktop context keeps).
	d.log.Info("window action", "component", "tools", "verb", verbName,
		"class", w.Class, "address", w.Address, "workspace", workspace)
	d.publish(verbName, w.Describe())
	return done + " Confirm it to the user in one short sentence. Do not describe the window in detail."
}

// list renders the inventory for the model.
func (d *Desktop) list(ctx context.Context) (string, error) {
	windows, err := d.windows(ctx)
	if err != nil {
		return d.unavailable(err), nil
	}
	if len(windows) == 0 {
		return "Nothing is open on the user's desktop. Tell them so in one short sentence.", nil
	}
	shown, extra := windows, 0
	if len(shown) > maxListedWindows {
		extra = len(shown) - maxListedWindows
		shown = shown[:maxListedWindows]
	}
	summary := fmt.Sprintf("%s open: %s.", plural(len(windows), "window is", "windows are"),
		summariseWindows(shown, d.nicknamesByAddress(windows)))
	if extra > 0 {
		summary += fmt.Sprintf(" (%d more not listed.)", extra)
	}
	d.publish("list", plural(len(windows), "window", "windows"))
	return summary + " Summarise this for the user in one or two spoken sentences, grouped by " +
		"application — say what is open, not every title, and never read out identifiers or raw data.", nil
}

// launch starts an application.
func (d *Desktop) launch(ctx context.Context, app string) (string, error) {
	name := strings.TrimSpace(app)
	if name == "" {
		return "", fmt.Errorf("%s: empty application name", LaunchAppToolName)
	}
	binary, candidates, err := d.resolveApp(name)
	switch {
	case len(candidates) > 1:
		return fmt.Sprintf("Several applications match %q: %s. Ask the user which one they meant "+
			"and call again with that name. Do not guess.", name, strings.Join(candidates, ", ")), nil
	case err != nil:
		// A dead-end refusal when a near-match is installed costs the user
		// the feature: told "use chrome" on a chromium machine, the model had
		// no way to discover the right name (issue #71). Name it, so the
		// recovery is one question and one retry rather than a shrug.
		if alternatives := d.installedNearMatches(name); errors.Is(err, errNotInstalled) &&
			len(alternatives) > 0 {
			d.log.Info("launch refused", "component", "tools", "tool", LaunchAppToolName,
				"app", name, "reason", err.Error(),
				"near_matches", strings.Join(alternatives, ","))
			// Still a refusal — nothing launched — so the activity feed gets
			// its row (issue #70), reason and suggestion together: the same
			// facts the model is being told, in one clause.
			d.publishRefusal("launch", name,
				fmt.Sprintf("it is not installed, but %s", spokenInstalled(alternatives)))
			return fmt.Sprintf("%q is not installed, but %s. Ask the user in one short sentence "+
				"whether to open %s instead; only if they say yes, call this tool again with that "+
				"name. Do not describe anything as opened.", name, spokenInstalled(alternatives),
				spokenChoice(alternatives)), nil
		}
		d.log.Info("launch refused", "component", "tools", "tool", LaunchAppToolName,
			"app", name, "reason", err.Error())
		// The resolver's sentence is the reason the user needs ("it is not
		// installed", "it is not on the allowed list"), and it never contains
		// paths or diagnostics — resolveApp writes it to be spoken.
		d.publishRefusal("launch", name, err.Error())
		return fmt.Sprintf("%q cannot be started on this computer: %v. Tell the user in one short "+
			"sentence, and do not retry.", name, err), nil
	}

	if err := d.launcher.Launch(ctx, binary); err != nil {
		d.log.Warn("launch failed", "component", "tools", "tool", LaunchAppToolName,
			"app", name, "binary", binary, "error", err.Error())
		// The launcher's error can carry paths and exec detail — operator
		// material, already in the journal above. The bus gets the fact.
		d.publishRefusal("launch", name, "it would not start")
		return fmt.Sprintf("%s would not start. Tell the user in one short sentence that it failed, "+
			"and do not retry.", filepath.Base(binary)), nil
	}
	started := filepath.Base(binary)
	d.log.Info("application launched", "component", "tools", "tool", LaunchAppToolName,
		"app", name, "binary", binary)
	d.publish("launch", started)
	return fmt.Sprintf("Started %s. Tell the user in one short sentence that it is opening; its "+
		"window will appear on its own.", started), nil
}

// resolveApp turns a spoken application name into a binary to execute.
//
// Three gates, in order, and the order is the security argument: the name must
// look like a program name (never a path, an argument, or shell syntax), it
// must be on the allow list when one is configured, and it must resolve
// through exec.LookPath. Only then does anything run — and it runs directly,
// with that one argument and no shell.
func (d *Desktop) resolveApp(name string) (binary string, candidates []string, err error) {
	if !validAppName(name) {
		return "", nil, fmt.Errorf("%q is not a program name", name)
	}
	entry, permitted := d.allowedApp(name)
	if permitted {
		if path, lookErr := d.lookupBinary(entry); lookErr == nil {
			return path, nil, nil
		}
	}
	// Not something installed under that name: it may be a category ("open a
	// browser"). Expanding it can only ever offer applications that are
	// installed *and* allowed, and several of them is a question, not a coin
	// toss.
	switch installed := d.installedCategoryApps(name); len(installed) {
	case 0:
	case 1:
		allowed, _ := d.allowedApp(installed[0])
		if path, lookErr := d.lookupBinary(allowed); lookErr == nil {
			return path, nil, nil
		}
	default:
		return "", installed, nil
	}
	if !permitted {
		return "", nil, fmt.Errorf("it is not on the allowed list (%s)", strings.Join(d.apps, ", "))
	}
	return "", nil, errNotInstalled
}

// errNotInstalled is the refusal that may carry a near-match suggestion: only
// "not installed" invites one, because an allow-list refusal already names
// everything the user permitted.
var errNotInstalled = errors.New("it is not installed")

// launchAliases maps a name a user is likely to say — or a model is likely to
// be told ("use chrome") — to the binaries that commonly provide it on Linux.
// Deliberately small and curated, like appCategories: each entry is a spelling
// mismatch seen in the wild, not a guess at intent. Every suggestion is still
// gated on being installed and permitted before it is spoken.
var launchAliases = map[string][]string{
	"chrome":           {"chromium", "google-chrome-stable", "google-chrome"},
	"google-chrome":    {"google-chrome-stable", "chromium"},
	"chromium-browser": {"chromium"},
	"code":             {"codium", "code-oss", "vscodium"},
	"vscode":           {"code", "codium", "code-oss", "vscodium"},
	"vs-code":          {"code", "codium", "code-oss", "vscodium"},
	"telegram":         {"telegram-desktop"},
	"signal":           {"signal-desktop"},
	"1password":        {"1password-gui"},
	"gedit":            {"gnome-text-editor"},
}

// maxLaunchSuggestions bounds a refusal to what fits in one spoken sentence.
const maxLaunchSuggestions = 3

// LaunchCandidates expands one program name into the spellings a launch
// command may actually be installed under: the name itself, then the
// "-desktop" packaging convention ("signal" → "signal-desktop"). It is the
// one copy of that convention — routine capture derives launch commands from
// window classes through it, and the launcher's near-match suggestions reuse
// it, so the two features can never disagree about what a class launches.
func LaunchCandidates(name string) []string {
	return []string{name, name + "-desktop"}
}

// installedNearMatches names what IS installed (and permitted) when name
// itself is not. Three sources, strongest reading first: the curated alias
// table, the "-desktop" packaging convention shared with routine capture
// (LaunchCandidates), and the other members of any category the name belongs
// to — the same table "focus my browser" resolves through, so "firefox is not
// installed" on a chromium machine can end with the name that works.
func (d *Desktop) installedNearMatches(name string) []string {
	lower := strings.ToLower(strings.TrimSpace(name))
	seen := map[string]bool{lower: true}
	var out []string
	consider := func(candidate string) {
		c := strings.ToLower(candidate)
		if len(out) >= maxLaunchSuggestions || seen[c] || !validAppName(c) {
			return
		}
		seen[c] = true
		entry, permitted := d.allowedApp(c)
		if !permitted {
			return
		}
		if _, err := d.lookupBinary(entry); err != nil {
			return
		}
		out = append(out, c)
	}
	for _, alias := range launchAliases[lower] {
		consider(alias)
	}
	for _, candidate := range LaunchCandidates(lower)[1:] {
		consider(candidate)
	}
	// Category siblings, in a stable order so the same machine always makes
	// the same suggestion.
	categories := make([]string, 0, len(appCategories))
	for category := range appCategories {
		categories = append(categories, category)
	}
	sort.Strings(categories)
	for _, category := range categories {
		members := appCategories[category]
		member := false
		for _, m := range members {
			if strings.EqualFold(m, lower) {
				member = true
				break
			}
		}
		if !member {
			continue
		}
		for _, m := range members {
			consider(m)
		}
	}
	return out
}

// spokenInstalled renders the suggestions as the clause after "but":
// "chromium is", "chromium and codium are".
func spokenInstalled(names []string) string {
	if len(names) == 1 {
		return names[0] + " is"
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1] + " are"
}

// spokenChoice is how the refusal refers to the suggestions when asking.
func spokenChoice(names []string) string {
	if len(names) == 1 {
		return names[0]
	}
	return "one of them"
}

// allowedApp matches a spoken name against the allow list (or, with no list,
// accepts it as given), returning the configured entry to resolve.
func (d *Desktop) allowedApp(name string) (string, bool) {
	if len(d.apps) == 0 {
		return name, true
	}
	for _, entry := range d.apps {
		if strings.EqualFold(filepath.Base(entry), name) || strings.EqualFold(entry, name) {
			return entry, true
		}
	}
	return "", false
}

// installedCategoryApps expands a category word into the applications that are
// both installed and permitted.
func (d *Desktop) installedCategoryApps(name string) []string {
	var found []string
	seen := map[string]bool{}
	for _, term := range categoryTerms(normaliseTokens(name)) {
		if seen[term] || !validAppName(term) {
			continue
		}
		seen[term] = true
		entry, ok := d.allowedApp(term)
		if !ok {
			continue
		}
		if _, err := d.lookupBinary(entry); err == nil {
			found = append(found, term)
		}
	}
	return found
}

// validAppName is the shape a program name may have. Deliberately strict: no
// separators, no whitespace, no shell metacharacters, so the name can only
// ever be a lookup key. A path is refused as well — the allow list is where
// an absolute path belongs, because the user wrote that.
func validAppName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case (r == '.' || r == '_' || r == '-' || r == '+') && i > 0:
		default:
			return false
		}
	}
	return true
}

// lookupBinary resolves an allow-list entry: an absolute path must exist and
// be executable, a bare name is looked up on PATH. The same rule advisor.ask
// follows — a lookup only, no invocation.
func (d *Desktop) lookupBinary(entry string) (string, error) {
	if filepath.IsAbs(entry) {
		info, err := os.Stat(entry)
		if err != nil {
			return "", err
		}
		if info.IsDir() || info.Mode()&0o111 == 0 {
			return "", fmt.Errorf("%s is not executable", entry)
		}
		return entry, nil
	}
	return d.lookPath(entry)
}

// windows returns the inventory, reusing a recent capture. The cache exists so
// that listing and then acting inside one turn costs one compositor call, and
// it is short because matching against a stale desktop is worse than a few
// milliseconds.
func (d *Desktop) windows(ctx context.Context) ([]desktop.Window, error) {
	d.mu.Lock()
	if time.Since(d.fetched) < d.ttl && d.inventory != nil {
		cached := append([]desktop.Window(nil), d.inventory...)
		d.mu.Unlock()
		return cached, nil
	}
	d.mu.Unlock()

	callCtx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	windows, err := d.comp.Windows(callCtx)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.inventory, d.fetched = windows, time.Now()
	d.mu.Unlock()
	return append([]desktop.Window(nil), windows...), nil
}

// invalidate drops the cached inventory, so the next look is a real one.
func (d *Desktop) invalidate() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.inventory, d.fetched = nil, time.Time{}
}

// resolve captures an inventory and matches the reference against it —
// nicknames first (#126), then the matcher's tiers.
func (d *Desktop) resolve(ctx context.Context, query string) (resolution, error) {
	windows, err := d.windows(ctx)
	if err != nil {
		return resolution{}, err
	}
	return resolveWindow(query, windows, d.names), nil
}

// verify reports whether the compositor still has *that* window at the
// captured address, and returns it as it is now (its title may have changed
// while the user was answering). Identity is the address plus the compositor's
// own window id and the application class: an address is a reusable handle, so
// matching on it alone would let a window created since the resolution inherit
// an answer the user gave about a different one.
func (d *Desktop) verify(ctx context.Context, w desktop.Window) (desktop.Window, bool, error) {
	windows, err := d.windows(ctx)
	if err != nil {
		return desktop.Window{}, false, err
	}
	for _, current := range windows {
		if current.Address != w.Address {
			continue
		}
		if current.StableID != w.StableID || !strings.EqualFold(current.Class, w.Class) {
			return desktop.Window{}, false, nil
		}
		return current, true, nil
	}
	return desktop.Window{}, false, nil
}

// explainResolution turns a non-answer into text for the model. done is true
// when there is nothing to act on — the caller must stop.
func (d *Desktop) explainResolution(res resolution) (string, bool) {
	switch res.Kind {
	case resolveMany:
		return fmt.Sprintf("Several windows match %q: %s. Ask the user which one they mean, naming "+
			"them, then call the tool again with a description that picks just one. Do not guess.",
			res.Query, describeCandidates(res.Candidates)), true
	case resolveReleased:
		// The nickname's window has closed (#126): honesty is the whole
		// answer, and it is a different answer from "never heard of it".
		return fmt.Sprintf("Nothing is called %q right now — the window that was has closed, and "+
			"names do not outlive their window. Tell the user in one short sentence, and do not "+
			"retry.", res.Query), true
	case resolveNone:
		what := res.Query
		if what == "" {
			what = "that"
		}
		return fmt.Sprintf("No window matches %q. Tell the user in one short sentence that nothing "+
			"like that is open, and do not retry.", what), true
	}
	return "", false
}

// unavailable is what every verb says when the compositor cannot be reached.
// It is a tool result, never an error: a desktop Jarvix cannot see is a thing
// to mention in one sentence, not a failed session.
func (d *Desktop) unavailable(err error) string {
	d.log.Warn("compositor unavailable", "component", "tools", "error", err.Error())
	return "The window manager is not available on this computer, so nothing could be seen or " +
		"changed. Tell the user in one short sentence that you cannot see their windows here, and " +
		"do not retry."
}

func (d *Desktop) publish(verb, target string) {
	if d.onAction != nil {
		d.onAction(verb, target)
	}
}

// publishRefusal reports an action that did not happen. reason is chosen at
// each call site to be safe on the bus: the resolver's own sentence for a
// refused launch, a generic clause where the underlying error is operator
// material (compositor diagnostics, launcher errors with paths).
func (d *Desktop) publishRefusal(verb, target, reason string) {
	if d.onRefusal != nil {
		d.onRefusal(verb, target, reason)
	}
}

// Confirmation implements Confirmable: the ask tier's question, built from the
// inventory rather than from the model's arguments.
//
// This is the difference between "I want to use the desktop.close_window tool"
// and "I want to close Firefox — the window titled …". It matters twice over:
// the user hears which window is about to go, and the resolution behind that
// sentence is kept, so approving *this* window cannot close another one that
// happens to match the same words a moment later.
func (t *windowTool) Confirmation(input json.RawMessage) (command, summary string, ok bool) {
	if t.verb == verbList || t.verb == verbFocus || t.verb == verbName {
		// Allow-tier verbs; never asked about. Naming belongs here because it
		// changes nothing on screen and the opposite assignment undoes it.
		return "", "", false
	}
	var args windowArgs
	if err := unmarshalWindowArgs(input, &args); err != nil {
		return "", "", false
	}
	if t.verb == verbLaunch {
		name := strings.TrimSpace(args.App)
		binary, candidates, err := t.d.resolveApp(name)
		if err != nil || len(candidates) > 0 {
			return "", "", false // Execute will explain; there is nothing to confirm
		}
		app := filepath.Base(binary)
		return "launch " + app, fmt.Sprintf("I want to open %s. Should I go ahead?", app), true
	}
	if t.verb == verbMove && (args.Workspace < 1 || args.Workspace > maxWorkspace) {
		return "", "", false
	}

	// Resolution needs the compositor, and the gate has no context of its own
	// — it is a synchronous decision on the session's think goroutine. Bound
	// it tightly: a compositor that will not answer must cost a generic
	// question, not a pause before one.
	ctx, cancel := context.WithTimeout(context.Background(), t.d.timeout)
	defer cancel()
	res, err := t.d.resolve(ctx, args.Window)
	if err != nil || res.Kind != resolveOne {
		return "", "", false
	}
	t.d.holdPending(t.Name(), args, res.Window)

	app, title := desktop.AppName(res.Window.Class), spokenTitle(res.Window.Title)
	switch t.verb {
	case verbClose:
		return "close " + res.Window.Describe(),
			fmt.Sprintf("I want to close %s%s. Should I go ahead?", app, title), true
	default:
		return fmt.Sprintf("move %s to workspace %d", res.Window.Describe(), args.Workspace),
			fmt.Sprintf("I want to move %s%s to workspace %d. Should I go ahead?",
				app, title, args.Workspace), true
	}
}

// spokenTitle renders a window title inside a confirmation question, bounded
// for speech and omitted when it says nothing the application name did not.
func spokenTitle(title string) string {
	title = strings.TrimSpace(title)
	if title == "" {
		return ""
	}
	runes := []rune(title)
	if len(runes) > maxSpokenTitle {
		title = strings.TrimSpace(string(runes[:maxSpokenTitle])) + "…"
	}
	return ", the window titled " + title
}

// pendingKey identifies one call for the resolution memo: the tool plus the
// arguments that produced the resolution, so an approval can never be spent on
// a different call that merely used the same tool.
func pendingKey(tool string, args windowArgs) string {
	return tool + "\x00" + strings.ToLower(strings.TrimSpace(args.Window)) +
		"\x00" + strconv.Itoa(args.Workspace)
}

// holdPending remembers the window a confirmation question was asked about.
func (d *Desktop) holdPending(tool string, args windowArgs, w desktop.Window) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	for key, held := range d.pending {
		if now.Sub(held.at) > resolutionTTL {
			delete(d.pending, key)
		}
	}
	d.pending[pendingKey(tool, args)] = pendingTarget{window: w, at: now}
}

// takePending consumes the resolution held for this call, if it is still
// current. Consumed rather than read: an approval authorises one action, and a
// second identical call is a second decision.
func (d *Desktop) takePending(tool string, args windowArgs) (desktop.Window, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	key := pendingKey(tool, args)
	held, ok := d.pending[key]
	if !ok {
		return desktop.Window{}, false
	}
	delete(d.pending, key)
	if time.Since(held.at) > resolutionTTL {
		return desktop.Window{}, false
	}
	return held.window, true
}

// appLauncher starts an application. An interface so tests can prove what
// would have been executed without starting anything.
type appLauncher interface {
	Launch(ctx context.Context, binary string) error
}

// execLauncher starts an application as a detached child.
//
// Two decisions are deliberate and both are the opposite of how every other
// subprocess in Jarvix is run. The context is *not* used to kill it: an
// application the user asked for must outlive the sentence that asked for it,
// unlike a command whose output the session is waiting on. And it gets its own
// process group, so the daemon being restarted or its group signalled does not
// take the user's editor with it.
type execLauncher struct {
	// scrubEnv names extra environment variables to withhold, on top of the
	// built-in secret-name patterns: a launched program is a third party, the
	// same as an advisor CLI, and has no business seeing Jarvix's keys.
	scrubEnv []string
}

// Launch implements appLauncher.
func (e *execLauncher) Launch(ctx context.Context, binary string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cmd := exec.Command(binary) //nolint:gosec // binary came from the allow list or exec.LookPath; the model never supplies a path
	cmd.Env = scrubbedEnv(os.Environ(), e.scrubEnv)
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

// plural renders a count with the right noun: "1 window", "3 windows".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}
