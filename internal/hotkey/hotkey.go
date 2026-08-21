// Package hotkey implements daemon-side push-to-talk: it watches keyboard
// input devices (evdev) for a configured key chord, so hold-to-talk works on
// real key press/release events instead of compositor bindings — Hyprland
// release-binds are unreliable for modifier chords (ADR 0004), and this is
// how established push-to-talk software (Mumble, Discord) solves it.
//
// Privacy: the watcher necessarily receives all key events from the devices
// it opens. Events that are not part of the configured chord are discarded
// immediately in ChordTracker.Handle; no key event is ever stored, buffered,
// or logged (ADR 0008).
package hotkey

import (
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
)

// KeyCodes maps friendly config names to Linux input event codes
// (input-event-codes.h). Names are matched case-insensitively.
var KeyCodes = map[string]uint16{
	"esc": 1,
	"1":   2, "2": 3, "3": 4, "4": 5, "5": 6, "6": 7, "7": 8, "8": 9, "9": 10, "0": 11,
	"q": 16, "w": 17, "e": 18, "r": 19, "t": 20, "y": 21, "u": 22, "i": 23, "o": 24, "p": 25,
	"a": 30, "s": 31, "d": 32, "f": 33, "g": 34, "h": 35, "j": 36, "k": 37, "l": 38,
	"z": 44, "x": 45, "c": 46, "v": 47, "b": 48, "n": 49, "m": 50,
	"leftctrl": 29, "leftshift": 42, "rightshift": 54, "leftalt": 56, "space": 57,
	"f1": 59, "f2": 60, "f3": 61, "f4": 62, "f5": 63, "f6": 64, "f7": 65, "f8": 66,
	"f9": 67, "f10": 68, "f11": 87, "f12": 88,
	"rightctrl": 97, "rightalt": 100,
	"leftmeta": 125, "rightmeta": 126,
	// Aliases matching common shorthand.
	"super": 125, "meta": 125, "alt": 56, "ctrl": 29, "shift": 42,
}

// ResolveChord translates config key names into event codes.
func ResolveChord(names []string) ([]uint16, error) {
	if len(names) == 0 {
		return nil, fmt.Errorf("empty chord")
	}
	seen := map[uint16]bool{}
	var codes []uint16
	for _, name := range names {
		code, ok := KeyCodes[strings.ToLower(strings.TrimSpace(name))]
		if !ok {
			known := make([]string, 0, len(KeyCodes))
			for k := range KeyCodes {
				known = append(known, k)
			}
			sort.Strings(known)
			return nil, fmt.Errorf("unknown key %q in activation.ptt_chord; known keys: %s",
				name, strings.Join(known, ", "))
		}
		if !seen[code] {
			seen[code] = true
			codes = append(codes, code)
		}
	}
	return codes, nil
}

// Linux input_event constants.
const (
	evKey = 1 // event type EV_KEY

	valRelease = 0
	valPress   = 1
	valRepeat  = 2

	// eventSize is sizeof(struct input_event) on 64-bit Linux:
	// two 8-byte timeval fields + type(2) + code(2) + value(4).
	eventSize = 24
)

// keyEvent is one decoded EV_KEY event.
type keyEvent struct {
	code  uint16
	value int32
}

// decodeEvents parses raw input_event bytes, keeping only EV_KEY events.
func decodeEvents(buf []byte) []keyEvent {
	var out []keyEvent
	for off := 0; off+eventSize <= len(buf); off += eventSize {
		typ := binary.LittleEndian.Uint16(buf[off+16 : off+18])
		if typ != evKey {
			continue
		}
		out = append(out, keyEvent{
			code:  binary.LittleEndian.Uint16(buf[off+18 : off+20]),
			value: int32(binary.LittleEndian.Uint32(buf[off+20 : off+24])),
		})
	}
	return out
}

// ChordTracker turns a stream of key events (possibly from several keyboards)
// into chord press/release notifications. It is not safe for concurrent use;
// the Watcher serialises events from all devices into one goroutine.
type ChordTracker struct {
	chord   map[uint16]bool // codes composing the chord
	pressed map[uint16]bool // chord keys currently down
	active  bool            // chord fired, waiting for a release

	// OnPress fires once when every chord key is down. OnRelease fires once
	// when any chord key is released after OnPress.
	OnPress   func()
	OnRelease func()
}

// NewChordTracker builds a tracker for the given key codes.
func NewChordTracker(codes []uint16) *ChordTracker {
	chord := make(map[uint16]bool, len(codes))
	for _, c := range codes {
		chord[c] = true
	}
	return &ChordTracker{chord: chord, pressed: make(map[uint16]bool, len(codes))}
}

// Handle consumes one key event. Events for keys outside the chord are
// discarded immediately — this is the privacy boundary.
func (t *ChordTracker) Handle(ev keyEvent) {
	if !t.chord[ev.code] {
		return
	}
	switch ev.value {
	case valPress:
		t.pressed[ev.code] = true
		if !t.active && len(t.pressed) == len(t.chord) {
			t.active = true
			if t.OnPress != nil {
				t.OnPress()
			}
		}
	case valRelease:
		delete(t.pressed, ev.code)
		if t.active {
			t.active = false
			if t.OnRelease != nil {
				t.OnRelease()
			}
		}
	case valRepeat:
		// Key repeat carries no chord-state information.
	}
}
