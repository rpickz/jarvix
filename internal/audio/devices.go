package audio

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// This file answers one question for doctor and anyone else who asks: which
// devices will capture and playback actually bind, right now, by name (issue
// #142)? Jarvix's streams follow the PipeWire defaults unless pinned, so the
// answer is whatever WirePlumber currently calls @DEFAULT_AUDIO_SINK@ and
// @DEFAULT_AUDIO_SOURCE@ — asked of wpctl, the same tool a user would reach
// for, so doctor's line and `wpctl status` can never disagree.

// Device identifies one PipeWire node two ways: Name is the node.name — the
// exact string an audio.input_device / audio.output_device pin would use —
// and Description is the human label `wpctl status` prints.
type Device struct {
	Name        string
	Description string
}

// DefaultSink reports the node playback binds when no output pin is set.
func DefaultSink(ctx context.Context) (Device, error) {
	return defaultNode(ctx, "@DEFAULT_AUDIO_SINK@")
}

// DefaultSource reports the node capture binds when no input pin is set.
func DefaultSource(ctx context.Context) (Device, error) {
	return defaultNode(ctx, "@DEFAULT_AUDIO_SOURCE@")
}

func defaultNode(ctx context.Context, which string) (Device, error) {
	out, err := exec.CommandContext(ctx, "wpctl", "inspect", which).Output()
	if err != nil {
		return Device{}, fmt.Errorf("wpctl inspect %s (is WirePlumber running?): %w", which, err)
	}
	d := Device{
		Name:        inspectProperty(out, "node.name"),
		Description: inspectProperty(out, "node.description"),
	}
	if d.Name == "" {
		return Device{}, fmt.Errorf("wpctl inspect %s reported no node.name", which)
	}
	return d, nil
}

// inspectProperty pulls one property out of `wpctl inspect` output. Lines
// look like `  * node.name = "alsa_output...."` — an optional inherited-mark
// asterisk, the key, and a quoted value.
func inspectProperty(out []byte, key string) string {
	for _, line := range strings.Split(string(out), "\n") {
		trimmed := strings.TrimSpace(line)
		trimmed = strings.TrimPrefix(trimmed, "* ")
		rest, ok := strings.CutPrefix(trimmed, key)
		if !ok {
			continue
		}
		rest = strings.TrimSpace(rest)
		rest, ok = strings.CutPrefix(rest, "=")
		if !ok {
			continue
		}
		return strings.Trim(strings.TrimSpace(rest), `"`)
	}
	return ""
}
