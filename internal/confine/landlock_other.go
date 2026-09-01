//go:build !linux

package confine

import "errors"

// Jarvix is a Linux desktop assistant and nothing here pretends otherwise. The
// file exists so that the refusal on a machine without Landlock is a compiled,
// readable "no" rather than a build failure — and so that nothing can be added
// to this package that only makes sense on Linux without noticing.

// abiVersion reports no Landlock, which makes Available refuse with the
// sentence it already has for a kernel that has none.
func abiVersion() int { return 0 }

// restrict is never reached: Run checks Available first and returns before it
// starts anything. It refuses rather than panics so that the guarantee holds
// even if a future caller reorders that check.
func restrict(plan) error {
	return errors.New("this machine has no way to hold a boundary around a command")
}

// cloexec is likewise never reached, for the same reason.
func cloexec(int) error {
	return errors.New("this machine has no way to hold a boundary around a command")
}
