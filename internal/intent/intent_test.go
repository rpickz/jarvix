package intent

import (
	"strconv"
	"strings"
	"testing"
)

func newRouter(t *testing.T, custom ...Custom) *Router {
	t.Helper()
	r, err := New(Options{Custom: custom})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

// TestBuiltinHits is the shipped grammar, phrase by phrase: what must match,
// which intent it becomes, and the exact argv it runs. The argv assertions
// are the security test — they prove the transcript contributes nothing but a
// parsed integer.
func TestBuiltinHits(t *testing.T) {
	tests := []struct {
		utterance string
		intent    string
		slot      int
		hasSlot   bool
		argv      []string
		ack       string
		control   Control
		desktop   Desktop
		program   string
	}{
		{
			utterance: "volume thirty", intent: "volume.set", slot: 30, hasSlot: true,
			argv: []string{"wpctl", "set-volume", "-l", "1.5", "@DEFAULT_AUDIO_SINK@", "30%"},
			ack:  "Volume thirty",
		},
		{
			// The same utterance transcribed as digits must be the same intent.
			utterance: "volume 30", intent: "volume.set", slot: 30, hasSlot: true,
			argv: []string{"wpctl", "set-volume", "-l", "1.5", "@DEFAULT_AUDIO_SINK@", "30%"},
			ack:  "Volume thirty",
		},
		{
			utterance: "Volume 30.", intent: "volume.set", slot: 30, hasSlot: true,
			ack: "Volume thirty",
		},
		{utterance: "set the volume to fifty five", intent: "volume.set", slot: 55, hasSlot: true, ack: "Volume fifty-five"},
		{utterance: "set volume to 0", intent: "volume.set", slot: 0, hasSlot: true, ack: "Volume zero"},
		{utterance: "volume one hundred and fifty", intent: "volume.set", slot: 150, hasSlot: true},
		{utterance: "volume a hundred", intent: "volume.set", slot: 100, hasSlot: true},
		{utterance: "volume twenty percent", intent: "volume.set", slot: 20, hasSlot: true},
		{
			utterance: "volume up", intent: "volume.up",
			argv: []string{"wpctl", "set-volume", "-l", "1.5", "@DEFAULT_AUDIO_SINK@", "5%+"},
			ack:  "Volume up",
		},
		{utterance: "louder", intent: "volume.up", ack: "Volume up"},
		{utterance: "turn it up", intent: "volume.up"},
		{
			utterance: "turn it down", intent: "volume.down",
			argv: []string{"wpctl", "set-volume", "@DEFAULT_AUDIO_SINK@", "5%-"},
			ack:  "Volume down",
		},
		{
			utterance: "mute", intent: "volume.mute",
			argv: []string{"wpctl", "set-mute", "@DEFAULT_AUDIO_SINK@", "1"}, ack: "Muted",
		},
		{
			utterance: "unmute the sound", intent: "volume.unmute",
			argv: []string{"wpctl", "set-mute", "@DEFAULT_AUDIO_SINK@", "0"}, ack: "Unmuted",
		},
		{utterance: "stop", intent: "speech.stop", control: ControlStopSpeech, ack: ""},
		{utterance: "stop talking", intent: "speech.stop", control: ControlStopSpeech, ack: ""},
		{utterance: "Shut up!", intent: "speech.stop", control: ControlStopSpeech},
		{
			utterance: "new conversation", intent: "conversation.new",
			control: ControlNewConversation, ack: "New conversation.",
		},
		{utterance: "start over", intent: "conversation.new", control: ControlNewConversation},
		// The two compositor intents carry an action, never an argv: how a
		// dispatch is written depends on the dialect the desktop seam probed
		// for, and a table that guessed it was issue #47.
		{
			utterance: "workspace 4", intent: "workspace.switch", slot: 4, hasSlot: true,
			desktop: DesktopWorkspace, ack: "Workspace four",
		},
		{
			utterance: "go to workspace ten", intent: "workspace.switch", slot: 10, hasSlot: true,
			desktop: DesktopWorkspace, ack: "Workspace ten",
		},
		{
			utterance: "open terminal", intent: "terminal.open",
			desktop: DesktopSpawn, program: DefaultTerminal, ack: "Terminal.",
		},
		{utterance: "open a terminal", intent: "terminal.open", desktop: DesktopSpawn, program: DefaultTerminal},
	}

	r := newRouter(t)
	for _, tc := range tests {
		t.Run(tc.utterance, func(t *testing.T) {
			m, ok := r.Match(tc.utterance)
			if !ok {
				t.Fatalf("%q did not match any intent", tc.utterance)
			}
			if m.Name != tc.intent {
				t.Errorf("intent = %q, want %q", m.Name, tc.intent)
			}
			if m.HasSlot != tc.hasSlot || m.Slot != tc.slot {
				t.Errorf("slot = %d (has %v), want %d (has %v)", m.Slot, m.HasSlot, tc.slot, tc.hasSlot)
			}
			if m.Control != tc.control {
				t.Errorf("control = %q, want %q", m.Control, tc.control)
			}
			if tc.argv != nil && strings.Join(m.Argv, " ") != strings.Join(tc.argv, " ") {
				t.Errorf("argv = %v, want %v", m.Argv, tc.argv)
			}
			if m.Desktop != tc.desktop || m.Program != tc.program {
				t.Errorf("desktop action = %q/%q, want %q/%q", m.Desktop, m.Program, tc.desktop, tc.program)
			}
			// A compositor intent must not also carry a command line: two
			// ways to perform one action is how the dialects diverged.
			if tc.desktop != DesktopNone && len(m.Argv) > 0 {
				t.Errorf("compositor intent also built an argv: %v", m.Argv)
			}
			if tc.ack != "" && m.Ack != tc.ack {
				t.Errorf("ack = %q, want %q", m.Ack, tc.ack)
			}
			if tc.control == ControlStopSpeech && m.Ack != "" {
				t.Errorf("stop must acknowledge with silence, got %q", m.Ack)
			}
			if m.UserDefined {
				t.Error("built-in intent reported as user-defined")
			}
			if m.Command != "" {
				t.Errorf("built-in intent carries a shell command %q", m.Command)
			}
		})
	}
}

// TestNearMissesFallThrough is the router's most important test: everything
// here is *nearly* an intent and must reach the model instead. A fuzzy
// matcher would claim most of these.
func TestNearMissesFallThrough(t *testing.T) {
	misses := []string{
		"turn it up a bit",                       // the canonical near-miss
		"turn it up a little",                    //
		"can you turn it up",                     // polite framing is a question
		"volume",                                 // no slot
		"volume please",                          //
		"set the volume",                         //
		"what is the volume",                     //
		"volume thirty please",                   // trailing words are not ignored
		"please mute",                            // leading words are not ignored
		"mute the microphone",                    // a different device entirely
		"stop the docker container",              // "stop" is not a prefix match
		"why did you stop",                       //
		"tell me about workspace 4",              //
		"open a terminal and run the build",      //
		"start a new conversation about Haskell", //
		"how loud is it",                         //
		"louder than that",                       //
		"a bit louder",                           // the follow-up that needs the model
		"a bit quieter",                          //
		"",                                       //
		"   ",                                    //
		"unmuted",                                // not a word in the table
		"volume up down",                         //
	}
	r := newRouter(t)
	for _, u := range misses {
		t.Run(u, func(t *testing.T) {
			if m, ok := r.Match(u); ok {
				t.Errorf("%q was claimed by intent %q; it must reach the model", u, m.Name)
			}
		})
	}
}

// TestSlotBounds proves an out-of-range value is a miss, not a clamp: the
// model gets the utterance, and no command is built from a number the table
// does not accept.
func TestSlotBounds(t *testing.T) {
	tests := []struct {
		utterance string
		want      int
		match     bool
	}{
		{"volume 0", 0, true},
		{"volume zero", 0, true},
		{"volume 150", 150, true},
		{"volume one hundred and fifty", 150, true},
		{"volume 151", 0, false},
		{"volume 200", 0, false},
		{"volume 1000", 0, false},
		{"volume nine hundred", 0, false},
		{"workspace 1", 1, true},
		{"workspace 10", 10, true},
		{"workspace 0", 0, false},
		{"workspace 11", 0, false},
		{"workspace eleven", 0, false},
		{"workspace one hundred", 0, false},
	}
	r := newRouter(t)
	for _, tc := range tests {
		t.Run(tc.utterance, func(t *testing.T) {
			m, ok := r.Match(tc.utterance)
			if ok != tc.match {
				t.Fatalf("match = %v, want %v (slot %d)", ok, tc.match, m.Slot)
			}
			if ok && m.Slot != tc.want {
				t.Errorf("slot = %d, want %d", m.Slot, tc.want)
			}
		})
	}
}

// TestNumberFormsAgree proves the digit and word spellings of the same value
// produce the identical command — the "locale-ish variants" requirement.
func TestNumberFormsAgree(t *testing.T) {
	r := newRouter(t)
	for n := 0; n <= 150; n++ {
		digits, okD := r.Match("volume " + strconv.Itoa(n))
		words, okW := r.Match("volume " + SpokenNumber(n))
		if !okD || !okW {
			t.Fatalf("n=%d: digits matched %v, words matched %v", n, okD, okW)
		}
		if digits.Slot != n || words.Slot != n {
			t.Fatalf("n=%d: digits slot %d, words slot %d", n, digits.Slot, words.Slot)
		}
		if strings.Join(digits.Argv, " ") != strings.Join(words.Argv, " ") {
			t.Fatalf("n=%d: %v != %v", n, digits.Argv, words.Argv)
		}
	}
}

func TestCustomIntents(t *testing.T) {
	r := newRouter(t,
		Custom{Match: "lock the screen", Run: "hyprlock", Say: "Locking."},
		Custom{Match: "Good  Night", Run: "systemctl suspend"},
	)
	m, ok := r.Match("Lock the screen.")
	if !ok {
		t.Fatal("user-defined intent did not match")
	}
	if !m.UserDefined {
		t.Error("user-defined intent not flagged")
	}
	if m.Command != "hyprlock" {
		t.Errorf("command = %q", m.Command)
	}
	if len(m.Argv) != 0 {
		t.Errorf("user-defined intent must carry no argv, got %v", m.Argv)
	}
	if m.Ack != "Locking." {
		t.Errorf("ack = %q", m.Ack)
	}

	// Whitespace and case in the pattern are normalized, and an entry with no
	// `say` gets a generic acknowledgement.
	m, ok = r.Match("good night")
	if !ok {
		t.Fatal("normalized user pattern did not match")
	}
	if m.Ack != "Done." {
		t.Errorf("default ack = %q", m.Ack)
	}
	if _, ok := r.Match("good night everyone"); ok {
		t.Error("user-defined patterns must match whole utterances")
	}
}

func TestCustomIntentValidation(t *testing.T) {
	tests := []struct {
		name    string
		custom  Custom
		wantSub []string
	}{
		{"empty match", Custom{Run: "hyprlock"}, []string{"intents.custom[0]", "match is empty"}},
		{"empty run", Custom{Match: "lock the screen"}, []string{"intents.custom[0]", "lock the screen", "no run command"}},
		{"placeholder", Custom{Match: "volume {volume}", Run: "wpctl"}, []string{"intents.custom[0]", "placeholder"}},
		{"unknown placeholder", Custom{Match: "set {level}", Run: "x"}, []string{"intents.custom[0]", "placeholder"}},
		{"punctuation only", Custom{Match: "!!!", Run: "x"}, []string{"intents.custom[0]", "not a plain spoken word"}},
		{"shadows a built-in", Custom{Match: "Mute", Run: "x"}, []string{"intents.custom[0]", "built-in intent", "volume.mute"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCustom([]Custom{tc.custom})
			if err == nil {
				t.Fatal("expected a validation error")
			}
			for _, sub := range tc.wantSub {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("error %q does not mention %q", err, sub)
				}
			}
		})
	}
	if err := ValidateCustom([]Custom{{Match: "lock the screen", Run: "hyprlock"}}); err != nil {
		t.Errorf("valid entry rejected: %v", err)
	}
}

func TestCustomIntentIndexIsNamed(t *testing.T) {
	err := ValidateCustom([]Custom{
		{Match: "lock the screen", Run: "hyprlock"},
		{Match: "sleep now", Run: ""},
	})
	if err == nil || !strings.Contains(err.Error(), "intents.custom[1]") {
		t.Fatalf("error must name the second entry, got %v", err)
	}
}

func TestTerminalValidation(t *testing.T) {
	if _, err := New(Options{Terminal: "ghostty"}); err != nil {
		t.Errorf("plain binary name rejected: %v", err)
	}
	if _, err := New(Options{Terminal: "/usr/bin/foot"}); err != nil {
		t.Errorf("absolute path rejected: %v", err)
	}
	for _, bad := range []string{"alacritty; rm -rf ~", "alacritty --title x", "$(evil)", "a`b`"} {
		if _, err := New(Options{Terminal: bad}); err == nil {
			t.Errorf("terminal %q should be rejected", bad)
		}
	}
	// An empty terminal falls back to the default rather than failing.
	r, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	m, _ := r.Match("open terminal")
	if m.Program != DefaultTerminal {
		t.Errorf("program = %q, want the default terminal", m.Program)
	}
}

// TestTerminalIsOneProgram is the security half of "open a terminal": the
// configured value reaches the compositor seam as one program name and
// nothing else, so it cannot become arguments, a command line, or — now that
// the Lua dialect renders it into `hl.dsp.exec_cmd("…")` — Lua syntax.
func TestTerminalIsOneProgram(t *testing.T) {
	r, err := New(Options{Terminal: "/usr/bin/foot"})
	if err != nil {
		t.Fatal(err)
	}
	m, ok := r.Match("open the terminal")
	if !ok {
		t.Fatal("no match")
	}
	if m.Program != "/usr/bin/foot" || m.Desktop != DesktopSpawn {
		t.Errorf("match = %q/%q, want a spawn of /usr/bin/foot", m.Desktop, m.Program)
	}
	if len(m.Argv) != 0 || m.Command != "" {
		t.Errorf("terminal intent built a command line: %v %q", m.Argv, m.Command)
	}
}

func TestNilRouterMatchesNothing(t *testing.T) {
	var r *Router
	if _, ok := r.Match("mute"); ok {
		t.Error("a nil router must claim nothing")
	}
}

func TestBuiltinBinaries(t *testing.T) {
	bins := BuiltinBinaries("")
	joined := strings.Join(bins, " ")
	for _, want := range []string{"wpctl", "hyprctl", DefaultTerminal} {
		if !strings.Contains(joined, want) {
			t.Errorf("doctor would not check for %q (got %v)", want, bins)
		}
	}
	if bins := BuiltinBinaries("foot"); bins[len(bins)-1] != "foot" {
		t.Errorf("configured terminal not checked: %v", bins)
	}
}

// BenchmarkMatchMiss is the budget that matters: the miss path is on every
// question the user ever asks, and must be invisible next to a model call.
func BenchmarkMatchMiss(b *testing.B) {
	r, err := New(Options{})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, ok := r.Match("why is my docker build failing on the second layer"); ok {
			b.Fatal("unexpected match")
		}
	}
}

// BenchmarkMatchNearMiss measures the worst case: an utterance that reaches
// the pattern list for its first word and then fails every one of them.
func BenchmarkMatchNearMiss(b *testing.B) {
	r, err := New(Options{})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, ok := r.Match("volume is too low on this machine"); ok {
			b.Fatal("unexpected match")
		}
	}
}

func BenchmarkMatchHit(b *testing.B) {
	r, err := New(Options{})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, ok := r.Match("volume one hundred and fifty"); !ok {
			b.Fatal("expected a match")
		}
	}
}

// TestRoutinePhraseRoutesToTheRoutine covers the routing half of ADR 0025:
// a routine phrase is claimed deterministically and carries nothing but the
// routine's name — no argv, no command, nothing a runner could mistake for
// an instruction.
func TestRoutinePhraseRoutesToTheRoutine(t *testing.T) {
	r, err := New(Options{Routines: []RoutinePhrases{
		{Name: "morning setup", Phrases: []string{"morning setup", "start my usual apps"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, utterance := range []string{"morning setup", "Start my usual apps.", "MORNING SETUP"} {
		m, ok := r.Match(utterance)
		if !ok {
			t.Fatalf("%q did not match", utterance)
		}
		if m.Routine != "morning setup" {
			t.Errorf("%q → routine %q", utterance, m.Routine)
		}
		if m.Name != "routine.run" {
			t.Errorf("%q → name %q", utterance, m.Name)
		}
		if len(m.Argv) != 0 || m.Command != "" || m.Program != "" || m.UserDefined {
			t.Errorf("%q carries an executable payload: %+v", utterance, m)
		}
		if m.Ack != "" {
			t.Errorf("%q carries an immediate ack %q; the run's summary is the acknowledgement", utterance, m.Ack)
		}
	}
	if _, ok := r.Match("morning setup please"); ok {
		t.Error("a longer sentence matched; routine phrases are whole-utterance like every other intent")
	}
}

// TestRoutinePhraseCollisionsFailCompilation: a phrase that already belongs to
// a built-in, a custom intent, or another routine is a load-time error naming
// both owners — never a silent coin toss at match time.
func TestRoutinePhraseCollisionsFailCompilation(t *testing.T) {
	tests := []struct {
		name string
		opts Options
		want string
	}{
		{"builtin", Options{Routines: []RoutinePhrases{
			{Name: "quiet", Phrases: []string{"mute"}},
		}}, `the built-in intent "volume.mute"`},
		{"custom", Options{
			Custom: []Custom{{Match: "lock it down", Run: "loginctl lock-session"}},
			Routines: []RoutinePhrases{
				{Name: "lockdown", Phrases: []string{"lock it down"}},
			}}, `intents.custom[0]`},
		{"another routine", Options{Routines: []RoutinePhrases{
			{Name: "morning", Phrases: []string{"set up my desk"}},
			{Name: "evening", Phrases: []string{"set up my desk"}},
		}}, `the trigger for routine "morning"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.opts)
			if err == nil {
				t.Fatal("compiled despite the collision")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not name %s", err, tt.want)
			}
		})
	}
}

// TestRoutinePhraseValidation: the malformed shapes fail with messages that
// name the offending routine.
func TestRoutinePhraseValidation(t *testing.T) {
	tests := []struct {
		name string
		opts Options
		want string
	}{
		{"empty name", Options{Routines: []RoutinePhrases{{Phrases: []string{"go"}}}},
			"routines[0]: name is empty"},
		{"no phrases", Options{Routines: []RoutinePhrases{{Name: "morning setup"}}},
			`routines[0] ("morning setup"): it has no phrases`},
		{"placeholder", Options{Routines: []RoutinePhrases{
			{Name: "morning setup", Phrases: []string{"setup workspace {workspace}"}}}},
			"contains a placeholder"},
		{"empty phrase", Options{Routines: []RoutinePhrases{
			{Name: "morning setup", Phrases: []string{"  "}}}},
			"pattern is empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.opts)
			if err == nil {
				t.Fatal("compiled despite the problem")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
		})
	}
}
