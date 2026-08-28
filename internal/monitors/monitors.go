// Package monitors is the monitor-nickname store (issue #180): the names the
// user gave their screens — "top" for HDMI-A-1, "bottom" for DP-2 — so a
// routine, a spoken request and the window's placement form can all name a
// screen the way its owner thinks about it.
//
// It exists because connector names are exact and brittle in exactly the way
// a cable is brittle. `DP-2` is unambiguous right up to the moment a dock
// moves or a GPU port changes, at which point every routine that named it
// breaks — and breaks *silently*, because a placement that cannot find its
// screen used to fall back to whichever one had focus. Nicknames are the
// user's own indirection over that: the routine says `top`, the store says
// what `top` currently means, and moving a cable is one correction rather
// than a rewrite of every routine.
//
// Three properties are the design.
//
//   - **Resolution happens at run time, never at write time.** Nothing ever
//     stores the connector a nickname resolved to *inside* a routine; the
//     routine keeps the nickname and the store is consulted on every run.
//     Baking the connector in would recreate the exact brittleness the
//     feature removes, one indirection later.
//   - **Persistent, on the memory book's storage discipline** (ADR 0011): one
//     hand-editable TOML file under the XDG state dir, 0600 in a 0700
//     directory, atomic fsync-and-rename writes, stat-per-operation hand-edit
//     pickup, and a corrupt file warned about and moved aside rather than
//     overwritten. This is the deliberate difference from window nicknames
//     (#130, ADR 0040), which are in-memory and die with the daemon: windows
//     are ephemeral, and monitors are furniture.
//   - **The vocabulary owns the words, not this package.** What counts as a
//     connector, which words are reserved, and how a ref resolves are all
//     internal/placement's (ADR 0056). This package fills in exactly one
//     field of placement.Resolver and refuses, at assignment, every name that
//     the vocabulary would refuse to resolve.
//
// Nicknames are not private in the way a memory fact is — "top" says nothing
// about anybody — but they live beside the stores that are, so they are
// written with the same permissions and never leave the machine.
package monitors

import "time"

// Nickname is one name the user gave one screen.
type Nickname struct {
	// Name is the nickname, normalised to the single lower-case word it is
	// looked up by. It is the entry's identity: naming a screen `top` when
	// something else is already called `top` is a refusal, not a second
	// entry.
	Name string
	// Connector is the output the name points at, spelled as the compositor
	// spells it (`HDMI-A-1`). Stored rather than resolved because it is the
	// binding itself — the *resolution* of that connector to a live screen
	// is what happens at run time.
	Connector string
	// Named is when the nickname was first given.
	Named time.Time
	// Updated is when it was last pointed at a different screen; equal to
	// Named until it is re-pointed.
	Updated time.Time
}
