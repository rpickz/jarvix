package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/rpickz/jarvix/internal/config"
	"github.com/rpickz/jarvix/internal/ipc"
)

// The `jarvix config get|set|reload` subcommands are thin clients of the
// daemon's config.* IPC methods — the same surface the settings screen uses —
// so every settings operation is scriptable and testable without a GUI.
// Plain `jarvix config` (the offline effective-config dump) stays in
// commands.go and needs no daemon.

// configGetResult mirrors the config.get IPC result.
type configGetResult struct {
	Path        string        `json:"path"`
	Fingerprint string        `json:"fingerprint"`
	Fields      []configField `json:"fields"`
	Secrets     []struct {
		Endpoint  string `json:"endpoint"`
		Env       string `json:"env"`
		EnvSet    bool   `json:"env_set"`
		InlineKey bool   `json:"inline_key"`
	} `json:"secrets"`
}

type configField struct {
	Key    string   `json:"key"`
	Label  string   `json:"label"`
	Type   string   `json:"type"`
	Reload string   `json:"reload"`
	Value  any      `json:"value"`
	Enum   []string `json:"enum"`
}

func fetchConfig(client *ipc.Client) (configGetResult, error) {
	var res configGetResult
	err := client.Call("config.get", nil, &res)
	return res, err
}

func cmdConfigGet(paths config.Paths, key string) error {
	client, err := ipc.Dial(paths.Socket)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	res, err := fetchConfig(client)
	if err != nil {
		return err
	}

	if key != "" {
		for _, f := range res.Fields {
			if f.Key == key {
				fmt.Println(displayValue(f.Value))
				return nil
			}
		}
		return fmt.Errorf("unknown setting %q (jarvix config get lists them all)", key)
	}

	fmt.Println("# Running configuration (from jarvixd; secrets shown as presence only)")
	fmt.Println("# File:", res.Path)
	for _, f := range res.Fields {
		line := fmt.Sprintf("%-34s = %-24s (%s", f.Key, displayValue(f.Value), f.Reload)
		if len(f.Enum) > 0 {
			line += "; one of: " + strings.Join(f.Enum, ", ")
		}
		fmt.Println(line + ")")
	}
	fmt.Println()
	fmt.Println("# API keys (values never shown)")
	for _, s := range res.Secrets {
		switch {
		case s.Env != "":
			state := "not set"
			if s.EnvSet {
				state = "set"
			}
			fmt.Printf("%-34s : %s (%s)\n", s.Env, state, s.Endpoint)
		case s.InlineKey:
			fmt.Printf("%-34s : inline key in config.toml (prefer api_key_env)\n", s.Endpoint)
		}
	}
	return nil
}

func cmdConfigSet(paths config.Paths, pairs []string) error {
	changes := make(map[string]any, len(pairs))
	for _, pair := range pairs {
		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			return fmt.Errorf("expected key=value, got %q", pair)
		}
		changes[strings.TrimSpace(key)] = value
	}

	client, err := ipc.Dial(paths.Socket)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	// The fingerprint from config.get makes the set refuse to clobber a file
	// that changes between read and write (an editor save racing this call).
	current, err := fetchConfig(client)
	if err != nil {
		return err
	}
	var res struct {
		Fingerprint  string   `json:"fingerprint"`
		Applied      bool     `json:"applied"`
		Reason       string   `json:"reason"`
		NeedsRestart []string `json:"needs_restart"`
	}
	err = client.Call("config.set", map[string]any{
		"changes":     changes,
		"fingerprint": current.Fingerprint,
	}, &res)
	if err != nil {
		return configCallError(err)
	}
	fmt.Println("saved to", current.Path)
	reportApplied(res.Applied, res.Reason, res.NeedsRestart)
	return nil
}

func cmdConfigReload(paths config.Paths) error {
	client, err := ipc.Dial(paths.Socket)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	var res struct {
		NeedsRestart []string `json:"needs_restart"`
	}
	if err := client.Call("config.reload", nil, &res); err != nil {
		return configCallError(err)
	}
	reportApplied(true, "", res.NeedsRestart)
	return nil
}

// reportApplied narrates a set/reload outcome: applied live, waiting for the
// engine to go idle, or needing a daemon restart for specific keys.
func reportApplied(applied bool, reason string, needsRestart []string) {
	if applied {
		fmt.Println("applied to the running daemon")
	} else {
		fmt.Println("not applied yet:", reason)
		fmt.Println("retry with: jarvix config reload")
	}
	if len(needsRestart) > 0 {
		fmt.Println("needs a daemon restart to take effect:", strings.Join(needsRestart, ", "))
		fmt.Println("restart with: systemctl --user restart jarvixd")
	}
}

// configCallError turns the structured config.* errors into readable output:
// per-field validation problems as a list, conflicts as reload guidance.
func configCallError(err error) error {
	var rpcErr *ipc.Error
	if !errors.As(err, &rpcErr) {
		return err
	}
	switch rpcErr.Code {
	case ipc.CodeConfigInvalid:
		msg := rpcErr.Message
		if data, ok := rpcErr.Data.(map[string]any); ok {
			if problems, ok := data["problems"].([]any); ok {
				for _, p := range problems {
					msg += fmt.Sprintf("\n  - %v", p)
				}
			}
		}
		return errors.New(msg)
	case ipc.CodeConfigConflict:
		return errors.New(rpcErr.Message + "\nreview with: jarvix config get, then run the set again")
	case ipc.CodeConfigBusy:
		return errors.New(rpcErr.Message)
	}
	return err
}

// displayValue renders a field value for the terminal.
func displayValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			parts = append(parts, fmt.Sprintf("%v", e))
		}
		return strings.Join(parts, ",")
	case map[string]any:
		// Rendered in the same key=value,key=value form `config set` accepts,
		// sorted so the output is stable between runs.
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%v", k, t[k]))
		}
		return strings.Join(parts, ",")
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	}
	return fmt.Sprintf("%v", v)
}
