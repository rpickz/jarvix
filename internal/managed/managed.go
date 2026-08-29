// Package managed is the record of which windows Jarvix may act inside
// (issue #197, ADR 0062).
//
// Every window on the desktop is one of two things. A **managed** window is
// one Jarvix opened, or one the user handed over by saying so; it may be
// read, typed into, placed, and used to run work. An **unmanaged** window is
// the user's own, and Jarvix keeps its hands off. The distinction exists so
// that "can Jarvix act in here?" is a glance at the window rather than a
// memory test, and so the set of windows a job may run in is a set with a
// name.
//
// What this package does NOT hold is worth stating first, because it is the
// whole security argument of the feature. Management is a fact about a
// *window*, never about a *command*. Typing into a terminal is running
// commands, so acquiring a terminal cannot be blanket consent to execute: a
// command typed into a managed terminal faces exactly the classification and
// confirmation `shell.run` faces (ADR 0014 — the verbatim card, the
// compound-command splitter, the risk words), and nothing in this store
// softens that. Acquisition grants ACCESS TO THE WINDOW; it grants no
// permission to run anything. See ADR 0062.
//
// Three properties are the design.
//
//   - **Identity is the whole window, never its address.** A Hyprland
//     address is a pointer value: stable while the window lives, and
//     RECYCLED afterwards. A record therefore carries the compositor
//     address, the compositor's own stable id, the application class and the
//     owning process id, and it matches a live window only when all four
//     agree — the same identity the window tools verify with before any
//     state-changing dispatch. A new window inheriting a dead one's
//     management would be the worst failure this store could have, and it
//     takes four coincidences at once rather than one.
//   - **Persistent, on the memory book's storage discipline** (ADR 0011):
//     one hand-editable TOML file under the XDG state dir, 0600 in a 0700
//     directory, atomic fsync-and-rename writes, stat-per-operation
//     hand-edit pickup, and a corrupt file warned about and moved aside
//     rather than overwritten. Persistent because a window outlives the
//     daemon: restarting jarvixd must not quietly hand the user's terminal
//     back, nor quietly keep managing something they released.
//   - **Absence is honest.** Every read is judged against the live
//     inventory, and a record whose window is not in it is dropped. Nothing
//     here can go on claiming a window that no longer exists (#180's
//     discipline), which is also why the store is never consulted without an
//     inventory to judge it against.
package managed

import "time"

// Source says how a window came to be managed. It is carried because the two
// answers are different promises: one Jarvix made to itself when it opened a
// window, one the user made out loud.
type Source string

const (
	// SourceLaunched is a window Jarvix opened — managed from birth, adopted
	// from the launch claim its identity carries (#198).
	SourceLaunched Source = "launched"
	// SourceAcquired is a window the user handed over ("take control of this
	// terminal") after a confirmation naming it.
	SourceAcquired Source = "acquired"
)

// Record is one managed window.
//
// The first four fields are its identity and are matched together, never
// singly — see the package comment on why an address alone is not a key. The
// rest is what a listing and a hand-editing user need: nothing here is ever
// used to decide anything.
type Record struct {
	// Address is the compositor's handle for the window. A reusable pointer
	// value: necessary to match, never sufficient.
	Address string
	// StableID is the compositor's own per-window identifier, empty on a
	// compositor that reports none.
	StableID string
	// Class is the application class ("ghostty", "dev.jarvix.claude").
	Class string
	// PID is the owning process. The strongest of the four: a live process
	// id is unique on the machine, and it is what stops a recycled address
	// from being mistaken for the window that used to hold it.
	PID int
	// App is the window's application name as a person would say it, kept
	// only so the file reads as something about the user's desktop rather
	// than a table of pointers.
	App string
	// Source is how it came to be managed.
	Source Source
	// Program is the program a launched window was opened to run, "" for an
	// acquired one.
	Program string
	// Since is when management began.
	Since time.Time
}

// Claim is a launch identity Jarvix has issued but whose window has not
// appeared yet (#198): "the next window classed `dev.jarvix.claude` is one I
// opened".
//
// It exists because managed-from-birth cannot be recorded as a Record — at
// the moment of launch there is no window, no address and no pid to record.
// The claim is the promise; the first inventory that shows a window wearing
// the class turns it into a Record and consumes the claim.
//
// A claim that nothing ever matches expires (ClaimGrace). That is the
// honest-absence rule applied to a promise: a launch that failed, or a
// terminal that ignored the class flag, must not leave a standing offer to
// adopt whatever turns up wearing that name later.
type Claim struct {
	// Class is the window class the launch asked for — the identity from
	// launchkind's terminal table, never anything a model chose.
	Class string
	// Program is what the window was opened to run, for the Record it
	// becomes and for the file's readability.
	Program string
	// Issued is when the launch happened; ClaimGrace is measured from it.
	Issued time.Time
}
