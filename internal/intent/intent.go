// Package intent implements the deterministic intent router that sits between
// a final transcript and the assistant (roadmap Phase 3).
//
// The router is an explicit grammar table, not a machine-learning system, and
// that is the whole design. "volume thirty" is not a question worth a model
// call: it is a fixed phrase with one integer in it, and turning it into an
// STT → LLM → TTS round trip costs seconds and a token bill for something the
// machine could have done in microseconds. Matching is therefore strict —
// every pattern matches a whole utterance, word for word — so anything the
// table does not recognise verbatim ("turn it up a bit") falls through to the
// model untouched. Ambiguity always belongs to the AI; the router only claims
// utterances it is certain about.
//
// Security follows from the same strictness: a built-in intent maps either to
// a FIXED argv (`wpctl set-volume …`) with no shell involved, or to a named
// compositor action the desktop seam renders (Desktop, below), and the
// transcript contributes nothing but a slot value that has been parsed as an
// integer and bounds-checked. There is no path by which spoken words become
// part of a command line. User-defined intents do run a shell command, which
// is why the engine puts them through the tool permission gate (ADR 0014)
// rather than executing them here.
package intent

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Control names an effect the session engine performs itself rather than by
// running a command. These are the intents that act on Jarvix, not on the
// desktop, so they cannot be expressed as an argv.
type Control string

// Desktop names an action the engine performs through the compositor seam
// (internal/desktop, ADR 0022) rather than by running an argv of its own.
//
// The distinction is not tidiness, it is the fix for issue #47. `hyprctl
// dispatch` changed syntax when Hyprland moved its configuration to Lua, and
// which syntax a given machine speaks is not a version question — it follows
// the config format, so it has to be *discovered*. The seam discovers it once
// and remembers it. A table entry that built its own `hyprctl dispatch
// workspace 4` was therefore a second, un-probed copy of a decision only the
// seam can make, and on a Lua-configured desktop it was a parse error:
// "workspace four" did nothing.
//
// So the table names the action and the seam decides how to say it. That also
// buys the honesty half of the same bug: the seam knows `hyprctl` exits 0 for
// a dispatch the compositor refused, and reports a refusal as a failure the
// user hears.
type Desktop string

// The engine-level intent effects.
const (
	// ControlNone is a normal intent: run its command, then acknowledge.
	ControlNone Control = ""
	// ControlStopSpeech halts spoken output through the engine's existing
	// CancelSpeech path. It carries no acknowledgement: silence is the
	// confirmation, and speaking over a "stop" would be absurd.
	ControlStopSpeech Control = "stop_speech"
	// ControlNewConversation clears the carried-over conversation, the same
	// reset `jarvix new` performs.
	ControlNewConversation Control = "new_conversation"
)

// The compositor actions the built-in table uses.
const (
	// DesktopNone is an intent that does not touch the compositor.
	DesktopNone Desktop = ""
	// DesktopWorkspace takes the user to the workspace in Match.Slot.
	DesktopWorkspace Desktop = "workspace"
	// DesktopSpawn starts the program named in Match.Program.
	DesktopSpawn Desktop = "spawn"
)

// Match is one routed utterance: everything the engine needs to act, announce,
// and log, with no further parsing.
type Match struct {
	// Name is the intent's stable identifier, e.g. "volume.set". It appears
	// in logs and in the intent.executed event.
	Name string
	// Slot is the validated integer the utterance supplied; HasSlot says
	// whether the intent has one at all (Slot is 0 for those that do not).
	Slot    int
	HasSlot bool
	// Ack is the terse spoken/overlay acknowledgement ("Volume thirty").
	// Empty means acknowledge with silence.
	Ack string
	// Control is the engine-level effect, ControlNone for command intents.
	Control Control
	// Desktop is the compositor action, DesktopNone for everything else.
	// A compositor intent carries no Argv: the dispatch syntax depends on the
	// dialect this machine's compositor speaks, and only the desktop seam
	// knows that.
	Desktop Desktop
	// Program is the executable DesktopSpawn starts. It has been validated as
	// a single bare token (ValidateTerminal), so it can only ever be one
	// program name. Empty for every other intent.
	Program string
	// Argv is the fixed command line for a built-in intent: argv[0] is the
	// binary, and any slot value has already been rendered into an argument.
	// No shell is involved and the transcript never reaches it verbatim.
	// Empty for a compositor intent, which names a Desktop action instead.
	Argv []string
	// Command is a user-defined intent's shell command, exactly as written in
	// configuration. Empty for built-ins. It is never executed here — the
	// engine runs it through the tool permission gate first.
	Command string
	// UserDefined distinguishes a configured intent from a built-in one, for
	// the gate decision and for observability.
	UserDefined bool
	// Routine is the configured routine this utterance triggers ([[routines]],
	// ADR 0026), empty for every other intent. Only the name travels: the
	// router decides *whether* an utterance is a routine, and the engine hands
	// the name to the routine runner, which owns what the steps mean.
	Routine string
	// CaptureName is the routine name spoken to "save this as <name>" (#62),
	// empty for every other intent. Like Routine, only the name travels: the
	// router decides *whether* the utterance asks for a capture, and the
	// engine hands the name to the capture service, which owns everything
	// about reading the desktop and writing configuration.
	CaptureName string
	// Script is the configured script this utterance triggers ([[scripts]],
	// ADR 0030), empty for every other intent. Only the name travels, and for
	// a stronger reason than Routine's tidiness: the script runner passes its
	// executable zero arguments by construction, and a Match that carried
	// anything more than a name would be the first place an argument could
	// come from.
	Script string
	// WindowName is the nickname spoken to "call this window <name>" (#126),
	// empty for every other intent. Like CaptureName, only the raw spoken
	// name travels: the router decides *whether* the utterance names a
	// window, and the engine hands the words to the window-name seam, which
	// owns normalisation, collision checks, and the assignment itself.
	WindowName string
	// WindowNames marks "what are my windows called" (#126): the engine
	// answers with the window-name seam's one spoken listing.
	WindowNames bool
	// Briefing marks "what did i miss" (#150, ADR 0050): the engine answers
	// with the return briefing's one composed account of the absence. Like
	// the listing above it is a flag rather than an argv — nothing about
	// what happened while the user was away can live in this table.
	Briefing bool
	// Focus is the focus-thread action this utterance asks for (#123),
	// FocusNone for every other intent. The engine performs none of these
	// itself — it hands the whole match to the focus runner, which owns the
	// thread store and every sentence spoken about it.
	Focus FocusAction
	// FocusText is the bounded free text a focus phrase carried — a thread
	// name or a parked thought — empty when the action takes none. It goes
	// nowhere but the focus service's own store: never an argv, never a
	// shell, never a dispatch.
	FocusText string
	// FocusWindows is how many windows the phrase asked to anchor (0, 1 or
	// 2), fixed by which pattern matched — "with this window" versus "with
	// these two windows" — never parsed from free text.
	FocusWindows int
	// Reminder is the one-shot reminder action this utterance asks for
	// (#141, ADR 0046), ReminderNone for every other intent. The engine
	// performs none of these itself — it hands the whole match to the
	// reminder runner, which owns the store and every sentence spoken.
	Reminder ReminderAction
	// ReminderWhen is the raw time words a set phrase carried ("at three",
	// "in twenty minutes") — already known to parse, because the {when} slot
	// only matches what ParseWhen accepts. The service parses the same words
	// again and resolves them against its own clock; no clock lives here.
	ReminderWhen string
	// ReminderText is the bounded free text of a set or cancel phrase — the
	// reminder's words. It goes nowhere but the reminder store: never an
	// argv, never a shell, never a dispatch.
	ReminderText string
	// VocabPhrase and VocabMeaning carry a spoken teach — "when i say
	// {phrase} i mean {meaning}" (#129). Both empty for every other intent.
	// Only the raw words travel: the router decides *whether* the utterance
	// teaches, and the vocabulary seam owns storage and supersede.
	VocabPhrase  string
	VocabMeaning string
	// VocabListen carries "listen for the word {phrase}" (#129): the phrase
	// to flag hard-to-hear, empty for every other intent.
	VocabListen string
	// VocabList marks "what words have i taught you" (#129): the engine
	// answers with the vocabulary seam's one spoken listing.
	VocabList bool
}

// Custom is one user-defined intent from configuration ([[intents.custom]]).
type Custom struct {
	// Match is the literal phrase to recognise. Slot placeholders are not
	// accepted: a slot would have to be interpolated into Run, and building
	// shell commands out of speech is exactly what this package refuses to do.
	Match string
	// Run is the shell command to execute, subject to the permission gate.
	Run string
	// Say is the spoken acknowledgement; empty uses a generic one.
	Say string
}

// RoutinePhrases is the router's view of one configured routine ([[routines]]):
// the name the engine will run and the phrases that trigger it. The steps are
// deliberately absent — routing decides *whether* an utterance is a routine,
// never what a routine does, so nothing about launching or placing windows can
// leak into the grammar table.
type RoutinePhrases struct {
	// Name is the routine's configured name, spoken in summaries and handed
	// to the runner on a match.
	Name string
	// Phrases are the literal trigger phrases. Placeholders are not accepted:
	// a routine's steps are fixed by configuration, so there is nothing a slot
	// value could parameterise.
	Phrases []string
	// Disabled parks the routine (issue #93): its own shape — name, phrases,
	// grammar — is still validated, so re-enabling cannot surprise, but the
	// phrases enter neither the grammar nor the collision set. The inverted
	// name keeps the zero value meaning "routable", matching every caller
	// that predates the switch.
	Disabled bool
}

// ScriptPhrases is the router's view of one configured script ([[scripts]],
// ADR 0030): the name the engine will run and the phrases that trigger it.
// The path, timeout, and report mode are deliberately absent — routing
// decides *whether* an utterance is a script, never what runs or how, so
// nothing about execution can leak into the grammar table.
type ScriptPhrases struct {
	// Name is the script's configured name, spoken in confirmations and
	// handed to the runner on a match.
	Name string
	// Phrases are the literal trigger phrases. Placeholders are not accepted:
	// a script runs with zero arguments in v1, so there is nothing a slot
	// value could ever become — and refusing the syntax outright means the
	// question of interpolating speech into an argv can never even be asked.
	Phrases []string
	// Disabled parks the script (issue #93): its own shape — name, phrases,
	// grammar — is still validated, so re-enabling cannot surprise, but the
	// phrases enter neither the grammar nor the collision set. The inverted
	// name keeps the zero value meaning "routable", matching every caller
	// that predates the switch.
	Disabled bool
}

// Options configures a router.
type Options struct {
	// Terminal is the binary "open terminal" launches. It is validated as a
	// single bare token so it can only ever be one argv element.
	Terminal string
	// Custom holds the user-defined intents.
	Custom []Custom
	// Routines holds the configured routines' trigger phrases (ADR 0026).
	Routines []RoutinePhrases
	// Scripts holds the configured scripts' trigger phrases (ADR 0030).
	Scripts []ScriptPhrases
}

// DefaultTerminal is the terminal "open terminal" launches when configuration
// does not name one. Omarchy's default; override it in [intents].
const DefaultTerminal = "alacritty"

// defaultSink is wpctl's name for "whatever is currently the output device",
// so the intents work across headphones, speakers, and HDMI without knowing
// any device ids.
const defaultSink = "@DEFAULT_AUDIO_SINK@"

// volumeStep is how far "louder"/"quieter" move the volume. Small enough that
// a second "louder" is a natural correction rather than an overshoot.
const volumeStep = "5%"

// Slot bounds. These are the security boundary for built-in intents: a value
// outside the range is not clamped, it simply fails to match, and the
// utterance goes to the model like any other unrecognised phrase.
const (
	minVolume    = 0
	maxVolume    = 150
	minWorkspace = 1
	maxWorkspace = 10
)

// Router is a compiled intent table. Everything is built once by New and is
// immutable afterwards, so Match is allocation-light, lock-free, and safe to
// call from any goroutine.
type Router struct {
	// byFirstWord indexes rules by their pattern's first (always literal)
	// word. A miss is therefore one map lookup, which is what keeps the
	// non-matching path — by far the common one — effectively free.
	byFirstWord map[string][]*rule
	// maxWords is the longest utterance any pattern could match, used to
	// reject long sentences before touching the index.
	maxWords int
	// names lists the compiled intents in table order, for diagnostics.
	names []string
	// owned maps every literal phrase this table claims to a human
	// description of its owner — the same map the collision checks in New
	// build. Retained so Owner can answer "is this phrase spoken for?" for
	// window nicknames (#126) with the exact wording a config collision
	// error uses.
	owned map[string]string
}

// rule is one compiled pattern together with what to do when it matches.
type rule struct {
	name        string
	pattern     pattern
	control     Control
	desktop     Desktop
	program     string
	argv        func(slot int) []string
	ack         func(slot int) string
	command     string
	userDefined bool
	routine     string
	script      string
	// capture marks the "save this as {name}" rules (#62): a match hands the
	// trailing free-text words to the engine as Match.CaptureName.
	capture bool
	// windowName marks the "call this window {name}" rules (#126): a match
	// hands the trailing free-text words to the engine as Match.WindowName.
	windowName bool
	// windowNames marks the "what are my windows called" rules (#126).
	windowNames bool
	// briefing marks the "what did i miss" rules (#150).
	briefing bool
	// focus and focusWindows carry a focus rule's action (#123); focus is
	// FocusNone for every other rule.
	focus        FocusAction
	focusWindows int
	// reminder carries a reminder rule's action (#141); ReminderNone for
	// every other rule.
	reminder ReminderAction
	// vocabTeach marks the "when i say {phrase} i mean {meaning}" rules
	// (#129): a match hands both slots to the engine via matchVocab.
	vocabTeach bool
	// vocabListen marks the "listen for the word {phrase}" rules (#129).
	vocabListen bool
	// vocabList marks the "what words have i taught you" rules (#129).
	vocabList bool
}

// builtin is one entry of the shipped grammar table.
type builtin struct {
	name     string
	patterns []string
	control  Control
	desktop  Desktop
	program  string
	argv     func(slot int) []string
	ack      func(slot int) string
}

// builtinTable is the shipped intent grammar: the phrases Jarvix answers
// without a model. It is deliberately a short, literal list — every entry is
// a phrase a person actually says, and adding a near-synonym is a code change
// with a test, not a similarity threshold.
//
// Placeholders: {volume} is an integer 0–150, {workspace} an integer 1–10.
// Both accept digits ("volume 30") and number words ("volume thirty").
func builtinTable(terminal string) []builtin {
	return []builtin{
		{
			name: "volume.set",
			patterns: []string{
				"volume {volume}",
				"volume {volume} percent",
				"volume to {volume}",
				"volume at {volume}",
				"volume level {volume}",
				"set volume {volume}",
				"set volume to {volume}",
				"set the volume {volume}",
				"set the volume to {volume}",
				"change the volume to {volume}",
			},
			// -l 1.5 raises wpctl's default 100% ceiling so the documented
			// 0–150 range is actually reachable.
			argv: func(n int) []string {
				return []string{"wpctl", "set-volume", "-l", "1.5", defaultSink, strconv.Itoa(n) + "%"}
			},
			ack: func(n int) string { return "Volume " + SpokenNumber(n) },
		},
		{
			name: "volume.up",
			patterns: []string{
				"volume up", "turn the volume up", "turn up the volume",
				"turn it up", "louder",
			},
			argv: func(int) []string {
				return []string{"wpctl", "set-volume", "-l", "1.5", defaultSink, volumeStep + "+"}
			},
			ack: func(int) string { return "Volume up" },
		},
		{
			name: "volume.down",
			patterns: []string{
				"volume down", "turn the volume down", "turn down the volume",
				"turn it down", "quieter",
			},
			argv: func(int) []string {
				return []string{"wpctl", "set-volume", defaultSink, volumeStep + "-"}
			},
			ack: func(int) string { return "Volume down" },
		},
		{
			name:     "volume.mute",
			patterns: []string{"mute", "mute the volume", "mute audio", "mute the audio", "mute the sound"},
			argv:     func(int) []string { return []string{"wpctl", "set-mute", defaultSink, "1"} },
			ack:      func(int) string { return "Muted" },
		},
		{
			name:     "volume.unmute",
			patterns: []string{"unmute", "unmute the volume", "unmute audio", "unmute the audio", "unmute the sound"},
			argv:     func(int) []string { return []string{"wpctl", "set-mute", defaultSink, "0"} },
			ack:      func(int) string { return "Unmuted" },
		},
		{
			// Silence is the acknowledgement: speaking after "stop talking"
			// would be its own joke. The engine routes this to CancelSpeech.
			name:     "speech.stop",
			patterns: []string{"stop", "stop talking", "stop it", "be quiet", "quiet", "shush", "shut up", "enough"},
			control:  ControlStopSpeech,
		},
		{
			name: "conversation.new",
			patterns: []string{
				"new conversation", "start a new conversation", "start a new chat",
				"new chat", "start over", "forget that", "forget all that",
				"clear the conversation", "reset the conversation",
			},
			control: ControlNewConversation,
			ack:     func(int) string { return "New conversation." },
		},
		{
			name: "workspace.switch",
			patterns: []string{
				"workspace {workspace}", "go to workspace {workspace}",
				"switch to workspace {workspace}", "move to workspace {workspace}",
			},
			desktop: DesktopWorkspace,
			ack:     func(n int) string { return "Workspace " + SpokenNumber(n) },
		},
		{
			name: "terminal.open",
			patterns: []string{
				"open terminal", "open a terminal", "open the terminal",
				"new terminal", "launch terminal", "launch a terminal",
			},
			desktop: DesktopSpawn,
			program: terminal,
			ack:     func(int) string { return "Terminal." },
		},
	}
}

// BuiltinBinaries lists the executables the built-in table needs, so `jarvix
// doctor` can report a missing one before a user discovers it by saying
// "mute" and hearing an apology. hyprctl is still on the list even though the
// compositor intents now go through the desktop seam: the seam drives hyprctl
// too, and a missing one breaks them the same way. Being installed is not
// sufficient, though — see the doctor check, which also asks whether a
// dispatch would actually reach a compositor.
func BuiltinBinaries(terminal string) []string {
	if strings.TrimSpace(terminal) == "" {
		terminal = DefaultTerminal
	}
	return []string{"wpctl", "hyprctl", terminal}
}

// New compiles a router. Errors name the offending configuration entry so a
// typo in [[intents.custom]] is a startup message, not a phrase that silently
// never works.
func New(opts Options) (*Router, error) {
	terminal := strings.TrimSpace(opts.Terminal)
	if terminal == "" {
		terminal = DefaultTerminal
	}
	if err := ValidateTerminal(terminal); err != nil {
		return nil, err
	}
	r := &Router{byFirstWord: make(map[string][]*rule)}
	builtinNames := make(map[string]string) // pattern → intent it belongs to

	for _, b := range builtinTable(terminal) {
		r.names = append(r.names, b.name)
		for _, raw := range b.patterns {
			p, err := compile(raw)
			if err != nil {
				return nil, fmt.Errorf("built-in intent %q pattern %q: %w", b.name, raw, err)
			}
			builtinNames[p.key()] = b.name
			r.add(&rule{name: b.name, pattern: p, control: b.control,
				desktop: b.desktop, program: b.program, argv: b.argv, ack: b.ack})
		}
	}

	// The window-name listing phrases (#126) are built-ins in all but table
	// membership: literal, whole-utterance, and owned, so a custom intent or
	// routine that wants one is refused naming this owner like any built-in's.
	// They live outside builtinTable because their rule carries a flag, not an
	// argv or a compositor action — the listing is composed by the window-name
	// seam at match time, and nothing about windows may live in this table.
	for _, raw := range windowNamesPatterns {
		p, err := compile(raw)
		if err != nil {
			// Unreachable for the shipped list; a bad pattern added later must
			// fail compilation, not silently never match.
			return nil, fmt.Errorf("window names pattern %q: %w", raw, err)
		}
		builtinNames[p.key()] = WindowNamesIntentName
		r.add(&rule{name: WindowNamesIntentName, pattern: p, windowNames: true})
	}
	r.names = append(r.names, WindowNamesIntentName)
	// The return briefing (#150, ADR 0050) compiles on exactly the same
	// terms as the listing above, and for the same reason: a flag-carrying
	// built-in whose answer is composed elsewhere. It is in the router at all
	// because ADR 0017's rule applies — a fixed phrase with one right outcome
	// must not spend a provider round-trip deciding it did.
	for _, raw := range briefingPatterns {
		p, err := compile(raw)
		if err != nil {
			// Unreachable for the shipped list; a bad pattern added later must
			// fail compilation, not silently never match.
			return nil, fmt.Errorf("briefing pattern %q: %w", raw, err)
		}
		builtinNames[p.key()] = BriefingIntentName
		r.add(&rule{name: BriefingIntentName, pattern: p, briefing: true})
	}
	r.names = append(r.names, BriefingIntentName)
	// The fixed half of the focus grammar (#123) compiles with the built-ins
	// and enters the same collision world: a custom intent or routine
	// claiming "take a break" is a config error naming both owners. The
	// free-text half compiles at the very end, beside the capture patterns,
	// for the same precedence reason those do.
	focusNamed := make(map[string]bool)
	for _, fb := range focusFixedTable() {
		if !focusNamed[fb.name] {
			// Two table entries may share a name ("focus.anchor" carries its
			// window count per entry); the diagnostics list names once.
			focusNamed[fb.name] = true
			r.names = append(r.names, fb.name)
		}
		for _, raw := range fb.patterns {
			p, err := compile(raw)
			if err != nil {
				return nil, fmt.Errorf("built-in intent %q pattern %q: %w", fb.name, raw, err)
			}
			builtinNames[p.key()] = fb.name
			r.add(&rule{name: fb.name, pattern: p, focus: fb.action, focusWindows: fb.windows})
		}
	}

	// The fixed half of the reminder grammar (#141) compiles with the
	// built-ins on the focus table's exact terms: whole-utterance, in the
	// collision map, refused to any custom intent or routine that wants one.
	// The free-text half compiles at the very end, beside the other slot
	// patterns, for the same precedence reason those do.
	for _, rb := range reminderFixedTable() {
		r.names = append(r.names, rb.name)
		for _, raw := range rb.patterns {
			p, err := compile(raw)
			if err != nil {
				return nil, fmt.Errorf("built-in intent %q pattern %q: %w", rb.name, raw, err)
			}
			builtinNames[p.key()] = rb.name
			r.add(&rule{name: rb.name, pattern: p, reminder: rb.action})
		}
	}

	// The vocabulary listing phrases (#129) are owned literals on the window
	// listing's exact terms: whole-utterance, in the collision map, refused
	// to any custom intent or routine that wants one.
	for _, raw := range vocabListPatterns {
		p, err := compile(raw)
		if err != nil {
			// Unreachable for the shipped list, same as the window listing's.
			return nil, fmt.Errorf("vocabulary list pattern %q: %w", raw, err)
		}
		builtinNames[p.key()] = VocabListIntentName
		r.add(&rule{name: VocabListIntentName, pattern: p, vocabList: true})
	}
	r.names = append(r.names, VocabListIntentName)

	// taken maps a compiled phrase to a human description of what owns it, so
	// a routine phrase collision is reported against whichever of the three
	// families — built-in, custom intent, another routine — got there first.
	taken := make(map[string]string, len(builtinNames))
	for key, owner := range builtinNames {
		taken[key] = fmt.Sprintf("the built-in intent %q", owner)
	}

	for i, c := range opts.Custom {
		p, err := compileCustom(i, c)
		if err != nil {
			return nil, err
		}
		if owner, clash := builtinNames[p.key()]; clash {
			return nil, fmt.Errorf("%s: match %q is already the built-in intent %q; "+
				"choose a different phrase", customLabel(i), c.Match, owner)
		}
		taken[p.key()] = fmt.Sprintf("%s (%q)", customLabel(i), c.Match)
		ack := strings.TrimSpace(c.Say)
		if ack == "" {
			ack = "Done."
		}
		r.add(&rule{
			name: "custom." + p.key(), pattern: p,
			command: strings.TrimSpace(c.Run), userDefined: true,
			ack: func(int) string { return ack },
		})
		r.names = append(r.names, "custom."+p.key())
	}

	// Routines last, and checked against everything: a phrase that already
	// belongs to a built-in, a custom intent, or an earlier routine is a
	// config error at load, never a silent coin toss at match time. The rules
	// they compile to carry only the routine's name — no ack (the engine
	// speaks the run's summary instead) and no argv or command of any kind.
	//
	// A disabled routine (#93) walks the same per-entry checks — its name,
	// its phrases, their grammar — so a parked entry that rotted is still a
	// load error, but its phrases enter neither the grammar nor the collision
	// set: the utterance falls through to the assistant, and another entry
	// may hold the phrase meanwhile. Re-enabling recompiles this table, which
	// is where such a collision is caught, with the same error a load gives.
	for i, rt := range opts.Routines {
		name := strings.TrimSpace(rt.Name)
		label := routineLabel(i, name)
		if name == "" {
			return nil, fmt.Errorf("%s: name is empty; give the routine a name to trigger and log under", label)
		}
		if len(rt.Phrases) == 0 {
			return nil, fmt.Errorf("%s: it has no phrases; add at least one trigger phrase", label)
		}
		for _, phrase := range rt.Phrases {
			if strings.ContainsAny(phrase, "{}") {
				// A routine's steps are fixed by configuration, so a slot
				// would have nothing to parameterise — refusing it here keeps
				// the schema honest for the capture tooling too (#62).
				return nil, fmt.Errorf("%s: phrase %q contains a placeholder; routine phrases are "+
					"literal, because the steps are fixed by the configuration", label, phrase)
			}
			p, err := compile(phrase)
			if err != nil {
				return nil, fmt.Errorf("%s: phrase %q: %w", label, phrase, err)
			}
			if rt.Disabled {
				continue
			}
			if owner, clash := taken[p.key()]; clash {
				return nil, fmt.Errorf("%s: phrase %q is already %s; choose a different phrase",
					label, phrase, owner)
			}
			taken[p.key()] = fmt.Sprintf("the trigger for routine %q", name)
			r.add(&rule{name: "routine.run", pattern: p, routine: name})
		}
		if rt.Disabled {
			continue
		}
		r.names = append(r.names, "routine:"+name)
	}

	// Scripts compile after routines and face the same closed world: a phrase
	// that already belongs to a built-in, a custom intent, a routine, or an
	// earlier script is a config error at load naming both owners, never a
	// silent coin toss at match time. The rules they compile to carry only
	// the script's name — no ack (the engine speaks the run's outcome), no
	// argv, no command, and nowhere an argument could hide (ADR 0030).
	for i, sc := range opts.Scripts {
		name := strings.TrimSpace(sc.Name)
		label := scriptLabel(i, name)
		if name == "" {
			return nil, fmt.Errorf("%s: name is empty; give the script a name to trigger and log under", label)
		}
		if len(sc.Phrases) == 0 {
			return nil, fmt.Errorf("%s: it has no phrases; add at least one trigger phrase", label)
		}
		for _, phrase := range sc.Phrases {
			if strings.ContainsAny(phrase, "{}") {
				// A slot value would have nowhere to go but the script's argv
				// or environment, which is the one flow this feature exists to
				// make impossible (zero-argument scripts, ADR 0030).
				return nil, fmt.Errorf("%s: phrase %q contains a placeholder; script phrases are "+
					"literal, because nothing spoken may ever reach the script", label, phrase)
			}
			p, err := compile(phrase)
			if err != nil {
				return nil, fmt.Errorf("%s: phrase %q: %w", label, phrase, err)
			}
			// A disabled script (#93) keeps every per-entry check above but
			// stays out of the grammar and the collision set — the routine
			// loop's rule, restated for the family whose phrase must reach an
			// executable only while the entry says so.
			if sc.Disabled {
				continue
			}
			if owner, clash := taken[p.key()]; clash {
				return nil, fmt.Errorf("%s: phrase %q is already %s; choose a different phrase",
					label, phrase, owner)
			}
			taken[p.key()] = fmt.Sprintf("the trigger for script %q", name)
			r.add(&rule{name: ScriptIntentName, pattern: p, script: name})
		}
		if sc.Disabled {
			continue
		}
		r.names = append(r.names, "script:"+name)
	}

	// The capture patterns (#62) compile last on purpose: rules are tried in
	// insertion order, so every literal phrase — built-in, custom, or routine
	// trigger — that happens to begin with the same words wins over the
	// free-text name slot. A user whose routine is literally called "save
	// this as backup" gets their routine, not a capture of a routine named
	// "backup"; only utterances no literal phrase claims reach the slot.
	for _, raw := range capturePatterns {
		p, err := compileCapture(raw)
		if err != nil {
			// Unreachable for the shipped table; a bad pattern added later must
			// fail compilation, not silently never match.
			return nil, fmt.Errorf("capture pattern %q: %w", raw, err)
		}
		r.add(&rule{name: CaptureIntentName, pattern: p, capture: true})
	}
	r.names = append(r.names, CaptureIntentName)

	// The window-name assignment patterns (#126) share the capture patterns'
	// shape — literal words ending in the one free-text slot — and their
	// position: compiled last, so every literal phrase wins over the slot.
	for _, raw := range windowNamePatterns {
		p, err := compileCapture(raw)
		if err != nil {
			// Unreachable for the shipped list, same as the capture table's.
			return nil, fmt.Errorf("window name pattern %q: %w", raw, err)
		}
		r.add(&rule{name: WindowNameIntentName, pattern: p, windowName: true})
	}
	r.names = append(r.names, WindowNameIntentName)

	// The vocabulary teach and listen patterns (#129) compile with the
	// free-text group, after every literal phrase, for the capture table's
	// reason: rules are tried in insertion order, so a literal phrase that
	// happens to open with the same words always wins over a slot.
	for _, vt := range vocabTeachPatterns {
		p, err := compileVocabTeach(vt.lead, vt.sep)
		if err != nil {
			// Unreachable for the shipped list; a bad pattern added later must
			// fail compilation, not silently never match.
			return nil, fmt.Errorf("vocabulary teach pattern %q: %w", vt.lead, err)
		}
		r.add(&rule{name: VocabTeachIntentName, pattern: p, vocabTeach: true})
	}
	r.names = append(r.names, VocabTeachIntentName)
	for _, raw := range vocabListenPatterns {
		p, err := compileCapture(raw)
		if err != nil {
			// Unreachable for the shipped list, same as the capture table's.
			return nil, fmt.Errorf("vocabulary listen pattern %q: %w", raw, err)
		}
		r.add(&rule{name: VocabListenIntentName, pattern: p, vocabListen: true})
	}
	r.names = append(r.names, VocabListenIntentName)

	// Keep the collision map: Owner answers nickname-assignment checks (#126)
	// from the very map the config collisions were judged against, so a
	// refused nickname and a refused routine phrase name owners identically.
	r.owned = taken
	// The free-text focus patterns (#123) compile after everything else, for
	// the capture patterns' reason: a {text} slot could claim an utterance a
	// literal phrase owns, so every literal phrase must sit ahead of it in
	// insertion order. Within the family, focusTextTable's own order rules —
	// most-suffixed first — so a sibling's anchoring words are never eaten by
	// a bare slot.
	for _, ft := range focusTextTable() {
		if !focusNamed[ft.name] {
			focusNamed[ft.name] = true
			r.names = append(r.names, ft.name)
		}
		for _, raw := range ft.patterns {
			p, err := compileFocus(raw, ft.textCap)
			if err != nil {
				// Unreachable for the shipped table; a bad pattern added later
				// must fail compilation, not silently never match.
				return nil, fmt.Errorf("focus pattern %q: %w", raw, err)
			}
			r.add(&rule{name: ft.name, pattern: p, focus: ft.action, focusWindows: ft.windows})
		}
	}
	// The free-text reminder patterns (#141) compile last for the same
	// reason: every literal phrase — the focus grammar's own "remind me
	// where i am every {minutes} minutes" included — sits ahead of the slots
	// in insertion order and wins its exact words.
	for _, rt := range reminderTextTable() {
		r.names = append(r.names, rt.name)
		for _, raw := range rt.patterns {
			p, err := compileReminder(raw)
			if err != nil {
				// Unreachable for the shipped table; a bad pattern added later
				// must fail compilation, not silently never match.
				return nil, fmt.Errorf("reminder pattern %q: %w", raw, err)
			}
			r.add(&rule{name: rt.name, pattern: p, reminder: rt.action})
		}
	}
	return r, nil
}

// compileFocus compiles one free-text focus pattern: literal words around
// exactly one {text} slot, with {minutes} available after it. Kept separate
// from compile so {text} stays unusable in custom intents and routine
// phrases, where free text would have to be interpolated into a command or
// would parameterise steps that are fixed — the compileCapture precedent.
func compileFocus(raw string, textCap int) (pattern, error) {
	const slot = "{text}"
	words := strings.Fields(strings.ToLower(raw))
	if len(words) == 0 {
		return pattern{}, fmt.Errorf("pattern is empty")
	}
	if textCap <= 0 {
		return pattern{}, fmt.Errorf("text slot has no word cap")
	}
	p := pattern{raw: strings.Join(words, " "), tokens: make([]token, 0, len(words))}
	texts := 0
	for _, w := range words {
		if w == slot {
			texts++
			p.tokens = append(p.tokens, token{kind: slotText, min: 1, max: textCap})
			continue
		}
		if t, ok := slotKinds[w]; ok {
			p.tokens = append(p.tokens, t)
			continue
		}
		if strings.ContainsAny(w, "{}") {
			return pattern{}, fmt.Errorf("unknown placeholder %q in a focus pattern", w)
		}
		norm := normalize(w)
		if len(norm) != 1 {
			return pattern{}, fmt.Errorf("word %q is not a plain spoken word", w)
		}
		p.tokens = append(p.tokens, token{word: norm[0]})
	}
	if texts != 1 {
		return pattern{}, fmt.Errorf("focus patterns carry exactly one %s slot", slot)
	}
	if p.tokens[0].kind != slotNone {
		return pattern{}, fmt.Errorf("pattern must begin with a word, not a placeholder")
	}
	return p, nil
}

// CaptureIntentName identifies the "save this as <name>" intent (#62) in
// logs and the intent.executed event.
const CaptureIntentName = "routine.capture"

// WindowNameIntentName identifies the "call this window <name>" intent
// (#126) in logs and the intent.executed event.
const WindowNameIntentName = "window.name"

// WindowNamesIntentName identifies the "what are my windows called" listing
// (#126) in logs and the intent.executed event.
const WindowNamesIntentName = "window.names"

// windowNamePatterns are the utterances that give the focused window a
// nickname (#126). Like the capture patterns they are a short, literal list
// ending in the one free-text slot, because the name is the user's to choose
// and cannot be enumerated. All of them say "window": "call this X" without
// it is far more likely a sentence for the model ("call this a success")
// than an assignment, and ambiguity belongs to the model, never this table.
var windowNamePatterns = []string{
	"call this window {name}",
	"call that window {name}",
	"name this window {name}",
	"nickname this window {name}",
}

// windowNamesPatterns are the utterances that list the current window
// nicknames (#126). Fully literal — a near-synonym is a code change with a
// test, like every entry of the built-in table.
var windowNamesPatterns = []string{
	"what are my windows called",
	"what are the windows called",
	"what are my windows named",
	"what did i call my windows",
	"list my window names",
}

// BriefingIntentName identifies the return briefing (#150, ADR 0050) in logs
// and the intent.executed event.
const BriefingIntentName = "briefing.speak"

// briefingPatterns are the utterances that ask for the return briefing.
// Fully literal, like every built-in: a near-synonym is a code change with a
// test, not a similarity threshold. The list is deliberately anchored on
// "miss" and "briefing" and takes no free text — "what did I miss in the
// standup" is a question for the model about a meeting, not a request for
// this account, and ambiguity belongs to the model. The model reaches the
// same answer for every phrasing this table does not claim, through the
// briefing tool.
var briefingPatterns = []string{
	"what did i miss",
	"what have i missed",
	"what did i miss while i was away",
	"what happened while i was away",
	"what happened while i was out",
	"give me the briefing",
	"give me my briefing",
	"brief me",
	"catch me up",
}

// Owner reports whether the router's grammar already claims this exact
// utterance, and names the owner in the wording a config collision error
// uses ("the built-in intent \"volume.mute\"", "the trigger for routine
// \"standup\""). It exists for window nicknames (#126): a nickname that is
// verbatim an intent phrase would be unspeakable — saying it alone would
// run the intent — so assignment refuses it, naming the owner found here.
// Matching is whole-utterance on the normalised words, the same identity
// the collision checks in New judge; nil-safe, like Match.
func (r *Router) Owner(utterance string) (owner string, taken bool) {
	if r == nil {
		return "", false
	}
	owner, taken = r.owned[strings.Join(normalize(utterance), " ")]
	return owner, taken
}

// ScriptIntentName identifies a matched script phrase (ADR 0030) in logs and
// the intent.executed event. It deliberately spells the same identity the
// permission gate judges (tools.ScriptToolName), so the audit trail reads as
// one story from match to verdict.
const ScriptIntentName = "script.run"

func scriptLabel(i int, name string) string {
	if name == "" {
		return fmt.Sprintf("scripts[%d]", i)
	}
	return fmt.Sprintf("scripts[%d] (%q)", i, name)
}

// capturePatterns are the utterances that save the live desktop as a routine
// (#62). Like every entry in the built-in table they are a short, literal
// list — a near-synonym is a code change with a test — but they end in the
// one free-text slot the router has, because the name is the user's to
// choose and cannot be enumerated.
var capturePatterns = []string{
	"save this as {name}",
	"save this layout as {name}",
	"save this setup as {name}",
	"save this desktop as {name}",
	"remember this layout as {name}",
}

// maxNameWords bounds how many words a spoken routine name may be. Past a
// handful of words the utterance is far more likely a sentence for the model
// than a name, and an unbounded slot would claim it.
const maxNameWords = 6

// compileCapture compiles one capture pattern: literal words ending in the
// {name} slot. Kept separate from compile so {name} stays unusable in custom
// intents and routine phrases, where free text would have to be interpolated
// into a command or would parameterise steps that are fixed.
func compileCapture(raw string) (pattern, error) {
	const slot = "{name}"
	words := strings.Fields(strings.ToLower(raw))
	if len(words) < 2 || words[len(words)-1] != slot {
		return pattern{}, fmt.Errorf("capture patterns must be literal words ending in %s", slot)
	}
	p, err := compile(strings.Join(words[:len(words)-1], " "))
	if err != nil {
		return pattern{}, err
	}
	p.raw = strings.Join(words, " ")
	p.trailingText = true
	return p, nil
}

func routineLabel(i int, name string) string {
	if name == "" {
		return fmt.Sprintf("routines[%d]", i)
	}
	return fmt.Sprintf("routines[%d] (%q)", i, name)
}

// ValidateCustom reports the first problem with the user-defined intents,
// naming the offending entry. Used by configuration validation so a malformed
// pattern fails at startup with a message that says which entry to fix.
func ValidateCustom(custom []Custom) error {
	_, err := New(Options{Custom: custom})
	return err
}

// terminalToken bounds what may be launched by "open terminal": one bare
// executable name or absolute path. Rejecting spaces and shell metacharacters
// here means the value can only ever be a single argv element — the setting
// cannot become a command line by being clever with quoting.
var terminalToken = regexp.MustCompile(`^[A-Za-z0-9._/+-]+$`)

// ValidateTerminal checks the [intents] terminal setting.
func ValidateTerminal(terminal string) error {
	if !terminalToken.MatchString(terminal) {
		return fmt.Errorf("intents.terminal %q must be a single executable name or absolute path "+
			"(letters, digits, . _ / + -); it is run directly, not through a shell", terminal)
	}
	return nil
}

// Names lists the compiled intents, in table order (built-ins first).
func (r *Router) Names() []string { return append([]string(nil), r.names...) }

func (r *Router) add(ru *rule) {
	first := ru.pattern.tokens[0].word
	r.byFirstWord[first] = append(r.byFirstWord[first], ru)
	if n := ru.pattern.maxFields(); n > r.maxWords {
		r.maxWords = n
	}
}

// Match reports whether the transcript is an intent, and which. It is the
// hot path in both directions: a hit must be instant, and a miss — the common
// case, since most utterances are questions — must cost effectively nothing.
// Hence the two early-outs (length, then a single map lookup on the first
// word) before any pattern is examined.
func (r *Router) Match(transcript string) (Match, bool) {
	if r == nil {
		return Match{}, false
	}
	fields := normalize(transcript)
	if len(fields) == 0 || len(fields) > r.maxWords {
		return Match{}, false
	}
	for _, ru := range r.byFirstWord[fields[0]] {
		if m, ok := ru.match(fields); ok {
			return m, true
		}
	}
	return Match{}, false
}

func (ru *rule) match(fields []string) (Match, bool) {
	if ru.capture {
		name, ok := ru.pattern.matchText(fields)
		if !ok {
			return Match{}, false
		}
		return Match{Name: ru.name, CaptureName: name}, true
	}
	if ru.windowName {
		name, ok := ru.pattern.matchText(fields)
		if !ok {
			return Match{}, false
		}
		return Match{Name: ru.name, WindowName: name}, true
	}
	if ru.vocabTeach {
		phrase, meaning, ok := ru.pattern.matchVocab(fields)
		if !ok {
			return Match{}, false
		}
		return Match{Name: ru.name, VocabPhrase: phrase, VocabMeaning: meaning}, true
	}
	if ru.vocabListen {
		phrase, ok := ru.pattern.matchText(fields)
		if !ok {
			return Match{}, false
		}
		return Match{Name: ru.name, VocabListen: phrase}, true
	}
	slot, hasSlot, text, when, ok := ru.pattern.match(fields)
	if !ok {
		return Match{}, false
	}
	m := Match{
		Name: ru.name, Slot: slot, HasSlot: hasSlot, Control: ru.control,
		Desktop: ru.desktop, Program: ru.program,
		Command: ru.command, UserDefined: ru.userDefined,
		Routine: ru.routine, Script: ru.script,
		WindowNames: ru.windowNames,
		Briefing:    ru.briefing,
		Focus:       ru.focus, FocusWindows: ru.focusWindows,
		Reminder:  ru.reminder,
		VocabList: ru.vocabList,
	}
	// The one free-text value lands with the family that owns the rule, so a
	// consumer can never read another feature's words by mistake.
	if ru.reminder != ReminderNone {
		m.ReminderText, m.ReminderWhen = text, when
	} else {
		m.FocusText = text
	}
	if ru.argv != nil {
		m.Argv = ru.argv(slot)
	}
	if ru.ack != nil {
		m.Ack = ru.ack(slot)
	}
	return m, true
}

// ------------------------------------------------------------- the grammar

// slotKind names a placeholder's type, which is also its bounds.
type slotKind uint8

const (
	slotNone slotKind = iota
	slotVolume
	slotWorkspace
	slotMinutes
	// slotText is a non-numeric slot: bounded free text (a thread name, a
	// parked thought (#123), or a reminder's words (#141)). Only compileFocus
	// and compileReminder can produce it, so free text stays impossible in
	// custom intents and routine phrases. min and max bound how many words it
	// may swallow.
	slotText
	// slotWhen is a time expression (#141): bounded words that must parse
	// under the ParseWhen table (when.go). Validation at match time is what
	// makes the slot deterministic the way the number slots are — an
	// expression the table does not recognise is a miss, and the utterance
	// belongs to the model. Only compileReminder can produce it.
	slotWhen
)

// token is one element of a pattern: a literal word, or a bounded slot.
type token struct {
	word     string // literal; empty for a slot
	kind     slotKind
	min, max int
}

// pattern is a compiled whole-utterance grammar. Matching is exact: every
// field of the transcript must be consumed and every literal must be equal.
// There is no prefix, suffix, or fuzzy matching anywhere — "turn it up" is an
// intent, "turn it up a bit" is a question for the model.
type pattern struct {
	tokens []token
	// raw is the source phrase, used for identity and error messages.
	raw string
	// trailingText marks a capture pattern (#62): every token is a literal,
	// and once they all match, the remaining one-to-maxNameWords fields are
	// the spoken name. Trailing-only by design — a slot in the middle would
	// make "save this as x on workspace two" ambiguous about where the name
	// stops, and ambiguity belongs to the model, never to this table.
	trailingText bool
	// vocabSep marks a vocabulary teach pattern (#129): the tokens are the
	// literal lead, and the two free-text slots hinge on this separator
	// occurring exactly once in what follows (see matchVocab). The one
	// middle-slot exception to trailingText's rule, made safe by exactly
	// that uniqueness requirement: an utterance where the boundary is
	// ambiguous is declined, not guessed at.
	vocabSep []string
}

// maxSlotWords bounds how many words one slot may swallow: "one hundred and
// forty five" — five words once the hyphen becomes a boundary — is the
// longest number the table admits.
const maxSlotWords = 5

func (p pattern) key() string { return p.raw }

// maxFields is the largest utterance this pattern could match.
func (p pattern) maxFields() int {
	n := 0
	for _, t := range p.tokens {
		switch t.kind {
		case slotNone:
			n++
		case slotText, slotWhen:
			n += t.max
		default:
			n += maxSlotWords
		}
	}
	if p.trailingText {
		n += maxNameWords
	}
	if len(p.vocabSep) > 0 {
		// A teach pattern (#129): the phrase slot, the separator, the meaning
		// slot — on top of the literal lead the tokens already counted.
		n += maxNameWords + len(p.vocabSep) + maxVocabMeaningWords
	}
	return n
}

// matchText matches a trailing-text pattern: every literal token in order,
// then one to maxNameWords remaining fields, which become the name.
func (p pattern) matchText(fields []string) (string, bool) {
	rest := len(fields) - len(p.tokens)
	if rest < 1 || rest > maxNameWords {
		return "", false
	}
	for i, t := range p.tokens {
		if fields[i] != t.word {
			return "", false
		}
	}
	return strings.Join(fields[len(p.tokens):], " "), true
}

func (p pattern) match(fields []string) (slot int, hasSlot bool, text, when string, ok bool) {
	ok = p.matchFrom(fields, 0, 0, &slot, &hasSlot, &text, &when)
	return slot, hasSlot, text, when, ok
}

// matchFrom walks pattern token pi against field fi. A slot backtracks over
// how many words it takes (shortest first) so "volume thirty five" resolves
// to 35 rather than failing after greedily reading "thirty", and a text slot
// (#123) backtracks the same way, so the literal words after it — "with this
// window", "thread", "for {minutes} minutes" — anchor exactly one reading.
// A when slot (#141) backtracks too, and only a reading its table actually
// parses can win, so the time words and the reminder's words split exactly
// once. The recursion is bounded by the pattern length, which is single
// digits.
func (p pattern) matchFrom(fields []string, pi, fi int, slot *int, hasSlot *bool, text, when *string) bool {
	for pi < len(p.tokens) {
		t := p.tokens[pi]
		if t.kind == slotNone {
			if fi >= len(fields) || fields[fi] != t.word {
				return false
			}
			pi++
			fi++
			continue
		}
		remaining := len(p.tokens) - pi - 1
		if t.kind == slotText {
			// Free text, bounded by the rule's word cap. Shortest first, like
			// the number slots, so the fixed words around it always win the
			// fields they name.
			for n := t.min; n <= t.max && fi+n+remaining <= len(fields); n++ {
				prev := *text
				*text = strings.Join(fields[fi:fi+n], " ")
				if p.matchFrom(fields, pi+1, fi+n, slot, hasSlot, text, when) {
					return true
				}
				*text = prev
			}
			return false
		}
		if t.kind == slotWhen {
			// A time expression: like the number slots, validation is the
			// match — words the ParseWhen table refuses simply do not fill
			// the slot, and the whole utterance stays the model's.
			for n := t.min; n <= t.max && fi+n+remaining <= len(fields); n++ {
				if _, ok := parseWhenWords(fields[fi : fi+n]); !ok {
					continue
				}
				prev := *when
				*when = strings.Join(fields[fi:fi+n], " ")
				if p.matchFrom(fields, pi+1, fi+n, slot, hasSlot, text, when) {
					return true
				}
				*when = prev
			}
			return false
		}
		for n := 1; n <= maxSlotWords && fi+n+remaining <= len(fields); n++ {
			v, parsed := parseNumber(fields[fi : fi+n])
			// Out of bounds is a miss, never a clamp: "volume five hundred"
			// belongs to the model, not to a silently truncated command.
			if !parsed || v < t.min || v > t.max {
				continue
			}
			prevSlot, prevHas := *slot, *hasSlot
			*slot, *hasSlot = v, true
			if p.matchFrom(fields, pi+1, fi+n, slot, hasSlot, text, when) {
				return true
			}
			*slot, *hasSlot = prevSlot, prevHas
		}
		return false
	}
	return fi == len(fields)
}

var slotKinds = map[string]token{
	"{volume}":    {kind: slotVolume, min: minVolume, max: maxVolume},
	"{workspace}": {kind: slotWorkspace, min: minWorkspace, max: maxWorkspace},
	"{minutes}":   {kind: slotMinutes, min: minMinutes, max: maxMinutes},
}

// compile turns a pattern source into tokens. Patterns must begin with a
// literal word so the router can index them by it.
func compile(raw string) (pattern, error) {
	words := strings.Fields(strings.ToLower(raw))
	if len(words) == 0 {
		return pattern{}, fmt.Errorf("pattern is empty")
	}
	p := pattern{raw: strings.Join(words, " "), tokens: make([]token, 0, len(words))}
	for _, w := range words {
		if t, ok := slotKinds[w]; ok {
			p.tokens = append(p.tokens, t)
			continue
		}
		if strings.ContainsAny(w, "{}") {
			return pattern{}, fmt.Errorf("unknown placeholder %q; the supported placeholders are {volume}, {workspace} and {minutes}", w)
		}
		norm := normalize(w)
		if len(norm) != 1 {
			return pattern{}, fmt.Errorf("word %q is not a plain spoken word", w)
		}
		p.tokens = append(p.tokens, token{word: norm[0]})
	}
	if p.tokens[0].kind != slotNone {
		return pattern{}, fmt.Errorf("pattern must begin with a word, not a placeholder")
	}
	return p, nil
}

// compileCustom compiles one configured intent, with errors that name the
// entry by index and phrase.
func compileCustom(i int, c Custom) (pattern, error) {
	label := customLabel(i)
	match := strings.TrimSpace(c.Match)
	if match == "" {
		return pattern{}, fmt.Errorf("%s: match is empty; set the phrase to recognise", label)
	}
	if strings.TrimSpace(c.Run) == "" {
		return pattern{}, fmt.Errorf("%s: match %q has no run command; set the command to execute", label, c.Match)
	}
	if strings.ContainsAny(match, "{}") {
		// A slot would have to be substituted into Run, i.e. speech would
		// become part of a shell command. That is the one thing this package
		// exists to make impossible.
		return pattern{}, fmt.Errorf("%s: match %q contains a placeholder; user-defined intents "+
			"match literal phrases only, because a slot would have to be interpolated into the command", label, c.Match)
	}
	p, err := compile(match)
	if err != nil {
		return pattern{}, fmt.Errorf("%s: match %q: %w", label, c.Match, err)
	}
	return p, nil
}

func customLabel(i int) string { return fmt.Sprintf("intents.custom[%d]", i) }

// normalize reduces an utterance to comparable words: lower case, no
// punctuation, single spaces. STT output varies in capitalisation and
// trailing punctuation for the same spoken phrase, so the table would be
// unusable without it. Nothing else is rewritten — no stemming, no synonym
// folding, no edit distance.
func normalize(text string) []string {
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range strings.ToLower(text) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '\'', r == '’':
			// Apostrophes vanish rather than split: "don't" and "dont" are the
			// same spoken word, and STT punctuates inconsistently.
		default:
			// Hyphens ("twenty-five") and all other punctuation are word
			// boundaries.
			b.WriteByte(' ')
		}
	}
	return strings.Fields(b.String())
}
