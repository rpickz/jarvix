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
