package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/rpickz/jarvix/internal/config"
)

// hyprBind is the subset of `hyprctl binds -j` output the conflict check
// needs. A chord is (modmask, key, release-phase); two binds on the same
// chord fire together, so a Jarvix chord shared with anything else means one
// of them is broken — usually an Omarchy default the user still expects.
type hyprBind struct {
	Modmask     int    `json:"modmask"`
	Key         string `json:"key"`
	Release     bool   `json:"release"`
	Description string `json:"description"`
	Arg         string `json:"arg"`
}

func (b hyprBind) isJarvix() bool {
	return strings.Contains(b.Description, "Jarvix") || strings.Contains(b.Arg, "jarvix")
}

func (b hyprBind) chord() string {
	return fmt.Sprintf("%d/%s/release=%v", b.Modmask, strings.ToLower(b.Key), b.Release)
}

func (b hyprBind) label() string {
	if b.Description != "" {
		return b.Description
	}
	return b.Arg
}

// findBindConflicts returns one message per Jarvix chord that is shared with
// a non-Jarvix bind, and whether any Jarvix binds exist at all.
func findBindConflicts(bindsJSON []byte) (conflicts []string, installed bool, err error) {
	var binds []hyprBind
	if err := json.Unmarshal(bindsJSON, &binds); err != nil {
		return nil, false, fmt.Errorf("parse hyprctl binds output: %w", err)
	}
	byChord := make(map[string][]hyprBind)
	for _, b := range binds {
		byChord[b.chord()] = append(byChord[b.chord()], b)
	}
	for _, group := range byChord {
		var jarvix, others []string
		for _, b := range group {
			if b.isJarvix() {
				jarvix = append(jarvix, b.label())
			} else {
				others = append(others, b.label())
			}
		}
		if len(jarvix) > 0 {
			installed = true
			if len(others) > 0 {
				conflicts = append(conflicts, fmt.Sprintf(
					"%q shares its key with %q", jarvix[0], strings.Join(others, ", ")))
			}
		}
	}
	sort.Strings(conflicts)
	return conflicts, installed, nil
}

func checkKeybindings(_ config.Config, _ config.Paths) Result {
	if _, err := exec.LookPath("hyprctl"); err != nil {
		return Result{Status: Warn, Name: "keybindings",
			Detail: "hyprctl not found; skipped (not running under Hyprland?)"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "hyprctl", "binds", "-j").Output()
	if err != nil {
		return Result{Status: Warn, Name: "keybindings",
			Detail: "could not query Hyprland binds: " + err.Error()}
	}
	conflicts, installed, err := findBindConflicts(out)
	if err != nil {
		return Result{Status: Warn, Name: "keybindings", Detail: err.Error()}
	}
	if !installed {
		return Result{Status: Warn, Name: "keybindings installed",
			Detail: "no Jarvix bindings found in Hyprland",
			Fix:    "Install them: make install-hyprland (the CLI works without them)"}
	}
	if len(conflicts) > 0 {
		return Result{Status: Fail, Name: "keybindings conflict-free",
			Detail: strings.Join(conflicts, "; "),
			Fix: "Another binding claimed a Jarvix chord (an update, or a manual bind).\n" +
				"Rebind Jarvix in the managed block of ~/.config/hypr/bindings.lua,\n" +
				"then reload: hyprctl reload"}
	}
	return Result{Status: OK, Name: "keybindings conflict-free"}
}
