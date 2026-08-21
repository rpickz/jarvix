package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultPathsFollowXDGEnvironment(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/x/config")
	t.Setenv("XDG_DATA_HOME", "/x/data")
	t.Setenv("XDG_STATE_HOME", "/x/state")
	t.Setenv("XDG_RUNTIME_DIR", "/x/run")
	p := DefaultPaths()
	if p.Config != filepath.Join("/x/config", "jarvix") {
		t.Errorf("Config = %q", p.Config)
	}
	if p.Data != filepath.Join("/x/data", "jarvix") {
		t.Errorf("Data = %q", p.Data)
	}
	if p.State != filepath.Join("/x/state", "jarvix") {
		t.Errorf("State = %q", p.State)
	}
	if p.Runtime != filepath.Join("/x/run", "jarvix") {
		t.Errorf("Runtime = %q", p.Runtime)
	}
	if p.Socket != filepath.Join("/x/run", "jarvix.sock") {
		t.Errorf("Socket = %q", p.Socket)
	}
	if p.ConfigFile() != filepath.Join(p.Config, "config.toml") {
		t.Errorf("ConfigFile = %q", p.ConfigFile())
	}
	if p.WhisperModelDir() != filepath.Join(p.Data, "models", "whisper") {
		t.Errorf("WhisperModelDir = %q", p.WhisperModelDir())
	}
}

func TestDefaultPathsFallBackWithoutXDG(t *testing.T) {
	for _, env := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR"} {
		t.Setenv(env, "")
	}
	p := DefaultPaths()
	// Everything must live under the home directory, nothing scattered.
	for name, path := range map[string]string{
		"Config": p.Config, "Data": p.Data, "State": p.State, "Runtime": p.Runtime,
	} {
		if !strings.Contains(path, "jarvix") || !filepath.IsAbs(path) {
			t.Errorf("%s = %q, want an absolute jarvix dir", name, path)
		}
	}
	// No runtime dir: the socket degrades to the state directory.
	if !strings.HasSuffix(p.Socket, filepath.Join(".local", "state", "jarvix.sock")) {
		t.Errorf("Socket = %q, want the state-dir fallback", p.Socket)
	}
}
