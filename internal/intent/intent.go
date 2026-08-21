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
			if owner, clash := taken[p.key()]; clash {
				return nil, fmt.Errorf("%s: phrase %q is already %s; choose a different phrase",
					label, phrase, owner)
			}
			taken[p.key()] = fmt.Sprintf("the trigger for routine %q", name)
			r.add(&rule{name: "routine.run", pattern: p, routine: name})
		}
		r.names = append(r.names, "routine:"+name)
	}
	return r, nil
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
	slot, hasSlot, ok := ru.pattern.match(fields)
	if !ok {
		return Match{}, false
	}
	m := Match{
		Name: ru.name, Slot: slot, HasSlot: hasSlot, Control: ru.control,
		Desktop: ru.desktop, Program: ru.program,
		Command: ru.command, UserDefined: ru.userDefined,
		Routine: ru.routine,
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
		if t.kind == slotNone {
			n++
			continue
		}
		n += maxSlotWords
	}
	return n
}

func (p pattern) match(fields []string) (slot int, hasSlot, ok bool) {
	ok = p.matchFrom(fields, 0, 0, &slot, &hasSlot)
	return slot, hasSlot, ok
}

// matchFrom walks pattern token pi against field fi. A slot backtracks over
// how many words it takes (shortest first) so "volume thirty five" resolves
// to 35 rather than failing after greedily reading "thirty". The recursion is
// bounded by the pattern length, which is single digits.
func (p pattern) matchFrom(fields []string, pi, fi int, slot *int, hasSlot *bool) bool {
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
		for n := 1; n <= maxSlotWords && fi+n+remaining <= len(fields); n++ {
			v, parsed := parseNumber(fields[fi : fi+n])
			// Out of bounds is a miss, never a clamp: "volume five hundred"
			// belongs to the model, not to a silently truncated command.
			if !parsed || v < t.min || v > t.max {
				continue
			}
			prevSlot, prevHas := *slot, *hasSlot
			*slot, *hasSlot = v, true
			if p.matchFrom(fields, pi+1, fi+n, slot, hasSlot) {
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
			return pattern{}, fmt.Errorf("unknown placeholder %q; the supported placeholders are {volume} and {workspace}", w)
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
