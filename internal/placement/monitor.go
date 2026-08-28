package placement

import (
	"fmt"
	"sort"
	"strings"
)

// This file is the monitor half of the vocabulary: how a placement names a
// screen, and how that name becomes a real output with a usable area to
// resolve percentages against.
//
// It is deliberately a seam rather than a lookup. A monitor is named by its
// connector (`DP-2`), as "current", or by a nickname the user chose — "top",
// "bottom" — which resolves at run time so a routine survives a cable moving
// (#180, ADR 0057). That issue extended Resolver.Nicknames and nothing else:
// every consumer already went through MonitorRef and Resolver, so the
// nickname arrived everywhere at once. Where the nicknames are KEPT is still
// not here, on purpose — internal/monitors owns the store, this file owns the
// vocabulary, and a lookup table compiled into the vocabulary could not be
// re-read when the user edits it.

// MonitorRef is how a placement names a screen. Three forms, one seam:
//
//   - a connector name as the compositor reports it (`HDMI-A-1`, `DP-2`):
//     exact, and brittle in exactly the way a cable swap is brittle;
//   - MonitorCurrent, the monitor holding focus right now;
//   - a nickname the user chose (#180), which is anything else.
//
// The empty ref means the placement said nothing about a screen, which is a
// legitimate answer: the workspace stays wherever the compositor already has
// it, and percentages resolve against that monitor.
type MonitorRef string

// MonitorCurrent is the reserved word for "the monitor holding focus". It is
// reserved against nicknames for the obvious reason: a nickname that shadowed
// it would make every routine using it mean something else.
const MonitorCurrent MonitorRef = "current"

// reservedMonitorWords are the refs a nickname may never take (#180's
// collision discipline, declared here because the vocabulary owns the words
// and the nickname store will have to ask), each mapped to what owns it.
//
// The owner text lives beside the word rather than in the store, for the
// reason the window matcher supplies its own reserved vocabulary (ADR 0040):
// a refusal that names the owner is only useful if the name is right, and two
// copies of "what does `current` mean" would eventually disagree.
//
// "primary" is reserved although nothing resolves it yet: it is the word a
// user is most likely to reach for next, and taking it back later would break
// the routines written meanwhile.
var reservedMonitorWords = map[string]string{
	string(MonitorCurrent): "it is the screen you are on",
	"primary":              "it is kept for the main screen",
}

// ReservedMonitorWords returns the words a monitor nickname may not use,
// sorted so a sentence listing them reads the same way twice.
func ReservedMonitorWords() []string {
	out := make([]string, 0, len(reservedMonitorWords))
	for word := range reservedMonitorWords {
		out = append(out, word)
	}
	sort.Strings(out)
	return out
}

// ReservedMonitorWord reports whether a name is reserved, and what owns it.
// The comparison folds case for the reason Kind does: the user says the word,
// and how it was capitalised on the way in is not a distinction they made.
func ReservedMonitorWord(name string) (owner string, taken bool) {
	owner, taken = reservedMonitorWords[strings.ToLower(strings.TrimSpace(name))]
	return owner, taken
}

// RefKind is which of the three forms a ref is.
type RefKind int

const (
	// RefNone is the empty ref: no screen was named.
	RefNone RefKind = iota
	// RefCurrent is MonitorCurrent.
	RefCurrent
	// RefConnector is a name shaped like a compositor connector.
	RefConnector
	// RefNickname is anything else — a name the user chose (#180).
	RefNickname
)

// connectorish reports whether a ref is spelled the way compositors spell
// outputs: an all-caps family, a dash, and an index (`DP-2`, `HDMI-A-1`,
// `eDP-1`). The test is a shape rather than a list because the families are
// the kernel's, not ours, and a new one must not have to be added here.
//
// It matters only for telling a connector from a nickname when both could be
// meant, and the precedence rule below makes that a display question rather
// than a resolution question: a present output always wins, whatever it looks
// like.
func connectorish(s string) bool {
	i := strings.IndexByte(s, '-')
	if i <= 0 || i == len(s)-1 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
		default:
			return false
		}
	}
	// The tail after the last dash is the output's index on that connector.
	tail := s[strings.LastIndexByte(s, '-')+1:]
	if tail == "" {
		return false
	}
	for _, r := range tail {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Kind classifies a ref.
func (r MonitorRef) Kind() RefKind {
	trimmed := strings.TrimSpace(string(r))
	switch {
	case trimmed == "":
		return RefNone
	case strings.EqualFold(trimmed, string(MonitorCurrent)):
		return RefCurrent
	case connectorish(trimmed):
		return RefConnector
	default:
		return RefNickname
	}
}

// maxRefLength bounds a monitor name before it may be rendered into a
// dispatch. Connector names are short; anything longer is a mistake, and a
// bound means the mistake is a sentence rather than an eight-kilobyte
// argument handed to the compositor.
const maxRefLength = 64

// Problem reports what is wrong with the ref's spelling, or "" when it could
// name something. Whether it names something that is plugged in right now is
// Resolver's question, not this one's — a routine is allowed to mention a
// monitor that is currently unplugged, and must say so at run time rather
// than refusing to load.
//
// It is exported because the nickname store (#180) has to refuse at
// assignment exactly what the vocabulary would refuse at resolution: a name
// that cannot be spelled as a ref could be stored and listed and would then
// never resolve, which is a worse failure than the refusal it replaced.
func (r MonitorRef) Problem() string {
	trimmed := strings.TrimSpace(string(r))
	if trimmed == "" {
		return ""
	}
	if len(trimmed) > maxRefLength {
		return fmt.Sprintf("monitor %q is too long to be a screen name", trimmed)
	}
	for _, ch := range trimmed {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9', ch == '-', ch == '_':
		default:
			return fmt.Sprintf("monitor %q is not a screen name; use a connector name like "+
				"\"DP-2\", or %q for the screen you are on", trimmed, MonitorCurrent)
		}
	}
	return ""
}

// Monitor is one output as the compositor sees it, reduced to what placement
// needs: where it is, how big it is, and how much of it the bars have taken.
// Everything else `hyprctl monitors` reports (modes, colour management, the
// tearing diagnostics) is deliberately not modelled — unread fields are
// fields that cannot go stale.
type Monitor struct {
	// Name is the connector, and the only thing ever dispatched.
	Name string
	// X and Y are the monitor's origin in the global layout, in logical
	// pixels — the same coordinate space a window's position is in.
	X, Y int
	// Width and Height are the mode's pixel size, BEFORE scaling. Scale
	// divides them into the logical size a window is placed in.
	Width, Height int
	// Scale is the output's scale factor; 0 or 1 means unscaled.
	Scale float64
	// Reserved is what the layer-shell bars took, in logical pixels, in
	// Hyprland's own order: left, top, right, bottom.
	Reserved [4]int
	// Focused marks the monitor MonitorCurrent resolves to.
	Focused bool
	// ActiveWorkspace is the workspace currently shown on it, so a placement
	// naming no monitor can still find the one its workspace lives on.
	ActiveWorkspace int
}

// Area is a rectangle in logical pixels.
type Area struct {
	X, Y          int
	Width, Height int
}

// Logical is the whole output in the coordinate space every dispatch uses:
// the mode's pixels divided by the scale, at the monitor's own origin. It is
// the glass, bars included — what a preview draws its frame to (#181), and
// what a fullscreen window covers.
func (m Monitor) Logical() Area {
	w, h := m.Width, m.Height
	if m.Scale > 0 && m.Scale != 1 {
		// Logical pixels are the mode's pixels divided by the scale, and
		// every coordinate in a dispatch is logical. This is reasoned from
		// Hyprland's coordinate model rather than probed — both monitors on
		// the machine this was written against run unscaled — which is why
		// scripts/verify-window-placement.sh prints the numbers it computed
		// beside the ones the compositor reports.
		w = int(float64(w)/m.Scale + 0.5)
		h = int(float64(h)/m.Scale + 0.5)
	}
	return Area{X: m.X, Y: m.Y, Width: w, Height: h}
}

// Usable is the part of the monitor a window can actually occupy: the logical
// size with the bars' reservation removed. It is what a percentage resolves
// against, because "two thirds of the screen" means two thirds of the part
// that is not already spoken for — a window sized against the whole output
// would overhang the bar by exactly the bar's height and look wrong on every
// monitor the user owns.
func (m Monitor) Usable() Area {
	logical := m.Logical()
	return Area{
		X:      logical.X + m.Reserved[0],
		Y:      logical.Y + m.Reserved[1],
		Width:  logical.Width - m.Reserved[0] - m.Reserved[2],
		Height: logical.Height - m.Reserved[1] - m.Reserved[3],
	}
}

// Describe renders a monitor the way a picker or a spoken sentence should:
// the connector and what size it is, never the serial number.
func (m Monitor) Describe() string {
	return fmt.Sprintf("%s (%d by %d)", m.Name, m.Width, m.Height)
}

// Resolver turns a MonitorRef into a real output. It is a value rather than
// an interface because there is exactly one resolution rule and it belongs to
// the vocabulary; what varies is the nickname table, which is a field.
type Resolver struct {
	// Nicknames resolves a user-chosen name to a connector name — the
	// monitor-nickname store's Lookup (#180, ADR 0057). Nil is legitimate and
	// is what a daemon with no nicknames uses: a ref that is not a connector
	// or "current" then resolves to nothing and says so, exactly as it did
	// before nicknames existed.
	//
	// It is a func rather than a map so it can be backed by the store's LIVE
	// state instead of a snapshot taken when the runner was built. That is the
	// difference between a nickname the user assigned thirty seconds ago
	// working and needing a restart.
	Nicknames func(name string) (connector string, known bool)
}

// Resolve finds the monitor a ref names in one inventory.
//
// Precedence, and the reason for it: a **present output** whose connector
// matches wins over everything, because a name that is on the end of a cable
// right now is never ambiguous; then MonitorCurrent, which is a reserved word
// and cannot be shadowed; then a nickname. That order means a nickname can
// never quietly redirect a routine that named a real connector, which is the
// failure a nickname feature must not introduce.
//
// A ref that names nothing present is an error naming the ref — "no monitor
// is called top right now" — rather than a silent fallback to the focused
// screen. Falling back would place the user's morning windows on the wrong
// monitor and report success, which is the class of failure this whole ticket
// exists to end.
func (r Resolver) Resolve(ref MonitorRef, inventory []Monitor) (Monitor, error) {
	if len(inventory) == 0 {
		return Monitor{}, fmt.Errorf("the window manager reports no monitors")
	}
	trimmed := strings.TrimSpace(string(ref))
	switch ref.Kind() {
	case RefNone:
		return focusedOr(inventory), nil
	case RefCurrent:
		return focusedOr(inventory), nil
	}
	for _, m := range inventory {
		if strings.EqualFold(m.Name, trimmed) {
			return m, nil
		}
	}
	if r.Nicknames != nil {
		if connector, ok := r.Nicknames(trimmed); ok {
			for _, m := range inventory {
				if strings.EqualFold(m.Name, connector) {
					return m, nil
				}
			}
			// Worded to LEAD with "no monitor is called", which is not a
			// stylistic choice: desktop.PlacementSentence extracts the
			// speakable half of an error by finding a known prefix, so a
			// sentence that opened with the nickname would arrive at the user
			// with its most useful clause — what the name means — trimmed off.
			return Monitor{}, fmt.Errorf("no monitor is called %q right now: it means %s, "+
				"which is not plugged in; the screens plugged in are %s",
				trimmed, connector, listMonitors(inventory))
		}
	}
	return Monitor{}, fmt.Errorf("no monitor is called %q right now; the screens plugged in are %s",
		trimmed, listMonitors(inventory))
}

// ForWorkspace finds the monitor a workspace is currently shown on, for the
// case where a placement named no monitor but a percentage still has to
// resolve against something real. Falls back to the focused monitor, which is
// where an unvisited workspace will open.
func ForWorkspace(workspace int, inventory []Monitor) Monitor {
	for _, m := range inventory {
		if m.ActiveWorkspace == workspace {
			return m
		}
	}
	return focusedOr(inventory)
}

// focusedOr returns the focused monitor, or the first one when the compositor
// named none — an inventory with no focus is not a state worth an error, and
// the first output is the only defensible guess.
func focusedOr(inventory []Monitor) Monitor {
	for _, m := range inventory {
		if m.Focused {
			return m
		}
	}
	if len(inventory) == 0 {
		return Monitor{}
	}
	return inventory[0]
}

// listMonitors renders the present outputs for an error message, sorted so
// the sentence a user reads twice is the same sentence twice.
func listMonitors(inventory []Monitor) string {
	names := make([]string, 0, len(inventory))
	for _, m := range inventory {
		names = append(names, m.Name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
