package config

import (
	"os"
	"path/filepath"
)

// Paths resolves the XDG base directories Jarvix uses. All Jarvix files live
// under these roots; nothing is scattered through the home directory.
type Paths struct {
	Config  string // $XDG_CONFIG_HOME/jarvix
	Data    string // $XDG_DATA_HOME/jarvix
	State   string // $XDG_STATE_HOME/jarvix
	Runtime string // $XDG_RUNTIME_DIR/jarvix
	Socket  string // $XDG_RUNTIME_DIR/jarvix.sock
}

// DefaultPaths resolves XDG directories from the environment, falling back to
// the standard defaults when the variables are unset.
func DefaultPaths() Paths {
	home, _ := os.UserHomeDir()
	xdg := func(env, fallback string) string {
		if v := os.Getenv(env); v != "" {
			return filepath.Join(v, "jarvix")
		}
		return filepath.Join(home, fallback, "jarvix")
	}
	runtime := os.Getenv("XDG_RUNTIME_DIR")
	if runtime == "" {
		// No runtime dir (e.g. bare TTY session); degrade to state dir.
		runtime = filepath.Join(home, ".local", "state")
	}
	return Paths{
		Config:  xdg("XDG_CONFIG_HOME", ".config"),
		Data:    xdg("XDG_DATA_HOME", filepath.Join(".local", "share")),
		State:   xdg("XDG_STATE_HOME", filepath.Join(".local", "state")),
		Runtime: filepath.Join(runtime, "jarvix"),
		Socket:  filepath.Join(runtime, "jarvix.sock"),
	}
}

// ConfigFile returns the path of the primary configuration file.
func (p Paths) ConfigFile() string { return filepath.Join(p.Config, "config.toml") }

// WhisperModelDir returns where Whisper models are stored.
func (p Paths) WhisperModelDir() string { return filepath.Join(p.Data, "models", "whisper") }

// HistoryFile returns where conversation history is persisted across daemon
// restarts. State, not data: it is machine-local operational memory the user
// may delete at will (jarvix new).
func (p Paths) HistoryFile() string { return filepath.Join(p.State, "history.json") }

// MemoryFile returns where the knowledge base lives (ADR 0025). State, like
// history, because it is machine-local and the user may delete it at will —
// but unlike history it is a file the user is invited to open and edit.
func (p Paths) MemoryFile() string { return filepath.Join(p.State, "memory.toml") }

// VocabularyFile returns where the taught vocabulary lives (issue #129).
// State, on the memory file's exact terms: machine-local, deletable at will,
// and a file the user is invited to open and edit.
func (p Paths) VocabularyFile() string { return filepath.Join(p.State, "vocabulary.toml") }

// FeedsFile returns where cached feed values live (ADR 0031). State, like
// history: it is a machine-written cache the user may delete at will — the
// next fetch simply rebuilds it — but it may hold sensitive values, so it is
// written 0600 like everything else here.
func (p Paths) FeedsFile() string { return filepath.Join(p.State, "feeds.toml") }

// AutomationsFile returns where the schedule trail lives (ADR 0032). State,
// like the feed cache: a machine-written record — when each scheduled routine
// or script last fired — the user may delete at will; the only cost is one
// boot's missed-while-down report.
func (p Paths) AutomationsFile() string { return filepath.Join(p.State, "automations.toml") }

// FocusFile is the focus-thread store (#123, ADR 0041): threads, anchors,
// parked thoughts, and the live timebox, hand-editable like the memory store
// it sits beside.
func (p Paths) FocusFile() string { return filepath.Join(p.State, "focus.toml") }

// RemindersFile is the one-shot reminder store (#141, ADR 0046): pending
// reminders and the capped fired history, hand-editable like the focus
// store it sits beside — deliberately state, never config.toml, so creating
// one by voice needs no config-write ceremony.
func (p Paths) RemindersFile() string { return filepath.Join(p.State, "reminders.toml") }

// ApprovalsFile is the approval ledger (#162, ADR 0052): when each
// `[tools.policy] shell_allow` pattern was agreed to on a confirmation card,
// and how often it has since let a command run unprompted.
//
// State rather than config, and the line is sharper here than anywhere else:
// the patterns themselves stay in config.toml, which alone decides what runs
// without asking. This file is the history beside them, machine-written
// several times a minute, and deleting it costs the user their record of when
// they agreed to something — never a permission, in either direction.
func (p Paths) ApprovalsFile() string { return filepath.Join(p.State, "approvals.toml") }

// ConversationsDir returns where archived conversations live (ADR 0027).
// State, like history: transcripts of what was said in the user's home,
// machine-local, and deletable at will (`jarvix conversations delete`).
func (p Paths) ConversationsDir() string { return filepath.Join(p.State, "conversations") }
